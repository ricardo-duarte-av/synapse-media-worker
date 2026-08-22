package main

import (
	"encoding/binary"
	"image"

	"github.com/disintegration/imaging"
)

// exif.go extracts the EXIF orientation tag so thumbnails come out the right
// way up. Synapse applies the same correction via PIL's getexif(), so skipping
// it would leave phone photos rotated relative to what Synapse serves.
//
// Only JPEG is handled. PNG and WebP can carry EXIF but effectively never do
// for camera images, and GIF has no EXIF at all.

const exifOrientationTag = 0x0112

// readJPEGOrientation returns the EXIF orientation value (1-8) for a JPEG, or 1
// when there is no usable tag. It never errors: a malformed EXIF block just
// means no rotation, which is what PIL does too.
func readJPEGOrientation(data []byte) int {
	const defaultOrientation = 1

	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return defaultOrientation // not a JPEG
	}

	// Walk the marker segments looking for APP1.
	for i := 2; i+4 <= len(data); {
		if data[i] != 0xFF {
			return defaultOrientation // desynchronised
		}
		marker := data[i+1]
		// Standalone markers carry no length.
		if marker == 0xD8 || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
			i += 2
			continue
		}
		// Start of scan: image data follows, no more metadata.
		if marker == 0xDA || marker == 0xD9 {
			return defaultOrientation
		}
		if i+4 > len(data) {
			return defaultOrientation
		}
		segLen := int(binary.BigEndian.Uint16(data[i+2 : i+4]))
		if segLen < 2 || i+2+segLen > len(data) {
			return defaultOrientation
		}
		if marker == 0xE1 {
			payload := data[i+4 : i+2+segLen]
			if o, ok := orientationFromExif(payload); ok {
				return o
			}
		}
		i += 2 + segLen
	}
	return defaultOrientation
}

// orientationFromExif parses an APP1 payload beginning with the "Exif\0\0"
// identifier followed by a TIFF header.
func orientationFromExif(p []byte) (int, bool) {
	const header = "Exif\x00\x00"
	if len(p) < len(header)+8 || string(p[:len(header)]) != header {
		return 0, false
	}
	tiff := p[len(header):]

	var bo binary.ByteOrder
	switch {
	case tiff[0] == 'I' && tiff[1] == 'I':
		bo = binary.LittleEndian
	case tiff[0] == 'M' && tiff[1] == 'M':
		bo = binary.BigEndian
	default:
		return 0, false
	}
	if bo.Uint16(tiff[2:4]) != 0x002A {
		return 0, false
	}
	ifdOffset := int(bo.Uint32(tiff[4:8]))
	if ifdOffset < 8 || ifdOffset+2 > len(tiff) {
		return 0, false
	}
	count := int(bo.Uint16(tiff[ifdOffset : ifdOffset+2]))
	entries := tiff[ifdOffset+2:]
	if count*12 > len(entries) {
		return 0, false
	}
	for n := range count {
		e := entries[n*12 : n*12+12]
		if bo.Uint16(e[0:2]) != exifOrientationTag {
			continue
		}
		// Orientation is a SHORT; its value sits in the first two bytes of the
		// value field, which is padded to four.
		if bo.Uint16(e[2:4]) != 3 {
			return 0, false
		}
		v := int(bo.Uint16(e[8:10]))
		if v < 1 || v > 8 {
			return 0, false
		}
		return v, true
	}
	return 0, false
}

// applyOrientation rotates and mirrors an image so it displays upright,
// reproducing Synapse's EXIF_TRANSPOSE_MAPPINGS.
//
// PIL names rotations counter-clockwise, and so does the imaging package, so
// orientation 6 ("right-top", i.e. rotate 90 clockwise to correct) maps to
// Rotate270 in both.
func applyOrientation(img image.Image, orientation int) image.Image {
	switch orientation {
	case 2:
		return imaging.FlipH(img)
	case 3:
		return imaging.Rotate180(img)
	case 4:
		return imaging.FlipV(img)
	case 5:
		return imaging.Transpose(img)
	case 6:
		return imaging.Rotate270(img)
	case 7:
		return imaging.Transverse(img)
	case 8:
		return imaging.Rotate90(img)
	default:
		return img
	}
}

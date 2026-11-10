package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"

	"github.com/gen2brain/avif"
	"github.com/gen2brain/heic"
)

// avifheic.go thumbnails AVIF and HEIC, which Synapse does not: its
// PILLOW_FORMATS has no entry for either, so it answers
// 400 "Failed to generate thumbnail." This is a deliberate deviation (see
// README.md), and it is confined to responses:
//
//   - Thumbnails derived from these formats live only in the worker's own
//     cache. They are never written through to remote_thumbnail/ or
//     remote_media_cache_thumbnails, because Synapse never writes a thumbnail
//     row for such media and the rows the worker writes must be ones Synapse
//     could have written.
//   - Upload-time thumbnails are untouched: synapseThumbnailType still decides
//     what is pre-generated, exactly as Synapse does.
//
// Both decoders run their codecs (libavif with dav1d, and a Rust HEVC decoder)
// as WebAssembly under wazero. A malformed file can at worst fail inside the
// sandbox, which bounds what a hostile upload can do to the worker. The
// packages would otherwise dlopen a system libavif/libheif when one is
// present, which runs native code in-process; builds therefore use
// -tags nodynamic, and ForceWasmMode covers HEIC even without the tag.

func init() {
	heic.ForceWasmMode = true
}

// extraSourceTypes are the media types decoded here. image/heif is the
// container's generic name and may carry either codec; the ftyp brands decide.
var extraSourceTypes = map[string]struct{}{
	"image/avif": {}, "image/heic": {}, "image/heif": {},
}

// isExtraSourceType reports whether baseType is decoded by this file rather
// than by the Synapse-equivalent decoders.
func isExtraSourceType(baseType string) bool {
	_, ok := extraSourceTypes[baseType]
	return ok
}

// heifIsAV1 reports whether an ISOBMFF file's ftyp box names an AVIF brand,
// major or compatible. Anything else in the HEIF family is handed to the HEIC
// decoder, which refuses what it cannot read.
func heifIsAV1(raw []byte) bool {
	if len(raw) < 16 || string(raw[4:8]) != "ftyp" {
		return false
	}
	size := int(binary.BigEndian.Uint32(raw[0:4]))
	if size < 16 || size > len(raw) {
		size = min(len(raw), 64)
	}
	isAV1 := func(b []byte) bool {
		s := string(b)
		return s == "avif" || s == "avis"
	}
	if isAV1(raw[8:12]) {
		return true
	}
	// raw[12:16] is the minor version; compatible brands follow.
	for i := 16; i+4 <= size; i += 4 {
		if isAV1(raw[i : i+4]) {
			return true
		}
	}
	return false
}

// decodeExtra decodes an AVIF or HEIC source, refusing it before decoding if
// its declared dimensions are over the pixel limit. Orientation (irot/imir) is
// applied by the decoders; HEIF makes those properties authoritative, so the
// EXIF orientation such files also carry must not be applied on top.
func (t *Thumbnailer) decodeExtra(raw []byte) (image.Image, error) {
	av1 := heifIsAV1(raw)

	var cfg image.Config
	var err error
	if av1 {
		cfg, err = avif.DecodeConfig(bytes.NewReader(raw))
	} else {
		cfg, err = heic.DecodeConfig(bytes.NewReader(withoutMdat(raw)))
	}
	if err != nil {
		return nil, fmt.Errorf("%w: reading source header: %v", ErrCannotGenerate, err)
	}
	if err := t.checkPixels(cfg.Width, cfg.Height); err != nil {
		return nil, err
	}

	var img image.Image
	if av1 {
		img, err = avif.Decode(bytes.NewReader(raw), avif.Options{AutoRotate: true})
	} else {
		img, err = heic.Decode(bytes.NewReader(raw))
	}
	if err != nil {
		return nil, fmt.Errorf("%w: decoding source: %v", ErrCannotGenerate, err)
	}
	return img, nil
}

// withoutMdat returns the file's top-level boxes minus any mdat, for reading
// the header. heic.DecodeConfig only looks at the first 256 KiB, and some
// phones (Samsung, for one) write mdat before meta, which pushes the header
// past it: the pixel-limit check then refused photos that decode fine. The
// header needs only meta; mdat holds the coded pixels.
func withoutMdat(raw []byte) []byte {
	out := make([]byte, 0, 64<<10)
	for i := 0; i+8 <= len(raw); {
		size := uint64(binary.BigEndian.Uint32(raw[i:]))
		switch size {
		case 0: // extends to the end of the file
			size = uint64(len(raw) - i)
		case 1: // 64-bit size follows the type
			if i+16 > len(raw) {
				return raw
			}
			size = binary.BigEndian.Uint64(raw[i+8:])
		}
		if size < 8 || size > uint64(len(raw)-i) {
			// Malformed or truncated: let the decoder judge the original.
			return raw
		}
		if string(raw[i+4:i+8]) != "mdat" {
			out = append(out, raw[i:i+int(size)]...)
		}
		i += int(size)
	}
	return out
}

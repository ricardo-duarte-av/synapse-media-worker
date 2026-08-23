package main

import "encoding/binary"

// webpanim.go extracts the first frame of an animated WebP as a still image.
//
// golang.org/x/image/webp decodes still WebP -- both the simple VP8/VP8L forms
// and the extended VP8X form with alpha -- but it cannot walk animation frames.
// Without this, every thumbnail of an animated WebP was handed to Synapse
// instead, even for a plain static thumbnail that needs only the first frame.
//
// The parsing here is deliberately defensive: the input is a file from an
// arbitrary remote server, so every length is checked against what is actually
// present rather than trusted.

const (
	riffHeaderSize  = 12 // "RIFF" + size + "WEBP"
	chunkHeaderSize = 8  // fourcc + size
	vp8xPayloadSize = 10
	anmfHeaderSize  = 16 // frame x/y/w/h/duration/flags, before the image data
	vp8xAlphaBit    = 1 << 4
	vp8xAnimBit     = 1 << 1
)

// isAnimatedWebP reports whether the data is a WebP whose VP8X header sets the
// animation flag.
func isAnimatedWebP(data []byte) bool {
	if !isWebP(data) {
		return false
	}
	for _, c := range webpChunks(data) {
		if c.fourCC == "VP8X" && len(c.payload) == vp8xPayloadSize {
			return c.payload[0]&vp8xAnimBit != 0
		}
	}
	return false
}

func isWebP(data []byte) bool {
	return len(data) >= riffHeaderSize &&
		string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP"
}

type webpChunk struct {
	fourCC  string
	payload []byte
}

// webpChunks walks the RIFF chunks of a WebP file, stopping at the first
// malformed or truncated one rather than reading past the buffer.
func webpChunks(data []byte) []webpChunk {
	var out []webpChunk
	for off := riffHeaderSize; off+chunkHeaderSize <= len(data); {
		fourCC := string(data[off : off+4])
		size := int(binary.LittleEndian.Uint32(data[off+4 : off+8]))
		start := off + chunkHeaderSize
		if size < 0 || start+size > len(data) {
			break
		}
		out = append(out, webpChunk{fourCC: fourCC, payload: data[start : start+size]})
		// Chunks are padded to an even length.
		off = start + size
		if size%2 == 1 {
			off++
		}
	}
	return out
}

// firstWebPFrame rebuilds the first frame of an animated WebP as a still WebP
// that a standard decoder will accept, reporting false if the data is not an
// animation or the frame cannot be found.
func firstWebPFrame(data []byte) ([]byte, bool) {
	if !isAnimatedWebP(data) {
		return nil, false
	}
	for _, c := range webpChunks(data) {
		if c.fourCC != "ANMF" || len(c.payload) <= anmfHeaderSize {
			continue
		}
		// The frame's own dimensions follow x and y, each three bytes and
		// stored as the value minus one.
		h := c.payload
		width := int(uint32(h[6])|uint32(h[7])<<8|uint32(h[8])<<16) + 1
		height := int(uint32(h[9])|uint32(h[10])<<8|uint32(h[11])<<16) + 1

		var alpha, image []byte
		for _, sub := range framePayloadChunks(c.payload[anmfHeaderSize:]) {
			switch sub.fourCC {
			case "ALPH":
				alpha = sub.payload
			case "VP8 ", "VP8L":
				image = rawChunk(sub)
			}
		}
		if image == nil {
			continue
		}
		return buildStillWebP(width, height, alpha, image), true
	}
	return nil, false
}

// framePayloadChunks walks the sub-chunks inside an ANMF frame.
func framePayloadChunks(data []byte) []webpChunk {
	var out []webpChunk
	for off := 0; off+chunkHeaderSize <= len(data); {
		fourCC := string(data[off : off+4])
		size := int(binary.LittleEndian.Uint32(data[off+4 : off+8]))
		start := off + chunkHeaderSize
		if size < 0 || start+size > len(data) {
			break
		}
		out = append(out, webpChunk{fourCC: fourCC, payload: data[start : start+size]})
		off = start + size
		if size%2 == 1 {
			off++
		}
	}
	return out
}

// rawChunk re-serialises a chunk with its header, ready to be placed in a new
// container.
func rawChunk(c webpChunk) []byte {
	out := make([]byte, 0, chunkHeaderSize+len(c.payload)+1)
	out = append(out, c.fourCC...)
	var size [4]byte
	binary.LittleEndian.PutUint32(size[:], uint32(len(c.payload)))
	out = append(out, size[:]...)
	out = append(out, c.payload...)
	if len(c.payload)%2 == 1 {
		out = append(out, 0)
	}
	return out
}

// buildStillWebP wraps a frame's image data in a container a still decoder
// accepts: the simple form when there is no alpha, and the extended VP8X form
// when there is, so transparency survives into the thumbnail.
func buildStillWebP(width, height int, alpha, image []byte) []byte {
	var body []byte
	if alpha != nil {
		vp8x := make([]byte, vp8xPayloadSize)
		vp8x[0] = vp8xAlphaBit
		putUint24(vp8x[4:], uint32(width-1))
		putUint24(vp8x[7:], uint32(height-1))
		body = append(body, rawChunk(webpChunk{fourCC: "VP8X", payload: vp8x})...)
		body = append(body, rawChunk(webpChunk{fourCC: "ALPH", payload: alpha})...)
	}
	body = append(body, image...)

	out := make([]byte, 0, riffHeaderSize+len(body))
	out = append(out, "RIFF"...)
	var size [4]byte
	// The RIFF size covers "WEBP" plus everything after it.
	binary.LittleEndian.PutUint32(size[:], uint32(len(body)+4))
	out = append(out, size[:]...)
	out = append(out, "WEBP"...)
	out = append(out, body...)
	return out
}

func putUint24(b []byte, v uint32) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
}

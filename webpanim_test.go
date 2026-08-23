package main

import (
	"bytes"
	"context"
	"image"
	"os"
	"testing"

	_ "golang.org/x/image/webp"
)

// buildAnimatedWebP assembles a minimal animated WebP around a still frame, so
// the extractor can be tested without depending on a particular file.
func buildAnimatedWebP(t *testing.T, frame []byte, w, h int, withAlpha bool) []byte {
	t.Helper()
	vp8x := make([]byte, vp8xPayloadSize)
	vp8x[0] = vp8xAnimBit
	if withAlpha {
		vp8x[0] |= vp8xAlphaBit
	}
	putUint24(vp8x[4:], uint32(w-1))
	putUint24(vp8x[7:], uint32(h-1))

	anmf := make([]byte, anmfHeaderSize)
	putUint24(anmf[6:], uint32(w-1))
	putUint24(anmf[9:], uint32(h-1))
	anmf = append(anmf, frame...)

	var body []byte
	body = append(body, rawChunk(webpChunk{fourCC: "VP8X", payload: vp8x})...)
	body = append(body, rawChunk(webpChunk{fourCC: "ANIM", payload: make([]byte, 6)})...)
	body = append(body, rawChunk(webpChunk{fourCC: "ANMF", payload: anmf})...)
	return buildRIFF(body)
}

func buildRIFF(body []byte) []byte {
	out := append([]byte("RIFF"), 0, 0, 0, 0)
	out = append(out, "WEBP"...)
	out = append(out, body...)
	out[4] = byte(len(body) + 4)
	out[5] = byte((len(body) + 4) >> 8)
	out[6] = byte((len(body) + 4) >> 16)
	out[7] = byte((len(body) + 4) >> 24)
	return out
}

func TestFirstWebPFrameRebuildsAStill(t *testing.T) {
	// A stand-in VP8 payload: the extractor must relocate it verbatim.
	payload := bytes.Repeat([]byte{0xAB}, 41) // odd length, to exercise padding
	animated := buildAnimatedWebP(t, rawChunk(webpChunk{fourCC: "VP8 ", payload: payload}), 64, 48, false)

	if !isAnimatedWebP(animated) {
		t.Fatal("did not recognise the animation flag")
	}
	still, ok := firstWebPFrame(animated)
	if !ok {
		t.Fatal("no frame extracted")
	}
	if !isWebP(still) {
		t.Fatal("result is not a WebP container")
	}
	if isAnimatedWebP(still) {
		t.Error("the rebuilt still is still marked animated")
	}
	// The frame's image data must survive unchanged.
	var found bool
	for _, c := range webpChunks(still) {
		if c.fourCC == "VP8 " {
			found = true
			if !bytes.Equal(c.payload, payload) {
				t.Error("frame payload was altered")
			}
		}
	}
	if !found {
		t.Error("no VP8 chunk in the rebuilt still")
	}
}

// Transparency must survive, or animated stickers thumbnail with black boxes.
func TestFirstWebPFramePreservesAlpha(t *testing.T) {
	alpha := bytes.Repeat([]byte{0x11}, 7)
	frame := append(rawChunk(webpChunk{fourCC: "ALPH", payload: alpha}),
		rawChunk(webpChunk{fourCC: "VP8 ", payload: bytes.Repeat([]byte{0xCD}, 10)})...)
	animated := buildAnimatedWebP(t, frame, 32, 32, true)

	still, ok := firstWebPFrame(animated)
	if !ok {
		t.Fatal("no frame extracted")
	}
	var sawVP8X, sawALPH bool
	for _, c := range webpChunks(still) {
		switch c.fourCC {
		case "VP8X":
			sawVP8X = true
			if c.payload[0]&vp8xAlphaBit == 0 {
				t.Error("alpha bit not set on the rebuilt VP8X")
			}
			if c.payload[0]&vp8xAnimBit != 0 {
				t.Error("animation bit left set on the rebuilt VP8X")
			}
		case "ALPH":
			sawALPH = true
			if !bytes.Equal(c.payload, alpha) {
				t.Error("alpha payload altered")
			}
		}
	}
	if !sawVP8X || !sawALPH {
		t.Errorf("rebuilt still is missing VP8X=%v ALPH=%v", sawVP8X, sawALPH)
	}
}

// A still WebP must be left alone, not rewritten.
func TestFirstWebPFrameIgnoresStillImages(t *testing.T) {
	still := buildRIFF(rawChunk(webpChunk{fourCC: "VP8 ", payload: bytes.Repeat([]byte{1}, 8)}))
	if isAnimatedWebP(still) {
		t.Error("a still was reported as animated")
	}
	if _, ok := firstWebPFrame(still); ok {
		t.Error("a still was rewritten")
	}
	// And non-WebP input must simply be declined.
	for _, data := range [][]byte{nil, []byte("not a webp"), []byte("RIFF")} {
		if _, ok := firstWebPFrame(data); ok {
			t.Errorf("accepted non-WebP input %q", data)
		}
	}
}

// The input is a file from an arbitrary remote server, so malformed and
// truncated containers must be declined rather than read past.
func TestFirstWebPFrameRejectsMalformedInput(t *testing.T) {
	good := buildAnimatedWebP(t, rawChunk(webpChunk{fourCC: "VP8 ", payload: bytes.Repeat([]byte{2}, 16)}), 8, 8, false)
	for cut := len(good) - 1; cut > 0; cut -= 7 {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panicked on input truncated to %d bytes: %v", cut, r)
				}
			}()
			_, _ = firstWebPFrame(good[:cut])
			_ = isAnimatedWebP(good[:cut])
		}()
	}
	// A chunk claiming more data than exists must not be followed.
	lying := append([]byte{}, good...)
	lying[riffHeaderSize+4] = 0xFF
	lying[riffHeaderSize+5] = 0xFF
	lying[riffHeaderSize+6] = 0xFF
	if _, ok := firstWebPFrame(lying); ok {
		t.Error("followed a chunk length past the end of the buffer")
	}
}

// The real proof: an animated WebP from a live server, which the decoder
// rejects outright, must become a decodable still.
func TestFirstWebPFrameOnRealFile(t *testing.T) {
	path := os.Getenv("SMW_ANIMATED_WEBP")
	if path == "" {
		t.Skip("set SMW_ANIMATED_WEBP to a real animated WebP to run")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := image.Decode(bytes.NewReader(raw)); err == nil {
		t.Skip("this file decodes as-is; it is not an animation")
	}
	still, ok := firstWebPFrame(raw)
	if !ok {
		t.Fatal("no frame extracted from a real animated WebP")
	}
	img, format, err := image.Decode(bytes.NewReader(still))
	if err != nil {
		t.Fatalf("the rebuilt still does not decode: %v", err)
	}
	b := img.Bounds()
	t.Logf("decoded %s, %dx%d from a %d byte animation", format, b.Dx(), b.Dy(), len(raw))
	if b.Dx() == 0 || b.Dy() == 0 {
		t.Error("decoded to an empty image")
	}

	// And the whole thumbnail path, not just the decode: this is what was
	// previously refused and handed to Synapse.
	th := NewThumbnailer(104857600, 2)
	req := ThumbnailRequest{Width: 96, Height: 96, Method: methodCrop, Type: typePNG}
	if !th.CanGenerate("image/webp", req) {
		t.Fatal("an animated webp source is not considered generatable")
	}
	out, err := th.Generate(context.Background(), path, "image/webp", req)
	if err != nil {
		t.Fatalf("generating a static thumbnail failed: %v", err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("the thumbnail does not decode: %v", err)
	}
	if cfg.Width != 96 || cfg.Height != 96 {
		t.Errorf("thumbnail is %dx%d, want 96x96", cfg.Width, cfg.Height)
	}
	t.Logf("generated a %dx%d png thumbnail, %d bytes", cfg.Width, cfg.Height, len(out))
}

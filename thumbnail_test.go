package main

import (
	"image"
	"testing"
)

// Golden values taken from a real Synapse media store: a 899x1599 JPEG whose
// stored scale thumbnails are 134x240, 269x480 and 337x600. These are the
// post-aspect dimensions Synapse computed with Python integer division, so they
// verify the port of that arithmetic rather than restating it.
func TestAspectMatchesSynapseStoredDimensions(t *testing.T) {
	const srcW, srcH = 899, 1599
	cases := []struct{ maxW, maxH, wantW, wantH int }{
		{320, 240, 134, 240},
		{640, 480, 269, 480},
		{800, 600, 337, 600},
	}
	for _, c := range cases {
		w, h := aspect(srcW, srcH, c.maxW, c.maxH)
		w, h = min(srcW, w), min(srcH, h)
		if w != c.wantW || h != c.wantH {
			t.Errorf("aspect(%d,%d,%d,%d) = %dx%d, want %dx%d",
				srcW, srcH, c.maxW, c.maxH, w, h, c.wantW, c.wantH)
		}
	}
}

func TestAspectLandscape(t *testing.T) {
	// A 16:9 source scaled into 320x240 gives 320x180, which matches the most
	// common stored size on a real server.
	w, h := aspect(1920, 1080, 320, 240)
	if w != 320 || h != 180 {
		t.Errorf("got %dx%d, want 320x180", w, h)
	}
}

// Extremely elongated images must never produce a zero dimension.
func TestAspectNeverReturnsZero(t *testing.T) {
	w, h := aspect(10000, 1, 32, 32)
	if w < 1 || h < 1 {
		t.Errorf("got %dx%d, want both >= 1", w, h)
	}
	w, h = aspect(1, 10000, 32, 32)
	if w < 1 || h < 1 {
		t.Errorf("got %dx%d, want both >= 1", w, h)
	}
}

func TestCropBox(t *testing.T) {
	// Portrait source, square target: scale to width, crop the middle band.
	scaledW, scaledH, box := cropBox(899, 1599, 32, 32)
	if scaledW != 32 || scaledH != 56 {
		t.Errorf("scaled to %dx%d, want 32x56", scaledW, scaledH)
	}
	if box != image.Rect(0, 12, 32, 44) {
		t.Errorf("box = %v, want (0,12)-(32,44)", box)
	}
	if box.Dx() != 32 || box.Dy() != 32 {
		t.Errorf("crop produced %dx%d, want 32x32", box.Dx(), box.Dy())
	}
}

func TestCropBoxLandscape(t *testing.T) {
	// Landscape source, square target: scale to height, crop the middle column.
	scaledW, scaledH, box := cropBox(1920, 1080, 96, 96)
	if scaledH != 96 {
		t.Errorf("scaled height = %d, want 96", scaledH)
	}
	if scaledW != 170 {
		t.Errorf("scaled width = %d, want 170", scaledW)
	}
	if box.Dx() != 96 || box.Dy() != 96 {
		t.Errorf("crop produced %dx%d, want 96x96", box.Dx(), box.Dy())
	}
	// The crop must be centred.
	if box.Min.X != (170-96)/2 {
		t.Errorf("crop not centred: %v", box)
	}
}

// Whatever the source shape, a crop always yields exactly the requested size.
func TestCropAlwaysYieldsRequestedSize(t *testing.T) {
	sources := [][2]int{{899, 1599}, {1920, 1080}, {500, 500}, {3000, 17}, {17, 3000}}
	targets := [][2]int{{32, 32}, {96, 96}, {800, 600}, {173, 91}}
	for _, s := range sources {
		for _, tg := range targets {
			_, _, box := cropBox(s[0], s[1], tg[0], tg[1])
			if box.Dx() != tg[0] || box.Dy() != tg[1] {
				t.Errorf("source %dx%d target %dx%d: box %v is %dx%d",
					s[0], s[1], tg[0], tg[1], box, box.Dx(), box.Dy())
			}
		}
	}
}

func TestFindExactRequiresAllFourFields(t *testing.T) {
	rows := []ThumbnailRow{
		{Width: 32, Height: 32, Method: "crop", Type: "image/jpeg"},
		{Width: 96, Height: 96, Method: "crop", Type: "image/png"},
		{Width: 320, Height: 180, Method: "scale", Type: "image/png"},
	}
	// Exact match.
	if _, ok := FindExact(rows, ThumbnailRequest{Width: 96, Height: 96, Method: "crop", Type: "image/png"}); !ok {
		t.Error("exact match not found")
	}
	// Right size and method, wrong type: with dynamic_thumbnails this is a miss,
	// which is why upload-time JPEG thumbnails are never served to clients.
	if _, ok := FindExact(rows, ThumbnailRequest{Width: 32, Height: 32, Method: "crop", Type: "image/png"}); ok {
		t.Error("matched despite differing type")
	}
	// Right size and type, wrong method.
	if _, ok := FindExact(rows, ThumbnailRequest{Width: 96, Height: 96, Method: "scale", Type: "image/png"}); ok {
		t.Error("matched despite differing method")
	}
	// Near-miss on size is still a miss; there is no nearest-match fallback.
	if _, ok := FindExact(rows, ThumbnailRequest{Width: 320, Height: 240, Method: "scale", Type: "image/png"}); ok {
		t.Error("matched a different size")
	}
}

func TestCanGenerate(t *testing.T) {
	th := NewThumbnailer(100_000_000)
	png := ThumbnailRequest{Width: 96, Height: 96, Method: "crop", Type: typePNG}
	for _, src := range []string{"image/jpeg", "image/png", "image/gif", "image/webp"} {
		if !th.CanGenerate(src, png) {
			t.Errorf("should be able to generate png from %s", src)
		}
	}
	for _, src := range []string{"image/svg+xml", "application/pdf", "video/mp4", ""} {
		if th.CanGenerate(src, png) {
			t.Errorf("should refuse to generate from %s", src)
		}
	}
	// Animated output needs a WebP encoder the worker does not have.
	animated := ThumbnailRequest{Width: 96, Height: 96, Method: "crop", Type: typeWebP, Animated: true}
	if th.CanGenerate("image/gif", animated) {
		t.Error("should refuse animated webp")
	}
}

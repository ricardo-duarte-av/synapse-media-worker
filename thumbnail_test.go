package main

import (
	"context"
	"errors"
	"image"
	"runtime"
	"testing"
	"time"
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
	th := NewThumbnailer(100_000_000, 4)
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

// Generation is CPU-bound and holds the decoded bitmap in memory, so it must
// not run unbounded: a burst of distinct sizes would otherwise decode all at
// once. This checks the limit is actually enforced.
func TestGenerationConcurrencyIsBounded(t *testing.T) {
	th := NewThumbnailer(100_000_000, 2)

	if err := th.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := th.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}

	// The third must block until a slot frees.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := th.acquire(ctx); err == nil {
		t.Fatal("acquired a third slot with a limit of two")
	}

	th.release()
	if err := th.acquire(context.Background()); err != nil {
		t.Errorf("could not acquire after a release: %v", err)
	}
}

// A client that goes away must not hold a generation slot.
func TestGenerationSlotReleasedOnCancelledRequest(t *testing.T) {
	th := NewThumbnailer(100_000_000, 1)
	if err := th.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- th.acquire(ctx) }()

	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("got %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("acquire did not give up when the caller went away")
	}
}

func TestConcurrencyDefaultsToCPUCount(t *testing.T) {
	th := NewThumbnailer(100_000_000, 0)
	if cap(th.slots) != runtime.GOMAXPROCS(0) {
		t.Errorf("default limit = %d, want GOMAXPROCS %d", cap(th.slots), runtime.GOMAXPROCS(0))
	}
}

// Synapse strips content-type parameters before deciding whether it can
// thumbnail something. Media stored as "image/png; charset=binary" is still a
// PNG; treating it as unthumbnailable handed decodable images to Synapse to do
// instead, which a live request confirmed.
func TestCanGenerateStripsContentTypeParameters(t *testing.T) {
	th := NewThumbnailer(100_000_000, 2)
	req := ThumbnailRequest{Width: 96, Height: 96, Method: "crop", Type: typePNG}

	for _, ct := range []string{
		"image/png; charset=binary",
		"image/jpeg;charset=utf-8",
		"IMAGE/PNG; charset=binary",
		"  image/webp ; foo=bar",
	} {
		if !th.CanGenerate(ct, req) {
			t.Errorf("%q should be thumbnailable; Synapse strips the parameters", ct)
		}
	}
	// Stripping must not make unsupported types look supported.
	for _, ct := range []string{
		"video/mp4", "image/svg+xml; charset=utf-8", "image/avif", "image/bmp",
		"application/pdf", "",
	} {
		if th.CanGenerate(ct, req) {
			t.Errorf("%q should not be thumbnailable", ct)
		}
	}
}

func TestBaseMediaType(t *testing.T) {
	cases := map[string]string{
		"image/png":                 "image/png",
		"image/png; charset=binary": "image/png",
		"IMAGE/PNG":                 "image/png",
		"  image/webp ; q=1":        "image/webp",
		"":                          "",
	}
	for in, want := range cases {
		if got := baseMediaType(in); got != want {
			t.Errorf("baseMediaType(%q) = %q, want %q", in, got, want)
		}
	}
}

// The upload-time format map must agree with what the thumbnailer can decode,
// or uploads would be told to generate something that then fails.
func TestUploadFormatMapAgreesWithDecodableTypes(t *testing.T) {
	th := NewThumbnailer(100_000_000, 2)
	req := ThumbnailRequest{Width: 96, Height: 96, Method: "crop", Type: typePNG}
	for _, ct := range []string{"image/jpeg", "image/jpg", "image/webp", "image/gif", "image/png"} {
		if _, ok := synapseThumbnailType(ct); !ok {
			t.Errorf("%q is missing from the upload format map", ct)
		}
		if !th.CanGenerate(ct, req) {
			t.Errorf("%q is in the format map but the thumbnailer cannot decode it", ct)
		}
	}
}

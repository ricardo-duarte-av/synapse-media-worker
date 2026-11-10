package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The fixtures are 64x48: the left half red, the right half blue. The rotated
// ones are the same pictures with an irot box (90 degrees anticlockwise) added,
// which libheif reads as 48x64 with blue on top.

func TestCanGenerateAVIFAndHEIC(t *testing.T) {
	th := NewThumbnailer(100_000_000, 2)
	req := ThumbnailRequest{Width: 96, Height: 96, Method: "crop", Type: typePNG}
	for _, ct := range []string{"image/avif", "image/heic", "image/heif", "IMAGE/HEIC; foo=bar"} {
		if !th.CanGenerate(ct, req) {
			t.Errorf("%q should be thumbnailable", ct)
		}
	}
	// Animated output still needs a WebP encoder.
	animated := ThumbnailRequest{Width: 96, Height: 96, Method: "crop", Type: typeWebP, Animated: true}
	if th.CanGenerate("image/avif", animated) {
		t.Error("animated output from AVIF should be refused")
	}

	// thumbnail_avif_heic: false restores Synapse's behaviour exactly.
	th.extraFormats = false
	for _, ct := range []string{"image/avif", "image/heic", "image/heif"} {
		if th.CanGenerate(ct, req) {
			t.Errorf("%q should be refused with the deviation turned off", ct)
		}
	}
	if !th.CanGenerate("image/png", req) {
		t.Error("turning the deviation off must not affect Synapse's own formats")
	}
}

// Synapse does not pre-generate thumbnails for these at upload time, so the
// worker must not either.
func TestUploadThumbnailsExcludeAVIFAndHEIC(t *testing.T) {
	for _, ct := range []string{"image/avif", "image/heic", "image/heif"} {
		if _, ok := synapseThumbnailType(ct); ok {
			t.Errorf("%q must not get upload-time thumbnails; Synapse makes none", ct)
		}
	}
}

func TestAVIFHEICDefaultOn(t *testing.T) {
	if !(MediaConfig{}).AVIFHEICThumbnails() {
		t.Error("thumbnail_avif_heic should default to true")
	}
	off := false
	if (MediaConfig{ThumbnailAVIFHEIC: &off}).AVIFHEICThumbnails() {
		t.Error("thumbnail_avif_heic: false should turn it off")
	}
}

func generateFixture(t *testing.T, th *Thumbnailer, file, sourceType string, req ThumbnailRequest) image.Image {
	t.Helper()
	out, err := th.Generate(context.Background(), filepath.Join("testdata", file), sourceType, req)
	if err != nil {
		t.Fatalf("%s as %s: %v", file, sourceType, err)
	}
	img, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("%s: output is not a png: %v", file, err)
	}
	return img
}

// isRed and isBlue tolerate lossy coding and resampling.
func isRed(img image.Image, x, y int) bool {
	r, g, b, _ := img.At(x, y).RGBA()
	return r > 0xc000 && g < 0x4000 && b < 0x4000
}

func isBlue(img image.Image, x, y int) bool {
	r, g, b, _ := img.At(x, y).RGBA()
	return b > 0xc000 && r < 0x4000 && g < 0x4000
}

func TestThumbnailAVIFAndHEIC(t *testing.T) {
	th := NewThumbnailer(100_000_000, 2)
	scale := ThumbnailRequest{Width: 32, Height: 32, Method: "scale", Type: typePNG}
	crop := ThumbnailRequest{Width: 16, Height: 16, Method: "crop", Type: typePNG}

	for _, tc := range []struct{ file, sourceType string }{
		{"landscape.avif", "image/avif"},
		{"landscape.heic", "image/heic"},
		{"landscape.heic", "image/heif"},
		// image/heif names the container, which may carry AV1.
		{"landscape.avif", "image/heif"},
	} {
		img := generateFixture(t, th, tc.file, tc.sourceType, scale)
		if b := img.Bounds(); b.Dx() != 32 || b.Dy() != 24 {
			t.Errorf("%s as %s: scaled to %dx%d, want 32x24", tc.file, tc.sourceType, b.Dx(), b.Dy())
			continue
		}
		if !isRed(img, 4, 12) || !isBlue(img, 28, 12) {
			t.Errorf("%s as %s: want red on the left and blue on the right", tc.file, tc.sourceType)
		}

		img = generateFixture(t, th, tc.file, tc.sourceType, crop)
		if b := img.Bounds(); b.Dx() != 16 || b.Dy() != 16 {
			t.Errorf("%s as %s: cropped to %dx%d, want 16x16", tc.file, tc.sourceType, b.Dx(), b.Dy())
		}
	}
}

// HEIF carries orientation as irot/imir properties, which are authoritative.
// Phones write them for every portrait photo; ignoring them would thumbnail
// those photos on their side.
func TestThumbnailAppliesHEIFRotation(t *testing.T) {
	th := NewThumbnailer(100_000_000, 2)
	req := ThumbnailRequest{Width: 64, Height: 64, Method: "scale", Type: typePNG}
	for _, tc := range []struct{ file, sourceType string }{
		{"rotated.avif", "image/avif"},
		{"rotated.heic", "image/heic"},
	} {
		img := generateFixture(t, th, tc.file, tc.sourceType, req)
		if b := img.Bounds(); b.Dx() != 48 || b.Dy() != 64 {
			t.Errorf("%s: got %dx%d, want the rotated 48x64", tc.file, b.Dx(), b.Dy())
			continue
		}
		if !isBlue(img, 24, 4) || !isRed(img, 24, 60) {
			t.Errorf("%s: want blue on top and red below after rotating anticlockwise", tc.file)
		}
	}
}

func TestAVIFHEICPixelLimit(t *testing.T) {
	// 64x48 is 3072 pixels; Synapse refuses at the limit, not only above it.
	th := NewThumbnailer(64*48, 2)
	req := ThumbnailRequest{Width: 32, Height: 32, Method: "scale", Type: typePNG}
	for _, tc := range []struct{ file, sourceType string }{
		{"landscape.avif", "image/avif"},
		{"landscape.heic", "image/heic"},
	} {
		_, err := th.Generate(context.Background(), filepath.Join("testdata", tc.file), tc.sourceType, req)
		if !errors.Is(err, ErrCannotGenerate) {
			t.Errorf("%s at the pixel limit: got %v, want ErrCannotGenerate", tc.file, err)
		}
	}
}

// The AVIF and HEIC packages register with image.Decode, so a mislabelled file
// would otherwise be decoded by them under a type Synapse can thumbnail. That
// stays Synapse's answer, a refusal.
func TestMislabelledAVIFIsRefused(t *testing.T) {
	th := NewThumbnailer(100_000_000, 2)
	req := ThumbnailRequest{Width: 32, Height: 32, Method: "scale", Type: typePNG}
	for _, file := range []string{"landscape.avif", "landscape.heic"} {
		_, err := th.Generate(context.Background(), filepath.Join("testdata", file), "image/png", req)
		if !errors.Is(err, ErrCannotGenerate) {
			t.Errorf("%s declared as image/png: got %v, want ErrCannotGenerate", file, err)
		}
	}
}

func TestUndecodableHEICIsRefused(t *testing.T) {
	th := NewThumbnailer(100_000_000, 2)
	req := ThumbnailRequest{Width: 32, Height: 32, Method: "scale", Type: typePNG}
	path := filepath.Join(t.TempDir(), "junk")
	// A plausible ftyp followed by nothing usable.
	junk := append([]byte("\x00\x00\x00\x18ftypheic\x00\x00\x00\x00mif1heic"), bytes.Repeat([]byte{0xAB}, 512)...)
	if err := os.WriteFile(path, junk, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, ct := range []string{"image/heic", "image/avif"} {
		_, err := th.Generate(context.Background(), path, ct, req)
		if !errors.Is(err, ErrCannotGenerate) {
			t.Errorf("junk as %s: got %v, want ErrCannotGenerate", ct, err)
		}
	}
}

func TestHEIFIsAV1(t *testing.T) {
	ftyp := func(major string, compat ...string) []byte {
		b := []byte{0, 0, 0, byte(16 + 4*len(compat))}
		b = append(b, "ftyp"+major+"\x00\x00\x00\x00"...)
		for _, c := range compat {
			b = append(b, c...)
		}
		return append(b, make([]byte, 16)...)
	}
	cases := []struct {
		raw  []byte
		want bool
	}{
		{ftyp("avif", "mif1"), true},
		{ftyp("avis"), true},
		{ftyp("mif1", "miaf", "avif"), true},
		{ftyp("heic", "mif1"), false},
		{ftyp("mif1", "heic"), false},
		{[]byte("not an isobmff file at all"), false},
		{nil, false},
	}
	for i, c := range cases {
		if got := heifIsAV1(c.raw); got != c.want {
			t.Errorf("case %d: got %v, want %v", i, got, c.want)
		}
	}
	for _, f := range []struct {
		file string
		want bool
	}{{"landscape.avif", true}, {"landscape.heic", false}} {
		raw, err := os.ReadFile(filepath.Join("testdata", f.file))
		if err != nil {
			t.Fatal(err)
		}
		if got := heifIsAV1(raw); got != f.want {
			t.Errorf("%s: got %v, want %v", f.file, got, f.want)
		}
	}
}

// AVIF and HEIC decoding needs far more memory than other formats, so it has
// its own smaller limit. Waiting on it must not hold a general slot, or a queue
// of HEIC photos would stall thumbnails of everything else.
func TestAVIFHEICConcurrencyIsBoundedSeparately(t *testing.T) {
	th := NewThumbnailer(100_000_000, 2)
	th.SetMaxConcurrentAVIFHEIC(1)
	req := ThumbnailRequest{Width: 32, Height: 32, Method: "scale", Type: typePNG}

	th.extraSlots <- struct{}{} // another AVIF/HEIC is being decoded

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := th.Generate(ctx, filepath.Join("testdata", "landscape.heic"), "image/heic", req)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want to wait for the AVIF/HEIC slot", err)
	}
	if len(th.slots) != 0 {
		t.Errorf("%d general slots held while waiting for the AVIF/HEIC one", len(th.slots))
	}

	pngPath := filepath.Join(t.TempDir(), "src.png")
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 64, 48))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pngPath, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := th.Generate(context.Background(), pngPath, "image/png", req); err != nil {
		t.Errorf("a PNG should not wait behind AVIF/HEIC: %v", err)
	}

	<-th.extraSlots
	if _, err := th.Generate(context.Background(), filepath.Join("testdata", "landscape.heic"), "image/heic", req); err != nil {
		t.Errorf("after the slot frees: %v", err)
	}
	if len(th.extraSlots) != 0 || len(th.slots) != 0 {
		t.Error("slots were not released after generating")
	}
}

// Some phones write mdat before meta, so the header sits past the 256 KiB the
// HEIC package reads for DecodeConfig. A real Samsung photo with a 1 MB mdat
// first was refused by the pixel-limit check while decoding fine.
func TestHEICWithMdatFirst(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "landscape.heic"))
	if err != nil {
		t.Fatal(err)
	}
	moved := mdatFirst(t, raw, 1<<20)
	path := filepath.Join(t.TempDir(), "mdat-first.heic")
	if err := os.WriteFile(path, moved, 0o600); err != nil {
		t.Fatal(err)
	}
	th := NewThumbnailer(100_000_000, 2)
	req := ThumbnailRequest{Width: 32, Height: 32, Method: "scale", Type: typePNG}
	out, err := th.Generate(context.Background(), path, "image/heic", req)
	if err != nil {
		t.Fatalf("HEIC with mdat first: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 32 || b.Dy() != 24 {
		t.Errorf("got %dx%d, want 32x24", b.Dx(), b.Dy())
	}
}

// mdatFirst rewrites an ftyp/meta/mdat file as ftyp/mdat/meta, padding mdat
// with pad bytes ahead of the coded data and fixing up iloc's offsets, which
// is the layout those phones write.
func mdatFirst(t *testing.T, raw []byte, pad int) []byte {
	t.Helper()
	type box struct {
		typ   string
		start int
		data  []byte
	}
	var boxes []box
	for i := 0; i+8 <= len(raw); {
		size := int(binary.BigEndian.Uint32(raw[i:]))
		boxes = append(boxes, box{string(raw[i+4 : i+8]), i, raw[i : i+size]})
		i += size
	}
	if len(boxes) != 3 || boxes[0].typ != "ftyp" || boxes[1].typ != "meta" || boxes[2].typ != "mdat" {
		t.Fatalf("fixture layout is %v, want ftyp meta mdat", boxes)
	}
	ftyp, meta, mdat := boxes[0], boxes[1], boxes[2]

	newMdat := make([]byte, 8, 8+pad+len(mdat.data)-8)
	newMdat = append(newMdat, make([]byte, pad)...)
	newMdat = append(newMdat, mdat.data[8:]...)
	binary.BigEndian.PutUint32(newMdat, uint32(len(newMdat)))
	copy(newMdat[4:], "mdat")

	// Every extent pointed into the old mdat payload; move it by the change in
	// that payload's file position.
	shift := (len(ftyp.data) + 8 + pad) - (mdat.start + 8)
	meta.data = append([]byte(nil), meta.data...)
	shiftILOC(t, meta.data, shift)

	out := append(append(append([]byte(nil), ftyp.data...), newMdat...), meta.data...)
	return out
}

// shiftILOC adds shift to every construction-method-0 offset in the iloc box
// inside meta, which is all ImageMagick's encoder writes.
func shiftILOC(t *testing.T, meta []byte, shift int) {
	t.Helper()
	i := bytes.Index(meta, []byte("iloc"))
	if i < 4 {
		t.Fatal("no iloc")
	}
	p := i + 4
	version := meta[p]
	p += 4
	offSize, lenSize := int(meta[p]>>4), int(meta[p]&15)
	baseSize, idxSize := int(meta[p+1]>>4), 0
	if version == 1 || version == 2 {
		idxSize = int(meta[p+1] & 15)
	}
	p += 2
	read := func(n int) uint64 {
		var v uint64
		for k := 0; k < n; k++ {
			v = v<<8 | uint64(meta[p+k])
		}
		return v
	}
	write := func(at, n int, v uint64) {
		for k := n - 1; k >= 0; k-- {
			meta[at+k] = byte(v)
			v >>= 8
		}
	}
	idField := 2
	if version == 2 {
		idField = 4
	}
	count := int(read(idField))
	p += idField
	for range count {
		p += idField
		method := 0
		if version == 1 || version == 2 {
			method = int(read(2) & 15)
			p += 2
		}
		p += 2 // data_reference_index
		if method == 0 && baseSize > 0 {
			write(p, baseSize, uint64(int(read(baseSize))+shift))
		}
		p += baseSize
		extents := int(read(2))
		p += 2
		for range extents {
			p += idxSize
			if method == 0 && baseSize == 0 {
				write(p, offSize, uint64(int(read(offSize))+shift))
			}
			p += offSize + lenSize
		}
	}
}

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/disintegration/imaging"
	_ "golang.org/x/image/webp" // decode support only
	_ "image/gif"               // decode support only
)

// thumbnail.go ports Synapse's thumbnail selection and generation.
//
// Two behaviours drive the whole design:
//
//   - With dynamic_thumbnails enabled, Synapse matches stored thumbnails
//     *exactly* on width, height, method and type. There is no nearest-match
//     fallback, so anything not already stored must be generated.
//
//   - The client and federation thumbnail servlets do not accept a type
//     parameter. The desired type is always image/png, or image/webp when
//     animated=true. Stored upload-time thumbnails are typically image/jpeg and
//     therefore never match a client request at all.
//
// Note also that Synapse records a *generated* thumbnail under the dimensions
// that were requested, while upload-time thumbnails are recorded under their
// post-aspect dimensions. Both populations coexist in the database. Keying the
// worker's cache by the requested dimensions is what keeps it consistent with
// the exact-match lookup.

const (
	methodScale = "scale"
	methodCrop  = "crop"

	typePNG  = "image/png"
	typeJPEG = "image/jpeg"
	typeWebP = "image/webp"
)

// ErrCannotGenerate means the worker will not produce this thumbnail and the
// request should be handed to Synapse instead.
var ErrCannotGenerate = errors.New("cannot generate thumbnail")

// ThumbnailRequest is a parsed thumbnail query.
type ThumbnailRequest struct {
	Width    int
	Height   int
	Method   string
	Animated bool
	// Type is the desired output type, derived from Animated rather than from
	// a request parameter, matching Synapse.
	Type string
}

// ParseThumbnailRequest reads the query parameters Synapse's servlets accept.
func ParseThumbnailRequest(r *http.Request) (ThumbnailRequest, error) {
	q := r.URL.Query()
	width, err := strconv.Atoi(q.Get("width"))
	if err != nil || width <= 0 {
		return ThumbnailRequest{}, fmt.Errorf("missing or invalid width")
	}
	height, err := strconv.Atoi(q.Get("height"))
	if err != nil || height <= 0 {
		return ThumbnailRequest{}, fmt.Errorf("missing or invalid height")
	}
	method := q.Get("method")
	if method == "" {
		method = methodScale
	}
	method = strings.ToLower(method)
	if method != methodScale && method != methodCrop {
		return ThumbnailRequest{}, fmt.Errorf("unknown method %q", method)
	}
	animated := q.Get("animated") == "true"
	t := typePNG
	if animated {
		t = typeWebP
	}
	return ThumbnailRequest{
		Width: width, Height: height, Method: method,
		Animated: animated, Type: t,
	}, nil
}

// FindExact returns the stored thumbnail matching the request exactly, which is
// the only kind of match Synapse makes when dynamic_thumbnails is on.
func FindExact(rows []ThumbnailRow, req ThumbnailRequest) (ThumbnailRow, bool) {
	for _, row := range rows {
		if row.Width == req.Width && row.Height == req.Height &&
			row.Method == req.Method && row.Type == req.Type {
			return row, true
		}
	}
	return ThumbnailRow{}, false
}

// aspect returns the largest size preserving aspect ratio that fits inside the
// given rectangle. This is Synapse's Thumbnailer.aspect, including its integer
// floor division, which must be reproduced exactly or dimensions differ by a
// pixel from Synapse's.
func aspect(srcW, srcH, maxW, maxH int) (int, int) {
	if maxW*srcH < maxH*srcW {
		return maxW, max(maxW*srcH/srcW, 1)
	}
	return max(maxH*srcW/srcH, 1), maxH
}

// cropBox reproduces Synapse's Thumbnailer.crop: scale so the target rectangle
// is covered, then take a centred crop.
func cropBox(srcW, srcH, w, h int) (scaledW, scaledH int, box image.Rectangle) {
	if w*srcH > h*srcW {
		scaledW = w
		scaledH = w * srcH / srcW
		top := (scaledH - h) / 2
		return scaledW, scaledH, image.Rect(0, top, w, h+top)
	}
	scaledW = h * srcW / srcH
	scaledH = h
	left := (scaledW - w) / 2
	return scaledW, scaledH, image.Rect(left, 0, w+left, h)
}

// Thumbnailer generates thumbnails from originals in Synapse's media store.
// It only ever reads from that store.
type Thumbnailer struct {
	maxImagePixels int64
	// slots bounds concurrent generation. Decoding holds the full bitmap in
	// memory -- at the default 100M pixel limit that is hundreds of megabytes
	// for a single image -- and resizing is CPU-bound, so letting every
	// request generate at once would trade a slow response for an OOM.
	slots chan struct{}
}

func NewThumbnailer(maxImagePixels int64, maxConcurrent int) *Thumbnailer {
	if maxConcurrent <= 0 {
		maxConcurrent = runtime.GOMAXPROCS(0)
	}
	return &Thumbnailer{
		maxImagePixels: maxImagePixels,
		slots:          make(chan struct{}, maxConcurrent),
	}
}

// acquire waits for a generation slot, giving up if the caller goes away so a
// disconnected client does not hold one.
func (t *Thumbnailer) acquire(ctx context.Context) error {
	select {
	case t.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (t *Thumbnailer) release() { <-t.slots }

// decodableSourceTypes mirrors Synapse's PILLOW_FORMATS: the decoders that are
// part of the trusted computing base. Anything else is refused rather than
// guessed at.
var decodableSourceTypes = map[string]struct{}{
	"image/jpeg": {}, "image/jpg": {}, "image/png": {},
	"image/webp": {}, "image/gif": {},
}

// CanGenerate reports whether the worker is able to produce this thumbnail
// itself. When false, the caller should proxy to Synapse.
func (t *Thumbnailer) CanGenerate(sourceType string, req ThumbnailRequest) bool {
	if _, ok := decodableSourceTypes[strings.ToLower(sourceType)]; !ok {
		return false
	}
	// Animated output needs a WebP encoder, which the standard library does not
	// provide. These are rare, so they are handed to Synapse rather than
	// pulling in a cgo dependency.
	if req.Animated || req.Type == typeWebP {
		return false
	}
	return req.Type == typePNG || req.Type == typeJPEG
}

// Generate produces the thumbnail bytes for req from the file at srcPath.
func (t *Thumbnailer) Generate(ctx context.Context, srcPath string, sourceType string, req ThumbnailRequest) ([]byte, error) {
	if !t.CanGenerate(sourceType, req) {
		return nil, ErrCannotGenerate
	}
	if err := t.acquire(ctx); err != nil {
		return nil, err
	}
	defer t.release()

	raw, err := os.ReadFile(srcPath)
	if err != nil {
		return nil, fmt.Errorf("reading source media: %w", err)
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		// A file that will not decode is not an error worth retrying; Synapse
		// treats it the same way and gives up on thumbnailing.
		return nil, fmt.Errorf("%w: decoding source: %v", ErrCannotGenerate, err)
	}

	img = applyOrientation(img, readJPEGOrientation(raw))

	bounds := img.Bounds()
	srcW, srcH := bounds.Dx(), bounds.Dy()
	if srcW <= 0 || srcH <= 0 {
		return nil, fmt.Errorf("%w: source has zero extent", ErrCannotGenerate)
	}
	// Synapse refuses at or above the limit, not merely above it.
	if int64(srcW)*int64(srcH) >= t.maxImagePixels {
		return nil, fmt.Errorf("%w: source is %dx%d, at or above the %d pixel limit",
			ErrCannotGenerate, srcW, srcH, t.maxImagePixels)
	}

	var out image.Image
	switch req.Method {
	case methodCrop:
		scaledW, scaledH, box := cropBox(srcW, srcH, req.Width, req.Height)
		scaled := imaging.Resize(img, scaledW, scaledH, imaging.Lanczos)
		out = imaging.Crop(scaled, box)
	case methodScale:
		w, h := aspect(srcW, srcH, req.Width, req.Height)
		// Synapse never upscales.
		w, h = min(srcW, w), min(srcH, h)
		out = imaging.Resize(img, w, h, imaging.Lanczos)
	default:
		return nil, fmt.Errorf("%w: unknown method %q", ErrCannotGenerate, req.Method)
	}

	return encodeImage(out, req.Type)
}

// encodeImage writes the thumbnail in the requested format at Synapse's
// quality setting.
func encodeImage(img image.Image, mediaType string) ([]byte, error) {
	var buf bytes.Buffer
	switch mediaType {
	case typePNG:
		enc := png.Encoder{CompressionLevel: png.DefaultCompression}
		if err := enc.Encode(&buf, img); err != nil {
			return nil, fmt.Errorf("encoding png: %w", err)
		}
	case typeJPEG:
		// Synapse passes quality=80 to Pillow for every format that accepts it.
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
			return nil, fmt.Errorf("encoding jpeg: %w", err)
		}
	default:
		return nil, fmt.Errorf("%w: no encoder for %q", ErrCannotGenerate, mediaType)
	}
	return buf.Bytes(), nil
}

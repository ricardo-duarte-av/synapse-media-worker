package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// readFromRecorder stands in for net/http's response writer, which implements
// io.ReaderFrom so bodies can be sent with sendfile.
type readFromRecorder struct {
	http.ResponseWriter
	usedReadFrom bool
	body         strings.Builder
}

func (r *readFromRecorder) Write(b []byte) (int, error) { return r.body.Write(b) }
func (r *readFromRecorder) WriteHeader(int)             {}
func (r *readFromRecorder) Header() http.Header         { return http.Header{} }

func (r *readFromRecorder) ReadFrom(src io.Reader) (int64, error) {
	r.usedReadFrom = true
	return io.Copy(&r.body, src)
}

// Wrapping the ResponseWriter for metrics must not cost the sendfile path.
// If statusRecorder stops forwarding ReadFrom, every media download quietly
// becomes a userspace copy loop and nothing else fails.
func TestStatusRecorderForwardsReadFrom(t *testing.T) {
	inner := &readFromRecorder{}
	rec := &statusRecorder{ResponseWriter: inner}

	if _, ok := any(rec).(io.ReaderFrom); !ok {
		t.Fatal("statusRecorder does not implement io.ReaderFrom")
	}

	// A bare reader, deliberately not a strings.Reader: types implementing
	// io.WriterTo short-circuit io.Copy before ReadFrom is consulted.
	const payload = "media body bytes"
	n, err := io.Copy(rec, &bareReader{s: payload})
	if err != nil {
		t.Fatal(err)
	}
	if !inner.usedReadFrom {
		t.Error("ReadFrom was not forwarded to the underlying writer")
	}
	if n != int64(len(payload)) || inner.body.String() != payload {
		t.Errorf("copied %d bytes: %q", n, inner.body.String())
	}
	if rec.bytes != int64(len(payload)) {
		t.Errorf("byte counter = %d, want %d", rec.bytes, len(payload))
	}
	if rec.status != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.status)
	}
}

// A writer with no ReadFrom must still work.
func TestStatusRecorderFallsBackToCopy(t *testing.T) {
	inner := &plainWriter{}
	rec := &statusRecorder{ResponseWriter: inner}
	const payload = "body"
	if _, err := io.Copy(rec, &bareReader{s: payload}); err != nil {
		t.Fatal(err)
	}
	if inner.body.String() != payload {
		t.Errorf("got %q", inner.body.String())
	}
}

// bareReader implements only io.Reader, so io.Copy must fall through to the
// destination's ReadFrom.
type bareReader struct {
	s string
	i int
}

func (b *bareReader) Read(p []byte) (int, error) {
	if b.i >= len(b.s) {
		return 0, io.EOF
	}
	n := copy(p, b.s[b.i:])
	b.i += n
	return n, nil
}

type plainWriter struct{ body strings.Builder }

func (p *plainWriter) Header() http.Header         { return http.Header{} }
func (p *plainWriter) Write(b []byte) (int, error) { return p.body.Write(b) }
func (p *plainWriter) WriteHeader(int)             {}

func TestStatusRecorderUnwrap(t *testing.T) {
	inner := &plainWriter{}
	rec := &statusRecorder{ResponseWriter: inner}
	if rec.Unwrap() != http.ResponseWriter(inner) {
		t.Error("Unwrap did not return the underlying writer")
	}
}

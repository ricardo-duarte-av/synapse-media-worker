package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestContentTypeHeader(t *testing.T) {
	cases := map[string]string{
		"image/png":        "image/png",
		"":                 "application/octet-stream",
		"text/plain":       "text/plain; charset=UTF-8",
		"application/json": "application/json; charset=UTF-8",
		"image/svg+xml":    "image/svg+xml; charset=UTF-8",
		// Synapse matches the whole header value, so a type that already
		// carries parameters is left untouched.
		"text/css; charset=UTF-16": "text/css; charset=UTF-16",
	}
	for in, want := range cases {
		if got := contentTypeHeader(in); got != want {
			t.Errorf("contentTypeHeader(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestContentDispositionInlineVsAttachment(t *testing.T) {
	cases := []struct {
		mediaType string
		want      string
	}{
		{"image/png", "inline"},
		{"image/jpeg", "inline"},
		{"video/mp4", "inline"},
		{"audio/flac", "inline"},
		// SVG can carry script and is deliberately not inline.
		{"image/svg+xml", "attachment"},
		{"text/html", "attachment"},
		{"application/pdf", "attachment"},
		{"application/octet-stream", "attachment"},
		{"", "attachment"},
		// Parameters must not defeat the lookup.
		{"image/png; charset=utf-8", "inline"},
		{"IMAGE/PNG", "inline"},
	}
	for _, c := range cases {
		if got := contentDisposition(c.mediaType, ""); got != c.want {
			t.Errorf("contentDisposition(%q) = %q, want %q", c.mediaType, got, c.want)
		}
	}
}

func TestContentDispositionFilenameEncoding(t *testing.T) {
	// A plain ASCII name goes out bare and unquoted.
	if got := contentDisposition("image/png", "cat.png"); got != "inline; filename=cat.png" {
		t.Errorf("token filename: got %q", got)
	}
	// A space is not a valid token character, so it forces the extended form.
	if got := contentDisposition("image/png", "my cat.png"); got != "inline; filename*=utf-8''my%20cat.png" {
		t.Errorf("spaced filename: got %q", got)
	}
	// Non-ASCII forces the extended form and is UTF-8 percent-encoded.
	if got := contentDisposition("image/png", "æther.png"); got != "inline; filename*=utf-8''%C3%A6ther.png" {
		t.Errorf("unicode filename: got %q", got)
	}
	// Synapse emits one form or the other, never both.
	got := contentDisposition("image/png", "my cat.png")
	if hasBoth := containsBoth(got, "filename=", "filename*="); hasBoth {
		t.Errorf("emitted both filename forms: %q", got)
	}
	// No upload_name at all means no filename parameter.
	if got := contentDisposition("application/pdf", ""); got != "attachment" {
		t.Errorf("empty filename: got %q", got)
	}
}

func containsBoth(s, a, b string) bool {
	ai, bi := indexOf(s, a), indexOf(s, b)
	return ai >= 0 && bi >= 0 && ai != bi
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			// "filename=" also matches inside "filename*=" only if the
			// characters line up, which they do not, so a plain scan is fine.
			return i
		}
	}
	return -1
}

// A crafted upload_name must not be able to inject extra response headers.
func TestHeaderInjectionIsStripped(t *testing.T) {
	evil := "a\r\nX-Injected: yes\r\n"
	got := sanitizeHeaderValue(contentDisposition("image/png", evil))
	if containsBoth(got, "\r", "\n") || indexOf(got, "\n") >= 0 || indexOf(got, "\r") >= 0 {
		t.Errorf("CRLF survived sanitisation: %q", got)
	}
	if indexOf(got, "X-Injected") >= 0 && indexOf(got, "\n") >= 0 {
		t.Errorf("header injection possible: %q", got)
	}
}

func TestNotModifiedUsesSynapseConstantETag(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("If-None-Match", "1")
	if !notModified(r) {
		t.Error("If-None-Match: 1 should be a match")
	}
	// Synapse's ETag is unquoted, so the quoted form is not what clients send back.
	r.Header.Set("If-None-Match", `"1"`)
	if notModified(r) {
		t.Error(`If-None-Match: "1" should not match Synapse's unquoted ETag`)
	}
	r.Header.Del("If-None-Match")
	if notModified(r) {
		t.Error("absent If-None-Match should not match")
	}
}

func TestETagIsUnquotedConstant(t *testing.T) {
	w := httptest.NewRecorder()
	addFileHeaders(w, "image/png", "cat.png")
	if got := w.Header().Get("ETag"); got != "1" {
		t.Errorf("ETag = %q, want 1", got)
	}
	if got := w.Header().Get("Cache-Control"); got != cacheControlValue {
		t.Errorf("Cache-Control = %q", got)
	}
	if got := w.Header().Get("X-Robots-Tag"); got == "" {
		t.Error("X-Robots-Tag not set")
	}
}

// Range support is a deliberate superset of Synapse, which does not implement
// it at all. Clients seeking within a video depend on it.
func TestServeFileSupportsRange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "media")
	payload := make([]byte, 4096)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	open := func() *os.File {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}

	// A whole-file request still returns 200.
	r := httptest.NewRequest("GET", "/media", nil)
	w := httptest.NewRecorder()
	f := open()
	addFileHeaders(w, "video/mp4", "clip.mp4")
	serveFile(w, r, f)
	_ = f.Close()
	if w.Code != http.StatusOK {
		t.Fatalf("whole file: status %d", w.Code)
	}
	if !bytes.Equal(w.Body.Bytes(), payload) {
		t.Error("whole file body differs")
	}

	// A ranged request returns 206 and just that slice.
	r = httptest.NewRequest("GET", "/media", nil)
	r.Header.Set("Range", "bytes=0-1023")
	w = httptest.NewRecorder()
	f = open()
	addFileHeaders(w, "video/mp4", "clip.mp4")
	serveFile(w, r, f)
	_ = f.Close()
	if w.Code != http.StatusPartialContent {
		t.Fatalf("ranged: status %d, want 206", w.Code)
	}
	if got := w.Header().Get("Content-Range"); got != "bytes 0-1023/4096" {
		t.Errorf("Content-Range = %q", got)
	}
	head := w.Body.Bytes()
	if len(head) != 1024 || !bytes.Equal(head, payload[:1024]) {
		t.Errorf("ranged body is %d bytes and does not match", len(head))
	}

	// The two halves must reassemble into the original.
	r = httptest.NewRequest("GET", "/media", nil)
	r.Header.Set("Range", "bytes=1024-")
	w = httptest.NewRecorder()
	f = open()
	serveFile(w, r, f)
	_ = f.Close()
	if w.Code != http.StatusPartialContent {
		t.Fatalf("tail: status %d", w.Code)
	}
	if !bytes.Equal(append(head, w.Body.Bytes()...), payload) {
		t.Error("reassembled body differs from the original")
	}
}

// Synapse sends no Last-Modified, and neither should the worker: the constant
// ETag is the only validator, and a Last-Modified would invite
// If-Modified-Since requests Synapse would answer differently.
func TestServeFileSendsNoLastModified(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "media")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	w := httptest.NewRecorder()
	addFileHeaders(w, "text/plain", "a.txt")
	serveFile(w, httptest.NewRequest("GET", "/media", nil), f)
	if got := w.Header().Get("Last-Modified"); got != "" {
		t.Errorf("Last-Modified = %q, want none", got)
	}
}

func TestRespondNotModified(t *testing.T) {
	w := httptest.NewRecorder()
	respondNotModified(w)
	if w.Code != http.StatusNotModified {
		t.Errorf("status = %d", w.Code)
	}
	// A 304 must still carry the cache headers so the client keeps its copy.
	if w.Header().Get("ETag") != "1" {
		t.Error("304 lacks ETag")
	}
	if w.Header().Get("Cache-Control") == "" {
		t.Error("304 lacks Cache-Control")
	}
}

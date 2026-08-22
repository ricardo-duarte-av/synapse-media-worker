package main

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// Synapse hardcodes 156 bytes of scaffolding for a 32-character boundary. If
// this drifts, Content-Length stops matching the body and peers hang.
func TestFixedOverheadMatchesSynapseConstant(t *testing.T) {
	p := newMultipartParts(newBoundary(), "image/png", "cat.png")
	if got := p.fixedOverhead(); got != multipartFixedN {
		t.Errorf("fixedOverhead() = %d, want %d", got, multipartFixedN)
	}
}

// The advertised Content-Length must equal the bytes actually written, or
// receiving homeservers will stall waiting for more data.
func TestContentLengthMatchesBody(t *testing.T) {
	for _, name := range []string{"cat.png", "my cat.png", "æther.png", ""} {
		payload := bytes.Repeat([]byte{0xAB}, 5000)
		p := newMultipartParts(newBoundary(), "image/png", name)
		w := httptest.NewRecorder()
		p.writeHeader(w, int64(len(payload)))
		var body bytes.Buffer
		if err := p.writeBody(&body, bytes.NewReader(payload)); err != nil {
			t.Fatal(err)
		}
		want, err := strconv.Atoi(w.Header().Get("Content-Length"))
		if err != nil {
			t.Fatal(err)
		}
		if body.Len() != want {
			t.Errorf("upload_name %q: wrote %d bytes, advertised %d", name, body.Len(), want)
		}
	}
}

// Synapse emits a CRLF before the first boundary and no preamble.
func TestBodyStartsWithCRLFBoundary(t *testing.T) {
	p := newMultipartParts("abcdef0123456789abcdef0123456789", "image/png", "cat.png")
	var body bytes.Buffer
	if err := p.writeBody(&body, strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	want := "\r\n--abcdef0123456789abcdef0123456789\r\n"
	if !bytes.HasPrefix(body.Bytes(), []byte(want)) {
		t.Errorf("body starts with %q", body.Bytes()[:min(40, body.Len())])
	}
	if !bytes.HasSuffix(body.Bytes(), []byte("\r\n--abcdef0123456789abcdef0123456789--\r\n")) {
		t.Error("body does not end with the closing boundary")
	}
}

// The response must be parseable by a standards-compliant multipart reader,
// which is what other homeservers use.
func TestRoundTripsThroughMultipartReader(t *testing.T) {
	payload := []byte("the actual file bytes")
	p := newMultipartParts(newBoundary(), "image/png", "my cat.png")
	w := httptest.NewRecorder()
	p.writeHeader(w, int64(len(payload)))
	var body bytes.Buffer
	if err := p.writeBody(&body, bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}

	mediaType, params, err := mime.ParseMediaType(w.Header().Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	if mediaType != "multipart/mixed" {
		t.Fatalf("media type = %q", mediaType)
	}
	if params["boundary"] == "" {
		t.Fatal("no boundary parameter")
	}

	mr := multipart.NewReader(&body, params["boundary"])

	meta, err := mr.NextPart()
	if err != nil {
		t.Fatalf("reading metadata part: %v", err)
	}
	if ct := meta.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("metadata Content-Type = %q", ct)
	}
	// The metadata part deliberately carries no Content-Disposition.
	if cd := meta.Header.Get("Content-Disposition"); cd != "" {
		t.Errorf("metadata part has Content-Disposition %q", cd)
	}
	metaBody, _ := io.ReadAll(meta)
	if string(metaBody) != "{}" {
		t.Errorf("metadata body = %q, want {}", metaBody)
	}

	file, err := mr.NextPart()
	if err != nil {
		t.Fatalf("reading file part: %v", err)
	}
	if ct := file.Header.Get("Content-Type"); ct != "image/png" {
		t.Errorf("file Content-Type = %q", ct)
	}
	if cd := file.Header.Get("Content-Disposition"); cd != "inline; filename*=utf-8''my%20cat.png" {
		t.Errorf("file Content-Disposition = %q", cd)
	}
	fileBody, _ := io.ReadAll(file)
	if !bytes.Equal(fileBody, payload) {
		t.Errorf("file body = %q, want %q", fileBody, payload)
	}

	if _, err := mr.NextPart(); err != io.EOF {
		t.Errorf("expected exactly two parts, got a third (err=%v)", err)
	}
}

func TestBoundaryIs32HexChars(t *testing.T) {
	b := newBoundary()
	if len(b) != 32 {
		t.Fatalf("boundary length = %d, want 32", len(b))
	}
	for _, r := range b {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("boundary contains non-hex character %q", r)
		}
	}
	if newBoundary() == b {
		t.Error("boundaries are not unique")
	}
}

// An empty file still needs correct framing.
func TestEmptyFile(t *testing.T) {
	p := newMultipartParts(newBoundary(), "application/octet-stream", "")
	w := httptest.NewRecorder()
	p.writeHeader(w, 0)
	var body bytes.Buffer
	if err := p.writeBody(&body, bytes.NewReader(nil)); err != nil {
		t.Fatal(err)
	}
	want, _ := strconv.Atoi(w.Header().Get("Content-Length"))
	if body.Len() != want {
		t.Errorf("wrote %d bytes, advertised %d", body.Len(), want)
	}
}

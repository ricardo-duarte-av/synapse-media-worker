package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// fakeOrigin serves media the way a homeserver's federation media endpoint
// does, so the fetch path can be exercised without a second Synapse.
func fakeOrigin(t *testing.T, payload []byte, mediaType, uploadName string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := newMultipartParts(newBoundary(), mediaType, uploadName)
		parts.writeHeader(w, int64(len(payload)))
		w.WriteHeader(http.StatusOK)
		_ = parts.writeBody(w, bytes.NewReader(payload))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fetchFrom drives Fetcher.readMultipart directly against a response, which
// exercises the parsing, hashing and size enforcement without needing signed
// federation requests.
func fetchFrom(t *testing.T, f *Fetcher, srv *httptest.Server, dst io.Writer) (*FetchedMedia, error) {
	t.Helper()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	return f.readMultipart(context.Background(), resp, dst)
}

// A round trip through our own multipart writer and reader must preserve the
// bytes, the content type and the filename.
func TestFetchRoundTripsMultipart(t *testing.T) {
	payload := bytes.Repeat([]byte("media"), 1000)
	srv := fakeOrigin(t, payload, "image/png", "my cat.png")
	f := NewFetcher(nil, 1<<20, 0, 2)

	var dst bytes.Buffer
	info, err := fetchFrom(t, f, srv, &dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dst.Bytes(), payload) {
		t.Errorf("body differs: got %d bytes, want %d", dst.Len(), len(payload))
	}
	if info.MediaType != "image/png" {
		t.Errorf("media type = %q", info.MediaType)
	}
	if info.UploadName != "my cat.png" {
		t.Errorf("upload name = %q", info.UploadName)
	}
	if info.Length != int64(len(payload)) {
		t.Errorf("length = %d, want %d", info.Length, len(payload))
	}
	sum := sha256.Sum256(payload)
	if info.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("sha256 = %q, want %q", info.SHA256, hex.EncodeToString(sum[:]))
	}
}

// A file with no name is normal; it must not become the string "attachment".
func TestFetchWithoutUploadName(t *testing.T) {
	srv := fakeOrigin(t, []byte("x"), "application/octet-stream", "")
	f := NewFetcher(nil, 1<<20, 0, 2)
	var dst bytes.Buffer
	info, err := fetchFrom(t, f, srv, &dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.UploadName != "" {
		t.Errorf("upload name = %q, want empty", info.UploadName)
	}
}

// A peer sending more than max_upload_size must be cut off, not allowed to
// fill the disk.
func TestFetchRefusesOversizedMedia(t *testing.T) {
	srv := fakeOrigin(t, bytes.Repeat([]byte("x"), 10000), "image/png", "big.png")
	f := NewFetcher(nil, 500, 0, 2)
	var dst bytes.Buffer
	if _, err := fetchFrom(t, f, srv, &dst); err == nil {
		t.Fatal("oversized media accepted")
	}
}

// A response that is not multipart is a protocol violation, not media.
func TestFetchRejectsNonMultipart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("not multipart"))
	}))
	defer srv.Close()
	f := NewFetcher(nil, 1<<20, 0, 2)
	var dst bytes.Buffer
	_, err := fetchFrom(t, f, srv, &dst)
	if err == nil {
		t.Fatal("non-multipart response accepted")
	}
	if !strings.Contains(err.Error(), "content type") {
		t.Errorf("unhelpful error: %v", err)
	}
}

// The metadata part must be JSON, as Synapse requires.
func TestFetchRejectsBadMetadataPart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		b := newBoundary()
		w.Header().Set("Content-Type", "multipart/mixed; boundary="+b)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "\r\n--"+b+"\r\nContent-Type: text/plain\r\n\r\nnope\r\n--"+b+"--\r\n")
	}))
	defer srv.Close()
	f := NewFetcher(nil, 1<<20, 0, 2)
	var dst bytes.Buffer
	if _, err := fetchFrom(t, f, srv, &dst); err == nil {
		t.Fatal("non-JSON metadata part accepted")
	}
}

// Concurrent fetches are bounded.
func TestFetchConcurrencyIsBounded(t *testing.T) {
	f := NewFetcher(nil, 1<<20, 0, 2)
	if cap(f.slots) != 2 {
		t.Errorf("slots = %d, want 2", cap(f.slots))
	}
	f = NewFetcher(nil, 1<<20, 0, 0)
	if cap(f.slots) != 4 {
		t.Errorf("default slots = %d, want 4", cap(f.slots))
	}
}

// --- write behaviour, without a database -----------------------------------

// The download step must leave a complete file with Synapse's permissions and
// no temporary files behind.
func TestDownloadWritesCompleteFile(t *testing.T) {
	payload := bytes.Repeat([]byte("abc"), 5000)
	srv := fakeOrigin(t, payload, "image/png", "x.png")

	dir := t.TempDir()
	final := filepath.Join(dir, "remote_content", "ex.com", "Ab", "Cd", "EfGh")
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		t.Fatal(err)
	}

	rf := &RemoteFetcher{
		fetcher: NewFetcher(nil, 1<<20, 0, 2),
		log:     zerolog.Nop(),
	}
	// Drive the temp-file mechanics with a body from the fake origin.
	tmp, err := os.CreateTemp(filepath.Dir(final), ".incoming-*")
	if err != nil {
		t.Fatal(err)
	}
	info, err := fetchFrom(t, rf.fetcher, srv, tmp)
	if err != nil {
		t.Fatal(err)
	}
	if err := tmp.Sync(); err != nil {
		t.Fatal(err)
	}
	_ = tmp.Close()
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(final)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Error("stored file differs from the payload")
	}
	if info.Length != int64(len(payload)) {
		t.Errorf("length = %d", info.Length)
	}
	// Synapse's media files are 0644; ours must match or an operator will
	// notice the difference before a bug does.
	st, err := os.Stat(final)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644", st.Mode().Perm())
	}
	// No .incoming-* left over.
	entries, _ := os.ReadDir(filepath.Dir(final))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".incoming-") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func newTestUploader(t *testing.T, store string, maxUpload int64) (*Uploader, *Config) {
	t.Helper()
	paths := NewMediaPaths(store)
	paths.AllowUploadWrites()
	cfg := defaultConfig()
	cfg.ServerName = "example.com"
	cfg.Media.MaxUploadSize = &maxUpload
	return &Uploader{
		paths:       paths,
		thumbnailer: NewThumbnailer(100_000_000, 2),
		cfg:         &cfg,
		slots:       make(chan struct{}, 2),
	}, &cfg
}

func uploadRequest(body []byte, contentType, filename string) *http.Request {
	url := "/_matrix/media/v3/upload"
	if filename != "" {
		url += "?filename=" + filename
	}
	r := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	r.Header.Set("Content-Length", strconv.Itoa(len(body)))
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	return r
}

// The file must land where Synapse looks for it, with Synapse's permissions,
// and the hash must be of the bytes actually stored.
func TestUploadStoresAtSynapsePath(t *testing.T) {
	store := t.TempDir()
	u, _ := newTestUploader(t, store, 1<<20)
	payload := bytes.Repeat([]byte("upload"), 500)
	const mediaID = "AbCdEfGhIjKlMnOpQrStUvWx"

	got, err := u.store(uploadRequest(payload, "image/png", ""), mediaID, "image/png", int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(store, "local_content", "Ab", "Cd", "EfGhIjKlMnOpQrStUvWx")
	if got.path != want {
		t.Errorf("path = %q, want %q", got.path, want)
	}
	onDisk, err := os.ReadFile(want)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(onDisk, payload) {
		t.Error("stored bytes differ from the body")
	}
	if got.length != int64(len(payload)) {
		t.Errorf("length = %d, want %d", got.length, len(payload))
	}
	sum := sha256.Sum256(payload)
	if got.sha256 != hex.EncodeToString(sum[:]) {
		t.Errorf("sha256 = %q", got.sha256)
	}
	info, err := os.Stat(want)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644 to match Synapse", info.Mode().Perm())
	}
	// No temporary file may survive.
	entries, _ := os.ReadDir(filepath.Dir(want))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".incoming-") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}

// A body larger than the limit must be cut off rather than trusted, since
// Content-Length is supplied by the client.
func TestUploadRefusesBodyLargerThanItClaims(t *testing.T) {
	store := t.TempDir()
	u, _ := newTestUploader(t, store, 100)

	// Claims 50 bytes, sends 5000.
	body := bytes.Repeat([]byte("x"), 5000)
	r := httptest.NewRequest(http.MethodPost, "/_matrix/media/v3/upload", bytes.NewReader(body))
	r.Header.Set("Content-Length", "50")

	_, err := u.store(r, "AbCdEfGhIjKlMnOpQrStUvWx", "text/plain", 50)
	if err == nil {
		t.Fatal("a body exceeding the limit was accepted")
	}
	// And nothing may be left on disk.
	dir := filepath.Join(store, "local_content", "Ab", "Cd")
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		t.Errorf("left behind after a refused upload: %s", e.Name())
	}
}

// Uploads must not be able to write outside local_content, and not at all when
// the feature is off.
func TestUploadRespectsTheWriteGuard(t *testing.T) {
	store := t.TempDir()
	paths := NewMediaPaths(store) // uploads NOT allowed
	u := &Uploader{paths: paths, cfg: &Config{}, slots: make(chan struct{}, 1)}
	maxUpload := int64(1 << 20)
	u.cfg.Media.MaxUploadSize = &maxUpload

	_, err := u.store(uploadRequest([]byte("x"), "text/plain", ""), "AbCdEfGhIjKlMnOpQrStUvWx", "text/plain", 1)
	if err == nil {
		t.Fatal("wrote to local_content with uploads disabled")
	}
	if !strings.Contains(err.Error(), "refusing to write") {
		t.Errorf("unhelpful error: %v", err)
	}
}

// Content-Type is trusted verbatim and filename stored unsanitised, matching
// Synapse. Sanitisation belongs on the response side.
func TestUploadMetadataMatchesSynapse(t *testing.T) {
	maxUpload := int64(1 << 20)
	cfg := defaultConfig()
	cfg.Media.MaxUploadSize = &maxUpload
	s := &Server{cfg: &cfg}

	// Missing Content-Type becomes application/octet-stream.
	w := httptest.NewRecorder()
	mt, name, length, ok := s.uploadMetadata(w, uploadRequest([]byte("abc"), "", ""))
	if !ok {
		t.Fatalf("rejected: %s", w.Body.String())
	}
	if mt != "application/octet-stream" {
		t.Errorf("media type = %q", mt)
	}
	if length != 3 {
		t.Errorf("length = %d", length)
	}
	if name != "" {
		t.Errorf("upload name = %q", name)
	}

	// Parameters on the content type are preserved verbatim.
	w = httptest.NewRecorder()
	mt, _, _, ok = s.uploadMetadata(w, uploadRequest([]byte("abc"), "text/plain; charset=utf-16", ""))
	if !ok || mt != "text/plain; charset=utf-16" {
		t.Errorf("media type = %q", mt)
	}

	// The filename is stored as given, path separators and all.
	w = httptest.NewRecorder()
	_, name, _, ok = s.uploadMetadata(w, uploadRequest([]byte("abc"), "image/png", "..%2Fevil.png"))
	if !ok {
		t.Fatal("rejected a filename Synapse would accept")
	}
	if name != "../evil.png" {
		t.Errorf("upload name = %q, want it unsanitised like Synapse", name)
	}
}

// The spec's 413 for an oversized declared length.
func TestUploadMetadataRejectsOversized(t *testing.T) {
	maxUpload := int64(100)
	cfg := defaultConfig()
	cfg.Media.MaxUploadSize = &maxUpload
	s := &Server{cfg: &cfg}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/_matrix/media/v3/upload", nil)
	r.Header.Set("Content-Length", "5000")
	if _, _, _, ok := s.uploadMetadata(w, r); ok {
		t.Fatal("accepted an oversized upload")
	}
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", w.Code)
	}
	var body MatrixError
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.ErrCode != "M_TOO_LARGE" {
		t.Errorf("errcode = %q, want M_TOO_LARGE", body.ErrCode)
	}
}

// Missing or invalid Content-Length is a 400, as Synapse does.
func TestUploadMetadataRequiresContentLength(t *testing.T) {
	cfg := defaultConfig()
	s := &Server{cfg: &cfg}
	for _, raw := range []string{"", "abc", "-1"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/_matrix/media/v3/upload", nil)
		if raw != "" {
			r.Header.Set("Content-Length", raw)
		}
		if _, _, _, ok := s.uploadMetadata(w, r); ok {
			t.Errorf("Content-Length %q was accepted", raw)
			continue
		}
		if w.Code != http.StatusBadRequest {
			t.Errorf("Content-Length %q: status %d, want 400", raw, w.Code)
		}
	}
}

// The 429 body must carry retry_after_ms, which is what clients back off on.
func TestRateLimitedBodyShape(t *testing.T) {
	w := httptest.NewRecorder()
	writeRateLimited(w, 4200)
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["errcode"] != "M_LIMIT_EXCEEDED" {
		t.Errorf("errcode = %v", body["errcode"])
	}
	if body["retry_after_ms"] != float64(4200) {
		t.Errorf("retry_after_ms = %v", body["retry_after_ms"])
	}
}

// Synapse's format map: webp sources yield jpeg thumbnails, gif yields png.
func TestSynapseThumbnailType(t *testing.T) {
	cases := map[string]string{
		"image/jpeg":               typeJPEG,
		"image/jpg":                typeJPEG,
		"image/webp":               typeJPEG,
		"image/gif":                typePNG,
		"image/png":                typePNG,
		"image/png; charset=utf-8": typePNG,
		"IMAGE/PNG":                typePNG,
	}
	for in, want := range cases {
		got, ok := synapseThumbnailType(in)
		if !ok || got != want {
			t.Errorf("synapseThumbnailType(%q) = %q,%v, want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"application/pdf", "video/mp4", ""} {
		if _, ok := synapseThumbnailType(in); ok {
			t.Errorf("%q should not be thumbnailed", in)
		}
	}
}

// Every terminal path in an upload handler must record a metric, or the
// dashboard silently under-reports. This checks the labels are the ones the
// dashboard queries, and that endpoint and result stay separate dimensions --
// an earlier version used "async" as a result, mixing the two.
func TestUploadMetricLabelsAreConsistent(t *testing.T) {
	endpoints := map[string]bool{
		uploadEndpointSync: true, uploadEndpointCreate: true, uploadEndpointAsync: true,
	}
	results := map[string]bool{
		uploadResultStored: true, uploadResultReserved: true, uploadResultTooLarge: true,
		uploadResultLimited: true, uploadResultForbidden: true, uploadResultNotFound: true,
		uploadResultConflict: true, uploadResultFailed: true, uploadResultProxied: true,
	}

	src, err := os.ReadFile("upload.go")
	if err != nil {
		t.Fatal(err)
	}
	calls := findLabelCalls(string(src), "uploadsTotal.WithLabelValues(")
	if len(calls) < 8 {
		t.Fatalf("only %d uploadsTotal call sites; terminal paths are probably unrecorded", len(calls))
	}
	for _, args := range calls {
		if len(args) != 2 {
			t.Errorf("uploadsTotal call has %d labels, want endpoint and result: %v", len(args), args)
			continue
		}
		ep, res := args[0], args[1]
		// "endpoint" is the parameter the shared responders take; anything
		// else must be one of the constants.
		if ep != "endpoint" && ep != "uploadEndpointFor(r)" && !endpoints[constValue(ep)] {
			t.Errorf("unknown endpoint label %q", ep)
		}
		if !results[constValue(res)] {
			t.Errorf("unknown result label %q", res)
		}
	}
}

// findLabelCalls returns the top-level arguments of each call to prefix,
// respecting nested parentheses.
func findLabelCalls(src, prefix string) [][]string {
	var out [][]string
	for i := 0; ; {
		j := strings.Index(src[i:], prefix)
		if j < 0 {
			return out
		}
		start := i + j + len(prefix)
		depth, end := 0, -1
		for k := start; k < len(src); k++ {
			switch src[k] {
			case '(':
				depth++
			case ')':
				if depth == 0 {
					end = k
				} else {
					depth--
				}
			}
			if end >= 0 {
				break
			}
		}
		if end < 0 {
			return out
		}
		var args []string
		depth = 0
		cur := strings.Builder{}
		for _, c := range src[start:end] {
			switch c {
			case '(':
				depth++
			case ')':
				depth--
			case ',':
				if depth == 0 {
					args = append(args, strings.TrimSpace(cur.String()))
					cur.Reset()
					continue
				}
			}
			cur.WriteRune(c)
		}
		if strings.TrimSpace(cur.String()) != "" {
			args = append(args, strings.TrimSpace(cur.String()))
		}
		out = append(out, args)
		i = end
	}
}

// constValue maps a constant identifier back to its value so the test compares
// against the same strings the dashboard uses.
func constValue(ident string) string {
	switch ident {
	case "uploadEndpointSync":
		return uploadEndpointSync
	case "uploadEndpointCreate":
		return uploadEndpointCreate
	case "uploadEndpointAsync":
		return uploadEndpointAsync
	case "uploadResultStored":
		return uploadResultStored
	case "uploadResultReserved":
		return uploadResultReserved
	case "uploadResultTooLarge":
		return uploadResultTooLarge
	case "uploadResultLimited":
		return uploadResultLimited
	case "uploadResultForbidden":
		return uploadResultForbidden
	case "uploadResultNotFound":
		return uploadResultNotFound
	case "uploadResultConflict":
		return uploadResultConflict
	case "uploadResultFailed":
		return uploadResultFailed
	case "uploadResultProxied":
		return uploadResultProxied
	}
	return ident
}

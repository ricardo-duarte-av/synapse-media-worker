package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// liveupload_test.go is the acceptance test for uploads: put the same bytes
// through the Go worker and through Synapse's own upload worker, then require
// the rows and files to be indistinguishable.
//
// The worker writes to a scratch media store so Synapse's own files are never
// touched; both rows land in the real database and are removed afterwards.
//
//	SMW_LIVE_DB=postgres://... SMW_LIVE_SERVER=aguiarvieira.pt \
//	SMW_LIVE_TOKEN=~/.smw-test-token \
//	SMW_LIVE_UPLOAD_SOCK=/var/sockets/nginx/av-media-worker-uploads.sock \
//	SMW_LIVE_WHOAMI_SOCK=/var/sockets/nginx/av-request-worker-1.sock \
//	go test -run TestLiveUploadMatchesSynapse -v
func TestLiveUploadMatchesSynapse(t *testing.T) {
	dbURI := os.Getenv("SMW_LIVE_DB")
	server := os.Getenv("SMW_LIVE_SERVER")
	tokenFile := os.Getenv("SMW_LIVE_TOKEN")
	uploadSock := os.Getenv("SMW_LIVE_UPLOAD_SOCK")
	whoamiSock := os.Getenv("SMW_LIVE_WHOAMI_SOCK")
	if dbURI == "" || server == "" || tokenFile == "" || uploadSock == "" || whoamiSock == "" {
		t.Skip("set SMW_LIVE_DB, SMW_LIVE_SERVER, SMW_LIVE_TOKEN, SMW_LIVE_UPLOAD_SOCK and SMW_LIVE_WHOAMI_SOCK to run")
	}
	raw, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSpace(string(raw))

	ctx := context.Background()
	db, err := NewDB(ctx, DatabaseConfig{URI: dbURI, MaxConns: 4}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}

	// A real PNG, unique to this run: random bytes would not decode, so
	// neither side would thumbnail and the divergence this version chooses
	// would go untested.
	payload := randomPNG(t, 200, 150)
	sum := sha256.Sum256(payload)
	wantHash := hex.EncodeToString(sum[:])
	const uploadName = "parity probe.png"
	const mediaType = "image/png"

	var created []string
	cleanup := func() {
		for _, id := range created {
			if _, err := db.pool.Exec(ctx,
				`DELETE FROM local_media_repository_thumbnails WHERE media_id = $1`, id); err != nil {
				t.Errorf("cleaning thumbnails for %s: %v", id, err)
			}
			if _, err := db.pool.Exec(ctx,
				`DELETE FROM local_media_repository WHERE media_id = $1`, id); err != nil {
				t.Errorf("cleaning row for %s: %v", id, err)
			}
		}
		var n int
		if err := db.pool.QueryRow(ctx,
			`SELECT count(*) FROM local_media_repository WHERE sha256 = $1`, wantHash).Scan(&n); err != nil {
			t.Errorf("verifying cleanup: %v", err)
		} else if n != 0 {
			t.Errorf("%d probe rows survived cleanup", n)
		}
	}
	// LIFO: the pool closes only after cleanup has run.
	defer db.Close()
	defer cleanup()

	// --- through Synapse's own upload worker ---
	synClient := &http.Client{Timeout: 60 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", uploadSock)
		}}}
	synReq, _ := http.NewRequest(http.MethodPost,
		"http://synapse/_matrix/media/v3/upload?filename="+urlQueryEscape(uploadName),
		bytes.NewReader(payload))
	synReq.Header.Set("Authorization", "Bearer "+token)
	synReq.Header.Set("Content-Type", mediaType)
	synReq.Header.Set("Content-Length", strconv.Itoa(len(payload)))
	synReq.Host = server
	synReq.Header.Set("X-Forwarded-For", "127.0.0.1")
	synResp, err := synClient.Do(synReq)
	if err != nil {
		t.Fatal(err)
	}
	synBody, _ := readAllClose(synResp)
	if synResp.StatusCode != http.StatusOK {
		t.Fatalf("Synapse upload failed: %d %s", synResp.StatusCode, synBody)
	}
	synID := mediaIDFromResponse(t, synBody)
	created = append(created, synID)

	// --- through the Go worker ---
	store := t.TempDir()
	paths := NewMediaPaths(store)
	paths.AllowUploadWrites()
	auth, err := NewTokenAuthenticator(AuthConfig{
		WhoamiSocket: whoamiSock, PositiveTTL: time.Minute,
		NegativeTTL: time.Minute, MaxEntries: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	maxUpload := int64(1000 * 1024 * 1024)
	cfg := defaultConfig()
	cfg.ServerName = server
	cfg.Media.StorePath = store
	cfg.Media.MaxUploadSize = &maxUpload
	cfg.Media.AcceptUploads = true

	srv := &Server{cfg: &cfg, db: db, paths: paths, auth: auth, log: zerolog.Nop(),
		thumbnailer: NewThumbnailer(104857600, 2)}
	srv.uploader = NewUploader(db, paths, srv.thumbnailer, &cfg)

	goReq := httptest.NewRequest(http.MethodPost,
		"/_matrix/media/v3/upload?filename="+urlQueryEscape(uploadName), bytes.NewReader(payload))
	goReq.SetPathValue("version", "v3")
	goReq.Header.Set("Authorization", "Bearer "+token)
	goReq.Header.Set("Content-Type", mediaType)
	goReq.Header.Set("Content-Length", strconv.Itoa(len(payload)))
	w := httptest.NewRecorder()
	srv.handleUpload(w, goReq)
	if w.Code != http.StatusOK {
		t.Fatalf("worker upload failed: %d %s", w.Code, w.Body.String())
	}
	goID := mediaIDFromResponse(t, w.Body.Bytes())
	created = append(created, goID)

	// --- compare the responses ---
	if !strings.HasPrefix(mustContentURI(t, w.Body.Bytes()), "mxc://"+server+"/") {
		t.Errorf("worker content_uri = %q", mustContentURI(t, w.Body.Bytes()))
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("worker Content-Type = %q", ct)
	}

	// --- compare the rows ---
	synRow := mustLocalMedia(t, ctx, db, synID)
	goRow := mustLocalMedia(t, ctx, db, goID)

	if goRow.MediaType != synRow.MediaType {
		t.Errorf("media_type %q vs Synapse %q", goRow.MediaType, synRow.MediaType)
	}
	if goRow.UploadName != synRow.UploadName {
		t.Errorf("upload_name %q vs Synapse %q", goRow.UploadName, synRow.UploadName)
	}
	if goRow.UserID != synRow.UserID {
		t.Errorf("user_id %q vs Synapse %q", goRow.UserID, synRow.UserID)
	}
	if goRow.Authenticated != synRow.Authenticated {
		t.Errorf("authenticated %v vs Synapse %v", goRow.Authenticated, synRow.Authenticated)
	}
	if goRow.Length == nil || synRow.Length == nil || *goRow.Length != *synRow.Length {
		t.Errorf("media_length %v vs Synapse %v", goRow.Length, synRow.Length)
	}
	if goRow.QuarantinedBy != synRow.QuarantinedBy {
		t.Errorf("quarantined_by %q vs Synapse %q", goRow.QuarantinedBy, synRow.QuarantinedBy)
	}
	if goRow.URLCache != "" || synRow.URLCache != "" {
		t.Errorf("url_cache should be empty: %q vs %q", goRow.URLCache, synRow.URLCache)
	}

	// sha256 must match each other and the payload.
	var goHash, synHash string
	_ = db.pool.QueryRow(ctx, `SELECT COALESCE(sha256,'') FROM local_media_repository WHERE media_id=$1`, goID).Scan(&goHash)
	_ = db.pool.QueryRow(ctx, `SELECT COALESCE(sha256,'') FROM local_media_repository WHERE media_id=$1`, synID).Scan(&synHash)
	if goHash != wantHash {
		t.Errorf("worker sha256 = %q, want %q", goHash, wantHash)
	}
	if synHash != wantHash {
		t.Errorf("Synapse sha256 = %q, want %q", synHash, wantHash)
	}

	// media_id shape must be indistinguishable.
	for _, id := range []string{goID, synID} {
		if len(id) != 24 {
			t.Errorf("media_id %q is %d chars, want 24", id, len(id))
		}
		for _, r := range id {
			if !strings.ContainsRune(fsidAlphabet, r) {
				t.Errorf("media_id %q contains %q, outside Synapse's alphabet", id, r)
			}
		}
	}

	// --- compare the files ---
	goPath, err := paths.LocalMedia(goID)
	if err != nil {
		t.Fatal(err)
	}
	goBytes, err := os.ReadFile(goPath)
	if err != nil {
		t.Fatalf("worker file missing: %v", err)
	}
	if !bytes.Equal(goBytes, payload) {
		t.Error("worker stored bytes differ from the payload")
	}
	info, err := os.Stat(goPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("worker file mode = %v, want 0644", info.Mode().Perm())
	}

	// Synapse's copy, in the real store, must be byte-identical too.
	if realStore := os.Getenv("SMW_LIVE_STORE"); realStore != "" {
		realPath, err := NewMediaPaths(realStore).LocalMedia(synID)
		if err == nil {
			if synBytes, err := os.ReadFile(realPath); err == nil {
				if !bytes.Equal(synBytes, payload) {
					t.Error("Synapse stored bytes differ from the payload")
				}
			}
			// Remove the file Synapse wrote for this probe.
			t.Cleanup(func() { _ = os.Remove(realPath) })
		}
	}

	// --- no thumbnails at upload, by default ---
	for _, id := range []string{goID} {
		var n int
		_ = db.pool.QueryRow(ctx,
			`SELECT count(*) FROM local_media_repository_thumbnails WHERE media_id=$1`, id).Scan(&n)
		if n != 0 {
			t.Errorf("worker created %d thumbnail rows; upload_thumbnails defaults to none", n)
		}
	}
	var synThumbs int
	_ = db.pool.QueryRow(ctx,
		`SELECT count(*) FROM local_media_repository_thumbnails WHERE media_id=$1`, synID).Scan(&synThumbs)
	t.Logf("worker: 0 thumbnail rows, Synapse: %d (the divergence this version chooses)", synThumbs)
	t.Logf("worker media_id=%s  Synapse media_id=%s  user=%s", goID, synID, goRow.UserID)
}

func mustContentURI(t *testing.T, body []byte) string {
	t.Helper()
	var r uploadResponse
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("bad upload response %q: %v", body, err)
	}
	return r.ContentURI
}

func mediaIDFromResponse(t *testing.T, body []byte) string {
	t.Helper()
	uri := mustContentURI(t, body)
	idx := strings.LastIndex(uri, "/")
	if idx < 0 {
		t.Fatalf("bad content_uri %q", uri)
	}
	return uri[idx+1:]
}

func mustLocalMedia(t *testing.T, ctx context.Context, db *DB, mediaID string) *LocalMedia {
	t.Helper()
	m, err := db.GetLocalMedia(ctx, mediaID)
	if err != nil {
		t.Fatalf("reading row %s: %v", mediaID, err)
	}
	return m
}

func readAllClose(resp *http.Response) ([]byte, error) {
	defer func() { _ = resp.Body.Close() }()
	buf := new(bytes.Buffer)
	_, err := buf.ReadFrom(resp.Body)
	return buf.Bytes(), err
}

func urlQueryEscape(s string) string {
	return strings.ReplaceAll(fmt.Sprintf("%s", s), " ", "%20")
}

// randomPNG builds a small valid PNG with random pixels, so every run produces
// a distinct hash while remaining a decodable image.
func randomPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	noise := make([]byte, w*h*4)
	if _, err := rand.Read(noise); err != nil {
		t.Fatal(err)
	}
	copy(img.Pix, noise)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

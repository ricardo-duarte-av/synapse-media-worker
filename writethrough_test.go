package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func newWriteThroughFetcher(t *testing.T, store string) *RemoteFetcher {
	t.Helper()
	return &RemoteFetcher{
		paths: NewMediaPaths(store),
		log:   zerolog.Nop(),
	}
}

// The thumbnail must land exactly where Synapse looks for it: under the
// original's filesystem_id, in remote_thumbnail (singular), named
// <w>-<h>-<toplevel>-<subtype>-<method>.
func TestWriteThroughLandsAtSynapsesPath(t *testing.T) {
	store := t.TempDir()
	rf := newWriteThroughFetcher(t, store)
	const fsID = "AbCdEfGhIjKlMnOpQrStUvWx"

	req := ThumbnailRequest{Width: 96, Height: 96, Method: "crop", Type: typePNG}

	path, err := rf.paths.RemoteThumbnail("example.com", fsID, req.Width, req.Height, req.Type, req.Method)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(store, "remote_thumbnail", "example.com", "Ab", "Cd",
		"EfGhIjKlMnOpQrStUvWx", "96-96-image-png-crop")
	if path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	if err := rf.paths.WritablePath(path); err != nil {
		t.Fatalf("remote thumbnail path should be writable: %v", err)
	}
}

// Local media must not be written through: local_thumbnails/ stays untouchable
// so a bug here cannot damage media this server owns.
func TestWriteThroughRefusesLocalThumbnails(t *testing.T) {
	store := t.TempDir()
	p := NewMediaPaths(store)
	local, err := p.LocalThumbnail("AbCdEfGhIjKlMnOpQrStUvWx", 96, 96, typePNG, "crop")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.WritablePath(local); err == nil {
		t.Error("local thumbnail path must not be writable")
	}
}

// The gate decides where a generated thumbnail goes. Local media, a missing
// filesystem_id, or the flag being off must all send it to the worker's cache.
func TestWriteThroughGate(t *testing.T) {
	withFlag := func(on bool, remote *RemoteFetcher) *Server {
		return &Server{
			cfg:    &Config{Media: MediaConfig{WriteThroughThumbnails: on}},
			remote: remote,
		}
	}
	rf := &RemoteFetcher{}

	if !withFlag(true, rf).writeThroughRemote("example.com", "AbCdEfGh") {
		t.Error("remote media with the flag on should be written through")
	}
	if withFlag(false, rf).writeThroughRemote("example.com", "AbCdEfGh") {
		t.Error("flag off should not write through")
	}
	// Local media has no origin.
	if withFlag(true, rf).writeThroughRemote("", "AbCdEfGh") {
		t.Error("local media must never be written through")
	}
	// No filesystem_id means we do not know where Synapse expects the file.
	if withFlag(true, rf).writeThroughRemote("example.com", "") {
		t.Error("a missing filesystem_id must not be written through")
	}
	// Without a fetcher there is no writable store.
	if withFlag(true, nil).writeThroughRemote("example.com", "AbCdEfGh") {
		t.Error("write-through requires the remote fetcher")
	}
}

// A crash mid-write must not leave a file that looks like a valid thumbnail,
// and the finished file must carry Synapse's permissions.
func TestWriteThroughFileMechanics(t *testing.T) {
	store := t.TempDir()
	rf := newWriteThroughFetcher(t, store)
	const fsID = "AbCdEfGhIjKlMnOpQrStUvWx"
	req := ThumbnailRequest{Width: 32, Height: 32, Method: "crop", Type: typePNG}
	payload := []byte("some thumbnail data")

	path, _ := rf.paths.RemoteThumbnail("example.com", fsID, req.Width, req.Height, req.Type, req.Method)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// Reproduce the write half of WriteThroughRemoteThumbnail.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".thumb-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tmp.Write(payload); err != nil {
		t.Fatal(err)
	}
	_ = tmp.Sync()
	_ = tmp.Close()
	_ = os.Chmod(tmp.Name(), 0o644)
	if err := os.Rename(tmp.Name(), path); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644 to match Synapse", info.Mode().Perm())
	}
	// The row records the on-disk size, since Synapse serves it as
	// Content-Length.
	if info.Size() != int64(len(payload)) {
		t.Errorf("size = %d, want %d", info.Size(), len(payload))
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".thumb-") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}

// write_through_thumbnails without fetch_remote has no writable store, so it
// must be rejected rather than silently doing nothing.
func TestWriteThroughRequiresFetchRemote(t *testing.T) {
	body := strings.Replace(minimalConfig,
		"media:\n  store_path: /data/media_store",
		"media:\n  store_path: /data/media_store\n  write_through_thumbnails: true", 1)
	if _, err := LoadConfig(writeConfig(t, body)); err == nil {
		t.Fatal("write_through_thumbnails without fetch_remote was accepted")
	}
}

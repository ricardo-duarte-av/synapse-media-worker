package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// The sweep walks every file under local_content and local_thumbnails. On a
// real store that is minutes, so it must never run before the listener is
// created -- doing so meant the socket did not appear and nothing could reach
// the worker at all.
//
// This guards the ordering in run(), which no other test covers.
func TestSweepDoesNotRunBeforeTheListener(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)

	sweep := strings.Index(body, "go sweepStaleUploads(")
	if sweep < 0 {
		t.Fatal("sweepStaleUploads is not launched in the background; it will block startup")
	}
	listener := strings.Index(body, "makeListener(cfg.Listen)")
	if listener < 0 {
		t.Fatal("could not find the listener creation")
	}
	if sweep < listener {
		t.Error("the stale-upload sweep is started before the listener; " +
			"on a large media store the socket will not appear for minutes")
	}
	// A blocking call would reintroduce the bug even if a background one exists.
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "sweepStaleUploads(") {
			t.Errorf("sweepStaleUploads is called synchronously: %q", trimmed)
		}
	}
}

// The sweep must abandon its walk when the worker is shutting down, rather than
// holding it open for the length of a full scan.
func TestSweepStopsOnShutdown(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "local_thumbnails", "ab", "cd")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := range 200 {
		name := filepath.Join(dir, "file"+string(rune('a'+i%26))+string(rune('a'+i/26)))
		if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	stop := make(chan struct{})
	close(stop) // already shutting down

	done := make(chan struct{})
	go func() {
		defer close(done)
		sweepStaleUploads(base, zerolog.Nop(), stop)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sweep did not give up when told to stop")
	}
}

// Abandoned temp files go; recent ones and real media stay.
func TestSweepRemovesOnlyAbandonedTempFiles(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "local_content", "ab", "cd")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	old := filepath.Join(dir, ".incoming-abandoned")
	fresh := filepath.Join(dir, ".incoming-inflight")
	real := filepath.Join(dir, "EfGhIjKlMnOpQrStUvWx")
	for _, p := range []string{old, fresh, real} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Age the abandoned one past the grace period.
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}

	sweepStaleUploads(base, zerolog.Nop(), make(chan struct{}))

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("an abandoned temp file survived the sweep")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("a temp file still being written was removed")
	}
	if _, err := os.Stat(real); err != nil {
		t.Error("real media was removed")
	}
}

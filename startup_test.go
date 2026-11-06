package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// Abandoned temp files go; recent ones and real media stay, and nothing outside
// the .tmp directories is looked at.
func TestSweepRemovesOnlyAbandonedTempFiles(t *testing.T) {
	base := t.TempDir()
	paths := NewMediaPaths(base)
	paths.AllowUploadWrites()

	tmpDir := filepath.Join(base, "local_content", tempDirName)
	shard := filepath.Join(base, "local_content", "ab", "cd")
	for _, d := range []string{tmpDir, shard} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	old := filepath.Join(tmpDir, ".incoming-abandoned")
	fresh := filepath.Join(tmpDir, ".incoming-inflight")
	remoteOld := filepath.Join(base, "remote_thumbnail", tempDirName, ".thumb-abandoned")
	real := filepath.Join(shard, "EfGhIjKlMnOpQrStUvWx")
	// A shard is never read, so even a stale temp-looking file there stays.
	inShard := filepath.Join(shard, ".incoming-legacy")
	for _, p := range []string{old, fresh, remoteOld, real, inShard} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-2 * time.Hour)
	for _, p := range []string{old, remoteOld, real, inShard} {
		if err := os.Chtimes(p, past, past); err != nil {
			t.Fatal(err)
		}
	}

	sweepStaleTempFiles(paths, zerolog.Nop())

	for _, p := range []string{old, remoteOld} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("abandoned temp file %s survived the sweep", p)
		}
	}
	for _, p := range []string{fresh, real, inShard} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed", p)
		}
	}
}

// Without uploads, local_content is not writable, so the sweep must not touch
// its .tmp either.
func TestSweepRespectsTheWriteGuard(t *testing.T) {
	base := t.TempDir()
	paths := NewMediaPaths(base)
	old := filepath.Join(base, "local_content", tempDirName, ".incoming-abandoned")
	if err := os.MkdirAll(filepath.Dir(old), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(old, past, past)

	sweepStaleTempFiles(paths, zerolog.Nop())
	if _, err := os.Stat(old); err != nil {
		t.Error("the sweep removed a file under local_content with uploads off")
	}
}

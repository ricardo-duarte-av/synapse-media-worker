package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
)

// livewritethrough_test.go exercises the whole write-through path against the
// real database schema, using a scratch media store so nothing is written into
// Synapse's own files.
//
// It writes rows for the origin "example.invalid" -- a TLD reserved by RFC 2606
// that can never be a real homeserver -- and removes them again, verifying the
// cleanup. Skipped unless SMW_LIVE_DB is set.
func TestLiveWriteThroughAgainstRealSchema(t *testing.T) {
	dbURI := os.Getenv("SMW_LIVE_DB")
	if dbURI == "" {
		t.Skip("set SMW_LIVE_DB to run")
	}

	const origin = "example.invalid"
	const mediaID = "SmwWriteThroughProbe"
	const fsID = "AbCdEfGhIjKlMnOpQrStUvWx"

	ctx := context.Background()
	db, err := NewDB(ctx, DatabaseConfig{URI: dbURI, MaxConns: 2}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}

	// Always clean up, even on failure: these rows must not outlive the test.
	//
	// This is deliberately a defer rather than t.Cleanup. Cleanup functions run
	// after the test body returns, which is after `defer db.Close()` above has
	// already closed the pool -- so the deletes would silently do nothing and
	// leave rows behind in a live database.
	cleanup := func() {
		if _, err := db.pool.Exec(ctx,
			`DELETE FROM remote_media_cache_thumbnails WHERE media_origin = $1 AND media_id = $2`,
			origin, mediaID); err != nil {
			t.Errorf("cleaning up thumbnail rows: %v", err)
		}
		if _, err := db.pool.Exec(ctx,
			`DELETE FROM remote_media_cache WHERE media_origin = $1 AND media_id = $2`,
			origin, mediaID); err != nil {
			t.Errorf("cleaning up media rows: %v", err)
		}
		var n int
		if err := db.pool.QueryRow(ctx,
			`SELECT count(*) FROM remote_media_cache WHERE media_origin = $1`, origin).Scan(&n); err != nil {
			t.Errorf("verifying cleanup: %v", err)
		} else if n != 0 {
			t.Errorf("%d probe rows survived cleanup", n)
		}
	}
	cleanup()
	// LIFO: the pool closes only after cleanup has run.
	defer db.Close()
	defer cleanup()

	store := t.TempDir()
	rf := &RemoteFetcher{db: db, paths: NewMediaPaths(store), log: zerolog.Nop(), authenticated: true}

	// A media row has to exist for the thumbnail to belong to.
	length := int64(4321)
	won, err := db.StoreRemoteMedia(ctx, &RemoteMedia{
		Origin: origin, MediaID: mediaID, MediaType: "image/png",
		Length: &length, UploadName: "probe.png", FilesystemID: fsID,
	}, "725c32e479cadadc84938d7024bdff35c6c3a4209ebb5db438ebfe4667228b41", true)
	if err != nil {
		t.Fatal(err)
	}
	if !won {
		t.Fatal("probe row already existed; a previous run did not clean up")
	}

	// The race path: a second insert must not win, and must not disturb the row.
	won2, err := db.StoreRemoteMedia(ctx, &RemoteMedia{
		Origin: origin, MediaID: mediaID, MediaType: "image/gif",
		Length: &length, FilesystemID: "ZZZZZZZZZZZZZZZZZZZZZZZZ",
	}, "deadbeef", true)
	if err != nil {
		t.Fatal(err)
	}
	if won2 {
		t.Error("a conflicting insert reported that it won")
	}
	stored, err := db.GetRemoteMedia(ctx, origin, mediaID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.FilesystemID != fsID {
		t.Errorf("filesystem_id = %q, want it unchanged (%q)", stored.FilesystemID, fsID)
	}
	if stored.MediaType != "image/png" {
		t.Errorf("media_type = %q, want it unchanged", stored.MediaType)
	}

	// Now the thumbnail write-through itself.
	req := ThumbnailRequest{Width: 96, Height: 96, Method: "crop", Type: typePNG}
	payload := []byte("a generated thumbnail")
	if err := rf.WriteThroughRemoteThumbnail(ctx, origin, mediaID, fsID, req, payload); err != nil {
		t.Fatal(err)
	}

	// The file must be where Synapse looks for it.
	wantPath := filepath.Join(store, "remote_thumbnail", origin, "Ab", "Cd",
		"EfGhIjKlMnOpQrStUvWx", "96-96-image-png-crop")
	onDisk, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("thumbnail not at Synapse's path: %v", err)
	}
	if string(onDisk) != string(payload) {
		t.Error("thumbnail contents differ")
	}

	// The row must describe the file, since Synapse serves thumbnail_length as
	// the Content-Length.
	rows, err := db.GetRemoteThumbnails(ctx, origin, mediaID)
	if err != nil {
		t.Fatal(err)
	}
	row, found := FindExact(rows, req)
	if !found {
		t.Fatalf("no exact thumbnail row was written; got %+v", rows)
	}
	if row.Length != int64(len(payload)) {
		t.Errorf("thumbnail_length = %d, want %d (the on-disk size)", row.Length, len(payload))
	}
	if row.FilesystemID != fsID {
		t.Errorf("thumbnail filesystem_id = %q, want the media's %q", row.FilesystemID, fsID)
	}

	// Rewriting must update the length and leave filesystem_id alone.
	bigger := []byte("a rather longer generated thumbnail")
	if err := rf.WriteThroughRemoteThumbnail(ctx, origin, mediaID, fsID, req, bigger); err != nil {
		t.Fatal(err)
	}
	rows, _ = db.GetRemoteThumbnails(ctx, origin, mediaID)
	row, _ = FindExact(rows, req)
	if row.Length != int64(len(bigger)) {
		t.Errorf("after rewrite thumbnail_length = %d, want %d", row.Length, len(bigger))
	}
	if row.FilesystemID != fsID {
		t.Errorf("after rewrite filesystem_id = %q, want %q", row.FilesystemID, fsID)
	}
	if len(rows) != 1 {
		t.Errorf("rewrite created %d rows, want 1", len(rows))
	}
}

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"go.mau.fi/util/exhttp"
	"maunium.net/go/mautrix/federation"
)

// livefetch_test.go checks the fetch path against real homeservers, using media
// Synapse has already cached as the ground truth: fetch it again ourselves and
// require the bytes, hash, content type and filename to match what Synapse
// stored. Nothing is written.
//
// It is skipped unless pointed at a real deployment:
//
//	SMW_LIVE_DB=postgres://... \
//	SMW_LIVE_KEY=/path/to/signing.key \
//	SMW_LIVE_STORE=/path/to/media_store \
//	SMW_LIVE_SERVER=example.com \
//	go test -run TestLiveFetchMatchesSynapse -v
func TestLiveFetchMatchesSynapse(t *testing.T) {
	dbURI := os.Getenv("SMW_LIVE_DB")
	keyPath := os.Getenv("SMW_LIVE_KEY")
	store := os.Getenv("SMW_LIVE_STORE")
	serverName := os.Getenv("SMW_LIVE_SERVER")
	if dbURI == "" || keyPath == "" || store == "" || serverName == "" {
		t.Skip("set SMW_LIVE_DB, SMW_LIVE_KEY, SMW_LIVE_STORE and SMW_LIVE_SERVER to run")
	}

	key, _, err := loadSigningKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	cache := federation.NewInMemoryCache()
	client := federation.NewClient(serverName, key, cache, exhttp.SensibleClientSettings)
	fetcher := NewFetcher(client, 100<<20, 60*time.Second, 2)
	paths := NewMediaPaths(store)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURI)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	// Small, recently cached media from distinct origins, so the sample spans
	// several implementations without leaning on any one server.
	const q = `
SELECT DISTINCT ON (media_origin)
       media_origin, media_id, filesystem_id, media_type,
       COALESCE(upload_name, ''), media_length, COALESCE(sha256, '')
  FROM remote_media_cache
 WHERE media_length IS NOT NULL AND media_length < 2000000
   AND filesystem_id IS NOT NULL AND quarantined_by IS NULL
 ORDER BY media_origin, created_ts DESC
 LIMIT 8`
	rows, err := pool.Query(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	type item struct {
		origin, mediaID, fsID, mediaType, uploadName, storedSHA string
		length                                                  int64
	}
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.origin, &it.mediaID, &it.fsID, &it.mediaType,
			&it.uploadName, &it.length, &it.storedSHA); err != nil {
			t.Fatal(err)
		}
		items = append(items, it)
	}
	rows.Close()
	if len(items) == 0 {
		t.Skip("no cached remote media to compare against")
	}

	_ = zerolog.Nop()
	var checked, unreachable int
	for _, it := range items {
		path, err := paths.RemoteMedia(it.origin, it.fsID)
		if err != nil {
			t.Errorf("%s/%s: path: %v", it.origin, it.mediaID, err)
			continue
		}
		onDisk, err := os.ReadFile(path)
		if err != nil {
			continue // Synapse's own copy is gone; nothing to compare against.
		}

		var got bytes.Buffer
		info, err := fetcher.Fetch(ctx, it.origin, it.mediaID, &got)
		if err != nil {
			// The origin may be down, have purged the media, or not support
			// authenticated media. That is not our bug.
			t.Logf("  %s/%s: unreachable (%v)", it.origin, it.mediaID, err)
			unreachable++
			continue
		}
		checked++

		if !bytes.Equal(got.Bytes(), onDisk) {
			t.Errorf("%s/%s: BYTES DIFFER from Synapse's copy (got %d, stored %d)",
				it.origin, it.mediaID, got.Len(), len(onDisk))
			continue
		}
		if info.Length != it.length {
			t.Errorf("%s/%s: length %d, Synapse stored %d", it.origin, it.mediaID, info.Length, it.length)
		}
		// The hash is what admin quarantine matches on, so it has to agree.
		sum := sha256.Sum256(onDisk)
		if info.SHA256 != hex.EncodeToString(sum[:]) {
			t.Errorf("%s/%s: sha256 does not match the file we received", it.origin, it.mediaID)
		}
		if it.storedSHA != "" && info.SHA256 != it.storedSHA {
			t.Errorf("%s/%s: sha256 %s, Synapse stored %s", it.origin, it.mediaID, info.SHA256, it.storedSHA)
		}
		// Synapse stores the content type verbatim, parameters included.
		if !strings.EqualFold(info.MediaType, it.mediaType) {
			t.Errorf("%s/%s: media type %q, Synapse stored %q", it.origin, it.mediaID, info.MediaType, it.mediaType)
		}
		if info.UploadName != it.uploadName {
			t.Errorf("%s/%s: upload name %q, Synapse stored %q", it.origin, it.mediaID, info.UploadName, it.uploadName)
		}
		t.Logf("  %s/%s: %d bytes, %s, ok", it.origin, it.mediaID, info.Length, info.MediaType)
	}
	t.Logf("compared %d media (%d origins unreachable)", checked, unreachable)
	if checked == 0 {
		t.Skip("no origin server could be reached")
	}
}

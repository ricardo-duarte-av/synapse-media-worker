package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

// ErrNotFound means no row exists for the requested media.
var ErrNotFound = errors.New("media not found")

// LocalMedia is a row of local_media_repository. Length is nil when the media
// ID has been created but the upload has not completed yet (MSC2246).
type LocalMedia struct {
	MediaID       string
	MediaType     string
	Length        *int64
	UploadName    string
	QuarantinedBy string
	URLCache      string
	Authenticated bool
	CreatedTS     int64
	UserID        string
}

// Quarantined reports whether the media has been quarantined by an admin.
func (m *LocalMedia) Quarantined() bool { return m.QuarantinedBy != "" }

// IsURLCache reports whether the file lives under url_cache/ rather than
// local_content/.
func (m *LocalMedia) IsURLCache() bool { return m.URLCache != "" }

// RemoteMedia is a row of remote_media_cache.
type RemoteMedia struct {
	Origin        string
	MediaID       string
	MediaType     string
	Length        *int64
	UploadName    string
	FilesystemID  string
	QuarantinedBy string
	Authenticated bool
}

func (m *RemoteMedia) Quarantined() bool { return m.QuarantinedBy != "" }

// ThumbnailRow is a row of local_media_repository_thumbnails or
// remote_media_cache_thumbnails.
type ThumbnailRow struct {
	Width        int
	Height       int
	Type         string
	Method       string
	Length       int64
	FilesystemID string // remote only
}

// DB is a read-mostly accessor over Synapse's database.
//
// The worker treats Synapse's schema as read-only truth. The single exception
// is last_access_ts, which is batched and flushed on a timer exactly as
// Synapse does; without it, media retention and LRU cleanup would treat
// everything the worker serves as never accessed.
type DB struct {
	pool *pgxpool.Pool
	log  zerolog.Logger

	updateLastAccess bool

	mu             sync.Mutex
	recentLocal    map[string]struct{}
	recentRemote   map[[2]string]struct{}
	flushInterval  time.Duration
	stopFlush      chan struct{}
	flushStopped   chan struct{}
	flushOnceGuard sync.Once
}

func NewDB(ctx context.Context, cfg DatabaseConfig, log zerolog.Logger) (*DB, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.URI)
	if err != nil {
		return nil, fmt.Errorf("parsing database.uri: %w", err)
	}
	if cfg.MaxConns > 0 {
		poolCfg.MaxConns = cfg.MaxConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("connecting to database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging database: %w", err)
	}
	interval := cfg.LastAccessInterval
	if interval <= 0 {
		interval = time.Minute
	}
	db := &DB{
		pool:             pool,
		log:              log,
		updateLastAccess: cfg.UpdateLastAccess,
		recentLocal:      make(map[string]struct{}),
		recentRemote:     make(map[[2]string]struct{}),
		flushInterval:    interval,
		stopFlush:        make(chan struct{}),
		flushStopped:     make(chan struct{}),
	}
	if db.updateLastAccess {
		go db.flushLoop()
	} else {
		close(db.flushStopped)
	}
	return db, nil
}

func (d *DB) Close() {
	d.flushOnceGuard.Do(func() {
		if d.updateLastAccess {
			close(d.stopFlush)
		}
	})
	<-d.flushStopped
	d.pool.Close()
}

const localMediaQuery = `
SELECT media_type, media_length, upload_name, quarantined_by, url_cache,
       COALESCE(authenticated, false), COALESCE(created_ts, 0),
       COALESCE(user_id, '')
  FROM local_media_repository
 WHERE media_id = $1`

func (d *DB) GetLocalMedia(ctx context.Context, mediaID string) (*LocalMedia, error) {
	m := &LocalMedia{MediaID: mediaID}
	var mediaType, uploadName, quarantinedBy, urlCache *string
	err := d.pool.QueryRow(ctx, localMediaQuery, mediaID).Scan(
		&mediaType, &m.Length, &uploadName, &quarantinedBy, &urlCache,
		&m.Authenticated, &m.CreatedTS, &m.UserID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, fmt.Errorf("querying local media: %w", err)
	}
	m.MediaType = derefString(mediaType)
	m.UploadName = derefString(uploadName)
	m.QuarantinedBy = derefString(quarantinedBy)
	m.URLCache = derefString(urlCache)
	return m, nil
}

const remoteMediaQuery = `
SELECT media_type, media_length, upload_name, filesystem_id, quarantined_by,
       COALESCE(authenticated, false)
  FROM remote_media_cache
 WHERE media_origin = $1 AND media_id = $2`

func (d *DB) GetRemoteMedia(ctx context.Context, origin, mediaID string) (*RemoteMedia, error) {
	m := &RemoteMedia{Origin: origin, MediaID: mediaID}
	var mediaType, uploadName, filesystemID, quarantinedBy *string
	err := d.pool.QueryRow(ctx, remoteMediaQuery, origin, mediaID).Scan(
		&mediaType, &m.Length, &uploadName, &filesystemID, &quarantinedBy,
		&m.Authenticated,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, fmt.Errorf("querying remote media: %w", err)
	}
	m.MediaType = derefString(mediaType)
	m.UploadName = derefString(uploadName)
	m.FilesystemID = derefString(filesystemID)
	m.QuarantinedBy = derefString(quarantinedBy)
	return m, nil
}

const localThumbnailsQuery = `
SELECT thumbnail_width, thumbnail_height, thumbnail_type, thumbnail_method,
       COALESCE(thumbnail_length, 0)
  FROM local_media_repository_thumbnails
 WHERE media_id = $1`

func (d *DB) GetLocalThumbnails(ctx context.Context, mediaID string) ([]ThumbnailRow, error) {
	rows, err := d.pool.Query(ctx, localThumbnailsQuery, mediaID)
	if err != nil {
		return nil, fmt.Errorf("querying local thumbnails: %w", err)
	}
	defer rows.Close()
	return scanThumbnails(rows, false)
}

const remoteThumbnailsQuery = `
SELECT thumbnail_width, thumbnail_height, thumbnail_type, thumbnail_method,
       COALESCE(thumbnail_length, 0), filesystem_id
  FROM remote_media_cache_thumbnails
 WHERE media_origin = $1 AND media_id = $2`

func (d *DB) GetRemoteThumbnails(ctx context.Context, origin, mediaID string) ([]ThumbnailRow, error) {
	rows, err := d.pool.Query(ctx, remoteThumbnailsQuery, origin, mediaID)
	if err != nil {
		return nil, fmt.Errorf("querying remote thumbnails: %w", err)
	}
	defer rows.Close()
	return scanThumbnails(rows, true)
}

func scanThumbnails(rows pgx.Rows, remote bool) ([]ThumbnailRow, error) {
	var out []ThumbnailRow
	for rows.Next() {
		var t ThumbnailRow
		var width, height *int
		var typ, method, fsID *string
		var dest []any
		if remote {
			dest = []any{&width, &height, &typ, &method, &t.Length, &fsID}
		} else {
			dest = []any{&width, &height, &typ, &method, &t.Length}
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("scanning thumbnail row: %w", err)
		}
		// Every column on these tables is nullable; a row missing any of the
		// identifying fields cannot be addressed on disk, so skip it.
		if width == nil || height == nil || typ == nil || method == nil {
			continue
		}
		t.Width, t.Height, t.Type, t.Method = *width, *height, *typ, *method
		t.FilesystemID = derefString(fsID)
		out = append(out, t)
	}
	return out, rows.Err()
}

// MarkRecentlyAccessedLocal records a local media access for the next batched
// last_access_ts flush.
func (d *DB) MarkRecentlyAccessedLocal(mediaID string) {
	if !d.updateLastAccess {
		return
	}
	d.mu.Lock()
	d.recentLocal[mediaID] = struct{}{}
	d.mu.Unlock()
}

// MarkRecentlyAccessedRemote records a remote media access for the next
// batched last_access_ts flush.
func (d *DB) MarkRecentlyAccessedRemote(origin, mediaID string) {
	if !d.updateLastAccess {
		return
	}
	d.mu.Lock()
	d.recentRemote[[2]string{origin, mediaID}] = struct{}{}
	d.mu.Unlock()
}

func (d *DB) flushLoop() {
	defer close(d.flushStopped)
	ticker := time.NewTicker(d.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			d.flushLastAccess()
		case <-d.stopFlush:
			d.flushLastAccess()
			return
		}
	}
}

func (d *DB) flushLastAccess() {
	d.mu.Lock()
	local, remote := d.recentLocal, d.recentRemote
	d.recentLocal = make(map[string]struct{})
	d.recentRemote = make(map[[2]string]struct{})
	d.mu.Unlock()

	if len(local) == 0 && len(remote) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	now := time.Now().UnixMilli()

	batch := &pgx.Batch{}
	for mediaID := range local {
		batch.Queue(`UPDATE local_media_repository SET last_access_ts = $1 WHERE media_id = $2`, now, mediaID)
	}
	for key := range remote {
		batch.Queue(`UPDATE remote_media_cache SET last_access_ts = $1 WHERE media_origin = $2 AND media_id = $3`,
			now, key[0], key[1])
	}
	if err := d.pool.SendBatch(ctx, batch).Close(); err != nil {
		// Losing an access timestamp is not worth failing a request over; the
		// next access will record it again.
		d.log.Warn().Err(err).
			Int("local", len(local)).Int("remote", len(remote)).
			Msg("Failed to flush last_access_ts updates")
		return
	}
	d.log.Debug().Int("local", len(local)).Int("remote", len(remote)).
		Msg("Flushed last_access_ts updates")
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// --- writes for fetched remote media ---------------------------------------

// storeRemoteMediaQuery mirrors Synapse's store_cached_remote_media
// (synapse/storage/databases/main/media_repository.py:734), column for column.
//
// Synapse uses a plain INSERT and lets the unique constraint raise, catching
// IntegrityError and re-reading the winner. ON CONFLICT DO NOTHING expresses
// the same intent without the round trip.
//
// It must never be DO UPDATE. A Synapse worker mid-request is holding the
// filesystem_id it read earlier; changing it underneath makes that worker look
// for a file that is no longer there, and orphans the old one.
const storeRemoteMediaQuery = `
INSERT INTO remote_media_cache (
    media_origin, media_id, media_type, media_length, created_ts,
    upload_name, filesystem_id, last_access_ts, authenticated, sha256
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (media_origin, media_id) DO NOTHING`

// StoreRemoteMedia inserts a row for freshly fetched remote media. It reports
// whether the row was ours; false means another writer won the race and the
// caller must discard its file and use the existing row.
//
// created_ts and last_access_ts are both set to now, never to any timestamp
// from the origin server: media retention and purge_media_cache delete on
// last_access_ts, so a backdated value invites the media to be deleted almost
// immediately.
func (d *DB) StoreRemoteMedia(ctx context.Context, m *RemoteMedia, sha256hex string, authenticated bool) (bool, error) {
	now := time.Now().UnixMilli()

	var uploadName, sha *string
	if m.UploadName != "" {
		uploadName = &m.UploadName
	}
	if sha256hex != "" {
		sha = &sha256hex
	}
	length := int64(0)
	if m.Length != nil {
		length = *m.Length
	}

	tag, err := d.pool.Exec(ctx, storeRemoteMediaQuery,
		m.Origin, m.MediaID, m.MediaType, length, now,
		uploadName, m.FilesystemID, now, authenticated, sha,
	)
	if err != nil {
		return false, fmt.Errorf("storing remote media: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// storeRemoteThumbnailQuery mirrors Synapse's store_remote_media_thumbnail
// (synapse/storage/databases/main/media_repository.py:863), which is a
// simple_upsert with thumbnail_length in `values` and filesystem_id in
// `insertion_values`.
//
// That distinction matters: on conflict Synapse updates only the length and
// leaves filesystem_id alone. The thumbnail file is looked up under
// remote_media_cache.filesystem_id, so a row pointing somewhere else would send
// Synapse to a path that does not exist, and it would never correct itself.
//
// The conflict target is the real unique index, whose column order puts
// thumbnail_type before thumbnail_method.
const storeRemoteThumbnailQuery = `
INSERT INTO remote_media_cache_thumbnails (
    media_origin, media_id, thumbnail_width, thumbnail_height,
    thumbnail_type, thumbnail_method, thumbnail_length, filesystem_id
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (media_origin, media_id, thumbnail_width, thumbnail_height,
             thumbnail_type, thumbnail_method)
DO UPDATE SET thumbnail_length = EXCLUDED.thumbnail_length`

// StoreRemoteThumbnail records a thumbnail the worker generated for remote
// media, so Synapse can serve it too.
//
// length must be the size of the file actually on disk, not of the buffer that
// produced it: Synapse sends this value as the Content-Length when it serves
// the thumbnail, so a row that disagrees with the file truncates the response.
func (d *DB) StoreRemoteThumbnail(ctx context.Context, origin, mediaID, filesystemID string, t ThumbnailRow) error {
	_, err := d.pool.Exec(ctx, storeRemoteThumbnailQuery,
		origin, mediaID, t.Width, t.Height, t.Type, t.Method, t.Length, filesystemID)
	if err != nil {
		return fmt.Errorf("storing remote thumbnail: %w", err)
	}
	return nil
}

// --- writes for uploaded local media ---------------------------------------

// storeLocalMediaQuery mirrors Synapse's store_local_media
// (synapse/storage/databases/main/media_repository.py), column for column.
// Synapse uses a plain INSERT; the unique constraint on media_id makes a
// collision an error rather than an overwrite, which is what we want too.
const storeLocalMediaQuery = `
INSERT INTO local_media_repository (
    media_id, media_type, created_ts, upload_name, media_length,
    user_id, url_cache, authenticated, sha256, quarantined_by
) VALUES ($1, $2, $3, $4, $5, $6, NULL, $7, $8, $9)`

// StoreLocalMedia inserts a row for a completed synchronous upload.
func (d *DB) StoreLocalMedia(ctx context.Context, m *LocalMedia, sha256hex string, authenticated bool, quarantinedBy string) error {
	length := int64(0)
	if m.Length != nil {
		length = *m.Length
	}
	_, err := d.pool.Exec(ctx, storeLocalMediaQuery,
		m.MediaID, m.MediaType, m.CreatedTS, nullableString(m.UploadName), length,
		m.UserID, authenticated, nullableString(sha256hex), nullableString(quarantinedBy))
	if err != nil {
		return fmt.Errorf("storing local media: %w", err)
	}
	return nil
}

// storeLocalMediaIDQuery mirrors store_local_media_id: the four columns
// /_matrix/media/v1/create populates, leaving media_length NULL to mark the
// media as pending.
const storeLocalMediaIDQuery = `
INSERT INTO local_media_repository (media_id, created_ts, user_id, authenticated)
VALUES ($1, $2, $3, $4)`

// StoreLocalMediaID reserves a media ID for an asynchronous upload.
func (d *DB) StoreLocalMediaID(ctx context.Context, mediaID string, createdTS int64, userID string, authenticated bool) error {
	_, err := d.pool.Exec(ctx, storeLocalMediaIDQuery, mediaID, createdTS, userID, authenticated)
	if err != nil {
		return fmt.Errorf("reserving media id: %w", err)
	}
	return nil
}

// completeLocalMediaQuery finishes an asynchronous upload.
//
// Synapse's equivalent UPDATE is unconditional on media_id, which is why it has
// to hold a cross-worker lock across the whole upload to stop two PUTs both
// completing the same media. Adding `AND media_length IS NULL` gets the same
// guarantee from the database: the second writer affects zero rows and is told
// 409, with no lock table, renewal or timeout to get wrong.
const completeLocalMediaQuery = `
UPDATE local_media_repository
   SET media_type = $2, upload_name = $3, media_length = $4, sha256 = $5,
       quarantined_by = COALESCE($6, quarantined_by)
 WHERE media_id = $1 AND media_length IS NULL`

// CompleteLocalMedia finishes an asynchronous upload, reporting whether this
// caller was the one that completed it. False means another writer got there
// first and the caller must answer 409.
func (d *DB) CompleteLocalMedia(ctx context.Context, mediaID, mediaType, uploadName string, length int64, sha256hex, quarantinedBy string) (bool, error) {
	tag, err := d.pool.Exec(ctx, completeLocalMediaQuery,
		mediaID, mediaType, nullableString(uploadName), length,
		nullableString(sha256hex), nullableString(quarantinedBy))
	if err != nil {
		return false, fmt.Errorf("completing local media: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// countPendingMediaQuery is the query Synapse runs to enforce
// max_pending_media_uploads, and the basis for the spec's 429 on /create.
const countPendingMediaQuery = `
SELECT COUNT(*), COALESCE(MIN(created_ts), 0)
  FROM local_media_repository
 WHERE user_id = $1 AND created_ts > $2 AND media_length IS NULL`

// CountPendingMedia returns how many un-uploaded media IDs a user holds that
// have not yet expired, and when the oldest of them expires.
func (d *DB) CountPendingMedia(ctx context.Context, userID string, notBefore int64) (int, int64, error) {
	var count int
	var oldest int64
	err := d.pool.QueryRow(ctx, countPendingMediaQuery, userID, notBefore).Scan(&count, &oldest)
	if err != nil {
		return 0, 0, fmt.Errorf("counting pending media: %w", err)
	}
	return count, oldest, nil
}

// isHashQuarantinedQuery mirrors get_is_hash_quarantined: media is quarantined
// by content hash across both the local and remote tables together.
const isHashQuarantinedQuery = `
SELECT 1 FROM local_media_repository WHERE sha256 = $1 AND quarantined_by IS NOT NULL
UNION ALL
SELECT 1 FROM remote_media_cache   WHERE sha256 = $1 AND quarantined_by IS NOT NULL
LIMIT 1`

// IsHashQuarantined reports whether this content has already been quarantined
// under some other media ID.
//
// Synapse applies this on upload only, and when it matches it stores the media
// anyway with quarantined_by = 'system' and answers the uploader with a normal
// 200. Skipping it would make the worker a way to re-upload quarantined
// content.
func (d *DB) IsHashQuarantined(ctx context.Context, sha256hex string) (bool, error) {
	if sha256hex == "" {
		return false, nil
	}
	var one int
	err := d.pool.QueryRow(ctx, isHashQuarantinedQuery, sha256hex).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("checking quarantined hashes: %w", err)
	}
	return true, nil
}

func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// storeLocalThumbnailQuery mirrors store_local_thumbnail, an upsert keyed on
// the five identifying columns.
const storeLocalThumbnailQuery = `
INSERT INTO local_media_repository_thumbnails (
    media_id, thumbnail_width, thumbnail_height, thumbnail_type,
    thumbnail_method, thumbnail_length
) VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (media_id, thumbnail_width, thumbnail_height, thumbnail_type, thumbnail_method)
DO UPDATE SET thumbnail_length = EXCLUDED.thumbnail_length`

// StoreLocalThumbnail records a thumbnail for local media.
func (d *DB) StoreLocalThumbnail(ctx context.Context, mediaID string, t ThumbnailRow) error {
	_, err := d.pool.Exec(ctx, storeLocalThumbnailQuery,
		mediaID, t.Width, t.Height, t.Type, t.Method, t.Length)
	if err != nil {
		return fmt.Errorf("storing local thumbnail: %w", err)
	}
	return nil
}

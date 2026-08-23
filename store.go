package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/rs/zerolog"
	"golang.org/x/sync/singleflight"
)

// store.go fetches remote media and persists it the way Synapse would, so
// Synapse cannot tell which of the two wrote it.
//
// The ordering below is the important part. Synapse writes the file first and
// inserts the row second, and so must we -- a row whose file is missing is
// worse than no row at all. Synapse's fetch_media returns nothing, it falls
// through to its download path, hits the unique constraint we created,
// re-reads the row, and returns media info whose file still is not there. That
// is a 404 loop which never self-heals.

// RemoteFetcher fetches remote media and stores it into Synapse's media store.
type RemoteFetcher struct {
	db      *DB
	paths   *MediaPaths
	fetcher *Fetcher
	log     zerolog.Logger

	// authenticated is written into the row and must match Synapse's
	// enable_authenticated_media, or the media becomes visible on the legacy
	// endpoints when it should not be, or invisible when it should.
	authenticated bool

	// inflight collapses concurrent requests for the same media within this
	// process. Synapse's own Linearizer is a per-process dict too, so this is
	// the same guarantee it has -- across processes, the unique constraint is
	// what actually protects us.
	inflight singleflight.Group
}

func NewRemoteFetcher(db *DB, paths *MediaPaths, fetcher *Fetcher, authenticated bool, log zerolog.Logger) *RemoteFetcher {
	return &RemoteFetcher{
		db: db, paths: paths, fetcher: fetcher,
		authenticated: authenticated, log: log,
	}
}

// FetchAndStore downloads remote media, writes it into the media store and
// inserts its row, returning the row that ended up winning.
//
// A returned error means the caller should fall back to proxying: nothing has
// been written that Synapse would trip over.
func (rf *RemoteFetcher) FetchAndStore(ctx context.Context, origin, mediaID string) (*RemoteMedia, error) {
	key := origin + "/" + mediaID
	res, err, _ := rf.inflight.Do(key, func() (any, error) {
		return rf.fetchAndStore(ctx, origin, mediaID)
	})
	if err != nil {
		return nil, err
	}
	return res.(*RemoteMedia), nil
}

func (rf *RemoteFetcher) fetchAndStore(ctx context.Context, origin, mediaID string) (*RemoteMedia, error) {
	// Another request may have stored it while this one queued.
	if existing, err := rf.db.GetRemoteMedia(ctx, origin, mediaID); err == nil {
		if existing.Length != nil {
			if path, perr := rf.paths.RemoteMedia(origin, existing.FilesystemID); perr == nil {
				if _, serr := os.Stat(path); serr == nil {
					return existing, nil
				}
			}
		}
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	fsID := newFilesystemID()
	path, err := rf.paths.RemoteMedia(origin, fsID)
	if err != nil {
		return nil, fmt.Errorf("building media path: %w", err)
	}
	// Every write goes through the guard, so a bug in path construction cannot
	// put us outside the remote cache directories.
	if err := rf.paths.WritablePath(path); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating media directory: %w", err)
	}

	info, tmpName, err := rf.download(ctx, origin, mediaID, path)
	if err != nil {
		return nil, err
	}

	// 1. File into place, complete and durable, before anything references it.
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return nil, fmt.Errorf("installing media file: %w", err)
	}

	media := &RemoteMedia{
		Origin:        origin,
		MediaID:       mediaID,
		MediaType:     info.MediaType,
		Length:        &info.Length,
		UploadName:    info.UploadName,
		FilesystemID:  fsID,
		Authenticated: rf.authenticated,
	}

	// 2. Row second.
	won, err := rf.db.StoreRemoteMedia(ctx, media, info.SHA256, rf.authenticated)
	if err != nil {
		// The file is on disk but unreferenced. Remove it rather than leave
		// an orphan: nothing can find it, and the next attempt makes its own.
		_ = os.Remove(path)
		return nil, err
	}
	if !won {
		// Another writer got there first. Discard our copy and defer to
		// theirs, exactly as Synapse's _store_remote_media_with_cleanup does.
		// Their filesystem_id is the one other processes are already holding.
		_ = os.Remove(path)
		existing, err := rf.db.GetRemoteMedia(ctx, origin, mediaID)
		if err != nil {
			return nil, fmt.Errorf("re-reading remote media after losing a race: %w", err)
		}
		rf.log.Debug().Str("origin", origin).Str("media_id", mediaID).
			Msg("Another writer stored this media first")
		return existing, nil
	}

	rf.log.Info().
		Str("origin", origin).Str("media_id", mediaID).
		Int64("bytes", info.Length).Str("media_type", info.MediaType).
		Msg("Fetched remote media")
	return media, nil
}

// download streams the media into a temporary file beside its final path and
// returns the temp file's name. Writing to a temp file and renaming means a
// crash mid-download cannot leave a truncated file that looks complete --
// Synapse writes in place and does not have that property.
func (rf *RemoteFetcher) download(ctx context.Context, origin, mediaID, finalPath string) (*FetchedMedia, string, error) {
	tmp, err := os.CreateTemp(filepath.Dir(finalPath), ".incoming-*")
	if err != nil {
		return nil, "", fmt.Errorf("creating temporary file: %w", err)
	}
	tmpName := tmp.Name()

	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}

	info, err := rf.fetcher.Fetch(ctx, origin, mediaID, tmp)
	if err != nil {
		cleanup()
		return nil, "", err
	}
	// Durability before the rename: the row that follows is a promise that
	// these bytes exist.
	if err := tmp.Sync(); err != nil {
		cleanup()
		return nil, "", fmt.Errorf("syncing media file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return nil, "", fmt.Errorf("closing media file: %w", err)
	}
	// Synapse's files are 0644; ours are 0600 from CreateTemp.
	if err := os.Chmod(tmpName, 0o644); err != nil {
		_ = os.Remove(tmpName)
		return nil, "", fmt.Errorf("setting media file permissions: %w", err)
	}
	return info, tmpName, nil
}

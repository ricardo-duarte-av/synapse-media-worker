package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"time"

	"github.com/rs/zerolog"
)

// thumbcache.go is the worker's own thumbnail store.
//
// It exists because Synapse's media store is mounted read-only: thumbnails the
// worker generates have to live somewhere else. Nothing in here ever touches
// media_store.
//
// There is no index and no database. The cache key determines the path, and
// everything needed to serve an entry (content type, dimensions) is implied by
// the request that produced the key, so a stat is the only metadata lookup.

// ThumbnailCache stores generated thumbnails on disk under its own directory.
type ThumbnailCache struct {
	dir      string
	maxBytes int64
	log      zerolog.Logger
}

func NewThumbnailCache(cfg CacheConfig, log zerolog.Logger) (*ThumbnailCache, error) {
	dir := filepath.Join(cleanAbs(cfg.Dir), "thumbs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating thumbnail cache directory: %w", err)
	}
	return &ThumbnailCache{dir: dir, maxBytes: cfg.MaxBytes, log: log}, nil
}

// cacheKey derives the storage key. The origin is empty for local media.
//
// The request's *requested* dimensions are used rather than the post-aspect
// output dimensions, because that is what the exact-match lookup compares
// against.
func cacheKey(origin, mediaID string, req ThumbnailRequest) string {
	h := sha256.New()
	write := func(s string) {
		_, _ = h.Write([]byte(s))
		_, _ = h.Write([]byte{0})
	}
	write(origin)
	write(mediaID)
	write(strconv.Itoa(req.Width) + "x" + strconv.Itoa(req.Height))
	write(req.Method)
	write(req.Type)
	write(strconv.FormatBool(req.Animated))
	return hex.EncodeToString(h.Sum(nil))
}

func (c *ThumbnailCache) pathFor(key string) string {
	return filepath.Join(c.dir, key[0:2], key[2:4], key)
}

// Open returns an open handle to a cached thumbnail, or false if it is absent.
func (c *ThumbnailCache) Open(key string) (*os.File, bool) {
	f, err := os.Open(c.pathFor(key))
	if err != nil {
		return nil, false
	}
	return f, true
}

// Put stores a thumbnail. It writes to a temporary file in the same directory
// and renames it into place, so a crash or a concurrent reader never observes a
// truncated thumbnail.
func (c *ThumbnailCache) Put(key string, data []byte) error {
	path := c.pathFor(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating cache directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return fmt.Errorf("creating temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		// Best-effort cleanup; a successful rename makes this a no-op.
		_ = os.Remove(tmpName)
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing thumbnail: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing thumbnail: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("installing thumbnail: %w", err)
	}
	return nil
}

// Delete removes a cached entry, used when the source media is no longer
// servable.
func (c *ThumbnailCache) Delete(key string) {
	_ = os.Remove(c.pathFor(key))
}

// entry is a candidate for eviction.
type entry struct {
	path  string
	size  int64
	atime int64
}

// Sweep evicts least-recently-used entries until the cache is under its size
// limit. It is safe to run while requests are being served: a file deleted
// under an open handle stays readable until that handle closes.
func (c *ThumbnailCache) Sweep() (evicted int, freed int64, err error) {
	var entries []entry
	var total int64

	walkErr := filepath.WalkDir(c.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory that vanished mid-walk is not a failure.
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		e := entry{path: path, size: info.Size(), atime: accessTime(info)}
		entries = append(entries, e)
		total += e.size
		return nil
	})
	if walkErr != nil {
		return 0, 0, fmt.Errorf("walking cache: %w", walkErr)
	}

	if c.maxBytes <= 0 || total <= c.maxBytes {
		return 0, 0, nil
	}

	// Oldest access first.
	slices.SortFunc(entries, func(a, b entry) int {
		return int(a.atime - b.atime)
	})

	for _, e := range entries {
		if total <= c.maxBytes {
			break
		}
		if err := os.Remove(e.path); err != nil {
			continue
		}
		total -= e.size
		freed += e.size
		evicted++
	}
	return evicted, freed, nil
}

// RunJanitor sweeps on an interval until stop is closed.
func (c *ThumbnailCache) RunJanitor(interval time.Duration, stop <-chan struct{}) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			evicted, freed, err := c.Sweep()
			if err != nil {
				c.log.Warn().Err(err).Msg("Thumbnail cache sweep failed")
				continue
			}
			if evicted > 0 {
				c.log.Info().Int("evicted", evicted).Int64("freed_bytes", freed).
					Msg("Evicted thumbnails from cache")
			}
		case <-stop:
			return
		}
	}
}

// accessTime reads atime, which is what makes eviction least-recently-*used*
// rather than least-recently-written. The cache volume must not be mounted
// noatime for this to be meaningful; mtime is used as a fallback.
func accessTime(info fs.FileInfo) int64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return st.Atim.Sec
	}
	return info.ModTime().Unix()
}

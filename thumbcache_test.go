package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func newTestCache(t *testing.T, maxBytes int64) *ThumbnailCache {
	t.Helper()
	c, err := NewThumbnailCache(CacheConfig{Dir: t.TempDir(), MaxBytes: maxBytes}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCachePutAndOpen(t *testing.T) {
	c := newTestCache(t, 1<<20)
	key := cacheKey("", "abc123", ThumbnailRequest{Width: 96, Height: 96, Method: "crop", Type: typePNG})
	if _, ok := c.Open(key); ok {
		t.Fatal("empty cache returned a hit")
	}
	if err := c.Put(key, []byte("thumbnail-bytes")); err != nil {
		t.Fatal(err)
	}
	f, ok := c.Open(key)
	if !ok {
		t.Fatal("stored entry not found")
	}
	defer func() { _ = f.Close() }()
	got, err := readAllFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "thumbnail-bytes" {
		t.Errorf("got %q", got)
	}
}

// Every field of the request must participate in the key, or one thumbnail
// would be served in place of another.
func TestCacheKeyDistinguishesEveryField(t *testing.T) {
	base := ThumbnailRequest{Width: 96, Height: 96, Method: "crop", Type: typePNG}
	seen := map[string]string{}
	add := func(label, key string) {
		if prev, ok := seen[key]; ok {
			t.Errorf("%s collides with %s", label, prev)
		}
		seen[key] = label
	}
	add("base", cacheKey("", "media1", base))
	add("origin", cacheKey("other.com", "media1", base))
	add("mediaID", cacheKey("", "media2", base))

	w := base
	w.Width = 97
	add("width", cacheKey("", "media1", w))
	h := base
	h.Height = 97
	add("height", cacheKey("", "media1", h))
	m := base
	m.Method = "scale"
	add("method", cacheKey("", "media1", m))
	ty := base
	ty.Type = typeJPEG
	add("type", cacheKey("", "media1", ty))
	an := base
	an.Animated = true
	add("animated", cacheKey("", "media1", an))
}

// Width and height must not be able to run together into the same key: a
// 32x321 request and a 323x21 request are different thumbnails.
func TestCacheKeyDimensionsAreDelimited(t *testing.T) {
	a := cacheKey("", "m", ThumbnailRequest{Width: 32, Height: 321, Method: "crop", Type: typePNG})
	b := cacheKey("", "m", ThumbnailRequest{Width: 323, Height: 21, Method: "crop", Type: typePNG})
	if a == b {
		t.Error("dimension fields are ambiguous in the cache key")
	}
}

// The origin and media ID must not be able to run together either.
func TestCacheKeyFieldsAreDelimited(t *testing.T) {
	req := ThumbnailRequest{Width: 96, Height: 96, Method: "crop", Type: typePNG}
	if cacheKey("ab", "cd", req) == cacheKey("a", "bcd", req) {
		t.Error("origin and media ID are ambiguous in the cache key")
	}
}

func TestSweepEvictsLeastRecentlyUsed(t *testing.T) {
	c := newTestCache(t, 300)
	payload := make([]byte, 100)

	keys := []string{
		cacheKey("", "oldest", ThumbnailRequest{Width: 1, Height: 1, Method: "crop", Type: typePNG}),
		cacheKey("", "middle", ThumbnailRequest{Width: 2, Height: 2, Method: "crop", Type: typePNG}),
		cacheKey("", "newest", ThumbnailRequest{Width: 3, Height: 3, Method: "crop", Type: typePNG}),
		cacheKey("", "newer2", ThumbnailRequest{Width: 4, Height: 4, Method: "crop", Type: typePNG}),
	}
	for _, k := range keys {
		if err := c.Put(k, payload); err != nil {
			t.Fatal(err)
		}
	}
	// Stagger access times so eviction order is well defined.
	now := time.Now()
	for i, k := range keys {
		at := now.Add(time.Duration(i-len(keys)) * time.Hour)
		if err := os.Chtimes(c.pathFor(k), at, at); err != nil {
			t.Fatal(err)
		}
	}

	evicted, freed, err := c.Sweep()
	if err != nil {
		t.Fatal(err)
	}
	if evicted == 0 {
		t.Fatal("nothing evicted despite exceeding the limit")
	}
	if freed <= 0 {
		t.Error("freed no bytes")
	}
	// The oldest must go first and the newest must survive.
	if _, ok := c.Open(keys[0]); ok {
		t.Error("least recently used entry survived")
	}
	if _, ok := c.Open(keys[len(keys)-1]); !ok {
		t.Error("most recently used entry was evicted")
	}
}

func TestSweepDoesNothingUnderLimit(t *testing.T) {
	c := newTestCache(t, 1<<20)
	key := cacheKey("", "m", ThumbnailRequest{Width: 1, Height: 1, Method: "crop", Type: typePNG})
	if err := c.Put(key, make([]byte, 100)); err != nil {
		t.Fatal(err)
	}
	evicted, _, err := c.Sweep()
	if err != nil {
		t.Fatal(err)
	}
	if evicted != 0 {
		t.Errorf("evicted %d entries while under the limit", evicted)
	}
}

// A failed write must not leave a partial file that would later be served as a
// valid thumbnail.
func TestPutIsAtomic(t *testing.T) {
	c := newTestCache(t, 1<<20)
	key := cacheKey("", "m", ThumbnailRequest{Width: 1, Height: 1, Method: "crop", Type: typePNG})
	if err := c.Put(key, []byte("data")); err != nil {
		t.Fatal(err)
	}
	// No temporary files should be left behind.
	var temps int
	_ = filepath.Walk(c.dir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && filepath.Base(path)[0] == '.' {
			temps++
		}
		return nil
	})
	if temps != 0 {
		t.Errorf("%d temporary files left behind", temps)
	}
}

func TestDelete(t *testing.T) {
	c := newTestCache(t, 1<<20)
	key := cacheKey("", "m", ThumbnailRequest{Width: 1, Height: 1, Method: "crop", Type: typePNG})
	if err := c.Put(key, []byte("x")); err != nil {
		t.Fatal(err)
	}
	c.Delete(key)
	if _, ok := c.Open(key); ok {
		t.Error("entry survived deletion")
	}
	// Deleting a missing entry must not panic.
	c.Delete(key)
}

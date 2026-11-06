package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testBase = "/data/media_store"

func TestLocalMedia(t *testing.T) {
	p := NewMediaPaths(testBase)
	got, err := p.LocalMedia("AbCdEfGhIjKlMnOpQrStUvWx")
	if err != nil {
		t.Fatal(err)
	}
	want := testBase + "/local_content/Ab/Cd/EfGhIjKlMnOpQrStUvWx"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestLocalThumbnail(t *testing.T) {
	p := NewMediaPaths(testBase)
	got, err := p.LocalThumbnail("AbCdEfGhIjKlMnOpQrStUvWx", 96, 96, "image/png", "crop")
	if err != nil {
		t.Fatal(err)
	}
	want := testBase + "/local_thumbnails/Ab/Cd/EfGhIjKlMnOpQrStUvWx/96-96-image-png-crop"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestRemoteMedia(t *testing.T) {
	p := NewMediaPaths(testBase)
	got, err := p.RemoteMedia("matrix.org", "QwErTyUiOpAsDfGhJkLzXc")
	if err != nil {
		t.Fatal(err)
	}
	want := testBase + "/remote_content/matrix.org/Qw/Er/TyUiOpAsDfGhJkLzXc"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

// The remote thumbnail directory is singular even though the table is plural.
func TestRemoteThumbnailDirectoryIsSingular(t *testing.T) {
	p := NewMediaPaths(testBase)
	got, err := p.RemoteThumbnail("matrix.org", "QwErTyUiOpAsDfGhJkLzXc", 320, 240, "image/jpeg", "scale")
	if err != nil {
		t.Fatal(err)
	}
	want := testBase + "/remote_thumbnail/matrix.org/Qw/Er/TyUiOpAsDfGhJkLzXc/320-240-image-jpeg-scale"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
	if strings.Contains(got, "remote_thumbnails/") {
		t.Error("used plural remote_thumbnails directory")
	}
}

func TestRemoteThumbnailLegacyHasNoMethod(t *testing.T) {
	p := NewMediaPaths(testBase)
	got, err := p.RemoteThumbnailLegacy("matrix.org", "QwErTyUiOpAsDfGhJkLzXc", 320, 240, "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	want := testBase + "/remote_thumbnail/matrix.org/Qw/Er/TyUiOpAsDfGhJkLzXc/320-240-image-jpeg"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

// url_cache IDs beginning with a date use a different split, and the hyphen
// after the date is dropped rather than becoming a directory separator.
func TestURLCacheNewFormat(t *testing.T) {
	p := NewMediaPaths(testBase)
	got, err := p.URLCache("2017-09-28-fsdRDt24DS234dsf")
	if err != nil {
		t.Fatal(err)
	}
	want := testBase + "/url_cache/2017-09-28/fsdRDt24DS234dsf"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestURLCacheOldFormat(t *testing.T) {
	p := NewMediaPaths(testBase)
	got, err := p.URLCache("AbCdEfGhIjKlMnOp")
	if err != nil {
		t.Fatal(err)
	}
	want := testBase + "/url_cache/Ab/Cd/EfGhIjKlMnOp"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestURLCacheThumbnailNewFormat(t *testing.T) {
	p := NewMediaPaths(testBase)
	got, err := p.URLCacheThumbnail("2017-09-28-fsdRDt24DS234dsf", 800, 600, "image/png", "scale")
	if err != nil {
		t.Fatal(err)
	}
	want := testBase + "/url_cache_thumbnails/2017-09-28/fsdRDt24DS234dsf/800-600-image-png-scale"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

// Server names legitimately contain ports and IPv6 literals.
func TestServerNameWithPortAndIPv6(t *testing.T) {
	p := NewMediaPaths(testBase)
	for _, server := range []string{"example.com:8448", "[::1]:8448"} {
		if _, err := p.RemoteMedia(server, "QwErTyUiOpAsDf"); err != nil {
			t.Errorf("server %q rejected: %v", server, err)
		}
	}
}

func TestRejectsTraversal(t *testing.T) {
	p := NewMediaPaths(testBase)
	bad := []string{"..", ".", "", "../../etc/passwd", "ab/cd", "a\x00b", "abc def"}
	for _, id := range bad {
		if got, err := p.LocalMedia(id); err == nil {
			t.Errorf("media ID %q accepted, produced %q", id, got)
		}
	}
	for _, server := range []string{"..", "../..", "a/b", ""} {
		if got, err := p.RemoteMedia(server, "QwErTyUiOpAsDf"); err == nil {
			t.Errorf("server name %q accepted, produced %q", server, got)
		}
	}
}

// A slash in the content type would otherwise inject a directory separator
// into the thumbnail filename.
func TestRejectsContentTypeInjection(t *testing.T) {
	p := NewMediaPaths(testBase)
	for _, ct := range []string{"image/../../png", "image", "/png", "image/"} {
		if got, err := p.LocalThumbnail("AbCdEfGhIjKl", 96, 96, ct, "crop"); err == nil {
			t.Errorf("content type %q accepted, produced %q", ct, got)
		}
	}
	if got, err := p.LocalThumbnail("AbCdEfGhIjKl", 96, 96, "image/png", "../crop"); err == nil {
		t.Errorf("method with traversal accepted, produced %q", got)
	}
}

func TestShortIDsRejected(t *testing.T) {
	p := NewMediaPaths(testBase)
	if _, err := p.LocalMedia("abcd"); err == nil {
		t.Error("four-character media ID accepted")
	}
}

// Golden paths observed in a live Synapse 1.x media store. These are the real
// on-disk layout, not a restatement of the implementation.
func TestAgainstObservedLiveLayout(t *testing.T) {
	p := NewMediaPaths(testBase)

	if got, _ := p.LocalMedia("GkxNhgIwUbxldpASFPparrFZ"); got != testBase+"/local_content/Gk/xN/hgIwUbxldpASFPparrFZ" {
		t.Errorf("local content: got %q", got)
	}
	if got, _ := p.LocalThumbnail("raLQcXeILTLJeNFYQhEvAQVp", 32, 32, "image/jpeg", "crop"); got != testBase+"/local_thumbnails/ra/LQ/cXeILTLJeNFYQhEvAQVp/32-32-image-jpeg-crop" {
		t.Errorf("local thumbnail: got %q", got)
	}
	// A non-default size, generated on demand because dynamic_thumbnails is on.
	if got, _ := p.LocalThumbnail("raLQcXeILTLJeNFYQhEvAQVp", 320, 180, "image/jpeg", "scale"); got != testBase+"/local_thumbnails/ra/LQ/cXeILTLJeNFYQhEvAQVp/320-180-image-jpeg-scale" {
		t.Errorf("dynamic local thumbnail: got %q", got)
	}
	if got, _ := p.RemoteThumbnail("airstrikeivanov.com", "THDmLkhukOtkXhSZmRVQKjdq", 600, 600, "image/png", "scale"); got != testBase+"/remote_thumbnail/airstrikeivanov.com/TH/Dm/LkhukOtkXhSZmRVQKjdq/600-600-image-png-scale" {
		t.Errorf("remote thumbnail: got %q", got)
	}
	if got, _ := p.URLCache("2026-03-21-yVDnyGLNTluDrjPE"); got != testBase+"/url_cache/2026-03-21/yVDnyGLNTluDrjPE" {
		t.Errorf("url cache: got %q", got)
	}
}

// The read-only mount is gone once the worker fetches media itself, so the
// remaining guard is this one: media the server owns must stay untouchable.
func TestWritablePathAllowsOnlyRemoteDirectories(t *testing.T) {
	p := NewMediaPaths(testBase)

	allowed := []string{
		testBase + "/remote_content/ab/cd/efgh",
		testBase + "/remote_thumbnail/example.com/ab/cd/efgh/96-96-image-png-crop",
	}
	for _, path := range allowed {
		if err := p.WritablePath(path); err != nil {
			t.Errorf("%q should be writable: %v", path, err)
		}
	}

	refused := []string{
		testBase + "/local_content/ab/cd/efgh",
		testBase + "/local_thumbnails/ab/cd/efgh/96-96-image-png-crop",
		testBase + "/url_cache/2026-01-01/abc",
		testBase + "/url_cache_thumbnails/2026-01-01/abc/96-96-image-png-crop",
		testBase,
		"/etc/passwd",
		testBase + "/../etc/passwd",
	}
	for _, path := range refused {
		if err := p.WritablePath(path); err == nil {
			t.Errorf("%q should NOT be writable", path)
		}
	}
}

// A prefix that merely starts with an allowed name is not an allowed directory.
func TestWritablePathIsNotFooledByPrefixes(t *testing.T) {
	p := NewMediaPaths(testBase)
	for _, path := range []string{
		testBase + "/remote_content_evil/ab/cd/ef",
		testBase + "/remote_thumbnails/ab/cd/ef", // note: plural, not Synapse's name
	} {
		if err := p.WritablePath(path); err == nil {
			t.Errorf("%q should NOT be writable", path)
		}
	}
}

// Every path the builder produces for remote media must be writable, and every
// path it produces for local media must not be.
func TestWritablePathAgreesWithTheBuilders(t *testing.T) {
	p := NewMediaPaths(testBase)

	remote, _ := p.RemoteMedia("example.com", "AbCdEfGhIjKlMnOpQrStUvWx")
	if err := p.WritablePath(remote); err != nil {
		t.Errorf("RemoteMedia path not writable: %v", err)
	}
	remoteThumb, _ := p.RemoteThumbnail("example.com", "AbCdEfGhIjKlMnOpQrStUvWx", 96, 96, "image/png", "crop")
	if err := p.WritablePath(remoteThumb); err != nil {
		t.Errorf("RemoteThumbnail path not writable: %v", err)
	}

	local, _ := p.LocalMedia("AbCdEfGhIjKlMnOpQrStUvWx")
	if err := p.WritablePath(local); err == nil {
		t.Error("LocalMedia path must not be writable")
	}
	urlCache, _ := p.URLCache("2026-03-21-yVDnyGLNTluDrjPE")
	if err := p.WritablePath(urlCache); err == nil {
		t.Error("URLCache path must not be writable")
	}
}

// Uploads need local_content, but only when that feature is on, and url_cache
// must stay unwritable in every configuration.
func TestWritablePathWidensForUploads(t *testing.T) {
	p := NewMediaPaths(testBase)
	local := testBase + "/local_content/ab/cd/efgh"
	localThumb := testBase + "/local_thumbnails/ab/cd/efgh/96-96-image-png-crop"

	if err := p.WritablePath(local); err == nil {
		t.Error("local_content should not be writable before uploads are enabled")
	}

	p.AllowUploadWrites()
	if err := p.WritablePath(local); err != nil {
		t.Errorf("local_content should be writable with uploads on: %v", err)
	}
	if err := p.WritablePath(localThumb); err != nil {
		t.Errorf("local_thumbnails should be writable with uploads on: %v", err)
	}
	// The remote cache stays writable.
	if err := p.WritablePath(testBase + "/remote_content/ab/cd/ef"); err != nil {
		t.Errorf("remote_content should remain writable: %v", err)
	}
	// The URL preview cache never becomes writable.
	for _, path := range []string{
		testBase + "/url_cache/2026-01-01/abc",
		testBase + "/url_cache_thumbnails/2026-01-01/abc/96-96-image-png-crop",
	} {
		if err := p.WritablePath(path); err == nil {
			t.Errorf("%q must never be writable", path)
		}
	}
}

// Temp files go in the .tmp of the destination's top-level directory, and only
// where the destination itself may be written.
func TestCreateTempLocation(t *testing.T) {
	base := t.TempDir()
	paths := NewMediaPaths(base)

	final, err := paths.RemoteMedia("example.org", "AbCdEfGhIjKlMnOpQrStUvWx")
	if err != nil {
		t.Fatal(err)
	}
	f, err := paths.CreateTemp(final, ".incoming-*")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if want := filepath.Join(base, "remote_content", tempDirName); filepath.Dir(f.Name()) != want {
		t.Errorf("temp file in %s, want %s", filepath.Dir(f.Name()), want)
	}

	local, err := paths.LocalMedia("AbCdEfGhIjKlMnOpQrStUvWx")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := paths.CreateTemp(local, ".incoming-*"); err == nil {
		t.Error("CreateTemp allowed a local_content write with uploads off")
	}
	if _, err := os.Stat(filepath.Join(base, "local_content")); !os.IsNotExist(err) {
		t.Error("a refused CreateTemp still created directories under local_content")
	}
	for _, refused := range []string{
		filepath.Join(base, "url_cache", "ab", "cd", "x"),
		filepath.Join(base, "remote_content"), // not inside a media directory
		filepath.Join(base, "..", "elsewhere", "x"),
	} {
		if _, err := paths.CreateTemp(refused, ".incoming-*"); err == nil {
			t.Errorf("CreateTemp(%s) was allowed", refused)
		}
	}
}

// assertNoTempFiles fails if a temp file survives beside finalPath or in the
// .tmp directory of any directory above it.
func assertNoTempFiles(t *testing.T, finalPath string) {
	t.Helper()
	if entries, _ := os.ReadDir(filepath.Dir(finalPath)); entries != nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".incoming-") || strings.HasPrefix(e.Name(), ".thumb-") {
				t.Errorf("temporary file left beside the media: %s", e.Name())
			}
		}
	}
	for dir := filepath.Dir(finalPath); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		entries, _ := os.ReadDir(filepath.Join(dir, tempDirName))
		for _, e := range entries {
			t.Errorf("temporary file left behind: %s", filepath.Join(dir, tempDirName, e.Name()))
		}
	}
}

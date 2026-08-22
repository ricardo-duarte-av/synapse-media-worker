package main

import (
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

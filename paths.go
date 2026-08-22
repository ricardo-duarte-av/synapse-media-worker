package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// paths.go is a port of Synapse's synapse/media/filepath.py. The layout must
// match byte for byte or the worker will not find files Synapse wrote.
//
// Two naming traps worth remembering:
//   - the remote thumbnail *directory* is "remote_thumbnail" (singular) even
//     though the table is remote_media_cache_thumbnails (plural), while the
//     local one is "local_thumbnails" (plural);
//   - remote thumbnails have a legacy filename variant with no method suffix,
//     which must be probed as a fallback.

// newFormatIDRe matches url_cache media IDs of the form 2017-09-28-fsdRDt24DS.
var newFormatIDRe = regexp.MustCompile(`^\d\d\d\d-\d\d-\d\d`)

// MediaPaths builds absolute paths inside Synapse's media store.
type MediaPaths struct {
	base string
}

func NewMediaPaths(base string) *MediaPaths {
	return &MediaPaths{base: cleanAbs(base)}
}

func cleanAbs(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	return filepath.Clean(abs)
}

// validPathComponentChar mirrors Synapse's _validate_path_component allowlist.
// Colons and brackets are permitted because server names carry ports and IPv6
// literals.
func validPathComponentChar(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z':
		return true
	case r >= 'A' && r <= 'Z':
		return true
	case r >= '0' && r <= '9':
		return true
	}
	switch r {
	case '_', '-', '.', '[', ']', ':':
		return true
	}
	return false
}

func validatePathComponent(component string) error {
	if component == "" || component == "." || component == ".." {
		return fmt.Errorf("invalid path component %q", component)
	}
	for _, r := range component {
		if !validPathComponentChar(r) {
			return fmt.Errorf("invalid character %q in path component %q", r, component)
		}
	}
	return nil
}

// shard splits an ID into Synapse's two-two-rest directory structure.
func shard(id string) (string, string, string, error) {
	if err := validatePathComponent(id); err != nil {
		return "", "", "", err
	}
	if len(id) < 5 {
		return "", "", "", fmt.Errorf("media ID %q is too short to shard", id)
	}
	return id[0:2], id[2:4], id[4:], nil
}

// thumbnailFileName builds Synapse's thumbnail filename:
// "<width>-<height>-<toplevel>-<subtype>-<method>".
func thumbnailFileName(width, height int, contentType, method string) (string, error) {
	top, sub, ok := strings.Cut(contentType, "/")
	if !ok || top == "" || sub == "" {
		return "", fmt.Errorf("thumbnail content type %q is not a media type", contentType)
	}
	name := fmt.Sprintf("%d-%d-%s-%s-%s", width, height, top, sub, method)
	if err := validatePathComponent(name); err != nil {
		return "", err
	}
	return name, nil
}

// legacyThumbnailFileName is the pre-method remote thumbnail filename.
func legacyThumbnailFileName(width, height int, contentType string) (string, error) {
	top, sub, ok := strings.Cut(contentType, "/")
	if !ok || top == "" || sub == "" {
		return "", fmt.Errorf("thumbnail content type %q is not a media type", contentType)
	}
	name := fmt.Sprintf("%d-%d-%s-%s", width, height, top, sub)
	if err := validatePathComponent(name); err != nil {
		return "", err
	}
	return name, nil
}

// join assembles a path from already-validated components and then verifies
// the result is still inside the base directory. The jail check is defence in
// depth: validatePathComponent should already make traversal impossible.
func (p *MediaPaths) join(components ...string) (string, error) {
	full := filepath.Clean(filepath.Join(append([]string{p.base}, components...)...))
	if full != p.base && !strings.HasPrefix(full, p.base+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes media store %q", full, p.base)
	}
	return full, nil
}

// LocalMedia returns local_content/<aa>/<bb>/<rest>.
func (p *MediaPaths) LocalMedia(mediaID string) (string, error) {
	a, b, rest, err := shard(mediaID)
	if err != nil {
		return "", err
	}
	return p.join("local_content", a, b, rest)
}

// LocalThumbnail returns local_thumbnails/<aa>/<bb>/<rest>/<filename>.
func (p *MediaPaths) LocalThumbnail(mediaID string, width, height int, contentType, method string) (string, error) {
	a, b, rest, err := shard(mediaID)
	if err != nil {
		return "", err
	}
	name, err := thumbnailFileName(width, height, contentType, method)
	if err != nil {
		return "", err
	}
	return p.join("local_thumbnails", a, b, rest, name)
}

// RemoteMedia returns remote_content/<server>/<aa>/<bb>/<rest>. The ID here is
// the row's filesystem_id, not its media_id.
func (p *MediaPaths) RemoteMedia(serverName, filesystemID string) (string, error) {
	if err := validatePathComponent(serverName); err != nil {
		return "", err
	}
	a, b, rest, err := shard(filesystemID)
	if err != nil {
		return "", err
	}
	return p.join("remote_content", serverName, a, b, rest)
}

// RemoteThumbnail returns the current-format remote thumbnail path. Note the
// singular "remote_thumbnail" directory.
func (p *MediaPaths) RemoteThumbnail(serverName, filesystemID string, width, height int, contentType, method string) (string, error) {
	dir, err := p.remoteThumbnailDir(serverName, filesystemID)
	if err != nil {
		return "", err
	}
	name, err := thumbnailFileName(width, height, contentType, method)
	if err != nil {
		return "", err
	}
	return p.join(dir, name)
}

// RemoteThumbnailLegacy returns the pre-method remote thumbnail path, which
// still exists for media cached by older Synapse versions.
func (p *MediaPaths) RemoteThumbnailLegacy(serverName, filesystemID string, width, height int, contentType string) (string, error) {
	dir, err := p.remoteThumbnailDir(serverName, filesystemID)
	if err != nil {
		return "", err
	}
	name, err := legacyThumbnailFileName(width, height, contentType)
	if err != nil {
		return "", err
	}
	return p.join(dir, name)
}

func (p *MediaPaths) remoteThumbnailDir(serverName, filesystemID string) (string, error) {
	if err := validatePathComponent(serverName); err != nil {
		return "", err
	}
	a, b, rest, err := shard(filesystemID)
	if err != nil {
		return "", err
	}
	return filepath.Join("remote_thumbnail", serverName, a, b, rest), nil
}

// URLCache returns the path for a url_cache preview's original file.
func (p *MediaPaths) URLCache(mediaID string) (string, error) {
	if err := validatePathComponent(mediaID); err != nil {
		return "", err
	}
	if newFormatIDRe.MatchString(mediaID) {
		if len(mediaID) < 12 {
			return "", fmt.Errorf("url cache media ID %q is too short", mediaID)
		}
		// The separator after the date is dropped, hence [11:] not [10:].
		return p.join("url_cache", mediaID[:10], mediaID[11:])
	}
	a, b, rest, err := shard(mediaID)
	if err != nil {
		return "", err
	}
	return p.join("url_cache", a, b, rest)
}

// URLCacheThumbnail returns the path for a url_cache preview's thumbnail.
func (p *MediaPaths) URLCacheThumbnail(mediaID string, width, height int, contentType, method string) (string, error) {
	if err := validatePathComponent(mediaID); err != nil {
		return "", err
	}
	name, err := thumbnailFileName(width, height, contentType, method)
	if err != nil {
		return "", err
	}
	if newFormatIDRe.MatchString(mediaID) {
		if len(mediaID) < 12 {
			return "", fmt.Errorf("url cache media ID %q is too short", mediaID)
		}
		return p.join("url_cache_thumbnails", mediaID[:10], mediaID[11:], name)
	}
	a, b, rest, err := shard(mediaID)
	if err != nil {
		return "", err
	}
	return p.join("url_cache_thumbnails", a, b, rest, name)
}

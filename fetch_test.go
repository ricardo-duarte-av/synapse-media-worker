package main

import (
	"bytes"
	"strings"
	"testing"
)

// upload_name comes from the file part's Content-Disposition. Synapse prefers
// filename*= and only accepts a bare filename= when it is ASCII.
func TestFilenameFromDisposition(t *testing.T) {
	cases := map[string]string{
		"inline; filename=cat.png":                "cat.png",
		"attachment; filename=\"my cat.png\"":     "my cat.png",
		"inline; filename*=utf-8''my%20cat.png":   "my cat.png",
		"inline; filename*=utf-8''%C3%A6ther.png": "æther.png",
		"attachment":        "",
		"":                  "",
		"inline; filename=": "",
	}
	for in, want := range cases {
		if got := filenameFromDisposition(in); got != want {
			t.Errorf("filenameFromDisposition(%q) = %q, want %q", in, got, want)
		}
	}
}

// A remote server chooses this value, so it must not be able to steer where we
// write or escape the media directory.
func TestFilenameFromDispositionStripsPaths(t *testing.T) {
	cases := map[string]string{
		`inline; filename="../../etc/passwd"`:   "passwd",
		`inline; filename="/etc/passwd"`:        "passwd",
		`inline; filename="..\\..\\windows\\x"`: "x",
		`inline; filename=".."`:                 "",
		`inline; filename="."`:                  "",
	}
	for in, want := range cases {
		if got := filenameFromDisposition(in); got != want {
			t.Errorf("filenameFromDisposition(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHeaderOrDefault(t *testing.T) {
	if got := headerOrDefault("", "application/octet-stream"); got != "application/octet-stream" {
		t.Errorf("got %q", got)
	}
	if got := headerOrDefault("   ", "application/octet-stream"); got != "application/octet-stream" {
		t.Errorf("whitespace-only header should fall back, got %q", got)
	}
	if got := headerOrDefault("image/png", "application/octet-stream"); got != "image/png" {
		t.Errorf("got %q", got)
	}
}

// The sha256 must be the hash of the raw bytes, lowercase hex, matching what
// Synapse stores -- admin quarantine resolves media by this value.
func TestCopyBodyHashesAndCounts(t *testing.T) {
	f := &Fetcher{maxUploadSize: 1 << 20}
	payload := []byte("the file bytes")
	var dst bytes.Buffer

	info, err := f.copyBody(bytes.NewReader(payload), &dst, &FetchedMedia{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dst.Bytes(), payload) {
		t.Errorf("wrote %q", dst.Bytes())
	}
	if info.Length != int64(len(payload)) {
		t.Errorf("length = %d, want %d", info.Length, len(payload))
	}
	// Synapse stores hashlib.sha256().hexdigest(): lowercase hex, 64 chars.
	// Verified against: printf 'the file bytes' | sha256sum
	const want = "725c32e479cadadc84938d7024bdff35c6c3a4209ebb5db438ebfe4667228b41"
	if info.SHA256 != want {
		t.Errorf("sha256 = %q, want %q", info.SHA256, want)
	}
	if strings.ToLower(info.SHA256) != info.SHA256 {
		t.Errorf("sha256 %q is not lowercase", info.SHA256)
	}
}

// A body larger than the limit must be refused even when Content-Length lied
// or was absent.
func TestCopyBodyRefusesOversizedBody(t *testing.T) {
	f := &Fetcher{maxUploadSize: 100}
	var dst bytes.Buffer
	_, err := f.copyBody(bytes.NewReader(make([]byte, 5000)), &dst, &FetchedMedia{})
	if err == nil {
		t.Fatal("oversized body accepted")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("unhelpful error: %v", err)
	}
}

// Exactly at the limit is allowed; one byte over is not.
func TestCopyBodyBoundary(t *testing.T) {
	f := &Fetcher{maxUploadSize: 100}
	var dst bytes.Buffer
	if _, err := f.copyBody(bytes.NewReader(make([]byte, 100)), &dst, &FetchedMedia{}); err != nil {
		t.Errorf("a body exactly at the limit was refused: %v", err)
	}
	dst.Reset()
	if _, err := f.copyBody(bytes.NewReader(make([]byte, 101)), &dst, &FetchedMedia{}); err == nil {
		t.Error("a body one byte over the limit was accepted")
	}
}

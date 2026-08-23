package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Synapse's parse_size uses binary multipliers. Getting this wrong by hand is
// what motivated reading homeserver.yaml in the first place: "100M" is
// 104857600, and this worker previously had 100000000 transcribed into its own
// config, refusing to thumbnail images Synapse would have accepted.
func TestParseSynapseSizeIsBinary(t *testing.T) {
	cases := map[any]int64{
		"100M":    104857600,
		"1000M":   1048576000,
		"50M":     52428800,
		"32M":     33554432,
		"87K":     89088,
		"1G":      1073741824,
		"1T":      1099511627776,
		"1024":    1024,
		1024:      1024,
		"  10M  ": 10485760,
		// Lowercase suffixes are accepted the same way.
		"10m": 10485760,
	}
	for in, want := range cases {
		got, err := parseSynapseSize(in)
		if err != nil {
			t.Errorf("parseSynapseSize(%v): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseSynapseSize(%v) = %d, want %d", in, got, want)
		}
	}
}

func TestParseSynapseSizeRejectsNonsense(t *testing.T) {
	for _, in := range []any{"", "abc", "10X5", nil, []string{"10M"}} {
		if _, err := parseSynapseSize(in); err == nil {
			t.Errorf("parseSynapseSize(%v) was accepted", in)
		}
	}
}

const sampleHomeserver = `
server_name: "example.com"
pid_file: /data/homeserver.pid
media_store_path: %s
signing_key_path: %s
dynamic_thumbnails: true
max_image_pixels: 100M
max_upload_size: 1000M
database:
  name: psycopg2
  args:
    user: synapse
    password: "s3cr3t"
    database: synapse-db
    host: "matrix-pgcat"
    port: "6432"
some_unknown_future_key:
  nested: [1, 2, 3]
`

func writeSynapseConfig(t *testing.T, storePath, keyPath string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "homeserver.yaml")
	body := strings.Replace(sampleHomeserver, "%s", storePath, 1)
	body = strings.Replace(body, "%s", keyPath, 1)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Unknown keys must be tolerated: homeserver.yaml is mostly none of this
// worker's business and varies between Synapse versions.
func TestLoadSynapseConfigIgnoresUnknownKeys(t *testing.T) {
	path := writeSynapseConfig(t, "/nonexistent/store", "/nonexistent/key")
	hs, err := LoadSynapseConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if hs.ServerName != "example.com" {
		t.Errorf("server_name = %q", hs.ServerName)
	}
	if hs.DynamicThumbnails == nil || !*hs.DynamicThumbnails {
		t.Error("dynamic_thumbnails not read")
	}
	// Absent from the sample, so it must stay nil rather than default to false.
	if hs.EnableAuthenticatedMedia != nil {
		t.Error("enable_authenticated_media should be nil when absent")
	}
}

func TestDeriveFillsInUnsetValues(t *testing.T) {
	store := t.TempDir()
	key := filepath.Join(t.TempDir(), "signing.key")
	if err := os.WriteFile(key, []byte("ed25519 a_test AAAA"), 0o600); err != nil {
		t.Fatal(err)
	}
	hsPath := writeSynapseConfig(t, store, key)

	cfg := defaultConfig()
	cfg.SynapseConfig = hsPath
	cfg.Cache.Dir = t.TempDir()
	cfg.Auth.WhoamiSocket = "/tmp/x.sock"
	if err := cfg.deriveFromSynapse(); err != nil {
		t.Fatal(err)
	}

	if cfg.ServerName != "example.com" {
		t.Errorf("server_name = %q", cfg.ServerName)
	}
	if got := cfg.Media.MaxImagePixelsOrDefault(); got != 104857600 {
		t.Errorf("max_image_pixels = %d, want 104857600 (100M binary)", got)
	}
	if got := cfg.Media.MaxUploadSizeOrDefault(); got != 1048576000 {
		t.Errorf("max_upload_size = %d, want 1048576000 (1000M binary)", got)
	}
	if !cfg.Media.DynamicThumbnailsEnabled() {
		t.Error("dynamic_thumbnails not derived")
	}
	if cfg.Media.StorePath != store {
		t.Errorf("store_path = %q, want %q", cfg.Media.StorePath, store)
	}
	if cfg.Media.SigningKeyPath != key {
		t.Errorf("signing_key_path = %q", cfg.Media.SigningKeyPath)
	}
	if !strings.Contains(cfg.Database.URI, "matrix-pgcat") {
		t.Errorf("database.uri = %q, want it derived", cfg.Database.URI)
	}
}

// Explicit configuration must always win: some of Synapse's values are wrong
// for this worker, notably a database host pointing at a pooler.
func TestExplicitConfigWinsOverSynapse(t *testing.T) {
	store := t.TempDir()
	key := filepath.Join(t.TempDir(), "signing.key")
	_ = os.WriteFile(key, []byte("ed25519 a_test AAAA"), 0o600)
	hsPath := writeSynapseConfig(t, store, key)

	explicitPixels := int64(7)
	cfg := defaultConfig()
	cfg.SynapseConfig = hsPath
	cfg.ServerName = "override.example"
	cfg.Media.MaxImagePixels = &explicitPixels
	cfg.Database.URI = "postgres://me@/db?host=/var/sockets"
	if err := cfg.deriveFromSynapse(); err != nil {
		t.Fatal(err)
	}
	if cfg.ServerName != "override.example" {
		t.Errorf("server_name = %q, want the explicit value", cfg.ServerName)
	}
	if cfg.Media.MaxImagePixelsOrDefault() != 7 {
		t.Errorf("max_image_pixels = %d, want the explicit 7", cfg.Media.MaxImagePixelsOrDefault())
	}
	if !strings.Contains(cfg.Database.URI, "/var/sockets") {
		t.Errorf("database.uri = %q, want the explicit socket", cfg.Database.URI)
	}
}

// homeserver.yaml records paths as they appear inside Synapse's container.
// Adopting one that does not exist here would fail later and confusingly.
func TestDeriveSkipsPathsThatDoNotResolve(t *testing.T) {
	hsPath := writeSynapseConfig(t, "/definitely/not/here", "/nor/this")
	cfg := defaultConfig()
	cfg.SynapseConfig = hsPath
	if err := cfg.deriveFromSynapse(); err != nil {
		t.Fatal(err)
	}
	if cfg.Media.StorePath != "" {
		t.Errorf("store_path = %q, want it left unset", cfg.Media.StorePath)
	}
	if len(cfg.derived.Skipped) != 2 {
		t.Errorf("expected both paths to be reported as skipped, got %v", cfg.derived.Skipped)
	}
}

// A deployment Synapse configures but this worker cannot honour must be called
// out rather than silently ignored.
func TestDeriveWarnsAboutUnsupportedSynapseFeatures(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "homeserver.yaml")
	body := `
server_name: "example.com"
dynamic_thumbnails: false
media_storage_providers:
  - module: s3_storage_provider.S3StorageProviderBackend
prevent_media_downloads_from:
  - evil.example
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.SynapseConfig = path
	cfg.Media.FetchRemote = true
	if err := cfg.deriveFromSynapse(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cfg.derived.Warnings, "\n")
	for _, want := range []string{"media_storage_providers", "prevent_media_downloads_from", "dynamic_thumbnails"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no warning about %s; got:\n%s", want, joined)
		}
	}
}

// The derived database URI must carry the credentials and the pooler details.
func TestDatabaseURIFromSynapseArgs(t *testing.T) {
	var hs SynapseConfig
	hs.Database.Args.User = "synapse"
	hs.Database.Args.Password = "p@ss word/!"
	hs.Database.Args.Database = "synapse-db"
	hs.Database.Args.Host = "/var/sockets"

	uri, ok := hs.DatabaseURI()
	if !ok {
		t.Fatal("no URI built")
	}
	// A unix socket directory belongs in the query string, not the URL host.
	if !strings.Contains(uri, "host=%2Fvar%2Fsockets") {
		t.Errorf("uri = %q, want the socket directory in the query", uri)
	}
	if !strings.Contains(uri, "/synapse-db") {
		t.Errorf("uri = %q, want the database name", uri)
	}
	// Incomplete args must not produce a half-built URI.
	var empty SynapseConfig
	if _, ok := empty.DatabaseURI(); ok {
		t.Error("built a URI from empty args")
	}
}

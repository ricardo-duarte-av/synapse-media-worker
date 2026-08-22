package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const minimalConfig = `
server_name: "example.com"
database:
  uri: "postgres://u:p@/db?host=/var/sockets"
media:
  store_path: /data/media_store
cache:
  dir: /data/cache
auth:
  whoami_socket: /var/sockets/client.sock
`

func TestLoadConfigAppliesDefaults(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, minimalConfig))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Media.MaxImagePixels != 100_000_000 {
		t.Errorf("max_image_pixels = %d", cfg.Media.MaxImagePixels)
	}
	if !cfg.Media.EnableAuthenticatedMedia {
		t.Error("enable_authenticated_media should default to true, matching Synapse")
	}
	if !cfg.Database.UpdateLastAccess {
		t.Error("update_last_access should default to true")
	}
	if cfg.Auth.PositiveTTL == 0 || cfg.Auth.NegativeTTL == 0 {
		t.Error("token cache TTLs have no default")
	}
}

// The worker must never write into Synapse's media store, so a cache directory
// nested inside it is a configuration error rather than something to warn about.
func TestRejectsCacheInsideMediaStore(t *testing.T) {
	body := strings.Replace(minimalConfig, "dir: /data/cache", "dir: /data/media_store/cache", 1)
	_, err := LoadConfig(writeConfig(t, body))
	if err == nil {
		t.Fatal("accepted a cache directory inside the media store")
	}
	if !strings.Contains(err.Error(), "must not be inside") {
		t.Errorf("unhelpful error: %v", err)
	}
}

func TestRequiredFields(t *testing.T) {
	cases := map[string]string{
		"server_name":      `server_name: "example.com"`,
		"database.uri":     `  uri: "postgres://u:p@/db?host=/var/sockets"`,
		"media.store_path": `  store_path: /data/media_store`,
		"cache.dir":        `  dir: /data/cache`,
	}
	for field, line := range cases {
		body := strings.Replace(minimalConfig, line, "", 1)
		if _, err := LoadConfig(writeConfig(t, body)); err == nil {
			t.Errorf("missing %s was accepted", field)
		}
	}
}

// A typo in a key should fail loudly rather than silently taking a default.
func TestUnknownFieldsRejected(t *testing.T) {
	body := minimalConfig + "\nunknown_option: true\n"
	if _, err := LoadConfig(writeConfig(t, body)); err == nil {
		t.Error("unknown configuration key was accepted")
	}
}

func TestUpstreamConfigured(t *testing.T) {
	if (UpstreamTarget{}).configured() {
		t.Error("empty target reported as configured")
	}
	if !(UpstreamTarget{Socket: "/x.sock"}).configured() {
		t.Error("socket target not reported as configured")
	}
	if !(UpstreamTarget{URL: "http://x"}).configured() {
		t.Error("url target not reported as configured")
	}
}

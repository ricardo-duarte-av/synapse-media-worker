package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func synapseConfigWithLimits(t *testing.T, body string) *Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "homeserver.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.SynapseConfig = path
	if err := cfg.deriveFromSynapse(); err != nil {
		t.Fatal(err)
	}
	return &cfg
}

// The sizes and durations are Synapse's spellings, and both suffix families are
// its own: parse_size is binary, parse_duration is not.
func TestUploadLimitsParsedFromSynapseConfig(t *testing.T) {
	cfg := synapseConfigWithLimits(t, `
server_name: example.com
public_baseurl: https://matrix.example.com
media_upload_limits:
  - max_size: 1G
    time_period: 1d
  - max_size: 10G
    time_period: 1w
    can_upgrade: true
    info_uri: https://example.com/plans
`)
	limits := cfg.Media.UploadLimits()
	if len(limits) != 2 {
		t.Fatalf("got %d limits, want 2", len(limits))
	}

	// Sorted longest window first, as Synapse sorts them: the evaluation order
	// decides which limit's error a user sees.
	if limits[0].Window != 7*24*time.Hour {
		t.Errorf("first window = %s, want the weekly limit first", limits[0].Window)
	}
	if limits[0].MaxBytes != 10*1024*1024*1024 {
		t.Errorf("max_size 10G = %d, want binary 10 GiB", limits[0].MaxBytes)
	}
	if !limits[0].CanUpgrade || limits[0].InfoURI != "https://example.com/plans" {
		t.Errorf("limit = %+v, want its own info_uri and can_upgrade", limits[0])
	}

	if limits[1].Window != 24*time.Hour || limits[1].MaxBytes != 1024*1024*1024 {
		t.Errorf("second limit = %+v, want 1 GiB per day", limits[1])
	}
	// No info_uri of its own, so Synapse's fallback page, built from
	// public_baseurl with its trailing slash forced on.
	want := "https://matrix.example.com/_synapse/client/media_upload_limit_exceeded"
	if limits[1].InfoURI != want {
		t.Errorf("info_uri = %q, want the fallback %q", limits[1].InfoURI, want)
	}
}

// public_baseurl is optional in Synapse and defaults to the server name.
func TestUploadLimitFallbackURIWithoutPublicBaseurl(t *testing.T) {
	cfg := synapseConfigWithLimits(t, `
server_name: example.com
media_upload_limits:
  - max_size: 100M
    time_period: 1h
`)
	want := "https://example.com/_synapse/client/media_upload_limit_exceeded"
	if got := cfg.Media.UploadLimits()[0].InfoURI; got != want {
		t.Errorf("info_uri = %q, want %q", got, want)
	}
}

// A limit Synapse would reject must not be silently ignored here: enforcing
// nothing looks identical to having no limits at all.
func TestUnparseableUploadLimitIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "homeserver.yaml")
	if err := os.WriteFile(path, []byte(`
server_name: example.com
media_upload_limits:
  - max_size: "not a size"
    time_period: 1d
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.SynapseConfig = path
	if err := cfg.deriveFromSynapse(); err == nil {
		t.Fatal("a malformed media_upload_limits entry was accepted")
	}
}

// The usage figure is carried between limits, so a user well under the longest
// window's limit costs one query rather than one per limit -- and the limit
// that is reported is the first one actually breached.
func TestEvaluateUploadLimits(t *testing.T) {
	weekly := MediaUploadLimit{MaxBytes: 1000, Window: 7 * 24 * time.Hour, InfoURI: "week"}
	daily := MediaUploadLimit{MaxBytes: 100, Window: 24 * time.Hour, InfoURI: "day"}
	limits := []MediaUploadLimit{weekly, daily}
	now := time.Now()

	t.Run("under both costs one query", func(t *testing.T) {
		queries := 0
		got, err := evaluateUploadLimits(limits, 10, now, func(int64) (int64, error) {
			queries++
			return 20, nil
		})
		if err != nil || got != nil {
			t.Fatalf("limit = %+v, err = %v, want no limit breached", got, err)
		}
		if queries != 1 {
			t.Errorf("%d queries, want 1: the weekly total already proves the daily one", queries)
		}
	})

	t.Run("over the longest window reports it first", func(t *testing.T) {
		got, err := evaluateUploadLimits(limits, 10, now, func(int64) (int64, error) {
			return 995, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if got == nil || got.InfoURI != "week" {
			t.Fatalf("limit = %+v, want the weekly limit", got)
		}
	})

	t.Run("under the longest but over a shorter one re-measures", func(t *testing.T) {
		var windows []int64
		got, err := evaluateUploadLimits(limits, 10, now, func(notBefore int64) (int64, error) {
			windows = append(windows, notBefore)
			if len(windows) == 1 {
				return 500, nil // a week's worth, under 1000
			}
			return 95, nil // today's, over 100 with this upload
		})
		if err != nil {
			t.Fatal(err)
		}
		if got == nil || got.InfoURI != "day" {
			t.Fatalf("limit = %+v, want the daily limit", got)
		}
		if len(windows) != 2 {
			t.Fatalf("%d queries, want 2", len(windows))
		}
		if !(windows[0] < windows[1]) {
			t.Error("the second query must cover the shorter, more recent window")
		}
	})

	t.Run("the limit is a ceiling, not a threshold", func(t *testing.T) {
		// Synapse refuses when used + length > max_bytes, so landing exactly
		// on the limit is allowed.
		exact, err := evaluateUploadLimits([]MediaUploadLimit{daily}, 5, now,
			func(int64) (int64, error) { return 95, nil })
		if err != nil || exact != nil {
			t.Errorf("an upload landing exactly on the limit was refused: %+v", exact)
		}
		over, err := evaluateUploadLimits([]MediaUploadLimit{daily}, 6, now,
			func(int64) (int64, error) { return 95, nil })
		if err != nil || over == nil {
			t.Errorf("one byte over the limit was allowed: %v", err)
		}
	})

	t.Run("no limits means no queries", func(t *testing.T) {
		got, err := evaluateUploadLimits(nil, 1<<40, now, func(int64) (int64, error) {
			t.Error("the database was queried with no limits configured")
			return 0, nil
		})
		if err != nil || got != nil {
			t.Errorf("limit = %+v, err = %v", got, err)
		}
	})
}

// Synapse's UserLimitExceededError: 403 M_USER_LIMIT_EXCEEDED, with info_uri
// always present and can_upgrade only when it is true.
func TestUserLimitExceededError(t *testing.T) {
	for _, tc := range []struct {
		name       string
		limit      MediaUploadLimit
		wantUpdate bool
	}{
		{"without upgrade", MediaUploadLimit{InfoURI: "https://example.com/why"}, false},
		{"with upgrade", MediaUploadLimit{InfoURI: "https://example.com/why", CanUpgrade: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			writeUserLimitExceeded(w, tc.limit)
			if w.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", w.Code)
			}
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body["errcode"] != "M_USER_LIMIT_EXCEEDED" {
				t.Errorf("errcode = %v", body["errcode"])
			}
			if body["error"] != "Media upload limit exceeded" {
				t.Errorf("error = %v", body["error"])
			}
			if body["info_uri"] != tc.limit.InfoURI {
				t.Errorf("info_uri = %v, want %q", body["info_uri"], tc.limit.InfoURI)
			}
			_, present := body["can_upgrade"]
			if present != tc.wantUpdate {
				t.Errorf("can_upgrade present = %t, want %t", present, tc.wantUpdate)
			}
		})
	}
}

// A module can replace the limits per user, and this worker cannot run it. The
// default is therefore to leave uploads to Synapse, with an explicit opt-out
// for deployments whose modules have nothing to do with media.
func TestUploadsDeferredWhenSynapseLoadsModules(t *testing.T) {
	for _, tc := range []struct {
		name    string
		modules bool
		policy  string
		want    bool
	}{
		{"no modules", false, "", true},
		{"modules, default policy", true, "", false},
		{"modules, explicitly proxied", true, moduleUploadLimitsProxy, false},
		{"modules, explicitly ignored", true, moduleUploadLimitsIgnore, true},
		{"no modules, ignore is a no-op", false, moduleUploadLimitsIgnore, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := MediaConfig{
				AcceptUploads:      true,
				ModuleUploadLimits: tc.policy,
				synapseModules:     tc.modules,
			}
			if got := m.UploadsAccepted(); got != tc.want {
				t.Errorf("UploadsAccepted() = %t, want %t", got, tc.want)
			}
		})
	}

	// accept_uploads off is still off, whatever the policy says.
	off := MediaConfig{ModuleUploadLimits: moduleUploadLimitsIgnore}
	if off.UploadsAccepted() {
		t.Error("uploads are accepted with accept_uploads off")
	}
}

// An operator who has both modules and uploads on must be told which of the
// two is winning, since neither answer is obviously right.
func TestUploadWarningWhenModulesLoaded(t *testing.T) {
	for _, tc := range []struct {
		policy string
		want   string
	}{
		{moduleUploadLimitsProxy, "uploads will be proxied to Synapse"},
		{moduleUploadLimitsIgnore, "is bypassed"},
	} {
		cfg := synapseConfigWithLimits(t, `
server_name: example.com
modules:
  - module: my.module.Thing
media_upload_limits:
  - max_size: 1G
    time_period: 1d
`)
		cfg.Media.AcceptUploads = true
		cfg.Media.ModuleUploadLimits = tc.policy
		// Re-derive now that accept_uploads is on, which is what the warning
		// is conditional on.
		if err := cfg.deriveFromSynapse(); err != nil {
			t.Fatal(err)
		}
		var found bool
		for _, warning := range cfg.derived.Warnings {
			if strings.Contains(warning, tc.want) {
				found = true
			}
		}
		if !found {
			t.Errorf("policy %q: no warning containing %q: %v",
				tc.policy, tc.want, cfg.derived.Warnings)
		}
	}
}

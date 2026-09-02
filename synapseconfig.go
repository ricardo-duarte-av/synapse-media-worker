package main

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// synapseconfig.go reads Synapse's own homeserver.yaml so the worker does not
// have to be told again what Synapse already knows.
//
// Duplicating these values is not merely tedious, it drifts. Synapse's
// parse_size uses binary multipliers, so `max_image_pixels: 100M` is
// 104857600 and not 100000000 -- a difference this worker got wrong by hand
// before this file existed, leaving a band of image sizes it refused to
// thumbnail while Synapse happily did.
//
// Values found here only fill in settings the worker's own config leaves
// unset. Explicit configuration always wins, because some paths in
// homeserver.yaml are relative to Synapse's container rather than ours, and
// its database args may point at a pooler this worker should not go through.

// SynapseConfig is the subset of homeserver.yaml the worker cares about.
// Unknown keys are ignored, so it tolerates any Synapse version.
type SynapseConfig struct {
	ServerName     string `yaml:"server_name"`
	MediaStorePath string `yaml:"media_store_path"`
	SigningKeyPath string `yaml:"signing_key_path"`

	// These accept either an integer or a suffixed string such as "100M".
	MaxUploadSize  any `yaml:"max_upload_size"`
	MaxImagePixels any `yaml:"max_image_pixels"`

	MaxPendingMediaUploads *int  `yaml:"max_pending_media_uploads"`
	UnusedExpirationTime   any   `yaml:"unused_expiration_time"`
	MediaUploadLimits      []any `yaml:"media_upload_limits"`

	DynamicThumbnails        *bool `yaml:"dynamic_thumbnails"`
	EnableAuthenticatedMedia *bool `yaml:"enable_authenticated_media"`

	Database struct {
		Name string `yaml:"name"`
		Args struct {
			User     string `yaml:"user"`
			Password string `yaml:"password"`
			Database string `yaml:"database"`
			Host     string `yaml:"host"`
			Port     any    `yaml:"port"`
		} `yaml:"args"`
	} `yaml:"database"`

	// Presence of these is a warning sign rather than something to read: the
	// worker cannot fetch from an object store or honour a blocklist yet.
	StorageProviders          []any    `yaml:"media_storage_providers"`
	PreventMediaDownloadsFrom []string `yaml:"prevent_media_downloads_from"`

	// Modules is read only to know whether any exist. A module can register a
	// get_media_config_for_user callback and replace the /media/config
	// response per user, which the worker cannot reproduce, so the endpoint is
	// handed back to Synapse whenever modules are loaded at all. Being coarse
	// costs a proxied request on a rarely called endpoint; being wrong would
	// report a limit that is not the user's.
	Modules []any `yaml:"modules"`
}

// LoadSynapseConfig reads and parses homeserver.yaml.
func LoadSynapseConfig(path string) (*SynapseConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading Synapse config: %w", err)
	}
	var cfg SynapseConfig
	// Deliberately permissive: homeserver.yaml is full of keys that are none
	// of this worker's business.
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parsing Synapse config: %w", err)
	}
	return &cfg, nil
}

// parseSynapseSize reproduces Synapse's Config.parse_size
// (synapse/config/_base.py:177). The suffixes are binary, not decimal: K is
// 1024, not 1000.
func parseSynapseSize(value any) (int64, error) {
	switch v := value.(type) {
	case nil:
		return 0, fmt.Errorf("no value")
	case int:
		return int64(v), nil
	case int64:
		return v, nil
	case float64:
		// YAML gives floats for very large plain integers.
		return int64(v), nil
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return 0, fmt.Errorf("empty value")
		}
		multiplier := int64(1)
		switch s[len(s)-1] {
		case 'K', 'k':
			multiplier = 1024
		case 'M', 'm':
			multiplier = 1024 * 1024
		case 'G', 'g':
			multiplier = 1024 * 1024 * 1024
		case 'T', 't':
			multiplier = 1024 * 1024 * 1024 * 1024
		}
		if multiplier > 1 {
			s = s[:len(s)-1]
		}
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%q is not a size", v)
		}
		return n * multiplier, nil
	default:
		return 0, fmt.Errorf("%v is not a size", value)
	}
}

// DatabaseURI builds a libpq connection string from Synapse's psycopg2 args.
//
// Note this may point at a connection pooler: Synapse runs many processes and
// often goes through one, while this worker is a single process that is
// usually better off connecting directly. Set database.uri explicitly to
// override.
func (s *SynapseConfig) DatabaseURI() (string, bool) {
	a := s.Database.Args
	if a.User == "" || a.Database == "" {
		return "", false
	}
	q := url.Values{}
	if a.Host != "" {
		q.Set("host", a.Host)
	}
	switch p := a.Port.(type) {
	case int:
		q.Set("port", strconv.Itoa(p))
	case string:
		if p != "" {
			q.Set("port", p)
		}
	}
	// Synapse's own connections are not TLS by default over a socket or a
	// local pooler.
	q.Set("sslmode", "disable")

	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(a.User, a.Password),
		Path:   "/" + a.Database,
	}
	// A unix socket directory cannot live in the URL host, so it stays in the
	// query string and the host is left empty.
	if a.Host != "" && !strings.HasPrefix(a.Host, "/") {
		u.Host = ""
	}
	u.RawQuery = q.Encode()
	return u.String(), true
}

// parseSynapseDuration reproduces Synapse's Config.parse_duration
// (synapse/config/_base.py:204). A bare integer is milliseconds; the suffixes
// are s, m, h, d, w, y.
func parseSynapseDuration(value any) (time.Duration, error) {
	switch v := value.(type) {
	case nil:
		return 0, fmt.Errorf("no value")
	case int:
		return time.Duration(v) * time.Millisecond, nil
	case int64:
		return time.Duration(v) * time.Millisecond, nil
	case float64:
		return time.Duration(int64(v)) * time.Millisecond, nil
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return 0, fmt.Errorf("empty value")
		}
		unit := time.Millisecond
		switch s[len(s)-1] {
		case 's':
			unit = time.Second
		case 'm':
			unit = time.Minute
		case 'h':
			unit = time.Hour
		case 'd':
			unit = 24 * time.Hour
		case 'w':
			unit = 7 * 24 * time.Hour
		case 'y':
			unit = 365 * 24 * time.Hour
		}
		if unit != time.Millisecond {
			s = s[:len(s)-1]
		}
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%q is not a duration", v)
		}
		return time.Duration(n) * unit, nil
	default:
		return 0, fmt.Errorf("%v is not a duration", value)
	}
}

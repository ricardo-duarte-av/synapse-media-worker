package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the worker's own configuration. It deliberately does not parse
// homeserver.yaml: the handful of values we need are duplicated here so the
// worker has no coupling to Synapse's config schema.
type Config struct {
	// ServerName is the homeserver's server_name, e.g. "example.com". It is
	// used to tell local media apart from remote and as the expected
	// `destination` in inbound X-Matrix auth headers.
	ServerName string `yaml:"server_name"`

	Listen   ListenConfig   `yaml:"listen"`
	Database DatabaseConfig `yaml:"database"`
	Media    MediaConfig    `yaml:"media"`
	Cache    CacheConfig    `yaml:"cache"`
	Auth     AuthConfig     `yaml:"auth"`
	Upstream UpstreamConfig `yaml:"upstream"`
	Log      LogConfig      `yaml:"log"`
}

type ListenConfig struct {
	// Socket is a unix socket path to listen on. Takes precedence over Addr.
	Socket string `yaml:"socket"`
	// SocketMode is the permission bits applied to Socket, e.g. 0666.
	SocketMode os.FileMode `yaml:"socket_mode"`
	// Addr is a TCP address, used when Socket is empty.
	Addr string `yaml:"addr"`
	// MetricsAddr, if set, serves /metrics on a separate TCP listener.
	MetricsAddr string `yaml:"metrics_addr"`
}

type DatabaseConfig struct {
	// URI is a libpq connection string. For a unix socket, set the host to the
	// directory containing the socket, e.g.
	//   postgres://synapse:pw@/synapse-db?host=/var/sockets
	URI string `yaml:"uri"`
	// MaxConns bounds the read pool.
	MaxConns int32 `yaml:"max_conns"`
	// UpdateLastAccess controls whether the worker writes last_access_ts back
	// to Synapse's database. This is the ONLY write the worker performs.
	// Leaving it off will make media retention and LRU cleanup treat media the
	// worker serves as never accessed.
	UpdateLastAccess bool `yaml:"update_last_access"`
	// LastAccessInterval is how often batched last_access_ts updates are
	// flushed. Synapse uses one minute.
	LastAccessInterval time.Duration `yaml:"last_access_interval"`
}

type MediaConfig struct {
	// StorePath is Synapse's media_store_path. The worker only ever reads from
	// it and it should be mounted read-only.
	StorePath string `yaml:"store_path"`
	// SigningKeyPath is Synapse's signing.key file, used to authenticate
	// outbound server-key lookups during inbound federation verification.
	SigningKeyPath string `yaml:"signing_key_path"`
	// MaxImagePixels mirrors Synapse's max_image_pixels. Images at or above
	// this many pixels are never thumbnailed by the worker.
	MaxImagePixels int64 `yaml:"max_image_pixels"`
	// DefaultTimeout is the default ?timeout_ms for waiting on a pending
	// async upload.
	DefaultTimeout time.Duration `yaml:"default_timeout"`
	// MaxTimeout caps ?timeout_ms.
	MaxTimeout time.Duration `yaml:"max_timeout"`
	// EnableAuthenticatedMedia mirrors Synapse's enable_authenticated_media.
	// When true, media rows with authenticated = true are hidden from the
	// legacy unauthenticated /_matrix/media endpoints.
	EnableAuthenticatedMedia bool `yaml:"enable_authenticated_media"`
}

type CacheConfig struct {
	// Dir is the worker's own writable cache directory, used for thumbnails it
	// generates. Never inside Synapse's media store.
	Dir string `yaml:"dir"`
	// MaxBytes is the eviction high-water mark for Dir.
	MaxBytes int64 `yaml:"max_bytes"`
	// SweepInterval is how often the LRU janitor runs.
	SweepInterval time.Duration `yaml:"sweep_interval"`
}

type AuthConfig struct {
	// WhoamiURL is the base URL used to validate client access tokens, e.g.
	// "http://unix/" when WhoamiSocket is set, or "https://example.com".
	WhoamiURL string `yaml:"whoami_url"`
	// WhoamiSocket is a unix socket for a Synapse client worker. When set, the
	// whoami request is made over it instead of over TCP.
	WhoamiSocket string `yaml:"whoami_socket"`
	// PositiveTTL is how long a successful token validation is cached.
	PositiveTTL time.Duration `yaml:"positive_ttl"`
	// NegativeTTL is how long a rejected token is cached.
	NegativeTTL time.Duration `yaml:"negative_ttl"`
	// MaxEntries bounds the token cache.
	MaxEntries int `yaml:"max_entries"`
}

type UpstreamConfig struct {
	// Download is the Synapse media worker to proxy to when the worker cannot
	// answer a download itself (uncached remote media).
	Download UpstreamTarget `yaml:"download"`
	// Thumbnail is the Synapse media worker to proxy to when the worker cannot
	// generate a thumbnail itself (animated, unsupported format, oversized).
	Thumbnail UpstreamTarget `yaml:"thumbnail"`
}

type UpstreamTarget struct {
	// Socket is a unix socket path to the Synapse worker.
	Socket string `yaml:"socket"`
	// URL is used when Socket is empty.
	URL string `yaml:"url"`
}

func (t UpstreamTarget) configured() bool { return t.Socket != "" || t.URL != "" }

type LogConfig struct {
	// Level is one of trace, debug, info, warn, error.
	Level string `yaml:"level"`
	// Pretty enables human-readable console output instead of JSON.
	Pretty bool `yaml:"pretty"`
}

func defaultConfig() Config {
	return Config{
		Listen: ListenConfig{
			SocketMode: 0666,
			Addr:       ":8090",
		},
		Database: DatabaseConfig{
			MaxConns:           16,
			UpdateLastAccess:   true,
			LastAccessInterval: time.Minute,
		},
		Media: MediaConfig{
			MaxImagePixels:           100_000_000,
			DefaultTimeout:           20 * time.Second,
			MaxTimeout:               60 * time.Second,
			EnableAuthenticatedMedia: true,
		},
		Cache: CacheConfig{
			MaxBytes:      10 << 30, // 10 GiB
			SweepInterval: 10 * time.Minute,
		},
		Auth: AuthConfig{
			PositiveTTL: 5 * time.Minute,
			NegativeTTL: 30 * time.Second,
			MaxEntries:  20000,
		},
		Log: LogConfig{Level: "info"},
	}
}

func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	cfg := defaultConfig()
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	if c.ServerName == "" {
		return fmt.Errorf("server_name is required")
	}
	if c.Database.URI == "" {
		return fmt.Errorf("database.uri is required")
	}
	if c.Media.StorePath == "" {
		return fmt.Errorf("media.store_path is required")
	}
	if c.Cache.Dir == "" {
		return fmt.Errorf("cache.dir is required")
	}
	if strings.HasPrefix(cleanAbs(c.Cache.Dir), cleanAbs(c.Media.StorePath)) {
		// The whole point of the worker is that it never writes to Synapse's
		// media store. Refuse to start rather than quietly violate that.
		return fmt.Errorf("cache.dir (%s) must not be inside media.store_path (%s)",
			c.Cache.Dir, c.Media.StorePath)
	}
	if c.Auth.WhoamiURL == "" && c.Auth.WhoamiSocket == "" {
		return fmt.Errorf("auth.whoami_url or auth.whoami_socket is required")
	}
	if c.Media.MaxImagePixels <= 0 {
		return fmt.Errorf("media.max_image_pixels must be positive")
	}
	return nil
}

package main

import (
	"fmt"
	"os"
	"sort"
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
	// `destination` in inbound X-Matrix auth headers. Derived from
	// SynapseConfig when unset.
	ServerName string `yaml:"server_name"`

	// SynapseConfig is the path to Synapse's homeserver.yaml, mounted
	// read-only. When set, anything this file leaves unset is taken from it
	// rather than duplicated -- sizes especially, since Synapse's suffixes are
	// binary and transcribing them by hand goes wrong.
	//
	// It contains every secret Synapse has, so mount a stripped copy if the
	// worker should not see them.
	SynapseConfig string `yaml:"synapse_config"`

	Listen   ListenConfig   `yaml:"listen"`
	Database DatabaseConfig `yaml:"database"`
	Media    MediaConfig    `yaml:"media"`
	Cache    CacheConfig    `yaml:"cache"`
	Auth     AuthConfig     `yaml:"auth"`
	Upstream UpstreamConfig `yaml:"upstream"`
	Log      LogConfig      `yaml:"log"`

	// derived records what was taken from Synapse's config, for startup logging.
	derived *derivedNotes `yaml:"-"`
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
	// this many pixels are never thumbnailed by the worker. Derived when unset.
	MaxImagePixels *int64 `yaml:"max_image_pixels"`
	// DefaultTimeout is the default ?timeout_ms for waiting on a pending
	// async upload.
	DefaultTimeout time.Duration `yaml:"default_timeout"`
	// MaxTimeout caps ?timeout_ms.
	MaxTimeout time.Duration `yaml:"max_timeout"`
	// MaxConcurrentThumbnails bounds how many thumbnails are generated at
	// once. Generation is CPU-bound and holds the decoded image in memory, so
	// an unbounded burst of distinct sizes could exhaust both. Defaults to the
	// number of usable CPUs.
	MaxConcurrentThumbnails int `yaml:"max_concurrent_thumbnails"`
	// AcceptUploads lets the worker handle media uploads from local users.
	// This is the only feature that writes into local_content, so it widens
	// the write guard; off by default.
	AcceptUploads bool `yaml:"accept_uploads"`
	// UploadThumbnails selects what is generated when media is uploaded:
	// "none" (the default) or "synapse" for byte-parity with Synapse's set.
	//
	// none is the better setting under dynamic_thumbnails, where Synapse's
	// upload-time thumbnails are written in a type no request path can ask
	// for, and where most uploads are never viewed at thumbnail size at all.
	UploadThumbnails string `yaml:"upload_thumbnails"`
	// MaxConcurrentUploads bounds simultaneous uploads being written.
	MaxConcurrentUploads int `yaml:"max_concurrent_uploads"`
	// UnusedExpirationTime mirrors Synapse's unused_expiration_time: how long
	// an async media ID may sit unused before it is treated as expired.
	UnusedExpirationTime time.Duration `yaml:"unused_expiration_time"`
	// MaxPendingMediaUploads mirrors Synapse's setting of the same name. The
	// spec requires 429 M_LIMIT_EXCEEDED on /create once a user is over it.
	MaxPendingMediaUploads int `yaml:"max_pending_media_uploads"`
	// FetchRemote lets the worker download uncached remote media itself
	// instead of proxying to Synapse. This is the one feature that writes to
	// Synapse's media store, so it is off by default and any failure falls
	// back to proxying.
	FetchRemote bool `yaml:"fetch_remote"`
	// ProxyFailedFetches hands a failed remote fetch to Synapse to try again,
	// rather than answering the client directly.
	//
	// Defaults to true, which is the safer starting point. Turning it off is
	// usually the right call once fetching is trusted: federation is full of
	// defunct servers, and a DNS or TLS failure will fail for Synapse exactly
	// as it failed here, so proxying buys a second attempt at the same answer
	// while the client waits for both.
	//
	// With it off, failures are answered with the status Synapse itself would
	// have produced: the origin's own error for a 404, and 502 for everything
	// else.
	ProxyFailedFetches *bool `yaml:"proxy_failed_fetches"`
	// MaxConcurrentFetches bounds simultaneous federated downloads.
	MaxConcurrentFetches int `yaml:"max_concurrent_fetches"`
	// FetchTimeout bounds a single federated download.
	FetchTimeout time.Duration `yaml:"fetch_timeout"`
	// WriteThroughThumbnails writes thumbnails generated for REMOTE media into
	// Synapse's media store instead of the worker's own cache, so they are
	// permanent rather than LRU-evictable and Synapse can serve them too.
	// Local media thumbnails are unaffected and stay in the worker's cache.
	WriteThroughThumbnails bool `yaml:"write_through_thumbnails"`
	// MaxUploadSize mirrors Synapse's max_upload_size and caps how large a
	// remote file the worker will store. Derived when unset.
	MaxUploadSize *int64 `yaml:"max_upload_size"`
	// EnableAuthenticatedMedia mirrors Synapse's enable_authenticated_media.
	// When true, media rows with authenticated = true are hidden from the
	// legacy unauthenticated /_matrix/media endpoints. Derived when unset.
	EnableAuthenticatedMedia *bool `yaml:"enable_authenticated_media"`
	// DynamicThumbnails mirrors Synapse's dynamic_thumbnails. The worker
	// currently implements only the dynamic behaviour (exact match, generate on
	// a miss); when Synapse has it off it selects a nearest match instead, and
	// the two will disagree. Derived when unset.
	DynamicThumbnails *bool `yaml:"dynamic_thumbnails"`

	// ModuleUploadLimits decides what to do when homeserver.yaml loads
	// modules, which may replace the per-user upload limits below or veto an
	// upload by size outright. "proxy" hands uploads to Synapse, which can run
	// the modules; "ignore" accepts them here and applies only the limits read
	// from config.
	ModuleUploadLimits string `yaml:"module_upload_limits"`

	// synapseModules records that homeserver.yaml loads modules, which may
	// override /media/config and the upload limits per user. Derived, never
	// configured: it has no yaml tag and is unexported so the strict decoder
	// cannot see it.
	synapseModules bool
	// uploadLimits are Synapse's media_upload_limits, sorted longest window
	// first as Synapse sorts them. Derived: transcribing a list of sizes and
	// durations by hand is exactly the drift synapse_config exists to avoid.
	uploadLimits []MediaUploadLimit
}

// MediaUploadLimit caps how many bytes a user may upload in a rolling window,
// mirroring Synapse's MediaUploadLimit.
type MediaUploadLimit struct {
	// MaxBytes is the ceiling for the window.
	MaxBytes int64
	// Window is how far back usage is summed.
	Window time.Duration
	// InfoURI is returned with the M_USER_LIMIT_EXCEEDED error. Synapse falls
	// back to a page it serves itself, so this is never empty by the time it
	// reaches a response.
	InfoURI string
	// CanUpgrade tells the client the limit can be raised by upgrading, and is
	// omitted from the error when false.
	CanUpgrade bool
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
	// Upload is the Synapse worker to proxy uploads to when accept_uploads is
	// off. Falls back to the download upstream when unset.
	Upload UpstreamTarget `yaml:"upload"`
	// Passthrough receives everything on the media surface this worker does
	// not implement -- preview_url and the media admin APIs.
	// Falls back to the download upstream when unset.
	//
	// The quarantine admin APIs must reach an instance configured as a
	// quarantined_media_changes writer, so this is not interchangeable with
	// any media worker.
	Passthrough UpstreamTarget `yaml:"passthrough"`
}

// UpstreamTarget names one or more Synapse workers to fall back to. Several
// may be given, in which case requests are spread across them by least
// connections, the same way nginx would.
type UpstreamTarget struct {
	// Socket is a unix socket path to a Synapse worker.
	Socket string `yaml:"socket"`
	// Sockets lists several unix sockets to balance across.
	Sockets []string `yaml:"sockets"`
	// URL is a TCP endpoint, used when no socket is given.
	URL string `yaml:"url"`
	// URLs lists several TCP endpoints to balance across.
	URLs []string `yaml:"urls"`
}

// Endpoints returns every configured target, singular and plural forms
// combined, in configuration order.
func (t UpstreamTarget) Endpoints() []UpstreamEndpoint {
	var out []UpstreamEndpoint
	if t.Socket != "" {
		out = append(out, UpstreamEndpoint{Socket: t.Socket})
	}
	for _, s := range t.Sockets {
		if s != "" {
			out = append(out, UpstreamEndpoint{Socket: s})
		}
	}
	if t.URL != "" {
		out = append(out, UpstreamEndpoint{URL: t.URL})
	}
	for _, u := range t.URLs {
		if u != "" {
			out = append(out, UpstreamEndpoint{URL: u})
		}
	}
	return out
}

// UpstreamEndpoint is a single Synapse worker to proxy to.
type UpstreamEndpoint struct {
	Socket string
	URL    string
}

// Name identifies the endpoint in logs and metrics.
func (e UpstreamEndpoint) Name() string {
	if e.Socket != "" {
		return "unix:" + e.Socket
	}
	return e.URL
}

func (t UpstreamTarget) configured() bool { return len(t.Endpoints()) > 0 }

type LogConfig struct {
	// Level is one of trace, debug, info, warn, error.
	Level string `yaml:"level"`
	// Pretty enables human-readable console output instead of JSON.
	Pretty bool `yaml:"pretty"`
	// Requests logs one line per request, recording what the worker did with
	// it: served from disk, from its own cache, generated, or proxied back to
	// Synapse. Set false on a busy server where the reverse proxy's own access
	// log is enough.
	Requests *bool `yaml:"requests"`
}

// LogRequests reports whether per-request logging is on, defaulting to true.
func (c LogConfig) LogRequests() bool {
	return c.Requests == nil || *c.Requests
}

// Upload thumbnail modes.
const (
	uploadThumbnailsNone    = "none"
	uploadThumbnailsSynapse = "synapse"
)

// Synapse's defaults, from synapse/config/repository.py.
const (
	synapseDefaultMaxUploadSize  int64 = 50 * 1024 * 1024
	synapseDefaultMaxImagePixels int64 = 32 * 1024 * 1024
)

// MaxImagePixelsOrDefault returns the effective pixel limit.
func (m MediaConfig) MaxImagePixelsOrDefault() int64 {
	if m.MaxImagePixels != nil {
		return *m.MaxImagePixels
	}
	return synapseDefaultMaxImagePixels
}

// MaxUploadSizeOrDefault returns the effective upload size limit.
func (m MediaConfig) MaxUploadSizeOrDefault() int64 {
	if m.MaxUploadSize != nil {
		return *m.MaxUploadSize
	}
	return synapseDefaultMaxUploadSize
}

// ProxyFetchFailures reports whether a failed remote fetch should be handed to
// Synapse, defaulting to true.
func (m MediaConfig) ProxyFetchFailures() bool {
	return m.ProxyFailedFetches == nil || *m.ProxyFailedFetches
}

// AuthenticatedMedia reports whether authenticated media is on, defaulting to
// true as Synapse does.
func (m MediaConfig) AuthenticatedMedia() bool {
	return m.EnableAuthenticatedMedia == nil || *m.EnableAuthenticatedMedia
}

// Module upload limit policies.
const (
	moduleUploadLimitsProxy  = "proxy"
	moduleUploadLimitsIgnore = "ignore"
)

// UploadLimits returns the per-user upload quotas read from Synapse's config.
func (m MediaConfig) UploadLimits() []MediaUploadLimit {
	return m.uploadLimits
}

// UploadsAccepted reports whether this worker should serve uploads itself.
//
// accept_uploads is the operator's intent; this is what is actually safe. A
// module may replace the upload limits per user or refuse a size outright, and
// the worker cannot run Synapse's modules, so with modules loaded the default
// is to leave uploads to Synapse entirely rather than enforce limits that may
// not be this user's.
func (m MediaConfig) UploadsAccepted() bool {
	if !m.AcceptUploads {
		return false
	}
	return !(m.synapseModules && m.ModuleUploadLimitsPolicy() == moduleUploadLimitsProxy)
}

// ModuleUploadLimitsPolicy returns the effective policy, defaulting to proxy:
// deferring is always correct, and enforcing a limit that a module would have
// overridden is not.
func (m MediaConfig) ModuleUploadLimitsPolicy() string {
	if m.ModuleUploadLimits == "" {
		return moduleUploadLimitsProxy
	}
	return m.ModuleUploadLimits
}

// ProxyMediaConfig reports whether /media/config must be handed to Synapse
// rather than answered here, because a loaded module may replace the response
// for some users.
func (m MediaConfig) ProxyMediaConfig() bool {
	return m.synapseModules
}

// DynamicThumbnailsEnabled reports Synapse's dynamic_thumbnails, whose default
// is false.
func (m MediaConfig) DynamicThumbnailsEnabled() bool {
	return m.DynamicThumbnails != nil && *m.DynamicThumbnails
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
			UploadThumbnails:       uploadThumbnailsNone,
			MaxConcurrentUploads:   8,
			UnusedExpirationTime:   24 * time.Hour,
			MaxPendingMediaUploads: 5,
			MaxConcurrentFetches:   4,
			FetchTimeout:           60 * time.Second,
			DefaultTimeout:         20 * time.Second,
			MaxTimeout:             60 * time.Second,
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
	if cfg.SynapseConfig != "" {
		if err := cfg.deriveFromSynapse(); err != nil {
			return nil, err
		}
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
	if c.Media.MaxImagePixelsOrDefault() <= 0 {
		return fmt.Errorf("media.max_image_pixels must be positive")
	}
	if c.Media.WriteThroughThumbnails && !c.Media.FetchRemote {
		// Both need a writable store, and the startup probe is tied to
		// fetch_remote. Rather than probe twice, require the flag that already
		// declares the intent to write.
		return fmt.Errorf("media.write_through_thumbnails requires media.fetch_remote to be enabled")
	}
	switch c.Media.ModuleUploadLimits {
	case "", moduleUploadLimitsProxy, moduleUploadLimitsIgnore:
	default:
		return fmt.Errorf("media.module_upload_limits must be %q or %q, got %q",
			moduleUploadLimitsProxy, moduleUploadLimitsIgnore, c.Media.ModuleUploadLimits)
	}
	switch c.Media.UploadThumbnails {
	case "", uploadThumbnailsNone, uploadThumbnailsSynapse:
	default:
		return fmt.Errorf("media.upload_thumbnails must be %q or %q, got %q",
			uploadThumbnailsNone, uploadThumbnailsSynapse, c.Media.UploadThumbnails)
	}
	if c.Media.AcceptUploads {
		if c.Media.MaxUploadSizeOrDefault() <= 0 {
			return fmt.Errorf("media.max_upload_size must be positive when accept_uploads is on")
		}
		if c.ServerName == "" {
			return fmt.Errorf("server_name is required when accept_uploads is on")
		}
	}
	if c.Media.FetchRemote {
		if c.Media.SigningKeyPath == "" {
			return fmt.Errorf("media.signing_key_path is required when fetch_remote is on")
		}
		if c.Media.MaxUploadSizeOrDefault() <= 0 {
			return fmt.Errorf("media.max_upload_size must be positive when fetch_remote is on")
		}
	}
	return nil
}

// deriveFromSynapse fills in anything this config leaves unset from Synapse's
// homeserver.yaml. Explicit values always win.
//
// Paths are only adopted if they resolve here: homeserver.yaml records them as
// they appear inside Synapse's container, which is not necessarily where this
// worker sees them.
func (c *Config) deriveFromSynapse() error {
	hs, err := LoadSynapseConfig(c.SynapseConfig)
	if err != nil {
		return err
	}
	c.derived = &derivedNotes{}

	if c.ServerName == "" && hs.ServerName != "" {
		c.ServerName = hs.ServerName
		c.derived.note("server_name", hs.ServerName)
	}
	if c.Media.MaxUploadSize == nil && hs.MaxUploadSize != nil {
		n, err := parseSynapseSize(hs.MaxUploadSize)
		if err != nil {
			return fmt.Errorf("max_upload_size in %s: %w", c.SynapseConfig, err)
		}
		c.Media.MaxUploadSize = &n
		c.derived.note("media.max_upload_size", fmt.Sprintf("%d", n))
	}
	if c.Media.MaxImagePixels == nil && hs.MaxImagePixels != nil {
		n, err := parseSynapseSize(hs.MaxImagePixels)
		if err != nil {
			return fmt.Errorf("max_image_pixels in %s: %w", c.SynapseConfig, err)
		}
		c.Media.MaxImagePixels = &n
		c.derived.note("media.max_image_pixels", fmt.Sprintf("%d", n))
	}
	if hs.MaxPendingMediaUploads != nil && c.Media.MaxPendingMediaUploads == 5 {
		c.Media.MaxPendingMediaUploads = *hs.MaxPendingMediaUploads
		c.derived.note("media.max_pending_media_uploads",
			fmt.Sprintf("%d", *hs.MaxPendingMediaUploads))
	}
	if hs.UnusedExpirationTime != nil {
		d, err := parseSynapseDuration(hs.UnusedExpirationTime)
		if err != nil {
			return fmt.Errorf("unused_expiration_time in %s: %w", c.SynapseConfig, err)
		}
		c.Media.UnusedExpirationTime = d
		c.derived.note("media.unused_expiration_time", d.String())
	}
	limits, err := parseMediaUploadLimits(hs)
	if err != nil {
		return fmt.Errorf("media_upload_limits in %s: %w", c.SynapseConfig, err)
	}
	c.Media.uploadLimits = limits
	if len(limits) > 0 {
		c.derived.note("media.upload_limits", describeUploadLimits(limits))
	}
	if c.Media.EnableAuthenticatedMedia == nil && hs.EnableAuthenticatedMedia != nil {
		c.Media.EnableAuthenticatedMedia = hs.EnableAuthenticatedMedia
		c.derived.note("media.enable_authenticated_media",
			fmt.Sprintf("%t", *hs.EnableAuthenticatedMedia))
	}
	if c.Media.DynamicThumbnails == nil && hs.DynamicThumbnails != nil {
		c.Media.DynamicThumbnails = hs.DynamicThumbnails
		c.derived.note("media.dynamic_thumbnails", fmt.Sprintf("%t", *hs.DynamicThumbnails))
	}
	if c.Media.StorePath == "" && hs.MediaStorePath != "" {
		if _, err := os.Stat(hs.MediaStorePath); err == nil {
			c.Media.StorePath = hs.MediaStorePath
			c.derived.note("media.store_path", hs.MediaStorePath)
		} else {
			c.derived.skip("media.store_path", hs.MediaStorePath,
				"path does not exist in this container")
		}
	}
	if c.Media.SigningKeyPath == "" && hs.SigningKeyPath != "" {
		if _, err := os.Stat(hs.SigningKeyPath); err == nil {
			c.Media.SigningKeyPath = hs.SigningKeyPath
			c.derived.note("media.signing_key_path", hs.SigningKeyPath)
		} else {
			c.derived.skip("media.signing_key_path", hs.SigningKeyPath,
				"path does not exist in this container")
		}
	}
	if c.Database.URI == "" {
		if uri, ok := hs.DatabaseURI(); ok {
			c.Database.URI = uri
			// Never logged with the value: it carries the password.
			c.derived.note("database.uri", "(from Synapse's database.args)")
		}
	}

	if len(hs.Modules) > 0 {
		// Sits with the other "Synapse does something this worker cannot"
		// warnings, but these are handled rather than merely reported.
		c.Media.synapseModules = true
		c.derived.warn("modules are configured in Synapse and one may override " +
			"/media/config per user; that endpoint will be passed through rather " +
			"than answered here")
		if c.Media.AcceptUploads {
			if c.Media.ModuleUploadLimitsPolicy() == moduleUploadLimitsProxy {
				c.derived.warn("modules are configured in Synapse and one may replace " +
					"the media upload limits per user; uploads will be proxied to Synapse " +
					"rather than accepted here (media.module_upload_limits: ignore " +
					"accepts them and applies only the limits from config)")
			} else {
				c.derived.warn("media.module_upload_limits is \"ignore\" and Synapse loads " +
					"modules; uploads are accepted here with only the media_upload_limits " +
					"from config, so any per-user limit a module imposes is bypassed")
			}
		}
	}

	if len(hs.StorageProviders) > 0 {
		c.derived.warn("media_storage_providers is configured in Synapse; " +
			"this worker only reads the local media store and will not find media held only in a provider")
	}
	if len(hs.PreventMediaDownloadsFrom) > 0 && c.Media.FetchRemote {
		c.derived.warn("prevent_media_downloads_from is configured in Synapse but " +
			"this worker does not implement it; fetch_remote would bypass that blocklist")
	}
	if !c.Media.DynamicThumbnailsEnabled() {
		c.derived.warn("Synapse has dynamic_thumbnails off, so it selects a nearest-matching " +
			"thumbnail; this worker only does exact matching and the two will disagree")
	}
	return nil
}

// parseMediaUploadLimits converts Synapse's media_upload_limits into the form
// the upload path checks against, in the order Synapse checks them.
func parseMediaUploadLimits(hs *SynapseConfig) ([]MediaUploadLimit, error) {
	if len(hs.MediaUploadLimits) == 0 {
		return nil, nil
	}
	// The fallback is a page Synapse serves. Synapse substitutes it whenever a
	// limit carries no info_uri of its own, so no error can go out without one.
	fallbackURI := hs.PublicBaseurlOrDefault() + "_synapse/client/media_upload_limit_exceeded"

	limits := make([]MediaUploadLimit, 0, len(hs.MediaUploadLimits))
	for i, raw := range hs.MediaUploadLimits {
		maxBytes, err := parseSynapseSize(raw.MaxSize)
		if err != nil {
			return nil, fmt.Errorf("entry %d: max_size: %w", i, err)
		}
		window, err := parseSynapseDuration(raw.TimePeriod)
		if err != nil {
			return nil, fmt.Errorf("entry %d: time_period: %w", i, err)
		}
		infoURI := raw.InfoURI
		if infoURI == "" {
			infoURI = fallbackURI
		}
		limits = append(limits, MediaUploadLimit{
			MaxBytes: maxBytes, Window: window,
			InfoURI: infoURI, CanUpgrade: raw.CanUpgrade,
		})
	}
	// Synapse sorts descending by time period and relies on that order to skip
	// the smaller windows once a larger one has been shown to be under its
	// limit. Reproduce the order, or a different limit reports the error.
	sort.SliceStable(limits, func(i, j int) bool {
		return limits[i].Window > limits[j].Window
	})
	return limits, nil
}

// describeUploadLimits renders the limits for the startup log.
func describeUploadLimits(limits []MediaUploadLimit) string {
	parts := make([]string, 0, len(limits))
	for _, l := range limits {
		parts = append(parts, fmt.Sprintf("%d bytes/%s", l.MaxBytes, l.Window))
	}
	return strings.Join(parts, ", ")
}

// derivedNotes records what was taken from Synapse's config, for logging once
// at startup.
type derivedNotes struct {
	Applied  []string
	Skipped  []string
	Warnings []string
}

func (d *derivedNotes) note(key, value string) {
	d.Applied = append(d.Applied, key+"="+value)
}

func (d *derivedNotes) skip(key, value, why string) {
	d.Skipped = append(d.Skipped, fmt.Sprintf("%s=%s (%s)", key, value, why))
}

func (d *derivedNotes) warn(msg string) {
	d.Warnings = append(d.Warnings, msg)
}

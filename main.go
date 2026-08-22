package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
	"go.mau.fi/util/exhttp"
	"maunium.net/go/mautrix/federation"
)

// Build information, stamped by the release build with -ldflags -X.
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to the configuration file")
	checkOnly := flag.Bool("check", false, "validate configuration and access, then exit")
	showVersion := flag.Bool("version", false, "print build information and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("synapse-media-worker %s (commit %s, built %s, %s)\n",
			Version, Commit, BuildTime, runtime.Version())
		return
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}

	log := newLogger(cfg.Log)

	if err := run(cfg, log, *checkOnly); err != nil {
		log.Fatal().Err(err).Msg("Fatal error")
	}
}

func newLogger(cfg LogConfig) zerolog.Logger {
	level, err := zerolog.ParseLevel(cfg.Level)
	if err != nil || cfg.Level == "" {
		level = zerolog.InfoLevel
	}
	var out = os.Stdout
	logger := zerolog.New(out).Level(level).With().Timestamp().Logger()
	if cfg.Pretty {
		logger = logger.Output(zerolog.ConsoleWriter{Out: out, TimeFormat: time.RFC3339})
	}
	return logger
}

func run(cfg *Config, log zerolog.Logger, checkOnly bool) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The media store must be readable. It should also be mounted read-only:
	// the worker never writes to it, and the mount option is what enforces
	// that rather than code discipline.
	if err := checkMediaStore(cfg.Media.StorePath); err != nil {
		return err
	}

	db, err := NewDB(ctx, cfg.Database, log)
	if err != nil {
		return err
	}
	defer db.Close()
	log.Info().Bool("update_last_access", cfg.Database.UpdateLastAccess).
		Msg("Connected to Synapse database")

	cache, err := NewThumbnailCache(cfg.Cache, log)
	if err != nil {
		return err
	}

	auth, err := NewTokenAuthenticator(cfg.Auth)
	if err != nil {
		return err
	}

	srv := &Server{
		cfg:         cfg,
		db:          db,
		paths:       NewMediaPaths(cfg.Media.StorePath),
		cache:       cache,
		thumbnailer: NewThumbnailer(cfg.Media.MaxImagePixels),
		auth:        auth,
		log:         log,
	}

	if cfg.Upstream.Download.configured() {
		if srv.downloadUp, err = NewProxy(cfg.Upstream.Download, log); err != nil {
			return fmt.Errorf("download upstream: %w", err)
		}
		log.Info().Str("target", srv.downloadUp.Target()).Msg("Download fallback configured")
	} else {
		log.Warn().Msg("No download upstream configured; uncached remote media will 404")
	}
	if cfg.Upstream.Thumbnail.configured() {
		if srv.thumbnailUp, err = NewProxy(cfg.Upstream.Thumbnail, log); err != nil {
			return fmt.Errorf("thumbnail upstream: %w", err)
		}
		log.Info().Str("target", srv.thumbnailUp.Target()).Msg("Thumbnail fallback configured")
	} else {
		log.Warn().Msg("No thumbnail upstream configured; animated and undecodable thumbnails will 404")
	}

	serverAuth, err := newFederationAuth(cfg, log)
	if err != nil {
		return err
	}

	if checkOnly {
		log.Info().Msg("Configuration and access checks passed")
		return nil
	}

	stopJanitor := make(chan struct{})
	go cache.RunJanitor(cfg.Cache.SweepInterval, stopJanitor)
	defer close(stopJanitor)

	handler := srv.routes(serverAuth)

	listener, err := makeListener(cfg.Listen)
	if err != nil {
		return err
	}
	httpServer := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
		// No WriteTimeout: large media downloads over slow links must not be
		// cut off mid-transfer.
		IdleTimeout: 120 * time.Second,
	}

	var metricsServer *http.Server
	if cfg.Listen.MetricsAddr != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		metricsServer = &http.Server{Addr: cfg.Listen.MetricsAddr, Handler: mux}
		go func() {
			if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error().Err(err).Msg("Metrics listener failed")
			}
		}()
		log.Info().Str("addr", cfg.Listen.MetricsAddr).Msg("Serving metrics")
	}

	errCh := make(chan error, 1)
	go func() {
		if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	log.Info().Str("listen", describeListener(cfg.Listen)).
		Str("server_name", cfg.ServerName).
		Msg("Media worker ready")

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		return err
	case sig := <-signals:
		log.Info().Str("signal", sig.String()).Msg("Shutting down")
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	if metricsServer != nil {
		_ = metricsServer.Shutdown(shutdownCtx)
	}
	return httpServer.Shutdown(shutdownCtx)
}

// routes wires the endpoints the worker owns. Anything not registered here
// stays with Synapse: uploads, URL previews, /_matrix/client/v1/media/config
// and every admin endpoint.
func (s *Server) routes(serverAuth *federation.ServerAuth) http.Handler {
	mux := http.NewServeMux()

	// Authenticated client media (MSC3916).
	mux.Handle("GET /_matrix/client/v1/media/download/{serverName}/{mediaId}",
		instrument("client_download", http.HandlerFunc(s.handleClientDownload)))
	mux.Handle("GET /_matrix/client/v1/media/download/{serverName}/{mediaId}/{fileName}",
		instrument("client_download", http.HandlerFunc(s.handleClientDownload)))
	mux.Handle("GET /_matrix/client/v1/media/thumbnail/{serverName}/{mediaId}",
		instrument("client_thumbnail", http.HandlerFunc(s.handleClientThumbnail)))

	// Legacy unauthenticated media. Media stored since authenticated media was
	// switched on is hidden from these, which on most servers is all of it.
	mux.Handle("GET /_matrix/media/{version}/download/{serverName}/{mediaId}",
		instrument("legacy_download", http.HandlerFunc(s.handleLegacyDownload)))
	mux.Handle("GET /_matrix/media/{version}/download/{serverName}/{mediaId}/{fileName}",
		instrument("legacy_download", http.HandlerFunc(s.handleLegacyDownload)))
	mux.Handle("GET /_matrix/media/{version}/thumbnail/{serverName}/{mediaId}",
		instrument("legacy_thumbnail", http.HandlerFunc(s.handleLegacyThumbnail)))

	// Federation media, behind X-Matrix signature verification.
	fedMux := http.NewServeMux()
	fedMux.Handle("GET /_matrix/federation/v1/media/download/{mediaId}",
		instrument("federation_download", http.HandlerFunc(s.handleFederationDownload)))
	fedMux.Handle("GET /_matrix/federation/v1/media/thumbnail/{mediaId}",
		instrument("federation_thumbnail", http.HandlerFunc(s.handleFederationThumbnail)))
	mux.Handle("/_matrix/federation/v1/media/", serverAuth.AuthenticateMiddleware(fedMux))

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.Handle("GET /metrics", promhttp.Handler())

	return withLogging(s.log, withCORSPreflight(mux))
}

// withCORSPreflight answers CORS preflight requests before routing.
//
// Authenticated media requires an Authorization header, which makes every
// media request from a cross-origin web client a preflighted one. Without this
// a browser on any origin other than the homeserver's cannot load media at all.
//
// It runs as middleware rather than as a route because the federation handler
// is registered for all methods on a path prefix, which a method-specific
// pattern for the same prefix would conflict with. Answering before the
// federation signature check is also correct: a preflight cannot carry an
// X-Matrix signature, and Synapse answers it unauthenticated too.
func withCORSPreflight(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			setCORSHeaders(w)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withLogging records one line per request at debug level.
func withLogging(log zerolog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		log.Debug().
			Str("method", r.Method).
			Str("path", r.URL.Path).
			Int("status", rec.status).
			Dur("duration", time.Since(start)).
			Msg("Request")
	})
}

// newFederationAuth builds the inbound X-Matrix verifier.
//
// mautrix's federation package does the whole job: parsing the header,
// fetching and caching the origin server's signing keys, checking their
// self-signatures and verifying the request signature. Reimplementing any of
// that would be a security liability for no gain.
func newFederationAuth(cfg *Config, log zerolog.Logger) (*federation.ServerAuth, error) {
	if cfg.Media.SigningKeyPath == "" {
		return nil, errors.New("media.signing_key_path is required to serve federation media")
	}
	key, total, err := loadSigningKey(cfg.Media.SigningKeyPath)
	if err != nil {
		return nil, err
	}
	log.Info().Str("key_id", string(key.ID)).Int("keys_in_file", total).
		Msg("Loaded signing key")

	// InMemoryCache satisfies both the resolution cache and the key cache, so
	// well-known lookups and signing keys are cached together.
	cache := federation.NewInMemoryCache()
	client := federation.NewClient(cfg.ServerName, key, cache, exhttp.SensibleClientSettings)
	auth := federation.NewServerAuth(client, cache,
		func(federation.XMatrixAuth) string { return cfg.ServerName })
	return auth, nil
}

// checkMediaStore verifies the media store is present and readable.
func checkMediaStore(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("media store %q is not accessible: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("media store %q is not a directory", path)
	}
	if _, err := os.Open(path); err != nil {
		return fmt.Errorf("media store %q is not readable: %w", path, err)
	}
	return nil
}

// makeListener opens the configured unix socket or TCP port. A stale socket
// from an unclean shutdown is removed first, since the worker is the only
// thing that should own that path.
func makeListener(cfg ListenConfig) (net.Listener, error) {
	if cfg.Socket == "" {
		l, err := net.Listen("tcp", cfg.Addr)
		if err != nil {
			return nil, fmt.Errorf("listening on %s: %w", cfg.Addr, err)
		}
		return l, nil
	}
	// The kernel caps sockaddr_un paths at 108 bytes including the terminator,
	// and the error it returns otherwise ("invalid argument") gives no hint why.
	if len(cfg.Socket) > 107 {
		return nil, fmt.Errorf("socket path %q is %d bytes; the kernel limit is 107",
			cfg.Socket, len(cfg.Socket))
	}
	if err := removeStaleSocket(cfg.Socket); err != nil {
		return nil, err
	}
	l, err := net.Listen("unix", cfg.Socket)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", cfg.Socket, err)
	}
	mode := cfg.SocketMode
	if mode == 0 {
		mode = 0o666
	}
	if err := os.Chmod(cfg.Socket, mode); err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("setting socket permissions: %w", err)
	}
	return l, nil
}

// removeStaleSocket deletes a socket file left behind by a previous run. It
// refuses to touch anything that is not a socket.
func removeStaleSocket(path string) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("checking socket path: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%q exists and is not a socket; refusing to remove it", path)
	}
	// If something is still listening, leave it alone and fail loudly.
	if conn, err := net.Dial("unix", path); err == nil {
		_ = conn.Close()
		return fmt.Errorf("%q is already in use by a running process", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("removing stale socket: %w", err)
	}
	return nil
}

func describeListener(cfg ListenConfig) string {
	if cfg.Socket != "" {
		return "unix:" + cfg.Socket
	}
	return cfg.Addr
}

// loadSigningKey reads Synapse's signing.key file, which may hold several keys
// one per line after a key rotation. Synapse signs with the first, so the
// worker uses the first too. The remaining keys are only ever needed to verify
// old signatures, which the worker does not do.
func loadSigningKey(path string) (*federation.SigningKey, int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, fmt.Errorf("reading signing key: %w", err)
	}
	var first *federation.SigningKey
	var count int
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, err := federation.ParseSynapseKey(line)
		if err != nil {
			return nil, 0, fmt.Errorf("parsing signing key line %d: %w", i+1, err)
		}
		count++
		if first == nil {
			first = key
		}
	}
	if first == nil {
		return nil, 0, fmt.Errorf("signing key file %q contains no keys", path)
	}
	return first, count, nil
}

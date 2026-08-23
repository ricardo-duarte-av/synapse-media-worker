package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"
)

// The media admin APIs live under prefixes that also carry unrelated admin
// APIs. Forwarding by prefix would make this a general admin proxy, so the
// paths are matched against the list docs/workers.md assigns to a media worker.
func TestOnlyMediaAdminPathsAreForwarded(t *testing.T) {
	forwarded := []string{
		"/_synapse/admin/v1/purge_media_cache",
		"/_synapse/admin/v1/media/example.com/abc",
		"/_synapse/admin/v1/media/example.com/delete",
		"/_synapse/admin/v1/quarantine_media/example.com/abc",
		"/_synapse/admin/v1/room/!abc:example.com/media",
		"/_synapse/admin/v1/room/!abc:example.com/media/quarantine",
		"/_synapse/admin/v1/user/@a:example.com/media",
		"/_synapse/admin/v1/users/@a:example.com/media",
	}
	for _, p := range forwarded {
		if !isMediaAdminPath(p) {
			t.Errorf("%q should be forwarded; it is a media admin API", p)
		}
	}

	// These share the prefixes but are nothing to do with media.
	refused := []string{
		"/_synapse/admin/v1/users/@a:example.com",
		"/_synapse/admin/v2/users/@a:example.com",
		"/_synapse/admin/v1/rooms/!abc:example.com",
		"/_synapse/admin/v1/room/!abc:example.com",
		"/_synapse/admin/v1/user/@a:example.com/devices",
		"/_synapse/admin/v1/register",
		"/_synapse/admin/v1/server_version",
		"/_synapse/admin/v1/purge_history/!abc:example.com",
		"/_synapse/admin/v1/purge_media_cache/extra",
	}
	for _, p := range refused {
		if isMediaAdminPath(p) {
			t.Errorf("%q must NOT be forwarded; it is not a media admin API", p)
		}
	}
}

// An unimplemented endpoint reaches Synapse rather than 404ing, which is the
// point: a deployment routes the whole media surface here without enumerating
// what this worker happens to implement.
func TestPassthroughForwardsUnimplementedEndpoints(t *testing.T) {
	var got string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path
		_, _ = w.Write([]byte(`{"m.upload.size":1000}`))
	}))
	defer upstream.Close()

	srv := newPassthroughServer(t, upstream.URL)
	for _, path := range []string{
		"/_matrix/client/v1/media/config",
		"/_matrix/client/v1/media/preview_url?url=x",
		"/_matrix/media/v3/preview_url?url=x",
		"/_matrix/media/v3/config",
	} {
		got = ""
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Errorf("%s: status %d, want it forwarded", path, w.Code)
		}
		if got == "" {
			t.Errorf("%s: never reached the upstream", path)
		}
	}
}

// Passing through must not swallow the endpoints the worker does implement.
func TestPassthroughDoesNotShadowOwnEndpoints(t *testing.T) {
	var reached bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	srv := newPassthroughServer(t, upstream.URL)
	// No token, so the worker's own handler answers 401 rather than the
	// request being forwarded.
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		"/_matrix/client/v1/media/download/example.com/abc", nil))
	if reached {
		t.Error("a download was forwarded instead of being handled here")
	}
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 from the worker's own handler", w.Code)
	}
}

// A non-media admin path must be refused here, not forwarded.
func TestAdminPassthroughRefusesNonMedia(t *testing.T) {
	var reached bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
	}))
	defer upstream.Close()

	srv := newPassthroughServer(t, upstream.URL)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		"/_synapse/admin/v1/users/@a:example.com", nil))
	if reached {
		t.Error("a non-media admin API was forwarded")
	}
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// An upstream aimed at our own socket would loop forever; it must be refused
// at startup instead.
func TestCheckNotSelf(t *testing.T) {
	listen := ListenConfig{Socket: "/var/sockets/nginx/av-media-worker-go.sock"}
	if err := checkNotSelf(UpstreamTarget{Socket: listen.Socket}, listen); err == nil {
		t.Error("an upstream pointing at our own socket was accepted")
	}
	// Also when it is one of several.
	if err := checkNotSelf(UpstreamTarget{
		Sockets: []string{"/var/sockets/nginx/av-media-worker-1.sock", listen.Socket},
	}, listen); err == nil {
		t.Error("a self-reference among several upstreams was accepted")
	}
	// A different worker is fine.
	if err := checkNotSelf(UpstreamTarget{Socket: "/var/sockets/nginx/av-media-worker-1.sock"}, listen); err != nil {
		t.Errorf("a legitimate upstream was refused: %v", err)
	}
	// And the TCP form.
	tcp := ListenConfig{Addr: "127.0.0.1:18090"}
	if err := checkNotSelf(UpstreamTarget{URL: "http://127.0.0.1:18090"}, tcp); err == nil {
		t.Error("an upstream pointing at our own address was accepted")
	}
}

// newPassthroughServer builds a minimal server with passthrough wired up.
func testLogger() zerolog.Logger { return zerolog.Nop() }

func newPassthroughServer(t *testing.T, upstreamURL string) http.Handler {
	t.Helper()
	cfg := defaultConfig()
	cfg.ServerName = "example.com"
	p, err := NewProxy(UpstreamTarget{URL: upstreamURL}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	auth, err := NewTokenAuthenticator(AuthConfig{WhoamiURL: upstreamURL})
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{cfg: &cfg, auth: auth, log: testLogger(), passthroughUp: p}

	mux := http.NewServeMux()
	mux.Handle("GET /_matrix/client/v1/media/download/{serverName}/{mediaId}",
		http.HandlerFunc(srv.handleClientDownload))
	for _, prefix := range mediaPrefixes {
		mux.Handle(prefix, http.HandlerFunc(srv.handlePassthrough))
	}
	for _, prefix := range []string{
		"/_synapse/admin/v1/purge_media_cache", "/_synapse/admin/v1/media/",
		"/_synapse/admin/v1/quarantine_media/", "/_synapse/admin/v1/room/",
		"/_synapse/admin/v1/user/", "/_synapse/admin/v1/users/",
	} {
		mux.Handle(prefix, http.HandlerFunc(srv.handleAdminPassthrough))
	}
	return mux
}

// ServeMux panics on a duplicate or conflicting pattern, and it does so at
// registration -- which is startup, in production. The passthrough prefixes
// overlap the worker's own patterns by design, so this asserts the real route
// table still builds.
func TestRoutesRegisterWithoutConflict(t *testing.T) {
	cfg := defaultConfig()
	cfg.ServerName = "example.com"
	auth, err := NewTokenAuthenticator(AuthConfig{WhoamiURL: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{cfg: &cfg, auth: auth, log: testLogger()}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("routes() panicked, which would be a startup crash: %v", r)
		}
	}()
	if srv.routes(nil) == nil {
		t.Fatal("routes() returned nil")
	}
}

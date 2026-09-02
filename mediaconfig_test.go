package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// newMediaConfigServer builds a worker whose whoami accepts any token.
func newMediaConfigServer(t *testing.T, maxUpload int64) *Server {
	t.Helper()
	auth, _ := newTestAuth(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user_id":"@a:example.com","is_guest":false}`))
	})
	cfg := defaultConfig()
	cfg.ServerName = "example.com"
	cfg.Media.MaxUploadSize = &maxUpload
	return &Server{cfg: &cfg, auth: auth, log: testLogger()}
}

func mediaConfigRequest(path string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("Authorization", "Bearer tok")
	return r
}

// The body is compared byte for byte: Synapse encodes it with
// encode_canonical_json, which has no spaces and no trailing newline, and the
// headers are the ones respond_with_json sets.
func TestMediaConfigMatchesSynapse(t *testing.T) {
	s := newMediaConfigServer(t, 104857600)

	for _, path := range []string{
		"/_matrix/client/v1/media/config",
		"/_matrix/media/v3/config",
	} {
		w := httptest.NewRecorder()
		mediaConfigRoutes(s).ServeHTTP(w, mediaConfigRequest(path))

		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", path, w.Code)
		}
		if got, want := w.Body.String(), `{"m.upload.size":104857600}`; got != want {
			t.Errorf("%s: body = %q, want %q", path, got, want)
		}
		h := w.Result().Header
		if got := h.Get("Content-Type"); got != "application/json" {
			t.Errorf("%s: Content-Type = %q", path, got)
		}
		if got := h.Get("Cache-Control"); got != "no-cache, no-store, must-revalidate" {
			t.Errorf("%s: Cache-Control = %q", path, got)
		}
		// send_cors=True: without it a web client on another origin sees
		// nothing, exactly as on the download endpoints.
		if got := h.Get("Access-Control-Allow-Origin"); got != "*" {
			t.Errorf("%s: Access-Control-Allow-Origin = %q", path, got)
		}
		if got, want := h.Get("Content-Length"), strconv.Itoa(w.Body.Len()); got != want {
			t.Errorf("%s: Content-Length = %q, want %q", path, got, want)
		}
	}
}

// The limit reported is the one uploads are actually checked against, whether
// it came from this worker's config or from homeserver.yaml.
func TestMediaConfigReportsSynapseDefaultWhenUnset(t *testing.T) {
	s := newMediaConfigServer(t, 0)
	s.cfg.Media.MaxUploadSize = nil

	w := httptest.NewRecorder()
	mediaConfigRoutes(s).ServeHTTP(w, mediaConfigRequest("/_matrix/client/v1/media/config"))
	if got, want := w.Body.String(), `{"m.upload.size":52428800}`; got != want {
		t.Errorf("body = %q, want Synapse's 50M default %q", got, want)
	}
}

// Synapse's MediaConfigResource calls get_user_by_req before answering, on
// both spellings -- the /_matrix/media one included, despite that family
// otherwise being the unauthenticated one.
func TestMediaConfigRequiresAuth(t *testing.T) {
	s := newMediaConfigServer(t, 100)
	for _, path := range []string{
		"/_matrix/client/v1/media/config",
		"/_matrix/media/v3/config",
	} {
		w := httptest.NewRecorder()
		mediaConfigRoutes(s).ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401 without a token", path, w.Code)
		}
	}
}

// Only the versions Synapse's pattern lists exist.
func TestMediaConfigLegacyVersions(t *testing.T) {
	s := newMediaConfigServer(t, 100)
	for _, tc := range []struct {
		version string
		want    int
	}{
		{"r0", http.StatusOK},
		{"v1", http.StatusOK},
		{"v3", http.StatusOK},
		{"v2", http.StatusNotFound},
		{"v4", http.StatusNotFound},
	} {
		w := httptest.NewRecorder()
		mediaConfigRoutes(s).ServeHTTP(w,
			mediaConfigRequest("/_matrix/media/"+tc.version+"/config"))
		if w.Code != tc.want {
			t.Errorf("version %q: status = %d, want %d", tc.version, w.Code, tc.want)
		}
	}
}

// A module can replace the response per user via get_media_config_for_user,
// and the worker cannot run Synapse's modules. Answering from our own config
// would report a limit that is not this user's, so the request must go to
// Synapse instead.
func TestMediaConfigProxiedWhenSynapseLoadsModules(t *testing.T) {
	var reached bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		_, _ = w.Write([]byte(`{"m.upload.size":1}`))
	}))
	defer upstream.Close()

	s := newMediaConfigServer(t, 100)
	s.cfg.Media.synapseModules = true
	p, err := NewProxy(UpstreamTarget{URL: upstream.URL}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	s.passthroughUp = p

	w := httptest.NewRecorder()
	mediaConfigRoutes(s).ServeHTTP(w, mediaConfigRequest("/_matrix/client/v1/media/config"))
	if !reached {
		t.Fatal("the request was answered here instead of by Synapse")
	}
	if got := w.Body.String(); got != `{"m.upload.size":1}` {
		t.Errorf("body = %q, want Synapse's answer", got)
	}
}

// modules in homeserver.yaml must turn the proxying on by itself: an operator
// who loads a module should not have to know this endpoint exists.
func TestSynapseModulesDetected(t *testing.T) {
	hs := filepath.Join(t.TempDir(), "homeserver.yaml")
	write := func(body string) {
		if err := os.WriteFile(hs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(`
server_name: example.com
modules:
  - module: my.module.Thing
    config: {}
`)
	cfg := defaultConfig()
	cfg.SynapseConfig = hs
	if err := cfg.deriveFromSynapse(); err != nil {
		t.Fatal(err)
	}
	if !cfg.Media.ProxyMediaConfig() {
		t.Error("modules are configured in Synapse but /media/config would be answered here")
	}

	write("server_name: example.com\n")
	cfg = defaultConfig()
	cfg.SynapseConfig = hs
	if err := cfg.deriveFromSynapse(); err != nil {
		t.Fatal(err)
	}
	if cfg.Media.ProxyMediaConfig() {
		t.Error("no modules are configured but /media/config would be proxied")
	}
}

// mediaConfigRoutes registers the two patterns exactly as routes() does.
func mediaConfigRoutes(s *Server) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /_matrix/client/v1/media/config",
		http.HandlerFunc(s.handleMediaConfig))
	mux.Handle("GET /_matrix/media/{version}/config",
		http.HandlerFunc(s.handleLegacyMediaConfig))
	return mux
}

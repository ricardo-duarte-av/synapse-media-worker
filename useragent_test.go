package main

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/exhttp"
	"maunium.net/go/mautrix/federation"
)

// uaRecorder is a homeserver that reports a version and remembers the
// User-Agent of every request it saw.
type uaRecorder struct {
	mu      sync.Mutex
	version string
	seen    []string
}

func (r *uaRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.seen = append(r.seen, req.Header.Get("User-Agent"))
	version := r.version
	r.mu.Unlock()
	if req.URL.Path == "/_matrix/federation/v1/version" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"server":{"name":"Synapse","version":"` + version + `"}}`))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (r *uaRecorder) last() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seen[len(r.seen)-1]
}

func (r *uaRecorder) setVersion(v string) {
	r.mu.Lock()
	r.version = v
	r.mu.Unlock()
}

// newUATestClient builds a federation client the way newFederationAuth does,
// with "test.example" pre-resolved to a local TLS server so no DNS or
// .well-known is involved.
func newUATestClient(t *testing.T, rec *uaRecorder, ua *UserAgent) (*federation.Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewTLSServer(rec)
	t.Cleanup(srv.Close)

	client := federation.NewClient("ours.example", nil, federation.NewInMemoryCache(), exhttp.SensibleClientSettings)
	client.AllowIP = nil // the test server is on loopback
	ft := InstallFederationTransport(client, exhttp.SensibleClientSettings, zerolog.Nop())
	ft.pin("test.example", srv.Listener.Addr().String())
	insecure := &tls.Config{InsecureSkipVerify: true}
	ft.Transport.TLSClientConfig = insecure
	client.ExtHTTP.Transport.(*http.Transport).TLSClientConfig = insecure

	InstallUserAgent(client, ua)
	return client, srv
}

func TestFederationRequestsCarrySynapsesUserAgent(t *testing.T) {
	rec := &uaRecorder{version: "1.164.1"}
	ua := NewUserAgent("", "contact/admin@ours.example")
	client, srv := newUATestClient(t, rec, ua)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	RefreshUserAgent(ctx, client, ua, "test.example", 5*time.Second, zerolog.Nop())

	// The discovery request itself went out before the version was known.
	if got := rec.seen[0]; got != "Synapse" {
		t.Errorf("discovery request User-Agent = %q, want the fallback %q", got, "Synapse")
	}

	if _, err := client.Version(ctx, "test.example"); err != nil {
		t.Fatal(err)
	}
	// Signed federation requests: Synapse sets the header per request from
	// hs.version_string, so user_agent_suffix never reaches them.
	if got := rec.last(); got != "Synapse/1.164.1" {
		t.Errorf("federation User-Agent = %q, want %q", got, "Synapse/1.164.1")
	}

	// Plain HTTPS, as for a media download's Location redirect, goes through
	// Synapse's SimpleHttpClient and does carry the suffix.
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/redirected", nil)
	resp, err := client.ExtHTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if want := "Synapse/1.164.1 contact/admin@ours.example"; rec.last() != want {
		t.Errorf("external User-Agent = %q, want %q", rec.last(), want)
	}

	// .well-known lookups go through the agent, which carries the suffix too.
	ft := client.HTTP.Transport.(*userAgentTransport).next.(*FederationTransport)
	if ft.WellKnownClient == client.ExtHTTP {
		t.Fatal(".well-known resolution does not carry the User-Agent")
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/.well-known/matrix/server", nil)
	resp, err = ft.WellKnownClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if want := "Synapse/1.164.1 contact/admin@ours.example"; rec.last() != want {
		t.Errorf(".well-known User-Agent = %q, want %q", rec.last(), want)
	}

	for _, seen := range rec.seen {
		if strings.Contains(seen, "mautrix") || strings.Contains(seen, "Go-http-client") {
			t.Errorf("a request went out as %q", seen)
		}
	}
}

func TestUserAgentFollowsAnUpgrade(t *testing.T) {
	rec := &uaRecorder{version: "1.164.1"}
	ua := NewUserAgent("", "")
	client, _ := newUATestClient(t, rec, ua)

	base, err := discoverSynapseUserAgent(context.Background(), client, "test.example")
	if err != nil {
		t.Fatal(err)
	}
	ua.set(base)
	rec.setVersion("1.165.0")
	base, err = discoverSynapseUserAgent(context.Background(), client, "test.example")
	if err != nil {
		t.Fatal(err)
	}
	if !ua.set(base) || ua.Federation() != "Synapse/1.165.0" {
		t.Errorf("after an upgrade the User-Agent is %q", ua.Federation())
	}
	if ua.External() != ua.Federation() {
		t.Errorf("with no suffix, external %q should equal federation %q",
			ua.External(), ua.Federation())
	}
}

func TestConfiguredUserAgentIsNotRediscovered(t *testing.T) {
	rec := &uaRecorder{version: "1.164.1"}
	ua := NewUserAgent("Synapse/1.100.0", "")
	client, _ := newUATestClient(t, rec, ua)

	RefreshUserAgent(context.Background(), client, ua, "test.example", time.Second, zerolog.Nop())
	if len(rec.seen) != 0 {
		t.Errorf("a configured User-Agent still asked the homeserver its version")
	}
	if _, err := client.Version(context.Background(), "test.example"); err != nil {
		t.Fatal(err)
	}
	if got := rec.last(); got != "Synapse/1.100.0" {
		t.Errorf("User-Agent = %q, want the configured %q", got, "Synapse/1.100.0")
	}
}

func TestUnreachableHomeserverKeepsTheFallback(t *testing.T) {
	ua := NewUserAgent("", "")
	client := federation.NewClient("ours.example", nil, federation.NewInMemoryCache(), exhttp.SensibleClientSettings)
	InstallFederationTransport(client, exhttp.SensibleClientSettings, zerolog.Nop())
	InstallUserAgent(client, ua)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// .invalid never resolves, so this fails quickly and must not block.
	RefreshUserAgent(ctx, client, ua, "nowhere.invalid", 2*time.Second, zerolog.Nop())
	if got := ua.Federation(); got != "Synapse" {
		t.Errorf("User-Agent after a failed lookup = %q, want %q", got, "Synapse")
	}
}

func TestUserAgentTransportLeavesTheRequestAlone(t *testing.T) {
	rt := withUserAgent(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if got := r.Header.Get("User-Agent"); got != "Synapse/1" {
			t.Errorf("sent User-Agent = %q", got)
		}
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
	}), func() string { return "Synapse/1" })

	req, _ := http.NewRequest(http.MethodGet, "https://example.com/", nil)
	req.Header.Set("User-Agent", "mautrix-go/0")
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("User-Agent"); got != "mautrix-go/0" {
		t.Errorf("caller's request was modified: User-Agent = %q", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

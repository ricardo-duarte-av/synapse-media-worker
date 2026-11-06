package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// resolverHarness drives a FederationTransport with a fake clock, a fake
// .well-known server and a fake DNS.
type resolverHarness struct {
	t   *testing.T
	ft  *FederationTransport
	now time.Time

	mu sync.Mutex
	// wellKnown answers a .well-known request for a host; nil means the
	// connection is refused.
	wellKnown map[string]func() (*http.Response, error)
	fetches   map[string]int
	// srv answers a lookup of "_service._tcp.host".
	srv     map[string]func() ([]*net.SRV, error)
	dialled []string
}

func newResolverHarness(t *testing.T) *resolverHarness {
	h := &resolverHarness{
		t:         t,
		now:       time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC),
		wellKnown: map[string]func() (*http.Response, error){},
		fetches:   map[string]int{},
		srv:       map[string]func() ([]*net.SRV, error){},
	}
	h.ft = &FederationTransport{
		WellKnownClient: &http.Client{Transport: roundTripFunc(h.serveWellKnown)},
		dial:            h.dial,
		lookup:          h.lookup,
		log:             zerolog.Nop(),
		now:             func() time.Time { return h.now },
		sleep:           func(context.Context, time.Duration) error { return nil },
		jitter:          func() float64 { return 1 },
		wellKnown:       map[string]wellKnownEntry{},
		hadValid:        map[string]time.Time{},
		srv:             map[string][]srvTarget{},
		inflight:        map[string]chan struct{}{},
	}
	return h
}

func (h *resolverHarness) serveWellKnown(req *http.Request) (*http.Response, error) {
	if req.URL.Path != "/.well-known/matrix/server" {
		h.t.Errorf("unexpected .well-known path %q", req.URL.Path)
	}
	h.mu.Lock()
	h.fetches[req.URL.Host]++
	answer := h.wellKnown[req.URL.Host]
	h.mu.Unlock()
	if answer == nil {
		return nil, errors.New("connection refused")
	}
	return answer()
}

func (h *resolverHarness) lookup(_ context.Context, service, proto, name string) (string, []*net.SRV, error) {
	key := "_" + service + "._" + proto + "." + name
	h.mu.Lock()
	answer := h.srv[key]
	h.mu.Unlock()
	if answer == nil {
		return "", nil, &net.DNSError{Err: "no such host", Name: key, IsNotFound: true}
	}
	records, err := answer()
	return "", records, err
}

func (h *resolverHarness) dial(_ context.Context, _, addr string) (net.Conn, error) {
	h.mu.Lock()
	h.dialled = append(h.dialled, addr)
	h.mu.Unlock()
	return nil, errors.New("test dialer does not connect")
}

func (h *resolverHarness) setWellKnown(host string, f func() (*http.Response, error)) {
	h.mu.Lock()
	h.wellKnown[host] = f
	h.mu.Unlock()
}

func (h *resolverHarness) fetchCount(host string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.fetches[host]
}

func delegateTo(server string, headers ...string) func() (*http.Response, error) {
	return func() (*http.Response, error) {
		resp := jsonResponse(http.StatusOK, `{"m.server":"`+server+`"}`)
		for i := 0; i+1 < len(headers); i += 2 {
			resp.Header.Add(headers[i], headers[i+1])
		}
		return resp, nil
	}
}

func status(code int) func() (*http.Response, error) {
	return func() (*http.Response, error) { return jsonResponse(code, `{}`), nil }
}

func jsonResponse(code int, body string) *http.Response {
	return &http.Response{
		StatusCode: code,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// hostFor resolves serverName and returns the Host header it would send.
func (h *resolverHarness) hostFor(serverName string) string {
	h.t.Helper()
	host, _, err := h.ft.resolve(context.Background(), serverName)
	if err != nil {
		h.t.Fatalf("resolve(%q): %v", serverName, err)
	}
	return host
}

func TestWellKnownOutageIsNotCachedForADay(t *testing.T) {
	h := newResolverHarness(t)
	// The .well-known server is restarting when we first ask.
	if got := h.hostFor("example.org"); got != "example.org" {
		t.Fatalf("Host = %q, want example.org (no delegation)", got)
	}
	// It came back. The failure is cached for an hour, as in Synapse, and
	// like any entry is refreshed from 80% of that.
	h.setWellKnown("example.org", delegateTo("matrix.example.org:443"))
	h.now = h.now.Add(47 * time.Minute)
	if got := h.hostFor("example.org"); got != "example.org" {
		t.Errorf("Host = %q before the failure was due a refresh", got)
	}
	h.now = h.now.Add(2 * time.Minute)
	if got := h.hostFor("example.org"); got != "matrix.example.org:443" {
		t.Errorf("Host = %q 49 minutes after the outage, want the delegation", got)
	}
}

func TestOutageAfterAValidWellKnownIsShortAndRetried(t *testing.T) {
	h := newResolverHarness(t)
	h.setWellKnown("example.org", delegateTo("matrix.example.org:443"))
	if got := h.hostFor("example.org"); got != "matrix.example.org:443" {
		t.Fatalf("Host = %q", got)
	}

	// The entry expires while the server is down.
	h.setWellKnown("example.org", status(http.StatusBadGateway))
	h.now = h.now.Add(25 * time.Hour)
	before := h.fetchCount("example.org")
	if got := h.hostFor("example.org"); got != "example.org" {
		t.Errorf("Host = %q with the entry expired and the server down", got)
	}
	if n := h.fetchCount("example.org") - before; n != wellKnownRetryAttempts {
		t.Errorf("fetched %d times, want %d retries for a domain that had a valid .well-known", n, wellKnownRetryAttempts)
	}

	// That failure is cached for two minutes, not an hour.
	h.setWellKnown("example.org", delegateTo("matrix.example.org:443"))
	h.now = h.now.Add(wellKnownDownCachePeriod + time.Second)
	if got := h.hostFor("example.org"); got != "matrix.example.org:443" {
		t.Errorf("Host = %q two minutes after the outage, want the delegation back", got)
	}
}

func TestTemporaryFailureKeepsACachedDelegation(t *testing.T) {
	h := newResolverHarness(t)
	h.setWellKnown("example.org", delegateTo("matrix.example.org:443", "Cache-Control", "max-age=3600"))
	h.hostFor("example.org")

	// Past 80% of the TTL a refresh is attempted; the server is down.
	h.setWellKnown("example.org", nil)
	h.now = h.now.Add(50 * time.Minute)
	before := h.fetchCount("example.org")
	if got := h.hostFor("example.org"); got != "matrix.example.org:443" {
		t.Errorf("Host = %q, want the still-cached delegation", got)
	}
	if h.fetchCount("example.org") == before {
		t.Error("no refresh was attempted inside the grace period")
	}
}

func TestPermanentFailureDropsACachedDelegation(t *testing.T) {
	h := newResolverHarness(t)
	h.setWellKnown("example.org", delegateTo("matrix.example.org:443", "Cache-Control", "max-age=3600"))
	h.hostFor("example.org")

	// A 404 is an answer, not an outage: the delegation was removed.
	h.setWellKnown("example.org", status(http.StatusNotFound))
	h.now = h.now.Add(50 * time.Minute)
	if got := h.hostFor("example.org"); got != "example.org" {
		t.Errorf("Host = %q after the .well-known was removed", got)
	}
}

func TestWellKnownIsNotConsultedWithAPortOrIP(t *testing.T) {
	h := newResolverHarness(t)
	for _, name := range []string{"example.org:8448", "192.0.2.1", "[2001:db8::1]"} {
		_, target, err := h.ft.resolve(context.Background(), name)
		if err != nil {
			t.Fatal(err)
		}
		if !target.explicit {
			t.Errorf("%s: target %+v is not explicit", name, target)
		}
	}
	if len(h.fetches) != 0 {
		t.Errorf(".well-known fetched for %v", h.fetches)
	}
}

func TestWellKnownCachePeriods(t *testing.T) {
	cases := []struct {
		name    string
		headers []string
		want    time.Duration
	}{
		{"default", nil, wellKnownDefaultCachePeriod},
		{"max-age", []string{"Cache-Control", "public, max-age=7200"}, 2 * time.Hour},
		{"max-age below the floor", []string{"Cache-Control", "max-age=10"}, wellKnownMinCachePeriod},
		{"max-age above the cap", []string{"Cache-Control", "max-age=999999"}, wellKnownMaxCachePeriod},
		// Synapse clamps no-store like any other figure.
		{"no-store", []string{"Cache-Control", "no-store"}, wellKnownMinCachePeriod},
		{"expires", []string{"Expires", "Fri, 06 Nov 2026 15:00:00 GMT"}, 3 * time.Hour},
		{"invalid expires", []string{"Expires", "0"}, wellKnownMinCachePeriod},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newResolverHarness(t)
			h.setWellKnown("example.org", delegateTo("matrix.example.org:443", c.headers...))
			h.hostFor("example.org")
			if got := h.ft.wellKnown["example.org"].ttl; got != c.want {
				t.Errorf("cached for %v, want %v", got, c.want)
			}
		})
	}
}

func TestSRVResolution(t *testing.T) {
	h := newResolverHarness(t)
	ctx := context.WithValue(context.Background(), dialTargetKey{}, dialTarget{host: "example.org"})
	dialled := func() string {
		h.mu.Lock()
		defer h.mu.Unlock()
		out := strings.Join(h.dialled, ",")
		h.dialled = nil
		return out
	}

	// Nothing published: host:8448.
	_, _ = h.ft.dialContext(ctx, "tcp", "")
	if got := dialled(); got != "example.org:8448" {
		t.Errorf("no SRV: dialled %q", got)
	}

	// Legacy record only.
	h.srv["_matrix._tcp.example.org"] = func() ([]*net.SRV, error) {
		return []*net.SRV{{Target: "legacy.example.org.", Port: 8449}}, nil
	}
	_, _ = h.ft.dialContext(ctx, "tcp", "")
	if got := dialled(); got != "legacy.example.org:8449" {
		t.Errorf("legacy SRV: dialled %q", got)
	}

	// Both records: _matrix-fed wins, and every target is tried in order.
	h.srv["_matrix-fed._tcp.example.org"] = func() ([]*net.SRV, error) {
		return []*net.SRV{{Target: "a.example.org.", Port: 443}, {Target: "b.example.org.", Port: 443}}, nil
	}
	_, _ = h.ft.dialContext(ctx, "tcp", "")
	if got := dialled(); got != "a.example.org:443,b.example.org:443" {
		t.Errorf("matrix-fed SRV: dialled %q", got)
	}

	// DNS fails: the last answer is used, never host:8448.
	h.srv["_matrix-fed._tcp.example.org"] = func() ([]*net.SRV, error) {
		return nil, &net.DNSError{Err: "server misbehaving", IsTemporary: true}
	}
	_, _ = h.ft.dialContext(ctx, "tcp", "")
	if got := dialled(); got != "a.example.org:443,b.example.org:443" {
		t.Errorf("DNS failure with a cached answer: dialled %q", got)
	}

	// DNS fails with nothing cached: the connection fails.
	other := context.WithValue(context.Background(), dialTargetKey{}, dialTarget{host: "other.org"})
	h.srv["_matrix-fed._tcp.other.org"] = h.srv["_matrix-fed._tcp.example.org"]
	if _, err := h.ft.dialContext(other, "tcp", ""); err == nil || !strings.Contains(err.Error(), "SRV lookup") {
		t.Errorf("DNS failure without a cached answer: err = %v", err)
	}
	if got := dialled(); got != "" {
		t.Errorf("DNS failure without a cached answer dialled %q", got)
	}

	// A "." target means the service is explicitly unavailable.
	h.srv["_matrix-fed._tcp.other.org"] = func() ([]*net.SRV, error) {
		return []*net.SRV{{Target: ".", Port: 0}}, nil
	}
	if _, err := h.ft.dialContext(other, "tcp", ""); err == nil {
		t.Error(`a "." SRV target was dialled`)
	}
}

func TestDelegationWithoutPortUsesSRVOfTheDelegate(t *testing.T) {
	h := newResolverHarness(t)
	h.setWellKnown("example.org", delegateTo("matrix.example.org"))
	host, target, err := h.ft.resolve(context.Background(), "example.org")
	if err != nil {
		t.Fatal(err)
	}
	if host != "matrix.example.org" || target.explicit || target.host != "matrix.example.org" {
		t.Errorf("host %q, target %+v", host, target)
	}
}

func TestInvalidDelegationFails(t *testing.T) {
	h := newResolverHarness(t)
	h.setWellKnown("example.org", delegateTo("not a server name"))
	if _, _, err := h.ft.resolve(context.Background(), "example.org"); err == nil {
		t.Error("an invalid m.server resolved")
	}
}

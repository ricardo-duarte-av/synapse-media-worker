package main

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// rewriteHeaders runs the proxy's rewrite step and returns what the upstream
// would receive.
func rewriteHeaders(t *testing.T, remoteAddr string, in http.Header) http.Header {
	t.Helper()
	inReq := httptest.NewRequest("GET", "/_matrix/client/v1/media/download/matrix.org/abc", nil)
	inReq.RemoteAddr = remoteAddr
	inReq.Host = "example.com"
	for k, vs := range in {
		for _, v := range vs {
			inReq.Header.Add(k, v)
		}
	}
	outReq := inReq.Clone(inReq.Context())
	target, _ := url.Parse("http://synapse")
	pr := &httputil.ProxyRequest{In: inReq, Out: outReq}
	pr.SetURL(target)

	// ReverseProxy strips these before calling a Rewrite hook; reproduce that
	// so the test exercises the same starting state as production.
	for _, h := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
		pr.Out.Header.Del(h)
	}

	forwardXForwardedFor(pr)
	return pr.Out.Header
}

// The production listener is a unix socket, so RemoteAddr is not host:port and
// net.SplitHostPort fails. Synapse reads getClientAddress().host on its
// remote-media path; without X-Forwarded-For that is a UNIXAddress with no
// .host and every uncached remote media fetch fails with a 500.
func TestForwardedForSurvivesUnixSocketListener(t *testing.T) {
	got := rewriteHeaders(t, "@", http.Header{
		"X-Forwarded-For":   {"172.25.0.1"},
		"X-Forwarded-Proto": {"https"},
	})
	if v := got.Get("X-Forwarded-For"); v != "172.25.0.1" {
		t.Errorf("X-Forwarded-For = %q, want it preserved", v)
	}
	// The front proxy knows the real scheme; the worker's own listener is
	// plain HTTP and must not overwrite it.
	if v := got.Get("X-Forwarded-Proto"); v != "https" {
		t.Errorf("X-Forwarded-Proto = %q, want https", v)
	}
}

// On a TCP listener our own peer is appended to the chain.
func TestForwardedForAppendsPeerOnTCP(t *testing.T) {
	got := rewriteHeaders(t, "10.0.0.5:4321", http.Header{
		"X-Forwarded-For": {"198.51.100.7"},
	})
	if v := got.Get("X-Forwarded-For"); v != "198.51.100.7, 10.0.0.5" {
		t.Errorf("X-Forwarded-For = %q, want the peer appended", v)
	}
}

func TestForwardedForWhenNoneInbound(t *testing.T) {
	// TCP with no inbound chain: our peer becomes the chain.
	got := rewriteHeaders(t, "10.0.0.5:4321", http.Header{})
	if v := got.Get("X-Forwarded-For"); v != "10.0.0.5" {
		t.Errorf("X-Forwarded-For = %q, want the peer", v)
	}
	// Unix socket with no inbound chain: nothing to say, and nothing invented.
	got = rewriteHeaders(t, "@", http.Header{})
	if v := got.Get("X-Forwarded-For"); v != "" {
		t.Errorf("X-Forwarded-For = %q, want empty rather than fabricated", v)
	}
}

func TestForwardedHostFallsBackToRequestHost(t *testing.T) {
	got := rewriteHeaders(t, "@", http.Header{})
	if v := got.Get("X-Forwarded-Host"); v != "example.com" {
		t.Errorf("X-Forwarded-Host = %q", v)
	}
	if v := got.Get("X-Forwarded-Proto"); v != "http" {
		t.Errorf("X-Forwarded-Proto = %q, want http for a plain listener", v)
	}
	// An inbound value wins.
	got = rewriteHeaders(t, "@", http.Header{"X-Forwarded-Host": {"real.example.net"}})
	if v := got.Get("X-Forwarded-Host"); v != "real.example.net" {
		t.Errorf("X-Forwarded-Host = %q, want the inbound value", v)
	}
}

// --- upstream pool ---------------------------------------------------------

func newTestPool(t *testing.T, urls ...string) *Proxy {
	t.Helper()
	p, err := NewProxy(UpstreamTarget{URLs: urls}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPoolSpreadsAcrossUpstreams(t *testing.T) {
	var hits [3]int32
	var srvs []*httptest.Server
	var urls []string
	for i := range hits {
		i := i
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt32(&hits[i], 1)
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(s.Close)
		srvs = append(srvs, s)
		urls = append(urls, s.URL)
	}
	_ = srvs

	p := newTestPool(t, urls...)
	for range 30 {
		w := httptest.NewRecorder()
		p.ServeHTTP(w, httptest.NewRequest("GET", "/_matrix/media/v3/download/x/y", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("status %d", w.Code)
		}
	}
	for i, h := range hits {
		if atomic.LoadInt32(&h) == 0 {
			t.Errorf("upstream %d received no requests: %v", i, hits)
		}
	}
}

// A dead upstream must not take requests down with it: nothing has been
// written to the client yet, and a GET has no body to replay, so another
// upstream can serve it.
func TestPoolFailsOverToAHealthyUpstream(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("served"))
	}))
	defer good.Close()

	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // nothing is listening any more

	p := newTestPool(t, deadURL, good.URL)
	for range 6 {
		w := httptest.NewRecorder()
		p.ServeHTTP(w, httptest.NewRequest("GET", "/_matrix/media/v3/download/x/y", nil))
		if w.Code != http.StatusOK || w.Body.String() != "served" {
			t.Fatalf("status %d body %q, want the healthy upstream to answer", w.Code, w.Body.String())
		}
	}
}

// When every upstream is unreachable the client gets a clean 502, not a hang
// or a panic.
func TestPoolAllUpstreamsDown(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	u := dead.URL
	dead.Close()

	p := newTestPool(t, u)
	w := httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest("GET", "/_matrix/media/v3/download/x/y", nil))
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
}

// An upstream error that is not a connection failure must not be retried: the
// upstream has already acted on the request.
func TestPoolDoesNotRetryRealResponses(t *testing.T) {
	var calls int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer s.Close()

	p := newTestPool(t, s.URL, s.URL)
	w := httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest("GET", "/_matrix/media/v3/download/x/y", nil))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want the upstream's 500 passed through", w.Code)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("upstream called %d times, want 1: a 500 is an answer, not a connection failure", got)
	}
}

// In-flight accounting must track the whole streamed response, or the balancer
// treats a slow download as finished the moment its headers arrive.
func TestInflightReleasedOnlyWhenBodyIsClosed(t *testing.T) {
	release := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-release
		_, _ = w.Write([]byte("tail"))
	}))
	defer s.Close()

	p := newTestPool(t, s.URL)
	ep := p.endpoints[0]

	done := make(chan struct{})
	go func() {
		defer close(done)
		w := httptest.NewRecorder()
		p.ServeHTTP(w, httptest.NewRequest("GET", "/_matrix/media/v3/download/x/y", nil))
	}()

	deadline := time.Now().Add(2 * time.Second)
	for ep.inflight.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if ep.inflight.Load() != 1 {
		t.Fatalf("inflight = %d while the body is still streaming, want 1", ep.inflight.Load())
	}

	close(release)
	<-done
	for ep.inflight.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := ep.inflight.Load(); got != 0 {
		t.Errorf("inflight = %d after completion, want 0", got)
	}
}

func TestEndpointsCombinesSingularAndPlural(t *testing.T) {
	got := UpstreamTarget{
		Socket:  "/a.sock",
		Sockets: []string{"/b.sock", ""},
		URL:     "http://c",
		URLs:    []string{"http://d"},
	}.Endpoints()
	want := []string{"unix:/a.sock", "unix:/b.sock", "http://c", "http://d"}
	if len(got) != len(want) {
		t.Fatalf("got %d endpoints, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Name() != want[i] {
			t.Errorf("endpoint %d = %q, want %q", i, got[i].Name(), want[i])
		}
	}
	if (UpstreamTarget{}).configured() {
		t.Error("empty target reported as configured")
	}
}

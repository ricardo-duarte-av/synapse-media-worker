package main

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"
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

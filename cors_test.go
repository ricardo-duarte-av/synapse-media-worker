package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Authenticated media requires an Authorization header, so every media request
// from a cross-origin web client is preflighted. If preflight fails, those
// clients cannot load media at all -- and nothing else in the worker would
// report an error.
func TestCORSPreflightAnswered(t *testing.T) {
	reached := false
	h := withCORSPreflight(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	}))

	r := httptest.NewRequest(http.MethodOptions,
		"/_matrix/client/v1/media/download/example.com/abc123", nil)
	r.Header.Set("Origin", "https://app.element.io")
	r.Header.Set("Access-Control-Request-Method", "GET")
	r.Header.Set("Access-Control-Request-Headers", "authorization")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if reached {
		t.Error("preflight was passed through to the handler instead of being answered")
	}
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204 to match Synapse", w.Code)
	}
	// These are the four headers Synapse returns on a media preflight.
	want := map[string]string{
		"Access-Control-Allow-Origin":   "*",
		"Access-Control-Allow-Methods":  "GET, HEAD, POST, PUT, DELETE, OPTIONS",
		"Access-Control-Allow-Headers":  "X-Requested-With, Content-Type, Authorization, Date",
		"Access-Control-Expose-Headers": "Synapse-Trace-Id, Server",
	}
	for h, v := range want {
		if got := w.Header().Get(h); got != v {
			t.Errorf("%s = %q, want %q", h, got, v)
		}
	}
	if w.Body.Len() != 0 {
		t.Errorf("preflight returned a body of %d bytes", w.Body.Len())
	}
}

// Preflight must not swallow real requests.
func TestNonPreflightPassesThrough(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		reached := false
		h := withCORSPreflight(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			reached = true
			w.WriteHeader(http.StatusOK)
		}))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, "/_matrix/client/v1/media/download/e/a", nil))
		if !reached {
			t.Errorf("%s was intercepted as a preflight", method)
		}
	}
}

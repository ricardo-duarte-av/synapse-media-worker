package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func TestClientIPPrefersForwardedFor(t *testing.T) {
	cases := []struct{ xff, remote, want string }{
		// nginx appends, so the leftmost entry is the original client.
		{"198.51.100.7, 10.0.0.1", "127.0.0.1:5555", "198.51.100.7"},
		{"203.0.113.9", "127.0.0.1:5555", "203.0.113.9"},
		{"  203.0.113.9  ", "127.0.0.1:5555", "203.0.113.9"},
		// No header: fall back to the peer, without the port.
		{"", "192.0.2.4:41234", "192.0.2.4"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/x", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := clientIP(r); got != c.want {
			t.Errorf("xff=%q remote=%q: got %q, want %q", c.xff, c.remote, got, c.want)
		}
	}
}

// The log line must carry what the handler decided, which is the part a
// reverse proxy's own access log cannot see.
func TestRequestLogRecordsOutcome(t *testing.T) {
	var buf strings.Builder
	log := zerolog.New(&buf)

	h := withRequestLog(log, true, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setMedia(r.Context(), "example.com", "abc123")
		setOutcome(r.Context(), outcomeGenerated)
		annotate(r.Context(), func(rl *reqLog) { rl.endpoint = "client_thumbnail" })
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("body"))
	}))

	r := httptest.NewRequest("GET", "/_matrix/client/v1/media/thumbnail/example.com/abc123", nil)
	r.Header.Set("X-Forwarded-For", "198.51.100.7")
	h.ServeHTTP(httptest.NewRecorder(), r)

	var line map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &line); err != nil {
		t.Fatalf("log line is not JSON: %v (%q)", err, buf.String())
	}
	want := map[string]any{
		"outcome":  outcomeGenerated,
		"endpoint": "client_thumbnail",
		"media":    "mxc://example.com/abc123",
		"ip":       "198.51.100.7",
		"status":   float64(http.StatusOK),
		"bytes":    float64(4),
	}
	for k, v := range want {
		if line[k] != v {
			t.Errorf("%s = %v, want %v", k, line[k], v)
		}
	}
}

// A proxied request must say why, so a rising fallback rate can be diagnosed
// from the log alone.
func TestRequestLogRecordsProxyReason(t *testing.T) {
	var buf strings.Builder
	h := withRequestLog(zerolog.New(&buf), true, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setProxied(r.Context(), "unsupported_format")
		w.WriteHeader(http.StatusOK)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))

	var line map[string]any
	_ = json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &line)
	if line["outcome"] != outcomeProxied || line["reason"] != "unsupported_format" {
		t.Errorf("outcome=%v reason=%v", line["outcome"], line["reason"])
	}
}

// Server errors must be logged above info so they survive a raised log level.
func TestRequestLogSeverity(t *testing.T) {
	cases := map[int]string{
		http.StatusOK:                  "info",
		http.StatusNotFound:            "info", // routine for media
		http.StatusUnauthorized:        "warn",
		http.StatusInternalServerError: "error",
	}
	for status, wantLevel := range cases {
		var buf strings.Builder
		h := withRequestLog(zerolog.New(&buf), true, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))
		var line map[string]any
		_ = json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &line)
		if line["level"] != wantLevel {
			t.Errorf("status %d logged at %v, want %v", status, line["level"], wantLevel)
		}
	}
}

func TestRequestLogCanBeDisabled(t *testing.T) {
	var buf strings.Builder
	h := withRequestLog(zerolog.New(&buf), false, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))
	if buf.Len() != 0 {
		t.Errorf("logged despite being disabled: %q", buf.String())
	}
}

// Annotating without the middleware present must not panic; handlers are
// reachable from tests that do not install it.
func TestAnnotateWithoutContextIsSafe(t *testing.T) {
	r := httptest.NewRequest("GET", "/x", nil)
	setOutcome(r.Context(), outcomeServed)
	setProxied(r.Context(), "reason")
	setMedia(r.Context(), "example.com", "abc")
}

func TestLogRequestsDefaultsOn(t *testing.T) {
	if !(LogConfig{}).LogRequests() {
		t.Error("request logging should default to on")
	}
	off := false
	if (LogConfig{Requests: &off}).LogRequests() {
		t.Error("requests: false should disable it")
	}
}

// The healthcheck polls every ten seconds and the metrics scrape as often.
// Logging them buries the requests that matter.
func TestOperationalPathsAreNotLogged(t *testing.T) {
	for _, path := range []string{"/health", "/metrics"} {
		var buf strings.Builder
		h := withRequestLog(zerolog.New(&buf), true, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
		if buf.Len() != 0 {
			t.Errorf("%s was logged: %q", path, buf.String())
		}
	}
	// A media request must still be logged.
	var buf strings.Builder
	h := withRequestLog(zerolog.New(&buf), true, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/_matrix/client/v1/media/download/e/a", nil))
	if buf.Len() == 0 {
		t.Error("a media request was not logged")
	}
}

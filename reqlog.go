package main

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// reqlog.go produces one access-log line per request.
//
// The reverse proxy in front already logs method, path and status. What it
// cannot see, and what this adds, is what the worker actually did: served a
// thumbnail Synapse had already stored, served one from its own cache,
// generated one, or handed the request back to Synapse -- and if so, why.

type ctxKey int

const reqLogKey ctxKey = iota

// Outcomes recorded on the access log line. These mirror the labels on the
// thumbnail_outcome metric so a log line and a graph can be reconciled.
const (
	outcomeServed       = "served"        // read straight from Synapse's media store
	outcomeSynapseStore = "synapse_store" // a thumbnail Synapse had already generated
	outcomeWorkerCache  = "worker_cache"  // a thumbnail this worker generated earlier
	outcomeGenerated    = "generated"     // generated during this request
	outcomeFetched      = "fetched"       // downloaded from the origin server just now
	outcomeUploaded     = "uploaded"      // accepted from a local user and stored
	outcomeReserved     = "reserved"      // an async media ID was created
	outcomeLimited      = "rate_limited"  // refused, too many pending uploads
	outcomeOverQuota    = "over_quota"    // refused, over the user's upload limit
	outcomeTooLarge     = "too_large"     // refused, over max_upload_size
	outcomeProxied      = "proxied"       // handed back to Synapse
	outcomeNotFound     = "not_found"
	outcomeUnreachable  = "unreachable" // the origin server could not be reached
	outcomeUnauthorized = "unauthorized"
	outcomeNotModified  = "not_modified"
)

// reqLog collects what a handler decided, for the access log line.
type reqLog struct {
	endpoint string
	origin   string
	mediaID  string
	outcome  string
	reason   string
	user     string
	thumb    string
}

// newRequestContext attaches a fresh annotation to the request.
func newRequestContext(r *http.Request) (*http.Request, *reqLog) {
	rl := &reqLog{}
	return r.WithContext(context.WithValue(r.Context(), reqLogKey, rl)), rl
}

// annotate runs f against the request's log annotation, if it has one. It is
// safe to call from anywhere, including code paths reached in tests without a
// logging middleware.
func annotate(ctx context.Context, f func(*reqLog)) {
	if rl, ok := ctx.Value(reqLogKey).(*reqLog); ok && rl != nil {
		f(rl)
	}
}

func setOutcome(ctx context.Context, outcome string) {
	annotate(ctx, func(rl *reqLog) { rl.outcome = outcome })
}

func setProxied(ctx context.Context, reason string) {
	annotate(ctx, func(rl *reqLog) {
		rl.outcome = outcomeProxied
		rl.reason = reason
	})
}

func setMedia(ctx context.Context, origin, mediaID string) {
	annotate(ctx, func(rl *reqLog) {
		rl.origin = origin
		rl.mediaID = mediaID
	})
}

// clientIP returns the caller's address, preferring the leftmost entry of
// X-Forwarded-For since the worker is expected to sit behind a reverse proxy.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first, _, found := strings.Cut(xff, ","); found {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(xff)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// withRequestLog logs one line per request once the response is complete.
func withRequestLog(log zerolog.Logger, enabled bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !enabled || isOperationalPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		annotated, rl := newRequestContext(r)
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, annotated)

		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}

		event := log.Info()
		// A failed request is worth surfacing even when info is filtered out.
		if status >= 500 {
			event = log.Error()
		} else if status >= 400 && status != http.StatusNotFound {
			event = log.Warn()
		}

		event = event.
			Str("method", r.Method).
			Str("path", r.URL.Path).
			Int("status", status).
			Int64("bytes", rec.bytes).
			Dur("duration", time.Since(start)).
			Str("ip", clientIP(r))

		if rl.endpoint != "" {
			event = event.Str("endpoint", rl.endpoint)
		}
		if rl.mediaID != "" {
			event = event.Str("media", mxcLabel(rl.origin, rl.mediaID))
		}
		if rl.thumb != "" {
			event = event.Str("thumb", rl.thumb)
		}
		if rl.outcome != "" {
			event = event.Str("outcome", rl.outcome)
		}
		if rl.reason != "" {
			event = event.Str("reason", rl.reason)
		}
		if rl.user != "" {
			event = event.Str("user", rl.user)
		}
		if ua := r.Header.Get("User-Agent"); ua != "" {
			event = event.Str("ua", truncate(ua, 80))
		}
		event.Msg("Request")
	})
}

// isOperationalPath reports whether a path is infrastructure rather than a
// media request. The compose healthcheck polls /health every ten seconds and a
// scrape hits /metrics as often; logging those buries the requests that carry
// something worth reading.
func isOperationalPath(path string) bool {
	return path == "/health" || path == "/metrics"
}

// mxcLabel renders the media as an mxc URI so log lines can be grepped with
// the same identifier that appears in events.
func mxcLabel(origin, mediaID string) string {
	if origin == "" {
		return mediaID
	}
	return "mxc://" + origin + "/" + mediaID
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

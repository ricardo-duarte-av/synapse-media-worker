package main

import (
	"net/http"
	"regexp"
)

// passthrough.go forwards requests the worker does not implement to Synapse.
//
// The worker owns a handful of endpoints and answers those itself. Everything
// else on the media surface -- preview_url, the admin APIs, and anything a
// future Synapse adds -- is passed through, so a deployment can
// route the whole media surface here without needing to enumerate which parts
// this worker happens to understand.
//
// The scope is deliberately not "everything". A blanket catch-all would turn
// the worker into a general reverse proxy for whatever reached it, so the
// prefixes below are exactly the ones Synapse's own docs list for
// synapse.app.media_repository, and the admin APIs are matched rather than
// waved through by prefix.

// mediaPrefixes are wholly owned by a media worker: anything beneath them is
// media by definition and is safe to pass through unexamined.
var mediaPrefixes = []string{
	"/_matrix/media/",
	"/_matrix/client/v1/media/",
}

// mediaAdminPatterns are the admin APIs docs/workers.md assigns to a media
// worker. They live under prefixes that also carry unrelated admin APIs, so
// they are matched exactly rather than proxied by prefix -- otherwise routing
// this worker any admin path would make it a general admin proxy.
var mediaAdminPatterns = []*regexp.Regexp{
	regexp.MustCompile(`^/_synapse/admin/v1/purge_media_cache$`),
	regexp.MustCompile(`^/_synapse/admin/v1/room/.*/media.*$`),
	regexp.MustCompile(`^/_synapse/admin/v1/user/.*/media.*$`),
	regexp.MustCompile(`^/_synapse/admin/v1/media/.*$`),
	regexp.MustCompile(`^/_synapse/admin/v1/quarantine_media/.*$`),
	regexp.MustCompile(`^/_synapse/admin/v1/users/.*/media$`),
}

// isMediaAdminPath reports whether a path is one of the media admin APIs.
func isMediaAdminPath(path string) bool {
	for _, re := range mediaAdminPatterns {
		if re.MatchString(path) {
			return true
		}
	}
	return false
}

// handlePassthrough forwards a request the worker does not implement.
func (s *Server) handlePassthrough(w http.ResponseWriter, r *http.Request) {
	s.passthroughFor(w, r, "not_implemented")
}

// passthroughFor forwards a request to Synapse, recording why. Most callers
// are simply endpoints the worker does not implement; an endpoint it usually
// answers can also defer, when something about this deployment means only
// Synapse can produce the right response.
func (s *Server) passthroughFor(w http.ResponseWriter, r *http.Request, reason string) {
	if s.passthroughUp == nil {
		// Nothing to forward to. Answering 404 is honest: this worker does not
		// serve the endpoint and cannot say who does.
		respondNotFound(w, r.URL.Path)
		return
	}
	setOutcome(r.Context(), outcomeProxied)
	annotate(r.Context(), func(rl *reqLog) { rl.reason = reason })
	proxiedTotal.WithLabelValues("passthrough", reason).Inc()
	s.passthroughUp.ServeHTTP(w, r)
}

// handleAdminPassthrough forwards the media admin APIs, and refuses anything
// else under the same prefixes.
//
// Quarantine in particular must reach an instance configured as a
// quarantined_media_changes writer, so the upstream this points at is not
// interchangeable with any media worker.
func (s *Server) handleAdminPassthrough(w http.ResponseWriter, r *http.Request) {
	if !isMediaAdminPath(r.URL.Path) {
		// Counted by instrument() as an admin_passthrough 404 rather than
		// here: nothing was proxied, which is the whole point.
		annotate(r.Context(), func(rl *reqLog) { rl.reason = "not_media_admin" })
		respondNotFound(w, r.URL.Path)
		return
	}
	s.handlePassthrough(w, r)
}

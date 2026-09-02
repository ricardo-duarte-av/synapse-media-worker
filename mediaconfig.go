package main

import (
	"net/http"
	"strconv"
)

// mediaconfig.go answers the media config endpoints, which report the upload
// size limit and nothing else.
//
// Synapse builds the body out of one number the worker already knows:
// max_upload_size, read from homeserver.yaml by deriveFromSynapse and used
// here for the same limit uploads are checked against. Proxying a constant is
// a round trip to Synapse for a value we hold, on an endpoint every client
// calls at startup.
//
// Both spellings are the same servlet upstream:
// synapse/rest/client/media.py:MediaConfigResource and
// synapse/rest/media/config_resource.py:MediaConfigResource are byte for byte
// the same handler under two path families. Both authenticate, including the
// one under /_matrix/media, which is otherwise the unauthenticated family.

// mediaConfigCacheControl is the default Synapse's respond_with_json inserts
// when a servlet has set no Cache-Control of its own.
const mediaConfigCacheControl = "no-cache, no-store, must-revalidate"

// mediaConfigBody renders the body byte for byte as Synapse sends it.
// respond_with_json encodes with encode_canonical_json, which emits no spaces
// and no trailing newline.
func mediaConfigBody(maxUploadSize int64) []byte {
	return []byte(`{"m.upload.size":` + strconv.FormatInt(maxUploadSize, 10) + `}`)
}

// handleMediaConfig serves GET /_matrix/client/v1/media/config.
func (s *Server) handleMediaConfig(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Media.ProxyMediaConfig() {
		// A module may replace this response per user, and the worker cannot
		// run Synapse's modules. Deferring is the only correct answer.
		s.passthroughFor(w, r, "module_config")
		return
	}
	if !s.authenticateClient(w, r) {
		return
	}
	body := mediaConfigBody(s.cfg.Media.MaxUploadSizeOrDefault())

	setOutcome(r.Context(), outcomeServed)
	setCORSHeaders(w)
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", mediaConfigCacheControl)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// handleLegacyMediaConfig serves GET /_matrix/media/{r0,v1,v3}/config.
//
// Unlike the other endpoints in that family this one is not weakened by
// authenticated media: it authenticates the caller exactly as the /client/v1
// spelling does, and reports the same limit.
func (s *Server) handleLegacyMediaConfig(w http.ResponseWriter, r *http.Request) {
	if !validLegacyVersion(r.PathValue("version")) {
		respondNotFound(w, r.URL.Path)
		return
	}
	s.handleMediaConfig(w, r)
}

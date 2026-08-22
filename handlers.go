package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/sync/singleflight"
)

// handlers.go implements the read endpoints the worker takes over from
// Synapse's media workers.
//
// Everything the worker cannot answer confidently is proxied back to Synapse
// rather than approximated, so a fallback is always a correct response.

// Server holds everything the handlers need.
type Server struct {
	cfg         *Config
	db          *DB
	paths       *MediaPaths
	cache       *ThumbnailCache
	thumbnailer *Thumbnailer
	auth        *TokenAuthenticator
	downloadUp  *Proxy
	thumbnailUp *Proxy
	log         zerolog.Logger

	// generating collapses concurrent generation of the same thumbnail, so a
	// popular image is decoded once rather than once per waiting request.
	generating singleflight.Group
}

// isMine reports whether a server name refers to this homeserver.
func (s *Server) isMine(serverName string) bool {
	return serverName == s.cfg.ServerName
}

// --- client download -------------------------------------------------------

// handleClientDownload serves GET /_matrix/client/v1/media/download/...
func (s *Server) handleClientDownload(w http.ResponseWriter, r *http.Request) {
	if !s.authenticateClient(w, r) {
		return
	}
	s.serveDownload(w, r, r.PathValue("serverName"), r.PathValue("mediaId"), true)
}

// handleLegacyDownload serves the unauthenticated /_matrix/media/{r0,v1,v3}
// download endpoints. Media stored since authenticated media was enabled is
// hidden from these.
func (s *Server) handleLegacyDownload(w http.ResponseWriter, r *http.Request) {
	if !validLegacyVersion(r.PathValue("version")) {
		respondNotFound(w, r.URL.Path)
		return
	}
	s.serveDownload(w, r, r.PathValue("serverName"), r.PathValue("mediaId"), false)
}

func validLegacyVersion(v string) bool {
	return v == "r0" || v == "v1" || v == "v3"
}

// serveDownload handles both the authenticated and legacy download endpoints.
func (s *Server) serveDownload(w http.ResponseWriter, r *http.Request, serverName, mediaID string, allowAuthenticated bool) {
	setMedia(r.Context(), serverName, mediaID)
	if s.isMine(serverName) {
		s.serveLocalDownload(w, r, mediaID, allowAuthenticated, false)
		return
	}
	s.serveRemoteDownload(w, r, serverName, mediaID, allowAuthenticated)
}

// serveLocalDownload serves media this homeserver owns.
func (s *Server) serveLocalDownload(w http.ResponseWriter, r *http.Request, mediaID string, allowAuthenticated, federation bool) {
	media, ok := s.resolveLocalMedia(w, r, mediaID, allowAuthenticated)
	if !ok {
		return
	}

	path, err := s.localMediaPath(media)
	if err != nil {
		s.log.Warn().Err(err).Str("media_id", mediaID).Msg("Could not build media path")
		setOutcome(r.Context(), outcomeNotFound)
		respondNotFound(w, r.URL.Path)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		// The row exists but the file does not. Synapse has no storage
		// providers configured here, so there is nowhere else to look.
		s.log.Warn().Str("media_id", mediaID).Str("path", path).
			Msg("Media row exists but file is missing")
		setOutcome(r.Context(), outcomeNotFound)
		respondNotFound(w, r.URL.Path)
		return
	}
	defer func() { _ = f.Close() }()

	s.db.MarkRecentlyAccessedLocal(mediaID)
	setOutcome(r.Context(), outcomeServed)

	if federation {
		// Synapse takes no filename from the request path here, but it does
		// send the stored upload_name, so the disposition matches the
		// non-federation response.
		s.respondMultipart(w, r, f, media.MediaType, media.UploadName, media.Length)
		return
	}

	setCORSHeaders(w)
	setCORPHeaders(w)
	setDownloadSecurityHeaders(w)
	if notModified(r) {
		setOutcome(r.Context(), outcomeNotModified)
		respondNotModified(w)
		return
	}
	addFileHeaders(w, media.MediaType, media.UploadName)
	serveFile(w, r, f)
}

// serveRemoteDownload serves another server's media out of Synapse's cache.
// A cache miss is proxied so Synapse can perform the federated fetch, along
// with the rate limiting and spam checks that go with it.
func (s *Server) serveRemoteDownload(w http.ResponseWriter, r *http.Request, serverName, mediaID string, allowAuthenticated bool) {
	media, err := s.db.GetRemoteMedia(r.Context(), serverName, mediaID)
	if errors.Is(err, ErrNotFound) {
		s.proxyDownload(w, r, "remote_not_cached")
		return
	} else if err != nil {
		s.internalError(w, r, err, "querying remote media")
		return
	}
	if media.Quarantined() {
		respondNotFound(w, r.URL.Path)
		return
	}
	if s.hiddenFromLegacy(media.Authenticated, allowAuthenticated) {
		respondNotFound(w, r.URL.Path)
		return
	}
	if media.Length == nil {
		s.proxyDownload(w, r, "remote_incomplete")
		return
	}

	path, err := s.paths.RemoteMedia(serverName, media.FilesystemID)
	if err != nil {
		s.log.Warn().Err(err).Str("origin", serverName).Str("media_id", mediaID).
			Msg("Could not build remote media path")
		respondNotFound(w, r.URL.Path)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		// Cached according to the database but absent on disk. Synapse can
		// re-fetch it; the worker cannot.
		s.proxyDownload(w, r, "remote_file_missing")
		return
	}
	defer func() { _ = f.Close() }()

	s.db.MarkRecentlyAccessedRemote(serverName, mediaID)
	setOutcome(r.Context(), outcomeServed)

	setCORSHeaders(w)
	setCORPHeaders(w)
	setDownloadSecurityHeaders(w)
	if notModified(r) {
		setOutcome(r.Context(), outcomeNotModified)
		respondNotModified(w)
		return
	}
	addFileHeaders(w, media.MediaType, media.UploadName)
	serveFile(w, r, f)
}

// --- federation download ---------------------------------------------------

// handleFederationDownload serves GET /_matrix/federation/v1/media/download/...
// It is reached only after the X-Matrix signature has been verified.
//
// Federation downloads are for local media only; a media ID here is never
// looked up against the remote cache.
func (s *Server) handleFederationDownload(w http.ResponseWriter, r *http.Request) {
	setMedia(r.Context(), s.cfg.ServerName, r.PathValue("mediaId"))
	s.serveLocalDownload(w, r, r.PathValue("mediaId"), true, true)
}

// respondMultipart writes the multipart/mixed body federation downloads use.
func (s *Server) respondMultipart(w http.ResponseWriter, r *http.Request, body *os.File, mediaType, uploadName string, length *int64) {
	parts := newMultipartParts(newBoundary(), mediaType, uploadName)
	size := int64(-1)
	if length != nil {
		size = *length
	}
	// Range requests have no meaning for a multipart envelope.
	r.Header.Del("Range")
	parts.writeHeader(w, size)
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if err := parts.writeBody(w, body); err != nil {
		s.log.Debug().Err(err).Msg("Federation multipart body write interrupted")
	}
}

// --- thumbnails ------------------------------------------------------------

// handleClientThumbnail serves GET /_matrix/client/v1/media/thumbnail/...
func (s *Server) handleClientThumbnail(w http.ResponseWriter, r *http.Request) {
	if !s.authenticateClient(w, r) {
		return
	}
	s.serveThumbnail(w, r, r.PathValue("serverName"), r.PathValue("mediaId"), true, false)
}

// handleLegacyThumbnail serves the unauthenticated thumbnail endpoints.
func (s *Server) handleLegacyThumbnail(w http.ResponseWriter, r *http.Request) {
	if !validLegacyVersion(r.PathValue("version")) {
		respondNotFound(w, r.URL.Path)
		return
	}
	s.serveThumbnail(w, r, r.PathValue("serverName"), r.PathValue("mediaId"), false, false)
}

// handleFederationThumbnail serves GET /_matrix/federation/v1/media/thumbnail/...
func (s *Server) handleFederationThumbnail(w http.ResponseWriter, r *http.Request) {
	s.serveThumbnail(w, r, s.cfg.ServerName, r.PathValue("mediaId"), true, true)
}

func (s *Server) serveThumbnail(w http.ResponseWriter, r *http.Request, serverName, mediaID string, allowAuthenticated, federation bool) {
	setMedia(r.Context(), serverName, mediaID)
	req, err := ParseThumbnailRequest(r)
	if err != nil {
		writeMatrixError(w, http.StatusBadRequest, "M_INVALID_PARAM", err.Error())
		return
	}

	annotate(r.Context(), func(rl *reqLog) {
		rl.thumb = fmt.Sprintf("%dx%d/%s", req.Width, req.Height, req.Method)
	})

	if s.isMine(serverName) {
		s.serveLocalThumbnail(w, r, mediaID, req, allowAuthenticated, federation)
		return
	}
	s.serveRemoteThumbnail(w, r, serverName, mediaID, req, allowAuthenticated)
}

func (s *Server) serveLocalThumbnail(w http.ResponseWriter, r *http.Request, mediaID string, req ThumbnailRequest, allowAuthenticated, federation bool) {
	media, ok := s.resolveLocalMedia(w, r, mediaID, allowAuthenticated)
	if !ok {
		return
	}

	// 1. A thumbnail Synapse already stored, matching exactly.
	rows, err := s.db.GetLocalThumbnails(r.Context(), mediaID)
	if err != nil {
		s.internalError(w, r, err, "querying local thumbnails")
		return
	}
	if row, found := FindExact(rows, req); found {
		var path string
		var perr error
		if media.IsURLCache() {
			path, perr = s.paths.URLCacheThumbnail(mediaID, row.Width, row.Height, row.Type, row.Method)
		} else {
			path, perr = s.paths.LocalThumbnail(mediaID, row.Width, row.Height, row.Type, row.Method)
		}
		if perr == nil {
			if f, err := os.Open(path); err == nil {
				defer func() { _ = f.Close() }()
				s.db.MarkRecentlyAccessedLocal(mediaID)
				thumbnailOutcome.WithLabelValues(outcomeSynapseStore).Inc()
				setOutcome(r.Context(), outcomeSynapseStore)
				s.respondThumbnailFile(w, r, f, row.Type, federation)
				return
			}
		}
	}

	// 2. Anything this worker generated earlier.
	srcPath, err := s.localMediaPath(media)
	if err != nil {
		respondNotFound(w, r.URL.Path)
		return
	}
	s.serveGeneratedThumbnail(w, r, "", mediaID, srcPath, media.MediaType, req, federation)
	s.db.MarkRecentlyAccessedLocal(mediaID)
}

func (s *Server) serveRemoteThumbnail(w http.ResponseWriter, r *http.Request, serverName, mediaID string, req ThumbnailRequest, allowAuthenticated bool) {
	media, err := s.db.GetRemoteMedia(r.Context(), serverName, mediaID)
	if errors.Is(err, ErrNotFound) {
		s.proxyThumbnail(w, r, "remote_not_cached")
		return
	} else if err != nil {
		s.internalError(w, r, err, "querying remote media")
		return
	}
	if media.Quarantined() {
		respondNotFound(w, r.URL.Path)
		return
	}
	if s.hiddenFromLegacy(media.Authenticated, allowAuthenticated) {
		respondNotFound(w, r.URL.Path)
		return
	}

	rows, err := s.db.GetRemoteThumbnails(r.Context(), serverName, mediaID)
	if err != nil {
		s.internalError(w, r, err, "querying remote thumbnails")
		return
	}
	if row, found := FindExact(rows, req); found {
		fsID := row.FilesystemID
		if fsID == "" {
			fsID = media.FilesystemID
		}
		// Older Synapse versions wrote the filename without the method, so
		// both spellings have to be probed before declaring a miss.
		current, err1 := s.paths.RemoteThumbnail(serverName, fsID, row.Width, row.Height, row.Type, row.Method)
		legacy, err2 := s.paths.RemoteThumbnailLegacy(serverName, fsID, row.Width, row.Height, row.Type)
		for _, candidate := range []struct {
			path string
			err  error
		}{{current, err1}, {legacy, err2}} {
			if candidate.err != nil {
				continue
			}
			if f, err := os.Open(candidate.path); err == nil {
				defer func() { _ = f.Close() }()
				s.db.MarkRecentlyAccessedRemote(serverName, mediaID)
				thumbnailOutcome.WithLabelValues(outcomeSynapseStore).Inc()
				setOutcome(r.Context(), outcomeSynapseStore)
				s.respondThumbnailFile(w, r, f, row.Type, false)
				return
			}
		}
	}

	srcPath, err := s.paths.RemoteMedia(serverName, media.FilesystemID)
	if err != nil {
		s.proxyThumbnail(w, r, "remote_path_invalid")
		return
	}
	s.serveGeneratedThumbnail(w, r, serverName, mediaID, srcPath, media.MediaType, req, false)
	s.db.MarkRecentlyAccessedRemote(serverName, mediaID)
}

// serveGeneratedThumbnail serves from the worker's own cache, generating the
// thumbnail first if necessary, and proxies to Synapse when it cannot.
func (s *Server) serveGeneratedThumbnail(w http.ResponseWriter, r *http.Request, origin, mediaID, srcPath, sourceType string, req ThumbnailRequest, federation bool) {
	key := cacheKey(origin, mediaID, req)

	if f, ok := s.cache.Open(key); ok {
		defer func() { _ = f.Close() }()
		thumbnailOutcome.WithLabelValues(outcomeWorkerCache).Inc()
		setOutcome(r.Context(), outcomeWorkerCache)
		s.respondThumbnailFile(w, r, f, req.Type, federation)
		return
	}

	if !s.thumbnailer.CanGenerate(sourceType, req) {
		reason := "unsupported_format"
		if req.Animated {
			reason = "animated"
		}
		s.proxyThumbnail(w, r, reason)
		return
	}

	data, err, _ := s.generating.Do(key, func() (any, error) {
		// Another request may have finished generating while this one waited.
		if f, ok := s.cache.Open(key); ok {
			defer func() { _ = f.Close() }()
			return readAllFile(f)
		}
		start := time.Now()
		out, err := s.thumbnailer.Generate(srcPath, sourceType, req)
		if err != nil {
			return nil, err
		}
		thumbnailGenerateDuration.Observe(time.Since(start).Seconds())
		if err := s.cache.Put(key, out); err != nil {
			// Being unable to cache is not a reason to fail the request.
			s.log.Warn().Err(err).Str("media_id", mediaID).
				Msg("Could not store generated thumbnail")
		}
		return out, nil
	})
	if err != nil {
		if errors.Is(err, ErrCannotGenerate) {
			s.proxyThumbnail(w, r, "generate_refused")
			return
		}
		if errors.Is(err, os.ErrNotExist) {
			// The original is not on disk; only Synapse can recover it.
			s.proxyThumbnail(w, r, "source_missing")
			return
		}
		s.log.Warn().Err(err).Str("media_id", mediaID).Msg("Thumbnail generation failed")
		s.proxyThumbnail(w, r, "generate_failed")
		return
	}

	thumbnailOutcome.WithLabelValues(outcomeGenerated).Inc()
	setOutcome(r.Context(), outcomeGenerated)
	s.respondThumbnailBytes(w, r, data.([]byte), req.Type, federation)
}

// respondThumbnailFile writes a thumbnail that is backed by a file.
func (s *Server) respondThumbnailFile(w http.ResponseWriter, r *http.Request, f *os.File, mediaType string, federation bool) {
	if federation {
		s.respondMultipart(w, r, f, mediaType, "", fileSize(f))
		return
	}
	setCORSHeaders(w)
	setCORPHeaders(w)
	if notModified(r) {
		setOutcome(r.Context(), outcomeNotModified)
		respondNotModified(w)
		return
	}
	// Thumbnails carry no upload name, and Synapse sets no CSP on this path.
	addFileHeaders(w, mediaType, "")
	serveFile(w, r, f)
}

// respondThumbnailBytes writes a thumbnail held in memory.
func (s *Server) respondThumbnailBytes(w http.ResponseWriter, r *http.Request, data []byte, mediaType string, federation bool) {
	if federation {
		parts := newMultipartParts(newBoundary(), mediaType, "")
		r.Header.Del("Range")
		parts.writeHeader(w, int64(len(data)))
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			if err := parts.writeBody(w, bytes.NewReader(data)); err != nil {
				s.log.Debug().Err(err).Msg("Federation multipart body write interrupted")
			}
		}
		return
	}
	setCORSHeaders(w)
	setCORPHeaders(w)
	if notModified(r) {
		setOutcome(r.Context(), outcomeNotModified)
		respondNotModified(w)
		return
	}
	addFileHeaders(w, mediaType, "")
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
}

// --- shared helpers --------------------------------------------------------

// authenticateClient validates the caller's access token, writing an error
// response and returning false when it is not usable.
func (s *Server) authenticateClient(w http.ResponseWriter, r *http.Request) bool {
	token := ExtractToken(r)
	if token == "" {
		authOutcome.WithLabelValues("missing").Inc()
		setOutcome(r.Context(), outcomeUnauthorized)
		writeMatrixError(w, http.StatusUnauthorized, "M_MISSING_TOKEN",
			"Missing access token")
		return false
	}
	verdict, err := s.auth.Authenticate(r.Context(), token)
	if err != nil {
		// Synapse could not be asked. This is not the caller's fault, so it
		// must not be reported as an invalid token.
		authOutcome.WithLabelValues("error").Inc()
		s.log.Warn().Err(err).Msg("Could not validate access token")
		writeMatrixError(w, http.StatusServiceUnavailable, "M_UNKNOWN",
			"Could not validate access token")
		return false
	}
	if !verdict.valid {
		authOutcome.WithLabelValues("rejected").Inc()
		setOutcome(r.Context(), outcomeUnauthorized)
		writeMatrixError(w, http.StatusUnauthorized, "M_UNKNOWN_TOKEN",
			"Invalid access token passed.")
		return false
	}
	authOutcome.WithLabelValues("accepted").Inc()
	tokenCacheSize.Set(float64(s.auth.Len()))
	annotate(r.Context(), func(rl *reqLog) { rl.user = verdict.userID })
	return true
}

// hiddenFromLegacy reports whether authenticated media must be hidden from an
// unauthenticated endpoint.
func (s *Server) hiddenFromLegacy(mediaAuthenticated, allowAuthenticated bool) bool {
	return s.cfg.Media.EnableAuthenticatedMedia && !allowAuthenticated && mediaAuthenticated
}

// resolveLocalMedia loads a local media row and applies the quarantine,
// authentication and pending-upload gates. It writes the error response itself
// and returns false when the request has been answered.
func (s *Server) resolveLocalMedia(w http.ResponseWriter, r *http.Request, mediaID string, allowAuthenticated bool) (*LocalMedia, bool) {
	media, err := s.db.GetLocalMedia(r.Context(), mediaID)
	if errors.Is(err, ErrNotFound) {
		respondNotFound(w, r.URL.Path)
		return nil, false
	} else if err != nil {
		s.internalError(w, r, err, "querying local media")
		return nil, false
	}
	if media.Quarantined() {
		annotate(r.Context(), func(rl *reqLog) {
			rl.outcome, rl.reason = outcomeNotFound, "quarantined"
		})
		respondNotFound(w, r.URL.Path)
		return nil, false
	}
	if s.hiddenFromLegacy(media.Authenticated, allowAuthenticated) {
		respondNotFound(w, r.URL.Path)
		return nil, false
	}
	if media.Length == nil {
		media, err = s.awaitUpload(r, mediaID)
		if err != nil {
			respondNotYetUploaded(w)
			return nil, false
		}
	}
	return media, true
}

// awaitUpload polls for an async upload to complete, as Synapse does.
func (s *Server) awaitUpload(r *http.Request, mediaID string) (*LocalMedia, error) {
	timeout := s.uploadTimeout(r)
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return nil, r.Context().Err()
		case <-ticker.C:
			media, err := s.db.GetLocalMedia(r.Context(), mediaID)
			if err != nil {
				return nil, err
			}
			if media.Length != nil {
				return media, nil
			}
			if time.Now().After(deadline) {
				return nil, context.DeadlineExceeded
			}
		}
	}
}

// uploadTimeout reads ?timeout_ms, applying Synapse's default and cap.
func (s *Server) uploadTimeout(r *http.Request) time.Duration {
	timeout := s.cfg.Media.DefaultTimeout
	if v := r.URL.Query().Get("timeout_ms"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms >= 0 {
			timeout = time.Duration(ms) * time.Millisecond
		}
	}
	return min(timeout, s.cfg.Media.MaxTimeout)
}

// localMediaPath picks between local_content and url_cache.
func (s *Server) localMediaPath(media *LocalMedia) (string, error) {
	if media.IsURLCache() {
		return s.paths.URLCache(media.MediaID)
	}
	return s.paths.LocalMedia(media.MediaID)
}

func (s *Server) proxyDownload(w http.ResponseWriter, r *http.Request, reason string) {
	proxiedTotal.WithLabelValues("download", reason).Inc()
	setProxied(r.Context(), reason)
	if s.downloadUp == nil {
		respondNotFound(w, r.URL.Path)
		return
	}
	s.downloadUp.ServeHTTP(w, r)
}

func (s *Server) proxyThumbnail(w http.ResponseWriter, r *http.Request, reason string) {
	proxiedTotal.WithLabelValues("thumbnail", reason).Inc()
	thumbnailOutcome.WithLabelValues(outcomeProxied).Inc()
	setProxied(r.Context(), reason)
	if s.thumbnailUp == nil {
		respondNotFound(w, r.URL.Path)
		return
	}
	s.thumbnailUp.ServeHTTP(w, r)
}

func (s *Server) internalError(w http.ResponseWriter, r *http.Request, err error, what string) {
	s.log.Error().Err(err).Str("path", r.URL.Path).Msg("Failed " + what)
	writeMatrixError(w, http.StatusInternalServerError, "M_UNKNOWN", "Internal server error")
}

func fileSize(f *os.File) *int64 {
	info, err := f.Stat()
	if err != nil {
		return nil
	}
	size := info.Size()
	return &size
}

func readAllFile(f *os.File) ([]byte, error) {
	return io.ReadAll(f)
}

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// upload.go accepts media from local users, the one path where the worker takes
// data rather than serving it.
//
// The shape follows Synapse's create_or_update_content: bytes to disk first,
// then the content hash, then the quarantine check, then the row. Nothing is
// deferred past the response.
//
// Thumbnails are deliberately not generated here by default. Under
// dynamic_thumbnails, Synapse's upload-time set is written in a type no request
// path can ask for, and most uploads are never viewed at thumbnail size at all,
// so generating them costs five decodes to produce files that are never served.

// quarantinedBySystem is the literal Synapse writes when content matches an
// already-quarantined hash.
const quarantinedBySystem = "system"

// Upload endpoints and outcomes, kept as separate metric dimensions.
const (
	uploadEndpointSync   = "sync"
	uploadEndpointCreate = "create"
	uploadEndpointAsync  = "async"

	uploadResultStored    = "stored"
	uploadResultReserved  = "reserved"
	uploadResultTooLarge  = "too_large"
	uploadResultLimited   = "limited"
	uploadResultForbidden = "forbidden"
	uploadResultNotFound  = "not_found"
	uploadResultConflict  = "conflict"
	uploadResultFailed    = "failed"
	uploadResultProxied   = "proxied"
)

// uploadResponse is the body both upload endpoints return.
type uploadResponse struct {
	ContentURI string `json:"content_uri"`
}

// createResponse is the body of POST /_matrix/media/v1/create.
type createResponse struct {
	ContentURI      string `json:"content_uri"`
	UnusedExpiresAt int64  `json:"unused_expires_at"`
}

// Uploader writes media uploaded by local users.
type Uploader struct {
	db          *DB
	paths       *MediaPaths
	thumbnailer *Thumbnailer
	cfg         *Config

	// slots bound simultaneous uploads, each of which holds an open file and
	// streams a body that may be very large.
	slots chan struct{}
}

func NewUploader(db *DB, paths *MediaPaths, thumbnailer *Thumbnailer, cfg *Config) *Uploader {
	n := cfg.Media.MaxConcurrentUploads
	if n <= 0 {
		n = 8
	}
	return &Uploader{
		db: db, paths: paths, thumbnailer: thumbnailer, cfg: cfg,
		slots: make(chan struct{}, n),
	}
}

// --- POST /_matrix/media/{r0,v1,v3}/upload ---------------------------------

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if !validLegacyVersion(r.PathValue("version")) {
		respondNotFound(w, r.URL.Path)
		return
	}
	if s.uploader == nil {
		s.proxyUpload(w, r, "uploads_disabled")
		return
	}
	user, ok := s.authenticateUploader(w, r)
	if !ok {
		return
	}

	mediaType, uploadName, length, ok := s.uploadMetadata(w, r)
	if !ok {
		return
	}

	mediaID := newFilesystemID() // Synapse uses the same random_string(24)
	stored, err := s.uploader.store(r, mediaID, mediaType, length)
	if err != nil {
		s.uploadFailed(w, r, uploadEndpointSync, err, "storing upload")
		return
	}

	quarantinedBy, err := s.uploader.quarantineFor(r.Context(), stored.sha256)
	if err != nil {
		_ = os.Remove(stored.path)
		s.uploadFailed(w, r, uploadEndpointSync, err, "checking quarantined hashes")
		return
	}

	now := time.Now().UnixMilli()
	media := &LocalMedia{
		MediaID: mediaID, MediaType: mediaType, Length: &stored.length,
		UploadName: uploadName, CreatedTS: now, UserID: user,
	}
	if err := s.db.StoreLocalMedia(r.Context(), media, stored.sha256,
		s.cfg.Media.AuthenticatedMedia(), quarantinedBy); err != nil {
		_ = os.Remove(stored.path)
		s.uploadFailed(w, r, uploadEndpointSync, err, "recording upload")
		return
	}

	s.uploader.maybeThumbnail(r, mediaID, stored.path, mediaType)
	s.respondUploaded(w, r, uploadEndpointSync, mediaID, user, stored.length, quarantinedBy)
}

// --- POST /_matrix/media/v1/create -----------------------------------------

func (s *Server) handleCreateMedia(w http.ResponseWriter, r *http.Request) {
	if s.uploader == nil {
		s.proxyUpload(w, r, "uploads_disabled")
		return
	}
	user, ok := s.authenticateUploader(w, r)
	if !ok {
		return
	}

	// The spec requires 429 M_LIMIT_EXCEEDED once a user holds too many
	// un-uploaded media IDs.
	notBefore := time.Now().Add(-s.cfg.Media.UnusedExpirationTime).UnixMilli()
	pending, oldest, err := s.db.CountPendingMedia(r.Context(), user, notBefore)
	if err != nil {
		s.internalError(w, r, err, "counting pending media")
		return
	}
	if s.cfg.Media.MaxPendingMediaUploads > 0 && pending >= s.cfg.Media.MaxPendingMediaUploads {
		retryAfter := oldest + s.cfg.Media.UnusedExpirationTime.Milliseconds() - time.Now().UnixMilli()
		if retryAfter < 0 {
			retryAfter = 0
		}
		setOutcome(r.Context(), outcomeLimited)
		uploadsTotal.WithLabelValues(uploadEndpointCreate, uploadResultLimited).Inc()
		writeRateLimited(w, retryAfter)
		return
	}

	mediaID := newFilesystemID()
	now := time.Now().UnixMilli()
	if err := s.db.StoreLocalMediaID(r.Context(), mediaID, now, user,
		s.cfg.Media.AuthenticatedMedia()); err != nil {
		s.internalError(w, r, err, "reserving media id")
		return
	}

	setMedia(r.Context(), "", mediaID)
	setOutcome(r.Context(), outcomeReserved)
	uploadsTotal.WithLabelValues(uploadEndpointCreate, uploadResultReserved).Inc()
	pendingMediaReserved.Inc()
	writeJSON(w, http.StatusOK, createResponse{
		ContentURI:      "mxc://" + s.cfg.ServerName + "/" + mediaID,
		UnusedExpiresAt: now + s.cfg.Media.UnusedExpirationTime.Milliseconds(),
	})
}

// --- PUT /_matrix/media/v3/upload/{serverName}/{mediaId} -------------------

func (s *Server) handleAsyncUpload(w http.ResponseWriter, r *http.Request) {
	if !validLegacyVersion(r.PathValue("version")) {
		respondNotFound(w, r.URL.Path)
		return
	}
	if s.uploader == nil {
		s.proxyUpload(w, r, "uploads_disabled")
		return
	}
	serverName := r.PathValue("serverName")
	mediaID := r.PathValue("mediaId")
	setMedia(r.Context(), "", mediaID)

	if !s.isMine(serverName) {
		uploadsTotal.WithLabelValues(uploadEndpointAsync, uploadResultNotFound).Inc()
		writeMatrixError(w, http.StatusNotFound, "M_NOT_FOUND", "Non-local server name specified")
		return
	}
	user, ok := s.authenticateUploader(w, r)
	if !ok {
		return
	}

	// Ordering matches Synapse's verify_can_upload: unknown, then ownership,
	// then already-uploaded, then expiry. A different user PUTting to someone
	// else's completed media therefore gets 403 rather than 409.
	media, err := s.db.GetLocalMedia(r.Context(), mediaID)
	if errors.Is(err, ErrNotFound) {
		uploadsTotal.WithLabelValues(uploadEndpointAsync, uploadResultNotFound).Inc()
		writeMatrixError(w, http.StatusNotFound, "M_NOT_FOUND", "Unknown media ID")
		return
	} else if err != nil {
		s.internalError(w, r, err, "looking up media id")
		return
	}
	if media.UserID != user {
		uploadsTotal.WithLabelValues(uploadEndpointAsync, uploadResultForbidden).Inc()
		writeMatrixError(w, http.StatusForbidden, "M_FORBIDDEN",
			"Only the creator of the media ID can upload to it")
		return
	}
	if media.Length != nil {
		uploadsTotal.WithLabelValues(uploadEndpointAsync, uploadResultConflict).Inc()
		writeMatrixError(w, http.StatusConflict, "M_CANNOT_OVERWRITE_MEDIA",
			"Media ID already has content")
		return
	}
	if media.CreatedTS < time.Now().Add(-s.cfg.Media.UnusedExpirationTime).UnixMilli() {
		uploadsTotal.WithLabelValues(uploadEndpointAsync, uploadResultNotFound).Inc()
		writeMatrixError(w, http.StatusNotFound, "M_NOT_FOUND", "Media ID has expired")
		return
	}

	mediaType, uploadName, length, ok := s.uploadMetadata(w, r)
	if !ok {
		return
	}

	stored, err := s.uploader.store(r, mediaID, mediaType, length)
	if err != nil {
		s.uploadFailed(w, r, uploadEndpointAsync, err, "storing upload")
		return
	}
	quarantinedBy, err := s.uploader.quarantineFor(r.Context(), stored.sha256)
	if err != nil {
		_ = os.Remove(stored.path)
		s.uploadFailed(w, r, uploadEndpointAsync, err, "checking quarantined hashes")
		return
	}

	// Conditional on the media still being pending. Synapse needs a
	// cross-worker lock here because its UPDATE is unconditional; letting the
	// database decide is the same guarantee with nothing to keep in sync.
	won, err := s.db.CompleteLocalMedia(r.Context(), mediaID, mediaType, uploadName,
		stored.length, stored.sha256, quarantinedBy)
	if err != nil {
		_ = os.Remove(stored.path)
		s.uploadFailed(w, r, uploadEndpointAsync, err, "completing upload")
		return
	}
	if !won {
		// Someone completed it between our check and our write. Their bytes
		// are the ones the row describes, so ours must go.
		_ = os.Remove(stored.path)
		uploadsTotal.WithLabelValues(uploadEndpointAsync, uploadResultConflict).Inc()
		writeMatrixError(w, http.StatusConflict, "M_CANNOT_OVERWRITE_MEDIA",
			"Media ID already has content")
		return
	}

	s.uploader.maybeThumbnail(r, mediaID, stored.path, mediaType)
	s.respondUploaded(w, r, uploadEndpointAsync, mediaID, user, stored.length, quarantinedBy)
}

// --- shared ----------------------------------------------------------------

// authenticateUploader resolves the uploading user, honouring appservice
// masquerading.
func (s *Server) authenticateUploader(w http.ResponseWriter, r *http.Request) (string, bool) {
	creds := ExtractCredentials(r)
	if creds.Token == "" {
		authOutcome.WithLabelValues("missing").Inc()
		setOutcome(r.Context(), outcomeUnauthorized)
		writeMatrixError(w, http.StatusUnauthorized, "M_MISSING_TOKEN", "Missing access token")
		return "", false
	}
	verdict, err := s.auth.AuthenticateAs(r.Context(), creds)
	if err != nil {
		authOutcome.WithLabelValues("error").Inc()
		s.log.Warn().Err(err).Msg("Could not validate access token")
		writeMatrixError(w, http.StatusServiceUnavailable, "M_UNKNOWN",
			"Could not validate access token")
		return "", false
	}
	if !verdict.valid {
		authOutcome.WithLabelValues("rejected").Inc()
		setOutcome(r.Context(), outcomeUnauthorized)
		// A rejected masquerade is Synapse telling us this token may not act
		// as that user, which the spec expresses as 403 rather than 401.
		if creds.UserID != "" {
			writeMatrixError(w, http.StatusForbidden, "M_FORBIDDEN",
				"Application service cannot masquerade as this user")
			return "", false
		}
		writeMatrixError(w, http.StatusUnauthorized, "M_UNKNOWN_TOKEN", "Invalid access token passed.")
		return "", false
	}
	authOutcome.WithLabelValues("accepted").Inc()
	annotate(r.Context(), func(rl *reqLog) { rl.user = verdict.userID })
	return verdict.userID, true
}

// uploadMetadata reads and validates the headers Synapse requires.
func (s *Server) uploadMetadata(w http.ResponseWriter, r *http.Request) (mediaType, uploadName string, length int64, ok bool) {
	raw := r.Header.Get("Content-Length")
	if raw == "" {
		writeMatrixError(w, http.StatusBadRequest, "M_UNKNOWN", "Request must specify a Content-Length")
		return "", "", 0, false
	}
	length, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || length < 0 {
		writeMatrixError(w, http.StatusBadRequest, "M_UNKNOWN", "Content-Length value is invalid")
		return "", "", 0, false
	}
	if max := s.cfg.Media.MaxUploadSizeOrDefault(); length > max {
		setOutcome(r.Context(), outcomeTooLarge)
		writeMatrixError(w, http.StatusRequestEntityTooLarge, "M_TOO_LARGE",
			"Upload request body is too large")
		return "", "", 0, false
	}

	// Trusted verbatim, exactly as Synapse does: no sniffing, no allowlist.
	mediaType = r.Header.Get("Content-Type")
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	// Stored unsanitised, also matching Synapse. The response side already
	// quotes and encodes it, which is where that belongs.
	uploadName = r.URL.Query().Get("filename")
	return mediaType, uploadName, length, true
}

// storedUpload describes bytes that have been written to their final path.
type storedUpload struct {
	path   string
	length int64
	sha256 string
}

// store streams the request body into the media store, hashing as it goes.
//
// The bytes go to a temporary file in the destination directory and are renamed
// into place, so a crash or a truncated body never leaves a file that looks
// complete. The temp file is on the same filesystem deliberately: rename cannot
// cross filesystems, and atomicity is worth more than staging elsewhere.
func (u *Uploader) store(r *http.Request, mediaID, mediaType string, declared int64) (*storedUpload, error) {
	select {
	case u.slots <- struct{}{}:
		defer func() { <-u.slots }()
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}

	path, err := u.paths.LocalMedia(mediaID)
	if err != nil {
		return nil, fmt.Errorf("building media path: %w", err)
	}
	if err := u.paths.WritablePath(path); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating media directory: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".incoming-*")
	if err != nil {
		return nil, fmt.Errorf("creating temporary file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}

	// One byte over the limit is enough to catch a body that lied about its
	// Content-Length, rather than trusting the header and filling the disk.
	max := u.cfg.Media.MaxUploadSizeOrDefault()
	hasher := sha256.New()
	written, err := io.Copy(io.MultiWriter(tmp, hasher), io.LimitReader(r.Body, max+1))
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("reading upload body: %w", err)
	}
	if written > max {
		cleanup()
		return nil, errUploadTooLarge
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return nil, fmt.Errorf("syncing upload: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return nil, fmt.Errorf("closing upload: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		_ = os.Remove(tmpName)
		return nil, fmt.Errorf("setting upload permissions: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return nil, fmt.Errorf("installing upload: %w", err)
	}

	return &storedUpload{path: path, length: written, sha256: hex.EncodeToString(hasher.Sum(nil))}, nil
}

// errUploadTooLarge marks a body that exceeded the limit while being read.
var errUploadTooLarge = errors.New("upload body exceeds max_upload_size")

// quarantineFor reproduces Synapse's silent hash quarantine: content matching
// something already quarantined is still stored and still answered 200, but the
// row is flagged so it will not be served.
//
// An error here fails the upload rather than guessing. Failing open would let
// quarantined content back in; failing closed would hand the uploader a 200 for
// media that is silently unusable, which is worse than an honest error they can
// retry. Synapse propagates the error too.
func (u *Uploader) quarantineFor(ctx context.Context, sha256hex string) (string, error) {
	quarantined, err := u.db.IsHashQuarantined(ctx, sha256hex)
	if err != nil {
		return "", fmt.Errorf("checking quarantined hashes: %w", err)
	}
	if quarantined {
		return quarantinedBySystem, nil
	}
	return "", nil
}

// maybeThumbnail generates Synapse's upload-time thumbnail set, when configured
// to. Failures are logged and swallowed, as Synapse does -- a thumbnail is not
// worth failing an upload over.
func (u *Uploader) maybeThumbnail(r *http.Request, mediaID, srcPath, mediaType string) {
	if u.cfg.Media.UploadThumbnails != uploadThumbnailsSynapse {
		return
	}
	go u.generateUploadThumbnails(mediaID, srcPath, mediaType)
}

func (s *Server) respondUploaded(w http.ResponseWriter, r *http.Request, endpoint, mediaID, user string, length int64, quarantinedBy string) {
	setMedia(r.Context(), "", mediaID)
	if quarantinedBy != "" {
		annotate(r.Context(), func(rl *reqLog) { rl.reason = "hash_quarantined" })
	}
	setOutcome(r.Context(), outcomeUploaded)
	uploadsTotal.WithLabelValues(endpoint, uploadResultStored).Inc()
	uploadedBytes.Add(float64(length))
	writeJSON(w, http.StatusOK, uploadResponse{
		ContentURI: "mxc://" + s.cfg.ServerName + "/" + mediaID,
	})
}

// uploadFailed answers a failed upload, preferring the spec's status where the
// cause is known.
func (s *Server) uploadFailed(w http.ResponseWriter, r *http.Request, endpoint string, err error, what string) {
	if errors.Is(err, errUploadTooLarge) {
		setOutcome(r.Context(), outcomeTooLarge)
		uploadsTotal.WithLabelValues(endpoint, uploadResultTooLarge).Inc()
		writeMatrixError(w, http.StatusRequestEntityTooLarge, "M_TOO_LARGE",
			"Upload request body is too large")
		return
	}
	if errors.Is(err, r.Context().Err()) && r.Context().Err() != nil {
		// The client went away mid-body; nothing to answer.
		return
	}
	uploadsTotal.WithLabelValues(endpoint, uploadResultFailed).Inc()
	s.internalError(w, r, err, what)
}

func (s *Server) proxyUpload(w http.ResponseWriter, r *http.Request, reason string) {
	proxiedTotal.WithLabelValues("upload", reason).Inc()
	uploadsTotal.WithLabelValues(uploadEndpointFor(r), uploadResultProxied).Inc()
	setProxied(r.Context(), reason)
	if s.uploadUp != nil {
		s.uploadUp.ServeHTTP(w, r)
		return
	}
	if s.downloadUp != nil {
		s.downloadUp.ServeHTTP(w, r)
		return
	}
	respondNotFound(w, r.URL.Path)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	setCORSHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeRateLimited answers the spec's 429 for too many pending media.
func writeRateLimited(w http.ResponseWriter, retryAfterMS int64) {
	setCORSHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"errcode":        "M_LIMIT_EXCEEDED",
		"error":          "Too many pending media uploads",
		"retry_after_ms": retryAfterMS,
	})
}

// generateUploadThumbnails reproduces Synapse's upload-time thumbnail set:
// DEFAULT_THUMBNAIL_SIZES crossed with the output type for the source format.
//
// This is off by default. It exists so a deployment can have byte-parity with
// Synapse if it needs it, but under dynamic_thumbnails these are written in a
// type no request path asks for, so they are generated and never served.
func (u *Uploader) generateUploadThumbnails(mediaID, srcPath, mediaType string) {
	outputType, ok := synapseThumbnailType(mediaType)
	if !ok {
		return // Synapse does not thumbnail this format either.
	}
	for _, size := range synapseDefaultThumbnailSizes {
		req := ThumbnailRequest{
			Width: size.width, Height: size.height,
			Method: size.method, Type: outputType,
		}
		data, err := u.thumbnailer.Generate(context.Background(), srcPath, mediaType, req)
		if err != nil {
			// Synapse logs and moves on; a thumbnail is not worth failing an
			// upload over, and the on-demand path will try again.
			continue
		}
		path, err := u.paths.LocalThumbnail(mediaID, req.Width, req.Height, req.Type, req.Method)
		if err != nil || u.paths.WritablePath(path) != nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			continue
		}
		if err := writeFileAtomic(path, data); err != nil {
			continue
		}
		length := int64(len(data))
		if info, err := os.Stat(path); err == nil {
			length = info.Size()
		}
		_ = u.db.StoreLocalThumbnail(context.Background(), mediaID, ThumbnailRow{
			Width: req.Width, Height: req.Height,
			Type: req.Type, Method: req.Method, Length: length,
		})
	}
}

// synapseDefaultThumbnailSizes is DEFAULT_THUMBNAIL_SIZES from
// synapse/config/repository.py.
var synapseDefaultThumbnailSizes = []struct {
	width, height int
	method        string
}{
	{32, 32, methodCrop},
	{96, 96, methodCrop},
	{320, 240, methodScale},
	{640, 480, methodScale},
	{800, 600, methodScale},
}

// synapseThumbnailType is THUMBNAIL_SUPPORTED_MEDIA_FORMAT_MAP: which output
// format Synapse produces for a given source. Note webp sources yield jpeg
// thumbnails, and gif yields png because gif can carry transparency.
func synapseThumbnailType(sourceType string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(strings.SplitN(sourceType, ";", 2)[0])) {
	case "image/jpeg", "image/jpg", "image/webp":
		return typeJPEG, true
	case "image/gif", "image/png":
		return typePNG, true
	default:
		return "", false
	}
}

// writeFileAtomic writes data via a temporary file in the same directory, so a
// crash cannot leave a partial file that looks complete.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".thumb-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// uploadEndpointFor labels a proxied request by which upload endpoint it was.
func uploadEndpointFor(r *http.Request) string {
	switch {
	case r.Method == http.MethodPut:
		return uploadEndpointAsync
	case strings.HasSuffix(r.URL.Path, "/create"):
		return uploadEndpointCreate
	default:
		return uploadEndpointSync
	}
}

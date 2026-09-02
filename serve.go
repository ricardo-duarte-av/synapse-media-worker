package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"
)

// serve.go reproduces the response headers from Synapse's synapse/media/_base.py.
// Divergences from Synapse are deliberate and marked; everything else should
// match byte for byte so clients cannot tell which worker answered.

// immutableETag is the literal ETag Synapse sets on every media response. It is
// unquoted, which is not valid HTTP, but changing it would break the 304
// handshake with clients that have cached Synapse's version.
const immutableETag = "1"

const cacheControlValue = "public,max-age=86400,s-maxage=0,proxy-revalidate"

// textContentTypes get an explicit charset appended. Synapse matches on the
// whole header value, so "text/css; charset=UTF-16" is deliberately left alone.
var textContentTypes = map[string]struct{}{
	"text/css": {}, "text/csv": {}, "text/html": {}, "text/calendar": {},
	"text/plain": {}, "text/javascript": {}, "application/json": {},
	"application/ld+json": {}, "application/rtf": {}, "image/svg+xml": {},
	"text/xml": {},
}

// inlineContentTypes may be rendered in the browser rather than downloaded.
// image/svg+xml is deliberately absent: SVG can carry script.
var inlineContentTypes = map[string]struct{}{
	"text/css": {}, "text/plain": {}, "text/csv": {}, "application/json": {},
	"application/ld+json": {}, "image/jpeg": {}, "image/gif": {},
	"image/png": {}, "image/apng": {}, "image/webp": {}, "image/avif": {},
	"video/mp4": {}, "video/webm": {}, "video/ogg": {}, "video/quicktime": {},
	"audio/mp4": {}, "audio/webm": {}, "audio/aac": {}, "audio/mpeg": {},
	"audio/ogg": {}, "audio/wave": {}, "audio/wav": {}, "audio/x-wav": {},
	"audio/x-pn-wav": {}, "audio/flac": {}, "audio/x-flac": {},
}

const mediaCSP = "sandbox; default-src 'none'; script-src 'none'; " +
	"plugin-types application/pdf; style-src 'unsafe-inline'; " +
	"media-src 'self'; object-src 'self';"

// MatrixError is the standard Matrix error body.
type MatrixError struct {
	ErrCode string `json:"errcode"`
	Error   string `json:"error"`
}

func writeMatrixError(w http.ResponseWriter, status int, errcode, message string) {
	setCORSHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(MatrixError{ErrCode: errcode, Error: message})
}

// writeMatrixErrorFields writes an error carrying the extra top-level fields
// some Matrix errors add, as Synapse's cs_error does with additional_fields.
func writeMatrixErrorFields(w http.ResponseWriter, status int, errcode, message string, extra map[string]any) {
	body := map[string]any{"errcode": errcode, "error": message}
	for k, v := range extra {
		body[k] = v
	}
	setCORSHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func respondNotFound(w http.ResponseWriter, path string) {
	writeMatrixError(w, http.StatusNotFound, "M_NOT_FOUND", "Not found '"+path+"'")
}

// respondNotYetUploaded matches Synapse's odd but specified 504 for media whose
// upload has not completed.
func respondNotYetUploaded(w http.ResponseWriter) {
	writeMatrixError(w, http.StatusGatewayTimeout, "M_NOT_YET_UPLOADED",
		"Media has not been uploaded yet")
}

func setCORSHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "GET, HEAD, POST, PUT, DELETE, OPTIONS")
	h.Set("Access-Control-Allow-Headers",
		"X-Requested-With, Content-Type, Authorization, Date")
	h.Set("Access-Control-Expose-Headers", "Synapse-Trace-Id, Server")
}

// setCORPHeaders allows the media to be embedded cross-origin, which clients
// running on a different domain to the homeserver rely on.
func setCORPHeaders(w http.ResponseWriter) {
	w.Header().Set("Cross-Origin-Resource-Policy", "cross-origin")
}

// setDownloadSecurityHeaders applies the sandboxing headers Synapse sets on
// download responses. Thumbnail responses deliberately do not get these.
func setDownloadSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", mediaCSP)
	h.Set("X-Content-Security-Policy", "sandbox;")
	h.Set("Referrer-Policy", "no-referrer")
}

func setCacheHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", cacheControlValue)
	h.Set("ETag", immutableETag)
}

// notModified reports whether the client already holds this media. Because
// every media response carries the same constant ETag, this is a simple string
// compare rather than real validator matching.
//
// It must only be consulted after authentication and quarantine checks, or a
// 304 would leak the existence of media the caller may not read.
func notModified(r *http.Request) bool {
	return r.Header.Get("If-None-Match") == immutableETag
}

func respondNotModified(w http.ResponseWriter) {
	setCORSHeaders(w)
	setCORPHeaders(w)
	setCacheHeaders(w)
	w.WriteHeader(http.StatusNotModified)
}

// contentTypeOrDefault mirrors Synapse substituting a generic type for media
// rows with no recorded content type.
func contentTypeOrDefault(mediaType string) string {
	if mediaType == "" {
		return "application/octet-stream"
	}
	return mediaType
}

// contentTypeHeader appends a charset for the text-like types Synapse lists.
func contentTypeHeader(mediaType string) string {
	mediaType = contentTypeOrDefault(mediaType)
	if _, ok := textContentTypes[strings.ToLower(mediaType)]; ok {
		return mediaType + "; charset=UTF-8"
	}
	return mediaType
}

// disallowedTokenChars are the HTTP separator characters that force a filename
// into the extended encoding.
const disallowedTokenChars = `()<>@,;:\"/[]?={}`

// canEncodeFilenameAsToken reports whether upload_name can be sent bare, with
// no quoting, in a Content-Disposition header.
func canEncodeFilenameAsToken(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if r <= 32 || r >= 127 {
			return false
		}
		if strings.ContainsRune(disallowedTokenChars, r) {
			return false
		}
	}
	return true
}

// contentDisposition builds the header the way Synapse does.
//
// Synapse never emits both `filename=` and `filename*=`; it picks one. Sending
// both would be more correct per RFC 6266 but would change what clients see, so
// this reproduces Synapse's choice.
func contentDisposition(mediaType, uploadName string) string {
	disposition := "attachment"
	base := strings.ToLower(strings.TrimSpace(strings.SplitN(contentTypeOrDefault(mediaType), ";", 2)[0]))
	if _, ok := inlineContentTypes[base]; ok {
		disposition = "inline"
	}
	if uploadName == "" {
		return disposition
	}
	if canEncodeFilenameAsToken(uploadName) {
		return disposition + "; filename=" + uploadName
	}
	return disposition + "; filename*=utf-8''" + urlEncodeFilename(uploadName)
}

// urlEncodeFilename percent-encodes like Python's urllib.parse.quote, which
// leaves _.-~ and / unescaped by default.
func urlEncodeFilename(name string) string {
	const safe = "_.-~/"
	var b strings.Builder
	for _, c := range []byte(name) {
		isAlnum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if isAlnum || strings.IndexByte(safe, c) >= 0 {
			b.WriteByte(c)
		} else {
			b.WriteString("%")
			const hex = "0123456789ABCDEF"
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0x0f])
		}
	}
	return b.String()
}

// sanitizeHeaderValue strips CR/LF and escapes quotes, matching the escaping
// Synapse's Header class performs. Header injection via upload_name would
// otherwise be possible.
func sanitizeHeaderValue(v string) string {
	v = strings.NewReplacer("\r", "", "\n", "").Replace(v)
	return strings.ReplaceAll(v, `"`, `\"`)
}

// addFileHeaders sets the common media response headers.
func addFileHeaders(w http.ResponseWriter, mediaType, uploadName string) {
	h := w.Header()
	h.Set("Content-Type", sanitizeHeaderValue(contentTypeHeader(mediaType)))
	h.Set("Content-Disposition", sanitizeHeaderValue(contentDisposition(mediaType, uploadName)))
	h.Set("X-Robots-Tag", "noindex, nofollow, noarchive, noimageindex")
	setCacheHeaders(w)
}

// serveFile writes an open file as the response body.
//
// Deliberate divergence from Synapse: http.ServeContent implements Range
// requests, which Synapse does not support at all. This is a superset of
// Synapse's behaviour and is where most of the performance win comes from, as
// the kernel can sendfile the body rather than copying it through userspace.
func serveFile(w http.ResponseWriter, r *http.Request, f *os.File) {
	// modtime is zeroed so ServeContent does not emit Last-Modified or perform
	// If-Modified-Since handling; Synapse sends neither, and the constant ETag
	// is the only validator clients should use.
	http.ServeContent(w, r, "", time.Time{}, f)
}

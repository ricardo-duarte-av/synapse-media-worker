package main

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"strconv"
)

// multipart.go writes the multipart/mixed body that federation media downloads
// return (MSC3916).
//
// The framing is hand-written rather than delegated to mime/multipart because
// Synapse's MultipartFileConsumer emits a specific byte sequence that other
// homeservers parse, and mime/multipart does not reproduce it: there is a
// leading CRLF before the first boundary, no preamble, and the metadata part
// carries no Content-Disposition.

const (
	crlf            = "\r\n"
	metadataJSON    = "{}"
	metadataCTLine  = "Content-Type: application/json"
	multipartFixedN = 156 // Synapse's constant, verified by fixedOverhead()
)

// newBoundary produces a 32-character hex boundary, matching Synapse's
// uuid4().hex. The value is used unquoted in the Content-Type header.
func newBoundary() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("reading random bytes for multipart boundary: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// multipartParts holds the pieces of the response whose lengths vary.
type multipartParts struct {
	boundary        string
	contentTypeLine string
	dispositionLine string
}

func newMultipartParts(boundary, mediaType, uploadName string) multipartParts {
	return multipartParts{
		boundary:        boundary,
		contentTypeLine: "Content-Type: " + sanitizeHeaderValue(contentTypeHeader(mediaType)),
		dispositionLine: "Content-Disposition: " + sanitizeHeaderValue(contentDisposition(mediaType, uploadName)),
	}
}

// contentLength returns the exact size of the multipart body wrapping a file of
// mediaLength bytes.
func (p multipartParts) contentLength(mediaLength int64) int64 {
	return mediaLength +
		int64(len(metadataJSON)) +
		int64(len(p.contentTypeLine)) +
		int64(len(p.dispositionLine)) +
		p.fixedOverhead()
}

// fixedOverhead is the boundary and CRLF scaffolding, which is constant for a
// 32-character boundary. Computing it rather than hardcoding 156 keeps the two
// in step if the framing ever changes.
func (p multipartParts) fixedOverhead() int64 {
	b := int64(len(p.boundary))
	return 2 + // leading CRLF
		(2 + b + 2) + // --boundary CRLF
		int64(len(metadataCTLine)) + 2 + // metadata Content-Type line
		2 + // blank line before the JSON
		2 + // CRLF after the JSON
		(2 + b + 2) + // --boundary CRLF
		2 + // CRLF ending the file part's Content-Type line
		2 + // CRLF ending the Content-Disposition line
		2 + // blank line before the body
		(2 + 2 + b + 2 + 2) // CRLF --boundary-- CRLF
}

// writeHeader sets the response headers for a multipart federation download.
// mediaLength may be negative when the size is unknown, in which case no
// Content-Length is sent.
func (p multipartParts) writeHeader(w http.ResponseWriter, mediaLength int64) {
	h := w.Header()
	h.Set("Content-Type", "multipart/mixed; boundary="+p.boundary)
	if mediaLength >= 0 {
		h.Set("Content-Length", strconv.FormatInt(p.contentLength(mediaLength), 10))
	}
	setCacheHeaders(w)
}

// writeBody streams the multipart body. It must be called after writeHeader.
func (p multipartParts) writeBody(w io.Writer, body io.Reader) error {
	// Metadata part. The JSON is exactly "{}" — Synapse notes that changing it
	// would require changing the constant ETag.
	if _, err := io.WriteString(w, crlf+"--"+p.boundary+crlf+
		metadataCTLine+crlf+
		crlf+metadataJSON); err != nil {
		return err
	}
	// File part.
	if _, err := io.WriteString(w, crlf+"--"+p.boundary+crlf+
		p.contentTypeLine+crlf+
		p.dispositionLine+crlf+crlf); err != nil {
		return err
	}
	if _, err := io.Copy(w, body); err != nil {
		return err
	}
	_, err := io.WriteString(w, crlf+"--"+p.boundary+"--"+crlf)
	return err
}

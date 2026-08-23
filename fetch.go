package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/federation"
)

// fetch.go downloads remote media over federation, the job Synapse's
// _federation_download_remote_file does.
//
// mautrix's federation.Client.DownloadMedia is deliberately not used. In
// v0.30.1 it returns (nil, nil, nil) when the request fails -- the error is
// swallowed, so a failure is indistinguishable from success with no data. It
// also gives no way to stream to disk while hashing and enforcing a size cap,
// which is what storing the file correctly requires.

// ErrTooLarge means the remote file exceeds max_upload_size. Synapse answers
// 502 M_TOO_LARGE for this, which is odd but is what peers expect.
var ErrTooLarge = errors.New("remote file is too large")

// ErrFetchFailed means the download did not complete. Callers fall back to
// proxying rather than surfacing it.
var ErrFetchFailed = errors.New("remote media fetch failed")

// ErrOriginNotFound means the origin server answered 404: the media does not
// exist there.
//
// This is worth distinguishing because it is definitive. A dead DNS name or a
// TLS failure might be our resolver or our network, and Synapse deserves a
// second opinion; an origin that answers "no such media" will tell Synapse the
// same thing, so proxying only makes the client wait for two attempts at the
// same answer.
var ErrOriginNotFound = errors.New("origin does not have this media")

// FetchedMedia describes a downloaded file, before it is stored.
type FetchedMedia struct {
	MediaType  string
	UploadName string
	Length     int64
	SHA256     string
}

// Fetcher downloads media from other homeservers.
type Fetcher struct {
	client        *federation.Client
	maxUploadSize int64
	timeout       time.Duration
	// slots bounds simultaneous downloads, which are network-bound but each
	// hold an open file and a buffer.
	slots chan struct{}
}

func NewFetcher(client *federation.Client, maxUploadSize int64, timeout time.Duration, maxConcurrent int) *Fetcher {
	if maxConcurrent <= 0 {
		maxConcurrent = 4
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &Fetcher{
		client:        client,
		maxUploadSize: maxUploadSize,
		timeout:       timeout,
		slots:         make(chan struct{}, maxConcurrent),
	}
}

// Fetch downloads the media and streams it into dst, returning its metadata.
//
// The sha256 is computed on the way past rather than in a second pass, matching
// Synapse's SHA256TransparentIOWriter. It is not optional: Synapse's admin
// quarantine resolves media by hash across the local and remote tables, so a
// row stored without one is invisible to it.
func (f *Fetcher) Fetch(ctx context.Context, serverName, mediaID string, dst io.Writer) (*FetchedMedia, error) {
	select {
	case f.slots <- struct{}{}:
		defer func() { <-f.slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()

	// timeout_ms tells the remote how long it may spend fetching on our
	// behalf; it does not bound our own read, which the context does.
	query := url.Values{}
	query.Set("timeout_ms", fmt.Sprintf("%d", f.timeout.Milliseconds()))

	_, resp, err := f.client.MakeFullRequest(ctx, federation.RequestParams{
		ServerName:   serverName,
		Method:       http.MethodGet,
		Path:         federation.URLPath{"v1", "media", "download", mediaID},
		Query:        query,
		Authenticate: true,
		DontReadBody: true,
	})
	if err != nil {
		if originStatus(err) == http.StatusNotFound {
			return nil, fmt.Errorf("%w: %v", ErrOriginNotFound, err)
		}
		return nil, fmt.Errorf("%w: %v", ErrFetchFailed, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.ContentLength > 0 && resp.ContentLength > f.maxUploadSize {
		return nil, fmt.Errorf("%w: %d bytes exceeds the %d byte limit",
			ErrTooLarge, resp.ContentLength, f.maxUploadSize)
	}

	return f.readMultipart(ctx, resp, dst)
}

// readMultipart unwraps the multipart/mixed envelope federation media downloads
// use. It is the mirror of the writer in multipart.go.
func (f *Fetcher) readMultipart(ctx context.Context, resp *http.Response, dst io.Writer) (*FetchedMedia, error) {
	mediaType, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return nil, fmt.Errorf("%w: parsing content type: %v", ErrFetchFailed, err)
	}
	if mediaType != "multipart/mixed" {
		return nil, fmt.Errorf("%w: unexpected content type %q", ErrFetchFailed, mediaType)
	}
	boundary := params["boundary"]
	if boundary == "" {
		return nil, fmt.Errorf("%w: no boundary in content type", ErrFetchFailed)
	}
	mr := multipart.NewReader(resp.Body, boundary)

	// First part is metadata. Synapse requires it to be application/json and
	// ignores the body, which is always {}.
	meta, err := mr.NextPart()
	if err != nil {
		return nil, fmt.Errorf("%w: reading metadata part: %v", ErrFetchFailed, err)
	}
	if ct := meta.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		_ = meta.Close()
		return nil, fmt.Errorf("%w: metadata part is %q, want application/json", ErrFetchFailed, ct)
	}
	// Bounded: a peer must not be able to make us buffer an arbitrary amount
	// of "metadata".
	if _, err := io.Copy(io.Discard, io.LimitReader(meta, 64*1024)); err != nil {
		_ = meta.Close()
		return nil, fmt.Errorf("%w: reading metadata part: %v", ErrFetchFailed, err)
	}
	_ = meta.Close()

	filePart, err := mr.NextPart()
	if err != nil {
		return nil, fmt.Errorf("%w: reading file part: %v", ErrFetchFailed, err)
	}
	defer func() { _ = filePart.Close() }()

	// A Location header means the file lives elsewhere; the part carries no
	// body. Synapse follows it with a plain HTTPS request.
	if redirect := filePart.Header.Get("Location"); redirect != "" {
		return f.fetchRedirect(ctx, redirect, dst)
	}

	info := &FetchedMedia{
		MediaType:  headerOrDefault(filePart.Header.Get("Content-Type"), "application/octet-stream"),
		UploadName: filenameFromDisposition(filePart.Header.Get("Content-Disposition")),
	}
	return f.copyBody(filePart, dst, info)
}

// fetchRedirect follows the Location a peer may return instead of the bytes.
func (f *Fetcher) fetchRedirect(ctx context.Context, rawURL string, dst io.Writer) (*FetchedMedia, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("%w: bad redirect url: %v", ErrFetchFailed, err)
	}
	// Synapse refuses anything but https here, and so do we: the redirect
	// target is chosen by a remote server.
	if parsed.Scheme != "https" {
		return nil, fmt.Errorf("%w: redirect to non-https url", ErrFetchFailed)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFetchFailed, err)
	}
	// ExtHTTP carries the client's IP allowlist, so a redirect cannot be
	// pointed at a private address.
	resp, err := f.client.ExtHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: following redirect: %v", ErrFetchFailed, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: redirect returned %d", ErrFetchFailed, resp.StatusCode)
	}
	if resp.ContentLength > 0 && resp.ContentLength > f.maxUploadSize {
		return nil, fmt.Errorf("%w: redirect target is %d bytes", ErrTooLarge, resp.ContentLength)
	}
	info := &FetchedMedia{
		MediaType:  headerOrDefault(resp.Header.Get("Content-Type"), "application/octet-stream"),
		UploadName: filenameFromDisposition(resp.Header.Get("Content-Disposition")),
	}
	return f.copyBody(resp.Body, dst, info)
}

// copyBody streams the file to dst, hashing as it goes and refusing to write
// more than max_upload_size.
func (f *Fetcher) copyBody(src io.Reader, dst io.Writer, info *FetchedMedia) (*FetchedMedia, error) {
	hasher := sha256.New()
	// One byte over the limit is enough to detect a body that lied about, or
	// omitted, its Content-Length.
	limited := io.LimitReader(src, f.maxUploadSize+1)
	written, err := io.Copy(io.MultiWriter(dst, hasher), limited)
	if err != nil {
		return nil, fmt.Errorf("%w: reading body: %v", ErrFetchFailed, err)
	}
	if written > f.maxUploadSize {
		return nil, fmt.Errorf("%w: body exceeds the %d byte limit", ErrTooLarge, f.maxUploadSize)
	}
	info.Length = written
	info.SHA256 = hex.EncodeToString(hasher.Sum(nil))
	return info, nil
}

func headerOrDefault(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// filenameFromDisposition extracts upload_name, mirroring Synapse's
// get_filename_from_headers: filename*= is preferred, and a plain filename= is
// only accepted when it is ASCII.
func filenameFromDisposition(disposition string) string {
	if disposition == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(disposition)
	if err != nil {
		return ""
	}
	// mime.ParseMediaType already decodes RFC 2231 filename* into "filename".
	name := params["filename"]
	if name == "" {
		return ""
	}
	// Strip any path components a peer may have included.
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	if name == "." || name == ".." {
		return ""
	}
	return name
}

// originStatus digs the origin server's HTTP status out of a federation error,
// or returns 0 when the request never got that far.
func originStatus(err error) int {
	var httpErr mautrix.HTTPError
	if !errors.As(err, &httpErr) {
		return 0
	}
	if httpErr.RespError != nil && httpErr.RespError.StatusCode != 0 {
		return httpErr.RespError.StatusCode
	}
	if httpErr.Response != nil {
		return httpErr.Response.StatusCode
	}
	return 0
}

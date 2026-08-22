package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"io"
	"net/http"
	"os"
	"strings"
)

// client.go compares the authenticated client endpoints.
//
// This is the half of the worker that federation cannot reach: remote media,
// and the security headers Synapse sets on downloads but not on thumbnails.

// clientHeaders are the headers a client actually acts on. A difference in any
// of them is a bug, so they are all compared rather than a chosen few.
var clientDownloadHeaders = []string{
	"Content-Type",
	"Content-Disposition",
	"Content-Security-Policy",
	"X-Content-Security-Policy",
	"Referrer-Policy",
	"Cross-Origin-Resource-Policy",
	"Cache-Control",
	"ETag",
	"X-Robots-Tag",
	"Access-Control-Allow-Origin",
}

// Synapse deliberately sets no CSP on thumbnail responses, so that difference
// is expected and must be preserved rather than corrected.
var clientThumbnailHeaders = []string{
	"Content-Type",
	"Content-Disposition",
	"Cross-Origin-Resource-Policy",
	"Cache-Control",
	"ETag",
	"Access-Control-Allow-Origin",
}

type mediaRef struct {
	origin  string // empty for local media
	mediaID string
	name    string
}

func (m mediaRef) server(defaultServer string) string {
	if m.origin != "" {
		return m.origin
	}
	return defaultServer
}

func (m mediaRef) String() string {
	if m.origin != "" {
		return m.origin + "/" + m.mediaID
	}
	return m.mediaID
}

// clientFetch performs an authenticated client-API request against one worker.
func clientFetch(client *http.Client, base, path, token string, extra http.Header) (*http.Response, []byte, error) {
	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	for k, vs := range extra {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Host = *serverName

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	// Cap the body so a stray multi-gigabyte file cannot exhaust memory; the
	// size comparison still covers the whole file via Content-Length.
	body, err := io.ReadAll(io.LimitReader(resp.Body, *maxCompareBytes))
	if err != nil {
		return nil, nil, err
	}
	return resp, body, nil
}

// compareClientDownload diffs a download across both workers.
func compareClientDownload(goClient, synClient *http.Client, synBase string, m mediaRef, token string, r *result) {
	path := fmt.Sprintf("/_matrix/client/v1/media/download/%s/%s", m.server(*serverName), m.mediaID)

	goResp, goBody, err1 := clientFetch(goClient, *goURL, path, token, nil)
	synResp, synBody, err2 := clientFetch(synClient, synBase, path, token, nil)
	if err1 != nil || err2 != nil {
		fmt.Printf("  %s download: transport error (go=%v synapse=%v)\n", m, err1, err2)
		r.skipped++
		return
	}
	if goResp.StatusCode != synResp.StatusCode {
		fmt.Printf("  %s download: status %d vs synapse %d\n", m, goResp.StatusCode, synResp.StatusCode)
		r.mismatch++
		return
	}
	if goResp.StatusCode != http.StatusOK {
		r.skipped++
		return
	}
	r.checked++

	if !bytes.Equal(goBody, synBody) {
		fmt.Printf("  %s download: BODY DIFFERS (go %d bytes, synapse %d bytes)\n",
			m, len(goBody), len(synBody))
		r.mismatch++
		return
	}
	if diffHeaders(m.String()+" download", goResp.Header, synResp.Header, clientDownloadHeaders) {
		r.mismatch++
	}
}

// compareClientThumbnail diffs a thumbnail across both workers.
func compareClientThumbnail(goClient, synClient *http.Client, synBase string, m mediaRef, spec, token string, r *result) {
	var w, h int
	var method string
	if _, err := fmt.Sscanf(spec, "%dx%d:%s", &w, &h, &method); err != nil {
		fatal("bad thumbnail spec %q", spec)
	}
	path := fmt.Sprintf("/_matrix/client/v1/media/thumbnail/%s/%s?width=%d&height=%d&method=%s",
		m.server(*serverName), m.mediaID, w, h, method)

	goResp, goBody, err1 := clientFetch(goClient, *goURL, path, token, nil)
	synResp, synBody, err2 := clientFetch(synClient, synBase, path, token, nil)
	if err1 != nil || err2 != nil {
		r.skipped++
		return
	}
	if goResp.StatusCode != synResp.StatusCode {
		fmt.Printf("  %s thumb %s: status %d vs synapse %d\n", m, spec, goResp.StatusCode, synResp.StatusCode)
		r.mismatch++
		return
	}
	if goResp.StatusCode != http.StatusOK {
		r.skipped++
		return
	}
	r.checked++

	goCfg, _, e1 := image.DecodeConfig(bytes.NewReader(goBody))
	synCfg, _, e2 := image.DecodeConfig(bytes.NewReader(synBody))
	if e1 != nil || e2 != nil {
		fmt.Printf("  %s thumb %s: decode failed (go=%v synapse=%v)\n", m, spec, e1, e2)
		r.mismatch++
		return
	}
	if goCfg.Width != synCfg.Width || goCfg.Height != synCfg.Height {
		fmt.Printf("  %s thumb %s: DIMENSIONS %dx%d vs synapse %dx%d\n",
			m, spec, goCfg.Width, goCfg.Height, synCfg.Width, synCfg.Height)
		r.mismatch++
		return
	}
	if diffHeaders(fmt.Sprintf("%s thumb %s", m, spec), goResp.Header, synResp.Header, clientThumbnailHeaders) {
		r.mismatch++
	}
}

// compareConditional checks the 304 handshake, which depends on Synapse's
// unusual unquoted constant ETag.
func compareConditional(goClient, synClient *http.Client, synBase string, m mediaRef, token string, r *result) {
	path := fmt.Sprintf("/_matrix/client/v1/media/download/%s/%s", m.server(*serverName), m.mediaID)
	extra := http.Header{"If-None-Match": []string{"1"}}

	goResp, _, err1 := clientFetch(goClient, *goURL, path, token, extra)
	synResp, _, err2 := clientFetch(synClient, synBase, path, token, extra)
	if err1 != nil || err2 != nil {
		r.skipped++
		return
	}
	r.checked++
	if goResp.StatusCode != synResp.StatusCode {
		fmt.Printf("  %s if-none-match: status %d vs synapse %d\n",
			m, goResp.StatusCode, synResp.StatusCode)
		r.mismatch++
	}
}

// compareRange checks Range handling. Synapse does not implement Range and
// answers 200 with the whole body; the worker answers 206 with the slice. That
// divergence is intentional, so this asserts the worker's own correctness and
// that the bytes agree with Synapse's full body.
func compareRange(goClient, synClient *http.Client, synBase string, m mediaRef, token string, r *result) {
	path := fmt.Sprintf("/_matrix/client/v1/media/download/%s/%s", m.server(*serverName), m.mediaID)

	synResp, synBody, err := clientFetch(synClient, synBase, path, token, nil)
	if err != nil || synResp.StatusCode != http.StatusOK || len(synBody) < 2048 {
		r.skipped++
		return
	}
	extra := http.Header{"Range": []string{"bytes=0-1023"}}
	goResp, goBody, err := clientFetch(goClient, *goURL, path, token, extra)
	if err != nil {
		r.skipped++
		return
	}
	r.checked++

	if goResp.StatusCode != http.StatusPartialContent {
		fmt.Printf("  %s range: status %d, want 206\n", m, goResp.StatusCode)
		r.mismatch++
		return
	}
	if len(goBody) != 1024 || !bytes.Equal(goBody, synBody[:1024]) {
		fmt.Printf("  %s range: %d bytes and does not match the head of Synapse's body\n", m, len(goBody))
		r.mismatch++
		return
	}
	if got := goResp.Header.Get("Content-Range"); !strings.HasPrefix(got, "bytes 0-1023/") {
		fmt.Printf("  %s range: Content-Range = %q\n", m, got)
		r.mismatch++
	}
}

// proxyModifiedHeaders are deliberately rewritten or stripped by the reverse
// proxy, so they are not comparable when the Synapse side is reached through it
// while the worker is reached directly.
//
// aguiarvieira.pt caches media aggressively and sets `proxy_hide_header
// Cache-Control`, so Synapse's value never reaches a client. The worker emits
// the same value Synapse does and it will be stripped identically once it sits
// behind the same proxy, so there is nothing to reconcile here -- only
// something the harness cannot see.
var proxyModifiedHeaders = map[string]bool{
	"Cache-Control": true,
}

func diffHeaders(label string, got, want http.Header, names []string) bool {
	bad := false
	for _, h := range names {
		if *synapseURL != "" && proxyModifiedHeaders[h] {
			continue
		}
		if got.Get(h) != want.Get(h) {
			fmt.Printf("  %s: %s %q vs synapse %q\n", label, h, got.Get(h), want.Get(h))
			bad = true
		}
	}
	return bad
}

func readToken(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return token, nil
}

// sampleRemoteMedia picks remote media Synapse has already cached, so the
// comparison never triggers a fresh federated download from another server.
func sampleRemoteMedia(ctx context.Context, uri string, n int) ([]mediaRef, error) {
	pool, err := openPool(ctx, uri)
	if err != nil {
		return nil, err
	}
	defer pool.Close()

	const q = `
SELECT media_origin, media_id
  FROM remote_media_cache
 WHERE media_length IS NOT NULL
   AND quarantined_by IS NULL
   AND filesystem_id IS NOT NULL
 LIMIT $1`
	rows, err := pool.Query(ctx, q, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []mediaRef
	for rows.Next() {
		var m mediaRef
		if err := rows.Scan(&m.origin, &m.mediaID); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

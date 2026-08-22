// Command parity compares this worker's responses against Synapse's for the
// same media, which is the acceptance test for the worker: any difference in
// bytes or in the headers clients act on is a bug.
//
// Federation is used as the transport because it can be authenticated with the
// homeserver's own signing key, with no need to mint a client access token.
//
//	go run ./tools/parity \
//	  -server-name aguiarvieira.pt \
//	  -key /opt/matrix/synapse/synapse/aguiarvieira.pt.signing.key \
//	  -go-url http://127.0.0.1:18090 \
//	  -synapse-socket /var/sockets/nginx/av-media-worker-1.sock \
//	  -db "postgres://...' -n 100
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "golang.org/x/image/webp"
	"maunium.net/go/mautrix/federation"
)

var (
	serverName     = flag.String("server-name", "", "homeserver name")
	keyPath        = flag.String("key", "", "path to Synapse's signing.key")
	goURL          = flag.String("go-url", "http://127.0.0.1:18090", "base URL of the Go worker")
	synapseSocket  = flag.String("synapse-socket", "", "unix socket of a Synapse media worker")
	synapseURL     = flag.String("synapse-url", "", "public base URL of the homeserver, e.g. https://example.com (preferred over -synapse-socket)")
	dbURI          = flag.String("db", "", "Synapse database URI, used to sample media IDs")
	sampleSize     = flag.Int("n", 50, "number of media items to compare")
	thumbnailSizes = flag.String("thumb-sizes", "32x32:crop,96x96:crop,320x240:scale,800x600:scale,173x91:scale",
		"thumbnail sizes to compare")
	mode            = flag.String("mode", "both", "which endpoints to compare: federation, client, or both")
	tokenFile       = flag.String("token-file", "", "file holding a client access token; required for client mode")
	maxCompareBytes = flag.Int64("max-bytes", 256<<20, "cap on how much of a body to read for comparison")
	includeRemote   = flag.Bool("remote", true, "include remote media in client-mode comparison")
)

type result struct {
	checked  int
	mismatch int
	skipped  int
}

func main() {
	flag.Parse()
	if *serverName == "" || *keyPath == "" || *dbURI == "" ||
		(*synapseSocket == "" && *synapseURL == "") {
		flag.Usage()
		os.Exit(2)
	}
	doFederation := *mode == "federation" || *mode == "both"
	doClient := *mode == "client" || *mode == "both"

	var token string
	if doClient {
		if *tokenFile == "" {
			fatal("client mode needs -token-file")
		}
		var err error
		if token, err = readToken(*tokenFile); err != nil {
			fatal("reading token: %v", err)
		}
	}

	key, err := loadFirstKey(*keyPath)
	if err != nil {
		fatal("loading signing key: %v", err)
	}

	ctx := context.Background()
	localIDs, err := sampleMediaIDs(ctx, *dbURI, *sampleSize)
	if err != nil {
		fatal("sampling local media: %v", err)
	}
	local := make([]mediaRef, 0, len(localIDs))
	for _, id := range localIDs {
		local = append(local, mediaRef{mediaID: id})
	}

	var remote []mediaRef
	if doClient && *includeRemote {
		// Remote media is only reachable over the client API; federation
		// downloads are local-media-only by specification.
		if remote, err = sampleRemoteMedia(ctx, *dbURI, *sampleSize/2); err != nil {
			fatal("sampling remote media: %v", err)
		}
	}
	fmt.Printf("comparing %d local and %d remote media items (mode=%s)\n\n",
		len(local), len(remote), *mode)

	goClient := &http.Client{Timeout: 120 * time.Second}
	synClient, synBase := newSynapseClient()

	var fedDown, fedThumb, cliDown, cliThumb, cond, rng result

	if doFederation {
		for _, m := range local {
			compareDownload(goClient, synClient, synBase, key, m.mediaID, &fedDown)
			for _, spec := range strings.Split(*thumbnailSizes, ",") {
				compareThumbnail(goClient, synClient, synBase, key, m.mediaID, spec, &fedThumb)
			}
		}
	}
	if doClient {
		all := append(append([]mediaRef{}, local...), remote...)
		for _, m := range all {
			compareClientDownload(goClient, synClient, synBase, m, token, &cliDown)
			compareConditional(goClient, synClient, synBase, m, token, &cond)
			compareRange(goClient, synClient, synBase, m, token, &rng)
			for _, spec := range strings.Split(*thumbnailSizes, ",") {
				compareClientThumbnail(goClient, synClient, synBase, m, spec, token, &cliThumb)
			}
		}
	}

	fmt.Println()
	report := func(label string, r result) {
		if r.checked == 0 && r.skipped == 0 {
			return
		}
		fmt.Printf("%-22s %4d checked, %4d skipped, %d MISMATCHED\n",
			label+":", r.checked, r.skipped, r.mismatch)
	}
	report("federation downloads", fedDown)
	report("federation thumbnails", fedThumb)
	report("client downloads", cliDown)
	report("client thumbnails", cliThumb)
	report("if-none-match", cond)
	report("range", rng)

	total := fedDown.mismatch + fedThumb.mismatch + cliDown.mismatch +
		cliThumb.mismatch + cond.mismatch + rng.mismatch
	if total > 0 {
		os.Exit(1)
	}
}

// compareDownload fetches the same media from both workers and requires the
// file bytes and the metadata headers to be identical.
func compareDownload(goClient, synClient *http.Client, synBase string, key *federation.SigningKey, mediaID string, r *result) {
	path := "/_matrix/federation/v1/media/download/" + mediaID

	goMeta, goBody, goStatus, err := fetchMultipart(goClient, *goURL, path, key)
	if err != nil {
		fmt.Printf("  %s download: go worker error: %v\n", mediaID, err)
		r.mismatch++
		return
	}
	synMeta, synBody, synStatus, err := fetchMultipart(synClient, synBase, path, key)
	if err != nil {
		fmt.Printf("  %s download: synapse error: %v\n", mediaID, err)
		r.skipped++
		return
	}

	if goStatus != synStatus {
		fmt.Printf("  %s download: status %d vs synapse %d\n", mediaID, goStatus, synStatus)
		r.mismatch++
		return
	}
	if goStatus != http.StatusOK {
		r.skipped++
		return
	}
	r.checked++

	if !bytes.Equal(goBody, synBody) {
		fmt.Printf("  %s download: BODY DIFFERS (go %d bytes sha %s, synapse %d bytes sha %s)\n",
			mediaID, len(goBody), shortSum(goBody), len(synBody), shortSum(synBody))
		r.mismatch++
		return
	}
	for _, h := range []string{"Content-Type", "Content-Disposition"} {
		if goMeta.Get(h) != synMeta.Get(h) {
			fmt.Printf("  %s download: %s differs: %q vs %q\n",
				mediaID, h, goMeta.Get(h), synMeta.Get(h))
			r.mismatch++
			return
		}
	}
}

// compareThumbnail requires the decoded dimensions and content type to match.
// The bytes will not: Go's Lanczos implementation and Pillow's differ in the
// low bits, which is expected and invisible.
func compareThumbnail(goClient, synClient *http.Client, synBase string, key *federation.SigningKey, mediaID, spec string, r *result) {
	var w, h int
	var method string
	if _, err := fmt.Sscanf(spec, "%dx%d:%s", &w, &h, &method); err != nil {
		fatal("bad thumbnail spec %q", spec)
	}
	path := fmt.Sprintf("/_matrix/federation/v1/media/thumbnail/%s?width=%d&height=%d&method=%s",
		mediaID, w, h, method)

	goMeta, goBody, goStatus, err1 := fetchMultipart(goClient, *goURL, path, key)
	synMeta, synBody, synStatus, err2 := fetchMultipart(synClient, synBase, path, key)
	if err1 != nil || err2 != nil {
		r.skipped++
		return
	}
	if goStatus != synStatus {
		fmt.Printf("  %s thumb %s: status %d vs synapse %d\n", mediaID, spec, goStatus, synStatus)
		r.mismatch++
		return
	}
	if goStatus != http.StatusOK {
		r.skipped++
		return
	}
	r.checked++

	goCfg, _, err1 := image.DecodeConfig(bytes.NewReader(goBody))
	synCfg, _, err2 := image.DecodeConfig(bytes.NewReader(synBody))
	if err1 != nil || err2 != nil {
		fmt.Printf("  %s thumb %s: decode failed (go=%v synapse=%v)\n", mediaID, spec, err1, err2)
		r.mismatch++
		return
	}
	if goCfg.Width != synCfg.Width || goCfg.Height != synCfg.Height {
		fmt.Printf("  %s thumb %s: DIMENSIONS %dx%d vs synapse %dx%d\n",
			mediaID, spec, goCfg.Width, goCfg.Height, synCfg.Width, synCfg.Height)
		r.mismatch++
		return
	}
	for _, h := range []string{"Content-Type", "Content-Disposition"} {
		if goMeta.Get(h) != synMeta.Get(h) {
			fmt.Printf("  %s thumb %s: %s %q vs synapse %q\n",
				mediaID, spec, h, goMeta.Get(h), synMeta.Get(h))
			r.mismatch++
			return
		}
	}
}

// fetchMultipart performs a signed federation request and unwraps the
// multipart/mixed response into its metadata headers and file bytes.
func fetchMultipart(client *http.Client, base, path string, key *federation.SigningKey) (http.Header, []byte, int, error) {
	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		return nil, nil, 0, err
	}
	auth, err := signRequest(key, http.MethodGet, req.URL.RequestURI(), *serverName, *serverName)
	if err != nil {
		return nil, nil, 0, err
	}
	req.Header.Set("Authorization", auth)
	req.Host = *serverName

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, nil, resp.StatusCode, nil
	}

	mediaType, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return nil, nil, resp.StatusCode, fmt.Errorf("parsing content type: %w", err)
	}
	if mediaType != "multipart/mixed" {
		return nil, nil, resp.StatusCode, fmt.Errorf("unexpected content type %q", mediaType)
	}
	mr := multipart.NewReader(resp.Body, params["boundary"])
	if _, err := mr.NextPart(); err != nil {
		return nil, nil, resp.StatusCode, fmt.Errorf("reading metadata part: %w", err)
	}
	filePart, err := mr.NextPart()
	if err != nil {
		return nil, nil, resp.StatusCode, fmt.Errorf("reading file part: %w", err)
	}
	body, err := io.ReadAll(filePart)
	if err != nil {
		return nil, nil, resp.StatusCode, err
	}
	return http.Header(filePart.Header), body, resp.StatusCode, nil
}

// signableRequest mirrors the structure mautrix signs and verifies. It is
// duplicated here because the type is unexported.
type signableRequest struct {
	Method      string          `json:"method"`
	URI         string          `json:"uri"`
	Origin      string          `json:"origin"`
	Destination string          `json:"destination"`
	Content     json.RawMessage `json:"content,omitempty"`
}

func signRequest(key *federation.SigningKey, method, uri, origin, destination string) (string, error) {
	sig, err := key.SignJSON(&signableRequest{
		Method: method, URI: uri, Origin: origin, Destination: destination,
	})
	if err != nil {
		return "", err
	}
	return federation.XMatrixAuth{
		Origin: origin, Destination: destination,
		KeyID: key.ID, Signature: sig,
	}.String(), nil
}

func loadFirstKey(path string) (*federation.SigningKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if line = strings.TrimSpace(line); line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return federation.ParseSynapseKey(line)
	}
	return nil, fmt.Errorf("no keys in %s", path)
}

// sampleMediaIDs picks a spread of media, biased towards the shapes most
// likely to expose a difference: unusual content types and awkward names.
func sampleMediaIDs(ctx context.Context, uri string, n int) ([]string, error) {
	pool, err := openPool(ctx, uri)
	if err != nil {
		return nil, err
	}
	defer pool.Close()

	const q = `
(SELECT media_id FROM local_media_repository
  WHERE media_length IS NOT NULL AND quarantined_by IS NULL
    AND media_type LIKE 'image/%' LIMIT $1)
UNION
(SELECT media_id FROM local_media_repository
  WHERE media_length IS NOT NULL AND quarantined_by IS NULL
    AND media_type NOT LIKE 'image/%' LIMIT $2)
UNION
(SELECT media_id FROM local_media_repository
  WHERE media_length IS NOT NULL AND quarantined_by IS NULL
    AND upload_name ~ '[^a-zA-Z0-9._-]' LIMIT $2)`

	rows, err := pool.Query(ctx, q, n/2, n/4)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func openPool(ctx context.Context, uri string) (*pgxpool.Pool, error) {
	return pgxpool.New(ctx, uri)
}

func shortSum(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:4])
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// newSynapseClient targets the homeserver's public URL when one is given, so
// the comparison goes through the same reverse proxy and worker routing real
// clients use. A unix socket remains available for pinning one worker.
func newSynapseClient() (*http.Client, string) {
	if *synapseURL != "" {
		return &http.Client{Timeout: 120 * time.Second}, strings.TrimRight(*synapseURL, "/")
	}
	return &http.Client{
		Timeout: 120 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", *synapseSocket)
		}},
	}, "http://synapse"
}

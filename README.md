# synapse-media-worker

A Go worker that serves Synapse's media **read** paths — client downloads,
federation downloads and thumbnails — by reading Synapse's own Postgres
database and media store directly.

Synapse is single-threaded per process, so serving media means running several
Python workers whose job is mostly copying bytes. Synapse streams every
response body through a thread in 16 KB chunks and does not implement Range
requests at all. This worker replaces that part with one Go binary that can
sendfile, handle Range, and serve thousands of concurrent requests from one
process.

It is deliberately **not** a media repository. It never uploads, never writes to
the media store, and never implements an admin API. Anything it cannot answer is
proxied back to Synapse, which keeps the worker small and every fallback
correct.

## What it serves

| Method | Path |
|---|---|
| GET | `/_matrix/client/v1/media/download/{serverName}/{mediaId}[/{fileName}]` |
| GET | `/_matrix/client/v1/media/thumbnail/{serverName}/{mediaId}` |
| GET | `/_matrix/federation/v1/media/download/{mediaId}` |
| GET | `/_matrix/federation/v1/media/thumbnail/{mediaId}` |
| GET | `/_matrix/media/{r0,v1,v3}/download|thumbnail/...` (legacy, unauthenticated) |
| GET | `/health`, `/metrics` |

**Left to Synapse — do not route these here:** `/_matrix/media/v3/upload`,
`/_matrix/media/v1/create`, `/_matrix/client/v1/media/preview_url`,
`/_matrix/client/v1/media/config`, and every `/_synapse/admin/*` endpoint.

## How it decides

**Downloads.** Local media is read straight off disk. Remote media is served
from Synapse's cache when the row and file both exist; otherwise the request is
proxied so Synapse can do the federated fetch with its own rate limiting and
spam checks.

**Thumbnails**, in order:

1. A thumbnail Synapse already stored that matches the request exactly.
2. A thumbnail this worker generated earlier, from its own cache.
3. Freshly generated, using Synapse's exact resize arithmetic, and stored in the
   worker's own cache.
4. Proxied to Synapse — animated output, undecodable formats, oversized images.

Concurrent requests for the same thumbnail are collapsed with `singleflight`, so
a viral image is decoded once rather than once per waiting request.

## Authentication

**Client tokens** are validated by asking Synapse's `/account/whoami`, with the
verdict cached in memory (positive and negative, LRU-bounded, keyed by
`sha256(token)`, never persisted). Reading the `access_tokens` table directly
would be faster but wrong: appservice tokens live in registration files, and
delegated auth (MAS) keeps tokens outside Synapse entirely.

**Federation requests** are verified with `mautrix-go`'s `federation` package,
which parses the `X-Matrix` header, fetches and caches the origin server's
signing keys, checks their self-signatures and verifies the request signature.
The worker loads Synapse's `signing.key` file directly (including the multi-key
form left behind by a key rotation; the first key is used, as Synapse does).

## What it writes

Two things, and nothing else:

- Its **own cache directory**, for thumbnails it generates.
- `last_access_ts` in Synapse's database, batched and flushed once a minute,
  exactly as Synapse does. Without it, media retention and LRU cleanup would
  treat everything this worker serves as never accessed and eventually delete
  it. Disable with `database.update_last_access: false` if you do not run
  retention.

It never writes to Synapse's media store. Mount it `:ro` — the mount option is
the guarantee, not the code. The worker also refuses to start if its cache
directory is inside the media store.

## Deviations from Synapse

Everything else is byte-for-byte identical, verified by the parity harness.

- **Range requests are supported.** Synapse has no Range support at all. This is
  a superset, and it is where most of the performance win comes from.
- **Animated thumbnails are not generated.** `?animated=true` asks for WebP,
  which has no good pure-Go encoder, so those requests are proxied. On a real
  server they are well under 1% of stored thumbnails.

## Configuration

Copy `config.sample.yaml` and edit it. Values must agree with `homeserver.yaml`;
they are duplicated rather than parsed out of it so the worker has no coupling
to Synapse's config schema. Two in particular matter:

- `media.max_image_pixels` must match, or the worker and Synapse will disagree
  about which images are too large to thumbnail.
- `media.enable_authenticated_media` must match, or the legacy endpoints will
  expose media Synapse hides (or hide media Synapse serves).

Validate without starting up:

```sh
./synapse-media-worker -config config.yaml -check
```

## Deploying

A container image is published to the GitHub Container Registry on every push
to the default branch:

```
ghcr.io/ricardo-duarte-av/synapse-media-worker:latest
```

Copy `config.sample.yaml` to `config.yaml` and fill it in, create the cache
directory owned by the container's uid, then validate before starting:

```sh
sudo mkdir -p /share/aguiarvieira.pt/media_cache_go
sudo chown 991:991 /share/aguiarvieira.pt/media_cache_go
docker compose run --rm media-worker -check
docker compose up -d
```

`-check` verifies the media store is readable, the database is reachable, the
signing key parses and the upstreams are configured, then exits without
listening. It is the fastest way to catch a wrong path or credential.

Then point nginx at it. Define the upstream once:

```nginx
upstream av-media-worker-go {
    least_conn;
    server unix:/var/sockets/nginx/av-media-worker-go.sock max_fails=0 fail_timeout=0;
    keepalive 8;
}
```

and repoint only the download and thumbnail locations at it, leaving uploads and
the `/_synapse/admin/` blocks on the Python workers.

Keep the Python media workers running. They still handle uploads, URL previews
and admin, and they are the proxy target for the fallbacks above. Once the Go
worker is proven, scale them down rather than removing them.

### A note on caching proxies

If your reverse proxy caches media, make sure the cache key includes the
credential. A key of `$request_uri` alone will serve authenticated media to
unauthenticated callers once any user has warmed the cache, which defeats
authenticated media entirely. `$request_uri$http_authorization` caches per
token and keeps the check meaningful.

## Verifying

`tools/parity` is the acceptance test. It fetches the same media from this
worker and from a Synapse media worker over signed federation requests — no
client access token needed — and diffs the bytes and the headers clients act on.

```sh
go run ./tools/parity \
  -server-name example.com \
  -key /opt/matrix/synapse/synapse/example.com.signing.key \
  -go-url http://127.0.0.1:18090 \
  -synapse-socket /var/sockets/nginx/av-media-worker-1.sock \
  -db "postgres://synapse:PASSWORD@/synapse-db?host=/var/sockets&sslmode=disable" \
  -n 100
```

It exits non-zero on any mismatch. Downloads are compared byte for byte;
thumbnails are compared on decoded dimensions and headers, because Go's Lanczos
and Pillow's differ in the low bits by design.

Then watch `synapse_media_worker_thumbnail_outcome_total` in production: if
`proxied` is a large share, something is falling back more than it should.

## Logs

With `log.requests` on (the default) the worker writes one line per request
saying what it did with it, which is the part the reverse proxy's own access log
cannot see:

```json
{"level":"info","method":"GET","status":200,"bytes":109169,"duration":49.4,
 "ip":"198.51.100.7","endpoint":"client_thumbnail",
 "media":"mxc://example.com/abc123","thumb":"507x311/scale",
 "outcome":"generated","user":"@alice:example.com"}
```

`outcome` is one of:

| Outcome | Meaning |
|---|---|
| `served` | read straight from Synapse's media store |
| `synapse_store` | a thumbnail Synapse had already generated |
| `worker_cache` | a thumbnail this worker generated earlier |
| `generated` | generated during this request |
| `proxied` | handed back to Synapse; `reason` says why |
| `not_modified` | answered 304 |
| `not_found` | no such media, or quarantined (`reason`) |
| `unauthorized` | missing or rejected access token |

`ip` is taken from the leftmost `X-Forwarded-For` entry, so it is the real
client rather than the proxy. Requests are logged at info, 4xx at warn (except
404, which is routine for media) and 5xx at error, so raising the level to
`warn` keeps the problems and drops the noise.

To see how much work is being avoided:

```sh
docker compose logs media-worker | grep -o '"outcome":"[a-z_]*"' | sort | uniq -c
```

## Development

```sh
go test ./...              # unit tests, no server needed
go build -o synapse-media-worker .
```

Note `go build ./...` will not write the binary once there is more than one main
package in the module; use `-o` as above.

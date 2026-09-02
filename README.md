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

The one exception is `media.fetch_remote`, which lets the worker download
uncached remote media itself rather than handing it to Synapse. It is off by
default; see below.

## What it serves

| Method | Path |
|---|---|
| GET | `/_matrix/client/v1/media/download/{serverName}/{mediaId}[/{fileName}]` |
| GET | `/_matrix/client/v1/media/thumbnail/{serverName}/{mediaId}` |
| GET | `/_matrix/federation/v1/media/download/{mediaId}` |
| GET | `/_matrix/federation/v1/media/thumbnail/{mediaId}` |
| GET | `/_matrix/media/{r0,v1,v3}/download|thumbnail/...` (legacy, unauthenticated) |
| GET | `/_matrix/client/v1/media/config`, `/_matrix/media/{r0,v1,v3}/config` |
| GET | `/health`, `/metrics` |

With `accept_uploads` on it also serves `POST /_matrix/media/{r0,v1,v3}/upload`,
`POST /_matrix/media/v1/create` and `PUT /_matrix/media/v3/upload/{server}/{id}`.

`/media/config` reports `max_upload_size`, the same value uploads are checked
against, read from `homeserver.yaml`. If `homeserver.yaml` loads any `modules`
it is passed through instead: a module can register a
`get_media_config_for_user` callback and replace that response per user, and
the worker cannot run Synapse's modules.

Everything else on the media surface — `preview_url`, the media admin APIs — is
[passed through](#passing-through-what-it-does-not-implement) to Synapse, so you
can route the whole surface here.

## Passing through what it does not implement

The worker forwards, unexamined, anything under `/_matrix/media/`,
`/_matrix/client/v1/media/` and `/_matrix/federation/v1/media/` that it does not
handle itself, plus the media admin APIs. So nginx can send it every path
`docs/workers.md` assigns to a `synapse.app.media_repository` and the worker
sorts out which half it answers — you do not have to keep a routing table in
step with which endpoints it happens to implement.

The scope is deliberately not "everything that arrives". A blanket catch-all
would make this a general reverse proxy for whatever reached the socket, and the
`/_synapse/admin/` prefixes in particular carry plenty that has nothing to do
with media. So the admin APIs are matched against Synapse's own list —

```
^/_synapse/admin/v1/purge_media_cache$
^/_synapse/admin/v1/room/.*/media.*$
^/_synapse/admin/v1/user/.*/media.*$
^/_synapse/admin/v1/media/.*$
^/_synapse/admin/v1/quarantine_media/.*$
^/_synapse/admin/v1/users/.*/media$
```

— and anything else under those prefixes gets a 404 here rather than a free ride
to your admin API. `/_synapse/admin/v1/users/@a:example.com` is refused;
`/_synapse/admin/v1/users/@a:example.com/media` is forwarded.

Two things to get right in `upstream.passthrough`:

- **Quarantine needs a writer.** The quarantine and purge admin APIs must reach
  an instance configured as a `quarantined_media_changes` writer. Any media
  worker will do for downloads; not for these.
- **URL previews need `url_preview_enabled`** on the target worker.

If no passthrough upstream is configured the worker answers 404 for these paths,
which is honest — it does not serve them and cannot say who does. An upstream
pointing back at the worker's own listen socket is refused at startup, since
with a catch-all that turns a typo into a request loop.

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

## Fetching remote media (`fetch_remote`)

Off by default. When on, remote media Synapse has not cached is downloaded over
federation by the worker, written into the media store and inserted into
`remote_media_cache`, instead of being proxied. It is the one feature that
writes to state Synapse owns, so it is built to fail safe:

- **Any failure falls back to proxying**, exactly as with the flag off. A bug
  degrades to the previous behaviour rather than breaking media.
- **Only `remote_content/` and `remote_thumbnail/` are writable**, enforced in
  `paths.go` rather than by convention. `local_content`, `local_thumbnails` and
  `url_cache` cannot be written even by a buggy path calculation.
- The worker **fails at startup** if `fetch_remote` is on and the store is not
  writable, rather than at the first fetch.

Turning it on means dropping `:ro` from the media store mount in
`docker-compose.yaml`.

### Matching Synapse exactly

The row has to be indistinguishable from one Synapse wrote, because Synapse
keeps serving the same media:

| Column | Value |
|---|---|
| `filesystem_id` | 24 random characters from `A-Za-z` — Synapse's `random_string(24)`, **no digits** |
| `sha256` | lowercase hex of the raw bytes, hashed while streaming to disk |
| `authenticated` | from `enable_authenticated_media`, which must match `homeserver.yaml` |
| `created_ts`, `last_access_ts` | both now, never a timestamp from the origin |

Three things that matter more than they look:

**The file is written, fsynced and renamed into place before the row is
inserted.** A row whose file is missing is worse than no row: Synapse finds
nothing, falls through to its own download path, hits the unique constraint this
row created, re-reads it, and returns media info whose file still is not there —
a 404 loop that never self-heals.

**`sha256` is not optional.** Synapse's admin quarantine resolves media by hash
across the local and remote tables together, so a row stored without one is
invisible to it: an admin quarantining an image would silently fail to
quarantine this copy.

**The insert is `ON CONFLICT DO NOTHING`, never `DO UPDATE`.** Losing the race
means deleting our file and deferring to the winner's row, as Synapse does. A
Synapse worker mid-request is holding the `filesystem_id` it read earlier;
changing it underneath makes that worker look for a file that no longer exists,
and orphans the old one.

`last_access_ts` is set to now for the same reason Synapse does it: media
retention and `purge_media_cache` delete on that column, so a backdated value
invites the media to be deleted almost immediately.

### What it deliberately does not do

- **No thumbnails at fetch time.** Synapse pre-generates its default set on
  download, but under `dynamic_thumbnails: true` those are written as
  `image/jpeg`/`image/png` at post-aspect dimensions while clients request
  `image/png` at the requested dimensions — so they almost never match and are
  never served. Thumbnails stay on-demand. This does not cause Synapse to
  re-download: storing the original is what prevents that.
  See `write_through_thumbnails` below for where they are kept.
- **No per-IP byte ratelimiting** (`remote_media_download_per_second`).
  `max_upload_size` is enforced, which is what protects the disk.
- **No `prevent_media_downloads_from` or `federation_domain_whitelist`.** If you
  rely on either, leave `fetch_remote` off until they are implemented.
- **No spam-checker callbacks.** Synapse runs these post-download, pre-persist.
  If you have a media spam-checker module, this bypasses it.

### `write_through_thumbnails`

Requires `fetch_remote`. Off by default.

Thumbnails the worker generates normally live in its own cache, which is
LRU-evictable and invisible to Synapse. With this on, thumbnails for **remote**
media are written into Synapse's media store instead — under the original's
`filesystem_id`, with a row in `remote_media_cache_thumbnails` — so they are
permanent, the existing exact-match lookup finds them next time, and Synapse can
serve them too.

Synapse persists dynamically generated thumbnails the same way: a size a client
asks for once becomes a permanent row at the **requested** dimensions, not the
post-aspect ones. This makes the worker behave identically instead of keeping a
second, weaker copy.

Local media is deliberately excluded. `local_thumbnails/` stays unwritable, so
nothing the worker does can touch media this server owns.

Two details that matter:

- The row's `filesystem_id` must equal `remote_media_cache.filesystem_id`, since
  that is where Synapse looks for the file. The upsert therefore updates only
  `thumbnail_length` and never `filesystem_id`, matching Synapse's
  `insertion_values` semantics.
- `thumbnail_length` is taken from the file on disk after the rename, not from
  the buffer that produced it. Synapse sends that value as the `Content-Length`
  when it serves the thumbnail, so a row disagreeing with the file would
  truncate the response.

## Uploads (`accept_uploads`)

Off by default. When on, the worker handles `POST /_matrix/media/{r0,v1,v3}/upload`,
`POST /_matrix/media/v1/create` and `PUT /_matrix/media/v3/upload/{server}/{id}`
itself instead of proxying them.

Unlike remote fetching, **uploads do not fall back to Synapse.** Once the worker
has read a client's body there is no honest way to hand it on, and a fallback
would mean two systems could both believe they own a media ID. A failure is an
error the client can retry, not a silent hand-off. The invariant that matters is
that no failure leaves a file without its row: every path that fails after the
bytes are in place removes them first.

### Matching Synapse

| Column | Value |
|---|---|
| `media_id` | 24 chars of `A-Za-z`, and for local media this is also the file_id on disk |
| `sha256` | lowercase hex of the raw bytes, hashed while streaming |
| `authenticated` | from `enable_authenticated_media` |
| `media_type` | trusted verbatim from the request; missing → `application/octet-stream` |
| `upload_name` | from `?filename=`, stored unsanitised, as Synapse does |

Three behaviours worth knowing:

**Appservice masquerading is resolved by asking Synapse.** An appservice token
resolves to a different user depending on `?user_id=`, so that parameter (and
`?device_id=`) is forwarded to `/account/whoami` and the MXID it returns is what
lands in `user_id`. Trusting the parameter directly would let an appservice
token write media as any local user; the namespace check is the only thing
stopping that. The token cache is keyed on the token *and* the masquerade
parameters, since one token maps to many users.

**Hash quarantine is silent.** Content whose sha256 matches something already
quarantined is still stored and still answered `200`, with
`quarantined_by = 'system'` so it will not be served. That is Synapse's
behaviour, and not reproducing it would make this a way to re-upload
quarantined content. If the check itself errors the upload fails rather than
guessing — failing open would let the content through, and failing closed would
hand back a `200` for media that is silently unusable.

**Async completion needs no lock.** Synapse holds a cross-worker lock across the
whole PUT because its completing UPDATE is unconditional. This worker adds
`AND media_length IS NULL` instead, so a second writer affects zero rows and is
told `409 M_CANNOT_OVERWRITE_MEDIA` — the same status, from the database, with
no lock table or renewal to keep in sync. This requires that **all** PUTs route
to the worker: a Synapse worker handling one concurrently could still clobber it
with its unconditional UPDATE.

### No thumbnails at upload

`upload_thumbnails: none` by default. Synapse generates its default set on every
upload, but under `dynamic_thumbnails` those are written in a type no request
path can ask for — on one real server, 81% of local thumbnails (2 GB) are
unreachable — and only 18.7% of uploads are ever viewed at thumbnail size at
all. Thumbnails are still generated on demand, which is what serves every
request anyway.

Set `upload_thumbnails: synapse` for byte-parity if you need it. Note that with
`dynamic_thumbnails` off, Synapse selects a nearest match from stored
thumbnails, and media uploaded through this worker would have none to score.

### `proxy_failed_fetches`

Defaults to true: a remote fetch the worker could not complete is handed to
Synapse, which tries again.

That is the safe starting point, but it is usually worth turning off once
fetching is trusted. Federation is full of defunct servers, and whatever
stopped the worker reaching an origin — a DNS name that no longer resolves, a
TLS certificate for a host that is gone, a refused connection — stops Synapse
just the same. The second attempt reaches the same conclusion while the client
waits for both. On one real server, 11,881 proxied requests ended in `404`
that way, and unreachable origins outnumbered genuine 404s heavily.

With it off, failures are answered with the status Synapse itself produces:

| Failure | Answer |
|---|---|
| Origin returned 404 | the origin's own error |
| DNS, TLS, refused, timeout, 5xx | `502` "Failed to fetch remote media" |

The one case where the fallback still earns its keep is a difference between
the two federation clients — if this worker resolved a server differently to
Synapse, proxying would paper over it. That is worth ruling out with metrics
before switching: `remote_fetches_total{result="failed"}` against
`{result="fetched"}` tells you the rate, and the log line names the origin and
the exact error for each one.

## Concurrency

Every request runs in its own goroutine, so the worker serves as many at once
as the machine allows — that is the point of it, and the reason one process
replaces several single-threaded Python ones. Three things are deliberately
bounded:

| Bound | Setting | Why |
|---|---|---|
| Database connections | `database.max_conns` (16) | Queries are short and indexed; this is rarely the limit |
| Thumbnail generation | `media.max_concurrent_thumbnails` (CPU count) | CPU-bound, and holds the decoded bitmap in memory — hundreds of MB per image at a 100M pixel limit |
| Proxied requests | none | Bounded by the upstream Synapse workers, not here |

Concurrent requests for the *same* thumbnail are collapsed with `singleflight`,
so a viral image is decoded once no matter how many clients ask at once.

Serving an existing file is not bounded at all: it is a database lookup and a
`sendfile`, and the kernel does the copying.

### Upstreams

List every Synapse media worker under `upstream.download.sockets` and
`upstream.thumbnail.sockets`. Requests are spread by **least connections**
rather than round robin, because these responses vary enormously in cost — a
cached thumbnail returns at once while an uncached remote download blocks on a
federated fetch from another server, and round robin would keep handing work to
a worker already stuck on a slow transfer.

If an upstream cannot be reached, the request is retried against another. That
is safe only because nothing has been written to the client yet and a GET has
no body to replay; a real response, including a 5xx, is passed straight
through rather than retried.

`synapse_media_worker_upstream_inflight` and
`synapse_media_worker_upstream_requests_total` are labelled per upstream, so an
unbalanced pool or one sick worker shows up directly instead of as latency.

## Configuration

Copy `config.sample.yaml` and edit it.

Point `synapse_config` at Synapse's `homeserver.yaml` and most of it fills
itself in: `server_name`, `max_upload_size`, `max_image_pixels`,
`enable_authenticated_media`, `dynamic_thumbnails`, and the database
credentials. Anything set in the worker's own config wins.

This is worth doing, because Synapse's size suffixes are **binary**.
`max_image_pixels: 100M` is 104857600, not 100000000 — a difference this worker
had wrong by hand, which left a band of image sizes it refused to thumbnail
while Synapse accepted them.

Two caveats:

- **It contains every secret Synapse has** — `macaroon_secret_key`,
  `registration_shared_secret`, `form_secret`. The worker only reads a handful
  of media keys, but the whole file is exposed to the container. Mount a
  stripped copy if that matters. (The worker already holds database credentials
  and the signing key, so this is an increment rather than a new category.)
- **Paths in it are Synapse's, not yours.** `media_store_path` and
  `signing_key_path` are recorded as they appear inside Synapse's container. A
  path that does not resolve in the worker's container is skipped with a
  warning rather than adopted, so set those explicitly unless the layouts match.

Synapse's `database.args` often points at a connection pooler. The worker is a
single process and is usually better off connecting directly, so set
`database.uri` explicitly to do that.

The worker logs exactly what it took from Synapse at startup:

```
Derived settings from Synapse's configuration
  values=["server_name=example.com","media.max_upload_size=1048576000",
          "media.max_image_pixels=104857600","media.dynamic_thumbnails=true"]
```

It also warns about Synapse settings it cannot honour — `media_storage_providers`,
`prevent_media_downloads_from`, and `dynamic_thumbnails: false` (see below).

### Which media can be thumbnailed

The worker decodes exactly what Synapse decodes, which is a much shorter list
than the formats Matrix carries:

| Source | Thumbnail produced |
|---|---|
| `image/jpeg`, `image/jpg`, `image/webp` | `image/jpeg` |
| `image/png`, `image/gif` | `image/png` |
| everything else | none |

That is `THUMBNAIL_SUPPORTED_MEDIA_FORMAT_MAP` and `PILLOW_FORMATS` from
Synapse, matched deliberately. **Video is never thumbnailed** — not by Synapse
either — and neither are `image/svg+xml`, `image/avif`, `image/bmp`,
`image/heic` or `image/x-icon`. A thumbnail request for any of them gets
`400 M_UNKNOWN "Failed to generate thumbnail."`, which is what Synapse answers.
Verified against real media of each type on a live server.

Content-type parameters are stripped before the lookup, as Synapse does, so
media stored as `image/png; charset=binary` is still thumbnailed rather than
being treated as an unknown format.

Animated WebP is thumbnailed by extracting its first frame, since the Go
decoder handles still WebP but cannot walk animation frames. A `?animated=true`
request, which asks for an *animated* WebP thumbnail, is still proxied: that
needs a WebP encoder, and serving a static image instead would be a visible
quality regression against what Synapse produces.

### `dynamic_thumbnails: false` is not yet supported

The worker only implements the dynamic behaviour: exact match on width, height,
method and type, generating on a miss. With `dynamic_thumbnails` off Synapse
instead scores the stored thumbnails and serves the nearest, so the two will
disagree — the worker will generate where Synapse would have reused. It is
detected and warned about at startup rather than failing, since the result is
still a correct thumbnail, just not the same one.

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

and point the media locations at it. Because the worker passes through what it
does not implement, that can be the whole media surface rather than a
hand-maintained list of the endpoints it serves:

```nginx
location ~ ^/_matrix/(media|client/v1/media|federation/v1/media)/ {
    proxy_pass http://av-media-worker-go;
}
```

Keep at least one Python media worker running. It is the passthrough target for
URL previews and the media admin APIs, and the fallback target for anything the
Go worker declines. One is enough once the Go worker is
carrying the reads and uploads.

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
  -go-socket /var/sockets/nginx/av-media-worker-go.sock \
  -synapse-url https://example.com \
  -db "postgres://synapse:PASSWORD@/synapse-db?host=/var/sockets&sslmode=disable" \
  -n 100
```

Add `-mode client -token-file <path>` to compare the authenticated client
endpoints as well, which is the only way to reach remote media.

Use `-go-socket` rather than `-go-url` where the worker listens on a socket. A
unix peer address is not `host:port`, and that difference has already hidden a
header-forwarding bug from a TCP-only run: `net/http`'s `SetXForwarded` drops
`X-Forwarded-For` entirely on a unix listener, which made every uncached remote
media fetch fail. Test the transport you actually run.

It exits non-zero on any mismatch. Downloads are compared byte for byte;
thumbnails are compared on decoded dimensions and headers, because Go's Lanczos
and Pillow's differ in the low bits by design.

Then watch `synapse_media_worker_thumbnail_outcome_total` in production: if
`proxied` is a large share, something is falling back more than it should.

## Metrics and dashboard

`/metrics` is served on whatever the worker listens on, plus an optional
separate `listen.metrics_addr`. A Grafana dashboard and the scrape
configuration are in [`dashboards/`](dashboards/).

The usual deployment listens on a unix socket, which Prometheus cannot scrape,
so `metrics_addr` is what makes the dashboard usable. Keep that port internal:
`/metrics` is unauthenticated.

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

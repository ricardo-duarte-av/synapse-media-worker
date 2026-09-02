# synapse-media-worker

Go worker serving Synapse's media read paths by reading Synapse's Postgres
database and media store directly. See README.md for what it serves and why.

Deployment-specific notes, including how to run the live tests, are in
`.claude/deployment-notes.md` (gitignored).

## Ground rules

- **Parity is the contract.** Responses must be indistinguishable from
  Synapse's, and rows written must be indistinguishable from rows Synapse
  wrote. Anything that differs is a bug unless it is one of the deliberate
  deviations listed in README.md.
- **When behaviour is uncertain, proxy to Synapse** rather than approximating.
  A fallback is always correct; a guess is not.
- **Read the upstream source before changing behaviour.** Every subtlety in the
  list below was found by reading Synapse, and most of them were not guessable.
  Fetch from `raw.githubusercontent.com/element-hq/synapse/v<version>/...`.
- **Test the transport you actually run.** Production listens on a unix socket.
  A TCP-only test run has already hidden one bug (see below).

## What writes where

The worker reads Synapse's media store. It writes in exactly three places:

| What | Where | Gate |
|---|---|---|
| Generated thumbnails | the worker's own cache directory | always |
| `last_access_ts` | Synapse's database, batched every minute | `database.update_last_access` |
| Fetched remote media + its thumbnails | `remote_content/`, `remote_thumbnail/`, `remote_media_cache` | `media.fetch_remote`, `media.write_through_thumbnails` |

`paths.go` refuses any write outside `remote_content/` and `remote_thumbnail/`.
`local_content`, `local_thumbnails` and `url_cache` are unwritable in code, so
media the server owns cannot be damaged. Keep it that way unless deliberately
lifting it.

## Things that are easy to get wrong

Paths and layout:

- The remote thumbnail **directory** is `remote_thumbnail` (singular) while the
  table is `remote_media_cache_thumbnails` (plural). Local is
  `local_thumbnails` (plural).
- Remote thumbnails have a legacy filename with no `-<method>` suffix. Probe
  both before declaring a miss.
- `url_cache` media IDs beginning with a date split as `<id[:10]>/<id[11:]>` —
  the hyphen is dropped, not turned into a separator.

Serving:

- The ETag is the literal unquoted string `1` for every media item. Go's
  `ServeContent` will not match it, so the 304 check is done manually, and only
  after auth and quarantine.
- `Content-Disposition` carries `filename=` or `filename*=`, never both.
- The federation multipart body starts with a CRLF **before** the first
  boundary, and its JSON part is exactly `{}` with no `Content-Disposition`.
- Federation downloads still send `upload_name`, despite the servlet passing
  `name=None` — that only suppresses the URL-path override.
- Authenticated media makes every cross-origin browser request a CORS
  preflight. Answer `OPTIONS` or web clients on other origins see nothing.
- `/media/config` is one servlet under two path families, and **both
  authenticate** — including the `/_matrix/media` spelling, which is otherwise
  the unauthenticated family. Its body is canonical JSON: no spaces, no
  trailing newline, and `Cache-Control: no-cache, no-store, must-revalidate`
  from `respond_with_json`. A module can replace it per user via
  `get_media_config_for_user`, so the worker proxies it whenever
  `homeserver.yaml` loads any `modules` at all.
- Wrapping the `ResponseWriter` costs sendfile unless the wrapper forwards
  `ReadFrom`. Nothing fails; downloads just quietly get slower.

Thumbnails:

- With `dynamic_thumbnails: true`, matching is exact on width, height, method
  *and* type. There is no nearest-match fallback. **The worker only implements
  this mode**; with the setting off Synapse scores stored thumbnails and serves
  the nearest, and the two disagree. Detected and warned about at startup.
- `dynamic_thumbnails` gates only *serving*. Synapse pre-generates its default
  set at download time regardless — but as jpeg/png at post-aspect dimensions,
  while clients request png at the requested dimensions, so those pre-generated
  ones are almost never served.
- Synapse stores a *generated* thumbnail under the dimensions that were
  **requested**, and an upload-time one under its **post-aspect** dimensions.
  Both populations coexist.
- All resize arithmetic is integer floor division. Reproduce it exactly.
- Synapse serves a thumbnail using the **database's** `thumbnail_length` as the
  Content-Length. A row that disagrees with the file truncates the response.

Upload limits:

- `media_upload_limits` is enforced on **both** upload paths: sync and async go
  through the same `create_or_update_content` upstream.
- The limits are sorted **longest window first**, and Synapse carries the usage
  figure between iterations — a longer window's total is an over-count for a
  shorter one, so being under the ceiling settles both. Sorting differently
  changes which limit's `info_uri` the user is told about.
- The check runs after the body is read and before the row is written, which is
  where Synapse runs it. Refusing before reading the body would answer a client
  that is still sending, which reads as a broken connection, not a 403.
- Over quota is `403 M_USER_LIMIT_EXCEEDED`, and `can_upgrade` is **omitted**
  rather than `false` when unset. A limit with no `info_uri` gets Synapse's own
  fallback page, built from `public_baseurl` (which defaults to
  `https://<server_name>/` and is forced to end in a slash).
- The usage sum ignores `media_length IS NULL`, so reserved-but-unfilled async
  media IDs hold no quota.

Writing remote media:

- **The file must be fsynced and renamed into place before the row exists.** A
  row without its file is worse than no row: Synapse finds nothing, falls
  through to its download path, hits the unique constraint the row created,
  re-reads it, and returns media info whose file is still missing — a 404 loop
  that never self-heals.
- `filesystem_id` is 24 characters of `A-Za-z`, **no digits**
  (`random_string(24)` over `string.ascii_letters`).
- `sha256` is mandatory. Admin quarantine resolves media by hash across the
  local and remote tables, so a row without one is invisible to it.
- The media insert is `ON CONFLICT DO NOTHING`, never `DO UPDATE`. Losing means
  deleting your file and deferring to the winner: a Synapse worker mid-request
  holds the `filesystem_id` it read earlier.
- The thumbnail upsert updates **only** `thumbnail_length`, never
  `filesystem_id`, matching Synapse's `insertion_values` semantics.
- `created_ts` and `last_access_ts` are both *now*, never a timestamp from the
  origin: retention and `purge_media_cache` delete on `last_access_ts`.
- Do not touch `quarantined_by` or `quarantined_media_changes`. That stream is a
  `MultiWriterIdGenerator` keyed on `instance_name`; an outside writer corrupts
  its position tracking.
- Synapse's `Linearizer` is a per-process dict and gives no cross-worker
  safety. The unique constraint is the only real guard — Synapse's own workers
  already race each other.
- No cache invalidation is needed: there are no `@cached` methods on Synapse's
  media store and no media replication stream.

Configuration:

- Synapse's `parse_size` suffixes are **binary**. `100M` is 104857600, not
  100000000. Prefer `synapse_config` over transcribing values, which has drifted
  before.
- Paths in `homeserver.yaml` are as they appear inside *Synapse's* container.

Passthrough:

- The worker forwards the media surface it does not implement (see
  `passthrough.go`), so nginx can route it every path `docs/workers.md` gives a
  media worker. Keep that list in step with Synapse when it grows.
- The `/_synapse/admin/` prefixes carry plenty that is **not** media. Match the
  media admin paths, never forward those prefixes wholesale, or the worker
  becomes a general admin proxy.
- The quarantine and purge admin APIs need a `quarantined_media_changes`
  writer, and `preview_url` needs `url_preview_enabled`. The passthrough
  upstream is not interchangeable with any media worker.
- Registering a prefix that overlaps the worker's own patterns is a `ServeMux`
  panic at *registration*, i.e. at startup. `TestRoutesRegisterWithoutConflict`
  builds the real route table for that reason.

Libraries:

- `federation.Client.DownloadMedia` in mautrix v0.30.1 returns `nil, nil, nil`
  on error, swallowing it. Use `MakeFullRequest` and parse the multipart
  directly.
- `httputil.ReverseProxy` strips every `X-Forwarded-*` header when a `Rewrite`
  hook is set, and `ProxyRequest.SetXForwarded` *deletes* `X-Forwarded-For` when
  the peer is not `host:port` — which is every unix socket listener. Synapse
  500s on remote media without that header.

## Verification

`tools/parity` is the acceptance test: it fetches the same media from the
worker and from Synapse and diffs bytes and headers. Prefer `-go-socket` and
`-synapse-url` so both sides match production.

Live tests that need a real deployment are guarded by environment variables and
skip otherwise: `TestLiveFetchMatchesSynapse`,
`TestLiveWriteThroughAgainstRealSchema`. See `.claude/deployment-notes.md`.

Validate SQL against the real schema inside a transaction that is rolled back,
rather than writing to a live database. When a test must write, delete in a
`defer` ordered *before* the pool closes — `t.Cleanup` runs after `defer
db.Close()` and the deletes silently do nothing.

## Commands

```sh
go test -race ./...
go build -o synapse-media-worker .    # ./... will not write the binary
./synapse-media-worker -config config.yaml -check
```

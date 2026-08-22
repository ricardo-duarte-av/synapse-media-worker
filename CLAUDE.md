# synapse-media-worker

Go worker serving Synapse's media read paths by reading Synapse's Postgres
database and media store directly. See README.md for what it serves and why.

## Ground rules

- **Never write to Synapse's media store.** The worker reads it; generated
  thumbnails go in its own cache directory. `config.validate` refuses to start
  if the cache is nested inside the store, and the store is mounted `:ro`.
- The only write to Synapse's database is the batched `last_access_ts` update in
  `db.go`. Do not add others.
- When behaviour is uncertain, **proxy to Synapse** rather than approximating.
  A fallback is always correct; a guess is not.

## Parity is the contract

Responses must be indistinguishable from Synapse's. The two intentional
deviations are Range support and not generating animated thumbnails; both are
documented in README.md. Everything else that differs is a bug.

Before changing anything in `serve.go`, `multipart.go` or `thumbnail.go`, read
the corresponding upstream code — `synapse/media/_base.py`,
`synapse/media/media_storage.py`, `synapse/media/thumbnailer.py` — and run the
parity harness against a real Synapse afterwards. Unit tests alone will not
catch a header or framing difference.

## Things that are easy to get wrong

- The remote thumbnail **directory** is `remote_thumbnail` (singular) while the
  table is `remote_media_cache_thumbnails` (plural). Local is
  `local_thumbnails` (plural).
- Remote thumbnails have a legacy filename with no `-<method>` suffix. Probe
  both before declaring a miss.
- `url_cache` media IDs beginning with a date split as `<id[:10]>/<id[11:]>` —
  the hyphen is dropped, not turned into a separator.
- The ETag is the literal unquoted string `1` for every media item. Go's
  `ServeContent` will not match it, so the 304 check is done manually, and only
  after auth and quarantine.
- `Content-Disposition` carries `filename=` or `filename*=`, never both.
- The federation multipart body starts with a CRLF **before** the first
  boundary, and its JSON part is exactly `{}` with no `Content-Disposition`.
- With `dynamic_thumbnails: true`, matching is exact on width, height, method
  *and* type. There is no nearest-match fallback.
- Synapse stores a *generated* thumbnail under the dimensions that were
  **requested**, but an upload-time thumbnail under its **post-aspect**
  dimensions. Both populations coexist; the cache is keyed on the requested
  dimensions to stay consistent with the exact-match lookup.
- All thumbnail resize arithmetic uses integer floor division. Reproduce it
  exactly or dimensions drift by a pixel from Synapse's.

## Commands

```sh
go test ./...
go build -o synapse-media-worker .    # ./... will not write the binary
./synapse-media-worker -config config.yaml -check
```

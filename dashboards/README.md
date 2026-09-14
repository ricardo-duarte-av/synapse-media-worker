# Grafana dashboard

`synapse-media-worker.json` — import it, or drop it into a provisioned
dashboards directory.

It answers one question at a glance: how much work is the worker actually
taking off Synapse, and where is it handing work back?

## Wiring it up

The worker serves `/metrics` on whatever it listens on. In the usual
deployment that is a **unix socket, which Prometheus cannot scrape**, so give
it a TCP listener as well:

```yaml
listen:
  socket: /var/sockets/nginx/av-media-worker-go.sock
  metrics_addr: ":9110"      # add this
```

`/metrics` is unauthenticated, so keep that port on an internal network and do
not route it through the reverse proxy. Only `/_matrix/...` paths should be
publicly reachable.

Then scrape it. The container has to be on a network Prometheus can reach:

```yaml
  - job_name: synapse-media-worker
    metrics_path: /metrics
    static_configs:
      - targets: ["av-media-worker-go:9110"]
```

Import the dashboard and pick the Prometheus datasource when prompted. The
`job` and `instance` variables are populated from the data, so it works with
any job name.

## Buckets

The **Bucket** variable sets both the rate window and the step of every query,
so each point on a graph summarises exactly one bucket: pick `1h` and you see
hourly averages, `1d` for daily. The stat tiles show the latest bucket.

`auto` scales the bucket with the time range, but never below `1m`. That floor
is four scrapes at a 15s interval, the least a `rate()` needs to return
anything; if you scrape less often, raise `auto_min` and drop the `1m` option
in the variable settings, or short buckets will render empty.

Grafana caps how many points a panel draws, so a small bucket over a long
range is widened automatically. That is why `auto` is the sensible default.

## Reading it

**Thumbnail outcomes** is the panel that matters most:

| Outcome | Meaning |
|---|---|
| `synapse_store` | Synapse had already generated it — free |
| `worker_cache` | this worker generated it earlier — free |
| `generated` | decoded and resized during this request |
| `proxied` | the worker could not do it; Synapse was asked instead |

A healthy server is mostly the first two, with `generated` tapering off as the
caches warm. `generated` staying flat suggests cache eviction is too aggressive
for `cache.max_bytes`. A large `proxied` share is worth investigating unless the
reasons are `unsupported_format` or `animated`, which are expected.

**Federated fetches** shows media the worker downloaded itself rather than
handing to Synapse — the whole point of `fetch_remote`. `failed` falls back to
proxying, so it degrades rather than breaks, but a steady rate means some origin
is unreachable.

**Upstream in-flight** shows how hard the Synapse media workers are still being
worked. Persistently high means the pool is too small; an uneven split under
least-connections usually means one worker is slow, not that balancing is
broken.

**Uploads by endpoint and result** keeps two dimensions apart deliberately.
`sync`, `create` and `async` are endpoints; `stored`, `reserved`, `too_large`,
`limited`, `over_quota`, `forbidden`, `conflict`, `not_found`, `failed` and
`proxied` are outcomes. Most of the refusals are spec-defined and routine at
low rates, with three worth watching: a rising `limited` means some client is
reserving async media IDs via `/create` and never uploading to them, which will
lock it out for up to `unused_expiration_time`; `over_quota` means users are
hitting `media_upload_limits`; and `failed` should be zero, since everything
else has a defined status.

`proxied` simply means `accept_uploads` is off and Synapse is handling them.

**Response status**: 404 is routine for media. 401 means tokens are being
rejected; 503 means Synapse could not be reached to validate one, which is a
worker-to-Synapse problem rather than a client one.

Note that some series only appear once the corresponding path has run — a
worker that has never proxied anything emits no `proxied_total` at all, so an
empty panel can simply mean it has not happened.

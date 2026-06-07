<br />
<div align="center">
  <a href="https://github.com/Y0sh1dk/roadrunner-otter">
    <img src="docs/images/logo.svg" alt="roadrunner-otter" height="300">
  </a>

  <h3 align="center">roadrunner-otter</h3>

  <p align="center">
    <a href="https://github.com/maypok86/otter">Otter</a> HTTP middleware for RoadRunner.
    <br />
  </p>
</div>

# roadrunner-otter

[RoadRunner](https://roadrunner.dev) HTTP middleware that caches and coalesces
upstream responses in-process, backed by
[`maypok86/otter`](https://github.com/maypok86/otter).

**Use it** in front of hot, idempotent endpoints where the upstream cost is
real and the response is shareable across callers — feed pages, public
profiles, expensive read APIs.

**Don't use it** for streaming responses (the body is buffered fully),
per-user endpoints you can't scope by header, or anything mutating. The cache
is in-process: shared across PHP workers on a host, never across hosts, lost
on restart.

## Install

Build a custom RoadRunner binary via
[velox](https://docs.roadrunner.dev/customization/build):

```toml
# velox.toml
[github.plugins]
otter = { ref = "main", owner = "Y0sh1dk", repository = "roadrunner-otter" }
```

`ref` must exist on GitHub — velox fetches the plugin's `go.mod` at build
time. See [`examples/velox/`](./examples/velox/) for a complete Dockerfile.

## Configure

All configuration is per-path. Entries are scanned in order, first match
wins. Requests that don't match any entry pass straight through.

```yaml
http:
  address: 0.0.0.0:8080
  middleware: [otter]

otter:
  paths:
    # Carve-out: never cache health probes. Disabled entries skip every other field.
    - pattern: "^/api/health$"
      disabled: true

    # Public feed, 30s TTL. Defaults apply for everything else.
    - pattern: "^/api/v1/feed$"
      methods: [GET, HEAD]
      cache:
        ttl: 30s

    # Per-user lookup: split the key by Authorization so callers don't share responses.
    # `name` is used as the metrics `path` label — friendlier than the regex.
    - pattern: "^/api/v1/users/[^/]+$"
      name: users
      methods: [GET, HEAD]
      cache:
        ttl: 5m
        key:
          include_headers: [Authorization]

    # Large public artifact: longer TTL, smaller capacity, raised body cap, success-only.
    - pattern: "^/api/v1/exports/[0-9]+$"
      methods: [GET]
      cache:
        ttl: 1h
        max_entries: 500
        max_body_bytes: 67108864    # 64 MiB
        statuses: [200]
```

### Fields

| Path | Default | Notes |
|---|---|---|
| `pattern` | required | Go regex matched against `r.URL.Path` |
| `name` | `pattern` value | Friendly label for Prometheus `path=` (see Metrics) |
| `disabled` | `false` | Bypass; no other fields needed |
| `methods` | `[GET, HEAD]` | Uppercased on load; the default mirrors RFC 7231's safe+cacheable set |
| `cache.ttl` | required | `"5s"`, `"1m"`, `"100ms"` |
| `cache.max_entries` | `10000` | Per-path capacity; W-TinyLFU eviction |
| `cache.max_body_bytes` | `8388608` (8 MiB) | Larger responses → 502, never cached |
| `cache.statuses` | RFC 7234 cacheable set¹ | Statuses outside this set are served but not stored |
| `cache.key.include_headers` | `[]` | Header values that contribute to the cache key |

¹ `[200, 203, 204, 206, 300, 301, 308, 404, 405, 410, 414, 501]` — excludes
5xx and most 4xx so a flaky upstream doesn't pin an error response in cache.

The cache key is `METHOD<space>RequestURI[\x1fHeader=value ...]`. The query
is always part of the key (`/feed?page=1` and `/feed?page=2` don't collide).

## Metrics

When the RoadRunner [`metrics`](https://docs.roadrunner.dev/docs/lab/metrics)
plugin is enabled, otter auto-registers a Prometheus collector with it — no
extra wiring beyond the standard block:

```yaml
metrics:
  address: 0.0.0.0:8001
```

Per-path series (label `path` = `name` if set, otherwise the regex pattern):

```
otter_cache_hits_total{path}              # served from cache, PHP not touched
otter_cache_misses_total{path}            # cache lookup found nothing
otter_cache_load_successes_total{path}    # upstream load completed
otter_cache_load_failures_total{path}     # upstream load errored (size cap, etc.)
otter_cache_evictions_total{path}         # W-TinyLFU evictions
otter_cache_size{path}                    # approximate current entry count
```

Use a `name` on each path you care about — regex patterns make ugly Grafana
labels. Two active paths resolving to the same label is a startup error.

## Develop

```sh
task           # lint + test
task bench     # benchmarks
task rr:run    # build rr with this plugin and serve test/integration/.rr.yaml
```

See `Taskfile.yml` for the full list.

## License

[MIT](./LICENSE)

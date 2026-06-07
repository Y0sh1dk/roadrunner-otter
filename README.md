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

[RoadRunner](https://roadrunner.dev) HTTP middleware that caches and coalesces
upstream responses in-process via [`maypok86/otter`](https://github.com/maypok86/otter).
Use it in front of hot, idempotent endpoints to reduce latency by caching responses in-memory and reduce load by coalescing concurrent requests. Configurable per-path, with support for custom cache keys and Prometheus metrics.

- [Usage](#usage)
- [Configuration](#configuration)
  - [Cache Key](#cache-key)
- [Install](#install)
- [Metrics](#metrics)
- [Develop](#develop)
- [Benchmarks](#benchmarks)
- [License](#license)

## Usage

Each entry in `paths` is independent. Incoming requests are processed by the first entry whose `pattern` regex matches the request path. If that entry is `disabled`, the request bypasses the middleware entirely. Otherwise, if the method is allowed, otter attempts to serve from cache; on a miss, it loads from the upstream handler and stores the response according to the entry's `cache` config.

Requests that match no entry pass straight through.

```yaml
http:
  address: 0.0.0.0:8080
  middleware: [otter]

otter:
  paths:
    # Disabled entries don't need any other fields. Place them before
    # broader patterns that would otherwise swallow the request.
    - pattern: "^/api/non-idempotent-path$"
      disabled: true

    # Minimal config, Only required fields.
    - pattern: "^/api/v1/feed$"
      cache:
        ttl: 30s

    # Add `name` for a friendly Prometheus `path=` label, defaults to the regex pattern if absent.
    - pattern: "^/api/v1/users/[^/]+$"
      name: users
      cache:
        ttl: 5m

    # Also match `OPTIONS` ontop of default `GET` and `HEAD`.
    - pattern: "^/api/v1/search$"
      methods: [GET, HEAD, OPTIONS]
      cache:
        ttl: 1m

    # Add `Authorization` so that authenticated users get separate cache entries.
    - pattern: "^/api/v1/me$"
      name: me
      cache:
        ttl: 5m
        key:
          include_headers: [Authorization]


    # Only cache 200 responses from upstream.
    - pattern: "^/api/v1/strict$"
      cache:
        ttl: 30s
        statuses: [200]


    # Full example, including all configurable fields.
    - pattern: "^/api/v1/exports/[0-9]+$"
      methods: [GET]
      cache:
        ttl: 1h
        max_entries: 500
        max_body_bytes: 67108864    # 64 MiB
        statuses: [200]
        key:
          include_headers: [Authorization, Accept-Language]
```

## Configuration

Entries are scanned in order; first match wins. Requests that don't match any
entry pass straight through.

| Field | Default | Notes |
|---|---|---|
| `pattern` | required | Go regex matched against `r.URL.Path` |
| `cache.ttl` | required | `"5s"`, `"1m"`, `"100ms"` |
| `name` | `pattern` | Friendly Prometheus `path=` label |
| `disabled` | `false` | Bypass, no other fields needed. Used to ensure certain paths are not caught by broader patterns. |
| `methods` | `[GET, HEAD]` | [RFC7231](https://datatracker.ietf.org/doc/html/rfc7231#section-4.2.3) |
| `cache.max_entries` | `10000` | Per-path capacity; W-TinyLFU eviction |
| `cache.max_body_bytes` | `8388608` (8 MiB) | Larger responses → 502, never cached |
| `cache.statuses` | `[200, 203, 204, 206, 300, 301, 308, 404, 405, 410, 414, 501]` | [RFC7231](https://datatracker.ietf.org/doc/html/rfc7231#section-6.1) |
| `cache.key.include_headers` | `[]` | Headers to include in the cache key |

### Cache Key

By default, the cache key contains the request path, query string, and method. If `cache.key.include_headers` is set, the specified headers are also included in the key. Header names are canonicalized (e.g. `accept-language` → `Accept-Language`) but values are not modified. The key is built by concatenating these components, headers use unit separators (`\x1f`) to avoid ambiguity.

For example, with the following config:

```yaml
- pattern: "^/api/v1/me$"
  name: me
  cache:
    ttl: 5m
    key:
      include_headers: [Authorization]
```

A request like

```
GET /api/v1/me?verbose=true HTTP/1.1
Host: example.com
Authorization: Bearer abc123
```

would produce the following cache key:

```
GET /api/v1/me?verbose=true\x1fAuthorization=Bearer abc123
```

## Install
Build a custom RoadRunner binary with
[velox](https://docs.roadrunner.dev/customization/build):

```toml
# velox.toml
[github.plugins]
otter = { ref = "main", owner = "Y0sh1dk", repository = "roadrunner-otter" }
```

## Metrics

Enable RoadRunner's [metrics](https://docs.roadrunner.dev/docs/lab/metrics)
plugin and otter auto-registers a Prometheus collector — no extra wiring:

```yaml
metrics:
  address: 0.0.0.0:8001
```

Every series carries a single label, `path`, set to the path's `name`
(if configured) or its regex pattern.

| Metric | Type | Description |
|---|---|---|
| `otter_cache_hits_total` | counter | Cache lookups served from memory; the upstream PHP handler was not invoked. |
| `otter_cache_misses_total` | counter | Cache lookups that found no stored response and triggered an upstream load. |
| `otter_cache_load_successes_total` | counter | Upstream loads that completed and were stored (or kept ephemerally for in-flight coalescers). |
| `otter_cache_load_failures_total` | counter | Upstream loads that errored — exceeded `cache.max_body_bytes`, panicked, or returned a non-cacheable status routed via `errSkipCache`. |
| `otter_cache_evictions_total` | counter | Entries removed by otter's W-TinyLFU policy (not manual deletions). |
| `otter_cache_size` | gauge | Approximate number of entries currently held in the cache. |

Two active paths resolving to the same label is a startup error — set `name`
on each path you care about.

## Develop

```sh
task           # lint + test
task bench     # benchmarks
task rr:run    # build rr with the plugin baked in, serve test/integration/.rr.yaml
```

See [Taskfile.yml](./Taskfile.yml) for the full list.

## Benchmarks
*Coming soon!*

## License

[MIT](./LICENSE)

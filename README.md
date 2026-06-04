# roadrunner-otter

A [RoadRunner](https://roadrunner.dev) HTTP middleware that caches
upstream responses in-process using
[`maypok86/otter`](https://github.com/maypok86/otter) (a W-TinyLFU cache
with stampede protection).

Each path you configure gets its own cache. For a matching request:

1. **Cache hit** → return the buffered response without touching PHP.
2. **Cache miss** → invoke the upstream handler, buffer the response, and
   store it for the configured TTL (unless its status is outside
   `cache_statuses` — see below).
3. **Concurrent miss for the same key** → only one upstream call runs;
   the other callers block on the in-flight loader and receive the same
   buffered response (singleflight semantics, provided by otter's
   `LoaderFunc`).

This is what you want in front of hot, idempotent endpoints — popular
feed pages, public profile lookups, cache warmups — where the upstream
cost is real and the response is genuinely shareable across callers.

**Not a fit for:** streaming responses (SSE, long-poll — we buffer
fully), per-user endpoints you can't scope by header (cookies leak
through cache hits), or write endpoints. The cache lives in the `rr`
process — shared across all PHP workers on a host, but NOT shared
across hosts and lost on restart. Pair with a CDN or Redis-backed
cache if you need either.

## Installation

Build a custom RoadRunner binary that includes this plugin. See the
[RoadRunner custom-binary docs](https://docs.roadrunner.dev/docs/customization/build).

In your `velox.toml`:

```toml
[github.plugins]
otter = { ref = "main", owner = "Y0sh1dk", repository = "roadrunner-otter" }
```

Velox fetches the plugin's `go.mod` from GitHub before building, so the
referenced ref (`main` above, or a tagged release) must exist on the
remote.

See [`examples/velox/`](./examples/velox/) for a complete working
example, including a multi-stage `Dockerfile` and the matching plugin
versions for RoadRunner v2024.3.5.

## Configuration

All configuration is per-path. There are no global defaults at the
`otter:` level — each entry in `paths` is self-contained. Requests that
don't match any entry pass straight through to the upstream unchanged.

```yaml
version: "3"

http:
  address: 0.0.0.0:8080
  middleware: [otter]

otter:
  paths:
    # 1. Carve-out: never cache health probes. Must come before any
    #    broader entry that would otherwise catch them. Disabled entries
    #    don't need methods or cache_ttl.
    - pattern: "^/api/health$"
      disabled: true

    # 2. Public feed: cached for 30 seconds. max_body_bytes defaults to
    #    8 MiB; cache_max_entries defaults to 10000.
    - pattern: "^/api/v1/feed$"
      methods: [GET, HEAD]
      cache_ttl: 30s

    # 3. Per-tenant user lookup: split the key by Authorization so users
    #    don't share each other's cached response.
    - pattern: "^/api/v1/users/[^/]+$"
      methods: [GET, HEAD]
      cache_ttl: 5m
      key:
        include_headers: [Authorization]

    # 4. Large public artifact: longer TTL, smaller cache (bigger
    #    individual entries), raised body cap.
    - pattern: "^/api/v1/exports/[0-9]+$"
      methods: [GET]
      cache_ttl: 1h
      cache_max_entries: 500
      max_body_bytes: 67108864      # 64 MiB

    # 5. Catch-all (recreates the "apply to every request" behavior).
    #    Place it last so the more specific entries above win.
    # - pattern: "^.*$"
    #   methods: [GET, HEAD]
    #   cache_ttl: 5s
```

Per-entry fields and their defaults:

| Field | Default | Notes |
|---|---|---|
| `pattern` | *(required)* | Go regex matched against `r.URL.Path` |
| `disabled` | `false` | If true, matching requests bypass the middleware (and no other fields are needed) |
| `methods` | *(required)* | No default — startup fails if missing |
| `cache_ttl` | *(required)* | Go duration string (`"5s"`, `"1m"`, `"100ms"`). Startup fails if zero or missing |
| `cache_max_entries` | `10000` | Per-path cache capacity; W-TinyLFU eviction once exceeded |
| `cache_statuses` | RFC 7234 set (see below) | HTTP status codes whose responses get cached. Non-matching responses are served but not stored |
| `key.include_headers` | `[]` | Request headers whose values split the cache key |
| `max_body_bytes` | `8388608` (8 MiB) | Upstream responses larger than this return 502 to the originator and are not cached |

The default `cache_statuses` is the RFC 7234 §3 "cacheable by default" set:
`[200, 203, 204, 206, 300, 301, 308, 404, 405, 410, 414, 501]`. This
deliberately excludes 5xx (other than 501) and most 4xx — so a flaky
upstream returning 502s for a minute won't pin a stale error in the
cache for the full TTL. Override per path when you need it:

```yaml
- pattern: "^/api/strict$"
  methods: [GET]
  cache_ttl: 30s
  cache_statuses: [200]            # cache only success
- pattern: "^/api/lax$"
  methods: [GET]
  cache_ttl: 30s
  cache_statuses: [200, 404, 500]  # cache 500s too (rarely a good idea)
```

The cache key for a matched request is always:

```
METHOD + " " + r.URL.RequestURI() + "\x1fHeader=value" ...
```

`RequestURI()` includes the query string, so `/feed?page=1` and
`/feed?page=2` never collide.

**Semantics:**

- **First match wins.** Put narrower / `disabled: true` entries before
  broader ones.
- **No match** → request bypasses the middleware (passes through to
  upstream unchanged).
- **Empty `paths`** → middleware is a no-op (every request bypasses).
- **Patterns are Go regular expressions** matched against `r.URL.Path` (no
  scheme, host, or query). Invalid patterns fail at plugin startup.

## How it works

1. A request arrives. The `paths` list is scanned in order. If no entry
   matches, or the matching entry is `disabled`, or the request's method
   isn't in the entry's `methods` list, the request passes straight
   through to the upstream.
2. Otherwise the key is built from method + RequestURI + included
   headers, and the request enters `cache.Get(ctx, key, loader)` on the
   route's per-path otter cache.
3. **Cache hit** → the cached snapshot is replayed to the response
   writer; PHP is not touched.
4. **Cache miss** → the loader invokes the next handler against a
   buffering recorder. Concurrent misses for the same key share the
   single loader call (stampede protection).
5. **Status check** → if the buffered response's status is in
   `cache_statuses`, the snapshot is stored for `cache_ttl` and returned.
   Otherwise the snapshot is returned to the caller but NOT stored — the
   next request retries upstream.
6. **Loader error** (size cap hit, etc.) → caller gets `502 Bad Gateway`,
   nothing is stored, concurrent waiters all receive the same error.

## Caveats

- **Streaming responses don't stream**: the entire response is buffered
  before any caller sees a byte. Don't put this middleware in front of
  SSE/long-polling endpoints.
- **Cached responses are byte-identical**: per-caller headers like
  `Set-Cookie` will be replayed verbatim to every cache hit. Use
  `key.include_headers` (e.g. `[Cookie]` or `[Authorization]`) to scope
  the cache per user. If you can't scope cleanly, don't cache that path.
- **Error bodies are NOT cached by default**: responses with a status
  outside `cache_statuses` (5xx and most 4xx by default) get served to
  the caller but skip the cache — the next request retries upstream.
  Override `cache_statuses` per path if you have a different policy.
- **Cap your bodies**: `max_body_bytes` protects against OOM when an
  unexpectedly large upstream response would otherwise be buffered. The
  default of 8 MiB is conservative — raise it if your endpoints
  legitimately return more.

## Development

```bash
go test ./... -race -count=1
go vet ./...
golangci-lint run

# Benchmarks (key paths):
#   BenchmarkBypass             — overhead when method is not coalesced (~ns)
#   BenchmarkColdSerial         — middleware overhead with distinct keys (cache always misses)
#   BenchmarkParallelCoalesce_SlowUpstream
#                               — reports `upstream/op`: average upstream calls
#                                 per incoming request (lower = more coalescing/cache hits)
#   BenchmarkRouteLookup        — cost of the per-request `paths` table scan
#                                 (worst case: matches the last entry)
go test -run='^$' -bench=. -benchmem ./...
```

## License

[MIT](./LICENSE)

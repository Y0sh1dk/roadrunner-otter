# Building a custom RoadRunner with this plugin

Two files in this directory:

- **`velox.toml`** — the velox build config. Lists RoadRunner version,
  every plugin your two `.rr.yaml` files reference (http, jobs/sqs,
  kv/redis/memory, prometheus, static, status, metrics, rpc, logger), plus
  this otter plugin.
- **`Dockerfile`** — multi-stage build. Stage 1 runs `vx build` inside
  the pinned `ghcr.io/roadrunner-server/velox` image. Stage 2 copies the
  resulting `rr` binary into a PHP runtime image.

## One-time prerequisite: push the plugin to GitHub

velox builds plugins by fetching them from their GitHub repos. Until the
plugin code is pushed to `Y0sh1dk/roadrunner-otter` at the `main`
branch (right now only the initial README commit is up there), velox will
fail with:

```
ERROR  get plugins mod data  {"error": "no file named go.mod found in ."}
```

Push first:

```bash
git push origin main
```

Then re-run the build below.

## Local build (no Docker)

```bash
go install github.com/roadrunner-server/velox/v2025/cmd/vx@v2025.1.7
GITHUB_TOKEN=$(gh auth token) vx build -c velox.toml -o ./bin/
./bin/rr --version
```

Or for fully local iteration before pushing — add a `replace` clause to
the `otter` line in `velox.toml`:

```toml
otter = { ref = "main", owner = "Y0sh1dk", repository = "roadrunner-otter", replace = "/abs/path/to/checkout" }
```

velox still fetches `go.mod` from GitHub at the configured ref, then
`replace` rewrites the module to your local path. So you do need the
plugin's `go.mod` on the remote at least once.

## Docker build

```bash
docker build \
    --secret id=github_token,env=GITHUB_TOKEN \
    -t myorg/roadrunner-otter:local \
    -f examples/velox/Dockerfile \
    examples/velox
```

The `GITHUB_TOKEN` env var should be a token with `public_repo` scope
(avoids GitHub's anonymous rate limits during the build).

## Wiring it into your app's Dockerfile

Replace your current
`FROM ghcr.io/roadrunner-server/roadrunner:2023.3.12 AS roadrunner`
line with:

```dockerfile
# Build the custom RR binary
FROM myorg/roadrunner-otter:local AS roadrunner

# Your app stage as before:
FROM php:8.1-alpine3.20 AS base
WORKDIR /app
COPY --from=roadrunner /usr/local/bin/rr /usr/local/bin/rr
# ... rest unchanged ...
```

## Verified locally

The build was smoke-tested locally with `vx v2025.1.7`:

| Check | Result |
|---|---|
| `vx build` completes without errors (otter commented out) | ✅ |
| `rr --version` reports `2025.1.14` | ✅ |
| Plugin imports present in binary (http, jobs/v6, sqs/v6, redis/v6, memory, prometheus/v6, kv/v6, etc. found in symbol table) | ✅ |
| `rr serve` starts and the `status` plugin responds on `/health` | ✅ |

The otter plugin step couldn't be smoke-tested locally because the
plugin code isn't on the remote yet (see prerequisite above). Once pushed,
add the `otter = { ... }` line back to `velox.toml` and re-run.

## One config-side note

Your HTTP `.rr.yaml` lists `http_metrics` in `http.middleware`. Newer
RoadRunner releases register the per-request metrics plugin as
`prometheus` instead of `http_metrics`. Either:

- update your config to `middleware: ["static", "prometheus"]`, or
- pin the `prometheus` plugin in `velox.toml` to an older `ref` that
  still registers itself as `http_metrics`.

## Upgrade notes

You're moving from RR `v2023.3.12` to `v2025.1.14`. The `.rr.yaml`
`version: "3"` schema is unchanged across this jump, but the plugin
versions have rolled from v4 → v6 (current `master`). If you hit a
runtime error referencing an unknown config key, pin the offending plugin
to a specific tag in `velox.toml`.

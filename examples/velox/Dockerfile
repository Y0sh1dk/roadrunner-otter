# syntax=docker/dockerfile:1.7
#
# Multi-stage build that produces a custom RoadRunner binary with the
# singleflight plugin baked in, then layers it onto a PHP image suitable
# for your app workers.
#
# Build:
#   docker build \
#     --secret id=github_token,env=GITHUB_TOKEN \
#     -t myorg/roadrunner-singleflight:local \
#     -f examples/velox/Dockerfile \
#     examples/velox
#
# (GITHUB_TOKEN avoids GitHub's anonymous rate limit during the build.
# For public plugins you can omit the secret — the build may still hit
# rate limits if you rebuild a lot.)

ARG TARGETPLATFORM
ARG PHP_TAG=8.1-alpine3.20
# Pinned because the velox v2025.2 beta introduces a different velox.toml
# schema (`[plugins.NAME]` instead of `[github.plugins]`). Bump deliberately
# and verify the velox.toml still matches.
ARG VELOX_TAG=v2025.1.7

# --- Stage 1: build the rr binary with velox ---
FROM --platform=${TARGETPLATFORM:-linux/amd64} ghcr.io/roadrunner-server/velox:${VELOX_TAG} AS velox

WORKDIR /build

COPY velox.toml ./velox.toml

# RR is built without CGO so it stays statically linked and portable.
ENV CGO_ENABLED=0

# Use a build secret for the GitHub token so it doesn't get baked into a layer.
# vx reads $GITHUB_TOKEN from the environment via the ${GITHUB_TOKEN} expansion
# in velox.toml's [github.token] block.
RUN --mount=type=secret,id=github_token,target=/run/secrets/github_token,required=false \
    set -eu; \
    if [ -s /run/secrets/github_token ]; then \
        export GITHUB_TOKEN="$(cat /run/secrets/github_token)"; \
    fi; \
    vx build -c velox.toml -o /usr/local/bin/

# Sanity check: the binary exists and reports its plugins.
RUN /usr/local/bin/rr --version

# --- Stage 2: PHP runtime image with the custom rr binary copied in ---
FROM --platform=${TARGETPLATFORM:-linux/amd64} php:${PHP_TAG} AS runtime

# Drop the prebuilt binary alongside other system binaries.
COPY --from=velox /usr/local/bin/rr /usr/local/bin/rr

# Verify the binary runs in the runtime image.
RUN rr --version

WORKDIR /app

# Replace this with your application copy / install steps. Example:
#   COPY composer.json composer.lock ./
#   RUN composer install --no-dev --optimize-autoloader
#   COPY . .
#
# Then point rr at your .rr.yaml at container start:
#   CMD ["rr", "serve", "-c", ".rr.yaml"]

# syntax=docker/dockerfile:1.7


ARG TARGETPLATFORM
ARG PHP_TAG=8.3-cli
ARG VELOX_TAG=2025.1.7

# --- Stage 1: build the rr binary with velox --------------------------------
FROM --platform=${TARGETPLATFORM:-linux/amd64} ghcr.io/roadrunner-server/velox:${VELOX_TAG} AS velox

WORKDIR /build

COPY velox.toml ./velox.toml

ENV CGO_ENABLED=0

RUN --mount=type=secret,id=github_token,target=/run/secrets/github_token,required=false \
    set -eu; \
    if [ -s /run/secrets/github_token ]; then \
        export GITHUB_TOKEN="$(cat /run/secrets/github_token)"; \
    fi; \
    vx build -c velox.toml -o /usr/local/bin/

RUN /usr/local/bin/rr --version

# --- Stage 2: PHP runtime + composer + worker -------------------------------
FROM --platform=${TARGETPLATFORM:-linux/amd64} php:${PHP_TAG} AS runtime

# composer is needed to install spiral/roadrunner; sockets is needed by the
# Goridge protocol the PHP worker speaks back to rr over.
RUN apt-get update \
 && apt-get install -y --no-install-recommends git unzip ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && docker-php-ext-install sockets \
 && curl -sSf https://getcomposer.org/installer | php -- --install-dir=/usr/local/bin --filename=composer

COPY --from=velox /usr/local/bin/rr /usr/local/bin/rr
RUN rr --version

WORKDIR /app

COPY composer.json ./
RUN composer install --no-dev --no-interaction --no-progress --optimize-autoloader

COPY .rr.yaml worker.php ./

EXPOSE 8080

CMD ["rr", "serve", "-c", ".rr.yaml"]

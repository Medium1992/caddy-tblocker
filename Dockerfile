# syntax=docker/dockerfile:1

ARG CADDY_DOCKER_TAG=2
ARG GO_VERSION=1
# Left empty on purpose: an "unknown" default would be baked into `caddy
# version` and into the image labels on any build that does not pass them.
ARG CADDY_VERSION=
ARG CUSTOM_VERSION=
ARG VCS_REF=
ARG BUILD_DATE=

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS builder

ARG CADDY_VERSION
ARG CUSTOM_VERSION
ARG VCS_REF
ARG BUILD_DATE
ARG TARGETOS
ARG TARGETARCH

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH GOFLAGS=-mod=readonly go build -trimpath -buildvcs=false \
    -ldflags="-s -w -X github.com/caddyserver/caddy/v2.CustomVersion=${CUSTOM_VERSION}" \
    -o /usr/bin/caddy ./cmd/caddy

FROM caddy:${CADDY_DOCKER_TAG}
ARG CADDY_VERSION
ARG CUSTOM_VERSION
ARG VCS_REF
ARG BUILD_DATE

# The base image ships its own OCI labels pointing at caddyserver/caddy-docker.
# They have to be overridden here: GHCR links a published package to a
# repository through org.opencontainers.image.source.
LABEL org.opencontainers.image.title="caddy-tblocker" \
      org.opencontainers.image.description="Caddy with the Remnawave torrent-blocker module" \
      org.opencontainers.image.source="https://github.com/Medium1992/caddy-tblocker" \
      org.opencontainers.image.url="https://github.com/Medium1992/caddy-tblocker" \
      org.opencontainers.image.documentation="https://github.com/Medium1992/caddy-tblocker#readme" \
      org.opencontainers.image.vendor="Medium1992" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${CADDY_VERSION}" \
      org.opencontainers.image.revision="${VCS_REF}" \
      org.opencontainers.image.created="${BUILD_DATE}"

COPY --from=builder /usr/bin/caddy /usr/bin/caddy

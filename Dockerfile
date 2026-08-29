## syntax=docker/dockerfile:1

ARG CADDY_DOCKER_TAG=2
ARG GO_VERSION=1
ARG CADDY_VERSION=unknown
ARG CUSTOM_VERSION=unknown
ARG VCS_REF=unknown
ARG BUILD_DATE=unknown

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

LABEL org.opencontainers.image.title="caddy-tblocker" \
      org.opencontainers.image.description="Caddy with the Remna torrent-blocker module" \
      org.opencontainers.image.version="${CADDY_VERSION}" \
      org.opencontainers.image.revision="${VCS_REF}" \
      org.opencontainers.image.created="${BUILD_DATE}"

COPY --from=builder /usr/bin/caddy /usr/bin/caddy

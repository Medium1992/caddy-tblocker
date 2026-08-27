ARG CADDY_DOCKER_TAG=2
ARG CADDY_VERSION=unknown
ARG CUSTOM_VERSION=unknown
ARG VCS_REF=unknown
ARG BUILD_DATE=unknown

FROM golang:1 AS builder

ARG CADDY_VERSION
ARG CUSTOM_VERSION
ARG VCS_REF
ARG BUILD_DATE

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOFLAGS=-mod=readonly go build -trimpath -buildvcs=false \
    -ldflags="-s -w -X github.com/caddyserver/caddy/v2.CustomVersion=${CUSTOM_VERSION}" \
    -o /usr/bin/caddy ./cmd/caddy \
    && printf 'CADDY_VERSION=%s\nCUSTOM_VERSION=%s\nGO_VERSION=%s\nVCS_REF=%s\nBUILD_DATE=%s\n' \
      "$CADDY_VERSION" "$CUSTOM_VERSION" "$(go version)" "$VCS_REF" "$BUILD_DATE" > /VERSION

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
COPY --from=builder /VERSION /usr/share/caddy-tblocker/VERSION

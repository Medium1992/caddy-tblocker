FROM caddy:2-builder AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /usr/bin/caddy ./cmd/caddy

FROM caddy:2
COPY --from=builder /usr/bin/caddy /usr/bin/caddy

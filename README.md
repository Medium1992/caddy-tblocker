# caddy-tblocker

> Custom [Caddy](https://caddyserver.com/) image with a native, in-memory TTL blocklist for the [Remna](https://github.com/remnawave) torrent-blocker webhook. It blocks a reported client IP at the HTTP edge before the request reaches Xray.

[English](README.md) | [Русский](README_RU.md) | [Telegram](https://t.me/+96HVPF3Ww6o3YTNi)

[![Docker Pulls](https://img.shields.io/docker/pulls/medium1992/caddy-tblocker?logo=docker&label=docker%20pulls)](https://hub.docker.com/r/medium1992/caddy-tblocker)
[![Docker Image Size](https://img.shields.io/docker/image-size/medium1992/caddy-tblocker/latest?logo=docker&label=image%20size)](https://hub.docker.com/r/medium1992/caddy-tblocker)
[![Caddy](https://img.shields.io/github/v/release/caddyserver/caddy?label=Caddy&logo=caddy)](https://github.com/caddyserver/caddy/releases)
![Platforms](https://img.shields.io/badge/arch-amd64%20%7C%20arm64-blue)
[![Telegram](https://img.shields.io/badge/Telegram-group-blue?logo=telegram)](https://t.me/+96HVPF3Ww6o3YTNi)

## ✨ Features

- 🧱 **Native Caddy module**: no database, Redis, sidecar, or external API on the request path.
- ⏱️ **Temporary IP bans**: accepts Remna's `willUnblockAt` or `blockDuration` and removes expired entries lazily.
- 🌐 **Correct client address**: checks Caddy's trusted-proxy-aware `{client_ip}`, not an untrusted request header.
- 🔒 **Internal webhook listener**: restrict the sender by Docker subnet and keep port `9080` unpublished.
- ⚡ **Early rejection**: bans are evaluated before the Xray reverse proxy.
- 🐳 **Ready-made images**: multi-architecture `amd64` and `arm64` images for Docker Hub and GHCR.
- 🔄 **Upstream tracking**: scheduled workflow rebuilds when Caddy or the Go toolchain changes.

> [!IMPORTANT]
> Bans live only in Caddy memory. A Caddy restart or reload clears them intentionally. This keeps a bad webhook or configuration from producing a persistent lockout.

## 🚀 Quick Start

```bash
docker pull medium1992/caddy-tblocker:latest
```

Use the image instead of stock `caddy` in Compose:

```yaml
services:
  caddy:
    image: medium1992/caddy-tblocker:latest
    restart: unless-stopped
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro
      - caddy_data:/data
      - caddy_config:/config
    # Do not publish 9080. Remna reaches it only inside this network.
    ports:
      - "443:443"
```

Images:

```text
medium1992/caddy-tblocker:latest
medium1992/caddy-tblocker:vX.Y.Z
ghcr.io/medium1992/caddy-tblocker:latest
ghcr.io/medium1992/caddy-tblocker:vX.Y.Z
```

## ⚙️ How It Works

1. Remna POSTs a torrent-blocker report to the internal `tblocker_webhook` endpoint.
2. The webhook stores the IP and expiration timestamp in Caddy memory.
3. `tblocker` compares Caddy's `{client_ip}` with that store.
4. A matching request is answered with `403` before it reaches Xray or another upstream.

The current Remna payload is accepted as-is:

```json
{
  "actionReport": {
    "ip": "203.0.113.42",
    "blockDuration": 60,
    "willUnblockAt": "2026-08-28T12:01:00.000Z"
  }
}
```

`willUnblockAt` has priority. If it is absent, `blockDuration` is interpreted as seconds. `default_ttl` is used only when neither field is available; `max_ttl` caps any supplied expiration.

> [!NOTE]
> Current Remna nodes emit torrent reports only when their nftables service is available. Keep the node's required nft capability: Caddy becomes the client-facing ban layer after the webhook is emitted.

## 🧩 Caddyfile

`route` is intentional: it preserves the declared directive order, ensuring the ban check always runs before terminal handlers such as `reverse_proxy`.

```caddy
{
  tblocker {
    default_ttl 1m
    max_ttl 24h
  }

  # Trust only real CDN ingress ranges here.
  servers :443 {
    trusted_proxies static 198.51.100.0/24 2001:db8:1234::/48
    client_ip_headers X-Real-IP
  }
}

# Docker-network only. Do not publish port 9080 to the Internet.
http://:9080 {
  @remna_webhook path /internal/tblocker/replace-with-a-long-random-secret
  handle @remna_webhook {
    tblocker_webhook {
      allow 192.168.243.0/28
      max_body 64KB
    }
  }

  respond 404
}

example.com {
  route {
    # Must be first: checks the trusted client IP before Xray.
    tblocker {
      status 403
    }

    reverse_proxy 192.168.243.3:10000
  }
}
```

Set Remna's torrent-blocker webhook URL to:

```text
http://caddy:9080/internal/tblocker/replace-with-a-long-random-secret
```

Use your Compose service name instead of `caddy`. If containers communicate through fixed addresses rather than Docker DNS, use Caddy's fixed internal IP instead.

## 🔐 Real Client IP

The module does **not** trust raw `X-Real-IP` or `X-Forwarded-For` by itself. It receives Caddy's resolved client address, so `trusted_proxies` and `client_ip_headers` must be configured on the listener receiving CDN traffic.

- Replace the example CIDRs with the CDN's documented ingress ranges.
- Firewall the origin so only that CDN can reach the public listener.
- Never configure a public listener with `trusted_proxies static 0.0.0.0/0`; a direct caller could forge the header.
- A separate CDN-only port protected by firewall may have its own trusted-proxy policy.

Without this, Caddy sees the CDN edge IP and a Remna report for the subscriber IP cannot match.

## 🛠️ Directives

### Global app

```caddy
tblocker {
  default_ttl <Go duration>
  max_ttl <Go duration>
}
```

| Option | Default | Description |
|---|---:|---|
| `default_ttl` | `1m` | Fallback expiry when the webhook supplies neither valid expiry field. |
| `max_ttl` | `24h` | Maximum accepted lifetime for a ban. |

### Request handler

```caddy
tblocker {
  status 403
}
```

| Option | Default | Description |
|---|---:|---|
| `status` | `403` | HTTP response for a blocked client. Must be a 4xx status. |

### Webhook handler

```caddy
tblocker_webhook {
  allow <CIDR> [<CIDR>...]
  max_body <bytes>
}
```

| Option | Default | Description |
|---|---:|---|
| `allow` | required | CIDRs permitted to call the webhook. |
| `max_body` | `64KB` | Maximum accepted webhook request body. |

Successful webhook requests return `204 No Content`. Malformed payloads return `400`; callers outside `allow` receive `403`.

## 🏗️ Local Build

```bash
docker build -t caddy-tblocker:local .
docker run --rm caddy-tblocker:local caddy version
```

The image retains standard Caddy modules and adds:

```text
http.handlers.tblocker
http.handlers.tblocker_webhook
```

## 🔄 Automatic Builds

The GitHub Actions workflow can be run manually and checks upstream once a day. A manual run always builds; the scheduled run publishes only when Caddy or the Go toolchain changes. Images are built natively for `amd64` and `arm64`, then published to Docker Hub and GHCR.

For Docker Hub publishing, add `DOCKERHUB_USERNAME` and `DOCKERHUB_TOKEN` as repository secrets. Set GitHub Actions **Workflow permissions** to **Read and write** so scheduled builds can update the tracked upstream version file.

## ⭐ Support

If the project was useful, give it a [star](https://github.com/Medium1992/caddy-tblocker/stargazers) and join the [Telegram group](https://t.me/+96HVPF3Ww6o3YTNi).

[English](README.md) | [Русский](README_RU.md) | [Telegram](https://t.me/+96HVPF3Ww6o3YTNi)

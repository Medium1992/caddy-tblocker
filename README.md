# caddy-tblocker

`caddy-tblocker` is a custom Caddy module for the Remna torrent-blocker webhook.
It keeps temporary bans in Caddy memory and rejects requests by Caddy's trusted-proxy-aware `client_ip` before they reach Xray.

It has no database and no additional service. A Caddy reload clears active bans; this is deliberate for short TTLs.

## What it accepts

The module accepts the JSON already sent by current Remna nodes:

```json
{
  "actionReport": {
    "ip": "203.0.113.42",
    "blockDuration": 60,
    "willUnblockAt": "2026-08-28T12:01:00.000Z"
  }
}
```

`willUnblockAt` is preferred. If it is absent, `blockDuration` is treated as seconds. `max_ttl` caps either value.

## Build

```bash
docker build -t caddy-tblocker:local .
```

The resulting image contains the standard Caddy modules plus `http.handlers.tblocker` and `http.handlers.tblocker_webhook`.
`caddy version` reports both the upstream Caddy version and the custom build suffix.

## Automatic upstream builds

The GitHub workflow runs daily at 03:17 UTC and can be launched manually. It
checks Caddy's latest GitHub release, updates `go.mod`, runs tests, builds with
the latest stable Go, and publishes multi-architecture images to both registries:

```text
ghcr.io/medium1992/caddy-tblocker:latest
ghcr.io/medium1992/caddy-tblocker:vX.Y.Z
medium1992/caddy-tblocker:latest
medium1992/caddy-tblocker:vX.Y.Z
```

`VERSIONS` tracks both the upstream Caddy release and exact Go toolchain. A
scheduled build runs when either changes. The Docker build cross-compiles the
static Go binary natively for `amd64` and `arm64`, rather than compiling through
QEMU. It updates the tracked `VERSIONS` file only after a successful image build. In
repository Actions settings, set **Workflow permissions** to **Read and write**
so the scheduled workflow can commit that update and publish to GHCR. Add the
repository secrets `DOCKERHUB_USERNAME` and `DOCKERHUB_TOKEN` for Docker Hub.

## Caddyfile

The webhook listener must stay inside the Docker network. Do not publish port `9080` to the Internet. The unguessable path is the authentication boundary available to the current Remna webhook client; `allow` additionally restricts callers to the Docker subnet.

```caddy
{
  tblocker {
    default_ttl 1m
    max_ttl 24h
  }
}

# Internal-only listener. Do not publish 9080 in Docker.
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
  # This must run before reverse_proxy/Xray.
  tblocker {
    status 403
  }

  reverse_proxy 127.0.0.1:10000
}
```

Set the Remna `torrentBlocker.webhookUrl` to:

```text
http://caddy:9080/internal/tblocker/replace-with-a-long-random-secret
```

Use the actual Compose service name instead of `caddy` if it differs.

Current Remna releases only activate torrent detection when their nftables
service is available. With the stock node, keep its required nft capability so
it emits the webhook; the Caddy block remains the effective client-facing ban.
For a strictly Caddy-only deployment, the node needs a small follow-up change
to configure Xray's torrent webhook even when nftables is unavailable.

## Real client IP is mandatory

`tblocker` never trusts raw `X-Real-IP` or `X-Forwarded-For` itself. It checks Caddy's `{client_ip}`, so configure Caddy's `trusted_proxies` and `client_ip_headers` with the CDN's genuine ingress ranges. Without that, Caddy sees the CDN edge address and the ban cannot match the reported subscriber address.

Example shape only; replace the CIDRs with the ranges of your CDN:

```caddy
{
  servers {
    trusted_proxies static 203.0.113.0/24 2001:db8:1234::/48
    trusted_proxies_strict
    client_ip_headers X-Real-IP
  }
}
```

The origin must also be firewall-restricted to the CDN so a direct caller cannot supply a forged `X-Real-IP`.

## Caddyfile directives

Global app:

```caddy
tblocker {
  default_ttl <Go duration>
  max_ttl <Go duration>
}
```

Request handler:

```caddy
tblocker {
  status 403
}
```

Webhook handler:

```caddy
tblocker_webhook {
  allow <CIDR> [<CIDR>...]
  max_body <bytes>
}
```

`allow` is required. The default webhook body limit is 64 KiB.

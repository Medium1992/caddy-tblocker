# caddy-tblocker

> Custom [Caddy](https://caddyserver.com/) image with a native, in-memory TTL blocklist for the [Remnawave](https://github.com/remnawave) torrent-blocker webhook. Behind a CDN the node's own nftables ban is useless, because every packet the node sees comes from Caddy; this module moves the ban to the one place that still knows the real client address — and can tear the live tunnel down with it.

[English](README.md) | [Русский](README_RU.md) | [Telegram](https://t.me/+96HVPF3Ww6o3YTNi)

[![Docker Pulls](https://img.shields.io/docker/pulls/medium1992/caddy-tblocker?logo=docker&label=docker%20pulls)](https://hub.docker.com/r/medium1992/caddy-tblocker)
[![Docker Image Size](https://img.shields.io/docker/image-size/medium1992/caddy-tblocker/latest?logo=docker&label=image%20size)](https://hub.docker.com/r/medium1992/caddy-tblocker)
[![Caddy](https://img.shields.io/github/v/release/caddyserver/caddy?label=Caddy&logo=caddy)](https://github.com/caddyserver/caddy/releases)
![Platforms](https://img.shields.io/badge/arch-amd64%20%7C%20arm64-blue)
[![Telegram](https://img.shields.io/badge/Telegram-group-blue?logo=telegram)](https://t.me/+96HVPF3Ww6o3YTNi)

## ✨ Features

- 🧱 **Native Caddy module**: no database, Redis, sidecar, or external API on the request path.
- ⏱️ **Temporary IP bans**: driven by the report's `blockDuration`, capped by `max_ttl`, swept in the background.
- ✂️ **Tears down live tunnels**: with `drop_existing`, a ban cancels the requests already in flight instead of waiting for the client to reconnect.
- 🌐 **One address end to end**: Caddy's resolved `{client_ip}` is what it forwards to Xray and what it later compares against.
- 🔒 **Internal listener**: the webhook and admin routes are bound to loopback and restricted by source CIDR.
- ⚡ **Checked first**: the directive is ordered ahead of every terminal handler, so no `route` wrapper is needed.
- 🛟 **Safety net**: an `ignore` list that can never be banned, plus a bounded store.
- 🔓 **Manual release**: list and lift bans over an internal admin route.
- 🐳 **Ready-made images**: `amd64` and `arm64` for Docker Hub and GHCR.

> [!IMPORTANT]
> Bans live only in Caddy's memory. A restart or a config reload clears them, on purpose: a bad webhook or a wrong configuration can never produce a lockout that outlives the process.

## 🚀 Quick Start

```bash
docker pull medium1992/caddy-tblocker:latest
```

```text
medium1992/caddy-tblocker:latest
medium1992/caddy-tblocker:vX.Y.Z
ghcr.io/medium1992/caddy-tblocker:latest
ghcr.io/medium1992/caddy-tblocker:vX.Y.Z
```

It is a drop-in replacement for the stock `caddy` image — same config paths, same volumes, plus four extra modules.

## ⚙️ How It Works

1. The CDN forwards the request to Caddy and puts the real client address in a header such as `X-Real-IP`.
2. Caddy resolves `{client_ip}` from that header — but only because the TCP peer matches `trusted_proxies`.
3. Caddy forwards that same `{client_ip}` to Xray as `X-Forwarded-For`, together with the marker header Xray is configured to trust.
4. Xray uses that address as the connection source. When the bittorrent routing rule fires, the node reports it.
5. The node POSTs the report to Caddy's internal `tblocker_webhook` route.
6. `tblocker` stores the address, drops whatever that client has running, and answers `403` to everything it sends next.

The report the node sends is accepted as-is:

```json
{
  "actionReport": {
    "blocked": true,
    "ip": "203.0.113.42",
    "blockDuration": 3600,
    "willUnblockAt": "2026-08-28T13:00:00.000Z",
    "userId": "user@example.test"
  },
  "xrayReport": { "source": "203.0.113.42:0", "email": "user@example.test" }
}
```

`blockDuration` (seconds) takes priority. `willUnblockAt` is only a fallback, because it carries the node's wall clock: if the two containers disagree, a relative duration still produces the right ban while an absolute timestamp would shift it or expire it on arrival. `default_ttl` applies when neither field is usable, and `max_ttl` caps whatever is accepted.

### What a ban actually stops

Every ban refuses the client's **next** request immediately. What happens to the session already running depends on `drop_existing`:

**Without `drop_existing`** the established tunnel is left alone, and how soon the ban bites depends entirely on the transport:

| Xray transport | When the ban bites |
|---|---|
| XHTTP `packet-up` | **Immediately.** Every uplink chunk is its own POST, and each one is refused. |
| XHTTP `stream-up` / `stream-one` | On reconnect. The session is a single long-lived request that already passed the check. |
| WebSocket, HTTPUpgrade | On reconnect, for the same reason. |
| gRPC | On reconnect. |

**With `drop_existing`** the ban also cancels every request that client currently has open, on all of those transports. Caddy closes the upstream connection when a request context is done — including for hijacked WebSocket and HTTPUpgrade streams — so the tunnel dies at once instead of running until the client feels like reconnecting.

Cancellation is per request, not per TCP connection. That distinction matters behind a CDN, where a single HTTP/2 connection to the origin can carry several unrelated subscribers: closing the socket would take all of them down, cancelling the request takes down only the offender's streams.

> [!WARNING]
> The whole scheme depends on Xray reading `X-Forwarded-For`, and Xray only does that for HTTP-based transports (`websocket`, `httpupgrade`, `splithttp`/XHTTP, and `grpc`). With a raw TCP + TLS inbound there is no header to read, Xray reports the address of whoever opened the TCP connection — Caddy — and the report is worthless.

## 🐳 Deployment

### Host networking — recommended

Remnawave's own documentation runs the node with [`network_mode: host`](https://docs.rw/install/remnawave-node/), which it needs for its nftables plugins. Put Caddy in the same network namespace and the whole thing collapses to loopback: no user-defined network, no static addresses, and the internal listener is unreachable from outside the machine by construction.

```yaml
services:
  remnanode:
    container_name: remnanode
    hostname: remnanode
    image: remnawave/node:latest
    restart: always
    network_mode: host
    # The torrent-blocker plugin only activates when nftables is available.
    cap_add:
      - NET_ADMIN
    environment:
      - NODE_PORT=2222
      - SECRET_KEY=supersecretkey

  caddy:
    container_name: caddy
    hostname: caddy
    image: medium1992/caddy-tblocker:latest
    restart: always
    network_mode: host
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro
      - caddy-data:/data
      - caddy-config:/config

volumes:
  caddy-data:
  caddy-config:
```

With host networking there are no `ports:` to publish and nothing to forget: Caddy binds `:443` on the host, the internal listener binds `127.0.0.1:9080` (see the `bind` directive in the Caddyfile below), and the node reaches it at `http://127.0.0.1:9080/…`.

### Shared bridge network

If you already run the node inside a user-defined network, the same setup works with container addresses instead of loopback. Substitute throughout: bind the internal listener to Caddy's address on that network, set `allow` to the network's CIDR, and point `webhookUrl` at Caddy's container address.

```yaml
services:
  caddy:
    image: medium1992/caddy-tblocker:latest
    restart: unless-stopped
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro
      - caddy-data:/data
      - caddy-config:/config
    ports:
      - "443:443"
    # Never publish 9080: the node reaches it over this network only.
    networks:
      remnawave-network:
        ipv4_address: 192.168.243.2

  remnanode:
    image: remnawave/node:latest
    cap_add:
      - NET_ADMIN
    networks:
      remnawave-network:
        ipv4_address: 192.168.243.3

networks:
  remnawave-network:
    external: true

volumes:
  caddy-data:
  caddy-config:
```

> [!CAUTION]
> On a bridge network the internal listener is reachable by every other container attached to it, and `allow` is your only guard. On host networking it is bound to loopback and reachable only from the machine itself. Prefer host networking unless you have a reason not to.

## 🧩 Caddyfile

Written for host networking; for a bridge network replace the loopback addresses as described above.

```caddy
{
	tblocker {
		default_ttl 1m
		max_ttl 24h

		# Never ban infrastructure. On host networking Xray's fallback address
		# is loopback, so protecting it keeps a malformed report from taking
		# the origin down.
		ignore 127.0.0.0/8
	}

	# Trust only the CDN's documented ingress ranges.
	servers :443 {
		trusted_proxies static 198.51.100.0/24 2001:db8:1234::/48
		client_ip_headers X-Real-IP
	}
}

# `bind` is what actually restricts the socket to loopback. A site address of
# `http://127.0.0.1:9080` alone only adds a Host matcher: Caddy would still
# listen on every interface.
http://127.0.0.1:9080 {
	bind 127.0.0.1

	@node_webhook path /internal/tblocker/replace-with-a-long-random-secret
	handle @node_webhook {
		tblocker_webhook {
			allow 127.0.0.0/8
			max_body 64KiB
		}
	}

	@tblocker_admin path /internal/tblocker/admin
	handle @tblocker_admin {
		tblocker_admin {
			allow 127.0.0.0/8
		}
	}

	respond 404

	# The secret path would otherwise be written to the access log verbatim.
	log {
		output discard
	}
}

example.com {
	# No `route` wrapper: the directive is already ordered ahead of every
	# handler that can end the chain, including `handle` and `respond`.
	tblocker {
		status 403
		drop_existing
	}

	reverse_proxy 127.0.0.1:10000 {
		# The marker that lets Xray trust the forwarded address.
		header_up X-Trusted-Proxy "caddy"
		# Send exactly the address tblocker will check later.
		header_up X-Forwarded-For {client_ip}
	}
}
```

> [!TIP]
> Earlier versions of this README wrapped the site in `route { ... }`. That is no longer necessary — `tblocker` is registered to run before `redir`, which puts it ahead of `handle`, `handle_path`, `route`, `respond`, `error`, `abort` and `reverse_proxy`. An existing `route` block keeps working unchanged.

## 🔌 Node Plugin and Xray

`webhookUrl` in the torrent-blocker plugin is available from **node v3.1.0**. This is the complete `torrentBlocker` block for the **node plugin configuration** — it belongs in the node's plugins, not in the Caddyfile and not in a client profile:

```json
{
  "torrentBlocker": {
    "enabled": true,
    "webhookUrl": "http://127.0.0.1:9080/internal/tblocker/replace-with-a-long-random-secret",
    "ignoreLists": {
      "ip": ["127.0.0.1"],
      "userId": []
    },
    "blockDuration": 3600
  }
}
```

| Field | What to set |
|---|---|
| `enabled` | `true` enables torrent detection and the node's Xray routing rule. |
| `webhookUrl` | Caddy's internal URL. On host networking that is `127.0.0.1:9080`; on a bridge network, Caddy's container address. The random path must match the Caddyfile matcher exactly. |
| `blockDuration` | Block duration in **seconds**; `3600` is one hour. Use a positive value. `0` means a permanent nftables ban on the node, which an in-memory store cannot express, so Caddy falls back to `default_ttl`. |
| `ignoreLists.ip` | Addresses that must never be blocked. **Put Caddy's own address here** — `127.0.0.1` on host networking. Entries are matched as exact strings, so a CIDR will never match; list the address itself. |
| `ignoreLists.userId` | Xray user identifiers (the `email` field of the inbound user) that must never be blocked. |

> [!NOTE]
> The plugin only activates when the node's nftables service is available, so the node container needs `CAP_NET_ADMIN`. Without it no report is produced at all and Caddy never hears about anything.

Note that it is the **node** that calls the webhook, directly, the moment its Xray fires the rule. The panel is not involved: it neither sends this report nor learns about the ban Caddy stored.

In the Xray **inbound** that accepts traffic from Caddy, declare the marker header:

```json
{
  "streamSettings": {
    "sockopt": {
      "trustedXForwardedFor": ["X-Trusted-Proxy"]
    }
  }
}
```

Xray accepts `X-Forwarded-For` only when one of the header names listed in `trustedXForwardedFor` is also present. Caddy sends `X-Trusted-Proxy: caddy`, so `X-Trusted-Proxy` has to appear in that list. The marker name is arbitrary, but the two sides must agree. This is separate from the plugin configuration: it is what makes the source address Xray reports equal to the real client address that Caddy will later check.

## 🔐 Real Client IP

`{client_ip}` is Caddy's own resolved client address. It is not a header, and it is not the TCP peer address Xray sees. Caddy takes it from the CDN header **only after** the TCP peer matches `trusted_proxies`; otherwise it is the peer address itself.

- Replace the example CIDRs with the CDN's documented ingress ranges.
- Firewall the origin so only the CDN can reach the public listener.
- Never use `trusted_proxies static 0.0.0.0/0` on a public listener: a direct caller could then forge the header.

> [!CAUTION]
> Forward `{client_ip}`, not a raw header. `header_up X-Forwarded-For {http.request.header.X-Real-Ip}` looks equivalent but is not: it copies an unvalidated header with no trust check at all. Anyone who reaches the origin directly can then set `X-Real-IP` to any address they like and have it reported as the offender — getting a third party banned while staying invisible themselves. If the header is simply missing, Caddy sets an empty `X-Forwarded-For`, Xray falls back to the TCP peer, and the node ends up reporting **Caddy's own address**, which its nftables ingress filter will then block. `{client_ip}` cannot do either of those things.

Without `trusted_proxies` configured at all, `{client_ip}` is the CDN edge address: every subscriber behind that edge shares one value, and a single report would block all of them.

## 🔓 Releasing a Ban

The panel's own "unblock" command goes to the node's nftables endpoint and never reaches Caddy, so a ban has to be lifted here. The `tblocker_admin` route does that, from the machine itself:

```bash
# List every live ban
curl http://127.0.0.1:9080/internal/tblocker/admin
# {"count":1,"bans":[{"ip":"203.0.113.42","expires_at":"2026-08-28T13:00:00Z"}]}

# Release one address
curl -X DELETE 'http://127.0.0.1:9080/internal/tblocker/admin?ip=203.0.113.42'
# {"removed":1}

# Clear the whole blocklist
curl -X DELETE http://127.0.0.1:9080/internal/tblocker/admin
# {"removed":3}
```

Releasing an address that has no live ban returns `404`; an unparsable address returns `400`. Restarting Caddy also clears everything.

## 🛠️ Directives

### Global app

```caddy
tblocker {
	default_ttl    <duration>
	max_ttl        <duration>
	sweep_interval <duration>
	max_entries    <int>
	ipv4_prefix    <1-32>
	ipv6_prefix    <1-128>
	ignore         <CIDR> [<CIDR>...]
}
```

| Option | Default | Description |
|---|---:|---|
| `default_ttl` | `1m` | Ban lifetime when the report carries no usable duration. |
| `max_ttl` | `24h` | Upper bound on any accepted ban. |
| `sweep_interval` | `1m` | How often expired entries are purged in the background. A banned address usually stops connecting, so without this its entry would linger until the next reload. |
| `max_entries` | `100000` | Store bound. Once full, expired entries are reclaimed first; if it is still full the report is logged and dropped. |
| `ipv4_prefix` | `32` | Ban width for IPv4. `24` bans the whole `/24`. |
| `ipv6_prefix` | `128` | Ban width for IPv6. `64` is the useful value if you serve IPv6 clients, since a single host rotates its address within its `/64`. |
| `ignore` | none | CIDRs that are never banned and never blocked. |

### Request handler

```caddy
tblocker {
	status 403
	drop_existing
}
```

| Option | Default | Description |
|---|---:|---|
| `status` | `403` | Response for a blocked client. Must be `400`–`599`; anything else is rejected when the config is loaded. |
| `drop_existing` | off | Cancel the requests already in flight for an address when it gets banned. Without it an established tunnel keeps running until the client reconnects. Accepts a bare flag, or `on` / `off`. |

A request whose `{client_ip}` is empty or unparsable is passed through rather than blocked.

### Webhook handler

```caddy
tblocker_webhook {
	allow    <CIDR> [<CIDR>...]
	max_body <size>
}
```

| Option | Default | Description |
|---|---:|---|
| `allow` | required | CIDRs permitted to submit reports, matched against the real TCP peer — never against a forwarded header. |
| `max_body` | `64KiB` | Maximum accepted request body. |

A stored report returns `204`. A report for an `ignore`d address also returns `204`, is logged, and stores nothing. Malformed payloads return `400`, and callers outside `allow` get `403`.

### Admin handler

```caddy
tblocker_admin {
	allow <CIDR> [<CIDR>...]
}
```

| Method | Effect |
|---|---|
| `GET` | Returns `{"count":N,"bans":[{"ip":…,"expires_at":…}]}`. |
| `DELETE ?ip=<addr>` | Releases one address. `200` if it was live, `404` if not. |
| `DELETE` | Clears the blocklist and returns how many live entries were removed. |

Protect this route the same way as the webhook: an unguessable path, a loopback bind, and an `allow` list.

## 🔎 Verifying the Chain

If bans never appear, find out which link is broken before changing anything:

1. **Does the node see the real client address?** Its log line reads `[TORRENT-BLOCKER] IP: <addr>, user: …`. If `<addr>` is Caddy's address, then `trustedXForwardedFor` or `header_up` is wrong — fix that first, everything downstream depends on it.
2. **Does the report arrive?** Caddy logs `torrent client temporarily blocked` with `client_ip`, `expires_at` and `dropped_requests`. Nothing there means the webhook URL, the secret path, or the `allow` list is wrong. The node ignores webhook errors silently, so it will not tell you.
3. **Is `drop_existing` doing anything?** `dropped_requests` in that same line is how many live requests were torn down. A steady `0` while sessions are clearly open means the ban is landing on a different address than the one Caddy resolves — go back to step 1.
4. **Does Caddy resolve the same address?** Compare the ban listed by `tblocker_admin` with `{client_ip}` in your access logs. If they differ, `trusted_proxies` / `client_ip_headers` do not match what the CDN actually sends.

## 🏗️ Local Build

```bash
docker build -t caddy-tblocker:local .
docker run --rm caddy-tblocker:local caddy version
```

The image keeps the standard Caddy modules and adds:

```text
tblocker
http.handlers.tblocker
http.handlers.tblocker_webhook
http.handlers.tblocker_admin
```

## 🔄 Automatic Builds

The workflow runs on demand and checks upstream once a day. A manual run always builds; the scheduled run publishes only when the Caddy release or the Go toolchain changes. The Go binary is cross-compiled for `amd64` and `arm64`, and the images are published to Docker Hub and GHCR.

For Docker Hub publishing add `DOCKERHUB_USERNAME` and `DOCKERHUB_TOKEN` as repository secrets, and set GitHub Actions **Workflow permissions** to **Read and write** so scheduled builds can update the tracked upstream version file.

## ⭐ Support

If the project was useful, give it a [star](https://github.com/Medium1992/caddy-tblocker/stargazers) and join the [Telegram group](https://t.me/+96HVPF3Ww6o3YTNi).

[English](README.md) | [Русский](README_RU.md) | [Telegram](https://t.me/+96HVPF3Ww6o3YTNi)

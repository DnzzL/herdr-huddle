# 001 — Doors for the live stream

Date: 2026-09-22. Question: how does `herdr-huddle serve`'s stream reach a collaborator
over the internet with the least friction, and which transport must carry it? Written to
decide ADR-006 (the door), which revises ADR-005's recommendation of Tailscale.

Method: primary sources only — vendor docs, first-party blog posts, and the `cloudflared`
source itself. A background agent was not available in this harness, so the passes were
run inline. Secondary write-ups were used only to locate primaries and are not cited as
authority. Nothing here was measured by us; measured items are in
## Not answerable from docs.

## The two candidates

### Cloudflare quick tunnels

- **No account, no zone, no DNS.** "Use TryCloudflare … to experiment with Cloudflare
  Tunnel without adding a site to Cloudflare's DNS. TryCloudflare will launch a process
  that generates a random subdomain on `trycloudflare.com`." The blog adds that the
  release "does not require any onboarding to Cloudflare" and is "powered by Cloudflare
  Workers", with `cloudflared` making an **outbound-only** connection.
  <https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/do-more-with-tunnels/trycloudflare/>,
  <https://blog.cloudflare.com/quick-tunnels-anytime-anywhere/>
- **The command:** `cloudflared tunnel --url http://localhost:8080` → prints a public URL
  for traffic "from the server on your local machine to the public Internet".
- **The origin is HTTP.** The docs describe pointing it at "your web server running on
  localhost". Raw TCP is not offered: `cloudflared`'s L4/TCP capability (`cloudflared access`)
  is for tunnels *you* protect, a different feature (README). **Consequence: a quick tunnel
  cannot carry our raw-TCP stream as built — the transport must change.**
- **WebSocket is proxied by cloudflared itself.** From the source:
  `proxy/proxy.go` — "ProxyHTTPRequest proxies requests of underlying type http and
  websocket to the origin service"; it sets `Connection: Upgrade` / `Upgrade: websocket`.
  `connection/http2.go` — `WebsocketUpgrade = "websocket"`. The tunnel's own carrier *is*
  WebSocket (`carrier/carrier.go`). Cloudflare's network docs state WebSocket is supported
  "without additional configuration".
  <https://developers.cloudflare.com/network/websockets/>,
  <https://github.com/cloudflare/cloudflared>
- **No documented fair-use limits.** The Quick Tunnels page states use cases only; no
  throttling, lifetime or abuse limits are published there. Absence of a statement is not
  a guarantee.
- The subdomain is generated *when the process connects*, so a restart gives a new URL
  (inferred from the wording; not stated as a rule).

### Tailscale

- **Two ways to let someone in**: invite a user (an invited user "can access any device or
  service in your tailnet by default", restricted only by ACLs) or **share one device**
  ("the people you shared the device with" only). <https://tailscale.com/docs/reference/inviting-vs-sharing>
- **GitHub is a first-party IdP**: <https://tailscale.com/kb/1013/sso-providers> →
  <https://tailscale.com/docs/integrations/identity/github>
- **Funnel is available for all plans and is in beta** (page last validated 2026-01-20):
  <https://tailscale.com/docs/features/tailscale-funnel>
- **Funnel is public and needs no Tailscale client on the guest**: share "a local service …
  for anyone to access — even if they don't use Tailscale". Traffic goes to a Funnel relay,
  which opens a TCP proxy to your machine over Tailscale; **the relay cannot decrypt it**,
  and your device's IP is never exposed. Same page.
- **Funnel's published limitations** (same page):
  - DNS names only within `*.ts.net`
  - listens only on ports **443, 8443, 10000**
  - **TLS only**
  - **non-configurable bandwidth limits**
  - the same port cannot be `serve` and `funnel` at once (last command wins, public vs private)
  - certificate requests can trip Let's Encrypt rate limits (a 34-hour wait)
- **Funnel has a raw-TCP mode**: `tailscale funnel --tcp=<port>` ("Expose a TCP forwarder"),
  alongside `--https=<port>` and `--tls-terminated-tcp=<port>`.
  <https://tailscale.com/docs/reference/tailscale-cli/funnel> — allowed TCP ports for Funnel
  are 443/8443/10000 (<https://github.com/tailscale/tailscale/issues/14625>, quoting the docs).
  **Consequence: a public endpoint for our stream without a WebSocket rewrite** — but the
  public side is TLS-terminated, so a client must TLS-dial rather than `net.Dial`.

### Pairing (the identity half) — GitHub device flow

Source: <https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/authorizing-oauth-apps#device-flow>

- Device flow is the RFC 8628 Device Authorization Grant, explicitly "for headless
  applications, such as a CLI tool".
- **It must be enabled in the app's settings** (our OAuth app already has it — `auth login`
  works — but the gate reuses the same prerequisite).
- Flow: `POST https://github.com/login/device/code` (`client_id`) → the human enters the code
  at `https://github.com/login/device` → poll `POST https://github.com/login/oauth/access_token`
  with `device_code`, honoring the returned minimum interval (rate limits are documented).
- **The code expires in 15 minutes / 900 seconds.**
- Both web and device flows support expiring tokens and refresh tokens (`offline_access`
  scope; 8-hour access token, 6-month refresh token).

## Comparison

| | Quick tunnel | Tailscale Funnel `--tcp` | Tailscale device share | LAN |
|---|---|---|---|---|
| Guest installs | our binary only | our binary only | Tailscale client + our binary | our binary only |
| Operator installs | `cloudflared` (nixpkgs) | Tailscale (free account) | Tailscale | none |
| Account anywhere | none | free Tailscale | free Tailscale | none |
| Our code change | **transport rewrite** (WS or chunked — unvalidated) | **TLS dial in `join`** (server unchanged) | none (raw TCP) | none |
| Endpoint | ephemeral `*.trycloudflare.com` | stable `*.ts.net` | tailnet IP | LAN IP |
| Exposed to the public internet | yes | yes | no | no |
| Caveats | undocumented limits/lifetime | beta; ports 443/8443/10000; bandwidth limits | guest friction | same network only |

Because **both public doors are public**, the GitHub pairing gate is mandatory in either
case: Cloudflare Access cannot be placed in front of a quick tunnel (it needs your own
zone), and Funnel is public by definition.

## Not answerable from docs

- **Does a quick tunnel flush small chunked responses promptly?** No primary source exists.
  The "~100 KB before flush" claim I repeated earlier could not be reproduced from any
  primary source (exact-phrase search returns nothing). The first-party Pingora report
  (<https://github.com/cloudflare/pingora/issues/841>) is an **SSE-not-flushed bug on macOS
  with self-hosted Pingora 0.8.0** — unrelated to the edge. Zone-level buffering settings
  cannot apply to a zoneless quick tunnel at all.
- **Does a WebSocket upgrade complete end-to-end through a quick tunnel?** cloudflared's
  source says it proxies WS to the origin, and Cloudflare documents WS support without
  configuration — but no first-party page states "quick tunnel + WebSocket", and we have
  not measured it.
- **How does a public client address Funnel's TCP forwarder** (TLS to `*.ts.net:443`?
  exact address format) — a two-minute experiment settles it.
- **Latency of either path**: no published numbers.
- **Quick tunnel lifetime/abuse behaviour**: undocumented.

# ADR-006 — The door is an implementation detail: quick tunnel + WebSocket, pairing as the gate

Date: 2026-09-22. Status: accepted. Revises the door recommendation in ADR-005
(Tailscale), which this ADR supersedes. Evidence: `docs/research/001-live-stream-doors.md`
and the spike measured below.

## Context

ADR-005 made a share two halves — a transport owned by the daemon, and GitHub as the
ledger — and named Tailscale as the door. The product requirement was then stated
plainly: **like VS Code Live Share, transparent — neither participant should know or
care what handles the tunnel.** That requirement changes which door wins, because it
makes *operator and guest friction* the deciding axis rather than network shape.

Research (`docs/research/001`) established from primary sources: quick tunnels need no
account, no domain and no DNS, and are spawned by one command; Tailscale doors need an
account on one or both sides; and a quick tunnel's origin is HTTP, so our raw-TCP stream
cannot ride it unchanged.

A spike was then measured on this machine (cloudflared 2026.9.1 from nixpkgs, run
transiently with `nix run`; local origin on `127.0.0.1:8791`):

- **Plain HTTP chunked responses are buffered to completion through the tunnel.** A
  local origin flushing 30 chunks 100ms apart delivered them *paced* when probed
  directly (first at `+0.001s`, gaps `~101ms`) and delivered **all 30 together at
  `+3.228s`** through the tunnel (`gap_median=0ms`, `gap_max=0ms`). The origin's
  `http.Flusher` reported supported in both cases, so the hold is in the tunnel path,
  not at the origin. This rules out SSE/chunked as the live transport.
- **WebSocket works through the same tunnel and is not batched.** `101 Switching
  Protocols` with a valid `Sec-WebSocket-Accept` in 112–136ms; 10 frames spaced 100ms
  apart each echoed in **31–34ms** (TLS handshake to the edge: 57–65ms).
- **The public URL dies with the process.** After `cloudflared` exited, the hostname
  answered `HTTP 530` — nothing of ours remained reachable.

## Decision

1. **`serve` owns the tunnel.** It spawns `cloudflared tunnel --url
   http://127.0.0.1:<port>`, parses the printed `https://<random>.trycloudflare.com`
   URL, prints exactly one line — the `herdr-huddle join <url>` command to send — and
   kills the child on exit. Neither participant types an address, a port, or a flag;
   the guest installs nothing but our binary, and no account exists anywhere.
   `cloudflared` missing on the operator's machine is detected and reported with the
   one install line that fixes it.
2. **The transport is WebSocket over that URL.** Because a quick tunnel's origin is
   HTTP and measured chunked buffering makes plain streaming unusable, frames ride
   `wss://`. This rewires only the socket adapter of the phase-1 stream: the frame
   records, one-`observe`-per-joiner, the disconnect/leak guard and `Render` are
   unchanged.
3. **Pairing is the gate, and it is GitHub's device flow.** The join client performs
   the device flow in the human's browser and sends its token as the first message;
   the server resolves the login, checks the share's allowlist, and only then emits
   the first frame. A token travels over `wss://` or loopback only — never a cleartext
   bind. This is ADR-005's identity decision (one allowlist, two doors) made load-
   bearing: a quick tunnel cannot have Cloudflare Access in front of it (no zone of
   ours), so this check is the *entire* gate for a public endpoint.
4. **Sequencing is a safety rule:** the tunnel is not enabled in any shipped path
   before the pairing gate exists. Exposure first would mean an unauthenticated
   public pane.
5. `--addr` survives as the escape hatch for tests and LAN use. The product surface
   stays one URL.

## Alternatives rejected

- **Tailscale (tailnet invite or single-device share)** — ADR-005's original
  recommendation: private by default, raw TCP works as committed, stable address.
  Rejected on the transparency requirement: the operator needs an account and the
  guest needs a client installed and approved. Kept in reserve if a private door is
  ever preferred to a public gated one.
- **Tailscale Funnel (`--tcp`)** — genuinely close: free on all plans, no guest
  install, stable `*.ts.net`, and raw TCP so no transport change. Rejected because it
  is beta, needs a Tailscale account and CLI on the operator's side, writes
  certificates and policy into his tailnet, allows only ports 443/8443/10000, carries
  non-configurable bandwidth limits, and has a Let's Encrypt rate-limit footgun
  (34-hour lockout).
- **Named Cloudflare tunnel / own domain / ngrok** — an account and, for the first
  two, a domain. Fails transparency outright.
- **LAN or loopback only** — no door at all, but it cannot reach a remote
  collaborator.
- **Chunked HTTP or SSE as the transport** — zero dependencies, and the direction
  I expected to prefer. Measured dead: buffered until the response completes.
- **Asking the user to pick a door** — contradicts the requirement that started this
  ADR. The door is chosen by the code, not by a human.
- **Hand-rolling RFC 6455** — to keep `go.mod` at zero dependencies. Rejected as
  more code, and more risk, than a small maintained library; `cloudflared` itself is
  built on the same lineage (gorilla/nhooyr, now `coder/websocket`).

## Consequences

- **`go.mod` stops being dependency-free** with one library for WebSocket. The
  zero-dep property ends here, deliberately, to avoid owning a wire protocol.
- The phase-1 `net.Listener` path is replaced by an HTTP+WS server on the same
  loopback bind; committed phase-1 code is rewired, not discarded.
- **A public, ephemeral endpoint now exists while `serve` runs**, so the pairing
  gate (phase 2) is the next thing to build — not the comment box.
- The operator gains a runtime dependency (`cloudflared`); the guest gains none.
- Latency for the guest includes an edge relay (~57–65ms TLS to the edge, ~31–34ms
  frame echo measured) — fine for a terminal.

## Known gaps

- **Measured for minutes, not hours:** WebSocket was proven over 10 spaced frames and
  a few short sessions. A long-lived stream across resize, scrollback, agent restarts
  and multi-hundred-KB full paints through the tunnel has not been run.
- **Quick tunnel lifetime, rate and abuse limits are undocumented** (the vendor page
  states use cases only). A multi-hour collaboration session is untested; the tunnel
  may be throttled or dropped without notice, and there is no reconnect yet.
- **SSE/chunked buffering is measured on this path only** — one edge, one day, one
  cloudflared version. Not portable to Cloudflare zones (which have their own
  settings) or to other relays.
- **No panic path for a flapping tunnel:** if `cloudflared` dies mid-session,
  `serve` currently has no defined behaviour (restart, report, or exit) — undecided.
- **The gate does not exist yet**, so today the only thing standing between a public
  URL and the pane is that nobody has the URL. Rule 4 above is what keeps this from
  being a live exposure.
- **Queued records are in memory only.** A `serve` restart loses whatever the
  ledger still owed; the shutdown log says how much, but nothing is persisted
  and replayed.
- **Tokens can die and nothing refreshes them.** GitHub may issue expiring
  access tokens (8 hours); when the stored one expires, `join` and `poll` both
  fail until `auth login` runs again. The gate reports this honestly but has no
  refresh path — observed live: a stored token was rejected with `401: Bad
  credentials` mid-verification.
- **`cloudflared` version pinning is undefined** — measured on nixpkgs 2026.9.1
  via `nix run`; nothing yet records which version a running `serve` used.

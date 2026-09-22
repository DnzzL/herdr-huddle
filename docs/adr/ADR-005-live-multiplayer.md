# ADR-005 — multiplayer as a live stream with GitHub as the ledger

Date: 2026-09-22. Status: accepted. Revises the posture of ADR-003 (the poller as the only bridge in both directions) and refines ADR-001's decision to reject terminal mirroring: what we rejected was mirroring our own TUI to terminals everyone could drive; what we adopt now is a one-way, daemon-owned live stream we had not discovered yet.

## Context

The MVP works and is verified end to end on a real PR (the two real transcript comments on molkky PR #13): the collaborator's window is the GitHub thread, updated by the poller at most once per turn, once per 10-second pass. The experience Thomas wants is Delta's (zed.dev): a teammate joins your session, watches the agent live, comments on what is happening, and steers — in real time. GitHub stays the backend and the traceability.

Two measured facts drive the design:

- **Herdr has the shareable live primitive we assumed did not exist.** `herdr terminal session observe <pane>` streams the pane's ANSI frames as NDJSON (`terminal.frame`, base64) to any number of observers, taking no input, no resize, no scroll and no takeover ownership. Verified on 0.9.0 against a live pane. (Herdr's full API schema contains no read-only/ACL/permission concept at all; `server-access`-style server-enforced authorization does not exist there.)
- **GitHub cannot carry real time.** A 10-second poll through the REST API is the floor unless something else bridges the gap; HTTP latency and rate limits rule out PR comments as a transport.

And a trust fact: the allowlist — not the prompt wrapper — is the real security boundary. Anyone allowed in can drive an agent running with the operator's permissions, in the operator's worktree, on the operator's quota. "No SSH" must therefore not mean "lower trust than the allowlist implies"; it means the same gate applies at whatever door opens.

## Decision

**A share now has two halves: a transport owned by the daemon, and a ledger that is GitHub.**

- **Participant.** One operator + one or more collaborators who join the operator's machine-bound world and run nothing of their own (no agent, no checkout, no Herdr). A peer bringing their own environment is explicitly out of scope.
- **Live view (out).** The daemon runs `herdr terminal session observe <pane>` locally and streams the frames onward to connected joiners. The collaborator never connects to Herdr's socket, never opens a shell, never sends bytes in that direction: read-only is true by construction, not by client behaviour and not by Herdr's grace.
- **Steering (in).** An instruction arriving from an authenticated joiner **over the live connection** is injected immediately (`herdr agent prompt`), without waiting for a poll pass — the gate that admitted them is the same allowlist. The daemon then posts the record to the PR, attributed to its author and labelled as having arrived live. GitHub's issue-comment API offers no reply anchoring (no `in_reply_to` on the create endpoint; issue comments are flat — verified against the REST docs), so the record attributes the turn instead of threading it under it. Delivery is never blocked by GitHub's availability.
- **The record.** The PR thread remains the canonical, append-only transcript (ADR-003's shape is unchanged: one comment per completed turn). The daemon posts the collaborator's live instructions retroactively as comments; if posting fails, it queues and flushes later rather than steering off the record. The record must eventually hold everything that happened; nothing in the delivery path waits for it.
- **Identity.** The collaborator's device-flow GitHub token is the proof for both halves: the same token that posts comments gains the live stream, and the allowlist decides who is a participant at all. **The door.** A transport that reaches a home machine without accounts-ssh-key-on-the-box is required for the stream to cross the internet. **Superseded by ADR-006**: the door is an implementation detail owned by `serve` — a Cloudflare quick tunnel, WebSocket as transport, device-flow pairing as the gate. Tailscale (GitHub a native IdP, ACLs scoping one person to one port, P2P with managed relay fallback) is the private-by-default reserve; a locked-down SSH account (`authorized_keys` with a single forced command) stays the last resort, but the identity is a key, not GitHub.
- **The join client.** `herdr-huddle join` — the client a collaborator runs to join a live share: it renders the frame stream, carries a comment box, and reuses device-flow auth. Live Share for a terminal agent: a live share of the agent's pane, whose participants can speak to it — not a co-editing session, and with the PR thread beneath it as the permanent record. Later, a browser client consuming the same stream replaces it without touching anything underneath.
- **Where typing stays honest.** Typing *directly* into a Herdr-attached pane is out of scope and discouraged: pi would record it as an unattributed user message attributed to the operator, which corrupts the record's traceability. All collaboration-hour steering goes through the comment path, which both identifies the human and passes the allowlist.

## Alternatives rejected

- **Herdr's own door** (`herdr --remote` over SSH, possibly a dedicated named session): the client can drive everything the session can reach — Herdr has no server-side authorization — and typing in the pane is misattributed in the transcript.Cheap, but trust comes before consent; accepted only if the collaboration-hour client's behave duty matters and the daemon were to be abandoned.
- **GitHub as transport, faster polling**: stays where the design already is, rate limits push back, and latency can never go under the poll interval; not the Delta experience requested.
- **Building our own P2P transport** (WebRTC/hole-punching): a product for two people; the mesh engines above do exactly this, maintained.
- **tmux instead of Herdr for the share** (tmux has server-enforced read-only ACLs, `server-access -a -r`): the agents live in Herdr; moving the agent out of Herdr to gain one tmux affordance trades the whole product away.
- **A peer model** (each participant runs their own agent and both are merely visible in one place): out of scope per the participant decision.

## Consequences

- **What gets built** (rough order): (1) the daemon-side stream (pipe `observe` frames outward, one connection per joiner; agent-status summary from `herdr api snapshot`); (2) the join client (render frames onto a TTY, comment box, device flow reused for auth); (3) the chosen door (Tailscale address, tunnel, or locked SSH); (4) live-comment injection (allowlist first, `agent prompt`, then the GitHub post) growing on the existing inbound path; (5) the web view later on the same stream.
- The poller stays the bridge for the *record* half of ADR-003 — comments posted passively, one per turn; what changes is only that instructions no longer need the poll to reach the agent when the daemon is live.
- The startup hook and `herdr plugin link` lifecycle gain a live pitfall: with a daemon always running, the "no active share" fast path of `poll --once` now has incoming obligations (queued comments, live listeners), not just file reads.
- Trust does not relax: the allowlist applies at both doors (stream and comments), administered on one list.
- The collaborator acquires the *limbs* to steer instead of "watch-and-wait-for-a-reaction"; the real round trip shortens from tens of seconds to sub-second.

## Known gaps

- **Built and verified locally across phases 1–4**: `serve` streams a pane's
  frames per joiner over WebSocket (each joiner its own `observe`, a late
  joiner still gets a complete first paint, a missing pane reported by name,
  no child left behind when a joiner leaves); the gate admits or refuses on
  GitHub identity plus the share's allowlist *before* any frame is observed;
  `join` draws the stream and delivers typed instructions through
  `herdr agent prompt`, recording each delivered one on the thread. Verified on
  0.9.0 against real GitHub (a dead token refused with zero bytes drawn, a gate
  pass logged) and against this machine's share record.
- **Live injection has never met a real agent.** The delivery path runs to the
  `herdr agent prompt` call, but no agent pane exists on this machine to receive
  one, and the operator's token is expired. Blocked/queued/held outcomes are
  exercised against fakes that speak herdr's error codes, not against herdr.
- **Nothing manages the serve process.** It is a foreground command with no
  lock, no idle rule and no supervision by the plugin, and it survives nothing:
  no reconnect after a network blip or a dead `cloudflared`, no restart of a
  dead pane's stream beyond the joiner being told and reconnecting. Its relation
  to the startup hook and to ADR-003's "no active share" fast path is undecided.
- **The agent-status summary is not built.** Phase 1 names it (`herdr api
  snapshot`) alongside the frame stream, as the thing that tells a joiner *what*
  the agent is doing rather than only what it is printing; the stream ships the
  frames alone, so a joiner sees the pane, not whether the agent is working,
  blocked or idle.
- **The viewport is the server's, not the joiner's.** Each stream is rendered at
  one fixed size chosen at startup; a joiner's real window size is not sent, so
  a narrow terminal wraps. Resize, scrollback and an agent restart mid-stream
  have not been watched beyond minutes.
- **Off-machine transport: partly measured.** The quick-tunnel WebSocket variant is
  measured (ADR-006); Tailscale (device share, Funnel) and Cloudflare Access remain
  untested in every variant.

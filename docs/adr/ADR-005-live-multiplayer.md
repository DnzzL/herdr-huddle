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
- **Steering (in).** A comment arriving from an authenticated joiner is allowlist-checked and injected immediately (`herdr agent prompt`), without waiting for a poll pass. The daemon then posts it to the PR — attributed, anchored as a reply to the turn it refers to. Delivery is never blocked by GitHub's availability.
- **The record.** The PR thread remains the canonical, append-only transcript (ADR-003's shape is unchanged: one comment per completed turn). The daemon posts the collaborator's live instructions retroactively as comments; if posting fails, it queues and flushes later rather than steering off the record. The record must eventually hold everything that happened; nothing in the delivery path waits for it.
- **Identity.** The collaborator's device-flow GitHub token is the proof for both halves: the same token that posts comments gains the live stream, and the allowlist decides who is a participant at all. **The door.** A transport that reaches a home machine without accounts-ssh-key-on-the-box is required for the stream to cross the internet. Tailscale is the recommended door (GitHub is a native IdP; ACLs scope one person to one port; P2P with managed relay fallback); Cloudflare Access with GitHub SSO is the fallback; a locked-down SSH account (`authorized_keys` with a single forced command) is acceptable but the identity is a key, not GitHub.
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

- None of the above is built yet: there is no stream, no join client, no door, no live injection path. The next phase must start from phase 1 (the daemon-side stream), which can be verified alone: frames delivered to a local `herdr-huddle join` on the same machine before any tunnel/policy exists.
- Off-machine transport is untested in every variant (Tailscale, Cloudflare Access, locked SSH); the ADR names a preference, not a measurement.
- The exact shape of a live comment's GitHub record (reply-to-turn vs reply-to-message, attribution line) is unverified against real API behaviour and must be defined before phase 1 is used for real.
- Herdr's `observe` survives pane resize, scroll and agent restarts in ways we have not yet watched (multi-observer is documented, not yet exercised by us beyond a 5-second peek).
- The daemon lives longer than a poller pass now — lifecycle, reconnect and the "no active share" rule all need re-examination against the new obligations.

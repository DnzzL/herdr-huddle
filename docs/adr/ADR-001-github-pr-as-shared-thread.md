# ADR-001 — GitHub draft PR as the shared thread for herdr-huddle

- Status: accepted
- Date: 2026-09-17
- Context owner: Thomas Legrand

## Context

Pair programming is disappearing: people code alone next to an agent, and code
review is thinning out. The goal is to bring a second human back into the loop
*before* execution (plan together), during execution (steer), and after
(traceability — a readable record of what was decided and why).

Herdr is a local-first terminal workspace manager for coding agents. It already
offers `herdr --remote <ssh>`, which gives true co-presence — but only to people
with SSH access to the machine, and it leaves **no artifact**: when the session
ends, the shared reasoning is gone.

Plugin v1 constraints (verified against https://herdr.dev/docs/plugins/):
- directory + `herdr-plugin.toml`, any executable
- actions, event hooks, startup hooks, terminal panes
- no native non-terminal UI, no runtime action registration
- no daemon/service manifest key; startup hooks are one-shot
- `herdr plugin config-dir` provides a Herdr-managed config directory

## Decision

The shared unit is a **GitHub draft PR, opened on an empty branch at share
time — before any code exists**. One object carries three phases:

| Phase | Surface |
|---|---|
| Plan | PR conversation tab — both humans and the agent |
| Execute | the diff appears; review comments become anchored |
| Trace | the closed/merged PR, permanently |

- **Auth**: OAuth App + device flow. `client_id` compiled in, no client secret,
  no hosted callback. Token in the OS keychain. `public_repo` for public repos,
  `repo` only for private ones.
- **Transcript → PR body**, rewritten each agent turn. Source, in order:
  Claude Code session JSONL (`~/.claude/projects/<slug>/<session-uuid>.jsonl`,
  where the UUID is handed over by `herdr agent list` as
  `agent_session.value`), falling back to `herdr agent read` for other agents
  and marked in the PR body as a partial terminal snapshot.
- **Comments → agent**: poll the PR comment endpoints every 10s with ETag
  (304s do not count against the 5000/h REST quota). A comment is injected via
  `herdr agent prompt` only if it passes **all three**: an `/agent` prefix, an
  `author_association` in `OWNER|MEMBER|COLLABORATOR`, and an explicit
  per-share allowlist. The injected text is tagged as untrusted third-party
  input carrying the author's login.
- **The permission gate is never delegated.** An agent blocked on an approval
  posts a PR comment naming what it is waiting for; only the operator answers it.
- **Concurrency**: a comment arriving while the agent is `working` gets a 🚧
  reaction and is queued, never dropped.
- **Poller lifecycle**: a single global process, PID-locked, state in
  `herdr plugin config-dir`, (re)started by the plugin's **startup hook** — which
  also re-fires on live handoff (`herdr update`). Exits when no share is active.
- **Nothing to install for the other person.** github.com is the whole client.

## Consequences

- No real-time co-presence: no cursors, no presence indicators, ~10s latency.
  Acceptable — human planning conversation tolerates it, and `herdr --remote`
  covers people who do have SSH.
- The collaborator needs at least **read** access to comment on a private repo.
  `share --invite @user` issues `PUT /repos/{owner}/{repo}/collaborators/{user}`
  with `permission: pull`. On a work org where the operator is not an admin,
  this is a hard limit — document it, do not work around it.
- Scope `repo` is broad, but only **one** person authenticates (the host), and
  the token never leaves their machine. No multi-tenant token store exists.
- The Claude-specific JSONL path is undocumented and may break on a Claude Code
  release. Mitigated by defensive parsing plus automatic fallback to the
  degraded `agent read` path, which is built anyway.

## Alternatives rejected

| Alternative | Why rejected |
|---|---|
| **GitHub Issue as the thread** | Comments cannot anchor to code; the execute phase loses its central feature. |
| **Private Gist** | ACL is a secret URL only; no anchored comments; no diff. |
| **PR created after the work** | Kills the phase Thomas actually wants back — thinking together *before* writing code. |
| **`herdr agent read` as the only transcript source** | Measured on a live pane: readable prose, but no cursor for incremental sync, bounded scrollback (1095 JSONL lines → ~120 terminal lines), collapsed tool calls, TUI chrome, width truncation. A degraded transcript would define the product. Kept as fallback, not as the primary. |
| **GitHub App instead of OAuth App** | A GitHub App only issues user tokens on repos where it is *installed*; on a company org that needs admin approval, which kills the core use case. User-to-server token refresh also pulls a `client_secret` back in. |
| **Self-hosted real-time relay (WebSocket)** | Requires a server, a domain, secret rotation and an account model. Contradicts the local-first, nothing-leaves-the-machine property of the Herdr plugin family. |
| **Official Claude Code GitHub Action** | Already does ~80% of "comment drives an agent", for free. It cannot do the part that matters here: resume an *existing warm session*, see uncommitted state, or reach local MCP servers, DBs and credentials. That gap is the whole justification for building. |
| **`herdr --remote` (SSH) alone** | True co-presence, zero code — but requires machine access and produces no artifact. Traceability is precisely what it cannot give. |

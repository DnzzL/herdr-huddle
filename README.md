# herdr-huddle

**Open your agent's terminal to other people. Leave a pull request behind as the
record.**

[![check](https://github.com/DnzzL/herdr-huddle/actions/workflows/check.yml/badge.svg)](https://github.com/DnzzL/herdr-huddle/actions/workflows/check.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.26%2B-00ADD8.svg)](go.mod)

> **Early.** It works end to end for you and one person you trust. What is and
> is not proven is listed under [Status](#status) — read it before you rely on it.

```
 pi · ~/Projects/demo
   ▸ Read internal/live/server.go
   ▸ Edit internal/live/server.go
   ● Thinking…

── huddle ─────────────────────────────────────────────────── acme/demo#13 ──
● working   @thomas (host), you and @ana (typing…)
@ana → make the gate refuse an expired token · delivered
agent › also update the README▏
```

You run a coding agent in a Herdr pane. `herdr-huddle` turns it into a room: one
link, sent to whoever you want in. They run one binary and watch the agent work —
live, in their own terminal, at their own size — and type to steer it. Every
instruction reaches the agent immediately and lands on a pull request afterwards,
attributed.

The pull request is the artifact. The agent's turns arrive as comments, so does
every instruction anyone gave live, and a comment beginning with `/agent` is
meant to steer the agent even from people who never open a terminal. The agent
stays in your terminal, under your control, and nothing is ever typed into your
pane.

## Install

| You are | Do this |
|---------|---------|
| **The host**, with an agent in a [Herdr](https://herdr.dev) pane (0.9.0+, Linux or macOS) | `herdr plugin install DnzzL/herdr-huddle` |
| **A guest**, joining somebody's huddle — no Herdr, no agent | Download the binary from [Releases](https://github.com/DnzzL/herdr-huddle/releases), or `go install github.com/DnzzL/herdr-huddle/cmd/herdr-huddle@latest` |
| **Hacking on it** (Go 1.26+) | `make build` → `bin/herdr-huddle` |

Check what you have with `herdr-huddle --version`.

### Joining a huddle

You were sent a link. A GitHub token proves who you are, and the host lets you
in with one keypress:

```
export GH_TOKEN=$(gh auth token)      # or: herdr-huddle auth login
herdr-huddle join https://k3f9x1.trycloudflare.com
```

`Ctrl-T` switches the line between the agent and the room, `?` lists every key,
`Ctrl-C` leaves.

## How it works

| Command | What it opens |
|---------|---------------|
| `share` | The **thread** — a branch, an empty commit, a draft pull request. |
| `serve` | The **huddle** — a link to send, and the door you answer. |
| `join`  | Everyone else's **way in** — the pane, the room, and a line to type. |

`share` and `poll` keep the pull request in step with the agent on their own.
`serve` and `join` are the live half, and they are optional: the thread works
without them.

## Quickstart

**You need** Herdr 0.9.0+ on Linux or macOS, Go 1.26+ to build, and an agent
running in a Herdr pane. `pi` and Claude Code have a transcript adapter; any
other agent works, with its transcript read from the pane's terminal and
labelled as such.

**1. Install it as a Herdr plugin.** This clones it, builds it, and registers it;
the poller starts on the next server start.

```
herdr plugin install DnzzL/herdr-huddle
```

To work on it instead, build and link the checkout — `make install` prints the
link command rather than running it, because registering a plugin is a change to
your Herdr, not to this repository:

```
make build && herdr plugin link /path/to/herdr-huddle
```

**2. Create a GitHub OAuth App**, and tell the binary about it. Settings →
Developer settings → OAuth Apps → New OAuth App. Any name and homepage URL; the
callback URL is unused but must be filled in (say `https://github.com`). **Tick
"Enable Device Flow"** and copy the client id — it looks like
`Iv1.0123456789abcdef`. Then put it in your shell profile:

```
export HERDR_HUDDLE_CLIENT_ID=Iv1.0123456789abcdef
```

> One OAuth App per person, not one per plugin — which is why it is not baked
> into the build. The client id is not a secret: the device flow was chosen
> precisely so that no client secret exists and nothing has to be hosted. If you
> would rather compile it in, `make build CLIENT_ID=Iv1.…` still does that.

**3. Authorize.**

```
./bin/herdr-huddle auth login --repo owner/name
```

It prints a code and a URL. Open the URL, type the code. The token goes in the OS
keychain, scoped to the narrowest thing that works: `public_repo` for a public
repository, `repo` for a private one. `GH_TOKEN` and `GITHUB_TOKEN` are honoured
first if you already have one.

**4. Open a share from the pane** whose agent you want to share:

```
./bin/herdr-huddle share
```

Or, once the plugin is linked, from Herdr itself: the plugin adds two actions,
**ouvrir le fil de ce projet** and **ouvrir le huddle sur ce pane**. An action
knows which pane has focus and which project it works in, so it does not depend
on where your shell happens to be. The huddle action opens your room in a pane
beside the agent's (`serve --split`), because an action's output only ever
reaches `herdr plugin log`.

That is the thread. `--invite @user` is optional — it gives someone repository
read access and puts them on the allowlist ahead of time, which is useful for a
person whose `/agent` comments should work before they ever join live.

## Open the huddle

```
# on your machine, in the pane's project
./bin/herdr-huddle serve
#   herdr-huddle: streaming w16:p1 for acme/demo — the record is https://github.com/acme/demo/pull/13
#   herdr-huddle: the door is knock: somebody new asks here, and you answer
#   herdr-huddle: live share ready — send them:
#     herdr-huddle join https://k3f9x1.trycloudflare.com

# on the other machine — nothing to install but this binary
herdr-huddle join https://k3f9x1.trycloudflare.com
```

`serve` starts a [quick tunnel](docs/adr/ADR-006-quick-tunnel-door.md) itself and
prints the one line to send. It needs `cloudflared` on your machine; without it
it says so and serves this machine only, which `--no-tunnel` forces. The
collaborator installs nothing else: no Herdr, no tunnel client, no account.

### The link is the invitation

Somebody who is not on the allowlist yet proves who they are on GitHub, and you
are asked on your own terminal:

```
herdr-huddle: @collaborator is at the door (github.com/collaborator).
  let them into the huddle? [y/N] y
13:42:07 let @collaborator in
```

One `y` and they are in for good: the login goes onto the share's allowlist, so
their pull-request comments start being delivered too, and a reconnect does not
ask you again.

This happens **before a single frame is observed**. A refused joiner is told why
(`@x was not let into this share`, `401: Bad credentials`) and sees nothing at
all — no pane, no partial paint, and no `observe` process ever started for them.

| Door | Behaviour |
|------|-----------|
| **knock** *(default)* | You are asked. A `y` is written to the allowlist and outlives the session. |
| `--open` | Anyone with the link and a GitHub identity is in, for as long as `serve` runs. Nothing is written. |
| `--closed` | The allowlist or nothing, and nobody is asked — for a `serve` nobody is sitting in front of. |

The question rings a bell and raises a Herdr notification, not just a line in
this log — you are watching your agent, not the terminal `serve` prints into.
Questions are asked one at a time in the order they arrived, and one about
somebody who has since disconnected is dropped rather than left on your screen.

### The host's room

In a terminal, `serve` draws your side of the huddle instead of scrolling a log:
the line to send at the top, then what happened — who came and left, the room's
chat, every instruction and what became of it — with the same roster, agent
state and typing marks a joiner has underneath, and a line that talks to the
room. The door's and the moderator's questions are answered there with a single
`y` or `n`, on an empty line.

```
15:22 @ana joined
15:23 @ana: wait, not the migration
15:24 @ana → fix the gate · delivered
? @carl is at the door (github.com/carl). let them into the huddle?  [y/n]
── huddle ──────────────────────────────────────── acme/demo#13 ──
● working   you (host), @ana (typing…) and @bo
room ›
```

You steer the agent from its own pane, as before: this line never reaches it.
`--plain` keeps the old log, for a `serve` nobody is sitting in front of.

### Letting somebody in is not letting them drive

`--moderated` splits those into two decisions. Every instruction is put to you,
with the words in front of you, before the agent sees it:

```
herdr-huddle: @ana wants to send the agent:

    drop the users table

  send it? [y/N] n
```

The joiner sees `waiting for the operator to approve it` while you decide — not
silence, which reads as a dropped instruction — and the whole room sees the
answer. Use it for anyone you would not hand your shell to.

### What the joiner gets

- **Their own stream**, rendered at *their* terminal's size and freshly painted
  from the top, so someone arriving late sees a whole screen rather than the tail
  of one. Resizing repaints it. Several people can be in at once, each at their
  own size.
- **The room**: who else is here — the operator included, marked as the host —
  and what the agent is
  doing: `working`, `idle`, or `waiting on the operator`. That last one is the
  thing a silent pane cannot tell you. Everyone has a colour, and it is the same
  colour everywhere, so the room is scanned rather than read.
- **Who is composing right now**, marked on the person and saying who it is
  for: `@ana (typing to agent…)` or `@ana (typing to room…)`. The first stops two
  people asking the agent for the same thing at once; the second is somebody
  about to speak. It lapses on its own, so a client that dies mid-sentence does
  not type forever. The host is marked too, when they write in their room.
- **A footer you can follow.** The last few things that happened — who came,
  what was said, what became of each instruction — stay on screen above the
  input line, with the shortcuts on the rule. `Ctrl-L` makes it taller (the pane
  gives up the rows and repaints at the new size), and again to put it back.
  `?` on an empty line lists every key: `↑↓` recalls what you sent, `Ctrl-U`
  clears the line, `Ctrl-W` deletes a word.
- **Two places to type.** `Ctrl-T` switches the input line between the agent and
  the room, and the line says which — `agent ›` or `room ›`. Talking to the room
  reaches the people and never the agent, so you can say "wait, don't touch the
  migration" to a colleague without the agent doing something about it. Room
  messages are **not** posted to the pull request: the thread is the record of
  the agent's work, and one full of "one sec" is a record of nothing.
- **A line to the agent** reaches it immediately, wrapped as a colleague's
  unverified message carrying their login, and recorded on the pull request
  afterwards. Everyone in the room sees **what was asked for and what became of
  it** — `@ana → make the gate refuse an expired token · delivered` — because
  knowing that somebody said *something* is not enough to avoid saying it twice.
  A held or failed delivery is reported the same way; nothing is silently
  dropped.
- **Up-arrow recalls what you sent**, so an instruction that came back `held`
  does not have to be retyped.
- **A huddle that survives its tunnel.** A dropped connection is retried, from
  half a second out to fifteen, with the terminal kept and the reason on screen.
  It gives up on the only two things retrying cannot fix: you left, or you were
  turned away. A GitHub hiccup or an operator who stepped away from the knock is
  retried like any other blip. (This is the TUI; piped output does not retry.)
- **Ctrl-C to leave.** It ends their stream and the observer behind it, the room
  is told, and nothing else changes.

Keystrokes never reach your terminal: the stream runs `herdr terminal session
observe`, which takes no input, no resize and no takeover. A joiner sends the
agent sentences, not keypresses, and cannot answer a permission prompt.

Piped somewhere that is not a terminal, `join` writes the pane's bytes out
plainly instead of drawing a room.

## The pull request is the record

Your collaborator installs nothing to follow the *thread* — no Herdr, no
herdr-huddle, no token. It is a normal pull request, and the URL is the whole
invitation. Watching the agent live is the one thing that needs a client.

- **The body** is a header, written once when the share opens. It says what the
  thread is and what the `/agent` rules are.
- **The transcript** arrives as comments, one per completed agent turn, each
  marked as this tool's own output. Nothing is posted while the agent is still
  working.
- **Instructions** are comments that start with `/agent`, delivered to the agent
  tagged as third-party input carrying the author's login, so the agent has no
  reason to treat them as coming from you.
- **The only acknowledgement is a 👀 reaction**, added when an instruction is
  held back because the agent is busy. A refused one gets nothing: the reason
  goes to the operator's log and nowhere public.
- **The diff is empty** until *you* commit. Uncommitted work is deliberately not
  mirrored: a diff that changes under a reviewer without a commit is a lie.

## Commands

```
herdr-huddle --version
herdr-huddle auth login [--repo owner/name] [--public]
herdr-huddle auth status
herdr-huddle auth logout
herdr-huddle share [--slug name] [--base ref] [--invite @user]... [--dry-run]
herdr-huddle poll [--once] [--interval 10s]
herdr-huddle serve [--pane id] [--open | --closed] [--moderated] [--notify] [--plain] [--no-tunnel] [--addr host:port]
herdr-huddle join [address] [--addr 127.0.0.1:8787]
```

`serve` opens one pane's huddle. The pane **and** the allowlist come from the
same active share — no share, no server, because there would be nothing to gate
it with; `--pane` picks which share when several are active.

`share --dry-run` reports what would happen and changes nothing: no token needed,
no branch, no push, no pull request. `poll --once` makes a single pass and prints
what it did, which is how to see what the daemon has been doing without waiting
for it. `poll` on its own is the daemon the plugin's startup hook starts; it takes
a pid lock, so starting it twice is harmless.

## Before you rely on it

**The door is the whole gate.** `serve` listens on `127.0.0.1`, but by default
puts a Cloudflare quick tunnel in front of it — and a quick tunnel cannot have
Cloudflare Access in front either (it needs a zone of yours). So GitHub pairing —
a token presented first, resolved to a login, then either on the allowlist or
admitted by you — is the *only* thing between that URL and your pane: no rate
limit, no second factor. `--no-tunnel` removes the exposure entirely. The token
itself only ever travels over `wss://` or loopback; the client refuses a
cleartext address.

**Letting someone in is giving them your agent.** It runs with your permissions,
in your worktree, on your quota. The prompt wrapper says the message is untrusted
and carries its author's login, but that is a defence, not a sandbox: an agent
that obeys a message can do anything you could. `--open` makes the link the
entire boundary with no human in the loop, which is exactly why it is a flag and
not the default.

**The permission gate is never delegated.** When the agent stops for approval,
only you can answer, at your terminal. Herdr refuses a prompt to a blocked agent
outright and this tool does not try; it posts a comment saying the agent is
waiting, so your collaborator is not left guessing.

**The unproven paths are listed under [Status](#status)** —
`--invite`, token expiry, and the refused and held instruction paths.
`## Known gaps` in each ADR carries the detail.

<details>
<summary><b>More limits, in detail</b> — GitHub's constraints, the record, the stream, the TUI</summary>

- **On a private repository, your collaborator needs at least read access to
  comment at all.** `--invite` asks for it (`permission: pull`). On a work org
  where you are not an admin that request will fail — the share says so and
  carries on, and you have to get access granted another way. This is a hard
  GitHub limit, not something this tool can work around.
- **Everything posted into the pull request is secret-scanned first**, but the
  scan is a pattern match, not a guarantee. The local session file is the
  authoritative record; the pull request is a projection of it.
- **Answering `y` is not reversible from here.** That admission *is* written to
  the share record, which is the point. Taking it back means editing
  `shares.json`; the poller picks the change up on its next pass, though a
  running `serve` keeps them until it restarts.
- **A dead token refuses cleanly.** If GitHub no longer accepts the stored token
  the gate says `GitHub could not confirm the token` and shows nothing; run
  `auth login` again. The same dead token stops `poll` posting, so the two fail
  together.
- **The token a collaborator pairs with is not saved** — it proves who they are
  for that session only, and never replaces your repo-scoped one.
- **The record trails delivery, by design.** Every delivered instruction is
  posted as a comment opening with this tool's marker, so the poller recognises
  it as ours and never delivers it twice. GitHub's issue-comment API has no reply
  threading, so the record attributes the turn rather than nesting under it. If
  GitHub is unreachable, records queue (up to 100) and are retried while `serve`
  runs — delivery never waits for them — and anything still queued at shutdown is
  logged, not hidden.
- **A resize costs a fresh stream.** `observe` takes no resize — that is what
  makes it read-only by construction — so a joiner whose window changes gets a
  restarted observer and a complete repaint. Dragging a window edge restarts it
  repeatedly. `--cols`/`--rows` are only the fallback for a client that cannot
  measure itself.
- **The joiner's TUI does not emulate a terminal.** It reserves the bottom four
  rows and repaints them over whatever the pane drew, which is enough for a pane
  that paints in place and not enough for scrollback: there is none, and the
  agent's own cursor is not visible because the cursor is parked on the input
  line. The status line saying `waiting on the operator` is what tells you the
  agent is at a prompt.
- **Nothing starts the stream for you.** `serve` is a foreground command you run
  and stop; the plugin's startup hook still starts only the poller.
- **Reconnect survives a blip, not a restart.** If *you* restart `serve`, the
  quick tunnel gets a new URL, so joiners back off against an address that no
  longer exists. Send them the new line.
- **Room chat has no history.** It lands on the one event line and is gone, so a
  message you scrolled past is lost. Nothing rate-limits it either, beyond a
  2000-byte cap per message.
- **Want an alert when the agent needs you?** That is Herdr's, not ours: turn on
  `[ui] toast` and `[ui] sound` in `config.toml` and it notifies on every agent
  state change, blocked included, with per-agent sound overrides.
- **Typing into the pane directly stays discouraged.** The agent would record it
  as the operator's own words, which costs the record its attribution.
- **The transcript comes from the agent's own session file**, read by an adapter
  for that agent kind. Herdr does not report a session for every kind and there
  is no adapter for the ones it does not; when no file can be found this falls
  back to reading the pane's terminal and labels the comment as a partial
  transcript — readable, but with no cursor for incremental sync and with
  collapsed tool calls.

</details>

## Status

Honest status, because the difference matters before you rely on it.

**Proven end to end**, against real sockets, real terminals and a real pull
request:

- The live huddle — `serve` and `join`, the room, per-joiner viewports, resize,
  the door, room chat, moderated steering, reconnect across a server restart.
- The outbound record: transcript comments really were posted to a real pull
  request.
- Instruction records a failing GitHub would not take survive a `serve`
  restart: they are written down per share and carried over.
- **Steering, both ways in.** A `/agent` comment written on a real pull request
  was read by the poller and delivered into a live Claude Code agent, which
  answered it — and the live path (`serve`'s own call) reached the same agent
  directly. The prompt arrives with its wrapper intact: the author's login, and
  the "colleague's message, not from your operator" framing.

**Not proven yet**, and each one is a thing a second person would hit:

- **`--invite` has been measured once**, against a login that already had
  access. Whether a work org permits the collaborator invitation at all is
  unknown.
- **Tokens expire (GitHub issues 8-hour ones) and nothing refreshes them.** When
  yours dies, `poll` stops recording and `join` stops working until you run
  `auth login` again. Observed again on 2026-09-27: the stored token answered
  `401: Bad credentials` and everything GitHub-side stopped until `GH_TOKEN` was
  supplied instead.
- **A refused or held instruction has never run against the real API.** The
  delivery that was measured succeeded, so the 👀 acknowledgement and the
  "waiting on the operator" notice are still fakes-only.

So: fine for you and one person you trust, with steering proven in both
directions. Still not fine for handing to somebody whose access depends on
`--invite` working, or for a session long enough to outlive a GitHub token.

## Development

```
make check     # gofmt, go vet, go test -race
```

The live Herdr and live GitHub tests skip themselves outside a Herdr pane and
without a token, so the suite runs offline. Everything that writes to a
repository — branches, empty commits, pushes — is tested against a local bare
repository as `origin`, and everything that writes to GitHub is tested against
fakes.

## Design

Every decision, with the alternatives it rejected, is in
[`docs/adr/`](docs/adr/) — read ADR-001 first.

| ADR | Decision |
|-----|----------|
| [001](docs/adr/ADR-001-github-pr-as-shared-thread.md) | A GitHub pull request is the shared thread |
| [002](docs/adr/ADR-002-share-branch-and-push.md) | The share's branch, and what gets pushed |
| [003](docs/adr/ADR-003-thread-contents-and-share-lifecycle.md) | What goes in the thread, and when a share dies |
| [004](docs/adr/ADR-004-transcript-adapters-per-agent-kind.md) | One transcript adapter per agent kind |
| [005](docs/adr/ADR-005-live-multiplayer.md) | Multiplayer as a live stream, with GitHub as the ledger |
| [006](docs/adr/ADR-006-quick-tunnel-door.md) | The door: quick tunnel, WebSocket, pairing as the gate |
| [007](docs/adr/ADR-007-the-huddle-room.md) | The stream becomes a room: knock to join, a viewport each |
| [008](docs/adr/ADR-008-two-channels-two-decisions-and-a-huddle-that-survives.md) | Two channels, two decisions, and a huddle that survives |
| [009](docs/adr/ADR-009-une-action-herdr-comme-second-point-d-entree.md) | A Herdr action as a second entry point |
| [010](docs/adr/ADR-010-the-host-is-a-seat.md) | The host is a seat in their own room |

The vocabulary this is all written in is in [`CONTEXT.md`](CONTEXT.md).

## Contributing

Small, focused changes are the easiest to merge: see
[CONTRIBUTING.md](CONTRIBUTING.md). A security problem goes through
[SECURITY.md](SECURITY.md), not a public issue.

## License

[Apache License 2.0](LICENSE) — permissive, with an express patent grant.

Herdr itself is not a dependency of this repository in the licensing sense:
every call into it is a subprocess (`herdr agent prompt`, `herdr terminal
session observe`), not a link. The Go dependencies are
[coder/websocket](https://github.com/coder/websocket) (ISC) and
`golang.org/x/term` and `golang.org/x/sys` (BSD-3-Clause), all of which are
compatible with this licence.

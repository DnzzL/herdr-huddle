# herdr-huddle

**Live Share for your coding agent.** Open a [Herdr](https://herdr.dev) pane to a
teammate: they watch the agent work in their own terminal, type to steer it, and
a pull request keeps the record.

[![CI](https://github.com/DnzzL/herdr-huddle/actions/workflows/ci.yml/badge.svg)](https://github.com/DnzzL/herdr-huddle/actions/workflows/ci.yml)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.26+-00ADD8?logo=go)](go.mod)
[![Herdr](https://img.shields.io/badge/herdr-%E2%89%A50.9-8A2BE2)](https://herdr.dev)

```
herdr plugin install DnzzL/herdr-huddle
```

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

## Why

Pairing on an agent session today means a screen share, or pasting transcripts
into a chat. The agent is in *your* terminal, with *your* permissions, and the
person who could unblock it is somewhere else.

- **They see what you see, at their own size.** One link, one binary, no Herdr
  and no agent on their side. Each guest gets their own stream, painted for
  their own terminal.
- **They can steer, and you stay in control.** Every instruction reaches the
  agent as a colleague's message carrying their login, you answer the door with
  one keypress, and `--moderated` puts each instruction to you first. Nothing is
  ever typed into your pane.
- **A pull request is left behind.** The agent's turns arrive as comments, so
  does every instruction anyone gave, attributed. A comment starting with
  `/agent` steers it even from people who never open a terminal.

Inspired by VS Code Live Share, and built on Herdr's own building blocks — panes,
`herdr agent prompt`, `herdr terminal session observe` and plugin actions — so
there is nothing to host and nothing to sign up for beyond GitHub.

## Install

| You are | Do this |
|---------|---------|
| **The host**, with an agent in a [Herdr](https://herdr.dev) pane (0.9.0+, Linux or macOS) | `herdr plugin install DnzzL/herdr-huddle` — a prebuilt, checksum-verified binary, no Go needed |
| **A guest**, joining somebody's huddle — no Herdr, no agent | Download the binary from [Releases](https://github.com/DnzzL/herdr-huddle/releases), or `go install github.com/DnzzL/herdr-huddle/cmd/herdr-huddle@latest` |
| **Hacking on it** (Go 1.26+) | `make build`, then `herdr plugin link .` |

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

You need an agent running in a Herdr pane. `pi` and Claude Code have a transcript
adapter; any other agent works, with its transcript read from the pane's
terminal and labelled as such.

**1. Create a GitHub OAuth App**, and tell the binary about it. Settings →
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

**2. Authorize.**

```
./bin/herdr-huddle auth login --repo owner/name
```

It prints a code and a URL. Open the URL, type the code. The token goes in the OS
keychain, scoped to the narrowest thing that works: `public_repo` for a public
repository, `repo` for a private one. `GH_TOKEN` and `GITHUB_TOKEN` are honoured
first if you already have one.

**3. Open a share from the pane** whose agent you want to share:

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

Then open the live half with `herdr-huddle serve` (or the **huddle** action) and
send the link it prints: [the huddle](docs/huddle.md).

## What this does to your machine

`herdr-huddle` opens a coding agent to other people, so the honest statement
comes before the install.

- **Letting somebody in is giving them your agent** — with your permissions, in
  your worktree, on your quota. The door is a GitHub login on an allowlist, or
  your answer to a knock. `--moderated` puts each instruction to you first, and
  nothing is ever typed into your pane.
- **A pull request is opened on the repository you share**: a branch
  (`herdr/<slug>`), an empty commit, pushed, and a draft PR. **On a public
  repository, the thread is public.** What the agent says is secret-scanned by
  pattern, which is not a guarantee.
- **`serve` listens on `127.0.0.1` and, by default, starts a Cloudflare quick
  tunnel** in front of it (`--no-tunnel` removes the exposure). Whoever has the
  link still has to prove a GitHub identity you let in.
- **The token** goes in the OS keychain (`GH_TOKEN` is honoured first), scoped to
  `public_repo` or `repo`. A guest's token proves who they are for that session
  and is never saved.
- **The permission gate is never delegated**: when the agent stops for approval,
  only you can answer.

The detail, and what the door does not protect, is in
[docs/limits.md](docs/limits.md); to report a vulnerability, see
[SECURITY.md](SECURITY.md).

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

## Documentation

| | |
|---|---|
| [The huddle](docs/huddle.md) | `serve` and `join`: the door, your room, what a guest gets |
| [The pull request is the record](docs/thread.md) | `share` and `poll`: what lands on GitHub, and `/agent` comments |
| [Commands](docs/commands.md) | Every command and flag |
| [Before you rely on it](docs/limits.md) | Limits of the door, GitHub, the record and the stream |
| [Decisions](docs/adr/) | Every ADR, with the alternative it rejected — read ADR-001 first |
| [`CONTEXT.md`](CONTEXT.md) | The vocabulary this is written in |
| [Changelog](CHANGELOG.md) | What changed for someone using it |

## Development

```
make check     # gofmt, go vet, go test -race — what CI runs
herdr plugin link .
```

The suite is offline: live Herdr and GitHub tests skip themselves without a pane
or a token, and everything that writes is tested against fakes or a local bare
repository. PRs welcome — start with [CONTRIBUTING.md](CONTRIBUTING.md); a
security problem goes through [SECURITY.md](SECURITY.md), not a public issue.

## License

[Apache License 2.0](LICENSE) — permissive, with an express patent grant.

Herdr itself is not a dependency of this repository in the licensing sense:
every call into it is a subprocess (`herdr agent prompt`, `herdr terminal
session observe`), not a link. The Go dependencies are
[coder/websocket](https://github.com/coder/websocket) (ISC) and
`golang.org/x/term` and `golang.org/x/sys` (BSD-3-Clause), all of which are
compatible with this licence.

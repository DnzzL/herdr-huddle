# herdr-huddle

Open your agent's terminal to other people, and leave a pull request behind as
the record.

You run an agent in a Herdr pane. You open a share — a branch, an empty commit
and a draft PR — and then a huddle: one link, sent to whoever you want in. They
run one binary, see the agent working in their own terminal at their own size,
see who else is in the room, and type to steer it. Everything they say reaches
the agent immediately and lands on the pull request afterwards, attributed.

The pull request is the artifact: the agent's turns arrive as comments, so does
every instruction anyone gave live, and a comment beginning with `/agent` steers
the agent even from people who never open a terminal. The agent stays in your
terminal, under your control, and nothing is ever typed into your pane.

The design is settled in [`docs/adr/`](docs/adr/) — read ADR-001 first.

## What it needs

- Herdr 0.9.0 or newer, on Linux or macOS.
- Go 1.26+ to build it.
- A GitHub **OAuth App** you create yourself (below). One per user, not one per
  share.
- An agent running in a Herdr pane that the share is opened from. `pi` and
  Claude Code have a transcript adapter; any other agent works, and its
  transcript comes from the pane's terminal and is labelled as such.

## Set it up

1. **Create an OAuth App.** GitHub → Settings → Developer settings → OAuth Apps →
   New OAuth App. Any name and homepage URL; the callback URL is unused but must
   be filled in (say `https://github.com`). **Tick "Enable Device Flow".** Copy
   the client id — it looks like `Iv1.0123456789abcdef`.

   The client id is not a secret. The device flow was chosen precisely so that no
   client secret exists and nothing has to be hosted.

2. **Build it with that client id.**

   ```
   make build CLIENT_ID=Iv1.0123456789abcdef
   ```

3. **Link the plugin.** This registers it with Herdr and starts the poller on the
   next server start. `make install` prints the command rather than running it,
   because registering a plugin is a change to your Herdr, not to this repository.

   ```
   herdr plugin link /path/to/herdr-huddle
   ```

4. **Authorize.**

   ```
   ./bin/herdr-huddle auth login --repo owner/name
   ```

   It prints a code and a URL. Open the URL, type the code. The token goes in the
   OS keychain, and is scoped to the narrowest thing that works: `public_repo` for
   a public repository, `repo` for a private one. `GH_TOKEN` and `GITHUB_TOKEN`
   are honoured first if you already have one.

5. **Open a share from inside the pane** whose agent you want to share:

   ```
   ./bin/herdr-huddle share --invite @collaborator
   ```

   `--invite` is optional. It gives that user read access to the repository and
   puts them on the allowlist ahead of time — useful for someone whose `/agent`
   comments should work before they ever join live. To open the huddle itself
   you do not need it: whoever holds the link knocks, and you let them in.

## What the two of you see

- **Your collaborator installs nothing to follow the thread.** No Herdr, no
  herdr-huddle, no token: the thread is a normal pull request, and the URL is
  the whole invitation. On a private repository they still need read access to
  comment at all — see the limits below. Watching the agent *live* is the one
  thing that does need a client; that is `herdr-huddle join`.
- **The pull request body** is a header, written once when the share opens. It
  says what the thread is and what the `/agent` rules are.
- **The transcript** arrives as comments, one per completed agent turn, each
  marked as this tool's own output. Nothing is posted while the agent is still
  working.
- **Instructions** are comments that start with `/agent`. They are delivered to
  the agent tagged as third-party input carrying the author's login, so the agent
  has no reason to treat them as coming from you.
- **The only acknowledgement is a 👀 reaction**, added when your instruction is
  held back because the agent is busy. A refused one gets nothing: the reason
  (`not on the allowlist`, `does not start with /agent`) goes to the operator's
  log and nowhere public. Silence means "check with whoever runs the poller".
- **The diff is empty** until *you* commit. Uncommitted work is deliberately not
  mirrored: a diff that changes under a reviewer without a commit is a lie.

### Open the huddle

`herdr-huddle serve` opens the room; `herdr-huddle join` is everyone else's way
in. It is the difference between reading what the agent already said and being in
the room while it says it.

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

`serve` starts a quick tunnel itself and prints the one line to send (it needs
`cloudflared` on your machine; without it, it says so and serves this machine
only — `--no-tunnel` forces that). The collaborator installs nothing: no Herdr,
no tunnel client, no account.

**The link is the invitation.** When somebody who is not on the allowlist yet
turns up, they prove who they are on GitHub and you are asked, on your own
terminal:

```
herdr-huddle: @collaborator is at the door (github.com/collaborator).
  let them into the huddle? [y/N] y
13:42:07 let @collaborator in
```

One `y` and they are in for good: the login goes onto the share's allowlist, so
their pull-request comments start being delivered too, and a reconnect does not
ask you again. `--open` skips the question (anyone with the link and a GitHub
identity is in) and `--closed` never asks (the allowlist or nothing) — that one
is for a `serve` nobody is sitting in front of.

What the joiner sees is a room, not a pipe:

```
 pi · ~/Projects/demo                                                     [agent's pane]
   ▸ Read internal/live/server.go
   ▸ Edit internal/live/server.go
   …

── huddle ────────────────────────────────────────────────── acme/demo#13 ──
● working   you and @ana
@ana: delivered to the agent
› make the gate refuse an expired token▏
```

- **Everyone proves who they are before a single frame is observed.** The client
  presents a GitHub token — the stored one, or GitHub's device flow in a browser
  if there is none — and the gate resolves it to a login first. A refused joiner
  is told why (`@x was not let into this share`, `401: Bad credentials`) and sees
  nothing at all.
- **Each joiner gets their own read-only stream**, rendered at *their* terminal's
  size and freshly painted from the top, so someone arriving late sees a whole
  screen rather than the tail of one. Resizing the window repaints it; several
  people can be in at once, each at their own size.
- **The room knows what the agent is doing.** `working`, `idle`, or `waiting on
  the operator` — the one thing a silent pane cannot tell you.
- **Type a line and it reaches the agent.** It is delivered immediately — wrapped
  as a colleague's unverified message carrying your login, exactly the treatment
  `/agent` comments get — echoed to everyone else in the room, and recorded on the
  pull request afterwards. A held delivery (the agent is waiting for its operator)
  or a failed one is reported to the room; nothing is silently dropped.
- **Leaving is Ctrl-C.** It ends your stream and the observer behind it, the room
  is told, and nothing else changes.

If the pane is gone, the joiner is told why (`terminal target w16:p1 not found`)
rather than left with a blank screen. Piped somewhere that is not a terminal,
`join` writes the pane's bytes out plainly instead of drawing a room.

## The limits worth knowing before you rely on it

- **On a private repository, your collaborator needs at least read access to
  comment at all.** `--invite` asks for it (`permission: pull`). On a work org
  where you are not an admin, that request will fail — the share says so and
  carries on, and you have to get access granted another way. This is a hard
  GitHub limit, not something this tool can work around.
- **The permission gate is never delegated.** When the agent stops for approval,
  only you can answer it, at your terminal. Herdr refuses a prompt to a blocked
  agent outright, and this tool does not try. It posts a comment saying the agent
  is waiting, so your collaborator is not left guessing.
- **Everything posted into the pull request is secret-scanned first**, but the
  scan is a pattern match, not a guarantee. The local session file is the
  authoritative record; the pull request is a projection of it.
- **`/agent` text is untrusted input.** The prompt that carries it says so, and
  carries the author's login. That is a defence, not a sandbox: an agent that
  obeys a comment can still do anything you could do.
- **The endpoint is public while `serve` runs, and the door is the whole gate.**
  `serve` still listens on `127.0.0.1`, but by default puts a Cloudflare quick
  tunnel in front of it — and a quick tunnel cannot have Cloudflare Access in
  front either (it needs a zone of yours). So GitHub pairing — a token presented
  first, resolved to a login, and then either on the allowlist or admitted by
  you — is the *only* thing between that URL and your pane: no rate limit, no
  second factor. Refusals are logged on your side and reported to the joiner.
  `--no-tunnel` removes the exposure entirely. The token itself only ever
  travels over `wss://` or loopback; the client refuses a cleartext address.
- **`--open` makes the link the entire boundary.** It is the Live Share posture
  and it is a real trade: anyone who can see the URL and has any GitHub account
  is in, with no human in the loop. That is why it is a flag and not the
  default.
- **Letting somebody in is not reversible from here.** The admission is written
  to the share record, so it also makes their `/agent` comments deliverable.
  Taking it back means editing `shares.json` and restarting the poller.
- **A dead token refuses cleanly.** If GitHub no longer accepts the stored
  token, the gate says `GitHub could not confirm the token` and shows nothing;
  run `herdr-huddle auth login` again. (The same dead token stops `poll` from
  posting, so the two fail together.)
- **The token the collaborator pairs with is not saved** — it is used to prove
  who they are for that session only, and never replaces your repo-scoped one.
- **The pane stays read-only by construction; the steering path does not.**
  The stream runs `herdr terminal session observe`, which takes no input, no
  resize and no takeover, so no code path can write to the pane itself. What a
  joiner types goes to the agent's prompt instead — allowlist-gated, attributed,
  wrapped as third-party input — through the same door the poller uses for
  `/agent` comments. Typing into the pane directly stays discouraged: the agent
  would record it as the operator's own words.
- **The record trails delivery, by design.** Every delivered instruction is
  posted to the thread as a comment opening with this tool's marker, so the
  poller recognises it as ours and never delivers it a second time; it
  attributes its author and says it arrived live. GitHub's issue-comment API has
  no reply threading, so the record attributes the turn rather than nesting it.
  If GitHub is unreachable, records queue (up to 100) and are retried while
  `serve` runs — delivery never waits for them — and anything still queued at
  shutdown is logged, not hidden.
- **A resize costs a fresh stream.** `observe` takes no resize — that is what
  makes it read-only by construction — so a joiner whose window changes gets a
  restarted observer and a complete repaint. Dragging a window edge restarts it
  repeatedly. `--cols`/`--rows` are now only the fallback for a client that
  cannot measure itself.
- **The joiner's TUI does not emulate a terminal.** It reserves the bottom four
  rows and repaints them over whatever the pane drew, which is enough for a
  pane that paints in place and is not enough for scrollback: there is none, and
  the agent's own cursor is not visible, because the cursor is parked on the
  input line. The status line saying `waiting on the operator` is what tells you
  the agent is at a prompt.
- **Nothing starts the stream for you.** `serve` is a foreground command you run
  and stop; the plugin's startup hook still starts only the poller.
- **The transcript comes from the agent's own session file**, read by an
  adapter for that agent kind (`pi` or Claude Code). Herdr does not report a
  session for every kind, and there is no adapter for the ones it does not. When
  no file can be found, this falls back to
  reading the pane's terminal and labels the comment as a partial transcript —
  readable, but with no cursor for incremental sync and collapsed tool calls.
- **Nothing has driven the agent from a real comment yet.** Transcript comments
  have been posted for real — two on the first share's pull request — but the
  inbound direction, a collaborator's `/agent` comment reaching the agent, has
  only ever run against a fake server. See `## Known gaps` in ADR-002, ADR-003
  and ADR-004.

## Commands

```
herdr-huddle auth login [--repo owner/name] [--public]
herdr-huddle auth status
herdr-huddle auth logout
herdr-huddle share [--slug name] [--base ref] [--invite @user]... [--dry-run]
herdr-huddle poll [--once] [--interval 10s]
herdr-huddle serve [--pane id] [--open | --closed] [--no-tunnel] [--addr host:port] [--cols n] [--rows n]
herdr-huddle join [address] [--addr 127.0.0.1:8787]
```

`serve` opens one pane's huddle. The pane **and** the allowlist come from the
same active share — no share, no server, because there would be nothing to gate
it with; `--pane` picks which share when several are active. It starts a quick
tunnel unless `--no-tunnel` is given and prints the line to send, and it answers
the door unless `--open` or `--closed` decides for it. `join` takes that address
(or `--addr`), proves who you are, and draws the room in the current terminal —
type a line to send it to the agent, Ctrl-C to leave.

`share --dry-run` reports what would happen and changes nothing — no token
needed, no branch, no push, no pull request. `poll --once` makes a single pass and
prints what it did, which is the way to see what the daemon has been doing
without waiting for it. `poll` on its own is the daemon the plugin's startup hook
starts; it takes a pid lock, so starting it twice is harmless.

## Development

```
make check     # gofmt, go vet, go test -race
```

The live Herdr and live GitHub tests skip themselves outside a Herdr pane and
without a token, so the suite runs offline. Everything that writes to a
repository — branches, empty commits, pushes — is tested against a local bare
repository as `origin`, and everything that writes to GitHub is tested against
fakes.

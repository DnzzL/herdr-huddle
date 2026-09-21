# herdr-huddle

Turn a live Herdr agent session into a GitHub draft pull request that two people
can plan in, steer, and keep as a record.

You run an agent in a Herdr pane. You open a share: a branch, an empty commit and
a draft PR. From then on the agent's turns appear in the pull request as comments,
and anything a collaborator writes as a comment beginning with `/agent` is
delivered to the agent as a prompt. The pull request is the thread; the agent
stays in your terminal, under your control.

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

   `--invite` gives that user read access to the repository and adds them to the
   allowlist of people whose comments may drive the agent.

## What the two of you see

- **Your collaborator installs nothing.** No Herdr, no herdr-huddle, no token:
  the thread is a normal pull request, and the URL is the whole invitation. On a
  private repository they still need read access to comment at all — see the
  limits below.
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
```

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

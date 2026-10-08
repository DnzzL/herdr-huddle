# Before you rely on it

What the door does and does not protect, and the limits of GitHub, the record and the stream.

← [README](../README.md)

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

**The unproven paths are listed under [Status](../README.md#status)** —
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

# ADR-007 — the stream becomes a room: knock to join, one viewport each, presence on screen

- Status: accepted
- Date: 2026-09-27
- Context owner: Thomas Legrand
- Revises: ADR-005 (the join client, the fixed viewport, the allowlist as the
  only door key) and ADR-006 (pairing as the gate). Neither is overturned; both
  are finished.

## Context

ADR-005 and ADR-006 built a stream and a gate. What shipped is a *stream*, and
the product it is copying is a *room*. Three things are missing, and each of
them is named as a known gap in one of those ADRs rather than as a decision:

- **Joining is not easy.** The link is not the invitation. A collaborator can
  only get in if the operator ran `share --invite @them` *before* `serve` — and
  `--invite` also asks GitHub for repository access, which fails outright on a
  work org where the operator is not an admin (README, "The limits worth
  knowing"). So the failure mode is: operator sends the link, the guest runs
  `join`, and is refused by a list they cannot be added to without a second
  round trip. Live Share's whole premise is that the link is the invitation.
- **There is no room, only a pipe.** A joiner sees the pane and nothing else:
  not who else is in, not whether the agent is working, blocked or idle (ADR-005
  names the agent-status summary as unbuilt), not what the other collaborator
  just asked for. Two people steering one agent with no sight of each other is
  how the same instruction gets sent twice.
- **The viewport belongs to the wrong machine.** `serve --cols/--rows` fixes one
  size at startup for everybody (ADR-005: "the viewport is the server's, not the
  joiner's"). A narrow terminal wraps; a wide one is letterboxed. This is
  strange, because the server already runs **one `observe` per joiner** — the
  per-joiner size was always free and was simply never asked for.

And one implementation fact that forces a decision rather than a tweak: the
frames are a full-screen ANSI paint. `join` writes them to stdout and writes
delivery outcomes to stderr, so every `delivered to the agent` line is painted
*into* the viewport the next frame is about to repaint. There is no correct way
to add chrome to the current client. It has to composite.

## Decision

**A share's live half is a room: bounded by the operator at the door, drawn as a
room on every joiner's screen, and rendered at each joiner's own size.**

1. **The hello carries the joiner's viewport, and a `resize` record changes it
   mid-stream.** The server starts that joiner's `observe` at the size they
   asked for, clamped to sane bounds, and restarts it on resize — a restart is
   the right mechanism because Herdr's first frame is a complete paint, which is
   exactly what a resized terminal needs. `--cols/--rows` survive as the default
   for a client that asks for nothing.

2. **Knocking is the default way in, and the operator is the gate.** A joiner
   who proves a GitHub identity that is *not* on the allowlist is held at the
   door, and `serve` asks the operator, on their own terminal, in one keypress.
   An admitted login is appended to the share's allowlist and persisted, which
   has two consequences worth stating plainly: their PR comments become
   deliverable too — the two doors stay on one list, as ADR-005 requires — and a
   reconnect does not knock twice.

   `--open` admits any proven GitHub identity without asking (the link is the
   invitation, and the operator has decided that is enough) — **and does not
   persist it**. An open admission lasts as long as the server does, because
   "be convenient for the length of this huddle" is not the same decision as
   "trust this person's pull-request comments from now on"; only an answered
   knock is written to the share. `--closed` is today's behaviour: the
   allowlist or nothing, no prompt. The default is `--knock` because it is the
   only one of the three that is both frictionless for the guest and a
   decision the operator actually makes.

   **This does not relax the trust model; it moves the decision to where the
   evidence is.** The allowlist was set at `share` time, before anyone had
   asked to join, by an operator who had to predict who would want in. Knocking
   asks the same person the same question at the moment it matters, with the
   asker's proven login in front of them.

3. **The room is broadcast.** The server keeps the roster of who is connected
   and watches the agent's status (`herdr agent get <pane>`), and sends every
   joiner a `room` record whenever either changes. An instruction is echoed to
   the whole room as well as acknowledged to its author, so two collaborators
   see each other steer.

4. **`join` is a TUI.** The pane keeps the top of the terminal; the bottom four
   rows are ours — a rule, a status line (agent state, roster, the pull
   request), the last event, and an input line. Chrome is repainted after each
   frame batch, which is what makes it survive a full paint that clears the
   screen. Without a TTY, `join` falls back to today's raw render, so a pipe
   still works.

5. **The pull request stays the artifact and is now visible from inside the
   room.** The thread URL rides the `room` record and is drawn in the status
   line, because a huddle whose record nobody can find is not a record.

6. **`golang.org/x/term` is the second dependency.** Raw mode and window size
   are what a TUI is made of, and hand-rolling `termios` ioctls across Linux and
   macOS is more risk than a Go-team-maintained package. ADR-006 ended the
   zero-dependency property deliberately; this spends it a second time, for the
   same reason.

## Alternatives rejected

- **A join code minted by `serve` and pasted by the guest** (the link plus a
  shared secret admits anyone who holds both). Frictionless and stateless, but
  it replaces a *person* with a *secret*: the record would attribute turns to
  whoever holds the code, and the allowlist — which the comment path also reads
  — would have nothing to say. Rejected because the thread's attribution is the
  product.
- **Auto-admitting anyone with a valid GitHub identity, by default** (`--open`
  as the default). This is what Live Share does, and it is available as a flag,
  but as a default it turns an unguessable URL into the entire boundary with no
  human in the loop. Kept as an opt-in, not a default.
- **Widening `--invite` to ask GitHub for access at join time.** Does not help:
  the failure that makes joining hard is GitHub refusing the collaborator
  request on an org the operator does not administer. The knock works without
  GitHub granting anything.
- **Rendering the pane into a cell grid ourselves** and compositing properly (a
  real terminal emulator in the client). Correct, and the only way to give the
  pane a scrollback and the joiner a visible agent cursor. Rejected as far more
  code than the product needs: reserving rows and repainting chrome gets the
  same screen for a fraction of it.
- **Reserving the chrome with a DECSTBM scroll region only.** It confines
  scrolling but not `ESC[2J`, so it cannot stand alone; used *with* the repaint,
  not instead of it.
- **Resizing by asking Herdr to resize the stream.** `observe` takes no resize —
  that is the property ADR-005 depends on for read-only-by-construction. A
  restart is the only lever, and it is the one that yields a full repaint.
- **A separate presence channel / second connection.** One connection already
  carries both directions; a second one would need its own gate.

## Consequences

- `serve` acquires a foreground duty it did not have: answering the door. With
  `--open` or `--closed` it has none, and the flags exist for exactly the
  unattended case.
- `serve` now **writes** to the share record (the allowlist), where before it
  only read it. The poller reads the same file, so an admitted collaborator's
  comments start being delivered without either process restarting. That makes
  three processes writing one file, so two rules become load-bearing and are
  enforced in `share.Store` rather than in each caller:
  - **Every read-modify-write holds a lock** (`shares.json.lock`, an advisory
    flock, released by the kernel on exit so a crash leaves nothing to clear).
    Without it the last writer silently discards the others, and the costliest
    thing to discard is the poller's comment cursor — the agent would be handed
    an instruction it has already carried out.
  - **Fields have owners.** Cursors and retirement are the poller's; the
    allowlist is the door's. The poller therefore writes with `Save`, which
    keeps the allowlist that is on disk, because the record it holds was read
    at the start of its pass and may predate an admission by a minute. A lock
    alone does not fix this: it serialises the writes without making a stale
    copy less stale.
- Each joiner costs one `observe` child as before, plus one restart per resize.
  A joiner dragging a window edge restarts their stream repeatedly; the size is
  debounced for that reason.
- The agent-status watcher adds one `herdr agent get` per interval for as long
  as anyone is connected, and none when the room is empty.
- The client stops being a pipe, so `join`'s output is no longer something to
  redirect. The non-TTY fallback keeps that path honest rather than pretending.

## Known gaps

- not tested: the knock has never been answered by a real second person; the
  approver is exercised against a scripted stdin.
- not tested: resize restart is measured against a fake observer, not against
  `herdr terminal session observe` on a real pane.
- untouched: reconnect. A dropped tunnel still ends the room for everyone, and
  the client does not redial (ADR-006's gap, unchanged).
- untouched: the agent's own cursor is not visible in the TUI, because the
  chrome repaint parks the cursor on the input line. A pane waiting on a `y/n`
  looks the same as an idle one, except for the status line that now says so.
- fragile: chrome repaint after every frame means a full paint flickers four
  rows. Acceptable at terminal speeds; not measured through a tunnel under load.

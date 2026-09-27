# ADR-008 — two channels, two decisions, and a huddle that survives its tunnel

- Status: accepted
- Date: 2026-09-27
- Context owner: Thomas Legrand
- Builds on ADR-007 (the room), ADR-006 (the door), ADR-005 (one allowlist, two doors)

## Context

ADR-007 made the stream a room. Using it surfaced three things that the room's
shape could not express, and one thing Herdr already does that we were about to
build a second time.

- **Everything a joiner typed went to the agent.** There was one input line and
  one destination, so two collaborators could not say *"wait, don't touch the
  migration"* to each other without the agent doing something about it. This is
  not a missing feature; it is a missing channel. Every collaboration tool of
  this shape has one (Live Share chat, Zed chat), and the reason is the same:
  the side-talk that coordinates the work is not the work.
- **Admitting somebody and letting them drive were one decision.** The door
  (ADR-007) asks once, at the threshold, and from then on that person steers an
  agent running with the operator's permissions, in the operator's worktree, on
  the operator's quota. There was no tier between "watching" and "full control",
  which is exactly the tier you want for somebody outside the team.
- **A blip ended the huddle for everybody.** A quick tunnel is a quick tunnel;
  ADR-006 recorded "there is no reconnect yet" as a known gap and it stayed one.
  Nobody redialled, so one dropped connection meant every participant started
  again by hand — and in a terminal, "start again by hand" means finding the
  link somebody sent an hour ago.
- **Herdr already notifies on agent state change.** `notification.show` exists
  as an API method and a CLI verb, and the binary carries a
  `notification_sound_for_state_change_with_agent_labels` path plus `[ui] toast`
  / `[ui] sound` config and per-agent sound overrides. An alert when the agent
  becomes blocked was on our list; it is Herdr's, it is configurable, and
  building a second one would be a worse copy behind a different switch.

## Decision

1. **Room chat is a separate record type, and a separate method.** `Session.Chat`
   is not `Say` with a flag: "tell my colleague" and "tell the agent to do
   something" must never be one call separated by a boolean somebody can get the
   wrong way round. Server-side it touches neither the Instructor nor the
   Ledger.

   **Chat is not posted to the thread.** ADR-003 makes the pull request the
   record of the conversation *with the agent*; a pull request full of "one sec"
   and "wrong window" is a record of nothing. Side-talk is ephemeral and the
   README says so, so nobody mistakes the thread for a complete minute-book.

2. **The destination is drawn on the input line, always.** `agent ›` or
   `room ›`, switched with Ctrl-T. The expensive mistake here is silent — the
   agent acting on something meant for a person — so the mode is a *word*, not a
   colour, and it is never absent.

3. **`--moderated` splits the decision.** With it, every instruction is put to
   the operator with the words in front of them before the agent sees it. The
   joiner is told `waiting for the operator to approve it` — not silence, which
   reads as a dropped instruction — and a refusal reaches the whole room. A
   moderated instruction is handled off the connection's reader goroutine, so a
   joiner waiting on approval can still chat; ordering then belongs to the
   operator, which is the point of moderating.

4. **The client reconnects, and knows the two failures it must not retry.**
   Exponential backoff from 500ms to a 15-second ceiling, keeping the terminal
   it has already taken over. It gives up on exactly two things: the person left
   (Ctrl-C), and the person was **turned away** — the gate identified them and
   said no. The wire has to carry that distinction, so those two refusals are
   marked `fatal` on the error record and nothing else is.

   Being precise here is load-bearing, and the first attempt was not: it marked
   *every* gate failure fatal, so GitHub being briefly unreachable, or an
   operator who had stepped away from a knock, permanently ended a joiner's
   huddle. Those may work on the next attempt. Only "we know who you are and
   the answer is no" will not.

5. **Our questions go through Herdr's notification surface, and ring a bell.**
   `serve` asks the operator things, and the operator is watching their agent,
   not the log those questions are printed in. `herdr notification show` is the
   same door Herdr uses for agent state, so there is one place to configure and
   one place they appear — and because that toast is conditional on the
   operator's own `[ui]` settings, the terminal bell goes with it as the part
   nothing can switch off.

   The console that asks those questions has three properties, each of which
   cost a bug to learn: questions are answered **in arrival order** (a channel,
   not a mutex — Go promises FIFO for one and not the other); a question can be
   **abandoned** when the person who prompted it disconnects, or the operator
   would be left answering about somebody who is gone *while holding the turn
   that admits everybody else*; and an answer **answers the question in front of
   it**, so a `y` typed idly before anyone knocked cannot admit the next person
   who does.

6. **We do not build an agent-blocked alert.** It exists in Herdr, under
   `[ui] toast` / `[ui] sound`, and is what the operator should turn on. Joiners
   have no Herdr, and their signal stays the status line already saying
   `waiting on the operator`.

## Alternatives rejected

- **A `/` prefix for chat instead of a mode.** Cheaper and stateless, and
  rejected because the failure mode is one-way and silent: forget the prefix and
  the agent acts on a message meant for a person, with no way to recall it. A
  mode you can see beats a prefix you must remember.
- **Chat posted to the pull request** (as a comment, or a collapsed block).
  Rejected on ADR-003's terms: the thread is the record of the agent's work, and
  burying turns under coordination chatter makes it a worse record, not a fuller
  one.
- **Moderating by default.** It is the safe posture and the wrong default: the
  common case is two people who already trust each other, and asking the
  operator to approve every sentence makes the huddle slower than the pull
  request it replaced.
- **Per-joiner trust tiers** (`--invite @x --watch-only`). The right shape
  eventually, and more machinery than a share record can carry today; the
  moderator is per-share and that covers the case that exists.
- **Reconnecting inside `Session`**, so callers never notice. Rejected: the
  client has to be *told* it is reconnecting, and a transparent retry hides
  exactly the thing the room needs to show.
- **Our own notification path** (`notify-send`, a terminal bell, a desktop
  library). A second mechanism behind a second switch, worse than the one Herdr
  already ships and already configures.

## Consequences

- `go.mod` is unchanged; all of this is Herdr's CLI and the standard library.
- `serve` gains a second thing to answer, and both it and the door read the same
  stdin — so there is **one** console behind a mutex rather than two readers
  racing for one answer.
- The client's connection is no longer for the life of the process, so `App.Run`
  takes a `Dialer` rather than a session, and every redial re-reads the window
  size (it may have changed during the outage).
- A moderated share serialises steering behind a person, so a busy huddle is as
  fast as the operator. That is the trade being bought.

## Addendum — the queue is written down

ADR-006 recorded that owed instruction records were kept in memory only, so a
`serve` restart threw away whatever the thread had not accepted. That is now
closed, because it was the one open gap that lost *data* rather than comfort:
the pull request is the artifact, and an instruction the agent carried out but
the record never explains is a turn nobody can account for.

Owed records are written to a file beside the share records, atomically and
0600, and carried over on the next start — oldest first, so a restart pays its
predecessor's debts before its own. **One file per share, never one shared
file**: the records are posted to a specific pull request, so a restart that
recovered another share's queue would file one thread's instructions under
another's. The spool stays optional on the server, which is what keeps the
tests free of a filesystem.

## Known gaps

- not tested: `--moderated` and the knock have never been answered by a real
  second person; the console is exercised against a scripted stdin.
- not done: the moderation queue shows the operator a count of what is waiting
  ("2 more waiting") but not its contents, so there is no way to answer the
  third question first, or to see what is coming.
- not done: the non-TTY path (`joinPlainly`, for a pipe) does not reconnect.
  Retrying into a pipe whose reader wants a clean end is the wrong default, so
  this is a decision rather than an omission — but the README must not claim
  reconnect for it.
- fragile: the notification half of a knock depends on the operator's own
  `[ui] toast` / `[ui] sound`. A failed `notification show` is logged, and the
  bell and the terminal prompt are what remain.
- not tested: reconnect is verified against a killed and restarted local server,
  not against a real `cloudflared` tunnel dropping — which is the case it exists
  for, and which ADR-006 already records as unmeasured.
- fragile: a reconnect gets a new quick-tunnel URL if the *operator* restarts
  `serve`, so the client redials an address that no longer exists and backs off
  forever. Reconnect survives a network blip, not an operator restart.
- untouched: chat has no history — it lands on the one event line and is gone.
  The room-log panel ADR-007 declined is now more obviously wanted, because a
  message you scrolled past is worse than a delivery you scrolled past.
- unknown: whether chat should be rate-limited. Nothing bounds how fast a joiner
  may send messages to the room beyond the 2000-byte cap on each.

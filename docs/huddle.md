# The huddle

The live half: `serve` opens your room, `join` is everyone else's way in.

← [README](../README.md)

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

`serve` starts a [quick tunnel](adr/ADR-006-quick-tunnel-door.md) itself and
prints the one line to send. It needs `cloudflared` on your machine; without it
it says so and serves this machine only, which `--no-tunnel` forces. The
collaborator installs nothing else: no Herdr, no tunnel client, no account.

## The link is the invitation

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

## The host's room

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

## Letting somebody in is not letting them drive

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

## What the joiner gets

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

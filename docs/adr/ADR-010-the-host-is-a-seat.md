# ADR-010 — the host is a seat in their own room

- Status: accepted
- Date: 2026-10-08
- Context owner: Thomas Legrand
- Revises: ADR-007 (the operator is "in the room without being connected")

## Context

Joiners got a room (roster, typing, chat, outcomes). The operator got a log:
they could not see who was typing, read the chat, or answer the door without
a second terminal. "The host is at the pane" was true and was also why they were
blind to everything the room knew.

## Decision

`Server.Attach()` seats the operator in the room as `Server.Host`, in process,
with no socket and no stream. The seat has a joiner client's shape (`Run`,
`Chat`, `Typing`, `Close`) so one terminal UI drives both; `tui.App.Host` draws a
panel of what happened where a joiner's pane would be. `Say` is refused: the
host steers from the agent's own pane, so nothing is ever typed into it from here.

The door's and the moderator's questions move from a stdin line reader into the
panel (`App.Ask`), because the TUI owns the keyboard and two readers on one
terminal eat each other's keys.

The plugin action no longer runs `serve` in the background: `serve --split`
opens a pane to the right of the agent's (`herdr pane split --no-focus`) and runs
`serve --pane=…` there, so the chat and the typing marks are beside the work.
This replaces ADR-009's "join line as a notification" for the action.

Two follow-ups in the same change. A typing claim now says who it is for
(`to: "room"`; the room record's `chatting` is the subset of `typing` writing to
the room), and a message to the room claims typing as an instruction does —
revising ADR-007's rule that only instructions did — because a host writing in
their own pane was otherwise invisible. And the joiner's footer keeps history
(`Extra` rows above the four chrome rows, `Ctrl-L` to grow it), because one
event line was too little to follow a conversation.

## Rejected

- **A server-side event stream plus a host-only renderer.** A second protocol
  for the same facts the room already broadcasts, and two renderers to keep in step.
- **Toasts for typing.** The operator is watching the agent, but a toast per
  burst is noise and carries no history.
- **Letting the host steer from the panel.** A second way in would put their
  words on the pull request as a joiner's and blur "the agent is under your control".

## Known gaps

- untouched: panel rows are cut at the window's width, not wrapped, and there is
  no scrolling back past the last screenful.
- fragile: the new pane's shell is a fresh one, so `GH_TOKEN` exported in the
  shell that ran the action is not there; only a stored token or the user's
  profile reaches it. The token is deliberately not passed on a command line.
- not tested: `--split` has run from a shell, not from a Herdr action.
- not tested: the door's knock has been answered through `App.Ask` in tests only,
  not by a second GitHub identity.

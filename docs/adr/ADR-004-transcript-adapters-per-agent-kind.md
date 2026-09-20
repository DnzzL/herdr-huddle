# ADR-004 — one transcript adapter per agent kind

- Status: accepted
- Date: 2026-09-20
- Context owner: Thomas Legrand

## Context

ADR-001 makes the agent's own session file the transcript source, in order: a
session the agent reports, then the session file whose records name the pane's
working directory, then the terminal as a labelled fallback. It was written with
Claude Code in mind, and the first implementation hardcoded that:

- `internal/session` looked only in `~/.claude/projects`.
- `internal/transcript` parsed only Claude Code's record shape (`type:
  user|assistant`, `isMeta`, `content[]` of `text`/`tool_use`/`tool_result`,
  `uuid`/`parentUuid`).

That is a problem for the person this was built for. **Every agent on the
operator's machine is `pi`** — six live panes, and `herdr agent list` reports
`agent_session: none` for all of them. So on the machine this exists to serve,
the good path never runs: every share would sync through the terminal fallback,
which is TUI chrome, has no cursor, and collapses tool calls.

Measured on a real pi session (`~/.pi/agent/sessions/--home-thomas-Projects-herdr-huddle--/
2026-09-17T07-54-49-257Z_01a0ae5c-…jsonl`, 900 records, 3.0 MB):

- Layout is `<session root>/<cwd-with-separators-replaced>/<timestamp>_<uuid>.jsonl`,
  which is the same two-level shape Claude Code uses.
- The first record is `{"type":"session","version":3,"id":…,"cwd":…}`, so the
  same "identify the file by the `cwd` its own records name" rule works, and
  there is an `id` per record to use as a cursor.
- Records are `type: message` with `{id, parentId, timestamp, message:{role,
  content}}`, roles `user` / `assistant` / `toolResult`, and blocks `text` /
  `thinking` / `toolCall` (`name` + `arguments`).
- A `toolResult` is its own record (`toolCallId`, `toolName`, `isError`,
  `content[]`), not a meta-wrapped block inside a user record.
- `compaction` records carry a `summary` (14,716–44,259 characters in this
  session) that replaces the compacted turns in what is *sent to the model*.
- **Compaction does not prune the file.** Records older than every compaction's
  `firstKeptEntryId` are still present, and all 900 ids are unique. So a cursor
  never dangles because of compaction, and the file is a complete log.
- Pi writes no system prompt into the file. The only `<system` / `CLAUDE.md`
  text present is inside tool *results* — content the agent read, which belongs
  in the transcript anyway.

Herdr reports the agent kind in `Agent.Agent`; the two values observed are `pi`
and `claude`.

## Decision

- **The transcript source is chosen by the agent kind Herdr reports**, not by
  sniffing the file. `herdr.KindPi` and `herdr.KindClaude` are the two kinds with
  an adapter.
- **An unknown kind keeps the labelled terminal fallback**, with the reason
  naming the kind. It is not guessed at, and it is not an error: a new agent
  that works in a terminal has a usable, if degraded, transcript.
- **Each kind gets its own parser and renderer**, in the same package as the
  first one, sharing what is genuinely format-independent: the redaction pass,
  the truncation cap, the indented (never fenced) tool-result body, the timestamp
  format, and the human/agent/tool-result labels, which are asserted per adapter
  so they cannot drift apart. The on-disk shapes are different enough that one
  parser with branches inside it would be two parsers wearing a coat.
- **The share record remembers the kind** it was opened with. If the live agent's
  kind no longer matches, the transcript is re-resolved rather than rendered with
  the wrong parser — a wrong parser that knows no matching record type renders
  nothing at all, and silence is the one failure nobody notices.
- **A compaction is posted as a visible marker**, carrying the summary capped at
  4,000 characters with the cap itself marked (an excerpt, not a silent cut),
  plus a line saying the session file holds the full summary. It travels with the
  next turn, because the cursor is what makes a record new: a marker is posted
  once, not on every pass.
- **The cursor is a record id**, renamed from `LastUUID` because pi has no UUIDs.
  Both formats return "the id of the last record that has one".

## Consequences

- A share opened from a `pi` pane now syncs the real transcript: prose with tool
  calls in order, tool results indented, thinking behind `<details>`, and
  incremental by cursor. On this machine that is every share.

  Verified against the real session the measurement above came from (3,423,261
  bytes, 999 records): parses in 51 ms, renders 1,393,327 characters with
  thinking included, and contains human turns, agent turns, tool calls, tool
  results, thinking blocks and the compaction marker. A cursor at the end renders
  nothing; a cursor in the middle renders only what follows it; a cursor that is
  not in the file renders everything and reports `Truncated`.
- **The wrong adapter is silence, which is why the kind decides the source.**
  Measured: the Claude adapter on that same pi file renders 0 characters. A
  string of records it does not recognise is not an error, it is an empty turn,
  and an empty turn posts nothing and looks like a quiet agent. Hence an unknown
  kind is an error in `thread`, and the terminal in `session`, rather than a
  default.
- The thread is no longer silently truncated where the agent's context was
  compacted. What the collaborator sees matches what the agent can still
  remember, which is the honest thing to show.
- Adding a third agent costs one adapter and one entry in the kind switch. The
  existing ones do not change.
- Two parsers to keep working, each with real fixtures. The Claude adapter's
  tool_use/thinking paths are still unverified against a real Claude session
  (Claude Code is not logged in on this machine); the pi adapter is verified
  against a real one.
- Redaction runs per adapter, so a new adapter that forgets it would post
  unredacted output. It is asserted per adapter in tests rather than left to
  review.

## Known gaps

- not verified: pi's `toolCall` and `toolResult` shapes are covered from one
  real session. A tool that writes content in a shape that session never used
  renders as nothing rather than as an error.
- not verified: the Claude adapter's `tool_use`, `thinking` and sidechain paths
  still have no real fixture, because Claude Code is not logged in on this
  machine. They are covered by hand-written records only.
- untested: `findByID` for pi. Herdr reports no session id for any pi agent, so
  the path is exercised by unit tests and never on a live pane.
- fragile: the poller reads and parses the whole session file on every pass
  (51 ms and 3.4 MB at the time of writing, every 10 s per active share). Tailing
  the file would cost less; it was left alone because the cursor is what makes
  the render cheap, not the read.
- unknown: whether a compaction summary is better capped at 4,000 characters.
  Measured summaries ran 14,716 to 44,259 characters; the cap is a judgement
  about a planning thread, not a technical limit, and it is one constant to
  change.

## Alternatives rejected

| Alternative | Why rejected |
|---|---|
| **Keep Claude only, rely on the terminal fallback for pi** | The fallback is the degraded path by ADR-001's own reasoning: no cursor, collapsed tool calls, TUI chrome. Shipping only that to the machine this was built for is shipping the demo, not the product. |
| **Sniff the file format instead of using the agent kind** | The kind is known, explicit and already in hand. Sniffing guesses at something Herdr tells us, and guessing wrong renders nothing. |
| **One parser with per-format branches inside it** | The two record shapes share almost no structure. Branches would make every future change touch code for the other format. |
| **Translate pi records into Claude's `Record` type** | A lossy translation maintained in one direction, for a renderer that then reads fields pi never wrote. Cheaper to walk pi's records directly. |
| **Post compaction summaries in full** | Measured at 14k–44k characters each, and there were seven in one session. One of them would dominate a planning thread that already contains the turns being summarised. |
| **Render nothing for compaction** | The thread would stop making sense mid-conversation with no explanation of why, and the collaborator would have no way to tell a gap from an omission. |
| **Detect a compacted session by a missing cursor** | Compaction does not prune the file. A missing cursor means a rotated or replaced session, which is a different thing with a different response. |

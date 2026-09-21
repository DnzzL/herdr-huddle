# ADR-003 — what the shared thread contains, and when a share ends

- Status: accepted
- Date: 2026-09-18
- Context owner: Thomas Legrand

## Context

ADR-001 decided that the transcript goes into the pull request **body**,
rewritten each agent turn, and that the poller (re)starts from a startup hook and
"exits when no share is active". Two things turned out to be wrong or missing
once the mechanics existed:

- A rewritten body reaches nobody. GitHub sends no notification when a pull
  request description is edited — not email, not the notifications inbox, not the
  timeline. This has been an open gap for years
  ([isaacs/github#310](https://github.com/isaacs/github/issues/310), 70+
  reactions). The Plan phase exists so a second person reads the agent's work,
  and that person is never told anything arrived. The one surface that pushes to
  a person is a **comment**.
- Body content is capped at 65,536 characters and the API **rejects** longer
  bodies rather than truncating them (`body is too long (maximum is 65536
  characters)`). A transcript is a stream that grows without bound; a body is a
  document. The sync would stop working exactly when the thread had become worth
  reading.

Separately, nothing defined what a *share* is bound to on the operator's
machine. The share record as first written holds a repository, a branch, a pull
request number and an allowlist — where to post, and nothing about what to
drive. Nothing recorded which agent, which pane, or which session file, so the
poller could not inject a comment and the sync could not find the transcript.
ADR-001's "exits when no share is active" also had no definition of *active*.

## Decision

- **The body is a header, written once at `share` time.** It says what the thread
  is, and the rules a comment must satisfy to reach the agent. It is not
  rewritten.
- **The transcript travels as pull request comments**, append-only, one comment
  per completed agent turn, using the `LastUUID` cursor the renderer already
  returns to know what is new. Nothing is rewritten, so nothing anyone has quoted
  moves under them.
- **A comment is only posted when the agent is not `working`.** Herdr reports
  `agent_status` on the host, so a turn — including its tool calls — is posted as
  one comment when the agent pauses, rather than a comment per poll.
- **A turn larger than the 65,536-character cap is truncated** with an explicit
  marker naming the truncation, rather than failing the sync.
- **The share record holds the operator's local state**: the agent target, the
  pane id, the session kind and value, the working directory, the transcript
  cursor, and the last seen agent status. This is what lets the poller find the
  transcript and reach the right agent.
- **A share ends when its agent or pane is gone.** That is what makes "no share
  is active" mean something. The record is retired rather than deleted, so a
  share can be re-opened without losing its history.
- **The local JSONL is the record; the PR is a projection.** The session file
  holds the complete conversation — thinking, tool calls, everything. What
  reaches GitHub is a deliberate, redacted subset. This is what licenses
  redaction, and it is why the PR body is not expected to be complete.
- **Uncommitted state is deliberately not mirrored.** The diff is empty until the
  operator commits, and stays a truthful snapshot of what they chose to commit. A
  live mirror of the working tree would change under the reviewer and would show
  things the operator has not decided to keep.

## Consequences

- The Plan phase now notifies. A comment also quotes, threads, and is searchable;
  a body only ever holds the latest state.
- The body no longer reflects current state. That is intended: it is a header.
  A summary kept current by the agent is additive to this decision, not a
  replacement for it.
- Posting a comment needs a cursor in the share record, which is the same record
  the poller needs anyway.
- One long turn can still exceed the cap; truncation is lossy and visible, not
  silent.
- Retiring on agent exit means a share whose agent died is never synced again
  even if the session file kept growing. Re-opening the share resumes it.
- Redaction is now load-bearing rather than precautionary, since the projected
  output is all a collaborator ever sees.

## How it works out in practice

Written after the poller existed, so these are observations rather than
intentions. Each one is a decision that only became visible once the code ran.

- **The queue reaction is `eyes`.** GitHub has no 🚧 and refuses an unknown
  content with a 422, so ADR-001's 🚧 became `eyes`. The rest of the queue
  holds: the acknowledgement is written to a high-water mark in memory, the
  comment cursor does not move, and the instruction is delivered on the first
  pass that finds the agent free — in written order, because delivery stops
  rather than skipping ahead to something it can act on now.
- **A blocked agent gets one comment per episode.** Not per pass, or a thread
  would fill with the same "waiting for approval" line every 10 seconds. The
  flag clears when the agent stops being blocked, so the next block is announced
  again — a new approval is a new question.
- **Renaming or moving the agent does not move the share.** A share is bound to a
  **pane**, because that is the only stable handle Herdr offers: an agent's
  name is reassigned on rename and cleared when it exits, while the branch and
  the pull request have to outlive it.
- **The ETag is remembered before anything is delivered** in a pass, and a 304 is
  a normal answer. Reading the thread is the one call that happens every 10s, so
  it is the one that has to be free.
- **A cursor advances only after the write it describes succeeded.** A failed
  post or injection is retried rather than skipped, which is the direction that
  loses nothing.
- **Three different lengths are handled three different ways.** A turn longer
  than the 65,536-character comment limit is cut with a marker naming the cut.
  A single tool result longer than 4,000 characters is cut inline with an
  ellipsis, because one `cat` of a large file would otherwise be the entire
  comment. A session whose cursor is gone is not published at all: the cursor is
  rebased and the operator is told, because a half-read file presented as "what
  is new" misrepresents the conversation, and the operator is never the one who
  reads the thread.
- **A rotated session is reported, not reposted.** When the session file is
  replaced the cursor is rebased and the operator is told, rather than dumping
  the new conversation on the thread.
- **Opening a share does not replay the conversation you already had.** The
  transcript cursor starts at the end of the session, so the thread begins with
  the first turn after the share was opened. Posting the history would notify the
  collaborator about a week of work in one burst.
- **The first GitHub write happens in the poller, not in `share`.** `share`
  writes the body and opens the pull request; everything after that goes through
  the poller, which means a share whose agent is busy posts nothing until the
  agent pauses. This is the behaviour the "one comment per completed turn" rule
  implies, and it is worth knowing before wondering why a fresh share looks
  empty.

## Known gaps

- not tested: no transcript comment has ever been posted to a real pull request.
  The comment API is exercised against a fake server with the real request
  shapes. The share that would have posted the first one (DnzzL/molkky#13) was
  recorded without its origin, so the poller retired it — see ADR-002's gaps.
- not done: the process that reads a thread does not exist on the collaborator's
  side, because they do not run anything. If the operator's machine is asleep,
  their comment waits.
- not done: a share whose agent has exited is retired and kept forever. Nothing
  prunes retired records, and nothing reaps the branches and pull requests.
- not done: a token refreshed or revoked while the poller is running is not picked
  up until it restarts.
- fragile: a session path reported outside the operator's home directory is
  refused, so an agent run with an unusual home falls back to the terminal
  transcript.
- unknown: whether a long, pre-existing session chosen mid-conversation should
  post its tail. Today it does, bounded only by the truncation guard.
- unknown: whether posting the operator's own prompts from the session file is
  noise. They are currently included, since they are half of the conversation.

## Alternatives rejected

| Alternative | Why rejected |
|---|---|
| **Rewrite the transcript into the body (ADR-001 as written)** | Notifies nobody, and the API rejects it past 65,536 characters — the two failure modes above. |
| **Body as an agent-maintained summary, comments as the transcript** | The better artifact, and additive: it needs something to produce the summary, which is a new dependency on the agent doing work on request. Build the comment stream first. |
| **One comment per polled change** | Puts every tool call in the thread as its own comment. The thread becomes a log to scroll, not a conversation to read. |
| **Mirror `git status` into the thread** | The diff stops being a snapshot of what the operator committed. Reviewing a moving target is worse than reviewing less. |
| **Delete a share's record when its agent exits** | Loses the cursor, so re-opening the same workstream re-posts the whole transcript. |

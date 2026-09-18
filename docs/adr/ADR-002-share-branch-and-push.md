# ADR-002 — `share`: branch naming, base, idempotency, and pushing

- Status: accepted
- Date: 2026-09-18
- Context owner: Thomas Legrand

## Context

ADR-001 fixes what a share *is* — a GitHub draft PR opened on an empty branch
before any code exists — but not the mechanics of creating one. Four things it
leaves open, all of which `share` must decide:

- What `<slug>` is. The branch name `herdr/<slug>` comes from the P2 plan, not
  from ADR-001. ADR-001 uses `<slug>` for something else entirely: Claude Code's
  project directory under `~/.claude/projects/<slug>/`, which is the session's
  working directory with `/` replaced by `-`. The two are unrelated and sharing
  the name is a trap; this ADR calls the branch one the **share slug**.
- What the empty commit is cut from, and therefore what the PR's diff means.
- What a second `share` in the same workstream does.
- Whether `share` pushes.

`share` is the first command that writes to the operator's repositories, so
these are worth pinning before the code exists.

## Decision

- **Share slug**: `--slug` when given; otherwise the current git branch name,
  unless that is the repository's default branch, in which case the project
  directory name. So a worktree on `improve-pane` shares as
  `herdr/improve-pane`, and a share from `main` in `~/Projects/docket` shares as
  `herdr/docket`. A branch rather than an agent name, because the branch outlives
  the agent session while a name is reassigned on rename and cleared on exit.
- **Base**: the empty commit is cut from `origin`'s default branch, read from
  `refs/remotes/origin/HEAD`, so the PR's diff means "everything this workstream
  does relative to upstream". `--base <ref>` overrides. When `origin/HEAD` cannot
  be resolved — a repository set up with `git remote add` and `fetch` has none —
  the commit is cut from local `HEAD` with a printed warning, *and* the PR's base
  is taken from GitHub's own `default_branch`. A PR with no base is rejected with
  a 422 that says nothing useful, after the branch has already been pushed, so
  GitHub is asked before anything is created; if it cannot answer either, `share`
  stops and asks for `--base`.
- **No fetch.** `share` never touches the network for git. It uses the remote
  refs already present, so it cannot fail because a fetch was blocked or slow.
- **Idempotent.** Re-running `share` with the same slug reuses the existing
  branch and its open PR, printing the URL, and creates nothing. The PR is the
  thread; a second one would split it.
- **It pushes**, and says what it is about to do first: repo, branch, base. The
  push is what makes the PR possible, so making it a separate manual step is a
  worse version of the same thing. `--dry-run` stops after printing.
- `--invite @user` issues the `PUT .../collaborators/{user}` from ADR-001 once
  the PR exists, and reports the failure rather than hiding it when the operator
  lacks admin rights. The allowlist of comments that may drive the agent is the
  operator's own login first, then each successful invitation: without the
  operator on it, the person who owns the agent could not steer it from the PR,
  which is the loop the whole thing exists for.

## Consequences

- `share` on a repository whose default branch is checked out produces
  `herdr/<dirname>`, which is a guess at intent. It is visible in the output and
  `--slug` corrects it, so the cost of being wrong is one flag.
- Not fetching means a stale `origin/HEAD` produces a PR whose diff includes
  commits upstream already has. Visible in the PR, correctable by fetching.
- Idempotency is keyed on the branch name, so two different workstreams that
  derive the same slug share one PR. `--slug` is the escape hatch.
- Pushing is irreversible only in the sense that a branch must be deleted; no
  history is rewritten, and the draft PR is closed rather than the branch forced.
- The token is checked, and the operator's login read, before anything is
  written; a share never leaves a pushed branch behind because authorization
  failed. `--dry-run` is exempt, since it makes no requests.
- The `default_branch` lookup is one extra request, on the fallback path only,
  and skipped by `--dry-run` because a dry run makes no requests at all. A dry
  run therefore reports the base as unknown when local refs cannot say.

## Known gaps

- not tested: no push and no pull request has ever run against GitHub. The
  branch, empty commit, base selection, idempotency and push are exercised
  against a local bare repository, and the API against a fake server.
- not done: `share` records the share and opens the thread, but the transcript
  arrives only once the poller runs — see ADR-003 for why the body stays a
  header and what that means for a fresh share.
- not done: nothing reaps a share's branch or pull request. A retired share keeps
  both, deliberately, and its record is kept forever.
- unknown: whether a work-org installation permits collaborator invitations at
  all. The failure is a warning by design, and the operator's own login on the
  allowlist is what keeps the loop usable when it fails.
- fragile: slug derivation on a repository whose default branch is checked out
  produces `herdr/<dirname>`, which is a guess at intent.

## Alternatives rejected

| Alternative | Why rejected |
|---|---|
| **Cut from local `HEAD` always** | The PR diff then includes whatever else that branch already carried, so it stops meaning "this workstream". |
| **Slug from the agent name** | Reassigned on rename and cleared when the agent exits, while the branch and PR must outlive it. |
| **A fresh branch per share** | Duplicate PRs for one conversation, which is exactly the split ADR-001 exists to avoid. |
| **Never push; print the commands** | The shared object is a GitHub PR. Printing a two-step manual recipe delivers the same thing with more room to get it wrong. |
| **Fetch before cutting the base** | Makes a local-first command fail on a network condition, for a diff-quality improvement the operator can get with `git fetch` when they care. |

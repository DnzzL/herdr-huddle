# Contributing

The scope is deliberately narrow: **a huddle is one agent, opened to a few people
you trust, with a pull request as the record.** A hosted service, a
multi-agent orchestrator and a screen share belong to other tools. A PR that
grows the plugin past that line is likely to be declined on scope rather than
on quality — open an issue first and we'll work out whether it fits before you
write it.

## What is most wanted

- **The unproven paths** listed under [Status](README.md#status): `--invite` on a
  work org, a refused or held instruction against the real API, a second person
  joining from a second machine. A report of what broke is worth more than code.
- **Agents tested in the wild.** `pi` and Claude Code have a transcript adapter;
  anything else is read from the pane's terminal. Reports for `codex`,
  `opencode`, `gemini` or `cursor` are welcome.
- **Token refresh.** GitHub's 8-hour tokens expire and nothing renews them.
- **macOS reports.** CI runs there; nobody has used it there for long.

## Working on it

```bash
make check                  # gofmt, go vet, go test -race — CI enforces all three
make build                  # bin/herdr-huddle
herdr plugin link .         # run your checkout as the installed plugin
```

The suite is offline: live Herdr and GitHub tests skip themselves without a pane
or a token, and everything that writes is tested against fakes or a local bare
repository. `herdr plugin link` replaces a GitHub install, so a rebuild is
enough to try a change.

## House style

- **Comments explain why, not what.** The code says what it does.
- **Test first, through the seam** of the package — its public interface — not
  its internals.
- **No hidden behaviour.** If it would do something nobody asked for — retry,
  admit, clean up — report it and let the host decide.
- **A structural decision** (a protocol field, a module boundary, a dependency)
  gets a short ADR in `docs/adr/`, with the alternative you rejected.
- **Docs are state, not history**: if your change makes a `.md` wrong, fix it in
  the same pull request.
- Every user-visible change gets a `CHANGELOG.md` entry describing what it means
  for someone using the plugin, not what was refactored.
- Commit titles follow Conventional Commits (`feat(live): …`). French or
  English; the history has both.

A security problem goes through [SECURITY.md](SECURITY.md), not a public issue.

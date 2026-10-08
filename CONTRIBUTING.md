# Contributing

Thanks for looking. Small, focused changes are the easiest to merge.

## Setup

You need Go 1.26+. Herdr is only needed to try the plugin for real.

```
make check     # gofmt, go vet, go test -race — what CI runs
make build     # bin/herdr-huddle
```

The suite is offline: live Herdr and GitHub tests skip themselves without a
pane or a token, and everything that writes is tested against fakes or a local
bare repository.

## Before opening a pull request

- **Test first**, through the public seam of the package, not its internals.
- **A structural decision** (a protocol field, a module boundary, a dependency)
  gets a short ADR in `docs/adr/`, with the alternative you rejected.
- **Docs are state, not history**: if your change makes the README wrong, fix it
  in the same pull request.
- Commit titles follow Conventional Commits (`feat(live): …`, `fix(tui): …`).
  French or English; the history has both.

## Reporting a bug

Say what you ran, what you expected and what happened, with the output of
`herdr-huddle --version` and `herdr --version`. For a security problem, see
[SECURITY.md](SECURITY.md) instead.

# Changelog

What changed for someone using the plugin. Dates are release dates.

## v0.1.0 — 2026-10-08

First public release.

- **Open a coding agent in a Herdr pane to other people.** `serve` prints one
  link; a guest runs `herdr-huddle join` and watches the agent live, in their
  own terminal at their own size, and types to steer it. Each instruction
  reaches the agent as a colleague's message carrying their GitHub login.
- **A pull request is the record.** `share` opens a draft PR; the agent's turns
  arrive as comments, so does every instruction anyone gave, attributed. A
  comment starting with `/agent` steers the agent from people who never open a
  terminal.
- **You answer the door.** Somebody not on the allowlist knocks, you press `y`.
  `--open` and `--closed` change that; `--moderated` puts each instruction to
  you first.
- **Your own room.** `serve` draws who is here, who is typing (to the agent or
  to the room), the room's chat and the questions you answer. The plugin's
  **huddle** action opens it in a pane beside the agent (`serve --split`).
- **A guest's footer you can follow**: recent history above the input line,
  `Ctrl-L` to make it taller, `?` for every key.
- `herdr-huddle --version`. Prebuilt, checksum-verified binaries for Linux and
  macOS (arm64 and amd64), so `herdr plugin install` needs no Go toolchain.
- Not proven yet, and said so under *Status* in the README: `--invite` on a work
  org, a refused or held instruction against the real API, and token refresh.

# Security

herdr-huddle opens a coding agent to other people, so the boundary matters more
here than in most tools. Reports are welcome and taken seriously.

## Reporting a vulnerability

Please **do not open a public issue**. Use GitHub's private reporting instead:
[Report a vulnerability](https://github.com/DnzzL/herdr-huddle/security/advisories/new).

Include what you did, what you expected, what happened, and the output of
`herdr-huddle --version`. Expect an acknowledgement within a few days.

## What counts

- Getting into a huddle without being let in: a bypass of the allowlist, the
  knock, or the GitHub pairing at the door.
- Anything that reaches the agent, or the host's terminal, other than a
  delivered instruction wrapped as a colleague's message.
- A token or secret leaking: into a log, the pull request, or another joiner.
- Input from a joiner or a pull-request comment that is not bounded or escaped.

## What the model is

The door is the gate: a GitHub token, proven to a login, then either on the
allowlist or admitted by the host. There is no rate limit and no second factor.
Letting someone in is giving them your agent, with your permissions. The
README's *Before you rely on it* says the same, with the detail.

## Supported versions

The latest release. The project is young and does not backport.

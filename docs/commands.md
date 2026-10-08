# Commands

Every command and flag.

← [README](../README.md)

```
herdr-huddle --version
herdr-huddle auth login [--repo owner/name] [--public]
herdr-huddle auth status
herdr-huddle auth logout
herdr-huddle share [--slug name] [--base ref] [--invite @user]... [--dry-run]
herdr-huddle poll [--once] [--interval 10s]
herdr-huddle serve [--pane id] [--open | --closed] [--moderated] [--notify] [--plain] [--no-tunnel] [--addr host:port]
herdr-huddle join [address] [--addr 127.0.0.1:8787]
```

`serve` opens one pane's huddle. The pane **and** the allowlist come from the
same active share — no share, no server, because there would be nothing to gate
it with; `--pane` picks which share when several are active.

`share --dry-run` reports what would happen and changes nothing: no token needed,
no branch, no push, no pull request. `poll --once` makes a single pass and prints
what it did, which is how to see what the daemon has been doing without waiting
for it. `poll` on its own is the daemon the plugin's startup hook starts; it takes
a pid lock, so starting it twice is harmless.

#!/bin/sh
# Start the poller and get out of the way.
#
# Herdr's startup hooks are one-shot initialization commands, not supervised
# daemons, and Herdr starts them asynchronously without waiting for or reaping
# them (verified in the P0-A spike). Two consequences shape everything here:
#
#  1. The poller has to be detached from this process, because this process
#     exits immediately.
#  2. Its stdio has to be redirected, because a child that inherits the hook's
#     descriptors keeps Herdr's plugin log entry stuck at "running" forever.
#
# The hook re-fires on every server start and on a live handoff, so it may well
# be started while one is already running. That is fine: the poller takes a pid
# lock first and the second copy exits with a message rather than delivering
# every instruction to the agent twice.

set -u

root="${HERDR_PLUGIN_ROOT:-$(pwd)}"
bin="$root/bin/herdr-huddle"
state="${HERDR_PLUGIN_STATE_DIR:-$root/state}"
log="$state/poll.log"

if [ ! -x "$bin" ]; then
	echo "herdr-huddle: $bin is missing or not executable; build it with 'make build' in $root, then restart the Herdr server" >&2
	exit 1
fi

mkdir -p "$state" || exit 1

# setsid puts the poller in its own session, so a signal aimed at the hook's
# process group does not take the poller down with it. It is absent on some
# systems; a plain background job is still better than keeping the hook alive.
if command -v setsid >/dev/null 2>&1; then
	setsid "$bin" poll >>"$log" 2>&1 </dev/null &
else
	"$bin" poll >>"$log" 2>&1 </dev/null &
fi

exit 0

package poll

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Acquire takes the poller's lock, returning a function that releases it.
//
// ADR-001 makes this the first thing the daemon does, and the reason is
// Herdr's: a startup hook re-fires on every server start and on a live handoff,
// so without a lock a second poller would deliver every instruction to the
// agent a second time.
//
// It is a pid file rather than a flock because the lock has to outlive nothing
// but the process holding it: a crash leaves a stale pid, which is detected as
// a dead one and taken over, whereas a stale flock would need a supervisor to
// clear it. The check and the write are not atomic, which is fine here — the
// window is microseconds and both racers would be the same user starting the
// same daemon twice, where the worst case is one redundant poll.
func Acquire(path string) (func(), error) {
	if path == "" {
		return nil, errors.New("poll: a lock file path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("poll: create the lock directory: %w", err)
	}

	if raw, err := os.ReadFile(path); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid != os.Getpid() && alive(pid) {
			return nil, fmt.Errorf("poll: another poller is already running (pid %d, %s)", pid, path)
		}
	}

	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("poll: write the lock file: %w", err)
	}
	return func() { os.Remove(path) }, nil
}

// alive reports whether a pid is in use.
//
// EPERM is the case worth being careful about: it means the process exists but
// belongs to someone else, so treating it as gone would start a second poller
// on top of a running one.
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

package share

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// withLock runs f while holding an advisory lock on the store.
//
// Three processes write this file: `share` records a thread, `poll` advances
// cursors on every pass, and `serve` appends a login the operator admitted at
// the door (ADR-007). Every one of them is a read-modify-write of the whole
// list, so without a lock the last writer silently discards whatever the
// others did in between — and the thing most likely to be discarded is the
// poller's comment cursor, which is what stops an instruction being delivered
// to the agent twice.
//
// It is a flock rather than the poller's pid file because this lock is held
// for microseconds and must never outlive the process holding it: the kernel
// drops it when the descriptor closes, crash included. Nothing here nests —
// the locked bodies call the unexported methods — because a second flock on a
// second descriptor would block against the first, in this process as much as
// in any other.
func (s Store) withLock(f func() error) error {
	path := s.Path + ".lock"
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("share: create %s: %w", filepath.Dir(path), err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("share: open %s: %w", path, err)
	}
	defer file.Close()

	fd := int(file.Fd())
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		return fmt.Errorf("share: lock %s: %w", path, err)
	}
	defer func() { _ = syscall.Flock(fd, syscall.LOCK_UN) }()

	return f()
}

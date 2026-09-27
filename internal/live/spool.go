package live

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/DnzzL/herdr-huddle/internal/thread"
)

// Spool is where instruction records wait when the thread will not take them.
//
// It exists because the pull request *is* the artifact (ADR-001). A delivered
// instruction that never reaches the thread is a turn the record cannot
// explain: the agent did something, and nothing says who asked for it. Holding
// those records only in memory meant a `serve` restart — a Ctrl-C, a crash, a
// laptop lid — silently threw them away.
type Spool interface {
	// Load returns what a previous run still owed the thread.
	Load() ([]thread.LiveInstruction, error)
	// Save replaces the owed set.
	Save([]thread.LiveInstruction) error
}

// FileSpool keeps the queue next to the share records.
type FileSpool struct {
	Path string
}

// Load returns the owed records. A missing file is an empty queue, not an
// error: it is the state before anything has ever failed to post.
func (f FileSpool) Load() ([]thread.LiveInstruction, error) {
	raw, err := os.ReadFile(f.Path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("live: read %s: %w", f.Path, err)
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var owed []thread.LiveInstruction
	if err := json.Unmarshal(raw, &owed); err != nil {
		// Deliberately an error rather than an empty queue. Starting from
		// nothing would drop somebody's words without saying so, which is the
		// exact failure this type exists to prevent.
		return nil, fmt.Errorf("live: %s is not a valid queue: %w", f.Path, err)
	}
	return owed, nil
}

// Save replaces the file atomically, so a reader never sees half a queue and a
// crash mid-write leaves the previous one intact.
//
// The mode is 0600 because these records are what people typed at the agent.
func (f FileSpool) Save(owed []thread.LiveInstruction) error {
	dir := filepath.Dir(f.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("live: create %s: %w", dir, err)
	}
	if owed == nil {
		owed = []thread.LiveInstruction{}
	}
	buf, err := json.MarshalIndent(owed, "", "  ")
	if err != nil {
		return fmt.Errorf("live: encode the queue: %w", err)
	}
	buf = append(buf, '\n')

	tmp, err := os.CreateTemp(dir, ".pending-*")
	if err != nil {
		return fmt.Errorf("live: create a temporary file in %s: %w", dir, err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("live: set permissions on %s: %w", name, err)
	}
	if _, err := tmp.Write(buf); err != nil {
		tmp.Close()
		return fmt.Errorf("live: write %s: %w", name, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("live: sync %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("live: close %s: %w", name, err)
	}
	if err := os.Rename(name, f.Path); err != nil {
		return fmt.Errorf("live: replace %s: %w", f.Path, err)
	}
	return nil
}

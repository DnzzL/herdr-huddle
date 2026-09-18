package share

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// State is one active share, as the poller needs it. It is the record ADR-001
// puts in the plugin's config directory, and it is the only thing that outlives
// the command that opened the share.
type State struct {
	// Repo is owner/name.
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	Base   string `json:"base"`
	Number int    `json:"number"`
	URL    string `json:"url"`
	// Allowlist is the logins whose comments may drive the agent. ADR-001
	// requires all three of: an /agent prefix, a collaborative
	// author_association, and membership of this list.
	Allowlist []string  `json:"allowlist"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Key identifies the share. The branch is the identity ADR-002 makes idempotent,
// and it is unique within a repository.
func (s State) Key() string { return s.Repo + "#" + s.Branch }

// FromResult builds the record for a share that was just opened.
func FromResult(res Result, now time.Time) State {
	return State{
		Repo:      res.Repo.String(),
		Branch:    res.Branch,
		Base:      res.Base,
		Number:    res.PullRequest.Number,
		URL:       res.PullRequest.HTMLURL,
		Allowlist: append([]string(nil), res.Allowlist...),
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// Store is the on-disk list of active shares.
type Store struct {
	Path string
}

// Load returns the recorded shares, sorted by key so that output and tests do
// not depend on map iteration order. A missing file is an empty list, not an
// error: it is the state before the first share.
func (s Store) Load() ([]State, error) {
	raw, err := os.ReadFile(s.Path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("share: read %s: %w", s.Path, err)
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var states []State
	if err := json.Unmarshal(raw, &states); err != nil {
		// Deliberately an error rather than an empty list. Starting from
		// nothing would silently stop the poller for every active share, and
		// the operator would have no reason to suspect the file.
		return nil, fmt.Errorf("share: %s is not valid share state: %w", s.Path, err)
	}
	sort.Slice(states, func(i, j int) bool { return states[i].Key() < states[j].Key() })
	return states, nil
}

// Put records a share, replacing any record with the same key. Replacing rather
// than appending is what keeps a re-shared thread from being polled twice.
func (s Store) Put(state State) error {
	states, err := s.Load()
	if err != nil {
		return err
	}
	replaced := false
	for i := range states {
		if states[i].Key() == state.Key() {
			// A stable CreatedAt: the record was replaced, not created.
			if !states[i].CreatedAt.IsZero() {
				state.CreatedAt = states[i].CreatedAt
			}
			states[i] = state
			replaced = true
			break
		}
	}
	if !replaced {
		states = append(states, state)
	}
	return s.write(states)
}

// Delete removes a share and reports whether it was there.
func (s Store) Delete(key string) (bool, error) {
	states, err := s.Load()
	if err != nil {
		return false, err
	}
	kept := states[:0]
	found := false
	for _, st := range states {
		if st.Key() == key {
			found = true
			continue
		}
		kept = append(kept, st)
	}
	if !found {
		return false, nil
	}
	return true, s.write(kept)
}

// write replaces the file atomically.
//
// A half-written file would be read by the poller as either corrupt or, worse,
// as a shorter list of shares — a share dropping out of the state without any
// signal. Writing to a temporary file in the same directory and renaming means
// the file is only ever the old content or the new one.
func (s Store) write(states []State) error {
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("share: create %s: %w", dir, err)
	}
	buf, err := json.MarshalIndent(states, "", "  ")
	if err != nil {
		return fmt.Errorf("share: encode state: %w", err)
	}
	buf = append(buf, '\n')

	tmp, err := os.CreateTemp(dir, ".shares-*")
	if err != nil {
		return fmt.Errorf("share: create temporary file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		// A no-op once the rename has succeeded, and the cleanup of a failure
		// path otherwise.
		_ = os.Remove(tmpName)
	}()

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("share: set permissions on %s: %w", tmpName, err)
	}
	if _, err := tmp.Write(buf); err != nil {
		tmp.Close()
		return fmt.Errorf("share: write %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("share: sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("share: close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, s.Path); err != nil {
		return fmt.Errorf("share: replace %s: %w", s.Path, err)
	}
	return nil
}

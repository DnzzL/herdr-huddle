package share

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
	Allowlist []string `json:"allowlist"`
	// Origin is the local state the share is bound to. ADR-003 makes this part
	// of the record: without it the poller knows where to post and not what to
	// drive.
	Origin Origin `json:"origin"`
	// Cursors is how far each direction of the sync has been read.
	Cursors Cursors `json:"cursors"`
	// RetiredAt is when the share stopped being polled, because its agent or
	// pane was gone. The record is kept rather than deleted so that re-opening
	// the share resumes it instead of repeating the conversation.
	RetiredAt *time.Time `json:"retired_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// Origin is the local state on the operator's machine that a share depends on.
//
// Everything here is read from Herdr or from the session file. None of it can
// be recovered from GitHub, which is why it is recorded at share time rather
// than looked up by the poller.
type Origin struct {
	// Agent is the agent's identity, for display. It is not a target: herdr
	// resolves names only for agents it started itself.
	Agent string `json:"agent,omitempty"`
	// PaneID is what herdr accepts as a target, and what the poller checks for
	// liveness.
	PaneID string `json:"pane_id,omitempty"`
	// CWD is the pane's working directory, which is how the session file is
	// found again if it has to be.
	CWD string `json:"cwd,omitempty"`
	// Session is the transcript file. Empty when the agent has none.
	Session string `json:"session,omitempty"`
	// Kind is the agent kind that wrote Session, and so the adapter that reads
	// it. Recorded because a pane can outlive the agent that opened the share:
	// if the kind changes, the old file is another agent's format and has to be
	// re-resolved rather than parsed.
	Kind string `json:"kind,omitempty"`
	// SessionID is the id recorded inside the session file, which survives the
	// file being moved.
	SessionID string `json:"session_id,omitempty"`
	// Partial is true when the transcript can only come from the terminal.
	Partial bool `json:"partial,omitempty"`
}

// Cursors is the sync's read position in each direction.
type Cursors struct {
	// Transcript is the last transcript position delivered to the thread.
	Transcript string `json:"transcript,omitempty"`
	// Comment is the newest comment already handled. It is what stops a
	// restart from injecting an instruction that has already been delivered.
	Comment int64 `json:"comment,omitempty"`
	// Snapshot identifies the last terminal snapshot delivered. A terminal has
	// no cursor, so the content itself is the only thing to compare.
	Snapshot string `json:"snapshot,omitempty"`
	// ETag is the comment list's validator from the last read. A pass that
	// re-asks with it costs no quota when nothing has changed (ADR-001).
	ETag string `json:"etag,omitempty"`
	// Blocked records that the thread has already been told the agent is
	// waiting for an approval, so the same episode is announced once.
	Blocked bool `json:"blocked,omitempty"`
}

// Active reports whether the share should still be polled.
func (s State) Active() bool { return s.RetiredAt == nil }

// Owner splits Repo into its owner and name.
func (s State) Owner() (string, string) {
	if owner, name, ok := strings.Cut(s.Repo, "/"); ok {
		return owner, name
	}
	return "", s.Repo
}

// Key identifies the share. The branch is the identity ADR-002 makes idempotent,
// and it is unique within a repository.
func (s State) Key() string { return s.Repo + "#" + s.Branch }

// FromResult builds the record for a share that was just opened.
//
// The origin is a parameter rather than something the caller assigns afterwards.
// A record that reaches disk without it is indistinguishable from a share opened
// outside a pane: the poller retires it on its first pass, and the thread never
// receives a transcript. Making it required is the difference between that being
// impossible and being easy to forget.
func FromResult(res Result, origin Origin, now time.Time) State {
	return State{
		Repo:      res.Repo.String(),
		Branch:    res.Branch,
		Base:      res.Base,
		Number:    res.PullRequest.Number,
		URL:       res.PullRequest.HTMLURL,
		Allowlist: append([]string(nil), res.Allowlist...),
		Origin:    origin,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// Record stores a share for the poller, keeping the progress an existing record
// has already made.
//
// Re-running `share` on the same branch is how an operator picks a thread back
// up, so it must not reset the cursors. A reset instruction cursor would have
// the agent carry out every instruction in the thread a second time, and a reset
// transcript cursor would repost a conversation the thread already holds.
//
// seedTranscript is where the transcript cursor starts when there is nothing to
// carry over: the end of the session, so the first share does not replay the
// conversation the operator already lived through.
//
// The returned bool reports whether a record was resumed rather than created.
func (s Store) Record(fresh State, seedTranscript string) (State, bool, error) {
	if fresh.Key() == "#" {
		return State{}, false, errors.New("share: refusing to record a share with no repository or branch")
	}
	var resumed bool
	err := s.withLock(func() error {
		var err error
		fresh, resumed, err = s.record(fresh, seedTranscript)
		return err
	})
	return fresh, resumed, err
}

func (s Store) record(fresh State, seedTranscript string) (State, bool, error) {
	states, err := s.Load()
	if err != nil {
		return State{}, false, err
	}

	fresh.Cursors.Transcript = seedTranscript

	resumed := false
	for _, existing := range states {
		if existing.Key() != fresh.Key() {
			continue
		}
		resumed = true
		switch {
		case existing.Origin.Session != "" && existing.Origin.Session == fresh.Origin.Session:
			// The same session: nothing has changed underneath the share, so
			// every cursor still means what it meant.
			fresh.Cursors = existing.Cursors
		default:
			// A different session — the agent restarted, or a different agent
			// is in the pane. Instructions the thread has already delivered stay
			// delivered: re-injecting all of them into a new agent would fan a
			// pile of side effects out a second time.
			//
			// The snapshot is carried over too. It identifies terminal content
			// that has already been posted, and identical content is a duplicate
			// whatever the session is doing; dropping it would repost the pane
			// on every re-share of a share that has no session file at all.
			fresh.Cursors.Comment = existing.Cursors.Comment
			fresh.Cursors.ETag = existing.Cursors.ETag
			fresh.Cursors.Blocked = existing.Cursors.Blocked
			fresh.Cursors.Snapshot = existing.Cursors.Snapshot
		}
		break
	}

	return fresh, resumed, s.put(fresh)
}

// Store is the on-disk list of active shares.
type Store struct {
	Path string
}

// Allow adds a login to a share's allowlist, and reports whether it was new.
//
// It exists as one operation rather than as a Load the caller follows with a
// Put because it is the one write made *during* a live session, while the
// poller is writing cursors to the same file: reading and writing under one
// lock is what keeps an admission from discarding a cursor, or a cursor from
// discarding an admission (ADR-007).
func (s Store) Allow(key, login string) (bool, error) {
	if key == "" || strings.TrimSpace(login) == "" {
		return false, errors.New("share: refusing to allow an empty login")
	}
	added := false
	err := s.withLock(func() error {
		states, err := s.Load()
		if err != nil {
			return err
		}
		for _, state := range states {
			if state.Key() != key {
				continue
			}
			for _, allowed := range state.Allowlist {
				if strings.EqualFold(allowed, login) {
					return nil
				}
			}
			state.Allowlist = append(state.Allowlist, login)
			state.UpdatedAt = time.Now()
			added = true
			return s.put(state)
		}
		return fmt.Errorf("share %s is no longer recorded", key)
	})
	return added, err
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
//
// It replaces the whole record, allowlist included. A caller holding a record
// it read some time ago wants Save instead.
func (s Store) Put(state State) error {
	return s.withLock(func() error { return s.put(state) })
}

// Save records progress on a share that already exists, keeping the fields
// this caller does not own.
//
// The allowlist is the one of those, and the reason is ADR-007: `serve`
// appends an admitted login to it *while a huddle is running*, and the poller
// writes a record it read at the start of its pass. Writing that copy back
// whole would un-admit somebody who joined thirty seconds ago — quietly, and
// only until the next time they reconnected and had to knock again. Cursors
// and retirement belong to the poller; the allowlist belongs to the door.
func (s Store) Save(state State) error {
	return s.withLock(func() error {
		states, err := s.Load()
		if err != nil {
			return err
		}
		for _, existing := range states {
			if existing.Key() == state.Key() {
				state.Allowlist = append([]string(nil), existing.Allowlist...)
				break
			}
		}
		return s.put(state)
	})
}

func (s Store) put(state State) error {
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
	var found bool
	err := s.withLock(func() error {
		var err error
		found, err = s.delete(key)
		return err
	})
	return found, err
}

func (s Store) delete(key string) (bool, error) {
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

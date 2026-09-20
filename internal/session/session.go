// Package session finds the transcript behind a live agent.
//
// Herdr knows which pane an agent runs in, but not where its transcript is:
// on a machine whose agents are all `pi`, `agent_session` is empty for every
// one of them. So the file is located from what the pane does report — its
// working directory — and the identification is made from the file's own
// contents rather than from a guess about how the agent names its directories.
package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/DnzzL/herdr-huddle/internal/herdr"
)

// Source is where a share's transcript comes from.
type Source struct {
	// Path is the session JSONL. Empty when no file could be found, in which
	// case the transcript has to come from the terminal instead.
	Path string
	// Kind is the agent kind that wrote the file, and so the adapter that has
	// to read it. Empty when the agent kind has no adapter at all.
	Kind string
	// Partial is true when the only available transcript is a terminal
	// snapshot: readable prose, but no cursor for incremental sync and no tool
	// detail. ADR-001 keeps that path as a labelled fallback, never as the
	// product.
	Partial bool
	// Reason says why this source, in words an operator can act on.
	Reason string
}

// transcriptRoot is where an agent kind keeps its sessions. A kind that is not
// in here has no adapter, which is what makes the terminal the answer for a new
// agent rather than a guess at its file format.
func transcriptRoot(home, kind string) (string, bool) {
	switch kind {
	case herdr.KindClaude:
		return filepath.Join(home, ".claude", "projects"), true
	case herdr.KindPi:
		return filepath.Join(home, ".pi", "agent", "sessions"), true
	}
	return "", false
}

// Resolve picks the transcript for an agent, in the order ADR-001 sets out:
// a path reported by the agent, then a session id reported by the agent, then
// the session file whose own records name the pane's working directory. When
// none of those hold, the source is the terminal, and it is labelled as partial.
func Resolve(home string, agent herdr.Agent) (Source, error) {
	// The kind decides first, because it decides both where to look and what
	// the file means. An unknown kind is read from the terminal rather than
	// from a file that a parser would silently render nothing for.
	if _, ok := transcriptRoot(home, agent.Agent); !ok {
		return Source{Partial: true, Reason: fmt.Sprintf("no transcript adapter for agent kind %s, so the transcript comes from the terminal", quoteKind(agent.Agent))}, nil
	}
	if agent.Session != nil && agent.Session.Value != "" {
		switch agent.Session.Kind {
		case herdr.SessionKindPath:
			if path, err := acceptPath(home, agent.Session.Value); err == nil {
				return Source{Path: path, Kind: agent.Agent, Reason: "reported by " + source(agent.Session)}, nil
			} else {
				// Falling through is right: the agent told us something we will
				// not read, and the operator needs to know the file itself was
				// not ignored, only that value.
				return resolveByCWD(home, agent, fmt.Sprintf("the reported session path was refused: %v", err))
			}
		case herdr.SessionKindID:
			if path, err := findByID(home, agent.Agent, agent.Session.Value); err == nil {
				return Source{Path: path, Kind: agent.Agent, Reason: "session id " + agent.Session.Value}, nil
			}
			return resolveByCWD(home, agent, fmt.Sprintf("no file for session id %q", agent.Session.Value))
		}
	}
	return resolveByCWD(home, agent, "")
}

// quoteKind names an agent kind in a message, including the case where the
// agent reports none at all.
func quoteKind(kind string) string {
	if kind == "" {
		return "(none reported)"
	}
	return strconv.Quote(kind)
}

// SessionID is the session id of a file, read from its own records. It is what
// a share records so a re-opened share can find its transcript again.
func SessionID(path string) (string, error) {
	probe, err := probeFile(path)
	if err != nil {
		return "", err
	}
	return probe.sessionID(), nil
}

func resolveByCWD(home string, agent herdr.Agent, because string) (Source, error) {
	if agent.CWD == "" {
		return Source{Kind: agent.Agent, Partial: true, Reason: joinReason(because, "the agent reports no working directory")}, nil
	}
	path, err := newestForCWD(home, agent.Agent, agent.CWD)
	if err != nil {
		return Source{}, err
	}
	if path == "" {
		return Source{Kind: agent.Agent, Partial: true, Reason: joinReason(because, "no session file records the working directory "+agent.CWD)}, nil
	}
	return Source{Path: path, Kind: agent.Agent, Reason: "working directory " + agent.CWD}, nil
}

func joinReason(because, reason string) string {
	if because == "" {
		return reason
	}
	return because + "; " + reason
}

func source(s *herdr.AgentSession) string {
	if s.Source == "" {
		return "the agent"
	}
	return s.Source
}

// sessionIDRE is the shape of a session id. It is checked because the value
// becomes part of a glob pattern, where a `*` or a `..` would search somewhere
// else entirely.
var sessionIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{7,127}$`)

// findByID looks for a session file named after the id, in any project.
//
// The id is matched anywhere in the filename because the two kinds name their
// files differently: Claude Code writes <id>.jsonl, pi writes
// <timestamp>_<id>.jsonl.
func findByID(home, kind, id string) (string, error) {
	if !sessionIDRE.MatchString(id) || strings.Contains(id, "..") {
		return "", fmt.Errorf("%q is not a session id", id)
	}
	root, ok := transcriptRoot(home, kind)
	if !ok {
		return "", fmt.Errorf("no transcript adapter for agent kind %q", kind)
	}
	matches, err := filepath.Glob(filepath.Join(root, "*", "*"+id+"*.jsonl"))
	if err != nil {
		return "", err
	}
	newest := newestOf(matches)
	if newest == "" {
		return "", fmt.Errorf("no session file for %q", id)
	}
	return newest, nil
}

// acceptPath validates a path an agent reported before anything reads it.
//
// A pane can report any path it likes, and whatever is read here is published
// into a pull request. So the value has to look like a transcript and live
// under the operator's home directory, which refuses the obvious way to use
// this as a file exfiltration primitive.
func acceptPath(home, raw string) (string, error) {
	if !filepath.IsAbs(raw) {
		return "", fmt.Errorf("%q is not an absolute path", raw)
	}
	if filepath.Ext(raw) != ".jsonl" {
		return "", fmt.Errorf("%q is not a .jsonl file", raw)
	}
	clean := filepath.Clean(raw)
	if home != "" && !underDir(home, clean) {
		return "", fmt.Errorf("%q is outside %s", raw, home)
	}
	info, err := os.Stat(clean)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%q is a directory", raw)
	}
	return clean, nil
}

func underDir(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// newestForCWD finds the most recently modified session file whose own records
// name this working directory.
//
// Both kinds derive their project directory name from the path by replacing
// separators, a rule that is undocumented and that differs for paths containing
// dots. Reading the recorded `cwd` out of the candidate files avoids depending
// on it. Candidates are examined newest-first and the search stops at the first
// match, so the cost is one stat per session file on the machine plus a few
// small reads — not a read of every session.
func newestForCWD(home, kind, cwd string) (string, error) {
	root, ok := transcriptRoot(home, kind)
	if !ok {
		return "", fmt.Errorf("no transcript adapter for agent kind %q", kind)
	}
	matches, err := filepath.Glob(filepath.Join(root, "*", "*.jsonl"))
	if err != nil {
		return "", err
	}
	for _, path := range sortByModTimeDesc(matches) {
		probe, err := probeFile(path)
		if err != nil || probe.CWD == "" {
			continue
		}
		if probe.CWD == cwd {
			return path, nil
		}
	}
	return "", nil
}

// probe is the little of a record that identifies a session.
type probe struct {
	CWD string `json:"cwd"`
	// SessionID is Claude Code's key; ID is pi's, on its one cwd-bearing
	// record. Whichever is present is the session's id.
	SessionID string `json:"sessionId"`
	ID        string `json:"id"`
}

// ID is the session's id, whichever key the agent used for it.
func (p probe) sessionID() string {
	if p.SessionID != "" {
		return p.SessionID
	}
	return p.ID
}

// probeFile reads the first record that names a working directory. The earliest
// records in a Claude Code session are queue operations with no cwd, so the
// first line is not enough; in a pi session the first line is the session
// record, which carries the cwd.
func probeFile(path string) (probe, error) {
	f, err := os.Open(path)
	if err != nil {
		return probe{}, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for lines := 0; scanner.Scan() && lines < 50; lines++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var got probe
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			// A file that is not transcript JSONL is not this file's problem to
			// report: the next candidate may match.
			continue
		}
		if got.CWD != "" {
			return got, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return probe{}, err
	}
	return probe{}, errors.New("no record naming a working directory")
}

// newestOf returns the most recently modified path, or "" for none.
func newestOf(paths []string) string {
	sorted := sortByModTimeDesc(paths)
	if len(sorted) == 0 {
		return ""
	}
	return sorted[0]
}

func sortByModTimeDesc(paths []string) []string {
	type entry struct {
		path string
		mod  int64
	}
	entries := make([]entry, 0, len(paths))
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		entries = append(entries, entry{path: path, mod: info.ModTime().UnixNano()})
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].mod > entries[j].mod })

	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.path)
	}
	return out
}

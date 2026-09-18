package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DnzzL/herdr-huddle/internal/herdr"
)

// transcript builds a small session file the way Claude Code writes one: a
// queue operation first, which carries no cwd, then a conversation record
// which does.
func transcript(t *testing.T, home, project, name, cwd, sessionID string, mod time.Time) string {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name+".jsonl")
	body := fmt.Sprintf(
		"{\"type\":\"queue-operation\",\"operation\":\"enqueue\",\"sessionId\":%q}\n"+
			"{\"type\":\"user\",\"uuid\":\"u1\",\"sessionId\":%q,\"cwd\":%q,\"message\":{\"role\":\"user\",\"content\":\"hello\"}}\n",
		sessionID, sessionID, cwd)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
	return path
}

func agentWith(kind herdr.AgentSessionKind, value string) herdr.Agent {
	return herdr.Agent{
		Agent: "claude", Name: "claude", CWD: "/work/repo", PaneID: "w1:p1",
		Session: &herdr.AgentSession{Agent: "claude", Kind: kind, Value: value, Source: "claude-code"},
	}
}

func TestResolve_ReportedPathWins(t *testing.T) {
	home := t.TempDir()
	// A second, newer session exists, so "newest file" would pick the wrong one.
	// The reported path must win over any guessing.
	want := transcript(t, home, "-somewhere-else", "other", "/work/other", "other", time.Now().Add(time.Hour))
	agent := agentWith(herdr.SessionKindPath, want)

	got, err := Resolve(home, agent)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Path != want {
		t.Errorf("Path = %q, want the reported %q", got.Path, want)
	}
	if got.Partial {
		t.Error("Partial = true for a file that exists")
	}
	if !strings.Contains(got.Reason, "claude-code") {
		t.Errorf("Reason = %q, want it to name the reporter", got.Reason)
	}
}

func TestResolve_RefusesAPathItShouldNotRead(t *testing.T) {
	home := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secrets.jsonl")
	if err := os.WriteFile(outside, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(home, "notes.txt")
	if err := os.WriteFile(plain, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		path string
		want string
	}{
		{"outside home", outside, "outside"},
		{"not jsonl", plain, "not a .jsonl"},
		{"not absolute", "sessions/x.jsonl", "not an absolute"},
		{"traversal", filepath.Join(home, "..", "..", "etc", "passwd.jsonl"), "outside"},
		// An empty value is not a refused path: it means the agent reported no
		// session at all, so resolution goes straight to the working directory.
		{"empty", "", "no session file records"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Resolve(home, agentWith(herdr.SessionKindPath, tc.path))
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			// The refusal must not be silent: it falls back, and says so.
			if got.Path != "" {
				t.Errorf("Path = %q, want no file read", got.Path)
			}
			if !got.Partial {
				t.Error("Partial = false, want the terminal fallback")
			}
			if !strings.Contains(got.Reason, tc.want) {
				t.Errorf("Reason = %q, want it to mention %q", got.Reason, tc.want)
			}
		})
	}
}

func TestResolve_ReportedSessionID(t *testing.T) {
	home := t.TempDir()
	id := "867536ee-690c-4339-a289-4484603698d9"
	want := transcript(t, home, "-some-project", id, "/work/elsewhere", id, time.Now())

	got, err := Resolve(home, agentWith(herdr.SessionKindID, id))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Path != want {
		t.Errorf("Path = %q, want %q", got.Path, want)
	}
}

func TestResolve_ReportedIDThatIsNotAnID(t *testing.T) {
	home := t.TempDir()
	// The id becomes a glob, so a traversal or a wildcard would search
	// somewhere else. Each of these must be refused rather than matched.
	for _, id := range []string{"../../*", "*", "..", "a/b", "", "short"} {
		t.Run(id, func(t *testing.T) {
			got, err := Resolve(home, agentWith(herdr.SessionKindID, id))
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got.Path != "" {
				t.Errorf("Path = %q, want no match for %q", got.Path, id)
			}
			if !got.Partial {
				t.Error("Partial = false, want the terminal fallback")
			}
		})
	}
}

func TestResolve_ByWorkingDirectoryPrefersTheMatchingSession(t *testing.T) {
	home := t.TempDir()
	// Three sessions on the machine. The newest belongs to a different project:
	// taking the newest transcript would share the wrong conversation, which is
	// the failure that matters when several worktrees run at once.
	transcript(t, home, "-work-other", "other", "/work/other", "id-other", time.Now().Add(2*time.Hour))
	transcript(t, home, "-home-thomas-Projects-repo", "repo-old", "/work/repo", "id-old", time.Now().Add(-2*time.Hour))
	want := transcript(t, home, "-home-thomas-Projects-repo", "repo-new", "/work/repo", "id-new", time.Now().Add(-time.Hour))

	agent := herdr.Agent{Agent: "claude", CWD: "/work/repo", PaneID: "w1:p1"}
	got, err := Resolve(home, agent)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Path != want {
		t.Errorf("Path = %q, want the newest session recording %q", got.Path, "/work/repo")
	}
	if !strings.Contains(got.Reason, "/work/repo") {
		t.Errorf("Reason = %q, want it to name the working directory", got.Reason)
	}
}

func TestResolve_PartialWhenNoFileRecordsTheDirectory(t *testing.T) {
	home := t.TempDir()
	transcript(t, home, "-somewhere-else", "other", "/work/other", "id", time.Now())

	got, err := Resolve(home, herdr.Agent{Agent: "pi", CWD: "/work/nowhere", PaneID: "w1:p1"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Path != "" || !got.Partial {
		t.Errorf("got %+v, want the terminal fallback", got)
	}
	if !strings.Contains(got.Reason, "/work/nowhere") {
		t.Errorf("Reason = %q, want it to name the directory that found nothing", got.Reason)
	}
}

func TestResolve_NoDirectoryAtAll(t *testing.T) {
	got, err := Resolve(t.TempDir(), herdr.Agent{Agent: "pi", PaneID: "w1:p1"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !got.Partial || !strings.Contains(got.Reason, "no working directory") {
		t.Errorf("got %+v, want a partial source explaining the missing cwd", got)
	}
}

func TestResolve_EmptyProjectsDirectory(t *testing.T) {
	got, err := Resolve(t.TempDir(), herdr.Agent{Agent: "claude", CWD: "/work/repo"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Path != "" || !got.Partial {
		t.Errorf("got %+v, want the terminal fallback", got)
	}
}

func TestSessionID_IsReadFromTheFile(t *testing.T) {
	home := t.TempDir()
	path := transcript(t, home, "-work-repo", "name-that-differs-from-the-id", "/work/repo", "867536ee-690c-4339-a289-4484603698d9", time.Now())

	got, err := SessionID(path)
	if err != nil {
		t.Fatalf("SessionID: %v", err)
	}
	if got != "867536ee-690c-4339-a289-4484603698d9" {
		t.Errorf("SessionID = %q, want the id recorded inside the file", got)
	}
}

func TestProbeFile_NotATranscriptIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "junk.jsonl")
	if err := os.WriteFile(path, []byte("not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := probeFile(path); err == nil {
		t.Fatal("want an error for a file with no record naming a working directory")
	}
	if _, err := probeFile(filepath.Join(t.TempDir(), "missing.jsonl")); err == nil {
		t.Fatal("want an error for a missing file")
	}
}

func TestNewestForCWD_SkipsDirectoriesAndUnreadableFiles(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude", "projects", "-work-repo")
	if err := os.MkdirAll(filepath.Join(dir, "a-directory.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := transcript(t, home, "-work-repo", "real", "/work/repo", "id", time.Now())

	got, err := newestForCWD(home, "/work/repo")
	if err != nil {
		t.Fatalf("newestForCWD: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

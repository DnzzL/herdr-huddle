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

	got, err := newestForCWD(home, herdr.KindClaude, "/work/repo")
	if err != nil {
		t.Fatalf("newestForCWD: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// piTranscript builds a small session file the way pi writes one: the first
// line is the session record, which carries both the cwd and the id.
func piTranscript(t *testing.T, home, project, name, cwd, id string, mod time.Time) string {
	t.Helper()
	dir := filepath.Join(home, ".pi", "agent", "sessions", project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name+".jsonl")
	body := fmt.Sprintf(
		"{\"type\":\"session\",\"version\":3,\"id\":%q,\"timestamp\":\"2026-09-20T09:59:00.000Z\",\"cwd\":%q}\n"+
			"{\"type\":\"message\",\"id\":\"u1\",\"parentId\":null,\"timestamp\":\"2026-09-20T10:00:00.000Z\",\"message\":{\"role\":\"user\",\"content\":[{\"type\":\"text\",\"text\":\"hello\"}]}}\n",
		id, cwd)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolve_FindsAPiSessionAndNamesItsAdapter(t *testing.T) {
	home := t.TempDir()
	want := piTranscript(t, home, "--work-repo--", "2026-09-20T09-59-00-000Z_01a0ae5c-7228-70e3-9e63-2e82dacc2a2c", "/work/repo", "01a0ae5c-7228-70e3-9e63-2e82dacc2a2c", time.Now())

	got, err := Resolve(home, herdr.Agent{Agent: herdr.KindPi, CWD: "/work/repo", PaneID: "w1:p1"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Path != want {
		t.Errorf("Path = %q, want the pi session recording %q", got.Path, "/work/repo")
	}
	if got.Kind != herdr.KindPi {
		t.Errorf("Kind = %q, want %q so the pi adapter reads it", got.Kind, herdr.KindPi)
	}
	if got.Partial {
		t.Errorf("got a partial source for a session file that exists: %+v", got)
	}
}

func TestResolve_PiIsFoundBySessionIDDespiteTheTimestampPrefix(t *testing.T) {
	// pi names its files <timestamp>_<id>.jsonl, so an id is not the whole name.
	home := t.TempDir()
	want := piTranscript(t, home, "--work-repo--", "2026-09-20T09-59-00-000Z_01a0ae5c-7228-70e3-9e63-2e82dacc2a2c", "/work/repo", "01a0ae5c-7228-70e3-9e63-2e82dacc2a2c", time.Now())
	agent := herdr.Agent{
		Agent: herdr.KindPi, CWD: "/work/elsewhere", PaneID: "w1:p1",
		Session: &herdr.AgentSession{Agent: "pi", Kind: herdr.SessionKindID, Value: "01a0ae5c-7228-70e3-9e63-2e82dacc2a2c"},
	}

	got, err := Resolve(home, agent)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Path != want || got.Kind != herdr.KindPi {
		t.Errorf("got %+v, want the pi session named by the id", got)
	}
}

func TestResolve_EachKindLooksOnlyInItsOwnRoot(t *testing.T) {
	// A file the other agent wrote is not this agent's conversation. Reading it
	// would publish the wrong session — the reason the kind decides first.
	home := t.TempDir()
	piTranscript(t, home, "--work-repo--", "2026-09-20T09-59-00-000Z_01a0ae5c-7228-70e3-9e63-2e82dacc2a2c", "/work/repo", "01a0ae5c", time.Now())

	got, err := Resolve(home, herdr.Agent{Agent: herdr.KindClaude, CWD: "/work/repo", PaneID: "w1:p1"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Path != "" || !got.Partial {
		t.Errorf("got %+v, want the terminal fallback rather than pi's session", got)
	}
}

func TestResolve_AnUnknownKindIsTheTerminalAndNamesTheKind(t *testing.T) {
	// A file that exists is not enough: without an adapter there is no parser,
	// and a parser that does not know the records renders nothing at all.
	home := t.TempDir()
	piTranscript(t, home, "--work-repo--", "2026-09-20T09-59-00-000Z_01a0ae5c-7228-70e3-9e63-2e82dacc2a2c", "/work/repo", "01a0ae5c", time.Now())

	got, err := Resolve(home, herdr.Agent{Agent: "some-new-agent", CWD: "/work/repo", PaneID: "w1:p1"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Path != "" || !got.Partial {
		t.Errorf("got %+v, want the terminal fallback", got)
	}
	if !strings.Contains(got.Reason, "some-new-agent") {
		t.Errorf("Reason = %q, want it to name the kind with no adapter", got.Reason)
	}
	if got.Kind != "" {
		t.Errorf("Kind = %q, want empty so no adapter is claimed for the fallback", got.Kind)
	}
}

func TestResolve_AnAgentReportingNoKindIsNotGuessedAt(t *testing.T) {
	// Claude is the fallback for a *recorded share* that predates the second
	// adapter, because that is a fact about what it was. An agent that reports
	// no kind is a different thing: nothing is known about what is writing that
	// terminal, so its transcript is the terminal.
	home := t.TempDir()
	transcript(t, home, "-work-repo", "s", "/work/repo", "id", time.Now())

	got, err := Resolve(home, herdr.Agent{CWD: "/work/repo", PaneID: "w1:p1"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Path != "" || !got.Partial {
		t.Errorf("got %+v, want the terminal fallback", got)
	}
	if !strings.Contains(got.Reason, "none reported") {
		t.Errorf("Reason = %q, want it to say the agent reported no kind", got.Reason)
	}
}

func TestSessionID_OfAPiSession(t *testing.T) {
	home := t.TempDir()
	path := piTranscript(t, home, "--work-repo--", "2026-09-20T09-59-00-000Z_x", "/work/repo", "01a0ae5c-7228-70e3-9e63-2e82dacc2a2c", time.Now())

	got, err := SessionID(path)
	if err != nil {
		t.Fatalf("SessionID: %v", err)
	}
	if got != "01a0ae5c-7228-70e3-9e63-2e82dacc2a2c" {
		t.Errorf("SessionID = %q, want pi's session record id", got)
	}
}

package herdr

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRunner records the command it was asked to run so tests can assert on the
// arguments and the environment, which is half of what this package does.
type fakeRunner struct {
	stdout   string
	stderr   string
	exitCode int
	err      error

	calls [][]string
	envs  [][]string
}

func (f *fakeRunner) Run(_ context.Context, name string, args []string, env []string) ([]byte, []byte, int, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	f.envs = append(f.envs, env)
	if f.err != nil {
		return nil, nil, -1, f.err
	}
	return []byte(f.stdout), []byte(f.stderr), f.exitCode, nil
}

func (f *fakeRunner) lastArgs() []string {
	if len(f.calls) == 0 {
		return nil
	}
	return f.calls[len(f.calls)-1]
}

func newTestClient(r *fakeRunner) *Client {
	return &Client{Bin: "herdr", Runner: r}
}

// readFixture returns a captured-then-anonymised herdr response. Values are
// invented; the key names and nesting are copied from a live `herdr agent list`
// on 0.9.0, so a shape change fails here rather than in the field.
func readFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(raw)
}

func TestAgents_ParsesListResponse(t *testing.T) {
	r := &fakeRunner{stdout: readFixture(t, "agent-list.json")}
	agents, err := newTestClient(r).Agents(context.Background())
	if err != nil {
		t.Fatalf("Agents: %v", err)
	}
	if len(agents) != 2 {
		t.Fatalf("got %d agents, want 2", len(agents))
	}

	first := agents[0]
	// "agent" is the identity, not a targetable name. On a live session this
	// field reads "pi", and `herdr agent get pi` answers agent_not_found.
	if first.Agent != "pi" {
		t.Errorf("Agent = %q, want pi", first.Agent)
	}
	// A detected agent has no name until it is named, and herdr omits the key
	// entirely rather than sending null.
	if first.Name != "" {
		t.Errorf("Name = %q, want empty for an unnamed agent", first.Name)
	}
	if first.Status != "idle" {
		t.Errorf("Status = %q, want idle", first.Status)
	}
	if first.PaneID != "w1:p1" {
		t.Errorf("PaneID = %q, want w1:p1", first.PaneID)
	}
	if first.CWD != "/home/example/Projects/alpha" {
		t.Errorf("CWD = %q", first.CWD)
	}
	if first.WorkspaceID != "w1" || first.TabID != "w1:t1" {
		t.Errorf("workspace/tab = %q/%q, want w1/w1:t1", first.WorkspaceID, first.TabID)
	}
	if first.TerminalTitle != "π - alpha" {
		t.Errorf("TerminalTitle = %q", first.TerminalTitle)
	}
	// Herdr only reports a session for agents an integration reported one for,
	// so absence must decode to nil rather than to a zero-value session.
	if first.Session != nil {
		t.Errorf("Session = %+v, want nil for an agent with no agent_session", first.Session)
	}

	// A started agent has both a name and an identity, and they differ.
	second := agents[1]
	if second.Name != "reviewer" {
		t.Errorf("Name = %q, want reviewer", second.Name)
	}
	if second.Agent != "claude" {
		t.Errorf("Agent = %q, want claude", second.Agent)
	}
}

// An agent_session is the primary transcript source, so its value must survive
// decoding.
func TestAgents_DecodesAgentSession(t *testing.T) {
	r := &fakeRunner{stdout: readFixture(t, "agent-list-with-session.json")}
	agents, err := newTestClient(r).Agents(context.Background())
	if err != nil {
		t.Fatalf("Agents: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("got %d agents, want 1", len(agents))
	}
	if agents[0].Session == nil {
		t.Fatal("Session is nil, want the decoded agent_session")
	}
	sess := agents[0].Session
	if got := sess.Value; got != "2f1c0b7e-9a44-4a1e-9f0d-6c2b8f5a1d30" {
		t.Errorf("Session.Value = %q", got)
	}
	// Kind is what decides how Value is read, so decoding it wrong silently
	// turns a session id into a filename.
	if sess.Kind != SessionKindID {
		t.Errorf("Session.Kind = %q, want %q", sess.Kind, SessionKindID)
	}
	if sess.Agent != "claude" {
		t.Errorf("Session.Agent = %q, want claude", sess.Agent)
	}
	if sess.Source != "claude-code-hook" {
		t.Errorf("Session.Source = %q", sess.Source)
	}
}

// The server schema allows kind to be "path", in which case Value already
// locates the transcript and no searching is needed.
func TestAgents_DecodesPathKindSession(t *testing.T) {
	stdout := `{"id":"cli:agent:list","result":{"agents":[{"agent":"claude","pane_id":"w3:p1",
		"agent_session":{"agent":"claude","kind":"path","source":"claude-code-hook",
		"value":"/home/example/.claude/projects/-home-example-proj/sess.jsonl"}}],"type":"agent_list"}}`
	r := &fakeRunner{stdout: stdout}
	agents, err := newTestClient(r).Agents(context.Background())
	if err != nil {
		t.Fatalf("Agents: %v", err)
	}
	sess := agents[0].Session
	if sess == nil {
		t.Fatal("Session is nil")
	}
	if sess.Kind != SessionKindPath {
		t.Errorf("Kind = %q, want %q", sess.Kind, SessionKindPath)
	}
	if !strings.HasSuffix(sess.Value, ".jsonl") {
		t.Errorf("Value = %q, want a transcript path", sess.Value)
	}
}

// A response whose shape changed must fail loudly. Silently returning zero
// agents would look exactly like "no agents are running" and the poller would
// spin on nothing forever.
func TestAgents_RejectsUnexpectedShape(t *testing.T) {
	for _, tc := range []struct{ name, stdout string }{
		{"empty object", `{}`},
		{"missing agents key", `{"id":"cli:agent:list","result":{"type":"agent_list"}}`},
		{"result is not an object", `{"result":42}`},
		{"not json at all", `<html>gateway</html>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRunner{stdout: tc.stdout}
			_, err := newTestClient(r).Agents(context.Background())
			if err == nil {
				t.Fatal("want an error for a response this package cannot read")
			}
			if !strings.Contains(err.Error(), "unexpected") && !strings.Contains(err.Error(), "decode") {
				t.Errorf("error should say the response was unreadable, got: %v", err)
			}
		})
	}
}

// An empty list is a real answer, not an error.
func TestAgents_EmptyListIsNotAnError(t *testing.T) {
	r := &fakeRunner{stdout: `{"id":"cli:agent:list","result":{"agents":[],"type":"agent_list"}}`}
	agents, err := newTestClient(r).Agents(context.Background())
	if err != nil {
		t.Fatalf("Agents: %v", err)
	}
	if len(agents) != 0 {
		t.Errorf("got %d agents, want 0", len(agents))
	}
}

func TestAgent_ParsesGetResponse(t *testing.T) {
	r := &fakeRunner{stdout: readFixture(t, "agent-get.json")}
	agent, err := newTestClient(r).Agent(context.Background(), "w1:p1")
	if err != nil {
		t.Fatalf("Agent: %v", err)
	}
	if agent.Agent != "pi" || agent.PaneID != "w1:p1" {
		t.Errorf("got %+v", agent)
	}
	if got := r.lastArgs(); strings.Join(got, " ") != "herdr agent get w1:p1" {
		t.Errorf("args = %v", got)
	}
}

// The CLI reports a missing target as JSON on stderr with exit 1. Callers need
// to tell "no such agent" from "herdr is broken".
func TestAgent_NotFoundIsRecognisable(t *testing.T) {
	r := &fakeRunner{
		stderr:   `{"error":{"code":"agent_not_found","message":"agent target nope:nope not found"},"id":"cli:agent:get"}`,
		exitCode: 1,
	}
	_, err := newTestClient(r).Agent(context.Background(), "nope:nope")
	if err == nil {
		t.Fatal("want an error")
	}
	if !IsNotFound(err) {
		t.Errorf("IsNotFound(%v) = false, want true", err)
	}
	var cli *Error
	if !errors.As(err, &cli) {
		t.Fatalf("error is %T, want *herdr.Error", err)
	}
	if cli.Code != "agent_not_found" {
		t.Errorf("Code = %q", cli.Code)
	}
	if cli.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", cli.ExitCode)
	}
	if !strings.Contains(err.Error(), "nope:nope") {
		t.Errorf("error should name the target, got: %v", err)
	}
}

func TestRead_ReturnsRenderedText(t *testing.T) {
	r := &fakeRunner{stdout: " line one\n line two\n"}
	out, err := newTestClient(r).Read(context.Background(), "w1:p1", 120)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if out != " line one\n line two\n" {
		t.Errorf("Read returned %q; the caller decides how to trim", out)
	}
	want := "herdr agent read w1:p1 --source recent-unwrapped --lines 120 --format text"
	if got := strings.Join(r.lastArgs(), " "); got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
}

// Reading is the degraded path, so the line count must always be explicit —
// the server default is not a snapshot we can rely on.
func TestRead_DefaultsLineCount(t *testing.T) {
	for _, lines := range []int{0, -1} {
		r := &fakeRunner{stdout: "text"}
		if _, err := newTestClient(r).Read(context.Background(), "w1:p1", lines); err != nil {
			t.Fatalf("Read: %v", err)
		}
		if got := strings.Join(r.lastArgs(), " "); !strings.Contains(got, "--lines 400") {
			t.Errorf("lines=%d produced args %q, want --lines 400", lines, got)
		}
	}
}

func TestRead_EmptyTargetIsRejected(t *testing.T) {
	r := &fakeRunner{}
	if _, err := newTestClient(r).Read(context.Background(), "", 10); err == nil {
		t.Fatal("want an error for an empty target")
	}
	if len(r.calls) != 0 {
		t.Errorf("must not shell out for an empty target, ran: %v", r.calls)
	}
}

// A CLI syntax error is plain text on exit 2, with no JSON to decode.
func TestRead_UsageErrorIsReported(t *testing.T) {
	r := &fakeRunner{
		stderr:   "usage: herdr agent read <target> [--source ...]",
		exitCode: 2,
	}
	_, err := newTestClient(r).Read(context.Background(), "w1:p1", 10)
	if err == nil {
		t.Fatal("want an error")
	}
	if IsNotFound(err) {
		t.Error("a usage error is not a missing agent")
	}
	if !strings.Contains(err.Error(), "usage:") {
		t.Errorf("error should surface the CLI's own text, got: %v", err)
	}
}

// The poller runs as a detached startup hook, outside any pane, so it cannot
// rely on the caller context Herdr injects. It must be able to pin the session
// it was started for.
func TestClient_PinsSessionInChildEnv(t *testing.T) {
	r := &fakeRunner{stdout: `{"result":{"agents":[]}}`}
	c := &Client{
		Bin:        "herdr",
		Runner:     r,
		SocketPath: "/home/example/.config/herdr/sessions/work/herdr.sock",
		Session:    "work",
	}
	if _, err := c.Agents(context.Background()); err != nil {
		t.Fatalf("Agents: %v", err)
	}
	env := strings.Join(r.envs[0], "\n")
	if !strings.Contains(env, "HERDR_SOCKET_PATH=/home/example/.config/herdr/sessions/work/herdr.sock") {
		t.Errorf("child env did not pin HERDR_SOCKET_PATH:\n%s", env)
	}
	if !strings.Contains(env, "HERDR_SESSION=work") {
		t.Errorf("child env did not pin HERDR_SESSION:\n%s", env)
	}
}

// With no pinned session the child must inherit the ambient environment, or a
// plugin running inside a pane would lose its context.
func TestClient_InheritsEnvWhenNotPinned(t *testing.T) {
	r := &fakeRunner{stdout: `{"result":{"agents":[]}}`}
	if _, err := newTestClient(r).Agents(context.Background()); err != nil {
		t.Fatalf("Agents: %v", err)
	}
	if len(r.envs[0]) != 0 {
		t.Errorf("env overrides = %v, want none", r.envs[0])
	}
}

func TestClient_MissingBinaryIsAClearError(t *testing.T) {
	r := &fakeRunner{err: exec.ErrNotFound}
	_, err := newTestClient(r).Agents(context.Background())
	if err == nil {
		t.Fatal("want an error")
	}
	if IsNotFound(err) {
		t.Error("a missing binary is not a missing agent")
	}
	if !strings.Contains(err.Error(), "herdr") {
		t.Errorf("error should name the binary, got: %v", err)
	}
}

func TestClient_BinDefaultsToEnvThenPath(t *testing.T) {
	t.Setenv("HERDR_BIN_PATH", "/nix/store/xyz-herdr/bin/herdr")
	if got := (&Client{}).bin(); got != "/nix/store/xyz-herdr/bin/herdr" {
		t.Errorf("bin() = %q, want HERDR_BIN_PATH", got)
	}
	t.Setenv("HERDR_BIN_PATH", "")
	if got := (&Client{}).bin(); got != DefaultBin {
		t.Errorf("bin() = %q, want %q", got, DefaultBin)
	}
}

// Exercises the real CLI when the tests run inside a Herdr pane. Skipped on a
// bare machine, which is the normal case for CI.
func TestLiveHerdrIfPresent(t *testing.T) {
	if os.Getenv("HERDR_ENV") != "1" {
		t.Skip("not running inside a Herdr pane")
	}
	if _, err := exec.LookPath(DefaultBin); err != nil {
		t.Skipf("herdr not on PATH: %v", err)
	}

	agents, err := (&Client{}).Agents(context.Background())
	if err != nil {
		t.Fatalf("live Agents: %v", err)
	}
	if len(agents) == 0 {
		t.Fatal("a live session with a pane hosting this test must report at least one agent")
	}
	for _, a := range agents {
		if a.PaneID == "" {
			t.Errorf("agent %+v has no pane id; the response shape may have changed", a)
		}
		if !strings.Contains(a.PaneID, ":p") {
			t.Errorf("PaneID %q does not look like w<n>:p<n>", a.PaneID)
		}
	}
}

func TestExecRunner_ReturnsExitCodeAndStreams(t *testing.T) {
	// `false` exits 1 with no output; `echo` proves stdout is captured.
	stdout, _, code, err := ExecRunner{}.Run(context.Background(), "/bin/sh", []string{"-c", "echo hi; exit 3"}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
	if strings.TrimSpace(string(stdout)) != "hi" {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestExecRunner_ReportsMissingBinary(t *testing.T) {
	_, _, _, err := ExecRunner{}.Run(context.Background(), "/nonexistent/herdr", nil, nil)
	if err == nil {
		t.Fatal("want an error for a missing binary")
	}
}

// Reads the pane this test runs in, which is the only agent target guaranteed
// to exist. Verifies the read path against the real CLI, including the flag
// names.
func TestLiveHerdrReadIfPresent(t *testing.T) {
	if os.Getenv("HERDR_ENV") != "1" {
		t.Skip("not running inside a Herdr pane")
	}
	pane := os.Getenv("HERDR_PANE_ID")
	if pane == "" {
		t.Skip("no HERDR_PANE_ID in the environment")
	}

	out, err := (&Client{}).Read(context.Background(), pane, 40)
	if err != nil {
		t.Fatalf("live Read(%s): %v", pane, err)
	}
	if strings.TrimSpace(out) == "" {
		t.Errorf("live Read(%s) returned nothing; the read surface may have changed", pane)
	}
}

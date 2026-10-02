package target

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/DnzzL/herdr-huddle/internal/herdr"
)

type panesStub struct {
	agent herdr.Agent
	err   error
	asked []string
}

func (p *panesStub) Agent(_ context.Context, target string) (herdr.Agent, error) {
	p.asked = append(p.asked, target)
	return p.agent, p.err
}

// rootsStub maps a directory to the worktree containing it.
type rootsStub map[string]string

func (r rootsStub) Root(_ context.Context, dir string) (string, error) {
	if root, ok := r[dir]; ok {
		return root, nil
	}
	return "", errors.New("not a git repository")
}

// The reason this package exists: a Herdr action's working directory is the
// plugin's root, never the project's, so the pane has to win.
func TestThePaneDecidesWhatIsActedOn(t *testing.T) {
	t.Setenv("HERDR_PANE_ID", "w33:p1")
	panes := &panesStub{agent: herdr.Agent{PaneID: "w33:p1", Agent: "claude", CWD: "/projects/demo/internal"}}
	roots := rootsStub{
		"/projects/demo/internal": "/projects/demo",
		"/plugins/herdr-huddle":   "/plugins/herdr-huddle", // what an action's cwd would be
	}

	got, warnings := Resolve(context.Background(), "/plugins/herdr-huddle", panes, roots)
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
	if got.Root != "/projects/demo" {
		t.Errorf("Root = %q, want the agent's project and not the caller's directory", got.Root)
	}
	if got.PaneID != "w33:p1" || got.Agent != "claude" {
		t.Errorf("got %+v, want the pane and its agent", got)
	}
	if !got.FromPane {
		t.Error("FromPane must say the answer came from the agent")
	}
}

// Typed in a shell with no pane, the caller's own directory is the answer.
func TestWithoutAPaneTheDirectoryDecides(t *testing.T) {
	t.Setenv("HERDR_PANE_ID", "")
	roots := rootsStub{"/projects/demo": "/projects/demo"}

	got, warnings := Resolve(context.Background(), "/projects/demo", nil, roots)
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
	if got.Root != "/projects/demo" || got.PaneID != "" || got.FromPane {
		t.Errorf("got %+v, want the caller's own project", got)
	}
}

// Herdr being unreachable is not a reason to refuse to act: fall back, and say
// so, rather than stop because the obvious answer could not be confirmed.
func TestAnUnreadablePaneFallsBackAndSaysSo(t *testing.T) {
	t.Setenv("HERDR_PANE_ID", "w9:p9")
	panes := &panesStub{err: errors.New("agent_not_found")}
	roots := rootsStub{"/projects/demo": "/projects/demo"}

	got, warnings := Resolve(context.Background(), "/projects/demo", panes, roots)
	if got.Root != "/projects/demo" {
		t.Errorf("Root = %q, want the fallback", got.Root)
	}
	if got.FromPane {
		t.Error("FromPane must be false when the pane could not be read")
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "w9:p9") {
		t.Errorf("warnings = %v, want one naming the pane", warnings)
	}
}

// An agent working outside any repository is reported, not silently treated as
// though it worked in the caller's one — that would be the old bug again.
func TestAnAgentOutsideARepositoryDoesNotBorrowTheCallersProject(t *testing.T) {
	t.Setenv("HERDR_PANE_ID", "w1:p1")
	panes := &panesStub{agent: herdr.Agent{PaneID: "w1:p1", Agent: "pi", CWD: "/tmp"}}
	roots := rootsStub{"/projects/demo": "/projects/demo"}

	got, warnings := Resolve(context.Background(), "/projects/demo", panes, roots)
	if got.Root != "" {
		t.Errorf("Root = %q, want nothing: the agent is not in a repository", got.Root)
	}
	if got.PaneID != "w1:p1" {
		t.Errorf("PaneID = %q, want the pane kept", got.PaneID)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "/tmp") {
		t.Errorf("warnings = %v, want one naming the directory", warnings)
	}
}

// Herdr's own id for the pane beats the one the environment claimed.
func TestHerdrsPaneIDWins(t *testing.T) {
	t.Setenv("HERDR_PANE_ID", "stale")
	panes := &panesStub{agent: herdr.Agent{PaneID: "w2:p2", Agent: "pi", CWD: "/projects/demo"}}
	roots := rootsStub{"/projects/demo": "/projects/demo"}

	got, _ := Resolve(context.Background(), "/elsewhere", panes, roots)
	if got.PaneID != "w2:p2" {
		t.Errorf("PaneID = %q, want the id Herdr reports", got.PaneID)
	}
	if panes.asked[0] != "stale" {
		t.Errorf("asked about %q, want the environment's id to be what is looked up", panes.asked[0])
	}
}

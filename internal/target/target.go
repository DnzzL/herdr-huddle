// Package target answers one question for every command: on what?
//
// It exists because the answer used to come from two places that could
// disagree — the repository from the process's working directory, the agent
// from HERDR_PANE_ID — and a Herdr action makes that arrangement untenable
// rather than merely fragile: an action's working directory is the plugin's
// root, never the project's (ADR-009).
package target

import (
	"context"
	"os"
	"strings"

	"github.com/DnzzL/herdr-huddle/internal/herdr"
	"github.com/DnzzL/herdr-huddle/internal/repo"
)

// Target is what a command acts on, however it was invoked.
type Target struct {
	// PaneID is the pane hosting the agent, empty when there is none.
	PaneID string
	// Root is the git worktree the command acts on — the project. Empty when
	// no directory in play is a repository.
	Root string
	// Agent is the agent's kind, as Herdr reports it. Empty without a pane.
	Agent string
	// FromPane records that Root came from the agent rather than from the
	// caller's directory, which is what callers warn about.
	FromPane bool
}

// Panes reads the agent in a pane. It is an interface so resolution can be
// tested without a Herdr session.
type Panes interface {
	Agent(ctx context.Context, target string) (herdr.Agent, error)
}

// Roots reports the worktree root containing a directory.
type Roots interface {
	Root(ctx context.Context, dir string) (string, error)
}

// GitRoots asks git, which is the only thing that knows.
type GitRoots struct{}

func (GitRoots) Root(ctx context.Context, dir string) (string, error) {
	return (&repo.Repo{Dir: dir}).Root(ctx)
}

// Resolve returns what this invocation acts on.
//
// The pane wins when there is one. That is the whole point: a command invoked
// as a Herdr action has a pane and a useless working directory, and a command
// typed in a pane has both — agreeing, in the ordinary case, and the pane is
// the one that cannot be wrong about which agent is meant.
//
// A failure to read the pane is not a failure to resolve: the caller falls
// back to its own directory and says so, because refusing to act when Herdr is
// merely unreachable would be worse than acting on the obvious thing.
func Resolve(ctx context.Context, dir string, panes Panes, roots Roots) (Target, []string) {
	var warnings []string

	pane := strings.TrimSpace(os.Getenv("HERDR_PANE_ID"))
	if pane != "" && panes != nil {
		agent, err := panes.Agent(ctx, pane)
		switch {
		case err != nil:
			warnings = append(warnings, "could not read the agent in pane "+pane+": "+err.Error())
		case agent.CWD == "":
			warnings = append(warnings, "the agent in pane "+pane+" reports no working directory")
		default:
			found := Target{PaneID: paneOf(agent, pane), Agent: agent.Agent}
			root, err := roots.Root(ctx, agent.CWD)
			if err != nil {
				warnings = append(warnings, "the agent in pane "+pane+" works in "+agent.CWD+", which git does not know as a repository")
				return found, warnings
			}
			found.Root = root
			found.FromPane = true
			return found, warnings
		}
	}

	// No pane, or none that could be read: the caller's own directory.
	if root, err := roots.Root(ctx, dir); err == nil {
		return Target{Root: root}, warnings
	}
	return Target{}, warnings
}

// paneOf prefers the id Herdr reports over the one the environment claimed.
func paneOf(agent herdr.Agent, fallback string) string {
	if agent.PaneID != "" {
		return agent.PaneID
	}
	return fallback
}

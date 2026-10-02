// Package herdr wraps the read-only parts of the herdr CLI that herdr-huddle
// needs: discovering live agents and reading one agent's rendered output.
//
// Everything here is deliberately read-only. The CLI is the integration point,
// so commands are never guessed at — the argument lists are the ones printed by
// `herdr agent` on 0.9.0, and the response shapes are those a live session
// actually returned.
//
// Two conventions of the CLI matter and are handled here so no caller has to
// know them: a successful command writes JSON to stdout, while a failure writes
// a JSON error object to stderr and exits 1. A syntax error instead prints a
// plain usage line and exits 2.
package herdr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// DefaultBin is used when neither Client.Bin nor HERDR_BIN_PATH is set.
const DefaultBin = "herdr"

// DefaultReadLines is the snapshot size used when the caller does not specify
// one. The CLI's own default is not a snapshot we can reason about, so the line
// count is always passed explicitly.
const DefaultReadLines = 400

// Runner runs one command and reports its streams and exit code. The exit code
// is only meaningful when err is nil; err is reserved for a failure to run the
// command at all, such as a missing binary. It exists so tests never spawn
// herdr.
type Runner interface {
	Run(ctx context.Context, name string, args []string, env []string) (stdout, stderr []byte, exitCode int, err error)
}

// ExecRunner runs commands as real child processes. The extra env entries are
// appended to the ambient environment.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args []string, env []string) ([]byte, []byte, int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return []byte(stdout.String()), []byte(stderr.String()), 0, nil
	}

	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return []byte(stdout.String()), []byte(stderr.String()), exit.ExitCode(), nil
	}
	// A failure to run the command at all. The caller names the binary, so this
	// does not, and every Runner gets the same treatment.
	return nil, nil, -1, err
}

// Agent is one entry from `herdr agent list`, as defined by the server's own
// schema (`herdr api schema`, AgentInfo).
//
// The three name-ish fields are easy to confuse and the difference matters:
// "agent" is the agent's identity, which for a detected (rather than started)
// agent is its kind; "name" is the live agent name that can be used as a
// command target, and is absent until the agent is named; "display_agent" is
// for presentation. Targeting by the "agent" field does not work: on a live
// session `herdr agent get pi` returns agent_not_found, because `pi` is a kind
// and not a name. Always prefer PaneID, which the schema marks required.
type Agent struct {
	Agent         string        `json:"agent"`
	Name          string        `json:"name"`
	DisplayAgent  string        `json:"display_agent"`
	Status        string        `json:"agent_status"`
	CWD           string        `json:"cwd"`
	PaneID        string        `json:"pane_id"`
	WorkspaceID   string        `json:"workspace_id"`
	TabID         string        `json:"tab_id"`
	TerminalTitle string        `json:"terminal_title"`
	Session       *AgentSession `json:"agent_session"`
}

// Agent kinds, as herdr reports them in Agent.Agent. A kind with a transcript
// adapter behind it is listed here; every other kind is read from the terminal.
// See ADR-004.
const (
	KindClaude = "claude"
	KindPi     = "pi"
)

// Agent status, as herdr reports it. The poller's behaviour depends on these
// values: a turn is published once the agent is no longer working, and an
// instruction is held back rather than typed into a busy terminal.
const (
	StatusIdle    = "idle"
	StatusWorking = "working"
	StatusBlocked = "blocked"
	StatusDone    = "done"
	StatusUnknown = "unknown"
)

// Busy reports whether the agent is in the middle of something, so that a new
// instruction would land inside a turn instead of starting one.
//
// Blocked counts as busy: herdr refuses a prompt to a blocked agent outright,
// which is the guarantee that an approval is only ever answered at the
// terminal.
func (a Agent) Busy() bool {
	return a.Status == StatusWorking || a.Status == StatusBlocked
}

// Label names the agent for the shared thread, preferring the name meant for
// people over the identity meant for the machine.
func (a Agent) Label() string {
	for _, candidate := range []string{a.DisplayAgent, a.Agent} {
		if candidate != "" {
			return candidate
		}
	}
	return "agent"
}

// AgentSessionKind says how to read AgentSession.Value, which is the whole
// point of the field. Both values are documented by the server's schema in an
// enum called AgentSessionRefKind.
type AgentSessionKind string

const (
	// SessionKindID means Value is a session identifier, so the caller has to
	// find the transcript itself.
	SessionKindID AgentSessionKind = "id"
	// SessionKindPath means Value is already the location of the session's
	// transcript, and no searching is needed.
	SessionKindPath AgentSessionKind = "path"
)

// AgentSession identifies the agent's own session, and is the primary source
// for the transcript. Herdr reports it only when an integration has reported it
// (see the pane.report_agent_session API method), so it is absent for agents
// Herdr can only detect. Callers must therefore always have a fallback.
//
// All four fields are required by the schema. Nothing here is inferred.
type AgentSession struct {
	// Agent is the agent this session belongs to.
	Agent string `json:"agent"`
	// Kind says whether Value is an id or a path.
	Kind AgentSessionKind `json:"kind"`
	// Source names the integration that reported the session.
	Source string `json:"source"`
	// Value is the session id or transcript path, per Kind.
	Value string `json:"value"`
}

// Client issues read-only queries against the herdr CLI.
type Client struct {
	// Bin overrides the herdr executable. Empty means HERDR_BIN_PATH, then
	// DefaultBin.
	Bin string
	// SocketPath pins the session socket for the child process. Required when
	// the caller is not itself running inside a pane, which is the case for a
	// startup hook.
	SocketPath string
	// Session pins the named session for the child process.
	Session string
	// Runner is injectable for tests.
	Runner Runner
}

func (c *Client) bin() string {
	if c.Bin != "" {
		return c.Bin
	}
	if v := os.Getenv("HERDR_BIN_PATH"); v != "" {
		return v
	}
	return DefaultBin
}

func (c *Client) runner() Runner {
	if c.Runner != nil {
		return c.Runner
	}
	return ExecRunner{}
}

// env returns the environment overrides for a child process. When nothing is
// pinned the child inherits the ambient environment untouched.
func (c *Client) env() []string {
	var env []string
	if c.SocketPath != "" {
		env = append(env, "HERDR_SOCKET_PATH="+c.SocketPath)
	}
	if c.Session != "" {
		env = append(env, "HERDR_SESSION="+c.Session)
	}
	return env
}

// Error is a failure reported by the herdr CLI.
type Error struct {
	// Code is the CLI's machine-readable code, empty when the CLI failed before
	// it could classify the error, as on a syntax error.
	Code string
	// Message is the human-readable counterpart of Code.
	Message string
	// Command is the CLI handler that failed, such as "cli:agent:read".
	Command string
	// ExitCode is the process exit status.
	ExitCode int
	// Stderr is the raw stderr, kept for the cases where it is not JSON.
	Stderr string
}

func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("herdr: %s: %s", e.Code, e.Message)
	}
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		return fmt.Sprintf("herdr: command failed with exit status %d", e.ExitCode)
	}
	return fmt.Sprintf("herdr: command failed with exit status %d: %s", e.ExitCode, msg)
}

// IsNotFound reports whether err says the target agent does not exist, as
// opposed to herdr being unreachable or unhappy.
func IsNotFound(err error) bool {
	var cli *Error
	return errors.As(err, &cli) && cli.Code == "agent_not_found"
}

// call runs one CLI command and returns its stdout.
func (c *Client) call(ctx context.Context, args ...string) ([]byte, error) {
	name := c.bin()
	stdout, stderr, code, err := c.runner().Run(ctx, name, args, c.env())
	if err != nil {
		return nil, fmt.Errorf("herdr: run %s: %w", name, err)
	}
	if code == 0 {
		return stdout, nil
	}

	cliErr := &Error{ExitCode: code, Stderr: string(stderr)}
	var envelope struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		ID string `json:"id"`
	}
	if json.Unmarshal(stderr, &envelope) == nil && envelope.Error != nil {
		cliErr.Code = envelope.Error.Code
		cliErr.Message = envelope.Error.Message
		cliErr.Command = envelope.ID
	}
	return nil, cliErr
}

// Agents lists the live agents in the session.
func (c *Client) Agents(ctx context.Context) ([]Agent, error) {
	stdout, err := c.call(ctx, "agent", "list")
	if err != nil {
		return nil, err
	}

	// The slice is a pointer so a response without an "agents" key is a loud
	// error. Decoding into a plain slice would make a changed response shape
	// indistinguishable from "no agents are running".
	var envelope struct {
		Result struct {
			Agents *[]Agent `json:"agents"`
		} `json:"result"`
	}
	if err := json.Unmarshal(stdout, &envelope); err != nil {
		return nil, fmt.Errorf("herdr: decode agent list: %w", err)
	}
	if envelope.Result.Agents == nil {
		return nil, fmt.Errorf("herdr: unexpected agent list response: no agents field in %s", truncateForError(stdout))
	}
	return *envelope.Result.Agents, nil
}

// Agent returns one agent by unique name or by the pane id hosting it.
func (c *Client) Agent(ctx context.Context, target string) (Agent, error) {
	if target == "" {
		return Agent{}, errors.New("herdr: agent target is required")
	}
	stdout, err := c.call(ctx, "agent", "get", target)
	if err != nil {
		return Agent{}, err
	}

	var envelope struct {
		Result struct {
			Agent *Agent `json:"agent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(stdout, &envelope); err != nil {
		return Agent{}, fmt.Errorf("herdr: decode agent get: %w", err)
	}
	if envelope.Result.Agent == nil {
		return Agent{}, fmt.Errorf("herdr: unexpected agent get response: no agent field in %s", truncateForError(stdout))
	}
	return *envelope.Result.Agent, nil
}

// Read returns the agent's rendered output as text. This is the fallback
// transcript source, so what comes back is a terminal snapshot: a fragment of
// the conversation, not the whole of it.
//
// The text is returned exactly as the CLI printed it. Trimming is the caller's
// business, because the caller knows whether it is looking at a snapshot or a
// transcript.
func (c *Client) Read(ctx context.Context, target string, lines int) (string, error) {
	if target == "" {
		return "", errors.New("herdr: agent target is required")
	}
	if lines <= 0 {
		lines = DefaultReadLines
	}
	stdout, err := c.call(ctx,
		"agent", "read", target,
		"--source", "recent-unwrapped",
		"--lines", strconv.Itoa(lines),
		"--format", "text",
	)
	if err != nil {
		return "", err
	}
	return string(stdout), nil
}

// Prompt submits text to an agent, as if it had been typed at the terminal.
//
// The text is one argument, not a shell string, so its contents cannot become
// a command. Herdr itself enforces the rule ADR-001 depends on most: a prompt
// to a **blocked** agent is refused with agent_blocked before any input is
// sent, so an approval the agent is waiting for can never be answered by
// anything but the operator.
func (c *Client) Prompt(ctx context.Context, target, text string) error {
	if target == "" {
		return errors.New("herdr: agent target is required")
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("herdr: refusing to submit an empty prompt")
	}
	_, err := c.call(ctx, "agent", "prompt", target, text)
	return err
}

// truncateForError keeps a decode failure readable when a response is large.
func truncateForError(raw []byte) string {
	const max = 200
	s := strings.TrimSpace(string(raw))
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// SoundRequest is the sound `herdr notification show --sound` plays for
// something waiting on a person. The vocabulary is Herdr's, not ours, and this
// is the one of its sounds that means what our questions mean.
const (
	SoundRequest = "request"
	// SoundDone is Herdr's sound for something that finished rather than
	// something waiting on a person.
	SoundDone = "done"
)

// Notify raises a notification on the operator's own screen.
//
// It exists because `serve` asks the operator questions — somebody is at the
// door, somebody wants to send the agent an instruction — and the operator is
// watching their agent, not the log those questions are printed in. A question
// nobody sees is a person waiting on nothing.
//
// Herdr owns the surface: it already toasts and plays a sound for agent state
// changes, so routing ours through the same door means one place to configure
// and one place they appear. A failure is the caller's to ignore — a missing
// toast must never stop the question being asked.
func (c *Client) Notify(ctx context.Context, title, body, sound string) error {
	if strings.TrimSpace(title) == "" {
		return errors.New("herdr: a notification needs a title")
	}
	args := []string{"notification", "show", title}
	if body != "" {
		args = append(args, "--body", body)
	}
	if sound != "" {
		args = append(args, "--sound", sound)
	}
	_, err := c.call(ctx, args...)
	return err
}

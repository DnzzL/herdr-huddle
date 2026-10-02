// Package poll is the process that keeps a share's thread and its agent in
// step, in both directions.
//
// ADR-001 makes it a single PID-locked process started from the plugin's
// startup hook, because the hook re-fires on every Herdr server start (and on a
// live handoff) and two pollers would inject every instruction twice.
package poll

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"time"

	"github.com/DnzzL/herdr-huddle/internal/github"
	"github.com/DnzzL/herdr-huddle/internal/herdr"
	"github.com/DnzzL/herdr-huddle/internal/session"
	"github.com/DnzzL/herdr-huddle/internal/share"
	"github.com/DnzzL/herdr-huddle/internal/thread"
	"github.com/DnzzL/herdr-huddle/internal/transcript"
)

// DefaultInterval is how often the thread is polled. ADR-001 chooses 10s:
// human planning conversation tolerates the latency, and the ETag on the
// comment list keeps the cost off the rate limit.
const DefaultInterval = 10 * time.Second

// DefaultMaxFailures is how many passes in a row may fail before the poller
// stops. Six is about a minute at the default interval: long enough for a
// transient problem, short enough that a poller whose Herdr server has gone
// does not sit there pretending to work. The startup hook starts it again on
// the next server start.
const DefaultMaxFailures = 6

// Herdr is the part of the Herdr client the poller uses. It is an interface so
// the whole poll can be exercised without a running server.
type Herdr interface {
	Agents(ctx context.Context) ([]herdr.Agent, error)
	Read(ctx context.Context, target string, lines int) (string, error)
	Prompt(ctx context.Context, target, text string) error
}

// Forge is the part of GitHub the poller uses.
type Forge interface {
	CreateComment(ctx context.Context, owner, repo string, number int, body string) (github.Comment, error)
	ListComments(ctx context.Context, owner, repo string, number int, since time.Time, etag string) (github.Comments, error)
	Acknowledge(ctx context.Context, owner, repo string, commentID int64, content string) error
}

// Options tunes a poller. The zero value is the intended default.
type Options struct {
	Interval time.Duration
	// Lines is how much terminal scrollback the fallback transcript reads.
	Lines int
	// IncludeThinking publishes the agent's reasoning. Off by default: the
	// thread is for planning together, and a person reads it.
	IncludeThinking bool
	// Home is where the agent's session files are looked for.
	Home string
	// MaxFailures is how many consecutive failed passes end the process.
	MaxFailures int
	Now         func() time.Time
}

func (o Options) maxFailures() int {
	if o.MaxFailures <= 0 {
		return DefaultMaxFailures
	}
	return o.MaxFailures
}

func (o Options) interval() time.Duration {
	if o.Interval <= 0 {
		return DefaultInterval
	}
	return o.Interval
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// Poller keeps every active share in step.
type Poller struct {
	Shares  share.Store
	Herdr   Herdr
	Forge   Forge
	Options Options
	// Log receives one line per event. A daemon with no terminal has nowhere
	// else to say what it did.
	Log func(format string, args ...any)
	// Alert tells the operator something only they can fix.
	//
	// Logging is not telling: the poller writes to a file nobody opens, and a
	// dead token does not even fail a pass — every share reports a warning and
	// the daemon carries on, silently recording nothing. Optional; without it
	// the behaviour is what it was.
	Alert func(title, body string)

	// Refused comments never advance the cursor — the operator may add the
	// author to the allowlist and expect the next pass to deliver it — so they
	// are reported once each rather than every pass.
	refusedUpTo int64
	// The same for an instruction held back while the agent is busy.
	acknowledged int64
	// warnedUnusable latches the credential alert. A daemon that notifies
	// every ten seconds is a daemon the operator turns off, so it fires once
	// and re-arms only after GitHub accepts the token again.
	warnedUnusable bool
}

// Result is what one pass did, which is also what a test asserts on.
type Result struct {
	Posted   []string
	Injected []string
	Refused  []thread.Refusal
	Retired  []string
	Warnings []string
}

// credentialCheck raises the one alarm the operator has to act on, once.
//
// It is called with every per-share failure because that is where a refused
// credential shows up: a dead token never fails a *pass*, it fails each share
// and leaves the daemon running with nothing to show for it.
func (p *Poller) credentialCheck(err error) {
	if !github.IsUnauthorized(err) {
		return
	}
	if p.warnedUnusable {
		return
	}
	p.warnedUnusable = true
	p.logf("poll: GitHub no longer accepts the stored token; nothing will be recorded until `herdr-huddle auth login` runs")
	if p.Alert != nil {
		p.Alert("herdr-huddle: GitHub no longer accepts your token",
			"Nothing is being recorded. Run: herdr-huddle auth login")
	}
}

// credentialAccepted re-arms the alarm once GitHub answers normally again, so
// a token replaced and later expired is reported a second time.
func (p *Poller) credentialAccepted() { p.warnedUnusable = false }

func (p *Poller) logf(format string, args ...any) {
	if p.Log != nil {
		p.Log(format, args...)
	}
}

func (p *Poller) home() string {
	if p.Options.Home != "" {
		return p.Options.Home
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

// Run polls until the context is cancelled, or until enough passes in a row
// have failed to conclude that nothing will work again.
//
// A single failure does not end the loop: a network blip, an expired token or a
// repository the operator lost access to must not take the poller down and leave
// every share silently unserved. A run of failures is different — the usual
// cause is that the Herdr server this poller belongs to has gone, and a process
// that outlives its reason to exist is worse than one that exits visibly.
func (p *Poller) Run(ctx context.Context) error {
	ticker := time.NewTicker(p.Options.interval())
	defer ticker.Stop()

	failures := 0
	p.logf("poll: started, every %s", p.Options.interval())
	for {
		select {
		case <-ctx.Done():
			p.logf("poll: stopping")
			return nil
		case <-ticker.C:
			_, err := p.Once(ctx)
			if err == nil {
				failures = 0
				continue
			}
			failures++
			if failures >= p.Options.maxFailures() {
				return fmt.Errorf("poll: giving up after %d failed passes: %w", failures, err)
			}
			p.logf("poll: pass failed (%d of %d): %v", failures, p.Options.maxFailures(), err)
		}
	}
}

// Once makes one pass over every active share.
//
// It returns an error only when the state itself cannot be read, or when Herdr
// cannot be asked who is running. A failure on one share is reported and the
// others are still served: one broken thread must not stop the machine.
func (p *Poller) Once(ctx context.Context) (Result, error) {
	var out Result
	states, err := p.Shares.Load()
	if err != nil {
		return out, err
	}

	// A poller with nothing to serve costs a file read and no more. It is up
	// because Herdr has no event to start it with (ADR-001), not because there
	// is work to do, and asking Herdr who is running every 10s forever to learn
	// nothing would be pure waste.
	served := 0
	for _, st := range states {
		if st.Active() {
			served++
		}
	}
	if served == 0 {
		return out, nil
	}

	// One call for every share, and the list is also the liveness check: an
	// agent that is not in it has exited or its pane has closed.
	agents, err := p.Herdr.Agents(ctx)
	if err != nil {
		return out, fmt.Errorf("poll: list agents: %w", err)
	}
	byPane := make(map[string]herdr.Agent, len(agents))
	for _, a := range agents {
		byPane[a.PaneID] = a
	}

	for _, st := range states {
		if !st.Active() {
			continue
		}
		before := st
		agent, alive := byPane[st.Origin.PaneID]

		switch {
		case st.Origin.PaneID == "":
			// A share with no agent cannot be driven or synced. It is retired
			// rather than left in the poller, and re-running `share` from the
			// pane revives it.
			p.retire(&st, "no agent was recorded for this share; re-run `herdr-huddle share` from the agent's pane", &out)
		case !alive:
			p.retire(&st, fmt.Sprintf("the agent in pane %s is gone", st.Origin.PaneID), &out)
		default:
			if err := p.serve(ctx, &st, agent, &out); err != nil {
				out.Warnings = append(out.Warnings, fmt.Sprintf("%s: %v", st.Branch, err))
				p.logf("poll: %s: %v", st.Branch, err)
				p.credentialCheck(err)
			}
		}

		if !reflect.DeepEqual(before, st) {
			st.UpdatedAt = p.Options.now()
			// Save, not Put: the allowlist may have grown at the door since
			// this record was read (ADR-007), and the poller does not own it.
			if err := p.Shares.Save(st); err != nil {
				return out, err
			}
		}
	}
	return out, nil
}

func (p *Poller) retire(st *share.State, why string, out *Result) {
	now := p.Options.now()
	st.RetiredAt = &now
	out.Retired = append(out.Retired, st.Branch)
	p.logf("poll: retired %s: %s", st.Branch, why)
}

// serve syncs one share, agent turn outwards first and instructions inwards
// second, so an answer is in the thread before the next question lands.
func (p *Poller) serve(ctx context.Context, st *share.State, agent herdr.Agent, out *Result) error {
	// A blocked episode ends when the agent is no longer blocked, so the next
	// one is announced too.
	if agent.Status != herdr.StatusBlocked {
		st.Cursors.Blocked = false
	}
	// Blocked is a pause, not progress: the agent has stopped producing to ask a
	// question, so its turn is complete and belongs in the thread *before* the
	// note saying it is waiting. `Busy()` is the wrong predicate here for exactly
	// that reason — it is the inbound direction that has to wait for a blocked
	// agent, not the outbound one. Only `working` means "mid-turn, not yet".
	if agent.Status != herdr.StatusWorking {
		if err := p.publish(ctx, st, agent, out); err != nil {
			return err
		}
	}
	return p.deliver(ctx, st, agent, out)
}

// publish posts whatever the agent has said since the last poll.
func (p *Poller) publish(ctx context.Context, st *share.State, agent herdr.Agent, out *Result) error {
	src, err := p.source(st, agent)
	if err != nil {
		return err
	}
	owner, repo, err := ownerRepo(st)
	if err != nil {
		return err
	}

	var turn thread.Turn
	if src.Path == "" {
		// No session file: the terminal is the only transcript there is. It has
		// no cursor, so the content itself decides whether anything changed.
		text, err := p.Herdr.Read(ctx, agent.PaneID, p.Options.Lines)
		if err != nil {
			return fmt.Errorf("read the terminal transcript: %w", err)
		}
		turn = thread.SnapshotTurn(text)
		if turn.Empty() || turn.Digest() == st.Cursors.Snapshot {
			return nil
		}
	} else {
		turn, err = thread.ReadTurn(src.Kind, src.Path, st.Cursors.Transcript, transcript.Options{IncludeThinking: p.Options.IncludeThinking})
		if err != nil {
			return err
		}
		if turn.Empty() {
			return nil
		}
		if turn.Rotated {
			// The cursor is not in the file, so this "turn" is the whole
			// conversation again. Publishing it would repeat the thread; the
			// local session file is the record, so the cursor moves on and the
			// gap is named instead.
			st.Cursors.Transcript = turn.Cursor
			out.Warnings = append(out.Warnings, fmt.Sprintf(
				"%s: the session file no longer contains the last position; skipped a turn rather than repeating the whole conversation", st.Branch))
			return nil
		}
	}

	body := thread.Comment(turn, agent.Label(), p.Options.now())
	comment, err := p.Forge.CreateComment(ctx, owner, repo, st.Number, body)
	if err != nil {
		return fmt.Errorf("post the agent's turn: %w", err)
	}

	if turn.Partial {
		st.Cursors.Snapshot = turn.Digest()
		out.Posted = append(out.Posted, fmt.Sprintf("%s: terminal snapshot %s", st.Branch, comment.HTMLURL))
		p.logf("poll: %s: posted a terminal snapshot (%s)", st.Branch, comment.HTMLURL)
		return nil
	}
	// The cursor advances only after the comment is posted, so a failure
	// reposts the same turn rather than losing it.
	st.Cursors.Transcript = turn.Cursor
	out.Posted = append(out.Posted, fmt.Sprintf("%s: %s", st.Branch, comment.HTMLURL))
	p.logf("poll: %s: posted a turn (%s)", st.Branch, comment.HTMLURL)
	return nil
}

// source is the transcript's location, re-resolving it when the recorded file
// has gone — an agent that restarted has a new session, and the pane's working
// directory is what finds it.
func (p *Poller) source(st *share.State, agent herdr.Agent) (session.Source, error) {
	if st.Origin.Session != "" {
		if _, err := os.Stat(st.Origin.Session); err == nil {
			if sameAdapter(st.Origin.Kind, agent.Agent) {
				return session.Source{Path: st.Origin.Session, Kind: st.Origin.Kind}, nil
			}
			// The pane is running a different agent now, so the file left behind
			// is another agent's format. A parser that does not recognise it
			// renders nothing at all, and silence is the one failure nobody
			// notices, so the transcript is looked up again instead.
			p.logf("poll: %s: %s now runs %s, looking for its session", st.Branch, agent.PaneID, agent.Label())
		} else {
			p.logf("poll: %s: session file %s is gone, looking for a new one", st.Branch, st.Origin.Session)
		}
		st.Origin.Session = ""
		// A new session is a new conversation: the old cursor names a record
		// that is not in it, and holding on to it would silently drop the new
		// session's first turns.
		st.Cursors.Transcript = ""
	}

	src, err := session.Resolve(p.home(), agent)
	if err != nil {
		return session.Source{}, err
	}
	st.Origin.Session = src.Path
	st.Origin.Kind = src.Kind
	st.Origin.Partial = src.Partial
	if src.Path != "" {
		if id, err := session.SessionID(src.Path); err == nil {
			st.Origin.SessionID = id
		}
	}
	return src, nil
}

// sameAdapter reports whether two agent kinds are read by the same transcript
// adapter. A share recorded before there was a second adapter has no kind
// stored, and Claude Code was the only one there was — comparing the kinds as
// stored would re-resolve those shares on every pass and lose their cursor.
func sameAdapter(a, b string) bool {
	if a == "" {
		a = herdr.KindClaude
	}
	if b == "" {
		b = herdr.KindClaude
	}
	return a == b
}

// deliver reads the thread and types anything allowed into the agent.
func (p *Poller) deliver(ctx context.Context, st *share.State, agent herdr.Agent, out *Result) error {
	owner, repo, err := ownerRepo(st)
	if err != nil {
		return err
	}

	comments, err := p.Forge.ListComments(ctx, owner, repo, st.Number, time.Time{}, st.Cursors.ETag)
	if err == nil {
		// GitHub answered, so the credential works. Re-arm the alarm here and
		// nowhere else: a pass that merely had nothing to do proves nothing.
		p.credentialAccepted()
	}
	if err != nil {
		return fmt.Errorf("read the thread: %w", err)
	}
	if comments.NotModified {
		return nil
	}
	// The ETag is remembered before anything is delivered: a failure later in
	// this pass must not throw away a valid one.
	st.Cursors.ETag = comments.ETag

	instructions, refused := thread.Instructions(comments.Items, st.Allowlist, st.Cursors.Comment)
	out.Refused = append(out.Refused, refused...)
	p.noteRefusals(st, refused)

	if agent.Status == herdr.StatusBlocked {
		p.noteBlocked(ctx, st, owner, repo, out)
	}

	for _, ins := range instructions {
		if agent.Busy() {
			// Queued, not dropped (ADR-001). The cursor stays where it is, so
			// the instruction is picked up on the first pass that finds the
			// agent free — in the order it was written, because delivery stops
			// here rather than skipping ahead.
			//
			// The acknowledgement and the note are recorded as done once: a
			// refused or held comment keeps its id, so this loop sees it again
			// on every pass, and repeating either one every 10s would be noise
			// in the log and quota spent on nothing.
			if ins.CommentID > p.acknowledged {
				if err := p.Forge.Acknowledge(ctx, owner, repo, ins.CommentID, "eyes"); err != nil {
					out.Warnings = append(out.Warnings, fmt.Sprintf("%s: could not acknowledge @%s's comment: %v", st.Branch, ins.Author, err))
				}
				p.acknowledged = ins.CommentID
				out.Warnings = append(out.Warnings, fmt.Sprintf(
					"%s: held back @%s's instruction: the agent is %s", st.Branch, ins.Author, agent.Status))
			}
			return nil
		}

		if err := p.Herdr.Prompt(ctx, agent.PaneID, ins.Prompt()); err != nil {
			// The usual cause is herdr refusing the prompt — a blocked agent,
			// or one that has gone. The cursor does not advance, so the
			// instruction is retried rather than lost.
			return fmt.Errorf("deliver @%s's instruction: %w", ins.Author, err)
		}
		st.Cursors.Comment = ins.CommentID
		out.Injected = append(out.Injected, fmt.Sprintf("%s: @%s", st.Branch, ins.Author))
		p.logf("poll: %s: delivered @%s's instruction (%s)", st.Branch, ins.Author, ins.URL)
	}
	return nil
}

// noteBlocked tells the thread that the agent is waiting for an approval.
//
// ADR-001 makes this the one thing the poller must never do on the operator's
// behalf: the permission gate is not delegated, and only the operator answers
// it. Saying so in the thread is what stops the other person waiting for
// progress that cannot come.
func (p *Poller) noteBlocked(ctx context.Context, st *share.State, owner, repo string, out *Result) {
	if st.Cursors.Blocked {
		return // already said so; the episode is still the same one
	}
	st.Cursors.Blocked = true

	body := thread.Marker + "\n**Waiting on the operator**\n\n" +
		"The agent is blocked on an approval at the operator's terminal. " +
		"This is deliberate: the permission prompt is never answered for them, " +
		"and it is never forwarded here.\n"
	if _, err := p.Forge.CreateComment(ctx, owner, repo, st.Number, body); err != nil {
		st.Cursors.Blocked = false // try again next pass rather than going quiet
		out.Warnings = append(out.Warnings, fmt.Sprintf("%s: could not say the agent is blocked: %v", st.Branch, err))
		return
	}
	out.Posted = append(out.Posted, fmt.Sprintf("%s: waiting on the operator", st.Branch))
	p.logf("poll: %s: agent is blocked, said so in the thread", st.Branch)
}

// noteRefusals reports each refused comment once. A refusal is not an error and
// does not advance any cursor, so without this the same line would be logged
// every ten seconds for as long as the comment exists.
func (p *Poller) noteRefusals(st *share.State, refused []thread.Refusal) {
	for _, r := range refused {
		if r.CommentID <= p.refusedUpTo {
			continue
		}
		p.refusedUpTo = r.CommentID
		p.logf("poll: %s: ignoring @%s: %s", st.Branch, r.Author, r.Reason)
	}
}

func ownerRepo(st *share.State) (string, string, error) {
	owner, repo := st.Owner()
	if owner == "" || repo == "" {
		return "", "", fmt.Errorf("share state has an unusable repository %q", st.Repo)
	}
	if st.Number <= 0 {
		return "", "", errors.New("share state has no pull request number")
	}
	return owner, repo, nil
}

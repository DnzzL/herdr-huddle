package thread

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/DnzzL/herdr-huddle/internal/github"
)

// LiveInstruction is an instruction that reached the agent over the live
// stream, to be recorded on the thread afterwards.
//
// ADR-005 splits steering into two halves: delivery is immediate and never
// waits for GitHub, and the thread — which eventually holds everything — gets
// the record after the fact.
type LiveInstruction struct {
	// Author is the collaborator's login, proved by the gate.
	Author string
	// Text is what they asked for, verbatim.
	Text string
	// At is when it was delivered.
	At time.Time
}

// Body renders the record comment.
//
// Three properties matter, in this order:
//
//  1. It opens with the Marker, so the poller recognises it as this tool's own
//     output and never delivers it a second time. This comment *describes* an
//     instruction the agent already received; re-delivering it would run the
//     colleague's text twice.
//  2. It never starts with the instruction prefix, for the same reason from
//     the other side.
//  3. It attributes the instruction and says it arrived live — the record has
//     to be honest about how a turn came to exist, or traceability is lost.
func (l LiveInstruction) Body() string {
	at := l.At.UTC().Format("2006-01-02 15:04 UTC")
	return Marker + "\n" +
		"**@" + l.Author + "** · " + at + " · sent live to the agent\n\n" +
		l.Text + "\n"
}

// InstructionLedger posts those records to the thread.
//
// GitHub has no reply anchoring for issue comments — the REST API's create
// endpoint has no in_reply_to, and issue comments are flat — so the record
// attributes the instruction instead of threading it under the turn it refers
// to.
type InstructionLedger struct {
	Client *github.Client
	Owner  string
	Repo   string
	Number int
}

// Post writes one record. A malformed one is refused rather than posted: a
// comment without the Marker could be read back as an instruction to deliver,
// which is the exact failure this whole type exists to avoid.
func (l *InstructionLedger) Post(ctx context.Context, in LiveInstruction) error {
	if l == nil || l.Client == nil {
		return errors.New("thread: the ledger has no client")
	}
	if strings.TrimSpace(in.Author) == "" || strings.TrimSpace(in.Text) == "" {
		return errors.New("thread: refusing to record an instruction with no author or no text")
	}
	if !strings.HasPrefix(in.Body(), Marker) {
		return errors.New("thread: refusing to record an instruction without the marker")
	}
	_, err := l.Client.CreateComment(ctx, l.Owner, l.Repo, l.Number, in.Body())
	return err
}

package live

import (
	"context"
	"errors"

	"github.com/DnzzL/herdr-huddle/internal/herdr"
)

// Instructor delivers an instruction to the agent in a pane.
//
// It is the inward half of ADR-005: the gate has already proved who is
// speaking, and this is what their words become. An error is classified
// rather than passed through raw, because "the agent is waiting for its
// operator" and "herdr is not running" call for different things to be told
// the person who typed it.
type Instructor interface {
	Deliver(ctx context.Context, pane, prompt string) error
}

// HerdrInstructor delivers through the herdr CLI — the same door the poller
// uses, so a live instruction and a commented one are indistinguishable to the
// agent.
type HerdrInstructor struct{}

func (HerdrInstructor) Deliver(ctx context.Context, pane, prompt string) error {
	return (&herdr.Client{}).Prompt(ctx, pane, prompt)
}

// Delivery statuses reported back to the joiner.
const (
	// StatusSent means herdr accepted the prompt: delivered or queued behind
	// the agent's current work, which herdr decides.
	StatusSent = "sent"
	// StatusHeld means the agent is blocked waiting for its operator, so the
	// instruction was rejected before it reached input.
	StatusHeld = "held"
	// StatusFailed means nothing reached the agent.
	StatusFailed = "failed"
)

// deliveryFailure turns a delivery error into what the joiner should hear.
func deliveryFailure(err error) (status, reason string) {
	var herr *herdr.Error
	if errors.As(err, &herr) && herr.Code == "agent_blocked" {
		return StatusHeld, "the agent is waiting on its operator"
	}
	return StatusFailed, oneLine([]byte(err.Error()))
}

//go:build liveagent

// This file is the one test that is not allowed to use a fake.
//
// ADR-005 recorded, for weeks, that "live injection has never met a real
// agent": the delivery path ran to the `herdr agent prompt` call and everything
// past that was a stub speaking herdr's error codes. That is the single most
// load-bearing unverified claim in the repository — steering is half the
// product — so it gets a test that talks to a real pane, and a build tag so it
// never runs by accident against somebody's working agent.
//
//	go test -tags liveagent -run TestLiveInjection ./internal/live/ \
//	  -args   # HUDDLE_LIVE_PANE=w33:p1
package live

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/DnzzL/herdr-huddle/internal/herdr"
	"github.com/DnzzL/herdr-huddle/internal/thread"
)

// TestLiveInjectionReachesARealAgent delivers one instruction the way a joiner
// does, and reads the pane back to prove it arrived.
func TestLiveInjectionReachesARealAgent(t *testing.T) {
	pane := os.Getenv("HUDDLE_LIVE_PANE")
	if pane == "" {
		t.Skip("set HUDDLE_LIVE_PANE to a pane you do not mind prompting")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	client := &herdr.Client{}
	before, err := client.Agent(ctx, pane)
	if err != nil {
		t.Fatalf("no such pane %s: %v", pane, err)
	}
	t.Logf("pane %s: %s, status %q, cwd %s", pane, before.Label(), before.Status, before.CWD)
	if before.Busy() {
		t.Skipf("agent is %s; a real joiner would be held back too", before.Status)
	}

	// The marker is what proves the agent read *this* message rather than
	// something already on screen.
	marker := "HUDDLE-PROBE-" + time.Now().UTC().Format("150405")
	instruction := thread.Instruction{
		Author: "huddle-probe",
		Text:   "Reply with exactly " + marker + " and nothing else. Make no changes to any file.",
		URL:    "https://github.com/DnzzL/molkky/pull/13",
	}

	// Exactly the call `Server.speak` makes — not a shortcut around it.
	if err := (HerdrInstructor{}).Deliver(ctx, pane, instruction.Prompt()); err != nil {
		t.Fatalf("delivering to a real agent failed: %v", err)
	}
	t.Log("delivered; waiting for the agent to show it")

	// The prompt text itself must appear in the pane: that is the proof the
	// wrapper survived the trip, quoting included.
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		out, err := client.Read(ctx, pane, 200)
		if err != nil {
			t.Fatalf("reading the pane back: %v", err)
		}
		if strings.Contains(out, marker) {
			t.Logf("the agent has it: %q is on the pane", marker)
			if !strings.Contains(out, "huddle-probe") {
				t.Errorf("the pane does not show the author; attribution was lost in the wrapper")
			}
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("%q never appeared on pane %s within 45s", marker, pane)
}

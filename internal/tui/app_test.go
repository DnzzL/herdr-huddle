package tui

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/DnzzL/herdr-huddle/internal/live"
)

// devNull stands in for a terminal in the tests that do not need one to be
// real — the backoff decides what to do with a failure long before anything is
// drawn.
func devNull(t *testing.T) *os.File {
	t.Helper()
	f, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func testApp(t *testing.T) *App {
	f := devNull(t)
	a := &App{In: f, Out: f}
	a.view.Cols, a.view.Rows = 80, 24
	return a
}

// The wait grows so a tunnel that is properly gone is not hammered, and it is
// capped so a huddle that comes back is rejoined within seconds rather than
// minutes.
func TestReconnectDelayGrowsAndIsCapped(t *testing.T) {
	first := reconnectDelay(0)
	if first != 500*time.Millisecond {
		t.Errorf("first delay = %s, want a blip to be retried almost at once", first)
	}
	last := time.Duration(0)
	for attempt := 0; attempt < 40; attempt++ {
		got := reconnectDelay(attempt)
		if got <= 0 {
			t.Fatalf("delay(%d) = %s, want a positive wait even after an overflow", attempt, got)
		}
		if got > 15*time.Second {
			t.Fatalf("delay(%d) = %s, want it capped", attempt, got)
		}
		if got < last {
			t.Fatalf("delay(%d) = %s went backwards from %s", attempt, got, last)
		}
		last = got
	}
	if last != 15*time.Second {
		t.Errorf("the delay never reached its ceiling, got %s", last)
	}
}

// Two failures must never be retried: the door was closed to this person, and
// the person left. Retrying either is a loop that cannot succeed.
func TestBackOffGivesUpOnWhatRetryingCannotFix(t *testing.T) {
	app := testApp(t)
	keys := make(chan rune)

	if got := app.backOff(context.Background(), keys, live.ErrRefused, new(int)); !errors.Is(got, live.ErrRefused) {
		t.Errorf("a refusal returned %v, want it to stop the loop", got)
	}
	if got := app.backOff(context.Background(), keys, errLeft, new(int)); !errors.Is(got, errLeft) {
		t.Errorf("leaving returned %v, want it to stop the loop", got)
	}

	// A cancelled context is leaving by another name.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := app.backOff(ctx, keys, errors.New("the tunnel died"), new(int)); !errors.Is(got, errLeft) {
		t.Errorf("a cancelled context returned %v, want it to stop the loop", got)
	}
}

// An ordinary failure is retried, and the attempt count is what grows the
// wait.
func TestBackOffRetriesAnOrdinaryFailure(t *testing.T) {
	app := testApp(t)
	keys := make(chan rune)
	attempt := 0

	done := make(chan error, 1)
	go func() { done <- app.backOff(context.Background(), keys, errors.New("the tunnel died"), &attempt) }()

	select {
	case got := <-done:
		if got != nil {
			t.Fatalf("backOff = %v, want nil so the loop dials again", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("backOff never returned")
	}
	if attempt != 1 {
		t.Errorf("attempt = %d, want the next wait to be longer", attempt)
	}
}

// Somebody watching "retrying in 15s" must be able to give up without waiting
// for it.
func TestBackOffAnswersCtrlCWhileWaiting(t *testing.T) {
	app := testApp(t)
	keys := make(chan rune, 1)
	attempt := 5 // long enough that the timer will not be what returns

	done := make(chan error, 1)
	go func() { done <- app.backOff(context.Background(), keys, errors.New("the tunnel died"), &attempt) }()
	keys <- 3 // Ctrl-C

	select {
	case got := <-done:
		if !errors.Is(got, errLeft) {
			t.Errorf("backOff = %v, want Ctrl-C to leave", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Ctrl-C was ignored while waiting to reconnect")
	}
}

// Ctrl-C is an ordinary exit, not a failure to report.
func TestLeavingIsNotAnError(t *testing.T) {
	if err := unlessLeft(errLeft); err != nil {
		t.Errorf("unlessLeft(errLeft) = %v, want nil", err)
	}
	refused := live.ErrRefused
	if err := unlessLeft(refused); !errors.Is(err, live.ErrRefused) {
		t.Errorf("unlessLeft kept %v, want the refusal reported", err)
	}
}

// The first retry is deliberately brief, and rounding it to seconds turns
// "500ms" into "0s" — which reads as broken rather than as fast.
func TestHumanWait(t *testing.T) {
	cases := map[time.Duration]string{
		500 * time.Millisecond: "a moment",
		999 * time.Millisecond: "a moment",
		time.Second:            "1s",
		2 * time.Second:        "2s",
		15 * time.Second:       "15s",
	}
	for in, want := range cases {
		if got := humanWait(in); got != want {
			t.Errorf("humanWait(%s) = %q, want %q", in, got, want)
		}
	}
	// No delay the loop can produce may word itself as zero.
	for attempt := 0; attempt < 10; attempt++ {
		if got := humanWait(reconnectDelay(attempt)); strings.HasPrefix(got, "0") {
			t.Errorf("attempt %d words its wait as %q", attempt, got)
		}
	}
}

// A moderated instruction passes through a state that is neither success nor
// failure, and every state needs a colour or success and failure look alike.
func TestEveryOutcomeIsColoured(t *testing.T) {
	for _, status := range []string{
		live.StatusSent, live.StatusQueued, live.StatusRefused,
		live.StatusHeld, live.StatusFailed, "something newer",
	} {
		if outcomeColour(status) == "" {
			t.Errorf("outcomeColour(%q) is unpainted", status)
		}
	}
	// Needing somebody to act and having failed must not look the same.
	if outcomeColour(live.StatusSent) == outcomeColour(live.StatusFailed) {
		t.Error("delivered and failed are the same colour")
	}
	if outcomeColour(live.StatusQueued) == outcomeColour(live.StatusRefused) {
		t.Error("waiting and refused are the same colour")
	}
}

// sayClient records where a typed line was routed.
type sayClient struct {
	said   []string
	chats  []string
	typing []bool
	toRoom []bool
	sizes  [][2]int
}

func (c *sayClient) Run(live.Events) error { return nil }
func (c *sayClient) Say(t string) error    { c.said = append(c.said, t); return nil }
func (c *sayClient) Chat(t string) error   { c.chats = append(c.chats, t); return nil }
func (c *sayClient) Resize(cols, rows int) error {
	c.sizes = append(c.sizes, [2]int{cols, rows})
	return nil
}
func (c *sayClient) Typing(on, toRoom bool) error {
	c.typing, c.toRoom = append(c.typing, on), append(c.toRoom, toRoom)
	return nil
}
func (c *sayClient) Close() error { return nil }

// The whole safety claim of the two-channel design: a line typed in room mode
// must reach the room and never the agent, and the other way round. Getting
// this wrong is silent and unrecallable — the agent acts on something meant
// for a person.
func TestCtrlTRoutesTheLineAndNeverBoth(t *testing.T) {
	app := testApp(t)
	client := &sayClient{}
	app.attach(client)

	type step struct {
		key  rune
		want string
	}
	// Type, send, toggle, type, send, toggle back, type, send.
	for _, line := range []struct {
		text   string
		toRoom bool
	}{
		{"restart the worker", false},
		{"wait, do not touch the migration", true},
		{"ok carry on", false},
	} {
		if line.toRoom != (app.view.Mode == toRoom) {
			app.key(20) // Ctrl-T
		}
		for _, r := range line.text {
			app.key(r)
		}
		app.key('\r')
	}

	wantSaid := []string{"restart the worker", "ok carry on"}
	wantChat := []string{"wait, do not touch the migration"}
	if len(client.said) != len(wantSaid) {
		t.Fatalf("agent received %v, want %v", client.said, wantSaid)
	}
	for i := range wantSaid {
		if client.said[i] != wantSaid[i] {
			t.Errorf("agent received %q, want %q", client.said[i], wantSaid[i])
		}
	}
	if len(client.chats) != 1 || client.chats[0] != wantChat[0] {
		t.Fatalf("room received %v, want %v", client.chats, wantChat)
	}
	// The one that would be a disaster: the room message must not also have
	// been sent to the agent under any spelling.
	for _, sent := range client.said {
		if strings.Contains(sent, "migration") {
			t.Fatalf("a room message reached the agent: %q", sent)
		}
	}
	// And the input line is cleared after either kind of send.
	if app.view.Input != "" {
		t.Errorf("input = %q after sending, want it cleared", app.view.Input)
	}
}

// Somebody composing a message to the room is somebody about to speak, and the
// room is owed that as much as it is owed an instruction coming. Without it a
// host typing in their own pane is invisible to every joiner.
func TestRoomMessagesClaimTypingToo(t *testing.T) {
	app := testApp(t)
	client := &sayClient{}
	app.attach(client)

	app.key(20) // into room mode
	for _, r := range "hang on" {
		app.key(r)
	}
	claimed := false
	for _, on := range client.typing {
		claimed = claimed || on
	}
	if !claimed {
		t.Fatal("typing a message to the room told nobody")
	}
	app.key('\r')
	if last := client.typing[len(client.typing)-1]; last {
		t.Error("sending the message left the typing claim standing")
	}
}

// The footer is a window onto the room, and Ctrl-L is how a joiner makes it
// bigger. The pane gives up the rows, and the server is asked for a stream at
// the new size, which arrives as a complete repaint — the mechanism a resize
// already uses.
func TestCtrlLGrowsTheFooterAndShrinksThePane(t *testing.T) {
	app := testApp(t)
	app.view.Rows = 30
	app.view.Extra = defaultExtra
	client := &sayClient{}
	app.attach(client)

	small := app.view.PaneRows()
	app.key(12) // Ctrl-L
	big := app.view.PaneRows()
	if big >= small {
		t.Fatalf("expanding left the pane at %d rows (was %d)", big, small)
	}
	if got := client.sizes[len(client.sizes)-1]; got[1] != big {
		t.Errorf("server asked for %d rows, want the pane's %d", got[1], big)
	}
	app.key(12)
	if got := app.view.PaneRows(); got != small {
		t.Errorf("collapsing left the pane at %d rows, want %d", got, small)
	}
	if got := client.sizes[len(client.sizes)-1]; got[1] != small {
		t.Errorf("server asked for %d rows after collapsing, want %d", got[1], small)
	}
}

// Everything that happens to a joiner is kept, not only the last of it.
func TestAJoinerKeepsWhatHappened(t *testing.T) {
	app := testApp(t)
	app.setRoom(live.Frame{You: "ana", Members: []string{"ana"}})
	app.heard(live.Frame{Author: "bo", Text: "first"})
	app.heard(live.Frame{Author: "bo", Text: "second"})
	app.said(live.Frame{Author: "bo", Text: "fix it", Status: live.StatusSent})

	got := logText(app)
	for _, want := range []string{"first", "second", "fix it"} {
		if !strings.Contains(got, want) {
			t.Errorf("history lost %q:\n%s", want, got)
		}
	}
}

// The claim says who the sentence is for, and changing your mind mid-sentence
// is a new claim, not a renewal of the old one.
func TestTypingClaimNamesItsDestination(t *testing.T) {
	app := testApp(t)
	client := &sayClient{}
	app.attach(client)

	app.key('h') // agent mode
	app.key(20)  // Ctrl-T: same sentence, now for the room
	app.key('i')

	if len(client.toRoom) != 2 || client.toRoom[0] || !client.toRoom[1] {
		t.Errorf("claims = %v, want one for the agent then one for the room", client.toRoom)
	}
}

// "?" opens the help only on an empty line: a question mark in a sentence is a
// question mark.
func TestQuestionMarkIsHelpOnlyOnAnEmptyLine(t *testing.T) {
	app := testApp(t)
	app.key('?')
	if !app.view.Help {
		t.Fatal("? on an empty line did not open the help")
	}
	app.key('?')
	if app.view.Help {
		t.Fatal("? did not close the help")
	}
	for _, r := range "why" {
		app.key(r)
	}
	app.key('?')
	if app.view.Help || app.view.Input != "why?" {
		t.Errorf("a ? mid-sentence: help=%v input=%q, want it typed", app.view.Help, app.view.Input)
	}
}

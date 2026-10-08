package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/DnzzL/herdr-huddle/internal/live"
)

func hostApp(t *testing.T) *App {
	a := testApp(t)
	a.Host = true
	a.view.Mode = toRoom
	return a
}

func logText(a *App) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var b strings.Builder
	for _, row := range a.view.Log {
		b.WriteString(row.plain() + "\n")
	}
	return b.String()
}

// The panel is what the pane is to a joiner: it fills everything above the
// chrome, newest at the bottom, and never grows into the chrome's rows.
func TestHostPanelKeepsTheLatestLinesAboveTheChrome(t *testing.T) {
	a := hostApp(t)
	for i := range 100 {
		a.Note(fmt.Sprintf("line %d", i))
	}
	a.Pin("herdr-huddle join https://x.trycloudflare.com")

	panel := a.view.Panel()
	if !strings.Contains(panel, "line 99") {
		t.Error("the newest line is not on the panel")
	}
	if strings.Contains(panel, "line 10 ") || strings.Contains(panel, "line 0\x1b") {
		t.Error("an old line the panel has no room for was drawn")
	}
	if !strings.Contains(panel, "herdr-huddle join https://x.trycloudflare.com") {
		t.Error("the join line is not pinned on the panel")
	}
	if got, want := strings.Count(panel, clrLine), PaneRows(24); got != want {
		t.Errorf("panel painted %d rows, want exactly the %d above the chrome", got, want)
	}
}

// What was said, who came and what became of an instruction all end up in the
// panel, so the host reads the huddle instead of watching a status row.
func TestHostPanelRecordsTheRoom(t *testing.T) {
	a := hostApp(t)
	a.setRoom(live.Frame{You: "thomas", Host: "thomas", Members: []string{"thomas"}})
	a.setRoom(live.Frame{You: "thomas", Host: "thomas", Members: []string{"ana", "thomas"}})
	a.heard(live.Frame{Author: "ana", Text: "wait, not the migration"})
	a.said(live.Frame{Author: "ana", Text: "fix the gate", Status: live.StatusSent})

	got := logText(a)
	for _, want := range []string{"@ana joined", "@ana", "wait, not the migration", "fix the gate"} {
		if !strings.Contains(got, want) {
			t.Errorf("panel is missing %q:\n%s", want, got)
		}
	}
}

// The host steers from the pane. This line is for the room, always, and has
// no second destination to get wrong.
func TestHostLineAlwaysGoesToTheRoom(t *testing.T) {
	a := hostApp(t)
	a.key(20) // Ctrl-T
	if a.view.Mode != toRoom {
		t.Error("Ctrl-T moved the host's line to the agent")
	}
}

// A question is answered by one key, but only when the input line is empty:
// a person halfway through "yes, wait" must not admit a stranger by typing.
func TestAskIsAnsweredByOneKeyOnlyOnAnEmptyLine(t *testing.T) {
	a := hostApp(t)
	got := make(chan bool, 1)
	go func() {
		yes, err := a.Ask(t.Context(), "@carl is at the door.\n  let them in?")
		if err != nil {
			t.Error(err)
		}
		got <- yes
	}()
	waitUntil(t, "the question to be up", func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.pending != nil })

	a.key('x') // composing a message: y/n must now type, not answer
	a.key('y')
	select {
	case <-got:
		t.Fatal("a y typed into a message answered the question")
	case <-time.After(150 * time.Millisecond):
	}
	if a.view.Input != "xy" {
		t.Errorf("input = %q, want the y typed into the message", a.view.Input)
	}

	a.key(21) // Ctrl-U clears the line
	a.key('y')
	select {
	case yes := <-got:
		if !yes {
			t.Error("y answered no")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("y on an empty line did not answer")
	}
	if !strings.Contains(logText(a), "@carl is at the door") {
		t.Errorf("the question is not on the panel:\n%s", logText(a))
	}
}

// Anything but y is no, and a question about somebody who left is dropped
// rather than left standing in front of the host.
func TestAskDefaultsToNoAndCanBeAbandoned(t *testing.T) {
	a := hostApp(t)

	got := make(chan bool, 1)
	go func() { yes, _ := a.Ask(t.Context(), "let @a in?"); got <- yes }()
	waitUntil(t, "the question", func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.pending != nil })
	a.key('n')
	if yes := <-got; yes {
		t.Error("n answered yes")
	}

	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	go func() { _, err := a.Ask(ctx, "let @b in?"); errc <- err }()
	waitUntil(t, "the second question", func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.pending != nil })
	cancel()
	if err := <-errc; err == nil {
		t.Error("an abandoned question returned no error")
	}
	if a.pending != nil {
		t.Error("an abandoned question is still on screen")
	}
}

// Two people knocking are asked about one at a time, in the order they came.
func TestAskIsOneAtATimeInArrivalOrder(t *testing.T) {
	a := hostApp(t)
	order := make(chan string, 2)
	ask := func(who string) {
		yes, _ := a.Ask(t.Context(), "let "+who+" in?")
		order <- fmt.Sprintf("%s=%v", who, yes)
	}
	go ask("@first")
	waitUntil(t, "the first question", func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.pending != nil })
	go ask("@second")
	time.Sleep(100 * time.Millisecond)

	a.key('y')
	if got := <-order; got != "@first=true" {
		t.Fatalf("first answer = %s", got)
	}
	waitUntil(t, "the second question", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.pending != nil && strings.Contains(logText2(a), "@second")
	})
	a.key('n')
	if got := <-order; got != "@second=false" {
		t.Fatalf("second answer = %s", got)
	}
}

func logText2(a *App) string {
	var b strings.Builder
	for _, row := range a.view.Log {
		b.WriteString(row.plain() + "\n")
	}
	return b.String()
}

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

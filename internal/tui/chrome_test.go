package tui

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/DnzzL/herdr-huddle/internal/herdr"
)

// sgr matches the colour and clear sequences, which are decoration. The cursor
// moves are not decoration — where each line lands is the whole arrangement —
// so they are matched separately and kept.
var sgr = regexp.MustCompile(`\x1b\[[0-9;]*[mK]|\x1b[78]`)

var moveTo = regexp.MustCompile(`\x1b\[(\d+);(\d+)H`)

// rows parses a chrome render back into "the line drawn at row N", which is
// what a person actually sees.
func rows(t *testing.T, chrome string) map[int]string {
	t.Helper()
	out := map[int]string{}
	moves := moveTo.FindAllStringSubmatchIndex(chrome, -1)
	for i, m := range moves {
		var row, col int
		fmt.Sscanf(chrome[m[2]:m[3]], "%d", &row)
		fmt.Sscanf(chrome[m[4]:m[5]], "%d", &col)
		end := len(chrome)
		if i+1 < len(moves) {
			end = moves[i+1][0]
		}
		text := sgr.ReplaceAllString(chrome[m[1]:end], "")
		// The last move is the cursor being parked on the input line and
		// carries no text of its own.
		if text == "" && col > 1 {
			continue
		}
		out[row] = text
	}
	return out
}

func TestPaneRowsLeavesTheChromeItsRows(t *testing.T) {
	if got := PaneRows(30); got != 30-ChromeRows {
		t.Errorf("PaneRows(30) = %d, want %d", got, 30-ChromeRows)
	}
	// A window too small for both still has to paint something.
	if got := PaneRows(4); got < MinPaneRows {
		t.Errorf("PaneRows(4) = %d, want at least %d", got, MinPaneRows)
	}
}

// The room lives on the bottom four rows and nowhere else: anything drawn
// higher would be inside the viewport the next frame repaints.
func TestChromeOccupiesOnlyTheBottomRows(t *testing.T) {
	view := View{
		Cols: 60, Rows: 24,
		Room:  Room{You: "ana", Members: []string{"ana", "bo"}, Agent: herdr.StatusWorking, Thread: "https://github.com/acme/demo/pull/13"},
		Event: "@bo joined",
		Input: "make it faster",
	}
	drawn := rows(t, view.Chrome())
	for row := range drawn {
		if row <= view.Rows-ChromeRows {
			t.Errorf("drew on row %d, which belongs to the pane", row)
		}
		if row > view.Rows {
			t.Errorf("drew on row %d, past the bottom of a %d-row terminal", row, view.Rows)
		}
	}
	if len(drawn) != ChromeRows {
		t.Errorf("drew %d rows, want exactly %d", len(drawn), ChromeRows)
	}
}

// Each of the four rows earns its place: the seam, the room, what just
// happened, and what you are typing.
func TestChromeSaysWhoIsHereAndWhatTheAgentIsDoing(t *testing.T) {
	view := View{
		Cols: 70, Rows: 24,
		Room:  Room{You: "ana", Members: []string{"ana", "bo"}, Agent: herdr.StatusBlocked, Thread: "https://github.com/acme/demo/pull/13"},
		Event: "@bo: delivered to the agent",
		Input: "make it faster",
	}
	drawn := rows(t, view.Chrome())
	all := strings.Join([]string{drawn[21], drawn[22], drawn[23], drawn[24]}, "\n")

	for _, want := range []string{
		"acme/demo#13",            // the record is one glance away
		"waiting on the operator", // blocked, in words a person reads
		"@bo",                     // who else is here
		"@bo: delivered to the agent",
		"make it faster", // what you have typed so far
	} {
		if !strings.Contains(all, want) {
			t.Errorf("chrome does not mention %q:\n%s", want, all)
		}
	}
	if strings.Contains(drawn[22], "@ana") {
		t.Errorf("the roster names you as a handle rather than as you: %q", drawn[22])
	}
}

func TestRosterWording(t *testing.T) {
	cases := []struct {
		name    string
		you     string
		members []string
		want    string
	}{
		{"alone", "ana", []string{"ana"}, "you are the only one here"},
		{"one other", "ana", []string{"ana", "bo"}, "you and @bo"},
		{"several", "ana", []string{"ana", "bo", "cy"}, "you and @bo, @cy"},
		{"before the roster arrives", "", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := View{Room: Room{You: c.you, Members: c.members}}.roster()
			if got != c.want {
				t.Errorf("roster = %q, want %q", got, c.want)
			}
		})
	}
}

// A narrow terminal must not wrap the chrome: a wrapped line pushes the pane
// up and the arrangement never recovers.
func TestChromeFitsANarrowTerminal(t *testing.T) {
	view := View{
		Cols: 24, Rows: 12,
		Room:  Room{You: "ana", Members: []string{"ana", "bonaventure", "cyrille"}, Agent: herdr.StatusBlocked, Thread: "https://github.com/acme/demo/pull/13"},
		Event: "@bonaventure: not delivered: the agent is waiting on its operator",
		Input: strings.Repeat("x", 200),
	}
	for row, text := range rows(t, view.Chrome()) {
		if len([]rune(text)) > view.Cols {
			t.Errorf("row %d is %d columns wide in a %d-column terminal: %q",
				row, len([]rune(text)), view.Cols, text)
		}
	}
}

func TestShortThread(t *testing.T) {
	cases := map[string]string{
		"https://github.com/acme/demo/pull/13": "acme/demo#13",
		"https://github.com/acme/demo/pull/1":  "acme/demo#1",
		"":                                     "",
		// Anything that is not a pull request URL is left alone: a wrong
		// abbreviation is worse than a long one.
		"https://example.com/somewhere": "https://example.com/somewhere",
		"https://github.com/acme/demo":  "https://github.com/acme/demo",
	}
	for in, want := range cases {
		if got := shortThread(in); got != want {
			t.Errorf("shortThread(%q) = %q, want %q", in, got, want)
		}
	}
}

// A line longer than the window scrolls with the cursor. Wrapping it would
// push the pane up by a row that never comes back.
func TestALongInputKeepsItsEndVisible(t *testing.T) {
	view := View{Cols: 20, Rows: 10, Input: "abcdefghijklmnopqrstuvwxyz"}
	line := sgr.ReplaceAllString(view.prompt(view.Cols), "")
	if !strings.HasSuffix(line, "z") {
		t.Errorf("prompt = %q, want the end of what was typed", line)
	}
	if len([]rune(line)) > view.Cols {
		t.Errorf("prompt is %d columns wide, want at most %d", len([]rune(line)), view.Cols)
	}
}

func TestDropWord(t *testing.T) {
	cases := map[string]string{
		"fix the parser":   "fix the ",
		"fix the parser  ": "fix the ",
		"one":              "",
		"":                 "",
	}
	for in, want := range cases {
		if got := string(dropWord([]rune(in))); got != want {
			t.Errorf("dropWord(%q) = %q, want %q", in, got, want)
		}
	}
}

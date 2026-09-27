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
		Event: line{}.add(dim, "@bo joined"),
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
		Event: line{}.add(dim, "@bo → rename the package · delivered"),
		Input: "make it faster",
	}
	drawn := rows(t, view.Chrome())
	all := strings.Join([]string{drawn[21], drawn[22], drawn[23], drawn[24]}, "\n")

	for _, want := range []string{
		"acme/demo#13",            // the record is one glance away
		"waiting on the operator", // blocked, in words a person reads
		"@bo",                     // who else is here
		"@bo → rename the package · delivered",
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
		{"several", "ana", []string{"ana", "bo", "cy"}, "you, @bo and @cy"},
		{"before the roster arrives", "", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := View{Room: Room{You: c.you, Members: c.members}}.roster().plain()
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
		Event: line{}.add(dim, "@bonaventure → ship it · not delivered: the agent is waiting on its operator"),
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
	shown := view.prompt(view.Cols).plain()
	if !strings.HasSuffix(shown, "z") {
		t.Errorf("prompt = %q, want the end of what was typed", shown)
	}
	if len([]rune(shown)) > view.Cols {
		t.Errorf("prompt is %d columns wide, want at most %d", len([]rune(shown)), view.Cols)
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

// The operator is in the room without being connected to it — they are sitting
// at the pane. A joiner told "you are the only one here" while the operator
// watches over their shoulder is being told something false.
func TestRosterNamesTheHost(t *testing.T) {
	cases := []struct {
		name      string
		you, host string
		members   []string
		want      string
	}{
		{"alone with the operator watching", "ana", "thomas", []string{"ana"}, "@thomas (host) and you"},
		{"a full room", "ana", "thomas", []string{"ana", "bo"}, "@thomas (host), you and @bo"},
		{"the operator joined their own huddle", "thomas", "thomas", []string{"thomas"}, "you (host)"},
		{"no host known", "ana", "", []string{"ana"}, "you are the only one here"},
		{"the host is also a joiner, listed once", "ana", "thomas", []string{"ana", "thomas"}, "@thomas (host) and you"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := View{Room: Room{You: c.you, Host: c.host, Members: c.members}}.roster().plain()
			if got != c.want {
				t.Errorf("roster = %q, want %q", got, c.want)
			}
		})
	}
}

// Typing is a fact about a person, so it is drawn on the person. Putting it on
// the event line instead would mean somebody else composing hides the
// confirmation that your own instruction was delivered — which is the one
// thing you are waiting to read.
func TestTypingIsShownOnThePerson(t *testing.T) {
	view := View{
		Cols: 70, Rows: 24,
		Room:  Room{You: "ana", Host: "thomas", Members: []string{"ana", "bo"}},
		Event: line{}.add(dim, "@bo → rename the package · delivered"),
	}
	if got := view.roster().plain(); got != "@thomas (host), you and @bo" {
		t.Errorf("roster = %q", got)
	}

	view.Room.Typing = []string{"bo"}
	if got := view.roster().plain(); got != "@thomas (host), you and @bo (typing…)" {
		t.Errorf("roster = %q, want @bo marked as typing", got)
	}
	// And the event line is untouched: what was delivered stays readable.
	if got := view.event().plain(); got != "@bo → rename the package · delivered" {
		t.Errorf("event = %q, want somebody typing not to hide it", got)
	}

	// The operator typing keeps being the operator.
	view.Room.Typing = []string{"thomas"}
	if got := view.roster().plain(); got != "@thomas (host, typing…), you and @bo" {
		t.Errorf("roster = %q, want the host marked as both", got)
	}
}

// A person is one colour everywhere — roster, typing line, and the record of
// what they asked for — or the room is read word by word instead of scanned.
func TestPersonColourIsStable(t *testing.T) {
	first := personColour("ana")
	if first != personColour("ana") {
		t.Error("the same login must always get the same colour")
	}
	// GitHub's casing is not a different person.
	if personColour("Ana") != first {
		t.Errorf("case changed the colour: %q vs %q", personColour("Ana"), first)
	}
	for _, colour := range []string{red, yellow} {
		for _, login := range []string{"ana", "bo", "cy", "thomas", "dee", "eve", "fay", "gil"} {
			if personColour(login) == colour {
				t.Errorf("@%s is coloured like an agent state, which reads as an alarm", login)
			}
		}
	}
}

// Colour must never survive a truncation: a styled string cut mid-escape
// leaves the terminal painting in a colour nobody asked for, for good.
func TestTruncationNeverCutsAnEscape(t *testing.T) {
	row := line{}.add(red, "aaaaaaaaaa").add(green, "bbbbbbbbbb").add(blue, "cccccccccc")
	for n := 1; n <= 35; n++ {
		out := row.render(n)
		if plain := sgr.ReplaceAllString(out, ""); len([]rune(plain)) > n {
			t.Errorf("render(%d) is %d columns wide: %q", n, len([]rune(plain)), plain)
		}
		if strings.Count(out, "\x1b[") != strings.Count(out, reset)*2 {
			// Every style opened is a style closed: one opener and one reset
			// per painted span, and \x1b[ counts both.
			t.Errorf("render(%d) leaves a style open: %q", n, out)
		}
	}
}

// The expensive mistake in a room where one input line feeds two destinations
// is a silent one: telling the agent to do something when you meant to say
// "hang on" to a colleague. So the destination is a word on the line you are
// typing into, never a colour alone and never absent.
func TestThePromptSaysWhereTheLineIsGoing(t *testing.T) {
	view := View{Cols: 60, Rows: 24, Input: "ship it"}

	agent := view.prompt(view.Cols).plain()
	if !strings.HasPrefix(agent, "agent › ") {
		t.Errorf("prompt = %q, want it to say the line goes to the agent", agent)
	}

	view.Mode = toRoom
	room := view.prompt(view.Cols).plain()
	if !strings.HasPrefix(room, "room › ") {
		t.Errorf("prompt = %q, want it to say the line goes to the room", room)
	}
	if !strings.HasSuffix(room, "ship it") {
		t.Errorf("prompt = %q, want what was typed", room)
	}
}

// The cursor has to sit after the text in either mode, or the terminal looks
// like it has lost it.
func TestPromptWidthFollowsTheMode(t *testing.T) {
	for _, c := range []struct {
		mode  mode
		input string
	}{
		{toAgent, ""},
		{toAgent, "ship it"},
		{toRoom, ""},
		{toRoom, "hang on"},
	} {
		view := View{Cols: 60, Rows: 24, Input: c.input, Mode: c.mode}
		want := len([]rune(view.prompt(view.Cols).plain())) + 1
		if got := promptWidth(c.input, c.mode, view.Cols); got != want {
			t.Errorf("mode %v input %q: cursor at column %d, want %d (just past the text)",
				c.mode, c.input, got, want)
		}
	}
}

// Both modes must survive a narrow window without wrapping the chrome into the
// pane.
func TestThePromptFitsInEitherMode(t *testing.T) {
	for _, m := range []mode{toAgent, toRoom} {
		view := View{Cols: 24, Rows: 12, Mode: m, Input: strings.Repeat("x", 200)}
		if got := len([]rune(view.prompt(view.Cols).plain())); got > view.Cols {
			t.Errorf("mode %v: prompt is %d columns wide, want at most %d", m, got, view.Cols)
		}
	}
}

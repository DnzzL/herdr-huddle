// Package tui draws a live share as a room rather than as a pipe.
//
// The pane's frames are a full-screen ANSI paint at a size the server chose,
// so chrome cannot simply be printed alongside them: anything written between
// frames lands inside the viewport the next frame repaints. The arrangement
// here is the one ADR-007 settles on — the pane keeps the top of the terminal,
// the bottom rows are ours, and they are repainted after every frame batch so
// they survive a paint that clears the screen.
//
// Everything in this file is a pure function of the room's state. That is
// deliberate: the layout is the part worth testing, and it is testable only
// while it does not own a terminal.
package tui

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/DnzzL/herdr-huddle/internal/herdr"
)

// ChromeRows is how many rows at the bottom of the terminal belong to the
// huddle: a rule, the room, the last thing that happened, and the input line.
const ChromeRows = 4

// MinPaneRows is the smallest pane worth painting. Below it the chrome would
// be most of the window, so the pane gets what is left and the terminal is
// simply too small — which the pane will show by wrapping.
const MinPaneRows = 3

// PaneRows is how many rows the pane gets in a terminal of this height.
func PaneRows(rows int) int {
	pane := rows - ChromeRows
	if pane < MinPaneRows {
		return MinPaneRows
	}
	return pane
}

// Room is what the status line has to say: who is here, what the agent is
// doing, and where the record lives.
type Room struct {
	// You is the viewer's own login, so they can be named as "you" rather
	// than listed twice.
	You string
	// Members is everyone connected, including You.
	Members []string
	// Agent is herdr's own status vocabulary, or empty when unknown.
	Agent string
	// Thread is the pull request URL the share is bound to.
	Thread string
}

// View is the whole of the chrome's state.
type View struct {
	Room  Room
	Event string
	Input string
	// Cols and Rows are the terminal's, not the pane's.
	Cols, Rows int
}

// ANSI pieces. Written out rather than pulled from a library: four lines of
// chrome do not justify a dependency, and every sequence here is in the
// subset every terminal this runs on has had for decades.
const (
	reset   = "\x1b[0m"
	dim     = "\x1b[2m"
	bold    = "\x1b[1m"
	green   = "\x1b[32m"
	yellow  = "\x1b[33m"
	red     = "\x1b[31m"
	cyan    = "\x1b[36m"
	grey    = "\x1b[90m"
	clrLine = "\x1b[2K"
	// save and restore bracket a chrome repaint so the pane's own cursor
	// position survives it.
	saveCursor    = "\x1b7"
	restoreCursor = "\x1b8"
)

// Chrome renders the bottom rows and leaves the cursor on the input line,
// where the person typing expects to find it.
func (v View) Chrome() string {
	cols := v.Cols
	if cols < 20 {
		cols = 20
	}
	first := v.Rows - ChromeRows + 1
	if first < 1 {
		first = 1
	}

	var b strings.Builder
	b.WriteString(saveCursor)
	lines := []string{
		v.rule(cols),
		v.status(cols),
		v.event(cols),
		v.prompt(cols),
	}
	for i, line := range lines {
		fmt.Fprintf(&b, "\x1b[%d;1H%s%s", first+i, clrLine, line)
	}
	b.WriteString(restoreCursor)
	// The cursor is parked where the typing goes, after the restore, because
	// a visible cursor anywhere else reads as the terminal having lost it.
	fmt.Fprintf(&b, "\x1b[%d;%dH", v.Rows, promptWidth(v.Input, cols))
	return b.String()
}

// rule is the seam between the pane and the room, with the record's name on
// the right so the pull request is never more than a glance away (ADR-007).
func (v View) rule(cols int) string {
	left := "── huddle "
	right := ""
	if short := shortThread(v.Room.Thread); short != "" {
		right = " " + short + " ──"
	}
	fill := cols - width(left) - width(right)
	if fill < 0 {
		right = ""
		fill = cols - width(left)
	}
	if fill < 0 {
		fill = 0
	}
	return dim + left + strings.Repeat("─", fill) + right + reset
}

// status is the room in one line: the agent's state, then who is in it.
func (v View) status(cols int) string {
	label, colour := agentState(v.Room.Agent)
	plain := "● " + label
	styled := colour + "●" + reset + " " + bold + label + reset

	if who := v.roster(); who != "" {
		plain += "   " + who
		styled += "   " + dim + who + reset
	}
	if over := width(plain) - cols; over > 0 {
		// Falling back to the unstyled line is the honest way to truncate:
		// cutting a styled string mid-escape leaves the terminal in a colour
		// it was never told to leave.
		return fit(plain, cols)
	}
	return styled
}

// roster names the room the way a person would: you first, then the others.
func (v View) roster() string {
	var others []string
	you := false
	for _, member := range v.Room.Members {
		if strings.EqualFold(member, v.Room.You) {
			you = true
			continue
		}
		others = append(others, "@"+member)
	}
	switch {
	case len(others) == 0 && you:
		return "you are the only one here"
	case len(others) == 0:
		return ""
	case you:
		return "you and " + strings.Join(others, ", ")
	default:
		return strings.Join(others, ", ")
	}
}

func (v View) event(cols int) string {
	if v.Event == "" {
		return ""
	}
	return grey + fit(v.Event, cols) + reset
}

func (v View) prompt(cols int) string {
	marker := "› "
	room := cols - width(marker)
	if room < 1 {
		room = 1
	}
	text := v.Input
	// A line longer than the window scrolls with the cursor rather than
	// wrapping into the pane above it.
	if width(text) > room-1 {
		text = tail(text, room-1)
	}
	return cyan + marker + reset + text
}

// promptWidth is the column the cursor belongs in, 1-based.
func promptWidth(input string, cols int) int {
	room := cols - 2
	if room < 1 {
		room = 1
	}
	shown := width(input)
	if shown > room-1 {
		shown = room - 1
	}
	return 3 + shown
}

// agentState turns herdr's vocabulary into something a person reads at a
// glance, and a colour for the dot.
func agentState(status string) (string, string) {
	switch status {
	case herdr.StatusWorking:
		return "working", yellow
	case herdr.StatusBlocked:
		return "waiting on the operator", red
	case herdr.StatusIdle:
		return "idle", green
	case herdr.StatusDone:
		return "done", green
	case "":
		return "connecting", grey
	default:
		return status, grey
	}
}

// shortThread names the pull request the way people say it out loud.
// Anything that is not a GitHub pull request URL is left alone, because a
// wrong abbreviation is worse than a long one.
func shortThread(url string) string {
	rest, ok := strings.CutPrefix(url, "https://github.com/")
	if !ok {
		return url
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 4 || parts[2] != "pull" {
		return url
	}
	return parts[0] + "/" + parts[1] + "#" + parts[3]
}

// width counts what the terminal will show, which is runes and not bytes.
// Double-width characters are counted as one; a CJK login in the roster is
// the known cost of not carrying a width table.
func width(s string) int { return utf8.RuneCountInString(s) }

// fit truncates to n columns, marking that it did.
func fit(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if width(s) <= n {
		return s
	}
	runes := []rune(s)
	if n == 1 {
		return "…"
	}
	return string(runes[:n-1]) + "…"
}

// tail keeps the end of a string, which is where someone typing is looking.
func tail(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[len(runes)-n:])
}

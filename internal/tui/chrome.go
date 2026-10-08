// Package tui draws a live share as a room rather than as a pipe.
//
// The pane's frames are a full-screen ANSI paint at a size the server chose,
// so chrome cannot simply be printed alongside them: anything written between
// frames lands inside the viewport the next frame repaints. The arrangement
// here is the one ADR-007 settles on — the pane keeps the top of the terminal,
// the bottom rows are ours, and they are repainted after every frame so they
// survive a paint that clears the screen.
//
// The four rows have one job each, and the split matters: the status line is
// *state* — who is here, what each of them is doing, what the agent is doing —
// and the event line is *history*, the last thing that happened. Facts about
// people belong on the people. Routing one through the event line means it
// hides whatever was there, which is how a delivery confirmation goes unseen.
//
// Everything in this file is a pure function of the room's state. That is
// deliberate: the layout is the part worth testing, and it is testable only
// while it does not own a terminal.
package tui

import (
	"fmt"
	"hash/fnv"
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
	// Host is the operator's login — the person at the pane. They are in the
	// room without being connected to it, so they are named separately from
	// Members or a joiner alone would be told they are alone.
	Host string
	// Members is every joiner connected, including You.
	Members []string
	// Typing is who is composing an instruction right now, never including
	// You.
	Typing []string
	// Chatting is the part of Typing writing to the room, not the agent.
	Chatting []string
	// Agent is herdr's own status vocabulary, or empty when unknown.
	Agent string
	// Thread is the pull request URL the share is bound to.
	Thread string
}

// View is the whole of the chrome's state.
type View struct {
	Room  Room
	Event line
	Input string
	// Mode is where the next typed line goes. It is drawn on the input line
	// and never anywhere else, because that is where the person is looking
	// when it matters.
	Mode mode
	// Cols and Rows are the terminal's, not the pane's.
	Cols, Rows int
	// Pinned and Log fill the host's panel, which stands where a joiner's pane
	// is: the line to send somebody, then what happened, oldest first.
	Pinned line
	Log    []line
	// Keys are the shortcuts shown on the rule, most useful first. The host
	// has none: its hints are on its event row.
	Keys []string
	// Help swaps the history rows for a list of every key the line knows.
	Help bool
	// Extra is how many rows of what led up to the last event are drawn above
	// the chrome's own four. Zero is the bare chrome.
	Extra int
}

// PaneRows is how many rows the pane gets under this view's footer.
func (v View) PaneRows() int {
	return max(v.Rows-ChromeRows-v.Extra, MinPaneRows)
}

// helpLines is every key the input line understands, in two rows so it fits
// the footer as it is by default.
var helpLines = []string{
	"enter send · ↑↓ recall what you sent · ctrl-t agent/room · ctrl-l taller footer",
	"ctrl-u clear line · ctrl-w delete word · ctrl-c leave · ? close this (empty line)",
}

// history is the Extra rows before the last event: the newest closest to it,
// the oldest at the top, blank where nothing has happened yet. The last log
// entry is not repeated, because the event row is already it.
func (v View) history() []line {
	rows := make([]line, v.Extra)
	if v.Help {
		// The newest rows, the ones nearest the input, so the keys sit where
		// the person is looking; a taller footer just has blank rows above.
		for i, text := range helpLines {
			if at := v.Extra - len(helpLines) + i; at >= 0 {
				rows[at] = line{}.add(grey, text)
			}
		}
		return rows
	}
	past := v.Log
	if len(past) > 0 {
		past = past[:len(past)-1]
	}
	if len(past) > v.Extra {
		past = past[len(past)-v.Extra:]
	}
	copy(rows[v.Extra-len(past):], past)
	return rows
}

// ANSI pieces. Written out rather than pulled from a library: four lines of
// chrome do not justify a dependency, and every sequence here is in the subset
// every terminal this runs on has had for decades.
const (
	reset   = "\x1b[0m"
	dim     = "\x1b[2m"
	bold    = "\x1b[1m"
	green   = "\x1b[32m"
	yellow  = "\x1b[33m"
	red     = "\x1b[31m"
	blue    = "\x1b[34m"
	magenta = "\x1b[35m"
	cyan    = "\x1b[36m"
	grey    = "\x1b[90m"
	clrLine = "\x1b[2K"
	// save and restore bracket a chrome repaint so the pane's own cursor
	// position survives it.
	saveCursor    = "\x1b7"
	restoreCursor = "\x1b8"
)

// span is a piece of a chrome row: what it says, and how it is painted.
type span struct {
	text  string
	style string
}

// line is a row assembled from spans.
//
// It exists so that truncation counts what the terminal will *show*. Cutting a
// styled string by byte or even by rune slices through escape sequences, which
// leaves the terminal painting in a colour nobody asked for, for the rest of
// the session.
type line []span

func (l line) add(style, text string) line {
	if text == "" {
		return l
	}
	return append(l, span{text: text, style: style})
}

// plain is the row as a person reads it, with no styling at all.
func (l line) plain() string {
	var b strings.Builder
	for _, s := range l {
		b.WriteString(s.text)
	}
	return b.String()
}

// render paints the row into at most cols columns, marking a truncation.
func (l line) render(cols int) string {
	if cols <= 0 {
		return ""
	}
	var b strings.Builder
	left := cols
	for _, s := range l {
		if left <= 0 {
			break
		}
		text := s.text
		if width(text) > left {
			// One column is kept for the ellipsis, so the row still ends by
			// saying that it was cut.
			text = fit(text, left)
		}
		left -= width(text)
		if s.style == "" {
			b.WriteString(text)
			continue
		}
		b.WriteString(s.style)
		b.WriteString(text)
		b.WriteString(reset)
	}
	return b.String()
}

// Chrome renders the bottom rows and leaves the cursor on the input line,
// where the person typing expects to find it.
func (v View) Chrome() string {
	cols := v.Cols
	if cols < 20 {
		cols = 20
	}
	first := v.Rows - ChromeRows - v.Extra + 1
	if first < 1 {
		first = 1
	}

	var b strings.Builder
	b.WriteString(saveCursor)
	rows := append([]line{v.rule(cols), v.status()}, v.history()...)
	rows = append(rows, v.event(), v.prompt(cols))
	for i, row := range rows {
		fmt.Fprintf(&b, "\x1b[%d;1H%s%s", first+i, clrLine, row.render(cols))
	}
	b.WriteString(restoreCursor)
	// The cursor is parked where the typing goes, after the restore, because a
	// visible cursor anywhere else reads as the terminal having lost it.
	fmt.Fprintf(&b, "\x1b[%d;%dH", v.Rows, promptWidth(v.Input, v.Mode, cols))
	return b.String()
}

// Panel paints the rows above the chrome with the host's view of the huddle —
// the pinned line first, then as much of the log as fits, newest at the
// bottom. It is the host's counterpart of the pane a joiner is looking at.
func (v View) Panel() string {
	cols := max(v.Cols, 20)
	n := v.PaneRows()
	rows := make([]line, 0, n)
	if len(v.Pinned) > 0 {
		rows = append(rows, v.Pinned)
	}
	logs := v.Log
	if room := n - len(rows); len(logs) > room {
		logs = logs[len(logs)-room:]
	}
	rows = append(rows, logs...)

	var b strings.Builder
	for i := 0; i < n; i++ {
		var row line
		if i < len(rows) {
			row = rows[i]
		}
		fmt.Fprintf(&b, "\x1b[%d;1H%s%s", i+1, clrLine, row.render(cols))
	}
	return b.String()
}

// rule is the seam between the pane and the room, with the record's name on
// the right so the pull request is never more than a glance away (ADR-007).
func (v View) rule(cols int) line {
	left := "── huddle "
	right := ""
	if short := shortThread(v.Room.Thread); short != "" {
		right = " " + short + " ──"
	}
	// Shortcuts, as many as fit, most useful first, and always leaving the
	// rule a stretch of line so it still reads as a rule.
	const minFill = 3
	var keys string
	for _, k := range v.Keys {
		next := keys + "· " + k + " "
		if width(left)+width(next)+width(right)+minFill > cols {
			break
		}
		keys = next
	}
	fill := cols - width(left) - width(keys) - width(right)
	if fill < 0 {
		keys, right, fill = "", "", cols-width(left)
	}
	if fill < 0 {
		fill = 0
	}
	return line{}.add(dim, left).add(grey, keys).add(dim, strings.Repeat("─", fill)+right)
}

// status is the room's state in one line: what the agent is doing, then who is
// in the room.
func (v View) status() line {
	label, colour := agentState(v.Room.Agent)
	row := line{}.add(colour, "●").add("", " ").add(bold, label)
	if who := v.roster(); len(who) > 0 {
		row = append(row.add("", "   "), who...)
	}
	return row
}

// roster names the room the way a person would: the host, then you, then
// everyone else — each in their own colour, so the room is scanned rather than
// read.
func (v View) roster() line {
	var people []line
	seen := map[string]bool{}

	typing := map[string]bool{}
	for _, who := range v.Room.Typing {
		typing[strings.ToLower(who)] = true
	}

	chatting := map[string]bool{}
	for _, who := range v.Room.Chatting {
		chatting[strings.ToLower(who)] = true
	}

	mark := func(login, note string) {
		key := strings.ToLower(login)
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		entry := line{}.add(personColour(login), "@"+login)
		// Typing is a fact about a person, so it is drawn on the person —
		// alongside (host), in the same shape. It deliberately does not live
		// on the event line: somebody else composing must never hide the
		// confirmation that your own instruction was delivered.
		if typing[key] {
			// Who it is for matters as much as that it is happening: a
			// message to the room is not about to change what the agent does.
			doing := "typing to agent…"
			if chatting[key] {
				doing = "typing to room…"
			}
			if note != "" {
				note += ", " + doing
			} else {
				note = doing
			}
		}
		if note != "" {
			entry = entry.add(dim, " ("+note+")")
		}
		people = append(people, entry)
	}

	you := func(note string) {
		key := strings.ToLower(v.Room.You)
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		entry := line{}.add(bold, "you")
		if note != "" {
			entry = entry.add(dim, " ("+note+")")
		}
		people = append(people, entry)
	}

	youIn := false
	for _, member := range v.Room.Members {
		if strings.EqualFold(member, v.Room.You) {
			youIn = true
		}
	}

	// The host leads, because they are whose machine this is. "You" comes
	// next, and everybody else after — which is also the order a person would
	// say it in.
	hosting := v.Room.Host != "" && strings.EqualFold(v.Room.Host, v.Room.You)
	if hosting {
		you("host")
	} else if v.Room.Host != "" {
		mark(v.Room.Host, "host")
	}
	if youIn || hosting {
		you("")
	}
	for _, member := range v.Room.Members {
		mark(member, "")
	}

	if len(people) == 0 {
		return nil
	}
	// Alone, and worth saying plainly rather than as a list of one.
	if len(people) == 1 && youIn && v.Room.Host == "" {
		return line{}.add(dim, "you are the only one here")
	}
	return join(people, line{}.add(dim, ", "), line{}.add(dim, " and "))
}

// join renders a list the way English does: commas, then "and" before the last.
func join(parts []line, comma, last line) line {
	var out line
	for i, part := range parts {
		switch {
		case i == 0:
		case i == len(parts)-1:
			out = append(out, last...)
		default:
			out = append(out, comma...)
		}
		out = append(out, part...)
	}
	return out
}

// event is the last thing that happened, and only that.
//
// Who is typing is deliberately *not* here. It is state, not an event, and it
// belongs on the person in the roster: routed through this line it would
// outrank — and so hide — the confirmation that somebody's instruction was
// delivered, for as long as anyone else kept typing.
func (v View) event() line { return v.Event }

// prompt is the input line, and it always says where the line is going.
//
// The expensive mistake here is a silent one: telling the agent to do
// something when you meant to say "hang on a second" to a colleague. So the
// destination is a word, not a colour, and it is never absent.
func (v View) prompt(cols int) line {
	label, colour := v.Mode.label()
	marker := label + " › "
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
	return line{}.add(colour, label).add(dim, " › ").add("", text)
}

func (m mode) label() (string, string) {
	if m == toRoom {
		return "room", magenta
	}
	return "agent", cyan
}

// promptWidth is the column the cursor belongs in, 1-based.
func promptWidth(input string, mode mode, cols int) int {
	label, _ := mode.label()
	marker := width(label) + 3 // "label › "
	room := cols - marker
	if room < 1 {
		room = 1
	}
	shown := width(input)
	if shown > room-1 {
		shown = room - 1
	}
	return marker + 1 + shown
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

// people is the palette logins are coloured from. Red and yellow are left out
// on purpose: they mean "the agent is blocked" and "the agent is working" one
// line above, and a person whose name is the same red would read as an alarm.
var people = []string{cyan, magenta, blue, green, "\x1b[96m", "\x1b[95m"}

// personColour picks a login's colour, and picks the same one every time.
//
// Stability is the whole point: @ana is one colour in the roster, in the
// typing line and in the record of what she asked for, so the room is scanned
// rather than read word by word.
func personColour(login string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(strings.ToLower(login)))
	return people[int(h.Sum32())%len(people)]
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

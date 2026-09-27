package tui

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/DnzzL/herdr-huddle/internal/live"
)

// Client is the half of a live session the room drives. It is an interface so
// the app can be exercised without a socket.
type Client interface {
	Run(live.Events) error
	Say(text string) error
	Resize(cols, rows int) error
	Typing(on bool) error
}

// Keys that are not characters. They are carried as runes from the Unicode
// private use area, which is the one range a person cannot type and so the one
// range that can never be mistaken for input.
const (
	keyFirst rune = 0xE000 + iota
	keyUp
	keyDown
)

// Terminal control. The alternate screen is what makes leaving the huddle
// restore the shell the person was in rather than leave a painted pane behind.
const (
	enterAlt    = "\x1b[?1049h"
	leaveAlt    = "\x1b[?1049l"
	clearScreen = "\x1b[2J\x1b[H"
	showCursor  = "\x1b[?25h"
)

// App draws a live share in this terminal, and sends what is typed into it.
type App struct {
	// In is the terminal keystrokes are read from, and the fd put into raw
	// mode. Required.
	In *os.File
	// Out is the terminal drawn onto, and the fd measured. Required.
	Out *os.File
	// Hint is the first thing on the event line, before anything has
	// happened.
	Hint string

	mu         sync.Mutex
	view       View
	session    Client
	paneRows   int
	lastMember map[string]bool

	// history is what this person has sent, oldest first, and histAt is where
	// they are while walking back through it. Retyping an instruction that
	// came back "held" is the papercut this exists to remove.
	history []string
	histAt  int

	// typingOn is whether the room currently believes this person is
	// composing, and typingAt is when we last said so. The claim is renewed
	// rather than repeated per keystroke: the room would otherwise be told
	// something it already knows, several times a second.
	typingOn bool
	typingAt time.Time
}

// typingRenew is how often a standing typing claim is renewed. Comfortably
// inside live.TypingTTL, so a person typing slowly never flickers out of the
// room's view.
const typingRenew = 2 * time.Second

// Smallest and fallback window. A pty opened without a size — a detached
// `script`, a CI runner, a terminal multiplexer mid-attach — reports 0x0, and
// zero is not a window: laid out literally it puts all four chrome rows on top
// of each other at the top of the screen.
const (
	minCols      = 20
	minRows      = ChromeRows + MinPaneRows
	fallbackCols = 80
	fallbackRows = 24
)

// Size reports the terminal's dimensions, never a size too small to lay a room
// out in.
//
// It is the one place a window size enters this program, so it is the one
// place that has to be suspicious of it: every caller — the dial that asks for
// a viewport, the first paint, every resize — goes through here.
func Size(f *os.File) (cols, rows int) {
	cols, rows, err := term.GetSize(int(f.Fd()))
	if err != nil {
		cols, rows = 0, 0
	}
	if cols < minCols {
		cols = fallbackCols
	}
	if rows < minRows {
		rows = fallbackRows
	}
	return cols, rows
}

// IsTerminal reports whether this is something to composite onto. A pipe is
// not, and `join` falls back to a raw render for it.
func IsTerminal(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }

// Run draws the session until it ends, the context is cancelled, or the person
// leaves with Ctrl-C.
//
// Leaving is not a failure: Ctrl-C and a stream that closed cleanly both
// return nil, because both are how a huddle ends.
func (a *App) Run(ctx context.Context, session Client) error {
	a.session = session

	state, err := term.MakeRaw(int(a.In.Fd()))
	if err != nil {
		return fmt.Errorf("tui: could not take the keyboard: %w", err)
	}
	defer func() { _ = term.Restore(int(a.In.Fd()), state) }()

	cols, rows := Size(a.Out)

	a.mu.Lock()
	a.view.Cols, a.view.Rows = cols, rows
	a.view.Event = line{}.add(dim, a.Hint)
	a.paneRows = PaneRows(rows)
	a.histAt = 0
	a.mu.Unlock()

	a.writeRaw(enterAlt + clearScreen + a.scrollRegion())
	defer a.writeRaw("\x1b[r" + leaveAlt + showCursor + reset)
	a.repaint()

	// SIGWINCH is the only way a terminal says it changed size, and the
	// server has to hear about it: the stream is rendered at the size this
	// joiner asked for (ADR-007).
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)

	streamed := make(chan error, 1)
	go func() {
		streamed <- session.Run(live.Events{
			Frame: a.drawFrame,
			Room:  a.setRoom,
			Said:  a.said,
		})
	}()

	keys := make(chan rune, 64)
	go a.readKeys(ctx, keys)

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-streamed:
			return err
		case <-winch:
			a.resized()
		case key, ok := <-keys:
			if !ok {
				// The keyboard ended — a closed stdin — but the pane has not.
				keys = nil
				continue
			}
			if leave := a.key(key); leave {
				return nil
			}
		}
	}
}

// key applies one keystroke and reports whether it was the one that leaves.
func (a *App) key(r rune) bool {
	switch r {
	case 3, 4: // Ctrl-C, Ctrl-D. Raw mode means no signal arrives for these.
		return true
	case '\r', '\n':
		a.submit()
	case 127, 8: // backspace
		a.edit(func(in []rune) []rune {
			if len(in) == 0 {
				return in
			}
			return in[:len(in)-1]
		})
	case 21: // Ctrl-U
		a.edit(func([]rune) []rune { return nil })
	case 23: // Ctrl-W
		a.edit(dropWord)
	case keyUp:
		a.recall(-1)
	case keyDown:
		a.recall(+1)
	default:
		if r < 0x20 || r >= keyFirst {
			return false // an unhandled control or navigation key types nothing
		}
		a.edit(func(in []rune) []rune { return append(in, r) })
	}
	return false
}

// recall walks back through what this person has sent, and forward again.
//
// Walking past the newest entry lands on an empty line rather than sticking,
// which is how every shell behaves and therefore what the hands expect.
func (a *App) recall(step int) {
	a.mu.Lock()
	if len(a.history) == 0 {
		a.mu.Unlock()
		return
	}
	at := a.histAt + step
	if at < 0 {
		at = 0
	}
	if at > len(a.history) {
		at = len(a.history)
	}
	a.histAt = at
	if at == len(a.history) {
		a.view.Input = ""
	} else {
		a.view.Input = a.history[at]
	}
	a.mu.Unlock()
	a.repaint()
	a.claimTyping()
}

// submit sends the typed line to the agent and clears the input.
//
// The line is cleared whatever the outcome: the room reports delivery on the
// event line, and an input box that keeps text after enter invites the same
// instruction being sent twice.
func (a *App) submit() {
	a.mu.Lock()
	text := strings.TrimSpace(a.view.Input)
	a.view.Input = ""
	if text != "" && (len(a.history) == 0 || a.history[len(a.history)-1] != text) {
		a.history = append(a.history, text)
	}
	a.histAt = len(a.history)
	a.mu.Unlock()

	// Whatever happens next, the sentence is finished.
	a.stopTyping()

	if text == "" {
		a.repaint()
		return
	}
	if err := a.session.Say(text); err != nil {
		a.setEvent(line{}.add(red, "could not send: "+err.Error()))
		return
	}
	a.setEvent(line{}.add(dim, "sending…"))
}

// claimTyping tells the room this person is composing, renewing a standing
// claim rather than repeating it.
func (a *App) claimTyping() {
	a.mu.Lock()
	empty := strings.TrimSpace(a.view.Input) == ""
	fresh := a.typingOn && time.Since(a.typingAt) < typingRenew
	if !empty {
		a.typingOn, a.typingAt = true, time.Now()
	}
	a.mu.Unlock()

	if empty {
		a.stopTyping()
		return
	}
	if fresh {
		return
	}
	_ = a.session.Typing(true)
}

// stopTyping retracts the claim, if one is standing.
func (a *App) stopTyping() {
	a.mu.Lock()
	was := a.typingOn
	a.typingOn = false
	a.mu.Unlock()
	if was {
		_ = a.session.Typing(false)
	}
}

func (a *App) edit(f func([]rune) []rune) {
	a.mu.Lock()
	a.view.Input = string(f([]rune(a.view.Input)))
	// Editing leaves the history where it was: the recalled line has been
	// changed, so it is this person's line now, not entry number four.
	a.histAt = len(a.history)
	a.mu.Unlock()
	a.repaint()
	a.claimTyping()
}

// dropWord deletes the word before the cursor, trailing spaces included.
func dropWord(in []rune) []rune {
	i := len(in)
	for i > 0 && in[i-1] == ' ' {
		i--
	}
	for i > 0 && in[i-1] != ' ' {
		i--
	}
	return in[:i]
}

// drawFrame writes the pane's bytes and puts the chrome back on top of them.
//
// The repaint is not decoration: a full paint from Herdr clears the screen,
// which would otherwise take the room's four rows with it.
func (a *App) drawFrame(data []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, err := a.Out.Write(data); err != nil {
		return err
	}
	return a.paintLocked()
}

// setRoom updates the roster and says who came and went. The arrivals are
// worth announcing: somebody joining a huddle silently is how two people end
// up sending the agent the same instruction.
func (a *App) setRoom(f live.Frame) {
	a.mu.Lock()
	a.view.Room = Room{
		You:     f.You,
		Host:    f.Host,
		Members: f.Members,
		Typing:  f.Typing,
		Agent:   f.Agent,
		Thread:  f.Thread,
	}
	if event := a.membershipEventLocked(f); len(event) > 0 {
		a.view.Event = event
	}
	a.mu.Unlock()
	a.repaint()
}

// membershipEventLocked words the difference between the last roster and this
// one. The first roster is not a difference — everybody is new — so it says
// nothing.
func (a *App) membershipEventLocked(f live.Frame) line {
	now := make(map[string]bool, len(f.Members))
	for _, member := range f.Members {
		now[strings.ToLower(member)] = true
	}
	first := a.lastMember == nil
	var event line
	arrivals := func(who string, verb string) {
		if len(event) > 0 {
			event = event.add(dim, " · ")
		}
		event = event.add(personColour(who), "@"+who).add(dim, verb)
	}
	if !first {
		for _, member := range f.Members {
			if !a.lastMember[strings.ToLower(member)] && !strings.EqualFold(member, f.You) {
				arrivals(member, " joined")
			}
		}
		for member := range a.lastMember {
			if !now[member] && !strings.EqualFold(member, f.You) {
				arrivals(member, " left")
			}
		}
	}
	a.lastMember = now
	return event
}

// said words a delivery outcome: who asked, what they asked for, and what
// became of it.
//
// The text matters as much as the outcome. Two people steering one agent need
// to know whether the other just asked for the thing they were about to ask
// for, and "delivered to the agent" does not tell them that.
func (a *App) said(f live.Frame) {
	a.mu.Lock()
	mine := f.Author != "" && strings.EqualFold(f.Author, a.view.Room.You)
	a.mu.Unlock()

	row := line{}
	switch {
	case mine:
		row = row.add(bold, "you")
	case f.Author != "":
		row = row.add(personColour(f.Author), "@"+f.Author)
	}
	if said := strings.TrimSpace(f.Text); said != "" {
		row = row.add(dim, " → ").add("", said)
	}

	outcome, style := outcomeOf(f)
	if len(row) > 0 {
		row = row.add(dim, " · ")
	}
	a.setEvent(row.add(style, outcome))
}

// outcomeOf words what became of an instruction, and colours it by whether
// anybody needs to do something about it.
func outcomeOf(f live.Frame) (string, string) {
	switch f.Status {
	case live.StatusSent:
		return "delivered", green
	case live.StatusHeld:
		return "held: " + f.Reason, yellow
	case live.StatusFailed:
		return "not delivered: " + f.Reason, red
	default:
		if f.Reason != "" {
			return f.Status + ": " + f.Reason, grey
		}
		return f.Status, grey
	}
}

func (a *App) setEvent(row line) {
	a.mu.Lock()
	a.view.Event = row
	a.mu.Unlock()
	a.repaint()
}

// resized re-lays the room out and asks the server for a stream at the new
// size, which arrives as a complete repaint.
func (a *App) resized() {
	cols, rows := Size(a.Out)
	a.mu.Lock()
	if cols == a.view.Cols && rows == a.view.Rows {
		a.mu.Unlock()
		return
	}
	a.view.Cols, a.view.Rows = cols, rows
	a.paneRows = PaneRows(rows)
	pane := a.paneRows
	a.mu.Unlock()

	a.writeRaw(clearScreen + a.scrollRegion())
	a.repaint()
	if err := a.session.Resize(cols, pane); err != nil {
		a.setEvent(line{}.add(red, "could not ask for a repaint at the new size: "+err.Error()))
	}
}

// scrollRegion confines the pane's scrolling to the rows it owns, so a line
// scrolling off the top of the pane never pushes the room's chrome up with it.
// It is half the guard; the repaint after each frame is the other half, for
// the paints that clear the screen instead of scrolling.
func (a *App) scrollRegion() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return fmt.Sprintf("\x1b[1;%dr", a.paneRows)
}

func (a *App) repaint() {
	a.mu.Lock()
	defer a.mu.Unlock()
	_ = a.paintLocked()
}

func (a *App) paintLocked() error {
	_, err := io.WriteString(a.Out, a.view.Chrome())
	return err
}

func (a *App) writeRaw(s string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, _ = io.WriteString(a.Out, s)
}

// readKeys turns the raw terminal into keystrokes.
//
// Escape sequences are parsed rather than discarded, because two of them are
// wanted: up and down walk back through what this person has already sent.
// Everything else a terminal sends — function keys, a mouse, a bracketed
// paste — is dropped, since typing it into the instruction box is worse than
// ignoring it.
func (a *App) readKeys(ctx context.Context, out chan<- rune) {
	defer close(out)
	reader := bufio.NewReader(a.In)
	send := func(r rune) bool {
		select {
		case out <- r:
			return true
		case <-ctx.Done():
			return false
		}
	}
	for {
		r, _, err := reader.ReadRune()
		if err != nil {
			return
		}
		if r != 0x1b {
			if !send(r) {
				return
			}
			continue
		}
		// A key press reaches the terminal as one read, so an escape with
		// nothing behind it is a person pressing Esc, not the start of a
		// sequence.
		if reader.Buffered() == 0 {
			continue
		}
		key, err := readCSI(reader)
		if err != nil {
			return
		}
		if key != 0 && !send(key) {
			return
		}
	}
}

// readCSI consumes one escape sequence and returns the key it meant, or zero
// for one this does not act on.
//
// A CSI runs from "[" to the first byte in 0x40–0x7E, which is what makes a
// parameterised sequence (ESC [ 1 ; 5 A, a modified arrow) as consumable as a
// bare one. Consuming the whole of it is the point: a sequence left half-read
// becomes garbage in the input line.
func readCSI(reader *bufio.Reader) (rune, error) {
	b, err := reader.ReadByte()
	if err != nil {
		return 0, err
	}
	if b != '[' && b != 'O' {
		return 0, nil // an escape followed by something that is not a sequence
	}
	for {
		b, err = reader.ReadByte()
		if err != nil {
			return 0, err
		}
		if b >= 0x40 && b <= 0x7E {
			switch b {
			case 'A':
				return keyUp, nil
			case 'B':
				return keyDown, nil
			}
			return 0, nil
		}
	}
}

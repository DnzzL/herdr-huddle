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

	"golang.org/x/term"

	"github.com/DnzzL/herdr-huddle/internal/live"
)

// Client is the half of a live session the room drives. It is an interface so
// the app can be exercised without a socket.
type Client interface {
	Run(live.Events) error
	Say(text string) error
	Resize(cols, rows int) error
}

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
}

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
	a.view.Cols, a.view.Rows, a.view.Event = cols, rows, a.Hint
	a.paneRows = PaneRows(rows)
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
	default:
		if r < 0x20 {
			return false // an unhandled control key types nothing
		}
		a.edit(func(in []rune) []rune { return append(in, r) })
	}
	return false
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
	a.mu.Unlock()

	if text == "" {
		a.repaint()
		return
	}
	if err := a.session.Say(text); err != nil {
		a.setEvent("could not send: " + err.Error())
		return
	}
	a.setEvent("sending: " + text)
}

func (a *App) edit(f func([]rune) []rune) {
	a.mu.Lock()
	a.view.Input = string(f([]rune(a.view.Input)))
	a.mu.Unlock()
	a.repaint()
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
	a.view.Room = Room{You: f.You, Members: f.Members, Agent: f.Agent, Thread: f.Thread}
	if event := a.membershipEventLocked(f); event != "" {
		a.view.Event = event
	}
	a.mu.Unlock()
	a.repaint()
}

// membershipEventLocked words the difference between the last roster and this
// one. The first roster is not a difference — everybody is new — so it says
// nothing.
func (a *App) membershipEventLocked(f live.Frame) string {
	now := make(map[string]bool, len(f.Members))
	for _, member := range f.Members {
		now[strings.ToLower(member)] = true
	}
	first := a.lastMember == nil
	var events []string
	if !first {
		for _, member := range f.Members {
			if !a.lastMember[strings.ToLower(member)] && !strings.EqualFold(member, f.You) {
				events = append(events, "@"+member+" joined")
			}
		}
		for member := range a.lastMember {
			if !now[member] && !strings.EqualFold(member, f.You) {
				events = append(events, "@"+member+" left")
			}
		}
	}
	a.lastMember = now
	return strings.Join(events, " · ")
}

// said words a delivery outcome, dropping the attribution when the person
// reading it is the one who typed it.
func (a *App) said(f live.Frame) {
	a.mu.Lock()
	mine := f.Author != "" && strings.EqualFold(f.Author, a.view.Room.You)
	a.mu.Unlock()
	if mine {
		f.Author = ""
	}
	a.setEvent(live.SaidLine(f))
}

func (a *App) setEvent(text string) {
	a.mu.Lock()
	a.view.Event = text
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
		a.setEvent("could not ask for a repaint at the new size: " + err.Error())
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

// readKeys turns the raw terminal into keystrokes, swallowing the escape
// sequences an arrow key or a mouse sends: typing them into the instruction
// box is worse than ignoring them.
func (a *App) readKeys(ctx context.Context, out chan<- rune) {
	defer close(out)
	reader := bufio.NewReader(a.In)
	for {
		r, _, err := reader.ReadRune()
		if err != nil {
			return
		}
		if r == 0x1b {
			// Whatever arrived with the escape is the rest of its sequence:
			// a key press reaches the terminal in one read, a typed escape
			// alone does not.
			for reader.Buffered() > 0 {
				if _, err := reader.ReadByte(); err != nil {
					return
				}
			}
			continue
		}
		select {
		case out <- r:
		case <-ctx.Done():
			return
		}
	}
}

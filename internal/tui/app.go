package tui

import (
	"bufio"
	"context"
	"errors"
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
	Chat(text string) error
	Resize(cols, rows int) error
	Typing(on bool) error
	Close() error
}

// Dialer opens a connection to the huddle. It is called again on every
// reconnect, so whatever it closes over — the viewport, the token — is read
// fresh each time.
type Dialer func(context.Context) (Client, error)

// Where a typed line goes. The two are never one call with a flag: "tell my
// colleague" and "tell the agent to do something" must not be separated by a
// boolean somebody can get the wrong way round.
type mode int

const (
	toAgent mode = iota
	toRoom
)

// errLeft is Ctrl-C: the person left, which is not a failure and never a
// reason to reconnect.
var errLeft = errors.New("tui: left the huddle")

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

// Run draws the huddle until the stream ends for good, the context is
// cancelled, or the person leaves with Ctrl-C.
//
// A dropped connection is not the end. The tunnel is a quick tunnel and the
// network is a network, so the loop below redials with a backoff and keeps the
// terminal it has already taken over — the alternative is that one blip ends
// the huddle for everybody and they all start again by hand.
//
// The two failures it does not retry are the two that retrying cannot fix: the
// person was not let in, and the person left.
func (a *App) Run(ctx context.Context, dial Dialer) error {
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

	// The keyboard and the window belong to the terminal, not to any one
	// connection, so they are set up once and survive every reconnect.
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)

	keys := make(chan rune, 64)
	go a.readKeys(ctx, keys)

	var attempt int
	joined := false
	for {
		client, err := dial(ctx)
		if err != nil {
			if stop := a.backOff(ctx, keys, err, &attempt); stop != nil {
				return unlessLeft(stop)
			}
			continue
		}
		attempt = 0
		a.attach(client)
		if joined {
			// Coming back is worth saying. Left on "reconnecting…", the room
			// reads as still broken while it is already working.
			a.setEvent(line{}.add(green, "reconnected"))
		}
		joined = true

		err = a.oneConnection(ctx, client, keys, winch)
		_ = client.Close()
		a.detach()

		if err == nil {
			return nil // the stream closed cleanly: the huddle is over
		}
		if stop := a.backOff(ctx, keys, err, &attempt); stop != nil {
			return unlessLeft(stop)
		}
	}
}

// unlessLeft turns "the person pressed Ctrl-C" back into an ordinary exit.
func unlessLeft(err error) error {
	if errors.Is(err, errLeft) {
		return nil
	}
	return err
}

// session1 runs one connection, and returns why it ended.
func (a *App) oneConnection(ctx context.Context, client Client, keys <-chan rune, winch <-chan os.Signal) error {
	streamed := make(chan error, 1)
	go func() {
		streamed <- client.Run(live.Events{
			Frame: a.drawFrame,
			Room:  a.setRoom,
			Said:  a.said,
			Chat:  a.heard,
		})
	}()

	for {
		select {
		case <-ctx.Done():
			return errLeft
		case err := <-streamed:
			if err == nil {
				return nil
			}
			return err
		case <-winch:
			a.resized()
		case key, ok := <-keys:
			if !ok {
				return errLeft // stdin ended
			}
			if leave := a.key(key); leave {
				return errLeft
			}
		}
	}
}

// backOff waits before dialling again, and reports a reason to stop instead.
//
// The wait grows and is capped, because a tunnel that is down stays down for
// seconds rather than milliseconds and hammering it helps nobody. Ctrl-C is
// still answered throughout: a person watching "reconnecting in 15s" must be
// able to give up without waiting for it.
func (a *App) backOff(ctx context.Context, keys <-chan rune, why error, attempt *int) error {
	switch {
	case errors.Is(why, errLeft):
		return why
	case errors.Is(why, live.ErrRefused):
		return why
	case ctx.Err() != nil:
		return errLeft
	}

	wait := reconnectDelay(*attempt)
	*attempt++
	a.disconnected(why, wait, *attempt)

	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return errLeft
		case <-timer.C:
			a.setEvent(line{}.add(dim, "reconnecting…"))
			return nil
		case key, ok := <-keys:
			if !ok {
				return errLeft
			}
			if key == 3 || key == 4 { // Ctrl-C, Ctrl-D
				return errLeft
			}
		}
	}
}

// reconnectDelay grows the wait to a ceiling: half a second for the blip that
// fixes itself, fifteen for the tunnel that is properly gone.
func reconnectDelay(attempt int) time.Duration {
	const first, max = 500 * time.Millisecond, 15 * time.Second
	wait := first << attempt
	if wait > max || wait <= 0 {
		wait = max
	}
	return wait
}

// disconnected says what happened and what is about to happen. A room that
// goes silent is indistinguishable from a room that is thinking.
func (a *App) disconnected(why error, wait time.Duration, attempt int) {
	a.mu.Lock()
	// Nothing known about the room is true any more: whoever was in it is not
	// reachable from here.
	a.view.Room = Room{You: a.view.Room.You, Thread: a.view.Room.Thread}
	a.lastMember = nil
	a.mu.Unlock()
	a.setEvent(line{}.
		add(red, "connection lost").
		add(dim, " · "+fit(strings.ReplaceAll(why.Error(), "\n", " "), 80)+" · ").
		add(yellow, "retrying in "+humanWait(wait)).
		add(dim, fmt.Sprintf(" (attempt %d, Ctrl-C to give up)", attempt)))
}

// humanWait words a delay for a person. Rounding to seconds turns the first,
// deliberately brief retry into "0s", which reads as broken rather than fast.
func humanWait(d time.Duration) string {
	if d < time.Second {
		return "a moment"
	}
	return d.Round(time.Second).String()
}

func (a *App) attach(client Client) {
	a.mu.Lock()
	a.session = client
	a.mu.Unlock()
}

func (a *App) detach() {
	a.mu.Lock()
	a.session = nil
	a.typingOn = false
	a.mu.Unlock()
}

// talk returns the connection to speak through, or nil while reconnecting.
func (a *App) talk() Client {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.session
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
	case 20: // Ctrl-T
		a.toggleMode()
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
	client := a.talk()
	if client == nil {
		a.setEvent(line{}.add(red, "not connected — nothing was sent"))
		return
	}

	a.mu.Lock()
	where := a.view.Mode
	a.mu.Unlock()

	if where == toRoom {
		if err := client.Chat(text); err != nil {
			a.setEvent(line{}.add(red, "could not send: "+err.Error()))
			return
		}
		// The input was cleared above; repaint so it *looks* cleared. Without
		// this the sent line sits on screen until the echo arrives, which
		// invites sending it twice.
		a.repaint()
		return // the echo comes back from the room, attributed
	}
	if err := client.Say(text); err != nil {
		a.setEvent(line{}.add(red, "could not send: "+err.Error()))
		return
	}
	a.setEvent(line{}.add(dim, "sending…"))
}

// toggleMode switches between talking to the agent and talking to the room.
//
// The mode is always drawn on the input line, because the expensive mistake is
// silent: telling the agent to do something when you meant to say "hang on" to
// a colleague.
func (a *App) toggleMode() {
	a.mu.Lock()
	if a.view.Mode == toAgent {
		a.view.Mode = toRoom
	} else {
		a.view.Mode = toAgent
	}
	a.mu.Unlock()
	a.repaint()
}

// heard draws a message somebody sent to the room. It never reached the agent,
// and the line says so by naming the person and nothing else.
func (a *App) heard(f live.Frame) {
	a.mu.Lock()
	mine := f.Author != "" && strings.EqualFold(f.Author, a.view.Room.You)
	a.mu.Unlock()

	row := line{}
	if mine {
		row = row.add(bold, "you")
	} else {
		row = row.add(personColour(f.Author), "@"+f.Author)
	}
	a.setEvent(row.add(dim, ": ").add(magenta, strings.TrimSpace(f.Text)))
}

// claimTyping tells the room this person is composing, renewing a standing
// claim rather than repeating it. Composing a message to the room is not
// composing an instruction, so it makes no claim at all.
func (a *App) claimTyping() {
	a.mu.Lock()
	empty := strings.TrimSpace(a.view.Input) == "" || a.view.Mode == toRoom
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
	if client := a.talk(); client != nil {
		_ = client.Typing(true)
	}
}

// stopTyping retracts the claim, if one is standing.
func (a *App) stopTyping() {
	a.mu.Lock()
	was := a.typingOn
	a.typingOn = false
	a.mu.Unlock()
	if !was {
		return
	}
	if client := a.talk(); client != nil {
		_ = client.Typing(false)
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

	if len(row) > 0 {
		row = row.add(dim, " · ")
	}
	a.setEvent(row.add(outcomeColour(f.Status), live.Outcome(f)))
}

// outcomeColour says whether anybody needs to do something about an outcome.
// The words are live's, once, so the two clients cannot drift apart; only the
// paint is ours.
func outcomeColour(status string) string {
	switch status {
	case live.StatusSent:
		return green
	case live.StatusQueued, live.StatusHeld:
		return yellow
	case live.StatusRefused, live.StatusFailed:
		return red
	default:
		return grey
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
	client := a.talk()
	if client == nil {
		return // reconnecting; the next hello carries the new size
	}
	if err := client.Resize(cols, pane); err != nil {
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

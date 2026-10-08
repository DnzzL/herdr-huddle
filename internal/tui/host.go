package tui

import (
	"context"
	"strings"
	"time"
)

// asking is a question on the host's screen, waiting for a key.
type asking struct{ answer chan bool }

// logLocked adds a stamped row to the host's panel. The caller holds mu.
func (a *App) logLocked(row line) {
	stamped := line{}.add(grey, time.Now().Format("15:04")+" ")
	a.view.Log = append(a.view.Log, append(stamped, row...))
	if over := len(a.view.Log) - maxLog; over > 0 {
		a.view.Log = append([]line(nil), a.view.Log[over:]...)
	}
}

// Note writes a line on the host's panel. It is how what `serve` used to print
// to stderr reaches a screen that is no longer stderr's.
func (a *App) Note(text string) {
	a.mu.Lock()
	a.logLocked(line{}.add(dim, text))
	running := a.running
	a.mu.Unlock()
	if running {
		a.repaint()
	}
}

// Pin sets the line that stays at the top of the panel: the one thing the
// host has to be able to find, which is what to send somebody.
func (a *App) Pin(text string) {
	a.mu.Lock()
	a.view.Pinned = line{}.add(bold+cyan, text)
	running := a.running
	a.mu.Unlock()
	if running {
		a.repaint()
	}
}

// Ask puts a yes/no question to the host and waits for one key.
//
// It is the door's and the moderator's seat at the host's terminal, now that
// the terminal is the TUI's and not a line reader's. The properties the line
// reader had are kept: one question at a time in arrival order, an answer only
// ever answers the question in front of it, anything but "y" is no, and a
// question about somebody who is gone is dropped when ctx ends.
func (a *App) Ask(ctx context.Context, question string) (bool, error) {
	a.askOnce.Do(func() {
		a.askTurn = make(chan struct{}, 1)
		a.askTurn <- struct{}{}
	})
	select {
	case <-a.askTurn:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	defer func() { a.askTurn <- struct{}{} }()

	var lines []string
	for l := range strings.SplitSeq(question, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	q := &asking{answer: make(chan bool, 1)}
	a.mu.Lock()
	for _, l := range lines {
		a.logLocked(line{}.add(yellow, "? "+l))
	}
	a.view.Event = line{}.add(bold+yellow, "? "+strings.Join(lines, " — ")+"  [y/n]")
	a.pending = q
	a.mu.Unlock()
	// The bell is for the operator who is looking at their agent's pane.
	a.writeRaw("\a")
	a.repaint()

	var yes bool
	var err error
	select {
	case yes = <-q.answer:
	case <-ctx.Done():
		err = ctx.Err()
	}

	a.mu.Lock()
	a.pending = nil
	a.view.Event = line{}.add(dim, a.Hint)
	switch {
	case err != nil:
		a.logLocked(line{}.add(dim, "never mind — they are gone"))
	case yes:
		a.logLocked(line{}.add(green, "→ yes"))
	default:
		a.logLocked(line{}.add(red, "→ no"))
	}
	a.mu.Unlock()
	a.repaint()
	return yes, err
}

// answered reports whether this key answered a standing question.
//
// Only on an empty input line: a person halfway through "yes, wait" must not
// admit a stranger by typing, and a key typed before the question existed
// never gets here, because keys are consumed as they arrive.
func (a *App) answered(r rune) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pending == nil || strings.TrimSpace(a.view.Input) != "" {
		return false
	}
	var yes bool
	switch r {
	case 'y', 'Y':
		yes = true
	case 'n', 'N':
	default:
		return false
	}
	select {
	case a.pending.answer <- yes:
	default: // already answered; the second key is not a second answer
	}
	return true
}

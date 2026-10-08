package live

import (
	"errors"
	"io"
	"sync"
)

// Local is the operator's own seat in their room.
//
// The host has always been in the room without being connected to it — they
// are at the pane — which is why nobody could see them typing, and they could
// not see the room. This gives them a seat like anybody's: it hears the same
// records a joiner does, and speaks through the same room, with no socket and
// no stream, because the pane is already in front of them.
//
// It has the shape of a joiner's client on purpose, so the same terminal UI
// can drive either.
type Local struct {
	srv  *Server
	st   *seat
	in   *io.PipeReader
	once sync.Once
	// pumped is shut when the goroutine feeding in has stopped, so Close can
	// wait for it rather than leave it behind.
	pumped chan struct{}
}

// ErrHostCannotSteer is what Say answers: the host steers from the agent's own
// pane, and this seat only talks to the room.
var ErrHostCannotSteer = errors.New("live: the host steers from the agent's own pane; this line talks to the room")

// Attach seats the operator in the room, as Server.Host.
//
// The room is told, so joiners see the host's typing and chat like anyone's.
// Call Close to leave; the server never closes it for you.
func (s *Server) Attach() *Local {
	login := s.Host
	if login == "" {
		login = "host"
	}
	st := newSeat(login)
	pr, pw := io.Pipe()
	l := &Local{srv: s, st: st, in: pr, pumped: make(chan struct{})}

	// The seat's queue becomes a stream of records, so Run is the same loop a
	// joiner runs over a socket rather than a second copy of it.
	go func() {
		defer close(l.pumped)
		defer pw.Close()
		for {
			select {
			case line := <-st.out:
				if _, err := pw.Write(line); err != nil {
					return
				}
			case <-st.closed:
				return
			}
		}
	}()

	s.room().join(st)
	return l
}

// Run delivers the room's records to e until the seat is closed or the server
// is gone. A seat that fell too far behind is ended like a joiner's, and Run
// returns so the caller can attach again.
func (l *Local) Run(e Events) error { return walk(l.in, e) }

// Chat talks to the room as the host. It never reaches the agent.
func (l *Local) Chat(text string) error {
	l.srv.chat(l.st.login, text)
	return nil
}

// Typing tells the room the host is composing, or has stopped.
func (l *Local) Typing(on, toRoom bool) error {
	l.srv.room().setTyping(l.st, on, toRoom)
	return nil
}

// Say is refused. The host steers the agent from the agent's own pane: a
// second way in would put their words on the pull request as a joiner's, and
// nothing is ever typed into the pane from here.
func (l *Local) Say(string) error {
	return ErrHostCannotSteer
}

// Resize is a no-op: there is no stream to repaint.
func (l *Local) Resize(int, int) error { return nil }

// Close takes the seat out of the room, and returns once nothing of it is
// left running.
func (l *Local) Close() error {
	l.once.Do(func() {
		l.srv.room().leave(l.st)
		l.st.close()
		_ = l.in.Close()
	})
	<-l.pumped
	return nil
}

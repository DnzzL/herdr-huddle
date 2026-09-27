package live

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/DnzzL/herdr-huddle/internal/thread"
)

// maxRecordBytes is the ceiling on one stream record. A full paint of a wide
// pane is the largest thing Herdr sends and it is large; the cap exists so a
// runaway producer is a failure rather than unbounded memory.
const maxRecordBytes = 8 << 20

// Ledger posts the record of a delivered instruction to the thread. It may
// fail; the queue retries it, because delivery never waits for the record.
type Ledger interface {
	Post(ctx context.Context, in thread.LiveInstruction) error
}

// Observer starts a live stream for one pane. It is an interface so the server
// can be exercised without a Herdr session.
type Observer interface {
	Observe(ctx context.Context, pane string, cols, rows int) (Stream, error)
}

// Stream is a running terminal stream: records on Read, and a process that
// ends.
//
// Wait reports why it ended, and is only meaningful once Read has returned an
// error or EOF.
type Stream interface {
	io.ReadCloser
	Wait() error
}

// Server serves one pane's live output to whoever the gate lets in.
//
// Each joiner gets its own Observer stream. That is deliberate and it is what
// makes the second joiner correct: Herdr's first frame is a complete paint, and
// only a fresh stream has one. Fanning a single stream out would leave every
// late joiner with a viewport painted from some intermediate diff — and it is
// also what lets every joiner be rendered at their own terminal's size
// (ADR-007), which costs nothing once the stream is already per-joiner.
//
// There is no queue and no fan-out here, so a slow joiner costs its own
// observer and nothing else — and when it goes away, that observer goes with
// it.
type Server struct {
	// Pane is the Herdr pane whose output is served. Required.
	Pane string
	// Cols and Rows are the viewport a joiner gets when it asks for none.
	// A joiner that reports its own window size is served at that size
	// instead, clamped (ADR-007).
	Cols int
	Rows int
	// Observe starts a stream. Required.
	Observe Observer
	// Gate decides who may join. Required: a server without one would be a
	// public pane, and ADR-006 makes this check the entire boundary for a
	// public endpoint.
	Gate *Gate
	// GateTimeout bounds how long a joiner has to present their hello before
	// being dropped. Zero means DefaultGateTimeout.
	GateTimeout time.Duration
	// Instructor delivers a joiner's words to the agent. Required: without it
	// a say would vanish silently.
	Instructor Instructor
	// Ledger records delivered instructions on the thread. Required. A failing
	// ledger is queued and flushed — delivery never waits for it (ADR-005).
	Ledger Ledger
	// ThreadURL is the pull request the share is bound to, quoted back to the
	// agent so it knows where the message came from, and carried to every
	// joiner so the room can find its own record (ADR-007).
	ThreadURL string
	// Host is the operator's login — the person at the pane. Optional, and
	// omitted when the token cannot say who they are; without it a joiner
	// alone in the room is told they are alone, which is false while the
	// operator is watching (ADR-007).
	Host string
	// Moderate decides whether a joiner's instruction reaches the agent.
	// Optional: without it, anyone the gate admitted steers directly, which
	// is the default. With it, admitting somebody to the room and letting
	// them drive the agent stop being the same decision.
	Moderate Moderator
	// Status reports what the agent is doing, for the room. Optional: without
	// it the room simply never says.
	Status Statuser
	// StatusInterval is how often that is re-read while anyone is connected.
	// Zero means DefaultStatusInterval.
	StatusInterval time.Duration
	// LedgerInterval is how often a queued record is retried. Zero means
	// DefaultLedgerInterval.
	LedgerInterval time.Duration
	// LedgerTimeout bounds one post attempt. Zero means
	// DefaultLedgerTimeout. It exists because the GitHub client sets no
	// timeout of its own: an unanswered post must become a queued record, not
	// a goroutine holding a record that is neither posted nor queued.
	LedgerTimeout time.Duration
	// Spool is where records the thread would not take are written down, so a
	// restart owes what this process owed. Optional, and a server without one
	// loses its queue on exit — which is what this used to do to everybody.
	Spool Spool

	ledgerMu sync.Mutex
	pending  []thread.LiveInstruction

	roomOnce sync.Once
	huddle   *room

	// Log receives one line per connection event.
	Log func(format string, args ...any)
}

// DefaultGateTimeout is how long a joiner gets to say who they are.
const DefaultGateTimeout = 15 * time.Second

// room returns the shared presence state, built once.
func (s *Server) room() *room {
	s.roomOnce.Do(func() { s.huddle = &room{thread: s.ThreadURL, host: s.Host} })
	return s.huddle
}

// Serve accepts connections until ctx is cancelled or the listener fails.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	if err := s.serveGuard(); err != nil {
		return err
	}

	// The frames ride one WebSocket each: ADR-006 measured that a quick tunnel
	// holds an ordinary chunked response until it ends, so HTTP streaming
	// cannot carry a live pane, while WebSocket frames arrive as they are
	// written. Everything below the upgrade — the per-joiner observer, the
	// gate, the records themselves — is unchanged by that.
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			// Accept has already answered the handshake; a plain GET gets an
			// HTTP error here and never reaches the gate or the pane.
			return
		}
		// Bound to the server's context so a retired share or a Ctrl-C ends
		// every live joiner, not just future ones.
		connCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		conn := websocket.NetConn(connCtx, ws, websocket.MessageBinary)
		s.serve(connCtx, conn)
	})

	hs := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	// A cancelled context must end the accept loop, which is how a Ctrl-C or a
	// retired share stops the port being held.
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	s.recover()

	go s.flushLoop(ctx)
	go s.room().watchAgent(ctx, s.Pane, s.Status, s.StatusInterval)

	if err := hs.Serve(ln); err != nil && ctx.Err() == nil {
		return fmt.Errorf("live: serve: %w", err)
	}
	if n := s.queuedCount(); n > 0 {
		if s.Spool == nil {
			// Nothing was written down, so this is the only notice anyone gets.
			s.logf("shutting down with %d instruction record(s) never posted to the thread", n)
		} else {
			s.logf("shutting down owing the thread %d instruction record(s); they are written down and will be retried next time", n)
		}
	}
	return nil
}

// recover picks up what a previous run still owed the thread.
//
// The records are prepended, oldest first: a restart owes its predecessor's
// debts before anything it takes on itself, and the thread reads in the order
// things were said.
func (s *Server) recover() {
	if s.Spool == nil {
		return
	}
	owed, err := s.Spool.Load()
	if err != nil {
		s.logf("could not read the instruction records a previous run owed: %v", err)
		return
	}
	if len(owed) == 0 {
		return
	}
	s.ledgerMu.Lock()
	s.pending = append(owed, s.pending...)
	s.ledgerMu.Unlock()
	s.logf("carrying over %d instruction record(s) a previous run never posted", len(owed))
}

// DefaultLedgerInterval is how often a queued record is retried.
const DefaultLedgerInterval = 10 * time.Second

// DefaultLedgerTimeout bounds one post attempt.
const DefaultLedgerTimeout = 30 * time.Second

// flushLoop retries queued records until the server stops. An idle pass costs
// a length check, the same bargain the poller makes (ADR-001).
func (s *Server) flushLoop(ctx context.Context) {
	interval := s.LedgerInterval
	if interval <= 0 {
		interval = DefaultLedgerInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.flush(ctx)
		}
	}
}

// maxQueuedRecords bounds what a broken ledger may hold.
const maxQueuedRecords = 100

// flush posts what is queued, keeping whatever still fails.
func (s *Server) flush(ctx context.Context) {
	s.ledgerMu.Lock()
	if len(s.pending) == 0 {
		s.ledgerMu.Unlock()
		return
	}
	batch := s.pending
	s.pending = nil
	s.ledgerMu.Unlock()

	var keep []thread.LiveInstruction
	for _, in := range batch {
		postCtx, cancel := s.ledgerCtx(ctx)
		err := s.Ledger.Post(postCtx, in)
		cancel()
		if err != nil {
			keep = append(keep, in)
			s.logf("still could not record @%s's instruction: %v", in.Author, err)
		}
	}
	s.ledgerMu.Lock()
	// Oldest first, including whatever arrived meanwhile; the cap keeps a
	// permanently broken ledger from growing without bound.
	s.pending = append(keep, s.pending...)
	if overflow := len(s.pending) - maxQueuedRecords; overflow > 0 {
		s.pending = s.pending[overflow:]
		s.logf("dropped %d oldest instruction record(s): the ledger has been failing for too long", overflow)
	}
	// Written down whether the queue grew or emptied: a posted record must
	// stop being owed, or a restart posts it to the thread a second time.
	s.writeDownLocked()
	s.ledgerMu.Unlock()
}

// writeDownLocked persists the queue. The caller holds ledgerMu.
func (s *Server) writeDownLocked() {
	if s.Spool == nil {
		return
	}
	if err := s.Spool.Save(s.pending); err != nil {
		s.logf("could not write down the %d instruction record(s) still owed: %v", len(s.pending), err)
	}
}

// ledgerCtx bounds one ledger attempt without shortening the connection's
// own lifetime. The caller cancels it.
func (s *Server) ledgerCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := s.LedgerTimeout
	if timeout <= 0 {
		timeout = DefaultLedgerTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

func (s *Server) queuedCount() int {
	s.ledgerMu.Lock()
	defer s.ledgerMu.Unlock()
	return len(s.pending)
}

// serveGuard is everything that must be true before a connection is accepted.
// It is separate from Serve so the requirements can be asserted without a
// listener, and so a missing gate reads as a startup failure rather than an
// open port.
func (s *Server) serveGuard() error {
	if s.Pane == "" {
		return errors.New("live: a pane is required to serve a stream")
	}
	if s.Observe == nil {
		return errors.New("live: an observer is required to serve a stream")
	}
	if s.Gate == nil {
		return errors.New("live: a gate is required to serve a stream")
	}
	// An empty allowlist used to be the end of it. It still is when the gate
	// has no way to say yes to anyone new — but a knocking or open gate makes
	// an empty list an ordinary starting point (ADR-007).
	if len(s.Gate.Allowlist) == 0 && !s.Gate.canAdmit() {
		return errors.New("live: the share's allowlist is empty and the door is closed, so nobody could join: run `share --invite`, or serve with --knock or --open")
	}
	if s.Instructor == nil {
		return errors.New("live: an instructor is required, or a joiner's instructions would vanish silently")
	}
	if s.Ledger == nil {
		return errors.New("live: a ledger is required, or delivered instructions would never reach the record")
	}
	return nil
}

// authorize reads the joiner's hello and checks it, before anything is
// observed. A refused joiner costs no child process and sees no frames.
//
// The wait is bounded by a select rather than by SetReadDeadline: a hit
// deadline closes the WebSocket abruptly, which would leave the joiner with a
// bare EOF instead of the reason it was dropped.
//
// The buffered reader is handed back rather than discarded: it holds whatever
// the joiner sent after the hello line, which is where instructions and
// resizes arrive. On the timeout path the reading goroutine is abandoned and
// unblocks when the connection closes below. The hello itself is handed back
// too, because it carries the size to render this joiner's stream at.
func (s *Server) authorize(ctx context.Context, conn net.Conn) (string, *bufio.Reader, Frame, error) {
	timeout := s.GateTimeout
	if timeout <= 0 {
		timeout = DefaultGateTimeout
	}
	reader := bufio.NewReader(conn)
	type helloLine struct {
		line     []byte
		isPrefix bool
		err      error
	}
	got := make(chan helloLine, 1)
	go func() {
		line, isPrefix, err := reader.ReadLine()
		got <- helloLine{line: line, isPrefix: isPrefix, err: err}
	}()

	var line helloLine
	select {
	case line = <-got:
	case <-ctx.Done():
		return "", nil, Frame{}, ctx.Err()
	case <-time.After(timeout):
		return "", nil, Frame{}, fmt.Errorf("live: no hello from the joiner within %s", timeout)
	}
	if line.err != nil {
		return "", nil, Frame{}, fmt.Errorf("live: no hello from the joiner: %w", line.err)
	}
	if line.isPrefix {
		// A hello is a type, a token and a size; anything longer is not one,
		// and refusing it beats buffering it.
		return "", nil, Frame{}, errors.New("live: hello record is too long")
	}
	hello, err := ParseFrame(line.line)
	if err != nil {
		// Parse errors quote the record they failed on — which here may be a
		// token in the clear. The generic message is the whole explanation
		// this path gets: only our own client sends hellos, and a malformed
		// one is an attack or a bug, not something to describe back.
		return "", nil, Frame{}, errors.New("live: the first record was not a valid hello")
	}
	if hello.Type != TypeHello {
		return "", nil, Frame{}, fmt.Errorf("live: first record was %s, want a hello", oneLine([]byte(hello.Type)))
	}
	login, err := s.Gate.Authorize(ctx, hello.Token)
	if err != nil {
		return "", nil, Frame{}, err
	}
	// The token has done its work. Blanking it means no later code path can
	// log, echo or keep it by accident.
	hello.Token = ""
	return login, reader, hello, nil
}

// serve runs one joiner's stream for as long as the joiner is there.
func (s *Server) serve(ctx context.Context, conn net.Conn) {
	ctx, cancel := context.WithCancel(ctx)
	// Order matters and is easy to get backwards: cancelling this context
	// closes the WebSocket *abruptly*, so the clean close handshake in
	// conn.Close() must run first. Reversed, the joiner sees a raw TCP EOF
	// where it expects a close frame and reports it as a read failure.
	defer cancel()
	defer conn.Close()

	login, reader, hello, err := s.authorize(ctx, conn)
	if err != nil {
		// Only a person the gate turned away is told not to come back. A
		// reconnect loop that redials a closed door knocks forever — but one
		// that gives up because GitHub was briefly unreachable ends a huddle
		// over a blip, which is the worse failure of the two.
		if NotAllowed(err) {
			_, _ = conn.Write(FatalRecord(err))
		} else {
			s.fail(conn, err)
		}
		s.logf("joiner %s: %v", conn.RemoteAddr(), err)
		return
	}

	// From here every write to conn goes through the seat, because three
	// goroutines want to write to it — the frames, the delivery
	// acknowledgement and the room broadcast — and a socket takes one writer.
	st := newSeat(login)
	defer st.close()
	drained := s.writeSeat(ctx, st, conn)

	view := &viewport{}
	view.set(clampViewport(hello.Width, hello.Height, s.Cols, s.Rows))
	s.logf("joiner %s joined as @%s at %dx%d", conn.RemoteAddr(), login, view.cols, view.rows)

	huddle := s.room()
	huddle.join(st)
	defer huddle.leave(st)

	// This is the reader for everything a joiner says, and the watcher that
	// ends their stream when they leave: a read result is either an
	// instruction, a resize or a departure, and a departure must take the
	// observe child with it — one leaked child per departed joiner, forever,
	// otherwise — which the cancelled context below prevents.
	go func() {
		defer cancel()
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 0, 64*1024), maxRecordBytes)
		for scanner.Scan() {
			record, err := ParseFrame(scanner.Bytes())
			if err != nil {
				// Never echo: a record can carry instruction text, and this
				// goes to a log.
				s.logf("joiner %s: ignoring a record it could not parse", conn.RemoteAddr())
				continue
			}
			switch record.Type {
			case TypeSay:
				if s.Moderate == nil {
					// In order, on this goroutine: two instructions from one
					// person must reach the agent in the order they were
					// typed.
					s.speak(ctx, st, login, record.Text)
					break
				}
				// Moderated, so the answer is a person's and may take a
				// minute. Off this goroutine, or a joiner waiting for approval
				// could not even chat to ask what the hold-up is. Ordering
				// then belongs to the operator, which is the point of
				// moderating.
				go s.speak(ctx, st, login, record.Text)
			case TypeChat:
				s.chat(login, record.Text)
			case TypeResize:
				view.set(clampViewport(record.Width, record.Height, s.Cols, s.Rows))
			case TypeTyping:
				// A claim about oneself only: the seat is the subject, never
				// anything the record names.
				huddle.setTyping(st, record.On)
			}
		}
	}()

	s.stream(ctx, st, view)

	// The joiner is owed the records already queued — the closing one above
	// most of all. Waiting for the writer to drain is what turns "the stream
	// ended" into something the joiner is told rather than something it has
	// to infer from a socket closing.
	st.close()
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
	case <-ctx.Done():
	}
}

// writeSeat is the one goroutine allowed to write to this connection. The
// returned channel is shut once it has stopped.
func (s *Server) writeSeat(ctx context.Context, st *seat, conn net.Conn) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case line := <-st.out:
				if _, err := conn.Write(line); err != nil {
					// The joiner left mid-write; closing the seat is what
					// stops everything else queueing for them.
					st.close()
					return
				}
			case <-st.closed:
				// Drain whatever is already queued, then stop. The closing
				// record is usually the last thing in here.
				for {
					select {
					case line := <-st.out:
						if _, err := conn.Write(line); err != nil {
							return
						}
					default:
						return
					}
				}
			}
		}
	}()
	return done
}

// viewport is the size this joiner's stream is rendered at, and the lever that
// restarts it when that changes.
//
// A restart is the only mechanism available: `herdr terminal session observe`
// takes no resize — that is the property ADR-005 relies on for read-only by
// construction — and a restart is also exactly what a resized terminal wants,
// because Herdr's first frame is a complete paint (ADR-007).
type viewport struct {
	mu         sync.Mutex
	cols, rows int
	dirty      bool
	cancel     context.CancelFunc
}

func (v *viewport) set(cols, rows int) {
	v.mu.Lock()
	if cols == v.cols && rows == v.rows {
		v.mu.Unlock()
		return
	}
	v.cols, v.rows = cols, rows
	v.dirty = true
	cancel := v.cancel
	v.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// arm records the cancel that a resize will pull, and reports the size the
// next stream should open at.
func (v *viewport) arm(cancel context.CancelFunc) (int, int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.cancel = cancel
	v.dirty = false
	return v.cols, v.rows
}

// resized reports whether the stream that just ended was ended by a resize,
// and disarms.
func (v *viewport) resized() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.cancel = nil
	was := v.dirty
	v.dirty = false
	return was
}

// stream observes the pane and forwards its records to the seat, restarting
// silently whenever the joiner's window changes size.
func (s *Server) stream(ctx context.Context, st *seat, view *viewport) {
	for {
		obsCtx, cancel := context.WithCancel(ctx)
		cols, rows := view.arm(cancel)
		ended, err := s.pump(obsCtx, st, cols, rows)
		cancel()

		if ctx.Err() != nil {
			return
		}
		if view.resized() {
			// Not an ending: the joiner's terminal changed size and the next
			// stream opens with a complete paint at the new one.
			continue
		}
		if err != nil {
			s.failSeat(st, err)
			s.logf("joiner @%s: %v", st.login, err)
			return
		}
		if !ended {
			// Herdr normally sends terminal.closed itself; when the stream
			// simply ends, say so as a record. The verdict has to reach the
			// joiner as data, because how the connection is torn down
			// afterwards is not ours to guarantee — and a joiner that must
			// infer success from a clean close is one race away from
			// reporting a good stream as a failed one.
			st.send(ErrorRecordClosed())
		}
		return
	}
}

// pump runs one observe at one size. It reports whether the stream announced
// its own end.
func (s *Server) pump(ctx context.Context, st *seat, cols, rows int) (bool, error) {
	stream, err := s.Observe.Observe(ctx, s.Pane, cols, rows)
	if err != nil {
		return false, err
	}
	defer stream.Close()

	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 0, 64*1024), maxRecordBytes)
	for scanner.Scan() {
		line := scanner.Bytes()
		frame, err := ParseFrame(line)
		if err != nil {
			// The joiner is told, rather than left with a viewport that will
			// never be repainted correctly.
			return false, err
		}
		if !st.send(append(append([]byte(nil), line...), '\n')) {
			// The joiner left, or fell too far behind to catch up; either way
			// the deferred Close ends their observer.
			return true, nil
		}
		if frame.Type == TypeClosed {
			// Worth a line on the operator's side too: a joiner that gets a
			// blank screen is otherwise unexplained.
			if frame.Reason != "" {
				s.logf("joiner @%s: %s", st.login, frame.Reason)
			}
			return true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return false, fmt.Errorf("live: read stream: %w", err)
	}
	return false, stream.Wait()
}

// speak delivers one instruction and reports the outcome to the room.
//
// Order is the whole design: the words go to the agent first and the record
// follows, so neither the ledger's availability nor its round trip stands
// between a person and the agent (ADR-005). The record is written only for
// instructions that were delivered — a comment claiming the agent received
// something it refused would poison the trace.
//
// The outcome goes to everyone, not only to whoever typed it (ADR-007): two
// people steering one agent need to see each other do it, and "held: the agent
// is waiting on its operator" is news for the room, not for one person.
func (s *Server) speak(ctx context.Context, st *seat, login, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		st.sendFrame(Frame{Type: TypeSaid, Status: StatusFailed, Reason: "the message was empty", Author: login})
		return
	}
	// Sending ends the sentence: leaving the claim standing would show the
	// author as still typing what they have already sent.
	s.room().setTyping(st, false)

	if s.Moderate != nil {
		// The room is told it is waiting, not that it failed: the answer is
		// coming, from a person, and silence in the meantime reads as a
		// dropped instruction.
		s.room().broadcast(Frame{Type: TypeSaid, Status: StatusQueued, Author: login, Text: text})
		ok, err := s.Moderate.Approve(ctx, login, text)
		switch {
		case err != nil:
			s.logf("could not put @%s's instruction to the operator: %v", login, err)
			s.room().broadcast(Frame{Type: TypeSaid, Status: StatusFailed,
				Reason: "nobody could be asked to approve it", Author: login, Text: text})
			return
		case !ok:
			s.logf("operator refused @%s's instruction", login)
			s.room().broadcast(Frame{Type: TypeSaid, Status: StatusRefused,
				Reason: "the operator did not send it", Author: login, Text: text})
			return
		}
	}

	instruction := thread.Instruction{Author: login, Text: text, URL: s.ThreadURL}
	if err := s.Instructor.Deliver(ctx, s.Pane, instruction.Prompt()); err != nil {
		status, reason := deliveryFailure(err)
		s.logf("instruction from @%s not delivered: %v", login, err)
		s.room().broadcast(Frame{Type: TypeSaid, Status: status, Reason: reason, Author: login, Text: text})
		return
	}
	s.room().broadcast(Frame{Type: TypeSaid, Status: StatusSent, Author: login, Text: text})

	record := thread.LiveInstruction{Author: login, Text: text, At: time.Now()}
	// Off this goroutine on purpose: "delivery never waits for the record"
	// (ADR-005) has to hold for latency as well as for failure. A ledger call
	// taking its time must not delay the *next* instruction's delivery, and
	// the only reader of this connection is the loop that delivers.
	go func() {
		postCtx, cancel := s.ledgerCtx(ctx)
		defer cancel()
		if err := s.Ledger.Post(postCtx, record); err != nil {
			s.ledgerMu.Lock()
			s.pending = append(s.pending, record)
			s.writeDownLocked()
			s.ledgerMu.Unlock()
			s.logf("could not record @%s's instruction yet: %v (queued)", login, err)
		}
	}()
}

// chat carries a message to the people in the room and to nobody else.
//
// It is the primitive the huddle was missing: every line a joiner typed went
// to the agent, so two collaborators could not say "wait, don't do that" to
// each other without the agent doing something about it. Nothing here touches
// the Instructor, and nothing here reaches the thread — this is side-talk, and
// a pull request full of "one sec" is not a record of anything (ADR-007).
func (s *Server) chat(login, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	// Cut on runes: slicing bytes lands mid-character and puts a replacement
	// glyph on every screen in the room.
	if runes := []rune(text); len(runes) > maxChatRunes {
		text = string(runes[:maxChatRunes])
	}
	s.room().broadcast(Frame{Type: TypeChat, Author: login, Text: text})
}

// maxChatRunes bounds one message. The room is four rows; anything longer is
// not a message, it is a paste.
const maxChatRunes = 2000

// failSeat tells a joiner why the stream stopped, through their own writer.
func (s *Server) failSeat(st *seat, err error) { st.send(ErrorRecord(err)) }

// fail tells a refused joiner why, before any seat exists. A failure to write
// it means the joiner is already gone, which is not worth reporting on top of
// the reason.
func (s *Server) fail(conn net.Conn, err error) {
	_, _ = conn.Write(ErrorRecord(err))
}

func (s *Server) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log(format, args...)
	}
}

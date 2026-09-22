package live

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

// maxRecordBytes is the ceiling on one stream record. A full paint of a wide
// pane is the largest thing Herdr sends and it is large; the cap exists so a
// runaway producer is a failure rather than unbounded memory.
const maxRecordBytes = 8 << 20

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

// Server serves one pane's live output to whoever connects.
//
// Each joiner gets its own Observer stream. That is deliberate and it is what
// makes the second joiner correct: Herdr's first frame is a complete paint, and
// only a fresh stream has one. Fanning a single stream out would leave every
// late joiner with a viewport painted from some intermediate diff.
//
// There is no queue and no fan-out here, so a slow joiner costs its own
// observer and nothing else — and when it goes away, that observer goes with
// it.
type Server struct {
	// Pane is the Herdr pane whose output is served. Required.
	Pane string
	// Cols and Rows are the viewport the stream is rendered at. The server
	// decides them, because it owns the observer; a joiner's window size is an
	// upgrade for later, not a phase-1 concern.
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
	// Log receives one line per connection event.
	Log func(format string, args ...any)
}

// DefaultGateTimeout is how long a joiner gets to say who they are.
const DefaultGateTimeout = 15 * time.Second

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

	if err := hs.Serve(ln); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("live: serve: %w", err)
	}
	return nil
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
	if len(s.Gate.Allowlist) == 0 {
		return errors.New("live: the share's allowlist is empty, so nobody could join: run `share` first")
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
// the joiner sent after the hello line, and phase 4 will read instructions
// from it. On the timeout path the reading goroutine is abandoned and unblocks
// when the connection closes below.
func (s *Server) authorize(ctx context.Context, conn net.Conn) (string, *bufio.Reader, error) {
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
		return "", nil, ctx.Err()
	case <-time.After(timeout):
		return "", nil, fmt.Errorf("live: no hello from the joiner within %s", timeout)
	}
	if line.err != nil {
		return "", nil, fmt.Errorf("live: no hello from the joiner: %w", line.err)
	}
	if line.isPrefix {
		// A hello is a type and a token; anything longer is not one, and
		// refusing it beats buffering it.
		return "", nil, errors.New("live: hello record is too long")
	}
	hello, err := ParseFrame(line.line)
	if err != nil {
		// Parse errors quote the record they failed on — which here may be a
		// token in the clear. The generic message is the whole explanation
		// this path gets: only our own client sends hellos, and a malformed
		// one is an attack or a bug, not something to describe back.
		return "", nil, errors.New("live: the first record was not a valid hello")
	}
	if hello.Type != TypeHello {
		return "", nil, fmt.Errorf("live: first record was %s, want a hello", oneLine([]byte(hello.Type)))
	}
	login, err := s.Gate.Authorize(ctx, hello.Token)
	if err != nil {
		return "", nil, err
	}
	return login, reader, nil
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

	login, reader, err := s.authorize(ctx, conn)
	if err != nil {
		s.fail(conn, err)
		s.logf("joiner %s: %v", conn.RemoteAddr(), err)
		return
	}
	s.logf("joiner %s joined as @%s", conn.RemoteAddr(), login)

	stream, err := s.Observe.Observe(ctx, s.Pane, s.Cols, s.Rows)
	if err != nil {
		s.fail(conn, err)
		return
	}
	defer stream.Close()

	// A joiner sends nothing in this direction after the hello in phase 2, so
	// any read result means it is gone. Watching for that is what stops a
	// disconnect leaking one `herdr terminal session observe` child per
	// joiner, forever — the cancelled context is what kills it. When
	// instructions arrive from a joiner, this becomes the reader for them
	// rather than a discard.
	go func() {
		_, _ = io.Copy(io.Discard, reader)
		cancel()
	}()

	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 0, 64*1024), maxRecordBytes)
	sawClosed := false
	for scanner.Scan() {
		line := scanner.Bytes()
		frame, err := ParseFrame(line)
		if err != nil {
			// The joiner is told, rather than left with a viewport that will
			// never be repainted correctly.
			s.fail(conn, err)
			s.logf("joiner %s: %v", conn.RemoteAddr(), err)
			return
		}
		if _, err := conn.Write(append(append([]byte(nil), line...), '\n')); err != nil {
			// The joiner left mid-write; the deferred Close ends its observer.
			return
		}
		if frame.Type == TypeClosed {
			// Worth a line on the operator's side too: a joiner that gets a
			// blank screen is otherwise unexplained.
			if frame.Reason != "" {
				s.logf("joiner %s: %s", conn.RemoteAddr(), frame.Reason)
			}
			sawClosed = true
			return
		}
	}
	if err := scanner.Err(); err != nil {
		s.fail(conn, fmt.Errorf("live: read stream: %w", err))
		s.logf("joiner %s: %v", conn.RemoteAddr(), err)
		return
	}
	if err := stream.Wait(); err != nil {
		s.fail(conn, err)
		s.logf("joiner %s: %v", conn.RemoteAddr(), err)
		return
	}
	// Herdr normally sends terminal.closed itself; when the stream simply
	// ends, say so as a record. The verdict has to reach the joiner as data,
	// because how the connection is torn down afterwards is not ours to
	// guarantee — and a joiner that must infer success from a clean close is
	// one race away from reporting a good stream as a failed one.
	if !sawClosed {
		if _, err := conn.Write(ErrorRecordClosed()); err != nil {
			s.logf("joiner %s: could not report the end of the stream: %v", conn.RemoteAddr(), err)
		}
	}
}

// fail tells a joiner why the stream stopped. A failure to write it means the
// joiner is already gone, which is not worth reporting on top of the reason.
func (s *Server) fail(conn net.Conn, err error) {
	_, _ = conn.Write(ErrorRecord(err))
}

func (s *Server) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log(format, args...)
	}
}

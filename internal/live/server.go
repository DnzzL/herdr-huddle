package live

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
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
	// Log receives one line per connection event.
	Log func(format string, args ...any)
}

// Serve accepts connections until ctx is cancelled or the listener fails.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	if s.Pane == "" {
		return errors.New("live: a pane is required to serve a stream")
	}
	if s.Observe == nil {
		return errors.New("live: an observer is required to serve a stream")
	}

	// A cancelled context must end the accept loop, which is how a Ctrl-C or a
	// retired share stops the port being held.
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("live: accept: %w", err)
		}
		go s.serve(ctx, conn)
	}
}

// serve runs one joiner's stream for as long as the joiner is there.
func (s *Server) serve(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := s.Observe.Observe(ctx, s.Pane, s.Cols, s.Rows)
	if err != nil {
		s.fail(conn, err)
		return
	}
	defer stream.Close()

	// A joiner sends nothing in this direction in phase 1, so any read result
	// means it is gone. Watching for that is what stops a disconnect leaking
	// one `herdr terminal session observe` child per joiner, forever — the
	// cancelled context is what kills it. When instructions arrive from a
	// joiner, this becomes the reader for them rather than a discard.
	go func() {
		_, _ = io.Copy(io.Discard, conn)
		cancel()
	}()

	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 0, 64*1024), maxRecordBytes)
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

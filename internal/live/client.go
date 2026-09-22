package live

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"

	"github.com/coder/websocket"
)

// walk reads records until a terminal one, dispatching each.
//
// One loop for both directions of the client keeps frame drawing and delivery
// feedback honest against the same protocol: an error record ends the stream,
// a said record is reported without ending it, and a record type from a newer
// Herdr is skipped rather than mistaken for the end.
func walk(r io.Reader, onFrame func([]byte) error, onSaid func(Frame)) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxRecordBytes)
	for scanner.Scan() {
		frame, err := ParseFrame(scanner.Bytes())
		if err != nil {
			return err
		}
		if !frame.Known() {
			// A record type this build does not know: skipping keeps the pane
			// rendering; there is nothing else sensible to do with it.
			continue
		}
		switch frame.Type {
		case TypeFrame:
			data, err := frame.Data()
			if err != nil {
				return err
			}
			if err := onFrame(data); err != nil {
				return fmt.Errorf("live: draw frame: %w", err)
			}
		case TypeError:
			if frame.Message == "" {
				return errors.New("live: the stream failed")
			}
			return fmt.Errorf("live: the stream failed: %s", frame.Message)
		case TypeClosed:
			// A closed record can carry a reason — a pane that no longer exists
			// is reported this way, with a clean exit — so it is only an
			// ordinary ending when there is nothing to explain.
			if frame.Reason != "" {
				return fmt.Errorf("live: the stream closed: %s", frame.Reason)
			}
			return nil
		case TypeSaid:
			if onSaid != nil {
				onSaid(frame)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("live: read stream: %w", err)
	}
	return nil
}

// Render draws a stream onto a terminal, and returns when the stream ends.
//
// It writes the frame payloads and nothing else: the stream is ANSI, the
// terminal is a terminal, and anything added here would be drawn into the
// viewport the frame is trying to paint.
func Render(stream io.Reader, out io.Writer) error {
	return walk(stream, func(data []byte) error {
		if _, err := out.Write(data); err != nil {
			return err
		}
		return nil
	}, nil)
}

// Session is a joiner's side of a live share: frames out, instructions in.
type Session struct {
	conn   net.Conn
	reader *bufio.Reader
}

// Dial connects, presents the token as the first record, and returns once the
// server's hello read has had its chance to refuse.
//
// Presenting before reading is the whole protocol: the server observes
// nothing until the gate has approved, so there is no window in which frames
// are sent to someone unapproved (ADR-006).
func Dial(ctx context.Context, endpoint, token string) (*Session, error) {
	u, err := wsEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	// The token is the gate's whole credential: over anything but TLS or
	// loopback it would travel in the clear, which is the one thing ADR-006
	// forbids of it.
	if err := tokenTravelsSafely(u); err != nil {
		return nil, err
	}
	conn, _, err := websocket.Dial(ctx, u, nil)
	if err != nil {
		return nil, fmt.Errorf("live: could not reach a live share at %s: %w", u, err)
	}
	stream := websocket.NetConn(ctx, conn, websocket.MessageBinary)

	hello, err := json.Marshal(Frame{Type: TypeHello, Token: token})
	if err != nil {
		_ = stream.Close()
		return nil, fmt.Errorf("live: build hello: %w", err)
	}
	if _, err := stream.Write(append(hello, '\n')); err != nil {
		_ = stream.Close()
		return nil, fmt.Errorf("live: could not present the token: %w", err)
	}
	return &Session{conn: stream, reader: bufio.NewReader(stream)}, nil
}

// Say sends one instruction. It does not wait for the outcome: Draw reports
// that, on the same connection.
func (s *Session) Say(text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("live: nothing to send")
	}
	line, err := json.Marshal(Frame{Type: TypeSay, Text: text})
	if err != nil {
		return fmt.Errorf("live: build instruction: %w", err) // cannot fail
	}
	if _, err := s.conn.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("live: could not send the instruction: %w", err)
	}
	return nil
}

// Draw renders frames to out and delivery outcomes to feedback, until the
// stream ends. Both writers may be the same stream; out gets ANSI and feedback
// gets plain lines, so typically stdout and stderr.
func (s *Session) Draw(out, feedback io.Writer) error {
	return walk(s.reader, func(data []byte) error {
		if _, err := out.Write(data); err != nil {
			return err
		}
		return nil
	}, func(frame Frame) {
		if feedback == nil {
			return
		}
		fmt.Fprintln(feedback, "herdr-huddle: "+saidLine(frame))
	})
}

// Close ends the session.
func (s *Session) Close() error {
	return s.conn.Close()
}

// saidLine words a delivery outcome for the person who typed it.
func saidLine(f Frame) string {
	switch f.Status {
	case StatusSent:
		return "delivered to the agent"
	case StatusHeld:
		return "held: " + f.Reason
	case StatusFailed:
		return "not delivered: " + f.Reason
	default:
		if f.Reason != "" {
			return f.Status + ": " + f.Reason
		}
		return f.Status
	}
}

// Join connects, draws the stream until it ends, and closes.
func Join(ctx context.Context, endpoint, token string, out io.Writer) error {
	session, err := Dial(ctx, endpoint, token)
	if err != nil {
		return err
	}
	defer session.Close()
	return session.Draw(out, nil)
}

// wsEndpoint turns whatever `serve` printed into a dialable WebSocket URL:
// the tunnel prints https://, a LAN address may come bare, and tests pass
// ws:// directly. The scheme is inferred rather than typed.
func wsEndpoint(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("live: no address to join")
	}
	switch {
	case strings.HasPrefix(s, "ws://"), strings.HasPrefix(s, "wss://"):
		return s, nil
	case strings.HasPrefix(s, "http://"):
		return "ws://" + strings.TrimPrefix(s, "http://"), nil
	case strings.HasPrefix(s, "https://"):
		return "wss://" + strings.TrimPrefix(s, "https://"), nil
	default:
		return "ws://" + s, nil
	}
}

// tokenTravelsSafely refuses an endpoint that would carry the GitHub token
// outside TLS: cleartext ws:// to anything but this machine's own loopback.
func tokenTravelsSafely(endpoint string) error {
	if !strings.HasPrefix(endpoint, "ws://") {
		return nil // wss:// — TLS terminates before the token moves
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("live: cannot read the address %q: %w", endpoint, err)
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("live: refusing to send the token over %s: it would travel in the clear — join with the https address serve printed, or from this machine", endpoint)
}

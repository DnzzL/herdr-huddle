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

// ErrRefused is the one failure a reconnect cannot fix: this person may not
// join this share. Everything else — a dropped tunnel, a dead pane, a network
// blip — is worth dialling again.
var ErrRefused = errors.New("live: not let in")

// Events is what a client does with the room it has joined.
//
// Every field is optional: a renderer that only wants the pane sets Frame, and
// the TUI sets all three. One loop feeds all of them, which keeps frame
// drawing, presence and delivery feedback honest against the same protocol —
// an error record ends the stream, a said record is reported without ending
// it, and a record type from a newer Herdr is skipped rather than mistaken for
// the end.
type Events struct {
	// Frame is the pane's ANSI bytes, already decoded.
	Frame func([]byte) error
	// Room is who is in the huddle and what the agent is doing.
	Room func(Frame)
	// Said is the outcome of somebody's instruction — the recipient's own or
	// another member's, told apart by comparing Author with the room's You.
	Said func(Frame)
	// Chat is a message from somebody in the room to the room. It never
	// reached the agent and never will.
	Chat func(Frame)
}

// walk reads records until a terminal one, dispatching each.
func walk(r io.Reader, e Events) error {
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
			if e.Frame == nil {
				continue
			}
			if err := e.Frame(data); err != nil {
				return fmt.Errorf("live: draw frame: %w", err)
			}
		case TypeError:
			if frame.Fatal {
				// The door was closed to this person. Reconnecting would
				// knock again, forever, so say so in a way the caller can
				// test.
				return fmt.Errorf("%w: %s", ErrRefused, frame.Message)
			}
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
			if e.Said != nil {
				e.Said(frame)
			}
		case TypeRoom:
			if e.Room != nil {
				e.Room(frame)
			}
		case TypeChat:
			if e.Chat != nil {
				e.Chat(frame)
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
// viewport the frame is trying to paint. Chrome belongs to the TUI, which
// composites rather than interleaves (ADR-007).
func Render(stream io.Reader, out io.Writer) error {
	return walk(stream, Events{Frame: func(data []byte) error {
		_, err := out.Write(data)
		return err
	}})
}

// Session is a joiner's side of a live share: frames out, instructions in.
type Session struct {
	conn   net.Conn
	reader *bufio.Reader
}

// Option tunes a dial.
type Option func(*Frame)

// WithViewport asks for the stream to be rendered at the joiner's own size.
// Without it the server serves the size it was started with (ADR-007).
func WithViewport(cols, rows int) Option {
	return func(hello *Frame) { hello.Width, hello.Height = cols, rows }
}

// Dial connects, presents the token as the first record, and returns once the
// server's hello read has had its chance to refuse.
//
// Presenting before reading is the whole protocol: the server observes
// nothing until the gate has approved, so there is no window in which frames
// are sent to someone unapproved (ADR-006).
func Dial(ctx context.Context, endpoint, token string, opts ...Option) (*Session, error) {
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

	frame := Frame{Type: TypeHello, Token: token}
	for _, opt := range opts {
		opt(&frame)
	}
	hello, err := json.Marshal(frame)
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

// Say sends one instruction. It does not wait for the outcome: the room
// reports that, on the same connection.
func (s *Session) Say(text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("live: nothing to send")
	}
	if err := s.write(Frame{Type: TypeSay, Text: text}); err != nil {
		return fmt.Errorf("live: could not send the instruction: %w", err)
	}
	return nil
}

// Resize asks for the stream to be repainted at a new size. The server
// restarts this joiner's observer, so the next frame is a complete paint.
func (s *Session) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return nil
	}
	return s.write(Frame{Type: TypeResize, Width: cols, Height: rows})
}

// Chat sends a message to the people in the room, and to nobody else.
//
// It is deliberately a different method from Say, not a flag on it: "talk to
// my colleague" and "tell the agent to do something" must never be one call
// that a boolean could get the wrong way round.
func (s *Session) Chat(text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("live: nothing to say")
	}
	if err := s.write(Frame{Type: TypeChat, Text: text}); err != nil {
		return fmt.Errorf("live: could not send the message: %w", err)
	}
	return nil
}

// Typing says this joiner has started or stopped composing — an instruction
// for the agent, or, with toRoom, a message for the room.
//
// It is a claim about oneself: the server attributes it to the login the gate
// proved and ignores anything the record might name. The claim lapses on its
// own (TypingTTL), so a client that dies mid-sentence stops appearing to type
// without having to say so.
func (s *Session) Typing(on, toRoom bool) error {
	f := Frame{Type: TypeTyping, On: on}
	if toRoom {
		f.To = ToRoom
	}
	return s.write(f)
}

func (s *Session) write(f Frame) error {
	line, err := json.Marshal(f)
	if err != nil {
		return err // Frame is a plain struct; this cannot fail.
	}
	_, err = s.conn.Write(append(line, '\n'))
	return err
}

// Run reads the stream and dispatches it, until the stream ends.
func (s *Session) Run(e Events) error { return walk(s.reader, e) }

// Draw renders frames to out and room events to feedback, until the stream
// ends. It is the fallback for a client with no terminal to composite onto;
// the TUI uses Run.
func (s *Session) Draw(out, feedback io.Writer) error {
	return s.Run(Events{
		Frame: func(data []byte) error {
			_, err := out.Write(data)
			return err
		},
		Said: func(frame Frame) {
			if feedback == nil {
				return
			}
			fmt.Fprintln(feedback, "herdr-huddle: "+SaidLine(frame))
		},
	})
}

// Close ends the session.
func (s *Session) Close() error { return s.conn.Close() }

// SaidLine words a delivery outcome for the room to read.
//
// It quotes what was asked for, not only that something was. Two people
// steering one agent need to know whether the other just asked for the thing
// they were about to ask for, and "delivered" does not tell them.
func SaidLine(f Frame) string {
	who := ""
	if f.Author != "" {
		who = "@" + f.Author + " "
	}
	said := strings.TrimSpace(f.Text)
	if said != "" {
		said = "→ " + said + " · "
	}
	return who + said + Outcome(f)
}

// Outcome words what became of one instruction.
//
// It lives here, once, because two clients render it — the plain renderer and
// the TUI — and a second switch on the same statuses is a second set of words
// that drifts from these.
func Outcome(f Frame) string {
	switch f.Status {
	case StatusSent:
		return "delivered"
	case StatusQueued:
		// Not a failure and not a delivery. Saying only "queued" leaves the
		// person wondering what the queue is.
		return "waiting for the operator to approve it"
	case StatusRefused:
		return "refused: " + f.Reason
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

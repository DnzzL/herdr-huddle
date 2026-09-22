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

// Render draws a stream onto a terminal, and returns when the stream ends.
//
// It writes the frame payloads and nothing else: the stream is ANSI, the
// terminal is a terminal, and anything added here would be drawn into the
// viewport the frame is trying to paint. Records it does not know are skipped,
// an `error` record becomes an error, and `terminal.closed` is an ordinary end.
func Render(stream io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 0, 64*1024), maxRecordBytes)
	for scanner.Scan() {
		frame, err := ParseFrame(scanner.Bytes())
		if err != nil {
			return err
		}
		if !frame.Known() {
			// A record type from a newer Herdr. Skipping it keeps the pane
			// rendering; there is nothing else sensible to do with it.
			continue
		}
		switch frame.Type {
		case TypeFrame:
			data, err := frame.Data()
			if err != nil {
				return err
			}
			if _, err := out.Write(data); err != nil {
				return fmt.Errorf("live: draw frame: %w", err)
			}
		case TypeError:
			if frame.Message == "" {
				return errors.New("live: the stream failed")
			}
			return fmt.Errorf("live: the stream failed: %s", frame.Message)
		case TypeClosed:
			// A closed record can carry a reason — a pane that no longer exists is
			// reported this way, with a clean exit — so it is only an ordinary
			// ending when there is nothing to explain.
			if frame.Reason != "" {
				return fmt.Errorf("live: the stream closed: %s", frame.Reason)
			}
			return nil
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("live: read stream: %w", err)
	}
	return nil
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

// Join connects to a live share, presents the token as the first record, and
// draws the stream until it ends.
//
// Presenting before reading is the whole protocol: the server observes
// nothing until the gate has approved, so there is no window in which frames
// are sent to someone unapproved (ADR-006).
func Join(ctx context.Context, endpoint, token string, out io.Writer) error {
	u, err := wsEndpoint(endpoint)
	if err != nil {
		return err
	}
	// The token is the gate's whole credential: over anything but TLS or
	// loopback it would travel in the clear, which is the one thing ADR-006
	// forbids of it.
	if err := tokenTravelsSafely(u); err != nil {
		return err
	}
	conn, _, err := websocket.Dial(ctx, u, nil)
	if err != nil {
		return fmt.Errorf("live: could not reach a live share at %s: %w", u, err)
	}
	stream := websocket.NetConn(ctx, conn, websocket.MessageBinary)
	defer func() { _ = stream.Close() }()

	hello, err := json.Marshal(Frame{Type: TypeHello, Token: token})
	if err != nil {
		// Frame is a plain struct; this cannot fail, but a stream must end
		// one way or another.
		return fmt.Errorf("live: build hello: %w", err)
	}
	if _, err := stream.Write(append(hello, '\n')); err != nil {
		return fmt.Errorf("live: could not present the token: %w", err)
	}
	return Render(stream, out)
}

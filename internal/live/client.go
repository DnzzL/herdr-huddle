package live

import (
	"bufio"
	"errors"
	"fmt"
	"io"
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

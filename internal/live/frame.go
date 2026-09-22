// Package live carries a pane's live output from the operator's machine to the
// people joining the huddle (ADR-005).
//
// The bytes are Herdr's. `herdr terminal session observe <pane>` prints
// newline-delimited records: terminal.frame, holding a base64 ANSI payload, and
// terminal.closed when the stream ends. Its first frame is a complete paint, so
// a joiner who arrives late sees a whole screen rather than the tail of one.
//
// This package relays those records unchanged and adds exactly one of its own,
// `error`, so that a joiner whose stream died is told why instead of watching a
// frozen screen. Nothing here decides who may join; that gate is the share's
// allowlist and it is opened before a connection is served (ADR-005).
package live

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

// Record types. The first two are Herdr's; the last is ours.
const (
	TypeFrame  = "terminal.frame"
	TypeClosed = "terminal.closed"
	TypeError  = "error"
	// TypeHello is the joiner's first record: who they are, proven by a
	// GitHub token. It travels client-to-server only.
	TypeHello = "hello"
	// TypeSay is an instruction from a joiner who is already past the gate.
	// Client-to-server only.
	TypeSay = "say"
	// TypeSaid reports what became of a say: delivered, held, or failed.
	// Server-to-client, and never terminal — the stream carries on.
	TypeSaid = "said"
)

// EncodingANSI is the only payload encoding Herdr sends, and the only one worth
// drawing into a terminal.
const EncodingANSI = "ansi"

// ErrEncoding reports a payload this client cannot draw.
var ErrEncoding = errors.New("live: unsupported frame encoding")

// Frame is one record of the stream.
//
// The fields are Herdr's names, and every one of them is kept: a joiner's
// viewport is painted from Width/Height as much as from Bytes.
type Frame struct {
	Type     string `json:"type"`
	Seq      int    `json:"seq"`
	Bytes    string `json:"bytes"`
	Encoding string `json:"encoding"`
	Full     bool   `json:"full"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	// Reason is set when the record exists to explain itself: on a closed
	// record, why the stream ended (Herdr reports a pane that is gone as a
	// clean exit with "terminal target <pane> not found", so a closed record
	// is not always an ordinary ending); on a said record, why the instruction
	// was not simply sent.
	Reason string `json:"reason,omitempty"`
	// Message is set on our own error records only.
	Message string `json:"message,omitempty"`
	// Token is set on a hello record only: the GitHub token the gate checks
	// before the first frame. Never logged, never written anywhere else.
	Token string `json:"token,omitempty"`
	// Text is the instruction on a say record, verbatim.
	Text string `json:"text,omitempty"`
	// Status is what a said record reports back: one of the Status*
	// constants. Reason (above) carries why, when it was not simply sent.
	Status string `json:"status,omitempty"`
}

// ParseFrame decodes one record.
//
// A record type we do not know is decoded and left for the caller to pass
// through: Herdr is free to add record types, and the one outcome worth
// protecting is that the pane keeps rendering. A record we cannot parse at all
// is an error, because drawing half a screen paint corrupts the viewport.
func ParseFrame(line []byte) (Frame, error) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return Frame{}, errors.New("live: empty record")
	}
	if trimmed[0] != '{' {
		return Frame{}, fmt.Errorf("live: record is not a JSON object: %s", oneLine(trimmed))
	}
	var frame Frame
	if err := json.Unmarshal(trimmed, &frame); err != nil {
		return Frame{}, fmt.Errorf("live: decode record: %w", err)
	}
	if frame.Type == "" {
		return Frame{}, fmt.Errorf("live: record has no type: %s", oneLine(trimmed))
	}
	if frame.Type == TypeFrame && frame.Bytes == "" {
		return Frame{}, errors.New("live: terminal.frame carries no bytes")
	}
	return frame, nil
}

// Known reports whether the record type is one this package understands. The
// relaying path does not need it — it passes every parsed record through — but
// a client does.
func (f Frame) Known() bool {
	switch f.Type {
	case TypeFrame, TypeClosed, TypeError, TypeHello, TypeSay, TypeSaid:
		return true
	}
	return false
}

// Data returns the frame's ANSI bytes.
func (f Frame) Data() ([]byte, error) {
	if f.Type != TypeFrame {
		return nil, fmt.Errorf("live: %s is not a frame", f.Type)
	}
	if f.Encoding != EncodingANSI {
		return nil, fmt.Errorf("%w: %q", ErrEncoding, f.Encoding)
	}
	data, err := base64.StdEncoding.DecodeString(f.Bytes)
	if err != nil {
		return nil, fmt.Errorf("live: decode frame payload: %w", err)
	}
	return data, nil
}

// ErrorRecord builds the one record this package adds to the stream.
func ErrorRecord(err error) []byte {
	line, marshalErr := json.Marshal(Frame{Type: TypeError, Message: err.Error()})
	if marshalErr != nil {
		// Impossible for this struct, and a stream must end one way or another.
		return []byte(`{"type":"error","message":"live: the stream failed"}` + "\n")
	}
	return append(line, '\n')
}

// ErrorRecordClosed is the deliberate end of a stream, as a record. Written
// before the connection closes so the joiner's verdict comes from data rather
// than from how the socket happened to shut.
func ErrorRecordClosed() []byte {
	return []byte(`{"type":"terminal.closed"}` + "\n")
}

// oneLine keeps an error readable when a record is long or chunky.
func oneLine(raw []byte) string {
	const max = 120
	s := string(bytes.TrimSpace(raw))
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

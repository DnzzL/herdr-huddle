package live

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// A frame from a live Herdr 0.9.0 session, byte for byte as observed:
//
//	herdr terminal session observe w16:p1 --cols 80 --rows 12
//
// The shape is fixed by the server, so these fixtures are the contract: if a
// future Herdr renames or reshapes a field, these tests fail rather than the
// stream quietly rendering nothing.
const sampleFrame = `{"bytes":"G1s/MjAyNmgbWz8yNWw=","encoding":"ansi","full":true,"height":12,"seq":1,"type":"terminal.frame","width":80}`

func TestParseFrame(t *testing.T) {
	frame, err := ParseFrame([]byte(sampleFrame))
	if err != nil {
		t.Fatalf("ParseFrame errored: %v", err)
	}
	if frame.Type != TypeFrame {
		t.Errorf("Type = %q, want %q", frame.Type, TypeFrame)
	}
	if frame.Seq != 1 {
		t.Errorf("Seq = %d, want 1", frame.Seq)
	}
	if frame.Encoding != EncodingANSI {
		t.Errorf("Encoding = %q, want %q", frame.Encoding, EncodingANSI)
	}
	if !frame.Full {
		t.Error("Full = false, want true: the first frame is a complete paint, and a joiner depends on it")
	}
	if frame.Width != 80 || frame.Height != 12 {
		t.Errorf("Size = %dx%d, want 80x12", frame.Width, frame.Height)
	}

	data, err := frame.Data()
	if err != nil {
		t.Fatalf("Data() errored: %v", err)
	}
	want, _ := base64.StdEncoding.DecodeString("G1s/MjAyNmgbWz8yNWw=")
	if string(data) != string(want) {
		t.Errorf("Data() = %q, want %q", data, want)
	}
}

// The closed record is how a joiner learns the stream ended deliberately
// instead of wondering why the bytes stopped.
func TestParseFrameClosed(t *testing.T) {
	frame, err := ParseFrame([]byte(`{"type":"terminal.closed"}`))
	if err != nil {
		t.Fatalf("ParseFrame errored: %v", err)
	}
	if frame.Type != TypeClosed {
		t.Errorf("Type = %q, want %q", frame.Type, TypeClosed)
	}
}

// A record we do not know about must not kill a live stream: Herdr is free to
// add more record types, and the one thing worth protecting is that the pane
// keeps rendering.
func TestParseFrameIgnoresUnknownTypes(t *testing.T) {
	frame, err := ParseFrame([]byte(`{"type":"terminal.title","title":"pi"}`))
	if err != nil {
		t.Fatalf("ParseFrame errored on an unknown record type: %v", err)
	}
	if frame.Type != "terminal.title" {
		t.Errorf("Type = %q, want it decoded but unrecognised", frame.Type)
	}
	if frame.Known() {
		t.Error("Known() = true for terminal.title, want false")
	}
}

// A truncated or corrupt line is a different matter: rendering half a screen
// paint produces a corrupt viewport, so it has to be an error.
func TestParseFrameRejectsGarbage(t *testing.T) {
	for _, line := range []string{
		``,
		`   `,
		`not json at all`,
		`{"bytes":`,
		`[]`,
	} {
		if _, err := ParseFrame([]byte(line)); err == nil {
			t.Errorf("ParseFrame(%q) must fail", line)
		}
	}
}

// bytes is the payload; a frame without it, or with a payload that is not
// base64, is a shape change rather than an empty screen.
func TestParseFrameRequiresAValidPayload(t *testing.T) {
	if _, err := ParseFrame([]byte(`{"type":"terminal.frame","seq":1}`)); err == nil {
		t.Error("a frame with no bytes must fail")
	}
	frame, err := ParseFrame([]byte(`{"type":"terminal.frame","seq":1,"bytes":"!!!not base64!!!","encoding":"ansi"}`))
	if err != nil {
		t.Fatalf("ParseFrame errored: %v", err)
	}
	if _, err := frame.Data(); err == nil {
		t.Error("Data() must fail on a payload that is not base64")
	}
}

// Only ANSI is understood. Guessing at another encoding would render noise
// into a terminal, which is worse than saying so.
func TestParseFrameRejectsAnUnknownEncoding(t *testing.T) {
	frame, err := ParseFrame([]byte(`{"type":"terminal.frame","seq":2,"bytes":"AAAA","encoding":"protobuf"}`))
	if err != nil {
		t.Fatalf("ParseFrame errored: %v", err)
	}
	_, err = frame.Data()
	if !errors.Is(err, ErrEncoding) {
		t.Errorf("Data() error = %v, want ErrEncoding", err)
	}
	if err != nil && !strings.Contains(err.Error(), "protobuf") {
		t.Errorf("the error must name the encoding, got %q", err)
	}
}

// Big frames are normal: a full paint of a wide pane is tens of kilobytes.
func TestParseFrameAcceptsLargePayloads(t *testing.T) {
	payload := make([]byte, 200_000)
	for i := range payload {
		payload[i] = byte('a' + i%26)
	}
	frame, err := ParseFrame([]byte(`{"type":"terminal.frame","seq":9,"bytes":"` + base64.StdEncoding.EncodeToString(payload) + `","encoding":"ansi"}`))
	if err != nil {
		t.Fatalf("ParseFrame errored: %v", err)
	}
	data, err := frame.Data()
	if err != nil {
		t.Fatalf("Data() errored: %v", err)
	}
	if len(data) != len(payload) {
		t.Errorf("Data() returned %d bytes, want %d", len(data), len(payload))
	}
}

// Herdr answers a pane that is gone with a clean exit and a closed record that
// carries the reason, so the reason is not optional: without it a joiner sees a
// blank screen and a success. Fixture copied from 0.9.0.
func TestParseFrameKeepsTheCloseReason(t *testing.T) {
	line := `{"reason":"terminal session observe failed: terminal target w99:p99 not found","type":"terminal.closed"}`
	frame, err := ParseFrame([]byte(line))
	if err != nil {
		t.Fatalf("ParseFrame errored: %v", err)
	}
	if !frame.Known() {
		t.Error("terminal.closed must be a known record")
	}
	if !strings.Contains(frame.Reason, "not found") {
		t.Errorf("Reason = %q, want the pane that was not found", frame.Reason)
	}
}

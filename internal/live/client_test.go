package live

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestRenderWritesTheDecodedFrames(t *testing.T) {
	stream := strings.Join([]string{
		frameLine(1, "first"),
		`{"type":"terminal.title","title":"pi - demo"}`,
		frameLine(2, "-second"),
		`{"type":"terminal.closed"}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	if err := Render(strings.NewReader(stream), &out); err != nil {
		t.Fatalf("Render errored: %v", err)
	}
	if got := out.String(); got != "first-second" {
		t.Errorf("rendered %q, want %q", got, "first-second")
	}
}

// A closed stream is an ending, not a failure: the operator stopped it, or the
// pane went away cleanly.
func TestRenderTreatsCloseAsAnOrdinaryEnd(t *testing.T) {
	if err := Render(strings.NewReader("{\"type\":\"terminal.closed\"}\n"), &bytes.Buffer{}); err != nil {
		t.Errorf("Render errored on a closed stream: %v", err)
	}
}

// The daemon's own error record is the only record we add to Herdr's stream,
// and it exists so a joiner sees a reason instead of a frozen screen.
func TestRenderReportsAnErrorRecord(t *testing.T) {
	err := Render(strings.NewReader(`{"type":"error","message":"herdr: pane_not_found"}`+"\n"), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "pane_not_found") {
		t.Fatalf("error = %v, want the server's reason", err)
	}
}

// An error record with nothing in it still has to stop the stream.
func TestRenderReportsAnEmptyErrorRecord(t *testing.T) {
	if err := Render(strings.NewReader(`{"type":"error"}`+"\n"), &bytes.Buffer{}); err == nil {
		t.Error("an error record must fail the render")
	}
}

// A malformed record must not be rendered as bytes, and must not be swallowed.
func TestRenderRefusesToDrawGarbage(t *testing.T) {
	var out bytes.Buffer
	err := Render(strings.NewReader(frameLine(1, "good")+"\nnot a record\n"), &out)
	if err == nil {
		t.Fatal("a malformed record must fail")
	}
	if out.String() != "good" {
		t.Errorf("rendered %q, want only the good frame", out.String())
	}
}

// A frame larger than a default line scanner holds is normal, and would
// otherwise end the stream with "token too long".
func TestRenderHandlesLargeFrames(t *testing.T) {
	big := strings.Repeat("x", 300_000)
	var out bytes.Buffer
	if err := Render(strings.NewReader(frameLine(1, big)+"\n"), &out); err != nil {
		t.Fatalf("Render errored: %v", err)
	}
	if out.Len() != len(big) {
		t.Errorf("rendered %d bytes, want %d", out.Len(), len(big))
	}
}

// The renderer writes to a terminal, so a write failure is worth reporting:
// it is usually a joiner that has gone away.
func TestRenderReportsAWriteFailure(t *testing.T) {
	err := Render(strings.NewReader(frameLine(1, "x")+"\n"), failingWriter{})
	if err == nil {
		t.Error("a failed write must be reported")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// A close that carries a reason is a failure that Herdr reported politely, and
// must not read as an ordinary ending.
func TestRenderReportsWhyTheStreamClosed(t *testing.T) {
	line := `{"reason":"terminal session observe failed: terminal target w99:p99 not found","type":"terminal.closed"}`
	err := Render(strings.NewReader(line+"\n"), &bytes.Buffer{})
	if err == nil {
		t.Fatal("a closed stream with a reason must fail")
	}
	if !strings.Contains(err.Error(), "w99:p99") {
		t.Errorf("error = %v, want the reason to name the pane", err)
	}
}

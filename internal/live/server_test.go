package live

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeObserver is the seam that keeps these tests off a real Herdr session: it
// hands the server a canned stream and records how it was treated.
type fakeObserver struct {
	// observeErr fails the Observe call itself, the way a nonexistent pane
	// does.
	observeErr error
	// lines is what each stream emits.
	lines []string
	// waitErr is what Wait reports once the lines are consumed.
	waitErr error
	// block holds a stream open until the test releases it, so a disconnecting
	// joiner can be observed.
	block bool

	mu      sync.Mutex
	opened  int
	closed  int
	streams []*fakeStream
}

func (f *fakeObserver) Observe(ctx context.Context, pane string, cols, rows int) (Stream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.observeErr != nil {
		return nil, f.observeErr
	}
	f.opened++
	s := &fakeStream{
		ctx:      ctx,
		reader:   strings.NewReader(strings.Join(f.lines, "\n") + "\n"),
		waitErr:  f.waitErr,
		block:    f.block,
		onClose:  func() { f.mu.Lock(); f.closed++; f.mu.Unlock() },
		released: make(chan struct{}),
		done:     make(chan struct{}),
	}
	f.streams = append(f.streams, s)
	return s, nil
}

func (f *fakeObserver) openedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opened
}

func (f *fakeObserver) closedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *fakeObserver) lastStream(t *testing.T) *fakeStream {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.streams) == 0 {
		t.Fatal("no stream was opened")
	}
	return f.streams[len(f.streams)-1]
}

type fakeStream struct {
	ctx      context.Context
	reader   io.Reader
	waitErr  error
	block    bool
	onClose  func()
	released chan struct{}

	closeOnce sync.Once
	done      chan struct{}
}

func (s *fakeStream) Read(p []byte) (int, error) {
	if s.block {
		// Cancellation ends the read for the same reason it ends a real one:
		// the context is what kills the observe child.
		select {
		case <-s.released:
		case <-s.ctx.Done():
			return 0, io.EOF
		case <-s.done:
			return 0, io.EOF
		}
	}
	return s.reader.Read(p)
}

func (s *fakeStream) Close() error {
	s.closeOnce.Do(func() {
		close(s.done)
		if s.onClose != nil {
			s.onClose()
		}
	})
	return nil
}

func (s *fakeStream) Wait() error { return s.waitErr }

// frameLine builds a frame record the way Herdr does.
func frameLine(seq int, text string) string {
	return fmt.Sprintf(`{"bytes":%q,"encoding":"ansi","full":%t,"height":12,"seq":%d,"type":"terminal.frame","width":80}`,
		base64.StdEncoding.EncodeToString([]byte(text)), seq == 1, seq)
}

// serveTest starts a server on a real loopback listener, because the transport
// is the one thing a fake cannot establish.
func serveTest(t *testing.T, obs Observer) (addr string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &Server{Pane: "w1:p1", Cols: 80, Rows: 12, Observe: obs}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.Serve(ctx, ln)
	}()
	return ln.Addr().String(), func() {
		cancel()
		ln.Close()
		<-done
	}
}

// A joiner dials in and gets rendered bytes. This is the whole of phase 1: a
// pane's live output arriving somewhere else, over a socket, with no Herdr on
// the far side.
func TestServerStreamsAPaneToAJoiner(t *testing.T) {
	obs := &fakeObserver{lines: []string{frameLine(1, "hello from the agent"), frameLine(2, "second frame")}}
	addr, stop := serveTest(t, obs)
	defer stop()

	var out bytes.Buffer
	if err := Render(dial(t, addr), &out); err != nil {
		t.Fatalf("Render errored: %v", err)
	}
	if got := out.String(); got != "hello from the agentsecond frame" {
		t.Errorf("rendered %q, want both frames in order", got)
	}
	if obs.openedCount() != 1 {
		t.Errorf("opened %d streams, want 1", obs.openedCount())
	}
}

// Herdr supports several observers on one pane, so each joiner gets its own
// stream. Nothing is fanned out or cached here: a joiner that arrives late
// still receives Herdr's own first, complete paint.
func TestServerGivesEachJoinerItsOwnStream(t *testing.T) {
	obs := &fakeObserver{lines: []string{frameLine(1, "first")}}
	addr, stop := serveTest(t, obs)
	defer stop()

	for i := 0; i < 3; i++ {
		var out bytes.Buffer
		if err := Render(dial(t, addr), &out); err != nil {
			t.Fatalf("joiner %d: %v", i, err)
		}
		if out.String() != "first" {
			t.Errorf("joiner %d rendered %q, want %q", i, out.String(), "first")
		}
	}
	if obs.openedCount() != 3 {
		t.Errorf("opened %d streams for 3 joiners, want 3", obs.openedCount())
	}
}

// A pane that is gone must be reported, not rendered as an empty screen.
func TestServerReportsAFailedStream(t *testing.T) {
	obs := &fakeObserver{observeErr: errors.New("herdr: pane_not_found: no such pane")}
	addr, stop := serveTest(t, obs)
	defer stop()

	err := Render(dial(t, addr), io.Discard)
	if err == nil {
		t.Fatal("Render must report the failure")
	}
	if !strings.Contains(err.Error(), "pane_not_found") {
		t.Errorf("error = %v, want the observer's reason", err)
	}
}

// Likewise when a stream dies mid-way: the joiner is told why the screen
// stopped updating.
func TestServerReportsAStreamThatEndsBadly(t *testing.T) {
	obs := &fakeObserver{lines: []string{frameLine(1, "partial")}, waitErr: errors.New("herdr exited 1: terminal_gone")}
	addr, stop := serveTest(t, obs)
	defer stop()

	var out bytes.Buffer
	err := Render(dial(t, addr), &out)
	if err == nil || !strings.Contains(err.Error(), "terminal_gone") {
		t.Fatalf("error = %v, want the stream's reason", err)
	}
	if out.String() != "partial" {
		t.Errorf("rendered %q, want what did arrive before the failure", out.String())
	}
}

// A garbled record would half-draw a viewport, so it stops the stream rather
// than being written through.
func TestServerStopsAtAGarbledRecord(t *testing.T) {
	obs := &fakeObserver{lines: []string{frameLine(1, "good"), "this is not a frame", frameLine(2, "never rendered")}}
	addr, stop := serveTest(t, obs)
	defer stop()

	var out bytes.Buffer
	err := Render(dial(t, addr), &out)
	if err == nil {
		t.Fatal("a garbled record must fail the stream")
	}
	if out.String() != "good" {
		t.Errorf("rendered %q, want only the frames before the bad record", out.String())
	}
}

// A joiner that goes away must take its observer with it: otherwise every
// disconnect leaks a child process per pane, forever.
func TestServerClosesTheStreamWhenTheJoinerLeaves(t *testing.T) {
	obs := &fakeObserver{block: true}
	addr, stop := serveTest(t, obs)
	defer stop()

	conn := dial(t, addr)
	waitFor(t, func() bool { return obs.openedCount() == 1 })
	conn.Close()

	waitFor(t, func() bool { return obs.closedCount() == 1 })
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}

// A server with no pane to stream cannot answer anything meaningful, so it
// must refuse rather than serve an empty stream.
func TestServerRequiresAPane(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	srv := &Server{Observe: &fakeObserver{}}
	if err := srv.Serve(context.Background(), ln); err == nil {
		t.Error("Serve must fail without a pane")
	}
}

// The raw records are passed through unchanged, so a field Herdr adds is not
// silently dropped by an intermediate parser.
func TestServerPassesRecordsThroughUnchanged(t *testing.T) {
	line := frameLine(7, "verbatim")
	obs := &fakeObserver{lines: []string{line}}
	addr, stop := serveTest(t, obs)
	defer stop()

	raw, err := io.ReadAll(dial(t, addr))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(raw), &got); err != nil {
		t.Fatalf("the first record is not JSON: %v (%q)", err, raw)
	}
	var want map[string]any
	if err := json.Unmarshal([]byte(line), &want); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("record changed in flight:\n got %v\nwant %v", got, want)
	}
}

func dial(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// The reason a pane went away is the only useful thing a joiner can be told, so
// it must survive the relay.
func TestServerRelaysAReasonToTheJoiner(t *testing.T) {
	closed := `{"reason":"terminal session observe failed: terminal target w99:p99 not found","type":"terminal.closed"}`
	obs := &fakeObserver{lines: []string{closed}}
	addr, stop := serveTest(t, obs)
	defer stop()

	err := Render(dial(t, addr), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "w99:p99") {
		t.Fatalf("error = %v, want the relayed reason", err)
	}
}

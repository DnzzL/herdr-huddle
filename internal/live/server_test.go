package live

import (
	"bufio"
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

	"github.com/coder/websocket"
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
	// release, when set, ends every stream on close: the stream then reports
	// EOF, and the server closes the joiner cleanly. It exists because a
	// stream that dies on its own races whatever the test is asserting.
	release chan struct{}

	mu      sync.Mutex
	opened  int
	closed  int
	streams []*fakeStream
	// asked records the viewport each stream was opened at, in order. A
	// joiner's own terminal size is meant to reach here (ADR-007).
	asked [][2]int
}

func (f *fakeObserver) Observe(ctx context.Context, pane string, cols, rows int) (Stream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.observeErr != nil {
		return nil, f.observeErr
	}
	f.opened++
	f.asked = append(f.asked, [2]int{cols, rows})
	released := f.release
	if released == nil {
		released = make(chan struct{})
	}
	// An observer with no lines is a stream that ends immediately, not one
	// empty record — an empty line is a malformed record to the server, and
	// would end the joiner with a failure instead of a plain close.
	content := ""
	if len(f.lines) > 0 {
		content = strings.Join(f.lines, "\n") + "\n"
	}
	// A release switch means the test ends the stream, so the stream holds
	// open until then — otherwise it would EOF before the test says its piece.
	s := &fakeStream{
		ctx:      ctx,
		reader:   strings.NewReader(content),
		waitErr:  f.waitErr,
		block:    f.block || f.release != nil,
		onClose:  func() { f.mu.Lock(); f.closed++; f.mu.Unlock() },
		released: released,
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

// viewports reports the sizes every stream was opened at, in order.
func (f *fakeObserver) viewports() [][2]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][2]int(nil), f.asked...)
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
// newTestServer is the fully wired default: a permissive gate, a fake
// instructor and a fake ledger, because serveGuard requires all three. Tests
// override only the part they are about.
func newTestServer(obs Observer) *Server {
	return &Server{
		Pane: "w1:p1", Cols: 80, Rows: 12, Observe: obs,
		Gate:       &Gate{Verify: &verifierStub{login: "tester"}, Allowlist: []string{"tester"}},
		Instructor: &fakeInstructor{},
		Ledger:     &fakeLedger{},
		ThreadURL:  "https://github.com/acme/demo/pull/1",
	}
}

func serveTest(t *testing.T, obs Observer) (addr string, stop func()) {
	t.Helper()
	return startServer(t, newTestServer(obs))
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

	recs, err := readRecords(dial(t, addr))
	if err != nil {
		t.Fatalf("read records: %v", err)
	}
	if len(recs) == 0 {
		t.Fatal("no records arrived")
	}
	// The room record arrives first and is ours; the pane's frames are what
	// must survive the trip byte for byte.
	raw := firstOfType(t, recs, TypeFrame)
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("the frame record is not JSON: %v (%q)", err, raw)
	}
	var want map[string]any
	if err := json.Unmarshal([]byte(line), &want); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("record changed in flight:\n got %v\nwant %v", got, want)
	}
}

// dialSilent connects the way a probe does: TCP open, no hello. The gate is
// what happens to it.
func dialSilent(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, _, err := websocket.Dial(context.Background(), "ws://"+addr, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	netConn := websocket.NetConn(context.Background(), conn, websocket.MessageBinary)
	t.Cleanup(func() { _ = netConn.Close() })
	return netConn
}

// dial connects and says who it is, which is what every joiner must do before
// a single frame is observed.

// readRecords reads until a terminal record (closed or error), the way the
// real client does. Anything after that is connection teardown, which the
// protocol deliberately does not depend on: the verdict travels as data.
func readRecords(r io.Reader) ([][]byte, error) {
	scanner := bufio.NewScanner(r)
	var recs [][]byte
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		recs = append(recs, line)
		if frame, err := ParseFrame(line); err == nil &&
			(frame.Type == TypeClosed || frame.Type == TypeError) {
			return recs, nil
		}
	}
	return recs, scanner.Err()
}

func dial(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn := dialSilent(t, addr)
	if _, err := io.WriteString(conn, `{"token":"test-token","type":"hello"}`+"\n"); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	return conn
}

// startServer runs a Server of the caller's choosing, for the cases where the
// default one (allowlisted, permissive) is not the point.
func startServer(t *testing.T, srv *Server) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		err := srv.Serve(ctx, ln)
		// If the server refuses to start, close the listener straight away:
		// otherwise a dial connects into a backlog nobody accepts, and the
		// client hangs instead of reporting the startup failure.
		if err != nil {
			t.Errorf("Serve refused to start: %v", err)
			_ = ln.Close()
		}
	}()
	return ln.Addr().String(), func() {
		cancel()
		_ = ln.Close()
		<-done
	}
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

// The gate runs before anything is observed: a refused joiner costs no child
// process and sees no frames. This is the property ADR-006 calls the entire
// boundary for a public endpoint.
func TestServerRefusesAJoinerBeforeOpeningAStream(t *testing.T) {
	obs := &fakeObserver{lines: []string{frameLine(1, "never rendered")}}
	srv := newTestServer(obs)
	srv.Gate = &Gate{Verify: &verifierStub{login: "stranger"}, Allowlist: []string{"DnzzL", "pagbrl"}}
	addr, stop := startServer(t, srv)
	defer stop()

	var out bytes.Buffer
	err := Render(dial(t, addr), &out)
	if err == nil {
		t.Fatal("a joiner off the allowlist must be refused")
	}
	if !strings.Contains(err.Error(), "stranger") {
		t.Errorf("error = %v, want it to name the refused login", err)
	}
	if obs.openedCount() != 0 {
		t.Errorf("opened %d streams for a refused joiner, want 0", obs.openedCount())
	}
	if out.Len() != 0 {
		t.Errorf("rendered %q to a refused joiner, want nothing", out.String())
	}
}

// A token GitHub cannot confirm is refused the same way, before any stream.
func TestServerRefusesATokenGitHubCannotConfirm(t *testing.T) {
	obs := &fakeObserver{}
	srv := newTestServer(obs)
	srv.Gate = &Gate{Verify: &verifierStub{err: errors.New("github: 401 Bad credentials")}, Allowlist: []string{"tester"}}
	addr, stop := startServer(t, srv)
	defer stop()

	err := Render(dial(t, addr), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("error = %v, want GitHub's reason", err)
	}
	if obs.openedCount() != 0 {
		t.Errorf("opened %d streams for an unverifiable token, want 0", obs.openedCount())
	}
}

// A joiner that opens the port and says nothing is dropped rather than held.
func TestServerDropsAJoinerThatNeverSaysHello(t *testing.T) {
	obs := &fakeObserver{block: true}
	srv := newTestServer(obs)
	srv.GateTimeout = 150 * time.Millisecond
	addr, stop := startServer(t, srv)
	defer stop()

	// Read through the client's own reader: the verdict comes from the error
	// record, not from how the connection was torn down afterwards.
	err := Render(dialSilent(t, addr), io.Discard)
	if err == nil {
		t.Fatal("a joiner that never says hello must be dropped")
	}
	if !strings.Contains(err.Error(), "hello") {
		t.Errorf("error = %v, want the server to say what it was waiting for", err)
	}
	if obs.openedCount() != 0 {
		t.Errorf("opened %d streams for a silent joiner, want 0", obs.openedCount())
	}
}

// The stream's verdict must arrive as data: the joiner's reader decides from
// records, and a teardown it cannot control must not change the answer.
func TestServerEndsTheStreamWithARecordWhenHerdrDoesNot(t *testing.T) {
	obs := &fakeObserver{lines: []string{frameLine(1, "the pane, then silence")}}
	addr, stop := serveTest(t, obs)
	defer stop()

	recs, err := readRecords(dial(t, addr))
	if err != nil {
		t.Fatalf("read records: %v", err)
	}
	if len(recs) == 0 {
		t.Fatal("no records arrived")
	}
	var last Frame
	if err := json.Unmarshal(recs[len(recs)-1], &last); err != nil {
		t.Fatalf("last record is not JSON: %v", err)
	}
	if last.Type != TypeClosed {
		t.Errorf("last record = %q, want %q so the joiner knows it ended deliberately",
			last.Type, TypeClosed)
	}
}

// firstOfType picks the first record of a kind out of a stream that now
// carries presence alongside the pane.
func firstOfType(t *testing.T, recs [][]byte, want string) []byte {
	t.Helper()
	for _, rec := range recs {
		frame, err := ParseFrame(rec)
		if err != nil {
			t.Fatalf("record is not parseable: %v (%q)", err, rec)
		}
		if frame.Type == want {
			return rec
		}
	}
	t.Fatalf("no %s record in %d records", want, len(recs))
	return nil
}

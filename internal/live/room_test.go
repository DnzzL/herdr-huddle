package live

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DnzzL/herdr-huddle/internal/herdr"
)

// tokenVerifier gives every connection its own identity, which is what makes
// a room of more than one person testable.
type tokenVerifier struct {
	mu      sync.Mutex
	logins  map[string]string
	fallbck string
}

func (v *tokenVerifier) Login(_ context.Context, token string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if login, ok := v.logins[token]; ok {
		return login, nil
	}
	return v.fallbck, nil
}

// statusStub is the agent's state, as the room would read it from herdr.
type statusStub struct {
	mu     sync.Mutex
	status string
}

func (s *statusStub) Status(context.Context, string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status, nil
}

func (s *statusStub) set(status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
}

// dialAs connects with a hello of the caller's choosing — a token that names
// someone, a window size, or both.
func dialAs(t *testing.T, addr string, hello Frame) *bufio.Reader {
	t.Helper()
	conn := dialSilent(t, addr)
	hello.Type = TypeHello
	line, err := json.Marshal(hello)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return bufio.NewReader(&deadlineConn{conn})
}

// deadlineConn keeps a test that is waiting for a record it will never get
// from hanging until the whole suite times out.
type deadlineConn struct{ net.Conn }

func (d *deadlineConn) Read(p []byte) (int, error) {
	_ = d.Conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	return d.Conn.Read(p)
}

// readerFor wraps a connection so a test waiting on a record it will never
// get fails instead of hanging.
func readerFor(conn net.Conn) *bufio.Reader { return bufio.NewReader(&deadlineConn{conn}) }

// await reads until a record the predicate accepts, and fails the test if the
// stream ends first.
func await(t *testing.T, r *bufio.Reader, what string, ok func(Frame) bool) Frame {
	t.Helper()
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			t.Fatalf("waiting for %s: %v", what, err)
		}
		frame, err := ParseFrame(line)
		if err != nil {
			t.Fatalf("a record from the server did not parse: %v", err)
		}
		if ok(frame) {
			return frame
		}
	}
}

// roomServer starts a server two named people can join. Every field a test
// cares about is set before Serve runs: a Server is read from its own
// goroutines the moment it starts, so configuring it afterwards is a race.
func roomServer(t *testing.T, obs Observer, tweak ...func(*Server)) string {
	t.Helper()
	srv := newTestServer(obs)
	srv.Gate = &Gate{
		Verify:    &tokenVerifier{logins: map[string]string{"tok-a": "ana", "tok-b": "bo"}, fallbck: "tester"},
		Allowlist: []string{"ana", "bo", "tester"},
	}
	for _, f := range tweak {
		f(srv)
	}
	addr, stop := startServer(t, srv)
	t.Cleanup(stop)
	return addr
}

// Two people in one huddle have to be able to see each other. A roster that
// only the server knows is how the same instruction gets sent twice.
func TestTheRoomTellsEveryoneWhoIsHere(t *testing.T) {
	obs := &fakeObserver{release: make(chan struct{})}
	addr := roomServer(t, obs)

	ana := dialAs(t, addr, Frame{Token: "tok-a"})
	first := await(t, ana, "the first room record", func(f Frame) bool { return f.Type == TypeRoom })
	if first.You != "ana" {
		t.Errorf("You = %q, want the recipient's own login", first.You)
	}
	if len(first.Members) != 1 || first.Members[0] != "ana" {
		t.Errorf("members = %v, want just [ana]", first.Members)
	}
	if first.Thread != "https://github.com/acme/demo/pull/1" {
		t.Errorf("thread = %q, want the pull request the huddle records into", first.Thread)
	}

	dialAs(t, addr, Frame{Token: "tok-b"})
	both := await(t, ana, "bo's arrival", func(f Frame) bool { return f.Type == TypeRoom && len(f.Members) == 2 })
	if both.Members[0] != "ana" || both.Members[1] != "bo" {
		t.Errorf("members = %v, want both, sorted", both.Members)
	}
	if both.You != "ana" {
		t.Errorf("You = %q on the second record too, want ana", both.You)
	}
}

// The agent's state is the thing a joiner cannot read off the pane: a silent
// terminal is a thinking agent and a finished one alike. ADR-005 named this
// and did not build it.
func TestTheRoomReportsWhatTheAgentIsDoing(t *testing.T) {
	obs := &fakeObserver{release: make(chan struct{})}
	srv := newTestServer(obs)
	srv.Gate = &Gate{Verify: &verifierStub{login: "tester"}, Allowlist: []string{"tester"}}
	status := &statusStub{status: herdr.StatusWorking}
	srv.Status, srv.StatusInterval = status, 5*time.Millisecond
	addr, stop := startServer(t, srv)
	defer stop()

	joiner := dialAs(t, addr, Frame{Token: "tok"})
	working := await(t, joiner, "a working agent", func(f Frame) bool { return f.Type == TypeRoom && f.Agent == herdr.StatusWorking })
	if working.Agent != herdr.StatusWorking {
		t.Fatalf("agent = %q", working.Agent)
	}

	status.set(herdr.StatusBlocked)
	await(t, joiner, "the agent becoming blocked", func(f Frame) bool { return f.Type == TypeRoom && f.Agent == herdr.StatusBlocked })
}

// Steering is the half of this that two people share. Ana's instruction has to
// reach Bo's screen, attributed, or they are steering blind.
func TestAnInstructionIsEchoedToTheWholeRoom(t *testing.T) {
	obs := &fakeObserver{release: make(chan struct{})}
	instructor := &fakeInstructor{}
	addr := roomServer(t, obs, func(s *Server) { s.Instructor = instructor })

	anaConn := dialSilent(t, addr)
	defer anaConn.Close()
	if _, err := io.WriteString(anaConn, `{"type":"hello","token":"tok-a"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	bo := dialAs(t, addr, Frame{Token: "tok-b"})
	// Bo must be seated before Ana speaks, or the echo has nowhere to go.
	await(t, bo, "both of us in the room", func(f Frame) bool { return f.Type == TypeRoom && len(f.Members) == 2 })

	if _, err := io.WriteString(anaConn, `{"type":"say","text":"rename the package"}`+"\n"); err != nil {
		t.Fatal(err)
	}

	said := await(t, bo, "ana's instruction", func(f Frame) bool { return f.Type == TypeSaid })
	if said.Author != "ana" {
		t.Errorf("author = %q, want ana — the login the gate proved, not one she claimed", said.Author)
	}
	if said.Text != "rename the package" {
		t.Errorf("text = %q, want what she typed", said.Text)
	}
	if said.Status != StatusSent {
		t.Errorf("status = %q, want %q", said.Status, StatusSent)
	}
}

// Each joiner already gets its own observer, so each joiner can have its own
// size. A stream rendered at somebody else's window is the wrapping ADR-005
// left behind.
func TestAJoinerIsServedAtItsOwnSize(t *testing.T) {
	obs := &fakeObserver{release: make(chan struct{})}
	addr := roomServer(t, obs)

	dialAs(t, addr, Frame{Token: "tok-a", Width: 132, Height: 44})
	waitFor(t, func() bool { return obs.openedCount() == 1 })

	got := obs.viewports()
	if got[0] != [2]int{132, 44} {
		t.Errorf("observed at %v, want the joiner's own 132x44", got[0])
	}
}

// A hello with no size is a client that could not measure itself, and must get
// the server's default rather than the smallest legal window.
func TestAJoinerWithNoSizeGetsTheServerDefault(t *testing.T) {
	obs := &fakeObserver{release: make(chan struct{})}
	addr := roomServer(t, obs)

	dialAs(t, addr, Frame{Token: "tok-a"})
	waitFor(t, func() bool { return obs.openedCount() == 1 })

	if got := obs.viewports()[0]; got != [2]int{80, 12} {
		t.Errorf("observed at %v, want the server's own 80x12", got)
	}
}

// Dragging a window edge has to repaint, and a repaint is a fresh observe —
// `observe` takes no resize, and a restart is also what produces the complete
// first paint a resized terminal needs (ADR-007).
func TestAResizeRestartsTheStreamAtTheNewSize(t *testing.T) {
	obs := &fakeObserver{release: make(chan struct{})}
	addr := roomServer(t, obs)

	conn := dialSilent(t, addr)
	defer conn.Close()
	if _, err := io.WriteString(conn, `{"type":"hello","token":"tok-a","width":80,"height":24}`+"\n"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return obs.openedCount() == 1 })

	if _, err := io.WriteString(conn, `{"type":"resize","width":120,"height":40}`+"\n"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return obs.openedCount() == 2 })

	got := obs.viewports()
	if got[0] != [2]int{80, 24} {
		t.Errorf("first stream at %v, want 80x24", got[0])
	}
	if got[1] != [2]int{120, 40} {
		t.Errorf("second stream at %v, want 120x40", got[1])
	}
}

// The numbers arrive from the network and become the size of a grid a child
// process allocates, so they are believed only this far.
func TestClampViewport(t *testing.T) {
	cases := []struct {
		name                   string
		cols, rows, defC, defR int
		wantCols, wantRows     int
	}{
		{"what the joiner asked for", 100, 30, 80, 24, 100, 30},
		{"no preference takes the server's", 0, 0, 90, 28, 90, 28},
		{"no preference and no default takes ours", 0, 0, 0, 0, DefaultCols, DefaultRows},
		{"absurdly large is capped", 100000, 100000, 80, 24, MaxCols, MaxRows},
		{"negative is not a size", -5, -5, 80, 24, 80, 24},
		{"tiny is floored", 1, 1, 80, 24, MinCols, MinRows},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cols, rows := clampViewport(c.cols, c.rows, c.defC, c.defR)
			if cols != c.wantCols || rows != c.wantRows {
				t.Errorf("clampViewport = %dx%d, want %dx%d", cols, rows, c.wantCols, c.wantRows)
			}
		})
	}
}

// The typing signal is the one that stops two people asking the agent for the
// same thing at once, so it has to reach the other person — and never be
// echoed back to the person whose hands are moving.
func TestTypingReachesTheRestOfTheRoom(t *testing.T) {
	obs := &fakeObserver{release: make(chan struct{})}
	addr := roomServer(t, obs)

	ana := dialSilent(t, addr)
	defer ana.Close()
	if _, err := io.WriteString(ana, `{"type":"hello","token":"tok-a"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	bo := dialAs(t, addr, Frame{Token: "tok-b"})
	await(t, bo, "both of us in the room", func(f Frame) bool { return f.Type == TypeRoom && len(f.Members) == 2 })

	if _, err := io.WriteString(ana, `{"type":"typing","on":true}`+"\n"); err != nil {
		t.Fatal(err)
	}
	seen := await(t, bo, "ana typing", func(f Frame) bool { return f.Type == TypeRoom && len(f.Typing) > 0 })
	if len(seen.Typing) != 1 || seen.Typing[0] != "ana" {
		t.Errorf("typing = %v, want [ana]", seen.Typing)
	}

	if _, err := io.WriteString(ana, `{"type":"typing","on":false}`+"\n"); err != nil {
		t.Fatal(err)
	}
	await(t, bo, "ana stopping", func(f Frame) bool { return f.Type == TypeRoom && len(f.Typing) == 0 })
}

// Nobody is told they are typing: a client knows what its own hands are doing,
// and an echo would fight the input line it is drawn under.
func TestATypistIsNotToldTheyAreTyping(t *testing.T) {
	obs := &fakeObserver{release: make(chan struct{})}
	addr := roomServer(t, obs)

	ana := dialSilent(t, addr)
	defer ana.Close()
	if _, err := io.WriteString(ana, `{"type":"hello","token":"tok-a"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(&deadlineConn{ana})
	await(t, reader, "the room", func(f Frame) bool { return f.Type == TypeRoom })

	// A second person, so the claim is worth broadcasting at all.
	bo := dialAs(t, addr, Frame{Token: "tok-b"})
	await(t, bo, "bo arriving", func(f Frame) bool { return f.Type == TypeRoom && len(f.Members) == 2 })

	if _, err := io.WriteString(ana, `{"type":"typing","on":true}`+"\n"); err != nil {
		t.Fatal(err)
	}
	await(t, bo, "bo being told", func(f Frame) bool { return f.Type == TypeRoom && len(f.Typing) == 1 })

	// Everything ana has been sent up to her own delivery, inspected: not one
	// record may name her as typing.
	if _, err := io.WriteString(ana, `{"type":"say","text":"go"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	for {
		record := await(t, reader, "records addressed to ana", func(Frame) bool { return true })
		for _, who := range record.Typing {
			if strings.EqualFold(who, "ana") {
				t.Fatalf("ana was told she is typing: %+v", record)
			}
		}
		if record.Type == TypeSaid {
			return
		}
	}
}

// Sending finishes the sentence: a claim left standing would show the author
// as still typing what they have already sent.
func TestSendingClearsTheTypingClaim(t *testing.T) {
	obs := &fakeObserver{release: make(chan struct{})}
	addr := roomServer(t, obs)

	ana := dialSilent(t, addr)
	defer ana.Close()
	if _, err := io.WriteString(ana, `{"type":"hello","token":"tok-a"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	bo := dialAs(t, addr, Frame{Token: "tok-b"})
	await(t, bo, "both of us in the room", func(f Frame) bool { return f.Type == TypeRoom && len(f.Members) == 2 })

	if _, err := io.WriteString(ana, `{"type":"typing","on":true}`+"\n"); err != nil {
		t.Fatal(err)
	}
	await(t, bo, "ana typing", func(f Frame) bool { return f.Type == TypeRoom && len(f.Typing) > 0 })

	if _, err := io.WriteString(ana, `{"type":"say","text":"ship it"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	await(t, bo, "ana no longer typing", func(f Frame) bool { return f.Type == TypeRoom && len(f.Typing) == 0 })
}

// A claim made by a client that then dies must lapse on its own: a typing
// indicator that is sometimes a ghost is worse than none.
func TestATypingClaimLapses(t *testing.T) {
	st := newSeat("ana")
	r := &room{seats: []*seat{st}}

	st.typingUntil = time.Now().Add(-time.Second)
	if got := r.typistsLocked(); len(got) != 0 {
		t.Errorf("typists = %v, want a lapsed claim to count for nothing", got)
	}
	st.typingUntil = time.Now().Add(TypingTTL)
	if got := r.typistsLocked(); len(got) != 1 {
		t.Errorf("typists = %v, want the standing claim", got)
	}
}

// The room's heartbeat lapses typing claims whether or not the server was
// given anything that can read the agent's status — and, the part that matters,
// *tells the room* when one lapses.
//
// Zeroing the claim without announcing it is not a fix: every joiner's screen
// keeps showing somebody typing until something else happens to trigger a
// broadcast, which on a quiet agent is never. That was a real bug, and it was
// invisible to a test that only checked the field.
func TestALapsedTypingClaimIsAnnounced(t *testing.T) {
	ana, bo := newSeat("ana"), newSeat("bo")
	r := &room{seats: []*seat{ana, bo}}
	ana.typingUntil = time.Now().Add(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.watchAgent(ctx, "", nil, 5*time.Millisecond)

	deadline := time.After(3 * time.Second)
	for {
		select {
		case raw := <-bo.out:
			frame, err := ParseFrame(raw)
			if err != nil {
				t.Fatalf("a record did not parse: %v", err)
			}
			if frame.Type == TypeRoom && len(frame.Typing) == 0 {
				return
			}
		case <-deadline:
			t.Fatal("the lapse was never announced, so @ana would appear to type forever")
		}
	}
}

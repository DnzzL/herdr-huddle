package live

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DnzzL/herdr-huddle/internal/herdr"
	"github.com/DnzzL/herdr-huddle/internal/thread"
)

// fakeInstructor stands in for the agent: it records what it was asked to
// deliver, in order, and can be told to fail like herdr does.
type fakeInstructor struct {
	mu      sync.Mutex
	panes   []string
	prompts []string
	err     error
}

func (f *fakeInstructor) Deliver(_ context.Context, pane, prompt string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.panes = append(f.panes, pane)
	f.prompts = append(f.prompts, prompt)
	return f.err
}

// delivered reports how many instructions arrived, plus the last one's pane
// and prompt. Both are empty-safe: most of its callers are asking whether
// nothing was delivered.
func (f *fakeInstructor) delivered() (int, string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.prompts) == 0 {
		return 0, "", ""
	}
	return len(f.prompts), f.panes[len(f.panes)-1], f.prompts[len(f.prompts)-1]
}

// fakeLedger stands in for the pull request: it records what should end up on
// the thread, and can fail the way a dead token or an outage does.
type fakeLedger struct {
	mu    sync.Mutex
	posts []thread.LiveInstruction
	err   error
	failN int // how many further calls fail before succeeding
}

func (f *fakeLedger) Post(_ context.Context, in thread.LiveInstruction) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		if f.failN > 0 {
			f.failN--
		}
		return f.err
	}
	f.posts = append(f.posts, in)
	return nil
}

func (f *fakeLedger) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.posts)
}

func (f *fakeLedger) first() thread.LiveInstruction {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.posts) == 0 {
		return thread.LiveInstruction{}
	}
	return f.posts[0]
}

// waitSaid reads records until the delivery outcome arrives, bounded by a read
// deadline. Tests that assert on a said record use this instead of Draw so
// they do not depend on when the stream ends.
func waitSaid(t *testing.T, session *Session) Frame {
	t.Helper()
	_ = session.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	defer func() { _ = session.conn.SetReadDeadline(time.Time{}) }()
	for {
		line, err := session.reader.ReadBytes('\n')
		if err != nil {
			t.Fatalf("reading a delivery outcome: %v", err)
		}
		frame, err := ParseFrame(line)
		if err != nil {
			t.Fatalf("a record from the server did not parse: %v", err)
		}
		if frame.Type == TypeSaid {
			return frame
		}
	}
}

// An instruction typed into the joiner's terminal reaches the agent, wrapped
// the way ADR-001 requires — tagged as a colleague's unverified message — and
// is then recorded on the thread with its author.
func TestSayReachesTheAgentAndIsRecorded(t *testing.T) {
	release := make(chan struct{})
	obs := &fakeObserver{release: release}
	instructor := &fakeInstructor{}
	ledger := &fakeLedger{}
	srv := newTestServer(obs)
	srv.Instructor, srv.Ledger = instructor, ledger
	addr, stop := startServer(t, srv)
	defer stop()

	session, err := Dial(context.Background(), addr, "test-token")
	if err != nil {
		t.Fatalf("Dial errored: %v", err)
	}
	defer session.Close()
	if err := session.Say("add a rule: three consecutive misses eliminates the player"); err != nil {
		t.Fatalf("Say errored: %v", err)
	}

	waitFor(t, func() bool {
		n, _, _ := instructor.delivered()
		return n == 1
	})
	_, pane, prompt := instructor.delivered()
	if pane != "w1:p1" {
		t.Errorf("delivered to pane %q, want the shared pane", pane)
	}
	for _, want := range []string{"@tester", "three consecutive misses", "not from your operator"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q — it must carry the author and the untrusted-input wrapper:\n%s", want, prompt)
		}
	}

	// The ack is written before the record in speak(), so a posted record means
	// Draw will see the outcome before the (about to be released) stream ends.
	waitFor(t, func() bool { return ledger.count() == 1 })
	close(release)

	var feedback strings.Builder
	if err := session.Draw(io.Discard, &feedback); err != nil {
		t.Fatalf("Draw errored: %v", err)
	}
	if !strings.Contains(feedback.String(), "delivered") {
		t.Errorf("feedback = %q, want the joiner to see the instruction landed", feedback.String())
	}

	got := ledger.first()
	if got.Author != "tester" || !strings.Contains(got.Text, "three consecutive misses") {
		t.Errorf("recorded %+v, want the author and the verbatim text", got)
	}
	if strings.HasPrefix(got.Body(), thread.Prefix) {
		t.Errorf("the record would be re-read as an instruction: %q", got.Body())
	}
}

// Pairing cannot be skipped: the first record must be the hello, so a client
// that starts talking immediately is refused before anything is delivered.
func TestSayBeforeTheHelloIsRefused(t *testing.T) {
	obs := &fakeObserver{}
	instructor := &fakeInstructor{}
	srv := newTestServer(obs)
	srv.Instructor = instructor
	addr, stop := startServer(t, srv)
	defer stop()

	conn := dialSilent(t, addr)
	if _, err := conn.Write([]byte(`{"text":"sneak past the gate","type":"say"}` + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	err := Render(conn, io.Discard)
	if err == nil {
		t.Fatal("a say before the hello must be refused")
	}
	if !strings.Contains(err.Error(), "hello") {
		t.Errorf("error = %v, want it to say a hello was expected", err)
	}
	if n, _, _ := instructor.delivered(); n != 0 {
		t.Errorf("delivered %d prompts to a joiner who never identified themselves", n)
	}
}

// Two delivery outcomes change what happens next: a blocked agent keeps the
// instruction undelivered (so nothing is recorded), and so does a failure.
// Both must reach the joiner — silence here is the failure mode ADR-005 names.
func TestUndeliveredInstructionsAreReportedAndNotRecorded(t *testing.T) {
	cases := []struct {
		name           string
		deliverErr     error
		wantInFeedback []string
	}{
		{
			name:           "blocked agent",
			deliverErr:     &herdr.Error{Code: "agent_blocked", Message: "agent is blocked"},
			wantInFeedback: []string{"held", "operator"},
		},
		{
			name:           "delivery failed",
			deliverErr:     errors.New("herdr: command failed: gone"),
			wantInFeedback: []string{"not delivered"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			obs := &fakeObserver{release: make(chan struct{})}
			instructor := &fakeInstructor{err: c.deliverErr}
			ledger := &fakeLedger{}
			srv := newTestServer(obs)
			srv.Instructor, srv.Ledger = instructor, ledger
			addr, stop := startServer(t, srv)
			defer stop()

			session, err := Dial(context.Background(), addr, "test-token")
			if err != nil {
				t.Fatalf("Dial errored: %v", err)
			}
			defer session.Close()
			if err := session.Say("do the thing"); err != nil {
				t.Fatalf("Say errored: %v", err)
			}

			said := waitSaid(t, session)
			line := saidLine(said)
			for _, want := range c.wantInFeedback {
				if !strings.Contains(line, want) {
					t.Errorf("feedback = %q, want it to mention %q", line, want)
				}
			}
			if ledger.count() != 0 {
				t.Errorf("recorded %d instructions the agent never received", ledger.count())
			}
		})
	}
}

// The ledger being down must not delay or refuse delivery: the record is
// queued and flushed later (ADR-005 — delivery never waits for GitHub).
func TestLedgerOutageQueuesWithoutBlockingDelivery(t *testing.T) {
	obs := &fakeObserver{release: make(chan struct{})}
	instructor := &fakeInstructor{}
	ledger := &fakeLedger{err: errors.New("github: 401: Bad credentials")}
	srv := newTestServer(obs)
	srv.Instructor, srv.Ledger = instructor, ledger
	srv.LedgerInterval = 30 * time.Millisecond
	addr, stop := startServer(t, srv)
	defer stop()

	session, err := Dial(context.Background(), addr, "test-token")
	if err != nil {
		t.Fatalf("Dial errored: %v", err)
	}
	defer session.Close()
	if err := session.Say("first instruction"); err != nil {
		t.Fatalf("Say errored: %v", err)
	}

	// Delivery happened already, despite the ledger being down.
	waitFor(t, func() bool {
		n, _, _ := instructor.delivered()
		return n == 1
	})
	if ledger.count() != 0 {
		t.Fatalf("the failing ledger recorded something? posts=%d", ledger.count())
	}
	waitFor(t, func() bool { return srv.queuedCount() == 1 })

	// GitHub comes back; the queue flushes on its own.
	ledger.mu.Lock()
	ledger.err = nil
	ledger.mu.Unlock()
	// The queue flushes on its own once the ledger recovers — no joiner
	// action, no restart, and delivery above never waited for any of this.
	waitFor(t, func() bool { return ledger.count() == 1 })

	got := ledger.first()
	if got.Author != "tester" || !strings.Contains(got.Text, "first instruction") {
		t.Errorf("flushed record = %+v, want the queued instruction", got)
	}
	if n := srv.queuedCount(); n != 0 {
		t.Errorf("queued = %d after a successful flush, want 0", n)
	}
}

// The gate's siblings: a server that cannot deliver or cannot record would be
// a silent dead end, so neither is allowed to start.
func TestServerRefusesToStartWithoutAnInstructorOrLedger(t *testing.T) {
	bases := []struct {
		name   string
		mutate func(*Server)
		want   string
	}{
		{"no instructor", func(s *Server) { s.Instructor = nil }, "instructor"},
		{"no ledger", func(s *Server) { s.Ledger = nil }, "ledger"},
	}
	for _, b := range bases {
		t.Run(b.name, func(t *testing.T) {
			srv := newTestServer(&fakeObserver{})
			b.mutate(srv)
			err := srv.serveGuard()
			if err == nil {
				t.Fatal("serveGuard must refuse")
			}
			if !strings.Contains(err.Error(), b.want) {
				t.Errorf("error = %v, want it to name the %s", err, b.want)
			}
		})
	}
}

// hungLedger never answers until its context ends — which is exactly how a
// real GitHub call with no timeout behaves when the network stalls.
type hungLedger struct {
	posts int
	mu    sync.Mutex
}

func (h *hungLedger) Post(ctx context.Context, _ thread.LiveInstruction) error {
	h.mu.Lock()
	h.posts++
	h.mu.Unlock()
	<-ctx.Done()
	return ctx.Err()
}

// A ledger that never answers must not lose the record: the post is bounded,
// and a timed-out post is queued like any other failure. Without the bound it
// would sit in a goroutine forever — neither posted nor queued, and invisible
// even to the shutdown log.
func TestAHungLedgerLosesNothing(t *testing.T) {
	obs := &fakeObserver{release: make(chan struct{})}
	instructor := &fakeInstructor{}
	ledger := &hungLedger{}
	srv := newTestServer(obs)
	srv.Instructor = instructor
	srv.Ledger = ledger
	srv.LedgerTimeout = 50 * time.Millisecond
	addr, stop := startServer(t, srv)
	defer stop()

	session, err := Dial(context.Background(), addr, "test-token")
	if err != nil {
		t.Fatalf("Dial errored: %v", err)
	}
	defer session.Close()
	if err := session.Say("an instruction the record must survive"); err != nil {
		t.Fatalf("Say errored: %v", err)
	}

	waitFor(t, func() bool { return srv.queuedCount() == 1 })
}

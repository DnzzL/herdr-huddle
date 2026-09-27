package live

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

// moderatorStub is the operator answering per instruction.
type moderatorStub struct {
	mu    sync.Mutex
	asked []string
	yes   bool
	err   error
}

func (m *moderatorStub) Approve(_ context.Context, login, text string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.asked = append(m.asked, "@"+login+": "+text)
	return m.yes, m.err
}

func (m *moderatorStub) askedAbout() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.asked...)
}

// The primitive the huddle was missing: two people in a room could not say
// anything to each other without the agent doing something about it.
func TestChatReachesTheRoomAndNotTheAgent(t *testing.T) {
	obs := &fakeObserver{release: make(chan struct{})}
	instructor := &fakeInstructor{}
	ledger := &fakeLedger{}
	addr := roomServer(t, obs, func(s *Server) { s.Instructor, s.Ledger = instructor, ledger })

	ana := dialSilent(t, addr)
	defer ana.Close()
	if _, err := io.WriteString(ana, `{"type":"hello","token":"tok-a"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	bo := dialAs(t, addr, Frame{Token: "tok-b"})
	await(t, bo, "both of us in the room", func(f Frame) bool { return f.Type == TypeRoom && len(f.Members) == 2 })

	if _, err := io.WriteString(ana, `{"type":"chat","text":"wait, do not touch the migration"}`+"\n"); err != nil {
		t.Fatal(err)
	}

	msg := await(t, bo, "ana's message", func(f Frame) bool { return f.Type == TypeChat })
	if msg.Author != "ana" {
		t.Errorf("author = %q, want the login the gate proved", msg.Author)
	}
	if msg.Text != "wait, do not touch the migration" {
		t.Errorf("text = %q", msg.Text)
	}
	// The whole point: the agent heard nothing, and neither did the thread.
	if n, _, _ := instructor.delivered(); n != 0 {
		t.Errorf("the agent was sent %d things; chat must never reach it", n)
	}
	if ledger.count() != 0 {
		t.Errorf("the thread recorded %d messages; side-talk is not the record", ledger.count())
	}
}

// Moderation splits one decision into two: letting somebody into the room, and
// letting them drive an agent that runs with the operator's permissions.
func TestModeratedInstructionWaitsForTheOperator(t *testing.T) {
	obs := &fakeObserver{release: make(chan struct{})}
	instructor := &fakeInstructor{}
	moderator := &moderatorStub{yes: true}
	addr := roomServer(t, obs, func(s *Server) {
		s.Instructor, s.Moderate = instructor, moderator
	})

	conn := dialSilent(t, addr)
	defer conn.Close()
	if _, err := io.WriteString(conn, `{"type":"hello","token":"tok-a"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	reader := readerFor(conn)
	await(t, reader, "the room", func(f Frame) bool { return f.Type == TypeRoom })

	if _, err := io.WriteString(conn, `{"type":"say","text":"drop the table"}`+"\n"); err != nil {
		t.Fatal(err)
	}

	// Queued first: silence while a person decides reads as a dropped
	// instruction.
	queued := await(t, reader, "the queued notice", func(f Frame) bool {
		return f.Type == TypeSaid && f.Status == StatusQueued
	})
	if queued.Text != "drop the table" {
		t.Errorf("the queued notice must quote what is waiting, got %q", queued.Text)
	}
	await(t, reader, "the delivery", func(f Frame) bool {
		return f.Type == TypeSaid && f.Status == StatusSent
	})

	if got := moderator.askedAbout(); len(got) != 1 || got[0] != "@ana: drop the table" {
		t.Errorf("asked %v, want the operator shown the words", got)
	}
	if n, _, _ := instructor.delivered(); n != 1 {
		t.Errorf("delivered %d, want 1", n)
	}
}

// A refused instruction must never reach the agent, and must never be recorded
// as though it had.
func TestARefusedInstructionNeverReachesTheAgent(t *testing.T) {
	obs := &fakeObserver{release: make(chan struct{})}
	instructor := &fakeInstructor{}
	ledger := &fakeLedger{}
	addr := roomServer(t, obs, func(s *Server) {
		s.Instructor, s.Ledger = instructor, ledger
		s.Moderate = &moderatorStub{yes: false}
	})

	conn := dialSilent(t, addr)
	defer conn.Close()
	if _, err := io.WriteString(conn, `{"type":"hello","token":"tok-a"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	reader := readerFor(conn)
	await(t, reader, "the room", func(f Frame) bool { return f.Type == TypeRoom })
	if _, err := io.WriteString(conn, `{"type":"say","text":"rm -rf /"}`+"\n"); err != nil {
		t.Fatal(err)
	}

	refused := await(t, reader, "the refusal", func(f Frame) bool {
		return f.Type == TypeSaid && f.Status == StatusRefused
	})
	if refused.Reason == "" {
		t.Error("a refusal with no reason leaves the joiner guessing")
	}
	if n, _, _ := instructor.delivered(); n != 0 {
		t.Errorf("the agent received %d refused instructions", n)
	}
	if ledger.count() != 0 {
		t.Errorf("the thread recorded %d refused instructions", ledger.count())
	}
}

// Nobody to ask is a failure, never an approval — the same rule the door
// follows.
func TestAnUnanswerableModerationDoesNotDeliver(t *testing.T) {
	obs := &fakeObserver{release: make(chan struct{})}
	instructor := &fakeInstructor{}
	addr := roomServer(t, obs, func(s *Server) {
		s.Instructor = instructor
		s.Moderate = &moderatorStub{yes: true, err: errors.New("stdin is closed")}
	})

	conn := dialSilent(t, addr)
	defer conn.Close()
	if _, err := io.WriteString(conn, `{"type":"hello","token":"tok-a"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	reader := readerFor(conn)
	await(t, reader, "the room", func(f Frame) bool { return f.Type == TypeRoom })
	if _, err := io.WriteString(conn, `{"type":"say","text":"go"}`+"\n"); err != nil {
		t.Fatal(err)
	}

	await(t, reader, "the failure", func(f Frame) bool {
		return f.Type == TypeSaid && f.Status == StatusFailed
	})
	if n, _, _ := instructor.delivered(); n != 0 {
		t.Errorf("the agent received %d instructions nobody approved", n)
	}
}

// A client that reconnects has to tell "the stream broke" from "you may not
// come in": redialling a closed door knocks forever.
func TestARefusalIsMarkedSoAClientStopsRetrying(t *testing.T) {
	obs := &fakeObserver{}
	srv := newTestServer(obs)
	srv.Gate = &Gate{Verify: &verifierStub{login: "stranger"}, Allowlist: []string{"operator"}}
	addr, stop := startServer(t, srv)
	defer stop()

	err := Join(context.Background(), addr, "their-token", &strings.Builder{})
	if err == nil {
		t.Fatal("a refused join must be an error")
	}
	if !errors.Is(err, ErrRefused) {
		t.Errorf("error = %v, want it to be ErrRefused so a reconnect gives up", err)
	}
	if !strings.Contains(err.Error(), "stranger") {
		t.Errorf("error = %v, want the gate's own reason kept", err)
	}
}

// A stream that simply broke is *not* a refusal, or a blipped tunnel would
// end the huddle for good.
func TestABrokenStreamIsNotARefusal(t *testing.T) {
	obs := &fakeObserver{observeErr: errors.New("terminal target w1:p1 not found")}
	addr, stop := serveTest(t, obs)
	defer stop()

	err := Join(context.Background(), addr, "test-token", &strings.Builder{})
	if err == nil {
		t.Fatal("a failed stream must be an error")
	}
	if errors.Is(err, ErrRefused) {
		t.Errorf("error = %v, want a retryable failure", err)
	}
}

// Reconnect depends on this distinction, so it is tested rather than assumed:
// only a person the gate turned away is told not to come back. Everything else
// may work on the next attempt, and ending a huddle over a GitHub blip is the
// worse failure of the two.
func TestOnlyBeingTurnedAwayIsFatal(t *testing.T) {
	turnedAway := []struct {
		name string
		gate *Gate
	}{
		{"not on the allowlist", &Gate{
			Verify: &verifierStub{login: "stranger"}, Allowlist: []string{"operator"},
		}},
		{"refused at the door", &Gate{
			Verify: &verifierStub{login: "stranger"}, Allowlist: []string{"operator"},
			Admit: &approverStub{yes: false},
		}},
	}
	for _, c := range turnedAway {
		t.Run(c.name, func(t *testing.T) {
			_, err := c.gate.Authorize(context.Background(), "tok")
			if err == nil || !NotAllowed(err) {
				t.Errorf("err = %v, want it marked as being turned away", err)
			}
		})
	}

	mayWorkLater := []struct {
		name string
		gate *Gate
	}{
		{"GitHub was unreachable", &Gate{
			Verify:    &verifierStub{err: errors.New("dial tcp: no route to host")},
			Allowlist: []string{"ana"},
		}},
		{"the operator stepped away from the knock", &Gate{
			Verify: &verifierStub{login: "ana"}, Allowlist: []string{"operator"},
			Admit: &approverStub{err: errors.New("stdin is closed")},
		}},
		{"no allowlist yet", &Gate{Verify: &verifierStub{login: "ana"}}},
	}
	for _, c := range mayWorkLater {
		t.Run(c.name, func(t *testing.T) {
			_, err := c.gate.Authorize(context.Background(), "tok")
			if err == nil {
				t.Fatal("want a failure")
			}
			if NotAllowed(err) {
				t.Errorf("err = %v, want it retryable: a blip must not end the huddle", err)
			}
		})
	}
}

// And the same distinction has to survive the wire, because it is the client
// that acts on it.
func TestAGitHubBlipDoesNotStopAClientRetrying(t *testing.T) {
	obs := &fakeObserver{}
	srv := newTestServer(obs)
	srv.Gate = &Gate{
		Verify:    &verifierStub{err: errors.New("503 Service Unavailable")},
		Allowlist: []string{"ana"},
	}
	addr, stop := startServer(t, srv)
	defer stop()

	err := Join(context.Background(), addr, "tok", &strings.Builder{})
	if err == nil {
		t.Fatal("want a failure")
	}
	if errors.Is(err, ErrRefused) {
		t.Errorf("error = %v, want a retryable failure — GitHub was merely unreachable", err)
	}
}

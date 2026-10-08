package live

import (
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

// recorder collects what a seat hears, so a test can wait for it.
type recorder struct {
	mu    sync.Mutex
	rooms []Frame
	chats []Frame
	saids []Frame
}

func (r *recorder) events() Events {
	return Events{
		Room: func(f Frame) { r.mu.Lock(); r.rooms = append(r.rooms, f); r.mu.Unlock() },
		Chat: func(f Frame) { r.mu.Lock(); r.chats = append(r.chats, f); r.mu.Unlock() },
		Said: func(f Frame) { r.mu.Lock(); r.saids = append(r.saids, f); r.mu.Unlock() },
	}
}

func (r *recorder) waitFor(t *testing.T, what string, ok func(*recorder) bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		done := ok(r)
		r.mu.Unlock()
		if done {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("never saw %s", what)
}

// The operator is in their own room: they see who comes, what is said, and
// they are named as the host — with nothing but a seat, no socket and no pane.
func TestTheHostHearsTheRoom(t *testing.T) {
	var srv *Server
	addr := roomServer(t, &fakeObserver{release: make(chan struct{})}, func(s *Server) { s.Host = "thomas"; srv = s })

	host := srv.Attach()
	defer host.Close()
	heard := &recorder{}
	go func() { _ = host.Run(heard.events()) }()

	ana := dialSilent(t, addr)
	defer ana.Close()
	if _, err := io.WriteString(ana, `{"type":"hello","token":"tok-a"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	heard.waitFor(t, "ana in the host's roster", func(r *recorder) bool {
		for _, f := range r.rooms {
			if f.You == "thomas" && f.Host == "thomas" && len(f.Members) == 2 {
				return true
			}
		}
		return false
	})

	if _, err := io.WriteString(ana, `{"type":"chat","text":"wait, not the migration"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	heard.waitFor(t, "ana's chat", func(r *recorder) bool {
		return len(r.chats) == 1 && r.chats[0].Author == "ana" && r.chats[0].Text == "wait, not the migration"
	})

	if _, err := io.WriteString(ana, `{"type":"typing","on":true}`+"\n"); err != nil {
		t.Fatal(err)
	}
	heard.waitFor(t, "ana typing", func(r *recorder) bool {
		if len(r.rooms) == 0 {
			return false
		}
		last := r.rooms[len(r.rooms)-1]
		return len(last.Typing) == 1 && last.Typing[0] == "ana"
	})
}

// The host talks to the room as themselves, and a joiner hears it attributed.
func TestTheHostCanTalkToTheRoom(t *testing.T) {
	var srv *Server
	addr := roomServer(t, &fakeObserver{release: make(chan struct{})}, func(s *Server) { s.Host = "thomas"; srv = s })

	ana := dialAs(t, addr, Frame{Token: "tok-a"})
	await(t, ana, "ana in the room", func(f Frame) bool { return f.Type == TypeRoom })

	host := srv.Attach()
	defer host.Close()
	go func() { _ = host.Run(Events{}) }()

	if err := host.Chat("one sec"); err != nil {
		t.Fatal(err)
	}
	got := await(t, ana, "the host's chat", func(f Frame) bool { return f.Type == TypeChat })
	if got.Author != "thomas" || got.Text != "one sec" {
		t.Errorf("chat = %q from %q, want %q from thomas", got.Text, got.Author, "one sec")
	}

	if err := host.Typing(true, true); err != nil {
		t.Fatal(err)
	}
	await(t, ana, "the host typing", func(f Frame) bool {
		return f.Type == TypeRoom && len(f.Typing) == 1 && f.Typing[0] == "thomas"
	})
}

// The host steers the agent from the agent's own pane. A second way in would
// put their words on the pull request as a joiner's, and nothing is ever typed
// into the pane from here.
func TestTheHostSeatNeverSteersTheAgent(t *testing.T) {
	var srv *Server
	roomServer(t, &fakeObserver{release: make(chan struct{})}, func(s *Server) { s.Host = "thomas"; srv = s })

	host := srv.Attach()
	defer host.Close()
	if err := host.Say("drop the users table"); !errors.Is(err, ErrHostCannotSteer) {
		t.Errorf("Say = %v, want ErrHostCannotSteer", err)
	}
	if n, _, _ := srv.Instructor.(*fakeInstructor).delivered(); n != 0 {
		t.Errorf("the agent was sent %d instruction(s), want none", n)
	}
}

// Leaving takes the seat out of the room: a host who closed their panel must
// not stay in the roster, or typing, forever.
func TestClosingTheHostSeatLeavesTheRoom(t *testing.T) {
	var srv *Server
	addr := roomServer(t, &fakeObserver{release: make(chan struct{})}, func(s *Server) { s.Host = "thomas"; srv = s })
	ana := dialAs(t, addr, Frame{Token: "tok-a"})

	host := srv.Attach()
	ran := make(chan error, 1)
	go func() { ran <- host.Run(Events{}) }()
	await(t, ana, "both in", func(f Frame) bool { return f.Type == TypeRoom && len(f.Members) == 2 })

	_ = host.Close()
	await(t, ana, "only ana", func(f Frame) bool { return f.Type == TypeRoom && len(f.Members) == 1 })
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Error("Run did not return after Close")
	}
}

// Writing to the room and writing to the agent are different news: one is
// somebody about to speak, the other is somebody about to steer.
func TestTheRoomTellsWhoIsWritingToWhom(t *testing.T) {
	var srv *Server
	addr := roomServer(t, &fakeObserver{release: make(chan struct{})}, func(s *Server) { s.Host = "thomas"; srv = s })
	host := srv.Attach()
	defer host.Close()
	heard := &recorder{}
	go func() { _ = host.Run(heard.events()) }()

	ana := dialSilent(t, addr)
	defer ana.Close()
	if _, err := io.WriteString(ana, `{"type":"hello","token":"tok-a"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	last := func(r *recorder) Frame {
		if len(r.rooms) == 0 {
			return Frame{}
		}
		return r.rooms[len(r.rooms)-1]
	}

	io.WriteString(ana, `{"type":"typing","on":true}`+"\n")
	heard.waitFor(t, "ana typing to the agent", func(r *recorder) bool {
		f := last(r)
		return len(f.Typing) == 1 && len(f.Chatting) == 0
	})

	io.WriteString(ana, `{"type":"typing","on":true,"to":"room"}`+"\n")
	heard.waitFor(t, "ana switching to the room", func(r *recorder) bool {
		f := last(r)
		return len(f.Typing) == 1 && len(f.Chatting) == 1 && f.Chatting[0] == "ana"
	})
}

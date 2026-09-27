package live

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/DnzzL/herdr-huddle/internal/thread"
)

func testSpool(t *testing.T) FileSpool {
	t.Helper()
	return FileSpool{Path: filepath.Join(t.TempDir(), "nested", "pending.json")}
}

// The regression this whole type exists for: a `serve` that goes down owing
// the thread must still owe it afterwards. The pull request is the artifact,
// and an instruction the agent carried out but the record never explains is a
// turn nobody can account for.
func TestAnOwedRecordSurvivesARestart(t *testing.T) {
	spool := testSpool(t)
	obs := &fakeObserver{release: make(chan struct{})}
	broken := &fakeLedger{err: errNoGitHub}

	// A server whose ledger is down takes an instruction and cannot record it.
	first := newTestServer(obs)
	first.Ledger, first.Spool = broken, spool
	addr, stop := startServer(t, first)

	conn := dialSilent(t, addr)
	if _, err := io.WriteString(conn, `{"type":"hello","token":"tok"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	reader := readerFor(conn)
	await(t, reader, "the room", func(f Frame) bool { return f.Type == TypeRoom })
	if _, err := io.WriteString(conn, `{"type":"say","text":"restart the worker"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	await(t, reader, "the delivery", func(f Frame) bool { return f.Type == TypeSaid && f.Status == StatusSent })

	waitFor(t, func() bool {
		owed, err := spool.Load()
		return err == nil && len(owed) == 1
	})
	conn.Close()
	stop()

	// It was written down, verbatim.
	owed, err := spool.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(owed) != 1 || owed[0].Text != "restart the worker" || owed[0].Author != "tester" {
		t.Fatalf("owed = %+v, want the instruction with its author", owed)
	}

	// A new server with a working ledger pays the debt without anyone typing
	// anything again.
	working := &fakeLedger{}
	second := newTestServer(obs)
	second.Ledger, second.Spool = working, spool
	second.LedgerInterval = 10 * time.Millisecond
	_, stop2 := startServer(t, second)
	defer stop2()

	waitFor(t, func() bool { return working.count() == 1 })
	posted := working.posted()
	if posted[0].Text != "restart the worker" {
		t.Errorf("posted %q, want the carried-over instruction", posted[0].Text)
	}

	// And it stops being owed, or the next restart posts it a second time.
	waitFor(t, func() bool {
		owed, err := spool.Load()
		return err == nil && len(owed) == 0
	})
}

// A missing file is the state before anything has ever failed, not a fault.
func TestAnEmptySpoolIsNotAnError(t *testing.T) {
	owed, err := testSpool(t).Load()
	if err != nil {
		t.Fatalf("Load errored on a file that was never written: %v", err)
	}
	if len(owed) != 0 {
		t.Errorf("owed = %v, want nothing", owed)
	}
}

// A queue that cannot be read is an error, never an empty one: starting from
// nothing would drop somebody's words without saying so.
func TestACorruptSpoolIsRefusedRatherThanEmptied(t *testing.T) {
	spool := testSpool(t)
	if err := os.MkdirAll(filepath.Dir(spool.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spool.Path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := spool.Load(); err == nil {
		t.Error("a queue that cannot be read must be an error, not an empty queue")
	}
}

// The records are what people typed at somebody's agent.
func TestTheSpoolIsNotWorldReadable(t *testing.T) {
	spool := testSpool(t)
	if err := spool.Save([]thread.LiveInstruction{{Author: "ana", Text: "ship it", At: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(spool.Path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("mode = %o, want it readable only by its owner", mode)
	}
}

// A server with no spool still works; it just forgets, which is what this
// used to do to everybody.
func TestAServerWithoutASpoolStillRuns(t *testing.T) {
	srv := newTestServer(&fakeObserver{})
	srv.Spool = nil
	if err := srv.serveGuard(); err != nil {
		t.Errorf("a spool must stay optional: %v", err)
	}
	srv.recover() // must not panic
}

var errNoGitHub = context.DeadlineExceeded

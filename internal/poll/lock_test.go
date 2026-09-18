package poll

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestAcquire_TakesTheLockOnceAndReleasesIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "poll.lock")

	release, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the lock: %v", err)
	}
	if got, want := strings.TrimSpace(string(raw)), strconv.Itoa(os.Getpid()); got != want {
		t.Errorf("lock file holds %q, want our pid %q", got, want)
	}

	release()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the lock file survived release: %v", err)
	}
	// Releasing twice must not panic: a deferred release and an error path can
	// both fire.
	release()
}

func TestAcquire_RefusesWhenAnotherPollerIsRunning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "poll.lock")
	// A pid that certainly exists and is not ours. Signalling it is what the
	// check does, so this is the real question: is the lock held by a live
	// process?
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getppid())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	release, err := Acquire(path)
	if err == nil {
		release()
		t.Fatal("Acquire took a lock held by a live process, which would start a second poller")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Errorf("err = %v, want it to say another poller is running", err)
	}
	// The holder's lock file must be left alone.
	raw, _ := os.ReadFile(path)
	if strings.TrimSpace(string(raw)) != strconv.Itoa(os.Getppid()) {
		t.Errorf("lock file = %q, want the existing holder's pid untouched", raw)
	}
}

func TestAcquire_TakesOverAStaleLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "poll.lock")
	// A pid that is certain not to exist: the maximum on Linux is far below
	// this, and a crash leaves exactly this behind.
	if err := os.WriteFile(path, []byte("4194304\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	release, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire refused a stale lock, so a crash would need a human: %v", err)
	}
	defer release()
	raw, _ := os.ReadFile(path)
	if strings.TrimSpace(string(raw)) != strconv.Itoa(os.Getpid()) {
		t.Errorf("lock file = %q, want it taken over", raw)
	}
}

func TestAcquire_IgnoresRubbishInTheLockFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "poll.lock")
	for _, content := range []string{"", "not a pid", "-1", "0"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		release, err := Acquire(path)
		if err != nil {
			t.Fatalf("Acquire refused a lock file containing %q: %v", content, err)
		}
		release()
	}
}

// The lock is not a security boundary, but it is a permission boundary: a lock
// file another user owns must not be silently overwritten.
func TestAcquire_ReportsAnUnwritableLockFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "poll.lock")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(path); err == nil {
		t.Fatal("want an error when the lock path cannot be written")
	}
}

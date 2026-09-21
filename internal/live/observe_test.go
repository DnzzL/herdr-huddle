package live

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeHerdr writes a stand-in for the herdr CLI. Herdr is the integration
// point, so the argument list and the process lifecycle are exactly what a
// script can verify without a Herdr session.
func fakeHerdr(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "herdr")
	// /bin/sh, not bash: this runs on NixOS, where /bin/bash does not exist.
	script := "#!/bin/sh\n" + body
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHerdrObserverAsksForTheRightStream(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	bin := fakeHerdr(t, `printf '%s\n' "$@" > `+argsFile+`
printf '%s\n' '{"bytes":"AAAA","encoding":"ansi","full":true,"seq":1,"type":"terminal.frame","width":80,"height":12}'
`)
	obs := &HerdrObserver{Bin: bin}
	stream, err := obs.Observe(context.Background(), "w16:p1", 80, 12)
	if err != nil {
		t.Fatalf("Observe errored: %v", err)
	}
	defer stream.Close()

	if err := Render(stream, &strings.Builder{}); err != nil {
		t.Errorf("Render errored: %v", err)
	}
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("the CLI was never run: %v", err)
	}
	got := strings.Fields(string(raw))
	want := []string{"terminal", "session", "observe", "w16:p1", "--cols", "80", "--rows", "12"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("args = %v, want %v", got, want)
	}
	if err := stream.Wait(); err != nil {
		t.Errorf("Wait = %v, want nil for a stream that ended cleanly", err)
	}
}

// A viewport is always requested explicitly: Herdr's own default is not a size
// this side can reason about.
func TestHerdrObserverPicksADefaultViewport(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	bin := fakeHerdr(t, `printf '%s\n' "$@" > `+argsFile+"\n")
	obs := &HerdrObserver{Bin: bin}
	stream, err := obs.Observe(context.Background(), "w1:p1", 0, 0)
	if err != nil {
		t.Fatalf("Observe errored: %v", err)
	}
	defer stream.Close()

	if err := Render(stream, &strings.Builder{}); err != nil {
		t.Fatalf("Render errored: %v", err)
	}
	raw, _ := os.ReadFile(argsFile)
	want := []string{"terminal", "session", "observe", "w1:p1", "--cols", strconv.Itoa(DefaultCols), "--rows", strconv.Itoa(DefaultRows)}
	if got := strings.Fields(string(raw)); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("args = %v, want %v", got, want)
	}
}

// A refused stream explains itself on stderr with exit 1, and that reason is
// the only thing a joiner can be told, so it must survive.
func TestHerdrObserverReportsWhyTheStreamFailed(t *testing.T) {
	bin := fakeHerdr(t, `printf '%s\n' '{"error":{"code":"pane_not_found","message":"no such pane"}}' >&2
exit 1
`)
	obs := &HerdrObserver{Bin: bin}
	stream, err := obs.Observe(context.Background(), "w1:p9", 80, 12)
	if err != nil {
		t.Fatalf("Observe errored: %v", err)
	}
	_ = Render(stream, &strings.Builder{})
	err = stream.Wait()
	if err == nil {
		t.Fatal("Wait must report the refused stream")
	}
	if !strings.Contains(err.Error(), "pane_not_found") {
		t.Errorf("Wait = %v, want the CLI's reason", err)
	}
}

// Closing a stream because its joiner left is not a failure, and must not be
// reported as one: the operator would otherwise see an error for every
// disconnect.
func TestHerdrObserverClosingEndsTheStreamQuietly(t *testing.T) {
	bin := fakeHerdr(t, `sleep 30
`)
	obs := &HerdrObserver{Bin: bin}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := obs.Observe(ctx, "w1:p1", 80, 12)
	if err != nil {
		t.Fatalf("Observe errored: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close errored: %v", err)
	}
	if err := stream.Wait(); err != nil {
		t.Errorf("Wait = %v, want nil after a deliberate Close", err)
	}
}

// A missing binary is a startup failure, not a stream that ends for no reason.
func TestHerdrObserverReportsAMissingBinary(t *testing.T) {
	obs := &HerdrObserver{Bin: filepath.Join(t.TempDir(), "not-herdr")}
	if _, err := obs.Observe(context.Background(), "w1:p1", 80, 12); err == nil {
		t.Error("Observe must fail when the CLI is not there")
	}
}

// A missing pane is a clean exit with a reason on stdout, not a failure — the
// fixture is what Herdr 0.9.0 actually prints.
func TestHerdrObserverRelaysAMissingPane(t *testing.T) {
	bin := fakeHerdr(t, `printf '%s\n' '{"reason":"terminal session observe failed: terminal target w99:p99 not found","type":"terminal.closed"}'
`)
	obs := &HerdrObserver{Bin: bin}
	stream, err := obs.Observe(context.Background(), "w99:p99", 80, 12)
	if err != nil {
		t.Fatalf("Observe errored: %v", err)
	}
	defer stream.Close()

	err = Render(stream, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "w99:p99") {
		t.Fatalf("Render = %v, want the reason Herdr gave", err)
	}
}

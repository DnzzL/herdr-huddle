package live

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeCloudflared stands in for the tunnel process: same output shape, same
// lifecycle, no network.
func fakeCloudflared(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cloudflared")
	// /bin/sh, not bash: NixOS has no /bin/bash.
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStartTunnelReturnsThePublicURL(t *testing.T) {
	bin := fakeCloudflared(t, `
printf '%s\n' '2026-09-22T10:00:00Z INF +--------------------------------------+'
printf '%s\n' '2026-09-22T10:00:00Z INF |  https://lobby-shepherd-ports.trycloudflare.com  |'
printf '%s\n' '2026-09-22T10:00:00Z INF +--------------------------------------+'
exec sleep 30
`)
	tunnel, err := StartTunnel(context.Background(), bin, "http://127.0.0.1:8791")
	if err != nil {
		t.Fatalf("StartTunnel errored: %v", err)
	}
	defer tunnel.Close()

	if want := "https://lobby-shepherd-ports.trycloudflare.com"; tunnel.URL != want {
		t.Errorf("URL = %q, want %q", tunnel.URL, want)
	}
}

// The URL is worthless after the process dies: a join line must not outlive
// the tunnel that serves it.
func TestTunnelCloseEndsTheChild(t *testing.T) {
	bin := fakeCloudflared(t, `
printf '%s\n' 'https://x-y-z.trycloudflare.com'
exec sleep 30
`)
	tunnel, err := StartTunnel(context.Background(), bin, "http://127.0.0.1:8791")
	if err != nil {
		t.Fatalf("StartTunnel errored: %v", err)
	}
	// Close must reap the child rather than wait for its own sleep: if it
	// blocked, this would take 30 seconds.
	started := time.Now()
	if err := tunnel.Close(); err != nil {
		t.Fatalf("Close errored: %v", err)
	}
	if waited := time.Since(started); waited > 5*time.Second {
		t.Errorf("Close took %s: it waited for the child instead of ending it", waited.Round(time.Millisecond))
	}
	select {
	case <-tunnel.done:
	default:
		t.Fatal("Close returned while the child was still running")
	}
}

// A tunnel that never comes up must say why, with its own output, rather than
// leaving the operator watching a silent terminal.
func TestStartTunnelReportsAProcessThatDiesFirst(t *testing.T) {
	bin := fakeCloudflared(t, `
printf '%s\n' 'ERR Failed to build tunnel: no route to origin' >&2
exit 1
`)
	_, err := StartTunnel(context.Background(), bin, "http://127.0.0.1:8791")
	if err == nil {
		t.Fatal("a tunnel that exits without a URL must fail")
	}
	if !strings.Contains(err.Error(), "no route to origin") {
		t.Errorf("error = %v, want the child's own reason", err)
	}
	if strings.Contains(err.Error(), "%!") {
		t.Errorf("error = %v, must not contain a malformed-format artefact", err)
	}
}

// The operator not having cloudflared is a different problem from cloudflared
// failing: one needs an install line, the other does not.
func TestStartTunnelReportsAMissingBinary(t *testing.T) {
	// Both shapes are exercised: a path that is not there, and a bare name
	// that PATH cannot resolve — production uses the second.
	for _, bin := range []string{filepath.Join(t.TempDir(), "nope"), "no-such-cloudflared-here"} {
		_, err := StartTunnel(context.Background(), bin, "http://127.0.0.1:8791")
		if !errors.Is(err, ErrTunnelBinaryMissing) {
			t.Errorf("bin %q: error = %v, want ErrTunnelBinaryMissing so the caller can offer the install hint", bin, err)
		}
	}
}

// The binary comes from the environment first, like every other child process
// in this tool, so a machine without it on PATH can still be tested.
func TestStartTunnelTakesTheBinaryFromTheEnvironment(t *testing.T) {
	bin := fakeCloudflared(t, `printf '%s\n' 'https://env-override.trycloudflare.com'`)
	t.Setenv("HERDR_HUDDLE_CLOUDFLARED", bin)
	tunnel, err := StartTunnel(context.Background(), "", "http://127.0.0.1:8791")
	if err != nil {
		t.Fatalf("StartTunnel errored: %v", err)
	}
	defer tunnel.Close()
	if !strings.Contains(tunnel.URL, "env-override") {
		t.Errorf("URL = %q, want the environment's binary to have been used", tunnel.URL)
	}
}

// Ctrl-C or a retired share must take the tunnel with it.
func TestStartTunnelStopsWithTheContext(t *testing.T) {
	bin := fakeCloudflared(t, `
printf '%s\n' 'https://ctx-bound.trycloudflare.com'
exec sleep 30
`)
	ctx, cancel := context.WithCancel(context.Background())
	tunnel, err := StartTunnel(ctx, bin, "http://127.0.0.1:8791")
	if err != nil {
		t.Fatalf("StartTunnel errored: %v", err)
	}
	defer tunnel.Close()

	cancel()
	select {
	case <-tunnel.done:
	case <-time.After(3 * time.Second):
		t.Fatal("the cloudflared child outlived its context")
	}
}

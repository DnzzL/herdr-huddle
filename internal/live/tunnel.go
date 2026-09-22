package live

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ErrTunnelBinaryMissing reports that cloudflared is not installed. The CLI
// turns it into the one install line that fixes it; a tunnel that failed for
// any other reason is a different problem and says so itself.
var ErrTunnelBinaryMissing = errors.New("live: cloudflared was not found")

// quickTunnelURL is the address cloudflared prints for a quick tunnel.
var quickTunnelURL = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)

// Tunnel is a running cloudflared quick tunnel.
//
// `serve` owns it: the URL exists only while this child lives, which is what
// keeps a public address from outliving the session that created it
// (ADR-006 — measured: after the child exits the hostname answers 530).
type Tunnel struct {
	URL string

	cmd  *exec.Cmd
	done chan struct{}
	// waitErr is the child's exit status, written before done is closed and
	// so safe to read once done is received.
	waitErr error
}

// StartTunnel runs `cloudflared tunnel --url <local>` and returns once the
// public URL is printed.
//
// bin "" means HERDR_HUDDLE_CLOUDFLARED, then cloudflared from PATH — the same
// convention as every other child process this tool starts. A missing binary
// is reported as exec.ErrNotFound so the caller can tell "install this" apart
// from "this failed".
func StartTunnel(ctx context.Context, bin, localURL string) (*Tunnel, error) {
	if bin == "" {
		bin = os.Getenv("HERDR_HUDDLE_CLOUDFLARED")
	}
	if bin == "" {
		bin = "cloudflared"
	}

	// cloudflared prints its URL to either stream depending on version, so
	// both are read as one. This is an os.Pipe rather than an io.Pipe
	// deliberately: with an io.Pipe exec runs a copying goroutine and Wait
	// blocks until *every* holder of the child's stdout is gone — a
	// grandchild that outlives the tunnel would stall Close for as long as it
	// lives. An *os.File is handed to the child directly, so Wait returns
	// when the tunnel process itself exits.
	rFile, wFile, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("live: pipe for the tunnel: %w", err)
	}
	cmd := exec.CommandContext(ctx, bin, "tunnel", "--url", localURL)
	cmd.Stdout, cmd.Stderr = wFile, wFile
	if err := cmd.Start(); err != nil {
		_ = rFile.Close()
		_ = wFile.Close()
		// Two shapes mean the same thing to the caller: a bare name not on
		// PATH (exec.ErrNotFound) and a path that does not exist (ENOENT).
		// One sentinel keeps "install this" from becoming two checks.
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrTunnelBinaryMissing, bin)
		}
		return nil, fmt.Errorf("live: start %s: %w", bin, err)
	}
	_ = wFile.Close() // the child holds its own reference now
	tunnel := &Tunnel{cmd: cmd, done: make(chan struct{})}

	urlCh := make(chan string, 1)
	scanDone := make(chan struct{})
	var tail tailBuffer
	go func() {
		defer close(scanDone)
		defer rFile.Close()
		scanner := bufio.NewScanner(rFile)
		for scanner.Scan() {
			line := scanner.Text()
			tail.add(line)
			if match := quickTunnelURL.FindString(line); match != "" {
				select {
				case urlCh <- match:
				default: // already reported; keep reading so the pipe drains
				}
			}
		}
	}()
	go func() {
		tunnel.waitErr = cmd.Wait()
		close(tunnel.done)
	}()

	select {
	case url := <-urlCh:
		tunnel.URL = url
		return tunnel, nil
	case <-tunnel.done:
		// The child printed and exited: wait a breath so its own reason is in
		// the buffer rather than lost to scheduling.
		select {
		case <-scanDone:
		case <-time.After(200 * time.Millisecond):
		}
		if tunnel.waitErr == nil {
			return nil, fmt.Errorf("live: %s exited without printing a tunnel URL%s", bin, tail.String())
		}
		return nil, fmt.Errorf("live: %s exited before its tunnel was up: %v%s", bin, tunnel.waitErr, tail.String())
	case <-ctx.Done():
		return nil, fmt.Errorf("live: starting the tunnel for %s: %w", localURL, ctx.Err())
	}
}

// Close ends the tunnel, which ends the public URL with it. Safe to call
// twice.
func (t *Tunnel) Close() error {
	if t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
	}
	<-t.done
	return nil
}

// tailBuffer keeps the child's last lines for a failure report: the reason a
// tunnel did not come up is on its own output, and the operator should not
// have to guess.
type tailBuffer struct {
	mu    sync.Mutex
	lines []string
}

const tailLines = 20

func (b *tailBuffer) add(line string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines = append(b.lines, line)
	if len(b.lines) > tailLines {
		b.lines = b.lines[len(b.lines)-tailLines:]
	}
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.lines) == 0 {
		return ""
	}
	return "\n" + strings.Join(b.lines, "\n")
}

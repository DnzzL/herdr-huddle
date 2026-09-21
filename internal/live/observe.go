package live

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/DnzzL/herdr-huddle/internal/herdr"
)

// DefaultViewport is the size a stream is rendered at when the caller does not
// choose one. It is a compromise: the pane decides what it actually paints, and
// these are the dimensions a remote terminal is most likely to fit.
const (
	DefaultCols = 100
	DefaultRows = 30
)

// HerdrObserver starts streams by running the herdr CLI, which is the same
// integration point every other package here uses.
//
// `herdr terminal session observe` is read-only by construction: it takes no
// input, no resize, no scroll and no takeover, and several observers may watch
// one pane. That property is the reason ADR-005 could make the collaborator's
// view one-way without asking anything of Herdr's policy — Herdr has none.
type HerdrObserver struct {
	// Bin overrides the herdr executable. Empty means HERDR_BIN_PATH, then the
	// same default every other package runs.
	Bin string
}

func (h *HerdrObserver) bin() string {
	if h.Bin != "" {
		return h.Bin
	}
	if v := os.Getenv("HERDR_BIN_PATH"); v != "" {
		return v
	}
	return herdr.DefaultBin
}

// Observe starts watching one pane. The caller must Close the stream.
func (h *HerdrObserver) Observe(ctx context.Context, pane string, cols, rows int) (Stream, error) {
	if pane == "" {
		return nil, errors.New("live: a pane is required to observe")
	}
	if cols <= 0 {
		cols = DefaultCols
	}
	if rows <= 0 {
		rows = DefaultRows
	}

	args := []string{
		"terminal", "session", "observe", pane,
		"--cols", strconv.Itoa(cols),
		"--rows", strconv.Itoa(rows),
	}
	cmd := exec.CommandContext(ctx, h.bin(), args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("live: observe %s: %w", pane, err)
	}
	// The CLI reports a failure as JSON on stderr with exit 1, so stderr is
	// kept: it is the only place the reason for a refused stream appears.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("live: run %s terminal session observe: %w", h.bin(), err)
	}
	return &processStream{cmd: cmd, stdout: stdout, stderr: &stderr}, nil
}

// processStream is a running observe command.
//
// Close and Wait may both be called, in either order — the server closes the
// stream when a joiner leaves and then asks how it ended — so the reaping
// happens exactly once.
type processStream struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr *bytes.Buffer

	once    sync.Once
	waitErr error
}

func (p *processStream) Read(b []byte) (int, error) { return p.stdout.Read(b) }

func (p *processStream) Close() error {
	_ = p.stdout.Close()
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	_ = p.Wait()
	return nil
}

func (p *processStream) Wait() error {
	p.once.Do(func() { p.waitErr = p.reap() })
	return p.waitErr
}

func (p *processStream) reap() error {
	err := p.cmd.Wait()
	if err == nil {
		return nil
	}
	// A killed child is not a failure to report: it means the joiner went away
	// and the stream was closed on purpose. ExitCode is negative for a process
	// that was signalled — and note that such a process has not "exited", so
	// asking for Exited first would get this exactly wrong.
	if p.cmd.ProcessState != nil && p.cmd.ProcessState.ExitCode() < 0 {
		return nil
	}
	if reason := strings.TrimSpace(p.stderr.String()); reason != "" {
		return fmt.Errorf("live: herdr terminal session observe: %s", oneLine([]byte(reason)))
	}
	return fmt.Errorf("live: herdr terminal session observe: %w", err)
}

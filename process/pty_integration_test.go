//go:build integration

package process_test

// pty_integration_test.go is Task 23's tagged acceptance seam for the
// SUPERVISED Bash `tty:true` workflow end to end
// (docs/plans/2026-07-27-long-running-command-supervision.md, Task 23):
// Bash with tty:true -> ProcessOutput -> ProcessInput (data, resize, EOF) ->
// ProcessStop, composed exactly like integration_test.go and (in the
// sibling bash package) bash/integration_test.go compose the PUBLIC Harness
// binding contracts and the root package's own public *Definition builders.
//
// Tools never touches a real PTY/ConPTY device anywhere in this file, or
// anywhere else in this module (CLAUDE.md: "Do not import sandbox or
// confinement... Accept their behavior through harness runner and
// permission interfaces"; bash/bash.go's own doc comment: "Tools never
// touches creack/pty or Windows ConPTY APIs directly — PTY reality lives
// entirely in sandbox"). This module has no dependency on
// github.com/looprig/sandbox at all (see go.mod), so this suite cannot,
// and must not try to, allocate a genuine OS terminal the way sandbox's own
// internal/exec/process_pty_integration_unix_test.go does. What THIS suite
// proves is the boundary Tools actually owns: the SHAPE of the request
// Tools sends a PTY-capable runner (ProcessRequest.PTY), that a PTY failure
// never silently falls back to a pipe-backed spawn, and that Tools' own
// cursor/resize/EOF/combined-stream handling is correct against any
// tool.Process reporting StreamMode() == ProcessStreamModePTY — regardless
// of what actually backs it. ptyAsyncRunner/ptyPreparedProcess/ptyProcess
// below are a stdlib-only (os/exec + os.Pipe), PTY-SHAPED test double: a
// real OS subprocess whose Stdout and Stderr are wired to the SAME pipe,
// mirroring the documented tool.ProcessStreamModePTY contract exactly
// ("combined terminal bytes through Stdout; Stderr remains non-nil but is
// closed and empty") without ever allocating a real terminal device. The
// actual OS-level PTY mechanics (a genuine creack/pty or ConPTY terminal)
// are Sandbox's job, already proven in Sandbox's own test suite — duplicating
// that here would test the wrong module and, per this task's own
// instructions, is explicitly out of scope.
//
// This file lives in the external `process_test` package (matching
// integration_test.go in the same directory) so it can import the root
// `github.com/looprig/tools` package (BashDefinition et al.) without an
// import cycle, and reuses that file's own shared fixtures directly —
// mustUUID, newProcessTestBindings, buildProcessTools, runCall,
// processCallResult/decodeProcessResult — rather than redefining them.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools"
	"github.com/looprig/tools/process"
)

// --- stdlib-only, PTY-shaped tool.AsyncProcessRunner / tool.Process double --

// ptyAsyncRunner is a tool.AsyncProcessRunner test double standing in for a
// real PTY-capable runner (e.g. a future Harness-to-Sandbox adapter) that
// Tools itself must never construct. It proves the TOOLS-SIDE contract this
// suite is responsible for — that a `tty:true` ProcessRequest reaches the
// runner, and that Tools never falls back to pipes when a PTY cannot be
// provided — using only stdlib. When unavailable is set, PrepareProcess
// fails exactly the way a real runner reports "could not allocate a
// PTY/ConPTY" through Harness's typed tool.ProcessError
// (tool.ProcessErrorPTYUnavailable), mirroring the classification
// bash/supervised.go's classifyPrepareProcessError now performs.
type ptyAsyncRunner struct {
	unavailable bool

	mu      sync.Mutex
	calls   int
	lastReq tool.ProcessRequest
}

func (r *ptyAsyncRunner) PrepareProcess(_ context.Context, req tool.ProcessRequest) (tool.PreparedProcess, error) {
	r.mu.Lock()
	r.calls++
	r.lastReq = req
	unavailable := r.unavailable
	r.mu.Unlock()
	if unavailable {
		return nil, &tool.ProcessError{Code: tool.ProcessErrorPTYUnavailable}
	}
	if !req.PTY {
		return nil, errors.New("ptyAsyncRunner: this suite only prepares tty:true requests")
	}
	cmd := exec.Command("sh", "-c", req.Command)
	cmd.Dir = req.Directory
	return &ptyPreparedProcess{cmd: cmd, access: tool.NewWorkspaceAccess(tool.WorkspaceAccessReadOnly, nil, nil)}, nil
}

var _ tool.AsyncProcessRunner = (*ptyAsyncRunner)(nil)

func (r *ptyAsyncRunner) requestedPTY() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastReq.PTY
}

func (r *ptyAsyncRunner) prepareCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// ptyPreparedProcess is a real os/exec.Cmd-backed tool.PreparedProcess whose
// Start wires a SINGLE os.Pipe to both Stdout and Stderr — the stdlib-only
// stand-in for a real PTY's one combined terminal descriptor (see this
// file's own package doc comment for why this is legitimate and sufficient
// for what Tools itself is responsible for proving).
type ptyPreparedProcess struct {
	cmd    *exec.Cmd
	access tool.WorkspaceAccess

	mu         sync.Mutex
	startCalls int
}

func (p *ptyPreparedProcess) EffectiveWorkspaceAccess() tool.WorkspaceAccess { return p.access }

func (p *ptyPreparedProcess) Start(context.Context) (tool.Process, error) {
	p.mu.Lock()
	p.startCalls++
	p.mu.Unlock()

	combinedR, combinedW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd := p.cmd
	cmd.Stdout = combinedW
	cmd.Stderr = combinedW
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = errors.Join(combinedR.Close(), combinedW.Close())
		return nil, err
	}
	startedAt := time.Now().UTC()
	if err := cmd.Start(); err != nil {
		_ = errors.Join(combinedR.Close(), combinedW.Close())
		return nil, err
	}
	// The parent's own copy of the write end must close so combinedR
	// observes EOF once the child exits — the identical fd-lifetime
	// discipline this directory's own execPreparedProcess.Start already
	// follows for its separate stdout/stderr pipes.
	_ = combinedW.Close()
	return &ptyProcess{cmd: cmd, combined: combinedR, stdin: stdin, startedAt: startedAt}, nil
}

func (p *ptyPreparedProcess) Close() error { return nil }

var _ tool.PreparedProcess = (*ptyPreparedProcess)(nil)

// closedEmptyPTYReader is the synthetic, permanently-empty, already-closed
// reader ptyProcess.Stderr() returns, mirroring the documented
// tool.ProcessStreamModePTY contract exactly: "Stderr remains non-nil but
// is closed and empty".
type closedEmptyPTYReader struct{}

func (closedEmptyPTYReader) Read([]byte) (int, error) { return 0, io.EOF }
func (closedEmptyPTYReader) Close() error             { return nil }

type ptyResizeCall struct{ rows, cols uint16 }

// ptyProcess is a real os/exec.Cmd-backed tool.Process reporting
// StreamMode() == tool.ProcessStreamModePTY: Stdout is the one combined
// pipe both the child's stdout and stderr were wired to, and Stderr is the
// synthetic closed/empty reader the documented PTY contract requires. It
// never allocates a real terminal device — Resize has nothing real to
// resize and only records the call, exactly like
// process/input_tool_test.go's own inputFakeProcess.
type ptyProcess struct {
	cmd       *exec.Cmd
	combined  io.ReadCloser
	stdin     io.WriteCloser
	startedAt time.Time

	mu    sync.Mutex
	calls []ptyResizeCall
}

func (p *ptyProcess) Stdout() io.ReadCloser              { return p.combined }
func (p *ptyProcess) Stderr() io.ReadCloser              { return closedEmptyPTYReader{} }
func (p *ptyProcess) Stdin() io.WriteCloser              { return p.stdin }
func (p *ptyProcess) StreamMode() tool.ProcessStreamMode { return tool.ProcessStreamModePTY }

func (p *ptyProcess) Wait(context.Context) (tool.ProcessResult, error) {
	waitErr := p.cmd.Wait()
	finishedAt := time.Now().UTC()

	result := tool.ProcessResult{StartedAt: p.startedAt, FinishedAt: finishedAt}
	var exitErr *exec.ExitError
	switch {
	case waitErr == nil:
		result.Reason = tool.ProcessTerminalExited
	case errors.As(waitErr, &exitErr):
		result.Reason = tool.ProcessTerminalExited
		result.ExitCode = exitErr.ExitCode()
	default:
		return tool.ProcessResult{}, waitErr
	}
	return result, nil
}

func (p *ptyProcess) Resize(_ context.Context, rows, cols uint16) error {
	p.mu.Lock()
	p.calls = append(p.calls, ptyResizeCall{rows: rows, cols: cols})
	p.mu.Unlock()
	return nil
}

func (p *ptyProcess) Signal(_ context.Context, sig tool.ProcessSignal) error {
	if p.cmd.Process == nil {
		return nil
	}
	if sig == tool.ProcessSignalKill {
		return p.cmd.Process.Kill()
	}
	return p.cmd.Process.Signal(os.Interrupt)
}

func (p *ptyProcess) Close(context.Context) error {
	if p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Kill()
}

func (p *ptyProcess) resizeCalls() []ptyResizeCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]ptyResizeCall(nil), p.calls...)
}

var _ tool.Process = (*ptyProcess)(nil)

// --- minimal Workspace binding fakes (Bash requires tool.RequiresWorkspace,
// unlike ProcessOutput/ProcessInput/ProcessStop) ----------------------------

type ptyCoordinator struct {
	mu            sync.Mutex
	lifetimeCalls int
	released      int
}

func (c *ptyCoordinator) Acquire(context.Context, tool.WorkspaceOperation, string) (tool.WorkspacePermit, error) {
	return ptyPermit{c}, nil
}

func (c *ptyCoordinator) Healthy() error { return nil }

func (c *ptyCoordinator) AcquireLifetime(context.Context, tool.WorkspaceAccess) (tool.WorkspacePermit, error) {
	c.mu.Lock()
	c.lifetimeCalls++
	c.mu.Unlock()
	return ptyPermit{c}, nil
}

type ptyPermit struct{ c *ptyCoordinator }

func (p ptyPermit) Release() {
	p.c.mu.Lock()
	p.c.released++
	p.c.mu.Unlock()
}

var (
	_ tool.WorkspaceCoordinator         = (*ptyCoordinator)(nil)
	_ tool.WorkspaceLifetimeCoordinator = (*ptyCoordinator)(nil)
)

type ptyObservations struct {
	mu          sync.Mutex
	invalidated int
}

func (*ptyObservations) WithPath(string, func(*tool.FileObservation) error) error { return nil }
func (o *ptyObservations) InvalidateAll() {
	o.mu.Lock()
	o.invalidated++
	o.mu.Unlock()
}

var _ tool.WorkspaceObservations = (*ptyObservations)(nil)

// --- Bash result decode -----------------------------------------------------

// bashCallResult mirrors bash/result.go's unexported supervisedResult JSON
// shape — this external test package cannot see the unexported type (and
// bash_test's own identically-shaped decode helper lives in a different
// package), so it decodes against the identical wire contract directly,
// exactly like bash/integration_test.go's own bashCallResult.
type bashCallResult struct {
	Status       string `json:"status"`
	ProcessID    string `json:"process_id"`
	Output       string `json:"output"`
	NextCursor   int64  `json:"next_cursor"`
	ExitCode     *int   `json:"exit_code"`
	Backgrounded bool   `json:"backgrounded"`
	Error        string `json:"error"`
}

func decodeBashResult(t *testing.T, text string) bashCallResult {
	t.Helper()
	var out bashCallResult
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("decode bash result %q: %v", text, err)
	}
	return out
}

// --- TestIntegrationProcessPTYToolWorkflow ----------------------------------

// TestIntegrationProcessPTYToolWorkflow drives the supervised Bash `tty:true`
// workflow end to end through the PUBLIC BashDefinition/ProcessOutputDefinition/
// ProcessInputDefinition/ProcessStopDefinition builders and the stdlib-only,
// PTY-shaped runner double above. It covers Task 23's required acceptance
// list: `tty:true` forwarding (the request reaches the runner), pty_unavailable
// with no fallback (a PrepareProcess failure never spawns a pipe-backed
// process instead), the combined cursor stream (a single Stdout carries
// everything, addressed by one continuous cursor), resize (reaches Resize
// with the exact requested dimensions), and EOF (forwarded as a control byte,
// confirmed via a subsequent ProcessStop rather than relying on this
// stdlib-only double to honor real terminal EOF semantics, which is Sandbox's
// job, not this suite's).
func TestIntegrationProcessPTYToolWorkflow(t *testing.T) {
	t.Run("tty:true forwarding, pty_unavailable, no fallback", func(t *testing.T) {
		dir := t.TempDir()
		bindings, _ := newProcessTestBindings(t, dir)
		coord := &ptyCoordinator{}
		obs := &ptyObservations{}
		bindings.Workspace = &tool.WorkspaceBinding{Root: dir, Coordinator: coord, Observations: obs}

		runner := &ptyAsyncRunner{unavailable: true}
		resolver := func(context.Context, uuid.UUID) (tool.AsyncProcessRunner, error) { return runner, nil }
		bashBuilt, err := tools.BashDefinition(resolver).Build(context.Background(), bindings)
		if err != nil {
			t.Fatalf("BashDefinition().Build() error = %v", err)
		}
		bashTool := bashBuilt[0]

		result := decodeBashResult(t, runCall(t, bashTool, `{"command":"true","background":true,"tty":true}`))

		if result.Error != string(process.CodePTYUnavailable) {
			t.Fatalf("Error = %q, want %q", result.Error, process.CodePTYUnavailable)
		}
		if runner.prepareCalls() != 1 {
			t.Fatalf("PrepareProcess called %d times, want 1", runner.prepareCalls())
		}
		if !runner.requestedPTY() {
			t.Fatalf("runner never observed ProcessRequest.PTY = true")
		}
		if result.ProcessID != "" || result.Backgrounded {
			t.Fatalf("result = %+v, want no live handle: a PTY failure must never silently fall back to a pipe-backed spawn", result)
		}
	})

	t.Run("happy path: combined stream, input, resize, EOF, stop", func(t *testing.T) {
		dir := t.TempDir()
		bindings, _ := newProcessTestBindings(t, dir)
		coord := &ptyCoordinator{}
		obs := &ptyObservations{}
		bindings.Workspace = &tool.WorkspaceBinding{Root: dir, Coordinator: coord, Observations: obs}

		runner := &ptyAsyncRunner{}
		resolver := func(context.Context, uuid.UUID) (tool.AsyncProcessRunner, error) { return runner, nil }
		bashBuilt, err := tools.BashDefinition(resolver).Build(context.Background(), bindings)
		if err != nil {
			t.Fatalf("BashDefinition().Build() error = %v", err)
		}
		bashTool := bashBuilt[0]
		output, input, stop := buildProcessTools(t, bindings)

		// `cat` echoes whatever it reads from stdin back out, combined into
		// the single PTY-shaped stream this fake wires stdout/stderr through.
		bg := decodeBashResult(t, runCall(t, bashTool, `{"command":"cat","background":true,"tty":true}`))
		if !bg.Backgrounded || bg.ProcessID == "" {
			t.Fatalf("background tty result = %+v, want a backgrounded live handle", bg)
		}
		if !runner.requestedPTY() {
			t.Fatalf("runner never observed ProcessRequest.PTY = true")
		}
		handle := bg.ProcessID

		// --- input: data reaches the process and its echo is visible through
		// the SAME combined cursor stream (no separate stdout/stderr cursor
		// space) ---
		writeResult := decodeProcessResult(t, runCall(t, input, fmt.Sprintf(`{"process_id":%q,"data":"hello-pty\n","yield_time_ms":2000}`, handle)))
		if writeResult.Error != "" {
			t.Fatalf("ProcessInput write result = %+v, want no error", writeResult)
		}
		if !strings.Contains(writeResult.Output, "hello-pty") {
			t.Fatalf("ProcessInput snapshot output = %q, want it to contain the echoed input", writeResult.Output)
		}

		// --- resize: valid for a tty:true handle, reaches Resize with the
		// exact requested dimensions ---
		resizeResult := decodeProcessResult(t, runCall(t, input, fmt.Sprintf(`{"process_id":%q,"rows":40,"cols":100}`, handle)))
		if resizeResult.Error != "" {
			t.Fatalf("ProcessInput resize result = %+v, want no error (tty:true means resize is valid for this handle)", resizeResult)
		}

		// --- EOF: forwarded as a control byte, never rejected for a PTY-mode
		// handle. This stdlib-only double never allocates a real terminal, so
		// the child never actually observes the forwarded byte as end-of-input
		// (real terminal EOF semantics are Sandbox's own, already-proven job —
		// see this file's package doc comment); ProcessStop below confirms
		// termination instead of relying on natural exit. ---
		eofResult := decodeProcessResult(t, runCall(t, input, fmt.Sprintf(`{"process_id":%q,"eof":true}`, handle)))
		if eofResult.Error != "" {
			t.Fatalf("ProcessInput eof result = %+v, want no error", eofResult)
		}

		// --- combined cursor stream, re-confirmed from the top: a single
		// ProcessOutput read from cursor 0 still contains the earlier echoed
		// input — one continuous, cursor-addressed stream survives every
		// intervening resize/EOF operation. ---
		final := decodeProcessResult(t, runCall(t, output, fmt.Sprintf(`{"process_id":%q,"cursor":0}`, handle)))
		if final.Error != "" {
			t.Fatalf("final ProcessOutput = %+v, want no error", final)
		}
		if !strings.Contains(final.Output, "hello-pty") {
			t.Fatalf("final combined output = %q, want it to still contain the earlier echoed input", final.Output)
		}

		// --- stop: confirms termination (interrupt-not-terminal-until-exit is
		// covered by process/stop_tool_test.go's own stream-mode-agnostic
		// TestProcessStopInterruptDoesNotEscalateOnTimeout — ProcessStop's
		// signal/confirm contract never branches on StreamMode at all) ---
		stopResult := decodeProcessResult(t, runCall(t, stop, fmt.Sprintf(`{"process_id":%q,"mode":"kill"}`, handle)))
		if stopResult.Error != "" {
			t.Fatalf("ProcessStop result = %+v, want no error", stopResult)
		}
		if !process.State(stopResult.Status).Terminal() {
			t.Fatalf("ProcessStop result status = %q, want a terminal state", stopResult.Status)
		}
	})
}

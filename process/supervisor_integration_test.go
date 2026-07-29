//go:build integration

package process

// This file is the Task 9 filesystem/subprocess acceptance seam
// (docs/plans/2026-07-27-long-running-command-supervision.md, Task 9's "9D
// -- filesystem/subprocess acceptance"). It starts here in 9B with
// TestSupervisorIntegrationPersistRestore, the 9B boundary; 9C adds
// TestSupervisorIntegrationShutdownAndRestore below, reusing
// execPreparedProcess/execProcess for its own coordinated-shutdown-then-
// restore scenario. Per the plan's phase-boundary-only execution override,
// this tagged suite is written now but its execution is deferred to Phase
// Gate 2 -- it is never run by a task/microtask agent.

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/tool"
)

// execPreparedProcess and execProcess are a real, OS-subprocess-backed
// implementation of tool.PreparedProcess/tool.Process, built directly on
// os/exec and its real OS pipes -- deliberately not fake_process_test.go's
// in-memory fakePreparedProcess/fakeProcess (those exist so unit tests never
// need a real OS process at all). The combined-acceptance text for this
// integration file calls for "a real temp resource root, OS pipes, ... a
// subprocess fake": a subprocess fake that wraps a real *exec.Cmd is exactly
// that -- fake only in the sense of never going through a real
// tool.AsyncProcessRunner/Harness Sandbox adapter, which does not exist in
// this module.
//
// Resize is minimal: no test in this file calls it. Signal started minimal
// too in 9B (TestSupervisorIntegrationPersistRestore never calls it --
// restore reconciliation never signals a live process at all, see
// restore.go) and was upgraded in 9C to a fuller, portable-signal-aware
// implementation for TestSupervisorIntegrationShutdownAndRestore -- see
// execProcess.Signal's own doc comment below.
type execPreparedProcess struct {
	cmd    *exec.Cmd
	access tool.WorkspaceAccess
}

func (p *execPreparedProcess) EffectiveWorkspaceAccess() tool.WorkspaceAccess {
	return p.access
}

func (p *execPreparedProcess) Start(context.Context) (tool.Process, error) {
	stdout, err := p.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := p.cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	stdin, err := p.cmd.StdinPipe()
	if err != nil {
		return nil, err
	}

	startedAt := time.Now().UTC()
	if err := p.cmd.Start(); err != nil {
		return nil, err
	}

	return &execProcess{cmd: p.cmd, stdout: stdout, stderr: stderr, stdin: stdin, startedAt: startedAt}, nil
}

// Close releases an unstarted preparation. execPreparedProcess reserves
// nothing of its own before Start (the real reservation, once this module
// has a real AsyncProcessRunner, belongs to that runner), so Close is a
// no-op, idempotent by construction.
func (p *execPreparedProcess) Close() error { return nil }

var _ tool.PreparedProcess = (*execPreparedProcess)(nil)

type execProcess struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr io.ReadCloser
	stdin  io.WriteCloser

	startedAt time.Time
}

func (p *execProcess) Stdout() io.ReadCloser { return p.stdout }
func (p *execProcess) Stderr() io.ReadCloser { return p.stderr }
func (p *execProcess) Stdin() io.WriteCloser { return p.stdin }

func (p *execProcess) StreamMode() tool.ProcessStreamMode { return tool.ProcessStreamModePipes }

func (p *execProcess) Wait(context.Context) (tool.ProcessResult, error) {
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

func (p *execProcess) Resize(context.Context, uint16, uint16) error { return nil }

// Signal forwards a portable signal request to the real OS process. Task 9C
// upgrades this from 9B's always-Kill placeholder (every Signal call used to
// force-kill regardless of which portable signal was requested -- see this
// type's original doc comment, which flagged "a more faithful ...
// escalation is 9C's coordinated-shutdown concern") to actually distinguish
// a graceful request from a forceful one, so
// TestSupervisorIntegrationShutdownAndRestore can exercise
// Supervisor.Shutdown's real terminate-then-escalate ordering against a
// genuine OS process tree. tool.ProcessSignalKill maps to Process.Kill
// (SIGKILL); every other portable signal maps to os.Interrupt, the only
// other signal value the os package guarantees is portable across
// platforms (os.Process.Signal's doc comment) -- this module has no
// dependency on the syscall package's platform-specific signal constants.
func (p *execProcess) Signal(_ context.Context, sig tool.ProcessSignal) error {
	if p.cmd.Process == nil {
		return nil
	}
	if sig == tool.ProcessSignalKill {
		return p.cmd.Process.Kill()
	}
	return p.cmd.Process.Signal(os.Interrupt)
}

func (p *execProcess) Close(context.Context) error {
	if p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Kill()
}

var _ tool.Process = (*execProcess)(nil)

// --- TestSupervisorIntegrationPersistRestore ---

// TestSupervisorIntegrationPersistRestore covers the 9B restore boundary end
// to end against real infrastructure: a real temp resource root, real OS
// pipes draining a real `sh -c` subprocess (mirroring bash.go's own
// documented sh -c exception), a real ManifestStore/Spool pair, and a
// from-scratch Supervisor simulating a session restore with no live process
// handles at all. Only the Harness lifecycle/notification publisher is
// faked (fakeLifecycleSink/fakeCompletionNotifier, entry_test.go).
func TestSupervisorIntegrationPersistRestore(t *testing.T) {
	manifestRoot := t.TempDir()
	spoolRoot := t.TempDir()

	store := NewManifestStore(manifestRoot)
	lifecycle := &fakeLifecycleSink{}
	notifications := &fakeCompletionNotifier{}

	sup, err := NewSupervisor(Config{}, store, spoolRoot, lifecycle, notifications)
	if err != nil {
		t.Fatalf("NewSupervisor() err = %v, want nil", err)
	}

	owner := testOwner(t)
	origin := testOrigin(t)

	const wantOutput = "hello from the integration subprocess"
	prepared := &execPreparedProcess{
		cmd:    exec.Command("sh", "-c", "printf '"+wantOutput+"'"),
		access: tool.NewWorkspaceAccess(tool.WorkspaceAccessReadOnly, nil, nil),
	}

	handle, err := sup.Start(context.Background(), owner, origin, prepared, nil, nil, nil, StorageCeiling{}, YieldSettings{})
	if err != nil {
		t.Fatalf("Start() err = %v, want nil", err)
	}

	e := testEntry(t, sup, handle)
	select {
	case <-e.done:
	case <-time.After(10 * time.Second):
		t.Fatal("subprocess did not reach a terminal state within 10s")
	}

	original, err := store.Load(handle)
	if err != nil {
		t.Fatalf("store.Load() err = %v, want nil", err)
	}
	if original.State != StateExited {
		t.Fatalf("State before restore = %v, want %v", original.State, StateExited)
	}

	// Simulate a session restore: a brand new Supervisor, over the exact
	// same on-disk resource root, with no live process handles at all --
	// not even the same *Supervisor instance that ran the subprocess above.
	restoredStore := NewManifestStore(manifestRoot)
	restoredLifecycle := &fakeLifecycleSink{}
	restoredNotifications := &fakeCompletionNotifier{}
	restored, err := NewSupervisor(Config{}, restoredStore, spoolRoot, restoredLifecycle, restoredNotifications)
	if err != nil {
		t.Fatalf("NewSupervisor() (restored) err = %v, want nil", err)
	}

	report, err := restored.Restore(context.Background())
	if err != nil {
		t.Fatalf("Restore() err = %v, want nil", err)
	}
	if len(report.Errors) != 0 {
		t.Fatalf("Restore() errors = %+v, want none", report.Errors)
	}
	if len(report.Reconciled) != 1 || report.Reconciled[0] != handle {
		t.Fatalf("Restore() reconciled = %+v, want [%v]", report.Reconciled, handle)
	}

	restoredEntry := testEntry(t, restored, handle)
	if restoredEntry.process != nil {
		t.Error("restored entry has a live tool.Process, want nil")
	}
	if !closed(restoredEntry.done) {
		t.Error("restored entry's done channel is not closed")
	}

	data, _, _, err := restoredEntry.spool.Read(0, 0)
	if err != nil {
		t.Fatalf("spool.Read() err = %v, want nil", err)
	}
	if !strings.Contains(string(data), wantOutput) {
		t.Errorf("restored spool content = %q, want it to contain %q", data, wantOutput)
	}

	reloaded, err := restoredStore.Load(handle)
	if err != nil {
		t.Fatalf("restoredStore.Load() err = %v, want nil", err)
	}
	if reloaded.State != StateExited {
		t.Errorf("restored State = %v, want %v", reloaded.State, StateExited)
	}
	if reloaded.Result.ExitCode == nil || *reloaded.Result.ExitCode != 0 {
		t.Errorf("restored Result.ExitCode = %v, want 0", reloaded.Result.ExitCode)
	}
}

// --- TestSupervisorIntegrationShutdownAndRestore ---

// TestSupervisorIntegrationShutdownAndRestore covers Task 9C's coordinated
// shutdown end to end against a real OS subprocess, on top of the same
// restore boundary TestSupervisorIntegrationPersistRestore already covers: a
// real long-running `sh -c` process that ignores the portable interrupt
// signal (so Shutdown's real terminate-then-escalate ordering is genuinely
// exercised against a real process tree, not merely a fake), a
// Supervisor.Shutdown call against it, and a from-scratch Supervisor
// simulating a session restore afterward. The point of the second half is
// the same one TestSupervisorIntegrationPersistRestore's own restore step
// makes for a naturally-exited process: the shutdown-terminated process must
// reconcile as an ordinary completed process, not lost_on_restore, because
// Shutdown's own coordinated termination already durably wrote its terminal
// manifest -- restore must never mistake a clean shutdown-driven exit for an
// abandoned, still-running one.
func TestSupervisorIntegrationShutdownAndRestore(t *testing.T) {
	manifestRoot := t.TempDir()
	spoolRoot := t.TempDir()

	store := NewManifestStore(manifestRoot)
	lifecycle := &fakeLifecycleSink{}
	notifications := &fakeCompletionNotifier{}

	cfg := Config{GracefulShutdownPeriod: 200 * time.Millisecond}
	sup, err := NewSupervisor(cfg, store, spoolRoot, lifecycle, notifications)
	if err != nil {
		t.Fatalf("NewSupervisor() err = %v, want nil", err)
	}

	owner := testOwner(t)
	origin := testOrigin(t)

	// A subprocess that ignores the portable interrupt signal (Signal's
	// graceful request, above) but still dies to a real SIGKILL, so
	// Shutdown's escalation path is genuinely exercised rather than exiting
	// on the very first (graceful) signal. The trailing `exec` matters: without
	// it, this shell forks sleep as a child rather than replacing itself (a
	// leading trap disqualifies the shell's usual tail-call exec optimization
	// for the final command), so execProcess.Kill's single-PID
	// cmd.Process.Kill only kills the now-empty parent shell while the
	// orphaned `sleep` keeps running for its full 30s -- verified directly
	// with a standalone os/exec repro before this fix (EOF/Wait arrived at
	// ~30s post-kill without `exec`, ~0.2ms with it). `exec` replaces the
	// shell's own process image with `sleep` in place (same PID), and
	// SIG_IGN dispositions -- unlike custom handlers -- survive exec, so the
	// ignored-INT behavior above is preserved. Real cross-grandchild process
	// tree teardown (killing an entire tree regardless of shell/exec shape)
	// is Sandbox's job (plan Task 12), not this Tools-only fake's.
	prepared := &execPreparedProcess{
		cmd:    exec.Command("sh", "-c", "trap '' INT; exec sleep 30"),
		access: tool.NewWorkspaceAccess(tool.WorkspaceAccessReadOnly, nil, nil),
	}

	handle, err := sup.Start(context.Background(), owner, origin, prepared, nil, nil, nil, StorageCeiling{}, YieldSettings{})
	if err != nil {
		t.Fatalf("Start() err = %v, want nil", err)
	}

	if err := sup.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() err = %v, want nil", err)
	}

	// Shutdown's own "confirm every tree has exited" step only waits on
	// e.exited, not e.done (see terminateOneEntry's doc comment), so wait on
	// done directly here before this test function returns and manifestRoot/
	// spoolRoot's t.TempDir() cleanup runs -- see supervisor_test.go's
	// waitEntryDone doc comment for why that matters even though this
	// particular scenario has no explicit unclosed-pipe pattern of its own.
	waitEntryDone(t, testEntry(t, sup, handle), 5*time.Second)

	original, err := store.Load(handle)
	if err != nil {
		t.Fatalf("store.Load() err = %v, want nil", err)
	}
	if !original.State.Terminal() {
		t.Fatalf("State after Shutdown() = %v, want a terminal state", original.State)
	}
	if original.State == StateLostOnRestore {
		t.Fatalf("State after Shutdown() = %v, want a clean shutdown-driven terminal state, not lost_on_restore", original.State)
	}

	// Simulate a session restore against the exact same on-disk resource
	// root, exactly like TestSupervisorIntegrationPersistRestore -- a brand
	// new Supervisor, no live process handles at all.
	restoredStore := NewManifestStore(manifestRoot)
	restoredLifecycle := &fakeLifecycleSink{}
	restoredNotifications := &fakeCompletionNotifier{}
	restored, err := NewSupervisor(Config{}, restoredStore, spoolRoot, restoredLifecycle, restoredNotifications)
	if err != nil {
		t.Fatalf("NewSupervisor() (restored) err = %v, want nil", err)
	}

	report, err := restored.Restore(context.Background())
	if err != nil {
		t.Fatalf("Restore() err = %v, want nil", err)
	}
	if len(report.Errors) != 0 {
		t.Fatalf("Restore() errors = %+v, want none", report.Errors)
	}
	if len(report.Reconciled) != 1 || report.Reconciled[0] != handle {
		t.Fatalf("Restore() reconciled = %+v, want [%v]", report.Reconciled, handle)
	}

	restoredEntry := testEntry(t, restored, handle)
	if restoredEntry.process != nil {
		t.Error("restored entry has a live tool.Process, want nil")
	}
	if !closed(restoredEntry.done) {
		t.Error("restored entry's done channel is not closed")
	}

	reloaded, err := restoredStore.Load(handle)
	if err != nil {
		t.Fatalf("restoredStore.Load() err = %v, want nil", err)
	}
	if reloaded.State != original.State {
		t.Errorf("restored State = %v, want unchanged from the pre-restore shutdown-driven state %v", reloaded.State, original.State)
	}
	if reloaded.State == StateLostOnRestore {
		t.Error("restored State = lost_on_restore, want the process to remain queryable as a normal completed process: it already had a clean shutdown-driven terminal manifest write, not an abandoned one")
	}
}

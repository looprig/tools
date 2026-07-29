package process

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/tool"
)

// --- Task 9D default-tag shutdown-then-restore seam tests ---
//
// supervisor_integration_test.go's tagged (//go:build integration)
// TestSupervisorIntegrationShutdownAndRestore already proves this same seam
// end to end against a real OS subprocess, but that suite is only run at a
// deliberate phase gate (see that file's own doc comment), never by a
// task/microtask agent. The two tests below prove the identical property --
// a shutdown-terminated process reconciles on restore as an ordinary
// completed process, never lost_on_restore, because its terminal manifest
// is already durably written before Restore ever scans it -- using
// fake_process_test.go's fakePreparedProcess/fakeProcess/fakeLease instead
// of a real subprocess, so they run fast and deterministically under a
// plain `go test ./process` with no build tag and no approved host.
//
// Both tests build two independent *Supervisor values over the same
// on-disk manifest/spool roots -- exactly like restore_test.go's fixtures
// and the tagged integration test -- rather than reusing a single
// Supervisor's own s.entries registry, so that Restore's reconciliation is
// genuinely exercised against durable state alone, not against any
// in-memory shortcut.

// --- TestShutdownNoopThenRestoreQueriesCompletedProcess ---

// TestShutdownNoopThenRestoreQueriesCompletedProcess covers the "already
// completed before shutdown" half of the seam: a process that exits on its
// own (fakeProcess.Wait returns immediately, no waitBlock) before Shutdown
// is ever called, so Shutdown's own snapshotRunningEntries step finds no
// running entries left to terminate (doShutdown's len(targets) == 0 early
// return) and simply closes admission. A fresh Supervisor restored over the
// same on-disk roots must still reconcile the process as an ordinary
// completed StateExited entry -- never lost_on_restore -- with its output
// still readable through the reopened Spool.
func TestShutdownNoopThenRestoreQueriesCompletedProcess(t *testing.T) {
	t.Parallel()

	manifestRoot := t.TempDir()
	spoolRoot := t.TempDir()
	store := NewManifestStore(manifestRoot)

	sup, err := NewSupervisor(shutdownTestConfig(100*time.Millisecond), store, spoolRoot, nil, nil)
	if err != nil {
		t.Fatalf("NewSupervisor() err = %v, want nil", err)
	}
	owner := testOwner(t)
	origin := testOrigin(t)

	const wantOutput = "already done output"
	proc := &fakeProcess{
		stdout:     io.NopCloser(strings.NewReader(wantOutput)),
		waitResult: tool.ProcessResult{Reason: tool.ProcessTerminalExited, ExitCode: 0},
	}
	handle := startShutdownFake(t, sup, owner, origin, proc, &fakeLease{})

	e := testEntry(t, sup, handle)
	select {
	case <-e.done:
	case <-time.After(2 * time.Second):
		t.Fatal("process did not reach a terminal state naturally within 2s")
	}

	if err := sup.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() err = %v, want nil (no running entries left to terminate)", err)
	}

	original, err := store.Load(handle)
	if err != nil {
		t.Fatalf("store.Load() err = %v, want nil", err)
	}
	if original.State != StateExited {
		t.Fatalf("State after natural exit + Shutdown() = %v, want %v", original.State, StateExited)
	}

	// Simulate a session restore: a brand new Supervisor, over the exact
	// same on-disk resource root, with no live process handles at all.
	restoredStore := NewManifestStore(manifestRoot)
	restored, err := NewSupervisor(Config{}, restoredStore, spoolRoot, nil, nil)
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
	if reloaded.State != StateExited {
		t.Errorf("restored State = %v, want unchanged %v", reloaded.State, StateExited)
	}
	if reloaded.State == StateLostOnRestore {
		t.Error("restored State = lost_on_restore, want the naturally-completed process to remain queryable as a normal completed process")
	}

	data, _, _, err := restoredEntry.spool.Read(0, 0)
	if err != nil {
		t.Fatalf("spool.Read() err = %v, want nil", err)
	}
	if string(data) != wantOutput {
		t.Errorf("restored spool content = %q, want %q", data, wantOutput)
	}
}

// --- TestShutdownTerminatesRunningThenRestoreQueriesCleanTerminal ---

// TestShutdownTerminatesRunningThenRestoreQueriesCleanTerminal covers the
// core seam the tagged integration test also proves, against a fake process
// instead of a real subprocess: a process still running when Shutdown is
// called is terminated by Shutdown's own signal (here, the fake exits
// promptly on the graceful ProcessSignalTerminate, so this test never has
// to wait out the grace period or exercise the kill escalation -- that
// ordering is TestShutdownEscalatesAndConfirmsTrees' job, not this one's).
// Shutdown's termination drives the process through its ordinary
// run/Wait/terminalize path, durably writing a clean terminal manifest
// *before* any restore ever runs. A fresh Supervisor restored over the same
// on-disk roots must therefore reconcile the process as that same clean
// terminal state, not lost_on_restore: Shutdown's coordinated termination is
// what makes restore correctly leave it alone rather than reclassifying it
// as abandoned.
func TestShutdownTerminatesRunningThenRestoreQueriesCleanTerminal(t *testing.T) {
	t.Parallel()

	manifestRoot := t.TempDir()
	spoolRoot := t.TempDir()
	store := NewManifestStore(manifestRoot)

	sup, err := NewSupervisor(shutdownTestConfig(2*time.Second), store, spoolRoot, nil, nil)
	if err != nil {
		t.Fatalf("NewSupervisor() err = %v, want nil", err)
	}
	owner := testOwner(t)
	origin := testOrigin(t)

	const wantOutput = "still running output"
	proc := &fakeProcess{
		waitBlock:  make(chan struct{}),
		stdout:     io.NopCloser(strings.NewReader(wantOutput)),
		waitResult: tool.ProcessResult{Reason: tool.ProcessTerminalTerminated},
	}
	proc.signalFunc = func(sig tool.ProcessSignal) error {
		if sig == tool.ProcessSignalTerminate {
			close(proc.waitBlock)
		}
		return nil
	}
	handle := startShutdownFake(t, sup, owner, origin, proc, &fakeLease{})

	if err := sup.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() err = %v, want nil", err)
	}

	// Shutdown's own "confirm every tree has exited" step only waits on
	// e.exited, not e.done (see terminateOneEntry's doc comment), so wait on
	// done directly here before asserting on the durably persisted manifest.
	e := testEntry(t, sup, handle)
	select {
	case <-e.done:
	case <-time.After(2 * time.Second):
		t.Fatal("entry never reached its terminal state after Shutdown() returned")
	}

	if calls := proc.SignalCalls(); len(calls) == 0 || calls[0] != tool.ProcessSignalTerminate {
		t.Fatalf("Signal calls = %v, want the first call to be ProcessSignalTerminate", calls)
	}

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
	// root -- a brand new Supervisor, no live process handles at all.
	restoredStore := NewManifestStore(manifestRoot)
	restored, err := NewSupervisor(Config{}, restoredStore, spoolRoot, nil, nil)
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

	data, _, _, err := restoredEntry.spool.Read(0, 0)
	if err != nil {
		t.Fatalf("spool.Read() err = %v, want nil", err)
	}
	if string(data) != wantOutput {
		t.Errorf("restored spool content = %q, want %q", data, wantOutput)
	}
}

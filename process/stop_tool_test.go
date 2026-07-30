package process

// stop_tool_test.go tests the ProcessStop tool (stop_tool.go) against the
// spec's "ProcessStop API" section. It follows the same two-layer shape
// output_tool_test.go/input_tool_test.go use: PrepareCall validation is
// tested directly, while InvokableRun behavior is tested through the full
// prepared flow (PrepareCall -> loop.WithPreparedCall -> InvokableRun).
//
// Every InvokableRun-level test drives a REAL, live entry through
// Supervisor.Start (not a bare registerEntry-built entry, unlike
// output_tool_test.go/input_tool_test.go's own fixtures): ProcessStop's
// whole contract is signal-then-confirm against entry.go's actual
// run/terminalize lifecycle, so its tests reuse shutdown_test.go's own
// fakeProcess-driven fixtures (startShutdownFake, shutdownTestConfig) --
// the exact same "Wait blocks on waitBlock until signalFunc lets it
// exit" pattern Task 9C's coordinated-shutdown tests already established
// for proving terminate-then-kill escalation deterministically.

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

// --- fixtures ---

func prepareStop(t *testing.T, tl *ProcessStopTool, argsJSON string) (tool.Request, tool.PreparedArtifact) {
	t.Helper()
	req, art, err := tl.PrepareCall(context.Background(), mustUUID(t), argsJSON)
	if err != nil {
		t.Fatalf("PrepareCall(%s) error = %v, want nil", argsJSON, err)
	}
	return req, art
}

func prepareStopErr(t *testing.T, tl *ProcessStopTool, argsJSON string) error {
	t.Helper()
	_, _, err := tl.PrepareCall(context.Background(), mustUUID(t), argsJSON)
	if err == nil {
		t.Fatalf("PrepareCall(%s) error = nil, want an error", argsJSON)
	}
	return err
}

func runStopCtx(t *testing.T, ctx context.Context, tl *ProcessStopTool, argsJSON string) string {
	t.Helper()
	req, art, err := tl.PrepareCall(ctx, mustUUID(t), argsJSON)
	if err != nil {
		t.Fatalf("PrepareCall(%s) error = %v, want nil", argsJSON, err)
	}
	prepCtx := loop.WithPreparedCall(ctx, tool.PreparedCall{Request: req, Artifact: art})
	result, err := tl.InvokableRun(prepCtx, argsJSON)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v, want nil (ProcessStop never returns a Go error)", err)
	}
	return textOf(t, result)
}

func runStop(t *testing.T, tl *ProcessStopTool, argsJSON string) string {
	t.Helper()
	return runStopCtx(t, context.Background(), tl, argsJSON)
}

func decodeStop(t *testing.T, text string) processStopResult {
	t.Helper()
	var out processStopResult
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v, want nil", text, err)
	}
	return out
}

// startStopFake admits proc as a live, non-terminal entry under owner/origin
// through the Supervisor's real Start path (mirroring shutdown_test.go's
// startShutdownFake), so its run goroutine actually drives Wait and
// terminalize -- exactly what ProcessStop's signal/confirm contract needs to
// be exercised meaningfully. sink, when non-nil, is recorded on the entry so
// a test can observe its eventual terminal lifecycle publish.
func startStopFake(t *testing.T, sup *Supervisor, owner Owner, origin Origin, proc *fakeProcess, sink lifecycleSink) Handle {
	t.Helper()
	prepared := &fakePreparedProcess{process: proc}
	handle, err := sup.Start(context.Background(), owner, origin, prepared, &fakeLease{}, sink, nil, StorageCeiling{}, YieldSettings{})
	if err != nil {
		t.Fatalf("Start() err = %v, want nil", err)
	}
	return handle
}

// stopWaitTimeout bounds every waitEntryDone call in this file.
const stopWaitTimeout = 5 * time.Second

// --- Info ---

func TestProcessStopInfo(t *testing.T) {
	t.Parallel()
	tl := NewProcessStop(newTestSupervisor(t, Config{}), testOwner(t))
	info, err := tl.Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error = %v, want nil", err)
	}
	if info.Name != "ProcessStop" {
		t.Errorf("Info().Name = %q, want %q", info.Name, "ProcessStop")
	}
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(info.Schema, &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	if _, ok := schema["properties"]; !ok {
		t.Error("schema has no \"properties\" key")
	}
}

// --- PrepareCall: invalid mode/grace, malformed input ---

func TestProcessStopPrepareCallInvalid(t *testing.T) {
	t.Parallel()
	h := string(testHandle(t, 1))

	cases := map[string]string{
		"not json":             `not json`,
		"missing process_id":   `{"mode":"kill"}`,
		"empty process_id":     `{"process_id":"","mode":"kill"}`,
		"malformed process_id": `{"process_id":"not-a-valid-handle","mode":"kill"}`,
		"missing mode":         `{"process_id":"` + h + `"}`,
		"empty mode":           `{"process_id":"` + h + `","mode":""}`,
		"unrecognized mode":    `{"process_id":"` + h + `","mode":"pause"}`,
		"negative grace_ms":    `{"process_id":"` + h + `","mode":"kill","grace_ms":-1}`,
	}

	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tl := NewProcessStop(newTestSupervisor(t, Config{}), testOwner(t))
			prepareStopErr(t, tl, args)
		})
	}
}

// TestProcessStopPrepareCallGraceDefaultsToSupervisorConfig proves an
// omitted grace_ms defaults to the supervisor's own configured
// GracefulShutdownPeriod (config.go's documented contract: "how long
// supervisor shutdown and ProcessStop's terminate mode wait before
// escalating to kill"), while an explicit grace_ms -- including 0 --
// overrides it exactly.
func TestProcessStopPrepareCallGraceDefaultsToSupervisorConfig(t *testing.T) {
	t.Parallel()
	const configuredGrace = 777 * time.Millisecond
	sup := newTestSupervisor(t, Config{GracefulShutdownPeriod: configuredGrace})
	tl := NewProcessStop(sup, testOwner(t))
	h := string(testHandle(t, 1))

	_, art := prepareStop(t, tl, `{"process_id":"`+h+`","mode":"terminate"}`)
	stopArt, ok := art.(*processStopArtifact)
	if !ok {
		t.Fatalf("artifact type = %T, want *processStopArtifact", art)
	}
	if stopArt.grace != configuredGrace {
		t.Errorf("omitted grace_ms -> grace = %v, want the supervisor's configured %v", stopArt.grace, configuredGrace)
	}

	_, art = prepareStop(t, tl, `{"process_id":"`+h+`","mode":"terminate","grace_ms":0}`)
	stopArt, ok = art.(*processStopArtifact)
	if !ok {
		t.Fatalf("artifact type = %T, want *processStopArtifact", art)
	}
	if stopArt.grace != 0 {
		t.Errorf("explicit grace_ms:0 -> grace = %v, want 0", stopArt.grace)
	}

	_, art = prepareStop(t, tl, `{"process_id":"`+h+`","mode":"terminate","grace_ms":1500}`)
	stopArt, ok = art.(*processStopArtifact)
	if !ok {
		t.Fatalf("artifact type = %T, want *processStopArtifact", art)
	}
	if stopArt.grace != 1500*time.Millisecond {
		t.Errorf("explicit grace_ms:1500 -> grace = %v, want 1.5s", stopArt.grace)
	}
}

// --- InvokableRun: interrupt ---

// TestProcessStopInterruptConfirmedExit proves that when interrupt DOES
// cause the process to exit within grace, ProcessStop confirms it and
// renders the terminal snapshot, sending only the interrupt signal (no
// escalation).
func TestProcessStopInterruptConfirmedExit(t *testing.T) {
	t.Parallel()
	sup := newTestSupervisor(t, shutdownTestConfig(2*time.Second))
	owner := testOwner(t)

	proc := &fakeProcess{
		waitBlock:  make(chan struct{}),
		waitResult: tool.ProcessResult{Reason: tool.ProcessTerminalInterrupted},
	}
	proc.signalFunc = func(sig tool.ProcessSignal) error {
		if sig == tool.ProcessSignalInterrupt {
			close(proc.waitBlock)
		}
		return nil
	}
	handle := startStopFake(t, sup, owner, testOrigin(t), proc, nil)
	e := testEntry(t, sup, handle)
	t.Cleanup(func() { waitEntryDone(t, e, stopWaitTimeout) })

	tl := NewProcessStop(sup, owner)
	text := runStop(t, tl, `{"process_id":"`+string(handle)+`","mode":"interrupt"}`)
	result := decodeStop(t, text)

	if result.Error != "" {
		t.Fatalf("Error = %q, want empty", result.Error)
	}
	if result.Status != string(StateInterrupted) {
		t.Errorf("Status = %q, want %q", result.Status, StateInterrupted)
	}
	if calls := proc.SignalCalls(); len(calls) != 1 || calls[0] != tool.ProcessSignalInterrupt {
		t.Errorf("SignalCalls = %v, want exactly [Interrupt]", calls)
	}
}

// TestProcessStopInterruptDoesNotEscalateOnTimeout proves the spec's
// "interrupt ... does not terminalize the supervisor state unless the
// process exits": a process that ignores the interrupt entirely is never
// escalated to terminate/kill, and ProcessStop reports its current
// (still-running) status rather than a failure.
func TestProcessStopInterruptDoesNotEscalateOnTimeout(t *testing.T) {
	t.Parallel()
	const grace = 30 * time.Millisecond
	sup := newTestSupervisor(t, shutdownTestConfig(2*time.Second))
	owner := testOwner(t)

	proc := &fakeProcess{waitBlock: make(chan struct{})}
	// signalFunc intentionally never closes waitBlock: the process ignores
	// every signal until the test's own cleanup lets it exit.
	handle := startStopFake(t, sup, owner, testOrigin(t), proc, nil)
	e := testEntry(t, sup, handle)
	t.Cleanup(func() {
		close(proc.waitBlock)
		waitEntryDone(t, e, stopWaitTimeout)
	})

	tl := NewProcessStop(sup, owner)
	text := runStop(t, tl, `{"process_id":"`+string(handle)+`","mode":"interrupt","grace_ms":`+durMS(grace)+`}`)
	result := decodeStop(t, text)

	if result.Error != "" {
		t.Fatalf("Error = %q, want empty", result.Error)
	}
	if result.Status != string(StateRunning) {
		t.Errorf("Status = %q, want %q (interrupt must not terminalize a process that ignores it)", result.Status, StateRunning)
	}
	if calls := proc.SignalCalls(); len(calls) != 1 || calls[0] != tool.ProcessSignalInterrupt {
		t.Errorf("SignalCalls = %v, want exactly [Interrupt] (no escalation)", calls)
	}
	if closed(e.done) {
		t.Error("entry reached terminal state despite the process never exiting")
	}
}

// --- InvokableRun: terminate ---

// TestProcessStopTerminateGracefulNoEscalation proves that when the
// process exits in response to the graceful terminate signal itself,
// ProcessStop confirms it without ever escalating to kill.
func TestProcessStopTerminateGracefulNoEscalation(t *testing.T) {
	t.Parallel()
	sup := newTestSupervisor(t, shutdownTestConfig(2*time.Second))
	owner := testOwner(t)

	proc := &fakeProcess{
		waitBlock:  make(chan struct{}),
		waitResult: tool.ProcessResult{Reason: tool.ProcessTerminalTerminated},
	}
	proc.signalFunc = func(sig tool.ProcessSignal) error {
		if sig == tool.ProcessSignalTerminate {
			close(proc.waitBlock)
		}
		return nil
	}
	handle := startStopFake(t, sup, owner, testOrigin(t), proc, nil)
	e := testEntry(t, sup, handle)
	t.Cleanup(func() { waitEntryDone(t, e, stopWaitTimeout) })

	tl := NewProcessStop(sup, owner)
	text := runStop(t, tl, `{"process_id":"`+string(handle)+`","mode":"terminate","grace_ms":2000}`)
	result := decodeStop(t, text)

	if result.Error != "" {
		t.Fatalf("Error = %q, want empty", result.Error)
	}
	if result.Status != string(StateTerminated) {
		t.Errorf("Status = %q, want %q", result.Status, StateTerminated)
	}
	if calls := proc.SignalCalls(); len(calls) != 1 || calls[0] != tool.ProcessSignalTerminate {
		t.Errorf("SignalCalls = %v, want exactly [Terminate] (no escalation)", calls)
	}
}

// TestProcessStopTerminateEscalatesAfterGrace proves the escalation half of
// terminate's contract end to end (mirrors supervisor.go's own
// TestShutdownEscalatesAndConfirmsTrees): a process that ignores terminate
// but exits promptly on kill still results in a confirmed terminal
// snapshot, only after the configured grace period has actually elapsed,
// with the exact terminate-then-kill call order.
func TestProcessStopTerminateEscalatesAfterGrace(t *testing.T) {
	t.Parallel()
	const grace = 30 * time.Millisecond
	sup := newTestSupervisor(t, shutdownTestConfig(2*time.Second))
	owner := testOwner(t)

	proc := &fakeProcess{
		waitBlock:  make(chan struct{}),
		waitResult: tool.ProcessResult{Reason: tool.ProcessTerminalKilled},
	}
	proc.signalFunc = func(sig tool.ProcessSignal) error {
		if sig == tool.ProcessSignalKill {
			close(proc.waitBlock)
		}
		return nil
	}
	handle := startStopFake(t, sup, owner, testOrigin(t), proc, nil)
	e := testEntry(t, sup, handle)
	t.Cleanup(func() { waitEntryDone(t, e, stopWaitTimeout) })

	tl := NewProcessStop(sup, owner)
	start := time.Now()
	text := runStop(t, tl, `{"process_id":"`+string(handle)+`","mode":"terminate","grace_ms":`+durMS(grace)+`}`)
	elapsed := time.Since(start)
	result := decodeStop(t, text)

	if elapsed < grace {
		t.Errorf("InvokableRun() returned after %v, want at least the configured grace period %v", elapsed, grace)
	}
	if result.Error != "" {
		t.Fatalf("Error = %q, want empty", result.Error)
	}
	if result.Status != string(StateKilled) {
		t.Errorf("Status = %q, want %q", result.Status, StateKilled)
	}
	calls := proc.SignalCalls()
	if len(calls) != 2 || calls[0] != tool.ProcessSignalTerminate || calls[1] != tool.ProcessSignalKill {
		t.Fatalf("SignalCalls = %v, want exactly [Terminate, Kill]", calls)
	}
}

// TestProcessStopTerminateNaturalExitRaceSkipsEscalation proves that a
// natural exit racing ahead of terminate's grace window -- entirely
// unrelated to the terminate signal itself, e.g. the command simply
// finished on its own -- is observed and confirmed without ever reaching
// escalation, even though the process never actually responded to the
// terminate signal.
func TestProcessStopTerminateNaturalExitRaceSkipsEscalation(t *testing.T) {
	t.Parallel()
	const grace = 300 * time.Millisecond
	sup := newTestSupervisor(t, shutdownTestConfig(2*time.Second))
	owner := testOwner(t)

	proc := &fakeProcess{
		waitBlock:  make(chan struct{}),
		waitResult: tool.ProcessResult{Reason: tool.ProcessTerminalExited},
	}
	// signalFunc never reacts to Terminate: the eventual exit below is
	// deliberately independent of the signal ProcessStop sends.
	handle := startStopFake(t, sup, owner, testOrigin(t), proc, nil)
	e := testEntry(t, sup, handle)
	t.Cleanup(func() { waitEntryDone(t, e, stopWaitTimeout) })

	go func() {
		time.Sleep(20 * time.Millisecond)
		close(proc.waitBlock)
	}()

	tl := NewProcessStop(sup, owner)
	text := runStop(t, tl, `{"process_id":"`+string(handle)+`","mode":"terminate","grace_ms":`+durMS(grace)+`}`)
	result := decodeStop(t, text)

	if result.Error != "" {
		t.Fatalf("Error = %q, want empty", result.Error)
	}
	if result.Status != string(StateExited) {
		t.Errorf("Status = %q, want %q", result.Status, StateExited)
	}
	if calls := proc.SignalCalls(); len(calls) != 1 || calls[0] != tool.ProcessSignalTerminate {
		t.Errorf("SignalCalls = %v, want exactly [Terminate] (the natural exit must win the race before escalation)", calls)
	}
}

// TestProcessStopTerminateCtxCancellationTriggersEscalationWithoutFalseSuccess
// proves the "timeout race" between the tool invocation's own ctx and
// terminate's grace window: when ctx ends first (here, well before even a
// very long configured grace), ProcessStop still attempts escalation (a
// caller-initiated stop request must not be abandoned just because the
// invocation's own deadline passed) but never blocks past ctx to confirm
// it, and -- because it never confirmed -- never reports a false terminal
// success.
func TestProcessStopTerminateCtxCancellationTriggersEscalationWithoutFalseSuccess(t *testing.T) {
	t.Parallel()
	const grace = 10 * time.Second
	sup := newTestSupervisor(t, shutdownTestConfig(2*time.Second))
	owner := testOwner(t)

	proc := &fakeProcess{waitBlock: make(chan struct{})}
	// The process never exits during this test at all: both signals are
	// ignored, so any confirmed-terminal render would be a false success.
	handle := startStopFake(t, sup, owner, testOrigin(t), proc, nil)
	e := testEntry(t, sup, handle)
	t.Cleanup(func() {
		close(proc.waitBlock)
		waitEntryDone(t, e, stopWaitTimeout)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	tl := NewProcessStop(sup, owner)
	start := time.Now()
	text := runStopCtx(t, ctx, tl, `{"process_id":"`+string(handle)+`","mode":"terminate","grace_ms":`+durMS(grace)+`}`)
	elapsed := time.Since(start)
	result := decodeStop(t, text)

	if elapsed >= grace {
		t.Errorf("InvokableRun() took %v, want well under the configured grace period %v (ctx cancellation must bound the wait)", elapsed, grace)
	}
	if result.Error != "" {
		t.Fatalf("Error = %q, want empty (ctx cancellation is not a teardown failure)", result.Error)
	}
	if result.Status != string(StateRunning) {
		t.Errorf("Status = %q, want %q (must never report success before confirmation)", result.Status, StateRunning)
	}
	calls := proc.SignalCalls()
	if len(calls) != 2 || calls[0] != tool.ProcessSignalTerminate || calls[1] != tool.ProcessSignalKill {
		t.Errorf("SignalCalls = %v, want exactly [Terminate, Kill] (escalation must still be attempted on the caller's behalf)", calls)
	}
}

// --- InvokableRun: kill ---

// TestProcessStopKillConfirmsExitBeforeReturning proves "confirmed tree
// exit": InvokableRun only returns a terminal status once the entry has
// actually reached it -- not merely after asking for it. The fake process
// here only exits once tool.ProcessSignalKill actually arrives, so a
// terminal result can only appear after this call genuinely waited for
// that confirmation.
func TestProcessStopKillConfirmsExitBeforeReturning(t *testing.T) {
	t.Parallel()
	sup := newTestSupervisor(t, shutdownTestConfig(2*time.Second))
	owner := testOwner(t)

	proc := &fakeProcess{
		waitBlock:  make(chan struct{}),
		waitResult: tool.ProcessResult{Reason: tool.ProcessTerminalKilled},
	}
	proc.signalFunc = func(sig tool.ProcessSignal) error {
		if sig == tool.ProcessSignalKill {
			close(proc.waitBlock)
		}
		return nil
	}
	handle := startStopFake(t, sup, owner, testOrigin(t), proc, nil)
	e := testEntry(t, sup, handle)
	t.Cleanup(func() { waitEntryDone(t, e, stopWaitTimeout) })

	tl := NewProcessStop(sup, owner)
	text := runStop(t, tl, `{"process_id":"`+string(handle)+`","mode":"kill"}`)
	result := decodeStop(t, text)

	// e.exited -- not e.done -- is entry.go's documented synchronous
	// confirmation signal (closed inside doTerminalize once the terminal
	// manifest is durably persisted, strictly before the lifecycle
	// publish/notify calls that follow it); awaitExit's confirmed==true
	// path is built on exactly this channel, so it is the only one this
	// assertion can rely on being closed immediately upon return.
	if !closed(e.exited) {
		t.Fatal("entry not yet confirmed-exited immediately after InvokableRun returned")
	}
	if result.Error != "" {
		t.Fatalf("Error = %q, want empty", result.Error)
	}
	if result.Status != string(StateKilled) {
		t.Errorf("Status = %q, want %q", result.Status, StateKilled)
	}
	if result.ExitCode != nil {
		t.Errorf("ExitCode = %v, want nil (kill has no exit code)", result.ExitCode)
	}
	if calls := proc.SignalCalls(); len(calls) != 1 || calls[0] != tool.ProcessSignalKill {
		t.Errorf("SignalCalls = %v, want exactly [Kill]", calls)
	}
}

// --- InvokableRun: idempotence ---

// TestProcessStopTerminalIdempotence proves "Repeating a stop operation
// against a terminal process is successful and returns the existing
// terminal result": an already-exited entry is never re-signaled.
func TestProcessStopTerminalIdempotence(t *testing.T) {
	t.Parallel()
	sup := newTestSupervisor(t, shutdownTestConfig(2*time.Second))
	owner := testOwner(t)

	proc := &fakeProcess{waitResult: tool.ProcessResult{ExitCode: 7, Reason: tool.ProcessTerminalExited}}
	handle := startStopFake(t, sup, owner, testOrigin(t), proc, nil)
	e := testEntry(t, sup, handle)
	waitEntryDone(t, e, stopWaitTimeout)

	tl := NewProcessStop(sup, owner)
	for _, mode := range []string{"interrupt", "terminate", "kill"} {
		text := runStop(t, tl, `{"process_id":"`+string(handle)+`","mode":"`+mode+`"}`)
		result := decodeStop(t, text)
		if result.Error != "" {
			t.Fatalf("mode %q: Error = %q, want empty", mode, result.Error)
		}
		if result.Status != string(StateExited) {
			t.Errorf("mode %q: Status = %q, want %q", mode, result.Status, StateExited)
		}
		if result.ExitCode == nil || *result.ExitCode != 7 {
			t.Errorf("mode %q: ExitCode = %v, want 7", mode, result.ExitCode)
		}
	}
	if calls := proc.SignalCalls(); len(calls) != 0 {
		t.Errorf("SignalCalls = %v, want none (an already-terminal process must never be re-signaled)", calls)
	}
}

// --- InvokableRun: teardown failure ---

// TestProcessStopTeardownFailureRetainsAuthority mirrors supervisor.go's
// own TestShutdownTeardownFailureRetainsAuthority: a Signal call that
// itself reports an error -- even though the process still actually
// exits -- renders as a typed teardown_failed error, while the entry
// still reaches its own terminal, durably persisted state independently.
func TestProcessStopTeardownFailureRetainsAuthority(t *testing.T) {
	t.Parallel()
	sup := newTestSupervisor(t, shutdownTestConfig(2*time.Second))
	owner := testOwner(t)

	killErr := errors.New("boom: kill signal delivery failed")
	proc := &fakeProcess{
		waitBlock:  make(chan struct{}),
		waitResult: tool.ProcessResult{Reason: tool.ProcessTerminalKilled},
	}
	proc.signalFunc = func(sig tool.ProcessSignal) error {
		if sig == tool.ProcessSignalKill {
			close(proc.waitBlock)
			return killErr
		}
		return nil
	}
	handle := startStopFake(t, sup, owner, testOrigin(t), proc, nil)
	e := testEntry(t, sup, handle)
	t.Cleanup(func() { waitEntryDone(t, e, stopWaitTimeout) })

	tl := NewProcessStop(sup, owner)
	text := runStop(t, tl, `{"process_id":"`+string(handle)+`","mode":"kill"}`)
	result := decodeStop(t, text)

	if result.Error != string(CodeTeardownFailed) {
		t.Fatalf("Error = %q, want %q", result.Error, CodeTeardownFailed)
	}
	if result.Status != "" {
		t.Errorf("Status = %q, want empty on a teardown-failure render", result.Status)
	}

	select {
	case <-e.done:
	case <-time.After(stopWaitTimeout):
		t.Fatal("entry never reached its terminal state despite the teardown failure")
	}
	final, err := sup.manifests.Load(handle)
	if err != nil {
		t.Fatalf("manifests.Load() err = %v, want nil", err)
	}
	if !final.State.Terminal() {
		t.Errorf("manifest state after a stop teardown failure = %v, want a terminal state", final.State)
	}
}

// --- InvokableRun: lifecycle event ---

// TestProcessStopConfirmedExitPublishesCompletedLifecycleEvent proves that
// a ProcessStop-confirmed exit flows through the exact same durable
// terminal lifecycle publication every other termination path uses
// (entry.go's doTerminalize) -- not some separate ad hoc side channel --
// by observing the session's lifecycleSink directly.
func TestProcessStopConfirmedExitPublishesCompletedLifecycleEvent(t *testing.T) {
	t.Parallel()
	sup := newTestSupervisor(t, shutdownTestConfig(2*time.Second))
	owner := testOwner(t)
	sink := &fakeLifecycleSink{}

	proc := &fakeProcess{
		waitBlock:  make(chan struct{}),
		waitResult: tool.ProcessResult{Reason: tool.ProcessTerminalKilled},
	}
	proc.signalFunc = func(sig tool.ProcessSignal) error {
		if sig == tool.ProcessSignalKill {
			close(proc.waitBlock)
		}
		return nil
	}
	handle := startStopFake(t, sup, owner, testOrigin(t), proc, sink)
	e := testEntry(t, sup, handle)
	t.Cleanup(func() { waitEntryDone(t, e, stopWaitTimeout) })

	tl := NewProcessStop(sup, owner)
	text := runStop(t, tl, `{"process_id":"`+string(handle)+`","mode":"kill"}`)
	result := decodeStop(t, text)
	if result.Error != "" {
		t.Fatalf("Error = %q, want empty", result.Error)
	}
	if result.Status != string(StateKilled) {
		t.Fatalf("Status = %q, want %q", result.Status, StateKilled)
	}

	if got := sink.PublishCalls(); got != 1 {
		t.Fatalf("lifecycleSink.publish called %d times, want exactly 1", got)
	}
	if sink.lastEvent.Kind != tool.ProcessLifecycleCompleted {
		t.Errorf("published event Kind = %v, want ProcessLifecycleCompleted", sink.lastEvent.Kind)
	}
	if sink.lastEvent.State != StateKilled {
		t.Errorf("published event State = %v, want %v", sink.lastEvent.State, StateKilled)
	}
}

// --- InvokableRun: owner isolation ---

// TestProcessStopCrossOwnerNotFound proves owner isolation: a stop request
// from a different owner renders not_found -- indistinguishable from a
// missing handle -- and never touches the live process at all.
func TestProcessStopCrossOwnerNotFound(t *testing.T) {
	t.Parallel()
	sup := newTestSupervisor(t, shutdownTestConfig(2*time.Second))
	owner := testOwner(t)

	proc := &fakeProcess{waitBlock: make(chan struct{})}
	handle := startStopFake(t, sup, owner, testOrigin(t), proc, nil)
	e := testEntry(t, sup, handle)
	t.Cleanup(func() {
		close(proc.waitBlock)
		waitEntryDone(t, e, stopWaitTimeout)
	})

	otherOwner := Owner{SessionID: mustUUID(t), LoopID: mustUUID(t)}
	tl := NewProcessStop(sup, otherOwner)
	text := runStop(t, tl, `{"process_id":"`+string(handle)+`","mode":"kill"}`)
	result := decodeStop(t, text)

	if result.Error != string(CodeNotFound) {
		t.Fatalf("Error = %q, want %q", result.Error, CodeNotFound)
	}
	if calls := proc.SignalCalls(); len(calls) != 0 {
		t.Errorf("SignalCalls = %v, want none (a cross-owner stop must never signal the process)", calls)
	}
}

// TestProcessStopUnknownHandleNotFound proves a handle that never existed
// at all renders identically to a cross-owner one.
func TestProcessStopUnknownHandleNotFound(t *testing.T) {
	t.Parallel()
	sup := newTestSupervisor(t, Config{})
	tl := NewProcessStop(sup, testOwner(t))
	text := runStop(t, tl, `{"process_id":"`+string(testHandle(t, 9))+`","mode":"kill"}`)
	result := decodeStop(t, text)
	if result.Error != string(CodeNotFound) {
		t.Fatalf("Error = %q, want %q", result.Error, CodeNotFound)
	}
}

// --- InvokableRun: structural guards ---

func TestProcessStopInvokeWithoutPreparedCall(t *testing.T) {
	t.Parallel()
	tl := NewProcessStop(newTestSupervisor(t, Config{}), testOwner(t))
	result, err := tl.InvokableRun(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v, want nil", err)
	}
	text := textOf(t, result)
	var out struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v, want nil", text, err)
	}
	if out.Error != string(CodeInvalidArguments) {
		t.Errorf("Error = %q, want %q", out.Error, CodeInvalidArguments)
	}
}

func TestProcessStopUnavailableSupervisor(t *testing.T) {
	t.Parallel()
	tl := NewProcessStop(nil, testOwner(t))
	_, _, err := tl.PrepareCall(context.Background(), mustUUID(t), `{"process_id":"`+string(testHandle(t, 1))+`","mode":"kill"}`)
	if err == nil {
		t.Fatal("PrepareCall() error = nil, want non-nil for a nil supervisor")
	}

	result, err := tl.InvokableRun(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v, want nil", err)
	}
	text := textOf(t, result)
	var out struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v, want nil", text, err)
	}
	if out.Error != string(CodeLifetimeEnforcementUnavailable) {
		t.Errorf("Error = %q, want %q", out.Error, CodeLifetimeEnforcementUnavailable)
	}
}

// durMS renders d as the integer-millisecond string PrepareCall's grace_ms
// argument expects.
func durMS(d time.Duration) string {
	return strconv.FormatInt(int64(d/time.Millisecond), 10)
}

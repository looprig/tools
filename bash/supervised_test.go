package bash

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/process"
)

// --- shared fakes -----------------------------------------------------

// callOrderLog records the order distinct steps happen across several
// cooperating fakes, so a test can assert PrepareProcess precedes
// AcquireLifetime precedes Start without relying on timing.
type callOrderLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *callOrderLog) record(step string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, step)
}

func (l *callOrderLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

// fakeAsyncRunner is a tool.AsyncProcessRunner test double: it records the
// exact ProcessRequest PrepareProcess received and returns a canned
// tool.PreparedProcess (or error).
type fakeAsyncRunner struct {
	mu       sync.Mutex
	log      *callOrderLog
	calls    int
	lastReq  tool.ProcessRequest
	prepared tool.PreparedProcess
	err      error
}

func (f *fakeAsyncRunner) PrepareProcess(_ context.Context, req tool.ProcessRequest) (tool.PreparedProcess, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastReq = req
	if f.log != nil {
		f.log.record("prepare")
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.prepared, nil
}

var _ tool.AsyncProcessRunner = (*fakeAsyncRunner)(nil)

// fakePreparedProcess is a tool.PreparedProcess test double: it records
// Start/Close calls and returns a canned tool.Process (or error).
type fakePreparedProcess struct {
	log        *callOrderLog
	access     tool.WorkspaceAccess
	process    tool.Process
	startErr   error
	startCalls int
	closeCalls int
}

func (p *fakePreparedProcess) EffectiveWorkspaceAccess() tool.WorkspaceAccess { return p.access }

func (p *fakePreparedProcess) Start(context.Context) (tool.Process, error) {
	p.startCalls++
	if p.log != nil {
		p.log.record("start")
	}
	if p.startErr != nil {
		return nil, p.startErr
	}
	return p.process, nil
}

func (p *fakePreparedProcess) Close() error {
	p.closeCalls++
	return nil
}

var _ tool.PreparedProcess = (*fakePreparedProcess)(nil)

// nopWriteCloser discards every write; it stands in for a supervised
// process's stdin in tests that never write to it.
type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

// fakeProcess is a tool.Process test double. By default it exits
// immediately (empty stdout/stderr, Wait returns instantly). Setting ready
// to a non-nil, not-yet-closed channel makes Wait block until the test
// closes it — simulating a still-running process. fakeProcess always
// implements tool.ProcessActivitySource; Activities() returns activities,
// which is nil unless a test sets it, matching entry.run's own "no channel,
// no activity drain goroutine" handling.
type fakeProcess struct {
	stdout      io.ReadCloser
	stderr      io.ReadCloser
	stdin       io.WriteCloser
	mode        tool.ProcessStreamMode
	result      tool.ProcessResult
	waitErr     error
	ready       chan struct{}
	activities  chan tool.ProcessActivity
	signalCalls int
	closeCalls  int
}

func newFakeProcess(exitCode int) *fakeProcess {
	return &fakeProcess{
		stdout: io.NopCloser(strings.NewReader("")),
		stderr: io.NopCloser(strings.NewReader("")),
		stdin:  nopWriteCloser{},
		mode:   tool.ProcessStreamModePipes,
		result: tool.ProcessResult{Reason: tool.ProcessTerminalExited, ExitCode: exitCode},
	}
}

func (p *fakeProcess) Stdout() io.ReadCloser              { return p.stdout }
func (p *fakeProcess) Stderr() io.ReadCloser              { return p.stderr }
func (p *fakeProcess) Stdin() io.WriteCloser              { return p.stdin }
func (p *fakeProcess) StreamMode() tool.ProcessStreamMode { return p.mode }

func (p *fakeProcess) Wait(ctx context.Context) (tool.ProcessResult, error) {
	if p.ready != nil {
		select {
		case <-p.ready:
		case <-ctx.Done():
			return tool.ProcessResult{}, ctx.Err()
		}
	}
	if p.waitErr != nil {
		return tool.ProcessResult{}, p.waitErr
	}
	return p.result, nil
}

func (p *fakeProcess) Resize(context.Context, uint16, uint16) error { return nil }

func (p *fakeProcess) Signal(context.Context, tool.ProcessSignal) error {
	p.signalCalls++
	return nil
}

func (p *fakeProcess) Close(context.Context) error {
	p.closeCalls++
	return nil
}

func (p *fakeProcess) Activities() <-chan tool.ProcessActivity { return p.activities }

var (
	_ tool.Process               = (*fakeProcess)(nil)
	_ tool.ProcessActivitySource = (*fakeProcess)(nil)
)

// recordingLifetimeCoordinator is a tool.WorkspaceLifetimeCoordinator (and
// tool.WorkspaceCoordinator) test double: it records every AcquireLifetime
// call's access argument and every permit Release.
type recordingLifetimeCoordinator struct {
	log           *callOrderLog
	acquireErr    error
	gotAccess     tool.WorkspaceAccess
	lifetimeCalls int
	released      int
}

func (c *recordingLifetimeCoordinator) Acquire(context.Context, tool.WorkspaceOperation, string) (tool.WorkspacePermit, error) {
	return &recordingLeasePermit{c: c}, nil
}

func (c *recordingLifetimeCoordinator) Healthy() error { return nil }

func (c *recordingLifetimeCoordinator) AcquireLifetime(_ context.Context, access tool.WorkspaceAccess) (tool.WorkspacePermit, error) {
	c.lifetimeCalls++
	c.gotAccess = access
	if c.log != nil {
		c.log.record("acquire_lifetime")
	}
	if c.acquireErr != nil {
		return nil, c.acquireErr
	}
	return &recordingLeasePermit{c: c}, nil
}

type recordingLeasePermit struct{ c *recordingLifetimeCoordinator }

func (p *recordingLeasePermit) Release() { p.c.released++ }

var (
	_ tool.WorkspaceCoordinator         = (*recordingLifetimeCoordinator)(nil)
	_ tool.WorkspaceLifetimeCoordinator = (*recordingLifetimeCoordinator)(nil)
)

// syncWorkspaceObservations is a race-safe tool.WorkspaceObservations test
// double (unlike bash_test.go's recordingWorkspaceObservations, which is
// only ever touched by one goroutine at a time in its own tests): a
// supervised call's detached watchAndInvalidate goroutine can call
// InvalidateAll concurrently with a test goroutine reading the count.
type syncWorkspaceObservations struct{ invalidated atomic.Int64 }

func (*syncWorkspaceObservations) WithPath(string, func(*tool.FileObservation) error) error {
	return nil
}
func (o *syncWorkspaceObservations) InvalidateAll() { o.invalidated.Add(1) }

var _ tool.WorkspaceObservations = (*syncWorkspaceObservations)(nil)

// fakeSessionResourceRegistry is a tool.SessionResourceRegistry test double:
// GetOrCreate calls factory at most once (mirroring the real registry's
// singleflight contract closely enough for a sequential test) and caches
// the result, or returns err when set.
type fakeSessionResourceRegistry struct {
	mu       sync.Mutex
	dir      string
	resource tool.SessionResource
	err      error
	calls    int
}

func (r *fakeSessionResourceRegistry) GetOrCreate(_ context.Context, _ string, factory func(string) (tool.SessionResource, error)) (tool.SessionResource, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.err != nil {
		return nil, r.err
	}
	if r.resource == nil {
		res, err := factory(r.dir)
		if err != nil {
			return nil, err
		}
		r.resource = res
	}
	return r.resource, nil
}

var _ tool.SessionResourceRegistry = (*fakeSessionResourceRegistry)(nil)

// --- test setup helpers -------------------------------------------------

// newSupervisedTestTool builds a *BashTool via NewSupervisedFactory bound to
// a fresh workspace root, session/loop identity, and a fresh
// fakeSessionResourceRegistry (returned so a test can mutate its err field).
func newSupervisedTestTool(t *testing.T, runner tool.AsyncProcessRunner, coord tool.WorkspaceCoordinator, obs tool.WorkspaceObservations) (*BashTool, *fakeSessionResourceRegistry) {
	t.Helper()
	factory, err := NewSupervisedFactory()
	if err != nil {
		t.Fatalf("NewSupervisedFactory() error = %v", err)
	}
	registry := &fakeSessionResourceRegistry{dir: t.TempDir()}
	bindings := tool.Bindings{
		SessionID: mustUUID(t),
		LoopID:    mustUUID(t),
		Workspace: &tool.WorkspaceBinding{Root: t.TempDir(), Coordinator: coord, Observations: obs},
		Process:   &tool.ProcessBinding{Registry: registry},
	}
	b, err := factory(bindings, runner)
	if err != nil {
		t.Fatalf("factory() error = %v", err)
	}
	return b, registry
}

// runSupervisedCall prepares and runs one supervised Bash call, decoding
// the resulting text as strict JSON into a supervisedResult — decoding with
// DisallowUnknownFields means ANY extra top-level key (a host path, an OS
// PID, anything not in supervisedResult's closed field set) fails the
// decode, so every caller of this helper already partially exercises "no
// JSON contains host path or OS PID".
func runSupervisedCall(t *testing.T, b *BashTool, argsJSON string, grants []string) (supervisedResult, string, tool.PreparedCall) {
	t.Helper()
	id := mustUUID(t)
	req, art, err := b.PrepareCall(context.Background(), id, argsJSON)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	call := tool.PreparedCall{ExecutionID: id, Request: req, Artifact: art, Grants: grants}
	ctx := loop.WithPreparedCall(context.Background(), call)
	res, err := b.InvokableRun(ctx, argsJSON)
	if err != nil {
		t.Fatalf("InvokableRun() returned a Go error %v; Bash returns tool-result strings", err)
	}
	text := textOf(t, res)
	return decodeSupervisedResult(t, text), text, call
}

// decodeSupervisedResult strictly decodes text as a supervisedResult —
// DisallowUnknownFields means ANY extra top-level key (a host path, an OS
// PID, anything not in supervisedResult's closed field set) fails the
// decode. Factored out of runSupervisedCall so a caller that drives
// InvokableRun directly (TestSupervisedBashRunnerSelectionFixedAtBuildNotFromContext,
// which must reuse a single base ctx across two calls, and so can't go
// through runSupervisedCall's own PrepareCall/loop.WithPreparedCall
// sequencing) can still recover a handle worth synchronizing on with
// waitProcessDone.
func decodeSupervisedResult(t *testing.T, text string) supervisedResult {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(text))
	dec.DisallowUnknownFields()
	var out supervisedResult
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("result %q is not the expected supervisedResult JSON: %v", text, err)
	}
	return out
}

// waitProcessDone blocks briefly on registry's cached *process.Supervisor
// until processID's entry reaches a terminal state, letting its entry's
// background termination goroutine finish the SAME manifest/spool writes
// into this test's t.TempDir()-rooted registry directory before that
// TempDir's cleanup runs — closing the exact "TempDir RemoveAll cleanup:
// directory not empty" race process/supervisor_test.go's waitEntryDone doc
// comment describes, but through Supervisor.Wait's exported API: unlike
// package process's own tests, bash has no access to the unexported
// entry/done this package's Supervisor wraps.
//
// processID == "" (a TERMINAL or ERROR result names no live process — its
// own background goroutine, if any, has already fully terminalized before
// runSupervised ever returns; see supervised.go's waitForTerminal) and a
// registry that never reached GetOrCreate are both no-ops: there is nothing
// to wait on.
//
// The wait is bounded and its outcome deliberately ignored: a process a
// test keeps deliberately running (an unclosed proc.ready) simply times out
// here, which must never fail the test — this exists only to close the
// race for a process that actually finishes, not to force every caller to
// wait out a live process's full lifetime.
func waitProcessDone(t *testing.T, registry *fakeSessionResourceRegistry, owner process.Owner, processID string) {
	t.Helper()
	if processID == "" {
		return
	}
	registry.mu.Lock()
	resource := registry.resource
	registry.mu.Unlock()
	sr, ok := resource.(*process.SupervisorResource)
	if !ok || sr == nil || sr.Supervisor == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _ = sr.Supervisor.Wait(ctx, owner, process.WaitAll, []process.WaitTarget{{Handle: process.Handle(processID)}})
}

// waitForCount polls get with a short bounded retry until it reports at
// least want, or fails the test after timeout — used to observe the
// detached watchAndInvalidate goroutine's asynchronous InvalidateAll calls
// without a fixed sleep.
func waitForCount(t *testing.T, get func() int64, want int64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if got := get(); got >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for count >= %d (last = %d)", want, get())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func freshWorkspaceAccess() tool.WorkspaceAccess {
	return tool.NewWorkspaceAccess(tool.WorkspaceAccessReadOnly, nil, nil)
}

// --- workflow tests -------------------------------------------------

// TestSupervisedBashExplicitBackgroundReturnsAfterDurableRegistration
// asserts a `background:true` call returns a live, backgrounded result as
// soon as the process is durably registered (Start already persists the
// StateStarting manifest and registers the entry before ever returning a
// Handle), without waiting for any output.
func TestSupervisedBashExplicitBackgroundReturnsAfterDurableRegistration(t *testing.T) {
	t.Parallel()
	proc := newFakeProcess(0)
	proc.ready = make(chan struct{})
	t.Cleanup(func() { close(proc.ready) })
	prepared := &fakePreparedProcess{access: freshWorkspaceAccess(), process: proc}
	runner := &fakeAsyncRunner{prepared: prepared}
	coord := &recordingLifetimeCoordinator{}
	obs := &syncWorkspaceObservations{}
	b, _ := newSupervisedTestTool(t, runner, coord, obs)

	out, _, _ := runSupervisedCall(t, b, `{"command":"sleep 100","background":true}`, nil)

	if out.ProcessID == "" {
		t.Fatalf("result = %+v, want a non-empty handle", out)
	}
	if !out.Backgrounded {
		t.Errorf("Backgrounded = false, want true for an explicit background call")
	}
	if prepared.startCalls != 1 {
		t.Errorf("prepared.Start called %d times, want 1", prepared.startCalls)
	}
}

// TestSupervisedBashYieldedExitsWithinBudgetReturnsTerminalJSON asserts a
// `yield_time_ms` call whose command exits before the budget elapses
// returns a TERMINAL result carrying the real exit code from the process's
// own durable manifest.
func TestSupervisedBashYieldedExitsWithinBudgetReturnsTerminalJSON(t *testing.T) {
	t.Parallel()
	proc := newFakeProcess(3)
	prepared := &fakePreparedProcess{access: freshWorkspaceAccess(), process: proc}
	runner := &fakeAsyncRunner{prepared: prepared}
	coord := &recordingLifetimeCoordinator{}
	obs := &syncWorkspaceObservations{}
	b, _ := newSupervisedTestTool(t, runner, coord, obs)

	out, _, _ := runSupervisedCall(t, b, `{"command":"true","yield_time_ms":2000}`, nil)

	if out.Backgrounded {
		t.Errorf("Backgrounded = true, want false for a call that exited within budget")
	}
	if out.ProcessID != "" {
		t.Errorf("ProcessID = %q, want empty (a terminal result names no live process)", out.ProcessID)
	}
	if out.Status != string(process.StateExited) {
		t.Errorf("Status = %q, want %q", out.Status, process.StateExited)
	}
	if out.ExitCode == nil || *out.ExitCode != 3 {
		t.Errorf("ExitCode = %v, want 3", out.ExitCode)
	}
	if out.StartedAt == "" || out.FinishedAt == "" {
		t.Errorf("StartedAt/FinishedAt = %q/%q, want both populated from the durable manifest", out.StartedAt, out.FinishedAt)
	}
	if out.DurationMS == nil {
		t.Errorf("DurationMS = nil, want a computed duration")
	}
}

// TestSupervisedBashYieldedLiveReturnsHandleCursorOutputBackgrounded asserts
// a `yield_time_ms` call whose command is still running once the budget
// elapses returns a LIVE result: handle, cursor, output, and
// backgrounded:true.
func TestSupervisedBashYieldedLiveReturnsHandleCursorOutputBackgrounded(t *testing.T) {
	t.Parallel()
	proc := newFakeProcess(0)
	proc.ready = make(chan struct{})
	t.Cleanup(func() { close(proc.ready) })
	prepared := &fakePreparedProcess{access: freshWorkspaceAccess(), process: proc}
	runner := &fakeAsyncRunner{prepared: prepared}
	coord := &recordingLifetimeCoordinator{}
	obs := &syncWorkspaceObservations{}
	b, _ := newSupervisedTestTool(t, runner, coord, obs)

	out, _, _ := runSupervisedCall(t, b, `{"command":"sleep 100","yield_time_ms":20}`, nil)

	if out.ProcessID == "" {
		t.Fatalf("result = %+v, want a non-empty handle", out)
	}
	if !out.Backgrounded {
		t.Errorf("Backgrounded = false, want true for a call whose budget elapsed while still running")
	}
	if out.NextCursor != 0 {
		t.Errorf("NextCursor = %d, want 0 (no output-read accessor exists yet — see result.go)", out.NextCursor)
	}
	if out.Output != "" {
		t.Errorf("Output = %q, want empty", out.Output)
	}
	if out.StartedAt == "" {
		t.Errorf("StartedAt is empty, want the durable manifest's StartedAt")
	}
}

// TestSupervisedBashHardLifetimeTimeoutReachesRequestDeadline asserts a
// supervised call's clamped timeout becomes a concrete tool.ProcessRequest
// Deadline roughly `timeout` seconds out from now.
func TestSupervisedBashHardLifetimeTimeoutReachesRequestDeadline(t *testing.T) {
	t.Parallel()
	proc := newFakeProcess(0)
	prepared := &fakePreparedProcess{access: freshWorkspaceAccess(), process: proc}
	runner := &fakeAsyncRunner{prepared: prepared}
	coord := &recordingLifetimeCoordinator{}
	obs := &syncWorkspaceObservations{}
	b, registry := newSupervisedTestTool(t, runner, coord, obs)

	before := time.Now()
	out, _, _ := runSupervisedCall(t, b, `{"command":"true","background":true,"timeout":5}`, nil)
	after := time.Now()

	if runner.lastReq.Deadline.IsZero() {
		t.Fatalf("Deadline is zero, want a hard deadline ~5s out")
	}
	wantMin := before.Add(5 * time.Second)
	wantMax := after.Add(5 * time.Second)
	if runner.lastReq.Deadline.Before(wantMin) || runner.lastReq.Deadline.After(wantMax) {
		t.Errorf("Deadline = %v, want between %v and %v", runner.lastReq.Deadline, wantMin, wantMax)
	}
	waitProcessDone(t, registry, b.owner, out.ProcessID)
}

// TestSupervisedBashTimeoutZeroUnderSupervisionMeansNoDeadline asserts
// supervised `timeout: 0` becomes a ZERO tool.ProcessRequest Deadline (run
// until session shutdown), never a fabricated one.
func TestSupervisedBashTimeoutZeroUnderSupervisionMeansNoDeadline(t *testing.T) {
	t.Parallel()
	proc := newFakeProcess(0)
	prepared := &fakePreparedProcess{access: freshWorkspaceAccess(), process: proc}
	runner := &fakeAsyncRunner{prepared: prepared}
	coord := &recordingLifetimeCoordinator{}
	obs := &syncWorkspaceObservations{}
	b, registry := newSupervisedTestTool(t, runner, coord, obs)

	out, _, _ := runSupervisedCall(t, b, `{"command":"true","background":true,"timeout":0}`, nil)

	if !runner.lastReq.Deadline.IsZero() {
		t.Errorf("Deadline = %v, want zero", runner.lastReq.Deadline)
	}
	waitProcessDone(t, registry, b.owner, out.ProcessID)
}

// TestSupervisedBashPTYAndMaxOutputBytesReachRunnerAndCeiling asserts `tty`
// reaches the async runner's ProcessRequest.PTY, and `max_output_bytes`
// (which ProcessRequest has no field for — output retention is Tools' own
// concern) reaches the Supervisor's StorageCeiling.SpoolBytes instead.
func TestSupervisedBashPTYAndMaxOutputBytesReachRunnerAndCeiling(t *testing.T) {
	t.Parallel()
	proc := newFakeProcess(0)
	prepared := &fakePreparedProcess{access: freshWorkspaceAccess(), process: proc}
	runner := &fakeAsyncRunner{prepared: prepared}
	coord := &recordingLifetimeCoordinator{}
	obs := &syncWorkspaceObservations{}
	b, registry := newSupervisedTestTool(t, runner, coord, obs)

	out, _, _ := runSupervisedCall(t, b, `{"command":"true","background":true,"tty":true,"max_output_bytes":4096}`, nil)

	if !runner.lastReq.PTY {
		t.Errorf("ProcessRequest.PTY = false, want true")
	}
	waitProcessDone(t, registry, b.owner, out.ProcessID)
}

// TestSupervisedBashStorageCeilingFromMaxOutputBytes unit-tests
// storageCeiling directly: a present max_output_bytes becomes
// StorageCeiling.SpoolBytes; an absent one yields the zero StorageCeiling
// (Supervisor.Start's own per-process default applies).
func TestSupervisedBashStorageCeilingFromMaxOutputBytes(t *testing.T) {
	t.Parallel()
	withMax := &bashArtifact{hasMaxOutputBytes: true, maxOutputBytes: 4096}
	if got := storageCeiling(withMax); got.SpoolBytes != 4096 {
		t.Errorf("storageCeiling(with max) = %+v, want SpoolBytes 4096", got)
	}
	without := &bashArtifact{}
	if got := storageCeiling(without); got != (process.StorageCeiling{}) {
		t.Errorf("storageCeiling(without max) = %+v, want the zero StorageCeiling", got)
	}
}

// TestSupervisedBashPrepareProcessRunsBeforeLeaseAndDoesNotSpawn asserts
// PrepareProcess is called exactly once, before AcquireLifetime, and that
// PrepareProcess alone never causes Start to be called (no spawn until the
// lease is actually held).
func TestSupervisedBashPrepareProcessRunsBeforeLeaseAndDoesNotSpawn(t *testing.T) {
	t.Parallel()
	log := &callOrderLog{}
	proc := newFakeProcess(0)
	prepared := &fakePreparedProcess{log: log, access: freshWorkspaceAccess(), process: proc}
	runner := &fakeAsyncRunner{log: log, prepared: prepared}
	coord := &recordingLifetimeCoordinator{log: log}
	obs := &syncWorkspaceObservations{}
	b, registry := newSupervisedTestTool(t, runner, coord, obs)

	out, _, _ := runSupervisedCall(t, b, `{"command":"true","background":true}`, nil)

	want := []string{"prepare", "acquire_lifetime", "start"}
	if got := log.snapshot(); !slices.Equal(got, want) {
		t.Fatalf("call order = %v, want %v", got, want)
	}
	waitProcessDone(t, registry, b.owner, out.ProcessID)
}

// TestSupervisedBashLeaseUsesPreparedAuthoritativeAccess asserts the
// PREPARED process's own EffectiveWorkspaceAccess — never any
// caller-declared or guessed access — is exactly what AcquireLifetime
// receives.
func TestSupervisedBashLeaseUsesPreparedAuthoritativeAccess(t *testing.T) {
	t.Parallel()
	access := tool.NewWorkspaceAccess(tool.WorkspaceAccessScopedWrite, []string{"/workspace/a"}, nil)
	proc := newFakeProcess(0)
	prepared := &fakePreparedProcess{access: access, process: proc}
	runner := &fakeAsyncRunner{prepared: prepared}
	coord := &recordingLifetimeCoordinator{}
	obs := &syncWorkspaceObservations{}
	b, registry := newSupervisedTestTool(t, runner, coord, obs)

	out, _, _ := runSupervisedCall(t, b, `{"command":"true","background":true}`, nil)

	if coord.gotAccess.Kind != tool.WorkspaceAccessScopedWrite {
		t.Errorf("AcquireLifetime access.Kind = %v, want %v", coord.gotAccess.Kind, tool.WorkspaceAccessScopedWrite)
	}
	if got := coord.gotAccess.WritePaths(); !slices.Equal(got, []string{"/workspace/a"}) {
		t.Errorf("AcquireLifetime access.WritePaths() = %v, want [/workspace/a]", got)
	}
	waitProcessDone(t, registry, b.owner, out.ProcessID)
}

// TestSupervisedBashStartOnlyAfterLeaseConsumesPreparationOnce asserts
// Start is reached only after the lease is held (see the shared call-order
// test above) and is called exactly once, with Close never called on the
// success path (Start itself owns the single-use preparation once it
// succeeds).
func TestSupervisedBashStartOnlyAfterLeaseConsumesPreparationOnce(t *testing.T) {
	t.Parallel()
	proc := newFakeProcess(0)
	prepared := &fakePreparedProcess{access: freshWorkspaceAccess(), process: proc}
	runner := &fakeAsyncRunner{prepared: prepared}
	coord := &recordingLifetimeCoordinator{}
	obs := &syncWorkspaceObservations{}
	b, registry := newSupervisedTestTool(t, runner, coord, obs)

	out, _, _ := runSupervisedCall(t, b, `{"command":"true","background":true}`, nil)

	if prepared.startCalls != 1 {
		t.Errorf("Start called %d times, want exactly 1", prepared.startCalls)
	}
	if prepared.closeCalls != 0 {
		t.Errorf("Close called %d times, want 0 on the success path", prepared.closeCalls)
	}
	waitProcessDone(t, registry, b.owner, out.ProcessID)
}

// TestSupervisedBashFailureClosesPreparationAndReleasesReservations covers
// every failure point after PrepareProcess succeeds: each one must close
// the preparation and release every reservation/lease it had already
// acquired, without double-releasing what Start itself already released on
// its own failure paths.
func TestSupervisedBashFailureClosesPreparationAndReleasesReservations(t *testing.T) {
	t.Parallel()

	t.Run("AcquireLifetime fails", func(t *testing.T) {
		t.Parallel()
		proc := newFakeProcess(0)
		prepared := &fakePreparedProcess{access: freshWorkspaceAccess(), process: proc}
		runner := &fakeAsyncRunner{prepared: prepared}
		coord := &recordingLifetimeCoordinator{acquireErr: errors.New("lease unavailable")}
		obs := &syncWorkspaceObservations{}
		b, _ := newSupervisedTestTool(t, runner, coord, obs)

		out, _, _ := runSupervisedCall(t, b, `{"command":"true","background":true}`, nil)

		if out.Error != string(process.CodeLifetimeEnforcementUnavailable) {
			t.Errorf("Error = %q, want %q", out.Error, process.CodeLifetimeEnforcementUnavailable)
		}
		if prepared.closeCalls != 1 {
			t.Errorf("prepared.Close() called %d times, want 1", prepared.closeCalls)
		}
		if prepared.startCalls != 0 {
			t.Errorf("prepared.Start() called %d times, want 0", prepared.startCalls)
		}
	})

	t.Run("registry GetOrCreate fails", func(t *testing.T) {
		t.Parallel()
		proc := newFakeProcess(0)
		prepared := &fakePreparedProcess{access: freshWorkspaceAccess(), process: proc}
		runner := &fakeAsyncRunner{prepared: prepared}
		coord := &recordingLifetimeCoordinator{}
		obs := &syncWorkspaceObservations{}
		b, registry := newSupervisedTestTool(t, runner, coord, obs)
		registry.err = errors.New("registry unavailable")

		out, _, _ := runSupervisedCall(t, b, `{"command":"true","background":true}`, nil)

		if out.Error != string(process.CodeProcessSetupFailed) {
			t.Errorf("Error = %q, want %q", out.Error, process.CodeProcessSetupFailed)
		}
		if prepared.closeCalls != 1 {
			t.Errorf("prepared.Close() called %d times, want 1", prepared.closeCalls)
		}
		if coord.released != 1 {
			t.Errorf("lease released %d times, want 1", coord.released)
		}
		if prepared.startCalls != 0 {
			t.Errorf("prepared.Start() called %d times, want 0", prepared.startCalls)
		}
	})
}

// TestSupervisedBashMissingAsyncRunnerReturnsLifetimeEnforcementUnavailable
// asserts a Bash instance with NO async runner bound (the plain,
// foreground-only NewBash/NewFactory construction) fails a supervised call
// closed with lifetime_enforcement_unavailable rather than panicking or
// silently running it synchronously.
func TestSupervisedBashMissingAsyncRunnerReturnsLifetimeEnforcementUnavailable(t *testing.T) {
	t.Parallel()
	coord := &recordingLifetimeCoordinator{}
	obs := &syncWorkspaceObservations{}
	b := NewBash(t.TempDir(), WithWorkspaceCoordinator(coord), WithObservations(obs))

	out, _, _ := runSupervisedCall(t, b, `{"command":"true","background":true}`, nil)

	if out.Error != string(process.CodeLifetimeEnforcementUnavailable) {
		t.Errorf("Error = %q, want %q", out.Error, process.CodeLifetimeEnforcementUnavailable)
	}
	if out.ProcessID != "" {
		t.Errorf("ProcessID = %q, want empty on a failed call", out.ProcessID)
	}
}

// TestSupervisedBashMissingAccessSummaryReturnsLifetimeEnforcementUnavailable
// asserts a workspace coordinator that does not implement
// tool.WorkspaceLifetimeCoordinator (cannot summarize authoritative access
// for a lifetime lease) fails closed the same way, WITHOUT ever calling
// PrepareProcess.
func TestSupervisedBashMissingAccessSummaryReturnsLifetimeEnforcementUnavailable(t *testing.T) {
	t.Parallel()
	proc := newFakeProcess(0)
	prepared := &fakePreparedProcess{access: freshWorkspaceAccess(), process: proc}
	runner := &fakeAsyncRunner{prepared: prepared}
	coord := &recordingWorkspaceCoordinator{} // no AcquireLifetime (bash_test.go)
	obs := &syncWorkspaceObservations{}
	b, _ := newSupervisedTestTool(t, runner, coord, obs)

	out, _, _ := runSupervisedCall(t, b, `{"command":"true","background":true}`, nil)

	if out.Error != string(process.CodeLifetimeEnforcementUnavailable) {
		t.Errorf("Error = %q, want %q", out.Error, process.CodeLifetimeEnforcementUnavailable)
	}
	if runner.calls != 0 {
		t.Errorf("PrepareProcess called %d times, want 0 (the missing-capability check happens up front)", runner.calls)
	}
}

// TestSupervisedBashRunnerSelectionFixedAtBuildNotFromContext asserts the
// SAME Build-time async runner services every supervised call regardless of
// unrelated values riding on each call's context — Bash never consults
// invocation provenance to (re)select a runner.
func TestSupervisedBashRunnerSelectionFixedAtBuildNotFromContext(t *testing.T) {
	t.Parallel()
	proc := newFakeProcess(0)
	prepared := &fakePreparedProcess{access: freshWorkspaceAccess(), process: proc}
	runner := &fakeAsyncRunner{prepared: prepared}
	coord := &recordingLifetimeCoordinator{}
	obs := &syncWorkspaceObservations{}
	b, registry := newSupervisedTestTool(t, runner, coord, obs)

	type provenanceKey struct{}
	argsJSON := `{"command":"true","background":true}`
	for _, provenance := range []string{"loop-1", "loop-2"} {
		base := context.WithValue(context.Background(), provenanceKey{}, provenance)
		id := mustUUID(t)
		req, art, err := b.PrepareCall(base, id, argsJSON)
		if err != nil {
			t.Fatalf("PrepareCall() error = %v", err)
		}
		ctx := loop.WithPreparedCall(base, tool.PreparedCall{ExecutionID: id, Request: req, Artifact: art})
		res, err := b.InvokableRun(ctx, argsJSON)
		if err != nil {
			t.Fatalf("InvokableRun() error = %v", err)
		}
		out := decodeSupervisedResult(t, textOf(t, res))
		waitProcessDone(t, registry, b.owner, out.ProcessID)
	}

	if runner.calls != 2 {
		t.Fatalf("runner.calls = %d, want 2 (the SAME Build-time runner services every call)", runner.calls)
	}
}

// TestSupervisedBashRequestCarriesExecutionIDAndGrantsExactly asserts the
// prepared call's ExecutionID and Grants — never anything else — reach
// tool.ProcessRequest's OriginExecutionID and Grants.
func TestSupervisedBashRequestCarriesExecutionIDAndGrantsExactly(t *testing.T) {
	t.Parallel()
	proc := newFakeProcess(0)
	prepared := &fakePreparedProcess{access: freshWorkspaceAccess(), process: proc}
	runner := &fakeAsyncRunner{prepared: prepared}
	coord := &recordingLifetimeCoordinator{}
	obs := &syncWorkspaceObservations{}
	b, registry := newSupervisedTestTool(t, runner, coord, obs)

	grants := []string{"grant-x", "grant-y"}
	out, _, call := runSupervisedCall(t, b, `{"command":"true","background":true}`, grants)

	if runner.lastReq.OriginExecutionID != call.ExecutionID {
		t.Errorf("OriginExecutionID = %v, want %v", runner.lastReq.OriginExecutionID, call.ExecutionID)
	}
	if !slices.Equal(runner.lastReq.Grants, grants) {
		t.Errorf("Grants = %v, want %v", runner.lastReq.Grants, grants)
	}
	waitProcessDone(t, registry, b.owner, out.ProcessID)
}

// TestSupervisedBashSpawnActivityAndCompletionInvalidateObservations asserts
// the bound loop's observation set is invalidated at spawn, at least once
// more while the process is still producing output ("activity" — see
// watchAndInvalidate's doc comment for why this is driven by output
// generation bumps rather than tool.ProcessActivitySource directly, which
// Bash cannot bridge to process's package-private observationInvalidator
// from outside package process), and again at completion.
func TestSupervisedBashSpawnActivityAndCompletionInvalidateObservations(t *testing.T) {
	t.Parallel()
	stdoutR, stdoutW := io.Pipe()
	proc := newFakeProcess(0)
	proc.stdout = stdoutR
	proc.ready = make(chan struct{})
	prepared := &fakePreparedProcess{access: freshWorkspaceAccess(), process: proc}
	runner := &fakeAsyncRunner{prepared: prepared}
	coord := &recordingLifetimeCoordinator{}
	obs := &syncWorkspaceObservations{}
	b, _ := newSupervisedTestTool(t, runner, coord, obs)

	out, _, _ := runSupervisedCall(t, b, `{"command":"sleep 100","background":true}`, nil)
	if !out.Backgrounded {
		t.Fatalf("result = %+v, want a backgrounded live result", out)
	}
	waitForCount(t, func() int64 { return obs.invalidated.Load() }, 1, time.Second) // spawn

	if _, err := stdoutW.Write([]byte("intermediate output\n")); err != nil {
		t.Fatalf("write stdout: %v", err)
	}
	waitForCount(t, func() int64 { return obs.invalidated.Load() }, 2, time.Second) // activity/output

	if err := stdoutW.Close(); err != nil {
		t.Fatalf("close stdout: %v", err)
	}
	close(proc.ready)
	waitForCount(t, func() int64 { return obs.invalidated.Load() }, 3, time.Second) // completion
}

// TestSupervisedBashInvocationCancellationAfterHandleDoesNotKillProcess
// asserts canceling the INVOCATION context after a live handle has already
// been returned never signals the still-running process.
func TestSupervisedBashInvocationCancellationAfterHandleDoesNotKillProcess(t *testing.T) {
	t.Parallel()
	proc := newFakeProcess(0)
	proc.ready = make(chan struct{})
	t.Cleanup(func() { close(proc.ready) })
	prepared := &fakePreparedProcess{access: freshWorkspaceAccess(), process: proc}
	runner := &fakeAsyncRunner{prepared: prepared}
	coord := &recordingLifetimeCoordinator{}
	obs := &syncWorkspaceObservations{}
	b, _ := newSupervisedTestTool(t, runner, coord, obs)

	id := mustUUID(t)
	argsJSON := `{"command":"sleep 100","background":true}`
	baseCtx, cancel := context.WithCancel(context.Background())
	req, art, err := b.PrepareCall(baseCtx, id, argsJSON)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	ctx := loop.WithPreparedCall(baseCtx, tool.PreparedCall{ExecutionID: id, Request: req, Artifact: art})
	res, err := b.InvokableRun(ctx, argsJSON)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	_ = textOf(t, res)

	cancel()
	time.Sleep(20 * time.Millisecond)

	if proc.signalCalls != 0 {
		t.Errorf("Signal called %d times after invocation cancellation, want 0", proc.signalCalls)
	}
}

// TestSupervisedBashResultNeverLeaksHostPathOrPID asserts none of the LIVE,
// TERMINAL, or ERROR JSON shapes ever contain the host workspace root or
// any OS-PID-shaped substring.
func TestSupervisedBashResultNeverLeaksHostPathOrPID(t *testing.T) {
	t.Parallel()

	check := func(t *testing.T, root, text string) {
		t.Helper()
		if strings.Contains(text, root) {
			t.Errorf("result %q contains the host workspace root %q", text, root)
		}
		for _, forbidden := range []string{"pid", "PID", "os_metadata"} {
			if strings.Contains(text, forbidden) {
				t.Errorf("result %q unexpectedly contains %q", text, forbidden)
			}
		}
	}

	t.Run("live", func(t *testing.T) {
		t.Parallel()
		proc := newFakeProcess(0)
		proc.ready = make(chan struct{})
		t.Cleanup(func() { close(proc.ready) })
		prepared := &fakePreparedProcess{access: freshWorkspaceAccess(), process: proc}
		runner := &fakeAsyncRunner{prepared: prepared}
		coord := &recordingLifetimeCoordinator{}
		obs := &syncWorkspaceObservations{}
		b, _ := newSupervisedTestTool(t, runner, coord, obs)
		_, text, _ := runSupervisedCall(t, b, `{"command":"true","background":true}`, nil)
		check(t, b.root, text)
	})

	t.Run("terminal", func(t *testing.T) {
		t.Parallel()
		proc := newFakeProcess(0)
		prepared := &fakePreparedProcess{access: freshWorkspaceAccess(), process: proc}
		runner := &fakeAsyncRunner{prepared: prepared}
		coord := &recordingLifetimeCoordinator{}
		obs := &syncWorkspaceObservations{}
		b, _ := newSupervisedTestTool(t, runner, coord, obs)
		_, text, _ := runSupervisedCall(t, b, `{"command":"true","yield_time_ms":2000}`, nil)
		check(t, b.root, text)
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		coord := &recordingLifetimeCoordinator{}
		obs := &syncWorkspaceObservations{}
		b := NewBash(t.TempDir(), WithWorkspaceCoordinator(coord), WithObservations(obs))
		_, text, _ := runSupervisedCall(t, b, `{"command":"true","background":true}`, nil)
		check(t, b.root, text)
	})
}

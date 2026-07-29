package process

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/tool"
)

// --- Task 9C coordinated-shutdown tests ---
//
// These tests exercise Supervisor.Shutdown against fakePreparedProcess/
// fakeProcess (fake_process_test.go), which Task 9C extended with
// waitBlock/signalFunc/signalCalls specifically for these scenarios: a fake
// process that keeps "running" (Wait blocked on waitBlock) until its
// signalFunc callback decides to let it exit, so a test can deterministically
// control exactly when -- and in response to which signal -- a supervised
// process actually terminates.

// shutdownTestConfig returns a Config with generous quota limits (so Start
// never itself becomes the bottleneck) and the given grace period, matching
// this file's other tests' shared shape.
func shutdownTestConfig(grace time.Duration) Config {
	return Config{
		MaxRunningProcessesPerLoop:    4,
		MaxRunningProcessesPerSession: 4,
		MaxProcessInMemoryBytes:       100,
		MaxAggregateInMemoryBytes:     1000,
		MaxProcessSpoolBytes:          200,
		MaxAggregateSpoolBytes:        2000,
		GracefulShutdownPeriod:        grace,
	}
}

// startShutdownFake admits a fakeProcess whose Wait blocks on waitBlock until
// signalFunc decides to close it, returning the admitted Handle for
// convenience.
func startShutdownFake(t *testing.T, sup *Supervisor, owner Owner, origin Origin, proc *fakeProcess, lease Lease) Handle {
	t.Helper()
	prepared := &fakePreparedProcess{process: proc}
	handle, err := sup.Start(context.Background(), owner, origin, prepared, lease, nil, nil, StorageCeiling{}, YieldSettings{})
	if err != nil {
		t.Fatalf("Start() err = %v, want nil", err)
	}
	return handle
}

// --- TestShutdownClosesAdmissionBeforeStop ---

// TestShutdownClosesAdmissionBeforeStop proves that Shutdown closes
// admission (closeAdmission) before its later termination/escalation steps
// have completed -- not only once the whole call returns. The fake process
// here ignores tool.ProcessSignalTerminate entirely and only exits once
// tool.ProcessSignalKill is delivered, and the configured
// GracefulShutdownPeriod is long enough that this test can reliably observe
// Shutdown still in flight (via a non-blocking check on its result channel)
// at the moment it proves a concurrent Start call is already rejected.
func TestShutdownClosesAdmissionBeforeStop(t *testing.T) {
	t.Parallel()

	sup := newTestSupervisor(t, shutdownTestConfig(500*time.Millisecond))
	owner := testOwner(t)
	origin := testOrigin(t)

	proc := &fakeProcess{waitBlock: make(chan struct{})}
	proc.signalFunc = func(sig tool.ProcessSignal) error {
		if sig == tool.ProcessSignalKill {
			close(proc.waitBlock)
		}
		return nil
	}
	startShutdownFake(t, sup, owner, origin, proc, &fakeLease{})

	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- sup.Shutdown(context.Background())
	}()

	// Wait until admission has actually closed, without depending on any
	// particular scheduling delay.
	deadline := time.Now().Add(2 * time.Second)
	for {
		sup.mu.Lock()
		closedAdmission := sup.shuttingDown
		sup.mu.Unlock()
		if closedAdmission {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for Shutdown to close admission")
		}
		time.Sleep(time.Millisecond)
	}

	// Shutdown must still be in flight here: its escalation/confirmation
	// steps only complete once the (long) grace period elapses and the kill
	// signal is honored.
	select {
	case res := <-shutdownDone:
		t.Fatalf("Shutdown() already returned (result %v) by the time admission was observed closed; want it still terminating", res)
	default:
	}

	prepared := &fakePreparedProcess{process: &fakeProcess{}}
	_, err := sup.Start(context.Background(), owner, origin, prepared, &fakeLease{}, nil, nil, StorageCeiling{}, YieldSettings{})
	if !errors.Is(err, New(CodeSupervisorShuttingDown)) {
		t.Fatalf("Start() during in-flight Shutdown err = %v, want CodeSupervisorShuttingDown", err)
	}
	if got := prepared.StartCalls(); got != 0 {
		t.Errorf("PreparedProcess.Start called %d times during shutdown, want 0", got)
	}

	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("Shutdown() err = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Shutdown to complete")
	}
}

// --- TestShutdownEscalatesAndConfirmsTrees ---

// TestShutdownEscalatesAndConfirmsTrees proves the escalation half of the
// ordered shutdown sequence end to end: a fake process that ignores
// tool.ProcessSignalTerminate (never exits on its own) but exits promptly
// once tool.ProcessSignalKill is delivered still results in Shutdown
// completing successfully -- once the configured grace period elapses and
// the kill signal actually goes out -- and the fake's recorded Signal calls
// prove the exact terminate-then-kill order.
func TestShutdownEscalatesAndConfirmsTrees(t *testing.T) {
	t.Parallel()

	const grace = 30 * time.Millisecond
	sup := newTestSupervisor(t, shutdownTestConfig(grace))
	owner := testOwner(t)
	origin := testOrigin(t)

	proc := &fakeProcess{waitBlock: make(chan struct{})}
	proc.signalFunc = func(sig tool.ProcessSignal) error {
		if sig == tool.ProcessSignalKill {
			close(proc.waitBlock)
		}
		return nil
	}
	startShutdownFake(t, sup, owner, origin, proc, &fakeLease{})

	start := time.Now()
	if err := sup.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() err = %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed < grace {
		t.Errorf("Shutdown() returned after %v, want at least the configured grace period %v (kill must not fire before escalation)", elapsed, grace)
	}

	calls := proc.SignalCalls()
	if len(calls) != 2 {
		t.Fatalf("Signal called %d times, want exactly 2 (terminate, then kill): %v", len(calls), calls)
	}
	if calls[0] != tool.ProcessSignalTerminate {
		t.Errorf("first Signal call = %v, want ProcessSignalTerminate", calls[0])
	}
	if calls[1] != tool.ProcessSignalKill {
		t.Errorf("second Signal call = %v, want ProcessSignalKill", calls[1])
	}
}

// --- TestShutdownConcurrentCallersShareResult ---

// TestShutdownConcurrentCallersShareResult proves that calling Shutdown from
// many goroutines concurrently runs the underlying termination sequence
// exactly once -- the fake process here is signaled exactly twice in total
// (terminate, then kill), never once per caller -- and that every caller
// observes the exact same result.
func TestShutdownConcurrentCallersShareResult(t *testing.T) {
	t.Parallel()

	sup := newTestSupervisor(t, shutdownTestConfig(20*time.Millisecond))
	owner := testOwner(t)
	origin := testOrigin(t)

	proc := &fakeProcess{waitBlock: make(chan struct{})}
	proc.signalFunc = func(sig tool.ProcessSignal) error {
		if sig == tool.ProcessSignalKill {
			close(proc.waitBlock)
		}
		return nil
	}
	startShutdownFake(t, sup, owner, origin, proc, &fakeLease{})

	const callers = 8
	results := make([]error, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func(i int) {
			defer wg.Done()
			results[i] = sup.Shutdown(context.Background())
		}(i)
	}
	wg.Wait()

	for i, err := range results {
		if err != nil {
			t.Errorf("Shutdown() caller %d err = %v, want nil", i, err)
		}
		if err != results[0] {
			t.Errorf("Shutdown() caller %d err = %v, want the exact same result as caller 0 (%v)", i, err, results[0])
		}
	}

	if calls := proc.SignalCalls(); len(calls) != 2 {
		t.Errorf("Signal called %d times across %d concurrent Shutdown callers, want exactly 2 (the underlying termination must run only once): %v", len(calls), callers, calls)
	}
}

// --- TestShutdownTeardownFailureRetainsAuthority ---

// TestShutdownTeardownFailureRetainsAuthority proves that a teardown
// failure -- here, the escalating kill Signal call itself returning an
// error, even though it still causes the fake process to actually exit --
// is reported to the caller as a *Error wrapping CodeTeardownFailed, while
// the affected entry still reaches a terminal, durably persisted manifest
// state: Shutdown never loses track of a process just because its
// OS-level teardown call itself reported a failure.
func TestShutdownTeardownFailureRetainsAuthority(t *testing.T) {
	t.Parallel()

	manifestRoot := t.TempDir()
	store := NewManifestStore(manifestRoot)
	sup, err := NewSupervisor(shutdownTestConfig(10*time.Millisecond), store, t.TempDir(), nil, nil)
	if err != nil {
		t.Fatalf("NewSupervisor() err = %v, want nil", err)
	}
	owner := testOwner(t)
	origin := testOrigin(t)

	killErr := errors.New("boom: kill signal delivery failed")
	proc := &fakeProcess{
		waitBlock: make(chan struct{}),
		// The process does actually exit once killed (a real OS SIGKILL is
		// not interceptable) even though the signal-delivery call itself
		// reports failure here -- e.g. a runner-level bookkeeping error
		// returned alongside a kill that still took effect.
		waitResult: tool.ProcessResult{Reason: tool.ProcessTerminalKilled},
	}
	proc.signalFunc = func(sig tool.ProcessSignal) error {
		if sig == tool.ProcessSignalKill {
			close(proc.waitBlock)
			return killErr
		}
		return nil
	}
	lease := &fakeLease{}
	handle := startShutdownFake(t, sup, owner, origin, proc, lease)

	shutdownErr := sup.Shutdown(context.Background())
	if shutdownErr == nil {
		t.Fatal("Shutdown() err = nil, want non-nil (teardown failure)")
	}
	var procErr *Error
	if !errors.As(shutdownErr, &procErr) || procErr.Code != CodeTeardownFailed {
		t.Fatalf("Shutdown() err = %v, want *Error{Code: CodeTeardownFailed}", shutdownErr)
	}
	if !errors.Is(shutdownErr, killErr) {
		t.Errorf("Shutdown() err = %v, want it to wrap the underlying kill-signal failure %v", shutdownErr, killErr)
	}

	// Retained authority: despite the teardown failure, the entry still
	// reached a terminal state through its own ordinary run/Wait/terminalize
	// path -- it is not left dangling as "running" forever, and its lease
	// was still released exactly once.
	e := testEntry(t, sup, handle)
	select {
	case <-e.done:
	case <-time.After(2 * time.Second):
		t.Fatal("entry never reached its terminal state despite the teardown failure")
	}

	final, err := store.Load(handle)
	if err != nil {
		t.Fatalf("store.Load() err = %v, want nil", err)
	}
	if !final.State.Terminal() {
		t.Errorf("manifest state after a shutdown teardown failure = %v, want a terminal state", final.State)
	}
	if got := lease.ReleaseCalls(); got != 1 {
		t.Errorf("Lease.Release called %d times, want exactly 1 (terminalize's own release path runs independently of the teardown failure)", got)
	}
}

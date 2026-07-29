package process

import (
	"context"
	"errors"
	"testing"

	"github.com/looprig/harness/pkg/tool"
)

// newTestSupervisor builds a Supervisor rooted at two fresh temporary
// directories (one for manifests, one for the future spool root) governed
// by cfg. lifecycle/notifications are nil: 8A never calls either (see
// supervisor.go's lifecycleSink/completionNotifier doc comments).
func newTestSupervisor(t *testing.T, cfg Config) *Supervisor {
	t.Helper()
	store := NewManifestStore(t.TempDir())
	sup, err := NewSupervisor(cfg, store, t.TempDir(), nil, nil)
	if err != nil {
		t.Fatalf("NewSupervisor() err = %v, want nil", err)
	}
	return sup
}

// --- TestSupervisorReservesQuotaBeforeStart ---

// TestSupervisorReservesQuotaBeforeStart proves reserveQuota's counters are
// already updated by the time Start calls PreparedProcess.Start -- not
// after -- by observing them from inside a fakePreparedProcess.startFunc
// callback.
func TestSupervisorReservesQuotaBeforeStart(t *testing.T) {
	t.Parallel()

	cfg := Config{
		MaxRunningProcessesPerLoop:    4,
		MaxRunningProcessesPerSession: 4,
		MaxProcessInMemoryBytes:       100,
		MaxAggregateInMemoryBytes:     1000,
		MaxProcessSpoolBytes:          200,
		MaxAggregateSpoolBytes:        2000,
	}
	sup := newTestSupervisor(t, cfg)
	owner := testOwner(t)
	origin := testOrigin(t)

	var (
		observedLoop, observedSession int
		observedMemory, observedSpool int64
	)
	prepared := &fakePreparedProcess{
		startFunc: func(ctx context.Context) (tool.Process, error) {
			sup.mu.Lock()
			observedLoop = sup.runningByLoop[owner.LoopID]
			observedSession = sup.runningBySession[owner.SessionID]
			observedMemory = sup.reservedMemoryBytes
			observedSpool = sup.reservedSpoolBytes
			sup.mu.Unlock()
			return &fakeProcess{}, nil
		},
	}
	lease := &fakeLease{}

	handle, err := sup.Start(context.Background(), owner, origin, prepared, lease, nil, nil, StorageCeiling{}, YieldSettings{})
	if err != nil {
		t.Fatalf("Start() err = %v, want nil", err)
	}
	if !handle.Valid() {
		t.Fatalf("Start() returned invalid handle %q", handle)
	}

	if observedLoop != 1 {
		t.Errorf("loop reservation observed inside PreparedProcess.Start = %d, want 1 (reserved before Start)", observedLoop)
	}
	if observedSession != 1 {
		t.Errorf("session reservation observed inside PreparedProcess.Start = %d, want 1", observedSession)
	}
	if observedMemory != cfg.MaxProcessInMemoryBytes {
		t.Errorf("memory reservation observed inside PreparedProcess.Start = %d, want %d", observedMemory, cfg.MaxProcessInMemoryBytes)
	}
	if observedSpool != cfg.MaxProcessSpoolBytes {
		t.Errorf("spool reservation observed inside PreparedProcess.Start = %d, want %d", observedSpool, cfg.MaxProcessSpoolBytes)
	}
	if got := prepared.StartCalls(); got != 1 {
		t.Errorf("PreparedProcess.Start called %d times, want 1", got)
	}
	if got := lease.ReleaseCalls(); got != 0 {
		t.Errorf("Lease.Release called %d times on success, want 0", got)
	}
	if got := prepared.CloseCalls(); got != 0 {
		t.Errorf("PreparedProcess.Close called %d times on success, want 0", got)
	}
}

// --- TestSupervisorStartFailureReleasesQuota ---

// TestSupervisorStartFailureReleasesQuota proves that when
// PreparedProcess.Start fails, Start releases every quota reservation it
// made, releases the caller-supplied lease, and closes the preparation.
func TestSupervisorStartFailureReleasesQuota(t *testing.T) {
	t.Parallel()

	cfg := Config{
		MaxRunningProcessesPerLoop:    4,
		MaxRunningProcessesPerSession: 4,
		MaxProcessInMemoryBytes:       100,
		MaxAggregateInMemoryBytes:     1000,
		MaxProcessSpoolBytes:          200,
		MaxAggregateSpoolBytes:        2000,
	}
	sup := newTestSupervisor(t, cfg)
	owner := testOwner(t)
	origin := testOrigin(t)

	startErr := errors.New("boom: spawn failed")
	prepared := &fakePreparedProcess{startErr: startErr}
	lease := &fakeLease{}

	_, err := sup.Start(context.Background(), owner, origin, prepared, lease, nil, nil, StorageCeiling{}, YieldSettings{})
	if err == nil {
		t.Fatal("Start() err = nil, want non-nil")
	}
	if !errors.Is(err, New(CodeSpawnFailed)) {
		t.Errorf("Start() err = %v, want CodeSpawnFailed", err)
	}
	if !errors.Is(err, startErr) {
		t.Errorf("Start() err = %v, want it to wrap the underlying PreparedProcess.Start error", err)
	}

	sup.mu.Lock()
	loop := sup.runningByLoop[owner.LoopID]
	session := sup.runningBySession[owner.SessionID]
	memory := sup.reservedMemoryBytes
	spool := sup.reservedSpoolBytes
	sup.mu.Unlock()

	if loop != 0 {
		t.Errorf("loop reservation after failed start = %d, want 0 (rolled back)", loop)
	}
	if session != 0 {
		t.Errorf("session reservation after failed start = %d, want 0 (rolled back)", session)
	}
	if memory != 0 {
		t.Errorf("memory reservation after failed start = %d, want 0 (rolled back)", memory)
	}
	if spool != 0 {
		t.Errorf("spool reservation after failed start = %d, want 0 (rolled back)", spool)
	}
	if got := lease.ReleaseCalls(); got != 1 {
		t.Errorf("Lease.Release called %d times, want exactly 1", got)
	}
	if got := prepared.CloseCalls(); got != 1 {
		t.Errorf("PreparedProcess.Close called %d times, want exactly 1", got)
	}
	if got := prepared.StartCalls(); got != 1 {
		t.Errorf("PreparedProcess.Start called %d times, want exactly 1", got)
	}
}

// --- TestSupervisorRejectsSessionAndLoopQuota ---

// TestSupervisorRejectsSessionAndLoopQuota proves that admission is
// rejected with CodeProcessQuotaExceeded, without ever calling
// PreparedProcess.Start or PreparedProcess.Close, once either the per-loop
// or per-session running-process quota is already exhausted.
func TestSupervisorRejectsSessionAndLoopQuota(t *testing.T) {
	t.Parallel()

	baseCfg := Config{
		MaxProcessInMemoryBytes:   100,
		MaxAggregateInMemoryBytes: 1000,
		MaxProcessSpoolBytes:      200,
		MaxAggregateSpoolBytes:    2000,
	}

	tests := []struct {
		name string
		// perLoop and perSession are the quota caps under test; Config
		// requires perLoop <= perSession, so each case sets exactly one
		// of them to a binding value of 1 and leaves the other generous.
		perLoop, perSession int
		// sameLoop/sameSession decide whether the second admission shares
		// the first admission's LoopID/SessionID: exactly one must match
		// so the binding cap (and only that cap) is exercised.
		sameLoop, sameSession bool
	}{
		{name: "loop quota", perLoop: 1, perSession: 4, sameLoop: true, sameSession: false},
		{name: "session quota", perLoop: 1, perSession: 1, sameLoop: false, sameSession: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := baseCfg
			cfg.MaxRunningProcessesPerLoop = tt.perLoop
			cfg.MaxRunningProcessesPerSession = tt.perSession
			sup := newTestSupervisor(t, cfg)

			firstOwner := testOwner(t)
			secondOwner := firstOwner
			if !tt.sameLoop {
				secondOwner.LoopID = mustUUID(t)
			}
			if !tt.sameSession {
				secondOwner.SessionID = mustUUID(t)
			}

			first := &fakePreparedProcess{process: &fakeProcess{}}
			if _, err := sup.Start(context.Background(), firstOwner, testOrigin(t), first, &fakeLease{}, nil, nil, StorageCeiling{}, YieldSettings{}); err != nil {
				t.Fatalf("first Start() err = %v, want nil", err)
			}

			second := &fakePreparedProcess{process: &fakeProcess{}}
			_, err := sup.Start(context.Background(), secondOwner, testOrigin(t), second, &fakeLease{}, nil, nil, StorageCeiling{}, YieldSettings{})
			if !errors.Is(err, New(CodeProcessQuotaExceeded)) {
				t.Fatalf("second Start() err = %v, want CodeProcessQuotaExceeded", err)
			}
			if got := second.StartCalls(); got != 0 {
				t.Errorf("PreparedProcess.Start called %d times for rejected admission, want 0", got)
			}
			if got := second.CloseCalls(); got != 0 {
				t.Errorf("PreparedProcess.Close called %d times for rejected admission, want 0", got)
			}
		})
	}
}

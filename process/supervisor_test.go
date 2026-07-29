package process

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

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

// testEntry looks up the *entry Start registered for h, failing the test if
// none exists. It is a white-box accessor (same package) used by 8B's
// stream-drain tests, which need to reach the entry's Buffer/Spool
// directly; Supervisor itself exposes no such accessor publicly.
func testEntry(t *testing.T, sup *Supervisor, h Handle) *entry {
	t.Helper()
	sup.mu.Lock()
	defer sup.mu.Unlock()
	e, ok := sup.entries[h]
	if !ok {
		t.Fatalf("no entry registered for handle %q", h)
	}
	return e
}

// loadOnlyManifest scans dir -- a ManifestStore's root -- for exactly one
// manifest file and loads it through store. It exists so a test can
// observe a manifest durably written by a Start call it has not seen
// return yet, and therefore does not know the Handle for (see
// TestSupervisorPersistsBeforeReturningHandle). ManifestStore has no
// listing method of its own (manifest.go); this is a small, test-only scan
// built directly on the already-in-package manifestSuffix constant, not a
// new public API surface.
func loadOnlyManifest(t *testing.T, store *ManifestStore, dir string) Manifest {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q) err = %v, want nil", dir, err)
	}
	var handle Handle
	found := 0
	for _, de := range entries {
		if name := de.Name(); strings.HasSuffix(name, manifestSuffix) {
			handle = Handle(strings.TrimSuffix(name, manifestSuffix))
			found++
		}
	}
	if found != 1 {
		t.Fatalf("found %d manifest files in %q, want exactly 1", found, dir)
	}
	m, err := store.Load(handle)
	if err != nil {
		t.Fatalf("store.Load(%q) err = %v, want nil", handle, err)
	}
	return m
}

// --- TestSupervisorPersistsBeforeReturningHandle ---

// TestSupervisorPersistsBeforeReturningHandle proves that Start durably
// persists this process's manifest in state StateStarting -- observable on
// disk through a real ManifestStore, not a fake -- strictly before it ever
// calls PreparedProcess.Start, and that the manifest advances to
// StateRunning once the spawn succeeds, all before Start returns a Handle
// to its caller.
//
// The fake PreparedProcess.Start blocks until the test releases it, so the
// manifest observation made from inside that call is made while Start is
// still blocked in the very call the ordering claim is about -- proving
// manifest-before-spawn-completion ordering without any race or polling.
func TestSupervisorPersistsBeforeReturningHandle(t *testing.T) {
	t.Parallel()

	cfg := Config{
		MaxRunningProcessesPerLoop:    4,
		MaxRunningProcessesPerSession: 4,
		MaxProcessInMemoryBytes:       100,
		MaxAggregateInMemoryBytes:     1000,
		MaxProcessSpoolBytes:          200,
		MaxAggregateSpoolBytes:        2000,
	}
	manifestRoot := t.TempDir()
	store := NewManifestStore(manifestRoot)
	sup, err := NewSupervisor(cfg, store, t.TempDir(), nil, nil)
	if err != nil {
		t.Fatalf("NewSupervisor() err = %v, want nil", err)
	}
	owner := testOwner(t)
	origin := testOrigin(t)

	release := make(chan struct{})
	observed := make(chan Manifest, 1)
	observeErr := make(chan error, 1)

	prepared := &fakePreparedProcess{
		startFunc: func(ctx context.Context) (tool.Process, error) {
			m := loadOnlyManifest(t, store, manifestRoot)
			observed <- m
			observeErr <- nil
			<-release
			return &fakeProcess{}, nil
		},
	}
	lease := &fakeLease{}

	var handle Handle
	var startErr error
	done := make(chan struct{})
	go func() {
		handle, startErr = sup.Start(context.Background(), owner, origin, prepared, lease, nil, nil, StorageCeiling{}, YieldSettings{})
		close(done)
	}()

	var observedManifest Manifest
	select {
	case observedManifest = <-observed:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting to observe the manifest from inside PreparedProcess.Start")
	}
	if err := <-observeErr; err != nil {
		t.Fatalf("load manifest while Start was blocked: %v", err)
	}
	if observedManifest.State != StateStarting {
		t.Errorf("manifest state observed while blocked in PreparedProcess.Start = %v, want %v", observedManifest.State, StateStarting)
	}
	if !observedManifest.Handle.Valid() {
		t.Errorf("observed manifest handle %q is not Valid()", observedManifest.Handle)
	}
	if !observedManifest.Owner.Equal(owner) {
		t.Errorf("observed manifest owner = %+v, want %+v", observedManifest.Owner, owner)
	}
	if observedManifest.Origin != origin {
		t.Errorf("observed manifest origin = %+v, want %+v", observedManifest.Origin, origin)
	}
	if observedManifest.StartedAt != nil {
		t.Errorf("observed manifest StartedAt = %v, want nil while still starting", observedManifest.StartedAt)
	}

	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Start to return")
	}
	if startErr != nil {
		t.Fatalf("Start() err = %v, want nil", startErr)
	}
	if handle != observedManifest.Handle {
		t.Errorf("Start() returned handle %q, want the observed manifest's handle %q", handle, observedManifest.Handle)
	}

	final, err := store.Load(handle)
	if err != nil {
		t.Fatalf("store.Load(%q) err = %v, want nil", handle, err)
	}
	if final.State != StateRunning {
		t.Errorf("manifest state after Start returns = %v, want %v", final.State, StateRunning)
	}
	if final.StartedAt == nil {
		t.Error("manifest StartedAt is nil after Start returns, want a timestamp")
	}
}

// --- TestSupervisorDrainsOrderedStreams ---

// TestSupervisorDrainsOrderedStreams proves that the entry goroutine
// Start spawns after a successful spawn drains Stdout and Stderr
// concurrently into one deterministic, append-ordered sequence covering
// both the in-memory Buffer and the durable Spool (spec "Output capture
// and storage": "Writes use a single per-process append sequence so
// cursor order is deterministic even when stdout and stderr are read
// concurrently").
//
// Each write below is followed by a wait for the drain goroutines to have
// fully appended it before the next write starts, so at most one stream
// ever has pending unconsumed data at a time. That is what makes the
// resulting interleaving order deterministic and precisely assertable,
// even though stdout and stderr are drained by two concurrent goroutines
// funnelled through entry.go's single appendMu-guarded append sequence.
func TestSupervisorDrainsOrderedStreams(t *testing.T) {
	t.Parallel()

	cfg := Config{
		MaxRunningProcessesPerLoop:    4,
		MaxRunningProcessesPerSession: 4,
		MaxProcessInMemoryBytes:       1 << 20,
		MaxAggregateInMemoryBytes:     10 << 20,
		MaxProcessSpoolBytes:          1 << 20,
		MaxAggregateSpoolBytes:        10 << 20,
	}
	sup := newTestSupervisor(t, cfg)
	owner := testOwner(t)
	origin := testOrigin(t)

	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()

	proc := &fakeProcess{stdout: stdoutR, stderr: stderrR}
	prepared := &fakePreparedProcess{process: proc}
	lease := &fakeLease{}

	handle, err := sup.Start(context.Background(), owner, origin, prepared, lease, nil, nil, StorageCeiling{}, YieldSettings{})
	if err != nil {
		t.Fatalf("Start() err = %v, want nil", err)
	}
	e := testEntry(t, sup, handle)

	waitForTotal := func(want int64) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if e.buffer.TotalBytes() == want {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("timed out waiting for buffer total bytes = %d, got %d", want, e.buffer.TotalBytes())
	}

	writeAndWait := func(w *io.PipeWriter, data string, wantTotal int64) {
		t.Helper()
		if _, err := w.Write([]byte(data)); err != nil {
			t.Fatalf("write(%q) err = %v, want nil", data, err)
		}
		waitForTotal(wantTotal)
	}

	writeAndWait(stdoutW, "AAAA", 4)
	writeAndWait(stderrW, "BBBB", 8)
	writeAndWait(stdoutW, "CCCC", 12)

	if err := stdoutW.Close(); err != nil {
		t.Fatalf("stdoutW.Close() err = %v, want nil", err)
	}
	if err := stderrW.Close(); err != nil {
		t.Fatalf("stderrW.Close() err = %v, want nil", err)
	}

	const want = "AAAABBBBCCCC"

	data, next, gap, err := e.buffer.Read(0, 0)
	if err != nil {
		t.Fatalf("buffer.Read() err = %v, want nil", err)
	}
	if gap {
		t.Error("buffer.Read(0, ...) gap = true, want false")
	}
	if string(data) != want {
		t.Errorf("buffer content = %q, want %q", data, want)
	}
	if next != int64(len(want)) {
		t.Errorf("buffer next cursor = %d, want %d", next, len(want))
	}

	spoolData, spoolNext, spoolGap, err := e.spool.Read(0, 0)
	if err != nil {
		t.Fatalf("spool.Read() err = %v, want nil", err)
	}
	if spoolGap {
		t.Error("spool.Read(0, ...) gap = true, want false")
	}
	if string(spoolData) != want {
		t.Errorf("spool content = %q, want %q", spoolData, want)
	}
	if spoolNext != int64(len(want)) {
		t.Errorf("spool next cursor = %d, want %d", spoolNext, len(want))
	}
}

// --- TestSupervisorSpoolCeilingDropsOldest ---

// TestSupervisorSpoolCeilingDropsOldest proves that the supervisor's drain
// path wires a Start call's StorageCeiling.SpoolBytes into the entry's
// Spool correctly: when drained output exceeds the configured ceiling, the
// Spool (already implemented and tested independently in Task 6) drops the
// oldest retained bytes while TotalBytes keeps counting the complete
// stream, and a read from cursor 0 reports gap: true.
func TestSupervisorSpoolCeilingDropsOldest(t *testing.T) {
	t.Parallel()

	cfg := Config{
		MaxRunningProcessesPerLoop:    4,
		MaxRunningProcessesPerSession: 4,
		MaxProcessInMemoryBytes:       1 << 20,
		MaxAggregateInMemoryBytes:     10 << 20,
		MaxProcessSpoolBytes:          1 << 20,
		MaxAggregateSpoolBytes:        10 << 20,
	}
	sup := newTestSupervisor(t, cfg)
	owner := testOwner(t)
	origin := testOrigin(t)

	const ceiling = 8
	const total = 32

	payload := bytes.Repeat([]byte("x"), total)
	proc := &fakeProcess{stdout: io.NopCloser(bytes.NewReader(payload))}
	prepared := &fakePreparedProcess{process: proc}
	lease := &fakeLease{}

	handle, err := sup.Start(context.Background(), owner, origin, prepared, lease, nil, nil, StorageCeiling{SpoolBytes: ceiling}, YieldSettings{})
	if err != nil {
		t.Fatalf("Start() err = %v, want nil", err)
	}
	e := testEntry(t, sup, handle)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && e.spool.TotalBytes() < total {
		time.Sleep(time.Millisecond)
	}
	if got := e.spool.TotalBytes(); got != total {
		t.Fatalf("spool.TotalBytes() = %d, want %d (drain must keep counting every appended byte even after drops)", got, total)
	}
	if got := e.spool.RetainedFrom(); got != total-ceiling {
		t.Errorf("spool.RetainedFrom() = %d, want %d", got, total-ceiling)
	}

	data, next, gap, err := e.spool.Read(0, 0)
	if err != nil {
		t.Fatalf("spool.Read(0, ...) err = %v, want nil", err)
	}
	if !gap {
		t.Error("spool.Read(0, ...) gap = false, want true: oldest bytes were dropped by the ceiling")
	}
	if int64(len(data)) != ceiling {
		t.Errorf("spool.Read(0, ...) returned %d bytes, want %d retained bytes", len(data), ceiling)
	}
	if next != total {
		t.Errorf("spool.Read(0, ...) next cursor = %d, want %d", next, total)
	}
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

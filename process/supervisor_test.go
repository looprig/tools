package process

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
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

// waitEntryDone blocks until e's done channel closes, or fails the test if
// that does not happen within timeout. done only closes once run's Wait call
// has returned, both drain goroutines have finished, and terminalize --
// including its manifest Save and (Task 8D) its terminal-LRU retention hook,
// which can itself delete a durable manifest/spool file -- has fully
// returned (entry.go's done field doc comment).
//
// Every test that starts a live entry (directly, or via Supervisor.Start)
// whose eventual termination writes into a t.TempDir()-rooted ManifestStore/
// Spool root must synchronize on this exact signal before the test function
// returns: Go's t.TempDir() cleanup (registered by the t.TempDir() call
// itself, in call order, and therefore run LAST relative to any cleanup a
// test registers afterward -- see testing.T.Cleanup's LIFO ordering) does
// not know to wait for an entry's own background termination goroutine, and
// races it otherwise -- "directory not empty" ENOTEMPTY flakes under -race
// are the observable symptom. A test that keeps a fake process artificially
// "running" via an unclosed io.Pipe (e.g. TestSupervisorNeverEvictsRunning's
// pattern) must call this from the SAME t.Cleanup that closes the pipe,
// after closing it, and that cleanup must itself be registered after the
// Supervisor's own TempDir-backed store/spoolRoot are established, so LIFO
// ordering runs it before those TempDirs are ever removed.
func waitEntryDone(t *testing.T, e *entry, timeout time.Duration) {
	t.Helper()
	select {
	case <-e.done:
	case <-time.After(timeout):
		t.Fatalf("timed out after %s waiting for entry %q to terminalize", timeout, e.identity.Handle)
	}
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

// spoolFileExists reports whether a Handle's spool file exists on disk
// beneath spoolRoot, using the package's own resourcePath/spoolSuffix
// (manifest.go/spool.go) rather than hand-rolling the path -- so a
// retention-eviction test (TestSupervisorEvictsCompletedLRU) can prove
// Supervisor.evictResources' spool.Remove call actually deleted the file.
func spoolFileExists(t *testing.T, spoolRoot string, h Handle) bool {
	t.Helper()
	path, _, err := resourcePath(spoolRoot, h, spoolSuffix)
	if err != nil {
		t.Fatalf("resourcePath() err = %v, want nil", err)
	}
	_, err = os.Stat(path)
	switch {
	case err == nil:
		return true
	case os.IsNotExist(err):
		return false
	default:
		t.Fatalf("os.Stat(%q) err = %v", path, err)
		return false
	}
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

	// stdoutR/stderrR are deliberately never closed until this test's own
	// post-Start manifest assertions are done: an entry whose process looks
	// already-exited (fakeProcess{}'s zero-value Wait) now drives real
	// terminal arbitration (Task 8C), including an asynchronous manifest
	// Save that would otherwise race this test's own synchronous
	// store.Load(handle) check of the StateRunning manifest immediately
	// below.
	//
	// handle is declared here, before the pipes/cleanup below, so the
	// cleanup closure can see its final value (Go's declare-before-use
	// scoping): it is assigned below, inside the goroutine that calls
	// sup.Start, well before this cleanup ever runs.
	var handle Handle
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	t.Cleanup(func() {
		_ = stdoutW.Close()
		_ = stderrW.Close()
		// Closing the pipes above only unblocks the entry's drain
		// goroutines asynchronously; it does not wait for the resulting
		// terminalize (manifest Save into manifestRoot) to actually finish.
		// This wait must happen here, in the same cleanup, registered after
		// manifestRoot's own t.TempDir() call above so LIFO ordering runs it
		// strictly before that TempDir is removed (waitEntryDone's doc
		// comment).
		waitEntryDone(t, testEntry(t, sup, handle), 5*time.Second)
	})

	prepared := &fakePreparedProcess{
		startFunc: func(ctx context.Context) (tool.Process, error) {
			m := loadOnlyManifest(t, store, manifestRoot)
			observed <- m
			observeErr <- nil
			<-release
			return &fakeProcess{stdout: stdoutR, stderr: stderrR}, nil
		},
	}
	lease := &fakeLease{}

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

	// Closing the pipes above only unblocks the entry's drain goroutines
	// asynchronously; the resulting terminalize (manifest Save into
	// newTestSupervisor's t.TempDir()-rooted manifest store) is not
	// guaranteed to have finished yet. Wait for it here, before this test
	// function returns and that TempDir's cleanup runs (waitEntryDone's doc
	// comment).
	waitEntryDone(t, e, 5*time.Second)

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

	// proc.stdout is a finite bytes.Reader, so drain reaches EOF and the
	// entry terminalizes on its own, with no test action to unblock it.
	// e.spool.TotalBytes() reaching total above only proves the drain
	// finished appending; it says nothing about whether the resulting
	// terminalize (manifest Save into newTestSupervisor's t.TempDir()-rooted
	// manifest store) has completed yet. Wait for it here, before this test
	// function returns and that TempDir's cleanup runs (waitEntryDone's doc
	// comment).
	waitEntryDone(t, e, 5*time.Second)
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

	// See TestSupervisorPersistsBeforeReturningHandle's stdoutR/stderrR
	// comment: keep the admitted entry's process looking "still running"
	// (drain blocked on unclosed pipes) until this test's own post-Start
	// assertions -- including lease.ReleaseCalls() == 0 -- are done, so
	// Task 8C's real terminal arbitration cannot race them.
	//
	// handle is declared here, before the pipes/cleanup below, so the
	// cleanup closure can see its final value once sup.Start assigns it
	// further down (Go's declare-before-use scoping).
	var handle Handle
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	t.Cleanup(func() {
		_ = stdoutW.Close()
		_ = stderrW.Close()
		// See waitEntryDone's doc comment: this must run, in this same
		// cleanup, after closing the pipes above and before
		// newTestSupervisor's TempDir-rooted store/spoolRoot are removed.
		waitEntryDone(t, testEntry(t, sup, handle), 5*time.Second)
	})

	prepared := &fakePreparedProcess{
		startFunc: func(ctx context.Context) (tool.Process, error) {
			sup.mu.Lock()
			observedLoop = sup.runningByLoop[owner.LoopID]
			observedSession = sup.runningBySession[owner.SessionID]
			observedMemory = sup.reservedMemoryBytes
			observedSpool = sup.reservedSpoolBytes
			sup.mu.Unlock()
			return &fakeProcess{stdout: stdoutR, stderr: stderrR}, nil
		},
	}
	lease := &fakeLease{}

	var err error
	handle, err = sup.Start(context.Background(), owner, origin, prepared, lease, nil, nil, StorageCeiling{}, YieldSettings{})
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

			// Keep first's process looking "still running" (drain blocked on
			// unclosed pipes) for the rest of this subtest: otherwise Task
			// 8C's real terminal arbitration could release first's quota
			// reservation before second's Start call below, which would
			// wrongly let second be admitted instead of rejected.
			//
			// firstHandle is declared here, before the pipes/cleanup below,
			// so the cleanup closure can see its final value once sup.Start
			// assigns it further down (Go's declare-before-use scoping).
			var firstHandle Handle
			firstStdoutR, firstStdoutW := io.Pipe()
			firstStderrR, firstStderrW := io.Pipe()
			t.Cleanup(func() {
				_ = firstStdoutW.Close()
				_ = firstStderrW.Close()
				// See waitEntryDone's doc comment: this must run, in this
				// same cleanup, after closing the pipes above and before
				// newTestSupervisor's TempDir-rooted store/spoolRoot are
				// removed.
				waitEntryDone(t, testEntry(t, sup, firstHandle), 5*time.Second)
			})

			first := &fakePreparedProcess{process: &fakeProcess{stdout: firstStdoutR, stderr: firstStderrR}}
			var err error
			firstHandle, err = sup.Start(context.Background(), firstOwner, testOrigin(t), first, &fakeLease{}, nil, nil, StorageCeiling{}, YieldSettings{})
			if err != nil {
				t.Fatalf("first Start() err = %v, want nil", err)
			}

			second := &fakePreparedProcess{process: &fakeProcess{}}
			_, err = sup.Start(context.Background(), secondOwner, testOrigin(t), second, &fakeLease{}, nil, nil, StorageCeiling{}, YieldSettings{})
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

// --- TestSupervisorTerminalRaceChoosesOnce ---

// TestSupervisorTerminalRaceChoosesOnce proves entry.terminalize's one-shot
// compare-and-set: when several goroutines race to terminalize the same
// entry concurrently -- standing in for the natural Wait() return racing an
// explicit stop request, a timeout, and supervisor shutdown, none of which
// have a caller yet (Task 9C) -- exactly one of them wins. Its dominant
// assertion is that the manifest was written exactly once (SaveCalls()==1)
// and, consequently, that the final persisted manifest carries exactly one
// racer's ExitCode rather than some interleaving/corruption of several.
// TestSupervisorReleasesLeaseOnce exercises the same race, but leads with
// the lease-release assertion instead.
func TestSupervisorTerminalRaceChoosesOnce(t *testing.T) {
	t.Parallel()

	e, fakes := newRaceEntry(t)

	const racers = 8
	var wg sync.WaitGroup
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		code := i
		go func() {
			defer wg.Done()
			e.terminalize(context.Background(), StateExited, Result{ExitCode: &code, Reason: "exited"}, time.Now().UTC())
		}()
	}
	wg.Wait()

	if got := fakes.manifests.SaveCalls(); got != 1 {
		t.Errorf("manifest Save called %d times across %d concurrent terminalize calls, want exactly 1", got, racers)
	}
	if got := fakes.lifecycle.PublishCalls(); got != 1 {
		t.Errorf("lifecycle publish called %d times, want exactly 1", got)
	}
	if got := fakes.notifications.NotifyCalls(); got != 1 {
		t.Errorf("completion notify called %d times, want exactly 1", got)
	}
	if got := fakes.quotaReleases.Count(); got != 1 {
		t.Errorf("quota released %d times, want exactly 1", got)
	}

	final, err := fakes.manifests.Load(e.identity.Handle)
	if err != nil {
		t.Fatalf("Load() err = %v, want nil", err)
	}
	if !final.State.Terminal() {
		t.Errorf("final manifest state = %v, want a terminal state", final.State)
	}
	if final.Result.ExitCode == nil {
		t.Fatal("final manifest Result.ExitCode is nil, want the winning racer's exit code")
	}
	if got := *final.Result.ExitCode; got < 0 || got >= racers {
		t.Errorf("final manifest Result.ExitCode = %d, want one of the %d racers' codes [0, %d)", got, racers, racers)
	}

	// A second, later call must remain a no-op: terminalOnce has already
	// fired, so this must not attempt (and therefore must not fail
	// against) another terminal Save.
	e.terminalize(context.Background(), StateKilled, Result{Reason: "killed"}, time.Now().UTC())
	if got := fakes.manifests.SaveCalls(); got != 1 {
		t.Errorf("manifest Save called %d times after a later terminalize call, want still exactly 1", got)
	}
}

// --- TestSupervisorReleasesLeaseOnce ---

// TestSupervisorReleasesLeaseOnce proves the same one-shot race as
// TestSupervisorTerminalRaceChoosesOnce, but leads with the
// combined-acceptance text's "one lease release" property: fakeLease's
// call counter (reused from 8A's fake_process_test.go) reports exactly one
// Release call no matter how many goroutines race to terminalize the same
// entry concurrently.
func TestSupervisorReleasesLeaseOnce(t *testing.T) {
	t.Parallel()

	e, fakes := newRaceEntry(t)

	const racers = 8
	var wg sync.WaitGroup
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func() {
			defer wg.Done()
			e.terminalize(context.Background(), StateFailed, Result{Reason: "failed"}, time.Now().UTC())
		}()
	}
	wg.Wait()

	if got := fakes.lease.ReleaseCalls(); got != 1 {
		t.Errorf("Lease.Release called %d times across %d concurrent terminalize calls, want exactly 1", got, racers)
	}
}

// --- TestSupervisorPublishesStableLifecycleIDs ---

// TestSupervisorPublishesStableLifecycleIDs proves that Start allocates and
// durably persists this process's stable LifecycleEventIDs (manifest.go)
// before the process can ever reach a terminal state, and that the actual
// terminal-path publication reuses those exact pre-persisted IDs rather
// than minting new ones: (a) every Events field is already non-zero in the
// manifest immediately after Start returns, before any terminal event, and
// (b) reloading the manifest from disk -- once right after the entry
// terminalizes, and again a second time simulating a crash-recovery reload
// / retry of the terminal-publication step -- reports the exact same
// values both times, not freshly minted ones.
func TestSupervisorPublishesStableLifecycleIDs(t *testing.T) {
	t.Parallel()

	cfg := Config{
		MaxRunningProcessesPerLoop:    4,
		MaxRunningProcessesPerSession: 4,
		MaxProcessInMemoryBytes:       1 << 20,
		MaxAggregateInMemoryBytes:     10 << 20,
		MaxProcessSpoolBytes:          1 << 20,
		MaxAggregateSpoolBytes:        10 << 20,
	}
	manifestRoot := t.TempDir()
	store := NewManifestStore(manifestRoot)
	sup, err := NewSupervisor(cfg, store, t.TempDir(), nil, nil)
	if err != nil {
		t.Fatalf("NewSupervisor() err = %v, want nil", err)
	}
	owner := testOwner(t)
	origin := testOrigin(t)

	proc := &fakeProcess{
		waitResult: tool.ProcessResult{ExitCode: 0, Reason: tool.ProcessTerminalExited, FinishedAt: time.Now()},
	}
	prepared := &fakePreparedProcess{process: proc}
	lease := &fakeLease{}

	handle, err := sup.Start(context.Background(), owner, origin, prepared, lease, nil, nil, StorageCeiling{}, YieldSettings{})
	if err != nil {
		t.Fatalf("Start() err = %v, want nil", err)
	}

	afterStart, err := store.Load(handle)
	if err != nil {
		t.Fatalf("store.Load() err = %v, want nil", err)
	}
	if afterStart.Events.Started.IsZero() ||
		afterStart.Events.Backgrounded.IsZero() ||
		afterStart.Events.Completed.IsZero() ||
		afterStart.Events.Lost.IsZero() ||
		afterStart.Events.CommandID.IsZero() {
		t.Fatalf("manifest Events immediately after Start = %+v, want every field non-zero", afterStart.Events)
	}

	e := testEntry(t, sup, handle)
	select {
	case <-e.done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the entry to terminalize")
	}

	afterTerminal, err := store.Load(handle)
	if err != nil {
		t.Fatalf("store.Load() err = %v, want nil", err)
	}
	if !afterTerminal.State.Terminal() {
		t.Errorf("manifest state after terminalize = %v, want terminal", afterTerminal.State)
	}
	if afterTerminal.Events != afterStart.Events {
		t.Errorf("manifest Events after terminalize = %+v, want unchanged from %+v", afterTerminal.Events, afterStart.Events)
	}

	// Simulate a crash-recovery reload / retry of the terminal-publication
	// step: reload the manifest from disk a second time and confirm it
	// still reports the exact same pre-persisted IDs, not freshly minted
	// ones.
	reloaded, err := store.Load(handle)
	if err != nil {
		t.Fatalf("store.Load() (second reload) err = %v, want nil", err)
	}
	if reloaded.Events != afterStart.Events {
		t.Errorf("manifest Events on reload = %+v, want unchanged from %+v", reloaded.Events, afterStart.Events)
	}
}

// --- TestSupervisorStartPublishesStartedOrBackgrounded ---

// TestSupervisorStartPublishesStartedOrBackgrounded proves the Phase Gate 2
// fix to Task 8's combined-acceptance text: "lifecycle sink receives the
// pre-persisted started EventID exactly once" and "explicit/yield handoff
// emits backgrounded with its stable EventID". With YieldSettings{Yield:
// false}, Start must cause the sink to receive exactly one Started publish
// carrying the manifest's own Events.Started ID; with YieldSettings{Yield:
// true}, exactly one Backgrounded publish carrying Events.Backgrounded's ID
// instead, and never a Started publish either way -- exactly one of the two
// kinds is ever emitted per Start call, never both.
func TestSupervisorStartPublishesStartedOrBackgrounded(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		yield    bool
		wantKind tool.ProcessLifecycleKind
	}{
		{name: "no yield emits started", yield: false, wantKind: tool.ProcessLifecycleStarted},
		{name: "yield emits backgrounded", yield: true, wantKind: tool.ProcessLifecycleBackgrounded},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := Config{
				MaxRunningProcessesPerLoop:    4,
				MaxRunningProcessesPerSession: 4,
				MaxProcessInMemoryBytes:       1 << 20,
				MaxAggregateInMemoryBytes:     10 << 20,
				MaxProcessSpoolBytes:          1 << 20,
				MaxAggregateSpoolBytes:        10 << 20,
			}
			manifestRoot := t.TempDir()
			store := NewManifestStore(manifestRoot)
			sup, err := NewSupervisor(cfg, store, t.TempDir(), nil, nil)
			if err != nil {
				t.Fatalf("NewSupervisor() err = %v, want nil", err)
			}
			owner := testOwner(t)
			origin := testOrigin(t)

			proc := &fakeProcess{
				waitResult: tool.ProcessResult{ExitCode: 0, Reason: tool.ProcessTerminalExited, FinishedAt: time.Now()},
			}
			prepared := &fakePreparedProcess{process: proc}
			lease := &fakeLease{}
			sink := &fakeLifecycleSink{}

			handle, err := sup.Start(context.Background(), owner, origin, prepared, lease, sink, nil, StorageCeiling{}, YieldSettings{Yield: tt.yield})
			if err != nil {
				t.Fatalf("Start() err = %v, want nil", err)
			}

			t.Cleanup(func() {
				waitEntryDone(t, testEntry(t, sup, handle), 5*time.Second)
			})

			afterStart, err := store.Load(handle)
			if err != nil {
				t.Fatalf("store.Load() err = %v, want nil", err)
			}

			if got := sink.PublishStartCalls(); got != 1 {
				t.Fatalf("publishStart called %d times, want exactly 1", got)
			}
			gotEvent := sink.LastStartEvent()
			if gotEvent.Kind != tt.wantKind {
				t.Errorf("publishStart Kind = %v, want %v", gotEvent.Kind, tt.wantKind)
			}

			wantEventID := afterStart.Events.Started
			if tt.yield {
				wantEventID = afterStart.Events.Backgrounded
			}
			if gotEvent.EventID.IsZero() {
				t.Error("publishStart EventID is zero, want the manifest's pre-persisted stable ID")
			}
			if gotEvent.EventID != wantEventID {
				t.Errorf("publishStart EventID = %s, want %s", gotEvent.EventID, wantEventID)
			}
			if gotEvent.CreatedAt.IsZero() || gotEvent.StartedAt.IsZero() {
				t.Errorf("publishStart CreatedAt/StartedAt = %v/%v, want both non-zero", gotEvent.CreatedAt, gotEvent.StartedAt)
			}
		})
	}
}

// --- TestSupervisorNeverEvictsRunning ---

// TestSupervisorNeverEvictsRunning proves that a running entry (one whose
// process has not yet terminalized) is never evicted by terminal-LRU
// retention, no matter how low Config.MaxRetainedCompletedProcessesPerSession
// is configured. Every admitted process in this test is kept "still
// running" by never closing its stdout/stderr pipes (the same technique
// TestSupervisorRejectsSessionAndLoopQuota and others already use), and the
// retention limit is set to 1 -- lower than the number of concurrently
// running processes -- so an implementation that evicted by raw count
// rather than by State.Terminal() would wrongly evict some of them.
func TestSupervisorNeverEvictsRunning(t *testing.T) {
	t.Parallel()

	cfg := Config{
		MaxRunningProcessesPerLoop:              8,
		MaxRunningProcessesPerSession:           8,
		MaxRetainedCompletedProcessesPerSession: 1,
		MaxProcessInMemoryBytes:                 1 << 20,
		MaxAggregateInMemoryBytes:               10 << 20,
		MaxProcessSpoolBytes:                    1 << 20,
		MaxAggregateSpoolBytes:                  10 << 20,
	}
	sup := newTestSupervisor(t, cfg)
	owner := testOwner(t)
	origin := testOrigin(t)

	const running = 3
	handles := make([]Handle, 0, running)
	for i := 0; i < running; i++ {
		// h is declared here, before the pipes/cleanup below, so the
		// cleanup closure can see its final value once sup.Start assigns it
		// further down (Go's declare-before-use scoping). Each loop
		// iteration gets its own fresh h (var inside the loop body), so
		// every iteration's cleanup closes over its own handle, not a
		// shared one.
		var h Handle
		stdoutR, stdoutW := io.Pipe()
		stderrR, stderrW := io.Pipe()
		t.Cleanup(func() {
			_ = stdoutW.Close()
			_ = stderrW.Close()
			// See waitEntryDone's doc comment: this must run, in this same
			// cleanup, after closing the pipes above and before
			// newTestSupervisor's TempDir-rooted store/spoolRoot are
			// removed.
			waitEntryDone(t, testEntry(t, sup, h), 5*time.Second)
		})

		prepared := &fakePreparedProcess{process: &fakeProcess{stdout: stdoutR, stderr: stderrR}}
		started, err := sup.Start(context.Background(), owner, origin, prepared, &fakeLease{}, nil, nil, StorageCeiling{}, YieldSettings{})
		if err != nil {
			t.Fatalf("Start() [%d] err = %v, want nil", i, err)
		}
		h = started
		handles = append(handles, h)
	}

	for i, h := range handles {
		sup.mu.Lock()
		_, ok := sup.entries[h]
		sup.mu.Unlock()
		if !ok {
			t.Errorf("handle %d (%q) missing from registry while still running, want it retained regardless of the retention limit", i, h)
		}
	}
}

// --- TestSupervisorEvictsCompletedLRU ---

// TestSupervisorEvictsCompletedLRU proves terminal-LRU retention's eviction
// behavior end to end: four processes for the same owner/session are
// started and allowed to terminalize one at a time (each iteration waits on
// its own entry's done channel before starting the next, so completion
// order is exact and deterministic), against a retention limit of 2. The
// two oldest-completed handles must be evicted -- gone from the in-memory
// registry, their manifest no longer loadable, and their spool file removed
// from disk -- while the two most-recently-completed handles remain fully
// intact in all three places.
func TestSupervisorEvictsCompletedLRU(t *testing.T) {
	t.Parallel()

	const retain = 2
	cfg := Config{
		MaxRunningProcessesPerLoop:              8,
		MaxRunningProcessesPerSession:           8,
		MaxRetainedCompletedProcessesPerSession: retain,
		MaxProcessInMemoryBytes:                 1 << 20,
		MaxAggregateInMemoryBytes:               10 << 20,
		MaxProcessSpoolBytes:                    1 << 20,
		MaxAggregateSpoolBytes:                  10 << 20,
	}
	manifestRoot := t.TempDir()
	spoolRoot := t.TempDir()
	store := NewManifestStore(manifestRoot)
	sup, err := NewSupervisor(cfg, store, spoolRoot, nil, nil)
	if err != nil {
		t.Fatalf("NewSupervisor() err = %v, want nil", err)
	}
	owner := testOwner(t)
	origin := testOrigin(t)

	const total = 4
	handles := make([]Handle, 0, total)
	for i := 0; i < total; i++ {
		// A non-empty stdout ensures a spool file actually gets created on
		// disk (Spool.Append -- and therefore its first durable write --
		// only ever happens when at least one byte is appended), so this
		// test can meaningfully assert the spool file's removal below
		// rather than asserting the absence of a file that was never
		// written in the first place.
		prepared := &fakePreparedProcess{process: &fakeProcess{stdout: io.NopCloser(strings.NewReader("done"))}}
		h, err := sup.Start(context.Background(), owner, origin, prepared, &fakeLease{}, nil, nil, StorageCeiling{}, YieldSettings{})
		if err != nil {
			t.Fatalf("Start() [%d] err = %v, want nil", i, err)
		}
		e := testEntry(t, sup, h)
		select {
		case <-e.done:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for process %d to terminalize", i)
		}
		handles = append(handles, h)
	}

	for i, h := range handles {
		wantRetained := i >= total-retain

		sup.mu.Lock()
		_, inRegistry := sup.entries[h]
		sup.mu.Unlock()
		if inRegistry != wantRetained {
			t.Errorf("handle %d (%q) present in registry = %v, want %v", i, h, inRegistry, wantRetained)
		}

		_, loadErr := store.Load(h)
		manifestLoadable := loadErr == nil
		if manifestLoadable != wantRetained {
			t.Errorf("handle %d (%q) manifest loadable = %v (err=%v), want %v", i, h, manifestLoadable, loadErr, wantRetained)
		}

		if got := spoolFileExists(t, spoolRoot, h); got != wantRetained {
			t.Errorf("handle %d (%q) spool file exists = %v, want %v", i, h, got, wantRetained)
		}
	}
}

// --- TestSupervisorInvalidatesObservations ---

// TestSupervisorInvalidatesObservations proves that a process's bound
// observation cache is invalidated at exactly the three points the spec's
// "Workspace coordination" section requires: process spawn, each reported
// tool.ProcessActivity, and process completion. The fake process here
// implements tool.ProcessActivitySource via fake_process_test.go's
// activities field and reports two synthetic activity events before its
// channel closes, so the expected total is 1 (spawn) + 2 (activity) + 1
// (completion) = 4.
func TestSupervisorInvalidatesObservations(t *testing.T) {
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

	activities := make(chan tool.ProcessActivity, 2)
	activities <- tool.ProcessActivity{Kind: tool.WorkspaceActivityWrite}
	activities <- tool.ProcessActivity{Kind: tool.WorkspaceActivityBroadWrite}
	close(activities)

	prepared := &fakePreparedProcess{process: &fakeProcess{activities: activities}}
	lease := &fakeLease{}
	invalidator := &fakeObservationInvalidator{}

	handle, err := sup.Start(context.Background(), owner, origin, prepared, lease, nil, invalidator, StorageCeiling{}, YieldSettings{})
	if err != nil {
		t.Fatalf("Start() err = %v, want nil", err)
	}
	e := testEntry(t, sup, handle)

	select {
	case <-e.done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the entry to terminalize")
	}

	if got, want := invalidator.InvalidateCalls(), 4; got != want {
		t.Errorf("observationInvalidator.invalidate called %d times, want %d (spawn + 2 activity events + completion)", got, want)
	}
}

// --- TestSupervisorShutdownRejectsAdmission ---

// TestSupervisorShutdownRejectsAdmission proves that once closeAdmission
// has been called, Start rejects every subsequent admission immediately
// with CodeSupervisorShuttingDown, without ever calling
// PreparedProcess.Start or PreparedProcess.Close, without releasing the
// caller-supplied lease (nothing was ever reserved on its behalf), and
// without reserving any quota.
func TestSupervisorShutdownRejectsAdmission(t *testing.T) {
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

	sup.closeAdmission()

	prepared := &fakePreparedProcess{process: &fakeProcess{}}
	lease := &fakeLease{}

	_, err := sup.Start(context.Background(), owner, origin, prepared, lease, nil, nil, StorageCeiling{}, YieldSettings{})
	if !errors.Is(err, New(CodeSupervisorShuttingDown)) {
		t.Fatalf("Start() after closeAdmission err = %v, want CodeSupervisorShuttingDown", err)
	}
	if got := prepared.StartCalls(); got != 0 {
		t.Errorf("PreparedProcess.Start called %d times after shutdown, want 0", got)
	}
	if got := prepared.CloseCalls(); got != 0 {
		t.Errorf("PreparedProcess.Close called %d times after shutdown, want 0", got)
	}
	if got := lease.ReleaseCalls(); got != 0 {
		t.Errorf("Lease.Release called %d times after shutdown, want 0", got)
	}

	sup.mu.Lock()
	loop := sup.runningByLoop[owner.LoopID]
	session := sup.runningBySession[owner.SessionID]
	memory := sup.reservedMemoryBytes
	spool := sup.reservedSpoolBytes
	sup.mu.Unlock()
	if loop != 0 || session != 0 || memory != 0 || spool != 0 {
		t.Errorf("quota reserved after rejected admission: loop=%d session=%d memory=%d spool=%d, want all 0", loop, session, memory, spool)
	}
}

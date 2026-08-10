package process

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// blockingCompletionNotifier is a completionNotifier whose notify call
// blocks until release is closed, recording the exact moment it was
// entered on entered (closed once, on first call). It exists to prove
// doTerminalize releases the quota reservation and workspace lease BEFORE
// -- not behind -- a completion notifier that is slow or blocked reaching
// its target (TestEntryTerminalizeReleasesLeaseAndQuotaBeforeNotifying).
type blockingCompletionNotifier struct {
	entered chan struct{}
	release <-chan struct{}

	mu    sync.Mutex
	calls int
}

func (f *blockingCompletionNotifier) notify(ctx context.Context, event completionEvent) error {
	close(f.entered)
	select {
	case <-f.release:
	case <-ctx.Done():
	}
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return nil
}

func (f *blockingCompletionNotifier) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

var _ completionNotifier = (*blockingCompletionNotifier)(nil)

// TestEntryTerminalizeReleasesLeaseAndQuotaBeforeNotifying proves
// doTerminalize releases the quota reservation and the workspace lifetime
// lease BEFORE calling the completion notifier (and so, by the same
// ordering, before the lifecycle publisher too) -- not after. A process has
// already fully exited by the time doTerminalize ever runs (it is only
// reached after Process.Wait has returned), so it can no longer touch the
// workspace regardless of when its lease is released; deferring release
// behind two best-effort notifications served no safety purpose and, when a
// completion notifier needs to reach a busy owning loop actor (a real
// SessionResourceServices-backed notifier ultimately dispatches into that
// loop's own command channel), created a circular wait whenever the loop's
// own SessionIdle-triggered workspace checkpoint needed this exact
// still-held lease to proceed -- discovered via Carbon's Task 28
// end-to-end integration tests, which reproduced a genuine ~10 second
// deadlock (bounded only by an unrelated shutdown drain timeout) the first
// time a real Carbon session let a turn go idle while a background
// process it had just started was still running. Before this test's fix,
// lease.ReleaseCalls()/the quota release both waited behind notify(),
// exactly reproducing that deadlock's root cause in isolation.
func TestEntryTerminalizeReleasesLeaseAndQuotaBeforeNotifying(t *testing.T) {
	t.Parallel()

	handle := testHandle(t, 11)
	owner := testOwner(t)
	origin := testOrigin(t)
	identity := Identity{Handle: handle, Owner: owner, Origin: origin}

	events := LifecycleEventIDs{
		Started: mustUUID(t), Backgrounded: mustUUID(t),
		Completed: mustUUID(t), Lost: mustUUID(t), CommandID: mustUUID(t),
	}
	createdAt := time.Now().UTC()
	startedAt := createdAt.Add(time.Millisecond)

	store := NewManifestStore(t.TempDir())
	seed := NewManifest(identity, CommandMetadata{Command: "echo hi"}, AccessReadOnly, false, createdAt, nil)
	seed.Events = events
	seed.State = StateRunning
	seed.StartedAt = &startedAt
	if err := store.Save(seed); err != nil {
		t.Fatalf("store.Save() err = %v, want nil", err)
	}

	spool, err := OpenSpool(t.TempDir(), handle, 0)
	if err != nil {
		t.Fatalf("OpenSpool() err = %v, want nil", err)
	}

	release := make(chan struct{})
	notifications := &blockingCompletionNotifier{entered: make(chan struct{}), release: release}
	lease := &fakeLease{}
	quotaReleases := &callCounter{}

	e := &entry{
		identity:      identity,
		lease:         lease,
		lifecycle:     &fakeLifecycleSink{},
		notifications: notifications,
		manifests:     &fakeManifestSaver{store: store},
		reservation:   reservation{loopID: owner.LoopID, sessionID: owner.SessionID, memoryBytes: 10, spoolBytes: 10},
		releaseQuota:  func(reservation) { quotaReleases.inc() },
		base: manifestBase{
			command: CommandMetadata{Command: "echo hi"}, access: AccessReadOnly, tty: false,
			createdAt: createdAt, startedAt: startedAt, events: events,
		},
		buffer: NewBuffer(0),
		spool:  spool,
		done:   make(chan struct{}),
	}

	finished := make(chan struct{})
	go func() {
		code := 0
		e.terminalize(context.Background(), StateExited, Result{ExitCode: &code, Reason: "exited"}, time.Now().UTC())
		close(finished)
	}()

	select {
	case <-notifications.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for notify to be entered")
	}

	// notify() is now blocked inside the notifier -- release/lease must
	// ALREADY have been reversed by this point, not waiting behind it.
	if got := quotaReleases.Count(); got != 1 {
		t.Errorf("quota released %d times while notify() was still blocked, want exactly 1 (release must not wait behind notify)", got)
	}
	if got := lease.ReleaseCalls(); got != 1 {
		t.Errorf("lease released %d times while notify() was still blocked, want exactly 1 (release must not wait behind notify)", got)
	}

	close(release)
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for terminalize to return")
	}
	if got := notifications.Calls(); got != 1 {
		t.Errorf("notify called %d times, want exactly 1", got)
	}
}

// TestEntryRunClosesDoneAfterDrainingBothStreams is a finer-grained unit
// test of entry.run/drain in isolation from Supervisor.Start's admission
// plumbing: it constructs an entry directly (a Buffer and a Spool only --
// no Manifest, no Supervisor, no quota reservation) and proves run's
// minimal 8B contract holds on its own: both streams are fully drained
// into the Buffer and Spool, and done closes once run's Wait call has
// returned and both drain goroutines have finished.
//
// TestSupervisorDrainsOrderedStreams (supervisor_test.go) is the
// strict-ordering proof through the full Supervisor.Start path; this test
// intentionally does not duplicate that precise-interleaving assertion --
// it only proves complete, lossless capture and the done-closes contract.
func TestEntryRunClosesDoneAfterDrainingBothStreams(t *testing.T) {
	t.Parallel()

	spool, err := OpenSpool(t.TempDir(), testHandle(t, 1), 0)
	if err != nil {
		t.Fatalf("OpenSpool() err = %v, want nil", err)
	}

	e := &entry{
		process: &fakeProcess{
			stdout: io.NopCloser(strings.NewReader("hello ")),
			stderr: io.NopCloser(strings.NewReader("world")),
		},
		buffer: NewBuffer(0),
		spool:  spool,
		done:   make(chan struct{}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.run(ctx)

	select {
	case <-e.done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for run to close done")
	}

	const wantLen = int64(len("hello ") + len("world"))
	if got := e.buffer.TotalBytes(); got != wantLen {
		t.Errorf("buffer.TotalBytes() = %d, want %d", got, wantLen)
	}
	if got := e.spool.TotalBytes(); got != wantLen {
		t.Errorf("spool.TotalBytes() = %d, want %d", got, wantLen)
	}

	data, _, gap, err := e.buffer.Read(0, 0)
	if err != nil {
		t.Fatalf("buffer.Read() err = %v, want nil", err)
	}
	if gap {
		t.Error("buffer.Read(0, ...) gap = true, want false")
	}
	combined := string(data)
	if !strings.Contains(combined, "hello ") || !strings.Contains(combined, "world") {
		t.Errorf("buffer content = %q, want it to contain both %q and %q", combined, "hello ", "world")
	}
	if int64(len(combined)) != wantLen {
		t.Errorf("buffer content length = %d, want %d", len(combined), wantLen)
	}
}

// --- Task 8C terminal-arbitration test fakes ---
//
// fakeManifestSaver, fakeLifecycleSink, and fakeCompletionNotifier are
// deterministic, call-counting implementations of entry's manifestSaver/
// lifecycleSink/completionNotifier dependencies, in the style of
// fake_process_test.go's fakeLease/fakePreparedProcess. They live here,
// alongside entry's own finer-grained unit test, rather than in
// fake_process_test.go: unlike fakeProcess/fakePreparedProcess (which fake
// a Harness contract this package does not own), these fake the narrow,
// package-local interfaces entry.terminalize itself defines.

// fakeManifestSaver wraps a real *ManifestStore so persisted values
// round-trip exactly like production, while independently counting Save
// calls -- the only way to directly observe terminalize's terminalOnce
// (sync.Once) "exactly one closure executes" guarantee at the manifest
// layer, since ManifestStore itself keeps no call counter of its own.
//
// loadErr, when non-nil, makes Load fail with that exact error instead of
// delegating to store -- the fault-injection seam
// TestEntryTerminalizeSynthesizesManifestWhenReloadFails uses to prove
// doTerminalize's reload-failure fallback (entry.go's synthesizeManifest):
// Save always still delegates to the real store regardless of loadErr, so a
// test can independently confirm what the fallback path actually persisted.
type fakeManifestSaver struct {
	store   *ManifestStore
	loadErr error

	mu        sync.Mutex
	saveCalls int
	loadCalls int
}

func (f *fakeManifestSaver) Load(h Handle) (Manifest, error) {
	f.mu.Lock()
	f.loadCalls++
	err := f.loadErr
	f.mu.Unlock()
	if err != nil {
		return Manifest{}, err
	}
	return f.store.Load(h)
}

func (f *fakeManifestSaver) Save(m Manifest) error {
	f.mu.Lock()
	f.saveCalls++
	f.mu.Unlock()
	return f.store.Save(m)
}

// SaveCalls reports how many times Save was called.
func (f *fakeManifestSaver) SaveCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.saveCalls
}

// LoadCalls reports how many times Load was called.
func (f *fakeManifestSaver) LoadCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loadCalls
}

var _ manifestSaver = (*fakeManifestSaver)(nil)

// fakeLifecycleSink is a call-counting implementation of lifecycleSink,
// tracking publish (terminal events) and publishStart (Started/Backgrounded
// events) independently, since a single Start call and a single terminalize
// call each drive a separate one-shot emission through this same fake.
type fakeLifecycleSink struct {
	mu           sync.Mutex
	publishCalls int
	lastEvent    lifecycleTerminalEvent

	publishStartCalls int
	lastStartEvent    lifecycleStartEvent
}

func (f *fakeLifecycleSink) publish(ctx context.Context, event lifecycleTerminalEvent) error {
	f.mu.Lock()
	f.publishCalls++
	f.lastEvent = event
	f.mu.Unlock()
	return nil
}

// PublishCalls reports how many times publish was called.
func (f *fakeLifecycleSink) PublishCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.publishCalls
}

func (f *fakeLifecycleSink) publishStart(ctx context.Context, event lifecycleStartEvent) error {
	f.mu.Lock()
	f.publishStartCalls++
	f.lastStartEvent = event
	f.mu.Unlock()
	return nil
}

// PublishStartCalls reports how many times publishStart was called.
func (f *fakeLifecycleSink) PublishStartCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.publishStartCalls
}

// LastStartEvent reports the most recent publishStart event.
func (f *fakeLifecycleSink) LastStartEvent() lifecycleStartEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastStartEvent
}

var _ lifecycleSink = (*fakeLifecycleSink)(nil)

// fakeCompletionNotifier is a call-counting implementation of
// completionNotifier.
type fakeCompletionNotifier struct {
	mu          sync.Mutex
	notifyCalls int
	lastEvent   completionEvent
}

func (f *fakeCompletionNotifier) notify(ctx context.Context, event completionEvent) error {
	f.mu.Lock()
	f.notifyCalls++
	f.lastEvent = event
	f.mu.Unlock()
	return nil
}

// NotifyCalls reports how many times notify was called.
func (f *fakeCompletionNotifier) NotifyCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.notifyCalls
}

var _ completionNotifier = (*fakeCompletionNotifier)(nil)

// fakeObservationInvalidator is a call-counting implementation of
// observationInvalidator (Task 8D). It lives alongside entry's other
// package-local-interface fakes (fakeManifestSaver, fakeLifecycleSink,
// fakeCompletionNotifier) for the same reason documented on those types:
// observationInvalidator is a narrow, package-local interface entry.run
// itself defines, not a Harness contract this package merely fakes an
// implementation of.
type fakeObservationInvalidator struct {
	mu    sync.Mutex
	calls int
}

func (f *fakeObservationInvalidator) invalidate(ctx context.Context, handle Handle) error {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return nil
}

// InvalidateCalls reports how many times invalidate was called.
func (f *fakeObservationInvalidator) InvalidateCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

var _ observationInvalidator = (*fakeObservationInvalidator)(nil)

// callCounter is a minimal thread-safe call counter, used by
// newRaceEntry's releaseQuota fake (entry.releaseQuota is a plain func
// field, not an interface, so it has no fake type of its own to count
// calls on).
type callCounter struct {
	mu    sync.Mutex
	count int
}

func (c *callCounter) inc() {
	c.mu.Lock()
	c.count++
	c.mu.Unlock()
}

func (c *callCounter) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count
}

// raceEntryFakes bundles every fake dependency newRaceEntry wires onto the
// *entry it returns, so a test can inspect each one's call count/last event
// independently.
type raceEntryFakes struct {
	manifests     *fakeManifestSaver
	lease         *fakeLease
	notifications *fakeCompletionNotifier
	lifecycle     *fakeLifecycleSink
	quotaReleases *callCounter
}

// newRaceEntry builds a fully-wired *entry directly (bypassing
// Supervisor.Start's admission plumbing entirely), with a manifest already
// durably persisted in StateRunning -- including pre-persisted
// LifecycleEventIDs, mirroring what Supervisor.Start's first two manifest
// Saves would have produced for a live admission. It exists so
// TestSupervisorTerminalRaceChoosesOnce, TestSupervisorReleasesLeaseOnce,
// and any other terminalize-focused test can race concurrent
// entry.terminalize calls against a single entry without needing a real
// tool.PreparedProcess/tool.Process pair at all (bullet 1 of Task 8C's
// description: "e.g. by calling an internal entry.terminalize(reason,
// result)-shaped method").
func newRaceEntry(t *testing.T) (*entry, raceEntryFakes) {
	t.Helper()

	handle := testHandle(t, 1)
	owner := testOwner(t)
	origin := testOrigin(t)
	identity := Identity{Handle: handle, Owner: owner, Origin: origin}

	store := NewManifestStore(t.TempDir())
	base := NewManifest(identity, CommandMetadata{Command: "echo hi"}, AccessReadOnly, false, time.Now().UTC(), nil)
	base.Events = LifecycleEventIDs{
		Started:      mustUUID(t),
		Backgrounded: mustUUID(t),
		Completed:    mustUUID(t),
		Lost:         mustUUID(t),
		CommandID:    mustUUID(t),
	}
	if err := store.Save(base); err != nil {
		t.Fatalf("store.Save(starting) err = %v, want nil", err)
	}
	startedAt := time.Now().UTC()
	base.State = StateRunning
	base.StartedAt = &startedAt
	if err := store.Save(base); err != nil {
		t.Fatalf("store.Save(running) err = %v, want nil", err)
	}

	spool, err := OpenSpool(t.TempDir(), handle, 0)
	if err != nil {
		t.Fatalf("OpenSpool() err = %v, want nil", err)
	}

	fakes := raceEntryFakes{
		manifests:     &fakeManifestSaver{store: store},
		lease:         &fakeLease{},
		notifications: &fakeCompletionNotifier{},
		lifecycle:     &fakeLifecycleSink{},
		quotaReleases: &callCounter{},
	}

	e := &entry{
		identity:      identity,
		lease:         fakes.lease,
		lifecycle:     fakes.lifecycle,
		notifications: fakes.notifications,
		manifests:     fakes.manifests,
		reservation:   reservation{loopID: owner.LoopID, sessionID: owner.SessionID, memoryBytes: 10, spoolBytes: 10},
		releaseQuota:  func(reservation) { fakes.quotaReleases.inc() },
		buffer:        NewBuffer(0),
		spool:         spool,
		done:          make(chan struct{}),
	}

	return e, fakes
}

// --- TestEntryTerminalizeSynthesizesManifestWhenReloadFails ---

// TestEntryTerminalizeSynthesizesManifestWhenReloadFails is the Phase Gate 2
// quality/security fault-injection proof for doTerminalize's reload-failure
// fallback (entry.go's synthesizeManifest): when e.manifests.Load fails --
// simulating a transient reload failure against a manifest Supervisor.Start
// already durably persisted, exactly like production would encounter --
// doTerminalize must still (a) persist SOME terminal manifest rather than
// leaving the process stuck at a nonterminal state, (b) publish the
// lifecycle event and completion notification using the entry's own
// pre-persisted stable Events (entry.base), never a zero-value/nil
// EventID/CommandID, and (c) still run quota release, lease release, and
// the onTerminal retention hook exactly once each -- the three steps the
// original report already found correct (they sit outside the old
// haveManifest guard) and which this fix must not regress.
func TestEntryTerminalizeSynthesizesManifestWhenReloadFails(t *testing.T) {
	t.Parallel()

	handle := testHandle(t, 9)
	owner := testOwner(t)
	origin := testOrigin(t)
	identity := Identity{Handle: handle, Owner: owner, Origin: origin}

	events := LifecycleEventIDs{
		Started:      mustUUID(t),
		Backgrounded: mustUUID(t),
		Completed:    mustUUID(t),
		Lost:         mustUUID(t),
		CommandID:    mustUUID(t),
	}
	createdAt := time.Now().UTC()
	startedAt := createdAt.Add(time.Millisecond)

	// Persist a real StateRunning manifest first -- exactly what
	// Supervisor.Start would have already durably written before this
	// entry's run goroutine ever reaches terminalize -- so the reload
	// failure injected below is a genuine transient-failure simulation, not
	// a "nothing was ever persisted" scenario.
	store := NewManifestStore(t.TempDir())
	seed := NewManifest(identity, CommandMetadata{Command: "echo hi"}, AccessReadOnly, false, createdAt, nil)
	seed.Events = events
	if err := store.Save(seed); err != nil {
		t.Fatalf("store.Save(starting) err = %v, want nil", err)
	}
	seed.State = StateRunning
	seed.StartedAt = &startedAt
	if err := store.Save(seed); err != nil {
		t.Fatalf("store.Save(running) err = %v, want nil", err)
	}

	spool, err := OpenSpool(t.TempDir(), handle, 0)
	if err != nil {
		t.Fatalf("OpenSpool() err = %v, want nil", err)
	}

	manifests := &fakeManifestSaver{store: store, loadErr: errors.New("injected reload failure")}
	lifecycle := &fakeLifecycleSink{}
	notifications := &fakeCompletionNotifier{}
	lease := &fakeLease{}
	quotaReleases := &callCounter{}
	onTerminalCalls := &callCounter{}

	e := &entry{
		identity:      identity,
		lease:         lease,
		lifecycle:     lifecycle,
		notifications: notifications,
		manifests:     manifests,
		reservation:   reservation{loopID: owner.LoopID, sessionID: owner.SessionID, memoryBytes: 10, spoolBytes: 10},
		releaseQuota:  func(reservation) { quotaReleases.inc() },
		onTerminal:    func(Handle, Owner) { onTerminalCalls.inc() },
		base: manifestBase{
			command:   CommandMetadata{Command: "echo hi"},
			access:    AccessReadOnly,
			tty:       false,
			createdAt: createdAt,
			startedAt: startedAt,
			events:    events,
		},
		buffer: NewBuffer(0),
		spool:  spool,
		done:   make(chan struct{}),
	}

	code := 0
	finishedAt := time.Now().UTC()
	e.terminalize(context.Background(), StateExited, Result{ExitCode: &code, Reason: "exited"}, finishedAt)

	if got := manifests.LoadCalls(); got == 0 {
		t.Fatal("Load was never called; this test is not exercising the reload-failure path")
	}

	// (a) SOME terminal manifest still gets persisted, not left stuck
	// nonterminal.
	if got := manifests.SaveCalls(); got != 1 {
		t.Errorf("manifest Save called %d times, want exactly 1", got)
	}
	final, err := store.Load(handle)
	if err != nil {
		t.Fatalf("store.Load() after terminalize err = %v, want nil", err)
	}
	if !final.State.Terminal() {
		t.Errorf("final manifest state = %v, want a terminal state", final.State)
	}
	if final.Result.ExitCode == nil || *final.Result.ExitCode != code {
		t.Errorf("final manifest Result = %+v, want ExitCode %d", final.Result, code)
	}
	if final.Events != events {
		t.Errorf("final manifest Events = %+v, want unchanged %+v", final.Events, events)
	}

	// (b) publish/notify are never called with zero-value/nil IDs -- they
	// use the entry's own pre-persisted stable Events, recovered from
	// e.base rather than the failed reload.
	if got := lifecycle.PublishCalls(); got != 1 {
		t.Errorf("lifecycle publish called %d times, want exactly 1", got)
	}
	if lifecycle.lastEvent.EventID.IsZero() {
		t.Error("lifecycle publish EventID is zero, want the stable pre-persisted Completed ID")
	}
	if lifecycle.lastEvent.EventID != events.Completed {
		t.Errorf("lifecycle publish EventID = %s, want %s", lifecycle.lastEvent.EventID, events.Completed)
	}

	if got := notifications.NotifyCalls(); got != 1 {
		t.Errorf("completion notify called %d times, want exactly 1", got)
	}
	if notifications.lastEvent.CommandID.IsZero() {
		t.Error("completion notify CommandID is zero, want the stable pre-persisted CommandID")
	}
	if notifications.lastEvent.CommandID != events.CommandID {
		t.Errorf("completion notify CommandID = %s, want %s", notifications.lastEvent.CommandID, events.CommandID)
	}

	// (c) quota release / lease release / onTerminal retention hook still
	// run correctly -- unaffected by the reload failure.
	if got := quotaReleases.Count(); got != 1 {
		t.Errorf("quota released %d times, want exactly 1", got)
	}
	if got := lease.ReleaseCalls(); got != 1 {
		t.Errorf("lease released %d times, want exactly 1", got)
	}
	if got := onTerminalCalls.Count(); got != 1 {
		t.Errorf("onTerminal called %d times, want exactly 1", got)
	}
}

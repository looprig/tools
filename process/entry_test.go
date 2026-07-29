package process

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

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
type fakeManifestSaver struct {
	store *ManifestStore

	mu        sync.Mutex
	saveCalls int
}

func (f *fakeManifestSaver) Load(h Handle) (Manifest, error) {
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

var _ manifestSaver = (*fakeManifestSaver)(nil)

// fakeLifecycleSink is a call-counting implementation of lifecycleSink.
type fakeLifecycleSink struct {
	mu           sync.Mutex
	publishCalls int
	lastEvent    lifecycleTerminalEvent
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

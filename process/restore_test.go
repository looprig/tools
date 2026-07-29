package process

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/tool"
)

// --- Task 9B restore fixtures ---
//
// restoreFixture bundles a real ManifestStore + spool root pair and the
// owner/origin identity every restore scenario in this file shares, plus the
// small helper sequence (startingManifest -> runningManifest ->
// exitedManifestWithOutput) needed to build a manifest through the same
// approved state-machine edges Supervisor.Start/entry.terminalize would --
// mirroring entry_test.go's newRaceEntry, which does the same thing for
// terminal-arbitration tests, without depending on a live Supervisor.Start
// admission at all.

type restoreFixture struct {
	manifestRoot string
	spoolRoot    string
	store        *ManifestStore
	owner        Owner
	origin       Origin
}

func newRestoreFixture(t *testing.T) *restoreFixture {
	t.Helper()
	manifestRoot := t.TempDir()
	spoolRoot := t.TempDir()
	return &restoreFixture{
		manifestRoot: manifestRoot,
		spoolRoot:    spoolRoot,
		store:        NewManifestStore(manifestRoot),
		owner:        testOwner(t),
		origin:       testOrigin(t),
	}
}

// newSupervisor builds a Supervisor rooted at the fixture's real store and
// spool root, exactly as a caller would after loading a session's existing
// resource root back from disk.
func (f *restoreFixture) newSupervisor(t *testing.T, lifecycle lifecycleSink, notifications completionNotifier) *Supervisor {
	t.Helper()
	sup, err := NewSupervisor(Config{}, f.store, f.spoolRoot, lifecycle, notifications)
	if err != nil {
		t.Fatalf("NewSupervisor() err = %v, want nil", err)
	}
	return sup
}

// startingManifest persists and returns h's manifest in StateStarting, with
// every stable LifecycleEventIDs field already populated -- mirroring what
// Supervisor.Start's first manifest Save produces for a live admission
// (supervisor.go's newLifecycleEventIDs).
func (f *restoreFixture) startingManifest(t *testing.T, h Handle) Manifest {
	t.Helper()
	identity := Identity{Handle: h, Owner: f.owner, Origin: f.origin}
	m := NewManifest(identity, CommandMetadata{Command: "echo hi"}, AccessReadOnly, false, time.Now().UTC(), nil)
	m.Events = LifecycleEventIDs{
		Started:      mustUUID(t),
		Backgrounded: mustUUID(t),
		Completed:    mustUUID(t),
		Lost:         mustUUID(t),
		CommandID:    mustUUID(t),
	}
	if err := f.store.Save(m); err != nil {
		t.Fatalf("Save(starting) err = %v, want nil", err)
	}
	return m
}

// runningManifest advances m (already saved in StateStarting by
// startingManifest) to StateRunning.
func (f *restoreFixture) runningManifest(t *testing.T, m Manifest) Manifest {
	t.Helper()
	startedAt := time.Now().UTC()
	m.State = StateRunning
	m.StartedAt = &startedAt
	if err := f.store.Save(m); err != nil {
		t.Fatalf("Save(running) err = %v, want nil", err)
	}
	return m
}

// exitedManifestWithOutput advances m (already running) to a terminal
// StateExited with the given exit code and cursors (see appendSpoolOutput),
// and persists it.
func (f *restoreFixture) exitedManifestWithOutput(t *testing.T, m Manifest, exitCode int, cursors SpoolCursors) Manifest {
	t.Helper()
	finishedAt := time.Now().UTC()
	code := exitCode
	m.State = StateExited
	m.FinishedAt = &finishedAt
	m.Result = Result{ExitCode: &code, Reason: "exited"}
	m.Cursors = cursors
	m.CompletionPublished++
	if err := f.store.Save(m); err != nil {
		t.Fatalf("Save(exited) err = %v, want nil", err)
	}
	return m
}

// appendSpoolOutput opens h's real on-disk Spool beneath f.spoolRoot and
// appends data to it, returning the resulting SpoolCursors so a test can
// carry them into exitedManifestWithOutput -- exactly the cursors a live
// entry.doTerminalize would have recorded from its own Spool at the same
// moment.
func (f *restoreFixture) appendSpoolOutput(t *testing.T, h Handle, data []byte) SpoolCursors {
	t.Helper()
	spool, err := OpenSpool(f.spoolRoot, h, 0)
	if err != nil {
		t.Fatalf("OpenSpool() err = %v, want nil", err)
	}
	if _, err := spool.Append(data); err != nil {
		t.Fatalf("spool.Append() err = %v, want nil", err)
	}
	return SpoolCursors{TotalBytes: spool.TotalBytes(), RetainedFrom: spool.RetainedFrom()}
}

// recordingLifecycleSink records every publish call it receives, in order,
// so a test can compare the EventID used across two separate calls (e.g. an
// original attempt and a simulated post-crash retry) -- unlike entry_test.go's
// fakeLifecycleSink, which only retains the most recent call.
type recordingLifecycleSink struct {
	mu     sync.Mutex
	events []lifecycleTerminalEvent
}

func (r *recordingLifecycleSink) publish(_ context.Context, event lifecycleTerminalEvent) error {
	r.mu.Lock()
	r.events = append(r.events, event)
	r.mu.Unlock()
	return nil
}

// Events returns every event recorded so far, in call order.
func (r *recordingLifecycleSink) Events() []lifecycleTerminalEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]lifecycleTerminalEvent, len(r.events))
	copy(out, r.events)
	return out
}

// publishStart is a no-op: restore_test.go only exercises restore
// reconciliation's terminal (Lost) publication path, never Supervisor.Start's
// Started/Backgrounded emission.
func (r *recordingLifecycleSink) publishStart(_ context.Context, _ lifecycleStartEvent) error {
	return nil
}

var _ lifecycleSink = (*recordingLifecycleSink)(nil)

// recordingCompletionNotifier is recordingLifecycleSink's completionNotifier
// counterpart.
type recordingCompletionNotifier struct {
	mu     sync.Mutex
	events []completionEvent
}

func (r *recordingCompletionNotifier) notify(_ context.Context, event completionEvent) error {
	r.mu.Lock()
	r.events = append(r.events, event)
	r.mu.Unlock()
	return nil
}

// Events returns every event recorded so far, in call order.
func (r *recordingCompletionNotifier) Events() []completionEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]completionEvent, len(r.events))
	copy(out, r.events)
	return out
}

var _ completionNotifier = (*recordingCompletionNotifier)(nil)

// --- TestRestoreCompletedOutput ---

// TestRestoreCompletedOutput proves that a manifest already in a terminal
// state at restore time is reopened as a queryable, non-running entry: its
// exit code, state, and previously-appended output all remain readable
// through the restored entry's Spool, with no live tool.Process registered
// against it.
func TestRestoreCompletedOutput(t *testing.T) {
	t.Parallel()

	f := newRestoreFixture(t)
	h := testHandle(t, 1)

	m := f.startingManifest(t, h)
	m = f.runningManifest(t, m)
	cursors := f.appendSpoolOutput(t, h, []byte("hello from a completed process"))
	f.exitedManifestWithOutput(t, m, 7, cursors)

	sup := f.newSupervisor(t, nil, nil)

	report, err := sup.Restore(context.Background())
	if err != nil {
		t.Fatalf("Restore() err = %v, want nil", err)
	}
	if len(report.Errors) != 0 {
		t.Fatalf("Restore() errors = %+v, want none", report.Errors)
	}
	if len(report.Reconciled) != 1 || report.Reconciled[0] != h {
		t.Fatalf("Restore() reconciled = %+v, want [%v]", report.Reconciled, h)
	}

	e := testEntry(t, sup, h)
	if e.identity.Owner != f.owner || e.identity.Origin != f.origin {
		t.Errorf("restored entry identity = %+v, want owner %+v origin %+v", e.identity, f.owner, f.origin)
	}
	if !closed(e.done) {
		t.Error("restored terminal entry's done channel is not closed")
	}
	if e.process != nil {
		t.Error("restored entry has a live tool.Process, want nil")
	}

	data, _, gap, err := e.spool.Read(0, 0)
	if err != nil {
		t.Fatalf("spool.Read() err = %v, want nil", err)
	}
	if gap {
		t.Error("spool.Read(0, ...) gap = true, want false")
	}
	if string(data) != "hello from a completed process" {
		t.Errorf("spool content = %q, want %q", data, "hello from a completed process")
	}

	reloaded, err := sup.manifests.Load(h)
	if err != nil {
		t.Fatalf("manifests.Load() err = %v, want nil", err)
	}
	if reloaded.State != StateExited {
		t.Errorf("State = %v, want %v", reloaded.State, StateExited)
	}
	if reloaded.Result.ExitCode == nil || *reloaded.Result.ExitCode != 7 {
		t.Errorf("Result.ExitCode = %v, want 7", reloaded.Result.ExitCode)
	}
}

// --- TestRestoreRunningBecomesLost ---

// TestRestoreRunningBecomesLost proves that a manifest still in
// StateStarting or StateRunning at restore time is durably transitioned to
// the terminal StateLostOnRestore -- the supervisor process that was running
// it no longer exists -- and that the resulting entry is queryable (not
// running), all while its pre-persisted stable LifecycleEventIDs are left
// unchanged.
func TestRestoreRunningBecomesLost(t *testing.T) {
	t.Parallel()

	f := newRestoreFixture(t)
	h := testHandle(t, 1)
	m := f.startingManifest(t, h)
	m = f.runningManifest(t, m)

	sup := f.newSupervisor(t, nil, nil)

	report, err := sup.Restore(context.Background())
	if err != nil {
		t.Fatalf("Restore() err = %v, want nil", err)
	}
	if len(report.Errors) != 0 {
		t.Fatalf("Restore() errors = %+v, want none", report.Errors)
	}

	reloaded, err := sup.manifests.Load(h)
	if err != nil {
		t.Fatalf("manifests.Load() err = %v, want nil", err)
	}
	if reloaded.State != StateLostOnRestore {
		t.Errorf("State = %v, want %v", reloaded.State, StateLostOnRestore)
	}
	if reloaded.FinishedAt == nil {
		t.Error("FinishedAt = nil, want set")
	}
	if reloaded.Result.Reason != "lost-on-restore" {
		t.Errorf("Result.Reason = %q, want %q", reloaded.Result.Reason, "lost-on-restore")
	}
	if reloaded.Events.Lost != m.Events.Lost || reloaded.Events.CommandID != m.Events.CommandID {
		t.Errorf("Events = %+v, want unchanged from %+v", reloaded.Events, m.Events)
	}

	e := testEntry(t, sup, h)
	if !closed(e.done) {
		t.Error("restored entry's done channel is not closed")
	}
	if e.process != nil {
		t.Error("restored entry has a live tool.Process, want nil")
	}
}

// --- TestRestoreNeverSignalsPersistedPID ---

// TestRestoreNeverSignalsPersistedPID proves that a populated, nonzero
// osMetadata.PID recorded on a running manifest is never read or acted on
// during restore reconciliation. Supervisor.Restore's own signature (a
// context only) accepts no tool.Process, tool.PreparedProcess, or
// tool.AsyncProcessRunner value at all, so there is structurally nothing in
// this reconciliation path capable of ever calling Signal on anything;
// restore only ever transitions state through the ManifestStore. This test
// proves the behavioral half of that claim: reconciliation completes
// correctly into lost_on_restore despite the populated PID, and the PID
// itself is left exactly as persisted -- neither read out and used, nor
// cleared -- restore simply never acts on it.
func TestRestoreNeverSignalsPersistedPID(t *testing.T) {
	t.Parallel()

	f := newRestoreFixture(t)
	h := testHandle(t, 1)
	m := f.startingManifest(t, h)
	m = f.runningManifest(t, m)
	m.os = osMetadata{PID: 424242}
	if err := f.store.Save(m); err != nil {
		t.Fatalf("Save(running, with os metadata) err = %v, want nil", err)
	}

	sup := f.newSupervisor(t, nil, nil)

	report, err := sup.Restore(context.Background())
	if err != nil {
		t.Fatalf("Restore() err = %v, want nil", err)
	}
	if len(report.Errors) != 0 {
		t.Fatalf("Restore() errors = %+v, want none", report.Errors)
	}

	reloaded, err := sup.manifests.Load(h)
	if err != nil {
		t.Fatalf("manifests.Load() err = %v, want nil", err)
	}
	if reloaded.State != StateLostOnRestore {
		t.Errorf("State = %v, want %v", reloaded.State, StateLostOnRestore)
	}
	if reloaded.os.PID != 424242 {
		t.Errorf("os.PID = %d, want unchanged 424242", reloaded.os.PID)
	}
}

// --- TestRestorePublicationCrashRetriesStableID ---

// TestRestorePublicationCrashRetriesStableID proves that restore's lost
// lifecycle event and completion notification publication always reuses the
// manifest's already-persisted LifecycleEventIDs.Lost/CommandID, even across
// a simulated crash-and-retry: the first call is Restore's own original
// reconciliation pass, and the second directly re-invokes the same
// publish/notify step (markLostOnRestore) against the manifest state restore
// already reached, standing in for a retry after a crash between the
// original journal append and its marker rewrite. Both attempts must use the
// exact same IDs -- restore never mints a fresh one.
func TestRestorePublicationCrashRetriesStableID(t *testing.T) {
	t.Parallel()

	f := newRestoreFixture(t)
	h := testHandle(t, 1)
	m := f.startingManifest(t, h)
	m = f.runningManifest(t, m)

	lifecycle := &recordingLifecycleSink{}
	notifications := &recordingCompletionNotifier{}
	sup := f.newSupervisor(t, lifecycle, notifications)

	// Attempt 1: the original restore pass.
	if _, err := sup.Restore(context.Background()); err != nil {
		t.Fatalf("Restore() err = %v, want nil", err)
	}

	afterFirst, err := sup.manifests.Load(h)
	if err != nil {
		t.Fatalf("manifests.Load() err = %v, want nil", err)
	}
	if afterFirst.State != StateLostOnRestore {
		t.Fatalf("State after first restore = %v, want %v", afterFirst.State, StateLostOnRestore)
	}

	// Attempt 2: simulate a crash between the original journal append and
	// its marker rewrite by directly re-invoking the same publish/notify
	// step.
	if _, err := sup.markLostOnRestore(context.Background(), afterFirst); err != nil {
		t.Fatalf("markLostOnRestore() (retry) err = %v, want nil", err)
	}

	lifecycleEvents := lifecycle.Events()
	if len(lifecycleEvents) != 2 {
		t.Fatalf("lifecycle publish calls = %d, want 2", len(lifecycleEvents))
	}
	for i, ev := range lifecycleEvents {
		if ev.EventID != m.Events.Lost {
			t.Errorf("lifecycleEvents[%d].EventID = %v, want %v (manifest's pre-persisted Events.Lost)", i, ev.EventID, m.Events.Lost)
		}
		if ev.Kind != tool.ProcessLifecycleLost {
			t.Errorf("lifecycleEvents[%d].Kind = %v, want ProcessLifecycleLost", i, ev.Kind)
		}
	}

	notifyEvents := notifications.Events()
	if len(notifyEvents) != 2 {
		t.Fatalf("notify calls = %d, want 2", len(notifyEvents))
	}
	for i, ev := range notifyEvents {
		if ev.CommandID != m.Events.CommandID {
			t.Errorf("notifyEvents[%d].CommandID = %v, want %v (manifest's pre-persisted Events.CommandID)", i, ev.CommandID, m.Events.CommandID)
		}
	}
}

// --- TestRestoreIsolatesCorruptManifest ---

// TestRestoreIsolatesCorruptManifest proves the combined-acceptance bullet
// "corrupt entries are isolated and reported without hiding healthy
// entries": a manifest file that fails to load (malformed JSON, reported as
// CodeManifestCorrupt) is recorded in RestoreReport.Errors, while a healthy
// sibling manifest in the same resource root is still fully reconciled and
// remains queryable.
func TestRestoreIsolatesCorruptManifest(t *testing.T) {
	t.Parallel()

	f := newRestoreFixture(t)

	healthy := testHandle(t, 1)
	m := f.startingManifest(t, healthy)
	m = f.runningManifest(t, m)
	cursors := f.appendSpoolOutput(t, healthy, []byte("still readable"))
	f.exitedManifestWithOutput(t, m, 0, cursors)

	corrupt := testHandle(t, 2)
	corruptPath := filepath.Join(f.manifestRoot, string(corrupt)+manifestSuffix)
	if err := os.WriteFile(corruptPath, []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("WriteFile(corrupt manifest) err = %v, want nil", err)
	}

	sup := f.newSupervisor(t, nil, nil)

	report, err := sup.Restore(context.Background())
	if err != nil {
		t.Fatalf("Restore() err = %v, want nil", err)
	}

	if len(report.Reconciled) != 1 || report.Reconciled[0] != healthy {
		t.Fatalf("Restore() reconciled = %+v, want [%v]", report.Reconciled, healthy)
	}
	if len(report.Errors) != 1 || report.Errors[0].Handle != corrupt {
		t.Fatalf("Restore() errors = %+v, want exactly one error for handle %v", report.Errors, corrupt)
	}

	var procErr *Error
	if !errors.As(report.Errors[0].Err, &procErr) || procErr.Code != CodeManifestCorrupt {
		t.Errorf("Restore() error for corrupt handle = %v, want *Error{Code: CodeManifestCorrupt}", report.Errors[0].Err)
	}

	e := testEntry(t, sup, healthy)
	data, _, _, err := e.spool.Read(0, 0)
	if err != nil {
		t.Fatalf("spool.Read() err = %v, want nil", err)
	}
	if string(data) != "still readable" {
		t.Errorf("spool content = %q, want %q", data, "still readable")
	}
}

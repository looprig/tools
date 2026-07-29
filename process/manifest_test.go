package process

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testHandle builds a well-formed Handle deterministically, so tests do not
// depend on NewHandle's randomness or a HandleExists check. seed
// differentiates handles within a single test that needs more than one.
func testHandle(t *testing.T, seed byte) Handle {
	t.Helper()
	buf := make([]byte, HandleEntropyBytes)
	buf[0] = seed
	h := Handle(base64.RawURLEncoding.EncodeToString(buf))
	if !h.Valid() {
		t.Fatalf("test fixture bug: constructed handle %q is not Valid()", h)
	}
	return h
}

// testOwner and testOrigin build fresh identity fixtures using identity_test.go's
// mustUUID helper (shared within package process).
func testOwner(t *testing.T) Owner {
	t.Helper()
	return Owner{SessionID: mustUUID(t), LoopID: mustUUID(t)}
}

func testOrigin(t *testing.T) Origin {
	t.Helper()
	return Origin{ToolExecutionID: mustUUID(t)}
}

// baseManifest returns a minimal, Validate-passing, non-terminal manifest
// for handle h.
func baseManifest(t *testing.T, h Handle) Manifest {
	t.Helper()
	id := Identity{Handle: h, Owner: testOwner(t), Origin: testOrigin(t)}
	return NewManifest(id, CommandMetadata{Command: "echo hi"}, AccessReadOnly, false, time.Now().UTC(), nil)
}

func exitedManifest(t *testing.T, m Manifest, finishedAt time.Time, code int) Manifest {
	t.Helper()
	m.State = StateExited
	m.FinishedAt = &finishedAt
	m.Result = Result{ExitCode: &code, Reason: "exited"}
	return m
}

// --- Manifest.Validate ---

func TestManifestValidateAcceptsBaseManifest(t *testing.T) {
	t.Parallel()
	m := baseManifest(t, testHandle(t, 1))
	if err := m.Validate(); err != nil {
		t.Errorf("Validate() err = %v, want nil", err)
	}
}

func TestManifestValidateRequiresOwner(t *testing.T) {
	t.Parallel()
	m := baseManifest(t, testHandle(t, 1))
	m.Owner = Owner{}
	assertManifestCorrupt(t, m.Validate())
}

func TestManifestValidateRequiresOrigin(t *testing.T) {
	t.Parallel()
	m := baseManifest(t, testHandle(t, 1))
	m.Origin = Origin{}
	assertManifestCorrupt(t, m.Validate())
}

func TestManifestValidateRequiresValidHandle(t *testing.T) {
	t.Parallel()
	m := baseManifest(t, testHandle(t, 1))
	m.Handle = "not valid base64!!"
	assertManifestCorrupt(t, m.Validate())
}

func TestManifestValidateRejectsImpossibleState(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()

	tests := []struct {
		name string
		mut  func(*Manifest)
	}{
		{"unrecognized state", func(m *Manifest) { m.State = "bogus" }},
		{"terminal without finished_at", func(m *Manifest) { m.State = StateExited; code := 0; m.Result.ExitCode = &code }},
		{"non-terminal with finished_at", func(m *Manifest) { m.FinishedAt = &now }},
		{"finished before created", func(m *Manifest) {
			m.State = StateExited
			before := m.CreatedAt.Add(-time.Hour)
			m.FinishedAt = &before
			code := 0
			m.Result.ExitCode = &code
		}},
		{"started before created", func(m *Manifest) {
			before := m.CreatedAt.Add(-time.Hour)
			m.StartedAt = &before
		}},
		{"exited without exit code", func(m *Manifest) { m.State = StateExited; m.FinishedAt = &now }},
		{"exit code outside exited", func(m *Manifest) { code := 0; m.Result.ExitCode = &code }},
		{"retained_from exceeds total_bytes", func(m *Manifest) { m.Cursors = SpoolCursors{TotalBytes: 1, RetainedFrom: 2} }},
		{"negative completion_published", func(m *Manifest) { m.CompletionPublished = -1 }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := baseManifest(t, testHandle(t, 1))
			tt.mut(&m)
			assertManifestCorrupt(t, m.Validate())
		})
	}
}

func assertManifestCorrupt(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("err = nil, want CodeManifestCorrupt")
	}
	if !errors.Is(err, New(CodeManifestCorrupt)) {
		t.Errorf("err = %v, want CodeManifestCorrupt", err)
	}
}

// --- ManifestStore: versioned JSON on disk ---

func TestManifestStoreSaveWritesVersionedJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewManifestStore(dir)
	h := testHandle(t, 1)
	m := baseManifest(t, h)

	if err := store.Save(m); err != nil {
		t.Fatalf("Save() err = %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, string(h)+manifestSuffix))
	if err != nil {
		t.Fatalf("ReadFile() err = %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("Unmarshal() err = %v", err)
	}
	version, ok := doc["version"].(float64)
	if !ok || int(version) != manifestVersion {
		t.Errorf("on-disk version = %v, want %d", doc["version"], manifestVersion)
	}
}

func TestManifestStoreLoadRoundTrips(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewManifestStore(dir)
	h := testHandle(t, 1)
	want := baseManifest(t, h)

	if err := store.Save(want); err != nil {
		t.Fatalf("Save() err = %v", err)
	}
	got, err := store.Load(h)
	if err != nil {
		t.Fatalf("Load() err = %v", err)
	}
	if got.Handle != want.Handle || !got.Owner.Equal(want.Owner) || got.Origin != want.Origin {
		t.Errorf("Load() identity = %+v, want %+v", got.Identity, want.Identity)
	}
	if got.Command != want.Command || got.Access != want.Access || got.State != want.State {
		t.Errorf("Load() = %+v, want %+v", got, want)
	}
}

func TestManifestStoreLoadMissingReportsNotFound(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewManifestStore(dir)

	_, err := store.Load(testHandle(t, 1))
	if !errors.Is(err, New(CodeNotFound)) {
		t.Errorf("Load() err = %v, want CodeNotFound", err)
	}
}

func TestManifestStoreLoadMalformedJSONIsCorrupt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewManifestStore(dir)
	h := testHandle(t, 1)

	if err := os.WriteFile(filepath.Join(dir, string(h)+manifestSuffix), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("WriteFile() err = %v", err)
	}

	_, err := store.Load(h)
	assertManifestCorrupt(t, err)
}

func TestManifestStoreLoadUnknownVersionIsCorrupt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewManifestStore(dir)
	h := testHandle(t, 1)

	doc := map[string]any{"version": 999}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("Marshal() err = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, string(h)+manifestSuffix), raw, 0o600); err != nil {
		t.Fatalf("WriteFile() err = %v", err)
	}

	_, err = store.Load(h)
	assertManifestCorrupt(t, err)
}

func TestManifestStoreLoadInvalidOwnerIsCorrupt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewManifestStore(dir)
	h := testHandle(t, 1)

	w := toWire(baseManifest(t, h))
	w.Owner = Owner{} // zero owner: invalid per Validate
	raw, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("Marshal() err = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, string(h)+manifestSuffix), raw, 0o600); err != nil {
		t.Fatalf("WriteFile() err = %v", err)
	}

	_, err = store.Load(h)
	assertManifestCorrupt(t, err)
}

// --- ManifestStore: state/cursors never move backward ---

func TestManifestStoreSaveAllowsApprovedStateTransition(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewManifestStore(dir)
	h := testHandle(t, 1)
	m := baseManifest(t, h)

	if err := store.Save(m); err != nil {
		t.Fatalf("Save(starting) err = %v", err)
	}
	m.State = StateRunning
	started := time.Now().UTC()
	m.StartedAt = &started
	if err := store.Save(m); err != nil {
		t.Fatalf("Save(running) err = %v", err)
	}

	got, err := store.Load(h)
	if err != nil {
		t.Fatalf("Load() err = %v", err)
	}
	if got.State != StateRunning {
		t.Errorf("State = %v, want %v", got.State, StateRunning)
	}
}

func TestManifestStoreSaveRejectsUnapprovedStateTransition(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewManifestStore(dir)
	h := testHandle(t, 1)
	m := baseManifest(t, h)
	if err := store.Save(m); err != nil {
		t.Fatalf("Save(starting) err = %v", err)
	}

	// starting -> exited is not an approved edge (state.go transitions).
	m = exitedManifest(t, m, time.Now().UTC(), 0)
	err := store.Save(m)
	var transErr *TransitionError
	if !errors.As(err, &transErr) {
		t.Fatalf("Save() err = %v, want *TransitionError", err)
	}

	got, loadErr := store.Load(h)
	if loadErr != nil {
		t.Fatalf("Load() err = %v", loadErr)
	}
	if got.State != StateStarting {
		t.Errorf("on-disk State = %v, want unchanged %v after rejected update", got.State, StateStarting)
	}
}

func TestManifestStoreSaveRejectsCursorsMovingBackward(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewManifestStore(dir)
	h := testHandle(t, 1)
	m := baseManifest(t, h)
	m.Cursors = SpoolCursors{TotalBytes: 100, RetainedFrom: 10}
	if err := store.Save(m); err != nil {
		t.Fatalf("Save() err = %v", err)
	}

	regressed := m
	regressed.Cursors.TotalBytes = 50
	err := store.Save(regressed)
	var nonMono *NonMonotonicUpdateError
	if !errors.As(err, &nonMono) {
		t.Fatalf("Save() err = %v, want *NonMonotonicUpdateError", err)
	}

	regressedRetained := m
	regressedRetained.Cursors.RetainedFrom = 5
	err = store.Save(regressedRetained)
	if !errors.As(err, &nonMono) {
		t.Fatalf("Save() err = %v, want *NonMonotonicUpdateError", err)
	}
}

func TestManifestStoreSaveAllowsCursorsAdvancing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewManifestStore(dir)
	h := testHandle(t, 1)
	m := baseManifest(t, h)
	m.Cursors = SpoolCursors{TotalBytes: 10, RetainedFrom: 0}
	if err := store.Save(m); err != nil {
		t.Fatalf("Save() err = %v", err)
	}
	m.Cursors = SpoolCursors{TotalBytes: 20, RetainedFrom: 5}
	if err := store.Save(m); err != nil {
		t.Fatalf("Save() err = %v, want nil for advancing cursors", err)
	}
}

func TestManifestStoreSaveRejectsCompletionPublishedMovingBackward(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewManifestStore(dir)
	h := testHandle(t, 1)
	m := baseManifest(t, h)
	m.CompletionPublished = 3
	if err := store.Save(m); err != nil {
		t.Fatalf("Save() err = %v", err)
	}

	m.CompletionPublished = 1
	err := store.Save(m)
	var nonMono *NonMonotonicUpdateError
	if !errors.As(err, &nonMono) {
		t.Fatalf("Save() err = %v, want *NonMonotonicUpdateError", err)
	}
}

func TestManifestStoreSaveAllowsRepeatingCompletionPublished(t *testing.T) {
	// completion-published is monotonic but is not a deduplication boundary:
	// saving the same value again (a retry) must succeed, not error.
	t.Parallel()
	dir := t.TempDir()
	store := NewManifestStore(dir)
	h := testHandle(t, 1)
	m := baseManifest(t, h)
	m.CompletionPublished = 2
	if err := store.Save(m); err != nil {
		t.Fatalf("Save() err = %v", err)
	}
	if err := store.Save(m); err != nil {
		t.Errorf("Save() (repeat) err = %v, want nil", err)
	}
}

// --- ManifestStore: terminal result immutable ---

func TestManifestStoreSaveRejectsChangedTerminalResult(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewManifestStore(dir)
	h := testHandle(t, 1)
	m := baseManifest(t, h)
	m.State = StateRunning
	started := time.Now().UTC()
	m.StartedAt = &started
	if err := store.Save(m); err != nil {
		t.Fatalf("Save(running) err = %v", err)
	}

	finishedAt := time.Now().UTC()
	terminal := exitedManifest(t, m, finishedAt, 0)
	if err := store.Save(terminal); err != nil {
		t.Fatalf("Save(exited) err = %v", err)
	}

	changed := terminal
	newCode := 1
	changed.Result = Result{ExitCode: &newCode, Reason: "exited"}
	err := store.Save(changed)
	var resultErr *TerminalResultChangedError
	if !errors.As(err, &resultErr) {
		t.Fatalf("Save() err = %v, want *TerminalResultChangedError", err)
	}
}

func TestManifestStoreSaveAllowsRepeatingIdenticalTerminalResult(t *testing.T) {
	// A retry that resaves the exact same terminal manifest (e.g. after a
	// crash between the atomic write and a completion-publish attempt) must
	// succeed, not be rejected as a "change".
	t.Parallel()
	dir := t.TempDir()
	store := NewManifestStore(dir)
	h := testHandle(t, 1)
	m := baseManifest(t, h)
	terminal := exitedManifest(t, m, time.Now().UTC(), 7)
	if err := store.Save(terminal); err != nil {
		t.Fatalf("Save() err = %v", err)
	}
	if err := store.Save(terminal); err != nil {
		t.Errorf("Save() (repeat identical) err = %v, want nil", err)
	}
}

func TestManifestStoreSaveRejectsTransitionOutOfTerminalState(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewManifestStore(dir)
	h := testHandle(t, 1)
	m := baseManifest(t, h)
	terminal := exitedManifest(t, m, time.Now().UTC(), 0)
	if err := store.Save(terminal); err != nil {
		t.Fatalf("Save() err = %v", err)
	}

	terminal.State = StateFailed
	terminal.Result = Result{Reason: "failed"}
	err := store.Save(terminal)
	var transErr *TransitionError
	if !errors.As(err, &transErr) {
		t.Fatalf("Save() err = %v, want *TransitionError", err)
	}
}

// --- ManifestStore: stable lifecycle EventIDs / CommandID ---

func TestManifestStoreSaveAllocatesLifecycleEventIDsOnce(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewManifestStore(dir)
	h := testHandle(t, 1)
	m := baseManifest(t, h)
	m.Events.Started = mustUUID(t)
	m.Events.CommandID = mustUUID(t)
	if err := store.Save(m); err != nil {
		t.Fatalf("Save() err = %v", err)
	}

	got, err := store.Load(h)
	if err != nil {
		t.Fatalf("Load() err = %v", err)
	}
	if got.Events.Started != m.Events.Started || got.Events.CommandID != m.Events.CommandID {
		t.Errorf("Load().Events = %+v, want %+v", got.Events, m.Events)
	}

	reassigned := got
	reassigned.Events.Started = mustUUID(t)
	err = store.Save(reassigned)
	var idErr *LifecycleEventIDChangedError
	if !errors.As(err, &idErr) {
		t.Fatalf("Save() err = %v, want *LifecycleEventIDChangedError", err)
	}
	if idErr.Field != "started" {
		t.Errorf("Field = %q, want %q", idErr.Field, "started")
	}
}

func TestManifestStoreSaveAllowsResavingSameLifecycleEventIDs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewManifestStore(dir)
	h := testHandle(t, 1)
	m := baseManifest(t, h)
	m.Events.Completed = mustUUID(t)
	if err := store.Save(m); err != nil {
		t.Fatalf("Save() err = %v", err)
	}
	if err := store.Save(m); err != nil {
		t.Errorf("Save() (repeat) err = %v, want nil", err)
	}
}

func TestManifestStoreSaveAllowsAllocatingASecondLifecycleEventID(t *testing.T) {
	// Started may be allocated on one Save and Completed on a later Save,
	// as long as neither previously-allocated field changes.
	t.Parallel()
	dir := t.TempDir()
	store := NewManifestStore(dir)
	h := testHandle(t, 1)
	m := baseManifest(t, h)
	m.Events.Started = mustUUID(t)
	if err := store.Save(m); err != nil {
		t.Fatalf("Save() err = %v", err)
	}
	m.Events.Completed = mustUUID(t)
	if err := store.Save(m); err != nil {
		t.Errorf("Save() err = %v, want nil for allocating a previously-zero field", err)
	}
}

// --- ManifestStore: immutable identity ---

func TestManifestStoreSaveRejectsIdentityChange(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewManifestStore(dir)
	h := testHandle(t, 1)
	m := baseManifest(t, h)
	if err := store.Save(m); err != nil {
		t.Fatalf("Save() err = %v", err)
	}

	changed := m
	changed.Owner = testOwner(t)
	err := store.Save(changed)
	var idErr *ImmutableIdentityChangedError
	if !errors.As(err, &idErr) {
		t.Fatalf("Save() err = %v, want *ImmutableIdentityChangedError", err)
	}
}

// --- ManifestStore: refuses to overwrite corrupt-on-disk manifests ---

func TestManifestStoreSaveRefusesToOverwriteCorruptExisting(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewManifestStore(dir)
	h := testHandle(t, 1)

	if err := os.WriteFile(filepath.Join(dir, string(h)+manifestSuffix), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("WriteFile() err = %v", err)
	}

	err := store.Save(baseManifest(t, h))
	assertManifestCorrupt(t, err)
}

// --- ManifestStore: no path escapes the resource root ---

func TestManifestStorePathNeverEscapesRoot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewManifestStore(dir)

	malicious := []Handle{
		"../../../../etc/passwd",
		"..",
		"/etc/passwd",
		"a/../../b",
		"",
	}
	for _, h := range malicious {
		if _, err := store.Load(h); !errors.Is(err, New(CodeNotFound)) {
			t.Errorf("Load(%q) err = %v, want CodeNotFound", h, err)
		}
		m := baseManifest(t, testHandle(t, 1))
		m.Handle = h
		if err := store.Save(m); err == nil {
			t.Errorf("Save(handle=%q) err = nil, want rejection", h)
		}
	}

	// No file should have been created anywhere outside dir.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() err = %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("resource root has %d unexpected entries after malicious handles", len(entries))
	}
}

// --- os metadata: unexported, persisted, ignored by state transition ---

func TestManifestOSMetadataRoundTrips(t *testing.T) {
	// White-box: this test lives in package process and reaches m.os
	// directly, exactly like a future same-process teardown helper would.
	// No exported Manifest field or method exposes this value.
	t.Parallel()
	dir := t.TempDir()
	store := NewManifestStore(dir)
	h := testHandle(t, 1)
	m := baseManifest(t, h)
	m.os = osMetadata{PID: 4242}

	if err := store.Save(m); err != nil {
		t.Fatalf("Save() err = %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, string(h)+manifestSuffix))
	if err != nil {
		t.Fatalf("ReadFile() err = %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("Unmarshal() err = %v", err)
	}
	osDoc, ok := doc["os_metadata"].(map[string]any)
	if !ok {
		t.Fatalf("on-disk document has no os_metadata object: %v", doc)
	}
	if pid, _ := osDoc["PID"].(float64); int(pid) != 4242 {
		t.Errorf("on-disk os_metadata.PID = %v, want 4242", osDoc["PID"])
	}

	got, err := store.Load(h)
	if err != nil {
		t.Fatalf("Load() err = %v", err)
	}
	if got.os != m.os {
		t.Errorf("Load().os = %+v, want %+v", got.os, m.os)
	}
}

// TestManifestOSMetadataIgnoredByStateTransition proves that
// validateManifestUpdate's state-transition check — the mechanism a future
// restore-reconciliation path (Task 9) will reuse to move a manifest toward
// lost_on_restore — never reads or depends on osMetadata. Two manifests
// identical except for os content must transition identically.
func TestManifestOSMetadataIgnoredByStateTransition(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewManifestStore(dir)

	for i, pid := range []int{0, 777} {
		h := testHandle(t, byte(i+1))
		m := baseManifest(t, h)
		m.State = StateRunning
		started := time.Now().UTC()
		m.StartedAt = &started
		m.os = osMetadata{PID: pid}
		if err := store.Save(m); err != nil {
			t.Fatalf("Save(running, pid=%d) err = %v", pid, err)
		}

		m.State = StateLostOnRestore
		finished := time.Now().UTC()
		m.FinishedAt = &finished
		m.Result = Result{Reason: "lost-on-restore"}
		if err := store.Save(m); err != nil {
			t.Fatalf("Save(lost_on_restore, pid=%d) err = %v, want nil regardless of os content", pid, err)
		}

		got, err := store.Load(h)
		if err != nil {
			t.Fatalf("Load() err = %v", err)
		}
		if got.State != StateLostOnRestore {
			t.Errorf("State = %v, want %v", got.State, StateLostOnRestore)
		}
	}
}

func TestManifestOSMetadataZeroByDefault(t *testing.T) {
	t.Parallel()
	m := baseManifest(t, testHandle(t, 1))
	if m.os != (osMetadata{}) {
		t.Errorf("default os metadata = %+v, want zero value", m.os)
	}
}

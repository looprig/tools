package filemutation

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/looprig/tools/internal/workspace"
)

// hostwrites_test.go covers WithHostWrites(): the write-side counterpart to
// readfile/grep/glob's WithHostReads(). It pins the eight behaviors the option
// must have at the prepare/resolve layer (commit-time behavior is later work):
//
//  1. Without the option, legacy resolution is byte-for-byte unchanged: a
//     relative "../" escape is still hard-rejected with the historical error
//     message, and an absolute input is still silently anchored under root
//     (never rejected at prepare time -- see the note below).
//  2. With the option, an out-of-workspace absolute target prepares successfully.
//  3. With the option, an absolute target's lexical form is the CLEANED INPUT,
//     never workspace.JoinedPath(root, input) (the double-join bug class).
//  4. With the option, a relative "../" escape is still hard-rejected, never
//     widened.
//  5. An uncontained requirement's Candidates is nil (never a persistable
//     standing-allow candidate for a host write).
//  6. An uncontained requirement's Description is create-vs-overwrite aware.
//  7. A contained requirement (workspace-relative input) is byte-for-byte
//     unchanged whether or not the option is set.
//  8. The run-time stage-1 recheck (enforceApprovedResolution) refuses an
//     approved uncontained target whose resolution changed since approval.

// resolvedAbsHost resolves input against root exactly as resolveMutationTarget
// does for a host (uncontained) target, giving tests the canonical abs to
// compare against without hardcoding platform-specific symlink resolution
// (e.g. macOS's /tmp -> /private/tmp).
func resolvedAbsHost(t *testing.T, root, input string) string {
	t.Helper()
	abs, _, err := workspace.ResolvedPath(root, input)
	if err != nil {
		t.Fatalf("workspace.ResolvedPath() error = %v", err)
	}
	return abs
}

// --- 1: regression pin -- legacy (no-option) behavior is unchanged ---
//
// workspace.ContainedPath (the legacy, hostWrites=false path) does NOT reject
// an absolute input outright: per its own doc comment, an absolute input is
// "NOT honoured as absolute" -- it is Join'd (and therefore silently
// ANCHORED) under root rather than rejected, since Join can never produce an
// escape for an already-under-root result. This is pre-existing behavior,
// unrelated to this task, and the exact same fixture in the read-side
// reference test (readfile_hostreads_test.go's
// TestReadFileWithoutHostReadsRejectsOutsideWorkspace) confirms it: that test
// expects "error: file not found" -- a COMMIT-time miss at the wrong anchored
// location -- not a PREPARE-time rejection. The one input that legitimately
// produces the historical "path is outside the workspace" message, with or
// without the option, is a RELATIVE "../" escape (hasParentEscape). So the
// regression pin below is two-part: (a) a relative escape is rejected with
// the exact historical message, unchanged; (b) an absolute input is NOT
// rejected at prepare time -- it is silently anchored under root, exactly as
// before -- documented explicitly so a future change to that quirk is caught.

func TestWriteFileWithoutHostWritesRejectsRelativeEscape(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	w := NewWriteFile(root, newFileObservations())

	got := prepareRun(context.Background(), t, w, mustJSON(t, map[string]any{"path": "../sibling-secret.txt", "content": "x"}))
	want := "error: tool preparation failed: path is outside the workspace"
	if got != want {
		t.Errorf("WriteFile(../ escape) without WithHostWrites() = %q, want %q", got, want)
	}
}

func TestEditFileWithoutHostWritesRejectsRelativeEscape(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	e := NewEditFile(root, newFileObservations())

	got := prepareRun(context.Background(), t, e, mustJSON(t, map[string]any{"path": "../sibling-secret.txt", "old": "a", "new": "b"}))
	want := "error: tool preparation failed: path is outside the workspace"
	if got != want {
		t.Errorf("EditFile(../ escape) without WithHostWrites() = %q, want %q", got, want)
	}
}

func TestWriteFilePrepareCallWithoutHostWritesAnchorsAbsoluteInputUnderRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "host.txt")
	w := NewWriteFile(root, newFileObservations())

	_, _, err := w.PrepareCall(context.Background(), mustUUID(t), mustJSON(t, map[string]any{"path": outside, "content": "x"}))
	if err != nil {
		t.Errorf("PrepareCall(%q) without WithHostWrites() error = %v, want nil (legacy containedPath anchors an absolute input under root instead of rejecting it)", outside, err)
	}
}

func TestEditFilePrepareCallWithoutHostWritesAnchorsAbsoluteInputUnderRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "host.txt")
	e := NewEditFile(root, newFileObservations())

	_, _, err := e.PrepareCall(context.Background(), mustUUID(t), mustJSON(t, map[string]any{"path": outside, "old": "a", "new": "b"}))
	if err != nil {
		t.Errorf("PrepareCall(%q) without WithHostWrites() error = %v, want nil (legacy containedPath anchors an absolute input under root instead of rejecting it)", outside, err)
	}
}

// --- 2: WithHostWrites() allows an uncontained absolute target to prepare ---

func TestWriteFileWithHostWritesAllowsOutsideWorkspace(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "host.txt")
	w := NewWriteFile(root, newFileObservations(), WithHostWrites())

	_, _, err := w.PrepareCall(context.Background(), mustUUID(t), mustJSON(t, map[string]any{"path": outside, "content": "x"}))
	if err != nil {
		t.Errorf("WriteFile(%q) with WithHostWrites() PrepareCall() error = %v, want nil", outside, err)
	}
}

func TestEditFileWithHostWritesAllowsOutsideWorkspace(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "host.txt")
	e := NewEditFile(root, newFileObservations(), WithHostWrites())

	_, _, err := e.PrepareCall(context.Background(), mustUUID(t), mustJSON(t, map[string]any{"path": outside, "old": "a", "new": "b"}))
	if err != nil {
		t.Errorf("EditFile(%q) with WithHostWrites() PrepareCall() error = %v, want nil", outside, err)
	}
}

// --- 3: absolute uncontained lexical must be the cleaned input, never double-joined ---

func TestResolveMutationTargetHostWritesAbsoluteLexicalIsCleanedInputNotJoined(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	input := filepath.Join(t.TempDir(), "host.txt")

	target, err := resolveMutationTarget(root, input, true)
	if err != nil {
		t.Fatalf("resolveMutationTarget() error = %v", err)
	}
	if target.contained {
		t.Fatalf("target.contained = true, want false (input is outside root)")
	}
	wantLexical := filepath.Clean(input)
	if target.lexical != wantLexical {
		t.Errorf("target.lexical = %q, want %q (filepath.Clean(input))", target.lexical, wantLexical)
	}
	if joined := workspace.JoinedPath(root, input); target.lexical == joined {
		t.Errorf("target.lexical = %q must NOT equal workspace.JoinedPath(root, input) = %q (double-join bug)", target.lexical, joined)
	}
}

// --- 4: relative "../" escape is never widened, even with the option ---

func TestWriteFileWithHostWritesRelativeEscapeStillRejected(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	w := NewWriteFile(root, newFileObservations(), WithHostWrites())

	got := prepareRun(context.Background(), t, w, mustJSON(t, map[string]any{"path": "../sibling-secret.txt", "content": "x"}))
	want := "error: tool preparation failed: path is outside the workspace"
	if got != want {
		t.Errorf("WriteFile(../ escape) with WithHostWrites() = %q, want %q", got, want)
	}
}

func TestEditFileWithHostWritesRelativeEscapeStillRejected(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	e := NewEditFile(root, newFileObservations(), WithHostWrites())

	got := prepareRun(context.Background(), t, e, mustJSON(t, map[string]any{"path": "../sibling-secret.txt", "old": "a", "new": "b"}))
	want := "error: tool preparation failed: path is outside the workspace"
	if got != want {
		t.Errorf("EditFile(../ escape) with WithHostWrites() = %q, want %q", got, want)
	}
}

// --- 5: uncontained requirement never carries a persistable candidate ---

func TestWriteFilePrepareCallUncontainedRequirementHasNilCandidates(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "host.txt")
	w := NewWriteFile(root, newFileObservations(), WithHostWrites())

	req, _, err := w.PrepareCall(context.Background(), mustUUID(t), mustJSON(t, map[string]any{"path": outside, "content": "x"}))
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	if len(req.Requirements) != 1 {
		t.Fatalf("Requirements = %d, want 1", len(req.Requirements))
	}
	if req.Requirements[0].Candidates != nil {
		t.Errorf("Candidates = %+v, want nil -- an uncontained host-write requirement must never offer a persistable standing-allow candidate", req.Requirements[0].Candidates)
	}
}

func TestEditFilePrepareCallUncontainedRequirementHasNilCandidates(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "host.txt")
	e := NewEditFile(root, newFileObservations(), WithHostWrites())

	req, _, err := e.PrepareCall(context.Background(), mustUUID(t), mustJSON(t, map[string]any{"path": outside, "old": "a", "new": "b"}))
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	if len(req.Requirements) != 1 {
		t.Fatalf("Requirements = %d, want 1", len(req.Requirements))
	}
	if req.Requirements[0].Candidates != nil {
		t.Errorf("Candidates = %+v, want nil -- an uncontained host-write requirement must never offer a persistable standing-allow candidate", req.Requirements[0].Candidates)
	}
}

// --- 6: uncontained Description is create-vs-overwrite aware ---

func TestWriteFilePrepareCallUncontainedRequirementDescriptionCreateVsOverwrite(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	hostDir := t.TempDir()
	absent := filepath.Join(hostDir, "absent.txt")
	existing := filepath.Join(hostDir, "existing.txt")
	if err := os.WriteFile(existing, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := NewWriteFile(root, newFileObservations(), WithHostWrites())

	reqAbsent, _, err := w.PrepareCall(context.Background(), mustUUID(t), mustJSON(t, map[string]any{"path": absent, "content": "x"}))
	if err != nil {
		t.Fatalf("PrepareCall(absent) error = %v", err)
	}
	wantAbsentDesc := "create new file outside the workspace: " + resolvedAbsHost(t, root, absent)
	if got := reqAbsent.Requirements[0].Description; got != wantAbsentDesc {
		t.Errorf("Description(absent target) = %q, want %q", got, wantAbsentDesc)
	}

	reqExisting, _, err := w.PrepareCall(context.Background(), mustUUID(t), mustJSON(t, map[string]any{"path": existing, "content": "y"}))
	if err != nil {
		t.Fatalf("PrepareCall(existing) error = %v", err)
	}
	wantExistingDesc := "overwrite existing file outside the workspace: " + resolvedAbsHost(t, root, existing)
	if got := reqExisting.Requirements[0].Description; got != wantExistingDesc {
		t.Errorf("Description(existing target) = %q, want %q", got, wantExistingDesc)
	}
}

func TestEditFilePrepareCallUncontainedRequirementDescriptionOverwrite(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	existing := filepath.Join(t.TempDir(), "existing.txt")
	if err := os.WriteFile(existing, []byte("alpha"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := NewEditFile(root, newFileObservations(), WithHostWrites())

	req, _, err := e.PrepareCall(context.Background(), mustUUID(t), mustJSON(t, map[string]any{"path": existing, "old": "alpha", "new": "beta"}))
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	wantDesc := "overwrite existing file outside the workspace: " + resolvedAbsHost(t, root, existing)
	if got := req.Requirements[0].Description; got != wantDesc {
		t.Errorf("Description(existing target) = %q, want %q", got, wantDesc)
	}
}

// --- 7: contained requirement is unchanged whether or not the option is set ---

func TestWriteFilePrepareCallContainedRequirementUnchangedWithHostWrites(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	w := NewWriteFile(root, newFileObservations(), WithHostWrites())

	req, _, err := w.PrepareCall(context.Background(), mustUUID(t), mustJSON(t, map[string]any{"path": "f.txt", "content": "hi"}))
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	wantAbs := resolvedJoin(t, root, "f.txt")
	r := req.Requirements[0]
	if r.Description != "write "+wantAbs {
		t.Errorf("Description = %q, want %q", r.Description, "write "+wantAbs)
	}
	if len(r.Candidates) != 1 || r.Candidates[0].Match != wantAbs || r.Candidates[0].Kind != r.Kind {
		t.Errorf("Candidates = %+v, want one exact-path candidate for %q", r.Candidates, wantAbs)
	}
}

// --- 8: run-time stage-1 recheck refuses a changed resolution for a host target ---

func TestEnforceApprovedResolutionHostWritesRefusesChangedResolution(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	hostDir := t.TempDir()
	d := filepath.Join(hostDir, "d")
	e := filepath.Join(hostDir, "e")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(e, 0o755); err != nil {
		t.Fatal(err)
	}
	display := filepath.Join(d, "f.txt")

	target, err := resolveMutationTarget(root, display, true)
	if err != nil {
		t.Fatalf("resolveMutationTarget() error = %v", err)
	}
	if target.contained {
		t.Fatalf("target.contained = true, want false")
	}

	// Swap d -> symlink to e AFTER "approval".
	if err := os.RemoveAll(d); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(e, d); err != nil {
		t.Fatal(err)
	}

	if err := enforceApprovedResolution(root, target, true); err == nil {
		t.Fatalf("enforceApprovedResolution() error = nil, want refusal after resolution changed")
	}
}

package filemutation

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/internal/workspace"
	"github.com/looprig/tools/permission"
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
//
// It also pins commit-time behavior for an uncontained target (later work,
// same product decision as WithHostReads(): a prior host read/write must never
// authorize a later host write/read in either direction):
//
//  9. An uncontained CREATE succeeds with NO prior observation, and records
//     none afterward.
// 10. An uncontained OVERWRITE of an existing file succeeds with NO prior
//     observation (no StaleFileError), and records none afterward.
// 11. An uncontained target whose parent directory is missing fails with a
//     typed error and does NOT create the directory.
// 12. An uncontained symlink target is still refused with IrregularFileError.
// 13. Info()'s description swaps to the host-writes-aware wording when the
//     option is set.
//
// EditFile's freshness mechanism differs from WriteFile's (it already reads
// the file fresh at commit time via readForEdit; the recorded observation
// hash is only ever a COMPARATOR, never the source of the edited bytes), so
// its commit-time behaviors 9-13 above are pinned separately below (see "---
// EditFile commit-time host-write behavior ---"), plus two EditFile-only
// behaviors:
//
// 14. EditFile's PrepareCall additionally carries a paired filesystem.read
//     requirement (Candidates nil) for an UNCONTAINED target only; a
//     contained target still gets exactly one requirement.
// 15. Skipping the freshness comparator for an uncontained target does NOT
//     weaken anchor enforcement for replace_all=false: a WRONG `old` anchor
//     still fails with the distinct editAnchorError, never a silent
//     wrong-edit.
// 16. For replace_all=true, the anchor match is WEAKER protection: it only
//     confirms `old` exists somewhere, not that the occurrence set is
//     unchanged. An uncontained target whose on-disk occurrence count
//     drifted since a hypothetical prior read is silently over-replaced --
//     an accepted, intentional residual risk (a contained target's CAS check
//     would catch this drift; see commitUncontained's doc comment).

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
	// Unlike WriteFile, an uncontained EditFile target also carries a paired
	// filesystem.read requirement (EditFile performs an in-process read via
	// readForEdit before writing back) -- see
	// TestEditFilePrepareCallPairedReadRequirementUncontainedOnly for the
	// dedicated pin on that second requirement's shape. This test stays
	// focused on the WRITE requirement's Candidates.
	if len(req.Requirements) != 2 {
		t.Fatalf("Requirements = %d, want 2 (write + paired read)", len(req.Requirements))
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

// --- 7b: an ABSOLUTE input that still resolves INSIDE the workspace stays
// contained even with WithHostWrites() enabled -- proving it is
// target.contained (not merely "the option is on") that gates Candidates and
// Description, mirroring the read side's
// TestReadFileWithHostReadsInWorkspaceAbsolutePathStillObserves
// (readfile/readfile_hostreads_test.go). ---

func TestWriteFilePrepareCallHostWritesAbsoluteInWorkspacePathStillContained(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	inside := filepath.Join(root, "f.txt")
	w := NewWriteFile(root, newFileObservations(), WithHostWrites())

	req, _, err := w.PrepareCall(context.Background(), mustUUID(t), mustJSON(t, map[string]any{"path": inside, "content": "hi"}))
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

func TestEditFilePrepareCallHostWritesAbsoluteInWorkspacePathStillContained(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	inside := filepath.Join(root, "f.txt")
	if err := os.WriteFile(inside, []byte("alpha"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := NewEditFile(root, newFileObservations(), WithHostWrites())

	req, _, err := e.PrepareCall(context.Background(), mustUUID(t), mustJSON(t, map[string]any{"path": inside, "old": "alpha", "new": "beta"}))
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

// TestEnforceApprovedResolutionHostWritesRefusesUnresolvableTarget exercises
// enforceApprovedResolution's OTHER failure branch: RE-RESOLUTION itself
// failing outright (a bare non-nil err from resolveMutationTarget, returned
// as-is) rather than resolving to a DIFFERENT valid target (the
// "abs != target.abs" comparison branch already covered above). A
// self-referential symlink swapped in after approval makes the display path
// unresolvable (ELOOP), which must surface as "path could not be resolved"
// (resolveMutationTarget's absolute-input resolution-failure reason) --
// distinct from "path resolution changed since approval".
func TestEnforceApprovedResolutionHostWritesRefusesUnresolvableTarget(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	hostDir := t.TempDir()
	a := filepath.Join(hostDir, "a")
	if err := os.MkdirAll(a, 0o755); err != nil {
		t.Fatal(err)
	}
	display := filepath.Join(a, "f.txt")

	target, err := resolveMutationTarget(root, display, true)
	if err != nil {
		t.Fatalf("resolveMutationTarget() error = %v", err)
	}
	if target.contained {
		t.Fatalf("target.contained = true, want false")
	}

	// Swap "a" -> a SELF-REFERENTIAL symlink AFTER "approval": resolving
	// display now hits ELOOP instead of landing on any valid (even if
	// different) target.
	if err := os.RemoveAll(a); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(a, a); err != nil {
		t.Fatal(err)
	}

	err = enforceApprovedResolution(root, target, true)
	if err == nil {
		t.Fatalf("enforceApprovedResolution() error = nil, want refusal for an unresolvable target")
	}
	wfe, ok := err.(*writeFileError)
	if !ok {
		t.Fatalf("error type = %T, want *writeFileError", err)
	}
	if wfe.reason != "path could not be resolved" {
		t.Errorf("reason = %q, want %q (the bare-error-return branch, not the changed-resolution branch)", wfe.reason, "path could not be resolved")
	}
}

// --- 9-12: commit-time behavior for an UNCONTAINED (host) target ---
//
// The product decision: a prior host READ must never authorize a later host
// WRITE, and vice versa (the same decision WithHostReads() already enforces on
// the read side by never recording an observation for an uncontained read).
// So for an uncontained target, WriteFile.commit must skip BOTH the
// observation check before an overwrite AND recording an observation after a
// successful write -- in either direction, never read from or write to the
// observation map for an uncontained target.

// recordedObservation returns the observation obs currently holds for path,
// mirroring the inspection idiom used by
// readfile_hostreads_test.go's TestReadFileWithHostReadsDoesNotRecordObservationForUncontainedRead.
func recordedObservation(t *testing.T, obs *fileObservations, path string) tool.FileObservation {
	t.Helper()
	var recorded tool.FileObservation
	if err := obs.WithPath(path, func(o *tool.FileObservation) error {
		recorded = *o
		return nil
	}); err != nil {
		t.Fatalf("obs.WithPath: %v", err)
	}
	return recorded
}

// TestWriteFileHostWritesCreateSucceedsWithoutPriorObservationAndRecordsNone
// pins behavior 9: an uncontained CREATE (an absent host target) succeeds with
// NO prior observation, and after success no observation was recorded for it
// either -- a host write must never feed the same-loop write-authorization map.
func TestWriteFileHostWritesCreateSucceedsWithoutPriorObservationAndRecordsNone(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	hostDir := t.TempDir()
	target := filepath.Join(hostDir, "new.txt")
	obs := newFileObservations()
	w := NewWriteFile(root, obs, WithHostWrites())

	out := prepareRun(context.Background(), t, w, mustJSON(t, map[string]any{"path": target, "content": "hello"}))
	if strings.HasPrefix(out, "error:") {
		t.Fatalf("uncontained create = %q, want success", out)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read created file: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("on-disk body = %q, want %q", got, "hello")
	}

	abs := resolvedAbsHost(t, root, target)
	if recorded := recordedObservation(t, obs, abs); recorded.Observed {
		t.Errorf("uncontained create recorded an observation %+v, want none", recorded)
	}
}

// TestWriteFileHostWritesOverwritesExistingWithoutPriorObservationAndRecordsNone
// pins behavior 10, the key behavior change: overwriting an EXISTING
// uncontained target succeeds with NO prior observation and no StaleFileError
// -- for a CONTAINED target the identical setup (an existing file, no prior
// read) is refused (see TestWriteFreshnessGate's "overwrite without any
// observation is refused" and writefile_test.go's "unobserved existing file is
// rejected without clobbering"). After success, no observation is recorded
// either.
func TestWriteFileHostWritesOverwritesExistingWithoutPriorObservationAndRecordsNone(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	hostDir := t.TempDir()
	target := filepath.Join(hostDir, "existing.txt")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	obs := newFileObservations()
	w := NewWriteFile(root, obs, WithHostWrites())

	out := prepareRun(context.Background(), t, w, mustJSON(t, map[string]any{"path": target, "content": "new"}))
	if strings.HasPrefix(out, "error:") {
		t.Fatalf("uncontained overwrite without prior observation = %q, want success (no StaleFileError)", out)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read overwritten file: %v", err)
	}
	if string(got) != "new" {
		t.Errorf("on-disk body = %q, want %q", got, "new")
	}

	abs := resolvedAbsHost(t, root, target)
	if recorded := recordedObservation(t, obs, abs); recorded.Observed {
		t.Errorf("uncontained overwrite recorded an observation %+v, want none", recorded)
	}
}

// TestWriteFileHostWritesMissingParentDirFailsWithoutCreatingIt pins behavior
// 11: an uncontained CREATE whose parent directory does not exist fails with a
// typed error and, critically, does NOT manufacture the missing directory
// chain (stageTempFile's MkdirAll would silently do so for a typo'd host
// path the approval never named -- this pre-flight check exists specifically
// to prevent that for uncontained targets).
func TestWriteFileHostWritesMissingParentDirFailsWithoutCreatingIt(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	hostDir := t.TempDir()
	missingParent := filepath.Join(hostDir, "nope")
	target := filepath.Join(missingParent, "new.txt")
	obs := newFileObservations()
	w := NewWriteFile(root, obs, WithHostWrites())

	out := prepareRun(context.Background(), t, w, mustJSON(t, map[string]any{"path": target, "content": "x"}))
	if !strings.HasPrefix(out, "error:") {
		t.Fatalf("uncontained create with missing parent dir = %q, want an error", out)
	}
	if _, err := os.Stat(missingParent); !os.IsNotExist(err) {
		t.Fatalf("missing parent dir %q was created (stat err=%v), want it left absent", missingParent, err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("target %q exists, want no file written", target)
	}
}

// TestWriteFileHostWritesIrregularTargetRefused pins behavior 12: an
// uncontained target whose final component is a symlink is still refused with
// *IrregularFileError, mirroring the contained-side
// TestWriteFileUnobservedSymlinkRejected/TestIrregularWriteTargetIsTyped.
// Neither the symlink node nor its target is touched.
func TestWriteFileHostWritesIrregularTargetRefused(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	hostDir := t.TempDir()
	const targetBody = "ORIGINAL TARGET BODY"
	real := filepath.Join(hostDir, "real.txt")
	if err := os.WriteFile(real, []byte(targetBody), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(hostDir, "link.txt")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	obs := newFileObservations()
	w := NewWriteFile(root, obs, WithHostWrites())

	out := prepareRun(context.Background(), t, w, mustJSON(t, map[string]any{"path": link, "content": "NEW"}))
	if !strings.HasPrefix(out, "error:") {
		t.Fatalf("uncontained write to a symlink target = %q, want a fail-secure rejection", out)
	}
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat link: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link.txt is no longer a symlink; the rejected write mutated it")
	}
	gotTarget, err := os.ReadFile(real)
	if err != nil {
		t.Fatalf("read real target: %v", err)
	}
	if string(gotTarget) != targetBody {
		t.Fatalf("symlink target was clobbered: %q, want %q", gotTarget, targetBody)
	}
}

// --- 13: Info() reflects host-write mode ---

// TestWriteFileInfoHostWritesDesc pins behavior 13, mirroring readfile.go's
// readFileDesc/readFileHostReadsDesc swap: with WithHostWrites() set, Info()'s
// Desc must differ from the default writeFileDesc and must convey that an
// absolute path may resolve outside the workspace and that such writes are
// NOT covered by session checkpoint/undo.
func TestWriteFileInfoHostWritesDesc(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	def, err := NewWriteFile(root, newFileObservations()).Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	if def.Desc != writeFileDesc {
		t.Errorf("Info().Desc without WithHostWrites() = %q, want the unchanged default %q", def.Desc, writeFileDesc)
	}

	host, err := NewWriteFile(root, newFileObservations(), WithHostWrites()).Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	if host.Desc == writeFileDesc {
		t.Errorf("Info().Desc with WithHostWrites() = %q, want a distinct host-writes-aware description", host.Desc)
	}
	if !strings.Contains(host.Desc, "outside the workspace") {
		t.Errorf("Info().Desc = %q, want it to mention resolving outside the workspace", host.Desc)
	}
	if !strings.Contains(host.Desc, "checkpoint") {
		t.Errorf("Info().Desc = %q, want it to mention writes outside the workspace are not covered by checkpoint/undo", host.Desc)
	}
}

// --- EditFile commit-time host-write behavior ---
//
// EditFile's freshness mechanism differs from WriteFile's: EditFile ALREADY
// reads the current file fresh at commit time (readForEdit), and the
// recorded observation hash is used only as a COMPARATOR against that fresh
// read -- never as the source of the bytes being edited. So for an
// uncontained target, EditFile.commit skips ONLY the
// obs.Observed/obs.Present/obs.Hash comparison; the fresh read, the exact
// `old`-substring anchor match (applyReplacement), the atomic write-back,
// and the diff preview all stay identical to the contained path. The anchor
// match is itself real, independent freshness protection: an `old` that no
// longer matches exactly once fails loudly with a distinct editAnchorError,
// never a silent wrong-edit.

// TestEditFileHostWritesSucceedsWithoutPriorObservationAndRecordsNone pins
// pinned behavior 1: an uncontained edit of an EXISTING host file succeeds
// with NO prior observation (no StaleFileError -- proving the comparator is
// genuinely skipped, not merely permissive), and after success no
// observation was recorded for it either -- a host edit must never feed the
// same-loop write-authorization map, mirroring WriteFile's
// TestWriteFileHostWritesOverwritesExistingWithoutPriorObservationAndRecordsNone.
func TestEditFileHostWritesSucceedsWithoutPriorObservationAndRecordsNone(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	hostDir := t.TempDir()
	target := filepath.Join(hostDir, "existing.txt")
	if err := os.WriteFile(target, []byte("alpha bravo charlie\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	obs := newFileObservations()
	e := NewEditFile(root, obs, WithHostWrites())

	out := prepareRun(context.Background(), t, e, mustJSON(t, map[string]any{"path": target, "old": "bravo", "new": "BRAVO"}))
	if strings.HasPrefix(out, "error:") {
		t.Fatalf("uncontained edit without prior observation = %q, want success (no StaleFileError)", out)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read edited file: %v", err)
	}
	if string(got) != "alpha BRAVO charlie\n" {
		t.Errorf("on-disk body = %q, want %q", got, "alpha BRAVO charlie\n")
	}

	abs := resolvedAbsHost(t, root, target)
	if recorded := recordedObservation(t, obs, abs); recorded.Observed {
		t.Errorf("uncontained edit recorded an observation %+v, want none", recorded)
	}
}

// TestEditFileHostWritesWrongAnchorStillFails pins pinned behavior 2: even
// with the freshness comparator skipped for an uncontained target, a WRONG
// `old` anchor still fails with the distinct anchor error (not silently
// applied, and not masquerading as a StaleFileError) -- proving the anchor
// match is real, independent freshness protection, not bypassed alongside
// the CAS skip.
func TestEditFileHostWritesWrongAnchorStillFails(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	hostDir := t.TempDir()
	target := filepath.Join(hostDir, "existing.txt")
	if err := os.WriteFile(target, []byte("alpha bravo charlie\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := NewEditFile(root, newFileObservations(), WithHostWrites())

	out := prepareRun(context.Background(), t, e, mustJSON(t, map[string]any{"path": target, "old": "zulu", "new": "X"}))
	if !strings.HasPrefix(out, "error:") {
		t.Fatalf("uncontained edit with a non-matching anchor = %q, want an error", out)
	}
	if !strings.Contains(out, "not found in the file") {
		t.Errorf("result = %q, want the anchor error (%q), not a freshness error", out, "not found in the file")
	}
	if strings.Contains(out, "must be read before writing") {
		t.Errorf("result = %q, must NOT be the StaleFileError message -- the anchor mismatch is a distinct failure", out)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(got) != "alpha bravo charlie\n" {
		t.Errorf("on-disk body = %q, want it left untouched by the rejected edit", got)
	}
}

// TestEditFileHostWritesReplaceAllOverReplacesDriftedOccurrences makes explicit
// and regression-proof the residual risk documented on commitUncontained: for
// replace_all=true, applyReplacement only requires `old` to occur AT LEAST
// ONCE and then replaces EVERY occurrence -- it confirms `old` still exists
// somewhere, not that the occurrence SET is unchanged from whatever the model
// last saw. This test simulates a model that looked at the file when it held
// exactly one occurrence of "old", but by the time the uncontained edit
// commits the file has DRIFTED on disk to hold a second occurrence the model
// never saw. Because an uncontained target skips the CAS check entirely (see
// commitUncontained), the edit proceeds and silently replaces BOTH
// occurrences, with no error. A contained target does not have this gap: its
// CAS check requires a fresh full-file read matching the exact current
// on-disk hash before authorizing ANY edit, so this drift would be caught
// before applyReplacement ever ran. This is the accepted, intentional
// residual risk of skipping the freshness comparator for host targets -- this
// test exists to keep that risk visible and testable, not to change it.
func TestEditFileHostWritesReplaceAllOverReplacesDriftedOccurrences(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	hostDir := t.TempDir()
	target := filepath.Join(hostDir, "drift.txt")

	// The model is presumed to have seen a file with a single occurrence of
	// "old" (e.g. "alpha old charlie\n"). By commit time the on-disk file has
	// drifted to hold a SECOND, unrelated occurrence the model never observed.
	if err := os.WriteFile(target, []byte("alpha old charlie\ndelta old echo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := NewEditFile(root, newFileObservations(), WithHostWrites())

	out := prepareRun(context.Background(), t, e, mustJSON(t, map[string]any{"path": target, "old": "old", "new": "NEW", "replace_all": true}))
	if strings.HasPrefix(out, "error:") {
		t.Fatalf("uncontained replace_all edit = %q, want success (this is the accepted residual risk, not a refusal)", out)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read edited file: %v", err)
	}
	want := "alpha NEW charlie\ndelta NEW echo\n"
	if string(got) != want {
		t.Errorf("on-disk body = %q, want %q -- both occurrences replaced, including the one that drifted in after the model's presumed read", got, want)
	}
}

// TestEditFilePrepareCallPairedReadRequirementUncontainedOnly pins pinned
// behavior 4: an uncontained PrepareCall carries a SECOND requirement of
// kind filesystem.read, for the SAME canonical path, with nil Candidates
// (the same reason writeRequirement suppresses Candidates for an uncontained
// write -- a persisted "approve always" read rule for a host path would
// silently authorize every future read there). A contained target still
// gets exactly one requirement (write only), never a paired read.
func TestEditFilePrepareCallPairedReadRequirementUncontainedOnly(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// Uncontained: two requirements -- write (Candidates nil, already pinned by
	// TestEditFilePrepareCallUncontainedRequirementHasNilCandidates) plus a
	// paired read for the SAME path, also with nil Candidates.
	hostDir := t.TempDir()
	outside := filepath.Join(hostDir, "host.txt")
	if err := os.WriteFile(outside, []byte("alpha"), 0o600); err != nil {
		t.Fatal(err)
	}
	eHost := NewEditFile(root, newFileObservations(), WithHostWrites())
	reqHost, _, err := eHost.PrepareCall(context.Background(), mustUUID(t), mustJSON(t, map[string]any{"path": outside, "old": "alpha", "new": "beta"}))
	if err != nil {
		t.Fatalf("PrepareCall(uncontained) error = %v", err)
	}
	if len(reqHost.Requirements) != 2 {
		t.Fatalf("Requirements = %d, want 2 (write + paired read) for an uncontained EditFile target", len(reqHost.Requirements))
	}
	wantAbs := resolvedAbsHost(t, root, outside)
	readReq := reqHost.Requirements[1]
	if readReq.Kind != permission.CapabilityFilesystemRead {
		t.Errorf("Requirements[1].Kind = %q, want %q", readReq.Kind, permission.CapabilityFilesystemRead)
	}
	if readReq.Scope != wantAbs || readReq.Match != wantAbs {
		t.Errorf("Requirements[1] Scope/Match = %q/%q, want both %q", readReq.Scope, readReq.Match, wantAbs)
	}
	if readReq.Candidates != nil {
		t.Errorf("Requirements[1].Candidates = %+v, want nil -- a persisted host-read candidate would silently authorize every future read here", readReq.Candidates)
	}

	// Contained: still exactly one requirement (write only), never a paired read.
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("alpha"), 0o600); err != nil {
		t.Fatal(err)
	}
	eContained := NewEditFile(root, newFileObservations())
	reqContained, _, err := eContained.PrepareCall(context.Background(), mustUUID(t), mustJSON(t, map[string]any{"path": "f.txt", "old": "alpha", "new": "beta"}))
	if err != nil {
		t.Fatalf("PrepareCall(contained) error = %v", err)
	}
	if len(reqContained.Requirements) != 1 {
		t.Fatalf("Requirements = %d, want 1 for a contained EditFile target (never a paired read)", len(reqContained.Requirements))
	}
}

// TestEditFileHostWritesIrregularTargetRefused pins pinned behavior 5: an
// uncontained target whose final component is a symlink is still refused
// with *IrregularFileError, mirroring the contained-side
// TestIrregularWriteTargetIsTyped and WriteFile's
// TestWriteFileHostWritesIrregularTargetRefused. Neither the symlink node
// nor its target is touched.
func TestEditFileHostWritesIrregularTargetRefused(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	hostDir := t.TempDir()
	const targetBody = "ORIGINAL TARGET BODY"
	real := filepath.Join(hostDir, "real.txt")
	if err := os.WriteFile(real, []byte(targetBody), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(hostDir, "link.txt")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	e := NewEditFile(root, newFileObservations(), WithHostWrites())

	out := prepareRun(context.Background(), t, e, mustJSON(t, map[string]any{"path": link, "old": "ORIGINAL", "new": "NEW"}))
	if !strings.HasPrefix(out, "error:") {
		t.Fatalf("uncontained edit of a symlink target = %q, want a fail-secure rejection", out)
	}
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat link: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link.txt is no longer a symlink; the rejected edit mutated it")
	}
	gotTarget, err := os.ReadFile(real)
	if err != nil {
		t.Fatalf("read real target: %v", err)
	}
	if string(gotTarget) != targetBody {
		t.Fatalf("symlink target was clobbered: %q, want %q", gotTarget, targetBody)
	}
}

// TestEditFileInfoHostWritesDesc mirrors
// TestWriteFileInfoHostWritesDesc/readfile.go's readFileDesc split: with
// WithHostWrites() set, Info()'s Desc must differ from the default
// editFileDesc and must convey that an absolute path may resolve outside the
// workspace and that such edits are NOT covered by session checkpoint/undo.
func TestEditFileInfoHostWritesDesc(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	def, err := NewEditFile(root, newFileObservations()).Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	if def.Desc != editFileDesc {
		t.Errorf("Info().Desc without WithHostWrites() = %q, want the unchanged default %q", def.Desc, editFileDesc)
	}

	host, err := NewEditFile(root, newFileObservations(), WithHostWrites()).Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	if host.Desc == editFileDesc {
		t.Errorf("Info().Desc with WithHostWrites() = %q, want a distinct host-writes-aware description", host.Desc)
	}
	if !strings.Contains(host.Desc, "outside the workspace") {
		t.Errorf("Info().Desc = %q, want it to mention resolving outside the workspace", host.Desc)
	}
	if !strings.Contains(host.Desc, "checkpoint") {
		t.Errorf("Info().Desc = %q, want it to mention edits outside the workspace are not covered by checkpoint/undo", host.Desc)
	}
}

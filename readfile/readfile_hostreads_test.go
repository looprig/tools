package readfile

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

// runReadFileOpts is runReadFile plus the ability to pass ReadFileOptions and
// get back the WorkspaceObservations the call recorded into, so a test can
// inspect what a read did or did not record.
func runReadFileOpts(t *testing.T, root string, guard loop.ReadGuard, args map[string]any, opts ...ReadFileOption) (string, tool.WorkspaceObservations) {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	obs := tool.NewWorkspaceObservations()
	rf := NewReadFile(root, guard, obs, opts...)
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() error = %v", err)
	}
	req, art, err := rf.PrepareCall(context.Background(), id, string(b))
	if err != nil {
		return "error: tool preparation failed: " + err.Error(), obs
	}
	ctx := loop.WithPreparedCall(context.Background(), tool.PreparedCall{ExecutionID: id, Request: req, Artifact: art})
	res, err := rf.InvokableRun(ctx, string(b))
	if err != nil {
		t.Fatalf("InvokableRun returned a Go error %v; read tools return tool-result strings", err)
	}
	return resultText(t, res), obs
}

// TestReadFileWithoutHostReadsRejectsOutsideWorkspace pins today's (legacy)
// behavior unchanged: with no WithHostReads() option, an absolute path is
// lexically re-anchored UNDER root by workspace.ContainedPath's Join (it is
// never honoured as absolute, and this never escapes root the way a "../"
// climb can), so it resolves to a nonsensical nested path that does not exist
// on disk and fails at open time with "file not found" -- NOT a containment
// rejection. This is the pre-existing quirk WithHostReads() exists to fix;
// this test only pins that WithHostReads() being absent changes nothing.
func TestReadFileWithoutHostReadsRejectsOutsideWorkspace(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "host.txt")
	mustWrite(t, outside, "secret\n")

	got, _ := runReadFileOpts(t, root, newFakeReadGuard(1<<20), map[string]any{"path": outside})
	want := "error: file not found"
	if got != want {
		t.Errorf("ReadFile(%q) without WithHostReads = %q, want %q", outside, got, want)
	}
}

// TestReadFileWithHostReadsAllowsOutsideWorkspace proves WithHostReads() lets
// an absolute out-of-workspace path resolve and read successfully when the
// guard does not deny it.
func TestReadFileWithHostReadsAllowsOutsideWorkspace(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "host.txt")
	mustWrite(t, outside, "line one\nline two\n")

	got, _ := runReadFileOpts(t, root, newFakeReadGuard(1<<20), map[string]any{"path": outside}, WithHostReads())
	want := "   1\tline one\n   2\tline two"
	if got != want {
		t.Errorf("ReadFile(%q) with WithHostReads = %q, want %q", outside, got, want)
	}
}

// TestReadFileWithHostReadsStillHonoursDeniedRead proves the guard's denylist
// still applies to a widened host read — WithHostReads() only changes
// containment, not the ReadGuard.DeniedRead secret-path check.
func TestReadFileWithHostReadsStillHonoursDeniedRead(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "id_rsa")
	mustWrite(t, outside, "PRIVATE KEY\n")

	resolvedAbs := resolvedJoin(t, outsideDir, "id_rsa")
	guard := newFakeReadGuard(1<<20, resolvedAbs)

	got, _ := runReadFileOpts(t, root, guard, map[string]any{"path": outside}, WithHostReads())
	want := "error: read denied: " + outside
	if got != want {
		t.Errorf("ReadFile(%q) with WithHostReads and denied guard = %q, want %q", outside, got, want)
	}
}

// TestReadFileWithHostReadsRelativeEscapeStillRejected proves WithHostReads()
// does NOT widen relative "../" traversal -- only a literal absolute path
// argument gets the new treatment. This is the no-new-lexical-escape-surface
// guarantee.
func TestReadFileWithHostReadsRelativeEscapeStillRejected(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	parent := filepath.Dir(root)
	mustWrite(t, filepath.Join(parent, "sibling-secret.txt"), "x")

	got, _ := runReadFileOpts(t, root, newFakeReadGuard(1<<20), map[string]any{"path": "../sibling-secret.txt"}, WithHostReads())
	want := "error: tool preparation failed: path is outside the workspace"
	if got != want {
		t.Errorf("relative ../ escape with WithHostReads = %q, want %q", got, want)
	}
}

// TestReadFileWithHostReadsDoesNotRecordObservationForUncontainedRead proves a
// successful out-of-workspace read does not record any observation for that
// path -- a host read must never feed the same-loop write-authorization map
// (WriteFile/EditFile stay workspace-confined regardless of WithHostReads()).
func TestReadFileWithHostReadsDoesNotRecordObservationForUncontainedRead(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "host.txt")
	mustWrite(t, outside, "hi\n")

	_, obs := runReadFileOpts(t, root, newFakeReadGuard(1<<20), map[string]any{"path": outside}, WithHostReads())

	resolvedAbs := resolvedJoin(t, outsideDir, "host.txt")
	var recorded tool.FileObservation
	if err := obs.WithPath(resolvedAbs, func(o *tool.FileObservation) error {
		recorded = *o
		return nil
	}); err != nil {
		t.Fatalf("obs.WithPath: %v", err)
	}
	if recorded.Observed {
		t.Errorf("uncontained read recorded an observation %+v, want none", recorded)
	}
}

// TestReadFileWithHostReadsInWorkspaceAbsolutePathStillObserves proves the
// widened resolver does not regress in-workspace behavior: an absolute path
// that spells an in-workspace file still records its observation normally.
func TestReadFileWithHostReadsInWorkspaceAbsolutePathStillObserves(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	inside := filepath.Join(root, "a.txt")
	mustWrite(t, inside, "hi\n")

	_, obs := runReadFileOpts(t, root, newFakeReadGuard(1<<20), map[string]any{"path": inside}, WithHostReads())

	resolvedAbs := resolvedJoin(t, root, "a.txt")
	var recorded tool.FileObservation
	if err := obs.WithPath(resolvedAbs, func(o *tool.FileObservation) error {
		recorded = *o
		return nil
	}); err != nil {
		t.Fatalf("obs.WithPath: %v", err)
	}
	if !recorded.Observed || !recorded.Present {
		t.Errorf("in-workspace absolute-path read did not record observation, got %+v", recorded)
	}
}

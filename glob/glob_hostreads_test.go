package glob

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

// runGlobOpts is runGlob plus the ability to pass GlobOptions.
func runGlobOpts(t *testing.T, root string, guard *fakeReadGuard, args map[string]any, opts ...GlobOption) string {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	g := NewGlob(root, guard, opts...)
	id, uerr := uuid.New()
	if uerr != nil {
		t.Fatalf("uuid.New() error = %v", uerr)
	}
	req, art, perr := g.PrepareCall(context.Background(), id, string(b))
	if perr != nil {
		return "error: tool preparation failed: " + perr.Error()
	}
	ctx := loop.WithPreparedCall(context.Background(), tool.PreparedCall{ExecutionID: id, Request: req, Artifact: art})
	res, rerr := g.InvokableRun(ctx, string(b))
	if rerr != nil {
		t.Fatalf("InvokableRun returned a Go error %v; read tools return tool-result strings", rerr)
	}
	return textOfResult(t, res)
}

// TestGlobWithoutHostReadsRejectsOutsideWorkspace pins today's (legacy)
// behavior unchanged: with no WithHostReads() option, an absolute search root
// is lexically re-anchored under root by workspace.ContainedPath's Join
// (never honoured as absolute), so it walks a nonexistent nested directory
// and finds nothing -- not a containment rejection. This is the pre-existing
// quirk WithHostReads() exists to fix; this test only pins the option-absent
// case.
func TestGlobWithoutHostReadsRejectsOutsideWorkspace(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outsideDir := t.TempDir()
	mustWrite(t, filepath.Join(outsideDir, "host.txt"), "x")

	got := runGlobOpts(t, root, newFakeReadGuard(1<<20), map[string]any{"pattern": "*.txt", "root": outsideDir})
	if got != "no matches" {
		t.Errorf("Glob(%q) without WithHostReads = %q, want %q", outsideDir, got, "no matches")
	}
}

// TestGlobWithHostReadsSearchesOutsideWorkspace proves WithHostReads() lets an
// absolute out-of-workspace search root resolve and be walked, with results
// displayed relative to THAT searched directory.
func TestGlobWithHostReadsSearchesOutsideWorkspace(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outsideDir := t.TempDir()
	mustWrite(t, filepath.Join(outsideDir, "host.txt"), "x")

	got := runGlobOpts(t, root, newFakeReadGuard(1<<20), map[string]any{"pattern": "*.txt", "root": outsideDir}, WithHostReads())
	want := "host.txt"
	if got != want {
		t.Errorf("Glob(%q) with WithHostReads = %q, want %q", outsideDir, got, want)
	}
}

// TestGlobWithHostReadsStillHonoursDeniedRead proves the guard's denylist
// still applies to a widened host walk.
func TestGlobWithHostReadsStillHonoursDeniedRead(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outsideDir := t.TempDir()
	denied := filepath.Join(outsideDir, "id_rsa.txt")
	mustWrite(t, denied, "x")
	mustWrite(t, filepath.Join(outsideDir, "ok.txt"), "x")

	resolvedDenied := resolvedJoin(t, outsideDir, "id_rsa.txt")
	guard := newFakeReadGuard(1<<20, resolvedDenied)

	got := runGlobOpts(t, root, guard, map[string]any{"pattern": "*.txt", "root": outsideDir}, WithHostReads())
	want := "ok.txt"
	if got != want {
		t.Errorf("Glob(%q) with WithHostReads and a denied file = %q, want %q", outsideDir, got, want)
	}
}

// TestGlobWithHostReadsRelativeEscapeStillRejected proves WithHostReads()
// does NOT widen relative "../" traversal -- only a literal absolute path
// argument gets the new treatment.
func TestGlobWithHostReadsRelativeEscapeStillRejected(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	parent := filepath.Dir(root)
	mustWrite(t, filepath.Join(parent, "sibling.txt"), "x")

	got := runGlobOpts(t, root, newFakeReadGuard(1<<20), map[string]any{"pattern": "*.txt", "root": ".."}, WithHostReads())
	want := "error: tool preparation failed: search root is outside the workspace: .."
	if got != want {
		t.Errorf("relative .. escape with WithHostReads = %q, want %q", got, want)
	}
}

// TestGlobWithHostReadsInWorkspaceAbsolutePathStillWorks proves the widened
// resolver does not regress in-workspace behavior: an absolute path spelling
// an in-workspace directory still walks and displays workspace-relative
// results.
func TestGlobWithHostReadsInWorkspaceAbsolutePathStillWorks(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "src", "a.txt"), "x")

	got := runGlobOpts(t, root, newFakeReadGuard(1<<20), map[string]any{"pattern": "**/*.txt", "root": root}, WithHostReads())
	want := "src/a.txt"
	if got != want {
		t.Errorf("Glob(%q) with WithHostReads (in-workspace absolute) = %q, want %q", root, got, want)
	}
}

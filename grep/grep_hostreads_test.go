package grep

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

// runGrepOpts is runGrep plus the ability to pass GrepOptions, forcing the
// deterministic WalkDir fallback so results do not depend on whether ripgrep
// is installed on the host.
func runGrepOpts(t *testing.T, root string, guard loop.ReadGuard, args map[string]any, opts ...GrepOption) string {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	g := newGrepWithBackend(root, guard, false, opts...)
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
	return grepText(t, res)
}

// TestGrepWithoutHostReadsRejectsOutsideWorkspace pins today's (legacy)
// behavior unchanged: with no WithHostReads() option, an absolute search path
// is lexically re-anchored under root by workspace.ContainedPath's Join (never
// honoured as absolute), so it walks a nonexistent nested directory and finds
// nothing -- not a containment rejection. This is the pre-existing quirk
// WithHostReads() exists to fix; this test only pins the option-absent case.
func TestGrepWithoutHostReadsRejectsOutsideWorkspace(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outsideDir := t.TempDir()
	mustWrite(t, filepath.Join(outsideDir, "host.txt"), "needle\n")

	got := runGrepOpts(t, root, newFakeReadGuard(1<<20), map[string]any{"pattern": "needle", "path": outsideDir})
	if got != "no matches" {
		t.Errorf("Grep(%q) without WithHostReads = %q, want %q", outsideDir, got, "no matches")
	}
}

// TestGrepWithHostReadsSearchesOutsideWorkspace proves WithHostReads() lets an
// absolute out-of-workspace search root resolve and be walked, with results
// displayed relative to THAT searched directory (not the workspace root, which
// the host directory has no meaningful relative path to).
func TestGrepWithHostReadsSearchesOutsideWorkspace(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outsideDir := t.TempDir()
	mustWrite(t, filepath.Join(outsideDir, "host.txt"), "needle here\n")

	got := runGrepOpts(t, root, newFakeReadGuard(1<<20), map[string]any{"pattern": "needle", "path": outsideDir}, WithHostReads())
	want := "host.txt:1:needle here"
	if got != want {
		t.Errorf("Grep(%q) with WithHostReads = %q, want %q", outsideDir, got, want)
	}
}

// TestGrepWithHostReadsStillHonoursDeniedRead proves the guard's denylist
// still applies to a widened host search.
func TestGrepWithHostReadsStillHonoursDeniedRead(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outsideDir := t.TempDir()
	denied := filepath.Join(outsideDir, "id_rsa")
	mustWrite(t, denied, "needle in a private key\n")
	mustWrite(t, filepath.Join(outsideDir, "ok.txt"), "needle in the open\n")

	resolvedDenied := resolvedJoin(t, outsideDir, "id_rsa")
	guard := newFakeReadGuard(1<<20, resolvedDenied)

	got := runGrepOpts(t, root, guard, map[string]any{"pattern": "needle", "path": outsideDir}, WithHostReads())
	want := "ok.txt:1:needle in the open"
	if got != want {
		t.Errorf("Grep(%q) with WithHostReads and a denied file = %q, want %q", outsideDir, got, want)
	}
}

// TestGrepWithHostReadsRelativeEscapeStillRejected proves WithHostReads()
// does NOT widen relative "../" traversal -- only a literal absolute path
// argument gets the new treatment.
func TestGrepWithHostReadsRelativeEscapeStillRejected(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	parent := filepath.Dir(root)
	mustWrite(t, filepath.Join(parent, "sibling-secret.txt"), "needle\n")

	got := runGrepOpts(t, root, newFakeReadGuard(1<<20), map[string]any{"pattern": "needle", "path": ".."}, WithHostReads())
	want := "error: tool preparation failed: search path is outside the workspace: .."
	if got != want {
		t.Errorf("relative .. escape with WithHostReads = %q, want %q", got, want)
	}
}

// TestGrepWithHostReadsInWorkspaceAbsolutePathStillWorks proves the widened
// resolver does not regress in-workspace behavior: an absolute path spelling
// an in-workspace directory still searches and displays workspace-relative
// results.
func TestGrepWithHostReadsInWorkspaceAbsolutePathStillWorks(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "src", "a.txt"), "needle\n")

	got := runGrepOpts(t, root, newFakeReadGuard(1<<20), map[string]any{"pattern": "needle", "path": root}, WithHostReads())
	want := "src/a.txt:1:needle"
	if got != want {
		t.Errorf("Grep(%q) with WithHostReads (in-workspace absolute) = %q, want %q", root, got, want)
	}
}

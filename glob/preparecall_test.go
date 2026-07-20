package glob

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/permission"
)

func mustUUID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() error = %v", err)
	}
	return id
}

// TestGlobPrepareCallRequirement pins the prepared request shape for a tree
// walk: ONE filesystem.read requirement for the canonical ROOT being walked —
// Scope is the plain canonical root path (profile routing), Match is the
// canonical tree encoding (durable tree-rule matching), grant pair EMPTY.
func TestGlobPrepareCallRequirement(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	g := NewGlob(root, newFakeReadGuard(1<<20))
	id := mustUUID(t)

	req, art, err := g.PrepareCall(context.Background(), id, `{"pattern":"**/*.go","root":"sub"}`)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	if err := tool.ValidateRequest(req); err != nil {
		t.Fatalf("ValidateRequest() error = %v", err)
	}
	if len(req.Requirements) != 1 {
		t.Fatalf("Requirements = %d, want ONE for the walked root (tree semantics)", len(req.Requirements))
	}
	r := req.Requirements[0]
	wantRoot := resolvedJoin(t, root, "sub")
	if r.Kind != permission.CapabilityFilesystemRead {
		t.Errorf("Kind = %q, want %q", r.Kind, permission.CapabilityFilesystemRead)
	}
	if r.Scope != wantRoot {
		t.Errorf("Scope = %q, want the plain canonical root %q", r.Scope, wantRoot)
	}
	if r.Match != permission.TreeMatch(wantRoot) {
		t.Errorf("Match = %q, want %q", r.Match, permission.TreeMatch(wantRoot))
	}
	if r.GrantClass != "" || r.GrantTarget != "" {
		t.Errorf("grant pair = %q/%q, want empty (direct tool)", r.GrantClass, r.GrantTarget)
	}
	if art == nil {
		t.Fatal("artifact = nil, want a typed glob artifact")
	}
}

// TestGlobPrepareCallDefaultsToWorkspaceRoot proves the default search root is
// the canonical workspace root.
func TestGlobPrepareCallDefaultsToWorkspaceRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	g := NewGlob(root, newFakeReadGuard(1<<20))
	req, _, err := g.PrepareCall(context.Background(), mustUUID(t), `{"pattern":"*.go"}`)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	wantRoot := resolvedJoin(t, root, ".")
	if req.Requirements[0].Scope != wantRoot {
		t.Errorf("Scope = %q, want workspace root %q", req.Requirements[0].Scope, wantRoot)
	}
}

// TestGlobPrepareCallRejectsMalformedArgs proves malformed input fails at
// preparation.
func TestGlobPrepareCallRejectsMalformedArgs(t *testing.T) {
	t.Parallel()
	g := NewGlob(t.TempDir(), newFakeReadGuard(1<<20))
	for name, args := range map[string]string{
		"not json":      `[`,
		"empty pattern": `{"pattern":""}`,
		"escape root":   `{"pattern":"*","root":"../.."}`,
	} {
		if _, _, err := g.PrepareCall(context.Background(), mustUUID(t), args); err == nil {
			t.Errorf("%s: PrepareCall() error = nil, want an error", name)
		}
	}
}

// TestGlobRunConsumesArtifactNotRawJSON proves execution uses the prepared
// pattern/root, not the (mutated) raw JSON, and fails closed without an
// artifact.
func TestGlobRunConsumesArtifactNotRawJSON(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "match.go"), "package x\n")
	mustWriteFile(t, filepath.Join(root, "other.txt"), "text\n")
	g := NewGlob(root, newFakeReadGuard(1<<20))
	id := mustUUID(t)

	req, art, err := g.PrepareCall(context.Background(), id, `{"pattern":"*.go"}`)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	ctx := loop.WithPreparedCall(context.Background(), tool.PreparedCall{ExecutionID: id, Request: req, Artifact: art})
	res, err := g.InvokableRun(ctx, `{"pattern":"*.txt"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	text := textOfResult(t, res)
	if !strings.Contains(text, "match.go") || strings.Contains(text, "other.txt") {
		t.Fatalf("result = %q, want the PREPARED pattern applied, not the mutated raw args", text)
	}

	// Without an artifact the tool fails closed.
	res2, err := g.InvokableRun(context.Background(), `{"pattern":"*.go"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	if text := textOfResult(t, res2); !strings.HasPrefix(text, "error:") {
		t.Fatalf("result = %q, want fail-closed without an artifact", text)
	}
}

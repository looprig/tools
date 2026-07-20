package grep

import (
	"context"
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

// TestGrepPrepareCallRequirement pins the prepared request shape for a content
// search: ONE filesystem.read requirement for the canonical ROOT being walked —
// Scope is the plain canonical root path, Match the canonical tree encoding,
// grant pair EMPTY.
func TestGrepPrepareCallRequirement(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "sub", "f.go"), "package x\n")
	g := newGrepWithBackend(root, newFakeReadGuard(1<<20), false)
	id := mustUUID(t)

	req, art, err := g.PrepareCall(context.Background(), id, `{"pattern":"package","path":"sub"}`)
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
	if r.Kind != permission.CapabilityFilesystemRead || r.Scope != wantRoot || r.Match != permission.TreeMatch(wantRoot) {
		t.Errorf("requirement = %+v, want filesystem.read tree on %q", r, wantRoot)
	}
	if r.GrantClass != "" || r.GrantTarget != "" {
		t.Errorf("grant pair = %q/%q, want empty (direct tool)", r.GrantClass, r.GrantTarget)
	}
	if art == nil {
		t.Fatal("artifact = nil, want a typed grep artifact")
	}
}

// TestGrepPrepareCallRejectsMalformedArgs proves malformed input — including an
// invalid regex — fails at preparation, never reaching evaluation.
func TestGrepPrepareCallRejectsMalformedArgs(t *testing.T) {
	t.Parallel()
	g := newGrepWithBackend(t.TempDir(), newFakeReadGuard(1<<20), false)
	for name, args := range map[string]string{
		"not json":       `x`,
		"empty pattern":  `{"pattern":""}`,
		"invalid regex":  `{"pattern":"("}`,
		"negative lines": `{"pattern":"x","context_lines":-1}`,
		"escape path":    `{"pattern":"x","path":"../.."}`,
	} {
		if _, _, err := g.PrepareCall(context.Background(), mustUUID(t), args); err == nil {
			t.Errorf("%s: PrepareCall() error = nil, want an error", name)
		}
	}
}

// TestGrepRunConsumesArtifactNotRawJSON proves execution uses the prepared
// pattern, not the (mutated) raw JSON, and fails closed without an artifact.
func TestGrepRunConsumesArtifactNotRawJSON(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "f.txt"), "alpha\nbeta\n")
	g := newGrepWithBackend(root, newFakeReadGuard(1<<20), false)
	id := mustUUID(t)

	req, art, err := g.PrepareCall(context.Background(), id, `{"pattern":"alpha"}`)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	ctx := loop.WithPreparedCall(context.Background(), tool.PreparedCall{ExecutionID: id, Request: req, Artifact: art})
	res, err := g.InvokableRun(ctx, `{"pattern":"beta"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	out := grepText(t, res)
	if !strings.Contains(out, "alpha") || strings.Contains(out, "beta") {
		t.Fatalf("result = %q, want the PREPARED pattern applied, not the mutated raw args", out)
	}

	res2, err := g.InvokableRun(context.Background(), `{"pattern":"alpha"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	if out := grepText(t, res2); !strings.HasPrefix(out, "error:") {
		t.Fatalf("result = %q, want fail-closed without an artifact", out)
	}
}

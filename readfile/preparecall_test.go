package readfile

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

// TestReadFilePrepareCallRequirement pins the prepared request shape for a
// direct read: one filesystem.read requirement whose Scope and Match are BOTH
// the canonical resolved path and whose grant pair is EMPTY.
func TestReadFilePrepareCallRequirement(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "f.txt"), "body\n")
	rf := NewReadFile(root, newFakeReadGuard(1<<20), tool.NewWorkspaceObservations())
	id := mustUUID(t)

	req, art, err := rf.PrepareCall(context.Background(), id, `{"path":"sub/../f.txt"}`)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	if err := tool.ValidateRequest(req); err != nil {
		t.Fatalf("ValidateRequest() error = %v", err)
	}
	if req.ToolName != "ReadFile" || req.ExecutionID != id.String() {
		t.Errorf("ToolName/ExecutionID = %q/%q, want ReadFile/%q", req.ToolName, req.ExecutionID, id.String())
	}
	if len(req.Requirements) != 1 {
		t.Fatalf("Requirements = %d, want 1", len(req.Requirements))
	}
	r := req.Requirements[0]
	wantAbs := resolvedJoin(t, root, "f.txt")
	if r.Kind != permission.CapabilityFilesystemRead || r.Scope != wantAbs || r.Match != wantAbs {
		t.Errorf("requirement = %+v, want filesystem.read on %q", r, wantAbs)
	}
	if r.GrantClass != "" || r.GrantTarget != "" {
		t.Errorf("grant pair = %q/%q, want empty (direct tool)", r.GrantClass, r.GrantTarget)
	}
	if art == nil {
		t.Fatal("artifact = nil, want a typed read artifact")
	}
}

// TestReadFilePrepareCallRejectsMalformedArgs proves malformed input fails at
// preparation, never reaching evaluation.
func TestReadFilePrepareCallRejectsMalformedArgs(t *testing.T) {
	t.Parallel()
	rf := NewReadFile(t.TempDir(), newFakeReadGuard(1<<20), tool.NewWorkspaceObservations())
	for name, args := range map[string]string{
		"not json":   `{`,
		"empty path": `{"path":""}`,
		"bad range":  `{"path":"f.txt","start_line":5,"end_line":2}`,
		"escape":     `{"path":"../../etc/passwd"}`,
	} {
		if _, _, err := rf.PrepareCall(context.Background(), mustUUID(t), args); err == nil {
			t.Errorf("%s: PrepareCall() error = nil, want an error", name)
		}
	}
}

// TestReadFileRunConsumesArtifactNotRawJSON proves execution consumes the
// typed artifact: mutating the raw args after preparation changes nothing.
func TestReadFileRunConsumesArtifactNotRawJSON(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "approved.txt"), "approved-body\n")
	mustWrite(t, filepath.Join(root, "secret.txt"), "secret-body\n")
	rf := NewReadFile(root, newFakeReadGuard(1<<20), tool.NewWorkspaceObservations())
	id := mustUUID(t)

	req, art, err := rf.PrepareCall(context.Background(), id, `{"path":"approved.txt"}`)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	ctx := loop.WithPreparedCall(context.Background(), tool.PreparedCall{ExecutionID: id, Request: req, Artifact: art})
	res, err := rf.InvokableRun(ctx, `{"path":"secret.txt"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	out := resultText(t, res)
	if !strings.Contains(out, "approved-body") || strings.Contains(out, "secret-body") {
		t.Fatalf("result = %q, want the APPROVED file, never the raw-args swap", out)
	}
}

// TestReadFileRunWithoutArtifactFailsClosed proves the read refuses to execute
// without its prepared artifact.
func TestReadFileRunWithoutArtifactFailsClosed(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "f.txt"), "body\n")
	rf := NewReadFile(root, newFakeReadGuard(1<<20), tool.NewWorkspaceObservations())
	res, err := rf.InvokableRun(context.Background(), `{"path":"f.txt"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	if out := resultText(t, res); !strings.HasPrefix(out, "error:") || strings.Contains(out, "body") {
		t.Fatalf("result = %q, want a fail-closed error without the body", out)
	}
}

// TestReadFileRunRefusesChangedResolution proves run-time enforcement of the
// approved resolved path: a parent-dir symlink swap between prepare and run
// refuses the read.
func TestReadFileRunRefusesChangedResolution(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "e"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "d", "f.txt"), "approved\n")
	mustWrite(t, filepath.Join(root, "e", "f.txt"), "swapped\n")
	rf := NewReadFile(root, newFakeReadGuard(1<<20), tool.NewWorkspaceObservations())
	id := mustUUID(t)

	req, art, err := rf.PrepareCall(context.Background(), id, `{"path":"d/f.txt"}`)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	if err := os.RemoveAll(filepath.Join(root, "d")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "e"), filepath.Join(root, "d")); err != nil {
		t.Fatal(err)
	}
	ctx := loop.WithPreparedCall(context.Background(), tool.PreparedCall{ExecutionID: id, Request: req, Artifact: art})
	res, err := rf.InvokableRun(ctx, `{"path":"d/f.txt"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	if out := resultText(t, res); !strings.HasPrefix(out, "error:") || strings.Contains(out, "swapped") {
		t.Fatalf("result = %q, want a refusal that never serves the swapped file", out)
	}
}

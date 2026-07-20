package filemutation

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

// mustUUID mints a v4 execution ID for a prepared-call test.
func mustUUID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() error = %v", err)
	}
	return id
}

// TestWriteFilePrepareCallRequirement pins the prepared request shape for a
// direct write: one filesystem.write requirement whose Scope and Match are BOTH
// the canonical resolved target path, an EMPTY grant pair (the direct tool
// enforces the approved path itself), and one reusable exact-path candidate.
func TestWriteFilePrepareCallRequirement(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	w := NewWriteFile(root, newFileObservations())
	id := mustUUID(t)

	req, art, err := w.PrepareCall(context.Background(), id, `{"path":"sub/../f.txt","content":"hello"}`)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	if err := tool.ValidateRequest(req); err != nil {
		t.Fatalf("ValidateRequest() error = %v", err)
	}
	if req.ToolName != "WriteFile" {
		t.Errorf("ToolName = %q, want WriteFile", req.ToolName)
	}
	if req.ExecutionID != id.String() {
		t.Errorf("ExecutionID = %q, want %q", req.ExecutionID, id.String())
	}
	if len(req.Requirements) != 1 {
		t.Fatalf("Requirements = %d, want 1", len(req.Requirements))
	}
	r := req.Requirements[0]
	wantAbs := resolvedJoin(t, root, "f.txt")
	if r.Kind != permission.CapabilityFilesystemWrite {
		t.Errorf("Kind = %q, want %q", r.Kind, permission.CapabilityFilesystemWrite)
	}
	if r.Scope != wantAbs || r.Match != wantAbs {
		t.Errorf("Scope/Match = %q/%q, want both %q", r.Scope, r.Match, wantAbs)
	}
	if r.GrantClass != "" || r.GrantTarget != "" {
		t.Errorf("grant pair = %q/%q, want empty (direct tool)", r.GrantClass, r.GrantTarget)
	}
	if len(r.Candidates) != 1 || r.Candidates[0].Match != wantAbs || r.Candidates[0].Kind != r.Kind {
		t.Errorf("Candidates = %+v, want one exact-path candidate for %q", r.Candidates, wantAbs)
	}
	if art == nil {
		t.Fatal("PrepareCall() artifact = nil, want a typed write artifact")
	}
}

// TestWriteFilePrepareCallRejectsMalformedArgs proves malformed input fails
// during preparation and never reaches evaluation or execution.
func TestWriteFilePrepareCallRejectsMalformedArgs(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	w := NewWriteFile(root, newFileObservations())

	for name, args := range map[string]string{
		"not json":     `{"path":`,
		"empty path":   `{"path":"","content":"x"}`,
		"missing path": `{"content":"x"}`,
		"escape":       `{"path":"../out.txt","content":"x"}`,
	} {
		if _, _, err := w.PrepareCall(context.Background(), mustUUID(t), args); err == nil {
			t.Errorf("%s: PrepareCall() error = nil, want an error", name)
		}
	}
}

// TestWriteTargetKeyIsPreparedScope proves the write scheduling key IS the
// prepared canonical path: two arg spellings of the same file collide, distinct
// files do not, and the key equals the prepared requirement Scope.
func TestWriteTargetKeyIsPreparedScope(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	w := NewWriteFile(root, newFileObservations())

	keyA, ok, err := w.WriteTarget(`{"path":"a/../f.txt","content":"1"}`)
	if err != nil || !ok {
		t.Fatalf("WriteTarget(alias) = %q, %v, %v", keyA, ok, err)
	}
	keyB, _, err := w.WriteTarget(`{"path":"f.txt","content":"2"}`)
	if err != nil {
		t.Fatalf("WriteTarget(plain) error = %v", err)
	}
	if keyA != keyB {
		t.Errorf("same canonical file: keys %q != %q, want collision", keyA, keyB)
	}
	keyC, _, err := w.WriteTarget(`{"path":"g.txt","content":"3"}`)
	if err != nil {
		t.Fatalf("WriteTarget(other) error = %v", err)
	}
	if keyC == keyA {
		t.Errorf("distinct files share key %q, want distinct keys", keyC)
	}

	req, _, err := w.PrepareCall(context.Background(), mustUUID(t), `{"path":"f.txt","content":"2"}`)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	if req.Requirements[0].Scope != keyA {
		t.Errorf("requirement Scope %q != scheduling key %q, want the same canonical path", req.Requirements[0].Scope, keyA)
	}
}

// TestWriteFileRunConsumesArtifactNotRawJSON proves execution consumes the
// typed prepared artifact: mutating the raw argsJSON AFTER preparation changes
// nothing — the approved path and content are written.
func TestWriteFileRunConsumesArtifactNotRawJSON(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	w := NewWriteFile(root, newFileObservations())
	id := mustUUID(t)

	req, art, err := w.PrepareCall(context.Background(), id, `{"path":"approved.txt","content":"approved"}`)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	ctx := loop.WithPreparedCall(context.Background(), tool.PreparedCall{ExecutionID: id, Request: req, Artifact: art})

	res, err := w.InvokableRun(ctx, `{"path":"attacker.txt","content":"swapped"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	if text := textBlock(t, res); strings.HasPrefix(text, "error:") {
		t.Fatalf("InvokableRun() = %q, want success", text)
	}
	got, err := os.ReadFile(filepath.Join(root, "approved.txt"))
	if err != nil || string(got) != "approved" {
		t.Fatalf("approved.txt = %q, %v; want %q written", got, err, "approved")
	}
	if _, err := os.Lstat(filepath.Join(root, "attacker.txt")); !os.IsNotExist(err) {
		t.Fatalf("attacker.txt exists (err=%v), want the mutated raw args ignored", err)
	}
}

// TestWriteFileRunWithoutArtifactFailsClosed proves the effectful tool refuses
// to execute without its prepared artifact — no file is written.
func TestWriteFileRunWithoutArtifactFailsClosed(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	w := NewWriteFile(root, newFileObservations())

	res, err := w.InvokableRun(context.Background(), `{"path":"f.txt","content":"x"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	if text := textBlock(t, res); !strings.HasPrefix(text, "error:") {
		t.Fatalf("InvokableRun() = %q, want a fail-closed error", text)
	}
	if _, err := os.Lstat(filepath.Join(root, "f.txt")); !os.IsNotExist(err) {
		t.Fatal("f.txt written without a prepared artifact")
	}
}

// TestWriteFileRunRefusesChangedResolution proves run-time enforcement of the
// APPROVED resolved path: if the path's resolution changes between prepare and
// run (a parent directory swapped to a symlink), the write is refused.
func TestWriteFileRunRefusesChangedResolution(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "e"), 0o755); err != nil {
		t.Fatal(err)
	}
	w := NewWriteFile(root, newFileObservations())
	id := mustUUID(t)

	req, art, err := w.PrepareCall(context.Background(), id, `{"path":"d/f.txt","content":"x"}`)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	// Swap d to a symlink pointing at e AFTER preparation.
	if err := os.Remove(filepath.Join(root, "d")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "e"), filepath.Join(root, "d")); err != nil {
		t.Fatal(err)
	}

	ctx := loop.WithPreparedCall(context.Background(), tool.PreparedCall{ExecutionID: id, Request: req, Artifact: art})
	res, err := w.InvokableRun(ctx, `{"path":"d/f.txt","content":"x"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	if text := textBlock(t, res); !strings.HasPrefix(text, "error:") {
		t.Fatalf("InvokableRun() = %q, want a refusal after resolution changed", text)
	}
	if _, err := os.Lstat(filepath.Join(root, "e", "f.txt")); !os.IsNotExist(err) {
		t.Fatal("write followed the swapped symlink into e/")
	}
}

// TestEditFilePrepareCallRequirementAndArtifact pins EditFile's prepared shape
// (mirroring WriteFile) and that run consumes the artifact, not the raw JSON.
func TestEditFilePrepareCallRequirementAndArtifact(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("alpha beta"), 0o600); err != nil {
		t.Fatal(err)
	}
	obs := newFileObservations()
	e := NewEditFile(root, obs)
	observeFile(t, root, obs, "f.txt")
	id := mustUUID(t)

	req, art, err := e.PrepareCall(context.Background(), id, `{"path":"f.txt","old":"alpha","new":"gamma"}`)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	if err := tool.ValidateRequest(req); err != nil {
		t.Fatalf("ValidateRequest() error = %v", err)
	}
	wantAbs := resolvedJoin(t, root, "f.txt")
	if len(req.Requirements) != 1 {
		t.Fatalf("Requirements = %d, want 1", len(req.Requirements))
	}
	r := req.Requirements[0]
	if r.Kind != permission.CapabilityFilesystemWrite || r.Scope != wantAbs || r.Match != wantAbs || r.GrantClass != "" || r.GrantTarget != "" {
		t.Errorf("requirement = %+v, want direct filesystem.write on %q", r, wantAbs)
	}

	// Mutated raw args after preparation change nothing: the approved edit runs.
	ctx := loop.WithPreparedCall(context.Background(), tool.PreparedCall{ExecutionID: id, Request: req, Artifact: art})
	res, err := e.InvokableRun(ctx, `{"path":"f.txt","old":"beta","new":"ATTACK"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	if text := textBlock(t, res); strings.HasPrefix(text, "error:") {
		t.Fatalf("InvokableRun() = %q, want success", text)
	}
	got, err := os.ReadFile(filepath.Join(root, "f.txt"))
	if err != nil || string(got) != "gamma beta" {
		t.Fatalf("f.txt = %q, %v; want the APPROVED edit applied", got, err)
	}
}

// TestEditFilePrepareCallRejectsMalformedArgs proves malformed edit input fails
// at preparation: bad JSON, empty path, empty old, escapes.
func TestEditFilePrepareCallRejectsMalformedArgs(t *testing.T) {
	t.Parallel()
	e := NewEditFile(t.TempDir(), newFileObservations())
	for name, args := range map[string]string{
		"not json":   `nope`,
		"empty path": `{"path":"","old":"a","new":"b"}`,
		"empty old":  `{"path":"f.txt","old":"","new":"b"}`,
		"escape":     `{"path":"../f.txt","old":"a","new":"b"}`,
	} {
		if _, _, err := e.PrepareCall(context.Background(), mustUUID(t), args); err == nil {
			t.Errorf("%s: PrepareCall() error = nil, want an error", name)
		}
	}
}

// TestEditFileRunWithoutArtifactFailsClosed proves EditFile refuses to run
// without its prepared artifact.
func TestEditFileRunWithoutArtifactFailsClosed(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("alpha"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := NewEditFile(root, newFileObservations())
	res, err := e.InvokableRun(context.Background(), `{"path":"f.txt","old":"alpha","new":"beta"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	if text := textBlock(t, res); !strings.HasPrefix(text, "error:") {
		t.Fatalf("InvokableRun() = %q, want a fail-closed error", text)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "f.txt")); string(got) != "alpha" {
		t.Fatalf("f.txt = %q, want untouched without a prepared artifact", got)
	}
}

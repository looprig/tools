//go:build integration

package skill

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

// wsSkillBody is the markdown body of the workspace fixture skill (the bytes after
// the frontmatter that loadWorkspaceSkill returns as Body).
const wsSkillBody = "Workspace body line.\n"

// wsSkill is a well-formed workspace SKILL.md whose name does NOT collide with the
// embedded code-style fixture, so it exercises the pure workspace path.
const wsSkill = "---\nname: ws-refactor\ndescription: a workspace skill\n---\n" + wsSkillBody

// newWorkspaceSkillTool wires an embedded loader (the operator may load code-style)
// PLUS a workspace root, returning a workspace-enabled Skill bound to "operator".
func newWorkspaceSkillTool(t *testing.T, root string) *Skill {
	t.Helper()
	loader := NewEmbeddedSkillLoader(newSkillToolFS(), skillToolAllow())
	return NewSkill(loader, identity.AgentName("operator"), WithWorkspaceRoot(root))
}

// TestSkillWorkspaceNoReReadAfterPrepare is the TOCTOU proof: after PrepareCall takes
// the snapshot, the on-disk file is MUTATED (and then DELETED). InvokableRun must
// still return the ORIGINAL snapshot Body — it reads the artifact bound to the
// call, never re-opening the file — so a workspace writer cannot swap the body
// between the human prompt and execution.
func TestSkillWorkspaceNoReReadAfterPrepare(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	rel := ".skills/ws-refactor/SKILL.md"
	writeSkillFile(t, root, rel, wsSkill)

	s := newWorkspaceSkillTool(t, root)
	args := `{"name":"ws-refactor"}`

	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() error = %v", err)
	}
	req, prepared, err := s.PrepareCall(context.Background(), id, args)
	if err != nil {
		t.Fatalf("PrepareCall error = %v, want nil", err)
	}

	full := filepath.Join(root, filepath.FromSlash(rel))

	// 1. MUTATE the file to a different, malicious body after the snapshot.
	tampered := "---\nname: ws-refactor\ndescription: tampered\n---\nMALICIOUS injected body.\n"
	if err := os.WriteFile(full, []byte(tampered), 0o644); err != nil {
		t.Fatalf("rewrite file: %v", err)
	}
	ctx := loop.WithPreparedCall(context.Background(), tool.PreparedCall{ExecutionID: id, Request: req, Artifact: prepared})
	res, err := s.InvokableRun(ctx, args)
	if err != nil {
		t.Fatalf("InvokableRun (after mutate) Go error = %v, want nil", err)
	}
	got := textOf(t, res)
	if got != wsSkillBody {
		t.Errorf("after mutate, result = %q, want the ORIGINAL snapshot body %q (no re-read)", got, wsSkillBody)
	}
	if strings.Contains(got, "MALICIOUS") {
		t.Errorf("result = %q served the tampered on-disk body — TOCTOU re-read bug", got)
	}

	// 2. DELETE the file entirely; InvokableRun must STILL serve the snapshot.
	if err := os.Remove(full); err != nil {
		t.Fatalf("remove file: %v", err)
	}
	res2, err := s.InvokableRun(ctx, args)
	if err != nil {
		t.Fatalf("InvokableRun (after delete) Go error = %v, want nil", err)
	}
	if got := textOf(t, res2); got != wsSkillBody {
		t.Errorf("after delete, result = %q, want the snapshot body %q (no re-read)", got, wsSkillBody)
	}
}

// TestSkillWorkspaceBadArgsFailSecure proves a workspace-enabled tool with
// unparseable/empty-name args returns a typed preparation error (fail-secure)
// rather than silently loading nothing.
func TestSkillWorkspaceBadArgsFailSecure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		argsJSON string
	}{
		{name: "unparseable args", argsJSON: `not json`},
		{name: "empty name", argsJSON: `{"name":""}`},
		{name: "whitespace name", argsJSON: `{"name":"   "}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			s := newWorkspaceSkillTool(t, root)

			id, uerr := uuid.New()
			if uerr != nil {
				t.Fatalf("uuid.New() error = %v", uerr)
			}
			_, prepared, err := s.PrepareCall(context.Background(), id, tt.argsJSON)
			if prepared != nil {
				t.Errorf("PrepareCall artifact = %v, want nil on bad args", prepared)
			}
			var ce *SkillContainmentError
			if !errors.As(err, &ce) {
				t.Fatalf("Prepare error = %v, want *SkillContainmentError (fail-secure)", err)
			}
		})
	}
}

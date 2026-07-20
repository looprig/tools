package skill

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/permission"
)

// writeWorkspaceSkill seeds <root>/.skills/<name>/SKILL.md with doc.
func writeWorkspaceSkill(t *testing.T, root, name, doc string) {
	t.Helper()
	dir := filepath.Join(root, ".skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustUUID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() error = %v", err)
	}
	return id
}

// TestSkillPrepareCallEmbedded pins the prepared request for an EMBEDDED skill
// load: exactly one context.load requirement scoped to the canonical embedded
// skill identity, empty grant pair, no durable candidates (context.load has no
// durable rule representation).
func TestSkillPrepareCallEmbedded(t *testing.T) {
	t.Parallel()
	loader := NewEmbeddedSkillLoader(newSkillToolFS(), skillToolAllow())
	s := NewSkill(loader, identity.AgentName("operator"))
	id := mustUUID(t)

	req, art, err := s.PrepareCall(context.Background(), id, `{"name":"code-style"}`)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	if err := tool.ValidateRequest(req); err != nil {
		t.Fatalf("ValidateRequest() error = %v", err)
	}
	if req.ToolName != "Skill" || req.ExecutionID != id.String() {
		t.Errorf("ToolName/ExecutionID = %q/%q", req.ToolName, req.ExecutionID)
	}
	if len(req.Requirements) != 1 {
		t.Fatalf("Requirements = %d, want 1 (context.load only)", len(req.Requirements))
	}
	r := req.Requirements[0]
	wantScope := EmbeddedSkillIdentity("code-style")
	if r.Kind != CapabilityContextLoad || r.Scope != wantScope || r.Match != wantScope {
		t.Errorf("requirement = %+v, want context.load on %q", r, wantScope)
	}
	if r.GrantClass != "" || r.GrantTarget != "" || len(r.Candidates) != 0 {
		t.Errorf("requirement = %+v, want empty grant pair and no candidates", r)
	}
	if art == nil {
		t.Fatal("artifact = nil, want the bound name artifact")
	}
}

// TestSkillPrepareCallWorkspace pins the prepared request for a WORKSPACE
// skill load: context.load scoped to the workspace skill identity PLUS the
// applicable filesystem.read requirement for the canonical snapshot path, and
// the TOCTOU-safe snapshot artifact.
func TestSkillPrepareCallWorkspace(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeWorkspaceSkill(t, root, "ws-skill", "---\nname: ws-skill\ndescription: d.\n---\nWS BODY\n")
	loader := NewEmbeddedSkillLoader(newSkillToolFS(), skillToolAllow())
	s := NewSkill(loader, identity.AgentName("operator"), WithWorkspaceRoot(root))
	id := mustUUID(t)

	req, art, err := s.PrepareCall(context.Background(), id, `{"name":"ws-skill"}`)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	if err := tool.ValidateRequest(req); err != nil {
		t.Fatalf("ValidateRequest() error = %v", err)
	}
	if len(req.Requirements) != 2 {
		t.Fatalf("Requirements = %d, want context.load + filesystem.read", len(req.Requirements))
	}
	var haveLoad, haveRead bool
	for _, r := range req.Requirements {
		switch r.Kind {
		case CapabilityContextLoad:
			haveLoad = true
			want := WorkspaceSkillIdentity("ws-skill")
			if r.Scope != want || r.Match != want {
				t.Errorf("context.load Scope/Match = %q/%q, want %q", r.Scope, r.Match, want)
			}
		case permission.CapabilityFilesystemRead:
			haveRead = true
			if !strings.HasSuffix(r.Scope, "/.skills/ws-skill/SKILL.md") || r.Scope != r.Match {
				t.Errorf("filesystem.read Scope/Match = %q/%q, want the canonical SKILL.md path", r.Scope, r.Match)
			}
		default:
			t.Errorf("unexpected requirement kind %q", r.Kind)
		}
		if r.GrantClass != "" || r.GrantTarget != "" {
			t.Errorf("grant pair = %q/%q, want empty (direct tool)", r.GrantClass, r.GrantTarget)
		}
	}
	if !haveLoad || !haveRead {
		t.Fatalf("requirements = %+v, want both context.load and filesystem.read", req.Requirements)
	}
	sa, ok := art.(*tool.SkillArtifact)
	if !ok || !sa.Workspace || sa.Body != "WS BODY\n" {
		t.Fatalf("artifact = %#v, want the workspace snapshot", art)
	}
}

// TestSkillPrepareCallRejectsMalformedArgs proves malformed args fail during
// preparation for BOTH shapes (embedded-only and workspace-enabled).
func TestSkillPrepareCallRejectsMalformedArgs(t *testing.T) {
	t.Parallel()
	loader := NewEmbeddedSkillLoader(newSkillToolFS(), skillToolAllow())
	embedded := NewSkill(loader, identity.AgentName("operator"))
	workspaceEnabled := NewSkill(loader, identity.AgentName("operator"), WithWorkspaceRoot(t.TempDir()))
	for name, s := range map[string]*Skill{"embedded-only": embedded, "workspace-enabled": workspaceEnabled} {
		for argsName, args := range map[string]string{
			"not json":   `{`,
			"empty name": `{"name":"  "}`,
		} {
			if _, _, err := s.PrepareCall(context.Background(), mustUUID(t), args); err == nil {
				t.Errorf("%s/%s: PrepareCall() error = nil, want an error", name, argsName)
			}
		}
	}
}

// TestSkillRunConsumesArtifactNotRawJSON proves the embedded execution path
// serves the PREPARED name even when the raw args are mutated after
// preparation, and that a call without a prepared artifact fails closed.
func TestSkillRunConsumesArtifactNotRawJSON(t *testing.T) {
	t.Parallel()
	loader := NewEmbeddedSkillLoader(newSkillToolFS(), skillToolAllow())
	s := NewSkill(loader, identity.AgentName("operator"))
	id := mustUUID(t)

	req, art, err := s.PrepareCall(context.Background(), id, `{"name":"code-style"}`)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	ctx := loop.WithPreparedCall(context.Background(), tool.PreparedCall{ExecutionID: id, Request: req, Artifact: art})
	res, err := s.InvokableRun(ctx, `{"name":"some-other-skill"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	if got := textOf(t, res); got != skillToolBody {
		t.Fatalf("result = %q, want the PREPARED skill body", got)
	}

	res2, err := s.InvokableRun(context.Background(), `{"name":"code-style"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	if got := textOf(t, res2); !strings.HasPrefix(got, "error:") || strings.Contains(got, "checklist") {
		t.Fatalf("result = %q, want fail-closed without a prepared artifact", got)
	}
}

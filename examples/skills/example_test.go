package skills_test

import (
	"context"
	"fmt"
	"strings"
	"testing/fstest"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/skill"
)

// Example_embeddedSkill wires a product-owned skill catalogue to one named
// agent. The closed allow-set is checked before the loader constructs a path.
func Example_embeddedSkill() {
	agent := identity.AgentName("reviewer")
	catalogue := fstest.MapFS{
		"skills/check/SKILL.md": {Data: []byte("---\nname: check\ndescription: Check a change.\n---\nRun the focused tests.\n")},
	}
	loader := skill.NewEmbeddedSkillLoader(catalogue, map[identity.AgentName]map[string]struct{}{
		agent: {"check": {}},
	})
	skillTool := skill.NewSkill(loader, agent)
	id := uuid.MustParse("44444444-4444-4444-8444-444444444444")

	request, artifact, err := skillTool.PrepareCall(context.Background(), id, `{"name":"check"}`)
	if err != nil {
		panic(err)
	}
	ctx := loop.WithPreparedCall(context.Background(), tool.PreparedCall{ExecutionID: id, Request: request, Artifact: artifact})
	result, err := skillTool.InvokableRun(ctx, `{}`)
	if err != nil {
		panic(err)
	}
	fmt.Print(result.Content[0].(*content.TextBlock).Text)

	request, artifact, err = skillTool.PrepareCall(context.Background(), id, `{"name":"deploy"}`)
	if err != nil {
		panic(err)
	}
	ctx = loop.WithPreparedCall(context.Background(), tool.PreparedCall{ExecutionID: id, Request: request, Artifact: artifact})
	result, err = skillTool.InvokableRun(ctx, `{}`)
	if err != nil {
		panic(err)
	}
	denied := result.Content[0].(*content.TextBlock).Text
	fmt.Println("unknown denied:", strings.HasPrefix(denied, "error:"))

	// Output:
	// Run the focused tests.
	// unknown denied: true
}

package definition

import (
	"context"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
)

type namedTool struct{ name string }

func (n namedTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: n.name}, nil
}

func (namedTool) InvokableRun(context.Context, string) (*tool.ToolResult, error) {
	return tool.TextResult("ok"), nil
}

func TestWithCaptureSafetyDeclaresWithoutChangingTheDefinition(t *testing.T) {
	t.Parallel()
	inner := tool.NewBundleDefinition("Thing", []string{"Thing"}, 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{namedTool{name: "Thing"}}, nil
	})
	declared := tool.DeclaredCaptureSafety{Streaming: true, HighOutput: true}
	wrapped := WithCaptureSafety(inner, declared)

	declarer, ok := wrapped.(tool.CaptureSafetyDeclarer)
	if !ok {
		t.Fatalf("%T does not declare capture safety", wrapped)
	}
	if got := declarer.DeclaredCaptureSafety(); got != declared {
		t.Fatalf("DeclaredCaptureSafety() = %+v, want %+v", got, declared)
	}
	if wrapped.Name() != "Thing" || wrapped.Requirements() != inner.Requirements() || len(wrapped.ProducedToolNames()) != 1 {
		t.Fatalf("wrapped definition = (%q, %v), want the inner name and requirements", wrapped.Name(), wrapped.Requirements())
	}
	built, err := wrapped.Build(context.Background(), tool.Bindings{
		SessionID: uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		LoopID:    uuid.MustParse("22222222-2222-4222-8222-222222222222"),
	})
	if err != nil || len(built) != 1 {
		t.Fatalf("Build() = %v, %v", built, err)
	}
	row := tool.ProjectCaptureSafety([]tool.Definition{wrapped}, 0).Definitions[0]
	if row.Class != tool.CaptureClassStreaming || !row.HighOutput {
		t.Fatalf("projected row = %+v, want streaming high-output", row)
	}
}

package preparation_test

import (
	"context"
	"fmt"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/bash"
)

type recordingRunner struct {
	calls   int
	command string
}

func (r *recordingRunner) RunCommand(_ context.Context, _ string, command string) ([]byte, int, error) {
	r.calls++
	r.command = command
	return []byte("prepared command ran"), 0, nil
}

// Example_preparedBashCall shows the two-phase call contract. PrepareCall
// validates and freezes the request first. The effect happens only after the
// caller supplies that prepared artifact in the invocation context.
func Example_preparedBashCall() {
	runner := &recordingRunner{}
	bashTool := bash.NewBash("/workspace", bash.WithRunner(runner))
	executionID := uuid.MustParse("33333333-3333-4333-8333-333333333333")

	request, artifact, err := bashTool.PrepareCall(context.Background(), executionID, `{"command":"printf prepared"}`)
	if err != nil {
		panic(err)
	}
	fmt.Println("after prepare:", runner.calls, request.Requirements[0].Kind, request.Requirements[0].Match)

	ctx := loop.WithPreparedCall(context.Background(), tool.PreparedCall{
		ExecutionID: executionID,
		Request:     request,
		Artifact:    artifact,
	})
	result, err := bashTool.InvokableRun(ctx, `{"command":"printf changed"}`)
	if err != nil {
		panic(err)
	}
	text := result.Content[0].(*content.TextBlock).Text
	fmt.Println("after invoke:", runner.calls, runner.command)
	fmt.Println(text)

	// Output:
	// after prepare: 0 command.execute printf prepared
	// after invoke: 1 printf prepared
	// prepared command ran
	// [exit code: 0]
}

package tasks_test

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/task"
)

func invoke(t tool.InvokableTool, id uuid.UUID, args string) string {
	preparer := t.(tool.CallPreparer)
	request, artifact, err := preparer.PrepareCall(context.Background(), id, args)
	if err != nil {
		panic(err)
	}
	ctx := loop.WithPreparedCall(context.Background(), tool.PreparedCall{ExecutionID: id, Request: request, Artifact: artifact})
	result, err := t.InvokableRun(ctx, args)
	if err != nil {
		panic(err)
	}
	return result.Content[0].(*content.TextBlock).Text
}

// Example_taskBundle uses the four tools from one bundle. They share one
// Loop-local in-memory graph, while separate NewTools calls do not.
func Example_taskBundle() {
	bundle := task.NewTools()
	id := uuid.MustParse("55555555-5555-4555-8555-555555555555")

	createdJSON := invoke(bundle[0], id, `{"subject":"Document tools","description":"Add runnable examples"}`)
	var created struct {
		Task task.Task `json:"task"`
	}
	if err := json.Unmarshal([]byte(createdJSON), &created); err != nil {
		panic(err)
	}
	fmt.Println(created.Task.Subject)

	listedJSON := invoke(bundle[3], id, `{}`)
	var listed struct {
		Tasks []task.Task `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(listedJSON), &listed); err != nil {
		panic(err)
	}
	fmt.Println("task count:", len(listed.Tasks))

	// Output:
	// Document tools
	// task count: 1
}

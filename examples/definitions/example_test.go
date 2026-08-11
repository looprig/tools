package definitions_test

import (
	"context"
	"fmt"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
	standardtools "github.com/looprig/tools"
	"github.com/looprig/tools/bash"
)

type commandRunner struct{}

func (commandRunner) RunCommand(context.Context, string, string) ([]byte, int, error) {
	return []byte("ok"), 0, nil
}

type coordinator struct{}

func (coordinator) Acquire(context.Context, tool.WorkspaceOperation, string) (tool.WorkspacePermit, error) {
	return permit{}, nil
}

func (coordinator) Healthy() error { return nil }

type permit struct{}

func (permit) Release() {}

// Example_definitionRequirements contrasts a pure in-memory bundle with an
// effectful definition. Definitions declare what bindings they need before a
// Harness builds any concrete tool instances.
func Example_definitionRequirements() {
	pure := standardtools.TaskDefinitions()
	effectful := standardtools.Bash(bash.WithRunner(commandRunner{}))

	fmt.Println(pure.Name(), pure.Requirements() == 0)
	fmt.Println(effectful.Name(), effectful.Requirements() == tool.RequiresWorkspace)

	bindings := tool.Bindings{
		SessionID: uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		LoopID:    uuid.MustParse("22222222-2222-4222-8222-222222222222"),
		Workspace: &tool.WorkspaceBinding{
			Root:         "/workspace",
			Coordinator:  coordinator{},
			Observations: tool.NewWorkspaceObservations(),
		},
	}
	built, err := effectful.Build(context.Background(), bindings)
	if err != nil {
		panic(err)
	}
	info, err := built[0].Info(context.Background())
	if err != nil {
		panic(err)
	}
	fmt.Println(info.Name, len(built))

	// Output:
	// Tasks true
	// Bash true
	// Bash 1
}

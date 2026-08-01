package tools

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/bash"
	"github.com/looprig/tools/websearch"
)

type fakeReadGuard struct{ maxBytes int64 }

func (*fakeReadGuard) DeniedRead(string) bool { return false }
func (guard *fakeReadGuard) MaxReadBytes() int64 {
	return guard.maxBytes
}

type definitionRunner struct{}

func (*definitionRunner) RunCommand(context.Context, string, string) ([]byte, int, error) {
	return nil, 0, nil
}

type definitionCoordinator struct{}

func (*definitionCoordinator) Acquire(context.Context, tool.WorkspaceOperation, string) (tool.WorkspacePermit, error) {
	return definitionPermit{}, nil
}
func (*definitionCoordinator) Healthy() error { return nil }

type definitionPermit struct{}

func (definitionPermit) Release() {}

func TestDefinitionBlueprints(t *testing.T) {
	t.Parallel()
	guard := &fakeReadGuard{maxBytes: 1024}
	tests := []struct {
		name           string
		definition     tool.Definition
		wantName       string
		wantBuiltNames []string
		requires       tool.Requirements
	}{
		{name: "read file", definition: ReadFileDefinition(guard), wantName: "ReadFile", wantBuiltNames: []string{"ReadFile"}, requires: tool.RequiresWorkspace},
		{name: "write file", definition: WriteFileDefinition(), wantName: "WriteFile", wantBuiltNames: []string{"WriteFile"}, requires: tool.RequiresWorkspace},
		{name: "edit file", definition: EditFileDefinition(), wantName: "EditFile", wantBuiltNames: []string{"EditFile"}, requires: tool.RequiresWorkspace},
		{name: "glob", definition: GlobDefinition(guard), wantName: "Glob", wantBuiltNames: []string{"Glob"}, requires: tool.RequiresWorkspace},
		{name: "grep", definition: GrepDefinition(guard), wantName: "Grep", wantBuiltNames: []string{"Grep"}, requires: tool.RequiresWorkspace},
		{name: "tasks", definition: TaskDefinitions(), wantName: "Tasks", wantBuiltNames: []string{"TaskCreate", "TaskUpdate", "TaskGet", "TaskList"}},
		{name: "ask user", definition: AskUserDefinition(), wantName: "AskUser", wantBuiltNames: []string{"AskUser"}},
		{name: "fetch", definition: FetchDefinition(http.DefaultClient), wantName: "Fetch", wantBuiltNames: []string{"Fetch"}},
		{name: "bash", definition: Bash(bash.WithRunner(&definitionRunner{})), wantName: "Bash", wantBuiltNames: []string{"Bash"}, requires: tool.RequiresWorkspace},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if test.definition.Name() != test.wantName {
				t.Fatalf("Name() = %q, want %q", test.definition.Name(), test.wantName)
			}
			if test.definition.Requirements() != test.requires {
				t.Fatalf("Requirements() = %v, want %v", test.definition.Requirements(), test.requires)
			}
			built, err := test.definition.Build(context.Background(), blueprintBindings())
			if err != nil {
				t.Fatal(err)
			}
			if len(built) != len(test.wantBuiltNames) {
				t.Fatalf("Build() returned %d tools, want %d", len(built), len(test.wantBuiltNames))
			}
			for index, wantName := range test.wantBuiltNames {
				info, err := built[index].Info(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if info.Name != wantName {
					t.Fatalf("built tool[%d] name = %q, want %q", index, info.Name, wantName)
				}
			}
		})
	}
}

func TestDefinitionDependencyValidation(t *testing.T) {
	t.Parallel()
	var typedNilGuard *fakeReadGuard
	var typedNilRunner *definitionRunner
	var typedNilProvider *definitionProvider
	var typedNilClient *http.Client
	tests := []struct {
		name       string
		definition tool.Definition
		dependency string
	}{
		{name: "nil read guard", definition: ReadFileDefinition(nil), dependency: "read_guard"},
		{name: "typed nil read guard", definition: ReadFileDefinition(typedNilGuard), dependency: "read_guard"},
		{name: "nil bash option", definition: Bash(nil), dependency: "option"},
		{name: "typed nil runner", definition: Bash(bash.WithRunner(typedNilRunner)), dependency: "runner"},
		{name: "nil search provider", definition: WebSearchDefinition(nil), dependency: "provider"},
		{name: "typed nil search provider", definition: WebSearchDefinition(typedNilProvider), dependency: "provider"},
		{name: "nil fetch client", definition: FetchDefinition(nil), dependency: "client"},
		{name: "typed nil fetch client", definition: FetchDefinition(typedNilClient), dependency: "client"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := test.definition.Build(context.Background(), blueprintBindings())
			var buildErr *DefinitionBuildError
			if !errors.As(err, &buildErr) {
				t.Fatalf("Build() error = %T %v, want *DefinitionBuildError", err, err)
			}
			if buildErr.Dependency != test.dependency {
				t.Fatalf("dependency = %q, want %q", buildErr.Dependency, test.dependency)
			}
		})
	}
}

func TestDefinitionBashEagerlyResolvesOptions(t *testing.T) {
	t.Parallel()
	applications := 0
	definition := Bash(func(*bash.BashTool) { applications++ })
	if applications != 1 {
		t.Fatalf("option applications after Bash() = %d, want 1", applications)
	}
	for range 2 {
		if _, err := definition.Build(context.Background(), blueprintBindings()); err != nil {
			t.Fatal(err)
		}
	}
	if applications != 1 {
		t.Fatalf("option applications after Build() = %d, want 1", applications)
	}
}

func TestDefinitionConcurrentBuildsAreFresh(t *testing.T) {
	t.Parallel()
	definition := WriteFileDefinition()
	results := make(chan tool.InvokableTool, 2)
	errs := make(chan error, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			built, err := definition.Build(context.Background(), blueprintBindings())
			if err != nil {
				errs <- err
				return
			}
			results <- built[0]
		}()
	}
	group.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	first, second := <-results, <-results
	if first == second {
		t.Fatal("concurrent builds returned the same tool instance")
	}
}

func blueprintBindings() tool.Bindings {
	return tool.Bindings{
		SessionID: uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		LoopID:    uuid.MustParse("22222222-2222-4222-8222-222222222222"),
		Workspace: &tool.WorkspaceBinding{
			Root:         "/workspace",
			Coordinator:  &definitionCoordinator{},
			Observations: tool.NewWorkspaceObservations(),
		},
	}
}

var _ loop.ReadGuard = (*fakeReadGuard)(nil)

// definitionProvider is a minimal SearchProvider (with declared endpoints) for
// building the WebSearch definition.
type definitionProvider struct{}

func (*definitionProvider) Search(context.Context, string, int) ([]websearch.SearchResult, error) {
	return nil, nil
}

func (*definitionProvider) Endpoints() []websearch.Endpoint {
	return []websearch.Endpoint{{Host: "search.example.test", Port: 443}}
}

// TestDefinitionToolsArePrepared proves EVERY tool built from the public
// definitions — including the network tools Bash, Fetch, and WebSearch —
// implements the mandatory preparation capability (tool.CallPreparer): an
// effectful tool without it fails closed in the runner and a pure tool without
// it would be unusable.
func TestDefinitionToolsArePrepared(t *testing.T) {
	t.Parallel()
	guard := &fakeReadGuard{maxBytes: 1024}
	definitions := []tool.Definition{
		ReadFileDefinition(guard),
		WriteFileDefinition(),
		EditFileDefinition(),
		GlobDefinition(guard),
		GrepDefinition(guard),
		TaskDefinitions(),
		AskUserDefinition(),
		Bash(bash.WithRunner(&definitionRunner{})),
		FetchDefinition(http.DefaultClient),
		WebSearchDefinition(&definitionProvider{}),
	}
	for _, definition := range definitions {
		built, err := definition.Build(context.Background(), blueprintBindings())
		if err != nil {
			t.Fatalf("%s: Build() error = %v", definition.Name(), err)
		}
		for _, builtTool := range built {
			if _, ok := builtTool.(tool.CallPreparer); !ok {
				t.Errorf("%s: built tool %T does not implement tool.CallPreparer", definition.Name(), builtTool)
			}
		}
	}
}

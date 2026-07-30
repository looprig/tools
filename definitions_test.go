package tools

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
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

// fakeAsyncProcessRunner is a minimal tool.AsyncProcessRunner test double.
// Task 19's own tests never reach PrepareProcess (that only happens inside
// bash's own runSupervised, at actual invocation, not at Build), so it is
// never called here.
type fakeAsyncProcessRunner struct{}

func (*fakeAsyncProcessRunner) PrepareProcess(context.Context, tool.ProcessRequest) (tool.PreparedProcess, error) {
	return nil, nil
}

var _ tool.AsyncProcessRunner = (*fakeAsyncProcessRunner)(nil)

// fakeProcessRegistry is a tool.SessionResourceRegistry test double that
// mirrors the real registry's get-or-create semantics: the factory runs at
// most once per key, and every caller (regardless of order) receives the
// SAME resource afterward. factoryCalls counts how many times the supplied
// factory actually ran, letting a test prove "separately built definitions
// in one session obtain the same supervisor registry entry" (factoryCalls
// stays 1 across multiple Build calls sharing this registry).
type fakeProcessRegistry struct {
	dir string

	mu           sync.Mutex
	resource     tool.SessionResource
	factoryCalls int
	err          error
}

func (r *fakeProcessRegistry) GetOrCreate(_ context.Context, _ string, factory func(string) (tool.SessionResource, error)) (tool.SessionResource, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	if r.resource == nil {
		r.factoryCalls++
		resource, err := factory(r.dir)
		if err != nil {
			return nil, err
		}
		r.resource = resource
	}
	return r.resource, nil
}

var _ tool.SessionResourceRegistry = (*fakeProcessRegistry)(nil)

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

// blueprintBindingsWithProcess extends blueprintBindings with a
// tool.ProcessBinding over registry, for the process-services-requiring
// definitions (BashDefinition, ProcessOutputDefinition, ProcessInputDefinition,
// ProcessStopDefinition).
func blueprintBindingsWithProcess(registry tool.SessionResourceRegistry) tool.Bindings {
	bindings := blueprintBindings()
	bindings.Process = &tool.ProcessBinding{Registry: registry}
	return bindings
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

// TestBashLegacyDefinitionExcludesProcessServices proves Bash stays
// foreground-only and unchanged by Task 19: it declares no
// tool.RequiresProcessServices bit and builds successfully against bindings
// that carry no Process binding at all — only BashDefinition ever touches
// process services.
func TestBashLegacyDefinitionExcludesProcessServices(t *testing.T) {
	t.Parallel()
	definition := Bash(bash.WithRunner(&definitionRunner{}))
	if definition.Requirements() != tool.RequiresWorkspace {
		t.Fatalf("Requirements() = %v, want %v", definition.Requirements(), tool.RequiresWorkspace)
	}
	bindings := blueprintBindings()
	if bindings.Process != nil {
		t.Fatal("blueprintBindings() unexpectedly set Process")
	}
	if _, err := definition.Build(context.Background(), bindings); err != nil {
		t.Fatal(err)
	}
}

// TestBashDefinitionRequiresProcessServices proves BashDefinition declares
// both tool.RequiresWorkspace and tool.RequiresProcessServices, unlike the
// unchanged legacy Bash.
func TestBashDefinitionRequiresProcessServices(t *testing.T) {
	t.Parallel()
	definition := BashDefinition(func(context.Context, uuid.UUID) (tool.AsyncProcessRunner, error) {
		return &fakeAsyncProcessRunner{}, nil
	})
	if definition.Name() != "Bash" {
		t.Fatalf("Name() = %q, want %q", definition.Name(), "Bash")
	}
	want := tool.RequiresWorkspace | tool.RequiresProcessServices
	if definition.Requirements() != want {
		t.Fatalf("Requirements() = %v, want %v", definition.Requirements(), want)
	}
}

// TestBashDefinitionResolvesRunnerWithValidatedLoopID proves Build calls the
// resolver exactly once, with the exact Harness-validated bindings.LoopID,
// and passes the concrete runner through to build a Bash tool.
func TestBashDefinitionResolvesRunnerWithValidatedLoopID(t *testing.T) {
	t.Parallel()
	var (
		mu    sync.Mutex
		calls int
		gotID uuid.UUID
	)
	runner := &fakeAsyncProcessRunner{}
	definition := BashDefinition(func(_ context.Context, loopID uuid.UUID) (tool.AsyncProcessRunner, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		gotID = loopID
		return runner, nil
	})
	bindings := blueprintBindingsWithProcess(&fakeProcessRegistry{dir: t.TempDir()})

	built, err := definition.Build(context.Background(), bindings)
	if err != nil {
		t.Fatal(err)
	}
	if len(built) != 1 {
		t.Fatalf("Build() returned %d tools, want 1", len(built))
	}
	info, err := built[0].Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "Bash" {
		t.Fatalf("built tool name = %q, want %q", info.Name, "Bash")
	}

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("resolver called %d times, want 1", calls)
	}
	if gotID != bindings.LoopID {
		t.Fatalf("resolver LoopID = %v, want %v", gotID, bindings.LoopID)
	}
}

// TestBashDefinitionRejectsNilResolver proves a nil resolver never produces
// a Bash tool.
func TestBashDefinitionRejectsNilResolver(t *testing.T) {
	t.Parallel()
	definition := BashDefinition(nil)
	bindings := blueprintBindingsWithProcess(&fakeProcessRegistry{dir: t.TempDir()})

	built, err := definition.Build(context.Background(), bindings)
	if built != nil {
		t.Fatalf("Build() tools = %v, want nil", built)
	}
	var buildErr *DefinitionBuildError
	if !errors.As(err, &buildErr) {
		t.Fatalf("Build() error = %T %v, want *DefinitionBuildError", err, err)
	}
	if buildErr.Dependency != "resolver" {
		t.Fatalf("dependency = %q, want %q", buildErr.Dependency, "resolver")
	}
}

// TestBashDefinitionRejectsResolverErrorWithoutBuildingATool proves a
// resolver failure rejects Build without producing a tool and without
// retrying the resolver (exactly one call, matching Build's own single
// invocation).
func TestBashDefinitionRejectsResolverErrorWithoutBuildingATool(t *testing.T) {
	t.Parallel()
	var calls int
	wantErr := errors.New("resolver boom")
	definition := BashDefinition(func(context.Context, uuid.UUID) (tool.AsyncProcessRunner, error) {
		calls++
		return nil, wantErr
	})
	bindings := blueprintBindingsWithProcess(&fakeProcessRegistry{dir: t.TempDir()})

	built, err := definition.Build(context.Background(), bindings)
	if built != nil {
		t.Fatalf("Build() tools = %v, want nil", built)
	}
	var buildErr *DefinitionBuildError
	if !errors.As(err, &buildErr) {
		t.Fatalf("Build() error = %T %v, want *DefinitionBuildError", err, err)
	}
	if buildErr.Dependency != "runner" {
		t.Fatalf("dependency = %q, want %q", buildErr.Dependency, "runner")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("Build() error does not wrap the resolver error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("resolver called %d times, want 1 (no retry after failure)", calls)
	}
}

// TestBashDefinitionRejectsNilOrTypedNilRunner proves a resolver returning a
// nil or typed-nil runner rejects Build without producing a tool.
func TestBashDefinitionRejectsNilOrTypedNilRunner(t *testing.T) {
	t.Parallel()
	var typedNilRunner *fakeAsyncProcessRunner
	tests := []struct {
		name   string
		runner tool.AsyncProcessRunner
	}{
		{name: "nil runner", runner: nil},
		{name: "typed nil runner", runner: typedNilRunner},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			definition := BashDefinition(func(context.Context, uuid.UUID) (tool.AsyncProcessRunner, error) {
				return test.runner, nil
			})
			bindings := blueprintBindingsWithProcess(&fakeProcessRegistry{dir: t.TempDir()})

			built, err := definition.Build(context.Background(), bindings)
			if built != nil {
				t.Fatalf("Build() tools = %v, want nil", built)
			}
			var buildErr *DefinitionBuildError
			if !errors.As(err, &buildErr) {
				t.Fatalf("Build() error = %T %v, want *DefinitionBuildError", err, err)
			}
			if buildErr.Dependency != "runner" {
				t.Fatalf("dependency = %q, want %q", buildErr.Dependency, "runner")
			}
		})
	}
}

// TestBashDefinitionEagerlyResolvesOptions proves BashDefinition resolves
// its bash.BashOptions once, at construction, exactly like Bash's own
// TestDefinitionBashEagerlyResolvesOptions — never reapplying them per Build.
func TestBashDefinitionEagerlyResolvesOptions(t *testing.T) {
	t.Parallel()
	applications := 0
	definition := BashDefinition(
		func(context.Context, uuid.UUID) (tool.AsyncProcessRunner, error) {
			return &fakeAsyncProcessRunner{}, nil
		},
		func(*bash.BashTool) { applications++ },
	)
	if applications != 1 {
		t.Fatalf("option applications after BashDefinition() = %d, want 1", applications)
	}
	bindings := blueprintBindingsWithProcess(&fakeProcessRegistry{dir: t.TempDir()})
	for range 2 {
		if _, err := definition.Build(context.Background(), bindings); err != nil {
			t.Fatal(err)
		}
	}
	if applications != 1 {
		t.Fatalf("option applications after Build() = %d, want 1", applications)
	}
}

// TestBashDefinitionConcurrentBuildsAreFresh proves concurrent Build calls
// each resolve their own runner and produce independent tool instances.
func TestBashDefinitionConcurrentBuildsAreFresh(t *testing.T) {
	t.Parallel()
	var calls int32
	definition := BashDefinition(func(context.Context, uuid.UUID) (tool.AsyncProcessRunner, error) {
		atomic.AddInt32(&calls, 1)
		return &fakeAsyncProcessRunner{}, nil
	})
	bindings := blueprintBindingsWithProcess(&fakeProcessRegistry{dir: t.TempDir()})

	results := make(chan tool.InvokableTool, 2)
	errs := make(chan error, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			built, err := definition.Build(context.Background(), bindings)
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
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("resolver called %d times, want 2 (one per Build)", got)
	}
}

// TestProcessCompanionDefinitionBlueprints proves each companion definition
// produces exactly one tool with the exact expected name, requires only
// tool.RequiresProcessServices, and implements tool.CallPreparer.
func TestProcessCompanionDefinitionBlueprints(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		definition tool.Definition
		wantName   string
	}{
		{name: "process output", definition: ProcessOutputDefinition(), wantName: "ProcessOutput"},
		{name: "process input", definition: ProcessInputDefinition(), wantName: "ProcessInput"},
		{name: "process stop", definition: ProcessStopDefinition(), wantName: "ProcessStop"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if test.definition.Name() != test.wantName {
				t.Fatalf("Name() = %q, want %q", test.definition.Name(), test.wantName)
			}
			if test.definition.Requirements() != tool.RequiresProcessServices {
				t.Fatalf("Requirements() = %v, want %v", test.definition.Requirements(), tool.RequiresProcessServices)
			}
			bindings := blueprintBindingsWithProcess(&fakeProcessRegistry{dir: t.TempDir()})
			built, err := test.definition.Build(context.Background(), bindings)
			if err != nil {
				t.Fatal(err)
			}
			if len(built) != 1 {
				t.Fatalf("Build() returned %d tools, want 1", len(built))
			}
			info, err := built[0].Info(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if info.Name != test.wantName {
				t.Fatalf("built tool name = %q, want %q", info.Name, test.wantName)
			}
			if _, ok := built[0].(tool.CallPreparer); !ok {
				t.Fatalf("%s: built tool %T does not implement tool.CallPreparer", test.wantName, built[0])
			}
		})
	}
}

// TestProcessCompanionDefinitionsRequireNoWorkspace proves the three
// companion definitions are runner-free AND workspace-free: they build
// successfully against bindings that carry only a Process binding, with no
// Workspace and nothing resembling a tool.AsyncProcessRunner anywhere.
func TestProcessCompanionDefinitionsRequireNoWorkspace(t *testing.T) {
	t.Parallel()
	bindings := tool.Bindings{
		SessionID: uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		LoopID:    uuid.MustParse("22222222-2222-4222-8222-222222222222"),
		Process:   &tool.ProcessBinding{Registry: &fakeProcessRegistry{dir: t.TempDir()}},
	}
	for _, definition := range []tool.Definition{ProcessOutputDefinition(), ProcessInputDefinition(), ProcessStopDefinition()} {
		if _, err := definition.Build(context.Background(), bindings); err != nil {
			t.Fatalf("%s: Build() error = %v", definition.Name(), err)
		}
	}
}

// TestProcessCompanionDefinitionsRequireProcessRegistry proves each
// companion definition rejects bindings that carry no Process binding.
func TestProcessCompanionDefinitionsRequireProcessRegistry(t *testing.T) {
	t.Parallel()
	for _, definition := range []tool.Definition{ProcessOutputDefinition(), ProcessInputDefinition(), ProcessStopDefinition()} {
		if _, err := definition.Build(context.Background(), blueprintBindings()); err == nil {
			t.Fatalf("%s: Build() error = nil, want a missing process binding error", definition.Name())
		}
	}
}

// TestProcessCompanionDefinitionsRejectRegistryFailure proves a registry
// GetOrCreate failure rejects Build with a wrapped, typed error.
func TestProcessCompanionDefinitionsRejectRegistryFailure(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("registry unavailable")
	registry := &fakeProcessRegistry{dir: t.TempDir(), err: wantErr}
	bindings := blueprintBindingsWithProcess(registry)

	_, err := ProcessOutputDefinition().Build(context.Background(), bindings)
	var buildErr *DefinitionBuildError
	if !errors.As(err, &buildErr) {
		t.Fatalf("Build() error = %T %v, want *DefinitionBuildError", err, err)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("Build() error does not wrap the registry error: %v", err)
	}
}

// TestProcessCompanionDefinitionsShareOneSupervisorPerSession proves
// "separately built definitions in one session obtain the same supervisor
// registry entry": the shared factory runs exactly once across all three
// companion definitions built against the same registry.
func TestProcessCompanionDefinitionsShareOneSupervisorPerSession(t *testing.T) {
	t.Parallel()
	registry := &fakeProcessRegistry{dir: t.TempDir()}
	bindings := blueprintBindingsWithProcess(registry)

	for _, definition := range []tool.Definition{ProcessOutputDefinition(), ProcessInputDefinition(), ProcessStopDefinition()} {
		if _, err := definition.Build(context.Background(), bindings); err != nil {
			t.Fatalf("%s: Build() error = %v", definition.Name(), err)
		}
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.factoryCalls != 1 {
		t.Fatalf("supervisor factory invoked %d times across 3 separately built definitions, want 1", registry.factoryCalls)
	}
}

// TestProcessCompanionDefinitionsDifferentSessionsGetDifferentSupervisors
// proves "different sessions obtain different supervisors": two definitions
// built against two independent registries never share a *process.Supervisor.
func TestProcessCompanionDefinitionsDifferentSessionsGetDifferentSupervisors(t *testing.T) {
	t.Parallel()
	registryA := &fakeProcessRegistry{dir: t.TempDir()}
	registryB := &fakeProcessRegistry{dir: t.TempDir()}

	if _, err := ProcessOutputDefinition().Build(context.Background(), blueprintBindingsWithProcess(registryA)); err != nil {
		t.Fatal(err)
	}
	if _, err := ProcessOutputDefinition().Build(context.Background(), blueprintBindingsWithProcess(registryB)); err != nil {
		t.Fatal(err)
	}

	registryA.mu.Lock()
	resourceA := registryA.resource
	registryA.mu.Unlock()
	registryB.mu.Lock()
	resourceB := registryB.resource
	registryB.mu.Unlock()

	supervisorA, ok := resourceA.(*processSupervisorResource)
	if !ok {
		t.Fatalf("registryA resource type = %T, want *processSupervisorResource", resourceA)
	}
	supervisorB, ok := resourceB.(*processSupervisorResource)
	if !ok {
		t.Fatalf("registryB resource type = %T, want *processSupervisorResource", resourceB)
	}
	if supervisorA.supervisor == supervisorB.supervisor {
		t.Fatal("two different sessions' registries produced the same *process.Supervisor")
	}
}

// TestProcessSupervisorResourceKeyMatchesBashSupervisedKey pins
// processSupervisorResourceKey against silent drift. bash/supervised.go's
// own unexported supervisorResourceKey must stay byte-for-byte identical for
// "any of the four definitions may win get-or-create" to hold; this test
// cannot reach across packages to compare the two constants directly (bash's
// is unexported), so it pins the known-good literal instead.
func TestProcessSupervisorResourceKeyMatchesBashSupervisedKey(t *testing.T) {
	t.Parallel()
	const bashSupervisorResourceKey = "github.com/looprig/tools/process.supervisor"
	if processSupervisorResourceKey != bashSupervisorResourceKey {
		t.Fatalf("processSupervisorResourceKey = %q, want %q (must match bash/supervised.go's unexported supervisorResourceKey)", processSupervisorResourceKey, bashSupervisorResourceKey)
	}
}

// Package tools provides independent definition builders for Looprig's standard
// tools. Concrete constructors and options live in focused subpackages.
package tools

import (
	"context"
	"net/http"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/askuser"
	"github.com/looprig/tools/bash"
	"github.com/looprig/tools/editfile"
	"github.com/looprig/tools/fetch"
	"github.com/looprig/tools/glob"
	"github.com/looprig/tools/grep"
	"github.com/looprig/tools/internal/definition"
	"github.com/looprig/tools/internal/workspace"
	"github.com/looprig/tools/process"
	"github.com/looprig/tools/readfile"
	"github.com/looprig/tools/task"
	"github.com/looprig/tools/websearch"
	"github.com/looprig/tools/writefile"
)

type DefinitionBuildError = definition.BuildError

func GlobDefinition(readGuard loop.ReadGuard, options ...glob.GlobOption) tool.Definition {
	sealed := append([]glob.GlobOption(nil), options...)
	return tool.NewDefinition("Glob", tool.RequiresWorkspace, func(_ context.Context, bindings tool.Bindings) ([]tool.InvokableTool, error) {
		if workspace.IsNil(readGuard) {
			return nil, &DefinitionBuildError{Definition: "Glob", Dependency: "read_guard"}
		}
		return []tool.InvokableTool{glob.NewGlob(bindings.Workspace.Root, readGuard, sealed...)}, nil
	})
}

func GrepDefinition(readGuard loop.ReadGuard, options ...grep.GrepOption) tool.Definition {
	sealed := append([]grep.GrepOption(nil), options...)
	return tool.NewDefinition("Grep", tool.RequiresWorkspace, func(_ context.Context, bindings tool.Bindings) ([]tool.InvokableTool, error) {
		if workspace.IsNil(readGuard) {
			return nil, &DefinitionBuildError{Definition: "Grep", Dependency: "read_guard"}
		}
		return []tool.InvokableTool{grep.NewGrep(bindings.Workspace.Root, readGuard, sealed...)}, nil
	})
}

func TaskDefinitions() tool.Definition {
	return tool.NewBundleDefinition("Tasks", []string{"TaskCreate", "TaskUpdate", "TaskGet", "TaskList"}, 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
		return task.NewTools(), nil
	})
}

func AskUserDefinition() tool.Definition {
	return tool.NewDefinition("AskUser", 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{askuser.NewAskUser()}, nil
	})
}

func WebSearchDefinition(provider websearch.SearchProvider) tool.Definition {
	return tool.NewDefinition("WebSearch", 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
		if workspace.IsNil(provider) {
			return nil, &DefinitionBuildError{Definition: "WebSearch", Dependency: "provider"}
		}
		return []tool.InvokableTool{websearch.NewWebSearch(provider)}, nil
	})
}

func FetchDefinition(client *http.Client) tool.Definition {
	return tool.NewDefinition("Fetch", 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
		if workspace.IsNil(client) {
			return nil, &DefinitionBuildError{Definition: "Fetch", Dependency: "client"}
		}
		return []tool.InvokableTool{fetch.NewFetch(client)}, nil
	})
}

func ReadFileDefinition(readGuard loop.ReadGuard, options ...readfile.ReadFileOption) tool.Definition {
	sealed := append([]readfile.ReadFileOption(nil), options...)
	return tool.NewDefinition("ReadFile", tool.RequiresWorkspace, func(_ context.Context, bindings tool.Bindings) ([]tool.InvokableTool, error) {
		if workspace.IsNil(readGuard) {
			return nil, &DefinitionBuildError{Definition: "ReadFile", Dependency: "read_guard"}
		}
		return []tool.InvokableTool{readfile.NewReadFile(bindings.Workspace.Root, readGuard, loopObservations(bindings.Workspace.Observations), sealed...)}, nil
	})
}

// WriteFileDefinition accepts writefile.Option values such as
// writefile.WithHostWrites(). Do not pass writefile.WithMutationCoordinator
// here: this entry point already injects the session-bound coordinator, and a
// caller-supplied one is applied after it and silently wins, defeating the
// PathMutation permit and lease-health check with no error.
func WriteFileDefinition(options ...writefile.Option) tool.Definition {
	sealed := append([]writefile.Option(nil), options...)
	return tool.NewDefinition("WriteFile", tool.RequiresWorkspace, func(_ context.Context, bindings tool.Bindings) ([]tool.InvokableTool, error) {
		opts := append([]writefile.Option{writefile.WithMutationCoordinator(bindings.Workspace.Coordinator)}, sealed...)
		return []tool.InvokableTool{writefile.New(bindings.Workspace.Root, loopObservations(bindings.Workspace.Observations), opts...)}, nil
	})
}

// EditFileDefinition accepts editfile.Option values such as
// editfile.WithHostWrites(). Do not pass editfile.WithMutationCoordinator
// here: this entry point already injects the session-bound coordinator, and a
// caller-supplied one is applied after it and silently wins, defeating the
// PathMutation permit and lease-health check with no error.
func EditFileDefinition(options ...editfile.Option) tool.Definition {
	sealed := append([]editfile.Option(nil), options...)
	return tool.NewDefinition("EditFile", tool.RequiresWorkspace, func(_ context.Context, bindings tool.Bindings) ([]tool.InvokableTool, error) {
		opts := append([]editfile.Option{editfile.WithMutationCoordinator(bindings.Workspace.Coordinator)}, sealed...)
		return []tool.InvokableTool{editfile.New(bindings.Workspace.Root, loopObservations(bindings.Workspace.Observations), opts...)}, nil
	})
}

func Bash(options ...bash.BashOption) tool.Definition {
	factory, initErr := bash.NewFactory(options...)
	return tool.NewDefinition("Bash", tool.RequiresWorkspace, func(_ context.Context, bindings tool.Bindings) ([]tool.InvokableTool, error) {
		if initErr != nil {
			return nil, initErr
		}
		return []tool.InvokableTool{factory(bindings.Workspace.Root, bindings.Workspace.Coordinator, bindings.Workspace.Observations)}, nil
	})
}

// AsyncProcessRunnerResolver resolves the concrete tool.AsyncProcessRunner a
// session-supervised Bash definition binds to, from the Harness-validated
// bindings.LoopID at Build (design spec "Workspace coordination": "At
// definition Build, Tools invokes the resolver with the validated
// bindings.LoopID"). Tools owns this resolver shape; a consumer (e.g.
// Coderig) supplies the concrete implementation over its own per-role
// executor set. Runner selection is therefore complete before the concrete
// Bash tool is ever invoked — it never derives from invocation-time
// provenance.
type AsyncProcessRunnerResolver func(context.Context, uuid.UUID) (tool.AsyncProcessRunner, error)

// processSupervisorResourceKey MUST stay byte-for-byte identical to bash's
// own unexported supervisorResourceKey (bash/supervised.go): Bash's
// runSupervised and this package's own ProcessOutputDefinition/
// ProcessInputDefinition/ProcessStopDefinition resolve the SAME
// tool.SessionResourceRegistry entry only by keying on the identical string
// (bash/supervised.go's doc comment: "'any of the four definitions may win
// get-or-create' ... requires all of them to key on this exact same
// string"). TestProcessSupervisorResourceKeyMatchesBashSupervisedKey pins
// this literal against drift.
const processSupervisorResourceKey = "github.com/looprig/tools/process.supervisor"

// BashDefinition builds the session-supervised Bash tool: unlike Bash, a
// SUPERVISED call (background, or a present yield_time_ms — bash/prepare.go's
// normalizeSupervision) routes through the shared, runner-free
// process.Supervisor (bash/supervised.go). resolver supplies the concrete
// tool.AsyncProcessRunner: Build calls it exactly once, only after Harness
// has validated bindings, with the validated bindings.LoopID, and rejects a
// resolver error or a nil/typed-nil returned runner without ever producing a
// tool. options configure the underlying BashTool exactly like Bash's own —
// resolved once here, never reapplied per Build.
func BashDefinition(resolver AsyncProcessRunnerResolver, options ...bash.BashOption) tool.Definition {
	factory, initErr := bash.NewSupervisedFactory(options...)
	return tool.NewDefinition("Bash", tool.RequiresWorkspace|tool.RequiresProcessServices, func(ctx context.Context, bindings tool.Bindings) ([]tool.InvokableTool, error) {
		if initErr != nil {
			return nil, initErr
		}
		if resolver == nil {
			return nil, &DefinitionBuildError{Definition: "Bash", Dependency: "resolver"}
		}
		runner, err := resolver(ctx, bindings.LoopID)
		if err != nil {
			return nil, &DefinitionBuildError{Definition: "Bash", Dependency: "runner", Cause: err}
		}
		if workspace.IsNil(runner) {
			return nil, &DefinitionBuildError{Definition: "Bash", Dependency: "runner"}
		}
		built, err := factory(bindings, runner)
		if err != nil {
			return nil, err
		}
		return []tool.InvokableTool{built}, nil
	})
}

// ProcessOutputDefinition builds the read-only ProcessOutput tool bound to
// this session's shared, runner-free process.Supervisor (process/
// output_tool.go). Argument-free: unlike BashDefinition it captures no
// resolver and no options — a caller's owned process is read through the
// same registry entry Bash and its two sibling definitions share, keyed by
// processSupervisorResourceKey alone.
func ProcessOutputDefinition() tool.Definition {
	return tool.NewDefinition("ProcessOutput", tool.RequiresProcessServices, func(ctx context.Context, bindings tool.Bindings) ([]tool.InvokableTool, error) {
		supervisor, err := resolveProcessSupervisor(ctx, bindings)
		if err != nil {
			return nil, err
		}
		owner := process.Owner{SessionID: bindings.SessionID, LoopID: bindings.LoopID}
		return []tool.InvokableTool{process.NewProcessOutput(supervisor, owner)}, nil
	})
}

// ProcessInputDefinition builds the mutating ProcessInput tool over the same
// shared supervisor entry ProcessOutputDefinition resolves (process/
// input_tool.go). Argument-free for the identical reason.
func ProcessInputDefinition() tool.Definition {
	return tool.NewDefinition("ProcessInput", tool.RequiresProcessServices, func(ctx context.Context, bindings tool.Bindings) ([]tool.InvokableTool, error) {
		supervisor, err := resolveProcessSupervisor(ctx, bindings)
		if err != nil {
			return nil, err
		}
		owner := process.Owner{SessionID: bindings.SessionID, LoopID: bindings.LoopID}
		return []tool.InvokableTool{process.NewProcessInput(supervisor, owner)}, nil
	})
}

// ProcessStopDefinition builds the mutating ProcessStop tool over the same
// shared supervisor entry (process/stop_tool.go). Argument-free for the
// identical reason.
func ProcessStopDefinition() tool.Definition {
	return tool.NewDefinition("ProcessStop", tool.RequiresProcessServices, func(ctx context.Context, bindings tool.Bindings) ([]tool.InvokableTool, error) {
		supervisor, err := resolveProcessSupervisor(ctx, bindings)
		if err != nil {
			return nil, err
		}
		owner := process.Owner{SessionID: bindings.SessionID, LoopID: bindings.LoopID}
		return []tool.InvokableTool{process.NewProcessStop(supervisor, owner)}, nil
	})
}

// resolveProcessSupervisor obtains this session's ONE shared, runner-free
// process.Supervisor session resource, keyed by processSupervisorResourceKey,
// through bindings.Process.Registry — the keyed registry, never a package
// global. Harness's own Definition.Build already validated that
// bindings.Process and its Registry are present before this factory ever
// runs (tool.RequiresProcessServices' bindings validation), so the only
// failures reachable here are the registry's own (e.g. session closing) or an
// unexpected resource already occupying this key.
func resolveProcessSupervisor(ctx context.Context, bindings tool.Bindings) (*process.Supervisor, error) {
	resource, err := bindings.Process.Registry.GetOrCreate(ctx, processSupervisorResourceKey, newProcessSupervisorResource)
	if err != nil {
		return nil, &DefinitionBuildError{Definition: "Process", Dependency: "process_registry", Cause: err}
	}
	sr, ok := resource.(*processSupervisorResource)
	if !ok || sr == nil || sr.supervisor == nil {
		return nil, &DefinitionBuildError{Definition: "Process", Dependency: "process_registry"}
	}
	return sr.supervisor, nil
}

// processSupervisorResource adapts the shared *process.Supervisor to
// tool.SessionResource for this package's own GetOrCreate calls (the three
// companion definitions above).
//
// bash/supervised.go independently resolves and wraps the SAME registry
// entry — processSupervisorResourceKey and bash's own private
// supervisorResourceKey are the identical string — the first time a
// supervised Bash call actually executes, using its own unexported wrapper
// type. Whichever of the four process-backed definitions' GetOrCreate call
// reaches this key FIRST determines the concrete tool.SessionResource value
// every later caller with that key receives back, including a caller in a
// different package.
//
// KNOWN LIMITATION, out of this task's file scope: because bash's wrapper
// type is unexported to package bash, a companion definition here that wins
// the race hands bash's own runSupervised a resource it cannot type-assert
// to its own *supervisorResource, and bash's supervised path then fails
// closed (process_setup_failed) for that session. The design spec
// ("Bash, ProcessOutput, ProcessInput, and ProcessStop can each win the
// registry's get-or-create race because the supervisor contains no runner")
// requires a single shared resource type across both packages to hold in
// every ordering; today that type still lives twice, once here and once in
// bash/supervised.go. Closing this gap needs an exported shared type/factory
// (most naturally added to package process) that bash/supervised.go also
// adopts — both are outside definitions.go/dependency_test.go's file scope
// for this task and are flagged here for the owning phase gate.
type processSupervisorResource struct {
	supervisor *process.Supervisor
}

// Activate is intentionally a no-op: the Supervisor this resource wraps is
// constructed notification-free (newProcessSupervisorResource passes nil for
// both NewSupervisor's lifecycle and notifications parameters), mirroring
// bash/supervised.go's own supervisorResource.Activate.
func (r *processSupervisorResource) Activate(context.Context, tool.SessionResourceServices) error {
	return nil
}

// Shutdown releases every resource the shared Supervisor still holds.
func (r *processSupervisorResource) Shutdown(ctx context.Context) error {
	return r.supervisor.Shutdown(ctx)
}

// newProcessSupervisorResource is the tool.SessionResourceRegistry.GetOrCreate
// factory for processSupervisorResourceKey: it is runner-free (constructs no
// tool.AsyncProcessRunner and calls neither PrepareProcess nor Start), so any
// of ProcessOutputDefinition/ProcessInputDefinition/ProcessStopDefinition may
// win the get-or-create race for a session's shared supervisor. dir is the
// private per-session storage directory the registry reserves for this key.
func newProcessSupervisorResource(dir string) (tool.SessionResource, error) {
	manifests := process.NewManifestStore(dir)
	supervisor, err := process.NewSupervisor(process.Config{}, manifests, dir, nil, nil)
	if err != nil {
		return nil, err
	}
	return &processSupervisorResource{supervisor: supervisor}, nil
}

var _ tool.SessionResource = (*processSupervisorResource)(nil)

func loopObservations(shared tool.WorkspaceObservations) tool.WorkspaceObservations {
	if workspace.IsNil(shared) {
		return tool.NewWorkspaceObservations()
	}
	return shared
}

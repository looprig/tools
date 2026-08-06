// Package tools provides independent definition builders for Looprig's standard
// tools. Concrete constructors and options live in focused subpackages.
package tools

import (
	"context"
	"net/http"

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

func loopObservations(shared tool.WorkspaceObservations) tool.WorkspaceObservations {
	if workspace.IsNil(shared) {
		return tool.NewWorkspaceObservations()
	}
	return shared
}

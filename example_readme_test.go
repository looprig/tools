package tools_test

// example_readme_test.go compiles the code snippets shown in README.md so the
// documentation cannot drift from the real API. Each snippet below mirrors the
// README verbatim (modulo the surrounding declarations these compile checks
// need). Update the README and this file together.

import (
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools"
	"github.com/looprig/tools/bash"
	"github.com/looprig/tools/permission"
)

// readmeSelectTools is the README's tool-selection snippet.
func readmeSelectTools(readGuard loop.ReadGuard) loop.Option {
	return loop.WithTools(
		tools.ReadFileDefinition(readGuard),
		tools.GlobDefinition(readGuard),
		tools.GrepDefinition(readGuard),
		tools.TaskDefinitions(),
	)
}

// readmeWorkspaceStore is the README's permission-store snippet.
func readmeWorkspaceStore(permissionFilePath string) (*permission.Store, []permission.Diagnostic, error) {
	store, diagnostics, err := permission.NewWorkspaceStore(permission.Config{
		Path: permissionFilePath, // one explicit absolute path; never discovered
	})
	return store, diagnostics, err
}

// readmeBash is the README's Bash composition snippet.
func readmeBash(confinedRunner tool.CommandRunner, familyEligible permission.FamilyEligibility) tool.Definition {
	return tools.Bash(
		bash.WithRunner(confinedRunner),
		bash.WithFamilyCatalog(familyEligible),
	)
}

// Reference the helpers so the compile checks survive unused-linting.
var _ = readmeSelectTools
var _ = readmeWorkspaceStore
var _ = readmeBash

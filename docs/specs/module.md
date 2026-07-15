# Tools Module Specification

## Purpose

Move reusable tool implementations out of the harness into `github.com/looprig/tools`.

The harness is the runtime for building Loops and Rigs. Tools are optional capabilities selected by a consumer. A consumer may import the standard tools module, use another tool library, or implement every tool directly against the harness contracts.

## Core decision

Every tool is an independent definition. There is no `Files` bundle.

The standard module exposes separate definitions for:

- ReadFile
- WriteFile
- EditFile
- Glob
- Grep
- Bash
- WebSearch
- Fetch
- Todo
- AskUser
- Skill

Consumers select only what a Loop needs. A read-only reviewer imports `ReadFile` directly and never constructs mutating tools.

## Shared workspace state

Independent definitions still need safe coordination. Read freshness, mutations, and Bash invalidation currently share observations through a private implementation hidden by `Files`.

That state already has a correct home: `tool.WorkspaceBinding`.

The harness creates the workspace binding and supplies:

- root
- mutation coordinator
- workspace observations

Each independent tool definition reads those dependencies from `tool.Bindings`. No bundled constructor and no consumer-owned type assertion are required.

The observation implementation becomes internal harness workspace machinery or a public contract implementation under `pkg/tool`. The harness must not import the optional tools module to create it.

## Public definitions

Definitions use names that describe the tool, not the constructor mechanism:

```go
tools.ReadFileDefinition(readGuard)
tools.WriteFileDefinition()
tools.EditFileDefinition()
tools.GlobDefinition(readGuard)
tools.GrepDefinition(readGuard, options...)
tools.Bash(options...)
tools.TodoDefinition()
tools.AskUserDefinition()
```

Each function returns one `tool.Definition`, and each definition builds a fresh `tool.InvokableTool` from the current `tool.Bindings`.

Concrete constructors may remain available for testing and advanced composition, but their dependencies must use exported interfaces. No public constructor may require an unexported concrete type.

Concrete tools and advanced options live in focused packages: `readfile`, `writefile`, `editfile`, `glob`, `grep`, `bash`, `fetch`, `websearch`, `todo`, `askuser`, and `skill`. The permission policy subsystem lives in `permission`. Shared security-sensitive mechanics that are not consumer contracts remain beneath `internal`.

## Permission policy

The module contains the standard permission checker, hard-deny policy, matching, approval persistence, and posture support because those policies describe the standard tools.

The permission API should use names scoped to permission behavior:

```go
type PermissionOption func(*permissionConfig)

func NewPermissionChecker(
    policy PermissionPolicy,
    options ...PermissionOption,
) (*PermissionChecker, error)
```

`PolicyFingerprint` remains available. Application code must not manually duplicate its hashing logic. Loop definitions already include produced tool names in their fingerprint, so policy revision hashing should cover permission policy and application-owned policy identity only.

## Core delegation tool

The Subagent control surface is part of harness delegation, not an optional utility tool. The harness may inject an internal implementation when a Loop declares delegates. That implementation must depend only on harness contracts.

The optional tools module must not be imported by the harness to implement delegation. This avoids a module cycle and keeps delegation available to Rigs that do not use the standard utility tools.

## Migration

1. Move standard tool implementations and their tests to the new module.
2. Replace harness workspace observation construction with an internal implementation behind `tool.WorkspaceObservations`.
3. Move the Subagent implementation behind harness delegation internals.
4. Delete `harness/pkg/tools` after all production and tests stop importing it.
5. Update CodeRig and consumer examples to import `github.com/looprig/tools`.
6. Add dependency tests proving harness does not import tools, sandbox, or confinement.

No compatibility shim is required. The project has no external consumers of the old package.

## Consumer documentation

The project documentation must show two paths:

### Use a standard tool

Select an individual definition and add it to a Loop with `loop.WithTools`.

### Create a tool

Implement `tool.InvokableTool` and wrap it with `tool.NewDefinition`. Explain:

- metadata and JSON schema
- capability classification
- binding requirements
- effect and permission behavior
- audit summaries
- context cancellation
- deterministic errors
- tests for malformed input and denied effects

The documentation must make clear that importing the standard tools module is optional.

## Verification

- every exported definition produces exactly one named tool
- consumers can select ReadFile without constructing WriteFile or EditFile
- file tools share binding observations correctly
- Bash invalidates the same workspace observations
- harness has no import of `github.com/looprig/tools`
- CodeRig has no local generic tool-definition wrappers
- the new module passes race, lint, security, and integration tests

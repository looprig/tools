# Tool Package Layout Design

## Goal

Make the tools module reflect its core contract: every standard tool is independently selectable. Keep the module root small, place each tool in its own package, and keep shared implementation details private.

## Public layout

The module root remains the consumer-facing definition facade. It exports one `tool.Definition` builder per standard tool and the typed error produced when a definition cannot be built.

Each concrete tool lives in its own package:

- `askuser`
- `bash`
- `editfile`
- `fetch`
- `glob`
- `grep`
- `readfile`
- `skill`
- `todo`
- `websearch`
- `writefile`

The permission system is a separate `permission` package because it is a cohesive policy subsystem rather than an invokable tool. DuckDuckGo search belongs to `websearch` as one provider implementation.

The root definition facade keeps the existing names such as `ReadFileDefinition`, `WriteFileDefinition`, and `Bash`. Consumers that only assemble Loops continue to import `github.com/looprig/tools`. Consumers that customize construction import the focused package that owns the option or implementation.

## Internal layout

Shared implementation that is not a consumer contract moves under `internal`:

- `internal/workspace` owns canonical path containment and glob matching used by file, command, and permission packages.
- `internal/filemutation` owns structured mutation coordination shared by WriteFile and EditFile.
- `internal/hashcache` remains the private permission hash cache.

Shared behavior is internal only when two or more packages require it. Tool-specific parsing, rendering, errors, and tests remain beside the tool that owns them.

## Dependency direction

The root facade may import tool packages. Tool packages may import harness contracts and shared internal packages. Tool packages must not import the root facade or one another except for a narrow, explicit shared contract. No production package may import harness internals, sandbox, confinement, or carbon.

`confinement` will use `bash`, `grep`, and `permission` directly for their extension options. `carbon` will use the root definition facade plus `permission`, `skill`, and `websearch` where it needs advanced construction.

## Compatibility

The standard definition-builder API remains at module root. Accidental root-level concrete constructors and policy types are intentionally moved to their owning packages before the tools module receives its first commit. First-party consumers are updated in the same change.

## Verification

The refactor is complete when:

- the root contains only facade, documentation, module, build, and boundary files;
- every invokable tool implementation and its tests live in its own package;
- first-party consumers compile against the focused APIs;
- dependency tests enforce the direction above;
- formatting, vet, Staticcheck, Gosec, module verification, race tests, and builds pass;
- Govulncheck remains configured but is documented as locally runnable because tenant policy blocks dependency metadata egress from this environment.

# tools

`github.com/looprig/tools` provides optional standard tools for looprig Loops. The harness defines the contracts. This module provides implementations that consumers can select one at a time.

```go
loop.WithTools(
	tools.ReadFileDefinition(readGuard),
	tools.GlobDefinition(readGuard),
	tools.GrepDefinition(readGuard),
	tools.TodoDefinition(),
)
```

There is no bundled file-tool definition. A read-only Loop can receive ReadFile without also constructing WriteFile or EditFile. Consumers can mix these tools with their own definitions or use no standard tools at all.

The module root is intentionally a small definition facade. Each concrete tool has a focused package, such as `readfile`, `writefile`, `grep`, `bash`, and `websearch`. Policy composition lives in `permission`, while shared containment and mutation mechanics remain private under `internal`.

Most consumers only need the root package. Advanced composition imports the package that owns the required extension point:

```go
runnerOption := bash.WithRunner(commandRunner)
policyOption := permission.WithPosture(posture, commandRunner)
```

See the [module specification](docs/specs/module.md) and the [consumer guide](https://github.com/looprig/looprig/blob/main/docs/consumers/tools.md).

Run the full local security suite with:

```bash
make secure
go test -race ./...
```

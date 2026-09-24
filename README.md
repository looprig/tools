# tools

`github.com/looprig/tools` provides optional standard tools for looprig Loops. The harness defines the contracts. This module provides implementations that consumers can select individually, plus the deliberate related-family `Tasks` bundle.

```sh
go get github.com/looprig/tools@latest
```

Pin it together with `harness` v0.40.0 or later: the retained-output support below depends on it.

```go
loop.WithTools(
	tools.ReadFileDefinition(readGuard),
	tools.GlobDefinition(readGuard),
	tools.GrepDefinition(readGuard),
	tools.TaskDefinitions(),
)
```

`TaskDefinitions()` produces the four model-facing tools `TaskCreate`,
`TaskUpdate`, `TaskGet`, and `TaskList`. They are one deliberate bundle because
the four operations must share one bounded, Loop-local task graph. Each
definition build creates a fresh graph; parent and child Loops are isolated,
while modes within one Loop share the graph. The Harness owns and injects the
agent-tool bundle (`StartAgent`, `MessageAgent`, `ListAgents`, `StopAgent`) for
delegation; consumers must not add delegation tools from this module.

There is no bundled file-tool definition. A read-only Loop can receive ReadFile without also constructing WriteFile or EditFile. Consumers can mix these tools with their own definitions or use no standard tools at all.

The module root is intentionally a small definition facade (`ReadFileDefinition`, `WriteFileDefinition`, `EditFileDefinition`, `GlobDefinition`, `GrepDefinition`, `Bash`, `BashDefinition`, `ProcessOutputDefinition`, `ProcessInputDefinition`, `ProcessStopDefinition`, `FetchDefinition`, `WebSearchDefinition`, `AskUserDefinition`, `TaskDefinitions`, `ReadToolResultDefinition`). Each concrete tool has a focused package:

| Package | Purpose |
|---|---|
| `readfile`, `writefile`, `editfile` | Workspace-contained file read, write and edit. |
| `glob`, `grep` | Workspace-contained filename and content search (Grep prefers ripgrep, with a stdlib fallback). |
| `bash` | Single-command shell execution with a bounded timeout and capped combined output. |
| `process` | Long-running command supervision (`BashDefinition` plus the `Process*` tools); see `docs/specs/long-running-command-supervision.md`. |
| `fetch`, `websearch` | One bounded HTTP request via an injected `*http.Client`; web search over an injected `SearchProvider`. |
| `askuser` | The AskUser tool. |
| `skill` | On-demand reader of curated embedded (and optionally workspace) `SKILL.md` bodies. |
| `task` | The four related task operations. |
| `readtoolresult` | `read_tool_result`, which pages a retained tool result. |
| `permission` | The shared workspace permission rule store. |

Shared containment and mutation mechanics remain private under `internal`. Runnable examples live under `examples/` (`definitions`, `permissions`, `preparation`, `processes`, `skills`, `tasks`).

All README snippets are compiled by `example_readme_test.go` at the module root.

## Tool preparation

Every tool is a `tool.CallPreparer`. `PrepareCall` owns the whole preparation boundary: it decodes and validates the untrusted arguments once, normalizes commands, URLs, and paths, resolves canonical resource identities, and emits one typed `tool.Request` listing every capability `Requirement` the call needs. Invalid input fails during preparation and never reaches the permission gate. Execution consumes the typed prepared artifact bound to the call — the raw arguments are never reparsed — and a prepared tool that runs without its artifact fails closed.

Tools classify capabilities; they never decide Deny, Gated, or Allow. That three-state decision belongs to the harness gate evaluator, which consumes requests structurally without any tool-specific field extraction.

## The permission package

`permission` implements the single hardened workspace permission store of the access-profile specification. It stores capability rules — kind (`command.execute`, `network`, `filesystem.read`, `filesystem.write`), effect (`allow` or `deny`, deny always beats allow), enforcement class, and match — under the strict schema-version-2 JSON codec.

```go
store, diagnostics, err := permission.NewWorkspaceStore(permission.Config{
	Path: permissionFilePath, // one explicit absolute path; never discovered
})
```

Hardening: the store serves exactly one explicit permission-file path (it never computes HOME-relative or implicit locations), requires owner-only `0600` files, bounds file size, re-reads the file per query in interactive mode (so concurrent processes observe each other's atomically renamed updates), loads one immutable snapshot in read-only headless mode, and persists approved allow candidates atomically under an interprocess lock. Any load failure fails closed as an error.

Bash command rules come in three enforcement classes: an exact normalized command, the wildcard `Bash(*)`, and the token-prefix family `Bash(git log:*)`. Family matching is per shell segment: the normalized command is split at `&&`, `||`, `;`, `|`, `|&`, `&`, newline, and subshell boundaries, and a family covers a segment only when the segment is a provably simple command whose leading bare literal tokens equal the family tokens exactly — token equality, never string prefix. Anything the conservative grammar cannot prove simple (substitution, redirection, dynamic expansion, ambiguous quoting, …) is matchable only by a wildcard or an exact rule. Everything fails closed.

The automatic-family eligibility catalog is injected by the consumer (`Config.FamilyEligible`); a manually authored allow family outside the catalog stays authoritative but produces a non-fatal `Diagnostic` the consumer must surface. Deny families never warn.

The harness gate consumes the store structurally as its rule matcher and writer; deny-before-allow ordering belongs to the gate, and the store answers both queries independently.

## Bash access declarations

A Bash call may carry a structured `access` declaration of the filesystem and network deltas the command needs. The declaration **requests** authority — it never grants it. Each declared delta becomes one more requirement in the same typed request, so a gated command and its deltas share a single combined approval; an omitted gated delta stays OS-blocked by the sandbox at run time, and the model retries with a new call that declares the needed capability. Grants are minted only after the gate's decision, and command issuance is always exact-command even when a wildcard or family rule satisfied the decision.

```go
tools.Bash(
	bash.WithRunner(confinedRunner),
	bash.WithFamilyCatalog(familyEligible),
)
```

## Shared network capability

Bash network deltas, Fetch, and WebSearch all emit the same `network` capability kind with the same canonical target match encoding, so one saved workspace rule for a host and port serves all three tools. Fetch derives its single endpoint from the validated URL; WebSearch emits one requirement per endpoint its injected `SearchProvider` declares, and the provider fails closed on any secondary target outside that declaration.

## Retained tool output

Bash implements Harness's streaming capture (`tool.CapturingInvokableTool`). While a command runs, its complete combined output streams to the capture sink, and only then does Bash apply its own 32 KiB head/tail preview. A supervised command streams its retained process spool the same way. When nothing was elided, the captured bytes equal the returned result byte for byte, so Harness retains no object for an ordinary small command.

Every standard definition declares its capture safety (`tool.CaptureSafetyDeclarer`):

- Bash and BashDefinition stream only under direct `sh -c` execution. With an injected runner (`bash.WithRunner`, including a granted sandbox runner), `tool.CommandRunner` returns the whole output at once, so the result is resident before capture and the definition declares itself materialized and high-output.
- ReadFile, Grep, ProcessOutput and ProcessInput are materialized and high-output.
- Every other tool is small by construction.

Direct `sh -c` execution runs the command in its own process group:

- A timed-out or cancelled command kills the whole group, including detached or `nohup`'d jobs. Only `setsid` escapes it.
- Once `sh` has exited, a descendant still holding the output pipe keeps the call open for at most two seconds. A background job that writes after that loses its output.
- A terminal Ctrl-C does not reach a running Bash command in a non-raw CLI; context cancellation still kills it.

`ReadToolResultDefinition()` provides `read_tool_result`, which the model uses to page through a result that Harness retained in full:

```go
loop.WithTools(
	tools.Bash(),
	tools.ReadToolResultDefinition(),
)
```

- The tool declares `tool.RequiresToolResultReader`, so Harness binds it to a reader scoped to the calling loop.
- It accepts exactly `{capture_id, offset?, max_bytes?}`. The model can name a capture only by the id that a retention marker prints.
- Unknown or foreign ids fail closed, and so do malformed arguments.
- A rig must wire `rig.WithToolResultObjects` to register the tool. Without it, loop definition fails.

Consumer obligations for retained output: wire `rig.WithToolResultObjects`, register `read_tool_result` only where capture is wired, and set a finite `ToolLimits.ResultBytes` (with it zero nothing is elided, so nothing is retained). Keep the capture ceiling (`ToolLimits.CaptureBytes`) at or below what the serving Factory will verify when reading objects back (64 MiB by default).

Known limit: the `limit_bytes` argument of ProcessOutput and ProcessInput is not clamped.

## Fail-closed properties

- Invalid or unparseable arguments fail during preparation; nothing reaches the gate or the filesystem.
- A prepared tool invoked without its typed artifact refuses to run.
- Permission-file load failures (missing when required, wrong mode, oversized, malformed, unsupported schema or normalization version) are errors, never empty-rule successes, in interactive mode.
- Unsegmentable or unprovably simple shell input never matches a family rule.
- Definition builders reject nil (including typed-nil) dependencies at build time with a `DefinitionBuildError`.

See the access-profile specification (`docs/plans/access-profiles.md` in the `carbon` repository) for the cross-module design, and the historical [module specification](docs/specs/module.md) for the original extraction plan.

## Where it sits

Tier 4 in the Looprig graph. Direct Looprig dependencies: `core` and `harness`, plus test-only `inference` and `storage`. Consumed by `carbon` and `tests`.

## Development

Go 1.26.8 baseline. Verify standalone against the pinned dependencies, then run the checks:

```bash
GOWORK=off go test ./...
make test     # go test -race ./...
make secure   # fmt-check, vet, staticcheck, gosec, go mod verify, govulncheck
make check    # the CI surface: fmt-check, vet, staticcheck, gosec, govulncheck, test, build
```

## License

Apache License 2.0. See `LICENSE`.

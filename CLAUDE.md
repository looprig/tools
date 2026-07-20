# Repository instructions

This module provides optional standard tools for looprig consumers. The harness defines the contracts. Consumers choose these tools individually or implement their own.

## Ownership

- This module owns tool preparation and normalization (each tool's `PrepareCall` decodes, validates, and canonicalizes its arguments once and emits one typed `tool.Request`) and durable rule storage (the `permission` package's hardened workspace store).
- The harness owns permission evaluation and routing: the gate evaluator decides Deny/Gated/Allow over prepared requests (deny before allow) and consumes the store structurally. Tools never make that decision.
- The sandbox module owns access profiles and OS enforcement. Bash access declarations request authority; grants are minted after the gate's decision and enforced by the sandbox, never by this module.
- The `permission` package is the shared rule library, not a tool: its canonical requirement-match encodings are the pinned contract between tool preparation and stored-rule matching.

## Boundaries

- Export one `tool.Definition` per tool. Do not bundle unrelated tools.
- Read workspace root, observations, coordinators, ceiling, and delegates from `tool.Bindings`.
- Depend on harness contracts, never harness internals.
- Do not import sandbox or confinement. Accept their behavior through harness runner and permission interfaces.
- Tool packages stay independent of each other: no public tool package imports a sibling public tool package. The one allowlisted sibling import is `permission` (the shared rule library); shared mechanics live under `internal`. `dependency_test.go` at the module root enforces this — extend it, never weaken it, when adding packages.
- Keep tool effects explicit and fail secure when permission or containment is uncertain: invalid input fails during preparation, a prepared tool without its typed artifact refuses to run, and rule-file load failures are errors.
- Deliberate exception, recorded here: Bash hands the model-supplied command to `sh -c` (an argv list cannot express shell features). The security boundary is the permission gate over the prepared command-backed request plus the injected confined runner, not the argv shape.

## Testing

- Run `GOWORK=off go test -race ./...` (and `-tags integration` for the integration suite).
- Use table-driven tests when cases share structure and focused tests for singular behavior.
- Cover malformed input, cancellation, containment, permission denial, audit output, and concurrency where relevant.
- Use typed errors when callers need stable classification or recovery.
- README code snippets are compiled by `example_readme_test.go`; keep them in lockstep.

## Style

- Keep packages cohesive and public APIs small.
- Introduce interfaces at consumer boundaries or when multiple implementations justify them.
- Split functions for clear ownership and control flow, not an arbitrary line count.
- Run `gofmt` on changed Go files.

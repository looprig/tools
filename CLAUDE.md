# Repository instructions

This module provides optional standard tools for looprig consumers. The harness defines the contracts. Consumers choose these tools individually or implement their own.

## Boundaries

- Export one `tool.Definition` per tool. Do not bundle unrelated tools.
- Read workspace root, observations, coordinators, ceiling, and delegates from `tool.Bindings`.
- Depend on harness contracts, never harness internals.
- Do not import sandbox or confinement. Accept their behavior through harness runner and permission interfaces.
- Keep tool effects explicit and fail secure when permission or containment is uncertain.

## Testing

- Run `go test -race ./...`.
- Use table-driven tests when cases share structure and focused tests for singular behavior.
- Cover malformed input, cancellation, containment, permission denial, audit output, and concurrency where relevant.
- Use typed errors when callers need stable classification or recovery.

## Style

- Keep packages cohesive and public APIs small.
- Introduce interfaces at consumer boundaries or when multiple implementations justify them.
- Split functions for clear ownership and control flow, not an arbitrary line count.
- Run `gofmt` on changed Go files.

# Loop-Scoped Task Tools Design

**Date:** 2026-07-30

**Status:** Approved

## Goal

Replace the current multiplexed `Todo` capability with a Claude-compatible
structured task-tool family while keeping task state entirely inside the tools
module.

Tasks are not part of the Loop domain or session runtime. Each bound Loop owns
an independent in-memory task graph. Task state is shared across that Loop's
modes but not across Loops, and it intentionally does not survive process
restart or session restoration in this version.

## Model-Facing Tools

One bundle definition produces four tools backed by the same loop-local store:

- `TaskCreate`
- `TaskUpdate`
- `TaskGet`
- `TaskList`

The consumer-facing constructor is:

```go
tools.TaskDefinitions()
```

The bundle is a deliberate exception to the tools module's default
one-definition-per-tool convention. These tools are one capability family rather
than independently selectable capabilities: they must share one store whose
lifetime is exactly one Loop binding. A bundle expresses that ownership without
promoting task state into the harness.

The bundle is built once for each Loop binding. Its factory creates one
mutex-protected task store and constructs all four tools over that store.

### TaskCreate

```json
{
  "subject": "Implement parser validation",
  "description": "Add validation and focused tests.",
  "activeForm": "Implementing parser validation",
  "blockedBy": ["optional-task-id"],
  "metadata": {
    "component": "parser"
  }
}
```

`subject` and `description` are required non-empty strings. `activeForm`,
`blockedBy`, and `metadata` are optional.

The tool returns structured JSON containing the created task.

### TaskUpdate

```json
{
  "taskId": "task-id",
  "subject": "Updated subject",
  "description": "Updated description",
  "activeForm": "Updated active form",
  "status": "in_progress",
  "addBlockedBy": ["another-task-id"],
  "removeBlockedBy": ["old-task-id"],
  "metadata": {
    "component": "runtime"
  }
}
```

`taskId` is required. Every other field is optional. Status accepts
`pending`, `in_progress`, `completed`, or `deleted`. `deleted` is a command
that removes the task rather than a persisted status.

Updates are patches: omitted scalar fields remain unchanged. Supplied metadata
replaces the existing metadata object. Dependency additions and removals are
applied atomically with the rest of the update.

### TaskGet

```json
{
  "taskId": "task-id"
}
```

The tool returns the complete task, including derived `blocks` information.

### TaskList

```json
{}
```

The tool returns all tasks in deterministic order. The result includes each
task's status, dependencies, and derived `blocks` list.

## Task Model

```go
type taskRecord struct {
	ID          string
	Subject     string
	Description string
	ActiveForm  string
	Status      Status
	BlockedBy   []string
	Metadata    json.RawMessage
}

type Task struct {
	ID          string          `json:"id"`
	Subject     string          `json:"subject"`
	Description string          `json:"description"`
	ActiveForm  string          `json:"activeForm,omitempty"`
	Status      Status          `json:"status"`
	BlockedBy   []string        `json:"blockedBy,omitempty"`
	Blocks      []string        `json:"blocks,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
}
```

IDs are UUID strings. Status is one of `pending`, `in_progress`, or
`completed`.

Only `taskRecord` values are stored. A task's `Blocks` list is derived while
building the returned `Task` view by finding records whose `BlockedBy` list
contains its ID. This avoids maintaining two inverse collections that can drift
and prevents an output-only field from entering stored state.

Metadata must be a JSON object. Preparation rejects `null`, arrays, scalars,
malformed values, and values above the metadata limit. The canonical JSON bytes
are stored and defensively copied. On update, omission preserves metadata and an
explicit `{}` clears it.

All returned slices and metadata bytes are defensive copies.

## Resource Limits

The store is bounded even though it is in-memory and loop-local. V1 uses private,
fixed limits rather than public configuration:

```go
const (
	maxTaskArgsBytes     = 64 << 10
	maxTasksPerLoop      = 256
	maxSubjectBytes      = 512
	maxDescriptionBytes  = 16 << 10
	maxActiveFormBytes   = 512
	maxMetadataBytes     = 16 << 10
	maxDependencies      = 128
	maxStoreBytes        = 2 << 20
)
```

Preparation rejects a raw argument document above `maxTaskArgsBytes` before
decoding it. The aggregate store size is the sum of the stored IDs, strings,
dependency IDs, and canonical metadata bytes. Create and update validate both
per-field limits and the aggregate candidate graph before committing. Limit
failures leave the graph unchanged. These limits also bound `TaskList` output to
a practical size.

## Scope and Lifetime

The bundle factory receives `tool.Bindings`, which already includes the
`SessionID` and `LoopID`. No new harness capability or requirement bit is
needed.

The mutable store belongs to the built bundle instance:

```text
Session
├── Loop A binding
│   └── task store A
└── Loop B binding
    └── task store B
```

All modes within Loop A reuse task store A because Loop binding builds each
definition once and reuses its concrete tools across resolved modes. Consumers
that explicitly list the Tasks bundle in multiple modes must reuse the same
`tool.Definition` value; constructing distinct definitions with the same name is
rejected by Loop binding.

A restored Loop receives a newly built, empty task store. Persistence may be
added later behind the same private store interface without changing the
model-facing schemas.

## Dependency Rules

- Every referenced dependency must already exist in the same Loop's store.
- A task cannot depend on itself.
- An update that would introduce a dependency cycle is rejected atomically.
- A task with any non-completed dependency cannot transition to
  `in_progress`.
- Completing a dependency does not automatically change dependent task
  statuses. It only makes them eligible to start.
- Deleting a task removes its ID from every remaining task's `BlockedBy` list.
- Duplicate dependency IDs are normalized away.

## Concurrency

The store uses one mutex to protect the complete graph. Create, update,
delete, dependency validation, cycle detection, and result snapshotting happen
under that mutex.

Using one graph-level critical section keeps dependency updates atomic and is
appropriate for the bounded task counts expected within one agent Loop.

All four tools also implement `tool.Sequential`. The runner therefore executes
task calls in model call order. This makes dependent operations deterministic:
for example, completing a blocker and then starting its dependent in one batch
always observes the completion first. The mutex remains necessary for direct
callers, tests, and future execution paths.

## Preparation, Execution, and Errors

Each concrete tool owns a typed prepared artifact that embeds
`tool.TokenArtifact` and contains its fully decoded, validated, and normalized
arguments. `PrepareCall`:

1. strictly decodes exactly one JSON object and rejects unknown fields;
2. validates required fields, UUID syntax, status values, metadata shape and
   size, field sizes, and dependency-count limits;
3. parses task IDs to canonical UUID strings, de-duplicates and sorts dependency
   slices, and preserves accepted model-supplied text verbatim;
4. returns a pure `tool.Request` with the concrete tool name, no requirements,
   and the typed artifact.

`InvokableRun` never reparses `argsJSON`. It retrieves the artifact from
`loop.PreparedCallFromContext` and fails closed with a model-visible error if
the artifact is absent, nil, or belongs to another task tool.

Preparation cannot validate facts that may change before execution. Unknown
task IDs, unknown dependency IDs, cycles, aggregate-store limits, and blocked
status transitions are checked atomically against live store state in
`InvokableRun`.

Preparation failures are model-safe Go errors returned by `PrepareCall`; the
harness renders them to the model and never executes the tool. Live-state
validation failures are model-visible tool-result strings with no Go error.
Structured successful results are JSON.

Schemas set `additionalProperties: false`, and runtime preparation independently
rejects unknown fields.

## Compatibility

The existing `Todo` definition remains available during migration. It is not
automatically included with the new task bundle, and the two stores do not
share state.

Consumers migrate from:

```go
tools.TodoDefinition()
```

to:

```go
tools.TaskDefinitions()
```

The old tool can be deprecated and removed separately after consumers have
migrated.

## Testing

Tests cover:

- exact tool names, descriptions, and JSON schemas;
- typed preparation artifacts and normalized requests;
- rejection during preparation of malformed, trailing, unknown, missing,
  wrong-type, oversized, and invalid-enum inputs;
- fail-closed execution with missing, nil, or cross-tool artifacts;
- proof that changing raw arguments after preparation does not change execution;
- one shared store across all four tools in one binding;
- isolation between separate Loop bindings;
- mode reuse through the existing definition-build cache using the same
  definition value;
- deterministic call-order behavior through `tool.Sequential`;
- create defaults and UUID generation;
- patch semantics and metadata replacement;
- get and deterministic list ordering;
- deletion cleanup;
- missing and duplicate dependency handling;
- self-dependency and multi-node cycle rejection;
- blocked transition rejection and eligibility after completion;
- malformed JSON and unknown-field rejection;
- defensive copies;
- raw-argument, field-size, dependency, task-count, and aggregate-store limits;
- concurrent access under `go test -race`;
- the existing `Todo` compatibility surface.

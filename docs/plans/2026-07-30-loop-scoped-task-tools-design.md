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
type Task struct {
	ID          string
	Subject     string
	Description string
	ActiveForm  string
	Status      Status
	BlockedBy   []string
	Metadata    map[string]any
}
```

IDs are UUID strings. Status is one of `pending`, `in_progress`, or
`completed`.

Only `BlockedBy` is stored. A task's `Blocks` list is derived by finding tasks
whose `BlockedBy` list contains its ID. This avoids maintaining two inverse
collections that can drift.

All returned slices and metadata maps are defensive copies.

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
definition once and reuses its concrete tools across resolved modes.

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

## Errors and Results

Like the existing standard tools, model-correctable failures are returned as
tool-result strings rather than Go errors. Structured successful results are
JSON.

Failures include:

- malformed or unknown input fields;
- missing required fields;
- unknown task IDs;
- unknown dependency IDs;
- invalid statuses;
- self-dependencies;
- dependency cycles;
- attempts to start blocked tasks.

Schemas set `additionalProperties: false`, and runtime decoding also rejects
unknown fields.

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
- one shared store across all four tools in one binding;
- isolation between separate Loop bindings;
- mode reuse through the existing definition-build cache;
- create defaults and UUID generation;
- patch semantics and metadata replacement;
- get and deterministic list ordering;
- deletion cleanup;
- missing and duplicate dependency handling;
- self-dependency and multi-node cycle rejection;
- blocked transition rejection and eligibility after completion;
- malformed JSON and unknown-field rejection;
- defensive copies;
- concurrent access under `go test -race`;
- the existing `Todo` compatibility surface.

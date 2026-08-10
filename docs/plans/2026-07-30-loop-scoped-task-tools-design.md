# Loop-Scoped Task Tools Design

**Date:** 2026-07-30

**Revised:** 2026-07-31

**Status:** Approved

## Goal

Replace the multiplexed `Todo` capability with a structured task-tool family
backed by one bounded in-memory graph per bound Loop.

This is a greenfield hard cut. The `Todo` package, `TodoDefinition`, examples,
tests, documentation, and consumer wiring are removed in the same change. There
is no compatibility shim and no second task store.

The model-facing API is inspired by Claude's task-tool family, but Looprig keeps
its stronger graph operations, UUID validation, explicit dependency removal,
atomic state validation, and complete structured success responses. It is not
advertised as wire-compatible with Claude Agent SDK task tools.

## Ownership Boundaries

Task tracking is an optional standard utility owned by `github.com/looprig/tools`.
Tasks are not part of the Loop domain, session runtime, journal, or restore
protocol. The Harness supplies only its existing tool contracts and binding
identity.

The model-facing `Subagent` tool remains Harness-owned. Delegation is an
intrinsic control-plane capability: Harness owns child-Loop creation and restore,
parent-scoped authorization, depth and quota enforcement, request correlation,
follow-up, status, wait, and interrupt. A delegate-bearing Loop receives the
Harness tool automatically; a Loop without delegates receives no Subagent tool.
Moving that adapter into the optional tools module would invert the dependency
direction and could let consumer wiring drift from the frozen delegation
topology.

The two families interact only through normal model behavior. A parent or child
may report task progress through `Subagent` messages, but neither tool imports or
calls the other.

## Scope and Lifetime

One bundle definition produces four tools backed by the same Loop-local store:

- `TaskCreate`
- `TaskUpdate`
- `TaskGet`
- `TaskList`

The consumer-facing constructor is:

```go
tools.TaskDefinitions()
```

The bundle is the deliberate exception to the tools module's default
one-definition-per-independently-selectable-tool convention. The four operations
are one capability family and must share one store whose lifetime is exactly one
definition build.

```text
Session
├── Parent Loop binding
│   └── parent task store
├── Operator child Loop binding
│   └── operator task store
└── Reviewer child Loop binding
    └── reviewer task store
```

Every bound Loop, including a subagent, receives an independent task graph when
its definition selects `TaskDefinitions()`. Parent and child graphs never share
state. Coordination crosses that boundary through Subagent messages.

All modes within one Loop reuse its store because Loop binding builds one
definition value once and reuses the concrete tools across resolved modes.
Consumers selecting Tasks in multiple modes must reuse the same definition
value. Distinct definitions with the same name are rejected by Loop binding.

A restored Loop receives a newly built, empty store. Task state intentionally
does not survive restart or restore in this version.

## Model-Facing Operations

All schemas use `type: "object"` and `additionalProperties: false`. Runtime
preparation independently enforces byte limits, whitespace rules, UUID syntax,
and metadata shape.

### TaskCreate

Description: `Create one task in this Loop's private task graph.`

```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "subject": {"type": "string", "minLength": 1, "description": "Brief task title."},
    "description": {"type": "string", "minLength": 1, "description": "Detailed task requirements."},
    "activeForm": {"type": "string", "description": "Present-continuous text shown while the task is active."},
    "blockedBy": {"type": "array", "items": {"type": "string"}, "maxItems": 128, "description": "Existing task UUIDs that must complete first."},
    "metadata": {"type": "object", "description": "Optional structured task metadata."}
  },
  "required": ["subject", "description"]
}
```

```json
{
  "subject": "Implement parser validation",
  "description": "Add validation and focused tests.",
  "activeForm": "Implementing parser validation",
  "blockedBy": ["optional-task-uuid"],
  "metadata": {
    "component": "parser"
  }
}
```

`subject` and `description` are required, non-empty, non-whitespace strings.
`activeForm`, `blockedBy`, and `metadata` are optional. Accepted text is stored
verbatim after validation. Dependencies must already exist in the same store.

Success returns the complete created task:

```json
{"task":{"id":"uuid","subject":"...","description":"...","status":"pending"}}
```

### TaskUpdate

Description: `Patch, transition, rewire, or delete one task in this Loop's private task graph.`

```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "taskId": {"type": "string", "description": "Task UUID to update."},
    "subject": {"type": "string", "description": "Replacement task title."},
    "description": {"type": "string", "description": "Replacement task requirements."},
    "activeForm": {"type": "string", "description": "Replacement present-continuous activity text."},
    "status": {"type": "string", "enum": ["pending", "in_progress", "completed", "deleted"], "description": "Replacement status, or deleted to remove the task."},
    "addBlockedBy": {"type": "array", "items": {"type": "string"}, "maxItems": 128, "description": "Existing task UUIDs to add as dependencies."},
    "removeBlockedBy": {"type": "array", "items": {"type": "string"}, "maxItems": 128, "description": "Task UUIDs to remove as dependencies."},
    "metadata": {"type": "object", "description": "Replacement metadata object; an empty object clears metadata."}
  },
  "required": ["taskId"]
}
```

```json
{
  "taskId": "task-uuid",
  "subject": "Updated subject",
  "description": "Updated description",
  "activeForm": "Updating task",
  "status": "in_progress",
  "addBlockedBy": ["another-task-uuid"],
  "removeBlockedBy": ["old-task-uuid"],
  "metadata": {
    "component": "runtime"
  }
}
```

`taskId` is required. Every other field is optional. Status accepts `pending`,
`in_progress`, `completed`, or `deleted`. `deleted` removes the task rather than
becoming stored state.

Updates are patches. Omitted scalar fields and metadata remain unchanged.
Supplied metadata replaces the existing object. An explicit `{}` clears metadata
to absent stored state, so later responses omit the field. Dependency additions
and removals are normalized and applied atomically with the rest of the patch.
If the same dependency appears in both lists, removal wins; this rule is pinned
by tests.

Ordinary success returns the complete updated task. Deletion returns:

```json
{"deletedTaskId":"uuid"}
```

### TaskGet

Description: `Get one complete task from this Loop's private task graph.`

```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "taskId": {"type": "string", "description": "Task UUID to retrieve."}
  },
  "required": ["taskId"]
}
```

```json
{"taskId":"task-uuid"}
```

Success returns `{"task":{...}}` with the complete task and derived `blocks`
information. An unknown ID is a model-visible error result rather than a
successful null response.

### TaskList

Description: `List every task in this Loop's private task graph in deterministic order.`

```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {}
}
```

```json
{}
```

Success returns `{"tasks":[]}` or all tasks in deterministic UUID order. Every
task includes its status, dependencies, derived `blocks`, and optional metadata.

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

IDs are canonical UUID strings. Persisted status is `pending`, `in_progress`, or
`completed`.

Only `taskRecord` values are stored. `Blocks` is derived while building a
response by finding records whose `BlockedBy` contains the task ID. This avoids
maintaining inverse collections that can drift and prevents an output-only field
from entering stored state.

Metadata must be a JSON object. Preparation rejects `null`, arrays, scalars,
malformed values, and oversized values. Canonical JSON bytes are stored and
defensively copied. All returned slices and metadata bytes are defensive copies.

## Dependency and State Invariants

- Every referenced dependency already exists in the same Loop's store.
- A task cannot depend on itself.
- A candidate graph containing a dependency cycle is rejected atomically.
- An `in_progress` task may have only completed dependencies.
- Transitioning a blocked task to `in_progress` is rejected.
- Adding an incomplete dependency to an already `in_progress` task is rejected.
- Completing a dependency makes dependents eligible but does not change them.
- Deleting a task removes its ID from every remaining `BlockedBy` list.
- Duplicate dependency IDs are normalized away and sorted.
- Removal wins when one update both adds and removes the same dependency.

## Resource Limits

The Loop-local store is bounded with private V1 constants:

```go
const (
	maxTaskArgsBytes    = 64 << 10
	maxTasksPerLoop     = 256
	maxSubjectBytes     = 512
	maxDescriptionBytes = 16 << 10
	maxActiveFormBytes  = 512
	maxMetadataBytes    = 16 << 10
	maxDependencies     = 128
	maxStoreBytes       = 2 << 20
)
```

Preparation rejects a raw argument document above `maxTaskArgsBytes` before
decoding. The aggregate size is the sum of stored IDs, strings, dependency IDs,
and canonical metadata. Create and update validate per-field and aggregate
limits against a candidate graph before commit. Limit failures leave the graph
unchanged.

The ID source is injectable in tests and defaults to `uuid.New`. Generation
failure and collision both reject creation without mutation. Production does not
retry collisions: fail-closed behavior is simpler and deterministic.

## Concurrency and Call Ordering

One mutex protects the complete graph. Create, update, delete, dependency
validation, cycle detection, candidate-size validation, and result snapshotting
occur under the mutex.

All four tools implement `tool.Sequential`. Task calls in one model batch execute
in their original relative order, so completing a blocker before starting a
dependent observes the completion. The mutex remains necessary for direct
callers, concurrent tests, and future execution paths.

## Preparation, Execution, and Errors

Each concrete tool owns a typed prepared artifact embedding `tool.TokenArtifact`.
`PrepareCall`:

1. rejects oversized raw arguments;
2. strictly decodes exactly one non-null JSON object;
3. rejects trailing JSON and unknown fields;
4. validates required-field presence, types, whitespace, UUID syntax, status,
   metadata shape, per-field sizes, and dependency counts;
5. canonicalizes UUIDs and metadata, and de-duplicates and sorts dependencies;
6. returns `tool.Request{ToolName: concreteName}` with no requirements plus the
   concrete typed artifact.

`InvokableRun` never reparses raw arguments. It retrieves only its own artifact
through the existing prepared-call helper and fails closed when the artifact is
missing, nil, or belongs to another Task tool.

Preparation failures are typed, model-safe Go errors. Facts depending on current
state—unknown IDs, cycles, blocked-state invariants, aggregate limits, ID-source
failure, and collision—are checked atomically during execution and returned as
model-visible error results with no Go error. Errors never echo task text or
metadata. Successful results are structured JSON in one text block.

Audit summaries are the concrete tool name only. They never contain subject,
description, active form, IDs, dependencies, or metadata.

## Consumer and Presentation Migration

CodeRig replaces Todo with the Tasks bundle in both operator and reviewer tool
definitions. Because those definitions back the primary and delegated Loops,
the primary operator, operator children, and reviewer children all receive the
four tools and each receives a fresh graph.

The TUI removes Todo-specific summary parsing. Task cards display their tool name
without reconstructing model-supplied task text or identifiers, including nested
subagent cards.

Public tools and consumer documentation remove Todo entirely, describe Tasks as
the deliberate related-family bundle, and document Loop-local subagent scope.
Harness documentation records `Subagent` as the deliberate Harness-owned
model-facing control capability.

## Testing

Tests cover:

- exact names, descriptions, schemas, required fields, and success shapes;
- malformed, trailing, null, scalar, array, unknown-field, missing, wrong-type,
  oversized, invalid-enum, and invalid-UUID inputs;
- typed prepared artifacts and pure requests;
- missing, nil, and cross-tool artifact rejection;
- execution independence from changed raw arguments;
- create defaults, UUID generation failure, and UUID collision;
- patch and metadata replacement/clear semantics;
- unknown IDs and unknown create/update dependencies;
- duplicate dependencies, add/remove overlap, self-dependency, and cycles;
- active-task dependency invariants and completion eligibility;
- deletion cleanup and deterministic list ordering;
- defensive copies and every resource limit;
- concurrent direct access under the race detector;
- one shared graph across one Loop's modes;
- distinct graphs across two Loop bindings;
- distinct graphs across a CodeRig parent, operator child, and reviewer child;
- presence of all four tools and absence of Todo in every migrated roster;
- redacted live and reconstructed TUI summaries;
- complete removal of the Todo package, symbol, documentation, and consumer use.

Before feature work, the tools module manifest is synchronized with the current
sibling module graph so required `GOWORK=off` verification runs cleanly.

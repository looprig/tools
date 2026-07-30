# Loop-Scoped Task Tools Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add `TaskCreate`, `TaskUpdate`, `TaskGet`, and `TaskList` as a Claude-compatible tool bundle backed by one bounded, loop-scoped in-memory task graph.

**Architecture:** A new `task` package owns an unexported mutex-protected store and four sequential concrete tools. Each tool strictly prepares a typed artifact and executes only that artifact. `tools.TaskDefinitions()` returns one deliberate `tool.NewBundleDefinition` exception whose per-binding factory creates a fresh bounded store and all four tools over it; the harness and Loop domain remain unchanged.

**Tech Stack:** Go 1.26, `encoding/json`, `sync.Mutex`, Looprig `tool.Definition`, `tool.InvokableTool`, `tool.CallPreparer`, `tool.Sequential`, and `core/uuid`.

---

### Task 1: Define the stored record, response model, and cloning rules

**Files:**
- Create: `task/model.go`
- Create: `task/model_test.go`

**Step 1: Write the failing model tests**

Add table tests that pin:

```go
func TestStatusValid(t *testing.T) {
	tests := []struct {
		status Status
		want   bool
	}{
		{StatusPending, true},
		{StatusInProgress, true},
		{StatusCompleted, true},
		{Status("deleted"), false},
		{Status("unknown"), false},
	}
	// Assert status.valid() for every row.
}
```

Add:

- `TestCloneRecordDeepCopiesDependenciesAndMetadata`, using canonical
  `json.RawMessage` metadata and proving that mutating cloned bytes and slices
  does not affect the source;
- `TestTaskFromRecordDerivesOutputOnlyFields`, proving `Blocks` appears only in
  the response view and cannot be persisted.

**Step 2: Run the tests to verify they fail**

Run: `GOWORK=off go test -race ./task`

Expected: FAIL because package `task`, `Status`, `Task`, and `cloneRecord` do not
exist.

**Step 3: Implement the minimal model**

Create:

```go
package task

type Status string

const (
	StatusPending    Status = "pending"
	StatusInProgress Status = "in_progress"
	StatusCompleted  Status = "completed"
)

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

Implement `Status.valid`, `cloneRecord`, and `taskFromRecord`. Clone metadata
with `append(json.RawMessage(nil), source...)`. `Blocks` exists only on the
response model.

**Step 4: Run the tests to verify they pass**

Run: `GOWORK=off go test -race ./task`

Expected: PASS.

**Step 5: Commit**

```bash
git add task/model.go task/model_test.go
git commit -m "feat(task): define task value model"
```

### Task 2: Build the loop-local store and basic create/read/list behavior

**Files:**
- Create: `task/store.go`
- Create: `task/store_test.go`

**Step 1: Write failing store tests**

Cover:

- create defaults status to `pending`;
- create generates a non-empty UUID;
- blank subject and description are rejected;
- get rejects an unknown ID;
- list returns tasks in deterministic ID order;
- get/list return defensive copies;
- `Blocks` is derived from other tasks' `BlockedBy` values;
- task count, subject, description, active-form, metadata, dependency-count, and
  aggregate-store limits reject the candidate without mutation.

Use a deterministic ID seam:

```go
type idSource func() (uuid.UUID, error)

store := newStore(sequenceIDs(idA, idB))
```

Keep the production default wired to `uuid.New`.

**Step 2: Run the tests to verify they fail**

Run: `GOWORK=off go test -race ./task -run 'TestStore(Create|Get|List|DerivedBlocks|Limits)'`

Expected: FAIL because `store`, `newStore`, and its operations do not exist.

**Step 3: Implement the store**

Add:

```go
type store struct {
	mu    sync.Mutex
	newID idSource
	tasks map[string]taskRecord
}

type createInput struct {
	Subject     string
	Description string
	ActiveForm  string
	BlockedBy   []string
	Metadata    json.RawMessage
}
```

Define the exact private V1 bounds:

```go
const (
	maxTaskArgsBytes     = 64 << 10
	maxTasksPerLoop      = 256
	maxSubjectBytes      = 512
	maxDescriptionBytes = 16 << 10
	maxActiveFormBytes   = 512
	maxMetadataBytes     = 16 << 10
	maxDependencies      = 128
	maxStoreBytes        = 2 << 20
)
```

Implement `create`, `get`, and `list`. Hold the graph mutex through validation,
mutation, derived-block calculation, and snapshot cloning.

Normalize dependency slices by removing duplicates and sorting IDs.
`validateCandidateGraph` must check the aggregate byte count before assigning
the candidate map to `store.tasks`.

**Step 4: Run the tests to verify they pass**

Run: `GOWORK=off go test -race ./task -run 'TestStore(Create|Get|List|DerivedBlocks|Limits)'`

Expected: PASS.

**Step 5: Commit**

```bash
git add task/store.go task/store_test.go
git commit -m "feat(task): add in-memory task store"
```

### Task 3: Add dependency validation and atomic updates

**Files:**
- Modify: `task/store.go`
- Modify: `task/store_test.go`

**Step 1: Write failing dependency and update tests**

Add focused tests for:

- scalar patch fields leave omitted fields unchanged;
- metadata replacement is deep-copied;
- unknown status is rejected;
- unknown dependency is rejected;
- self-dependency is rejected;
- a two-node and a three-node cycle are rejected;
- rejected updates leave the graph unchanged;
- a blocked task cannot become `in_progress`;
- completing its last blocker makes it eligible but leaves it `pending`;
- dependency additions/removals are normalized;
- deleting a task removes it and cleans references from remaining tasks;
- an update exceeding a field, dependency, or aggregate-store bound is
  rejected atomically.

Pin deletion as a command, not a persisted status:

```go
updated, deleted, err := store.update(updateInput{
	TaskID: taskID,
	Status: statusPointer(StatusCommandDeleted),
})
```

**Step 2: Run the tests to verify they fail**

Run: `GOWORK=off go test -race ./task -run 'TestStore(Update|Dependency|Cycle|Blocked|Delete|Limits)'`

Expected: FAIL because update behavior is missing.

**Step 3: Implement atomic update behavior**

Add:

```go
const StatusCommandDeleted Status = "deleted"

type updateInput struct {
	TaskID          string
	Subject         *string
	Description     *string
	ActiveForm      *string
	Status          *Status
	AddBlockedBy    []string
	RemoveBlockedBy []string
	Metadata        *json.RawMessage
}
```

Build a candidate graph while holding the mutex. Validate referenced IDs,
self-dependencies, cycles with a bounded DFS over `BlockedBy`, and all resource
limits. Commit the candidate only after every check passes.

For deletion, remove the target task and its ID from every remaining
`BlockedBy` slice before committing.

**Step 4: Run the tests to verify they pass**

Run: `GOWORK=off go test -race ./task -run 'TestStore(Update|Dependency|Cycle|Blocked|Delete|Limits)'`

Expected: PASS.

**Step 5: Commit**

```bash
git add task/store.go task/store_test.go
git commit -m "feat(task): add task updates and dependencies"
```

### Task 4: Add strict decoding, metadata, result, and audit helpers

**Files:**
- Create: `task/tool.go`
- Create: `task/tool_test.go`

**Step 1: Write failing helper tests**

Test that:

- decoding rejects malformed JSON, trailing JSON, and unknown fields;
- metadata preparation accepts only objects, canonicalizes them, distinguishes
  omission from `{}`, rejects `null`, and enforces `maxMetadataBytes`;
- JSON results contain exactly one text block holding valid JSON;
- model-safe preparation errors have a stable concrete type;
- audit summaries contain only the tool name and never model-supplied task text.

**Step 2: Run the tests to verify they fail**

Run: `GOWORK=off go test -race ./task -run 'Test(Decode|Metadata|JSONResult|PrepareError|Audit)'`

Expected: FAIL because the helpers do not exist.

**Step 3: Implement minimal helpers**

Implement a generic strict-object decoder that first rejects argument documents
above `maxTaskArgsBytes`, then uses `json.Decoder.DisallowUnknownFields`, rejects
top-level `null`, arrays, and scalars, and requires EOF after the one decoded
object. Add a `jsonResult` helper that marshals a value and returns
`tool.TextResult(string(encoded))`.

Add `canonicalMetadata(json.RawMessage) (json.RawMessage, error)`. It must enforce
`maxMetadataBytes` before and after decoding, reject explicit `null` and
non-object JSON, decode exactly one value, re-encode the object, and return owned
bytes. Absence remains nil; an explicit `{}` remains a present empty object.

Add a private model-safe typed preparation error:

```go
type prepareError struct {
	toolName string
	reason   string
}

func (e *prepareError) Error() string {
	return e.toolName + ": " + e.reason
}
```

Create a small embedded metadata base:

```go
type toolBase struct {
	name string
	desc string
}

func (b toolBase) AuditSummary(string) string { return b.name }
```

Do not implement `PrepareCall` on `toolBase`: each concrete tool must own its
argument type and typed prepared artifact. Each concrete tool still implements
its own `Info` so its exact schema remains obvious and testable.

**Step 4: Run the tests to verify they pass**

Run: `GOWORK=off go test -race ./task -run 'Test(Decode|Metadata|JSONResult|PrepareError|Audit)'`

Expected: PASS.

**Step 5: Commit**

```bash
git add task/tool.go task/tool_test.go
git commit -m "feat(task): add strict task tool helpers"
```

### Task 5: Implement TaskCreate and TaskGet

**Files:**
- Create: `task/create.go`
- Create: `task/create_test.go`
- Create: `task/get.go`
- Create: `task/get_test.go`

**Step 1: Write failing preparation, execution, and schema tests**

For both tools, assert:

- exact tool name and description;
- schema has `type: object` and `additionalProperties: false`;
- required fields are exact;
- malformed, trailing, unknown, missing, wrong-type, invalid-UUID, and oversized
  inputs return `*prepareError` from `PrepareCall` and never reach execution;
- `PrepareCall` returns a pure request with the concrete tool name and a non-nil
  artifact of the tool's private type;
- execution without a prepared call, with a nil artifact, or with the other
  tool's artifact fails closed as model-visible error text;
- execution ignores a different raw argument string after preparation;
- both tools satisfy `tool.Sequential` and return `true`;
- successful output is structured JSON.

Pin these input shapes:

```json
{"subject":"S","description":"D","activeForm":"Doing S","blockedBy":[],"metadata":{}}
```

```json
{"taskId":"uuid"}
```

**Step 2: Run the tests to verify they fail**

Run: `GOWORK=off go test -race ./task -run 'TestTask(Create|Get)'`

Expected: FAIL because the concrete tools do not exist.

**Step 3: Implement TaskCreate and TaskGet**

Each tool holds `*store`, embeds `toolBase`, and implements `Info`,
`PrepareCall`, `InvokableRun`, and `Sequential`.

Use presence-aware decoded argument structs and private prepared artifacts:

```go
type taskCreateArtifact struct {
	tool.TokenArtifact
	input createInput
}

type taskGetArtifact struct {
	tool.TokenArtifact
	taskID string
}
```

`PrepareCall` strictly decodes and validates the arguments, canonicalizes
metadata and dependencies, and returns `tool.Request{ToolName: concreteName}`
plus the artifact. `InvokableRun` ignores its raw string argument and retrieves
the expected artifact with `prepared.FromContext`. Store-dependent failures
remain model-visible tool-result strings.

Each tool satisfies:

```go
var (
	_ tool.InvokableTool = (*TaskCreate)(nil)
	_ tool.CallPreparer  = (*TaskCreate)(nil)
	_ tool.Auditable     = (*TaskCreate)(nil)
	_ tool.Sequential    = (*TaskCreate)(nil)
)
```

Implement `Sequential() bool { return true }` for both tools.

Return successful create/get results as:

```json
{"task":{...complete task...}}
```

**Step 4: Run the tests to verify they pass**

Run: `GOWORK=off go test -race ./task -run 'TestTask(Create|Get)'`

Expected: PASS.

**Step 5: Commit**

```bash
git add task/create.go task/create_test.go task/get.go task/get_test.go
git commit -m "feat(task): add create and get tools"
```

### Task 6: Implement TaskList and TaskUpdate

**Files:**
- Create: `task/list.go`
- Create: `task/list_test.go`
- Create: `task/update.go`
- Create: `task/update_test.go`

**Step 1: Write failing tool tests**

Pin:

- `TaskList` prepares only a non-null empty object, returns a typed non-nil
  artifact, and returns `{"tasks":[]}` when empty;
- list output is deterministic and includes derived `blocks`;
- `TaskUpdate` rejects malformed, trailing, unknown, missing, wrong-type,
  invalid-UUID, invalid-status, invalid-metadata, and oversized input during
  preparation;
- both tools fail closed without their own prepared artifact and ignore changed
  raw arguments after preparation;
- both tools satisfy `tool.Sequential` and return `true`;
- every patch field maps to `updateInput`;
- `status:"deleted"` returns a structured deletion result;
- store validation failures remain model-visible tool-result errors.

Use this successful deletion shape:

```json
{"deletedTaskId":"uuid"}
```

**Step 2: Run the tests to verify they fail**

Run: `GOWORK=off go test -race ./task -run 'TestTask(List|Update)'`

Expected: FAIL because the tools do not exist.

**Step 3: Implement TaskList and TaskUpdate**

Use pointer fields in the decoded update arguments to distinguish omitted scalar
fields from explicit empty strings. Decode metadata into `json.RawMessage`:
nil means omitted, `{}` means clear, and `null` is rejected during preparation.
The prepared update artifact owns the normalized `updateInput`.

Add:

```go
type taskListArtifact struct {
	tool.TokenArtifact
}

type taskUpdateArtifact struct {
	tool.TokenArtifact
	input updateInput
}
```

Like TaskCreate and TaskGet, `InvokableRun` consumes only these artifacts.
Preserve the exact camelCase field names from the approved design.

Add the same compile-time capability assertions used by TaskCreate and TaskGet,
including `tool.Sequential`, and implement `Sequential() bool { return true }`.

**Step 4: Run the tests to verify they pass**

Run: `GOWORK=off go test -race ./task -run 'TestTask(List|Update)'`

Expected: PASS.

**Step 5: Commit**

```bash
git add task/list.go task/list_test.go task/update.go task/update_test.go
git commit -m "feat(task): add list and update tools"
```

### Task 7: Expose the four-tool bundle and prove Loop isolation

**Files:**
- Modify: `definitions.go`
- Modify: `definitions_test.go`
- Modify: `dependency_test.go`
- Create: `task_bundle_test.go`

**Step 1: Write failing public-definition tests**

Add expectations:

```go
definition := TaskDefinitions()

if definition.Name() != "Tasks" { ... }
if diff := cmp.Diff(
	[]string{"TaskCreate", "TaskUpdate", "TaskGet", "TaskList"},
	definition.ProducedToolNames(),
); diff != "" { ... }
if definition.Requirements() != 0 { ... }
```

Build once, invoke `TaskCreate`, then prove the built `TaskList` sees the
created task. Drive every invocation through
`PrepareCall → loop.WithPreparedCall → InvokableRun`.

Build the definition a second time with a different `LoopID` and prove its list
is empty. Also run concurrent create/update/list calls under the race detector.

**Step 2: Run the tests to verify they fail**

Run: `GOWORK=off go test -race . -run 'TestTaskDefinition|TestTaskBundle'`

Expected: FAIL because `TaskDefinitions` does not exist.

**Step 3: Implement the bundle factory**

In `definitions.go`, import `github.com/looprig/tools/task` and add:

```go
func TaskDefinitions() tool.Definition {
	return tool.NewBundleDefinition(
		"Tasks",
		[]string{"TaskCreate", "TaskUpdate", "TaskGet", "TaskList"},
		0,
		func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
			return task.NewTools(), nil
		},
	)
}
```

`task.NewTools` creates one private store and returns the four tools in the
declared order. Every definition build creates a fresh store, which makes each
Loop binding independent.

Do not add the bundle to `TestDefinitionBlueprints` and do not weaken that
test's one-definition/one-tool assertion. Add a dedicated
`TestTaskDefinitionsBundle` covering its four declared and built names.

Add `task` to `TestToolPackageLayout`. The existing dependency walker must
continue proving that `task` imports no sibling public tool packages. Add
`TaskDefinitions()` to the all-tools-prepared test and assert every built task
tool has a non-nil typed artifact for valid input.

**Step 4: Run the tests to verify they pass**

Run: `GOWORK=off go test -race . -run 'TestTaskDefinition|TestTaskBundle|TestDefinition|TestToolPackageLayout|TestProductionDependencyBoundary'`

Expected: PASS.

**Step 5: Commit**

```bash
git add definitions.go definitions_test.go dependency_test.go task_bundle_test.go
git commit -m "feat: expose loop-scoped task tool bundle"
```

### Task 8: Prove one bundle instance is reused across Loop modes

**Files:**
- Modify: `task_bundle_test.go`

**Step 1: Write the binding integration tests**

Assign `tasks := TaskDefinitions()` once. Define a Loop whose base mode and
alternate mode both select that exact `tasks` value. Bind the Loop once, obtain
the base tool set from `bound.Tools()` and the alternate tool set from
`bound.Mode(alternateName)`, create a task through the base `TaskCreate`, and
verify the alternate mode's `TaskList` sees it.

Create a second bound Loop with another `LoopID` and prove it starts empty.
Also construct a negative fixture with two distinct `TaskDefinitions()` values
under the same definition name and assert binding rejects it rather than
silently sharing or duplicating state.

**Step 2: Run the test to verify current wiring**

Run: `GOWORK=off go test -race . -run 'TestTaskBundle(SharesStateAcrossModes|RejectsDistinctSameNameDefinitions)' -v`

Expected before any fix: the sharing test passes if the existing
definition-build cache behaves as documented, and the distinct-definition test
passes by observing the existing duplicate-definition error. If either fails,
stop and inspect `loop.Definition.Bind`; do not add task state to
`tool.Bindings` or the Loop runtime.

**Step 3: Make only the minimal correction if the test exposes a defect**

The expected implementation needs no production change: one immutable bundle
definition must appear in every selected mode so `Bind` builds it once by
definition name and reuses its concrete tools.

**Step 4: Re-run the focused test**

Run: `GOWORK=off go test -race . -run 'TestTaskBundle(SharesStateAcrossModes|RejectsDistinctSameNameDefinitions)' -v`

Expected: PASS.

**Step 5: Commit**

```bash
git add task_bundle_test.go
git commit -m "test: prove task state follows loop binding"
```

### Task 9: Update public documentation while preserving Todo compatibility

**Files:**
- Modify: `CLAUDE.md`
- Modify: `CONTRIBUTING.md`
- Modify: `README.md`
- Modify: `example_readme_test.go`
- Modify: `docs/specs/module.md`

**Step 1: Update the compile-checked README example**

Replace the primary task example:

```go
tools.TodoDefinition(),
```

with:

```go
tools.TaskDefinitions(),
```

Add a short compatibility note that `TodoDefinition` remains available but
uses a separate legacy in-memory list.

State that standard tools remain individually selectable by default and Tasks
is the single deliberate related-family bundle.

**Step 2: Update repository policy and the module specification**

Change the one-definition-per-tool rule consistently in `CLAUDE.md`,
`CONTRIBUTING.md`, and `docs/specs/module.md`:

> Export one `tool.Definition` per independently selectable tool by default.
> Never bundle unrelated tools. `Tasks` is the deliberate exception: its four
> model-facing operations form one capability family and require one per-build
> loop-local store.

Keep the existing Files non-bundle decision and all sibling-package dependency
rules unchanged.

List all four produced model-facing names and their scope/lifetime.

**Step 3: Run documentation compile tests**

Run: `GOWORK=off go test -race . -run 'TestExample|TestDefinition|TestToolPackageLayout|TestProductionDependencyBoundary'`

Expected: PASS.

**Step 4: Run formatting**

Run: `GOWORK=off make fmt`

Expected: command succeeds and formats changed Go files.

**Step 5: Commit**

```bash
git add CLAUDE.md CONTRIBUTING.md README.md example_readme_test.go docs/specs/module.md
git commit -m "docs: document structured task tools"
```

### Task 10: Run complete verification

**Files:**
- No expected source changes

**Step 1: Run the task package with the race detector**

Run: `GOWORK=off go test -race ./task`

Expected: PASS with no race reports.

**Step 2: Run the complete tools test suite**

Run: `GOWORK=off make test`

Expected: PASS.

**Step 3: Run static and security checks**

Run: `GOWORK=off make secure`

Expected: formatting, `go vet`, `staticcheck`, `gosec`, module verification,
and `govulncheck` all pass.

**Step 4: Review the final diff**

Run: `git status --short`

Expected: no uncommitted files.

Run: `git diff --check`

Expected: no output and exit status 0.

Run: `git log --oneline -10`

Expected: the task commits appear in order.

**Step 5: Record verification evidence**

No commit is needed if all checks pass without changes. If verification
required a correction, add a focused regression test, apply the smallest fix,
repeat Steps 1–4, and commit that correction separately.

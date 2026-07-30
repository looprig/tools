# Loop-Scoped Task Tools Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add `TaskCreate`, `TaskUpdate`, `TaskGet`, and `TaskList` as a Claude-compatible tool bundle backed by one loop-scoped in-memory task graph.

**Architecture:** A new `task` package owns an unexported mutex-protected store and four concrete tools. `tools.TaskDefinitions()` returns one `tool.NewBundleDefinition` whose per-binding factory creates a fresh store and all four tools over it; the harness and Loop domain remain unchanged.

**Tech Stack:** Go 1.26, `encoding/json`, `sync.Mutex`, Looprig `tool.Definition`, `tool.InvokableTool`, and `core/uuid`.

---

### Task 1: Define the task value model and cloning rules

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

Add `TestCloneTaskDeepCopiesDependenciesAndMetadata`. Use nested metadata such
as `map[string]any{"labels": []any{"parser"}}`, mutate the clone, and prove the
source task remains unchanged.

**Step 2: Run the tests to verify they fail**

Run: `go test ./task`

Expected: FAIL because package `task`, `Status`, `Task`, and `cloneTask` do not
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

type Task struct {
	ID          string         `json:"id"`
	Subject     string         `json:"subject"`
	Description string         `json:"description"`
	ActiveForm  string         `json:"activeForm,omitempty"`
	Status      Status         `json:"status"`
	BlockedBy   []string       `json:"blockedBy,omitempty"`
	Blocks      []string       `json:"blocks,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}
```

Implement `Status.valid`, `cloneTask`, and recursive JSON-value cloning for
`map[string]any` and `[]any`. `Blocks` is output-only and is never persisted by
the store.

**Step 4: Run the tests to verify they pass**

Run: `go test ./task`

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
- `Blocks` is derived from other tasks' `BlockedBy` values.

Use a deterministic ID seam:

```go
type idSource func() (uuid.UUID, error)

store := newStore(sequenceIDs(idA, idB))
```

Keep the production default wired to `uuid.New`.

**Step 2: Run the tests to verify they fail**

Run: `go test ./task -run 'TestStore(Create|Get|List|DerivedBlocks)'`

Expected: FAIL because `store`, `newStore`, and its operations do not exist.

**Step 3: Implement the store**

Add:

```go
type store struct {
	mu    sync.Mutex
	newID idSource
	tasks map[string]Task
}

type createInput struct {
	Subject     string
	Description string
	ActiveForm  string
	BlockedBy   []string
	Metadata    map[string]any
}
```

Implement `create`, `get`, and `list`. Hold the graph mutex through validation,
mutation, derived-block calculation, and snapshot cloning.

Normalize dependency slices by removing duplicates and sorting IDs.

**Step 4: Run the tests to verify they pass**

Run: `go test ./task -run 'TestStore(Create|Get|List|DerivedBlocks)'`

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
- deleting a task removes it and cleans references from remaining tasks.

Pin deletion as a command, not a persisted status:

```go
updated, deleted, err := store.update(updateInput{
	TaskID: taskID,
	Status: statusPointer(StatusCommandDeleted),
})
```

**Step 2: Run the tests to verify they fail**

Run: `go test ./task -run 'TestStore(Update|Dependency|Cycle|Blocked|Delete)'`

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
	Metadata        *map[string]any
}
```

Build a candidate graph while holding the mutex. Validate referenced IDs,
self-dependencies, and cycles with a bounded DFS over `BlockedBy`. Commit the
candidate only after every check passes.

For deletion, remove the target task and its ID from every remaining
`BlockedBy` slice before committing.

**Step 4: Run the tests to verify they pass**

Run: `go test ./task -run 'TestStore(Update|Dependency|Cycle|Blocked|Delete)'`

Expected: PASS.

**Step 5: Commit**

```bash
git add task/store.go task/store_test.go
git commit -m "feat(task): add task updates and dependencies"
```

### Task 4: Add shared JSON, result, preparation, and audit helpers

**Files:**
- Create: `task/tool.go`
- Create: `task/tool_test.go`

**Step 1: Write failing helper tests**

Test that:

- decoding rejects malformed JSON, trailing JSON, and unknown fields;
- JSON results contain exactly one text block holding valid JSON;
- all task calls prepare as pure requests with the concrete tool name;
- audit summaries contain only the tool name and never model-supplied task text.

**Step 2: Run the tests to verify they fail**

Run: `go test ./task -run 'Test(Decode|JSONResult|Prepare|Audit)'`

Expected: FAIL because the helpers do not exist.

**Step 3: Implement minimal helpers**

Implement a generic strict decoder using `json.Decoder.DisallowUnknownFields`
and an EOF check. Add a `jsonResult` helper that marshals a value and returns
`tool.TextResult(string(encoded))`.

Create a small embedded base:

```go
type toolBase struct {
	name string
	desc string
}

func (b toolBase) PrepareCall(
	context.Context, uuid.UUID, string,
) (tool.Request, tool.PreparedArtifact, error) {
	return tool.Request{ToolName: b.name}, nil, nil
}

func (b toolBase) AuditSummary(string) string { return b.name }
```

Each concrete tool still implements its own `Info` so its exact schema remains
obvious and testable.

**Step 4: Run the tests to verify they pass**

Run: `go test ./task -run 'Test(Decode|JSONResult|Prepare|Audit)'`

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

**Step 1: Write failing metadata and schema tests**

For both tools, assert:

- exact tool name and description;
- schema has `type: object` and `additionalProperties: false`;
- required fields are exact;
- malformed and unknown inputs return `error:` tool-result text and no Go
  error;
- successful output is structured JSON.

Pin these input shapes:

```json
{"subject":"S","description":"D","activeForm":"Doing S","blockedBy":[],"metadata":{}}
```

```json
{"taskId":"uuid"}
```

**Step 2: Run the tests to verify they fail**

Run: `go test ./task -run 'TestTask(Create|Get)'`

Expected: FAIL because the concrete tools do not exist.

**Step 3: Implement TaskCreate and TaskGet**

Each tool holds `*store`, embeds `toolBase`, implements `Info` and
`InvokableRun`, and satisfies:

```go
var (
	_ tool.InvokableTool = (*TaskCreate)(nil)
	_ tool.CallPreparer  = (*TaskCreate)(nil)
	_ tool.Auditable     = (*TaskCreate)(nil)
)
```

Return successful create/get results as:

```json
{"task":{...complete task...}}
```

**Step 4: Run the tests to verify they pass**

Run: `go test ./task -run 'TestTask(Create|Get)'`

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

- `TaskList` accepts only `{}` and returns `{"tasks":[]}` when empty;
- list output is deterministic and includes derived `blocks`;
- `TaskUpdate` requires `taskId`;
- every patch field maps to `updateInput`;
- `status:"deleted"` returns a structured deletion result;
- store validation failures remain model-visible tool-result errors.

Use this successful deletion shape:

```json
{"deletedTaskId":"uuid"}
```

**Step 2: Run the tests to verify they fail**

Run: `go test ./task -run 'TestTask(List|Update)'`

Expected: FAIL because the tools do not exist.

**Step 3: Implement TaskList and TaskUpdate**

Use pointer fields in the decoded update arguments to distinguish omission
from clearing a string or metadata object. Preserve the exact camelCase field
names from the approved design.

Add the same compile-time capability assertions used by TaskCreate and TaskGet.

**Step 4: Run the tests to verify they pass**

Run: `go test ./task -run 'TestTask(List|Update)'`

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
created task.

Build the definition a second time with a different `LoopID` and prove its list
is empty. Also run concurrent create/update/list calls under the race detector.

**Step 2: Run the tests to verify they fail**

Run: `go test . -run 'TestTaskDefinition|TestTaskBundle'`

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
declared order. Every call creates a fresh store, which makes each Loop binding
independent.

Update `TestDefinitionBlueprints` to handle bundles by checking
`ProducedToolNames` rather than assuming every definition builds exactly one
tool. Add `TaskDefinitions()` to the all-tools-prepared test.

**Step 4: Run the tests to verify they pass**

Run: `go test . -run 'TestTaskDefinition|TestTaskBundle|TestDefinition'`

Expected: PASS.

**Step 5: Commit**

```bash
git add definitions.go definitions_test.go task_bundle_test.go
git commit -m "feat: expose loop-scoped task tool bundle"
```

### Task 8: Prove one bundle instance is reused across Loop modes

**Files:**
- Modify: `task_bundle_test.go`

**Step 1: Write the failing integration test**

Define a Loop whose base mode and alternate mode both select the same
`TaskDefinitions()` value. Bind the Loop once, create a task in the base mode,
switch/select the alternate bound mode through the existing `BoundDefinition`
API, and verify `TaskList` sees the same task.

Create a second bound Loop with another `LoopID` and prove it starts empty.

**Step 2: Run the test to verify current wiring**

Run: `go test . -run TestTaskBundleSharesStateAcrossModes -v`

Expected before any fix: PASS if the existing definition-build cache behaves as
documented. If it fails, stop and inspect `loop.Definition.Bind`; do not add
task state to `tool.Bindings` or the Loop runtime.

**Step 3: Make only the minimal correction if the test exposes a defect**

The expected implementation needs no production change: one immutable bundle
definition must appear in every selected mode so `Bind` builds it once by
definition name and reuses its concrete tools.

**Step 4: Re-run the focused test**

Run: `go test . -run TestTaskBundleSharesStateAcrossModes -v`

Expected: PASS.

**Step 5: Commit**

```bash
git add task_bundle_test.go
git commit -m "test: prove task state follows loop binding"
```

### Task 9: Update public documentation while preserving Todo compatibility

**Files:**
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

**Step 2: Update the module specification**

Document `Tasks` as the deliberate exception to the “one definition produces
one tool” convention: the four tools require one shared loop-local store, and
`NewBundleDefinition` provides that ownership without a harness binding.

List all four produced model-facing names and their scope/lifetime.

**Step 3: Run documentation compile tests**

Run: `go test . -run 'TestExample|TestDefinition'`

Expected: PASS.

**Step 4: Run formatting**

Run: `make fmt`

Expected: command succeeds and formats changed Go files.

**Step 5: Commit**

```bash
git add README.md example_readme_test.go docs/specs/module.md
git commit -m "docs: document structured task tools"
```

### Task 10: Run complete verification

**Files:**
- No expected source changes

**Step 1: Run the task package with the race detector**

Run: `go test -race ./task`

Expected: PASS with no race reports.

**Step 2: Run the complete tools test suite**

Run: `make test`

Expected: PASS.

**Step 3: Run static and security checks**

Run: `make secure`

Expected: formatting, `go vet`, `staticcheck`, `gosec`, module verification,
and `govulncheck` all pass.

**Step 4: Review the final diff**

Run: `git status --short && git diff --check && git log --oneline -10`

Expected: no uncommitted files, no whitespace errors, and the task commits
appear in order.

**Step 5: Record verification evidence**

No commit is needed if all checks pass without changes. If verification
required a correction, add a focused regression test, apply the smallest fix,
repeat Steps 1–4, and commit that correction separately.

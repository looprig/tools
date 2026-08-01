# Loop-Scoped Task Tools Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Replace Todo with `TaskCreate`, `TaskUpdate`, `TaskGet`, and `TaskList`, giving every parent and subagent Loop its own bounded in-memory task graph.

**Architecture:** A new `task` package owns an unexported mutex-protected store and four sequential concrete tools. `tools.TaskDefinitions()` returns one `tool.NewBundleDefinition` whose per-binding factory creates a fresh store. Todo is deleted without a compatibility shim; CodeRig selects Tasks for primary and delegated Loops, while the Harness-owned Subagent control tool remains unchanged.

**Tech Stack:** Go 1.26, `encoding/json`, `sync.Mutex`, Looprig `tool.Definition`, `tool.InvokableTool`, `tool.CallPreparer`, `tool.Sequential`, `core/uuid`, and existing Harness managed delegation.

---

### Task 0: Synchronize the tools module baseline

**Files:**
- Modify: `go.mod`
- Modify: `go.sum`

**Step 1: Record the current failure**

Run:

```bash
GOWORK=off go test -race ./...
```

Expected: FAIL before package compilation with `updates to go.mod needed`.

**Step 2: Inspect the non-mutating module delta**

Run:

```bash
GOWORK=off go mod tidy -diff
```

Expected: a diff aligning the module with the current sibling Harness/Core graph.
Review it for unexpected new direct dependencies before proceeding.

**Step 3: Synchronize the manifest**

Run:

```bash
GOWORK=off go mod tidy
```

Do not add a new external production dependency for Task tools. The implementation
uses only stdlib plus existing Looprig modules.

**Step 4: Verify the clean baseline**

Run:

```bash
GOWORK=off go test -race ./...
GOWORK=off go mod verify
```

Expected: PASS.

**Step 5: Commit**

```bash
git add go.mod go.sum
git commit -m "chore: synchronize tools module baseline"
```

### Task 1: Define task values and defensive cloning

**Files:**
- Create: `task/model.go`
- Create: `task/model_test.go`

**Step 1: Write failing model tests**

Add table coverage for:

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

Also prove:

- `cloneRecord` deep-copies dependency and metadata backing storage;
- `taskFromRecord` derives `Blocks` without persisting it;
- returned `Task` slices and metadata can be mutated without changing the record;
- nil metadata is omitted while canonical non-empty metadata is retained.

**Step 2: Run the focused test and observe RED**

```bash
GOWORK=off go test -race ./task -run 'Test(Status|Clone|TaskFromRecord)'
```

Expected: FAIL because package `task` and its types do not exist.

**Step 3: Implement the model**

Add the approved types:

```go
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
with owned bytes and copy every slice.

**Step 4: Run GREEN**

```bash
GOWORK=off go test -race ./task -run 'Test(Status|Clone|TaskFromRecord)'
```

Expected: PASS.

**Step 5: Commit**

```bash
git add task/model.go task/model_test.go
git commit -m "feat(task): define task value model"
```

### Task 2: Add bounded create, get, and list storage

**Files:**
- Create: `task/store.go`
- Create: `task/store_test.go`

**Step 1: Write failing store tests**

Cover:

- create defaults status to `pending`;
- non-empty UUID generation;
- UUID-source failure and generated-ID collision reject without mutation;
- empty and whitespace-only subject/description rejection;
- unknown dependencies during create;
- unknown get ID;
- deterministic UUID ordering;
- defensive get/list snapshots;
- derived `Blocks` from other records' `BlockedBy` lists;
- count, field, metadata, dependency, and aggregate limits;
- every rejected create leaves the graph unchanged.

Use a deterministic seam:

```go
type idSource func() (uuid.UUID, error)

store := newStore(sequenceIDs(idA, idB))
```

**Step 2: Run RED**

```bash
GOWORK=off go test -race ./task -run 'TestStore(Create|Get|List|DerivedBlocks|Limits|ID)'
```

Expected: FAIL because the store does not exist.

**Step 3: Implement bounded storage**

Add:

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

type store struct {
	mu    sync.Mutex
	newID idSource
	tasks map[string]taskRecord
}
```

Implement `create`, `get`, `list`, dependency normalization, candidate cloning,
aggregate-size calculation, and candidate-graph validation. Hold the mutex
through live-state validation, candidate construction, commit, and snapshotting.
Reject ID collisions instead of retrying.

**Step 4: Run GREEN**

```bash
GOWORK=off go test -race ./task -run 'TestStore(Create|Get|List|DerivedBlocks|Limits|ID)'
```

Expected: PASS.

**Step 5: Commit**

```bash
git add task/store.go task/store_test.go
git commit -m "feat(task): add bounded task storage"
```

### Task 3: Add atomic updates and graph invariants

**Files:**
- Modify: `task/store.go`
- Modify: `task/store_test.go`

**Step 1: Write failing update tests**

Cover:

- omitted scalar and metadata fields remain unchanged;
- supplied metadata replaces existing metadata;
- `{}` clears metadata to nil/absent;
- unknown status and unknown task IDs;
- unknown dependency, self-dependency, two-node cycle, and three-node cycle;
- duplicate additions/removals are normalized;
- removal wins when one ID appears in both add and remove lists;
- a blocked task cannot transition to `in_progress`;
- an `in_progress` task cannot acquire an incomplete blocker;
- completed blockers permit a later transition without auto-starting dependents;
- deletion removes the task and all inbound references;
- every field/dependency/aggregate rejection is atomic.

Pin deletion as a command:

```go
updated, deleted, err := store.update(updateInput{
	TaskID: taskID,
	Status: statusPointer(StatusCommandDeleted),
})
```

**Step 2: Run RED**

```bash
GOWORK=off go test -race ./task -run 'TestStore(Update|Dependency|Cycle|Blocked|Delete|Metadata)'
```

Expected: FAIL because update behavior is absent.

**Step 3: Implement candidate-graph updates**

Add `StatusCommandDeleted`, pointer-based `updateInput`, candidate-map cloning,
bounded DFS cycle detection, deletion cleanup, and the active-task invariant.
Apply additions first and removals second so removal wins. Commit only after all
candidate validation succeeds.

**Step 4: Run GREEN**

```bash
GOWORK=off go test -race ./task -run 'TestStore(Update|Dependency|Cycle|Blocked|Delete|Metadata)'
```

Expected: PASS.

**Step 5: Commit**

```bash
git add task/store.go task/store_test.go
git commit -m "feat(task): enforce atomic task graph updates"
```

### Task 4: Add strict preparation and result helpers

**Files:**
- Create: `task/tool.go`
- Create: `task/tool_test.go`

**Step 1: Write failing helper tests**

Test:

- oversized raw documents fail before decode;
- malformed, trailing, null, array, scalar, and unknown-field JSON fails;
- duplicate top-level object members fail closed;
- required-field presence is distinguishable from zero values;
- metadata accepts only one object, canonicalizes keys, owns its bytes, and
  rejects malformed/oversized/non-object values;
- omitted metadata remains nil and `{}` becomes the clear sentinel;
- `jsonResult` returns exactly one text block containing valid JSON;
- preparation errors have a stable private concrete type and model-safe text;
- audit summaries are exactly the concrete tool name.

**Step 2: Run RED**

```bash
GOWORK=off go test -race ./task -run 'Test(Decode|Metadata|JSONResult|PrepareError|Audit)'
```

Expected: FAIL because helpers do not exist.

**Step 3: Implement the helpers**

Implement a strict object decoder using `json.Decoder`,
`DisallowUnknownFields`, explicit top-level/object checks, duplicate-member
detection, and an EOF check. Add `canonicalMetadata`, the explicit-empty-object
clear signal, `jsonResult`, `prepareError`, and:

```go
type toolBase struct {
	name string
	desc string
}

func (b toolBase) AuditSummary(string) string { return b.name }
```

Do not put `PrepareCall` or `Info` on `toolBase`; concrete tools own their
schemas and artifact types.

**Step 4: Run GREEN**

```bash
GOWORK=off go test -race ./task -run 'Test(Decode|Metadata|JSONResult|PrepareError|Audit)'
```

Expected: PASS.

**Step 5: Commit**

```bash
git add task/tool.go task/tool_test.go
git commit -m "feat(task): add strict task preparation helpers"
```

### Task 5: Implement TaskCreate and TaskGet

**Files:**
- Create: `task/create.go`
- Create: `task/create_test.go`
- Create: `task/get.go`
- Create: `task/get_test.go`

**Step 1: Write failing contract tests**

Pin the exact descriptions and schemas from the approved design. Test required
fields, `additionalProperties: false`, all preparation failures, canonical UUIDs,
dependency normalization, non-nil typed artifacts, pure requests, changed raw
argument immunity, structured success, unknown-ID error results, and missing,
nil, or cross-tool artifacts.

**Step 2: Run RED**

```bash
GOWORK=off go test -race ./task -run 'TestTask(Create|Get)'
```

Expected: FAIL because the concrete tools do not exist.

**Step 3: Implement both tools**

Each tool embeds `toolBase`, holds `*store`, and implements `Info`,
`PrepareCall`, `InvokableRun`, and `Sequential`. Use private typed artifacts:

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

Retrieve artifacts with `internal/prepared.FromContext`. Return
`{"task":{...}}` on success and model-visible `error:` results for live-state
failures. Implement `Sequential() bool { return true }`.

**Step 4: Run GREEN**

```bash
GOWORK=off go test -race ./task -run 'TestTask(Create|Get)'
```

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

**Step 1: Write failing contract tests**

Pin exact design schemas and descriptions. Test:

- TaskList accepts only `{}` and returns `{"tasks":[]}` when empty;
- deterministic list output and derived `blocks`;
- presence-aware scalar and metadata patches;
- `{}` metadata clear behavior;
- dependency add/remove overlap;
- deleted status result `{"deletedTaskId":"uuid"}`;
- every invalid preparation input;
- every live-state failure as a model-visible result;
- typed artifact isolation and raw-argument immunity;
- `tool.Sequential` on both tools.

**Step 2: Run RED**

```bash
GOWORK=off go test -race ./task -run 'TestTask(List|Update)'
```

Expected: FAIL because the tools do not exist.

**Step 3: Implement both tools**

Use pointer fields for supplied scalar patches and a presence-aware metadata
field. Add private `taskListArtifact` and `taskUpdateArtifact`, compile-time
capability assertions, structured results, and `Sequential() == true`.

**Step 4: Run GREEN**

```bash
GOWORK=off go test -race ./task -run 'TestTask(List|Update)'
```

Expected: PASS.

**Step 5: Commit**

```bash
git add task/list.go task/list_test.go task/update.go task/update_test.go
git commit -m "feat(task): add list and update tools"
```

### Task 7: Expose Tasks and remove Todo from the tools module

**Files:**
- Modify: `definitions.go`
- Modify: `definitions_test.go`
- Modify: `dependency_test.go`
- Modify: `example_readme_test.go`
- Create: `task_bundle_test.go`
- Delete: `todo/todo.go`
- Delete: `todo/todo_test.go`
- Delete: `todo/preparecall_test.go`
- Delete: `todo/result_test.go`

**Step 1: Write failing bundle and removal tests**

Assert:

```go
definition := TaskDefinitions()

if definition.Name() != "Tasks" { ... }
if diff := cmp.Diff(
	[]string{"TaskCreate", "TaskUpdate", "TaskGet", "TaskList"},
	definition.ProducedToolNames(),
); diff != "" { ... }
if definition.Requirements() != 0 { ... }
```

Build once, create a task, and prove list/get/update share one store. Build a
second time with another Loop ID and prove it is empty. Assert every produced
tool implements `CallPreparer`, `Auditable`, and `Sequential` and prepares a
non-nil typed artifact.

Add `task` and remove `todo` in `TestToolPackageLayout`. Remove Todo from the
one-tool definition table and all-tools-prepared roster. Add a source guard that
rejects `TodoDefinition` and a `todo` package directory.

**Step 2: Run RED**

```bash
GOWORK=off go test -race . -run 'TestTaskDefinition|TestTaskBundle|TestTodoRemoved|TestDefinition|TestToolPackageLayout'
```

Expected: FAIL until the bundle exists and Todo is removed.

**Step 3: Implement the bundle and hard deletion**

Add:

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

`task.NewTools` creates one private store and returns tools in declared order.
Delete the Todo import, definition, package, tests, and README compile fixture.
Do not leave a deprecated alias.

**Step 4: Run GREEN**

```bash
GOWORK=off go test -race . -run 'TestTaskDefinition|TestTaskBundle|TestTodoRemoved|TestDefinition|TestToolPackageLayout|TestProductionDependencyBoundary'
```

Expected: PASS.

**Step 5: Commit**

```bash
git add definitions.go definitions_test.go dependency_test.go example_readme_test.go task_bundle_test.go task
git add -u todo
git commit -m "feat: replace Todo with loop-scoped Tasks"
```

### Task 8: Prove mode reuse, Loop isolation, and concurrency

**Files:**
- Modify: `task_bundle_test.go`

**Step 1: Add binding integration tests**

Create `tasks := TaskDefinitions()` once and select the same value in a base mode
and alternate mode. Bind one Loop; create through the base mode and list through
the alternate mode. Bind a second Loop and prove it starts empty.

Add a negative fixture with two distinct `TaskDefinitions()` values named
`Tasks` in one Loop and assert `BindDuplicateDefinitionName`.

Drive all invocations through:

```text
PrepareCall → loop.WithPreparedCall → InvokableRun
```

Add concurrent direct create/update/get/list calls and verify them under the race
detector.

**Step 2: Run the tests**

```bash
GOWORK=off go test -race . -run 'TestTaskBundle(SharesStateAcrossModes|IsolatesLoops|RejectsDistinctSameNameDefinitions|Concurrent)' -v
```

Expected: PASS without Harness production changes. If mode reuse fails, stop and
inspect `loop.Definition.Bind`; do not move task state into Harness.

**Step 3: Commit**

```bash
git add task_bundle_test.go
git commit -m "test: prove task state follows one Loop binding"
```

### Task 9: Update tools documentation for the hard cut

**Files:**
- Modify: `CLAUDE.md`
- Modify: `CONTRIBUTING.md`
- Modify: `README.md`
- Modify: `example_readme_test.go`
- Modify: `docs/specs/module.md`

**Step 1: Update the compile-checked example**

Use `tools.TaskDefinitions()` and list the four produced model-facing names.
Explain that Tasks is the one related-family bundle and that every definition
build owns one Loop-local graph.

**Step 2: Update policy and package documentation**

Replace the one-definition-per-tool rule with:

> Export one `tool.Definition` per independently selectable capability by
> default. Never bundle unrelated tools. `Tasks` is the deliberate exception:
> its four operations require one per-build Loop-local store.

Delete all Todo references. State that parent and child Loops each receive an
independent graph. Record that Harness, not this module, owns and injects the
Subagent control tool.

**Step 3: Add a documentation/source cleanup guard**

Extend a root test to search tracked tools sources and documentation for
`TodoDefinition`, `NewTodo`, and the `github.com/looprig/tools/todo` import.

**Step 4: Verify documentation and formatting**

```bash
GOWORK=off make fmt
GOWORK=off go test -race . -run 'TestExample|TestDefinition|TestToolPackageLayout|TestProductionDependencyBoundary|TestTodoRemoved'
```

Expected: PASS.

**Step 5: Commit**

```bash
git add CLAUDE.md CONTRIBUTING.md README.md example_readme_test.go docs/specs/module.md
git commit -m "docs: document loop-scoped task tools"
```

### Task 10: Migrate CodeRig primary and subagent Loops

**Files:**
- Modify: `../coderig/internal/app/toolsets.go`
- Modify: `../coderig/internal/app/swarm_test.go`
- Modify: `../coderig/internal/app/managed_delegation_test.go`
- Modify: `../coderig/go.mod`
- Modify: `../coderig/go.sum`

**Step 1: Write failing roster tests**

For operator and reviewer definitions, assert all four Task names are present and
`Todo` is absent. Keep the existing primary-minus-Subagent equality assertion:
the Harness-injected Subagent tool remains the only primary/leaf tool difference.

**Step 2: Write the managed parent/child isolation test**

Use the existing scripted managed-delegation harness to drive:

1. primary `TaskCreate`;
2. primary `Subagent start` for operator with `wait:true`;
3. operator child `TaskList`, observing empty;
4. operator child `TaskCreate`;
5. primary `TaskList`, observing only the primary task;
6. primary `Subagent start` for reviewer;
7. reviewer child `TaskList`, observing empty.

Assert every inference request for primary/operator/reviewer advertises exactly
`TaskCreate`, `TaskUpdate`, `TaskGet`, and `TaskList`, and no Todo. The test proves
real managed children receive fresh stores rather than only comparing static
definition metadata.

**Step 3: Run RED**

```bash
cd ../coderig
GOWORK=off go test -race ./internal/app -run 'Test.*Task|TestManagedSubagentTaskIsolation|TestSwarmDefinitionsAntiDrift' -v
```

Expected: FAIL while CodeRig still selects Todo.

**Step 4: Replace consumer wiring**

Replace both `tools.TodoDefinition()` calls with `tools.TaskDefinitions()`.
Synchronize module metadata only if the local Tools version/pseudo-version
requires it. Do not add or move the Subagent tool; it remains structurally
injected by Harness.

**Step 5: Run GREEN**

```bash
GOWORK=off go test -race ./internal/app -run 'Test.*Task|TestManagedSubagentTaskIsolation|TestSwarmDefinitionsAntiDrift|TestManagedSubagent' -v
```

Expected: PASS.

**Step 6: Commit in the CodeRig repository**

```bash
git add internal/app/toolsets.go internal/app/swarm_test.go internal/app/managed_delegation_test.go go.mod go.sum
git commit -m "feat: give every CodeRig Loop scoped task tools"
```

### Task 11: Remove Todo presentation and keep Task summaries redacted

**Files:**
- Modify: `../tui/internal/presentation/toolsummary.go`
- Modify: `../tui/internal/presentation/transcript_test.go`
- Modify: `../tui/internal/presentation/summary_test.go`

**Step 1: Write failing presentation tests**

For `TaskCreate`, `TaskUpdate`, `TaskGet`, and `TaskList`, pass inputs containing
subjects, descriptions, metadata, and UUIDs. Assert reconstructed summaries
contain none of those values. Verify nested subagent cards still display the
concrete tool name. Add a guard that Todo has no presentation-specific case.

**Step 2: Run RED**

```bash
cd ../tui
GOWORK=off go test -race ./internal/presentation -run 'Test.*Task.*Summary|Test.*Nested.*Task' -v
```

Expected: FAIL until the explicit Task redaction contract is represented.

**Step 3: Update presentation behavior**

Remove `toolNameTodo`, `todoSummaryArgs`, `todoSummary`, and the Todo switch case.
Recognize the four Task names as intentionally detail-free summaries, returning
`""`; the surrounding card already displays `ToolName`. Never reconstruct task
text or IDs from stored arguments.

**Step 4: Run GREEN**

```bash
GOWORK=off go test -race ./internal/presentation -run 'Test.*Task.*Summary|Test.*Nested.*Task' -v
```

Expected: PASS.

**Step 5: Commit in the TUI repository**

```bash
git add internal/presentation/toolsummary.go internal/presentation/transcript_test.go internal/presentation/summary_test.go
git commit -m "refactor(tui): replace Todo summaries with redacted Tasks"
```

### Task 12: Update Harness and public consumer documentation

**Files:**
- Modify: `../harness/CLAUDE.md`
- Modify: `../harness/pkg/tool/README.md`
- Modify: `../www/looprig/docs/consumers/tools.md`
- Modify: `../www/looprig/docs/consumers/loop.md`
- Modify: `../www/looprig/docs/consumers/larger-systems.md`
- Modify: `../www/looprig/profile/README.md`

**Step 1: Document the ownership boundary**

In Harness, state that Subagent is the deliberate Harness-owned model-facing
control tool because its schema/catalog is derived from frozen delegate topology
and its authority is a parent-scoped controller. Optional task tracking remains
in `github.com/looprig/tools` and Harness does not import it.

**Step 2: Update consumer examples**

Replace Todo with `TaskDefinitions()`. Explain:

- four model-facing names come from one selected definition;
- each bound Loop, including each subagent, receives an independent graph;
- modes within one Loop share it;
- Subagent remains automatically injected and must not be added manually;
- task coordination between agents uses Subagent messages, not shared memory.

**Step 3: Run documentation checks**

Use the module-specific test/build commands documented by Harness and WWW. At a
minimum:

```bash
cd ../harness
GOWORK=off go test -race ./pkg/tool ./pkg/rig ./internal/delegationtool ./internal/sessionruntime

cd ../www
GOWORK=off go test -race ./...
```

Expected: PASS.

**Step 4: Commit separately in each repository**

```bash
cd ../harness
git add CLAUDE.md pkg/tool/README.md
git commit -m "docs: clarify Harness ownership of Subagent"

cd ../www
git add looprig/docs/consumers/tools.md looprig/docs/consumers/loop.md looprig/docs/consumers/larger-systems.md looprig/profile/README.md
git commit -m "docs: replace Todo with loop-scoped Tasks"
```

### Task 13: Complete cross-module verification and removal audit

**Files:**
- No expected production changes

**Step 1: Prove Todo is gone**

From `/Users/ipotter/code/looprig` run:

```bash
rg -n 'TodoDefinition|NewTodo|github.com/looprig/tools/todo|toolNameTodo' tools coderig tui www tests -g '*.go' -g '*.md'
```

Expected: no matches. Historical dated plans may retain Todo only when explicitly
excluded from the cleanup command; active documentation and production code may
not.

**Step 2: Verify Tools**

```bash
cd tools
GOWORK=off go test -race ./task
GOWORK=off make test
GOWORK=off make secure
git diff --check
```

Expected: PASS with no race or static/security findings.

**Step 3: Verify CodeRig and real managed delegation**

```bash
cd ../coderig
GOWORK=off go test -race ./internal/app
GOWORK=off make test
GOWORK=off make secure
git diff --check
```

Expected: PASS, including the real parent/operator/reviewer task-isolation test.

**Step 4: Verify TUI, Harness, WWW, and integration ownership**

```bash
cd ../tui
GOWORK=off make test
GOWORK=off make secure

cd ../harness
GOWORK=off go test -race ./pkg/tool ./pkg/rig ./internal/delegationtool ./internal/sessionruntime

cd ../www
GOWORK=off go test -race ./...

cd ../tests
GOWORK=off go test -race ./...
```

Expected: PASS.

**Step 5: Review repository state and commits**

In every changed repository run:

```bash
git status --short
git diff --check
git log --oneline -15
```

Expected: no uncommitted feature files; commits are separated by module and
logical responsibility. If verification requires a fix, add a focused regression
test, make the smallest correction, rerun the affected module and downstream
integration suite, and commit the correction in its owning repository.

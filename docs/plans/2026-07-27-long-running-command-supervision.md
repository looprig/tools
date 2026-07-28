# Long-Running Command Supervision Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add secure, durable, cross-platform long-running command supervision with background/yielded Bash, incremental output, interactive input, stop control, process-tree cleanup, session lifecycle integration, and real integration coverage.

**Specification:** `docs/specs/long-running-command-supervision.md`

**Architecture:** Tools owns a session-scoped process supervisor and the four model-facing tools. Harness owns generic async contracts, session resources, identity, workspace leases, lifecycle events, notifications, shutdown, and restore. Sandbox owns enforced asynchronous processes, pipes, PTY/ConPTY, grants, process trees, signals, and parent-death cleanup. Coderig owns only the adapter and production composition between the independent modules.

**Tech Stack:** Go 1.26.4, Harness tool/session/event contracts, Sandbox Windows broker and Unix enforcement backends, `github.com/creack/pty` for Unix PTYs, `golang.org/x/sys/windows` for ConPTY and Job Objects, JSON manifests, atomic file replacement, race detector, tagged integration tests, Staticcheck, Gosec, Govulncheck, Windows and Linux CI.

---

## Execution rules

This plan is executed in these sibling worktrees:

```text
/Users/ipotter/code/looprig/.worktrees/long-running-commands/
  harness/
  tools/
  sandbox/
  coderig/
```

Unmodified local replacement modules are symlinked beside them. Run every Go
command with `GOWORK=off` and a repository-specific writable `GOCACHE` under
`/private/tmp`.

Every subagent receives one explicit absolute `workdir`; shell state never
carries between commands:

| Task owner | Required workdir |
| --- | --- |
| Harness | `/Users/ipotter/code/looprig/.worktrees/long-running-commands/harness` |
| Tools | `/Users/ipotter/code/looprig/.worktrees/long-running-commands/tools` |
| Sandbox | `/Users/ipotter/code/looprig/.worktrees/long-running-commands/sandbox` |
| Coderig | `/Users/ipotter/code/looprig/.worktrees/long-running-commands/coderig` |

Any relative `cd` shown later is explanatory only; the controller must replace
it with the absolute workdir above. A fresh subagent must never run from the
shared `/Users/ipotter/code/looprig` root.

For every numbered task:

1. Dispatch a fresh implementation subagent with the complete task text.
2. Require `superpowers:test-driven-development`.
3. The implementer writes one failing test and runs it before production code.
4. The implementer writes only enough code for green, runs the focused
   non-race test, refactors while green, self-reviews, and commits.
5. Dispatch a fresh spec-compliance reviewer.
6. Send every gap back to the same implementer, then re-review until approved.
7. Dispatch a fresh code-quality/security reviewer with the task's base and head
   SHAs.
8. Send critical and important findings back to the implementer, then re-review
   until approved.
9. Only then mark the task complete.

Before every task or microtask commit, run only:

1. `gofmt` on changed Go files;
2. the task's focused `go test` command without `-race`; and
3. `git diff --check`.

Do not run race tests, tagged integration discovery or execution, fuzzing,
repeated/count stress, static analysis, vulnerability checks, `make secure`, or
trimpath/cross-platform builds at task or microtask boundaries. This keeps the
TDD loop fast without weakening acceptance: every one of those checks is
batched at the owning `PHASE GATE`.

At every `PHASE GATE`, run its complete command matrix: full relevant
`go test -race` suites; `go test -tags integration -list` followed by tagged
integration execution where that phase owns integration tests; every fuzz
target accumulated through that phase; repository `make secure` (which includes
format checks, Vet, Staticcheck, Gosec, and Govulncheck); and `CGO_ENABLED=0`
trimpath native/cross-builds relevant to the changed repositories. Then dispatch
one phase-level spec reviewer and one phase-level code-quality/security reviewer
over the complete phase diff. Do not enter the next phase with an unresolved
finding or a deferred required check.

Append one section per gate to
`tools/docs/plans/2026-07-27-long-running-command-supervision-verification.md`
containing exact base/head SHAs for every affected repository, command strings,
exit codes, integration test names printed by `go test -list`, fuzz durations,
environment or CI run IDs, reviewer findings, fixes, re-review disposition, and
the final gate decision. Commit that evidence before the next phase.

No production code may be written before its failing test. Configuration and
module metadata synchronization in Task 0 is the only non-production exception.

Large numbered tasks below are decomposed into lettered microtasks. Each lettered
microtask is a separate subagent assignment, RED/GREEN cycle, review pair, and
commit. A heading such as Task 8 is a phase grouping, not permission for one
agent to implement every bullet at once.

Sandbox unit tests must use injected/null backends where OS confinement is not
the subject, so they run inside the managed development sandbox. Tagged live
integration tests deliberately use Seatbelt, Linux enforcement, loopback
proxies, Windows brokers, PTYs, and process trees. They require an approved host
or CI worker. An outer-sandbox denial is recorded as environment evidence and
rerun on the required worker; it is never converted to a skip, mock, or passing
claim.

## Phase 0: Reproducible baseline

### Task 0: Synchronize the multi-repository module baseline

**Files:**

- Modify: `../harness/go.mod`
- Modify: `../harness/go.sum`
- Modify: `../harness/vendor/**`
- Modify: `go.mod`
- Modify: `go.sum`
- Modify: `../coderig/go.mod`
- Modify: `../coderig/go.sum`
- Verify only: `../sandbox/go.mod`
- Verify only: `../sandbox/go.sum`

The current local replacement modules select newer transitive versions than the
checked-in Harness and Tools module graphs. Harness' vendor directory also omits
the replaced local modules. The clean baseline therefore fails before compiling
feature code. Synchronize those existing dependencies without introducing any
new direct dependency. `github.com/creack/pty` is added later and is the only
approved new direct dependency.

**Step 1: Reproduce the baseline failures**

Run:

```bash
cd /Users/ipotter/code/looprig/.worktrees/long-running-commands/harness
GOWORK=off GOCACHE=/private/tmp/looprig-harness-gocache GOFLAGS=-mod=readonly go test ./pkg/tool
cd /Users/ipotter/code/looprig/.worktrees/long-running-commands/tools
GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go test ./bash
```

Expected: both fail with `updates to go.mod needed, disabled by -mod=readonly`.

**Step 2: Synchronize existing graphs**

Run:

```bash
cd /Users/ipotter/code/looprig/.worktrees/long-running-commands/harness
GOWORK=off GOCACHE=/private/tmp/looprig-harness-gocache go mod tidy
GOWORK=off GOCACHE=/private/tmp/looprig-harness-gocache go mod vendor
cd /Users/ipotter/code/looprig/.worktrees/long-running-commands/tools
GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache go mod tidy
cd /Users/ipotter/code/looprig/.worktrees/long-running-commands/coderig
GOWORK=off GOCACHE=/private/tmp/looprig-coderig-gocache go mod tidy
```

Expected: only existing requirements are synchronized; no new direct module is
introduced.

**PHASE GATE 0: Verify the baseline**

Run:

```bash
cd /Users/ipotter/code/looprig/.worktrees/long-running-commands/harness
GOWORK=off GOCACHE=/private/tmp/looprig-harness-gocache GOFLAGS=-mod=vendor go test ./pkg/tool ./internal/sessionruntime
cd /Users/ipotter/code/looprig/.worktrees/long-running-commands/tools
GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go test ./bash .
cd /Users/ipotter/code/looprig/.worktrees/long-running-commands/sandbox
GOWORK=off GOCACHE=/private/tmp/looprig-sandbox-gocache go test ./internal/policy ./internal/platform ./pkg/profile
CGO_ENABLED=0 GOWORK=off GOCACHE=/private/tmp/looprig-sandbox-gocache go build -trimpath ./...
cd /Users/ipotter/code/looprig/.worktrees/long-running-commands/coderig
GOWORK=off GOCACHE=/private/tmp/looprig-coderig-gocache GOFLAGS=-mod=readonly go test ./internal/app
```

Expected: PASS. Separately run Sandbox's existing live `./internal/exec` suite
on an approved host/CI worker; the managed workspace may deny `sandbox_apply`
and loopback listener creation before repository code can execute. When the
separate Sandbox stabilization owner is still active, record the local denial
and defer the live rerun to the Phase 3 coordination prerequisite. That deferred
evidence blocks Phase 3, but not the independent Harness and Tools phases. If a
code test fails, stop and diagnose it with
`superpowers:systematic-debugging`; do not classify it as module hygiene.

**Step 4: Commit each repository independently**

```bash
cd /Users/ipotter/code/looprig/.worktrees/long-running-commands/harness
git add go.mod go.sum vendor
git commit -m "build: synchronize harness module baseline"
cd /Users/ipotter/code/looprig/.worktrees/long-running-commands/tools
git add go.mod go.sum
git commit -m "build: synchronize tools module baseline"
cd /Users/ipotter/code/looprig/.worktrees/long-running-commands/coderig
git add go.mod go.sum
git commit -m "build: synchronize coderig module baseline"
```

Skip a repository's commit only when its module files are unchanged.

## Phase 1: Harness contracts and lifetime coordination

### Task 1: Define generic asynchronous process contracts

**Files:**

- Create: `../harness/pkg/tool/process.go`
- Create: `../harness/pkg/tool/process_test.go`
- Modify: `../harness/pkg/tool/README.md`

Harness types are intentionally generic. Sandbox does not import them; Coderig
adapts between the two named APIs.

**Step 1: Write failing contract tests**

Add tests that compile a fake against these required interfaces and validate
enum/error behavior:

```go
type AsyncProcessRunner interface {
	PrepareProcess(context.Context, ProcessRequest) (PreparedProcess, error)
}

type PreparedProcess interface {
	EffectiveWorkspaceAccess() WorkspaceAccess
	Start(context.Context) (Process, error)
	Close() error
}

type Process interface {
	Stdout() io.ReadCloser
	Stderr() io.ReadCloser
	Stdin() io.WriteCloser
	Wait(context.Context) (ProcessResult, error)
	Resize(context.Context, uint16, uint16) error
	Signal(context.Context, ProcessSignal) error
	Close(context.Context) error
}

type ProcessActivitySource interface {
	Activities() <-chan ProcessActivity
}
```

`ProcessRequest` must include command, directory, grants, origin execution ID,
timeout deadline, and PTY flag. `PrepareProcess` validates and reserves
enforcement resources without spawning. Tools reads authoritative access,
acquires a workspace lease, then calls single-use `Start`. Closing an unstarted
preparation releases reservations. The `Start` context governs setup through
handoff only; the returned Process lives until wait, close, deadline, or runner
shutdown. `ProcessResult` must include exit code, typed terminal reason,
start/finish times, and no OS PID. Define
`ProcessSignalInterrupt`, `ProcessSignalTerminate`, and `ProcessSignalKill`.

Add `WorkspaceAccess` with read-only, scoped-write paths/trees, and broad-write
classification. Add typed error codes for unsupported lifetime enforcement,
spawn/setup, PTY unavailable, signal, wait, and teardown.
Add typed `ProcessActivity`/`WorkspaceActivityKind` values as an optional
capability. The channel contract requires closure before `Wait` returns. Every
activity invalidates the complete bound observation cache; scoped observation
paths are deliberately out of scope. Invalid activity maps to broad
invalidation and can never narrow the prepared lifetime lease.

**Step 2: Verify RED**

Run:

```bash
cd /Users/ipotter/code/looprig/.worktrees/long-running-commands/harness
GOWORK=off GOCACHE=/private/tmp/looprig-harness-gocache GOFLAGS=-mod=vendor go test ./pkg/tool -run 'TestProcessContract|TestProcessError|TestWorkspaceAccess'
```

Expected: FAIL because `process.go` and its types do not exist.

**Step 3: Implement the minimum public contracts**

Implement enums with explicit validation, a typed `ProcessError` supporting
`Error`, `Unwrap`, and `Is`, defensive copies for slices, and stdlib-only
dependencies plus existing Harness identity types. Keep model-facing process
handles out of this runner layer.

**Step 4: Verify GREEN**

Re-run the exact focused command from Step 2. Expected: PASS.

**Step 5: Commit**

```bash
git add pkg/tool/process.go pkg/tool/process_test.go pkg/tool/README.md
git commit -m "feat(tool): define async process contracts"
```

### Task 2: Add a keyed session-resource registry and process binding

**Files:**

- Create: `../harness/pkg/tool/session_resource.go`
- Modify: `../harness/pkg/tool/definition.go`
- Modify: `../harness/pkg/tool/definition_test.go`
- Create: `../harness/pkg/rig/session_resource_storage.go`
- Create: `../harness/pkg/rig/session_resource_storage_test.go`
- Modify: `../harness/pkg/rig/options.go`
- Modify: `../harness/pkg/rig/definition.go`
- Create: `../harness/internal/sessionruntime/session_resources.go`
- Create: `../harness/internal/sessionruntime/session_resources_test.go`
- Modify: `../harness/internal/sessionruntime/session.go`
- Modify: `../harness/internal/sessionruntime/restore_constructor.go`

The registry is created before restore planning, survives the probe-to-live
transition, and is activated only when the real Session, hub, publisher, and
notifier exist.

Execute each subsection below as its own implementer assignment, RED/GREEN
cycle, review pair, focused commit checks, and commit.

**2A — public contracts and binding attenuation**

Files: `pkg/tool/session_resource.go`, `pkg/tool/definition.go`, and
`pkg/tool/definition_test.go`. First add
`TestProcessBindingRequiresRegistry`,
`TestProcessBindingRejectsTypedNilServices`, and
`TestAttenuateBindingsPreservesOnlyRequiredProcessServices`, then run:

```bash
cd /Users/ipotter/code/looprig/.worktrees/long-running-commands/harness
GOWORK=off GOCACHE=/private/tmp/looprig-harness-gocache GOFLAGS=-mod=vendor go test ./pkg/tool -run 'Test(ProcessBinding|AttenuateBindings.*Process)'
```

Expected RED: missing contracts/requirement. Add `SessionResource`,
`SessionResourceRegistry`, `SessionResourceServices`, `ProcessBinding`, and
`RequiresProcessServices`, including typed-nil validation, cloning, and
attenuation. Re-run the exact command, and commit only those
three files as `feat(tool): bind session process resources`.

Use this public resource shape:

```go
type SessionResource interface {
	Activate(context.Context, SessionResourceServices) error
	Shutdown(context.Context) error
}

type SessionResourceRegistry interface {
	GetOrCreate(context.Context, string, func(string) (SessionResource, error)) (SessionResource, error)
}
```

The factory receives its private storage directory. In 2A,
`SessionResourceServices` is deliberately an empty staged late-binding carrier:

```go
type SessionResourceServices struct{}
```

Do not invent `any`, callback, event, command, or opaque publisher payloads in
this microtask. The exact bounded service DTOs cannot be correct until Task 4
defines the process lifecycle domain, and `pkg/tool` cannot import `pkg/event`
without reversing the dependency boundary. The typed-nil tests in 2A apply to
the `ProcessBinding` registry/runner interfaces. Task 4 completes the carrier
with privately held, typed lifecycle and completion services and their
constructor validation.

**2B — registry linearization**

Files: `internal/sessionruntime/session_resources.go` and
`internal/sessionruntime/session_resources_test.go`. First add
`TestSessionResourcesGetOrCreateSingleFlight`,
`TestSessionResourcesCreationFailureCanRetry`,
`TestSessionResourcesShutdownWinsCreationRace`, and
`TestSessionResourcesActivateAndShutdownOnce`. The 32-caller test must prove one
factory call and one returned resource; creation failure must be retryable;
shutdown racing creation must close rather than leak; activation/shutdown and
deterministic error aggregation are at most once. Run:

```bash
GOWORK=off GOCACHE=/private/tmp/looprig-harness-gocache GOFLAGS=-mod=vendor go test ./internal/sessionruntime -run '^TestSessionResources'
```

Expected RED: no concrete registry. Implement only registry creation,
activation, admission close, and shutdown linearization. Re-run the focused
non-race command, then commit these two files as
`feat(session): manage shared session resources`.

**2C — durable storage provider**

Files: `pkg/rig/session_resource_storage.go`,
`pkg/rig/session_resource_storage_test.go`, `pkg/rig/options.go`, and
`pkg/rig/definition.go`. First add
`TestRigRequiresResourceStorageForProcessDefinitions`,
`TestRigRejectsNilResourceStorageProvider`,
`TestRigRejectsTypedNilResourceStorageProvider`, and
`TestRigDefinitionCapturesResourceStorageProviderImmutably`, then run:

```bash
GOWORK=off GOCACHE=/private/tmp/looprig-harness-gocache GOFLAGS=-mod=vendor go test ./pkg/rig -run 'Test(RigRequiresResourceStorage|ResourceStorage)'
```

Expected RED: no provider option. Add only the public definition-time provider
contract and option capture in this microtask:

```go
type SessionResourceStorage struct {
	Path     string
	Identity string
}

type SessionResourceStorageProvider interface {
	StorageForSession(context.Context, uuid.UUID) (SessionResourceStorage, error)
}
```

The Rig definition requires the provider when any definition declares
`RequiresProcessServices`, rejects nil and typed-nil providers, and captures the
selected provider immutably. It does not call the provider, mint a SessionID,
create a directory, or decide new-versus-restore identity in `pkg/rig`.
Re-run the exact command, run the commit-boundary `gofmt` and diff checks, then
commit these files as
`feat(rig): provide durable session resource storage`.

**2D — restore late binding**

Files: `pkg/rig/definition.go`,
`pkg/rig/session_resource_storage_test.go`,
`internal/sessionruntime/lifecycle.go`,
`internal/sessionruntime/session.go`,
`internal/sessionruntime/restore_constructor.go`,
`internal/sessionruntime/session_resources.go`, and
`internal/sessionruntime/session_resources_test.go`. First add
`TestResourceStorageStableAcrossRestore`,
`TestResourceStorageRejectsIdentityMismatch`,
`TestResourceStorageUnavailableFailsConstruction`,
`TestRestorePlanningAndLiveBindingsShareResourceRegistry` and
`TestRestoreDoesNotPublishBeforeResourceBridgeActivation`, plus
`TestForeignLoopRejectsProcessServices`, then run:

```bash
GOWORK=off GOCACHE=/private/tmp/looprig-harness-gocache GOFLAGS=-mod=vendor go test ./pkg/rig ./internal/sessionruntime -run 'Test(ResourceStorage|Restore.*Resource|ForeignLoopRejectsProcessServices)'
```

Expected RED: storage is not resolved at a lifecycle point with a known
SessionID, and probe/live bindings do not share a bridge.

For a new session, mint the SessionID first, resolve
`StorageForSession(ctx, sessionID)`, canonicalize and validate the returned root
outside the workspace, then atomically create or validate a durable identity
anchor inside that private resource root (or equivalently in the session's
durable journal) before binding any process-enabled definition. The anchor is
owner-only, versioned, and binds the exact SessionID plus provider-supplied
Identity; write-new/sync/atomic-replace rules apply. For restore,
resolve and validate the same provider result and identity anchor before
`planLoops`; never create a fresh identity over an existing mismatched root.
An unavailable provider, invalid path, missing/corrupt restore anchor, or
identity mismatch aborts construction before process binding, resource factory
execution, or bridge activation.

Thread the resolved storage, same registry, and inert bridge through
`planLoops`, `buildRestoredSession`, and `attachRestoredLoop`; activate only
after the live Session, hub, publisher, and notifier exist. At this stage
activation proves ordering with the empty
`SessionResourceServices` carrier; Task 4 replaces that staged value with the
validated typed service set before any real process resource is composed.
Reject process-enabled definitions on non-native engines with
`process_notifications_unsupported`; legacy foreground-only Bash retains its
existing foreign-engine behavior. Never expose `*sessionruntime.Session`
publicly. Re-run the
focused command. Run the standard focused non-race commit checks and commit
these files as
`feat(session): activate process resources after restore`.

### Task 3: Add lifetime workspace leases

**Files:**

- Modify: `../harness/pkg/tool/definition.go`
- Modify: `../harness/internal/sessionruntime/workspace_coordinator.go`
- Modify: `../harness/internal/sessionruntime/workspace_coordinator_test.go`
- Modify: `../harness/internal/sessionruntime/workspace_restore.go`

**Step 1: Write failing compatibility tests**

Add a separate `WorkspaceLifetimeCoordinator` capability with
`AcquireLifetime(context.Context, WorkspaceAccess)`. Do not widen the existing
single-path `WorkspaceCoordinator.Acquire` signature or encode multi-scope
leases into one string. Use canonical path/tree scopes and preserve existing
operation values.

Test:

```text
read lease + path mutation                       => concurrent
read lease + checkpoint                          => concurrent
scoped writer /ws/a + mutation /ws/a/file       => blocked
scoped writer /ws/a/file + mutation /ws/a        => blocked
scoped writer /ws/a + mutation /ws/b             => concurrent
broad writer + any mutation                      => blocked
any writable lifetime lease + checkpoint         => blocked
waiting checkpoint + newer writable lease        => checkpoint wins
canceled waiter                                  => removed and successor wakes
double release                                   => harmless
```

**Step 2: Verify RED**

```bash
cd /Users/ipotter/code/looprig/.worktrees/long-running-commands/harness
GOWORK=off GOCACHE=/private/tmp/looprig-harness-gocache GOFLAGS=-mod=vendor go test ./internal/sessionruntime -run 'TestWorkspaceCoordinator.*Lifetime'
```

Expected: FAIL because lifetime operations and tree overlap do not exist.

**Step 3: Implement overlap and fairness**

Replace exact-path equality with component-aware ancestor/descendant overlap.
Preserve FIFO writer preference and existing whole/checkpoint behavior. Store
defensive copies of scope lists. Make restore stop registered resources before
acquiring the checkpoint permit, and always resume admission on failure.

**Step 4: Verify GREEN**

Re-run the exact focused command from Step 2. Expected: PASS.

**Step 5: Commit**

```bash
git add pkg/tool/definition.go internal/sessionruntime/workspace_coordinator.go internal/sessionruntime/workspace_coordinator_test.go internal/sessionruntime/workspace_restore.go
git commit -m "feat(workspace): coordinate background process leases"
```

### Task 4: Add metadata-only process lifecycle events

**Files:**

- Modify: `../harness/pkg/tool/session_resource.go`
- Create: `../harness/pkg/tool/session_resource_test.go`
- Modify: `../harness/pkg/tool/process.go`
- Modify: `../harness/pkg/tool/process_test.go`
- Create: `../harness/pkg/event/process.go`
- Create: `../harness/pkg/event/process_test.go`
- Modify: `../harness/pkg/event/event.go`
- Modify: `../harness/pkg/event/doc.go`
- Modify: `../harness/pkg/event/marshal.go`
- Modify: `../harness/pkg/event/validate.go`
- Modify: `../harness/pkg/event/marshal_test.go`
- Modify: `../harness/pkg/event/validate_test.go`
- Modify: `../harness/pkg/event/header_test.go`
- Create: `../harness/internal/sessionruntime/process_services.go`
- Create: `../harness/internal/sessionruntime/process_services_test.go`
- Modify: `../harness/internal/sessionruntime/session_resources.go`
- Create: `../harness/internal/sessionruntime/process_services_integration_test.go`

**Step 1: Write failing event tests**

Define `ProcessStarted`, `ProcessBackgrounded`, `ProcessCompleted`,
`ProcessStopRequested`, and `ProcessLost`.

First add `TestProcessLifecycleMetadataValidation`,
`TestProcessLifecycleKindStateReasonMatrix`,
`TestProcessLifecycleStopRequestedInvariants`,
`TestProcessLifecycleTimestampsPreserved`,
`TestProcessCompletionNotificationValidation`,
`TestSessionResourceServicesRejectsTypedNilLifecyclePublisher`, and
`TestSessionResourceServicesRejectsTypedNilCompletionNotifier` in `pkg/tool`.
Task 4 replaces 2A's empty staged carrier with a constructor-validated,
immutable service set. Use this shape (exact enum constant names may follow
package conventions, but the closed fields and bounds may not be widened):

```go
const (
	MaxProcessHandleBytes     = 128
	MaxProcessDiagnosticBytes = 512
)

type ProcessLifecycleKind uint8
type ProcessLifecycleState uint8

type ProcessLifecycleMetadata struct {
	EventID           uuid.UUID
	Kind              ProcessLifecycleKind
	SessionID         uuid.UUID
	LoopID            uuid.UUID
	ProcessHandle     string
	OriginExecutionID uuid.UUID
	State             ProcessLifecycleState
	ProcessCreatedAt  time.Time
	ProcessStartedAt  time.Time
	ProcessFinishedAt time.Time
	HasExitCode       bool
	ExitCode          int32
	Reason            ProcessTerminalReason
	Diagnostic        string
}

type ProcessCompletionNotification struct {
	CommandID       uuid.UUID
	SessionID       uuid.UUID
	LoopID          uuid.UUID
	ProcessHandle   string
	State           ProcessLifecycleState
	Reason          ProcessTerminalReason
}

type ProcessLifecyclePublisher interface {
	PublishProcessLifecycle(context.Context, ProcessLifecycleMetadata) error
}

type ProcessCompletionNotifier interface {
	NotifyProcessCompletion(context.Context, ProcessCompletionNotification) error
}
```

Define exactly these closed lifecycle kinds:
`ProcessLifecycleStarted`, `ProcessLifecycleBackgrounded`,
`ProcessLifecycleCompleted`, `ProcessLifecycleStopRequested`, and
`ProcessLifecycleLost`. Define exactly these closed lifecycle states:
`ProcessLifecycleStarting`, `ProcessLifecycleRunning`,
`ProcessLifecycleExited`, `ProcessLifecycleFailed`,
`ProcessLifecycleTimedOut`, `ProcessLifecycleInterrupted`,
`ProcessLifecycleTerminated`, `ProcessLifecycleKilled`, and
`ProcessLifecycleLostOnRestore`.

Reuse the bounded `ProcessTerminalReason` already defined by Task 1 in
`pkg/tool/process.go`; never redeclare or shadow it. Extend that existing enum
with `ProcessTerminalFailed`, `ProcessTerminalOutputLimit`, and
`ProcessTerminalLostOnRestore`, appended after the existing constants without
renumbering them, and extend its `Valid` method accordingly. Its complete
lifecycle matrix is:

| Kind | State | Allowed reason | Timestamp/exit invariants |
| --- | --- | --- | --- |
| started | running | zero | created/start required; finished/exit absent |
| backgrounded | running | zero | created/start required; finished/exit absent |
| stop-requested | starting or running | interrupted, terminated, or killed | created required; start required iff running; finished/exit absent |
| completed | exited | exited | created/start/finished required; exit required |
| completed | failed | failed | created/finished required; start optional; exit absent |
| completed | timed-out | timed-out | created/start/finished required; exit absent |
| completed | interrupted | interrupted | created/start/finished required; exit absent |
| completed | terminated | terminated, runner-shutdown, or output-limit | created/start/finished required; exit absent |
| completed | killed | killed, runner-shutdown, or output-limit | created/start/finished required; exit absent |
| lost | lost-on-restore | lost-on-restore | created/finished required; start optional; exit absent |

Every combination not listed is invalid. A stop-requested record is
nonterminal: it carries the requested portable signal as its reason, never
`ProcessFinishedAt`, an exit code, `runner-shutdown`, `output-limit`, `failed`, or
`lost-on-restore`. `Diagnostic` must be empty except for failed or lost records.
`ProcessCompletionNotification` accepts only the terminal state/reason pairs
from the completed and lost rows; starting/running and stop-request semantics
are invalid for a completion notification.

Task 5's Tools-owned supervision state remains an internal explicit string
enum. Its lifecycle sink maps each value mechanically to
`pkg/tool.ProcessLifecycleState`; the two types are not aliases and must not be
conflated across the module boundary.

`NewSessionResourceServices` accepts both interfaces, rejects nil and typed-nil
implementations, and stores them in unexported fields exposed only through
read-only accessors. Once Task 4 lands the zero service set is invalid:
session-runtime activation constructs and validates the value before invoking a
resource, and resources fail closed if handed an unvalidated zero value. Do not
use `any` or a generic callback. Process handles
must be non-empty URL-safe opaque tokens no longer than
`MaxProcessHandleBytes`; diagnostics must be valid UTF-8 and at most
`MaxProcessDiagnosticBytes`. No DTO has command, output, stdin, environment,
host path, spool path, OS PID, or another free-form field. Exit metadata uses
`HasExitCode` rather than a caller-retained pointer. Validation requires
non-zero `ProcessCreatedAt` and applies the matrix above; every non-zero
`ProcessStartedAt` must be at or after creation, and every non-zero
`ProcessFinishedAt` must be at or after both creation and start.
`HasExitCode` is valid only for completed/exited; when false, `ExitCode` must be
zero.

Tools owns and atomically persists `ProcessCreatedAt`, `ProcessStartedAt`, and
`ProcessFinishedAt` with the process manifest before it invokes this service.
Harness validates and preserves those exact values in the concrete event
payload. The `Process` prefix distinguishes these lifecycle clocks from the
Harness event Header's envelope `CreatedAt`; Task 24 must never replace the
process clocks. Harness adds only event-envelope metadata that is not
represented by this DTO.

Round-trip each event through the existing sealed event codec. Reject:

- zero session, loop, process handle, or origin execution ID;
- started events with terminal fields;
- completed/lost events with nonterminal states;
- invalid state/reason combinations;
- command text, output, stdin, host path, OS PID, or unbounded diagnostics;
- diagnostics over the fixed 512-byte UTF-8 limit.

**Step 2: Verify RED**

```bash
cd /Users/ipotter/code/looprig/.worktrees/long-running-commands/harness
GOWORK=off GOCACHE=/private/tmp/looprig-harness-gocache GOFLAGS=-mod=vendor go test ./pkg/tool ./pkg/event ./pkg/journal ./pkg/sessionstore ./internal/sessionruntime -run 'Test(Process|SessionResourceServices)'
```

Expected: FAIL because the typed service DTOs and process events are absent.

**Step 3: Implement event types and codec dispatch**

Complete `SessionResourceServices` with the typed, privately held services
above. Map `ProcessLifecycleMetadata.Kind` to the five sealed `pkg/event`
concrete types. Event payload fields use the bounded enums from `pkg/tool`; event
validation repeats the cross-field invariants and requires Header coordinates
and EventID to match the neutral DTO rather than replacing them. This dependency
direction (`pkg/event` imports `pkg/tool`) avoids a cycle. Keep the journal
record format unchanged; process events use the existing generic event envelope.
Replace 2D's empty activation value with a session-runtime-owned typed bridge
that implements both service interfaces, and construct
`SessionResourceServices` through its validated constructor. Before Task 24
binds the durable publisher/notifier implementation, method calls on that bridge
return an explicit unavailable error rather than dropping a lifecycle record or
notification. The bridge object itself remains stable across probe, restore,
and live activation. Task 24 attaches checked durable publication/delivery
behind it; it does not replace the bridge captured by resources. Task 4's
Harness-local fake resource integration must therefore activate with a fully
validated service set, never the staged empty value.

**Step 4: Verify GREEN**

Re-run the exact focused command from Step 2. Expected: PASS.

**Step 5: Commit**

```bash
git add pkg/tool/session_resource.go pkg/tool/session_resource_test.go pkg/tool/process.go pkg/tool/process_test.go pkg/event internal/sessionruntime/process_services.go internal/sessionruntime/process_services_test.go internal/sessionruntime/session_resources.go
git commit -m "feat(event): record process lifecycle metadata"
```

**Step 6: Add and prove the Phase 1 integration test**

Create the file with:

```go
//go:build integration
```

`TestProcessServicesIntegrationNewRestoreAndLease` must construct through the
public Rig/session API with a temp resource-storage provider. It defines four
Harness-local external-package fake `tool.Definition`s that declare
`RequiresProcessServices`; it does not import Tools or use the future Bash and
process definitions. Bind those fakes, verify one shared registry across
new/restore, and exercise a lifetime scoped-write/checkpoint conflict.

Verify the test exists and runs:

Defer tagged integration discovery and execution to Phase Gate 1, where the
exact test name must be listed before the test is run.

Commit this integration file separately so its reviewed SHA is part of Phase
Gate 1. Its tagged discovery and execution remain exclusively at the gate:

```bash
git add internal/sessionruntime/process_services_integration_test.go
git commit -m "test(session): integrate process service contracts"
```

Dispatch the normal task spec and quality/security reviewers over this
integration-test commit before entering Phase Gate 1.

## PHASE GATE 1: Harness contracts

Run:

```bash
cd /Users/ipotter/code/looprig/.worktrees/long-running-commands/harness
GOWORK=off GOCACHE=/private/tmp/looprig-harness-gocache GOFLAGS=-mod=vendor go test -race ./pkg/tool ./pkg/event ./pkg/journal ./pkg/sessionstore ./pkg/rig ./internal/sessionruntime
GOWORK=off GOCACHE=/private/tmp/looprig-harness-gocache GOFLAGS=-mod=vendor go test -tags integration -list '^TestProcessServicesIntegration' ./internal/sessionruntime
GOWORK=off GOCACHE=/private/tmp/looprig-harness-gocache GOFLAGS=-mod=vendor go test -tags integration -race ./internal/sessionruntime -run '^TestProcessServicesIntegration'
make secure
CGO_ENABLED=0 GOWORK=off GOCACHE=/private/tmp/looprig-harness-gocache GOFLAGS=-mod=vendor go build -trimpath ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOWORK=off GOCACHE=/private/tmp/looprig-harness-gocache GOFLAGS=-mod=vendor go build -trimpath ./...
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 GOWORK=off GOCACHE=/private/tmp/looprig-harness-gocache GOFLAGS=-mod=vendor go build -trimpath ./...
```

No fuzz target exists yet. `make secure` is the sole phase-boundary invocation
of Harness format checks, Vet, Staticcheck, Gosec, and Govulncheck. Record the
integration list output and every command result in the verification ledger.

Phase spec review must confirm:

- Harness contains no Bash schema or spool implementation;
- process bindings attenuate correctly;
- restore uses one late-bound registry/bridge;
- writable leases and checkpoints cannot race.

Phase quality/security review must inspect registry single-flight, shutdown
linearization, path overlap, event validation, and race coverage.

## Phase 2: Tools supervisor core

### Task 5: Define process identity, state, errors, and quotas

**Files:**

- Create: `process/config.go`
- Create: `process/config_test.go`
- Create: `process/errors.go`
- Create: `process/errors_test.go`
- Create: `process/identity.go`
- Create: `process/identity_test.go`
- Create: `process/state.go`
- Create: `process/state_test.go`
- Create: `process/types.go`

**Step 1: Write failing domain tests**

Test:

- URL-safe CSPRNG handles contain at least 128 random bits;
- generator failure and collision retry are typed;
- handles contain no owner, path, timestamp, or OS process identifier;
- authority owner is exactly SessionID+LoopID;
- origin ToolExecutionID is immutable provenance but not follow-up authority;
- only approved state transitions are accepted;
- terminal states never transition;
- error codes round-trip and support `errors.Is`;
- invalid/negative limits are rejected;
- zero configuration receives documented defaults;
- per-process values cannot exceed aggregate limits.

Use:

```go
type Owner struct {
	SessionID uuid.UUID
	LoopID    uuid.UUID
}

type Origin struct {
	ToolExecutionID uuid.UUID
}
```

**Step 2: Verify RED**

```bash
GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go test ./process -run 'Test(Handle|Owner|State|Error|Config)'
```

Expected: FAIL because package `process` does not exist.

**Step 3: Implement the minimum domain**

Use explicit string enums, unexported transition validation, defensive
configuration normalization, and dependency injection for random bytes and
clock in tests. Do not expose a PID field.

**Step 4: Verify GREEN**

Re-run the exact focused command from Step 2. Expected: PASS.

**Step 5: Commit**

```bash
git add process
git commit -m "feat(process): define supervision domain"
```

### Task 6: Add atomic manifests and bounded spools

**Files:**

- Create: `internal/atomicfile/replace.go`
- Create: `internal/atomicfile/replace_test.go`
- Create: `internal/atomicfile/syncdir_unix.go`
- Create: `internal/atomicfile/syncdir_windows.go`
- Create: `process/manifest.go`
- Create: `process/manifest_test.go`
- Create: `process/spool.go`
- Create: `process/spool_test.go`

**Step 1: Write failing storage tests**

Test atomic replacement with injected failures at create, write, file-sync,
rename, and directory-sync boundaries. The old manifest must remain readable
until the new one is fully committed.

Test manifest invariants:

- versioned JSON;
- owner and origin required;
- state/cursors never move backward;
- terminal result immutable;
- stable started/backgrounded/completed/lost EventIDs and completion CommandID
  are allocated and persisted before publication;
- completion-published marker monotonic but not relied on for deduplication;
- unknown version, malformed JSON, invalid owner, and impossible state are
  `manifest_corrupt`;
- persisted OS metadata is unexported and ignored during restore teardown.

Test spool invariants:

- append order determines global cursor;
- exact ceiling succeeds;
- the next byte returns `output_limit`;
- reads are bounded and cursor-addressed;
- truncated/corrupt files return `spool_corrupt`;
- no path escapes the private resource directory;
- close and removal are idempotent.

**Step 2: Verify RED**

```bash
GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go test ./internal/atomicfile ./process -run 'Test(Atomic|Manifest|Spool)'
```

Expected: FAIL because storage does not exist.

**Step 3: Implement atomic persistence**

Use `os.OpenFile` with owner-only permissions, full-write loops, file `Sync`,
same-directory rename, and directory sync where supported. Never write inside
the workspace. Keep spool and manifest paths unexported.

**Step 4: Verify GREEN**

Re-run the exact focused command from Step 2 once. Repeated failure-injection
stress is deferred to Phase Gate 2's race suite.

**Step 5: Commit**

```bash
git add internal/atomicfile process/manifest.go process/manifest_test.go process/spool.go process/spool_test.go
git commit -m "feat(process): persist manifests and bounded output"
```

### Task 7: Add cursor windows and safe output rendering

**Files:**

- Create: `process/buffer.go`
- Create: `process/buffer_test.go`
- Create: `internal/safetext/normalize.go`
- Create: `internal/safetext/normalize_test.go`
- Create: `process/render.go`
- Create: `process/render_test.go`

**Step 1: Write failing buffer tests**

Cover empty, partial, exact-capacity, wraparound, multiple wraps, arbitrary
cursor, gap, cursor-ahead, and concurrent append/read. Cursor values are raw
combined-stream byte offsets and never rune indexes.

**Step 2: Write failing safe-text tests**

Cover:

- invalid UTF-8;
- C0/C1 controls except approved whitespace;
- CSI, OSC, and DCS sequences;
- terminal sequences split across append/read boundaries;
- NUL-heavy and high-entropy binary detection;
- safe text unchanged;
- model text capped without splitting replacement sequences;
- `base64` mode returns exact raw bytes;
- artifact descriptor is opaque and contains no path.

**Step 3: Verify RED**

```bash
GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go test ./internal/safetext ./process -run 'Test(Buffer|Cursor|SafeText|Render|Base64|Artifact)'
```

Expected: FAIL.

**Step 4: Implement buffer and renderer**

The rolling window serves recent reads; the spool remains authoritative. A gap
returns the earliest retained data with `gap:true`. Cursor-ahead is a typed
error. Base64 reads use the same owner check and byte limits as safe text.

**Step 5: Verify GREEN**

Re-run the exact focused command from Step 3 once. Fuzzing is deferred to Phase
Gate 2.

**Step 6: Commit**

```bash
git add process/buffer.go process/buffer_test.go process/render.go process/render_test.go internal/safetext
git commit -m "feat(process): render cursor-safe bounded output"
```

### Task 8: Implement supervisor admission and terminalization

**Files:**

- Create: `process/entry.go`
- Create: `process/entry_test.go`
- Create: `process/supervisor.go`
- Create: `process/supervisor_test.go`
- Create: `process/fake_runner_test.go`

Execute as four reviewed microtasks:

- **8A — admission and quotas:** `TestSupervisorReservesQuotaBeforePrepare`,
  `TestSupervisorPrepareFailureReleasesQuota`, and
  `TestSupervisorRejectsSessionAndLoopQuota`.
- **8B — durable handoff and stream drain:**
  `TestSupervisorPersistsBeforeReturningHandle`,
  `TestSupervisorDrainsOrderedStreams`, and
  `TestSupervisorOutputLimitStopsProcess`.
- **8C — terminal arbiter and lifecycle IDs:**
  `TestSupervisorTerminalRaceChoosesOnce`,
  `TestSupervisorPublishesStableLifecycleIDs`, and
  `TestSupervisorReleasesLeaseOnce`.
- **8D — retention, observations, and admission close:**
  `TestSupervisorNeverEvictsRunning`,
  `TestSupervisorEvictsCompletedLRU`,
  `TestSupervisorInvalidatesObservations`, and
  `TestSupervisorShutdownRejectsAdmission`.

Run the microtasks as follows; the broader steps below are phase acceptance,
not a consolidated implementation assignment:

- **8A:** edit `process/supervisor.go`, `process/supervisor_test.go`, and
  `process/fake_runner_test.go`; RED/GREEN command
  `go test ./process -run '^TestSupervisor(ReservesQuotaBeforePrepare|PrepareFailureReleasesQuota|RejectsSessionAndLoopQuota)$'`;
  RED is missing admission/quota reservation. Implement reservation and rollback
  only, and commit `feat(process): reserve supervisor admission`.
- **8B:** edit `process/entry.go`, `process/entry_test.go`,
  `process/supervisor.go`, and `process/supervisor_test.go`; RED/GREEN command
  `go test ./process -run '^TestSupervisor(PersistsBeforeReturningHandle|DrainsOrderedStreams|OutputLimitStopsProcess)$'`;
  RED is missing durable handoff/drain. Implement manifest-before-handoff and
  bounded stream ownership only, and commit
  `feat(process): persist and drain supervised processes`.
- **8C:** edit `process/entry.go`, `process/entry_test.go`,
  `process/supervisor.go`, and `process/supervisor_test.go`; RED/GREEN command
  `go test ./process -run '^TestSupervisor(TerminalRaceChoosesOnce|PublishesStableLifecycleIDs|ReleasesLeaseOnce)$'`;
  RED is missing one-shot terminal arbitration. Implement only the CAS
  terminal path and pre-persisted IDs, verify with the focused command, and
  commit `feat(process): arbitrate terminal lifecycle`.
- **8D:** edit `process/entry.go`, `process/entry_test.go`,
  `process/supervisor.go`, and `process/supervisor_test.go`; RED/GREEN command
  `go test ./process -run '^TestSupervisor(NeverEvictsRunning|EvictsCompletedLRU|InvalidatesObservations|ShutdownRejectsAdmission)$'`;
  RED is missing retention/activity/admission-close behavior. Implement
  deterministic terminal LRU, spawn/activity/end invalidation, activity-channel
  drain/closure handling, and admission close only; verify with the focused
  command and commit
  `feat(process): retain entries and invalidate observations`.

**Task 8 combined acceptance**

Use deterministic Harness async-runner/process fakes. Test:

- reserve process, loop, session, memory, and spool quotas before spawn;
- failed setup releases every reservation and lease;
- manifest reaches durable `starting` before a handle can be returned;
- lifecycle sink receives the pre-persisted started EventID exactly once;
- `running` follows successful spawn;
- explicit/yield handoff emits backgrounded with its stable EventID;
- cancellation before handoff cancels start;
- invocation cancellation after handoff does not cancel lifetime;
- owner mismatch is exactly `not_found`;
- natural exit, spawn failure, timeout, output-limit, stop, and shutdown race
  through one terminal compare-and-set;
- one terminal manifest, one completion callback, one lease release;
- completed/lost publication uses the stable manifest IDs on every retry;
- per-process observations invalidate at spawn and completion, and activity
  notifications trigger intermediate invalidation;
- running entries are never evicted;
- terminal entries use deterministic LRU retention;
- shutting down rejects admission and input.

After 8A–8D are individually committed and reviewed, defer their combined race
suite to Phase Gate 2. `Start` must accept the bound owner, origin, prepared process,
workspace lease, lifecycle sink, observation capability, storage ceiling, and
initial yield settings. One entry goroutine owns wait, activity, and stream
drain; terminalization is idempotent.

### Task 9: Add waiters, restore reconciliation, and shutdown

**Files:**

- Create: `process/wait.go`
- Create: `process/wait_test.go`
- Create: `process/restore.go`
- Create: `process/restore_test.go`
- Create: `process/shutdown_test.go`
- Create: `process/supervisor_integration_test.go`
- Modify: `process/supervisor.go`
- Modify: `process/entry.go`

Execute as four reviewed microtasks:

- **9A — poll/any/all waiters:** exact tests
  `TestWaitPollReturnsImmediately`, `TestWaitAnyWakesOnAppend`,
  `TestWaitAllRequiresEveryEntry`, and `TestWaitCancelRemovesWaiter`.
- **9B — restore and stable publication IDs:** exact tests
  `TestRestoreCompletedOutput`, `TestRestoreRunningBecomesLost`,
  `TestRestoreNeverSignalsPersistedPID`, and
  `TestRestorePublicationCrashRetriesStableID`.
- **9C — coordinated shutdown:** exact tests
  `TestShutdownClosesAdmissionBeforeStop`,
  `TestShutdownEscalatesAndConfirmsTrees`,
  `TestShutdownConcurrentCallersShareResult`, and
  `TestShutdownTeardownFailureRetainsAuthority`.
- **9D — filesystem/subprocess acceptance:** exact tagged tests
  `TestSupervisorIntegrationPersistRestore` and
  `TestSupervisorIntegrationShutdownAndRestore`.

Execute independently:

- **9A:** create `process/wait.go` and `process/wait_test.go`, modifying
  `process/supervisor.go` only as needed. RED/GREEN command:
  `go test ./process -run '^TestWait(PollReturnsImmediately|AnyWakesOnAppend|AllRequiresEveryEntry|CancelRemovesWaiter)$'`.
  RED is the absent waiter API. Implement generation-based waiter fan-out and
  quota only, verify with the focused command, and commit
  `feat(process): wait on process generations`.
- **9B:** create `process/restore.go`, `process/restore_test.go`, and
  `process/supervisor_integration_test.go`, modifying `process/entry.go` and
  `process/supervisor.go` only as needed. RED command:
  `go test ./process -run '^TestRestore(CompletedOutput|RunningBecomesLost|NeverSignalsPersistedPID|PublicationCrashRetriesStableID)$'`.
  RED is absent reconciliation. Implement reopen/lost reconciliation and stable
  publication retries without live PID use. Defer the tagged
  `TestSupervisorIntegrationPersistRestore` execution to Phase Gate 2, then
  commit `feat(process): restore supervised process state`.
- **9C:** create `process/shutdown_test.go`, modifying
  `process/supervisor.go` and `process/entry.go`. RED/GREEN command:
  `go test ./process -run '^TestShutdown(ClosesAdmissionBeforeStop|EscalatesAndConfirmsTrees|ConcurrentCallersShareResult|TeardownFailureRetainsAuthority)$'`.
  RED is absent coordinated shutdown. Implement close-admission,
  terminate/escalate/confirm, shared result, and retained cleanup authority
  only; verify with the focused command, and commit
  `feat(process): shut down process trees`.
- **9D:** modify only `process/supervisor_integration_test.go`. Add
  `TestSupervisorIntegrationShutdownAndRestore` with
  focused default-tag seam tests after 9C. Tagged execution is deferred to
  Phase Gate 2; an actual gate failure starts a focused nested RED/GREEN fix.
  Commit
  `test(process): integrate shutdown and restore`.

**Task 9 combined acceptance**

Test poll, wait-any, and wait-all for one and multiple ordered processes.
Appending output and terminalization wake waiters without polling. Cancellation
removes waiters. Enforce a configured waiter quota.

**Step 2: Write failing restore/shutdown tests**

Test:

- completed manifests/spools reopen and remain queryable;
- starting/running manifests become `lost_on_restore`;
- persisted OS metadata is never passed to a runner or signal method;
- completion marker avoids needless retries but is not the deduplication
  boundary;
- a crash after journal append but before marker rewrite republishes the same
  EventID/CommandID and is deduplicated;
- fault injection before append, after append, before marker, and after marker
  produces one durable event and one notification;
- corrupt entries are isolated and reported without hiding healthy entries;
- shutdown closes admission, concurrently terminates, escalates, confirms exit,
  flushes, releases leases, and closes storage;
- concurrent shutdown callers receive the same result;
- notification backpressure cannot block terminalization;
- teardown failure retains authority and reports `teardown_failed`.

After 9A–9D are individually committed and reviewed, defer the combined
wait/restore/shutdown race suite to Phase Gate 2. Restore creates no live runner.
Publication retries always reuse IDs already in the manifest; the durable
Harness journal index and restored loop projection implemented in Task 24 are
the actual duplicate-suppression boundaries. Tools unit fakes implement that
same append-result contract.

The integration file begins with `//go:build integration`.
`TestSupervisorIntegrationPersistRestore` covers the 9B boundary;
`TestSupervisorIntegrationShutdownAndRestore` adds 9C shutdown behavior. They
use a real temp resource root, OS pipes, manifest replacement, spool reads, a
subprocess fake, session-style shutdown, and restore. They may fake only the
Harness publisher; filesystem and subprocess boundaries are real.

Phase Gate 2 must list both exact integration test names before executing them.

## PHASE GATE 2: Tools supervisor core

Run:

```bash
cd /Users/ipotter/code/looprig/.worktrees/long-running-commands/tools
GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go test -race ./internal/atomicfile ./internal/safetext ./process
GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go test -tags integration -list '^TestSupervisorIntegration' ./process
GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go test -tags integration -race ./process -run '^TestSupervisorIntegration'
GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go test ./internal/safetext -run '^$' -fuzz '^FuzzNormalize$' -fuzztime=10s
make secure
CGO_ENABLED=0 GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go build -trimpath ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go build -trimpath ./...
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go build -trimpath ./...
```

This is the first execution of the phase's integration and fuzz tests.
`make secure` batches Tools format checks, Vet, Staticcheck, Gosec, and
Govulncheck.

Phase spec review must trace every state transition, cursor rule, quota, manifest
field, restore rule, and completion marker to the approved design.

Phase quality/security review must stress terminal races, atomic replacement,
safe-text framing, base64 raw access, owner isolation, waiter cleanup, and
shutdown authority retention.

## Phase 3: Sandbox asynchronous pipe execution

**Coordination prerequisite:** Sandbox stabilization is merged at `2a21dda`
(`docs: record v0.4.1 stabilization evidence`), with candidate implementation
through `6343ed1`. The feature worktree must descend from that handoff. Its
unrestricted host race suite, portable checks, builds, lint/static/gosec, and
review evidence are recorded, but three stabilization gates remain mandatory:
native privileged Linux Rung-1/Rung-2 execution, live `windows-restricted` and
`windows-elevated` runs on their disposable workers with no mandatory skips or
residue, and the module-pinned vulnerability scan. Obtain and review those
native run IDs/logs at this phase boundary before Task 10. Cross-builds and
Docker probes do not replace runtime evidence.

The stabilized implementation is the base architecture, not disposable code.
Tasks 10–12 must preserve its `executorLifecycle`, delayed cleanup ownership,
`quarantinedSpawn` and later zero-proof reaper, `ExecutorSet.Close` admission and
wait ordering, exact grant/path identity checks, Windows release aggregation,
and existing restricted/elevated Job ownership. This phase adds async behavior
on top and must not duplicate or overwrite those fixes.

### Task 10: Add the public pipe-backed process API

**Files:**

- Create: `../sandbox/internal/exec/process.go`
- Create: `../sandbox/internal/exec/process_errors.go`
- Create: `../sandbox/internal/exec/process_test.go`
- Create: `../sandbox/internal/exec/process_acceptance_test.go`
- Modify: `../sandbox/internal/exec/executor.go`
- Modify: `../sandbox/internal/exec/executor_lifecycle.go`
- Modify: `../sandbox/internal/enforce/enforce.go`
- Modify: `../sandbox/internal/windows/elevated_backend_windows.go`
- Modify: `../sandbox/internal/windows/elevated_execution_bridge_windows.go`
- Modify: `../sandbox/internal/windows/elevated_runner_launcher_windows.go`
- Modify: `../sandbox/internal/windows/elevated_runner_launcher_windows_test.go`
- Modify: `../sandbox/sandbox.go`
- Modify: `../sandbox/facade_test.go`

Sandbox owns its named request/process API. It must not import Harness.

**Step 1: Write failing pipe-process tests**

The acceptance file starts with `//go:build integration` and defines
`TestIntegrationProcessPipeLifecycle` against a real unconfined subprocess.

Require:

```go
type ProcessOptions struct {
	Directory   string
	Command     string
	ExecutionID string
	Grants      []string
	TTY         bool
	Deadline    time.Time
}

type Process struct { /* opaque */ }

type PreparedProcess struct { /* opaque, single-use */ }
```

Test:

- prepare validates but does not spawn;
- prepared access is authoritative and immutable;
- closing an unstarted preparation releases reservations;
- start consumes preparation exactly once;
- stdout streams before process exit;
- stdout/stderr remain distinct in pipe mode;
- stdin write and idempotent EOF;
- nonzero exit is a result, spawn failure is an error;
- `Wait` is cached and safe for concurrent callers;
- `Close` is idempotent;
- public results expose no OS PID;
- canceling prepare/start before handoff prevents a returned process;
- canceling the caller context after handoff does not kill the returned process;
- legacy sync RunCommand behavior is byte-for-byte unchanged when rebuilt on the
  async primitive.
- an optional typed `Activities()` stream reports workspace activity and closes
  before `Wait` returns; invalid activity is reported conservatively as broad.

**Step 2: Verify RED**

Use the exact focused RED/GREEN command specified for each microtask below.
Expected: each command fails for its stated missing behavior before production
code is added.

**Step 3: Refactor shared preparation and implement start**

Execute as three reviewed microtasks:

- **10A — public types and pipe streams:** `PrepareProcess` returns a
  single-use prepared handle; RED tests are
  `TestPrepareProcessDoesNotSpawn`, `TestPreparedProcessStartOnce`,
  `TestPreparedProcessCloseBeforeStart`, and `TestProcessStreamsBeforeExit`.
- **10B — start/handoff contexts:** RED tests are
  `TestPrepareCancellationPreventsHandoff`,
  `TestStartCancellationPreventsHandoff`, and
  `TestCallerCancellationAfterHandoffDoesNotKill`.
- **10C — synchronous compatibility:** refactor `Executor.run` onto the prepared
  path; pre/post characterization tests retain exact RunCommand/RunArgv/granted
  results.
- **10D — elevated asynchronous ownership:** replace the blocking
  `enforce.Spec.Launch` result with an internal backend execution handle and
  transfer the existing `elevatedRunnerExecution` plus stdio bridge to the
  prepared process; never emulate async by wrapping the blocking launch in a
  goroutine.

Execute independently:

- **10A:** create `internal/exec/process.go`,
  `internal/exec/process_errors.go`, and `internal/exec/process_test.go`, plus
  the public facade declarations in `sandbox.go`/`facade_test.go`. RED/GREEN:
  `GOWORK=off GOCACHE=/private/tmp/looprig-sandbox-gocache go test
  ./internal/exec -run '^Test(PrepareProcessDoesNotSpawn|PreparedProcess(StartOnce|CloseBeforeStart)|ProcessStreamsBeforeExit)$'`.
  RED is missing API. Implement public types, immutable effective access, pipe
  streams, cached wait, and optional bounded activity-stream lifecycle only;
  commit `feat: define prepared pipe processes`.
- **10B:** modify `internal/exec/process.go`,
  `internal/exec/process_test.go`, and
  `internal/exec/executor_lifecycle.go`. RED/GREEN:
  `GOWORK=off GOCACHE=/private/tmp/looprig-sandbox-gocache go test
  ./internal/exec -run '^Test(PrepareCancellationPreventsHandoff|StartCancellationPreventsHandoff|CallerCancellationAfterHandoffDoesNotKill)$'`.
  RED is incorrect context ownership. Implement setup-versus-lifetime context
  transfer only; verify with the focused command and commit
  `feat: detach process lifetime after handoff`.
- **10C:** modify `internal/exec/executor.go`,
  `internal/exec/executor_lifecycle.go`, and characterization tests only.
  This is the explicit characterization/refactor exception: exact legacy
  RunCommand, RunArgv, and granted-result tests must be GREEN before and after
  the pure internal refactor. Do not claim a manufactured RED. Run
  `GOWORK=off GOCACHE=/private/tmp/looprig-sandbox-gocache go test
  ./internal/exec -run 'Test(RunCommand|RunArgv|Granted|ExecutorConformance)'`.
  Implement sync prepare/start/drain/wait adaptation only and commit
  `refactor: share prepared process execution`.
- **10D:** modify `internal/enforce/enforce.go`,
  `internal/windows/elevated_backend_windows.go`,
  `internal/windows/elevated_execution_bridge_windows.go`,
  `internal/windows/elevated_runner_launcher_windows.go`,
  `internal/windows/elevated_runner_launcher_windows_test.go`,
  `internal/exec/process.go`, and their focused tests. Characterize the current
  protected launch ordering first: token/host validation, suspended creation,
  Job assignment, resume, Job-empty proof, quarantine on failed proof, and
  lease release only after proof. Then make the internal enforcement seam
  return an owned asynchronous execution whose `Wait` is backed directly by the
  existing `elevatedRunnerExecution`; transfer the stdio bridge and release
  closure with it. The transfer includes two distinct retirement obligations:
  the per-execution broker lease release and the compiled elevated-spec
  `active.Done` registered before launch.

  Ownership is conditional and linearized:

  1. Before successful asynchronous handoff, the launch stack owns both
     obligations.
  2. A failure before creating either a Job or process authority releases the
     broker lease and calls `active.Done` synchronously, each exactly once.
  3. Once a Job exists or process creation may have returned authority, any
     suspended-create, assignment, resume, or later launch failure must first
     obtain exact Job-zero proof. If proof succeeds, that proof path releases
     the broker lease and calls `active.Done`; if proof is delayed or
     indeterminate, the full failed-launch capsule—including Job/process
     handles, streams, grant/path/proxy/backend releases, broker release, and
     `active.Done`—moves atomically to process-level quarantine. Only successful
     quarantine proof/cleanup retires those obligations.
  4. A successful handoff atomically transfers both obligations to the returned
     execution; the launch stack retains neither.

  The compiled spec's `Release` must remain blocked while any such `active`
  obligation is live, must never return before authority retirement, and must
  not remain permanently blocked after proof/quarantine completion.
  Add `TestElevatedAsyncExecutionRetainsBrokerAndSpecActivityUntilJobZero` and
  `TestElevatedAsyncExecutionQuarantineOwnsRetirements` before changing the
  bridge. Also add
  `TestElevatedAsyncExecutionLaunchFailureRetiresAfterProof` with
  suspended-create, Job-assignment, and resume-failure cases. It proves broker
  release and `active.Done` do not occur before exact proof, occur exactly once
  after direct or quarantined proof, and `Spec.Release` is neither early nor
  permanently blocked.
  RED/GREEN selector:
  `GOWORK=off GOCACHE=/private/tmp/looprig-sandbox-gocache go test
  ./internal/windows ./internal/exec -run
  '^Test(ElevatedAsyncExecution.*|ElevatedRunner.*(Job|Release|Quarantine))$'`.
  Commit `refactor: expose elevated runner execution ownership`.

**Task 10 combined acceptance**

After 10A–10D are individually committed and reviewed, defer their combined
race/conformance suite to Phase Gate 3.

### Task 11: Retain grants and enforcement resources across two-phase start

**Files:**

- Modify: `../sandbox/internal/exec/executor_lifecycle.go`
- Modify: `../sandbox/internal/exec/process.go`
- Modify: `../sandbox/internal/exec/process_quarantine.go`
- Modify: `../sandbox/internal/exec/process_quarantine_test.go`
- Modify: `../sandbox/internal/exec/executor_set.go`
- Modify: `../sandbox/internal/exec/executor_set_test.go`
- Create: `../sandbox/internal/exec/process_lifecycle_test.go`
- Create: `../sandbox/internal/exec/process_grant_test.go`
- Create: `../sandbox/internal/exec/process_grant_integration_test.go`
- Modify: `../sandbox/internal/exec/grant_path_lifecycle_test.go`
- Modify: `../sandbox/internal/exec/executor_proxy_backend_test.go`
- Modify: `../sandbox/internal/policy/pathhandle.go`
- Modify: `../sandbox/internal/policy/pathhandle_linux.go`
- Modify: `../sandbox/internal/policy/enumerate_linux.go`
- Modify: `../sandbox/internal/windows/elevated_execution_bridge_windows.go`

**Step 1: Write failing prepared-resource tests**

The integration file starts with `//go:build integration` and defines
`TestIntegrationProcessPreparedGrantLifetime` against a real supported backend.

Test:

- explicit lifetime/session cancellation kills it;
- ExecutorSet close prevents new starts, terminates live processes, waits for
  terminal cleanup, then returns;
- start/close linearization has no leak;
- grant validation/reservation happens during prepare without child spawn;
- authoritative effective access includes base profile plus approved deltas;
- grant replay/path drift fail during prepare;
- acquiring a workspace lease after prepare and before start cannot change the
  effective access;
- prepared close releases an unused grant reservation without making the token
  replayable;
- path handles, compiled backend cleanup, proxy credential, and route remain
  live through terminalization;
- consumers that never call `Wait` still get cleanup.
- an uncertain terminal state transfers the entire process/grant/proxy/backend
  ownership capsule to the existing `quarantinedSpawn`/retrying zero-proof path;
- `ExecutorSet.Close` closes admission, cancels every live execution, waits for
  normal and delayed cleanup, aggregates release errors once, and never returns
  while authority can still be released by an untracked goroutine.

**Step 2: Verify RED**

```bash
GOWORK=off GOCACHE=/private/tmp/looprig-sandbox-gocache go test ./internal/exec -run 'Test(ProcessLifecycle|AsyncGrant|GrantPath|ExecutorProxy|ExecutorSet)'
```

Expected: retained grant/path/route resource lifetime tests fail because the
first pipe implementation does not yet transfer every enforcement resource
through terminal cleanup.

**Step 3: Implement retained two-phase ownership**

Move grant verification, path handles, route credentials, compiled backend
resources, and effective-access calculation into the prepared object. `Start`
transfers them atomically to a process-owned goroutine. Executor/session close
terminates it and performs wait/cleanup even when the caller abandons the handle.

Preserve the stabilized ownership machinery rather than introducing a second
cleanup system. Grant redemption plus retained Landlock descriptor acquisition
is one prepare transaction: a preparation failure releases every descriptor,
Windows runtime release, proxy route, and compiled resource exactly once, while
the single-spawn grant remains consumed and cannot become replayable. Successful
`Start` performs one ownership transfer into `executorLifecycle`; normal Job/
cgroup zero proof releases it, and an uncertain proof transfers it intact into
the process-level `quarantinedSpawn`. That capsule includes streams, retained
grant/path handles, proxy credentials/routes, transient and compiled backend
release closures, the per-execution broker release, the elevated compiled-spec
`active.Done`, and both executor lifecycle barriers. No callback may stay
reachable only from the old blocking launch stack. Extend the existing
`quarantinedSpawn` ownership type from its stabilized Windows wording into a
platform-neutral process-level capsule that can retain a Windows Job execution
or Linux cgroup lifetime handle. Extend the existing `ExecutorSet.Close` tests
to cover live prepared and started handles.

**Step 4: Verify GREEN**

Re-run the exact focused command from Step 2. Expected: PASS.

**Step 5: Commit**

```bash
git add internal/exec
git commit -m "fix: retain async enforcement through process exit"
```

### Task 12: Add signals, confirmed tree teardown, and parent-death enforcement

**Files:**

- Modify: `../sandbox/internal/exec/process_tree_unix.go`
- Modify: `../sandbox/internal/exec/process_tree_windows.go`
- Modify: `../sandbox/internal/exec/process_tree_other.go`
- Modify: `../sandbox/internal/enforce/enforce.go`
- Modify: `../sandbox/internal/linux/backend.go`
- Modify: `../sandbox/internal/linux/cgroup.go`
- Modify: `../sandbox/internal/linux/cgroup_test.go`
- Modify: `../sandbox/internal/linux/init.go`
- Create: `../sandbox/internal/exec/process_tree_signal_test.go`
- Create: `../sandbox/internal/exec/process_tree_windows_test.go`
- Create: `../sandbox/internal/exec/process_parent_death_unix_test.go`
- Create: `../sandbox/internal/exec/process_parent_death_integration_unix_test.go`
- Create: `../sandbox/internal/exec/lifetime_unix.go`
- Create: `../sandbox/internal/exec/lifetime_other.go`
- Modify: `../sandbox/init_linux.go`
- Modify: `../sandbox/init_other.go`
- Modify: `../sandbox/internal/exec/process.go`
- Modify: `../sandbox/internal/windows/elevated_execution_bridge_windows.go`
- Modify: `../sandbox/internal/windows/elevated_runner_launcher_windows.go`

Execute as four reviewed microtasks:

- **12A — signal state machine:** interrupt, terminate/grace/escalate, kill,
  idempotence, and natural-exit races with a fake process tree.
- **12B — Unix lifetime shim and mandatory Linux containment:** real
  parent-death, grandchild, double-fork, and `setsid` integration tests; every
  supervised spawn must select Rung-1 PID namespace or a delegated cgroup with
  exact empty proof.
- **12C — Darwin fail-closed contract:** return
  `lifetime_enforcement_unavailable` before spawn until a real containment
  primitive exists; this phase does not claim Darwin async execution.
- **12D — existing Windows Job paths:** characterize both restricted and
  elevated suspended-create/Job-before-resume paths, then add signal mapping,
  close, and job-empty confirmation without bypassing either backend.

Execute independently:

- **12A:** files `internal/exec/process.go`,
  `internal/exec/process_tree_signal_test.go`, and platform-neutral fake-tree
  helpers. RED/GREEN command:
  `GOWORK=off GOCACHE=/private/tmp/looprig-sandbox-gocache go test
  ./internal/exec -run '^TestProcessSignal'`. Implement only the signal/terminal
  state machine; rerun the focused non-race command; commit
  `feat: control process signals`.
- **12B:** files `internal/exec/process_tree_unix.go`,
  `internal/exec/lifetime_unix.go`,
  `internal/exec/process_parent_death_unix_test.go`,
  `internal/exec/process_parent_death_integration_unix_test.go`, and
  `internal/enforce/enforce.go`,
  `internal/linux/backend.go`, `internal/linux/cgroup.go`,
  `internal/linux/cgroup_test.go`, `internal/linux/init.go`, and
  `init_linux.go`.
  RED/GREEN:
  `GOWORK=off GOCACHE=/private/tmp/looprig-sandbox-gocache go test
  ./internal/exec ./internal/linux -run
  '^Test(ProcessTreeLinuxContainmentPlan|CgroupLifetime(KillAndWait|ReadFailureIndeterminate|RetainsOnUnprovedEmpty))$'`.
  Phase Gate 3
  requires its list output to contain
  `TestIntegrationProcessTreeParentDeath`,
  `TestIntegrationProcessTreeDoubleFork`, and
  `TestIntegrationProcessTreeSetsidEscape`, then runs those tagged tests on the
  approved Linux worker. The plan test must reject supervised Rung-2 when
  delegated cgroup v2 is unavailable, select PID namespace for Rung 1, select
  and retain a delegated cgroup otherwise, and require `cgroup.kill` plus an
  exact empty proof before release. Build on Task 10D's result-bearing
  backend-execution seam in `internal/enforce`: the owned handle must expose
  wait/termination plus a proof result, not collapse lifetime completion into a
  blocking exit integer.

  Replace the current void/best-effort cgroup teardown with a result-bearing
  `KillAndWait(context.Context) error` (exact name may follow package
  conventions). Nil means `cgroup.kill` succeeded or was unnecessary, a
  successful read proved `cgroup.procs` empty, and owned cleanup completed.
  A `cgroup.kill` error, timeout, non-empty read, read/open error, or removal
  error is a typed failed/indeterminate proof; a read failure is never treated
  as empty. On any failed/indeterminate proof, retain the cgroup descriptor,
  directory, execution lifecycle, and entire authority capsule and transfer
  them to process-level quarantine for retry. `Wait` cannot report confirmed
  teardown before this returns nil. The existing optional/best-effort resource
  cgroup behavior is not a lifetime guarantee. During the microtask, implement
  the lifetime shim plus proven Linux containment only; commit
  `feat: contain Unix process descendants`.
- **12C:** files `internal/exec/lifetime_unix.go`,
  `internal/exec/process_parent_death_integration_unix_test.go`, and
  `init_other.go`. RED/GREEN:
  `GOWORK=off GOCACHE=/private/tmp/looprig-sandbox-gocache go test
  ./internal/exec -run '^TestDarwinLifetimeCapabilityFailsClosed$'`. Phase Gate
  3 runs `TestIntegrationProcessTreeDarwinSetsidFailsClosed` on the approved
  Darwin worker and proves no child or delayed marker exists. Implement the
  pre-spawn failure with `lifetime_enforcement_unavailable`; do not add a
  success path until a separately specified concrete containment primitive
  exists. Commit
  `fix: fail closed without Darwin lifetime containment`.
- **12D:** files `internal/exec/process_tree_windows.go`,
  `internal/exec/process.go`,
  `internal/windows/elevated_execution_bridge_windows.go`,
  `internal/windows/elevated_runner_launcher_windows.go`, and
  `internal/exec/process_tree_windows_test.go`. A focused non-race Windows
  RED/GREEN command is `GOWORK=off
  GOCACHE=/private/tmp/looprig-sandbox-gocache go test ./internal/exec -run
  '^TestProcessTreeWindows(JobBeforeResume|JobEmptyOnClose)$'`. Phase Gate 3
  records discovery and full race coverage. Before changing behavior,
  characterize the stabilized restricted backend and the existing elevated
  `elevatedRunnerExecution`: both create suspended, assign before resume,
  retain authority through Job-empty proof, and quarantine delayed proof.
  Implement signal/close mapping on those owned paths and their shared
  confirmation seam only; commit
  `feat: confirm Windows process job teardown`.

**Task 12 combined acceptance**

Test interrupt, graceful terminate, force kill, idempotence, natural-exit races,
descendant and grandchild cleanup, bounded teardown failure, and no successful
return until group/job emptiness is confirmed.

The parent-death integration file starts with `//go:build integration` and
defines `TestIntegrationProcessTreeParentDeath` plus the platform escape cases.
Add deliberate Unix escape tests:

- child forks a grandchild and closes stdio;
- child calls `setsid`;
- supervising helper is force-killed;
- descendant PID disappears;
- delayed marker is never written.

On Darwin, the test deliberately requests a command that would call `setsid`,
but `PrepareProcess` must fail before any helper or target spawn with
`lifetime_enforcement_unavailable`. Process-group polling or best-effort
descendant enumeration is not sufficient evidence, and Task 13 must not list a
successful Darwin async lifecycle test.

On Windows, assert the target and helper join the kill-on-close Job before resume
and that close empties the Job.

After 12A–12D are individually committed and reviewed, defer whole-tree
race/integration verification to Phase Gate 3. Unix uses a lifetime shim plus proven containment; Darwin
fails closed when that proof is unavailable; Windows uses the reviewed Job
path. `Pdeathsig` alone is insufficient.

### Task 13: Wire Sandbox async acceptance coverage into CI

**Files:**

- Verify: `../sandbox/internal/exec/process_acceptance_test.go`
- Verify: `../sandbox/internal/exec/process_grant_integration_test.go`
- Verify: `../sandbox/internal/exec/process_parent_death_integration_unix_test.go`
- Create: `../sandbox/scripts/test-async-ci-workflow.sh`
- Modify: `../sandbox/.github/workflows/ci.yml`
- Modify: `../sandbox/Makefile`

**Step 1: Verify feature integration tests already exist**

and fail this task if the exact pipe, grant, and tree tests from Tasks 10–12 are
not listed. The success-path tests exercise real commands under:

- unconfined test profile;
- scoped filesystem grant;
- network proxy grant lifetime;
- Linux enforced backend;
- Windows restricted and elevated broker backends.

The Darwin test separately proves the Seatbelt-backed supervised request fails
before spawn because lifetime containment is unavailable. On applicable success
platforms, verify output streaming, stdin, timeout, all stop modes,
grandchildren, grant denial, executor close, and no resource leaks.

**Step 2: Verify CI RED**

Create `scripts/test-async-ci-workflow.sh`. It parses
`.github/workflows/ci.yml` and extends the existing platform jobs rather than
requiring invented replacement job keys. Require async selectors in
`test-linux-rung1`/`test-linux-rung2`, the Darwin pre-spawn fail-closed selector
in `test-macos`, and live restricted/elevated Job selectors in
`windows-restricted` and `windows-elevated`. Require the integration tag, race
mode, exact applicable `TestIntegrationProcess` selectors, and Windows runtime
invocation. Run:

```bash
cd /Users/ipotter/code/looprig/.worktrees/long-running-commands/sandbox
sh scripts/test-async-ci-workflow.sh
```

Expected: FAIL because those commands are absent from the existing jobs. The
script must not make
a network call or silently accept missing YAML.

**Step 3: Add fixtures/CI targets without weakening checks**

Wire existing tests into platform jobs. Use capability-aware skips only when an
OS feature is genuinely unavailable.
Windows runtime tests must execute on Windows CI; cross-build success is not a
substitute.
The macOS job executes the Darwin fail-before-spawn integration test only; it
does not advertise pipe-process support. Preserve all stabilization steps,
worker labels, artifacts, skip guards, and residue cleanup while appending the
new selectors.
Add a `test-async-ci` Makefile target that invokes the same guard.

**Step 4: Verify GREEN**

Re-run the exact focused workflow-guard command from Step 2. Expected: PASS and
the Windows workflow contains a live runtime job. Repository tests, builds, and
live tagged execution are deferred to Phase Gate 3.

**Step 5: Commit**

```bash
git add scripts/test-async-ci-workflow.sh .github/workflows/ci.yml Makefile
git commit -m "test: cover async sandbox processes end to end"
```

## PHASE GATE 3: Sandbox pipe execution

Run:

```bash
cd /Users/ipotter/code/looprig/.worktrees/long-running-commands/sandbox
GOWORK=off GOCACHE=/private/tmp/looprig-sandbox-gocache go test -race ./...
GOWORK=off GOCACHE=/private/tmp/looprig-sandbox-gocache go test -tags integration -list 'TestIntegration(ProcessPipe|ProcessPreparedGrant|ProcessTree)' ./internal/exec
GOWORK=off GOCACHE=/private/tmp/looprig-sandbox-gocache go test -tags integration -race ./internal/exec -run 'TestIntegration(ProcessPipe|ProcessPreparedGrant|ProcessTree)'
make secure
CGO_ENABLED=0 GOWORK=off GOCACHE=/private/tmp/looprig-sandbox-gocache go build -trimpath ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOWORK=off GOCACHE=/private/tmp/looprig-sandbox-gocache go build -trimpath ./...
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 GOWORK=off GOCACHE=/private/tmp/looprig-sandbox-gocache go build -trimpath ./...
```

No new fuzz target is owned by this phase. The approved Linux, Darwin, and
Windows jobs must execute their applicable listed integration tests; cross-build
success is not runtime evidence. Darwin's applicable evidence is the
pre-spawn `lifetime_enforcement_unavailable` contract, not successful async
execution. `make secure` batches Sandbox format checks, Vet, Staticcheck, Gosec,
and Govulncheck.

Phase reviewers must trace every per-spawn cleanup resource through terminal
exit and any quarantined zero-proof retry, verify context separation, inspect
real parent-death evidence, characterize both Windows Job paths, confirm
Landlock/Windows releases remain retained through proof, and confirm that
Sandbox still imports neither Harness nor Tools.

## Phase 4: Model-facing Bash and process tools

### Task 14: Extend Bash preparation without breaking legacy calls

**Files:**

- Modify: `bash/bash.go`
- Modify: `bash/prepare.go`
- Modify: `bash/bash_test.go`
- Modify: `bash/preparecall_test.go`
- Modify: `bash/runner_injection_test.go`
- Modify: `bash/bash_grants_test.go`
- Create: `bash/supervision_args_test.go`

**Step 1: Lock legacy behavior with failing characterization additions**

Assert calls containing only existing fields still use the existing synchronous
path and produce identical plain text, exit markers, timeout, truncation, grant,
permit, observation, and runner behavior.

Add new failing schema/preparation cases for presence-aware:

```go
Timeout        *int
Background     bool
YieldTimeMS    *int
TTY            bool
MaxOutputBytes *int64
```

Validate ranges, `timeout:0` rules, explicit-supervision detection, and
conservative detached syntax only for supervised calls. The prepared artifact
must retain all normalized settings; mutating raw JSON later changes nothing.

**Step 2: Verify RED**

```bash
GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go test ./bash -run 'TestBash(Schema|Supervision|Legacy|Prepared)'
```

Expected: new field tests fail; legacy tests pass.

**Step 3: Implement parsing only**

Keep exported `Factory` and `NewFactory` unchanged. Add a separate binding-aware
factory used by root definitions. Do not route to the supervisor yet.

**Step 4: Verify GREEN**

Re-run the exact focused command from Step 2. Bash race coverage is deferred to
Phase Gate 4.

**Step 5: Commit**

```bash
git add bash
git commit -m "feat(bash): prepare supervised command options"
```

### Task 15: Route supervised Bash through the shared supervisor

**Files:**

- Create: `bash/supervised.go`
- Create: `bash/supervised_test.go`
- Create: `bash/result.go`
- Create: `bash/result_test.go`
- Modify: `bash/bash.go`
- Modify: `bash/prepare.go`

**Step 1: Write failing workflow tests**

Test:

- explicit background returns only after durable registration;
- yielded command returns a terminal JSON result when it exits within budget;
- yielded live command returns handle, cursor, output, and `backgrounded:true`;
- hard lifetime timeout;
- supervised `timeout:0`;
- PTY and max-output flags reach the runner;
- `PrepareProcess` runs before lease acquisition and does not spawn;
- prepared authoritative access determines the lifetime lease;
- `Start` occurs only after the lease is held and consumes preparation once;
- prepare/lease/start failure closes the preparation and releases every
  reservation/lease;
- missing async runner/access summary returns
  `lifetime_enforcement_unavailable`;
- spawn and completion invalidate the bound loop observations; optional runner
  activity triggers intermediate invalidation;
- invocation cancellation after returned handle does not kill;
- no JSON contains host path or OS PID.

**Step 2: Verify RED**

```bash
GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go test ./bash -run 'TestSupervisedBash'
```

Expected: FAIL because supervisor routing is absent.

**Step 3: Implement minimal routing**

Legacy calls execute the unchanged function. Supervised calls obtain the
session supervisor, owner, origin, prepared grants, runner, effective access,
observation capability, and settings from bindings/context. They prepare through
the async runner, acquire the exact lifetime lease, then hand the prepared
process to `Supervisor.Start`.

**Step 4: Verify GREEN**

Re-run the exact focused command from Step 2. Expected: PASS.

**Step 5: Commit**

```bash
git add bash
git commit -m "feat(bash): run commands under session supervision"
```

### Task 16: Implement ProcessOutput

**Files:**

- Create: `process/output_tool.go`
- Create: `process/output_tool_test.go`

**Step 1: Write failing tool tests**

Validate single versus multi-ID exclusivity, duplicates, empty lists, cursor,
limit, wait mode, wait timeout, and `safe_text|base64` encoding. Test poll,
wait-any, wait-all, input-order preservation, gap, cursor-ahead, terminal
metadata, opaque artifact, owner isolation, and metadata-safe errors.

**Step 2: Verify RED**

```bash
GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go test ./process -run 'TestProcessOutput'
```

Expected: FAIL.

**Step 3: Implement the tool**

Implement `Info`, `PrepareCall` with an empty effect request and sealed prepared
artifact, `InvokableRun`, and stable JSON rendering. Never return a spool path.

**Step 4: Verify GREEN**

Run the focused non-race test once. Stress repetition is deferred to Phase Gate
4's full race suite.

**Step 5: Commit**

```bash
git add process/output_tool.go process/output_tool_test.go
git commit -m "feat(process): expose incremental output"
```

### Task 17: Implement ProcessInput

**Files:**

- Create: `process/input_tool.go`
- Create: `process/input_tool_test.go`

**Step 1: Write failing tests**

Test data writes, empty-operation rejection, bounded backpressure, idempotent
pipe EOF, PTY EOF forwarding, resize validation, resize rejection for pipes,
optional cursor, omitted-cursor snapshot at pre-write end, optional yield,
closed input, terminal process, and cross-owner `not_found`.

**Step 2: Verify RED**

```bash
GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go test ./process -run 'TestProcessInput'
```

Expected: FAIL.

**Step 3: Implement serialized bounded input**

Serialize input per entry. Bound queued bytes and write duration. Apply resize
only through PTY-capable process. Return a cursor-aware snapshot after optional
yield.

**Step 4: Verify GREEN and commit**

```bash
git add process/input_tool.go process/input_tool_test.go
git commit -m "feat(process): support supervised process input"
```

Before committing, re-run the exact focused command from Step 2.

### Task 18: Implement ProcessStop

**Files:**

- Create: `process/stop_tool.go`
- Create: `process/stop_tool_test.go`

**Step 1: Write failing tests**

Test interrupt, terminate with grace then escalation, immediate kill, invalid
mode/grace, terminal idempotence, natural-exit race, timeout race, confirmed
tree exit, teardown failure, lifecycle event request, and owner isolation.

**Step 2: Verify RED**

```bash
GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go test ./process -run 'TestProcessStop'
```

Expected: FAIL.

**Step 3: Implement stop orchestration**

Interrupt does not choose a terminal state unless wait reports exit. Terminate
escalates once. Kill and terminal stops are idempotent. Do not report success
before runner confirmation.

**Step 4: Verify GREEN and commit**

```bash
git add process/stop_tool.go process/stop_tool_test.go
git commit -m "feat(process): stop supervised process trees"
```

Before committing, re-run the exact focused command from Step 2.

### Task 19: Export four independently selectable definitions

**Files:**

- Modify: `definitions.go`
- Modify: `definitions_test.go`
- Modify: `dependency_test.go`
- Create: `process/definitions_test.go`

**Step 1: Write failing definition tests**

Require:

- `Bash(...)`;
- `ProcessOutputDefinition()`;
- `ProcessInputDefinition()`;
- `ProcessStopDefinition()`.

Each definition produces one tool with the exact name. All require workspace
and process services as appropriate. Separately built definitions in one
session obtain the same supervisor registry entry. Different sessions obtain
different supervisors. Options resolve once and concurrent builds remain safe.

**Step 2: Verify RED**

```bash
GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go test . ./process -run 'Test.*Definition'
```

Expected: FAIL because companion definitions are absent.

**Step 3: Implement root facade and boundary rule**

Permit Bash to import shared public `process`, analogous to `permission`.
Continue forbidding Sandbox and Harness internal imports. Use the keyed registry,
never a package global.

**Step 4: Verify GREEN and commit**

```bash
git add definitions.go definitions_test.go dependency_test.go process/definitions_test.go
git commit -m "feat: export supervised process tools"
```

Before committing, re-run the exact focused command from Step 2.

### Task 20: Add Tools workflow integration tests

**Files:**

- Create: `process/integration_test.go`
- Create: `bash/integration_test.go`

**Step 1: Write tagged integration acceptance tests**

Compose public Harness binding contracts with a contract-faithful Tools test
registry and deterministic async runner fixture. Tools cannot import Harness
`internal/sessionruntime`; real registry composition is reserved for Coderig
Task 28. Cover foreground compatibility, background start, yield,
incremental output, wait-many, input, stop, output limit, owner isolation,
resource shutdown, and manifest restore.

Both files begin with:

```go
//go:build integration
```

They define exact tests `TestIntegrationBashSupervisedWorkflow` and
`TestIntegrationProcessToolsRestore`.

**Step 2: Verify selection and acceptance**

Defer tagged test discovery and execution to Phase Gate 4, which must list both
exact names before running them. During the task, use this one focused
default-tag seam command:

```bash
GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go test ./bash ./process -run 'Test(SupervisedBash|Process(Output|Input|Stop))'
```

An actual gate failure starts a focused RED/GREEN fix in the owning
task/repository before the gate is rerun.

**Step 3: Complete only integration seams**

Do not import Sandbox into Tools. Keep the integration runner in `_test.go`.

**Step 4: Verify GREEN and commit**

```bash
git commit -m "test: exercise supervised tool workflows"
```

## PHASE GATE 4: Model-facing tools

Run:

```bash
cd /Users/ipotter/code/looprig/.worktrees/long-running-commands/tools
GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go test -race ./...
GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go test -tags integration -list '^Test(SupervisorIntegration|Integration(BashSupervisedWorkflow|ProcessToolsRestore))' ./bash ./process
GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go test -tags integration -race ./bash ./process -run '^Test(SupervisorIntegration|Integration(BashSupervisedWorkflow|ProcessToolsRestore))'
GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go test ./internal/safetext -run '^$' -fuzz '^FuzzNormalize$' -fuzztime=10s
make secure
CGO_ENABLED=0 GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go build -trimpath ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go build -trimpath ./...
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go build -trimpath ./...
```

This gate reruns the accumulated safe-text fuzz target only once, after the
model-facing changes. `make secure` batches Tools format checks, Vet,
Staticcheck, Gosec, and Govulncheck.

Phase reviewers must compare exact JSON schemas/results with the design, rerun
legacy characterization tests, verify owner checks and binding sharing, and
confirm output-controlled bytes never enter trusted notifications.

## Phase 5: Unix PTY and Windows ConPTY

### Task 21: Add Unix PTY execution

**Files:**

- Modify: `../sandbox/go.mod`
- Modify: `../sandbox/go.sum`
- Create: `../sandbox/internal/exec/terminal.go`
- Create: `../sandbox/internal/exec/terminal_unix.go`
- Create: `../sandbox/internal/exec/terminal_other.go`
- Create: `../sandbox/internal/exec/process_pty_unix_test.go`
- Create: `../sandbox/internal/exec/process_pty_integration_unix_test.go`
- Modify: `../sandbox/internal/exec/process.go`
- Modify: `../sandbox/internal/exec/process_tree_unix.go`

`github.com/creack/pty` is the approved Unix PTY dependency. Pin
`github.com/creack/pty v1.1.24` and do not add any other direct dependency.

**Step 1: Write failing PTY tests**

Test real interactive echo, combined output, `stty size`, resize, input, EOF,
Ctrl-D, interrupt to the terminal foreground group, PTY EIO normalization,
allocation failure, and no pipe fallback.

Add a test proving PTY `Setsid`/`Setctty` setup does not conflict with the
existing `Setpgid` process-tree configuration.
`process_pty_integration_unix_test.go` begins with `//go:build integration` and
defines the exact live Linux test `TestIntegrationProcessPTYLifecycle`. Add
`TestIntegrationProcessPTYDarwinLifetimeUnavailable` to prove Darwin rejects
before PTY allocation or child spawn. Do not treat the `_unix.go` build
selection as evidence that Darwin can provide supervised process-tree
containment.

**Step 2: Verify RED**

```bash
cd /Users/ipotter/code/looprig/.worktrees/long-running-commands/sandbox
GOWORK=off GOCACHE=/private/tmp/looprig-sandbox-gocache go test ./internal/exec -run 'TestProcessPTY'
```

Expected: FAIL with `pty_unavailable` or missing implementation.

**Step 3: Add the dependency and implement**

Run:

```bash
GOWORK=off GOCACHE=/private/tmp/looprig-sandbox-gocache go get github.com/creack/pty@v1.1.24
```

Use `pty.Open`, attach the slave before the existing configure/start
linearization, and retain both terminal endpoints through process cleanup. Do
not use `pty.Start`, which would bypass the enforcement ownership point.

**Step 4: Verify GREEN**

Re-run the exact focused command from Step 2. Tagged integration is deferred to
Phase Gate 5.

**Step 5: Commit**

```bash
git add go.mod go.sum internal/exec
git commit -m "feat: support supervised Unix PTYs"
```

### Task 22: Add ConPTY to the existing Windows backend

**Files:**

- Create: `../sandbox/internal/exec/conpty_launch_plan.go`
- Create: `../sandbox/internal/exec/conpty_launch_plan_test.go`
- Create: `../sandbox/internal/exec/terminal_windows.go`
- Create: `../sandbox/internal/exec/process_conpty_windows_test.go`
- Create: `../sandbox/internal/exec/process_conpty_integration_windows_test.go`
- Modify: `../sandbox/internal/exec/process.go`
- Modify: `../sandbox/internal/exec/process_tree_windows.go`
- Modify: `../sandbox/internal/enforce/shell_windows.go`
- Modify: `../sandbox/internal/windows/runner_windows.go`
- Modify as needed: `../sandbox/internal/windows/broker_adapters_windows.go`
- Modify: `../sandbox/.github/workflows/ci.yml`

Build on the merged Windows restricted/elevated broker and Job Object path.
ConPTY must not create an unconfined side path.

Execute as three reviewed microtasks:

- **22A — platform-independent ConPTY launch plan:** extract an immutable launch
  plan describing pipes, pseudo-console attribute, suspended creation, Job
  assignment, broker token/desktop, and resume order. RED test:
  `TestConPTYLaunchPlanOrdersJobBeforeResume`. This fails locally before any
  Windows production code.
- **22B — Windows pseudo-console process:** implement create/input/output/resize
  and exact cleanup. RED must be observed on a live Windows worker for
  `TestProcessConPTYInteractive`.
- **22C — restricted/elevated broker and CI:** prove the same path preserves
  restricted/elevated security and Job ownership with
  `TestIntegrationConPTYRestricted` and
  `TestIntegrationConPTYElevated`.

Execute independently:

- **22A:** create `internal/exec/conpty_launch_plan.go` and
  `internal/exec/conpty_launch_plan_test.go` with no Windows build tag. RED:
  `GOWORK=off GOCACHE=/private/tmp/looprig-sandbox-gocache go test
  ./internal/exec -run '^TestConPTYLaunchPlanOrdersJobBeforeResume$'`.
  Expected failure is the absent plan/ordering. Implement the immutable
  platform-neutral plan only, re-run the exact command, and commit
  `feat: specify ConPTY launch ordering`.
- **22B:** create `internal/exec/terminal_windows.go` and
  `internal/exec/process_conpty_windows_test.go`, modifying the listed Windows
  implementation files. A live Windows RED/GREEN run executes
  `go test ./internal/exec -run
  '^TestProcessConPTYInteractive$'`. Implement pseudo-console
  create/input/output/resize/cleanup only and commit
  `feat: run supervised ConPTY processes`.
- **22C:** create
  `internal/exec/process_conpty_integration_windows_test.go` with
  `//go:build integration`, modify the broker adapters and CI workflow, and
  use `go test ./internal/exec -run '^TestConPTYBrokerJobPlan$'` as the single
  focused default-tag RED/GREEN command. Tagged discovery and execution
  of the restricted and elevated cases are deferred to Phase Gate 5. Implement
  broker/Job preservation and CI wiring only; commit
  `test: prove confined ConPTY execution`.

**Task 22 combined acceptance**

On Windows test:

- `CreatePseudoConsole` setup and typed unavailable behavior;
- interactive echo and combined output;
- resize;
- input and terminal EOF;
- interrupt;
- target/broker process belongs to the owned Job before resume;
- Job empties on stop, close, timeout, and session shutdown;
- restricted and elevated profiles preserve filesystem/network enforcement;
- no pipe fallback.

Add compile-only non-Windows tests for facade availability and typed errors.

After 22A–22C are individually committed and reviewed, defer their combined
launch-order, pseudo-console, broker, Job, and enforcement verification to
Phase Gate 5.

The Windows cross-build and test-binary compilation are deferred to Phase Gate
5.

### Task 23: Verify Tools PTY semantics end to end

**Files:**

- Modify: `bash/supervised_test.go`
- Modify: `process/input_tool_test.go`
- Create: `process/pty_integration_test.go`

**Step 1: Write PTY acceptance tests**

Cover `tty:true` forwarding, `pty_unavailable`, no fallback, combined cursor
stream, resize, EOF, input yield, and interrupt-not-terminal-until-exit.
`process/pty_integration_test.go` begins with `//go:build integration`.
It defines the exact live test `TestIntegrationProcessPTYToolWorkflow`.

**Step 2: Verify selection and acceptance**

Defer tagged discovery and execution to Phase Gate 5, which must list the exact
test before running it. During the task, use this focused default-tag RED/GREEN
command:

```bash
GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go test ./bash ./process -run 'Test.*(TTY|PTY|Resize|EOF|Interrupt)'
```

Any gate failure starts a focused RED/GREEN fix in the owning task/repository.

**Step 3: Implement only missing contract handling**

Do not import the PTY library into Tools.

**Step 4: Verify GREEN and commit**

```bash
git commit -m "test: verify interactive process tools"
```

## PHASE GATE 5: Interactive terminals

Run:

```bash
cd /Users/ipotter/code/looprig/.worktrees/long-running-commands/sandbox
GOWORK=off GOCACHE=/private/tmp/looprig-sandbox-gocache go test -race ./...
GOWORK=off GOCACHE=/private/tmp/looprig-sandbox-gocache go test -tags integration -list 'TestIntegration(ProcessPipe|ProcessPreparedGrant|ProcessTree|ProcessPTY|ConPTY)' ./internal/exec
GOWORK=off GOCACHE=/private/tmp/looprig-sandbox-gocache go test -tags integration -race ./internal/exec -run 'TestIntegration(ProcessPipe|ProcessPreparedGrant|ProcessTree|ProcessPTY|ConPTY)'
make secure
CGO_ENABLED=0 GOWORK=off GOCACHE=/private/tmp/looprig-sandbox-gocache go build -trimpath ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOWORK=off GOCACHE=/private/tmp/looprig-sandbox-gocache go build -trimpath ./...
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 GOWORK=off GOCACHE=/private/tmp/looprig-sandbox-gocache go build -trimpath ./...
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 GOWORK=off GOCACHE=/private/tmp/looprig-sandbox-gocache go test -c -o /private/tmp/sandbox-conpty.test.exe ./internal/exec
cd /Users/ipotter/code/looprig/.worktrees/long-running-commands/tools
GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go test -race ./...
GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go test -tags integration -list '^Test(SupervisorIntegration|Integration(BashSupervisedWorkflow|ProcessToolsRestore|ProcessPTYToolWorkflow))' ./bash ./process
GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go test -tags integration -race ./bash ./process -run '^Test(SupervisorIntegration|Integration(BashSupervisedWorkflow|ProcessToolsRestore|ProcessPTYToolWorkflow))'
GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go test ./internal/safetext -run '^$' -fuzz '^FuzzNormalize$' -fuzztime=10s
make secure
CGO_ENABLED=0 GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go build -trimpath ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go build -trimpath ./...
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go build -trimpath ./...
```

The approved Darwin worker must record
`TestIntegrationProcessPTYDarwinLifetimeUnavailable`; the existing
`windows-restricted` and `windows-elevated` jobs must record the list output
plus passing `TestProcessConPTYInteractive`,
`TestIntegrationConPTYRestricted`, and
`TestIntegrationConPTYElevated`. Cross-build output is not runtime evidence.

Phase reviewers must approve Unix session/group composition, terminal foreground
interrupt routing, ConPTY broker/Job integration, EOF semantics, and the absence
of silent fallback.

## Phase 6: Harness lifecycle and Coderig composition

### Task 24: Publish lifecycle events and deliver metadata-only notifications

**Files:**

- Create: `../harness/pkg/journal/idempotency.go`
- Create: `../harness/pkg/journal/idempotency_test.go`
- Modify: `../harness/pkg/journal/record.go`
- Modify: `../harness/pkg/journal/record_json_test.go`
- Modify: `../harness/pkg/journal/appender.go`
- Modify: `../harness/pkg/journal/appender_test.go`
- Modify: `../harness/pkg/hub/deps.go`
- Modify: `../harness/pkg/hub/hub.go`
- Modify: `../harness/pkg/hub/durability_test.go`
- Modify: `../harness/pkg/sessionstore/journal.go`
- Modify: `../harness/pkg/sessionstore/journal_test.go`
- Modify: `../harness/pkg/sessionstore/replay.go`
- Modify: `../harness/pkg/sessionstore/replay_test.go`
- Create: `../harness/internal/sessionruntime/process_lifecycle.go`
- Create: `../harness/internal/sessionruntime/process_lifecycle_test.go`
- Create: `../harness/pkg/command/process_notification.go`
- Create: `../harness/pkg/command/process_notification_test.go`
- Modify: `../harness/pkg/command/command.go`
- Modify: `../harness/pkg/command/validate.go`
- Modify: `../harness/pkg/command/marshal.go`
- Modify: `../harness/pkg/command/marshal_test.go`
- Modify: `../harness/pkg/command/validate_test.go`
- Modify: `../harness/internal/loopruntime/loop.go`
- Modify: `../harness/internal/loopruntime/config.go`
- Modify: `../harness/internal/loopruntime/restored.go`
- Modify: `../harness/internal/loopruntime/restored_test.go`
- Modify: `../harness/internal/sessionruntime/command_journal.go`
- Modify: `../harness/internal/sessionruntime/restore.go`
- Modify: `../harness/internal/sessionruntime/restore_constructor.go`
- Create: `../harness/internal/sessionruntime/process_notification_test.go`

Execute as three separate reviewed microtasks.

**24A — backend-neutral durable journal idempotency**

Add `AppendResult{Sequence, Appended}` and an optional idempotent journal seam
without weakening existing append callers. Tests:

- `TestSessionJournalConcurrentIdenticalIDAppendsOnce`;
- `TestSessionJournalReopenDeduplicatesEventAndCommand`;
- `TestSessionJournalIdempotencyCollisionFails`;
- `TestSessionJournalOffloadedRecordHydratesIdempotencyIndex`;
- `TestJournalAppenderDoesNotRepublishDuplicate`.

The sessionstore integration test releases the first writer lease, constructs a
fresh Store facade over the same storage test backend, uses both inline and
blob-offloaded records, and proves an identical retry
returns the original sequence with `Appended=false`; the same ID with a
different persisted kind/payload returns a typed collision. The fingerprint
does not include the transient `CommandRecord` route. Hydrate the index from the
full durable ledger before the opening fence and update it under the append
lock, but always append the ownership fence through the raw path so a repeated
lease epoch still advances fencing. Preserve envelope IDs through replay,
verify outer blob-pointer ID equals resolved inner ID, and reject mismatches.
RED:

```bash
cd /Users/ipotter/code/looprig/.worktrees/long-running-commands/harness
GOWORK=off GOCACHE=/private/tmp/looprig-harness-gocache GOFLAGS=-mod=vendor go test ./pkg/journal ./pkg/sessionstore -run 'Test(SessionJournal(ConcurrentIdenticalIDAppendsOnce|ReopenDeduplicatesEventAndCommand|IdempotencyCollisionFails|OffloadedRecordHydratesIdempotencyIndex)|JournalAppenderDoesNotRepublishDuplicate)'
```

Expected: duplicate IDs append multiple frames. Implement only journal result,
fingerprinting/collision, ID-preserving index hydration, raw opening fences, and
appender duplicate reporting.
Re-run the focused command, and commit
`feat(journal): deduplicate durable record retries`. Task 28 repeats the reopen
proof through Coderig's real fsstore composition.

**24B — checked process lifecycle publication**

Files:
`internal/sessionruntime/process_lifecycle.go`,
`internal/sessionruntime/process_lifecycle_test.go`, and the narrowly required
journal/Hub appender wiring in `pkg/hub/deps.go`, `pkg/hub/hub.go`, and
`pkg/hub/durability_test.go`. Tests cover started/backgrounded/completed/stop/lost,
append failure faulting, coordinates, owner validation, pre-persisted non-zero
EventIDs, and no Hub apply/broadcast when 24A returns `Appended=false`. Preserve
the old appender surface through an optional result-bearing extension; the nop
appender reports `Appended=true`. RED/GREEN:

```bash
GOWORK=off GOCACHE=/private/tmp/looprig-harness-gocache GOFLAGS=-mod=vendor go test ./internal/sessionruntime -run '^TestProcessLifecycle'
```

Attach the checked implementation behind Task 4's stable late-bound lifecycle
bridge; do not replace the bridge already captured by a resource. Validate the
Tools-supplied coordinates, lifecycle matrix, and persisted
ProcessCreatedAt/ProcessStartedAt/ProcessFinishedAt values, preserve them
byte-for-byte in the event payload, add only Harness envelope metadata not
present in the DTO, and never replace the Tools EventID. Durably append and
publish live only on a new append.
Run the focused non-race commit checks and commit
`feat(session): publish process lifecycle metadata`.

**24C — metadata-only notification and restored live dedupe**

Files: `pkg/command/process_notification.go`,
`pkg/command/process_notification_test.go`, `pkg/command/command.go`,
`pkg/command/validate.go`, `pkg/command/marshal.go`,
`pkg/command/marshal_test.go`, `pkg/command/validate_test.go`,
`internal/loopruntime/loop.go`, `internal/loopruntime/config.go`,
`internal/loopruntime/restored.go`, `internal/loopruntime/restored_test.go`,
`internal/sessionruntime/command_journal.go`,
`internal/sessionruntime/restore.go`,
`internal/sessionruntime/restore_constructor.go`, and
`internal/sessionruntime/process_notification_test.go`. Tests:

- `TestProcessNotificationRejectsOutputAndHostData`;
- `TestProcessNotificationStableCommandID`;
- `TestProcessNotificationLiveDuplicateIgnored`;
- `TestProcessNotificationCollisionRejected`;
- `TestProcessNotificationAppendBeforeDispatchRestoresDelivery`;
- `TestProcessNotificationDispatchBeforeCrashDoesNotRepeatModelTurn`.
- `TestProcessNotificationRestoreFailsWhenUnresolvedSetExceedsCap`;
- `TestProcessNotificationAppendThenQueueFullRetryKeepsReservation`;
- `TestProcessNotificationConcurrentFailedAndSuccessfulAppendKeepsPending`;
- `TestForeignLoopRejectsProcessNotification`.

The native loop owns a bounded set of unresolved
CommandID/payload-fingerprint entries. Restore reconstructs it by subtracting
enduring loop events whose cause references the CommandID from the full process
notification command replay, then re-enqueues an appended-but-undispatched
notification. It fails closed if the unresolved set exceeds the configured cap;
it never evicts an unresolved ID. `Appended=false` with no unresolved entry is
already consumed and cannot begin another model turn. Before append, the loop
atomically reserves the unresolved ID/fingerprint and bounded pending capacity.
No slot returns retryable-full before append; append failure releases the
reservation; append success commits it. If the ordinary inbox is full after
append, the reservation stays pending and a same-ID retry reuses it.
Singleflight by `(LoopID, CommandID)` gives each reservation a generation token:
identical concurrent attempts share the leader result, only the current
uncommitted generation can be released, and commit is idempotent. A failed
attempt cannot erase a later successful pending obligation. The
notification payload carries session/loop coordinates; append validates them
against the enclosing live `CommandRecord` route. Add a transient result channel
reporting accepted, duplicate, collision, retryable-full, or stopped. Command
append failure remains explicit for this process-notification path; existing
audit-only commands keep their established behavior. RED/GREEN:

```bash
GOWORK=off GOCACHE=/private/tmp/looprig-harness-gocache GOFLAGS=-mod=vendor go test ./pkg/command ./internal/loopruntime ./internal/sessionruntime -run 'Test(ProcessNotification|ForeignLoopRejectsProcessNotification)'
```

Implement only the sealed command codec, restored projection, and owning-loop
delivery. Queue backpressure cannot grow the inbox beyond its bound or block
terminalization: the retained pending reservation returns retryable-full and
the supervisor retries with the same CommandID. Remove an unresolved entry only
after an enduring loop causality event commits. Attach this implementation
behind Task 4's stable completion-notifier bridge rather than rebinding captured
resources. Foreign engines reject process notifications; they do not silently
drop them.
Verify with the focused non-race command, and commit
`feat(session): deliver idempotent process notifications`.

**Phase acceptance behavior**

Test checked publication for started/backgrounded/completed/stop/lost, append
failure faulting, session/loop/owner validation, and at-most-once acknowledgment.
The bridge receives stable IDs from Tools, installs them directly as
EventID/CommandID, and rejects zero or replacement IDs.

Test a dedicated completion command containing only process handle, terminal
state, enum reason, and target coordinates. It must not accept command, output,
stdin, path, PID, or arbitrary text. It must not reuse SubagentResult causality.

For lifecycle requests, validate and preserve Tools-supplied stable IDs, target
coordinates, and process lifecycle timestamps. For completion notifications,
validate and preserve the stable CommandID and target coordinates. Harness may
stamp only envelope metadata absent from the applicable neutral DTO; it must
not rewrite DTO fields or mint replacement IDs. The durable journal index, not
the ID field alone, deduplicates crash retries. Deliver notifications through
the owning loop with the stable CommandID and explicit delivery errors.

Combined journal/lifecycle/notification race verification is deferred to Phase
Gate 6.

### Task 25: Integrate shutdown, construction abort, restore, and workspace rewind

**Files:**

- Modify: `../harness/internal/sessionruntime/session.go`
- Modify: `../harness/internal/sessionruntime/shutdown_cleanup.go`
- Modify: `../harness/internal/sessionruntime/workspace_restore.go`
- Modify: `../harness/internal/sessionruntime/restore_constructor.go`
- Modify: `../harness/internal/sessionruntime/lifecycle_test.go`
- Modify: `../harness/internal/sessionruntime/restore_roundtrip_test.go`
- Modify: `../harness/internal/sessionruntime/workspace_restore_helpers_test.go`
- Modify: `../harness/internal/sessionruntime/construction_abort_test.go`

**Step 1: Write failing ordering tests**

Test:

- shutdown latches closing;
- process admission closes while hub publication still works;
- resources terminate/confirm/flush before loops, hub, leases, and session
  context are released;
- concurrent shutdown callers share a result;
- construction abort closes resources before root/session lease cleanup;
- workspace restore suspends admission, stops processes, releases lifetime
  leases, checkpoints/restores, and resumes on all paths;
- restore activates the bridge before `RestoreDone`;
- running manifests become lost without signalling persisted OS metadata;
- completion/lost durable marker follows checked publication;
- duplicate restore does not notify twice.

**Step 2: Verify RED**

```bash
GOWORK=off GOCACHE=/private/tmp/looprig-harness-gocache GOFLAGS=-mod=vendor go test ./internal/sessionruntime -run 'Test.*(ProcessShutdown|ProcessRestore|WorkspaceRestore.*Process|ConstructionAbort.*Resource)'
```

Expected: FAIL.

**Step 3: Implement lifecycle ordering**

Add a `session_resources` cleanup phase with bounded timeout reporting. Preserve
existing checkpoint/hustle cleanup semantics.

**Step 4: Verify GREEN**

Re-run the exact focused command from Step 2. Broader and tagged verification
is deferred to Phase Gate 6.

**Step 5: Commit**

```bash
git add internal/sessionruntime
git commit -m "feat(session): manage supervised process lifecycle"
```

### Task 26: Adapt Sandbox processes to Harness in Coderig

**Files:**

- Modify: `../coderig/internal/app/toolsets.go`
- Modify: `../coderig/internal/app/persistence.go`
- Modify: `../coderig/internal/app/persistence_test.go`
- Modify: `../coderig/internal/app/rig_restore_integration_test.go`
- Create: `../coderig/internal/app/process_adapter.go`
- Create: `../coderig/internal/app/process_adapter_test.go`

Execute as two reviewed microtasks:

- **26A — resource storage composition:** persisted sessions map to
  `<data-dir>/resources/<session-id>` with a stable provider identity threaded
  through the Rig option; headless sessions receive an isolated process-owned
  temporary base whose SessionID subdirectory is stable for same-process
  reconstruction and is discarded only when Coderig exits.
  RED tests are `TestProcessResourceRootOutsideWorkspace`,
  `TestProcessResourceRootStableAcrossRestore`,
  `TestProcessResourceRootIdentityMismatchFailsRestore`, and
  `TestHeadlessProcessResourceRootsAreIsolated`, plus
  `TestHeadlessProcessResourceRootStableForSameProcessRestore`. Its single
  RED/GREEN command is `GOWORK=off
  GOCACHE=/private/tmp/looprig-coderig-gocache GOFLAGS=-mod=readonly go test
  ./internal/app -run 'Test(ProcessResourceRoot|HeadlessProcessResourceRoot)'`.
- **26B — async adapter:** implement only the two-phase Sandbox-to-Harness type
  mapping below.

**Step 1: Write failing adapter contract tests**

Test:

- Harness request maps every field exactly once to Sandbox options;
- grants and origin execution ID are preserved;
- Sandbox prepared-process access maps exactly to Harness access without
  Coderig parsing opaque grants;
- Harness lease acquisition occurs between adapter prepare and start;
- prepared close/start single-use semantics map exactly;
- Sandbox process streams/wait/resize/signal/close map exactly;
- optional Sandbox activity values map exactly to Harness values, invalid
  activity broadens invalidation, and channel closure precedes wait;
- Sandbox error codes map to Harness codes without losing causes;
- no OS PID crosses the adapter;
- adapter satisfies `tool.AsyncProcessRunner`.

**Step 2: Verify RED**

```bash
cd /Users/ipotter/code/looprig/.worktrees/long-running-commands/coderig
GOWORK=off GOCACHE=/private/tmp/looprig-coderig-gocache GOFLAGS=-mod=readonly go test ./internal/app -run 'TestProcessAdapter'
```

Expected: FAIL.

**Step 3: Implement the mechanical adapter**

Follow the existing `grantedExecutor` composition pattern. Put no buffering,
authorization, event, or supervisor policy in Coderig.

**Step 4: Verify GREEN and commit**

```bash
git add internal/app/process_adapter.go internal/app/process_adapter_test.go internal/app/toolsets.go
git commit -m "feat: adapt sandbox async processes"
```

Before committing, re-run the exact focused command from Step 2.

### Task 27: Install the four process definitions in Coderig

**Files:**

- Modify: `../coderig/internal/app/toolsets.go`
- Modify: `../coderig/internal/app/access_acceptance_test.go`
- Create: `../coderig/internal/app/process_tools_test.go`

**Step 1: Write failing roster/binding tests**

Test operator and reviewer rosters contain Bash, ProcessOutput, ProcessInput, and
ProcessStop exactly once. Bind the same per-loop Sandbox executor used by the
gate. Verify all definitions share one session supervisor and sibling loops
cannot access one another's handles. Verify Coderig does not install the
process-enabled roster on a foreign-engine loop and reports
`process_notifications_unsupported` for an attempted explicit bind.

**Step 2: Verify RED**

```bash
GOWORK=off GOCACHE=/private/tmp/looprig-coderig-gocache GOFLAGS=-mod=readonly go test ./internal/app -run 'TestProcessTools|Test.*ToolDefinitions'
```

Expected: FAIL.

**Step 3: Wire definitions**

Construct the Bash definition with sync and async adapters. Append companion
definitions to both allowed rosters. Preserve reviewer access restrictions.

**Step 4: Verify GREEN and commit**

```bash
git add internal/app/toolsets.go internal/app/access_acceptance_test.go internal/app/process_tools_test.go
git commit -m "feat: install supervised process tools"
```

Before committing, re-run the exact focused command from Step 2.

### Task 28: Add full Coderig integration tests

**Files:**

- Create: `../coderig/internal/app/process_integration_test.go`
- Create: `../coderig/internal/app/process_restore_integration_test.go`
- Create: `../coderig/.github/workflows/ci.yml`

**Step 1: Write tagged end-to-end acceptance tests**

Both new files begin with `//go:build integration`.

Through real Coderig composition and Sandbox:

1. run an unchanged foreground Bash;
2. start a background command;
3. yield a foreground command;
4. poll incremental output;
5. wait on multiple processes;
6. send stdin and EOF;
7. resize/use PTY where supported;
8. interrupt, terminate, and kill;
9. hit output limit;
10. verify grant denial and effective workspace lease conflicts;
11. observe metadata-only completion;
12. restore completed output;
13. mark live manifests lost without PID signalling;
14. close and reopen the real fsstore-backed SessionStore, retry the persisted
    lifecycle EventID and notification CommandID, and prove one durable frame
    and no duplicate model turn in
    `TestIntegrationProcessJournalIdempotencyReopen`;
15. shut down with no descendants.

Add Windows CI variants for restricted and elevated broker profiles.

The fixture is explicit:

- start from existing `openAcceptanceAgentWithClient`;
- inject a scripted `inference.Client` that emits exact tool calls and no network
  model traffic;
- use fsstore under `t.TempDir()` for session/resource durability;
- use a separate `t.TempDir()` workspace;
- use the real Harness Rig/session registry and real Tools definitions;
- use Sandbox's unconfined profile only for portable local pipe tests;
- put enforced Linux/Windows tests behind `//go:build integration` and runtime
  capability probes that skip only when the documented backend is unavailable;
- on Darwin, require the typed pre-spawn
  `lifetime_enforcement_unavailable` integration path and prove that neither a
  target nor delayed marker was created; do not claim a supervised success path;
- require restricted and elevated Windows cases on separate live CI jobs, where
  a capability skip is a job failure.

**Step 2: Verify fixture selection and run acceptance**

Defer tagged discovery and execution to Phase Gate 6. Its list output must
include workflow, restore, and
`TestIntegrationProcessJournalIdempotencyReopen`. The newly added tests may
already pass as acceptance evidence. An actual failure starts the focused
nested RED/GREEN cycle below. The reopen test may script inference, but it must
use the real fsstore, sessionstore journal/replayer, Rig, Tools, and Sandbox
boundaries.

Use this single focused default-tag composition command during the task:

```bash
GOWORK=off GOCACHE=/private/tmp/looprig-coderig-gocache GOFLAGS=-mod=readonly go test ./internal/app -run 'Test(ProcessAdapter|ProcessTools)'
```

**Step 3: Fix only integration defects through TDD**

For every defect, first add the smallest focused failing unit test in the owning
repository, fix it there, review it, then rerun this integration suite.

**Step 4: Verify GREEN and commit**

```bash
git commit -m "test: exercise supervised commands end to end"
```

## PHASE GATE 6: Session and production composition

Run:

```bash
cd /Users/ipotter/code/looprig/.worktrees/long-running-commands/harness
GOWORK=off GOCACHE=/private/tmp/looprig-harness-gocache GOFLAGS=-mod=vendor go test -race ./...
GOWORK=off GOCACHE=/private/tmp/looprig-harness-gocache GOFLAGS=-mod=vendor go test -tags integration -list 'Test(ProcessServicesIntegration|IntegrationProcess)' ./internal/sessionruntime ./pkg/rig
GOWORK=off GOCACHE=/private/tmp/looprig-harness-gocache GOFLAGS=-mod=vendor go test -tags integration -race ./internal/sessionruntime ./pkg/rig
make secure
CGO_ENABLED=0 GOWORK=off GOCACHE=/private/tmp/looprig-harness-gocache GOFLAGS=-mod=vendor go build -trimpath ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOWORK=off GOCACHE=/private/tmp/looprig-harness-gocache GOFLAGS=-mod=vendor go build -trimpath ./...
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 GOWORK=off GOCACHE=/private/tmp/looprig-harness-gocache GOFLAGS=-mod=vendor go build -trimpath ./...
cd /Users/ipotter/code/looprig/.worktrees/long-running-commands/coderig
GOWORK=off GOCACHE=/private/tmp/looprig-coderig-gocache GOFLAGS=-mod=readonly go test -race ./...
GOWORK=off GOCACHE=/private/tmp/looprig-coderig-gocache GOFLAGS=-mod=readonly go test -tags integration -list '^TestIntegrationProcess' ./internal/app
GOWORK=off GOCACHE=/private/tmp/looprig-coderig-gocache GOFLAGS=-mod=readonly go test -tags integration -race ./internal/app
make secure
CGO_ENABLED=0 GOWORK=off GOCACHE=/private/tmp/looprig-coderig-gocache GOFLAGS=-mod=readonly go build -trimpath ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOWORK=off GOCACHE=/private/tmp/looprig-coderig-gocache GOFLAGS=-mod=readonly go build -trimpath ./...
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 GOWORK=off GOCACHE=/private/tmp/looprig-coderig-gocache GOFLAGS=-mod=readonly go build -trimpath ./...
```

No new fuzz target is owned by this phase. `make secure` runs each repository's
format checks, Vet, Staticcheck, Gosec, and Govulncheck once at the gate.

Phase spec review must trace the complete start-to-notification and
restore-to-query flows. Phase quality/security review must inspect adapter
fidelity, shutdown order, event injection boundaries, workspace lease release,
and absence of cross-loop authority.

## Phase 7: Hardening, documentation, and release acceptance

### Task 29: Add adversarial, quota, fuzz, and crash coverage

**Files:**

- Create: `process/arguments_fuzz_test.go`
- Create: `process/manifest_fuzz_test.go`
- Create: `process/cursor_fuzz_test.go`
- Create: `process/security_integration_test.go`
- Create: `../sandbox/internal/exec/process_security_integration_test.go`
- Create: `../harness/internal/sessionruntime/process_security_integration_test.go`
- Create: `../coderig/internal/app/process_security_integration_test.go`

This heading is four separate reviewed microtasks, never one cross-repository
implementation assignment:

- **29A — Tools parser/storage security:** named tests
  `FuzzProcessArguments`, `FuzzProcessManifest`, `FuzzProcessCursor`,
  `TestSecurityOutputControlFraming`, `TestSecurityQuotaExhaustion`, and
  `TestSecurityTerminalRace`. Commit only Tools files.
- **29B — Sandbox escape/security:** named integration tests
  `TestSecurityProcessSetsidEscape`, `TestSecurityProcessDoubleFork`,
  `TestSecurityProcessParentCrash`, and `TestSecurityGrantReservationRace`.
  Commit only Sandbox files after the stabilization handoff.
- **29C — Harness identity/crash publication:** named tests
  `TestSecurityCrossLoopProcessHandle`,
  `TestSecurityLifecycleIDCrashBoundaries`, and
  `TestSecurityWorkspaceLeaseRestoreRace`. Commit only Harness files.
- **29D — Coderig composed threats:** named integration tests
  `TestSecurityProcessPromptInjectionNotification`,
  `TestSecurityProcessOwnerIsolation`, and
  `TestSecurityProcessShutdownLeavesNoDescendants`. Commit only Coderig files.

Execute each microtask with separate evidence:

- **29A:** from the Tools worktree, RED/GREEN is
  `GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache
  GOFLAGS=-mod=readonly go test ./process -run '^TestSecurity'`. Author the
  three fuzz targets but defer fuzz execution to Phase Gate 7. Fix only Tools
  parsing/storage and commit only Tools files.
- **29B:** from Sandbox, use focused default-tag unit seams under
  `./internal/exec`; its single RED/GREEN command is `GOWORK=off
  GOCACHE=/private/tmp/looprig-sandbox-gocache go test ./internal/exec -run
  '^TestSecurityProcessUnit'`. Author all four tagged security cases, but defer
  their discovery and execution to Phase Gate 7. Fix and commit only Sandbox
  files.
- **29C:** from Harness, use focused default-tag process identity/publication
  seams; its single RED/GREEN command is `GOWORK=off
  GOCACHE=/private/tmp/looprig-harness-gocache GOFLAGS=-mod=vendor go test
  ./internal/sessionruntime -run '^TestSecurity(Process|Workspace)'`. Author all
  three tagged security cases, but defer their discovery and execution to Phase
  Gate 7. Fix and commit only Harness files.
- **29D:** from Coderig, use focused default-tag process adapter seams under
  `./internal/app`; its single RED/GREEN command is `GOWORK=off
  GOCACHE=/private/tmp/looprig-coderig-gocache GOFLAGS=-mod=readonly go test
  ./internal/app -run '^TestSecurityProcessUnit'`. Author all three tagged
  composed-security cases, but defer their discovery and execution to Phase
  Gate 7. Fix and commit only Coderig files.

**Task 29 combined acceptance**

Every `*_integration_test.go` created by this task begins with
`//go:build integration`; fuzz files remain in the default unit suite.

Cover:

- guessed/random/cross-session/cross-loop handles;
- process-handle timing oracle;
- command output containing prompt injection, ANSI/OSC links, invalid UTF-8,
  NUL, binary, and huge lines;
- runaway stdout/stderr;
- input backpressure and blocked readers;
- process, waiter, memory, and spool quota exhaustion;
- concurrent stop/exit/timeout/output-limit/shutdown;
- corrupted/truncated/swapped manifests and spools;
- shell background, `nohup`, double fork, and `setsid`;
- host process crash;
- workspace scoped-write ancestor/descendant races;
- completion publisher and notifier failures.

After 29A–29D are separately committed and reviewed, proceed directly to Task
30. Phase Gate 7 performs all four repositories' tagged race suites and 30
seconds of each fuzz target. A gate failure first receives the smallest focused
unit regression in the owning repository, a reviewed RED/GREEN fix, and then a
rerun of the affected gate matrix.

### Task 30: Document the public contract and operations

**Files:**

- Modify: `README.md`
- Modify: `CLAUDE.md`
- Modify: `docs/specs/module.md`
- Modify: `example_readme_test.go`
- Modify: `../harness/pkg/tool/README.md`
- Modify: `../harness/pkg/session/README.md`
- Modify: `../harness/pkg/event/README.md`
- Modify: `../sandbox/README.md`
- Modify: `../sandbox/SPEC.md`
- Create: `../coderig/README.md`
- Modify: `../coderig/docs/specs/coderig-assembly.md`

**Step 1: Write failing documentation/example tests**

Add compile-tested examples for legacy Bash, background, yield, polling,
wait-many, input, stop, PTY, and Coderig composition. Add schema assertions that
examples match the shipped tool definitions.

**Step 2: Verify RED**

Run the one focused documentation/example command:

```bash
GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go test . ./bash ./process -run 'Test(Readme|Example|Schema)'
```

Expected: FAIL before docs and examples are updated.

**Step 3: Document boundaries and operations**

Document:

- exact API and stable errors;
- lifecycle/state diagram;
- session/loop ownership;
- quotas and defaults;
- output cursor, gap, base64, and artifact behavior;
- PTY/ConPTY platform requirements;
- workspace lease effects;
- restore/lost semantics;
- safe shutdown and crash guarantees;
- integration-test and Windows CI commands;
- no local PID reattachment.

**Step 4: Verify GREEN and commit**

Re-run the exact focused command from Step 2.
Commit independently in each repository with
`docs: document supervised command lifecycle`.

## PHASE GATE 7: Run the complete release acceptance matrix

**Files:**

- No intended production changes.
- Modify: `docs/plans/2026-07-27-long-running-command-supervision-verification.md`

**Step 1: Verify clean diffs and module graphs**

In each repository:

```bash
git status --short
git diff --check
go mod verify
```

Expected: clean worktrees except the verification record while it is being
written; modules verify.

**Step 2: Run all race and integration suites**

Harness:

```bash
cd /Users/ipotter/code/looprig/.worktrees/long-running-commands/harness
GOWORK=off GOCACHE=/private/tmp/looprig-harness-gocache GOFLAGS=-mod=vendor go test -race ./...
GOWORK=off GOCACHE=/private/tmp/looprig-harness-gocache GOFLAGS=-mod=vendor go test -tags integration -list 'Test(ProcessServicesIntegration|IntegrationProcess|Security)' ./...
GOWORK=off GOCACHE=/private/tmp/looprig-harness-gocache GOFLAGS=-mod=vendor go test -tags integration -race ./...
```

Tools:

```bash
cd /Users/ipotter/code/looprig/.worktrees/long-running-commands/tools
GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go test -race ./...
GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go test -tags integration -list 'Test(SupervisorIntegration|Integration|Security)' ./...
GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go test -tags integration -race ./...
GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go test ./internal/safetext -run '^$' -fuzz '^FuzzNormalize$' -fuzztime=30s
GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go test ./process -run '^$' -fuzz '^FuzzProcessArguments$' -fuzztime=30s
GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go test ./process -run '^$' -fuzz '^FuzzProcessManifest$' -fuzztime=30s
GOWORK=off GOCACHE=/private/tmp/looprig-tools-gocache GOFLAGS=-mod=readonly go test ./process -run '^$' -fuzz '^FuzzProcessCursor$' -fuzztime=30s
```

Sandbox:

```bash
cd /Users/ipotter/code/looprig/.worktrees/long-running-commands/sandbox
GOWORK=off GOCACHE=/private/tmp/looprig-sandbox-gocache go test -race ./...
GOWORK=off GOCACHE=/private/tmp/looprig-sandbox-gocache go test -tags integration -list 'Test(Integration|Security)' ./...
GOWORK=off GOCACHE=/private/tmp/looprig-sandbox-gocache go test -tags integration -race ./...
```

Coderig:

```bash
cd /Users/ipotter/code/looprig/.worktrees/long-running-commands/coderig
GOWORK=off GOCACHE=/private/tmp/looprig-coderig-gocache GOFLAGS=-mod=readonly go test -race ./...
GOWORK=off GOCACHE=/private/tmp/looprig-coderig-gocache GOFLAGS=-mod=readonly go test -tags integration -list 'Test(Integration|Security)' ./...
GOWORK=off GOCACHE=/private/tmp/looprig-coderig-gocache GOFLAGS=-mod=readonly go test -tags integration -race ./...
```

Expected: PASS with zero race reports.

**Step 3: Run builds and static checks**

For every repository, from its absolute worktree:

```bash
CGO_ENABLED=0 GOWORK=off GOCACHE=/private/tmp/looprig-REPO-gocache go build -trimpath ./...
make secure
```

For Harness, Tools, Sandbox, and Coderig where supported:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOWORK=off go build -trimpath ./...
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 GOWORK=off go build -trimpath ./...
```

Run live Windows integration and acceptance workflows and record their run IDs.

**Step 4: Perform final reviews**

Dispatch:

1. a final spec-compliance reviewer over all four repository ranges;
2. a final concurrency/process-tree security reviewer;
3. a final API/backward-compatibility reviewer;
4. a final test-quality reviewer focused on integration realism and flake risk.

Fix every critical and important finding through TDD and repeat the entire
affected acceptance subset plus reviewer re-check.

**Step 5: Write and commit verification evidence**

Record exact SHAs, commands, exit results, Windows CI evidence, known
environment constraints, and reviewer approvals in the verification document.

```bash
git add docs/plans/2026-07-27-long-running-command-supervision-verification.md
git commit -m "docs: record supervised command verification"
```

## Completion criteria

The implementation is complete only when:

- unchanged Bash calls retain their exact behavior;
- background and yielded commands share one supervised lifecycle;
- output cursors, safe text, raw base64, input, resize, and stop work;
- Unix PTY and Windows ConPTY pass real integration tests;
- descendants cannot survive stop, session shutdown, parent crash, or restore;
- process handles are isolated by session and loop;
- workspace leases reflect authoritative enforced access;
- manifests and bounded spools restore safely without trusting PIDs;
- completion events and notifications are metadata-only and at most once;
- all phase and final reviewers approve;
- all unit, race, tagged integration, build, static, and cross-platform gates
  pass with recorded evidence.

# Long-Running Command Supervision Specification

**Status:** Approved

**Owners:** Tools, Harness, Sandbox; Coderig composes the concrete adapters

**Scope:** Supervised foreground, yielded, background, and interactive shell commands

## Summary

Looprig will support long-running shell commands through a session-scoped process
supervisor owned by Tools, generic lifecycle contracts owned by Harness, and
enforced process execution owned by Sandbox.

The design combines the strongest properties observed in current coding agents:

- Codex's single command API, bounded initial yield, interactive PTY, incremental
  output, and explicit stdin continuation;
- Claude's foreground-to-background handoff, completion notification, and
  separate output and stop operations;
- Grok Build's explicit background mode, multi-process waiting, durable output,
  process ownership, and process-tree cleanup;
- OpenCode's simple foreground compatibility, streaming output, timeout, and
  cancellation behavior.

The result remains backward-compatible for existing `Bash` callers while adding
the lifecycle, durability, isolation, and teardown guarantees required for
commands that outlive an individual tool invocation.

Tools and Sandbox intentionally remain independent modules. Because Go interface
method signatures do not permit a Sandbox method returning a Sandbox-named
process type to implement a Harness method returning a Harness-named process
interface, Coderig provides the small adapter that imports both modules. Neither
low-level module gains a reverse dependency.

## Goals

The implementation must:

1. preserve the current synchronous `Bash` behavior for existing arguments;
2. allow a command to be started in the background immediately;
3. allow a foreground command to yield after a caller-selected wait budget and
   continue under supervision;
4. allow output to be polled or waited on without replaying the entire transcript;
5. allow stdin, EOF, terminal resize, interrupt, terminate, and kill operations;
6. support real PTYs on Unix and Windows without silently degrading behavior;
7. bind every process to its originating session, loop, and tool execution;
8. prevent process handles from being used across owners;
9. preserve bounded completed output and metadata across normal session restore;
10. terminate process trees reliably during explicit stop, session shutdown,
    workspace restore, supervisor crash, and parent process death;
11. integrate background filesystem access with workspace mutation and checkpoint
    coordination for the entire process lifetime;
12. expose stable, typed failures suitable for model-facing rendering and
    programmatic callers;
13. keep output, stdin, and shell-controlled terminal content out of durable
    model event journals.

## Non-goals

The first implementation will not:

- reattach to an arbitrary local PID after a host or Harness restart;
- provide daemon management beyond the owning Looprig session;
- permit unsupervised `nohup`, disowned jobs, or shell backgrounding to escape
  the supervisor;
- offer remote execution or container orchestration;
- provide arbitrary random access to unbounded raw output;
- persist stdin or full command output in the Harness event journal;
- silently emulate PTY behavior with ordinary pipes;
- share a process between sibling delegates or loops;
- support shell job-control syntax as a substitute for process handles.

## Reference-agent comparison

| Agent | Useful behavior | Limitation to avoid |
| --- | --- | --- |
| Codex | One `exec_command` entry point; `tty`; bounded initial yield; process ID when still running; `write_stdin` doubles as poll and input; bounded head-tail output | Primarily in-memory process lifetime and a relatively small process table |
| Claude | `run_in_background`; automatic foreground handoff; completion notification; durable task output; `TaskOutput` and `TaskStop` | Background Bash is not centered on interactive PTY continuation; tasks are not restorable across session restart |
| Grok Build | Explicit background mode; auto-background; wait-one/wait-many; task snapshots; durable files; owner cleanup; output-runaway protection; process groups and Windows jobs | Multiple overlapping task abstractions and less uniform interactive control |
| OpenCode | Straightforward foreground shell streaming, timeout, abort, and force-kill behavior | No complete model-facing long-running process lifecycle |

Looprig deliberately uses one supervised process model rather than separate
"foreground command", "background task", and "terminal session" models.

## Ownership boundaries

### Tools

Tools owns the behavior visible to a model or direct tool consumer:

- `Bash`;
- `ProcessOutput`;
- `ProcessInput`;
- `ProcessStop`;
- the session-scoped `Supervisor`;
- process identity validation and owner matching;
- state transitions;
- in-memory output windows;
- disk spools and process manifests;
- output cursor semantics;
- waiters and completion fan-out;
- quotas and completed-process retention;
- safe-text normalization;
- model-facing result and error rendering.

Tools must not import Harness internals or Sandbox directly. It consumes public
Harness identity, workspace, and session-resource contracts through tool
bindings. A process-enabled Bash definition captures a Tools-owned, narrow
runner resolver as an immutable construction dependency and resolves the
concrete `AsyncProcessRunner` from the validated bound LoopID. ProcessOutput,
ProcessInput, ProcessStop, and the session supervisor never receive a runner.

### Harness

Harness owns generic orchestration contracts:

- session, loop, and tool-execution identity in bindings;
- registration of resources that must be closed with a session;
- keyed, concurrency-safe get-or-create access to shared session resources;
- a configured, restorable private per-session storage provider for resource
  manifests and spools;
- the public async process runner interface contract consumed by Tools;
- durable process lifecycle event types;
- completion notification delivery;
- session shutdown and restore ordering;
- workspace observation invalidation;
- workspace leases that may outlive one tool invocation.

Harness must not know Bash-specific JSON schemas, output cursor rendering, spool
formats, or shell semantics.

Harness does not provision an async runner through Rig/session lifecycle
options, and `ProcessBinding` contains only the session resource registry.
Coderig constructs a role-local Sandbox-to-Harness resolver over its
`ExecutorSet` and passes it only to the role's process-enabled Bash definition.

### Sandbox

Sandbox owns operating-system enforcement:

- asynchronous process spawn;
- ordinary stdin/stdout/stderr pipes;
- PTY/ConPTY creation;
- process group or Job Object membership;
- signals and graceful-to-forceful termination;
- parent-death cleanup;
- confirmation that the process tree has exited;
- enforcement of the prepared filesystem and network grants for the lifetime of
  a process.

Sandbox must not know sessions, loops, model tool names, output cursors, or
Harness event types.

### Coderig

Coderig is the production composition root. It owns:

- the adapter from Sandbox's public asynchronous process API to Harness's public
  async runner/process contracts;
- configuration of the durable resource root beneath Coderig's data directory
  for persisted sessions and an isolated temporary root for headless sessions;
- construction of one runner-free Tools supervisor through the Harness
  session-resource registry, regardless of which process definition first
  resolves it;
- role-specific Bash runner resolution from the same per-loop Sandbox
  `ExecutorSet` used by the access gate;
- installation of runner-free ProcessOutput, ProcessInput, and ProcessStop
  definitions;
- integration tests proving the composed path uses Sandbox enforcement.

The adapter contains no supervision policy, output buffering, authorization, or
process-tree implementation. Those remain in their owning modules.

## Identity and authorization

Each process has an immutable authority owner:

```text
SessionID + LoopID + ProcessID
```

`ProcessID` is an opaque, cryptographically random handle with at least 128 bits
of entropy. It must not encode an OS PID, filesystem path, owner identifier, or
creation timestamp.

Every follow-up operation provides the process handle and receives current
bindings. Tools resolves the handle and compares SessionID and LoopID. A mismatch
is rendered as `not_found`; callers must not be able to distinguish a nonexistent
handle from a handle owned by another session, loop, or delegate.

The originating Bash `ToolExecutionID` is stored immutably as audit provenance,
but it is not compared to the execution ID of ProcessOutput, ProcessInput, or
ProcessStop: every follow-up tool invocation necessarily has a new execution ID.
The opaque ProcessID is the stable process capability, attenuated by the current
session and loop bindings.

The initial Bash gate and Sandbox grants authorize process creation. Follow-up
operations do not re-run the original Bash gate, but they are restricted to the
immutable owner and the capabilities of their specific tool:

- `ProcessOutput` is read-only;
- `ProcessInput` may write only to the process input/terminal;
- `ProcessStop` may signal only the owned process tree.

## Bash API

The existing Bash definition is extended without changing the meaning of current
fields:

```json
{
  "command": "string, required",
  "workdir": "string, optional",
  "timeout": "integer seconds, optional",
  "background": "boolean, optional, default false",
  "yield_time_ms": "integer, optional",
  "tty": "boolean, optional, default false",
  "max_output_bytes": "integer, optional",
  "access": "existing prepared-access input, optional"
}
```

Semantics:

- A call using only existing fields follows the existing foreground path,
  including its current default and maximum timeout.
- `background: true` returns as soon as spawn, manifest persistence, and
  supervision registration succeed.
- `yield_time_ms` waits up to the requested initial budget. If the process exits,
  Bash returns its terminal result. If it is still running, Bash returns the
  process handle and output observed during the budget.
- `tty: true` requests a real PTY/ConPTY. Failure to allocate one returns
  `pty_unavailable`; it never falls back to pipes.
- `timeout` is a hard process-lifetime deadline, not a per-poll deadline.
- `timeout` remains integer seconds for wire compatibility with existing Bash.
- `timeout: 0` is valid only when `background` or `yield_time_ms` enables
  supervision and means "until session shutdown".
- `max_output_bytes` may reduce the per-process disk ceiling but may not exceed
  the configured supervisor ceiling.
- Detached shell syntax is rejected when it would escape supervision. Descendants
  that remain in the supervised process group or job are allowed.
- Legacy foreground Bash retains its current shell compatibility. Conservative
  detached-syntax rejection applies only when a call requests supervision;
  process-tree containment remains the security boundary.

The Bash result has one of two shapes:

```json
{
  "status": "exited",
  "exit_code": 0,
  "output": "...",
  "started_at": "...",
  "finished_at": "...",
  "duration_ms": 123
}
```

or:

```json
{
  "status": "running",
  "process_id": "opaque",
  "output": "...",
  "next_cursor": 42,
  "started_at": "...",
  "backgrounded": true
}
```

`backgrounded` is true for explicit background starts and for yielded foreground
starts that remain alive.

## ProcessOutput API

`ProcessOutput` supports non-mutating inspection:

```json
{
  "process_ids": ["opaque"],
  "cursor": 0,
  "limit_bytes": 32768,
  "encoding": "safe_text | base64",
  "wait": "poll | any | all",
  "timeout_ms": 10000
}
```

One process may be supplied as `process_id`; multiple processes use
`process_ids`. Supplying neither, both, duplicates, or an empty list is invalid.

- `poll` returns immediately.
- `any` waits until any selected process has new output after its supplied cursor
  or becomes terminal.
- `all` waits until every selected process has new output after its supplied
  cursor or becomes terminal.
- The wait timeout affects only the output call, never the process.
- Multi-process results preserve input order.
- Waiters are notified by output append and terminal-state transitions, not by
  periodic polling.

Each result includes:

```json
{
  "process_id": "opaque",
  "status": "running",
  "output": "...",
  "start_cursor": 0,
  "next_cursor": 42,
  "total_bytes": 42,
  "gap": false,
  "normalized": false,
  "binary": false,
  "artifact": {"id": "opaque", "encoding": "base64"},
  "exit_code": null,
  "reason": null,
  "started_at": "...",
  "finished_at": null
}
```

Cursors are monotonically increasing byte offsets in the combined process output
stream. A cursor older than retained/spooled data returns the earliest available
bytes with `gap: true`. A cursor beyond `total_bytes` returns `cursor_ahead`.
Calls never silently reset a cursor.

`encoding` defaults to `safe_text`. `base64` reads the same owner-authorized raw
spool bytes without exposing a host path. A result's `artifact` is an opaque
descriptor for that same bounded process spool; callers retrieve its bytes with
ProcessOutput and the original process handle, cursor, and `base64` encoding.

## ProcessInput API

`ProcessInput` writes to an owned live process:

```json
{
  "process_id": "opaque",
  "data": "string, optional",
  "cursor": "integer, optional",
  "eof": "boolean, optional",
  "rows": "integer, optional",
  "cols": "integer, optional",
  "yield_time_ms": "integer, optional"
}
```

At least one of data, EOF, or resize must be requested. Resize is valid only for
PTY processes. EOF is idempotent for pipe-backed processes and maps to terminal
input semantics appropriate to the platform for PTYs. Writes are serialized per
process and bounded; the tool may not block indefinitely behind a process that
does not consume input.

After the operation, `yield_time_ms` optionally waits for output or termination
and returns the same snapshot shape as `ProcessOutput`, beginning at `cursor`
when supplied and at the current end offset captured before the input operation
when omitted.

## ProcessStop API

`ProcessStop` controls the whole owned process tree:

```json
{
  "process_id": "opaque",
  "mode": "interrupt | terminate | kill",
  "grace_ms": 5000
}
```

- `interrupt` sends the platform-equivalent interactive interrupt. It does not
  terminalize the supervisor state unless the process exits.
- `terminate` requests graceful termination and escalates to kill after
  `grace_ms`.
- `kill` immediately force-terminates the process tree.
- Repeating a stop operation against a terminal process is successful and
  returns the existing terminal result.
- A stop result is not successful until Sandbox confirms that the owned process
  tree has exited or returns a typed teardown failure.

## State machine

The externally visible states are:

```text
starting
running
exited
failed
timed_out
interrupted
terminated
killed
lost_on_restore
```

Allowed transitions:

```text
starting -> running
starting -> failed
starting -> terminated | killed

running -> exited | failed | timed_out | terminated | killed
running -> interrupted only when interrupt causes exit

starting | running -> lost_on_restore during restore reconciliation
```

Terminal states are immutable. Completion is published only after the terminal
manifest is durably written. Concurrent exit, timeout, stop, output-limit, and
shutdown paths race through one compare-and-set terminalization path, producing
at most one terminal state and one completion event.

## Supervisor lifetime

The supervisor is created once per Harness session and registered as a session
resource. Harness bindings expose a keyed session-resource registry; every
process-tool definition resolves the same supervisor key with atomic
get-or-create semantics. The registry obtains storage from an explicit
session-resource storage provider. Persisted Coderig sessions use a stable
`<data-dir>/resources/<session-id>` root on both new and restore; headless
sessions use isolated temporary storage and do not claim cross-process restore.
The Coderig process owns each headless temporary root for its lifetime and keys
the session subdirectory by SessionID. Reconstructing the same headless session
in the same Coderig process therefore reopens the same resource root; only an
actual Coderig restart forfeits headless restore.
Unavailable or identity-mismatched durable storage fails session construction.
The resource root is never the workspace.

Storage resolution belongs to session construction, not Rig definition
assembly. A new session mints its SessionID, resolves the configured provider,
and creates or validates an owner-only, versioned durable identity anchor
binding that SessionID and the provider identity in the private resource root
before any process-enabled definition is bound. Restore resolves and validates
that same root/anchor before restore planning begins. Missing, unavailable,
corrupt, or identity-mismatched storage aborts construction before process
binding or resource creation.

New-session and restore construction use the same late-bound session bridge.
Definitions may be probed before the final Session and event hub exist, so a
supervisor must not capture an internal `*Session` during bind. Harness activates
the bridge only after the session, durable publisher, notifier, and restore
state are ready.

The bridge contract lives in `pkg/tool`, below both `pkg/event` and
`pkg/command`, so it cannot import either concrete journal payload package.
`SessionResourceServices` carries two narrow, typed services: lifecycle
publication and completion notification. Their request DTOs contain only
closed lifecycle/state/reason enums, stable UUIDs, a grammar- and length-bounded
opaque process handle, timestamps, exit metadata, and a 512-byte-bounded
diagnostic. They have no `any` payload, command text, output, stdin, host path,
OS PID, environment, or other unbounded string field. `pkg/event` and
`pkg/command` map those neutral DTOs into their sealed durable types and validate
that the DTO coordinates and stable IDs match the enclosing record. Service
construction rejects nil and typed-nil implementations before activation; the
post-contract zero service set is invalid and activation fails closed.

A supervised process is independent of the context of the Bash tool invocation.
Cancelling a foreground invocation before handoff cancels its start; after a
process handle has been returned, invocation cancellation does not kill the
process.

Supervisor shutdown:

1. closes admission to new processes and follow-up writes;
2. snapshots all running handles;
3. requests graceful termination concurrently;
4. force-kills trees that exceed the configured grace period;
5. confirms all trees have exited;
6. flushes final output and terminal manifests;
7. releases workspace leases;
8. closes waiters and storage handles;
9. returns any teardown failures to Harness.

Running processes are never silently evicted to satisfy metadata quotas.

## Output capture and storage

Defaults:

| Resource | Default |
| --- | --- |
| In-memory rolling window | 1 MiB per process |
| Disk spool | 64 MiB per process |
| Inline model result | 32 KiB |
| Process handle entropy | at least 128 bits |
| Graceful shutdown period | 5 seconds |

The supervisor writes a combined, ordered output stream. Pipe mode tags source
chunks internally as stdout or stderr while assigning one global cursor. PTY
mode naturally exposes one combined terminal stream.

The in-memory window is optimized for recent polling. The spool is the bounded
source of truth for completed output and cursor recovery. Writes use a single
per-process append sequence so cursor order is deterministic even when stdout
and stderr are read concurrently.

Reaching the configured spool ceiling triggers process-tree termination and the
terminal reason `output_limit`. The supervisor must not keep discarding live
output while allowing an unbounded producer to run.

Model-visible text passes through safe-text normalization:

- invalid UTF-8 is replaced deterministically;
- disallowed terminal control sequences are escaped or removed;
- binary detection is reported;
- normalization is reported;
- raw bytes remain only in the bounded spool and are exposed only through an
  opaque artifact descriptor plus owner-authorized ProcessOutput base64 reads.

No filesystem path to a spool or manifest is returned to a model.

## Manifests and durability

Before returning a process handle, Tools atomically persists a manifest containing:

- format version;
- opaque process ID;
- owner identity;
- sanitized command metadata;
- prepared access summary;
- PTY mode;
- process state;
- created and started timestamps, plus the finished timestamp when terminal;
- timeout deadline;
- spool metadata and cursor bounds;
- OS execution metadata needed only for same-process teardown;
- terminal result fields when complete;
- stable lifecycle EventIDs and completion-notification CommandID allocated
  before publication;
- completion-published marker as an optimization, not the deduplication boundary.

Manifest updates use write-new, sync, and atomic replace semantics. State and
cursor metadata never move backward.

On normal session restore:

- completed manifests and retained output are queryable;
- a manifest marked running or starting becomes `lost_on_restore`;
- recorded local PIDs are never trusted or signalled;
- lifecycle and notification publication reuses the stable persisted IDs, so
  the durable journal idempotency index deduplicates a crash between append and
  marker;
- the completion-published marker avoids needless retries but is not required
  for at-most-once journal state.

Authenticated reattachment to an external execution service may be added later;
local PID reattachment is explicitly excluded.

## Durable events and notifications

Harness defines typed metadata-only lifecycle events:

- `ProcessStarted`;
- `ProcessBackgrounded`;
- `ProcessCompleted`;
- `ProcessStopRequested`;
- `ProcessLost`.

Events include identity, state, timestamps, exit metadata, reason, and bounded
non-output diagnostics. Command output and stdin are excluded.

Tools owns and durably persists the lifecycle timestamps before publication.
Harness validates and preserves the supplied ProcessCreatedAt,
ProcessStartedAt, and ProcessFinishedAt in the event payload. These names
deliberately distinguish process lifecycle clocks from the Harness event
Header's envelope CreatedAt. Harness adds only envelope metadata absent from the
neutral DTO and never substitutes its publication clock for a process clock.

The closed lifecycle matrix is:

| Kind | State | Reason |
| --- | --- | --- |
| started | running | none |
| backgrounded | running | none |
| stop-requested | starting or running | interrupted, terminated, or killed |
| completed | exited | exited |
| completed | failed | failed |
| completed | timed-out | timed-out |
| completed | interrupted | interrupted |
| completed | terminated | terminated, runner-shutdown, or output-limit |
| completed | killed | killed, runner-shutdown, or output-limit |
| lost | lost-on-restore | lost-on-restore |

Started/backgrounded are nonterminal and require creation/start times.
Stop-requested is nonterminal, carries no finish/exit metadata, and names only
the requested portable signal. Completed/lost require a finish time; only
completed/exited requires an exit code, and failed/lost alone may carry the
bounded diagnostic. Every unlisted kind/state/reason combination is invalid.
Task 4 extends the existing `pkg/tool.ProcessTerminalReason` for failed,
output-limit, and lost-on-restore by appending values without renumbering the
Task 1 constants; it does not define a second reason type.

The public late-bound service DTOs are defined in `pkg/tool`; the concrete
events use the same bounded `pkg/tool` enums rather than making `pkg/tool`
depend on `pkg/event`. Completion notifications use a separate DTO containing
only the stable CommandID, target session/loop coordinates, bounded process
handle, terminal state, and enum reason. This leaf-package split is
load-bearing: a generic resource may publish and notify without importing
Harness runtime internals or accepting an opaque payload.

Tools allocates and persists the stable lifecycle/notification IDs before a
transition can be published. Harness uses those IDs as the EventID/CommandID
rather than minting replacements.

Stable IDs are necessary but not sufficient. Harness adds backend-neutral,
durable journal deduplication for both event and command records:

- opening a session journal reconstructs an index of every non-zero
  idempotency ID from the durable ledger before accepting appends;
- an identical retry returns the original sequence and `appended=false`
  without writing or publishing again;
- reusing an ID for a different record is a typed collision error;
- the index is updated under the same journal append lock, so two concurrent
  retries cannot both append;
- the behavior is storage-backend independent and is integration-tested through
  the real fsstore/sessionstore reopen path.

The opening ownership fence deliberately uses the raw fenced append path and is
never deduplicated, even when a lease epoch repeats. Replay preserves the
envelope ID through inline and blob-pointer decoding, verifies that an outer
blob-pointer ID equals the resolved inner ID, and rejects inconsistencies before
index hydration. The durable fingerprint is the persisted record kind plus
payload bytes; it excludes the transient `CommandRecord` route because the
current envelope does not persist that route. `ProcessNotification` therefore
carries its target coordinates in its sealed payload, and append validation
requires those coordinates to equal the enclosing live command route.

The Hub accepts an optional result-bearing appender. Its no-persistence appender
reports `appended=true`; a durable duplicate reports `appended=false`, and Hub
does not reapply or rebroadcast that event.

Completion command delivery is at-least-once across a crash boundary but
idempotent. The owning native loop keeps a bounded set of unresolved
CommandID/payload-fingerprint entries, seeded on restore by subtracting durable
command-causality events from the full process-notification command replay.
Consumed IDs are not kept in an evicting cache: `appended=false` with no
unresolved entry means the durable command was already consumed.

Live delivery uses a pre-append reservation handshake. The loop atomically
reserves the unresolved `(CommandID, fingerprint, pending)` entry and its
bounded capacity before Harness appends the command. No capacity returns
retryable-full before append. Append failure releases the reservation; append
success commits it. If the regular inbox is full after append, the pending
reservation remains and a same-ID retry reuses it, so `appended=false` cannot be
misclassified as consumed. Restore fails closed if the reconstructed unresolved
set exceeds its configured cap, and no path evicts an unresolved entry.
Reservation is singleflight per `(LoopID, CommandID)` and carries a unique
generation token. Identical concurrent claimants share the leader's outcome;
only the current uncommitted generation may be released, and commit is
idempotent. A failed leader cannot delete a later successful pending obligation.
A transient acknowledgement reports accepted, duplicate, collision,
retryable-full, or stopped. An identical unresolved command is retried; a
consumed duplicate is ignored; a conflicting payload is rejected. The
notification is removed from the unresolved set only after an enduring loop
event carrying its command cause commits. This closes both the
append-before-dispatch and dispatch-before-crash windows without making the
ordinary audit-only command path strict.

Process-enabled definitions are supported only by native Harness loops in this
release. The immutable loop definition exposes a read-only `Engine()` view so
Rig/session validation can reject `RequiresProcessServices` on a foreign engine
with `process_notifications_unsupported` before `Bind` or any tool factory is
called; rejection must not silently start a process that cannot receive
completion. Legacy foreground Bash remains available under its existing engine
rules. A future backend-neutral foreign notification contract may widen this
scope.

Completion notifications are also metadata-only. They tell the loop that a
process reached a terminal state and provide its opaque handle. The model must
call `ProcessOutput` to inspect command-controlled content. This keeps arbitrary
process output out of trusted notification framing and avoids turning output
into an unsolicited prompt-injection channel.

## Workspace coordination

Background processes require an enforcement-capable async runner. The prepared
filesystem access granted at spawn becomes a workspace lease held for the entire
process lifetime.

Lease classification comes from a two-phase enforcing-runner protocol, not from
the model's declaration or Coderig parsing opaque grants:

1. `PrepareProcess` validates and reserves grants/resources without spawning,
   returning an opaque prepared process and its authoritative effective access;
2. Tools acquires the matching Harness lifetime workspace lease;
3. `Start` consumes the preparation exactly once and spawns;
4. any failure closes the preparation and releases all reservations.

The Coderig adapter only maps Sandbox's normalized access type to Harness's
generic read-only, scoped-write, or broad-write description. If the runner
cannot truthfully prepare access or prove lifetime containment, supervised spawn
fails with `lifetime_enforcement_unavailable`.

This adapter is not a per-session Harness binding and is not a supervisor
dependency. Coderig captures a narrow resolver over each role's
`ExecutorSet` in that role's process-enabled Bash definition. At definition
Build, Tools invokes the resolver with the validated `bindings.LoopID`; the
resolver calls the same `ExecutorSet.For(bindings.LoopID.String())` key/path
that `roleGate` calls with the authorized invocation's provenance LoopID and
wraps that exact executor as an `AsyncProcessRunner`. A resolver error aborts
Build without producing a Bash tool.

The Tools-owned resolver contract is conceptually:

```go
type AsyncProcessRunnerResolver func(context.Context, LoopID) (AsyncProcessRunner, error)
```

Harness rejects a missing or zero LoopID before calling a definition factory.
Runner selection therefore needs no invocation-context provenance lookup.
Invocation context remains authoritative later: the prepared Bash call supplies
its ToolExecutionID and approved grants to `ProcessRequest`.

Harness binds only the shared `SessionResourceRegistry`. Bash, ProcessOutput,
ProcessInput, and ProcessStop can each win the registry's get-or-create race
because the supervisor contains no runner. Only Bash owns execution authority:
it prepares through its resolved runner, acquires the matching workspace lease,
and passes the resulting `PreparedProcess` to `Supervisor.Start`.

Lease compatibility:

| Background access | Compatibility |
| --- | --- |
| Read-only | May coexist with reads and structured writes |
| Scoped write | Blocks overlapping structured writes; disjoint writes may proceed |
| Broad/workspace write | Blocks all structured writes and checkpoints |

All writable background leases block workspace snapshots. Workspace restore
first stops all supervised processes, confirms their exit, then performs restore.

Each admission captures the originating loop's observation capability.
Observation caches are invalidated at process spawn, on reported filesystem
activity when the prepared/running process exposes an activity source, and at
process completion. A broad writer invalidates the entire workspace observation
set.

Activity reporting is an optional typed process capability:

```go
type ProcessActivity struct {
    Kind WorkspaceActivityKind
}

type ProcessActivitySource interface {
    Activities() <-chan ProcessActivity
}
```

Sandbox may expose the equivalent interface without importing Harness and
Coderig maps the value mechanically. Every reported filesystem activity
invalidates the loop's complete observation cache; scoped observation
invalidation is not part of this feature. Malformed or overflowed activity is
treated as broad activity. Activity can never narrow or mutate the immutable
lifetime lease. The activity channel closes before `Wait` returns.

The bare, unenforced runner remains available only for legacy foreground Bash.
It may not create a background, yielded, or detached process.

## Sandbox process contract

Harness exposes a public, shell-agnostic two-phase runner contract conceptually
equivalent to:

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
    ID() string
    Stdout() io.ReadCloser
    Stderr() io.ReadCloser
    Stdin() io.WriteCloser
    Wait(context.Context) (ProcessResult, error)
    Resize(context.Context, rows, cols uint16) error
    Signal(context.Context, ProcessSignal) error
    Close(context.Context) error
}
```

Harness owns these public types but does not manufacture or bind an
implementation. A Coderig resolver constructs the Sandbox adapter for the
validated loop-bound executor at Bash definition Build; `ProcessBinding`
remains registry-only. The runner is absent from supervisor construction and
from all three companion definitions.

Exact Go names may follow existing package conventions, but the contract must:

- distinguish pipe and PTY streams;
- validate/reserve grants before lease acquisition without spawning;
- make prepared start single-use and close idempotent;
- make `Wait` safe to call exactly once through a supervisor-controlled path;
- define concurrent stdin and signal behavior;
- optionally expose the typed activity stream above, with closure ordered
  before `Wait` returns;
- return typed spawn, setup, signal, wait, and teardown errors;
- confirm whole-tree exit;
- not expose an OS PID as the model-facing identity.

Sandbox exposes its equivalent concrete public API without importing Harness.
Coderig wraps the Sandbox request and process in the Harness interfaces; the
adapter is mechanical and is contract-tested in both directions. Sandbox's
implementation must start the child in its final enforcement and process-tree
container before executable code can run.

## Cross-platform process trees

### Unix

Each process starts in a dedicated process group. Sandbox signals the group, not
only its initial child. A lifetime shim holds a parent-death pipe; EOF causes the
shim to terminate the group. PTY support uses `github.com/creack/pty`, approved
for this feature.

Every supervised Linux spawn must enter a mandatory descendant-containment
primitive before executable code runs: either the Rung-1 PID-namespace path or
a delegated cgroup v2 scope retained through terminalization and emptied with
`cgroup.kill` plus an exact zero-process proof. The existing optional
cost-limiting cgroup behavior is insufficient by itself; supervised Rung-2
execution without usable delegation fails before spawn. Retained Landlock path
descriptors and their cleanup remain live across prepare, start, and the final
zero proof. The backend lifetime handle is result-bearing: a kill error,
timeout, non-empty `cgroup.procs`, read/open failure, or scope-removal failure is
failed or indeterminate proof, never success. In particular, inability to read
`cgroup.procs` is not evidence that it is empty. `Wait` retains the whole
authority/lifecycle capsule and transfers it to process-level quarantine until
an exact retry succeeds.

Darwin currently has no primitive in Sandbox that contains a descendant after
`setsid`. Until a concrete backend supplies and proves one, supervised
execution on Darwin reports `lifetime_enforcement_unavailable` before spawn.
Process groups, PID enumeration, or polling are not substitutes. Legacy
synchronous execution remains governed by its existing contract.

### Windows

Each process starts suspended, joins a kill-on-close Job Object, attaches the
configured pipes or ConPTY, then resumes. This extends Sandbox's existing,
reviewed Windows restricted/elevated backends and broker rather than creating an
unconfined side path. Existing `golang.org/x/sys/windows` support is used.
Closing the lifetime handle terminates the job.

The elevated backend's current blocking `enforce.Spec.Launch` bridge must be
split at its existing asynchronous ownership boundary: the protected launcher
returns and transfers its `elevatedRunnerExecution`, and the public process
handle waits or stops that owned execution. It must not place a goroutine around
the blocking `Launch` call. The transfer owns both the per-execution broker
release and the compiled elevated-spec `active.Done` retirement obligation;
the launch stack owns both until it reaches one of three outcomes. A failure
before any Job/process authority exists retires both synchronously exactly once.
After a Job exists or process authority may exist, launch failure retires
neither until exact Job-zero proof; delayed/indeterminate proof transfers the
whole failed-launch capsule to process-level quarantine, whose successful proof
retires both. Successful handoff atomically transfers both obligations to the
returned execution. Compiled-spec release continues waiting while any
obligation is live, never returns early, and unblocks after direct or
quarantined proof. Both restricted and elevated paths retain broker/ACL,
grant/path, proxy, and backend authority through that boundary.

### Unsupported PTY environments

If a platform cannot provide a real PTY/ConPTY, `tty: true` fails with
`pty_unavailable`. Pipe-backed background commands remain supported when
process-tree enforcement is available.

## Quotas and retention

Configuration includes:

- maximum concurrently running processes per loop;
- maximum concurrently running processes per session;
- maximum retained completed processes per session;
- maximum aggregate in-memory bytes;
- maximum aggregate spool bytes;
- maximum process spool bytes;
- maximum pending waiters;
- maximum pending input bytes.

Admission reserves quotas before spawn and releases them on failed setup.
Completed metadata is evicted by least-recently-used order only after the
retention limit is reached. Eviction deletes the manifest and spool atomically
from the supervisor's perspective and never affects a running process.

## Stable errors

The public error taxonomy includes:

- `invalid_arguments`;
- `invalid_settings`;
- `process_quota_exceeded`;
- `output_quota_exceeded`;
- `lifetime_enforcement_unavailable`;
- `process_notifications_unsupported`;
- `spawn_failed`;
- `process_setup_failed`;
- `pty_unavailable`;
- `not_found`;
- `stdin_closed`;
- `input_backpressure`;
- `cursor_gap`;
- `cursor_ahead`;
- `output_limit`;
- `timed_out`;
- `interrupted`;
- `terminated`;
- `killed`;
- `supervisor_shutting_down`;
- `manifest_corrupt`;
- `spool_corrupt`;
- `lost_on_restore`;
- `teardown_failed`.

Errors must support `errors.Is` or typed inspection and render to stable
model-facing codes without exposing host paths, OS PIDs, or cross-owner details.

## Security invariants

The implementation is unacceptable unless all of these remain true:

1. A process handle grants no authority outside its immutable session and loop
   owner; the originating tool execution remains immutable audit provenance.
2. Cross-owner lookup is indistinguishable from a missing handle.
3. A background process never outlives its session resource.
4. A process tree cannot escape through shell backgrounding, grandchildren, or
   closing inherited standard streams.
5. Filesystem and network grants cannot expand after spawn.
6. The model never receives a host path or OS PID as a control handle.
7. Command output never appears in a trusted completion notification.
8. Output and input remain bounded in memory and on disk.
9. Terminal state occurs once; lifecycle and notification publication reuse
   pre-persisted stable IDs, durable journal appends deduplicate identical IDs
   across reopen, and restored loop notification state suppresses replayed
   commands.
10. Restore never signals a PID recovered from persisted metadata.

## Delivery phases

### Phase 1: Harness contracts

Add async process contracts, a keyed session-resource registry and private
storage root, typed lifecycle events, immutable owner/provenance identity, and
lifetime workspace lease coordination.

Acceptance gate:

- contract and coordinator tests pass with `-race`;
- no Bash-specific behavior enters Harness;
- a spec reviewer approves ownership and lifecycle semantics;
- a quality/security reviewer approves the API and locking.

### Phase 2: Tools supervisor core

Add process state, IDs, quotas, cursor buffer, spool, atomic manifests, waiters,
restore reconciliation, and deterministic fakes.

Acceptance gate:

- state-machine, quota, cursor, corruption, and concurrency tests pass with
  `-race`;
- terminalization and completion are proven at-most-once;
- reviewers approve spec compliance, storage safety, and concurrency.

### Phase 3: Sandbox non-PTY execution

Add asynchronous spawn, pipes, process groups/jobs, signals, parent-death
enforcement, grants, and whole-tree confirmation.

Acceptance gate:

- descendant cleanup, timeout, stop escalation, parent death, and grant tests
  pass with `-race`;
- supported OS build checks pass;
- reviewers approve process-tree and enforcement security.

### Phase 4: Model-facing process tools

Extend Bash and add ProcessOutput, ProcessInput, and ProcessStop using the
supervisor and Harness bindings.

Acceptance gate:

- legacy Bash compatibility and all new schemas/results are tested;
- foreground, explicit background, yield, polling, waiting, input, and stop
  workflows pass;
- reviewers approve model API behavior and authorization.

### Phase 5: PTY and ConPTY

Add Unix PTY, Windows ConPTY, resize, terminal input, EOF, and interrupt behavior.

Acceptance gate:

- interactive echo, resize, EOF, interrupt, and unsupported-platform tests pass;
- there is no pipe fallback for `tty: true`;
- reviewers approve platform behavior and dependency use.

### Phase 6: Session lifecycle integration

Wire session construction, resource shutdown, durable lifecycle events,
metadata-only completion notifications, workspace restore, manifest restore,
and Coderig's Sandbox-to-Harness async adapter and process-tool composition.

Acceptance gate:

- shutdown ordering, restore reconciliation, notification deduplication, and
  workspace lease tests pass;
- reviewers approve durability, injection boundaries, and teardown.

### Phase 7: Hardening and acceptance

Exercise quotas, runaway output, binary/control output, concurrent stop/exit,
workspace conflicts, crash recovery, owner isolation, and documentation.

Acceptance gate:

- focused and repository-wide race tests pass;
- static analysis and platform build checks pass;
- a final spec review and final code-quality/security review have no unresolved
  critical or important findings.

## Test-first and review policy

Every implementation task is test-first but does not execute tests:

1. author the focused test before production code;
2. write the minimum contract-complete production change;
3. refactor by inspection only;
4. run `gofmt` on changed Go files;
5. inspect and commit the task diff.

All test execution and all independent spec/code review occur only at the owning
phase boundary. Task-local commands in the implementation plan are queued gate
coverage, never per-task execution instructions.

Unit tests are necessary but insufficient. Each phase that crosses a module or
OS boundary adds tagged integration coverage. Sandbox integration tests execute
real descendants, grants, process-tree teardown, and PTY behavior. Coderig
integration tests exercise the composed Bash-to-Tools-to-Harness-to-Sandbox
path. Harness integration tests cover shutdown, restore, checked events, and
workspace leases. Focused functional tests, race tests, tagged integration
discovery/execution, fuzzing, repeated stress, static analysis, vulnerability
checks, and trimpath builds all run only at phase boundaries. Supported
repositories run integration tests with `-race`; Windows ConPTY and Job behavior
also runs on a Windows CI worker.

At every phase boundary:

1. run every focused functional selector queued by the phase's tasks;
2. run the full relevant repository tests with the race detector;
3. list and run applicable tagged integration tests with `-race`;
4. run every fuzz target accumulated through the phase;
5. run repository format and diff checks, Vet, Staticcheck, Gosec, and
   vulnerability checks;
6. run native and relevant cross-platform `CGO_ENABLED=0 -trimpath` builds;
7. obtain an independent spec-compliance review;
8. fix every gap, rerun affected gate coverage, and obtain re-review;
9. obtain an independent code-quality and security review;
10. fix every critical or important issue, rerun affected coverage, and obtain
    re-review;
11. record the verified phase before starting the next phase.

## Final acceptance

The feature is complete when a caller can:

1. run a legacy foreground Bash command unchanged;
2. start a bounded background command and receive an opaque handle;
3. yield a foreground command into the same supervised lifecycle;
4. read only new output using cursors;
5. wait on one or multiple processes;
6. interact through pipes or a real terminal;
7. stop the whole process tree predictably;
8. observe metadata-only completion;
9. restore completed output without trusting stale PIDs;
10. shut down a session with no surviving descendants or unreleased workspace
    leases.

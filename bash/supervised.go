// supervised.go routes a SUPERVISED Bash call (background or yield_time_ms,
// normalized by prepare.go's normalizeSupervision into bashArtifact.supervised)
// through the shared, runner-free process.Supervisor. A legacy call never
// reaches this file: bash.go's InvokableRun dispatches to runSupervised only
// when the prepared artifact says so; every other call keeps executing the
// unchanged synchronous `sh -c` (or injected tool.CommandRunner) path.
//
// The concrete BashTool built by NewSupervisedFactory already owns, from its
// bound construction data (Bindings, resolved once at Build), the async
// process runner, the session resource registry, the owning session/loop
// identity, the workspace coordinator, and the observation capability.
// runSupervised never looks up invocation provenance to select a runner —
// b.asyncRunner is a fixed field set at Build.
package bash

import (
	"context"
	"errors"
	"time"

	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/internal/definition"
	"github.com/looprig/tools/internal/workspace"
	"github.com/looprig/tools/process"
)

// supervisorResourceKey names the ONE shared process.Supervisor session
// resource every supervised Bash call (and, from Task 16/17/18 on, every
// ProcessOutput/ProcessInput/ProcessStop call in the same session) obtains
// through tool.SessionResourceRegistry.GetOrCreate: "any of the four
// definitions may win get-or-create" (Task 19's own combined-acceptance
// text) requires all of them to key on this exact same string.
const supervisorResourceKey = "github.com/looprig/tools/process.supervisor"

// SupervisedFactory builds a session-supervised BashTool bound to bindings
// and the caller's already-resolved, validated tool.AsyncProcessRunner.
// bindings must satisfy tool.RequiresWorkspace|tool.RequiresProcessServices
// (the harness Definition.Build boundary validates SessionID/LoopID/
// Workspace/Process before any factory ever runs); runner is a per-Build
// input the caller resolved from the validated bound LoopID BEFORE calling
// this factory — never derived here from ctx or invocation provenance.
// Task 19's root definition owns that resolver; this task only accepts its
// already-resolved result.
type SupervisedFactory func(bindings tool.Bindings, runner tool.AsyncProcessRunner) (*BashTool, error)

// NewSupervisedFactory validates and seals Bash options once, exactly like
// NewFactory, for a session-supervised binding. It never adds a Runner field
// to tool.ProcessBinding and never accepts a runner as one of options: the
// async runner is a distinct per-Build input to the returned SupervisedFactory,
// not a sealed BashOption.
func NewSupervisedFactory(options ...BashOption) (SupervisedFactory, error) {
	config, err := resolveBashOptions(options)
	if err != nil {
		return nil, err
	}
	return func(bindings tool.Bindings, runner tool.AsyncProcessRunner) (*BashTool, error) {
		if workspace.IsNil(runner) {
			return nil, &definition.BuildError{Definition: bashToolName, Dependency: "async_runner"}
		}
		if bindings.Workspace == nil {
			return nil, &definition.BuildError{Definition: bashToolName, Dependency: "workspace"}
		}
		if bindings.Process == nil || workspace.IsNil(bindings.Process.Registry) {
			return nil, &definition.BuildError{Definition: bashToolName, Dependency: "process_registry"}
		}
		bound := config
		bound.coord = bindings.Workspace.Coordinator
		bound.obs = bindings.Workspace.Observations
		b := newBash(bindings.Workspace.Root, bound)
		b.asyncRunner = runner
		b.registry = bindings.Process.Registry
		b.owner = process.Owner{SessionID: bindings.SessionID, LoopID: bindings.LoopID}
		return b, nil
	}, nil
}

// runSupervised is InvokableRun's dispatch target for a supervised call: dir
// is the already-revalidated confined spawn directory (identical
// prepare/run-time consistency check InvokableRun already performs for the
// legacy path). It never returns a Go error — every failure renders as a
// supervisedErrorResult tool-result, matching InvokableRun's documented
// contract.
func (b *BashTool) runSupervised(ctx context.Context, call tool.PreparedCall, art *bashArtifact, dir string) (*tool.ToolResult, error) {
	if workspace.IsNil(b.asyncRunner) {
		return supervisedErrorResult(string(process.CodeLifetimeEnforcementUnavailable)), nil
	}
	lifetimeCoord, ok := b.coord.(tool.WorkspaceLifetimeCoordinator)
	if !ok || workspace.IsNil(lifetimeCoord) {
		return supervisedErrorResult(string(process.CodeLifetimeEnforcementUnavailable)), nil
	}
	if workspace.IsNil(b.registry) {
		return supervisedErrorResult(string(process.CodeLifetimeEnforcementUnavailable)), nil
	}

	// Step 1: PrepareProcess reserves enforcement resources WITHOUT
	// spawning anything (tool.AsyncProcessRunner's own doc contract) —
	// strictly before any lease acquisition below.
	req := tool.ProcessRequest{
		Command:           art.command,
		Directory:         dir,
		Grants:            call.Grants,
		OriginExecutionID: call.ExecutionID,
		Deadline:          processDeadline(art),
		PTY:               art.tty,
	}
	prepared, err := b.asyncRunner.PrepareProcess(ctx, req)
	if err != nil {
		return supervisedErrorResult(string(process.CodeProcessSetupFailed)), nil
	}

	// Step 2: the PREPARED, authoritative access (never any caller-declared
	// or guessed access) determines the exact lifetime lease.
	access := prepared.EffectiveWorkspaceAccess()
	permit, err := lifetimeCoord.AcquireLifetime(ctx, access)
	if err != nil {
		_ = prepared.Close()
		return supervisedErrorResult(string(process.CodeLifetimeEnforcementUnavailable)), nil
	}

	// Step 3: obtain the ONE shared, runner-free Supervisor session
	// resource. A failure here still holds the lease and the preparation —
	// both must be released/closed before returning.
	resource, err := b.registry.GetOrCreate(ctx, supervisorResourceKey, newSupervisorResource)
	if err != nil {
		permit.Release()
		_ = prepared.Close()
		return supervisedErrorResult(string(process.CodeProcessSetupFailed)), nil
	}
	sr, ok := resource.(*supervisorResource)
	if !ok || sr == nil || sr.supervisor == nil {
		permit.Release()
		_ = prepared.Close()
		return supervisedErrorResult(string(process.CodeProcessSetupFailed)), nil
	}

	// Step 4: hand the prepared process to the runner-free Supervisor.Start
	// only now that the lease is held. Start consumes prepared exactly
	// once and, on ANY failure of its own, already releases the lease,
	// closes prepared, and reverses its quota reservation itself (see its
	// doc comment) — runSupervised must NOT repeat any of that here.
	origin := process.Origin{ToolExecutionID: call.ExecutionID}
	lease := leaseFromPermit{permit: permit}
	ceiling := storageCeiling(art)
	yield := process.YieldSettings{Yield: art.background}

	handle, err := sr.supervisor.Start(ctx, b.owner, origin, prepared, lease, nil, nil, ceiling, yield)
	if err != nil {
		return supervisedErrorResult(classifyProcessError(err, process.CodeSpawnFailed)), nil
	}

	// The process is now durably registered (Supervisor.Start persists its
	// StateStarting manifest and registers the entry before ever returning
	// a Handle) — this is the "spawn" invalidation point.
	b.invalidateObservations()

	// Start's own manifest write (StateRunning, StartedAt) is already
	// durable by the time it returns the Handle, so a best-effort reload
	// here already reflects it — for BOTH the live and terminal shapes
	// below, not only the terminal one.
	startedAt := loadStartedAt(sr, handle)

	if art.background {
		// Explicit background: return immediately, exactly as soon as
		// registration is durable, without waiting for any output at all.
		go b.watchAndInvalidate(sr.supervisor, handle)
		return liveSupervisedResult(string(handle), 0, "", startedAt), nil
	}

	budget := time.Duration(art.yieldTimeMS) * time.Millisecond
	terminal, waitErr := waitForTerminal(ctx, sr.supervisor, b.owner, handle, budget, b.invalidateObservations)
	if waitErr != nil || !terminal {
		// Either the budget elapsed with the command still running, or the
		// invocation ctx ended first — either way the process itself keeps
		// running untouched; hand off to the detached watcher and report a
		// LIVE result.
		go b.watchAndInvalidate(sr.supervisor, handle)
		return liveSupervisedResult(string(handle), 0, "", startedAt), nil
	}

	status, exitCode, reason, finishedAt := readTerminalOutcome(sr, handle)
	return terminalSupervisedResult(status, exitCode, reason, startedAt, finishedAt), nil
}

// waitForTerminal blocks, in a sequence of process.Supervisor.Wait(WaitAny)
// calls each bounded by the remaining budget, until handle reaches a
// terminal state or budget elapses (returns terminal=false, err=nil — never
// treated as a hard failure) or ctx itself ends first (also terminal=false,
// err=nil: the caller must still hand off to the detached watcher rather
// than treat a canceled invocation as a process failure). onWake, when
// non-nil, is called once per observed change (including the final terminal
// one) — the caller's "intermediate invalidation" hook.
func waitForTerminal(ctx context.Context, supervisor *process.Supervisor, owner process.Owner, handle process.Handle, budget time.Duration, onWake func()) (terminal bool, err error) {
	if budget <= 0 {
		return false, nil
	}
	deadline := time.Now().Add(budget)
	var generation uint64
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false, nil
		}
		waitCtx, cancel := context.WithTimeout(ctx, remaining)
		statuses, waitErr := supervisor.Wait(waitCtx, owner, process.WaitAny, []process.WaitTarget{{Handle: handle, Generation: generation}})
		cancel()
		if waitErr != nil {
			if errors.Is(waitErr, context.DeadlineExceeded) || errors.Is(waitErr, context.Canceled) {
				return false, nil
			}
			return false, waitErr
		}
		if len(statuses) == 0 {
			return false, nil
		}
		status := statuses[0]
		generation = status.Generation
		if onWake != nil {
			onWake()
		}
		if status.Terminal {
			return true, nil
		}
	}
}

// watchAndInvalidate runs detached from the invocation context (via
// context.Background(), never ctx — mirroring tool.PreparedProcess's own
// documented "the returned Process lives ... independently of the Start
// context" contract, which every post-handoff observer must honor
// identically): it keeps polling the shared Supervisor's exported Wait API
// for handle until it reaches a terminal state, invalidating the bound
// loop's observation set on every wake. This is what makes "runner activity
// triggers intermediate invalidation" and "completion invalidates the bound
// loop observations" true for a call that has already returned its handle
// to the model — including the explicit-background case, which never waits
// at all before returning.
func (b *BashTool) watchAndInvalidate(supervisor *process.Supervisor, handle process.Handle) {
	ctx := context.Background()
	var generation uint64
	for {
		statuses, err := supervisor.Wait(ctx, b.owner, process.WaitAny, []process.WaitTarget{{Handle: handle, Generation: generation}})
		if err != nil || len(statuses) == 0 {
			return
		}
		status := statuses[0]
		generation = status.Generation
		b.invalidateObservations()
		if status.Terminal {
			return
		}
	}
}

// loadStartedAt reads handle's own durable Manifest.StartedAt, best-effort:
// a reload failure or a not-yet-started manifest yields the zero Time,
// which supervisedResult's formatTime/omitempty simply omits.
func loadStartedAt(sr *supervisorResource, handle process.Handle) time.Time {
	manifest, err := sr.manifests.Load(handle)
	if err != nil || manifest.StartedAt == nil {
		return time.Time{}
	}
	return *manifest.StartedAt
}

// readTerminalOutcome reads handle's own durable terminal Manifest —
// process.State, process.Result, and Manifest.FinishedAt — never a value
// Bash computed or guessed itself. A reload failure conservatively reports
// "failed" rather than fabricating a success.
func readTerminalOutcome(sr *supervisorResource, handle process.Handle) (status string, exitCode *int, reason string, finishedAt time.Time) {
	manifest, err := sr.manifests.Load(handle)
	if err != nil {
		return string(process.StateFailed), nil, "failed", time.Time{}
	}
	if manifest.FinishedAt != nil {
		finishedAt = *manifest.FinishedAt
	}
	return string(manifest.State), manifest.Result.ExitCode, manifest.Result.Reason, finishedAt
}

// processDeadline maps the prepared, frozen supervision settings to
// tool.ProcessRequest's Deadline: a zero Time means "no process deadline"
// (art.noDeadline — supervised `timeout: 0`), otherwise a hard deadline
// art.timeout (already clamped at preparation) out from now.
func processDeadline(art *bashArtifact) time.Time {
	if art.noDeadline {
		return time.Time{}
	}
	return time.Now().Add(art.timeout)
}

// storageCeiling maps the prepared `max_output_bytes` setting to the
// Supervisor's per-process StorageCeiling. ProcessRequest carries no
// output-limit field of its own (output retention is Tools' own concern,
// not the runner's), so max_output_bytes reaches admission through
// StorageCeiling.SpoolBytes, not through tool.ProcessRequest. An absent
// setting yields the zero StorageCeiling, which Supervisor.Start's own
// reserveQuota falls back from to its configured per-process default.
func storageCeiling(art *bashArtifact) process.StorageCeiling {
	if !art.hasMaxOutputBytes {
		return process.StorageCeiling{}
	}
	return process.StorageCeiling{SpoolBytes: art.maxOutputBytes}
}

// classifyProcessError renders err's stable process.Code when err is a
// *process.Error (as every failure Supervisor.Start itself returns is), or
// fallback otherwise. It never renders err's free-form Cause.
func classifyProcessError(err error, fallback process.Code) string {
	var perr *process.Error
	if errors.As(err, &perr) {
		return string(perr.Code)
	}
	return string(fallback)
}

// leaseFromPermit adapts a tool.WorkspacePermit (Release with no return
// value) to process.Lease (Release() error) — both exported types with
// exported methods, so, unlike process's package-private lifecycleSink/
// completionNotifier (left nil below; Task 24 wires real lifecycle/
// notification services), Bash CAN implement this one itself.
type leaseFromPermit struct{ permit tool.WorkspacePermit }

func (l leaseFromPermit) Release() error {
	l.permit.Release()
	return nil
}

// supervisorResource adapts the *process.Supervisor this session's supervised
// Bash (and, from Task 16/17/18, ProcessOutput/ProcessInput/ProcessStop)
// calls share to tool.SessionResource, so it can be obtained through
// tool.SessionResourceRegistry.GetOrCreate.
type supervisorResource struct {
	supervisor *process.Supervisor
	manifests  *process.ManifestStore
}

// Activate is intentionally a no-op today: the Supervisor this resource
// wraps is constructed notification-free (newSupervisorResource passes nil
// for both NewSupervisor's lifecycle and notifications parameters — both
// package-private process types Bash cannot implement from outside package
// process; see leaseFromPermit's doc comment for the one seam it CAN cross).
// Task 24 ("Publish lifecycle events and deliver metadata-only
// notifications") is the task that wires services's real capabilities
// through to a live Supervisor.
func (r *supervisorResource) Activate(context.Context, tool.SessionResourceServices) error {
	return nil
}

// Shutdown releases every resource the shared Supervisor still holds.
func (r *supervisorResource) Shutdown(ctx context.Context) error {
	return r.supervisor.Shutdown(ctx)
}

// newSupervisorResource is the tool.SessionResourceRegistry.GetOrCreate
// factory for supervisorResourceKey: it is runner-free (constructs no
// tool.AsyncProcessRunner and calls none of PrepareProcess/Start), exactly
// as the spec requires so any of the four process-backed definitions may
// win the get-or-create race. dir is the private per-session storage
// directory the registry reserves for this key.
func newSupervisorResource(dir string) (tool.SessionResource, error) {
	manifests := process.NewManifestStore(dir)
	supervisor, err := process.NewSupervisor(process.Config{}, manifests, dir, nil, nil)
	if err != nil {
		return nil, err
	}
	return &supervisorResource{supervisor: supervisor, manifests: manifests}, nil
}

var _ tool.SessionResource = (*supervisorResource)(nil)

package process

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
)

// drainChunkBytes bounds each single Read call inside drain. It is large
// enough that ordinary test fixtures and small real command output land in
// one Append per Read, while still bounding worst-case per-call memory for
// a genuinely large single read.
const drainChunkBytes = 32 << 10 // 32 KiB

// manifestSaver is the narrow persistence capability entry.terminalize
// needs from a ManifestStore: reload the manifest a live entry's Start call
// already durably persisted (to recover its pre-persisted stable
// LifecycleEventIDs and every other field a terminal Save must carry
// forward unchanged) and persist the terminal update. *ManifestStore
// satisfies it directly; entry depends on the interface, not the concrete
// type, purely so a test can substitute a call-counting fake in place of a
// real ManifestStore to observe terminalize's one-shot Save guarantee
// (TestSupervisorTerminalRaceChoosesOnce) without touching real disk I/O
// semantics.
type manifestSaver interface {
	Load(Handle) (Manifest, error)
	Save(Manifest) error
}

var _ manifestSaver = (*ManifestStore)(nil)

// lifecycleTerminalEvent is the payload lifecycleSink.publish receives at a
// process's one-shot terminal transition (entry.terminalize). EventID is
// always one of the manifest's pre-persisted, already-persisted
// LifecycleEventIDs (manifest.go) -- Completed for an ordinary terminal
// outcome, Lost for a restore-reconciliation outcome (Task 9; State ==
// StateLostOnRestore) -- never a freshly minted ID.
type lifecycleTerminalEvent struct {
	EventID    uuid.UUID
	Kind       tool.ProcessLifecycleKind
	Identity   Identity
	State      State
	Result     Result
	FinishedAt time.Time
}

// completionEvent is the payload completionNotifier.notify receives at a
// process's one-shot terminal transition. CommandID is always the
// manifest's pre-persisted LifecycleEventIDs.CommandID, never a freshly
// minted ID (mirrors harness/pkg/tool's ProcessCompletionNotification.CommandID).
type completionEvent struct {
	CommandID uuid.UUID
	Owner     Owner
	Handle    Handle
	State     State
	Result    Result
}

// entry is the supervisor's per-process registry record (supervisor.go's
// Supervisor.entries, keyed by Handle). It carries the process's immutable
// Identity, the caller-supplied dependencies Start received (lease,
// lifecycle sink, observation capability, storage ceiling, yield
// settings), the live tool.Process PreparedProcess.Start returned, and the
// exact quota reservation amounts (so release always reverses precisely
// what admission reserved -- see supervisor.go's reservation/reserveQuota/
// releaseQuota).
//
// Task 8B added the entry's output storage (buffer/spool) and its one
// lifetime-owning goroutine (run/drain, started by Supervisor.Start after a
// successful spawn). Task 8C adds terminal-state arbitration: terminalOnce
// guards the one-shot compare-and-set every path that can end this
// process's lifetime -- today only run's natural Wait() return, and
// starting in Task 9C/18 also an explicit stop request, a deadline
// timeout, and supervisor shutdown -- must go through
// (entry.terminalize), and manifests/notifications/releaseQuota are the
// dependencies that one-shot path needs to write the terminal manifest,
// notify completion, and release this process's quota reservation. Task 8D
// still owns retention/LRU bookkeeping and the observation-invalidation
// call sites.
type entry struct {
	identity Identity

	lease        Lease
	lifecycle    lifecycleSink
	observations observationInvalidator
	ceiling      StorageCeiling
	yield        YieldSettings

	// process is the live tool.Process PreparedProcess.Start returned.
	// run's drain goroutines and Wait call are its first consumers.
	process tool.Process

	// reservation is the exact quota amounts admission reserved for this
	// process (supervisor.go's reserveQuota). It is retained on the entry
	// so terminalize's release path (natural exit, and -- starting in Task
	// 9C -- stop/timeout/shutdown) can call releaseQuota with precisely
	// what was reserved, without re-deriving it from Config and risking
	// drift if Config changes after admission.
	reservation reservation

	// manifests, notifications, and releaseQuota are terminalize's
	// dependencies: reload/persist the terminal manifest, notify
	// completion, and reverse this entry's quota reservation. All three
	// are nil-tolerant (terminalize skips whichever step has a nil
	// dependency) so a test can construct a bare entry (as
	// TestEntryRunClosesDoneAfterDrainingBothStreams already does) without
	// wiring every dependency.
	manifests     manifestSaver
	notifications completionNotifier
	releaseQuota  func(reservation)

	// buffer is the in-memory rolling window (buffer.go) over this
	// process's combined stdout+stderr stream, sized from
	// reservation.memoryBytes.
	buffer *Buffer
	// spool is the durable disk retention window (spool.go) over the same
	// combined stream, sized from reservation.spoolBytes and opened
	// beneath the Supervisor's spoolRoot, keyed by this process's Handle.
	spool *Spool

	// appendMu serializes every Buffer/Spool append so concurrent stdout
	// and stderr reads can never interleave mid-chunk. This is the single
	// per-process append sequence the spec's "Output capture and storage"
	// section requires: "Writes use a single per-process append sequence
	// so cursor order is deterministic even when stdout and stderr are
	// read concurrently." Both stores are appended to, in the same order,
	// while appendMu is held, so Buffer and Spool always agree on append
	// order for a given process even though they are otherwise
	// independent stores.
	appendMu sync.Mutex

	// lifetimeCancel cancels the entry's own lifetime-scoped context (see
	// run's doc comment for why this is never the ctx a caller passed into
	// Supervisor.Start). 8B stores it purely so a later microtask (Stop,
	// coordinated shutdown -- Task 9C) can cancel the ongoing Wait/drain
	// without needing any new plumbing; nothing calls it yet.
	lifetimeCancel context.CancelFunc

	// terminalOnce guards terminalize's one-shot compare-and-set: whichever
	// caller's terminalize invocation sync.Once selects as the first is the
	// only one that computes and persists the terminal manifest, publishes
	// the completed/lost lifecycle event, notifies completion, releases the
	// quota reservation, and releases the lease -- see terminalize's doc
	// comment.
	terminalOnce sync.Once

	// done closes once run's Wait call has returned, both drain goroutines
	// have finished reading their stream to completion, and terminalize has
	// returned, so a caller (test, or a later microtask) can observe the
	// entry's complete terminal transition without polling.
	done chan struct{}
}

// run is the single goroutine an entry owns for the rest of its lifetime,
// matching the spec's "One entry goroutine owns wait, activity, and stream
// drain" (Task 8's combined-acceptance text). Supervisor.Start spawns run
// exactly once, immediately after registering the entry.
//
// ctx is the entry's own lifetime-scoped context -- Supervisor.Start builds
// it with context.WithCancel(context.Background()), deliberately NOT the
// ctx a caller passed into Supervisor.Start. Per the Harness
// PreparedProcess/Process contract (harness/pkg/tool/process.go):
// "The Start context governs setup through process handoff only. Once
// Start returns a Process, that process lives ... independently of the
// Start context." Cancelling a caller's original Start context after Start
// has already returned a Handle must never cancel this process's ongoing
// Wait or drain; using a separate background-derived context here is what
// makes that true.
//
// run drains Stdout and Stderr concurrently via two drain goroutines that
// both funnel through appendChunk's single per-process append sequence.
// This is harmless in PTY mode too: tool.Process's doc comment guarantees
// Stderr "remains non-nil but is closed and empty" for a PTY-mode process,
// so its drain goroutine simply observes EOF immediately and contributes no
// bytes -- no StreamMode branch is needed here.
//
// run does not yet drain any tool.ProcessActivitySource activity channel
// (the "activity" part of the spec language above): Task 8D is where the
// observationInvalidator gets its Invalidate method and the
// spawn/activity/completion call sites, including wiring an
// ProcessActivitySource's channel if the process implements one. 8B left
// that wiring out entirely rather than half-building it, and 8C does not
// add it either.
//
// Once every drain goroutine has finished and Wait has returned, run
// classifies the ProcessResult/error Wait returned into this package's
// State/Result domain and calls terminalize with it (classifyWaitOutcome,
// terminalize). run always calls terminalize with context.Background(),
// never ctx: ctx is this entry's own lifetime-scoped context, and a future
// caller that terminates the process by canceling it (e.g. a stop request
// cancelling lifetimeCancel to interrupt an in-flight Wait) must not also
// cause the resulting terminal manifest Save, lifecycle publish, and
// completion notify to be skipped or fail because the same context is
// already canceled by the time run reaches them.
func (e *entry) run(ctx context.Context) {
	var drainWG sync.WaitGroup
	drainWG.Add(2)
	go e.drain(&drainWG, e.process.Stdout())
	go e.drain(&drainWG, e.process.Stderr())

	result, waitErr := e.process.Wait(ctx)

	drainWG.Wait()

	state, outcome := classifyWaitOutcome(result, waitErr)
	e.terminalize(context.Background(), state, outcome, time.Now().UTC())

	close(e.done)
}

// drain reads r to EOF in bounded chunks, appending every non-empty chunk
// to the entry's Buffer and Spool through appendChunk. It never returns an
// error to its caller: a read or append failure only stops that one
// stream's draining early (the other stream and Wait are unaffected). This
// mirrors the spec's "a process is never terminated for producing too much
// output" -- a read or append failure here is similarly non-fatal to the
// process; it is simply the point at which that one stream stops
// contributing further bytes.
func (e *entry) drain(wg *sync.WaitGroup, r io.ReadCloser) {
	defer wg.Done()
	defer r.Close()

	buf := make([]byte, drainChunkBytes)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			e.appendChunk(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// appendChunk appends chunk to both the in-memory Buffer and the durable
// Spool while holding appendMu, so a chunk read from Stdout and a chunk
// read from Stderr can never interleave mid-write; see appendMu's doc
// comment for why this is the entry's single per-process append sequence.
// Append errors from either store are swallowed here for the same reason
// drain's are: 8B's drain path must never abort or block the process over
// an output-storage failure. Surfacing a persistent Spool append failure
// (e.g. disk full) more visibly is out of 8B's scope.
func (e *entry) appendChunk(chunk []byte) {
	e.appendMu.Lock()
	defer e.appendMu.Unlock()
	_, _ = e.buffer.Append(chunk)
	_, _ = e.spool.Append(chunk)
}

// terminalize is the one-shot compare-and-set terminal arbiter every path
// that can end this process's lifetime must call: today only run's natural
// Wait() return (this file's only caller), and -- starting in Task 9C/18 --
// also an explicit stop request, a deadline timeout, and supervisor
// shutdown, none of which exist yet. terminalOnce (sync.Once) guarantees
// that whichever concurrent caller's invocation is selected first is the
// only one whose state/result/finishedAt actually take effect: it is the
// only call that computes and persists the terminal manifest, publishes the
// completed/lost lifecycle event, notifies completion, releases the quota
// reservation, and releases the lease. Every other concurrent (or later)
// caller's terminalize call returns having done nothing -- its own
// state/result/finishedAt are silently discarded -- which is exactly the
// combined-acceptance text's "one terminal manifest, one completion
// callback, one lease release" for a process that could otherwise reach its
// terminal state via more than one racing path at once.
//
// ctx is used only for the lifecycle-publish and completion-notify calls
// doTerminalize makes; see run's doc comment for why run always passes
// context.Background() rather than its own (possibly already-canceled)
// lifetime context.
func (e *entry) terminalize(ctx context.Context, state State, result Result, finishedAt time.Time) {
	e.terminalOnce.Do(func() {
		e.doTerminalize(ctx, state, result, finishedAt)
	})
}

// doTerminalize is terminalize's guarded body; terminalOnce.Do ensures it
// runs at most once per entry. It reloads the manifest Start already
// durably persisted (rather than reconstructing one from scratch) so the
// terminal Save carries forward every already-persisted field unchanged --
// most importantly Events, the manifest's stable LifecycleEventIDs
// (manifest.go): this is what makes the completed/lost lifecycle event and
// the completion notification reuse the exact pre-persisted EventID/
// CommandID values rather than minting fresh ones, on this call and on any
// hypothetical future retry of the same publish step (spec "Manifests and
// durability": "stable lifecycle EventIDs ... allocated and persisted
// before publication"). Every dependency is nil-tolerant: a bare entry
// built directly by a test (no manifests/lifecycle/notifications/
// releaseQuota/lease wired) still terminalizes cleanly, simply skipping
// whichever step has no dependency to call.
func (e *entry) doTerminalize(ctx context.Context, state State, result Result, finishedAt time.Time) {
	var current Manifest
	haveManifest := false
	if e.manifests != nil {
		if loaded, err := e.manifests.Load(e.identity.Handle); err == nil {
			current = loaded
			haveManifest = true
		}
	}

	if haveManifest {
		current.State = state
		current.FinishedAt = &finishedAt
		current.Result = result
		current.Cursors = SpoolCursors{
			TotalBytes:   e.spool.TotalBytes(),
			RetainedFrom: e.spool.RetainedFrom(),
		}
		current.CompletionPublished++
		_ = e.manifests.Save(current)
	}

	kind := tool.ProcessLifecycleCompleted
	eventID := current.Events.Completed
	if state == StateLostOnRestore {
		kind = tool.ProcessLifecycleLost
		eventID = current.Events.Lost
	}

	if e.lifecycle != nil {
		_ = e.lifecycle.publish(ctx, lifecycleTerminalEvent{
			EventID:    eventID,
			Kind:       kind,
			Identity:   e.identity,
			State:      state,
			Result:     result,
			FinishedAt: finishedAt,
		})
	}

	if e.notifications != nil {
		_ = e.notifications.notify(ctx, completionEvent{
			CommandID: current.Events.CommandID,
			Owner:     e.identity.Owner,
			Handle:    e.identity.Handle,
			State:     state,
			Result:    result,
		})
	}

	if e.releaseQuota != nil {
		e.releaseQuota(e.reservation)
	}
	if e.lease != nil {
		_ = e.lease.Release()
	}
}

// classifyWaitOutcome maps the tool.ProcessResult/error pair
// tool.Process.Wait returns into this package's terminal State and Result,
// which terminalize's manifest write requires. A non-nil waitErr -- the
// runner itself failed to observe the process's own exit, as distinct from
// the process exiting on its own -- has no dedicated ProcessTerminalReason
// and conservatively maps to StateFailed with Result.Reason "failed",
// mirroring reasonString's own fallback for any ProcessTerminalReason this
// package's closed State domain has no direct counterpart for.
func classifyWaitOutcome(res tool.ProcessResult, waitErr error) (State, Result) {
	if waitErr != nil {
		return StateFailed, Result{Reason: "failed"}
	}
	state := terminalStateForReason(res.Reason)
	result := Result{Reason: reasonString(res.Reason)}
	if state == StateExited {
		code := res.ExitCode
		result.ExitCode = &code
	}
	return state, result
}

// terminalStateForReason maps a harness tool.ProcessTerminalReason to this
// package's closed State domain (state.go). tool.ProcessTerminalRunnerShutdown
// and tool.ProcessTerminalOutputLimit have no state of their own in the
// harness lifecycle tuple table either (both pair with either Terminated or
// Killed depending on which signal was actually used) and conservatively
// map to StateTerminated here; neither reason is reachable through 8C's
// only terminalize caller (run's natural Wait() return), so this mapping
// exists for completeness rather than current exercise. An unrecognized
// reason (including the zero value) conservatively maps to StateFailed.
func terminalStateForReason(reason tool.ProcessTerminalReason) State {
	switch reason {
	case tool.ProcessTerminalExited:
		return StateExited
	case tool.ProcessTerminalTimedOut:
		return StateTimedOut
	case tool.ProcessTerminalInterrupted:
		return StateInterrupted
	case tool.ProcessTerminalTerminated, tool.ProcessTerminalRunnerShutdown, tool.ProcessTerminalOutputLimit:
		return StateTerminated
	case tool.ProcessTerminalKilled:
		return StateKilled
	case tool.ProcessTerminalLostOnRestore:
		return StateLostOnRestore
	default:
		return StateFailed
	}
}

// reasonString maps a harness tool.ProcessTerminalReason to Manifest.Result's
// documented closed set of reason strings (manifest.go's Result.Reason doc
// comment: "exited", "failed", "timed-out", "interrupted", "terminated",
// "killed", "lost-on-restore"). An unrecognized reason (including the zero
// value) conservatively maps to "failed".
func reasonString(reason tool.ProcessTerminalReason) string {
	switch reason {
	case tool.ProcessTerminalExited:
		return "exited"
	case tool.ProcessTerminalTimedOut:
		return "timed-out"
	case tool.ProcessTerminalInterrupted:
		return "interrupted"
	case tool.ProcessTerminalTerminated, tool.ProcessTerminalRunnerShutdown, tool.ProcessTerminalOutputLimit:
		return "terminated"
	case tool.ProcessTerminalKilled:
		return "killed"
	case tool.ProcessTerminalLostOnRestore:
		return "lost-on-restore"
	default:
		return "failed"
	}
}

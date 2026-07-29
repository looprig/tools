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
// adds the observation-invalidation call sites (run calls
// invalidateObservations at spawn, at each received tool.ProcessActivity,
// and at completion) and onTerminal, the Supervisor's terminal-LRU
// retention hook.
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

	// onTerminal is the Supervisor's terminal-LRU retention-bookkeeping hook
	// (Task 8D, supervisor.go's recordTerminal). entry has no reference to
	// the Supervisor's registry, so doTerminalize calls this once -- inside
	// terminalOnce's guarded body, alongside releaseQuota/lifecycle/
	// notifications -- to let the Supervisor record this entry as newly
	// terminal for its owning session and evict that session's
	// least-recently-used terminal entry if the retention limit
	// (Config.MaxRetainedCompletedProcessesPerSession) is now exceeded.
	// Nil-tolerant, like every other terminalize dependency, so a bare
	// test-built entry (no Supervisor) still terminalizes cleanly.
	onTerminal func(Handle, Owner)

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

	// waitMu guards generation and wake, the append-driven wake mechanism
	// Task 9A's wait.go builds on (bumpGeneration, generationSnapshot). It
	// is a separate lock from appendMu -- disjoint from the Buffer/Spool
	// append sequence -- so a blocked waiter's read of (generation, wake)
	// never has to contend with, or wait behind, an in-flight append.
	waitMu sync.Mutex
	// generation counts how many times appendChunk has appended a
	// non-empty chunk to this entry, starting at zero. wait.go's waiters
	// compare a caller-supplied "last observed" generation against the
	// current value to decide whether new output has arrived since they
	// last looked.
	generation uint64
	// wake is closed, then immediately replaced with a fresh channel, by
	// every bumpGeneration call (the classic Go "close to broadcast every
	// current waiter, replace to re-arm for the next one" pattern). A
	// waiter that captures (generation, wake) together, atomically, under
	// waitMu (generationSnapshot) can safely decide "already advanced" vs.
	// "block on wake" with no lost-wakeup window: any bump after the
	// snapshot is read is guaranteed to close exactly the channel that
	// snapshot returned. A process's terminal transition is deliberately
	// not wired through this same mechanism -- see wait.go, which selects
	// on the entry's existing done channel (closed exactly once, by run,
	// after terminalize returns) as the independent "or the entry
	// terminalizes" wake condition instead.
	wake chan struct{}

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

	// exited closes inside doTerminalize as soon as this process's terminal
	// manifest is durably persisted (or, for a dependency-free entry, as
	// soon as that attempt has been made) -- strictly before the lifecycle
	// publish and completion notify calls that follow it. Task 9C:
	// Supervisor.Shutdown's "confirm every tree has exited" step waits on
	// this channel, deliberately not on done, so a slow or backpressured
	// lifecycleSink.publish/completionNotifier.notify call can never delay
	// Shutdown's own completion (combined-acceptance: "notification
	// backpressure cannot block terminalization"). Every other caller
	// (wait.go's Wait, via the entry's done/generation wake mechanism) is
	// unaffected and continues to depend on done exactly as it did before
	// 9C: it needs the fully completed terminal transition, not merely
	// confirmed-exited.
	//
	// Supervisor.Start populates this field for every entry it registers;
	// restore.go's reopenEntry sets it too (reusing its already-closed done
	// channel, since a restored entry is terminal by construction), purely
	// for hygiene. Every entry Shutdown ever actually waits on comes from
	// Start -- Restore's reopened entries are already terminal (done
	// pre-closed) and are filtered out of Shutdown's snapshot before exited
	// is ever touched. This field is nil-tolerant like every other entry
	// dependency (see bumpGeneration's identical nil-guard on wake) so a
	// bare test-built entry that predates 9C still terminalizes cleanly
	// without it.
	exited chan struct{}
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
// run also drains a tool.ProcessActivitySource activity channel, if the
// live tool.Process optionally implements one (drainActivity), invalidating
// this entry's bound observation cache once per received tool.ProcessActivity.
// run additionally invalidates observations once at the very top (spawn)
// and once more after every drain goroutine -- stdout, stderr, and
// activity, if any -- has finished and Wait has returned (completion),
// matching the spec's "Workspace coordination" section: "Observation caches
// are invalidated at process spawn, on reported filesystem activity ...,
// and at process completion."
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
// already canceled by the time run reaches them. The completion
// invalidateObservations call is made with context.Background() for the
// same reason.
func (e *entry) run(ctx context.Context) {
	e.invalidateObservations(ctx)

	var drainWG sync.WaitGroup
	drainWG.Add(2)
	go e.drain(&drainWG, e.process.Stdout())
	go e.drain(&drainWG, e.process.Stderr())

	// Activities is an optional capability: only start a third drain
	// goroutine when the live tool.Process actually implements
	// tool.ProcessActivitySource and returns a non-nil channel. Every
	// drainWG.Add call happens here, strictly before Wait is called below,
	// so there is no race with drainWG.Wait().
	if source, ok := e.process.(tool.ProcessActivitySource); ok {
		if activities := source.Activities(); activities != nil {
			drainWG.Add(1)
			go e.drainActivity(ctx, &drainWG, activities)
		}
	}

	result, waitErr := e.process.Wait(ctx)

	drainWG.Wait()

	e.invalidateObservations(context.Background())

	state, outcome := classifyWaitOutcome(result, waitErr)
	e.terminalize(context.Background(), state, outcome, time.Now().UTC())

	close(e.done)
}

// invalidateObservations calls e.observations.invalidate for this entry's
// Handle, tolerating a nil observations dependency exactly like every other
// entry dependency (manifests, lifecycle, notifications, releaseQuota,
// lease, onTerminal). See run's doc comment for the three points in a
// process's lifetime it is called from.
func (e *entry) invalidateObservations(ctx context.Context) {
	if e.observations == nil {
		return
	}
	_ = e.observations.invalidate(ctx, e.identity.Handle)
}

// drainActivity ranges over activities, calling invalidateObservations once
// per received tool.ProcessActivity, until activities closes. Per the
// Harness contract (tool.ProcessActivitySource's doc comment): "The
// activity channel must close before Process.Wait returns" -- so this
// goroutine always finishes on its own before run's Wait call above
// returns, and run's drainWG.Wait() call waits for it exactly like it waits
// for the stdout/stderr drain goroutines. If activities closes having never
// sent anything, `for range` returns immediately: this goroutine never
// blocks forever on a channel that only closes.
func (e *entry) drainActivity(ctx context.Context, wg *sync.WaitGroup, activities <-chan tool.ProcessActivity) {
	defer wg.Done()
	for range activities {
		e.invalidateObservations(ctx)
	}
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
//
// A non-empty chunk also bumps this entry's generation (Task 9A), waking
// every waiter currently blocked in wait.go's blocking Wait modes. The
// bump happens after appendMu is released: it uses its own lock (waitMu),
// and there is no ordering requirement between releasing appendMu and
// bumping generation beyond "after this chunk is durably appended", which
// is already true once appendMu.Unlock returns.
func (e *entry) appendChunk(chunk []byte) {
	if len(chunk) == 0 {
		return
	}

	e.appendMu.Lock()
	_, _ = e.buffer.Append(chunk)
	_, _ = e.spool.Append(chunk)
	e.appendMu.Unlock()

	e.bumpGeneration()
}

// bumpGeneration advances the entry's generation counter by one and wakes
// every waiter currently blocked on the previous generation's wake
// channel, by closing it and replacing it with a fresh one -- see wake's
// doc comment for the exact "close to broadcast, replace to re-arm"
// pattern and why it is race-safe with generationSnapshot. wake may still
// be nil here for an entry built directly by an older test fixture that
// predates Task 9A (entry_test.go's TestEntryRunClosesDoneAfterDrainingBothStreams,
// newRaceEntry); closing a nil channel would panic, so the first bump for
// such an entry only allocates wake, skipping the close (there is nothing
// listening on a channel that was never handed out).
func (e *entry) bumpGeneration() {
	e.waitMu.Lock()
	e.generation++
	if e.wake != nil {
		close(e.wake)
	}
	e.wake = make(chan struct{})
	e.waitMu.Unlock()
}

// generationSnapshot atomically returns this entry's current generation
// together with the wake channel that will close on the very next
// bumpGeneration call. Reading both values together, under the same lock
// acquisition, is what lets a caller compare generation against its own
// last-observed value and then select on wake with no lost-wakeup window
// (see wake's doc comment).
func (e *entry) generationSnapshot() (uint64, <-chan struct{}) {
	e.waitMu.Lock()
	defer e.waitMu.Unlock()
	return e.generation, e.wake
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

	// exited closes here -- see its doc comment for why this must happen
	// strictly before the lifecycle publish/completion notify calls below,
	// rather than after them (which is where done closes, in run, once this
	// whole method returns).
	if e.exited != nil {
		close(e.exited)
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
	if e.onTerminal != nil {
		e.onTerminal(e.identity.Handle, e.identity.Owner)
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

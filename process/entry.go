package process

import (
	"context"
	"io"
	"sync"

	"github.com/looprig/harness/pkg/tool"
)

// drainChunkBytes bounds each single Read call inside drain. It is large
// enough that ordinary test fixtures and small real command output land in
// one Append per Read, while still bounding worst-case per-call memory for
// a genuinely large single read.
const drainChunkBytes = 32 << 10 // 32 KiB

// entry is the supervisor's per-process registry record (supervisor.go's
// Supervisor.entries, keyed by Handle). It carries the process's immutable
// Identity, the caller-supplied dependencies Start received (lease,
// lifecycle sink, observation capability, storage ceiling, yield
// settings), the live tool.Process PreparedProcess.Start returned, and the
// exact quota reservation amounts (so release always reverses precisely
// what admission reserved -- see supervisor.go's reservation/reserveQuota/
// releaseQuota).
//
// Task 8B adds the entry's output storage (buffer/spool) and its one
// lifetime-owning goroutine (run/drain, started by Supervisor.Start after a
// successful spawn). entry still does not own a terminal-arbitration
// compare-and-set, stable lifecycle EventID bookkeeping, or retention
// bookkeeping -- Task 8C adds terminal-state arbitration on top of run's
// current minimal stub (see run's doc comment), and Task 8D adds
// retention/LRU and the observation-invalidation call sites.
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
	// so a later microtask's release path (natural exit, stop, shutdown)
	// can call releaseQuota with precisely what was reserved, without
	// re-deriving it from Config and risking drift if Config changes after
	// admission.
	reservation reservation

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
	// coordinated shutdown -- Task 8C/9) can cancel the ongoing Wait/drain
	// without needing any new plumbing; nothing calls it yet in 8B.
	lifetimeCancel context.CancelFunc

	// done closes once run's Wait call has returned and both drain
	// goroutines have finished reading their stream to completion, so a
	// caller (test, or a later microtask) can observe drain completion
	// without polling. Closing done is 8B's entire terminal-handling
	// story; Task 8C replaces the code right before the close with actual
	// terminal-state arbitration (compare-and-set, stable lifecycle
	// EventIDs, completion notification, lease release).
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
// ProcessActivitySource's channel if the process implements one. 8B leaves
// that wiring out entirely rather than half-building it.
//
// run also does not yet arbitrate a terminal manifest state,
// compare-and-set anything, publish a lifecycle event, notify completion,
// release the quota reservation, or release the lease -- Task 8C owns all
// of that. Once every drain goroutine has finished and Wait has returned,
// 8B's run only closes e.done as a minimal stub marking "this entry's
// wait/drain work is finished."
func (e *entry) run(ctx context.Context) {
	var drainWG sync.WaitGroup
	drainWG.Add(2)
	go e.drain(&drainWG, e.process.Stdout())
	go e.drain(&drainWG, e.process.Stderr())

	// Task 8C: consume the ProcessResult/err Wait returns to drive
	// terminal arbitration. 8B only waits so run's goroutine matches its
	// documented "owns wait" responsibility structurally; it does not yet
	// act on the result.
	_, _ = e.process.Wait(ctx)

	drainWG.Wait()
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

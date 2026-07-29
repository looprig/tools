package process

import "github.com/looprig/harness/pkg/tool"

// entry is the supervisor's per-process registry record (supervisor.go's
// Supervisor.entries, keyed by Handle). Task 8A only needs enough of it to
// track what one admission attempt must be able to roll back and what a
// later microtask will need to find again: the process's immutable
// Identity, the caller-supplied dependencies Start received (lease,
// lifecycle sink, observation capability, storage ceiling, yield
// settings), the live tool.Process PreparedProcess.Start returned, and the
// exact quota reservation amounts (so release always reverses precisely
// what admission reserved -- see supervisor.go's reservation/reserveQuota/
// releaseQuota).
//
// entry deliberately does not yet own a wait/activity/drain goroutine, a
// State (state.go), a Manifest (manifest.go), or any terminal-arbitration
// bookkeeping (a compare-and-set outcome, stable lifecycle EventIDs). Task
// 8B adds the goroutine and durable manifest handoff, Task 8C adds
// terminal-state arbitration, and Task 8D adds retention/LRU and
// observation-invalidation call sites. Keeping entry a small struct now,
// rather than guessing at the eventual goroutine/channel shape those
// microtasks need, avoids committing 8A to an internal design later
// microtasks would just have to unwind.
type entry struct {
	identity Identity

	lease        Lease
	lifecycle    lifecycleSink
	observations observationInvalidator
	ceiling      StorageCeiling
	yield        YieldSettings

	// process is the live tool.Process PreparedProcess.Start returned.
	// Nothing reads it yet in 8A; 8B's wait/drain goroutine is the first
	// consumer.
	process tool.Process

	// reservation is the exact quota amounts admission reserved for this
	// process (supervisor.go's reserveQuota). It is retained on the entry
	// so a later microtask's release path (natural exit, stop, shutdown)
	// can call releaseQuota with precisely what was reserved, without
	// re-deriving it from Config and risking drift if Config changes after
	// admission.
	reservation reservation
}

package process

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
)

// Lease is Task 8's minimal placeholder for the Harness lifetime workspace
// lease a caller acquires before calling Start (spec "Workspace
// coordination": "Tools acquires the matching Harness lifetime workspace
// lease" between PrepareProcess and Start). Task 8 does not wire Harness
// end-to-end and does not import any concrete Harness lease type; Start only
// needs "the resource the caller already acquired that must be released on
// every exit path". The method is named Release, not Close, so a call site
// reads unambiguously against PreparedProcess.Close (which releases the
// preparation, not the lease) -- Task 15/19 replaces this interface with (or
// adapts it to) the real Harness lease type.
type Lease interface {
	Release() error
}

// StorageCeiling bounds one supervised process's in-memory rolling window
// and disk spool retention window (mirrors Config's
// MaxProcessInMemoryBytes/MaxProcessSpoolBytes pair, and buffer.go's
// NewBuffer / spool.go's OpenSpool ceiling parameters). It is supplied per
// Start call rather than read from the Supervisor's Config, so a caller
// (Bash) can request a smaller window for one process; a non-positive field
// falls back to the Supervisor's configured per-process default
// (reserveQuota). Reserving InMemoryBytes/SpoolBytes against the
// Supervisor's aggregate ceilings before Start is this task's realization of
// the combined-acceptance text's "memory" and "spool" quotas.
type StorageCeiling struct {
	InMemoryBytes int64
	SpoolBytes    int64
}

// YieldSettings carries one Start call's explicit backgrounding preference
// (spec "Bash API": a foreground call may explicitly yield to the
// background rather than staying attached and returning inline). Task 8
// does not implement yield/backgrounded-handoff behavior yet -- that is
// part of Task 8B/8C's lifecycle-event work and, ultimately, the Bash tool
// built in Phase 4; this struct only reserves the field Start's signature
// commits to.
type YieldSettings struct {
	// Yield requests that the process detach to the background immediately
	// rather than staying attached to the foreground Bash call.
	Yield bool
}

// lifecycleSink is Task 8's minimal placeholder for the narrow lifecycle-
// publication capability the spec's "Supervisor lifetime" section describes
// as half of Harness's SessionResourceServices ("stable lifecycle EventIDs
// ... allocated and persisted before the corresponding Harness lifecycle
// event ... may be published"). It intentionally carries no methods yet:
// 8A only needs to accept and store an implementation (both at construction
// and, per admission, from Start -- see NewSupervisor and Start's doc
// comments for why both exist), never to call one. Task 8C adds the methods
// needed to publish started/backgrounded/completed/lost records using a
// manifest's pre-persisted stable EventIDs (manifest.go's
// LifecycleEventIDs).
type lifecycleSink interface{}

// completionNotifier is Task 8's minimal placeholder for the other half of
// SessionResourceServices: the bounded completion notification submitted to
// the owning loop (spec "Supervisor lifetime"; mirrors harness/pkg/tool's
// ProcessCompletionNotification). Like lifecycleSink, it carries no methods
// yet in 8A; Task 8C/8D add them alongside the terminal-arbiter and
// retention work that actually needs to notify.
type completionNotifier interface{}

// observationInvalidator is Task 8's minimal placeholder for the
// originating loop's observation-cache invalidation capability (spec
// "Workspace coordination": "Observation caches are invalidated at process
// spawn, on reported filesystem activity ..., and at process completion").
// 8A accepts and stores one per admission but never calls it; Task 8D adds
// the Invalidate method and wires the spawn/activity/completion call
// sites.
type observationInvalidator interface{}

// reservation records the exact quota amounts one admission attempt
// reserved (reserveQuota), so releaseQuota always reverses precisely what
// was reserved -- independent of which Config default filled in a
// non-positive StorageCeiling field, and independent of any later Config
// change.
type reservation struct {
	loopID      uuid.UUID
	sessionID   uuid.UUID
	memoryBytes int64
	spoolBytes  int64
}

// Supervisor is the runner-free process supervisor described by the spec's
// "Supervisor lifetime" and "Workspace coordination" sections: it never
// calls tool.AsyncProcessRunner.PrepareProcess itself, and NewSupervisor
// deliberately does not accept a tool.AsyncProcessRunner at all. A caller
// (Task 8 does not implement this caller; it is the Bash tool built in
// Phase 4, per the spec's "Only Bash owns execution authority") resolves a
// runner, calls PrepareProcess, acquires the matching workspace Lease, and
// hands the resulting tool.PreparedProcess to Start. Supervisor then owns
// admission -- reserving quotas and consuming the single-use preparation
// (this microtask, 8A) -- and, starting in Task 8B, durable handoff, stream
// drain, terminal arbitration, and retention.
//
// A Supervisor is safe for concurrent use by multiple goroutines.
type Supervisor struct {
	cfg       Config
	manifests *ManifestStore
	// spoolRoot is the private resource root every entry's Spool is opened
	// beneath (spool.go's OpenSpool root parameter). Start opens one Spool
	// per admitted process here, keyed by that process's Handle.
	spoolRoot string

	// lifecycle and notifications are the Supervisor's session-scoped
	// SessionResourceServices capabilities, accepted at construction per
	// the combined-acceptance text ("its factory accepts lifecycle ...
	// notification ... dependencies"). Start also accepts a lifecycleSink
	// per call (recorded on that process's entry): the per-call sink is
	// what a later microtask actually publishes a given process's events
	// through (mirroring how "each admission captures the originating
	// loop's observation capability" in the spec), while these
	// construction-time fields are the Supervisor's own session-wide
	// service handles for supervisor-level events. Neither is called
	// anywhere in 8A; both are empty interfaces (see their doc comments)
	// until a later microtask adds methods.
	lifecycle     lifecycleSink
	notifications completionNotifier

	mu sync.Mutex

	// runningByLoop and runningBySession track the per-loop/per-session
	// running-process counts reserveQuota enforces against
	// cfg.MaxRunningProcessesPerLoop/MaxRunningProcessesPerSession. A
	// missing key means zero; reserveQuota/releaseQuota keep both maps
	// exact mirror images of every currently-reserved entry.
	runningByLoop    map[uuid.UUID]int
	runningBySession map[uuid.UUID]int
	// reservedMemoryBytes and reservedSpoolBytes are the running totals
	// reserveQuota enforces against cfg.MaxAggregateInMemoryBytes/
	// MaxAggregateSpoolBytes.
	reservedMemoryBytes int64
	reservedSpoolBytes  int64

	// entries is the registry of admitted processes, keyed by their
	// Handle. 8A only ever adds to it (Start, on success); nothing removes
	// an entry yet -- that starts once a process can reach a terminal
	// state and be released/retained/evicted (Task 8B/8C/8D).
	entries map[Handle]*entry
}

// NewSupervisor returns a Supervisor governed by cfg (normalized; see
// Config.Normalize), persisting manifests to manifests and, from Task 8B
// on, opening spools beneath spoolRoot. lifecycle and notifications are the
// two narrow SessionResourceServices capabilities described by the spec's
// "Supervisor lifetime" section; see the Supervisor.lifecycle/notifications
// field doc for why they are accepted here even though 8A never calls
// either. NewSupervisor deliberately does not accept a
// tool.AsyncProcessRunner: the supervisor is runner-free by construction,
// unable to call PrepareProcess even if some future caller wanted it to.
func NewSupervisor(cfg Config, manifests *ManifestStore, spoolRoot string, lifecycle lifecycleSink, notifications completionNotifier) (*Supervisor, error) {
	normalized, err := cfg.Normalize()
	if err != nil {
		return nil, err
	}
	if manifests == nil {
		return nil, Wrap(CodeInvalidSettings, errors.New("manifest store is required"))
	}
	return &Supervisor{
		cfg:              normalized,
		manifests:        manifests,
		spoolRoot:        spoolRoot,
		lifecycle:        lifecycle,
		notifications:    notifications,
		runningByLoop:    make(map[uuid.UUID]int),
		runningBySession: make(map[uuid.UUID]int),
		entries:          make(map[Handle]*entry),
	}, nil
}

// quotaExceeded builds the typed, model-facing quota-rejection error
// (spec "Stable errors"): reason is retained only as Cause for trusted
// diagnostics (logs, tests) and is never itself model-facing -- callers
// must render only Code (errors.go's doc comment).
func quotaExceeded(reason string) error {
	return Wrap(CodeProcessQuotaExceeded, errors.New(reason))
}

// reserveQuota reserves, in one atomic step, every quota the spec's
// "Quotas and retention" section requires before a process may be spawned:
// the owning loop's and session's running-process counts, and this
// process's share of the aggregate in-memory and spool byte ceilings
// (ceiling's fields, defaulted from cfg.MaxProcess{InMemory,Spool}Bytes
// when non-positive). All four checks are evaluated before any counter is
// mutated, so a rejected admission leaves every counter exactly as it was
// -- nothing to roll back (TestSupervisorRejectsSessionAndLoopQuota) --
// and Start must never call PreparedProcess.Start until this returns
// successfully (TestSupervisorReservesQuotaBeforeStart).
func (s *Supervisor) reserveQuota(owner Owner, ceiling StorageCeiling) (reservation, error) {
	memoryBytes := ceiling.InMemoryBytes
	if memoryBytes <= 0 {
		memoryBytes = s.cfg.MaxProcessInMemoryBytes
	}
	spoolBytes := ceiling.SpoolBytes
	if spoolBytes <= 0 {
		spoolBytes = s.cfg.MaxProcessSpoolBytes
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	switch {
	case s.runningByLoop[owner.LoopID] >= s.cfg.MaxRunningProcessesPerLoop:
		return reservation{}, quotaExceeded("max running processes per loop reached")
	case s.runningBySession[owner.SessionID] >= s.cfg.MaxRunningProcessesPerSession:
		return reservation{}, quotaExceeded("max running processes per session reached")
	case s.reservedMemoryBytes+memoryBytes > s.cfg.MaxAggregateInMemoryBytes:
		return reservation{}, quotaExceeded("max aggregate in-memory bytes reached")
	case s.reservedSpoolBytes+spoolBytes > s.cfg.MaxAggregateSpoolBytes:
		return reservation{}, quotaExceeded("max aggregate spool bytes reached")
	}

	s.runningByLoop[owner.LoopID]++
	s.runningBySession[owner.SessionID]++
	s.reservedMemoryBytes += memoryBytes
	s.reservedSpoolBytes += spoolBytes

	return reservation{
		loopID:      owner.LoopID,
		sessionID:   owner.SessionID,
		memoryBytes: memoryBytes,
		spoolBytes:  spoolBytes,
	}, nil
}

// releaseQuota exactly reverses a reservation reserveQuota returned. Start
// only ever calls it with a reservation reserveQuota actually produced (a
// failed reserveQuota call has nothing to release), so it does not need to
// guard against an unreserved zero value.
func (s *Supervisor) releaseQuota(r reservation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runningByLoop[r.loopID] > 0 {
		s.runningByLoop[r.loopID]--
	}
	if s.runningBySession[r.sessionID] > 0 {
		s.runningBySession[r.sessionID]--
	}
	s.reservedMemoryBytes -= r.memoryBytes
	s.reservedSpoolBytes -= r.spoolBytes
}

// releaseLease releases lease, tolerating a nil lease (a caller that has no
// lease to release, e.g. a read-only or test scenario).
func (s *Supervisor) releaseLease(lease Lease) {
	if lease == nil {
		return
	}
	_ = lease.Release()
}

// accessMode maps a Harness WorkspaceAccessKind (tool.WorkspaceAccess,
// PreparedProcess.EffectiveWorkspaceAccess) to this package's manifest-level
// AccessMode (manifest.go). Both are closed, three-value domains in the same
// read-only/scoped-write/broad-write order. An unrecognized kind -- which
// tool.WorkspaceAccessKind.Valid() should never let through, but this
// function does not assume that -- conservatively maps to the narrowest,
// AccessReadOnly, rather than producing an invalid AccessMode that would
// fail Manifest.Validate.
func accessMode(kind tool.WorkspaceAccessKind) AccessMode {
	switch kind {
	case tool.WorkspaceAccessScopedWrite:
		return AccessScopedWrite
	case tool.WorkspaceAccessBroadWrite:
		return AccessBroadWrite
	default:
		return AccessReadOnly
	}
}

// handleExists reports whether h already names an entry in the registry; it
// is NewHandle's collision check (identity.go's HandleExists), so a minted
// Handle can never collide with one already admitted.
func (s *Supervisor) handleExists(h Handle) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.entries[h]
	return ok
}

// Start begins supervising one already-prepared process. The caller has
// already resolved an AsyncProcessRunner, called PrepareProcess, and
// acquired the matching workspace lease (spec "Workspace coordination");
// Start only ever consumes what it is handed -- it never calls
// PrepareProcess itself. owner and origin are recorded verbatim as the new
// process's Identity (identity.go); prepared is the single-use preparation
// this call consumes; lease is released on every path that does not end
// with a registered entry owning it; sink, observations, ceiling, and yield
// are recorded on the entry for later microtasks to use.
//
// Start reserves every quota reserveQuota enforces before calling
// prepared.Start (TestSupervisorReservesQuotaBeforeStart), at most once,
// exactly matching PreparedProcess.Start's own single-use contract. It then
// atomically persists this process's manifest in state StateStarting --
// via s.manifests, a real durable ManifestStore -- before ever calling
// prepared.Start, so a Handle can never be returned for a process that has
// no durable record (TestSupervisorPersistsBeforeReturningHandle; spec
// "Manifests and durability": "Before returning a process handle, Tools
// atomically persists a manifest"). This requires minting the Handle (and
// therefore checking handleExists) before prepared.Start is called, not
// after, unlike a Handle-agnostic admission flow would.
//
// If prepared.Start fails, Start releases every reservation it made,
// releases lease, and closes prepared -- idempotent per the Harness
// PreparedProcess contract -- before returning a *Error wrapping
// CodeSpawnFailed (TestSupervisorStartFailureReleasesQuota). The
// already-persisted StateStarting manifest is deliberately left as-is on
// this path: transitioning it to a terminal state is Task 8C's
// terminal-arbiter compare-and-set, not this microtask's. If a quota is
// already exhausted, Start returns a *Error wrapping
// CodeProcessQuotaExceeded without ever calling prepared.Start or
// prepared.Close, and without minting a Handle or writing any manifest
// (TestSupervisorRejectsSessionAndLoopQuota).
//
// Once prepared.Start succeeds, Start persists the manifest's StateRunning
// transition, opens this process's in-memory Buffer and durable Spool
// (both sized from the exact quota reservation -- see reservation's doc
// comment), registers the entry, and starts the entry's single
// wait/activity/drain goroutine (entry.go's run) on a lifetime-scoped
// context that is deliberately independent of ctx (see run's doc comment
// for why). Start returns the freshly minted Handle only after the entry
// is registered and its goroutine has been started
// (TestSupervisorDrainsOrderedStreams, TestSupervisorSpoolCeilingDropsOldest).
// Start does not yet arbitrate a terminal state (Task 8C).
func (s *Supervisor) Start(
	ctx context.Context,
	owner Owner,
	origin Origin,
	prepared tool.PreparedProcess,
	lease Lease,
	sink lifecycleSink,
	observations observationInvalidator,
	ceiling StorageCeiling,
	yield YieldSettings,
) (Handle, error) {
	if prepared == nil {
		return "", Wrap(CodeInvalidArguments, errors.New("prepared process is required"))
	}

	res, err := s.reserveQuota(owner, ceiling)
	if err != nil {
		return "", err
	}

	handle, err := NewHandle(s.handleExists)
	if err != nil {
		s.releaseQuota(res)
		s.releaseLease(lease)
		_ = prepared.Close()
		return "", err
	}

	identity := Identity{Handle: handle, Owner: owner, Origin: origin}

	// CommandMetadata is left at its zero value: Start's signature (fixed
	// by Task 8's combined-acceptance text) carries no sanitized command
	// description for this microtask to record, and Manifest.Validate does
	// not require one. A later phase (Bash, Phase 4) is the caller that
	// actually knows the command line; wiring it through is out of this
	// task's scope. TTY is likewise left false here: it is only knowable
	// from the live tool.Process's StreamMode after prepared.Start
	// succeeds, and this manifest write must happen strictly before that
	// call.
	manifest := NewManifest(identity, CommandMetadata{}, accessMode(prepared.EffectiveWorkspaceAccess().Kind), false, time.Now().UTC(), nil)
	if err := s.manifests.Save(manifest); err != nil {
		s.releaseQuota(res)
		s.releaseLease(lease)
		_ = prepared.Close()
		return "", Wrap(CodeProcessSetupFailed, err)
	}

	proc, err := prepared.Start(ctx)
	if err != nil {
		s.releaseQuota(res)
		s.releaseLease(lease)
		_ = prepared.Close()
		return "", Wrap(CodeSpawnFailed, err)
	}

	startedAt := time.Now().UTC()
	manifest.State = StateRunning
	manifest.StartedAt = &startedAt
	if err := s.manifests.Save(manifest); err != nil {
		s.releaseQuota(res)
		s.releaseLease(lease)
		_ = proc.Close(ctx)
		return "", Wrap(CodeProcessSetupFailed, err)
	}

	spool, err := OpenSpool(s.spoolRoot, handle, res.spoolBytes)
	if err != nil {
		s.releaseQuota(res)
		s.releaseLease(lease)
		_ = proc.Close(ctx)
		return "", Wrap(CodeProcessSetupFailed, err)
	}

	lifetimeCtx, cancel := context.WithCancel(context.Background())

	e := &entry{
		identity:       identity,
		lease:          lease,
		lifecycle:      sink,
		observations:   observations,
		ceiling:        ceiling,
		yield:          yield,
		process:        proc,
		reservation:    res,
		buffer:         NewBuffer(res.memoryBytes),
		spool:          spool,
		lifetimeCancel: cancel,
		done:           make(chan struct{}),
	}

	s.mu.Lock()
	s.entries[handle] = e
	s.mu.Unlock()

	go e.run(lifetimeCtx)

	return handle, nil
}

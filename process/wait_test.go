package process

import (
	"context"
	"errors"
	"testing"
	"time"
)

// --- Task 9A test fixtures ---
//
// newWaitEntry and registerEntry build and register a minimal *entry
// directly against a Supervisor's registry, bypassing Supervisor.Start's
// full admission/spawn plumbing entirely -- mirroring entry_test.go's
// newRaceEntry, which does the same for terminal-arbitration tests. wait.go
// only needs an entry's identity, buffer/spool, done channel, and the
// generation/wake fields entry.go's appendChunk drives; none of Start's
// quota reservation, manifest persistence, or live tool.Process is
// relevant to proving Wait's own poll/any/all/cancel/quota behavior.

// newWaitEntry builds a bare, directly-appendable *entry owned by owner and
// keyed by handle, with its own private Spool beneath a fresh temp dir.
func newWaitEntry(t *testing.T, owner Owner, handle Handle) *entry {
	t.Helper()
	spool, err := OpenSpool(t.TempDir(), handle, 0)
	if err != nil {
		t.Fatalf("OpenSpool() err = %v, want nil", err)
	}
	return &entry{
		identity: Identity{Handle: handle, Owner: owner, Origin: testOrigin(t)},
		buffer:   NewBuffer(0),
		spool:    spool,
		done:     make(chan struct{}),
		wake:     make(chan struct{}),
	}
}

// registerEntry inserts e directly into sup's registry, exactly as
// Supervisor.Start would after a successful admission.
func registerEntry(t *testing.T, sup *Supervisor, e *entry) {
	t.Helper()
	sup.mu.Lock()
	sup.entries[e.identity.Handle] = e
	sup.mu.Unlock()
}

// pendingWaiters reads sup's current blocking-waiter count for owner's
// session directly (same-package white-box access, mirroring
// supervisor_test.go's testEntry), so a test can observe
// acquireWaiterSlot/releaseWaiterSlot's bookkeeping without any exported
// accessor.
func pendingWaiters(sup *Supervisor, owner Owner) int {
	sup.mu.Lock()
	defer sup.mu.Unlock()
	return sup.pendingWaitersBySession[owner.SessionID]
}

// waitForPendingWaiters polls (test-side only -- not part of the
// implementation under test) until pendingWaiters(sup, owner) == want or a
// short deadline elapses, so a test can synchronize with a background
// Wait call's internal quota bookkeeping without a race.
func waitForPendingWaiters(t *testing.T, sup *Supervisor, owner Owner, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := pendingWaiters(sup, owner); got == want {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("pendingWaiters() = %d, want %d (timed out)", got, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitCallResult bundles one background Supervisor.Wait call's return
// values so a test goroutine can hand them back over a channel.
type waitCallResult struct {
	statuses []WaitStatus
	err      error
}

// --- TestWaitPollReturnsImmediately ---

// TestWaitPollReturnsImmediately proves WaitPoll returns every target's
// current status without blocking at all -- not even to consult ctx: the
// context supplied here is already canceled before the call, and Wait
// still succeeds, because poll mode never checks it. It also proves
// owner-scoping: a missing handle and a handle owned by a different Owner
// both report Found false, indistinguishably.
func TestWaitPollReturnsImmediately(t *testing.T) {
	t.Parallel()

	sup := newTestSupervisor(t, Config{})
	owner := testOwner(t)
	other := testOwner(t)

	h := testHandle(t, 1)
	e := newWaitEntry(t, owner, h)
	e.appendChunk([]byte("hello"))
	registerEntry(t, sup, e)

	foreign := testHandle(t, 2)
	fe := newWaitEntry(t, other, foreign)
	registerEntry(t, sup, fe)

	missing := testHandle(t, 3)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Already canceled: poll mode must not observe this at all.

	statuses, err := sup.Wait(ctx, owner, WaitPoll, []WaitTarget{
		{Handle: h},
		{Handle: missing},
		{Handle: foreign},
	})
	if err != nil {
		t.Fatalf("Wait(poll) err = %v, want nil", err)
	}
	if len(statuses) != 3 {
		t.Fatalf("len(statuses) = %d, want 3", len(statuses))
	}

	if got := statuses[0]; !got.Found || got.Terminal || got.Generation == 0 {
		t.Errorf("statuses[0] = %+v, want Found=true Terminal=false Generation>0", got)
	}
	if got := statuses[1]; got.Found {
		t.Errorf("statuses[1] (missing handle) = %+v, want Found=false", got)
	}
	if got := statuses[2]; got.Found {
		t.Errorf("statuses[2] (foreign-owned handle) = %+v, want Found=false", got)
	}
}

// --- TestWaitAnyWakesOnAppend ---

// TestWaitAnyWakesOnAppend proves WaitAny is woken directly by
// appendChunk's generation bump rather than by polling: ctx carries a long
// (30s) timeout, and the call is expected to return within a short,
// test-appropriate bound (well under one second) immediately after a
// background goroutine appends to the watched entry -- not after the
// context timeout elapses.
func TestWaitAnyWakesOnAppend(t *testing.T) {
	t.Parallel()

	sup := newTestSupervisor(t, Config{})
	owner := testOwner(t)
	h := testHandle(t, 1)
	e := newWaitEntry(t, owner, h)
	registerEntry(t, sup, e)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resultCh := make(chan waitCallResult, 1)
	go func() {
		statuses, err := sup.Wait(ctx, owner, WaitAny, []WaitTarget{{Handle: h}})
		resultCh <- waitCallResult{statuses: statuses, err: err}
	}()

	waitForPendingWaiters(t, sup, owner, 1)
	e.appendChunk([]byte("new output"))

	select {
	case got := <-resultCh:
		if got.err != nil {
			t.Fatalf("Wait(any) err = %v, want nil", got.err)
		}
		if len(got.statuses) != 1 {
			t.Fatalf("len(statuses) = %d, want 1", len(got.statuses))
		}
		if s := got.statuses[0]; !s.Found || s.Terminal || s.Generation != 1 {
			t.Errorf("statuses[0] = %+v, want Found=true Terminal=false Generation=1", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait(any) did not wake on append within 2s (want: woken well under the 30s ctx timeout)")
	}

	waitForPendingWaiters(t, sup, owner, 0)
}

// --- TestWaitAllRequiresEveryEntry ---

// TestWaitAllRequiresEveryEntry proves WaitAll blocks until every watched
// handle has advanced, not just one: it appends to the first of two
// watched entries, confirms the call has NOT returned, then appends to the
// second and confirms it returns promptly with both statuses reflecting
// the advance.
func TestWaitAllRequiresEveryEntry(t *testing.T) {
	t.Parallel()

	sup := newTestSupervisor(t, Config{})
	owner := testOwner(t)

	h1 := testHandle(t, 1)
	e1 := newWaitEntry(t, owner, h1)
	registerEntry(t, sup, e1)

	h2 := testHandle(t, 2)
	e2 := newWaitEntry(t, owner, h2)
	registerEntry(t, sup, e2)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resultCh := make(chan waitCallResult, 1)
	go func() {
		statuses, err := sup.Wait(ctx, owner, WaitAll, []WaitTarget{
			{Handle: h1},
			{Handle: h2},
		})
		resultCh <- waitCallResult{statuses: statuses, err: err}
	}()

	waitForPendingWaiters(t, sup, owner, 1)

	e1.appendChunk([]byte("only the first"))

	select {
	case got := <-resultCh:
		t.Fatalf("Wait(all) returned early after only one of two entries advanced: %+v", got)
	case <-time.After(150 * time.Millisecond):
		// Expected: still blocked with only one of two targets advanced.
	}

	e2.appendChunk([]byte("now the second"))

	select {
	case got := <-resultCh:
		if got.err != nil {
			t.Fatalf("Wait(all) err = %v, want nil", got.err)
		}
		if len(got.statuses) != 2 {
			t.Fatalf("len(statuses) = %d, want 2", len(got.statuses))
		}
		if s := got.statuses[0]; !s.Found || s.Generation == 0 {
			t.Errorf("statuses[0] = %+v, want Found=true Generation>0", s)
		}
		if s := got.statuses[1]; !s.Found || s.Generation == 0 {
			t.Errorf("statuses[1] = %+v, want Found=true Generation>0", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait(all) did not wake once every entry had advanced")
	}

	waitForPendingWaiters(t, sup, owner, 0)
}

// --- TestWaitCancelRemovesWaiter ---

// TestWaitCancelRemovesWaiter proves that canceling the caller's context
// while a blocking Wait call is outstanding returns promptly with a
// context error and cleans up the waiter registration -- no leaked
// goroutine, no stale entry in the quota bookkeeping. It also exercises
// the waiter quota (MaxPendingWaiters: 1): a second concurrent Wait call
// for the same session is rejected immediately with CodeOutputQuotaExceeded
// while the first is still outstanding, and a generation bump after the
// canceled waiter's cleanup neither panics nor hangs against any leaked
// watcher goroutine.
func TestWaitCancelRemovesWaiter(t *testing.T) {
	t.Parallel()

	sup := newTestSupervisor(t, Config{MaxPendingWaiters: 1})
	owner := testOwner(t)
	h := testHandle(t, 1)
	e := newWaitEntry(t, owner, h)
	registerEntry(t, sup, e)

	if got := pendingWaiters(sup, owner); got != 0 {
		t.Fatalf("pendingWaiters() = %d, want 0 before any Wait call", got)
	}

	ctx, cancel := context.WithCancel(context.Background())

	resultCh := make(chan waitCallResult, 1)
	go func() {
		statuses, err := sup.Wait(ctx, owner, WaitAny, []WaitTarget{{Handle: h}})
		resultCh <- waitCallResult{statuses: statuses, err: err}
	}()

	waitForPendingWaiters(t, sup, owner, 1)

	// The quota (MaxPendingWaiters: 1) is now exhausted for this session:
	// a second concurrent blocking wait must be rejected immediately,
	// without blocking and without disturbing the first waiter.
	if _, err := sup.Wait(context.Background(), owner, WaitAny, []WaitTarget{{Handle: h}}); err == nil {
		t.Fatal("Wait() with quota exhausted err = nil, want CodeOutputQuotaExceeded")
	} else {
		var procErr *Error
		if !errors.As(err, &procErr) || procErr.Code != CodeOutputQuotaExceeded {
			t.Fatalf("Wait() with quota exhausted err = %v, want *Error{Code: CodeOutputQuotaExceeded}", err)
		}
	}
	if got := pendingWaiters(sup, owner); got != 1 {
		t.Fatalf("pendingWaiters() = %d, want 1 (rejected call must not reserve a slot)", got)
	}

	cancel()

	select {
	case got := <-resultCh:
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("Wait(any) err = %v, want context.Canceled", got.err)
		}
		if got.statuses != nil {
			t.Errorf("Wait(any) statuses = %+v, want nil on cancellation", got.statuses)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait(any) did not return promptly after context cancellation")
	}

	waitForPendingWaiters(t, sup, owner, 0)

	// A generation bump after the canceled waiter's cleanup must not panic
	// or hang against any leaked watcher goroutine from the canceled call.
	e.appendChunk([]byte("after cancellation"))

	statuses, err := sup.Wait(context.Background(), owner, WaitPoll, []WaitTarget{{Handle: h}})
	if err != nil {
		t.Fatalf("Wait(poll) err = %v, want nil", err)
	}
	if len(statuses) != 1 || statuses[0].Generation != 1 {
		t.Fatalf("Wait(poll) statuses = %+v, want one status with Generation=1", statuses)
	}
}

package process

import (
	"context"
	"io"
	"strings"
	"sync"

	"github.com/looprig/harness/pkg/tool"
)

// fakePreparedProcess is a deterministic, test-controlled implementation of
// tool.PreparedProcess. It exists so process package tests never depend on
// a real AsyncProcessRunner (the supervisor is runner-free) or a real OS
// process. Every field is a plain, directly settable callback/value so a
// test can drive exactly the scenario it needs: which tool.Process (if
// any) Start returns, whether Start or Close fail, and how many times each
// method was called.
//
// This file starts minimal for 8A's three admission/quota tests and is
// expected to grow new fields -- never rename or remove ones an already
// -passing test depends on -- as Task 8B/8C/8D and Task 9 add scenarios
// (durable handoff, stream drain, terminal races, restore, shutdown).
type fakePreparedProcess struct {
	access tool.WorkspaceAccess

	// startFunc, when set, is called by Start instead of returning
	// (process, startErr) directly. It exists so a test can observe
	// Supervisor state (e.g. that quota is already reserved) from inside
	// the very call Start makes to PreparedProcess.Start -- see
	// TestSupervisorReservesQuotaBeforeStart. If nil, Start returns
	// (process, startErr).
	startFunc func(context.Context) (tool.Process, error)
	process   tool.Process
	startErr  error

	closeErr error

	mu         sync.Mutex
	startCalls int
	closeCalls int
}

func (p *fakePreparedProcess) EffectiveWorkspaceAccess() tool.WorkspaceAccess {
	return p.access
}

func (p *fakePreparedProcess) Start(ctx context.Context) (tool.Process, error) {
	p.mu.Lock()
	p.startCalls++
	p.mu.Unlock()
	if p.startFunc != nil {
		return p.startFunc(ctx)
	}
	return p.process, p.startErr
}

// Close matches the Harness PreparedProcess.Close contract's idempotence
// ("Close releases an unstarted preparation"): repeated calls only add to
// closeCalls and keep returning closeErr, never panicking or changing
// behavior based on call count.
func (p *fakePreparedProcess) Close() error {
	p.mu.Lock()
	p.closeCalls++
	p.mu.Unlock()
	return p.closeErr
}

// StartCalls reports how many times Start was called.
func (p *fakePreparedProcess) StartCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.startCalls
}

// CloseCalls reports how many times Close was called.
func (p *fakePreparedProcess) CloseCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closeCalls
}

var _ tool.PreparedProcess = (*fakePreparedProcess)(nil)

// fakeProcess is a deterministic, test-controlled implementation of
// tool.Process. Like fakePreparedProcess, it starts minimal: 8A only needed
// a value fakePreparedProcess.Start can return, so Stdout/Stderr defaulted
// to closed, empty streams unconditionally. 8B adds the stdout/stderr
// fields below so a stream-drain test can inject its own controllable
// io.ReadCloser (e.g. an io.Pipe end, or a bytes.Reader wrapped in
// io.NopCloser) -- see this type's original doc comment's own prediction:
// "expected to grow new fields ... as Task 8B/8C/8D ... add scenarios
// (durable handoff, stream drain, terminal races, restore, shutdown)". A
// zero-value field (nil) preserves 8A's exact default behavior, so no
// existing test's fakeProcess{} literal changes meaning.
// Wait/Resize/Signal are simple configurable stubs, and Close counts its
// calls.
type fakeProcess struct {
	streamMode tool.ProcessStreamMode

	// stdout and stderr, when set, are returned verbatim by Stdout/Stderr
	// instead of the default closed, empty reader. A test that wants to
	// drive the entry's drain goroutines (entry.go's drain) with
	// controlled bytes -- an io.Pipe end, for instance -- sets these
	// directly.
	stdout io.ReadCloser
	stderr io.ReadCloser

	waitResult tool.ProcessResult
	waitErr    error

	resizeErr error
	signalErr error

	mu         sync.Mutex
	closeCalls int
}

func (p *fakeProcess) Stdout() io.ReadCloser {
	if p.stdout != nil {
		return p.stdout
	}
	return io.NopCloser(strings.NewReader(""))
}

func (p *fakeProcess) Stderr() io.ReadCloser {
	if p.stderr != nil {
		return p.stderr
	}
	return io.NopCloser(strings.NewReader(""))
}

func (p *fakeProcess) Stdin() io.WriteCloser { return nopWriteCloser{io.Discard} }

func (p *fakeProcess) StreamMode() tool.ProcessStreamMode {
	if p.streamMode == 0 {
		return tool.ProcessStreamModePipes
	}
	return p.streamMode
}

func (p *fakeProcess) Wait(ctx context.Context) (tool.ProcessResult, error) {
	return p.waitResult, p.waitErr
}

func (p *fakeProcess) Resize(ctx context.Context, cols, rows uint16) error { return p.resizeErr }

func (p *fakeProcess) Signal(ctx context.Context, sig tool.ProcessSignal) error { return p.signalErr }

func (p *fakeProcess) Close(ctx context.Context) error {
	p.mu.Lock()
	p.closeCalls++
	p.mu.Unlock()
	return nil
}

// CloseCalls reports how many times Close was called.
func (p *fakeProcess) CloseCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closeCalls
}

var _ tool.Process = (*fakeProcess)(nil)

// nopWriteCloser adapts an io.Writer (typically io.Discard) into an
// io.WriteCloser whose Close is a no-op, mirroring the standard library's
// io.NopCloser for readers (which has no writer-side equivalent).
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// fakeLease is a deterministic, test-controlled implementation of Lease
// (supervisor.go's placeholder for the real Harness workspace lease Task
// 15/19 will wire in).
type fakeLease struct {
	releaseErr error

	mu           sync.Mutex
	releaseCalls int
}

func (l *fakeLease) Release() error {
	l.mu.Lock()
	l.releaseCalls++
	l.mu.Unlock()
	return l.releaseErr
}

// ReleaseCalls reports how many times Release was called.
func (l *fakeLease) ReleaseCalls() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.releaseCalls
}

var _ Lease = (*fakeLease)(nil)

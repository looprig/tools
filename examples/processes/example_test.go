package processes_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/process"
)

type preparedProcess struct{ process tool.Process }

func (p *preparedProcess) EffectiveWorkspaceAccess() tool.WorkspaceAccess {
	return tool.NewWorkspaceAccess(tool.WorkspaceAccessReadOnly, nil, nil)
}

func (p *preparedProcess) Start(context.Context) (tool.Process, error) { return p.process, nil }
func (p *preparedProcess) Close() error                                { return nil }

type controlledProcess struct {
	done      chan struct{}
	stopOnce  sync.Once
	mu        sync.Mutex
	signals   []tool.ProcessSignal
	startedAt time.Time
}

func (p *controlledProcess) Stdout() io.ReadCloser { return io.NopCloser(strings.NewReader("")) }
func (p *controlledProcess) Stderr() io.ReadCloser { return io.NopCloser(strings.NewReader("")) }
func (p *controlledProcess) Stdin() io.WriteCloser { return discardWriteCloser{io.Discard} }
func (p *controlledProcess) StreamMode() tool.ProcessStreamMode {
	return tool.ProcessStreamModePipes
}

func (p *controlledProcess) Wait(ctx context.Context) (tool.ProcessResult, error) {
	select {
	case <-p.done:
		return tool.ProcessResult{Reason: tool.ProcessTerminalTerminated, StartedAt: p.startedAt, FinishedAt: time.Now()}, nil
	case <-ctx.Done():
		return tool.ProcessResult{}, ctx.Err()
	}
}

func (*controlledProcess) Resize(context.Context, uint16, uint16) error { return nil }

func (p *controlledProcess) Signal(_ context.Context, signal tool.ProcessSignal) error {
	p.mu.Lock()
	p.signals = append(p.signals, signal)
	p.mu.Unlock()
	if signal == tool.ProcessSignalTerminate {
		p.stopOnce.Do(func() { close(p.done) })
	}
	return nil
}

func (*controlledProcess) Close(context.Context) error { return nil }

func (p *controlledProcess) firstSignal() tool.ProcessSignal {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.signals[0]
}

type discardWriteCloser struct{ io.Writer }

func (discardWriteCloser) Close() error { return nil }

type lease struct {
	mu       sync.Mutex
	releases int
}

func (l *lease) Release() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.releases++
	return nil
}

func (l *lease) releaseCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.releases
}

func mustTempDir() string {
	dir, err := os.MkdirTemp("", "looprig-tools-process-")
	if err != nil {
		panic(err)
	}
	return dir
}

func removeAll(path string) {
	if err := os.RemoveAll(path); err != nil {
		panic(err)
	}
}

// Example_processLifecycle demonstrates both sides of session ownership. A
// live supervisor terminates its process tree and releases its workspace
// lease during shutdown. A new supervisor reconciles a persisted running
// manifest as lost instead of trying to reconnect to or signal an old PID.
func Example_processLifecycle() {
	manifestDir, spoolDir := mustTempDir(), mustTempDir()
	defer removeAll(manifestDir)
	defer removeAll(spoolDir)
	store := process.NewManifestStore(manifestDir)
	supervisor, err := process.NewSupervisor(process.Config{GracefulShutdownPeriod: time.Millisecond}, store, spoolDir, nil, nil)
	if err != nil {
		panic(err)
	}
	owner := process.Owner{
		SessionID: uuid.MustParse("66666666-6666-4666-8666-666666666666"),
		LoopID:    uuid.MustParse("77777777-7777-4777-8777-777777777777"),
	}
	origin := process.Origin{ToolExecutionID: uuid.MustParse("88888888-8888-4888-8888-888888888888")}
	lifetime := &controlledProcess{done: make(chan struct{}), startedAt: time.Now()}
	workspaceLease := &lease{}
	_, err = supervisor.Start(context.Background(), owner, origin, &preparedProcess{process: lifetime}, workspaceLease, nil, nil, process.StorageCeiling{}, process.YieldSettings{})
	if err != nil {
		panic(err)
	}
	if err := supervisor.Shutdown(context.Background()); err != nil {
		panic(err)
	}
	fmt.Println("shutdown signal:", lifetime.firstSignal() == tool.ProcessSignalTerminate)
	fmt.Println("lease releases:", workspaceLease.releaseCount())

	restoreManifestDir, restoreSpoolDir := mustTempDir(), mustTempDir()
	defer removeAll(restoreManifestDir)
	defer removeAll(restoreSpoolDir)
	restoreStore := process.NewManifestStore(restoreManifestDir)
	handle := process.Handle("AAAAAAAAAAAAAAAAAAAAAA")
	manifest := process.NewManifest(process.Identity{Handle: handle, Owner: owner, Origin: origin}, process.CommandMetadata{Command: "old command"}, process.AccessReadOnly, false, time.Now(), nil)
	manifest.Events = process.LifecycleEventIDs{
		Started:      uuid.MustParse("99999999-9999-4999-8999-999999999991"),
		Backgrounded: uuid.MustParse("99999999-9999-4999-8999-999999999992"),
		Completed:    uuid.MustParse("99999999-9999-4999-8999-999999999993"),
		Lost:         uuid.MustParse("99999999-9999-4999-8999-999999999994"),
		CommandID:    uuid.MustParse("99999999-9999-4999-8999-999999999995"),
	}
	started := time.Now()
	manifest.State = process.StateRunning
	manifest.StartedAt = &started
	if err := restoreStore.Save(manifest); err != nil {
		panic(err)
	}
	restored, err := process.NewSupervisor(process.Config{}, restoreStore, restoreSpoolDir, nil, nil)
	if err != nil {
		panic(err)
	}
	report, err := restored.Restore(context.Background())
	if err != nil {
		panic(err)
	}
	reconciled, err := restoreStore.Load(handle)
	if err != nil {
		panic(err)
	}
	fmt.Println("restored:", len(report.Reconciled), reconciled.State)

	// Output:
	// shutdown signal: true
	// lease releases: 1
	// restored: 1 lost_on_restore
}

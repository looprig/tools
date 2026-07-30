//go:build integration

package process_test

// integration_test.go is Task 20's tagged acceptance seam for the
// ProcessOutput/ProcessInput/ProcessStop side of the supervised-command
// workflow (docs/plans/2026-07-27-long-running-command-supervision.md, Task
// 20): it composes the PUBLIC Harness binding contracts
// (tool.Bindings/tool.ProcessBinding/tool.SessionResourceRegistry) with a
// contract-faithful Tools test registry and drives the root package's own
// public ProcessOutputDefinition/ProcessInputDefinition/ProcessStopDefinition
// builders WITHOUT a tool.AsyncProcessRunner anywhere -- exactly the
// "Construct ProcessOutput, ProcessInput, and ProcessStop without a runner"
// acceptance requirement, since those three tools never touch a runner at
// all (they only ever read the session's already-admitted process registry).
//
// This file lives in the external `process_test` package (not `process`) so
// it can import the root `github.com/looprig/tools` package (the
// *Definition builders) without an import cycle: the root package imports
// `github.com/looprig/tools/process`, so an internal `package process` test
// file could never import it back, but an external `process_test` package
// can, because the root package never imports `process_test`.
//
// Where a real admitted process is needed (owner isolation, spool retention,
// resource shutdown), this file admits one directly through the exported
// *Supervisor.Start, handing it a real os/exec.Cmd-backed tool.PreparedProcess
// (execPreparedProcess/execProcess below) -- mirroring
// process/supervisor_integration_test.go's identically-named types -- so
// these are genuine OS subprocesses, not synthetic fakes. The manifest-restore
// scenario builds its fixture manifests directly through the exported
// Manifest/ManifestStore/Spool API instead (mirroring restore_test.go's own
// established, Supervisor.Start-free pattern), since restore reconciliation is
// a property of PERSISTED STATE, not of a live process, and a genuinely
// abandoned process has, by construction, no live supervisor left to race.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools"
	"github.com/looprig/tools/process"
)

// --- contract-faithful Tools test registry ----------------------------------

// processFakeRegistry is a tool.SessionResourceRegistry test double
// mirroring the real registry's get-or-create contract: a key's factory
// runs at most once, and every caller receives the same resource back
// afterward, regardless of order.
type processFakeRegistry struct {
	dir string

	mu        sync.Mutex
	resources map[string]tool.SessionResource
	err       error
}

func newProcessFakeRegistry(dir string) *processFakeRegistry {
	return &processFakeRegistry{dir: dir, resources: map[string]tool.SessionResource{}}
}

func (r *processFakeRegistry) GetOrCreate(_ context.Context, key string, factory func(string) (tool.SessionResource, error)) (tool.SessionResource, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	if res, ok := r.resources[key]; ok {
		return res, nil
	}
	res, err := factory(r.dir)
	if err != nil {
		return nil, err
	}
	r.resources[key] = res
	return res, nil
}

var _ tool.SessionResourceRegistry = (*processFakeRegistry)(nil)

// --- real-subprocess-backed tool.PreparedProcess ----------------------------

// execPreparedProcess/execProcess are a real os/exec.Cmd-backed
// tool.PreparedProcess/tool.Process pair, mirroring
// process/supervisor_integration_test.go's identically-shaped types: this
// suite hands them directly to the exported *Supervisor.Start (never through
// a tool.AsyncProcessRunner -- this file constructs no runner at all), so
// the scenarios that need a genuinely live process exercise real OS I/O.
type execPreparedProcess struct {
	cmd    *exec.Cmd
	access tool.WorkspaceAccess
}

func (p *execPreparedProcess) EffectiveWorkspaceAccess() tool.WorkspaceAccess { return p.access }

func (p *execPreparedProcess) Start(context.Context) (tool.Process, error) {
	stdout, err := p.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := p.cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	stdin, err := p.cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	startedAt := time.Now().UTC()
	if err := p.cmd.Start(); err != nil {
		return nil, err
	}
	return &execProcess{cmd: p.cmd, stdout: stdout, stderr: stderr, stdin: stdin, startedAt: startedAt}, nil
}

func (p *execPreparedProcess) Close() error { return nil }

var _ tool.PreparedProcess = (*execPreparedProcess)(nil)

type execProcess struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr io.ReadCloser
	stdin  io.WriteCloser

	startedAt time.Time
}

func (p *execProcess) Stdout() io.ReadCloser              { return p.stdout }
func (p *execProcess) Stderr() io.ReadCloser              { return p.stderr }
func (p *execProcess) Stdin() io.WriteCloser              { return p.stdin }
func (p *execProcess) StreamMode() tool.ProcessStreamMode { return tool.ProcessStreamModePipes }

func (p *execProcess) Wait(context.Context) (tool.ProcessResult, error) {
	waitErr := p.cmd.Wait()
	finishedAt := time.Now().UTC()

	result := tool.ProcessResult{StartedAt: p.startedAt, FinishedAt: finishedAt}
	var exitErr *exec.ExitError
	switch {
	case waitErr == nil:
		result.Reason = tool.ProcessTerminalExited
	case errors.As(waitErr, &exitErr):
		result.Reason = tool.ProcessTerminalExited
		result.ExitCode = exitErr.ExitCode()
	default:
		return tool.ProcessResult{}, waitErr
	}
	return result, nil
}

func (p *execProcess) Resize(context.Context, uint16, uint16) error { return nil }

func (p *execProcess) Signal(_ context.Context, sig tool.ProcessSignal) error {
	if p.cmd.Process == nil {
		return nil
	}
	if sig == tool.ProcessSignalKill {
		return p.cmd.Process.Kill()
	}
	return p.cmd.Process.Signal(os.Interrupt)
}

func (p *execProcess) Close(context.Context) error {
	if p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Kill()
}

var _ tool.Process = (*execProcess)(nil)

// --- shared fixtures ---------------------------------------------------------

func mustUUID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() error = %v", err)
	}
	return id
}

func mustHandle(t *testing.T) process.Handle {
	t.Helper()
	h, err := process.NewHandle(nil)
	if err != nil {
		t.Fatalf("process.NewHandle() error = %v", err)
	}
	return h
}

func freshLifecycleEventIDs(t *testing.T) process.LifecycleEventIDs {
	t.Helper()
	return process.LifecycleEventIDs{
		Started:      mustUUID(t),
		Backgrounded: mustUUID(t),
		Completed:    mustUUID(t),
		Lost:         mustUUID(t),
		CommandID:    mustUUID(t),
	}
}

// newProcessTestBindings builds a fresh tool.Bindings + processFakeRegistry
// pair over dir, with its own SessionID/LoopID.
func newProcessTestBindings(t *testing.T, dir string) (tool.Bindings, *processFakeRegistry) {
	t.Helper()
	registry := newProcessFakeRegistry(dir)
	bindings := tool.Bindings{
		SessionID: mustUUID(t),
		LoopID:    mustUUID(t),
		Process:   &tool.ProcessBinding{Registry: registry},
	}
	return bindings, registry
}

// buildProcessTools builds all three companion definitions against bindings
// -- runner-free, per this file's whole reason for existing (see the package
// doc comment above).
func buildProcessTools(t *testing.T, bindings tool.Bindings) (output, input, stop tool.InvokableTool) {
	t.Helper()
	outBuilt, err := tools.ProcessOutputDefinition().Build(context.Background(), bindings)
	if err != nil {
		t.Fatalf("ProcessOutputDefinition().Build() error = %v", err)
	}
	inBuilt, err := tools.ProcessInputDefinition().Build(context.Background(), bindings)
	if err != nil {
		t.Fatalf("ProcessInputDefinition().Build() error = %v", err)
	}
	stopBuilt, err := tools.ProcessStopDefinition().Build(context.Background(), bindings)
	if err != nil {
		t.Fatalf("ProcessStopDefinition().Build() error = %v", err)
	}
	return outBuilt[0], inBuilt[0], stopBuilt[0]
}

// supervisorResourceFor resolves registry's shared supervisor resource
// directly, the same way resolveProcessSupervisor (definitions.go) and
// runSupervised (bash/supervised.go) do, so a test can drive real admission
// (*Supervisor.Start) against the EXACT supervisor the built tools above
// already share.
func supervisorResourceFor(t *testing.T, registry *processFakeRegistry) *process.SupervisorResource {
	t.Helper()
	res, err := registry.GetOrCreate(context.Background(), process.SupervisorResourceKey, process.NewSupervisorResource)
	if err != nil {
		t.Fatalf("GetOrCreate(%s) error = %v", process.SupervisorResourceKey, err)
	}
	sr, ok := res.(*process.SupervisorResource)
	if !ok || sr == nil || sr.Supervisor == nil {
		t.Fatalf("registry resource = %#v, want a populated *process.SupervisorResource", res)
	}
	return sr
}

func startRealProcess(t *testing.T, sup *process.Supervisor, owner process.Owner, command string) process.Handle {
	t.Helper()
	cmd := exec.Command("sh", "-c", command)
	prepared := &execPreparedProcess{cmd: cmd, access: tool.NewWorkspaceAccess(tool.WorkspaceAccessReadOnly, nil, nil)}
	handle, err := sup.Start(context.Background(), owner, process.Origin{ToolExecutionID: mustUUID(t)}, prepared, nil, nil, nil, process.StorageCeiling{}, process.YieldSettings{})
	if err != nil {
		t.Fatalf("Supervisor.Start(%q) error = %v", command, err)
	}
	return handle
}

// waitTerminal blocks, using ONLY the exported Supervisor.Wait, until handle
// reaches a terminal state or the bound deadline elapses.
func waitTerminal(t *testing.T, sup *process.Supervisor, owner process.Owner, handle process.Handle) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var generation uint64
	for {
		statuses, err := sup.Wait(ctx, owner, process.WaitAll, []process.WaitTarget{{Handle: handle, Generation: generation}})
		if err != nil {
			t.Fatalf("Wait() error = %v", err)
		}
		if len(statuses) != 1 {
			t.Fatalf("Wait() statuses = %+v, want exactly 1", statuses)
		}
		generation = statuses[0].Generation
		if statuses[0].Terminal {
			return
		}
	}
}

// runCall drives tl through the exact PrepareCall -> loop.WithPreparedCall
// -> InvokableRun sequence the real runner uses.
func runCall(t *testing.T, tl tool.InvokableTool, argsJSON string) string {
	t.Helper()
	preparer, ok := tl.(tool.CallPreparer)
	if !ok {
		t.Fatalf("%T does not implement tool.CallPreparer", tl)
	}
	id := mustUUID(t)
	req, art, err := preparer.PrepareCall(context.Background(), id, argsJSON)
	if err != nil {
		t.Fatalf("PrepareCall(%s) error = %v", argsJSON, err)
	}
	call := tool.PreparedCall{ExecutionID: id, Request: req, Artifact: art}
	ctx := loop.WithPreparedCall(context.Background(), call)
	res, err := tl.InvokableRun(ctx, argsJSON)
	if err != nil {
		t.Fatalf("InvokableRun(%s) returned a Go error %v; tools in this suite return tool-result strings", argsJSON, err)
	}
	if res == nil || len(res.Content) != 1 {
		t.Fatalf("InvokableRun(%s) result = %#v, want one content block", argsJSON, res)
	}
	block, ok := res.Content[0].(*content.TextBlock)
	if !ok {
		t.Fatalf("InvokableRun(%s) content = %T, want *content.TextBlock", argsJSON, res.Content[0])
	}
	return block.Text
}

// processCallResult mirrors process's unexported processOutputResult AND
// processStopResult JSON shapes closely enough for every field this suite
// reads; a field absent from a given tool's own JSON simply decodes to its
// zero value.
type processCallResult struct {
	ProcessID   string `json:"process_id"`
	Status      string `json:"status"`
	Output      string `json:"output"`
	StartCursor int64  `json:"start_cursor"`
	NextCursor  int64  `json:"next_cursor"`
	TotalBytes  int64  `json:"total_bytes"`
	Gap         bool   `json:"gap"`
	ExitCode    *int   `json:"exit_code"`
	Reason      string `json:"reason"`
	Error       string `json:"error"`
}

func decodeProcessResult(t *testing.T, text string) processCallResult {
	t.Helper()
	var out processCallResult
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("decode process result %q: %v", text, err)
	}
	return out
}

// --- TestIntegrationProcessToolsRestore -------------------------------------

// TestIntegrationProcessToolsRestore drives ProcessOutput/ProcessInput/
// ProcessStop end to end through the PUBLIC ProcessOutputDefinition/
// ProcessInputDefinition/ProcessStopDefinition builders, a contract-faithful
// fake Harness registry, and (where a live process is needed) a real
// os/exec.Cmd admitted directly through Supervisor.Start. It covers: building
// all three tools without a runner, owner isolation, spool retention,
// resource shutdown, and manifest restore.
func TestIntegrationProcessToolsRestore(t *testing.T) {
	t.Run("construct without a runner and owner isolation", func(t *testing.T) {
		dir := t.TempDir()
		bindingsA, registry := newProcessTestBindings(t, dir)
		outputA, _, _ := buildProcessTools(t, bindingsA)
		sr := supervisorResourceFor(t, registry)

		ownerA := process.Owner{SessionID: bindingsA.SessionID, LoopID: bindingsA.LoopID}
		handle := startRealProcess(t, sr.Supervisor, ownerA, "printf owner-a")
		waitTerminal(t, sr.Supervisor, ownerA, handle)

		ownerRead := decodeProcessResult(t, runCall(t, outputA, fmt.Sprintf(`{"process_id":%q}`, handle)))
		if ownerRead.Error != "" {
			t.Fatalf("owner A ProcessOutput = %+v, want no error", ownerRead)
		}
		if !strings.Contains(ownerRead.Output, "owner-a") {
			t.Fatalf("owner A ProcessOutput output = %q, want it to contain %q", ownerRead.Output, "owner-a")
		}

		// A different loop sharing the SAME session and registry (so the
		// SAME shared supervisor) is a different Owner: every one of the
		// three companion tools, built runner-free exactly like owner A's,
		// must render owner A's handle as not_found -- indistinguishable
		// from a missing one.
		bindingsB := bindingsA
		bindingsB.LoopID = mustUUID(t)
		outputB, inputB, stopB := buildProcessTools(t, bindingsB)

		crossOwnerCalls := map[string]string{
			"ProcessOutput": runCall(t, outputB, fmt.Sprintf(`{"process_id":%q}`, handle)),
			"ProcessInput":  runCall(t, inputB, fmt.Sprintf(`{"process_id":%q,"eof":true}`, handle)),
			"ProcessStop":   runCall(t, stopB, fmt.Sprintf(`{"process_id":%q,"mode":"interrupt"}`, handle)),
		}
		for name, text := range crossOwnerCalls {
			r := decodeProcessResult(t, text)
			if r.Error != string(process.CodeNotFound) {
				t.Errorf("%s: cross-owner result = %+v, want error %q", name, r, process.CodeNotFound)
			}
		}
	})

	t.Run("spool retention truncates and reports a gap", func(t *testing.T) {
		dir := t.TempDir()
		bindings, registry := newProcessTestBindings(t, dir)
		output, _, _ := buildProcessTools(t, bindings)
		sr := supervisorResourceFor(t, registry)
		owner := process.Owner{SessionID: bindings.SessionID, LoopID: bindings.LoopID}

		payload := strings.Repeat("A", 20)
		cmd := exec.Command("sh", "-c", fmt.Sprintf("printf '%s'", payload))
		prepared := &execPreparedProcess{cmd: cmd, access: tool.NewWorkspaceAccess(tool.WorkspaceAccessReadOnly, nil, nil)}
		handle, err := sr.Supervisor.Start(context.Background(), owner, process.Origin{ToolExecutionID: mustUUID(t)}, prepared, nil, nil, nil, process.StorageCeiling{SpoolBytes: 16}, process.YieldSettings{})
		if err != nil {
			t.Fatalf("Supervisor.Start() error = %v", err)
		}
		waitTerminal(t, sr.Supervisor, owner, handle)

		res := decodeProcessResult(t, runCall(t, output, fmt.Sprintf(`{"process_id":%q,"cursor":0}`, handle)))
		if res.Error != "" {
			t.Fatalf("ProcessOutput = %+v, want no error", res)
		}
		if res.TotalBytes != 20 {
			t.Fatalf("TotalBytes = %d, want 20", res.TotalBytes)
		}
		if !res.Gap {
			t.Fatalf("Gap = false, want true: a 20-byte process with a 16-byte spool ceiling must have dropped its earliest retained bytes")
		}
		// StartCursor is documented (process/render.go's SafeTextResult and
		// RenderSafeText doc comments) as "the cursor Read was called with
		// (before any gap adjustment)" -- it always echoes the caller's
		// requested cursor back unchanged, never the earliest still-retained
		// byte. This call requested cursor 0, so StartCursor correctly comes
		// back as 0; there is no assertion to make on it here.
		//
		// The real "earliest retained byte" invariant is provable from the
		// retained window's actual length: 20 bytes were written against a
		// 16-byte spool ceiling, so the earliest 4 bytes were dropped and
		// exactly 16 remain, with the gap-adjusted read reaching the end of
		// the stream (NextCursor == TotalBytes, since nothing further was
		// ever written).
		if res.NextCursor != res.TotalBytes {
			t.Fatalf("NextCursor = %d, want %d (TotalBytes; the read reaches the end of the retained stream)", res.NextCursor, res.TotalBytes)
		}
		if res.Output == "" || strings.Trim(res.Output, "A") != "" {
			t.Fatalf("Output = %q, want a non-empty run of only %q characters", res.Output, "A")
		}
		if len(res.Output) != 16 {
			t.Fatalf("len(Output) = %d, want 16 (the configured spool ceiling: 20 bytes written, the earliest 4 dropped, 16 retained)", len(res.Output))
		}
	})

	t.Run("resource shutdown coordinately terminates running processes and closes admission", func(t *testing.T) {
		dir := t.TempDir()
		bindings, registry := newProcessTestBindings(t, dir)
		sr := supervisorResourceFor(t, registry)
		owner := process.Owner{SessionID: bindings.SessionID, LoopID: bindings.LoopID}

		handle := startRealProcess(t, sr.Supervisor, owner, "sleep 30")

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := sr.Shutdown(shutdownCtx); err != nil {
			t.Fatalf("SupervisorResource.Shutdown() error = %v", err)
		}

		m, err := sr.Manifests.Load(handle)
		if err != nil {
			t.Fatalf("Manifests.Load() error = %v", err)
		}
		if !m.State.Terminal() {
			t.Fatalf("State after Shutdown() = %v, want a terminal state (Shutdown confirms every tree has exited before returning)", m.State)
		}
		if m.State == process.StateLostOnRestore {
			t.Fatalf("State after Shutdown() = %v, want a clean shutdown-driven terminal state, not lost_on_restore", m.State)
		}

		secondCmd := exec.Command("sh", "-c", "true")
		secondPrepared := &execPreparedProcess{cmd: secondCmd, access: tool.NewWorkspaceAccess(tool.WorkspaceAccessReadOnly, nil, nil)}
		_, err = sr.Supervisor.Start(context.Background(), owner, process.Origin{ToolExecutionID: mustUUID(t)}, secondPrepared, nil, nil, nil, process.StorageCeiling{}, process.YieldSettings{})
		if !errors.Is(err, process.New(process.CodeSupervisorShuttingDown)) {
			t.Fatalf("Start() after Shutdown() error = %v, want a CodeSupervisorShuttingDown *process.Error (admission stays closed)", err)
		}
	})

	t.Run("manifest restore reconciles a still-running process as lost and leaves a terminal one unchanged", func(t *testing.T) {
		dir := t.TempDir()
		store := process.NewManifestStore(dir)
		owner := process.Owner{SessionID: mustUUID(t), LoopID: mustUUID(t)}
		origin := process.Origin{ToolExecutionID: mustUUID(t)}

		// A manifest still in StateRunning at restore time -- exactly what an
		// abandoned/crashed session leaves behind. Built directly through the
		// exported Manifest/ManifestStore API (mirroring restore_test.go's
		// startingManifest/runningManifest helpers), with NO live
		// Supervisor.Start admission, tool.Process, or goroutine anywhere: a
		// genuinely abandoned process has no live supervisor left to race,
		// and restore reconciliation is a property of persisted state, not
		// of a live process.
		longHandle := mustHandle(t)
		longManifest := process.NewManifest(process.Identity{Handle: longHandle, Owner: owner, Origin: origin}, process.CommandMetadata{Command: "sleep 30"}, process.AccessReadOnly, false, time.Now().UTC(), nil)
		longManifest.Events = freshLifecycleEventIDs(t)
		if err := store.Save(longManifest); err != nil {
			t.Fatalf("Save(starting) error = %v", err)
		}
		longManifest.State = process.StateRunning
		longStartedAt := time.Now().UTC()
		longManifest.StartedAt = &longStartedAt
		if err := store.Save(longManifest); err != nil {
			t.Fatalf("Save(running) error = %v", err)
		}

		// A manifest already terminal, with real spool output -- reconciliation
		// must leave it exactly as it was.
		doneHandle := mustHandle(t)
		doneManifest := process.NewManifest(process.Identity{Handle: doneHandle, Owner: owner, Origin: origin}, process.CommandMetadata{Command: "printf finished-before-restore"}, process.AccessReadOnly, false, time.Now().UTC(), nil)
		doneManifest.Events = freshLifecycleEventIDs(t)
		if err := store.Save(doneManifest); err != nil {
			t.Fatalf("Save(starting) error = %v", err)
		}
		doneManifest.State = process.StateRunning
		doneStartedAt := time.Now().UTC()
		doneManifest.StartedAt = &doneStartedAt
		if err := store.Save(doneManifest); err != nil {
			t.Fatalf("Save(running) error = %v", err)
		}

		spool, err := process.OpenSpool(dir, doneHandle, 0)
		if err != nil {
			t.Fatalf("OpenSpool() error = %v", err)
		}
		total, err := spool.Append([]byte("finished-before-restore"))
		if err != nil {
			t.Fatalf("Spool.Append() error = %v", err)
		}

		doneFinishedAt := time.Now().UTC()
		exitCode := 0
		doneManifest.State = process.StateExited
		doneManifest.FinishedAt = &doneFinishedAt
		doneManifest.Result = process.Result{ExitCode: &exitCode, Reason: "exited"}
		doneManifest.Cursors = process.SpoolCursors{TotalBytes: total, RetainedFrom: 0}
		doneManifest.CompletionPublished++
		if err := store.Save(doneManifest); err != nil {
			t.Fatalf("Save(exited) error = %v", err)
		}

		// Restore, through the SAME Bindings/registry contract
		// ProcessOutputDefinition resolves its shared, runner-free supervisor
		// through -- a fresh session over the identical on-disk resource root.
		bindings, registry := newProcessTestBindings(t, dir)
		bindings.SessionID, bindings.LoopID = owner.SessionID, owner.LoopID
		output, _, _ := buildProcessTools(t, bindings)
		sr := supervisorResourceFor(t, registry)

		report, err := sr.Supervisor.Restore(context.Background())
		if err != nil {
			t.Fatalf("Restore() error = %v", err)
		}
		if len(report.Errors) != 0 {
			t.Fatalf("Restore() errors = %+v, want none", report.Errors)
		}
		reconciled := map[process.Handle]bool{}
		for _, h := range report.Reconciled {
			reconciled[h] = true
		}
		if !reconciled[longHandle] || !reconciled[doneHandle] {
			t.Fatalf("Restore() reconciled = %+v, want both %v and %v", report.Reconciled, longHandle, doneHandle)
		}

		longAfter := decodeProcessResult(t, runCall(t, output, fmt.Sprintf(`{"process_id":%q}`, longHandle)))
		if longAfter.Status != string(process.StateLostOnRestore) {
			t.Fatalf("still-running process restored status = %q, want %q", longAfter.Status, process.StateLostOnRestore)
		}

		doneAfter := decodeProcessResult(t, runCall(t, output, fmt.Sprintf(`{"process_id":%q}`, doneHandle)))
		if doneAfter.Status != string(process.StateExited) {
			t.Fatalf("already-terminal process restored status = %q, want %q", doneAfter.Status, process.StateExited)
		}
		if !strings.Contains(doneAfter.Output, "finished-before-restore") {
			t.Fatalf("restored spool output = %q, want it to contain the original output", doneAfter.Output)
		}
	})
}

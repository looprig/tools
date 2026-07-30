//go:build integration

package bash_test

// integration_test.go is Task 20's tagged acceptance seam for the
// supervised Bash workflow (docs/plans/2026-07-27-long-running-command-
// supervision.md, Task 20): it composes the PUBLIC Harness binding contracts
// (tool.Bindings/tool.ProcessBinding/tool.SessionResourceRegistry) with a
// contract-faithful Tools test registry and drives the root package's own
// public definition builders (BashDefinition, ProcessOutputDefinition,
// ProcessInputDefinition, ProcessStopDefinition) exactly as a real consumer
// would, never reaching into any unexported field of bash or process.
//
// This file lives in the external `bash_test` package (not `bash`)
// specifically so it can import the root `github.com/looprig/tools` package
// (BashDefinition et al.) without an import cycle: the root package imports
// `github.com/looprig/tools/bash`, so an internal `package bash` test file
// could never import it back, but an external `bash_test` package can,
// because the root package never imports `bash_test`.
//
// Tools cannot import Harness's internal/sessionruntime, so this file's
// tool.SessionResourceRegistry is a local fake mirroring the real registry's
// get-or-create contract (SessionResourceRegistry's doc comment: "atomically
// resolves one session-owned resource by key... factory receives a private
// storage directory reserved for that key"). Real registry composition
// against the genuine Harness session runtime is reserved for Coderig Task
// 28, not this module.
//
// The injected tool.AsyncProcessRunner is backed by a real os/exec.Cmd
// (execAsyncRunner/execPreparedProcess/execProcess below), mirroring
// process/supervisor_integration_test.go's execPreparedProcess/execProcess
// and process/fake_process_test.go's documented reason for existing at all:
// this suite exercises real process I/O (real OS pipes, a real subprocess,
// real signal delivery) end to end through BashDefinition's resolver seam,
// not a purely synthetic fake.
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
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools"
	"github.com/looprig/tools/process"
)

// --- contract-faithful Tools test registry ---------------------------------

// fakeRegistry is a tool.SessionResourceRegistry test double that mirrors the
// real registry's get-or-create semantics: a given key's factory runs AT
// MOST ONCE, and every caller (regardless of which package or definition
// asked, and regardless of call order) receives the exact same resource
// back afterward. This is the load-bearing property "a companion definition
// may create the same runner-free supervisor before Bash is built" depends
// on: whichever of the four process-backed definitions' GetOrCreate call
// reaches a key first is the one that actually runs the factory.
type fakeRegistry struct {
	dir string

	mu        sync.Mutex
	resources map[string]tool.SessionResource
	calls     map[string]int
	err       error
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	return &fakeRegistry{dir: t.TempDir(), resources: map[string]tool.SessionResource{}, calls: map[string]int{}}
}

func (r *fakeRegistry) GetOrCreate(_ context.Context, key string, factory func(string) (tool.SessionResource, error)) (tool.SessionResource, error) {
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
	r.calls[key]++
	return res, nil
}

// callCount reports how many times GetOrCreate's factory actually ran for
// key, letting a test prove "separately built definitions in one session
// obtain the same supervisor registry entry" (the count never exceeds 1
// across every definition that shares this registry).
func (r *fakeRegistry) callCount(key string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[key]
}

var _ tool.SessionResourceRegistry = (*fakeRegistry)(nil)

// --- contract-faithful Harness Workspace binding capabilities --------------

// fakeCoordinator is a tool.WorkspaceCoordinator AND
// tool.WorkspaceLifetimeCoordinator test double: Acquire/AcquireLifetime
// always succeed, recording every Release.
type fakeCoordinator struct {
	mu            sync.Mutex
	lifetimeCalls int
	released      int
}

func (c *fakeCoordinator) Acquire(context.Context, tool.WorkspaceOperation, string) (tool.WorkspacePermit, error) {
	return fakePermit{c}, nil
}

func (c *fakeCoordinator) Healthy() error { return nil }

func (c *fakeCoordinator) AcquireLifetime(context.Context, tool.WorkspaceAccess) (tool.WorkspacePermit, error) {
	c.mu.Lock()
	c.lifetimeCalls++
	c.mu.Unlock()
	return fakePermit{c}, nil
}

type fakePermit struct{ c *fakeCoordinator }

func (p fakePermit) Release() {
	p.c.mu.Lock()
	p.c.released++
	p.c.mu.Unlock()
}

var (
	_ tool.WorkspaceCoordinator         = (*fakeCoordinator)(nil)
	_ tool.WorkspaceLifetimeCoordinator = (*fakeCoordinator)(nil)
)

// fakeObservations is a race-safe tool.WorkspaceObservations test double: a
// supervised call's detached watchAndInvalidate goroutine can call
// InvalidateAll concurrently with a later supervised call in the same test
// (mirroring bash/supervised_test.go's syncWorkspaceObservations).
type fakeObservations struct{ invalidated atomic.Int64 }

func (*fakeObservations) WithPath(string, func(*tool.FileObservation) error) error { return nil }
func (o *fakeObservations) InvalidateAll()                                         { o.invalidated.Add(1) }

var _ tool.WorkspaceObservations = (*fakeObservations)(nil)

// --- real-subprocess-backed tool.AsyncProcessRunner -------------------------

// execAsyncRunner is a tool.AsyncProcessRunner backed by a real os/exec.Cmd
// per PrepareProcess call, mirroring process/supervisor_integration_test.go's
// execPreparedProcess/execProcess: PrepareProcess never spawns anything (the
// documented AsyncProcessRunner contract), it only builds the exec.Cmd;
// Start is what actually spawns the real OS process.
type execAsyncRunner struct {
	mu    sync.Mutex
	calls int
}

func (r *execAsyncRunner) PrepareProcess(_ context.Context, req tool.ProcessRequest) (tool.PreparedProcess, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	cmd := exec.Command("sh", "-c", req.Command)
	cmd.Dir = req.Directory
	return &execPreparedProcess{cmd: cmd, access: tool.NewWorkspaceAccess(tool.WorkspaceAccessReadOnly, nil, nil)}, nil
}

var _ tool.AsyncProcessRunner = (*execAsyncRunner)(nil)

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

// Close releases an unstarted preparation; execPreparedProcess reserves
// nothing of its own before Start, so this is a no-op, idempotent by
// construction.
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

// Signal maps the portable kill signal to a real SIGKILL and every other
// portable signal to os.Interrupt -- the only other signal value the os
// package guarantees is portable across platforms -- exactly mirroring
// process/supervisor_integration_test.go's execProcess.Signal.
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

// --- call-driving helpers ---------------------------------------------------

func mustUUID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() error = %v", err)
	}
	return id
}

// runCall drives tl through the exact PrepareCall -> loop.WithPreparedCall
// -> InvokableRun sequence the real runner uses, and returns the decoded
// text of the single content block every tool in this suite returns.
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

// bashCallResult mirrors bash/result.go's unexported supervisedResult JSON
// shape -- this external test package cannot see the unexported type, so it
// decodes against the identical wire contract instead.
type bashCallResult struct {
	Status       string `json:"status"`
	ProcessID    string `json:"process_id"`
	Output       string `json:"output"`
	NextCursor   int64  `json:"next_cursor"`
	ExitCode     *int   `json:"exit_code"`
	Reason       string `json:"reason"`
	StartedAt    string `json:"started_at"`
	FinishedAt   string `json:"finished_at"`
	DurationMS   *int64 `json:"duration_ms"`
	Backgrounded bool   `json:"backgrounded"`
	Error        string `json:"error"`
}

func decodeBashResult(t *testing.T, text string) bashCallResult {
	t.Helper()
	var out bashCallResult
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("decode bash result %q: %v", text, err)
	}
	return out
}

// processCallResult mirrors process's unexported processOutputResult AND
// processStopResult JSON shapes: the two share every field this suite reads
// (process_id/status/output/next_cursor/exit_code/reason/error), so one
// decode target serves ProcessOutput, ProcessInput (which renders the exact
// same snapshot shape), and ProcessStop alike -- a field absent from a given
// tool's JSON simply decodes to its zero value.
type processCallResult struct {
	ProcessID  string `json:"process_id"`
	Status     string `json:"status"`
	Output     string `json:"output"`
	NextCursor int64  `json:"next_cursor"`
	TotalBytes int64  `json:"total_bytes"`
	ExitCode   *int   `json:"exit_code"`
	Reason     string `json:"reason"`
	Error      string `json:"error"`
}

func decodeProcessResult(t *testing.T, text string) processCallResult {
	t.Helper()
	var out processCallResult
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("decode process result %q: %v", text, err)
	}
	return out
}

type processCallResults struct {
	Results []processCallResult `json:"results"`
}

// --- TestIntegrationBashSupervisedWorkflow ----------------------------------

// TestIntegrationBashSupervisedWorkflow drives the supervised Bash workflow
// end to end through the PUBLIC BashDefinition/ProcessOutputDefinition/
// ProcessInputDefinition/ProcessStopDefinition builders, a contract-faithful
// fake Harness registry, and a real os/exec.Cmd-backed AsyncProcessRunner.
// It covers: the resolver contract (bound LoopID in, concrete runner out), a
// companion definition creating the shared runner-free supervisor BEFORE
// Bash is ever built, foreground compatibility (the unchanged synchronous
// path), background start, yield, incremental output, wait-many, input, and
// stop.
func TestIntegrationBashSupervisedWorkflow(t *testing.T) {
	registry := newFakeRegistry(t)
	coord := &fakeCoordinator{}
	obs := &fakeObservations{}
	sessionID := mustUUID(t)
	loopID := mustUUID(t)

	bindings := tool.Bindings{
		SessionID: sessionID,
		LoopID:    loopID,
		Workspace: &tool.WorkspaceBinding{Root: t.TempDir(), Coordinator: coord, Observations: obs},
		Process:   &tool.ProcessBinding{Registry: registry},
	}

	// --- a companion definition creates the shared runner-free supervisor
	// BEFORE Bash is ever built ---
	//
	// ProcessOutputDefinition, ProcessInputDefinition, and ProcessStopDefinition
	// are all argument-free and runner-free (definitions.go); building one of
	// them first, against the identical bindings/registry BashDefinition will
	// later share, is exactly the race direction the design note calls out:
	// "any of the four process-backed definitions' GetOrCreate call reaches
	// this key FIRST still hands every later caller ... a resource it can
	// type-assert successfully."
	outputBuilt, err := tools.ProcessOutputDefinition().Build(context.Background(), bindings)
	if err != nil {
		t.Fatalf("ProcessOutputDefinition().Build() error = %v", err)
	}
	processOutput := outputBuilt[0]
	if got := registry.callCount(process.SupervisorResourceKey); got != 1 {
		t.Fatalf("supervisor factory calls after the companion ProcessOutput Build = %d, want 1", got)
	}

	// --- resolver contract: BashDefinition calls the resolver exactly once,
	// with the bound LoopID, and only Bash ever receives a resolver ---
	var (
		resolverMu    sync.Mutex
		resolverCalls int
		gotLoopID     uuid.UUID
	)
	runner := &execAsyncRunner{}
	resolver := func(_ context.Context, id uuid.UUID) (tool.AsyncProcessRunner, error) {
		resolverMu.Lock()
		resolverCalls++
		gotLoopID = id
		resolverMu.Unlock()
		return runner, nil
	}

	bashDef := tools.BashDefinition(resolver)
	if want := tool.RequiresWorkspace | tool.RequiresProcessServices; bashDef.Requirements() != want {
		t.Fatalf("BashDefinition Requirements() = %v, want %v", bashDef.Requirements(), want)
	}

	bashBuilt, err := bashDef.Build(context.Background(), bindings)
	if err != nil {
		t.Fatalf("BashDefinition().Build() error = %v", err)
	}
	bashTool := bashBuilt[0]

	resolverMu.Lock()
	if resolverCalls != 1 {
		t.Fatalf("resolver called %d times, want 1", resolverCalls)
	}
	if gotLoopID != loopID {
		t.Fatalf("resolver received LoopID = %v, want the bound %v", gotLoopID, loopID)
	}
	resolverMu.Unlock()

	// Build() alone never touches the process registry (BashDefinition only
	// resolves the runner and seals options at Build; runSupervised resolves
	// the shared supervisor lazily, at the first supervised invocation).
	if got := registry.callCount(process.SupervisorResourceKey); got != 1 {
		t.Fatalf("supervisor factory calls after BashDefinition Build() = %d, want 1 (still only the companion's)", got)
	}

	// --- foreground compatibility: a legacy call (no background, no
	// yield_time_ms) stays on the unchanged synchronous `sh -c` path, through
	// a real OS subprocess, and never touches the process registry at all ---
	fg := runCall(t, bashTool, `{"command":"printf hello"}`)
	if !strings.Contains(fg, "hello") || !strings.Contains(fg, "[exit code: 0]") {
		t.Fatalf("foreground result = %q, want output containing %q and %q", fg, "hello", "[exit code: 0]")
	}
	if got := registry.callCount(process.SupervisorResourceKey); got != 1 {
		t.Fatalf("supervisor factory calls after a legacy foreground call = %d, want 1 (a legacy call never touches the process registry)", got)
	}

	// --- background start ---
	//
	// `cat` blocks reading its own stdin, so it stays live until EOF -- ideal
	// for chaining incremental output and input below onto the same handle.
	bg := decodeBashResult(t, runCall(t, bashTool, `{"command":"cat","background":true}`))
	if !bg.Backgrounded || bg.ProcessID == "" {
		t.Fatalf("background result = %+v, want a backgrounded live handle", bg)
	}
	if bg.Status != "running" {
		t.Fatalf("background result Status = %q, want %q (a LIVE result)", bg.Status, "running")
	}

	// Bash's own runSupervised resolved the SAME shared supervisor the
	// companion ProcessOutput definition created above -- this is the load-
	// bearing proof of the fixed shared-resource race: it must still be 1,
	// never 2.
	if got := registry.callCount(process.SupervisorResourceKey); got != 1 {
		t.Fatalf("supervisor factory calls after the first supervised call = %d, want 1 (Bash must reuse, not recreate, the companion's shared supervisor)", got)
	}
	handle := bg.ProcessID

	// --- input + incremental output ---
	inputBuilt, err := tools.ProcessInputDefinition().Build(context.Background(), bindings)
	if err != nil {
		t.Fatalf("ProcessInputDefinition().Build() error = %v", err)
	}
	processInput := inputBuilt[0]

	writeResult := decodeProcessResult(t, runCall(t, processInput, fmt.Sprintf(`{"process_id":%q,"data":"hello-input\n","yield_time_ms":2000}`, handle)))
	if writeResult.Error != "" {
		t.Fatalf("ProcessInput write result = %+v, want no error", writeResult)
	}
	if !strings.Contains(writeResult.Output, "hello-input") {
		t.Fatalf("ProcessInput snapshot output = %q, want it to contain the echoed input", writeResult.Output)
	}

	// Incremental output: reading again from the returned NextCursor observes
	// nothing new (cat only ever echoes what it is fed, and nothing new has
	// been fed since).
	poll := decodeProcessResult(t, runCall(t, processOutput, fmt.Sprintf(`{"process_id":%q,"cursor":%d}`, handle, writeResult.NextCursor)))
	if poll.Error != "" {
		t.Fatalf("ProcessOutput poll result = %+v, want no error", poll)
	}
	if poll.Output != "" {
		t.Fatalf("ProcessOutput poll at NextCursor = %+v, want no new output yet", poll)
	}

	// EOF closes cat's stdin, letting it exit; the returned snapshot observes
	// the confirmed terminal state.
	eofResult := decodeProcessResult(t, runCall(t, processInput, fmt.Sprintf(`{"process_id":%q,"eof":true,"yield_time_ms":2000}`, handle)))
	if eofResult.Status != string(process.StateExited) {
		t.Fatalf("ProcessInput eof result status = %q, want %q", eofResult.Status, process.StateExited)
	}

	// --- yield ---
	//
	// A fast command with a generous budget returns a TERMINAL result
	// carrying the real exit code from the process's own durable manifest.
	yieldedTerminal := decodeBashResult(t, runCall(t, bashTool, `{"command":"printf yielded","yield_time_ms":5000}`))
	if yieldedTerminal.ProcessID != "" {
		t.Fatalf("yielded (exited within budget) ProcessID = %q, want empty", yieldedTerminal.ProcessID)
	}
	if yieldedTerminal.Status != string(process.StateExited) {
		t.Fatalf("yielded (exited within budget) Status = %q, want %q", yieldedTerminal.Status, process.StateExited)
	}
	if yieldedTerminal.ExitCode == nil || *yieldedTerminal.ExitCode != 0 {
		t.Fatalf("yielded (exited within budget) ExitCode = %v, want 0", yieldedTerminal.ExitCode)
	}
	if yieldedTerminal.Output != "yielded" {
		t.Fatalf("yielded (exited within budget) Output = %q, want %q (the command's actual printed output, not empty)", yieldedTerminal.Output, "yielded")
	}

	// A slow command with a short budget returns a LIVE result: a handle and
	// backgrounded:true, without waiting for completion.
	yieldedLive := decodeBashResult(t, runCall(t, bashTool, `{"command":"sleep 5","yield_time_ms":20}`))
	if yieldedLive.ProcessID == "" || !yieldedLive.Backgrounded {
		t.Fatalf("yielded (still running) result = %+v, want a backgrounded live handle", yieldedLive)
	}

	// --- wait-many ---
	twinA := decodeBashResult(t, runCall(t, bashTool, `{"command":"printf twinA","background":true}`))
	twinB := decodeBashResult(t, runCall(t, bashTool, `{"command":"printf twinB","background":true}`))
	if twinA.ProcessID == "" || twinB.ProcessID == "" {
		t.Fatalf("twin background results = %+v / %+v, want non-empty handles", twinA, twinB)
	}

	// process/output_tool.go's awaitTargets treats a target as already
	// satisfied when "cursor < total_bytes || terminal" (its own doc
	// comment), and a process_ids call carries exactly one shared `cursor`
	// for every target (processOutputArgs has a single `Cursor *int64`,
	// never a per-target one). Waiting with the default cursor:0 is
	// therefore trivially satisfied the instant EITHER twin has produced
	// any output at all -- it proves nothing about blocking until
	// terminal. To actually exercise "blocks until terminal", first learn
	// each twin's own fully-written byte count via an individual wait:any
	// poll (printf writes its whole argument in one spool append, so by
	// the time this returns, TotalBytes is that twin's FINAL byte count,
	// with no more output still to arrive), then reissue wait:all with
	// that count as the shared cursor: cursor == total_bytes can no
	// longer be satisfied by "has new output", so a terminal result below
	// can only mean wait:all genuinely waited for terminal.
	firstA := decodeProcessResult(t, runCall(t, processOutput, fmt.Sprintf(`{"process_id":%q,"cursor":0,"wait":"any","timeout_ms":5000}`, twinA.ProcessID)))
	firstB := decodeProcessResult(t, runCall(t, processOutput, fmt.Sprintf(`{"process_id":%q,"cursor":0,"wait":"any","timeout_ms":5000}`, twinB.ProcessID)))
	if firstA.TotalBytes == 0 || firstB.TotalBytes == 0 {
		t.Fatalf("twin first-output snapshots = %+v / %+v, want nonzero total_bytes for both", firstA, firstB)
	}
	if firstA.TotalBytes != firstB.TotalBytes {
		t.Fatalf("twin total_bytes = %d / %d, want equal (printf twinA and printf twinB are both 5-byte outputs) so one shared wait:all cursor is valid for both", firstA.TotalBytes, firstB.TotalBytes)
	}
	sharedCursor := firstA.TotalBytes

	waitAllText := runCall(t, processOutput, fmt.Sprintf(`{"process_ids":[%q,%q],"cursor":%d,"wait":"all","timeout_ms":5000}`, twinA.ProcessID, twinB.ProcessID, sharedCursor))
	var multi processCallResults
	if err := json.Unmarshal([]byte(waitAllText), &multi); err != nil {
		t.Fatalf("decode wait:all result %q: %v", waitAllText, err)
	}
	if len(multi.Results) != 2 {
		t.Fatalf("wait:all results = %+v, want exactly 2 entries", multi.Results)
	}
	if multi.Results[0].ProcessID != twinA.ProcessID || multi.Results[1].ProcessID != twinB.ProcessID {
		t.Fatalf("wait:all results order = %+v, want [%s, %s] (input order preserved)", multi.Results, twinA.ProcessID, twinB.ProcessID)
	}
	for _, r := range multi.Results {
		if !process.State(r.Status).Terminal() {
			t.Errorf("wait:all entry %+v, want a terminal status (cursor == each twin's already-observed total_bytes, so no new output can satisfy the wait -- wait:all can only have returned because every target reached terminal)", r)
		}
	}

	// --- stop ---
	stopBuilt, err := tools.ProcessStopDefinition().Build(context.Background(), bindings)
	if err != nil {
		t.Fatalf("ProcessStopDefinition().Build() error = %v", err)
	}
	processStop := stopBuilt[0]

	longRun := decodeBashResult(t, runCall(t, bashTool, `{"command":"sleep 30","background":true}`))
	if longRun.ProcessID == "" {
		t.Fatalf("long-running background result = %+v, want a non-empty handle", longRun)
	}
	stopResult := decodeProcessResult(t, runCall(t, processStop, fmt.Sprintf(`{"process_id":%q,"mode":"terminate","grace_ms":2000}`, longRun.ProcessID)))
	if stopResult.Error != "" {
		t.Fatalf("ProcessStop result = %+v, want no error", stopResult)
	}
	if !process.State(stopResult.Status).Terminal() {
		t.Fatalf("ProcessStop result status = %q, want a terminal state (a stop result is not successful until Sandbox confirms the process tree has exited)", stopResult.Status)
	}

	// still-running yielded process: stop it too so nothing outlives the test.
	stillLive := decodeProcessResult(t, runCall(t, processStop, fmt.Sprintf(`{"process_id":%q,"mode":"kill"}`, yieldedLive.ProcessID)))
	if stillLive.Error != "" {
		t.Fatalf("ProcessStop(kill) on the still-running yielded process = %+v, want no error", stillLive)
	}
	if !process.State(stillLive.Status).Terminal() {
		t.Fatalf("ProcessStop(kill) result status = %q, want a terminal state", stillLive.Status)
	}
}

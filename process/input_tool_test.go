package process

// input_tool_test.go tests the ProcessInput tool (input_tool.go) against
// the spec's "ProcessInput API" section. It follows the same two-layer
// shape output_tool_test.go uses: PrepareCall validation is tested
// directly, while InvokableRun behavior is tested through the full
// prepared flow (PrepareCall -> loop.WithPreparedCall -> InvokableRun).
//
// Most fixtures reuse existing same-package test helpers: newTestSupervisor/
// testOwner/testOrigin/testHandle (manifest_test.go, supervisor_test.go),
// mustUUID (identity_test.go), and decodeSingle/textOf (output_tool_test.go)
// -- ProcessInput renders the exact same processOutputResult shape, so its
// tests decode results the same way ProcessOutput's do. Only the handful of
// helpers specific to driving a live, writable process -- inputFakeStdin,
// inputFakeProcess, and newInputEntry -- are added here. fake_process_test.go's
// shared fakeProcess is deliberately left untouched: its Stdin() is a fixed,
// unobservable io.Discard sink, which cannot support these tests' need to
// capture, block, or react to writes.

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

// --- fixtures ---

// inputFakeStdin is a deterministic, test-controlled io.WriteCloser
// standing in for the Harness Process.Stdin() writer -- mirroring its
// documented contract ("The returned stdin supports concurrent Write and
// Close calls. Closing it is idempotent ... and causes later writes to
// fail"). block, when set, makes every Write hang until it is closed,
// simulating a process that never reads its stdin. onWrite, when set, is
// invoked with exactly the bytes one successful Write captured, letting a
// test simulate a PTY echoing typed input straight back into the entry's
// own combined output stream.
type inputFakeStdin struct {
	mu         sync.Mutex
	written    []byte
	closed     bool
	writeCalls int
	closeCalls int

	block   chan struct{}
	onWrite func([]byte)
}

func (s *inputFakeStdin) Write(p []byte) (int, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	block := s.block
	s.mu.Unlock()

	if block != nil {
		<-block
	}

	s.mu.Lock()
	s.writeCalls++
	s.written = append(s.written, p...)
	onWrite := s.onWrite
	s.mu.Unlock()

	if onWrite != nil {
		onWrite(append([]byte(nil), p...))
	}
	return len(p), nil
}

func (s *inputFakeStdin) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.closeCalls++
	return nil
}

func (s *inputFakeStdin) writtenBytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.written...)
}

func (s *inputFakeStdin) writeCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeCalls
}

func (s *inputFakeStdin) closeCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeCalls
}

var _ io.WriteCloser = (*inputFakeStdin)(nil)

// resizeCall records one Resize invocation's exact arguments.
type resizeCall struct{ rows, cols uint16 }

// inputFakeProcess is a minimal, deterministic tool.Process double specific
// to this file: unlike fake_process_test.go's shared fakeProcess (whose
// Stdin() is hardcoded to a fixed, unobservable io.Discard sink),
// ProcessInput's tests need a controllable Stdin, so this file defines its
// own rather than extending the shared fixture.
type inputFakeProcess struct {
	stdin      *inputFakeStdin
	streamMode tool.ProcessStreamMode
	resizeErr  error

	mu    sync.Mutex
	calls []resizeCall
}

func (p *inputFakeProcess) Stdout() io.ReadCloser { return io.NopCloser(strings.NewReader("")) }
func (p *inputFakeProcess) Stderr() io.ReadCloser { return io.NopCloser(strings.NewReader("")) }
func (p *inputFakeProcess) Stdin() io.WriteCloser { return p.stdin }

func (p *inputFakeProcess) StreamMode() tool.ProcessStreamMode {
	if p.streamMode == 0 {
		return tool.ProcessStreamModePipes
	}
	return p.streamMode
}

func (p *inputFakeProcess) Wait(context.Context) (tool.ProcessResult, error) {
	return tool.ProcessResult{}, nil
}

func (p *inputFakeProcess) Resize(_ context.Context, rows, cols uint16) error {
	p.mu.Lock()
	p.calls = append(p.calls, resizeCall{rows: rows, cols: cols})
	p.mu.Unlock()
	return p.resizeErr
}

func (p *inputFakeProcess) Signal(context.Context, tool.ProcessSignal) error { return nil }
func (p *inputFakeProcess) Close(context.Context) error                      { return nil }

func (p *inputFakeProcess) resizeCalls() []resizeCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]resizeCall(nil), p.calls...)
}

func (p *inputFakeProcess) resizeCallCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

var _ tool.Process = (*inputFakeProcess)(nil)

// newInputEntry builds and registers a live, non-terminal *entry for
// owner/handle directly against sup's registry (mirrors output_tool_test.go's
// newOutputEntry), with its live tool.Process set to proc -- unlike
// ProcessOutput, ProcessInput always needs a live, writable process.
func newInputEntry(t *testing.T, sup *Supervisor, owner Owner, handle Handle, proc tool.Process) *entry {
	t.Helper()
	spool, err := OpenSpool(sup.spoolRoot, handle, 0)
	if err != nil {
		t.Fatalf("OpenSpool() error = %v, want nil", err)
	}
	e := &entry{
		identity:  Identity{Handle: handle, Owner: owner, Origin: testOrigin(t)},
		manifests: sup.manifests,
		process:   proc,
		buffer:    NewBuffer(0),
		spool:     spool,
		done:      make(chan struct{}),
		wake:      make(chan struct{}),
	}
	registerEntry(t, sup, e)
	return e
}

func prepareInput(t *testing.T, tl *ProcessInputTool, argsJSON string) (tool.Request, tool.PreparedArtifact) {
	t.Helper()
	req, art, err := tl.PrepareCall(context.Background(), mustUUID(t), argsJSON)
	if err != nil {
		t.Fatalf("PrepareCall(%s) error = %v, want nil", argsJSON, err)
	}
	return req, art
}

func prepareInputErr(t *testing.T, tl *ProcessInputTool, argsJSON string) error {
	t.Helper()
	_, _, err := tl.PrepareCall(context.Background(), mustUUID(t), argsJSON)
	if err == nil {
		t.Fatalf("PrepareCall(%s) error = nil, want an error", argsJSON)
	}
	return err
}

func runInputCtx(t *testing.T, ctx context.Context, tl *ProcessInputTool, argsJSON string) string {
	t.Helper()
	req, art, err := tl.PrepareCall(ctx, mustUUID(t), argsJSON)
	if err != nil {
		t.Fatalf("PrepareCall(%s) error = %v, want nil", argsJSON, err)
	}
	prepCtx := loop.WithPreparedCall(ctx, tool.PreparedCall{Request: req, Artifact: art})
	result, err := tl.InvokableRun(prepCtx, argsJSON)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v, want nil (ProcessInput never returns a Go error)", err)
	}
	return textOf(t, result)
}

func runInput(t *testing.T, tl *ProcessInputTool, argsJSON string) string {
	t.Helper()
	return runInputCtx(t, context.Background(), tl, argsJSON)
}

// --- Info ---

func TestProcessInputInfo(t *testing.T) {
	t.Parallel()
	tl := NewProcessInput(newTestSupervisor(t, Config{}), testOwner(t))
	info, err := tl.Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error = %v, want nil", err)
	}
	if info.Name != "ProcessInput" {
		t.Errorf("Info().Name = %q, want %q", info.Name, "ProcessInput")
	}
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(info.Schema, &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	if _, ok := schema["properties"]; !ok {
		t.Error("schema has no \"properties\" key")
	}
}

// --- PrepareCall: empty-operation rejection, resize validation, malformed input ---

// TestProcessInputPrepareCallInvalid table-drives PrepareCall rejection
// cases: malformed JSON/handle, the "at least one of data, EOF, or resize"
// rule (including that an explicit eof:false requests nothing), resize
// argument-shape validation (rows/cols must be supplied together, in
// range), and cursor/yield_time_ms bounds.
func TestProcessInputPrepareCallInvalid(t *testing.T) {
	t.Parallel()
	h := string(testHandle(t, 1))

	cases := map[string]string{
		"not json":                      `not json`,
		"missing process_id":            `{}`,
		"empty process_id":              `{"process_id":""}`,
		"malformed process_id":          `{"process_id":"not-a-valid-handle"}`,
		"no operation requested":        `{"process_id":"` + h + `"}`,
		"eof false is not an operation": `{"process_id":"` + h + `","eof":false}`,
		"rows without cols":             `{"process_id":"` + h + `","rows":24}`,
		"cols without rows":             `{"process_id":"` + h + `","cols":80}`,
		"zero rows":                     `{"process_id":"` + h + `","rows":0,"cols":80}`,
		"negative cols":                 `{"process_id":"` + h + `","rows":24,"cols":-1}`,
		"rows too large":                `{"process_id":"` + h + `","rows":65536,"cols":80}`,
		"cols too large":                `{"process_id":"` + h + `","rows":24,"cols":65536}`,
		"negative cursor":               `{"process_id":"` + h + `","data":"x","cursor":-1}`,
		"negative yield_time_ms":        `{"process_id":"` + h + `","data":"x","yield_time_ms":-1}`,
	}

	for name, argsJSON := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tl := NewProcessInput(newTestSupervisor(t, Config{}), testOwner(t))
			prepareInputErr(t, tl, argsJSON)
		})
	}
}

// TestProcessInputPrepareCallNormalizesAllFields proves a call supplying
// every field normalizes onto the artifact exactly, and produces an empty
// (no Requirements) valid Request -- the "resize validation" positive path
// plus the optional cursor/yield_time_ms presence-awareness.
func TestProcessInputPrepareCallNormalizesAllFields(t *testing.T) {
	t.Parallel()
	tl := NewProcessInput(newTestSupervisor(t, Config{}), testOwner(t))
	h := testHandle(t, 1)

	argsJSON := `{"process_id":"` + string(h) + `","data":"hi","cursor":5,"eof":true,"rows":24,"cols":80,"yield_time_ms":250}`
	req, artifact := prepareInput(t, tl, argsJSON)

	if req.ToolName != "ProcessInput" {
		t.Errorf("Request.ToolName = %q, want %q", req.ToolName, "ProcessInput")
	}
	if len(req.Requirements) != 0 {
		t.Errorf("Requirements = %+v, want none (empty effect request)", req.Requirements)
	}
	if err := tool.ValidateRequest(req); err != nil {
		t.Fatalf("ValidateRequest() error = %v, want nil", err)
	}

	art, ok := artifact.(*processInputArtifact)
	if !ok {
		t.Fatalf("artifact type = %T, want *processInputArtifact", artifact)
	}
	if art.handle != h {
		t.Errorf("handle = %v, want %v", art.handle, h)
	}
	if !art.hasData || string(art.data) != "hi" {
		t.Errorf("hasData/data = %v/%q, want true/%q", art.hasData, art.data, "hi")
	}
	if !art.eof {
		t.Error("eof = false, want true")
	}
	if !art.resize || art.rows != 24 || art.cols != 80 {
		t.Errorf("resize/rows/cols = %v/%d/%d, want true/24/80", art.resize, art.rows, art.cols)
	}
	if art.cursor == nil || *art.cursor != 5 {
		t.Errorf("cursor = %v, want *5", art.cursor)
	}
	if !art.hasYield || art.yieldMS != 250 {
		t.Errorf("hasYield/yieldMS = %v/%d, want true/250", art.hasYield, art.yieldMS)
	}
}

// --- InvokableRun: data writes ---

// TestProcessInputDataWrite proves data reaches the process's Stdin
// verbatim and the call reports no error.
func TestProcessInputDataWrite(t *testing.T) {
	t.Parallel()
	sup := newTestSupervisor(t, Config{})
	owner := testOwner(t)
	h := testHandle(t, 1)
	stdin := &inputFakeStdin{}
	proc := &inputFakeProcess{stdin: stdin}
	newInputEntry(t, sup, owner, h, proc)

	tl := NewProcessInput(sup, owner)
	text := runInput(t, tl, `{"process_id":"`+string(h)+`","data":"hello"}`)
	got := decodeSingle(t, text)

	if got.Error != "" {
		t.Fatalf("Error = %q, want empty", got.Error)
	}
	if string(stdin.writtenBytes()) != "hello" {
		t.Errorf("stdin received %q, want %q", stdin.writtenBytes(), "hello")
	}
}

// --- InvokableRun: bounded backpressure ---

// TestProcessInputBackpressure proves both halves of the spec's "bounded"
// requirement: data larger than the configured MaxPendingInputBytes
// ceiling is rejected before ever touching the process, and a write that
// never completes (a process that does not consume input) still returns
// promptly with CodeInputBackpressure rather than blocking indefinitely.
func TestProcessInputBackpressure(t *testing.T) {
	t.Parallel()

	t.Run("oversized data rejected without writing", func(t *testing.T) {
		t.Parallel()
		sup := newTestSupervisor(t, Config{MaxPendingInputBytes: 4})
		owner := testOwner(t)
		h := testHandle(t, 1)
		stdin := &inputFakeStdin{}
		proc := &inputFakeProcess{stdin: stdin}
		newInputEntry(t, sup, owner, h, proc)

		tl := NewProcessInput(sup, owner)
		text := runInput(t, tl, `{"process_id":"`+string(h)+`","data":"hello"}`)
		got := decodeSingle(t, text)

		if got.Error != string(CodeInputBackpressure) {
			t.Errorf("Error = %q, want %q", got.Error, CodeInputBackpressure)
		}
		if stdin.writeCallCount() != 0 {
			t.Errorf("Write was called %d times, want 0 (oversized data must never reach the process)", stdin.writeCallCount())
		}
	})

	t.Run("write that never completes is bounded", func(t *testing.T) {
		t.Parallel()
		sup := newTestSupervisor(t, Config{})
		owner := testOwner(t)
		h := testHandle(t, 1)
		stdin := &inputFakeStdin{block: make(chan struct{})} // never closed: Write hangs forever.
		proc := &inputFakeProcess{stdin: stdin}
		newInputEntry(t, sup, owner, h, proc)

		tl := NewProcessInput(sup, owner)
		start := time.Now()
		text := runInput(t, tl, `{"process_id":"`+string(h)+`","data":"hello"}`)
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("call took %s, want it bounded well under 2s despite the process never consuming input", elapsed)
		}
		got := decodeSingle(t, text)
		if got.Error != string(CodeInputBackpressure) {
			t.Errorf("Error = %q, want %q", got.Error, CodeInputBackpressure)
		}
	})
}

// --- InvokableRun: EOF (pipe idempotence, PTY forwarding) ---

// TestProcessInputEOFIdempotentForPipe proves eof:true against a pipe-mode
// process closes Stdin and that a second eof:true call succeeds again
// rather than erroring (spec: "EOF is idempotent for pipe-backed
// processes").
func TestProcessInputEOFIdempotentForPipe(t *testing.T) {
	t.Parallel()
	sup := newTestSupervisor(t, Config{})
	owner := testOwner(t)
	h := testHandle(t, 1)
	stdin := &inputFakeStdin{}
	proc := &inputFakeProcess{stdin: stdin, streamMode: tool.ProcessStreamModePipes}
	newInputEntry(t, sup, owner, h, proc)

	tl := NewProcessInput(sup, owner)
	for i := 0; i < 2; i++ {
		text := runInput(t, tl, `{"process_id":"`+string(h)+`","eof":true}`)
		got := decodeSingle(t, text)
		if got.Error != "" {
			t.Fatalf("call %d: Error = %q, want empty (EOF is idempotent for pipe-backed processes)", i, got.Error)
		}
	}
	if stdin.closeCallCount() != 2 {
		t.Errorf("Close was called %d times, want 2", stdin.closeCallCount())
	}
	if len(stdin.writtenBytes()) != 0 {
		t.Errorf("stdin received bytes %q for a pipe EOF, want none (pipe EOF closes, never writes)", stdin.writtenBytes())
	}
}

// TestProcessInputEOFForwardsToPTY proves eof:true against a PTY-mode
// process writes a single ASCII EOT (Ctrl-D) byte rather than closing
// Stdin (spec: EOF "maps to terminal input semantics appropriate to the
// platform for PTYs").
func TestProcessInputEOFForwardsToPTY(t *testing.T) {
	t.Parallel()
	sup := newTestSupervisor(t, Config{})
	owner := testOwner(t)
	h := testHandle(t, 1)
	stdin := &inputFakeStdin{}
	proc := &inputFakeProcess{stdin: stdin, streamMode: tool.ProcessStreamModePTY}
	newInputEntry(t, sup, owner, h, proc)

	tl := NewProcessInput(sup, owner)
	text := runInput(t, tl, `{"process_id":"`+string(h)+`","eof":true}`)
	got := decodeSingle(t, text)

	if got.Error != "" {
		t.Fatalf("Error = %q, want empty", got.Error)
	}
	if b := stdin.writtenBytes(); len(b) != 1 || b[0] != 0x04 {
		t.Errorf("stdin received %v, want a single 0x04 (ASCII EOT) byte", b)
	}
	if stdin.closeCallCount() != 0 {
		t.Errorf("Close was called %d times, want 0 (a PTY's EOF is forwarded as a control byte, never a close)", stdin.closeCallCount())
	}
}

// --- InvokableRun: resize ---

// TestProcessInputResizeRejectedForPipeProcess proves a resize request
// against a pipe-mode (non-PTY) process fails closed with
// CodePTYUnavailable and never calls Resize (spec: "Resize is valid only
// for PTY processes").
func TestProcessInputResizeRejectedForPipeProcess(t *testing.T) {
	t.Parallel()
	sup := newTestSupervisor(t, Config{})
	owner := testOwner(t)
	h := testHandle(t, 1)
	stdin := &inputFakeStdin{}
	proc := &inputFakeProcess{stdin: stdin, streamMode: tool.ProcessStreamModePipes}
	newInputEntry(t, sup, owner, h, proc)

	tl := NewProcessInput(sup, owner)
	text := runInput(t, tl, `{"process_id":"`+string(h)+`","rows":24,"cols":80}`)
	got := decodeSingle(t, text)

	if got.Error != string(CodePTYUnavailable) {
		t.Errorf("Error = %q, want %q", got.Error, CodePTYUnavailable)
	}
	if proc.resizeCallCount() != 0 {
		t.Errorf("Resize was called %d times, want 0", proc.resizeCallCount())
	}
}

// TestProcessInputResizeAppliedForPTY proves a resize request against a
// PTY-mode process reaches Resize with the exact requested dimensions.
func TestProcessInputResizeAppliedForPTY(t *testing.T) {
	t.Parallel()
	sup := newTestSupervisor(t, Config{})
	owner := testOwner(t)
	h := testHandle(t, 1)
	stdin := &inputFakeStdin{}
	proc := &inputFakeProcess{stdin: stdin, streamMode: tool.ProcessStreamModePTY}
	newInputEntry(t, sup, owner, h, proc)

	tl := NewProcessInput(sup, owner)
	text := runInput(t, tl, `{"process_id":"`+string(h)+`","rows":24,"cols":80}`)
	got := decodeSingle(t, text)

	if got.Error != "" {
		t.Fatalf("Error = %q, want empty", got.Error)
	}
	calls := proc.resizeCalls()
	if len(calls) != 1 || calls[0].rows != 24 || calls[0].cols != 80 {
		t.Errorf("resizeCalls = %+v, want one call with rows=24 cols=80", calls)
	}
}

// TestProcessInputResizeFailFastNeverWritesOrClosesInSameCall extends the
// standalone resize-rejection case above to a call that ALSO asked for a
// data write and EOF in the same request: applyOperations' documented
// "resize, then data, then EOF" ordering means a resize that fails closed
// (a non-PTY process) must stop data and EOF from ever being attempted in
// that same call, not merely when resize is the only operation requested.
func TestProcessInputResizeFailFastNeverWritesOrClosesInSameCall(t *testing.T) {
	t.Parallel()
	sup := newTestSupervisor(t, Config{})
	owner := testOwner(t)
	h := testHandle(t, 1)
	stdin := &inputFakeStdin{}
	proc := &inputFakeProcess{stdin: stdin, streamMode: tool.ProcessStreamModePipes}
	newInputEntry(t, sup, owner, h, proc)

	tl := NewProcessInput(sup, owner)
	text := runInput(t, tl, `{"process_id":"`+string(h)+`","rows":24,"cols":80,"data":"hello","eof":true}`)
	got := decodeSingle(t, text)

	if got.Error != string(CodePTYUnavailable) {
		t.Errorf("Error = %q, want %q", got.Error, CodePTYUnavailable)
	}
	if proc.resizeCallCount() != 0 {
		t.Errorf("Resize was called %d times, want 0", proc.resizeCallCount())
	}
	if stdin.writeCallCount() != 0 {
		t.Errorf("Write was called %d times, want 0 (a failing resize must stop data from being attempted in the same call)", stdin.writeCallCount())
	}
	if stdin.closeCallCount() != 0 {
		t.Errorf("Close was called %d times, want 0 (a failing resize must stop EOF from being attempted in the same call)", stdin.closeCallCount())
	}
}

// TestProcessInputEOFRepeatedForPTYSendsEOTEachTime proves a PTY-mode
// process's EOF forwarding tolerates repetition exactly like the pipe path's
// own idempotence test (TestProcessInputEOFIdempotentForPipe): a second
// eof:true call against a still-open PTY terminal succeeds again (never an
// error) and writes the EOT byte again — unlike a pipe's Close, forwarding
// EOF as an in-band control byte has nothing to reject on repetition, since
// the terminal's Stdin is never actually closed.
func TestProcessInputEOFRepeatedForPTYSendsEOTEachTime(t *testing.T) {
	t.Parallel()
	sup := newTestSupervisor(t, Config{})
	owner := testOwner(t)
	h := testHandle(t, 1)
	stdin := &inputFakeStdin{}
	proc := &inputFakeProcess{stdin: stdin, streamMode: tool.ProcessStreamModePTY}
	newInputEntry(t, sup, owner, h, proc)

	tl := NewProcessInput(sup, owner)
	for i := 0; i < 2; i++ {
		text := runInput(t, tl, `{"process_id":"`+string(h)+`","eof":true}`)
		got := decodeSingle(t, text)
		if got.Error != "" {
			t.Fatalf("call %d: Error = %q, want empty (a PTY's forwarded EOF must tolerate repetition)", i, got.Error)
		}
	}
	if b := stdin.writtenBytes(); len(b) != 2 || b[0] != 0x04 || b[1] != 0x04 {
		t.Errorf("stdin received %v, want two 0x04 (ASCII EOT) bytes", b)
	}
	if stdin.closeCallCount() != 0 {
		t.Errorf("Close was called %d times, want 0 (a PTY's EOF is forwarded as a control byte, never a close)", stdin.closeCallCount())
	}
}

// --- InvokableRun: cursor semantics ---

// TestProcessInputExplicitCursorOverridesDefault proves a caller-supplied
// cursor is honored verbatim rather than the pre-write default.
func TestProcessInputExplicitCursorOverridesDefault(t *testing.T) {
	t.Parallel()
	sup := newTestSupervisor(t, Config{})
	owner := testOwner(t)
	h := testHandle(t, 1)
	stdin := &inputFakeStdin{}
	proc := &inputFakeProcess{stdin: stdin}
	e := newInputEntry(t, sup, owner, h, proc)
	e.appendChunk([]byte("already here"))

	tl := NewProcessInput(sup, owner)
	text := runInput(t, tl, `{"process_id":"`+string(h)+`","data":"hi","cursor":0}`)
	got := decodeSingle(t, text)

	if got.Error != "" {
		t.Fatalf("Error = %q, want empty", got.Error)
	}
	if got.StartCursor != 0 {
		t.Errorf("StartCursor = %d, want 0", got.StartCursor)
	}
	if got.Output != "already here" {
		t.Errorf("Output = %q, want %q (explicit cursor 0 overrides the pre-write default)", got.Output, "already here")
	}
}

// TestProcessInputOmittedCursorSnapshotsFromPreWriteEnd proves an omitted
// cursor defaults to the combined-output end offset captured BEFORE this
// call's own operation, not to 0 and not to the offset after the
// operation's own side effects: a simulated PTY echo appended during the
// write is visible, but output that already existed before this call is
// not repeated (spec: "at the current end offset captured before the input
// operation when omitted").
func TestProcessInputOmittedCursorSnapshotsFromPreWriteEnd(t *testing.T) {
	t.Parallel()
	sup := newTestSupervisor(t, Config{})
	owner := testOwner(t)
	h := testHandle(t, 1)
	stdin := &inputFakeStdin{}
	proc := &inputFakeProcess{stdin: stdin}
	e := newInputEntry(t, sup, owner, h, proc)
	e.appendChunk([]byte("already here")) // 12 pre-existing bytes.

	stdin.onWrite = func(p []byte) { e.appendChunk(p) } // simulate a PTY echoing typed input back into the combined stream.

	tl := NewProcessInput(sup, owner)
	text := runInput(t, tl, `{"process_id":"`+string(h)+`","data":"hi"}`) // cursor omitted.
	got := decodeSingle(t, text)

	if got.Error != "" {
		t.Fatalf("Error = %q, want empty", got.Error)
	}
	if got.StartCursor != 12 {
		t.Errorf("StartCursor = %d, want 12 (the pre-write end offset, not 0)", got.StartCursor)
	}
	if got.Output != "hi" {
		t.Errorf("Output = %q, want %q (only the bytes produced by this call's own operation)", got.Output, "hi")
	}
}

// --- InvokableRun: optional yield ---

// TestProcessInputYieldTimesOutReturnsSnapshot proves a bounded
// yield_time_ms call that never becomes satisfied still returns, once the
// budget elapses, the current snapshot rather than an error -- mirroring
// ProcessOutput's identical wait-timeout contract.
func TestProcessInputYieldTimesOutReturnsSnapshot(t *testing.T) {
	t.Parallel()
	sup := newTestSupervisor(t, Config{})
	owner := testOwner(t)
	h := testHandle(t, 1)
	stdin := &inputFakeStdin{}
	proc := &inputFakeProcess{stdin: stdin}
	newInputEntry(t, sup, owner, h, proc)

	tl := NewProcessInput(sup, owner)
	start := time.Now()
	text := runInput(t, tl, `{"process_id":"`+string(h)+`","data":"hi","yield_time_ms":20}`)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("call took %s, want it bounded near its 20ms yield_time_ms", elapsed)
	}
	got := decodeSingle(t, text)
	if got.Error != "" {
		t.Errorf("Error = %q, want empty (a yield timeout is not a call failure)", got.Error)
	}
}

// --- InvokableRun: closed input, terminal process, owner isolation ---

// TestProcessInputClosedInputRejectsFurtherWrites proves a data write
// issued after a prior call already closed Stdin (via eof:true) fails with
// CodeStdinClosed rather than silently succeeding or blocking.
func TestProcessInputClosedInputRejectsFurtherWrites(t *testing.T) {
	t.Parallel()
	sup := newTestSupervisor(t, Config{})
	owner := testOwner(t)
	h := testHandle(t, 1)
	stdin := &inputFakeStdin{}
	proc := &inputFakeProcess{stdin: stdin}
	newInputEntry(t, sup, owner, h, proc)

	tl := NewProcessInput(sup, owner)
	if got := decodeSingle(t, runInput(t, tl, `{"process_id":"`+string(h)+`","eof":true}`)); got.Error != "" {
		t.Fatalf("eof call: Error = %q, want empty", got.Error)
	}

	text := runInput(t, tl, `{"process_id":"`+string(h)+`","data":"too late"}`)
	got := decodeSingle(t, text)
	if got.Error != string(CodeStdinClosed) {
		t.Errorf("Error = %q, want %q", got.Error, CodeStdinClosed)
	}
}

// TestProcessInputTerminalProcessRejected proves a call against a process
// whose entry has already reached its terminal state fails closed with
// CodeStdinClosed and never touches the live process's Stdin at all.
func TestProcessInputTerminalProcessRejected(t *testing.T) {
	t.Parallel()
	sup := newTestSupervisor(t, Config{})
	owner := testOwner(t)
	h := testHandle(t, 1)
	stdin := &inputFakeStdin{}
	proc := &inputFakeProcess{stdin: stdin}
	e := newInputEntry(t, sup, owner, h, proc)
	close(e.done)

	tl := NewProcessInput(sup, owner)
	text := runInput(t, tl, `{"process_id":"`+string(h)+`","data":"hi"}`)
	got := decodeSingle(t, text)

	if got.Error != string(CodeStdinClosed) {
		t.Errorf("Error = %q, want %q", got.Error, CodeStdinClosed)
	}
	if stdin.writeCallCount() != 0 {
		t.Errorf("Write was called %d times, want 0 (a terminal process must never be written to)", stdin.writeCallCount())
	}
}

// TestProcessInputCrossOwnerNotFound proves a missing handle and a handle
// owned by a different Owner render the IDENTICAL not_found error, and
// that neither ever reaches the live process's Stdin (spec "Identity and
// authorization").
func TestProcessInputCrossOwnerNotFound(t *testing.T) {
	t.Parallel()
	sup := newTestSupervisor(t, Config{})
	owner := testOwner(t)
	other := testOwner(t)

	foreign := testHandle(t, 1)
	stdin := &inputFakeStdin{}
	proc := &inputFakeProcess{stdin: stdin}
	newInputEntry(t, sup, other, foreign, proc)
	missing := testHandle(t, 2)

	tl := NewProcessInput(sup, owner)
	for _, h := range []Handle{foreign, missing} {
		text := runInput(t, tl, `{"process_id":"`+string(h)+`","data":"hi"}`)
		got := decodeSingle(t, text)
		if got.Error != string(CodeNotFound) {
			t.Errorf("handle %v: Error = %q, want %q", h, got.Error, CodeNotFound)
		}
		if got.Output != "" || got.Status != "" {
			t.Errorf("handle %v: got = %+v, want a bare not_found error with no other fields", h, got)
		}
	}
	if stdin.writeCallCount() != 0 {
		t.Errorf("Write was called %d times, want 0 (a foreign handle must never be written to)", stdin.writeCallCount())
	}
}

// --- InvokableRun: structural guards ---

// TestProcessInputInvokeWithoutPreparedCall proves InvokableRun fails
// closed with the stable invalid_arguments code when invoked outside a
// prepared call.
func TestProcessInputInvokeWithoutPreparedCall(t *testing.T) {
	t.Parallel()
	tl := NewProcessInput(newTestSupervisor(t, Config{}), testOwner(t))
	result, err := tl.InvokableRun(context.Background(), `{"process_id":"x","data":"y"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v, want nil", err)
	}
	got := decodeSingle(t, textOf(t, result))
	if got.Error != string(CodeInvalidArguments) {
		t.Errorf("Error = %q, want %q", got.Error, CodeInvalidArguments)
	}
}

// TestProcessInputUnavailableSupervisor proves a ProcessInputTool
// constructed without a supervisor fails every call closed, at both
// PrepareCall and InvokableRun, rather than panicking on the nil
// dependency.
func TestProcessInputUnavailableSupervisor(t *testing.T) {
	t.Parallel()
	tl := NewProcessInput(nil, testOwner(t))

	if _, _, err := tl.PrepareCall(context.Background(), mustUUID(t), `{"process_id":"x","data":"y"}`); err == nil {
		t.Error("PrepareCall() error = nil, want an error for a nil supervisor")
	}

	result, err := tl.InvokableRun(context.Background(), `{"process_id":"x","data":"y"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v, want nil", err)
	}
	text := textOf(t, result)
	if !strings.Contains(text, string(CodeLifetimeEnforcementUnavailable)) {
		t.Errorf("result = %q, want it to carry %q", text, CodeLifetimeEnforcementUnavailable)
	}
}

package bash

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/process"
)

// recordingSink is a tool.ResultCaptureSink that keeps every byte it is
// offered, so a test can compare the complete captured stream exactly.
type recordingSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *recordingSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *recordingSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// digestSink counts and hashes what it is offered WITHOUT retaining it, so a
// test can stream far more output than it could afford to keep.
type digestSink struct {
	n    int64
	hash hashWriter
	tail [256]byte
}

type hashWriter interface {
	io.Writer
	Sum([]byte) []byte
}

func newDigestSink() *digestSink { return &digestSink{hash: sha256.New()} }

func (s *digestSink) Write(p []byte) (int, error) {
	s.n += int64(len(p))
	_, _ = s.hash.Write(p)
	if len(p) >= len(s.tail) {
		copy(s.tail[:], p[len(p)-len(s.tail):])
	} else {
		copy(s.tail[:], s.tail[len(p):])
		copy(s.tail[len(s.tail)-len(p):], p)
	}
	return len(p), nil
}

// failingSink violates the sink contract (short write plus error) to prove the
// tool never lets a capture defect change the command or its model result.
type failingSink struct{ calls int }

func (s *failingSink) Write([]byte) (int, error) {
	s.calls++
	return 0, io.ErrShortWrite
}

// runCaptured drives the prepared flow through InvokableRunCaptured.
func runCaptured(t *testing.T, ctx context.Context, b *BashTool, argsJSON string, sink tool.ResultCaptureSink) string {
	t.Helper()
	id := mustUUID(t)
	req, art, err := b.PrepareCall(context.Background(), id, argsJSON)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	ctx = loop.WithPreparedCall(ctx, tool.PreparedCall{ExecutionID: id, Request: req, Artifact: art})
	res, err := b.InvokableRunCaptured(ctx, argsJSON, sink)
	if err != nil {
		t.Fatalf("InvokableRunCaptured returned a Go error %v; Bash returns tool-result strings", err)
	}
	return textOf(t, res)
}

func commandArgs(command string) string {
	raw, _ := json.Marshal(map[string]any{"command": command})
	return string(raw)
}

var _ tool.CapturingInvokableTool = (*BashTool)(nil)

// TestBashCapturedSmallOutputEqualsResult proves the property the harness's
// below-threshold path depends on: when nothing was elided, the captured
// stream is byte-identical to the returned result, so no object is retained
// for an ordinary small command.
func TestBashCapturedSmallOutputEqualsResult(t *testing.T) {
	t.Parallel()
	requireSh(t)
	tests := []struct {
		name    string
		command string
		want    string
	}{
		{name: "newline terminated", command: "printf 'hello\\n'", want: "hello\n[exit code: 0]"},
		{name: "no trailing newline", command: "printf 'hello'", want: "hello\n[exit code: 0]"},
		{name: "empty output", command: "true", want: "[exit code: 0]"},
		{name: "non-zero exit", command: "printf 'bad\\n' >&2; exit 3", want: "bad\n[exit code: 3]"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			sink := &recordingSink{}
			got := runCaptured(t, context.Background(), NewBash(t.TempDir()), commandArgs(test.command), sink)
			if got != test.want {
				t.Fatalf("result = %q, want %q", got, test.want)
			}
			if sink.String() != got {
				t.Fatalf("captured = %q, want exactly the result %q", sink.String(), got)
			}
		})
	}
}

// TestBashCapturedLargeOutputStreamsEveryByte proves the complete combined
// stream reaches the sink BEFORE Bash's own 32 KiB head/tail cap, while the
// returned model result keeps exactly its pre-capture bounded shape.
func TestBashCapturedLargeOutputStreamsEveryByte(t *testing.T) {
	t.Parallel()
	requireSh(t)
	// 3000 numbered lines (~60 KiB) on stderr: deterministic, portable, and
	// larger than maxBashOutputBytes.
	command := `i=0; while [ "$i" -lt 3000 ]; do printf 'line %05d of the build log\n' "$i" >&2; i=$((i + 1)); done`
	var want strings.Builder
	for i := range 3000 {
		want.WriteString("line " + leftPad(i) + " of the build log\n")
	}
	want.WriteString("[exit code: 0]")

	sink := &recordingSink{}
	got := runCaptured(t, context.Background(), NewBash(t.TempDir()), commandArgs(command), sink)

	if sink.String() != want.String() {
		t.Fatalf("captured %d bytes, want the complete %d-byte stream plus exit trailer", len(sink.String()), want.Len())
	}
	uncaptured := runPrepared(t, NewBash(t.TempDir()), commandArgs(command), nil)
	if got != uncaptured {
		t.Fatalf("captured result differs from the uncaptured result:\n%q\nvs\n%q", got, uncaptured)
	}
	if !strings.Contains(got, "[output truncated: omitted ") || len(got) > maxBashOutputBytes+256 {
		t.Fatalf("result is %d bytes (truncated marker present = %t); want Bash's own bounded preview", len(got), strings.Contains(got, "truncated"))
	}
}

func leftPad(i int) string {
	s := strconv.Itoa(i)
	return strings.Repeat("0", 5-len(s)) + s
}

// TestBashCapturedTimeoutKeepsStreamedOutput proves a timed-out command's
// partial output is captured (so it can be read back), followed by the same
// error line the model receives, while the returned result is unchanged.
func TestBashCapturedTimeoutKeepsStreamedOutput(t *testing.T) {
	t.Parallel()
	requireSh(t)
	sink := &recordingSink{}
	raw, _ := json.Marshal(map[string]any{"command": "printf 'partial'; exec sleep 5", "timeout": 1})
	got := runCaptured(t, context.Background(), NewBash(t.TempDir()), string(raw), sink)
	if got != "error: command timed out after 1s" {
		t.Fatalf("result = %q, want the unchanged timeout error", got)
	}
	if want := "partial\nerror: command timed out after 1s"; sink.String() != want {
		t.Fatalf("captured = %q, want %q", sink.String(), want)
	}
}

// TestBashCapturedRefusalCapturesTheResultVerbatim proves every path that
// never runs a command still captures exactly its returned text.
func TestBashCapturedRefusalCapturesTheResultVerbatim(t *testing.T) {
	t.Parallel()
	b := NewBash(t.TempDir())
	sink := &recordingSink{}
	res, err := b.InvokableRunCaptured(context.Background(), `{"command":"true"}`, sink)
	if err != nil {
		t.Fatal(err)
	}
	got := textOf(t, res)
	if !strings.Contains(got, "requires its prepared call artifact") {
		t.Fatalf("result = %q, want the fail-closed refusal", got)
	}
	if sink.String() != got {
		t.Fatalf("captured = %q, want exactly %q", sink.String(), got)
	}
}

// TestBashCapturedNilSinkBehavesLikeInvokableRun proves a nil or typed-nil
// sink degrades to the uncaptured path rather than panicking.
func TestBashCapturedNilSinkBehavesLikeInvokableRun(t *testing.T) {
	t.Parallel()
	requireSh(t)
	var typedNil *recordingSink
	for _, sink := range []tool.ResultCaptureSink{nil, typedNil} {
		got := runCaptured(t, context.Background(), NewBash(t.TempDir()), commandArgs("printf ok"), sink)
		if got != "ok\n[exit code: 0]" {
			t.Fatalf("result = %q, want the ordinary result", got)
		}
	}
}

// TestBashCapturedIgnoresAMisbehavingSink proves a sink that errors cannot
// fail or truncate the command, nor change the model result.
func TestBashCapturedIgnoresAMisbehavingSink(t *testing.T) {
	t.Parallel()
	requireSh(t)
	sink := &failingSink{}
	got := runCaptured(t, context.Background(), NewBash(t.TempDir()), commandArgs("printf 'a\\n'; printf 'b\\n' >&2; exit 4"), sink)
	if got != "a\nb\n[exit code: 4]" {
		t.Fatalf("result = %q, want the ordinary result", got)
	}
	if sink.calls == 0 {
		t.Fatal("the sink was never offered any bytes")
	}
}

// TestBashCapturedRunnerOutputReachesSinkInFull covers the injected-runner
// path: the runner materializes its output, and every byte it returns is
// captured while the returned preview carries Bash's own head/tail cap. The
// uncaptured path keeps its existing (uncapped) behaviour.
func TestBashCapturedRunnerOutputReachesSinkInFull(t *testing.T) {
	t.Parallel()
	body := strings.Repeat("r", 3*maxBashOutputBytes) + "\nTAIL-SENTINEL\n"
	for _, grants := range [][]string{nil, {"grant-token"}} {
		runner := &grantAwareRunner{fakeCommandRunner: fakeCommandRunner{out: []byte(body), exit: 2}}
		b := NewBash(t.TempDir(), WithRunner(runner))
		sink := &recordingSink{}
		id := mustUUID(t)
		args := commandArgs("make")
		req, art, err := b.PrepareCall(context.Background(), id, args)
		if err != nil {
			t.Fatal(err)
		}
		ctx := loop.WithPreparedCall(context.Background(), tool.PreparedCall{ExecutionID: id, Request: req, Artifact: art, Grants: grants})
		res, err := b.InvokableRunCaptured(ctx, args, sink)
		if err != nil {
			t.Fatal(err)
		}
		got := textOf(t, res)
		if want := body + "[exit code: 2]"; sink.String() != want {
			t.Fatalf("grants=%v: captured %d bytes, want %d", grants, len(sink.String()), len(want))
		}
		if len(got) > maxBashOutputBytes+256 || !strings.Contains(got, "TAIL-SENTINEL") || !strings.HasSuffix(got, "[exit code: 2]") {
			t.Fatalf("grants=%v: preview is %d bytes; want a bounded head/tail preview keeping the tail and exit code", grants, len(got))
		}
	}
	runner := &fakeCommandRunner{out: []byte(body)}
	if got := runPrepared(t, NewBash(t.TempDir(), WithRunner(runner)), commandArgs("make"), nil); got != body+"[exit code: 0]" {
		t.Fatalf("uncaptured runner result is %d bytes, want the unchanged full %d", len(got), len(body)+len("[exit code: 0]"))
	}
}

// grantAwareRunner is a runner that ALSO accepts grants, so both dispatch
// branches of the runner path are covered.
type grantAwareRunner struct{ fakeCommandRunner }

func (g *grantAwareRunner) RunCommandWithGrants(ctx context.Context, dir, command string, _ []string) ([]byte, int, error) {
	return g.RunCommand(ctx, dir, command)
}

// TestBashCapturedConcurrentCallsKeepTheirOwnStreams proves concurrent calls
// on one tool never cross-contaminate their sinks.
func TestBashCapturedConcurrentCallsKeepTheirOwnStreams(t *testing.T) {
	t.Parallel()
	requireSh(t)
	b := NewBash(t.TempDir())
	var group sync.WaitGroup
	for i := range 4 {
		group.Add(1)
		go func() {
			defer group.Done()
			marker := "call-" + strconv.Itoa(i)
			command := `n=0; while [ "$n" -lt 2000 ]; do printf '` + marker + ` %d\n' "$n"; n=$((n + 1)); done`
			sink := &recordingSink{}
			runCaptured(t, context.Background(), b, commandArgs(command), sink)
			lines := strings.Split(strings.TrimSuffix(sink.String(), "[exit code: 0]"), "\n")
			if len(lines) != 2001 {
				t.Errorf("%s: captured %d lines, want 2000", marker, len(lines)-1)
				return
			}
			for _, line := range lines[:2000] {
				if !strings.HasPrefix(line, marker+" ") {
					t.Errorf("%s: captured foreign line %q", marker, line)
					return
				}
			}
		}()
	}
	group.Wait()
}

// TestBashCapturedCancellationStopsTheCommand proves a cancelled call still
// returns promptly and never reports success.
func TestBashCapturedCancellationStopsTheCommand(t *testing.T) {
	t.Parallel()
	requireSh(t)
	ctx, cancel := context.WithCancel(context.Background())
	sink := &recordingSink{}
	go func() {
		// Cancel only once the command has demonstrably started streaming,
		// so the assertion below never races process start-up.
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			if strings.HasPrefix(sink.String(), "started") {
				break
			}
		}
		cancel()
	}()
	start := time.Now()
	got := runCaptured(t, ctx, NewBash(t.TempDir()), commandArgs("printf started; exec sleep 10"), sink)
	if time.Since(start) > 5*time.Second {
		t.Fatalf("cancelled call took %v", time.Since(start))
	}
	if strings.HasSuffix(got, "[exit code: 0]") {
		t.Fatalf("result = %q, want a non-success outcome for a cancelled command", got)
	}
	if !strings.HasPrefix(sink.String(), "started") {
		t.Fatalf("captured = %q, want the streamed output before cancellation", sink.String())
	}
}

// TestBashCapturedPeakMemoryIsIndependentOfOutputSize streams 64 MiB through a
// non-retaining sink and bounds the bytes Bash allocated doing it. A tool that
// buffered the stream would allocate at least the stream's size.
func TestBashCapturedPeakMemoryIsIndependentOfOutputSize(t *testing.T) {
	requireSh(t)
	if testing.Short() {
		t.Skip("streams 64 MiB")
	}
	const size = 64 << 20
	sink := newDigestSink()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	raw, _ := json.Marshal(map[string]any{"command": "head -c " + strconv.Itoa(size) + " /dev/zero", "timeout": 120})
	got := runCaptured(t, context.Background(), NewBash(t.TempDir()), string(raw), sink)
	runtime.ReadMemStats(&after)

	trailer := "\n[exit code: 0]"
	if sink.n != size+int64(len(trailer)) {
		t.Fatalf("captured %d bytes, want %d", sink.n, size+len(trailer))
	}
	if !strings.HasSuffix(string(sink.tail[:]), trailer) {
		t.Fatalf("captured tail = %q, want the exit trailer", sink.tail)
	}
	expected := sha256.New()
	_, _ = io.Copy(expected, io.LimitReader(zeroReader{}, size))
	_, _ = expected.Write([]byte(trailer))
	if hex.EncodeToString(sink.hash.Sum(nil)) != hex.EncodeToString(expected.Sum(nil)) {
		t.Fatal("captured digest differs from the produced stream")
	}
	if len(got) > maxBashOutputBytes+256 {
		t.Fatalf("result is %d bytes, want Bash's bounded preview", len(got))
	}
	// The sink-side test machinery (hash, tail) allocates too; 16 MiB is a
	// quarter of the stream, so a buffering implementation cannot pass.
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 16<<20 {
		t.Fatalf("allocated %d bytes while streaming %d; want allocation independent of output size", allocated, size)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// --- supervised path -------------------------------------------------------

func runSupervisedCaptured(t *testing.T, b *BashTool, argsJSON string, sink tool.ResultCaptureSink) (supervisedResult, string) {
	t.Helper()
	text := runCaptured(t, context.Background(), b, argsJSON, sink)
	return decodeSupervisedResult(t, text), text
}

// TestSupervisedBashCapturedCompleteOutputEqualsResult proves a supervised
// terminal result whose output already carries the whole stream captures the
// returned JSON verbatim, so no object is retained for it.
func TestSupervisedBashCapturedCompleteOutputEqualsResult(t *testing.T) {
	t.Parallel()
	proc := newFakeProcess(0)
	proc.stdout = io.NopCloser(strings.NewReader("hello from stdout\n"))
	runner := &fakeAsyncRunner{prepared: &fakePreparedProcess{access: freshWorkspaceAccess(), process: proc}}
	b, _ := newSupervisedTestTool(t, runner, &recordingLifetimeCoordinator{}, &syncWorkspaceObservations{})
	sink := &recordingSink{}
	out, text := runSupervisedCaptured(t, b, `{"command":"echo","yield_time_ms":2000}`, sink)
	if out.Output != "hello from stdout\n" {
		t.Fatalf("Output = %q", out.Output)
	}
	if sink.String() != text {
		t.Fatalf("captured = %q, want exactly the result %q", sink.String(), text)
	}
}

// TestSupervisedBashCapturedOversizedOutputStreamsTheSpool proves an oversized
// supervised terminal result streams the complete retained spool into the
// sink, followed by the terminal metadata, while the returned JSON keeps its
// 32 KiB inline bound.
func TestSupervisedBashCapturedOversizedOutputStreamsTheSpool(t *testing.T) {
	t.Parallel()
	oversized := strings.Repeat("a", int(process.DefaultMaxInlineResultBytes)) + strings.Repeat("b", 4096) + "END"
	proc := newFakeProcess(7)
	proc.stdout = io.NopCloser(strings.NewReader(oversized))
	runner := &fakeAsyncRunner{prepared: &fakePreparedProcess{access: freshWorkspaceAccess(), process: proc}}
	b, _ := newSupervisedTestTool(t, runner, &recordingLifetimeCoordinator{}, &syncWorkspaceObservations{})
	sink := &recordingSink{}
	out, _ := runSupervisedCaptured(t, b, `{"command":"produce","yield_time_ms":2000}`, sink)
	if int64(len(out.Output)) > process.DefaultMaxInlineResultBytes {
		t.Fatalf("inline output is %d bytes, want it bounded", len(out.Output))
	}
	captured := sink.String()
	if !strings.HasPrefix(captured, oversized+"\n") {
		t.Fatalf("captured %d bytes; want the complete %d-byte stream first", len(captured), len(oversized))
	}
	trailer := decodeSupervisedResult(t, strings.TrimPrefix(captured, oversized+"\n"))
	if trailer.Output != "" || trailer.Status != string(process.StateExited) || trailer.ExitCode == nil || *trailer.ExitCode != 7 {
		t.Fatalf("trailer = %+v, want the terminal metadata without the output", trailer)
	}
}

// TestSupervisedBashCapturedReportsSpoolRetentionLoss proves that when the
// process spool itself dropped the head of the stream, the capture says so
// instead of presenting the retained suffix as the whole output.
func TestSupervisedBashCapturedReportsSpoolRetentionLoss(t *testing.T) {
	t.Parallel()
	stream := strings.Repeat("x", 100<<10) + "SUFFIX"
	proc := newFakeProcess(0)
	proc.stdout = io.NopCloser(strings.NewReader(stream))
	runner := &fakeAsyncRunner{prepared: &fakePreparedProcess{access: freshWorkspaceAccess(), process: proc}}
	b, _ := newSupervisedTestTool(t, runner, &recordingLifetimeCoordinator{}, &syncWorkspaceObservations{})
	sink := &recordingSink{}
	const ceiling = 64 << 10
	runSupervisedCaptured(t, b, `{"command":"produce","yield_time_ms":2000,"max_output_bytes":`+strconv.Itoa(ceiling)+`}`, sink)
	captured := sink.String()
	if !strings.HasPrefix(captured, stream[len(stream)-ceiling:]+"\n") {
		t.Fatalf("captured does not start with the retained %d-byte suffix", ceiling)
	}
	dropped := len(stream) - ceiling
	notice := "[process output: the first " + strconv.Itoa(dropped) + " of " + strconv.Itoa(len(stream)) + " bytes were not retained by the process spool]\n"
	if !strings.Contains(captured, notice) {
		t.Fatalf("captured trailer lacks the retention notice %q; tail = %q", notice, captured[len(captured)-min(len(captured), 400):])
	}
}

// TestSupervisedBashCapturedLiveResultEqualsResult proves a live (running)
// supervised result captures its returned JSON verbatim.
func TestSupervisedBashCapturedLiveResultEqualsResult(t *testing.T) {
	t.Parallel()
	proc := newFakeProcess(0)
	proc.ready = make(chan struct{})
	runner := &fakeAsyncRunner{prepared: &fakePreparedProcess{access: freshWorkspaceAccess(), process: proc}}
	b, registry := newSupervisedTestTool(t, runner, &recordingLifetimeCoordinator{}, &syncWorkspaceObservations{})
	sink := &recordingSink{}
	out, text := runSupervisedCaptured(t, b, `{"command":"serve","background":true}`, sink)
	close(proc.ready)
	waitProcessDone(t, registry, b.owner, out.ProcessID)
	if out.Status != "running" {
		t.Fatalf("Status = %q, want running", out.Status)
	}
	if sink.String() != text {
		t.Fatalf("captured = %q, want exactly the result %q", sink.String(), text)
	}
}

// TestSupervisedBashCapturedStreamsWhenTheInlineOutputIsNotVerbatim covers the
// two ways a short inline output still is not the complete stream: the spool
// dropped its head (a gap), or safe-text normalization rewrote it. Either way
// the raw retained bytes are streamed instead of the returned JSON.
func TestSupervisedBashCapturedStreamsWhenTheInlineOutputIsNotVerbatim(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		stream  string
		ceiling int
		want    string
	}{
		{name: "retention gap", stream: strings.Repeat("h", 4096) + strings.Repeat("t", 1024), ceiling: 1024, want: strings.Repeat("t", 1024) + "\n"},
		{name: "normalized control sequence", stream: "\x1b[31mred\x1b[0m\n", want: "\x1b[31mred\x1b[0m\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			proc := newFakeProcess(0)
			proc.stdout = io.NopCloser(strings.NewReader(test.stream))
			runner := &fakeAsyncRunner{prepared: &fakePreparedProcess{access: freshWorkspaceAccess(), process: proc}}
			b, _ := newSupervisedTestTool(t, runner, &recordingLifetimeCoordinator{}, &syncWorkspaceObservations{})
			args := `{"command":"produce","yield_time_ms":2000}`
			if test.ceiling > 0 {
				args = `{"command":"produce","yield_time_ms":2000,"max_output_bytes":` + strconv.Itoa(test.ceiling) + `}`
			}
			sink := &recordingSink{}
			_, text := runSupervisedCaptured(t, b, args, sink)
			if sink.String() == text {
				t.Fatalf("captured the returned JSON verbatim; want the raw retained stream")
			}
			if !strings.HasPrefix(sink.String(), test.want) {
				t.Fatalf("captured = %q, want it to start with the raw retained bytes %q", sink.String(), test.want)
			}
		})
	}
}

package bash

// capture.go implements Harness's streaming result-capture capability
// (tool.CapturingInvokableTool) for Bash.
//
// The contract is two outputs from one run. The COMPLETE deterministic result
// streams into the capture sink while the command runs; the returned
// ToolResult keeps exactly the bounded shape the model has always seen (the
// 32 KiB head/tail cap, or the supervised JSON with its inline bound). Harness
// retains the sink's bytes and appends its own marker to the preview.
//
// One property is load-bearing: when nothing was elided, the captured bytes
// are byte-identical to the returned text. Harness compares the two and
// retains no object when they match. Every captured stream is therefore
// "output, then a newline if the output did not end in one, then the returned
// result's final line" — which is exactly how the returned text is built — and
// a path that never produced output captures its returned text verbatim.
//
// The sink never fails (Harness's contract), and a sink that does anyway is
// ignored: capture must not be able to fail, stall or truncate the command.

import (
	"context"
	"errors"
	"io"
	"strconv"
	"sync"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/internal/workspace"
	"github.com/looprig/tools/process"
)

// InvokableRunCaptured runs the prepared call exactly as InvokableRun does,
// additionally streaming the complete result into sink. A nil or typed-nil
// sink is the uncaptured path.
func (b *BashTool) InvokableRunCaptured(ctx context.Context, argsJSON string, sink tool.ResultCaptureSink) (*tool.ToolResult, error) {
	if workspace.IsNil(sink) {
		return b.InvokableRun(ctx, argsJSON)
	}
	stream := &captureStream{sink: sink}
	result := b.run(ctx, stream)
	// A path that returned without finishing the stream produced no output of
	// its own; its result text is the whole deterministic result.
	return stream.finish(result), nil
}

// captureStream is one call's view of the capture sink. A nil *captureStream
// is the uncaptured path: every method is a no-op that returns its input, so
// the execution code needs no branches.
type captureStream struct {
	sink tool.ResultCaptureSink

	mu       sync.Mutex
	written  int64
	last     byte
	finished bool
}

// Write offers p to the sink and always reports success, so the producer runs
// to completion whatever the sink does.
func (s *captureStream) Write(p []byte) (int, error) {
	if s == nil {
		return len(p), nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeLocked(p)
	return len(p), nil
}

func (s *captureStream) writeLocked(p []byte) {
	if len(p) == 0 {
		return
	}
	_, _ = s.sink.Write(p)
	s.written += int64(len(p))
	s.last = p[len(p)-1]
}

// finish closes the stream with result's own text as the trailer and returns
// result unchanged.
func (s *captureStream) finish(result *tool.ToolResult) *tool.ToolResult {
	return s.finishWithTrailer(result, resultText(result))
}

// finishWithTrailer closes the stream: a separating newline when output was
// streamed and did not end in one, then trailer. It runs once; later calls are
// no-ops, so the first path to finish decides the trailer.
func (s *captureStream) finishWithTrailer(result *tool.ToolResult, trailer string) *tool.ToolResult {
	if s == nil {
		return result
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return result
	}
	if s.written > 0 && s.last != '\n' {
		s.writeLocked([]byte{'\n'})
	}
	s.writeLocked([]byte(trailer))
	s.finished = true
	return result
}

// tee returns the writer a direct `sh -c` run writes its combined output to:
// buf alone when uncaptured, otherwise both buf and the stream.
func (s *captureStream) tee(buf *cappedBuffer) io.Writer {
	if s == nil {
		return buf
	}
	return teeWriter{buf: buf, stream: s}
}

// captureMaterialized handles an injected runner's already-materialized
// output: it captures every byte and returns the bounded head/tail preview the
// direct path would have produced. Uncaptured, the output is returned as is,
// preserving the runner path's existing behaviour.
func (s *captureStream) captureMaterialized(out []byte) []byte {
	if s == nil {
		return out
	}
	_, _ = s.Write(out)
	preview := cappedBuffer{limit: maxBashOutputBytes}
	_, _ = preview.Write(out)
	return []byte(preview.cappedString())
}

// teeWriter feeds one combined output stream to the inline cap and the
// capture stream. Neither can fail, so neither can stop the other.
type teeWriter struct {
	buf    *cappedBuffer
	stream *captureStream
}

func (t teeWriter) Write(p []byte) (int, error) {
	_, _ = t.buf.Write(p)
	_, _ = t.stream.Write(p)
	return len(p), nil
}

// resultText flattens a tool result the way Harness does for its preview: the
// text blocks, concatenated.
func resultText(result *tool.ToolResult) string {
	if result == nil {
		return ""
	}
	var text string
	for _, block := range result.Content {
		if tb, ok := block.(*content.TextBlock); ok {
			text += tb.Text
		}
	}
	return text
}

// captureTerminalOutput streams a finished supervised process's complete
// retained output into the stream, then the terminal result WITHOUT its inline
// output as the trailer. It is used only when the inline output did not carry
// the whole stream. When the spool cannot be read at all, nothing is written
// and the caller's result is captured verbatim instead.
func captureTerminalOutput(ctx context.Context, stream *captureStream, supervisor *process.Supervisor, owner process.Owner, handle process.Handle, trailer *tool.ToolResult) {
	if stream == nil {
		return
	}
	copied, err := supervisor.CopyOutput(ctx, owner, handle, stream)
	if err != nil && copied.Copied == 0 {
		return
	}
	var notice string
	if err != nil {
		notice += "[process output: capture stopped after " + strconv.FormatInt(copied.Copied, 10) + " bytes]\n"
	}
	if copied.RetainedFrom > 0 {
		notice += "[process output: the first " + strconv.FormatInt(copied.RetainedFrom, 10) + " of " +
			strconv.FormatInt(copied.TotalBytes, 10) + " bytes were not retained by the process spool]\n"
	}
	stream.finishWithTrailer(nil, notice+resultText(trailer))
}

// declaredCaptureSafety is what this tool's configuration honestly supports.
// Without an injected runner, Bash runs `sh -c` itself and streams every byte,
// so the complete result is never resident. With a runner (a confined sandbox
// executor, or a granted runner), tool.CommandRunner returns the WHOLE output
// as one []byte before Bash sees any of it. The capture is still complete, but
// the result was resident first, bounded only by the runner, so Bash declares
// it materialized.
func (b *BashTool) declaredCaptureSafety() tool.DeclaredCaptureSafety {
	if b.runner != nil {
		return tool.DeclaredCaptureSafety{HighOutput: true}
	}
	return tool.DeclaredCaptureSafety{Streaming: true, HighOutput: true}
}

// DeclaredCaptureSafety reports the capture safety of the tools this factory
// builds: streaming for direct execution, materialized when a runner is
// configured. The options are not reapplied; the sealed configuration is read.
func (f Factory) DeclaredCaptureSafety() tool.DeclaredCaptureSafety {
	if f == nil {
		return tool.DeclaredCaptureSafety{HighOutput: true}
	}
	return f("", nil, nil).declaredCaptureSafety()
}

// DeclaredCaptureSafety reports the capture safety of the tools this
// supervised factory builds, exactly as Factory.DeclaredCaptureSafety does.
// A supervised (background or yielded) call keeps its output in the process
// spool, whose retained window is bounded by the supervisor's spool ceiling.
// The synchronous path has the same runner-dependent behaviour as Bash.
func (f SupervisedFactory) DeclaredCaptureSafety() tool.DeclaredCaptureSafety {
	if f == nil {
		return tool.DeclaredCaptureSafety{HighOutput: true}
	}
	built, err := f(tool.Bindings{
		Workspace: &tool.WorkspaceBinding{},
		Process:   &tool.ProcessBinding{Registry: unusedRegistry{}},
	}, unusedAsyncRunner{})
	if err != nil {
		return tool.DeclaredCaptureSafety{HighOutput: true}
	}
	return built.declaredCaptureSafety()
}

// unusedRegistry and unusedAsyncRunner satisfy SupervisedFactory's non-nil
// checks for a configuration probe. The probed tool is discarded unused, so
// neither method is ever called.
type unusedRegistry struct{}

func (unusedRegistry) GetOrCreate(context.Context, string, func(string) (tool.SessionResource, error)) (tool.SessionResource, error) {
	return nil, errUnusedProbe
}

type unusedAsyncRunner struct{}

func (unusedAsyncRunner) PrepareProcess(context.Context, tool.ProcessRequest) (tool.PreparedProcess, error) {
	return nil, errUnusedProbe
}

var errUnusedProbe = errors.New("bash: capture-safety probe is not runnable")

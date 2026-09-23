package process

import (
	"context"
	"io"
)

// copy_output.go streams one process's retained combined output to a caller's
// writer in bounded chunks. It exists for the capture path: bash streams a
// supervised command's COMPLETE retained output into Harness's result-capture
// sink without ever holding more than one chunk of it, while the model-facing
// result keeps its inline bound (ProcessOutput's rendering path).
//
// It reads the same spool ProcessOutput reads, under the same owner check, so
// a missing handle and a foreign one stay indistinguishable (not_found).

// outputCopyChunkBytes bounds one CopyOutput read and write.
const outputCopyChunkBytes = 32 << 10

// OutputCopy describes one CopyOutput call.
//
// RetainedFrom is the cursor of the first byte copied: bytes before it were
// dropped by the spool's retention ceiling and exist nowhere. TotalBytes is the
// process's total combined output when the copy began; Copied is how many bytes
// reached the writer.
type OutputCopy struct {
	RetainedFrom int64
	TotalBytes   int64
	Copied       int64
}

// CopyOutput writes handle's retained combined stdout+stderr, from the earliest
// retained byte up to the total observed when the call began, to w in chunks of
// at most outputCopyChunkBytes. It is meant for a TERMINAL process, whose spool
// no longer changes; on a live process it copies the window as of the call and
// fails with cursor_gap if retention overtakes the copy.
//
// ctx is checked before every chunk. A writer error stops the copy and is
// returned as is; OutputCopy then reports what was copied before it.
func (s *Supervisor) CopyOutput(ctx context.Context, owner Owner, handle Handle, w io.Writer) (OutputCopy, error) {
	e, ok := s.resolveEntry(owner, handle)
	if !ok {
		return OutputCopy{}, New(CodeNotFound)
	}
	result := OutputCopy{TotalBytes: e.spool.TotalBytes(), RetainedFrom: -1}
	cursor := int64(0)
	for cursor < result.TotalBytes {
		if err := ctx.Err(); err != nil {
			return result.normalized(), err
		}
		want := min(result.TotalBytes-cursor, outputCopyChunkBytes)
		data, next, gap, err := e.spool.Read(cursor, int(want))
		if err != nil {
			return result.normalized(), err
		}
		if result.RetainedFrom < 0 {
			result.RetainedFrom = next - int64(len(data))
		} else if gap {
			return result.normalized(), New(CodeCursorGap)
		}
		if len(data) == 0 {
			break
		}
		if _, err := w.Write(data); err != nil {
			return result.normalized(), err
		}
		result.Copied += int64(len(data))
		cursor = next
	}
	return result.normalized(), nil
}

// normalized reports an untouched RetainedFrom as zero: nothing was read, so
// nothing is known to have been dropped.
func (c OutputCopy) normalized() OutputCopy {
	if c.RetainedFrom < 0 {
		c.RetainedFrom = 0
	}
	return c
}

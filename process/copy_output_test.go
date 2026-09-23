package process

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

// startFinishedProcess starts a fake process that writes payload to stdout and
// exits, waits for it to terminalize, and returns its handle and owner.
func startFinishedProcess(t *testing.T, sup *Supervisor, payload []byte, ceiling int64) (Handle, Owner) {
	t.Helper()
	owner := testOwner(t)
	proc := &fakeProcess{stdout: io.NopCloser(bytes.NewReader(payload))}
	handle, err := sup.Start(context.Background(), owner, testOrigin(t), &fakePreparedProcess{process: proc}, &fakeLease{}, nil, nil, StorageCeiling{SpoolBytes: ceiling}, YieldSettings{})
	if err != nil {
		t.Fatalf("Start() err = %v", err)
	}
	waitEntryDone(t, testEntry(t, sup, handle), 5*time.Second)
	return handle, owner
}

func copyOutputConfig() Config {
	return Config{
		MaxRunningProcessesPerLoop:    4,
		MaxRunningProcessesPerSession: 4,
		MaxProcessInMemoryBytes:       1 << 20,
		MaxAggregateInMemoryBytes:     10 << 20,
		MaxProcessSpoolBytes:          4 << 20,
		MaxAggregateSpoolBytes:        16 << 20,
	}
}

// chunkRecorder records each Write's length so a test can prove the copy is
// bounded per chunk rather than one whole-spool write.
type chunkRecorder struct {
	buf    bytes.Buffer
	chunks []int
}

func (c *chunkRecorder) Write(p []byte) (int, error) {
	c.chunks = append(c.chunks, len(p))
	return c.buf.Write(p)
}

func TestCopyOutputStreamsTheRetainedSpoolInBoundedChunks(t *testing.T) {
	t.Parallel()
	sup := newTestSupervisor(t, copyOutputConfig())
	payload := bytes.Repeat([]byte("0123456789abcdef"), 20000) // 320000 bytes
	handle, owner := startFinishedProcess(t, sup, payload, 0)

	var w chunkRecorder
	got, err := sup.CopyOutput(context.Background(), owner, handle, &w)
	if err != nil {
		t.Fatalf("CopyOutput() err = %v", err)
	}
	if !bytes.Equal(w.buf.Bytes(), payload) {
		t.Fatalf("copied %d bytes, want the exact %d-byte stream", w.buf.Len(), len(payload))
	}
	want := OutputCopy{RetainedFrom: 0, TotalBytes: int64(len(payload)), Copied: int64(len(payload))}
	if got != want {
		t.Fatalf("CopyOutput() = %+v, want %+v", got, want)
	}
	for _, n := range w.chunks {
		if n > outputCopyChunkBytes {
			t.Fatalf("a single write carried %d bytes, want <= %d", n, outputCopyChunkBytes)
		}
	}
}

func TestCopyOutputReportsTheSpoolRetentionWindow(t *testing.T) {
	t.Parallel()
	sup := newTestSupervisor(t, copyOutputConfig())
	payload := append(bytes.Repeat([]byte("x"), 70000), []byte("SUFFIX")...)
	const ceiling = 1 << 16
	handle, owner := startFinishedProcess(t, sup, payload, ceiling)

	var w bytes.Buffer
	got, err := sup.CopyOutput(context.Background(), owner, handle, &w)
	if err != nil {
		t.Fatalf("CopyOutput() err = %v", err)
	}
	dropped := int64(len(payload)) - ceiling
	if !bytes.Equal(w.Bytes(), payload[dropped:]) {
		t.Fatalf("copied %d bytes, want the retained %d-byte suffix", w.Len(), ceiling)
	}
	want := OutputCopy{RetainedFrom: dropped, TotalBytes: int64(len(payload)), Copied: ceiling}
	if got != want {
		t.Fatalf("CopyOutput() = %+v, want %+v", got, want)
	}
}

func TestCopyOutputRefusesAForeignOrUnknownHandle(t *testing.T) {
	t.Parallel()
	sup := newTestSupervisor(t, copyOutputConfig())
	handle, _ := startFinishedProcess(t, sup, []byte("secret"), 0)

	for name, owner := range map[string]Owner{"foreign owner": testOwner(t)} {
		var w bytes.Buffer
		_, err := sup.CopyOutput(context.Background(), owner, handle, &w)
		var perr *Error
		if !errors.As(err, &perr) || perr.Code != CodeNotFound {
			t.Fatalf("%s: err = %v, want not_found", name, err)
		}
		if w.Len() != 0 {
			t.Fatalf("%s: wrote %q to the destination", name, w.String())
		}
	}
}

func TestCopyOutputStopsOnCancellationAndWriterFailure(t *testing.T) {
	t.Parallel()
	sup := newTestSupervisor(t, copyOutputConfig())
	payload := bytes.Repeat([]byte("z"), 200000)
	handle, owner := startFinishedProcess(t, sup, payload, 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var w bytes.Buffer
	if _, err := sup.CopyOutput(ctx, owner, handle, &w); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled CopyOutput() err = %v, want context.Canceled", err)
	}
	if w.Len() != 0 {
		t.Fatalf("cancelled copy wrote %d bytes", w.Len())
	}

	failure := errors.New("destination failed")
	got, err := sup.CopyOutput(context.Background(), owner, handle, failingWriter{err: failure})
	if !errors.Is(err, failure) {
		t.Fatalf("CopyOutput() err = %v, want the writer's error", err)
	}
	if got.Copied != 0 {
		t.Fatalf("Copied = %d after the first write failed, want 0", got.Copied)
	}
}

type failingWriter struct{ err error }

func (f failingWriter) Write([]byte) (int, error) { return 0, f.err }

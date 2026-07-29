package process

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// TestEntryRunClosesDoneAfterDrainingBothStreams is a finer-grained unit
// test of entry.run/drain in isolation from Supervisor.Start's admission
// plumbing: it constructs an entry directly (a Buffer and a Spool only --
// no Manifest, no Supervisor, no quota reservation) and proves run's
// minimal 8B contract holds on its own: both streams are fully drained
// into the Buffer and Spool, and done closes once run's Wait call has
// returned and both drain goroutines have finished.
//
// TestSupervisorDrainsOrderedStreams (supervisor_test.go) is the
// strict-ordering proof through the full Supervisor.Start path; this test
// intentionally does not duplicate that precise-interleaving assertion --
// it only proves complete, lossless capture and the done-closes contract.
func TestEntryRunClosesDoneAfterDrainingBothStreams(t *testing.T) {
	t.Parallel()

	spool, err := OpenSpool(t.TempDir(), testHandle(t, 1), 0)
	if err != nil {
		t.Fatalf("OpenSpool() err = %v, want nil", err)
	}

	e := &entry{
		process: &fakeProcess{
			stdout: io.NopCloser(strings.NewReader("hello ")),
			stderr: io.NopCloser(strings.NewReader("world")),
		},
		buffer: NewBuffer(0),
		spool:  spool,
		done:   make(chan struct{}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.run(ctx)

	select {
	case <-e.done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for run to close done")
	}

	const wantLen = int64(len("hello ") + len("world"))
	if got := e.buffer.TotalBytes(); got != wantLen {
		t.Errorf("buffer.TotalBytes() = %d, want %d", got, wantLen)
	}
	if got := e.spool.TotalBytes(); got != wantLen {
		t.Errorf("spool.TotalBytes() = %d, want %d", got, wantLen)
	}

	data, _, gap, err := e.buffer.Read(0, 0)
	if err != nil {
		t.Fatalf("buffer.Read() err = %v, want nil", err)
	}
	if gap {
		t.Error("buffer.Read(0, ...) gap = true, want false")
	}
	combined := string(data)
	if !strings.Contains(combined, "hello ") || !strings.Contains(combined, "world") {
		t.Errorf("buffer content = %q, want it to contain both %q and %q", combined, "hello ", "world")
	}
	if int64(len(combined)) != wantLen {
		t.Errorf("buffer content length = %d, want %d", len(combined), wantLen)
	}
}

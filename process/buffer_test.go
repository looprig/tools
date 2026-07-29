package process

import (
	"errors"
	"sync"
	"testing"
)

// --- empty ---

func TestBufferEmptyReadReturnsNoData(t *testing.T) {
	t.Parallel()
	b := NewBuffer(10)
	data, next, gap, err := b.Read(0, 0)
	if err != nil {
		t.Fatalf("Read() err = %v", err)
	}
	if len(data) != 0 || next != 0 || gap {
		t.Errorf("Read() = (%q, %d, %v), want (\"\", 0, false)", data, next, gap)
	}
	if got := b.TotalBytes(); got != 0 {
		t.Errorf("TotalBytes() = %d, want 0", got)
	}
	if got := b.RetainedFrom(); got != 0 {
		t.Errorf("RetainedFrom() = %d, want 0", got)
	}
}

// --- partial fill (well under capacity) ---

func TestBufferPartialFillReadsBackExactly(t *testing.T) {
	t.Parallel()
	b := NewBuffer(10)
	cur, err := b.Append([]byte("abc"))
	if err != nil {
		t.Fatalf("Append() err = %v", err)
	}
	if cur != 3 {
		t.Errorf("cursor = %d, want 3", cur)
	}
	data, next, gap, err := b.Read(0, 0)
	if err != nil {
		t.Fatalf("Read() err = %v", err)
	}
	if string(data) != "abc" || next != 3 || gap {
		t.Errorf("Read() = (%q, %d, %v), want (\"abc\", 3, false)", data, next, gap)
	}
	if got := b.RetainedFrom(); got != 0 {
		t.Errorf("RetainedFrom() = %d, want 0 (well under capacity)", got)
	}
}

// --- exact capacity ---

func TestBufferExactCapacityFillRetainsEverything(t *testing.T) {
	t.Parallel()
	b := NewBuffer(5)
	if _, err := b.Append([]byte("abcde")); err != nil {
		t.Fatalf("Append() err = %v", err)
	}
	data, next, gap, err := b.Read(0, 0)
	if err != nil {
		t.Fatalf("Read() err = %v", err)
	}
	if string(data) != "abcde" || next != 5 || gap {
		t.Errorf("Read() = (%q, %d, %v), want (\"abcde\", 5, false)", data, next, gap)
	}
	if got := b.RetainedFrom(); got != 0 {
		t.Errorf("RetainedFrom() = %d, want 0", got)
	}
}

// --- wraparound: a single append that pushes total past capacity ---

func TestBufferWraparoundOverwritesOldestBytes(t *testing.T) {
	t.Parallel()
	b := NewBuffer(5)
	if _, err := b.Append([]byte("abcde")); err != nil {
		t.Fatalf("Append() err = %v", err)
	}
	cur, err := b.Append([]byte("FG")) // pushes total to 7, capacity 5
	if err != nil {
		t.Fatalf("Append() err = %v", err)
	}
	if cur != 7 {
		t.Errorf("cursor = %d, want 7", cur)
	}
	if got := b.TotalBytes(); got != 7 {
		t.Errorf("TotalBytes() = %d, want 7", got)
	}
	if got := b.RetainedFrom(); got != 2 {
		t.Errorf("RetainedFrom() = %d, want 2", got)
	}

	data, next, gap, err := b.Read(0, 0)
	if err != nil {
		t.Fatalf("Read() err = %v", err)
	}
	if !gap {
		t.Errorf("gap = false, want true: cursor 0 precedes the retained window")
	}
	if string(data) != "cdeFG" {
		t.Errorf("data = %q, want %q", data, "cdeFG")
	}
	if next != 7 {
		t.Errorf("next = %d, want 7", next)
	}
}

// --- wraparound: a single append larger than capacity in one call ---

func TestBufferAppendLargerThanCapacityInOneCall(t *testing.T) {
	t.Parallel()
	b := NewBuffer(4)
	if _, err := b.Append([]byte("abcdefgh")); err != nil { // 8 bytes, capacity 4
		t.Fatalf("Append() err = %v", err)
	}
	if got := b.TotalBytes(); got != 8 {
		t.Errorf("TotalBytes() = %d, want 8", got)
	}
	if got := b.RetainedFrom(); got != 4 {
		t.Errorf("RetainedFrom() = %d, want 4", got)
	}
	data, _, gap, err := b.Read(0, 0)
	if err != nil {
		t.Fatalf("Read() err = %v", err)
	}
	if !gap {
		t.Errorf("gap = false, want true")
	}
	if string(data) != "efgh" {
		t.Errorf("data = %q, want %q", data, "efgh")
	}
}

// --- multiple wraps across many small appends ---

func TestBufferMultipleWrapsRetainsOnlyMostRecentWindow(t *testing.T) {
	t.Parallel()
	b := NewBuffer(3)
	// "abcdefghij" is 10 bytes across a capacity-3 buffer: more than three
	// full wraps of the ring. Append one byte at a time to exercise the
	// wraparound path repeatedly rather than in a single large call.
	src := "abcdefghij"
	for i := range src {
		if _, err := b.Append([]byte{src[i]}); err != nil {
			t.Fatalf("Append() byte %d err = %v", i, err)
		}
	}
	if got := b.TotalBytes(); got != 10 {
		t.Errorf("TotalBytes() = %d, want 10", got)
	}
	if got := b.RetainedFrom(); got != 7 {
		t.Errorf("RetainedFrom() = %d, want 7", got)
	}
	data, next, gap, err := b.Read(0, 0)
	if err != nil {
		t.Fatalf("Read() err = %v", err)
	}
	if !gap {
		t.Errorf("gap = false, want true")
	}
	if string(data) != "hij" {
		t.Errorf("data = %q, want %q", data, "hij")
	}
	if next != 10 {
		t.Errorf("next = %d, want 10", next)
	}
}

// --- arbitrary cursor ---

func TestBufferReadAtArbitraryCursor(t *testing.T) {
	t.Parallel()
	b := NewBuffer(20)
	if _, err := b.Append([]byte("0123456789")); err != nil {
		t.Fatalf("Append() err = %v", err)
	}
	data, next, gap, err := b.Read(3, 4)
	if err != nil {
		t.Fatalf("Read() err = %v", err)
	}
	if string(data) != "3456" || next != 7 || gap {
		t.Errorf("Read(3,4) = (%q, %d, %v), want (\"3456\", 7, false)", data, next, gap)
	}
}

func TestBufferReadAtExactTotalReturnsEmpty(t *testing.T) {
	t.Parallel()
	b := NewBuffer(10)
	if _, err := b.Append([]byte("abc")); err != nil {
		t.Fatalf("Append() err = %v", err)
	}
	data, next, gap, err := b.Read(3, 10)
	if err != nil {
		t.Fatalf("Read() err = %v", err)
	}
	if len(data) != 0 || next != 3 || gap {
		t.Errorf("Read(3,10) = (%q, %d, %v), want (\"\", 3, false)", data, next, gap)
	}
}

// --- gap ---

func TestBufferGapReturnsEarliestRetainedData(t *testing.T) {
	t.Parallel()
	b := NewBuffer(5)
	if _, err := b.Append([]byte("0123456789")); err != nil { // capacity 5, total 10
		t.Fatalf("Append() err = %v", err)
	}
	data, next, gap, err := b.Read(0, 100)
	if err != nil {
		t.Fatalf("Read() err = %v", err)
	}
	if !gap {
		t.Errorf("gap = false, want true")
	}
	if string(data) != "56789" {
		t.Errorf("data = %q, want %q", data, "56789")
	}
	if next != 10 {
		t.Errorf("next = %d, want 10", next)
	}

	// A cursor already inside the retained window is not a gap.
	data, next, gap, err = b.Read(5, 100)
	if err != nil {
		t.Fatalf("Read() err = %v", err)
	}
	if gap {
		t.Errorf("gap = true, want false: cursor 5 is exactly RetainedFrom")
	}
	if string(data) != "56789" || next != 10 {
		t.Errorf("Read(5,100) = (%q, %d), want (\"56789\", 10)", data, next)
	}
}

// --- cursor-ahead ---

func TestBufferReadCursorAheadOfTotalIsTypedError(t *testing.T) {
	t.Parallel()
	b := NewBuffer(10)
	if _, err := b.Append([]byte("abc")); err != nil {
		t.Fatalf("Append() err = %v", err)
	}
	_, next, _, err := b.Read(1000, 10)
	if !errors.Is(err, New(CodeCursorAhead)) {
		t.Errorf("Read() err = %v, want CodeCursorAhead", err)
	}
	if next != 3 {
		t.Errorf("next = %d, want current TotalBytes 3", next)
	}
}

func TestBufferReadNegativeCursorIsInvalidArguments(t *testing.T) {
	t.Parallel()
	b := NewBuffer(10)
	_, _, _, err := b.Read(-1, 10)
	if !errors.Is(err, New(CodeInvalidArguments)) {
		t.Errorf("Read() err = %v, want CodeInvalidArguments", err)
	}
}

// --- defaults ---

func TestBufferNewDefaultsCapacity(t *testing.T) {
	t.Parallel()
	b := NewBuffer(0)
	if b.Capacity() != DefaultMaxProcessInMemoryBytes {
		t.Errorf("Capacity() = %d, want default %d", b.Capacity(), DefaultMaxProcessInMemoryBytes)
	}
	b2 := NewBuffer(-1)
	if b2.Capacity() != DefaultMaxProcessInMemoryBytes {
		t.Errorf("Capacity() (negative input) = %d, want default %d", b2.Capacity(), DefaultMaxProcessInMemoryBytes)
	}
}

func TestBufferAppendEmptyIsNoop(t *testing.T) {
	t.Parallel()
	b := NewBuffer(10)
	if _, err := b.Append([]byte("x")); err != nil {
		t.Fatalf("Append() err = %v", err)
	}
	cur, err := b.Append(nil)
	if err != nil {
		t.Fatalf("Append(nil) err = %v", err)
	}
	if cur != 1 {
		t.Errorf("Append(nil) cursor = %d, want unchanged 1", cur)
	}
}

// --- cursors are raw byte offsets, never rune indexes ---

// TestBufferCursorsAreByteOffsetsNotRuneIndexes appends a two-byte UTF-8
// rune ("é") split across two separate Append calls, one byte each, and
// verifies TotalBytes/cursors count exactly two bytes -- never one "rune" --
// and the raw bytes read back reassemble correctly. Buffer must never
// decode its payload to compute cursor positions.
func TestBufferCursorsAreByteOffsetsNotRuneIndexes(t *testing.T) {
	t.Parallel()
	b := NewBuffer(10)
	raw := []byte("é") // 2-byte UTF-8 encoding: 0xC3 0xA9
	if len(raw) != 2 {
		t.Fatalf("test fixture bug: %q is not 2 raw bytes", raw)
	}
	cur1, err := b.Append(raw[:1])
	if err != nil {
		t.Fatalf("Append() err = %v", err)
	}
	if cur1 != 1 {
		t.Errorf("cursor after first byte = %d, want 1 (byte offset, not rune count)", cur1)
	}
	cur2, err := b.Append(raw[1:])
	if err != nil {
		t.Fatalf("Append() err = %v", err)
	}
	if cur2 != 2 {
		t.Errorf("cursor after second byte = %d, want 2", cur2)
	}
	data, next, _, err := b.Read(0, 0)
	if err != nil {
		t.Fatalf("Read() err = %v", err)
	}
	if string(data) != "é" || next != 2 {
		t.Errorf("Read() = (%q, %d), want (\"é\", 2)", data, next)
	}
}

// --- concurrency ---

// TestBufferConcurrentAppendReadIsSafe exercises concurrent Append and Read
// calls from multiple goroutines and verifies the final TotalBytes equals
// the sum of every appended length exactly once. Run with -race at the
// phase gate to confirm no data race; every concurrent Read here only
// asserts internal consistency (nextCursor never exceeds the
// contemporaneous TotalBytes upper bound implied by err == nil), not an
// exact byte count, since reads race arbitrarily against in-flight appends.
func TestBufferConcurrentAppendReadIsSafe(t *testing.T) {
	t.Parallel()
	b := NewBuffer(1 << 10)

	const goroutines = 8
	const perGoroutine = 50
	chunk := []byte("0123456789") // 10 bytes

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range perGoroutine {
				if _, err := b.Append(chunk); err != nil {
					t.Errorf("Append() err = %v", err)
				}
			}
		}()
	}

	// Concurrent readers must never panic or observe a torn/inconsistent
	// state, and any error must be one of the two well-formed cases.
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for i := 0; i < 200; i++ {
			_, _, _, err := b.Read(0, 5)
			if err != nil && !errors.Is(err, New(CodeCursorAhead)) && !errors.Is(err, New(CodeInvalidArguments)) {
				t.Errorf("Read() unexpected err = %v", err)
			}
		}
	}()

	wg.Wait()
	<-readerDone

	want := int64(goroutines * perGoroutine * len(chunk))
	if got := b.TotalBytes(); got != want {
		t.Errorf("TotalBytes() = %d, want %d", got, want)
	}
}

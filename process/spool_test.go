package process

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// --- append order determines the global cursor ---

func TestSpoolAppendOrderDeterminesCursor(t *testing.T) {
	t.Parallel()
	s, err := OpenSpool(t.TempDir(), testHandle(t, 1), 0)
	if err != nil {
		t.Fatalf("OpenSpool() err = %v", err)
	}

	cur, err := s.Append([]byte("abc"))
	if err != nil {
		t.Fatalf("Append() err = %v", err)
	}
	if cur != 3 {
		t.Errorf("cursor after first append = %d, want 3", cur)
	}

	cur, err = s.Append([]byte("def"))
	if err != nil {
		t.Fatalf("Append() err = %v", err)
	}
	if cur != 6 {
		t.Errorf("cursor after second append = %d, want 6", cur)
	}
	if got := s.TotalBytes(); got != 6 {
		t.Errorf("TotalBytes() = %d, want 6", got)
	}

	data, next, gap, err := s.Read(0, 0)
	if err != nil {
		t.Fatalf("Read() err = %v", err)
	}
	if string(data) != "abcdef" {
		t.Errorf("Read() data = %q, want %q", data, "abcdef")
	}
	if next != 6 || gap {
		t.Errorf("Read() next=%d gap=%v, want 6 false", next, gap)
	}
}

func TestSpoolAppendEmptyIsNoop(t *testing.T) {
	t.Parallel()
	s, err := OpenSpool(t.TempDir(), testHandle(t, 1), 0)
	if err != nil {
		t.Fatalf("OpenSpool() err = %v", err)
	}
	if _, err := s.Append([]byte("x")); err != nil {
		t.Fatalf("Append() err = %v", err)
	}
	cur, err := s.Append(nil)
	if err != nil {
		t.Fatalf("Append(nil) err = %v", err)
	}
	if cur != 1 {
		t.Errorf("Append(nil) cursor = %d, want unchanged 1", cur)
	}
}

// --- ceiling overflow drops oldest bytes, total_bytes keeps counting ---

func TestSpoolOverflowDropsOldestBytesAndKeepsCounting(t *testing.T) {
	t.Parallel()
	s, err := OpenSpool(t.TempDir(), testHandle(t, 1), 10)
	if err != nil {
		t.Fatalf("OpenSpool() err = %v", err)
	}

	if _, err := s.Append([]byte("0123456789")); err != nil { // exactly at ceiling
		t.Fatalf("Append() err = %v", err)
	}
	cur, err := s.Append([]byte("ABCDE")) // pushes 5 bytes over ceiling
	if err != nil {
		t.Fatalf("Append() over ceiling err = %v, want nil (never fails on volume)", err)
	}
	if cur != 15 {
		t.Errorf("cursor = %d, want 15", cur)
	}
	if got := s.TotalBytes(); got != 15 {
		t.Errorf("TotalBytes() = %d, want 15 (counts the full stream)", got)
	}
	if got := s.RetainedFrom(); got != 5 {
		t.Errorf("RetainedFrom() = %d, want 5", got)
	}

	data, next, gap, err := s.Read(0, 0)
	if err != nil {
		t.Fatalf("Read() err = %v", err)
	}
	if !gap {
		t.Errorf("gap = false, want true: cursor 0 precedes the retained window")
	}
	if string(data) != "56789ABCDE" {
		t.Errorf("data = %q, want %q", data, "56789ABCDE")
	}
	if next != 15 {
		t.Errorf("next = %d, want 15", next)
	}
}

func TestSpoolAppendLargerThanCeilingInOneCall(t *testing.T) {
	t.Parallel()
	s, err := OpenSpool(t.TempDir(), testHandle(t, 1), 5)
	if err != nil {
		t.Fatalf("OpenSpool() err = %v", err)
	}
	if _, err := s.Append([]byte("abcdefghijkl")); err != nil { // 12 bytes, ceiling 5
		t.Fatalf("Append() err = %v", err)
	}
	if got := s.TotalBytes(); got != 12 {
		t.Errorf("TotalBytes() = %d, want 12", got)
	}
	if got := s.RetainedFrom(); got != 7 {
		t.Errorf("RetainedFrom() = %d, want 7", got)
	}
	data, _, gap, err := s.Read(0, 0)
	if err != nil {
		t.Fatalf("Read() err = %v", err)
	}
	if !gap {
		t.Errorf("gap = false, want true")
	}
	if string(data) != "hijkl" {
		t.Errorf("data = %q, want %q", data, "hijkl")
	}
}

func TestSpoolOpenDefaultsCeiling(t *testing.T) {
	t.Parallel()
	s, err := OpenSpool(t.TempDir(), testHandle(t, 1), 0)
	if err != nil {
		t.Fatalf("OpenSpool() err = %v", err)
	}
	if s.ceiling != DefaultMaxProcessSpoolBytes {
		t.Errorf("ceiling = %d, want default %d", s.ceiling, DefaultMaxProcessSpoolBytes)
	}
	s2, err := OpenSpool(t.TempDir(), testHandle(t, 2), -1)
	if err != nil {
		t.Fatalf("OpenSpool() err = %v", err)
	}
	if s2.ceiling != DefaultMaxProcessSpoolBytes {
		t.Errorf("ceiling (negative input) = %d, want default %d", s2.ceiling, DefaultMaxProcessSpoolBytes)
	}
}

// --- reads are bounded and cursor-addressed ---

func TestSpoolReadIsBounded(t *testing.T) {
	t.Parallel()
	s, err := OpenSpool(t.TempDir(), testHandle(t, 1), 0)
	if err != nil {
		t.Fatalf("OpenSpool() err = %v", err)
	}
	if _, err := s.Append([]byte("abcdefghij")); err != nil {
		t.Fatalf("Append() err = %v", err)
	}

	data, next, gap, err := s.Read(0, 3)
	if err != nil {
		t.Fatalf("Read() err = %v", err)
	}
	if string(data) != "abc" || next != 3 || gap {
		t.Errorf("Read(0,3) = (%q, %d, %v), want (\"abc\", 3, false)", data, next, gap)
	}

	data, next, gap, err = s.Read(3, 3)
	if err != nil {
		t.Fatalf("Read() err = %v", err)
	}
	if string(data) != "def" || next != 6 || gap {
		t.Errorf("Read(3,3) = (%q, %d, %v), want (\"def\", 6, false)", data, next, gap)
	}

	// A non-positive maxBytes returns everything from the start.
	data, next, _, err = s.Read(6, 0)
	if err != nil {
		t.Fatalf("Read() err = %v", err)
	}
	if string(data) != "ghij" || next != 10 {
		t.Errorf("Read(6,0) = (%q, %d), want (\"ghij\", 10)", data, next)
	}
}

func TestSpoolReadAtExactTotalReturnsEmpty(t *testing.T) {
	t.Parallel()
	s, err := OpenSpool(t.TempDir(), testHandle(t, 1), 0)
	if err != nil {
		t.Fatalf("OpenSpool() err = %v", err)
	}
	if _, err := s.Append([]byte("abc")); err != nil {
		t.Fatalf("Append() err = %v", err)
	}
	data, next, gap, err := s.Read(3, 10)
	if err != nil {
		t.Fatalf("Read() err = %v", err)
	}
	if len(data) != 0 || next != 3 || gap {
		t.Errorf("Read(3,10) = (%q, %d, %v), want (\"\", 3, false)", data, next, gap)
	}
}

func TestSpoolReadCursorAheadOfTotal(t *testing.T) {
	t.Parallel()
	s, err := OpenSpool(t.TempDir(), testHandle(t, 1), 0)
	if err != nil {
		t.Fatalf("OpenSpool() err = %v", err)
	}
	if _, err := s.Append([]byte("abc")); err != nil {
		t.Fatalf("Append() err = %v", err)
	}
	_, next, _, err := s.Read(1000, 10)
	if !errors.Is(err, New(CodeCursorAhead)) {
		t.Errorf("Read() err = %v, want CodeCursorAhead", err)
	}
	if next != 3 {
		t.Errorf("next = %d, want current TotalBytes 3", next)
	}
}

func TestSpoolReadNegativeCursorIsInvalidArguments(t *testing.T) {
	t.Parallel()
	s, err := OpenSpool(t.TempDir(), testHandle(t, 1), 0)
	if err != nil {
		t.Fatalf("OpenSpool() err = %v", err)
	}
	_, _, _, err = s.Read(-1, 10)
	if !errors.Is(err, New(CodeInvalidArguments)) {
		t.Errorf("Read() err = %v, want CodeInvalidArguments", err)
	}
}

// --- truncated/corrupt files return spool_corrupt ---

func TestSpoolOpenMalformedJSONIsCorrupt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := testHandle(t, 1)
	if err := os.WriteFile(filepath.Join(dir, string(h)+spoolSuffix), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("WriteFile() err = %v", err)
	}
	_, err := OpenSpool(dir, h, 0)
	assertSpoolCorrupt(t, err)
}

func TestSpoolOpenUnknownVersionIsCorrupt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := testHandle(t, 1)
	raw, err := json.Marshal(map[string]any{"version": 999, "total_bytes": 0, "retained_from": 0, "payload": ""})
	if err != nil {
		t.Fatalf("Marshal() err = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, string(h)+spoolSuffix), raw, 0o600); err != nil {
		t.Fatalf("WriteFile() err = %v", err)
	}
	_, err = OpenSpool(dir, h, 0)
	assertSpoolCorrupt(t, err)
}

func TestSpoolOpenImpossibleBoundsIsCorrupt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := testHandle(t, 1)
	raw, err := json.Marshal(spoolWire{Version: spoolVersion, TotalBytes: 5, RetainedFrom: 9})
	if err != nil {
		t.Fatalf("Marshal() err = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, string(h)+spoolSuffix), raw, 0o600); err != nil {
		t.Fatalf("WriteFile() err = %v", err)
	}
	_, err = OpenSpool(dir, h, 0)
	assertSpoolCorrupt(t, err)
}

func TestSpoolOpenPayloadLengthMismatchIsCorrupt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := testHandle(t, 1)
	// Declares a 10-byte retained window (total 10, retained_from 0) but
	// supplies only 3 bytes of payload: exactly the "truncated mid-write"
	// shape a crash between marshal and full durable write would leave.
	raw, err := json.Marshal(spoolWire{Version: spoolVersion, TotalBytes: 10, RetainedFrom: 0, Payload: []byte("abc")})
	if err != nil {
		t.Fatalf("Marshal() err = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, string(h)+spoolSuffix), raw, 0o600); err != nil {
		t.Fatalf("WriteFile() err = %v", err)
	}
	_, err = OpenSpool(dir, h, 0)
	assertSpoolCorrupt(t, err)
}

func TestSpoolOpenTruncatedFileIsCorrupt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := testHandle(t, 1)
	s, err := OpenSpool(dir, h, 0)
	if err != nil {
		t.Fatalf("OpenSpool() err = %v", err)
	}
	if _, err := s.Append([]byte("hello world")); err != nil {
		t.Fatalf("Append() err = %v", err)
	}

	path := filepath.Join(dir, string(h)+spoolSuffix)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() err = %v", err)
	}
	truncated := raw[:len(raw)/2]
	if err := os.WriteFile(path, truncated, 0o600); err != nil {
		t.Fatalf("WriteFile() err = %v", err)
	}

	_, err = OpenSpool(dir, h, 0)
	assertSpoolCorrupt(t, err)
}

func assertSpoolCorrupt(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("err = nil, want CodeSpoolCorrupt")
	}
	if !errors.Is(err, New(CodeSpoolCorrupt)) {
		t.Errorf("err = %v, want CodeSpoolCorrupt", err)
	}
}

// --- durability across reopen ---

func TestSpoolReopenPersistsState(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := testHandle(t, 1)

	s, err := OpenSpool(dir, h, 0)
	if err != nil {
		t.Fatalf("OpenSpool() err = %v", err)
	}
	if _, err := s.Append([]byte("persisted")); err != nil {
		t.Fatalf("Append() err = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() err = %v", err)
	}

	reopened, err := OpenSpool(dir, h, 0)
	if err != nil {
		t.Fatalf("OpenSpool() (reopen) err = %v", err)
	}
	if got := reopened.TotalBytes(); got != 9 {
		t.Errorf("TotalBytes() after reopen = %d, want 9", got)
	}
	data, _, _, err := reopened.Read(0, 0)
	if err != nil {
		t.Fatalf("Read() err = %v", err)
	}
	if string(data) != "persisted" {
		t.Errorf("data after reopen = %q, want %q", data, "persisted")
	}
}

// --- no path escapes the private resource directory ---

func TestSpoolPathNeverEscapesRoot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	malicious := []Handle{
		"../../../../etc/passwd",
		"..",
		"/etc/passwd",
		"a/../../b",
		"",
	}
	for _, h := range malicious {
		if _, err := OpenSpool(dir, h, 0); !errors.Is(err, New(CodeNotFound)) {
			t.Errorf("OpenSpool(%q) err = %v, want CodeNotFound", h, err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() err = %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("resource root has %d unexpected entries after malicious handles", len(entries))
	}
}

// --- close and removal are idempotent ---

func TestSpoolCloseIsIdempotent(t *testing.T) {
	t.Parallel()
	s, err := OpenSpool(t.TempDir(), testHandle(t, 1), 0)
	if err != nil {
		t.Fatalf("OpenSpool() err = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() err = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close() err = %v, want nil", err)
	}
	if _, err := s.Append([]byte("x")); !errors.Is(err, ErrSpoolClosed) {
		t.Errorf("Append() after Close() err = %v, want ErrSpoolClosed", err)
	}
	if _, _, _, err := s.Read(0, 0); !errors.Is(err, ErrSpoolClosed) {
		t.Errorf("Read() after Close() err = %v, want ErrSpoolClosed", err)
	}
}

func TestSpoolRemoveIsIdempotentAndDeletesFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := testHandle(t, 1)
	s, err := OpenSpool(dir, h, 0)
	if err != nil {
		t.Fatalf("OpenSpool() err = %v", err)
	}
	if _, err := s.Append([]byte("data")); err != nil {
		t.Fatalf("Append() err = %v", err)
	}

	path := filepath.Join(dir, string(h)+spoolSuffix)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Stat() err = %v, want file to exist before Remove", err)
	}

	if err := s.Remove(); err != nil {
		t.Fatalf("Remove() err = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("Stat() after Remove() err = %v, want IsNotExist", err)
	}
	if err := s.Remove(); err != nil {
		t.Errorf("second Remove() err = %v, want nil", err)
	}
}

func TestSpoolRemoveWithoutAnyAppendIsNoop(t *testing.T) {
	t.Parallel()
	s, err := OpenSpool(t.TempDir(), testHandle(t, 1), 0)
	if err != nil {
		t.Fatalf("OpenSpool() err = %v", err)
	}
	if err := s.Remove(); err != nil {
		t.Errorf("Remove() err = %v, want nil when no file was ever written", err)
	}
	if err := s.Remove(); err != nil {
		t.Errorf("second Remove() err = %v, want nil", err)
	}
}

// --- concurrency ---

// TestSpoolConcurrentAppendIsSafe exercises concurrent Append calls from
// multiple goroutines and verifies the final TotalBytes equals the sum of
// every appended length exactly once, with no lost or duplicated update.
// Run with -race at the phase gate to confirm no data race.
func TestSpoolConcurrentAppendIsSafe(t *testing.T) {
	t.Parallel()
	s, err := OpenSpool(t.TempDir(), testHandle(t, 1), 1<<20)
	if err != nil {
		t.Fatalf("OpenSpool() err = %v", err)
	}

	const goroutines = 8
	const perGoroutine = 20
	chunk := []byte("0123456789") // 10 bytes

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range perGoroutine {
				if _, err := s.Append(chunk); err != nil {
					t.Errorf("Append() err = %v", err)
				}
			}
		}()
	}
	wg.Wait()

	want := int64(goroutines * perGoroutine * len(chunk))
	if got := s.TotalBytes(); got != want {
		t.Errorf("TotalBytes() = %d, want %d", got, want)
	}
}

package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withHook replaces one of Replace's fault-injection seams for the duration
// of a test and restores it via t.Cleanup, so tests never leak a fake across
// cases.
func withHook[T any](t *testing.T, slot *T, fake T) {
	t.Helper()
	original := *slot
	*slot = fake
	t.Cleanup(func() { *slot = original })
}

// TestAtomicReplaceCreatesFile verifies a Replace against a path with no
// existing file creates it with exactly the given content and an
// owner-only permission mode.
func TestAtomicReplaceCreatesFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")

	if err := Replace(path, []byte("hello"), 0o600); err != nil {
		t.Fatalf("Replace() err = %v, want nil", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() err = %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("content = %q, want %q", got, "hello")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() err = %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("perm = %v, want 0600", info.Mode().Perm())
	}
}

// TestAtomicReplaceOverwritesFile verifies a second Replace against an
// existing file fully replaces its content.
func TestAtomicReplaceOverwritesFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")

	if err := Replace(path, []byte("version one"), 0o600); err != nil {
		t.Fatalf("first Replace() err = %v", err)
	}
	if err := Replace(path, []byte("version two"), 0o600); err != nil {
		t.Fatalf("second Replace() err = %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() err = %v", err)
	}
	if string(got) != "version two" {
		t.Errorf("content = %q, want %q", got, "version two")
	}
}

// TestAtomicReplaceNoTempFileLeftOnSuccess verifies a successful Replace
// leaves exactly one file (the destination) in the directory: no stray
// ".tmp-*" file survives.
func TestAtomicReplaceNoTempFileLeftOnSuccess(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")

	if err := Replace(path, []byte("data"), 0o600); err != nil {
		t.Fatalf("Replace() err = %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() err = %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "manifest.json" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("directory entries = %v, want exactly [manifest.json]", names)
	}
}

// TestAtomicReplaceFaultInjection exercises every documented durability
// boundary (create, write, file-sync, rename, directory-sync), injecting a
// failure at exactly one boundary per case, and verifies:
//   - Replace returns a *Error naming that exact Stage;
//   - for every boundary except the trailing directory-sync, the
//     destination's prior content (or absence) is completely unaffected —
//     "the old manifest must remain readable until the new one is fully
//     committed";
//   - no ".tmp-*" file is left behind in the directory.
func TestAtomicReplaceFaultInjection(t *testing.T) {
	injected := errors.New("injected failure")

	newFailingCreateTemp := func() func(string, string) (*os.File, error) {
		return func(dir, pattern string) (*os.File, error) {
			return nil, injected
		}
	}
	newFailingWrite := func() func(*os.File, []byte) error {
		return func(f *os.File, data []byte) error {
			return injected
		}
	}
	newFailingSync := func() func(*os.File) error {
		return func(f *os.File) error {
			return injected
		}
	}
	newFailingRename := func() func(string, string) error {
		return func(oldpath, newpath string) error {
			return injected
		}
	}
	newFailingSyncDir := func() func(string) error {
		return func(dir string) error {
			return injected
		}
	}

	tests := []struct {
		name        string
		wantStage   Stage
		install     func(t *testing.T)
		contentKept bool // whether the destination's prior content must be unchanged after the failed Replace
	}{
		{
			name:        "create",
			wantStage:   StageCreate,
			install:     func(t *testing.T) { withHook(t, &createTempFunc, newFailingCreateTemp()) },
			contentKept: true,
		},
		{
			name:        "write",
			wantStage:   StageWrite,
			install:     func(t *testing.T) { withHook(t, &writeFunc, newFailingWrite()) },
			contentKept: true,
		},
		{
			name:        "sync",
			wantStage:   StageSync,
			install:     func(t *testing.T) { withHook(t, &syncFunc, newFailingSync()) },
			contentKept: true,
		},
		{
			name:        "rename",
			wantStage:   StageRename,
			install:     func(t *testing.T) { withHook(t, &renameFunc, newFailingRename()) },
			contentKept: true,
		},
		{
			name:        "dirsync",
			wantStage:   StageDirSync,
			install:     func(t *testing.T) { withHook(t, &syncDirFunc, newFailingSyncDir()) },
			contentKept: false, // the rename already committed; only durability confirmation failed
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "manifest.json")

			if err := Replace(path, []byte("original"), 0o600); err != nil {
				t.Fatalf("seed Replace() err = %v", err)
			}

			tt.install(t)

			err := Replace(path, []byte("replacement"), 0o600)
			if err == nil {
				t.Fatalf("Replace() err = nil, want *Error at stage %v", tt.wantStage)
			}
			var atomicErr *Error
			if !errors.As(err, &atomicErr) {
				t.Fatalf("Replace() err = %v (%T), want *Error", err, err)
			}
			if atomicErr.Stage != tt.wantStage {
				t.Errorf("Stage = %v, want %v", atomicErr.Stage, tt.wantStage)
			}
			if !errors.Is(err, injected) {
				t.Errorf("errors.Is(err, injected) = false, want true (Unwrap must expose the cause)")
			}

			got, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatalf("ReadFile() err = %v, want destination still present", readErr)
			}
			wantContent := "original"
			if !tt.contentKept {
				wantContent = "replacement"
			}
			if string(got) != wantContent {
				t.Errorf("content after failed Replace = %q, want %q", got, wantContent)
			}

			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatalf("ReadDir() err = %v", err)
			}
			for _, e := range entries {
				if strings.Contains(e.Name(), ".tmp-") {
					t.Errorf("leftover temp file %q after failed Replace", e.Name())
				}
			}
		})
	}
}

// TestAtomicReplaceMissingDestinationOnCreateFailure verifies that when the
// destination never existed and Replace fails before commit, the
// destination still does not exist afterward (no partial file appears).
func TestAtomicReplaceMissingDestinationOnCreateFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")

	withHook(t, &writeFunc, func(f *os.File, data []byte) error {
		return errors.New("injected failure")
	})

	if err := Replace(path, []byte("data"), 0o600); err == nil {
		t.Fatalf("Replace() err = nil, want failure")
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("Stat(path) err = %v, want IsNotExist", err)
	}
}

// TestAtomicReplaceStageString verifies String renders a distinct,
// non-empty label for every documented Stage, plus a safe fallback for an
// out-of-range value.
func TestAtomicReplaceStageString(t *testing.T) {
	t.Parallel()
	stages := []Stage{StageCreate, StageWrite, StageSync, StageRename, StageDirSync}
	seen := map[string]bool{}
	for _, s := range stages {
		label := s.String()
		if label == "" {
			t.Errorf("Stage(%d).String() = empty, want non-empty", s)
		}
		if seen[label] {
			t.Errorf("Stage(%d).String() = %q, duplicate label", s, label)
		}
		seen[label] = true
	}
	if got := Stage(999).String(); got == "" {
		t.Errorf("Stage(999).String() = empty, want a safe fallback label")
	}
}

// TestAtomicReplaceErrorUnwrap verifies *Error exposes its cause through
// Unwrap so errors.Is/errors.As can see past it.
func TestAtomicReplaceErrorUnwrap(t *testing.T) {
	t.Parallel()
	cause := errors.New("boom")
	err := &Error{Stage: StageWrite, Path: "/tmp/x", Err: cause}
	if !errors.Is(err, cause) {
		t.Errorf("errors.Is(err, cause) = false, want true")
	}
	if !strings.Contains(err.Error(), "write") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("Error() = %q, want it to mention stage %q and cause %q", err.Error(), "write", "boom")
	}
}

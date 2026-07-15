package filemutation

import (
	"path/filepath"
	"testing"
)

type fakeReadGuard struct {
	maxBytes int64
	denied   map[string]bool
}

func newFakeReadGuard(maxBytes int64, denied ...string) *fakeReadGuard {
	guard := &fakeReadGuard{maxBytes: maxBytes, denied: make(map[string]bool)}
	for _, path := range denied {
		guard.denied[path] = true
	}
	return guard
}

func (guard *fakeReadGuard) DeniedRead(path string) bool { return guard.denied[path] }
func (guard *fakeReadGuard) MaxReadBytes() int64         { return guard.maxBytes }

func resolvedJoin(t *testing.T, root, relativePath string) string {
	t.Helper()
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	absolutePath, err := filepath.Abs(filepath.Join(resolvedRoot, relativePath))
	if err != nil {
		t.Fatal(err)
	}
	return absolutePath
}

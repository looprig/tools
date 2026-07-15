//go:build integration

package filemutation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fsWorkspace(t *testing.T) string {
	t.Helper()
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return workspace
}

func fsWrite(t *testing.T, root, relativePath, data string) {
	t.Helper()
	absolutePath := filepath.Join(root, relativePath)
	if err := os.MkdirAll(filepath.Dir(absolutePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(absolutePath, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertNoTempLitter(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".looprig-write-") || strings.HasSuffix(entry.Name(), ".tmp") {
			t.Errorf("leftover temp file %q", entry.Name())
		}
	}
}

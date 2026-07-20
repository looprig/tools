package glob

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

// fakeReadGuard is a configurable test double for loop.ReadGuard. denied holds
// the set of ABSOLUTE paths DeniedRead reports true for; maxBytes is returned by
// MaxReadBytes. It exercises the read tools' two checks (denied-path filtering
// and the read cap) without depending on the concrete PermissionChecker.
type fakeReadGuard struct {
	denied   map[string]bool
	maxBytes int64
}

// newFakeReadGuard builds a fakeReadGuard with the given byte cap and the given
// absolute paths marked denied.
func newFakeReadGuard(maxBytes int64, deniedAbs ...string) *fakeReadGuard {
	d := make(map[string]bool, len(deniedAbs))
	for _, p := range deniedAbs {
		d[p] = true
	}
	return &fakeReadGuard{denied: d, maxBytes: maxBytes}
}

func (g *fakeReadGuard) DeniedRead(absPath string) bool { return g.denied[absPath] }
func (g *fakeReadGuard) MaxReadBytes() int64            { return g.maxBytes }

// compile-time assertion that the fake satisfies the narrow read guard.
var _ loop.ReadGuard = (*fakeReadGuard)(nil)

// patternReadGuard is a loop.ReadGuard double that decides DeniedRead with a
// PREDICATE over the absolute path rather than an enumerated set. It models the
// §10.5 read-adaptation seam faithfully: a sandbox adapter derives
// DeniedRead from the sandbox Policy's read RULES (globs like "**/.env*"), not a
// fixed path list, so pinning the seam with a rule-shaped guard proves the native
// read tools honour a policy-derived deny, not just an exact path.
type patternReadGuard struct {
	deny     func(absPath string) bool
	maxBytes int64
}

func (g *patternReadGuard) DeniedRead(absPath string) bool { return g.deny(absPath) }
func (g *patternReadGuard) MaxReadBytes() int64            { return g.maxBytes }

// compile-time assertion that the pattern guard satisfies the narrow read guard.
var _ loop.ReadGuard = (*patternReadGuard)(nil)

// resolvedJoin returns the symlink-resolved absolute path of rel under root —
// the exact form containedPath produces and DeniedRead's contract expects (on
// macOS t.TempDir() lives under a /var -> /private/var symlink, so the raw
// filepath.Join would not match the resolved path the tool passes to DeniedRead).
func resolvedJoin(t *testing.T, root, rel string) string {
	t.Helper()
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", root, err)
	}
	abs, err := filepath.Abs(filepath.Join(resolvedRoot, rel))
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}
	return abs
}

// mustWriteFile seeds one file (creating parents) for a prepare/run test.
func mustWriteFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// textOfResult extracts the single text block from a tool result.
func textOfResult(t *testing.T, res *tool.ToolResult) string {
	t.Helper()
	if res == nil || len(res.Content) != 1 {
		t.Fatalf("result = %v, want exactly 1 block", res)
	}
	tb, ok := res.Content[0].(*content.TextBlock)
	if !ok {
		t.Fatalf("block type = %T, want *content.TextBlock", res.Content[0])
	}
	return tb.Text
}

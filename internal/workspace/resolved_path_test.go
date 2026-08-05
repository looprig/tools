package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolvedPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// setup builds the fixture under the per-test temp dir and returns the
		// (root, input) pair to feed ResolvedPath. Any host-side path it needs
		// outside root is created under tmp too so the test never touches real
		// host paths.
		setup         func(t *testing.T, tmp string) (root, input string)
		wantContained bool
		// wantRelToRoot, when set, is the expected resolved path relative to the
		// resolved workspace root (independent of the temp dir name).
		wantRelToRoot string
		// wantRelToTmp, when set, is the expected resolved path relative to tmp
		// (used for out-of-root results, where "relative to root" isn't
		// meaningful).
		wantRelToTmp string
		wantErr      bool
	}{
		{
			name: "relative in-workspace file is contained",
			setup: func(t *testing.T, tmp string) (string, string) {
				root := mkdir(t, tmp, "ws")
				writeFile(t, filepath.Join(root, "a.go"), "x")
				return root, "a.go"
			},
			wantContained: true,
			wantRelToRoot: "a.go",
		},
		{
			name: "relative dotdot escape is still rejected outright",
			setup: func(t *testing.T, tmp string) (string, string) {
				root := mkdir(t, tmp, "ws")
				writeFile(t, filepath.Join(tmp, "secret.txt"), "x")
				return root, "../secret.txt"
			},
			wantErr: true,
		},
		{
			name: "absolute path outside the workspace resolves uncontained, no error",
			setup: func(t *testing.T, tmp string) (string, string) {
				root := mkdir(t, tmp, "ws")
				outside := filepath.Join(tmp, "host.txt")
				writeFile(t, outside, "x")
				return root, outside
			},
			wantContained: false,
			wantRelToTmp:  "host.txt",
		},
		{
			name: "absolute path spelling an in-workspace file is contained",
			setup: func(t *testing.T, tmp string) (string, string) {
				root := mkdir(t, tmp, "ws")
				inside := filepath.Join(root, "a.go")
				writeFile(t, inside, "x")
				return root, inside
			},
			wantContained: true,
			wantRelToRoot: "a.go",
		},
		{
			name: "absolute path resolves a symlinked existing ancestor",
			setup: func(t *testing.T, tmp string) (string, string) {
				root := mkdir(t, tmp, "ws")
				real := mkdir(t, tmp, "real-outside")
				writeFile(t, filepath.Join(real, "f.txt"), "x")
				link := filepath.Join(tmp, "link-outside")
				symlink(t, real, link)
				return root, filepath.Join(link, "f.txt")
			},
			wantContained: false,
			wantRelToTmp:  filepath.Join("real-outside", "f.txt"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tmp := t.TempDir()
			root, input := tt.setup(t, tmp)

			abs, contained, err := ResolvedPath(root, input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ResolvedPath(%q, %q) = (%q, %v, nil), want an error", root, input, abs, contained)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolvedPath(%q, %q) unexpected error: %v", root, input, err)
			}
			if contained != tt.wantContained {
				t.Errorf("ResolvedPath(%q, %q) contained = %v, want %v", root, input, contained, tt.wantContained)
			}
			if tt.wantRelToRoot != "" {
				if got := relToRoot(t, root, abs); got != filepath.Clean(tt.wantRelToRoot) {
					t.Errorf("ResolvedPath(%q, %q) = %q, want relative-to-root %q (got rel %q)", root, input, abs, tt.wantRelToRoot, got)
				}
			}
			if tt.wantRelToTmp != "" {
				rTmp, everr := filepath.EvalSymlinks(tmp)
				if everr != nil {
					t.Fatalf("EvalSymlinks(tmp): %v", everr)
				}
				rTmp, everr = filepath.Abs(rTmp)
				if everr != nil {
					t.Fatalf("Abs(tmp): %v", everr)
				}
				rel, rerr := filepath.Rel(rTmp, abs)
				if rerr != nil {
					t.Fatalf("Rel(tmp, abs): %v", rerr)
				}
				if rel != filepath.Clean(tt.wantRelToTmp) {
					t.Errorf("ResolvedPath(%q, %q) = %q, want relative-to-tmp %q (got rel %q)", root, input, abs, tt.wantRelToTmp, rel)
				}
			}
		})
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink %q -> %q: %v", link, target, err)
	}
}

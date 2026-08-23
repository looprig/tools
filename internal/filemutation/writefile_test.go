package filemutation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/readfile"
)

var _ tool.MutationPreviewer = (*writeFileArtifact)(nil)

// runWriteFile invokes WriteFile (bound to the given per-loop observation map) and
// extracts the single text block, failing on any structural surprise (including a
// Go error — write tools return tool-result strings). The shared obs lets a test
// observe a file (via observeFile) before overwriting it, exercising the read-then-
// write optimistic-concurrency path.
func runWriteFile(t *testing.T, root string, obs *fileObservations, args map[string]any) string {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return prepareRun(context.Background(), t, NewWriteFile(root, obs), string(b))
}

func prepareWritePreviewArtifact(t *testing.T, root, path, content string, opts ...FileMutatorOption) *writeFileArtifact {
	t.Helper()
	_, preparedArtifact, err := NewWriteFile(root, newFileObservations(), opts...).PrepareCall(
		context.Background(),
		mustUUID(t),
		mustJSON(t, map[string]any{"path": path, "content": content}),
	)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	art, ok := preparedArtifact.(*writeFileArtifact)
	if !ok {
		t.Fatalf("PrepareCall() artifact = %T, want *writeFileArtifact", preparedArtifact)
	}
	return art
}

func prepareHostWriteArtifact(t *testing.T, path, content string) (*WriteFile, tool.PreparedCall, *writeFileArtifact) {
	t.Helper()
	write := NewWriteFile(t.TempDir(), newFileObservations(), WithHostWrites())
	executionID := mustUUID(t)
	request, preparedArtifact, err := write.PrepareCall(
		context.Background(),
		executionID,
		mustJSON(t, map[string]any{"path": path, "content": content}),
	)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	art, ok := preparedArtifact.(*writeFileArtifact)
	if !ok {
		t.Fatalf("PrepareCall() artifact = %T, want *writeFileArtifact", preparedArtifact)
	}
	return write, tool.PreparedCall{ExecutionID: executionID, Request: request, Artifact: art}, art
}

func commitPreparedWriteArtifact(t *testing.T, write *WriteFile, call tool.PreparedCall) string {
	t.Helper()
	ctx := loop.WithPreparedCall(context.Background(), call)
	result, err := write.InvokableRun(ctx, "")
	if err != nil {
		t.Fatalf("InvokableRun() Go error = %v", err)
	}
	return textBlock(t, result)
}

func TestWriteFilePreviewMarksCreateAndRendersContent(t *testing.T) {
	root := t.TempDir()
	art := prepareWritePreviewArtifact(t, root, "nested/new.go", "hello\nworld\n")

	preview, ok := art.MutationPreview()

	if !ok {
		t.Fatal("MutationPreview() declined a well-formed create")
	}
	if preview.Path != "nested/new.go" {
		t.Errorf("MutationPreview().Path = %q, want %q", preview.Path, "nested/new.go")
	}
	if !preview.Creates {
		t.Error("MutationPreview().Creates = false for an absent target")
	}
	for _, want := range []string{"--- a/nested/new.go", "+++ b/nested/new.go", "+hello", "+world"} {
		if !strings.Contains(preview.UnifiedDiff, want) {
			t.Errorf("MutationPreview().UnifiedDiff missing %q:\n%s", want, preview.UnifiedDiff)
		}
	}
}

func TestWriteFilePreviewDistinguishesEmptyExistingFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "empty.go"), nil, 0o600); err != nil {
		t.Fatalf("seed empty target: %v", err)
	}
	art := prepareWritePreviewArtifact(t, root, "empty.go", "hello\n")

	preview, ok := art.MutationPreview()

	if !ok {
		t.Fatal("MutationPreview() declined an empty existing file")
	}
	if preview.Creates {
		t.Error("MutationPreview().Creates = true for an existing empty file")
	}
}

func TestWriteFilePreviewRendersOverwrite(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("keep\nold\n"), 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	art := prepareWritePreviewArtifact(t, root, "a.go", "keep\nnew\n")

	preview, ok := art.MutationPreview()

	if !ok {
		t.Fatal("MutationPreview() declined a UTF-8 overwrite")
	}
	if preview.Creates {
		t.Error("MutationPreview().Creates = true for an existing file")
	}
	for _, want := range []string{"-old", "+new"} {
		if !strings.Contains(preview.UnifiedDiff, want) {
			t.Errorf("MutationPreview().UnifiedDiff missing %q:\n%s", want, preview.UnifiedDiff)
		}
	}
}

func TestUncontainedWriteCommitRefusesTargetChangedAfterPreview(t *testing.T) {
	tests := []struct {
		name       string
		seed       func(t *testing.T, path string)
		transition func(t *testing.T, path string)
		assert     func(t *testing.T, path string)
	}{
		{
			name: "existing content drifted",
			seed: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("reviewed old\n"), 0o600); err != nil {
					t.Fatalf("seed target: %v", err)
				}
			},
			transition: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("drifted old\n"), 0o600); err != nil {
					t.Fatalf("drift target: %v", err)
				}
			},
			assert: func(t *testing.T, path string) {
				t.Helper()
				got, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read refused target: %v", err)
				}
				if string(got) != "drifted old\n" {
					t.Fatalf("refused target body = %q, want drifted bytes preserved", got)
				}
			},
		},
		{
			name: "existing disappeared",
			seed: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("reviewed old\n"), 0o600); err != nil {
					t.Fatalf("seed target: %v", err)
				}
			},
			transition: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatalf("remove target: %v", err)
				}
			},
			assert: func(t *testing.T, path string) {
				t.Helper()
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("refused target was recreated (lstat error %v)", err)
				}
			},
		},
		{
			name: "existing became irregular",
			seed: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("reviewed old\n"), 0o600); err != nil {
					t.Fatalf("seed target: %v", err)
				}
			},
			transition: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatalf("remove target: %v", err)
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatalf("replace target with directory: %v", err)
				}
			},
			assert: func(t *testing.T, path string) {
				t.Helper()
				fi, err := os.Lstat(path)
				if err != nil || !fi.IsDir() {
					t.Fatalf("refused irregular target = (%v, %v), want directory preserved", fi, err)
				}
			},
		},
		{
			name: "absent became existing",
			seed: func(*testing.T, string) {},
			transition: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("appeared\n"), 0o600); err != nil {
					t.Fatalf("create target after preview: %v", err)
				}
			},
			assert: func(t *testing.T, path string) {
				t.Helper()
				got, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read refused target: %v", err)
				}
				if string(got) != "appeared\n" {
					t.Fatalf("refused target body = %q, want appeared bytes preserved", got)
				}
			},
		},
		{
			name: "absent became irregular",
			seed: func(*testing.T, string) {},
			transition: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatalf("create directory after preview: %v", err)
				}
			},
			assert: func(t *testing.T, path string) {
				t.Helper()
				fi, err := os.Lstat(path)
				if err != nil || !fi.IsDir() {
					t.Fatalf("refused irregular target = (%v, %v), want directory preserved", fi, err)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "host.txt")
			tc.seed(t, path)
			write, call, art := prepareHostWriteArtifact(t, path, "approved new\n")
			if _, ok := art.MutationPreview(); !ok {
				t.Fatal("MutationPreview() declined")
			}
			tc.transition(t, path)

			out := commitPreparedWriteArtifact(t, write, call)

			if !strings.HasPrefix(out, "error:") || !strings.Contains(out, "changed since preview") {
				t.Fatalf("commit result = %q, want changed-since-preview refusal", out)
			}
			tc.assert(t, path)
		})
	}
}

func TestWriteFilePreviewDeclinesUnsafeExistingTargets(t *testing.T) {
	t.Run("oversized", func(t *testing.T) {
		root := t.TempDir()
		body := strings.Repeat("x", int(maxPreviewFileBytes)+1)
		if err := os.WriteFile(filepath.Join(root, "large.txt"), []byte(body), 0o600); err != nil {
			t.Fatalf("seed oversized target: %v", err)
		}
		art := prepareWritePreviewArtifact(t, root, "large.txt", "small\n")
		if preview, ok := art.MutationPreview(); ok || preview != (tool.MutationPreview{}) {
			t.Fatalf("MutationPreview() = (%+v, %v), want (zero, false)", preview, ok)
		}
	})

	t.Run("directory", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "dir"), 0o700); err != nil {
			t.Fatalf("seed directory target: %v", err)
		}
		art := prepareWritePreviewArtifact(t, root, "dir", "new\n")
		if preview, ok := art.MutationPreview(); ok || preview != (tool.MutationPreview{}) {
			t.Fatalf("MutationPreview() = (%+v, %v), want (zero, false)", preview, ok)
		}
	})

	t.Run("final component symlink", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "target.txt"), []byte("secret\n"), 0o600); err != nil {
			t.Fatalf("seed symlink target: %v", err)
		}
		if err := os.Symlink("target.txt", filepath.Join(root, "link.txt")); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		art := prepareWritePreviewArtifact(t, root, "link.txt", "new\n")
		if preview, ok := art.MutationPreview(); ok || preview != (tool.MutationPreview{}) {
			t.Fatalf("MutationPreview() = (%+v, %v), want (zero, false)", preview, ok)
		}
	})

	t.Run("non UTF-8", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "binary.dat"), []byte{0xff, 0xfe, 0x00}, 0o600); err != nil {
			t.Fatalf("seed non-UTF-8 target: %v", err)
		}
		art := prepareWritePreviewArtifact(t, root, "binary.dat", "new\n")
		if preview, ok := art.MutationPreview(); ok || preview != (tool.MutationPreview{}) {
			t.Fatalf("MutationPreview() = (%+v, %v), want (zero, false)", preview, ok)
		}
	})
}

func TestWriteFilePreviewDeclinesAfterParentResolutionChanges(t *testing.T) {
	root := t.TempDir()
	insideParent := filepath.Join(root, "dir")
	if err := os.Mkdir(insideParent, 0o700); err != nil {
		t.Fatalf("create approved parent: %v", err)
	}
	insideTarget := filepath.Join(insideParent, "target.txt")
	if err := os.WriteFile(insideTarget, []byte("approved old\n"), 0o600); err != nil {
		t.Fatalf("seed approved target: %v", err)
	}
	art := prepareWritePreviewArtifact(t, root, "dir/target.txt", "new\n")

	outsideParent := t.TempDir()
	const outsideBody = "outside-secret\n"
	if err := os.WriteFile(filepath.Join(outsideParent, "target.txt"), []byte(outsideBody), 0o600); err != nil {
		t.Fatalf("seed outside target: %v", err)
	}
	if err := os.Remove(insideTarget); err != nil {
		t.Fatalf("remove approved target: %v", err)
	}
	if err := os.Remove(insideParent); err != nil {
		t.Fatalf("remove approved parent: %v", err)
	}
	if err := os.Symlink(outsideParent, insideParent); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	preview, ok := art.MutationPreview()

	if ok || preview != (tool.MutationPreview{}) {
		t.Fatalf("MutationPreview() after parent retarget = (%+v, %v), want (zero, false); outside content %q must not be previewed", preview, ok, outsideBody)
	}
}

func TestWriteFilePreviewWithHostWritesDeclinesAfterParentResolutionChanges(t *testing.T) {
	root := t.TempDir()
	hostRoot := t.TempDir()
	approvedParent := filepath.Join(hostRoot, "approved")
	if err := os.Mkdir(approvedParent, 0o700); err != nil {
		t.Fatalf("create approved host parent: %v", err)
	}
	approvedTarget := filepath.Join(approvedParent, "target.txt")
	if err := os.WriteFile(approvedTarget, []byte("approved old\n"), 0o600); err != nil {
		t.Fatalf("seed approved host target: %v", err)
	}
	art := prepareWritePreviewArtifact(t, root, approvedTarget, "new\n", WithHostWrites())

	retargetedParent := t.TempDir()
	const outsideBody = "host-secret\n"
	if err := os.WriteFile(filepath.Join(retargetedParent, "target.txt"), []byte(outsideBody), 0o600); err != nil {
		t.Fatalf("seed retargeted host target: %v", err)
	}
	if err := os.Remove(approvedTarget); err != nil {
		t.Fatalf("remove approved host target: %v", err)
	}
	if err := os.Remove(approvedParent); err != nil {
		t.Fatalf("remove approved host parent: %v", err)
	}
	if err := os.Symlink(retargetedParent, approvedParent); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	preview, ok := art.MutationPreview()

	if ok || preview != (tool.MutationPreview{}) {
		t.Fatalf("MutationPreview() after host parent retarget = (%+v, %v), want (zero, false); outside content %q must not be previewed", preview, ok, outsideBody)
	}
}

func TestWriteFilePreviewDeclinesOversizedContentWithoutChangingWrite(t *testing.T) {
	root := t.TempDir()
	content := strings.Repeat("x", maxPreviewResultBytes+1)
	art := prepareWritePreviewArtifact(t, root, "large.txt", content)

	if preview, ok := art.MutationPreview(); ok || preview != (tool.MutationPreview{}) {
		t.Fatalf("MutationPreview() = (%+v, %v), want (zero, false)", preview, ok)
	}

	out := runWriteFile(t, root, newFileObservations(), map[string]any{"path": "large.txt", "content": content})
	if strings.HasPrefix(out, "error:") {
		t.Fatalf("normal WriteFile execution rejected preview-oversized content: %q", out)
	}
	fi, err := os.Stat(filepath.Join(root, "large.txt"))
	if err != nil {
		t.Fatalf("stat written target: %v", err)
	}
	if fi.Size() != int64(len(content)) {
		t.Fatalf("written size = %d, want %d", fi.Size(), len(content))
	}
}

// observeFile runs a real ReadFile bound to obs so a subsequent WriteFile/EditFile
// on the same loop is authorized to overwrite/edit the path — the faithful read-
// then-write workflow. It fails if the read did not succeed.
func observeFile(t *testing.T, root string, obs *fileObservations, rel string) {
	t.Helper()
	b, err := json.Marshal(map[string]any{"path": rel})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	if got := prepareRun(context.Background(), t, readfile.NewReadFile(root, newFakeReadGuard(1<<20), obs), string(b)); strings.HasPrefix(got, "error:") {
		t.Fatalf("observe read of %q failed: %q", rel, got)
	}
}

func TestWriteFileInfo(t *testing.T) {
	t.Parallel()
	info, err := NewWriteFile(t.TempDir(), newFileObservations()).Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	if info.Name != "WriteFile" {
		t.Errorf("Info().Name = %q, want %q", info.Name, "WriteFile")
	}
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(info.Schema, &schema); err != nil {
		t.Fatalf("Schema is not a JSON object: %v", err)
	}
	if _, ok := schema["properties"]; !ok {
		t.Errorf("Schema missing 'properties'")
	}
}

func TestWriteFile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       map[string]any
		setup      func(t *testing.T, root string)
		observeRel string // if set, ReadFile this path first so an overwrite is authorized
		wantErr    bool   // result string begins with "error:"
		wantOnDisk string // relative path that should exist with wantBody
		wantBody   string
	}{
		{
			name:       "new file in root",
			args:       map[string]any{"path": "out.txt", "content": "hello\nworld\n"},
			wantOnDisk: "out.txt",
			wantBody:   "hello\nworld\n",
		},
		{
			name:       "nested dirs are created",
			args:       map[string]any{"path": "a/b/c/deep.txt", "content": "deep"},
			wantOnDisk: "a/b/c/deep.txt",
			wantBody:   "deep",
		},
		{
			name: "observed existing file is overwritten",
			args: map[string]any{"path": "exists.txt", "content": "new"},
			setup: func(t *testing.T, root string) {
				if err := os.WriteFile(filepath.Join(root, "exists.txt"), []byte("old contents here"), 0o600); err != nil {
					t.Fatalf("seed: %v", err)
				}
			},
			observeRel: "exists.txt",
			wantOnDisk: "exists.txt",
			wantBody:   "new",
		},
		{
			name: "unobserved existing file is rejected without clobbering",
			args: map[string]any{"path": "exists.txt", "content": "new"},
			setup: func(t *testing.T, root string) {
				if err := os.WriteFile(filepath.Join(root, "exists.txt"), []byte("old contents here"), 0o600); err != nil {
					t.Fatalf("seed: %v", err)
				}
			},
			wantErr:    true,
			wantOnDisk: "exists.txt",
			wantBody:   "old contents here", // unchanged: no observation, no clobber
		},
		{
			name:       "empty content writes an empty file",
			args:       map[string]any{"path": "empty.txt", "content": ""},
			wantOnDisk: "empty.txt",
			wantBody:   "",
		},
		{
			name:    "escape path is rejected",
			args:    map[string]any{"path": "../escape.txt", "content": "x"},
			wantErr: true,
		},
		{
			name:    "absolute path is anchored under root (not /etc)",
			args:    map[string]any{"path": "/etc/passwd", "content": "x"},
			wantErr: false, // anchored under root -> writes root/etc/passwd
		},
		{
			name:    "missing path is rejected",
			args:    map[string]any{"content": "x"},
			wantErr: true,
		},
		{
			name:    "empty path is rejected",
			args:    map[string]any{"path": "", "content": "x"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			obs := newFileObservations()
			if tt.setup != nil {
				tt.setup(t, root)
			}
			if tt.observeRel != "" {
				observeFile(t, root, obs, tt.observeRel)
			}
			out := runWriteFile(t, root, obs, tt.args)
			gotErr := strings.HasPrefix(out, "error:")
			if gotErr != tt.wantErr {
				t.Fatalf("result = %q, wantErr = %v", out, tt.wantErr)
			}
			// The on-disk body is checked even on the expected-error rows so a
			// rejected write is proven NOT to have clobbered the existing bytes.
			if tt.wantOnDisk != "" {
				got, err := os.ReadFile(filepath.Join(root, tt.wantOnDisk))
				if err != nil {
					t.Fatalf("read written file: %v", err)
				}
				if string(got) != tt.wantBody {
					t.Errorf("on-disk body = %q, want %q", got, tt.wantBody)
				}
			}
		})
	}
}

// TestWriteFileSymlinkNotFollowed ensures a write through an in-workspace symlink
// that points OUTSIDE the workspace is rejected by containment (defense in depth;
// the gate also denies it).
func TestWriteFileSymlinkNotFollowed(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := t.TempDir()
	// link -> outside (an absolute symlink escaping the workspace).
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	out := runWriteFile(t, root, newFileObservations(), map[string]any{"path": "link/evil.txt", "content": "x"})
	if !strings.HasPrefix(out, "error:") {
		t.Fatalf("write via escaping symlink = %q, want an error", out)
	}
	if _, err := os.Stat(filepath.Join(outside, "evil.txt")); err == nil {
		t.Fatalf("write escaped to %s/evil.txt", outside)
	}
}

// TestWriteFileUnobservedSymlinkRejected asserts that a write to a path whose
// final component is an EXISTING in-workspace symlink is REFUSED under the
// optimistic-concurrency policy: a final-component symlink cannot be observed (a
// ReadFile of it fails O_NOFOLLOW with ELOOP and records no observation), so it is
// an existing-but-unverifiable path and any mutation is denied fail-secure. Neither
// the symlink node nor its target is touched — a strict hardening of the previous
// "replace the symlink" behavior.
func TestWriteFileUnobservedSymlinkRejected(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	const targetBody = "ORIGINAL TARGET BODY"
	target := filepath.Join(root, "target.txt")
	if err := os.WriteFile(target, []byte(targetBody), 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	out := runWriteFile(t, root, newFileObservations(), map[string]any{"path": "link.txt", "content": "NEW"})
	if !strings.HasPrefix(out, "error:") {
		t.Fatalf("write to unobserved symlink path = %q, want a fail-secure rejection", out)
	}

	// The symlink node is intact (not replaced) and still points at its target.
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat link: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link.txt is no longer a symlink; the rejected write mutated it")
	}
	// The symlink's target must be UNTOUCHED (the write neither followed nor
	// clobbered it).
	gotTarget, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(gotTarget) != targetBody {
		t.Fatalf("symlink target was clobbered: %q, want %q", gotTarget, targetBody)
	}
}

func TestWriteFileWriteTarget(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	wf := NewWriteFile(root, newFileObservations())

	// Valid args -> resolved path, ok=true.
	key, ok, err := wf.WriteTarget(`{"path":"sub/x.txt","content":"y"}`)
	if err != nil {
		t.Fatalf("WriteTarget err = %v", err)
	}
	if !ok {
		t.Fatalf("WriteTarget ok = false, want true")
	}
	want := resolvedJoin(t, root, filepath.Join("sub", "x.txt"))
	if key != want {
		t.Errorf("WriteTarget key = %q, want %q", key, want)
	}

	// Escape -> err, ok=false.
	if _, ok, err := wf.WriteTarget(`{"path":"../x.txt","content":"y"}`); ok || err == nil {
		t.Errorf("WriteTarget(escape) = (ok=%v, err=%v), want (false, non-nil)", ok, err)
	}
	// Unparseable args -> err, ok=false.
	if _, ok, err := wf.WriteTarget(`not json`); ok || err == nil {
		t.Errorf("WriteTarget(bad json) = (ok=%v, err=%v), want (false, non-nil)", ok, err)
	}
}

// Interim: the legacy gate-seam tests here were removed with the old harness
// prompt contracts; Task 3.3 adds PrepareCall coverage.

func TestWriteFileAuditSummary(t *testing.T) {
	t.Parallel()
	wf := NewWriteFile(t.TempDir(), newFileObservations())
	got := wf.AuditSummary(`{"path":"a/b.txt","content":"super secret payload"}`)
	if !strings.Contains(got, "a/b.txt") {
		t.Errorf("AuditSummary = %q, want it to contain the path", got)
	}
	if strings.Contains(got, "super secret payload") {
		t.Errorf("AuditSummary leaked content: %q", got)
	}
	if !strings.Contains(got, "bytes") {
		t.Errorf("AuditSummary = %q, want a byte count", got)
	}
	if got := wf.AuditSummary("not json"); !strings.Contains(got, "unparsable") {
		t.Errorf("AuditSummary(bad) = %q, want an unparsable note", got)
	}
}

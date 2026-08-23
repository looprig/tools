package filemutation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

var _ tool.MutationPreviewer = (*editFileArtifact)(nil)

// runEditFile invokes EditFile (bound to the given per-loop observation map) and
// extracts the single text block. Editing an existing file requires it to have been
// observed first (observeFile), the faithful read-then-edit optimistic-concurrency
// path.
func runEditFile(t *testing.T, root string, obs *fileObservations, args map[string]any) string {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return prepareRun(context.Background(), t, NewEditFile(root, obs), string(b))
}

func prepareEditPreviewArtifact(t *testing.T, root, path, old, replacement string, replaceAll bool, opts ...FileMutatorOption) *editFileArtifact {
	t.Helper()
	_, preparedArtifact, err := NewEditFile(root, newFileObservations(), opts...).PrepareCall(
		context.Background(),
		mustUUID(t),
		mustJSON(t, map[string]any{
			"path":        path,
			"old":         old,
			"new":         replacement,
			"replace_all": replaceAll,
		}),
	)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	art, ok := preparedArtifact.(*editFileArtifact)
	if !ok {
		t.Fatalf("PrepareCall() artifact = %T, want *editFileArtifact", preparedArtifact)
	}
	return art
}

func commitPreparedEditArtifact(t *testing.T, edit *EditFile, call tool.PreparedCall) string {
	t.Helper()
	ctx := loop.WithPreparedCall(context.Background(), call)
	result, err := edit.InvokableRun(ctx, "")
	if err != nil {
		t.Fatalf("InvokableRun() Go error = %v", err)
	}
	return textBlock(t, result)
}

func prepareHostEditArtifact(t *testing.T, path, old, replacement string, replaceAll bool) (*EditFile, tool.PreparedCall, *editFileArtifact) {
	t.Helper()
	edit := NewEditFile(t.TempDir(), newFileObservations(), WithHostWrites())
	executionID := mustUUID(t)
	request, preparedArtifact, err := edit.PrepareCall(
		context.Background(),
		executionID,
		mustJSON(t, map[string]any{
			"path":        path,
			"old":         old,
			"new":         replacement,
			"replace_all": replaceAll,
		}),
	)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	art, ok := preparedArtifact.(*editFileArtifact)
	if !ok {
		t.Fatalf("PrepareCall() artifact = %T, want *editFileArtifact", preparedArtifact)
	}
	return edit, tool.PreparedCall{ExecutionID: executionID, Request: request, Artifact: art}, art
}

// TestPrepareCallNeverReadsTheTarget is the ungated-oracle guard. A PrepareCall
// error reaches the model with NO gate opening, so if preparation touched the
// file, "does string S occur in host file F, and how many times?" would be
// answerable with zero approvals. Preparation must resolve paths only.
func TestPrepareCallNeverReadsTheTarget(t *testing.T) {
	for _, tc := range []struct {
		name string
		file []byte // nil means do not create it
		old  string
	}{
		{name: "missing file", file: nil, old: "anything"},
		{name: "substring absent", file: []byte("x\n"), old: "not-present"},
		{name: "substring ambiguous", file: []byte("dup\ndup\n"), old: "dup"},
		{name: "invalid UTF-8", file: []byte{0xff, 0xfe}, old: "anything"},
		{name: "oversized", file: bytes.Repeat([]byte("x"), int(maxPreviewFileBytes)+1), old: "x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.file != nil {
				if err := os.WriteFile(filepath.Join(root, "a.go"), tc.file, 0o600); err != nil {
					t.Fatalf("seed target: %v", err)
				}
			}
			_, _, err := NewEditFile(root, newFileObservations()).PrepareCall(
				context.Background(),
				mustUUID(t),
				mustJSON(t, map[string]any{"path": "a.go", "old": tc.old, "new": "new"}),
			)
			if err != nil {
				t.Fatalf("PrepareCall failed, leaking file state to the model: %v", err)
			}
		})
	}
}

// TestPreviewFailureDoesNotChangeTheToolResult pins that declining a preview is
// invisible to the model.
func TestPreviewFailureDoesNotChangeTheToolResult(t *testing.T) {
	run := func(t *testing.T, previewFirst bool) (string, string) {
		t.Helper()
		root := t.TempDir()
		target := filepath.Join(t.TempDir(), "host.go")
		const original = "x\n"
		if err := os.WriteFile(target, []byte(original), 0o600); err != nil {
			t.Fatalf("seed target: %v", err)
		}

		edit := NewEditFile(root, newFileObservations(), WithHostWrites())
		executionID := mustUUID(t)
		request, preparedArtifact, err := edit.PrepareCall(
			context.Background(),
			executionID,
			mustJSON(t, map[string]any{"path": target, "old": "old", "new": "new"}),
		)
		if err != nil {
			t.Fatalf("PrepareCall() error = %v", err)
		}
		art, ok := preparedArtifact.(*editFileArtifact)
		if !ok {
			t.Fatalf("PrepareCall() artifact = %T, want *editFileArtifact", preparedArtifact)
		}
		if previewFirst {
			preview, ok := art.MutationPreview()
			if ok || preview != (tool.MutationPreview{}) {
				t.Fatalf("MutationPreview() = (%+v, %v), want a declined zero preview", preview, ok)
			}
		}
		// Drift both fresh setups to the same applicable content after the optional
		// declined preview. A failed preview must not arm the host-only hash binding.
		const beforeCommit = "x\nold\nz\n"
		if err := os.WriteFile(target, []byte(beforeCommit), 0o600); err != nil {
			t.Fatalf("drift target after preparation: %v", err)
		}

		out := commitPreparedEditArtifact(t, edit, tool.PreparedCall{
			ExecutionID: executionID,
			Request:     request,
			Artifact:    art,
		})
		body, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("read target after commit: %v", err)
		}
		// Each fresh host setup necessarily has a distinct absolute path. Normalize
		// only that display value before comparing the model-facing text.
		return strings.ReplaceAll(out, target, "<target>"), string(body)
	}

	withPreview, withPreviewBody := run(t, true)
	withoutPreview, withoutPreviewBody := run(t, false)

	if withPreview != withoutPreview {
		t.Fatalf("previewing changed the model-facing result:\n%q\nvs\n%q", withPreview, withoutPreview)
	}
	if withPreviewBody != withoutPreviewBody {
		t.Fatalf("previewing changed the file effect:\n%q\nvs\n%q", withPreviewBody, withoutPreviewBody)
	}
	if strings.HasPrefix(withPreview, "error:") {
		t.Fatalf("equivalent host edits failed after a declined preview: %q", withPreview)
	}
	if want := "x\nnew\nz\n"; withPreviewBody != want {
		t.Fatalf("host edit body = %q, want %q", withPreviewBody, want)
	}
}

func TestUncontainedCommitRefusesWhenTheFileDriftedAfterPreview(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host.go")
	if err := os.WriteFile(path, []byte("x\nold\nz\n"), 0o600); err != nil {
		t.Fatalf("seed host target: %v", err)
	}
	edit, call, art := prepareHostEditArtifact(t, path, "old", "new", false)
	if _, ok := art.MutationPreview(); !ok {
		t.Fatal("MutationPreview() declined")
	}
	if err := os.WriteFile(path, []byte("x\nold\nDIFFERENT\n"), 0o600); err != nil {
		t.Fatalf("drift host target: %v", err)
	}

	out := commitPreparedEditArtifact(t, edit, call)

	if !strings.HasPrefix(out, "error:") {
		t.Fatalf("commit result = %q, want a refusal", out)
	}
	if !strings.Contains(out, "changed since preview") {
		t.Errorf("commit result = %q, want a changed-since-preview refusal", out)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read refused host target: %v", err)
	}
	if string(got) != "x\nold\nDIFFERENT\n" {
		t.Errorf("refused commit changed body to %q", got)
	}
}

func TestUncontainedCommitProceedsWithoutAPreview(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host.go")
	if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
		t.Fatalf("seed host target: %v", err)
	}
	edit, call, _ := prepareHostEditArtifact(t, path, "old", "new", false)

	out := commitPreparedEditArtifact(t, edit, call)

	if strings.HasPrefix(out, "error:") {
		t.Fatalf("an unpreviewed commit was refused: %s", out)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read committed host target: %v", err)
	}
	if string(got) != "new\n" {
		t.Errorf("committed body = %q, want %q", got, "new\n")
	}
}

func TestEditFilePreviewRendersPendingChange(t *testing.T) {
	root := t.TempDir()
	original := []byte("x\nold\nz\n")
	if err := os.WriteFile(filepath.Join(root, "a.go"), original, 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	art := prepareEditPreviewArtifact(t, root, "a.go", "old", "new", false)

	preview, ok := art.MutationPreview()

	if !ok {
		t.Fatal("MutationPreview() declined a well-formed edit")
	}
	if preview.Path != "a.go" {
		t.Errorf("MutationPreview().Path = %q, want %q", preview.Path, "a.go")
	}
	if preview.Creates {
		t.Error("MutationPreview().Creates = true for an existing file")
	}
	if !art.previewed || art.previewedHash != sha256.Sum256(original) {
		t.Errorf("successful MutationPreview() recorded previewed=%v hash=%x, want true and %x", art.previewed, art.previewedHash, sha256.Sum256(original))
	}
	for _, want := range []string{"--- a/a.go", "+++ b/a.go", "-old", "+new"} {
		if !strings.Contains(preview.UnifiedDiff, want) {
			t.Errorf("MutationPreview().UnifiedDiff missing %q:\n%s", want, preview.UnifiedDiff)
		}
	}
}

func TestEditFileSuccessfulResultReusesExactLargePreviewDiff(t *testing.T) {
	var original strings.Builder
	for i := 0; i < 160; i++ {
		fmt.Fprintf(&original, "OLD-%03d\n", i)
		for contextLine := 0; contextLine < 8; contextLine++ {
			fmt.Fprintf(&original, "keep-%03d-%d\n", i, contextLine)
		}
	}

	path := filepath.Join(t.TempDir(), "host.go")
	if err := os.WriteFile(path, []byte(original.String()), 0o600); err != nil {
		t.Fatalf("seed host target: %v", err)
	}
	edit, call, art := prepareHostEditArtifact(t, path, "OLD", "NEW", true)
	preview, ok := art.MutationPreview()
	if !ok {
		t.Fatal("MutationPreview() declined a valid large multi-hunk edit")
	}
	if len(preview.UnifiedDiff) <= maxToolResultDiffBytes {
		t.Fatalf("test fixture preview is only %d bytes; want more than the legacy %d-byte result budget", len(preview.UnifiedDiff), maxToolResultDiffBytes)
	}

	result := commitPreparedEditArtifact(t, edit, call)
	prefix := "edited " + path + "\n"
	if !strings.HasPrefix(result, prefix) {
		t.Fatalf("result = %q, want prefix %q", result, prefix)
	}
	resultDiff := strings.TrimPrefix(result, prefix)
	if resultDiff != preview.UnifiedDiff {
		t.Fatalf("successful result diff differs from the reviewed preview:\nresult bytes=%d\npreview bytes=%d", len(resultDiff), len(preview.UnifiedDiff))
	}
}

func TestEditFilePreviewDeclinesWithoutDisturbingLaterPreview(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "a.go")
	if err := os.WriteFile(target, []byte("x\n"), 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	art := prepareEditPreviewArtifact(t, root, "a.go", "missing", "new", false)

	preview, ok := art.MutationPreview()

	if ok {
		t.Fatalf("MutationPreview() succeeded for an unappliable edit: %+v", preview)
	}
	if preview != (tool.MutationPreview{}) {
		t.Errorf("MutationPreview() = %+v on decline, want zero preview", preview)
	}
	if art.previewed || art.previewedHash != ([32]byte{}) {
		t.Fatalf("failed MutationPreview() recorded preview state: previewed=%v hash=%x", art.previewed, art.previewedHash)
	}

	if err := os.WriteFile(target, []byte("missing\n"), 0o600); err != nil {
		t.Fatalf("make target appliable: %v", err)
	}
	if preview, ok := art.MutationPreview(); !ok || !strings.Contains(preview.UnifiedDiff, "+new") {
		t.Fatalf("later MutationPreview() = (%+v, %v), want a normal successful preview", preview, ok)
	}
}

func TestEditFilePreviewDeclinesForNonUTF8(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.bin"), []byte{0xff, 0xfe, 0x00}, 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	art := prepareEditPreviewArtifact(t, root, "a.bin", "\xff", "x", false)

	if preview, ok := art.MutationPreview(); ok || preview != (tool.MutationPreview{}) {
		t.Fatalf("MutationPreview() = (%+v, %v) for non-UTF-8 target, want (zero, false)", preview, ok)
	}
}

func TestEditFilePreviewDeclinesForUnsafeReadTargets(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		root := t.TempDir()
		art := prepareEditPreviewArtifact(t, root, "missing.txt", "old", "new", false)
		if preview, ok := art.MutationPreview(); ok || preview != (tool.MutationPreview{}) {
			t.Fatalf("MutationPreview() = (%+v, %v), want (zero, false)", preview, ok)
		}
	})

	t.Run("oversized", func(t *testing.T) {
		root := t.TempDir()
		body := strings.Repeat("x", int(maxEditFileBytes)+1)
		if err := os.WriteFile(filepath.Join(root, "large.txt"), []byte(body), 0o600); err != nil {
			t.Fatalf("seed oversized target: %v", err)
		}
		art := prepareEditPreviewArtifact(t, root, "large.txt", "x", "y", true)
		if preview, ok := art.MutationPreview(); ok || preview != (tool.MutationPreview{}) {
			t.Fatalf("MutationPreview() = (%+v, %v), want (zero, false)", preview, ok)
		}
	})

	t.Run("directory", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "dir"), 0o700); err != nil {
			t.Fatalf("seed directory target: %v", err)
		}
		art := prepareEditPreviewArtifact(t, root, "dir", "old", "new", false)
		if preview, ok := art.MutationPreview(); ok || preview != (tool.MutationPreview{}) {
			t.Fatalf("MutationPreview() = (%+v, %v), want (zero, false)", preview, ok)
		}
	})

	t.Run("final component symlink", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "target.txt"), []byte("old\n"), 0o600); err != nil {
			t.Fatalf("seed symlink target: %v", err)
		}
		if err := os.Symlink("target.txt", filepath.Join(root, "link.txt")); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		art := prepareEditPreviewArtifact(t, root, "link.txt", "old", "new", false)
		if preview, ok := art.MutationPreview(); ok || preview != (tool.MutationPreview{}) {
			t.Fatalf("MutationPreview() = (%+v, %v), want (zero, false)", preview, ok)
		}
	})
}

func TestEditFilePreviewDeclinesAfterParentResolutionChanges(t *testing.T) {
	root := t.TempDir()
	insideParent := filepath.Join(root, "dir")
	if err := os.Mkdir(insideParent, 0o700); err != nil {
		t.Fatalf("create approved parent: %v", err)
	}
	insideTarget := filepath.Join(insideParent, "target.txt")
	if err := os.WriteFile(insideTarget, []byte("approved old\n"), 0o600); err != nil {
		t.Fatalf("seed approved target: %v", err)
	}
	art := prepareEditPreviewArtifact(t, root, "dir/target.txt", "old", "new", false)

	outsideParent := t.TempDir()
	const outsideBody = "outside-secret old\n"
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

func TestEditFilePreviewWithHostWritesDeclinesAfterParentResolutionChanges(t *testing.T) {
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
	art := prepareEditPreviewArtifact(t, root, approvedTarget, "old", "new", false, WithHostWrites())

	retargetedParent := t.TempDir()
	const outsideBody = "host-secret old\n"
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

func TestEditFilePreviewDeclinesWhenReplacementExpansionExceedsLimit(t *testing.T) {
	root := t.TempDir()
	original := strings.Repeat("x", 512<<10)
	if err := os.WriteFile(filepath.Join(root, "expand.txt"), []byte(original), 0o600); err != nil {
		t.Fatalf("seed expansion target: %v", err)
	}
	art := prepareEditPreviewArtifact(t, root, "expand.txt", "x", "xxxx", true)

	preview, ok := art.MutationPreview()

	if ok || preview != (tool.MutationPreview{}) {
		t.Fatalf("MutationPreview() for a 2 MiB replacement result returned ok=%v path=%q creates=%v diffBytes=%d, want (zero, false)", ok, preview.Path, preview.Creates, len(preview.UnifiedDiff))
	}
	if art.previewed || art.previewedHash != ([32]byte{}) {
		t.Fatalf("declined oversized replacement recorded preview state: previewed=%v hash=%x", art.previewed, art.previewedHash)
	}
}

func TestEditFileInfo(t *testing.T) {
	t.Parallel()
	info, err := NewEditFile(t.TempDir(), newFileObservations()).Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	if info.Name != "EditFile" {
		t.Errorf("Info().Name = %q, want %q", info.Name, "EditFile")
	}
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(info.Schema, &schema); err != nil {
		t.Fatalf("Schema is not a JSON object: %v", err)
	}
}

func TestEditFile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		seed        string // initial file body ("" means do not create the file)
		args        map[string]any
		wantErr     bool
		wantBody    string   // expected on-disk body when !wantErr
		wantContain []string // substrings the result string must contain
	}{
		{
			name:        "single unique match is replaced",
			seed:        "alpha\nbravo\ncharlie\n",
			args:        map[string]any{"path": "f.txt", "old": "bravo", "new": "BRAVO"},
			wantBody:    "alpha\nBRAVO\ncharlie\n",
			wantContain: []string{"-bravo", "+BRAVO"},
		},
		{
			name:    "zero matches is not-found error",
			seed:    "alpha\nbravo\n",
			args:    map[string]any{"path": "f.txt", "old": "zulu", "new": "X"},
			wantErr: true,
		},
		{
			name:    "two matches without replace_all is ambiguous",
			seed:    "x\nx\n",
			args:    map[string]any{"path": "f.txt", "old": "x", "new": "y"},
			wantErr: true,
		},
		{
			name:     "two matches with replace_all replaces all",
			seed:     "x\nx\nother\n",
			args:     map[string]any{"path": "f.txt", "old": "x", "new": "y", "replace_all": true},
			wantBody: "y\ny\nother\n",
		},
		{
			name:     "replace_all with a single match still works",
			seed:     "only-one\n",
			args:     map[string]any{"path": "f.txt", "old": "only-one", "new": "two", "replace_all": true},
			wantBody: "two\n",
		},
		{
			name:    "missing file is an error",
			seed:    "", // not created
			args:    map[string]any{"path": "nope.txt", "old": "a", "new": "b"},
			wantErr: true,
		},
		{
			name:    "empty old is rejected",
			seed:    "hello\n",
			args:    map[string]any{"path": "f.txt", "old": "", "new": "b"},
			wantErr: true,
		},
		{
			name:    "escape path is rejected",
			seed:    "",
			args:    map[string]any{"path": "../f.txt", "old": "a", "new": "b"},
			wantErr: true,
		},
		{
			name:    "missing path is rejected",
			seed:    "",
			args:    map[string]any{"old": "a", "new": "b"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			obs := newFileObservations()
			if tt.seed != "" {
				if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte(tt.seed), 0o600); err != nil {
					t.Fatalf("seed: %v", err)
				}
				// Observe the seeded file so the edit is authorized (read-then-edit).
				observeFile(t, root, obs, "f.txt")
			}
			out := runEditFile(t, root, obs, tt.args)
			gotErr := strings.HasPrefix(out, "error:")
			if gotErr != tt.wantErr {
				t.Fatalf("result = %q, wantErr = %v", out, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if tt.wantBody != "" {
				got, err := os.ReadFile(filepath.Join(root, "f.txt"))
				if err != nil {
					t.Fatalf("read edited file: %v", err)
				}
				if string(got) != tt.wantBody {
					t.Errorf("on-disk body = %q, want %q", got, tt.wantBody)
				}
			}
			for _, sub := range tt.wantContain {
				if !strings.Contains(out, sub) {
					t.Errorf("result %q missing %q", out, sub)
				}
			}
		})
	}
}

// TestEditFileSymlinkFinalComponentRejected ensures EditFile, like ReadFile,
// REFUSES to follow a final-component symlink (even one that points to an
// in-workspace regular file): the open is on the LEXICAL joined path with
// O_NOFOLLOW, so the symlink's target is never read and never modified.
func TestEditFileSymlinkFinalComponentRejected(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// target is a real in-workspace file; link.txt -> target (final-component
	// symlink, both ends inside the workspace so containment passes).
	const targetBody = "alpha\nbravo\ncharlie\n"
	target := filepath.Join(root, "target.txt")
	if err := os.WriteFile(target, []byte(targetBody), 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	out := runEditFile(t, root, newFileObservations(), map[string]any{"path": "link.txt", "old": "bravo", "new": "BRAVO"})
	if !strings.HasPrefix(out, "error:") {
		t.Fatalf("edit via final-component symlink = %q, want an error", out)
	}
	// The symlink target must be untouched (the edit did not follow the link).
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(got) != targetBody {
		t.Fatalf("symlink target was modified: %q, want %q", got, targetBody)
	}
}

// TestEditFileDiffPreview verifies the result carries a unified-ish diff header
// and the changed lines.
func TestEditFileDiffPreview(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("one\ntwo\nthree\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	obs := newFileObservations()
	observeFile(t, root, obs, "f.txt")
	out := runEditFile(t, root, obs, map[string]any{"path": "f.txt", "old": "two", "new": "TWO"})
	for _, want := range []string{"--- a/f.txt", "+++ b/f.txt", "-two", "+TWO"} {
		if !strings.Contains(out, want) {
			t.Errorf("diff preview %q missing %q", out, want)
		}
	}
}

func TestEditFileWriteTarget(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ef := NewEditFile(root, newFileObservations())
	key, ok, err := ef.WriteTarget(`{"path":"sub/x.txt","old":"a","new":"b"}`)
	if err != nil || !ok {
		t.Fatalf("WriteTarget = (%q, %v, %v), want (path, true, nil)", key, ok, err)
	}
	want := resolvedJoin(t, root, filepath.Join("sub", "x.txt"))
	if key != want {
		t.Errorf("WriteTarget key = %q, want %q", key, want)
	}
	if _, ok, err := ef.WriteTarget(`{"path":"../x","old":"a","new":"b"}`); ok || err == nil {
		t.Errorf("WriteTarget(escape) = (ok=%v, err=%v), want (false, non-nil)", ok, err)
	}
}

// Interim: the legacy gate-seam tests here were removed with the old harness
// prompt contracts; Task 3.3 adds PrepareCall coverage.

func TestEditFileAuditSummary(t *testing.T) {
	t.Parallel()
	ef := NewEditFile(t.TempDir(), newFileObservations())
	got := ef.AuditSummary(`{"path":"a/b.txt","old":"secret-old","new":"secret-new"}`)
	if !strings.Contains(got, "a/b.txt") {
		t.Errorf("AuditSummary = %q, want the path", got)
	}
	if strings.Contains(got, "secret-old") || strings.Contains(got, "secret-new") {
		t.Errorf("AuditSummary leaked substrings: %q", got)
	}
}

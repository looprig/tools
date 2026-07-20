package permission

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/looprig/harness/pkg/tool"
)

func writeValidFile(t *testing.T, path string, rules []Rule) {
	t.Helper()
	encoded, err := encodeFile(rules)
	if err != nil {
		t.Fatalf("encodeFile: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func allowStatus() []Rule {
	return []Rule{{Effect: EffectAllow, Capability: CapabilityCommandExecute, Class: ClassCommandInvoke, Command: "git status"}}
}

func statusRequirement() tool.Requirement {
	return tool.Requirement{Kind: CapabilityCommandExecute, Match: "git status", Description: "d", GrantClass: GrantClassCommandStart, GrantTarget: "git status"}
}

// expectMatchError asserts that both matcher queries fail closed with a
// FileError of the given reason.
func expectMatchError(t *testing.T, store *Store, reason FileErrorReason) {
	t.Helper()
	for name, query := range map[string]func(context.Context, tool.Requirement) (bool, error){
		"MatchesDeny":  store.MatchesDeny,
		"MatchesAllow": store.MatchesAllow,
	} {
		matched, err := query(context.Background(), statusRequirement())
		if matched {
			t.Fatalf("%s matched against an insecure file", name)
		}
		var fileErr *FileError
		if !errors.As(err, &fileErr) || fileErr.Reason != reason {
			t.Fatalf("%s error = %v, want FileError reason %s", name, err, reason)
		}
	}
}

// TestHardeningRejections walks the required hardening matrix: symlink,
// owner, mode, link count, size, and version.
func TestHardeningRejections(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		path := testPath(t)
		real := filepath.Join(filepath.Dir(path), "real.json")
		writeValidFile(t, real, allowStatus())
		if err := os.Symlink(real, path); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		expectMatchError(t, mustWorkspaceStoreLenient(t, path), FileSymlink)
	})

	t.Run("group readable mode", func(t *testing.T) {
		path := testPath(t)
		writeValidFile(t, path, allowStatus())
		if err := os.Chmod(path, 0o640); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		expectMatchError(t, mustWorkspaceStoreLenient(t, path), FileModeUnexpected)
	})

	t.Run("world writable mode", func(t *testing.T) {
		path := testPath(t)
		writeValidFile(t, path, allowStatus())
		if err := os.Chmod(path, 0o666); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		expectMatchError(t, mustWorkspaceStoreLenient(t, path), FileModeUnexpected)
	})

	t.Run("unexpected owner", func(t *testing.T) {
		path := testPath(t)
		writeValidFile(t, path, allowStatus())
		store := mustWorkspaceStoreLenient(t, path)
		store.euid = os.Geteuid() + 1 // simulate a file owned by someone else
		expectMatchError(t, store, FileOwnerUnexpected)
	})

	t.Run("unexpected link count", func(t *testing.T) {
		path := testPath(t)
		writeValidFile(t, path, allowStatus())
		if err := os.Link(path, path+".hardlink"); err != nil {
			t.Fatalf("hardlink: %v", err)
		}
		expectMatchError(t, mustWorkspaceStoreLenient(t, path), FileLinkCount)
	})

	t.Run("oversized", func(t *testing.T) {
		path := testPath(t)
		writeValidFile(t, path, allowStatus())
		store, _, err := newWorkspaceStoreNoLoad(Config{Path: path, MaxFileBytes: 16})
		if err != nil {
			t.Fatalf("construct: %v", err)
		}
		expectMatchError(t, store, FileTooLarge)
	})

	t.Run("unsupported version", func(t *testing.T) {
		path := testPath(t)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(`{"version":1,"normalization_version":1,"rules":[]}`), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		expectMatchError(t, mustWorkspaceStoreLenient(t, path), FileVersionUnsupported)
	})
}

// mustWorkspaceStoreLenient builds a workspace store without the
// construction-time load so each subtest can observe the exact match-time
// failure.
func mustWorkspaceStoreLenient(t *testing.T, path string) *Store {
	t.Helper()
	store, _, err := newWorkspaceStoreNoLoad(Config{Path: path})
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	return store
}

// TestWorkspaceConstructionFailsOnInsecureFile proves interactive
// construction refuses an existing insecure or malformed file instead of
// silently starting with different authority.
func TestWorkspaceConstructionFailsOnInsecureFile(t *testing.T) {
	path := testPath(t)
	writeValidFile(t, path, allowStatus())
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, _, err := NewWorkspaceStore(Config{Path: path}); err == nil {
		t.Fatal("NewWorkspaceStore accepted a group/world-readable file")
	}
}

// TestWriteRefusesInsecureExistingFile proves a workspace write against a
// tampered existing file fails and leaves it untouched.
func TestWriteRefusesInsecureExistingFile(t *testing.T) {
	path := testPath(t)
	writeValidFile(t, path, allowStatus())
	store := mustWorkspaceStoreLenient(t, path)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	before, _ := os.ReadFile(path)
	if err := store.WriteRules(context.Background(), []tool.RuleCandidate{commandCandidate("git diff")}); err == nil {
		t.Fatal("WriteRules merged through an insecure file")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("failed write altered the insecure file")
	}
}

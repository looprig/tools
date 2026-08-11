package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDiscoverWorkspaceSkillMetadata(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeSkill := func(name, document string) {
		t.Helper()
		dir := filepath.Join(root, workspaceSkillsDir, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, skillFileName), []byte(document), 0o644); err != nil {
			t.Fatalf("WriteFile(%q): %v", name, err)
		}
	}

	writeSkill("alpha", "---\nname: alpha\ndescription: Alpha description.\n---\nSECRET BODY MARKER\n")
	writeSkill("zeta", "---\nname: zeta\ndescription: Zeta description.\n---\nSECRET ZETA BODY\n")
	writeSkill("bravo", "---\nname: bravo\ndescription: Bravo description.\n---\nSECRET BRAVO BODY\n")
	writeSkill("broken", "not frontmatter\nSECRET BROKEN BODY\n")
	writeSkill("empty-name", "---\nname:   \ndescription: Missing a name.\n---\nSECRET EMPTY NAME BODY\n")
	writeSkill("empty-description", "---\nname: empty-description\ndescription:   \n---\nSECRET EMPTY BODY\n")
	writeSkill("mismatch", "---\nname: another-name\ndescription: Cannot be loaded by its advertised name.\n---\nSECRET MISMATCH BODY\n")
	writeSkill("..escape", "---\nname: escape\ndescription: Must not escape.\n---\nSECRET ESCAPE BODY\n")

	outside := t.TempDir()
	outsideSkill := filepath.Join(outside, skillFileName)
	if err := os.WriteFile(outsideSkill, []byte("---\nname: linked\ndescription: Must not follow.\n---\nSECRET LINK BODY\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(outside): %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, workspaceSkillsDir, "linked")); err != nil {
		t.Logf("Symlink unavailable; traversal-like directory still covers unsafe candidate rejection: %v", err)
	}
	if err := os.Symlink("alpha", filepath.Join(root, workspaceSkillsDir, "alpha-link")); err != nil {
		t.Logf("Symlink unavailable; outside symlink candidate still covers symlink rejection: %v", err)
	}

	got := DiscoverWorkspaceSkills(root)
	want := []SkillMeta{
		{Name: "alpha", Description: "Alpha description."},
		{Name: "bravo", Description: "Bravo description."},
		{Name: "zeta", Description: "Zeta description."},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DiscoverWorkspaceSkills() = %#v, want %#v", got, want)
	}
	for _, meta := range got {
		if strings.Contains(meta.Name, "SECRET") || strings.Contains(meta.Description, "SECRET") {
			t.Fatalf("DiscoverWorkspaceSkills() leaked a skill body: %#v", got)
		}
	}
}

func TestDiscoverWorkspaceSkillsRejectsSkillsDirectorySymlink(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	outside := t.TempDir()
	outSkill := filepath.Join(outside, "outside", skillFileName)
	if err := os.MkdirAll(filepath.Dir(outSkill), 0o755); err != nil {
		t.Fatalf("MkdirAll(outside): %v", err)
	}
	if err := os.WriteFile(outSkill, []byte("---\nname: outside\ndescription: Must not follow the catalog symlink.\n---\nSECRET OUTSIDE BODY\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(outside): %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, workspaceSkillsDir)); err != nil {
		t.Skipf("Symlink unavailable: %v", err)
	}
	if got := DiscoverWorkspaceSkills(root); len(got) != 0 {
		t.Fatalf("DiscoverWorkspaceSkills() = %#v, want no metadata through .skills symlink", got)
	}
}

func TestDiscoverWorkspaceSkillsRejectsCandidateDirectorySymlink(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	target := filepath.Join(root, workspaceSkillsDir, "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("MkdirAll(target): %v", err)
	}
	document := "---\nname: linked\ndescription: Must not follow an in-root directory symlink.\n---\nSECRET LINK BODY\n"
	if err := os.WriteFile(filepath.Join(target, skillFileName), []byte(document), 0o644); err != nil {
		t.Fatalf("WriteFile(target): %v", err)
	}
	if err := os.Symlink("target", filepath.Join(root, workspaceSkillsDir, "linked")); err != nil {
		t.Skipf("Symlink unavailable: %v", err)
	}

	if got := DiscoverWorkspaceSkills(root); len(got) != 0 {
		t.Fatalf("DiscoverWorkspaceSkills() = %#v, want no metadata through candidate symlink", got)
	}
}

func TestDiscoverWorkspaceSkillsBoundsAggregateWork(t *testing.T) {
	t.Parallel()

	t.Run("result limit", func(t *testing.T) {
		root := t.TempDir()
		const wantLimit = 32
		for i := wantLimit + 7; i >= 0; i-- {
			name := fmt.Sprintf("skill-%02d", i)
			dir := filepath.Join(root, workspaceSkillsDir, name)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatalf("MkdirAll(%q): %v", name, err)
			}
			document := fmt.Sprintf("---\nname: %s\ndescription: Description %d.\n---\nBODY %d\n", name, i, i)
			if err := os.WriteFile(filepath.Join(dir, skillFileName), []byte(document), 0o644); err != nil {
				t.Fatalf("WriteFile(%q): %v", name, err)
			}
		}

		var stats workspaceDiscoveryStats
		got := discoverWorkspaceSkills(root, &stats)
		if len(got) != wantLimit || stats.results != wantLimit {
			t.Fatalf("results = (%d metadata, %d stats), want hard limit %d", len(got), stats.results, wantLimit)
		}
		for i, meta := range got {
			if want := fmt.Sprintf("skill-%02d", i); meta.Name != want {
				t.Fatalf("metadata[%d].Name = %q, want deterministic member %q", i, meta.Name, want)
			}
		}
	})

	t.Run("document limit", func(t *testing.T) {
		root := t.TempDir()
		for i := 0; i < 72; i++ {
			name := fmt.Sprintf("broken-%02d", i)
			dir := filepath.Join(root, workspaceSkillsDir, name)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatalf("MkdirAll(%q): %v", name, err)
			}
			if err := os.WriteFile(filepath.Join(dir, skillFileName), []byte("malformed"), 0o644); err != nil {
				t.Fatalf("WriteFile(%q): %v", name, err)
			}
		}

		var stats workspaceDiscoveryStats
		if got := discoverWorkspaceSkills(root, &stats); len(got) != 0 {
			t.Fatalf("discoverWorkspaceSkills() = %#v, want no malformed metadata", got)
		}
		if stats.documents != 64 {
			t.Fatalf("documents inspected = %d, want hard limit 64", stats.documents)
		}
	})

	t.Run("oversized directory is empty and batched", func(t *testing.T) {
		root := t.TempDir()
		for i := 0; i < 257; i++ {
			name := fmt.Sprintf("entry-%03d", i)
			dir := filepath.Join(root, workspaceSkillsDir, name)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatalf("MkdirAll(%q): %v", name, err)
			}
			document := fmt.Sprintf("---\nname: %s\ndescription: Entry %d.\n---\nBODY\n", name, i)
			if err := os.WriteFile(filepath.Join(dir, skillFileName), []byte(document), 0o644); err != nil {
				t.Fatalf("WriteFile(%q): %v", name, err)
			}
		}

		var stats workspaceDiscoveryStats
		if got := discoverWorkspaceSkills(root, &stats); len(got) != 0 {
			t.Fatalf("discoverWorkspaceSkills() returned %d entries, want empty for oversized directory", len(got))
		}
		if stats.candidates != 257 || !stats.oversized {
			t.Fatalf("directory stats = {candidates:%d oversized:%v}, want {257 true}", stats.candidates, stats.oversized)
		}
		if stats.maxBatch > 32 || stats.readBatches < 2 {
			t.Fatalf("ReadDir batching = {max:%d calls:%d}, want max 32 across multiple calls", stats.maxBatch, stats.readBatches)
		}
	})
}

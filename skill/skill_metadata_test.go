package skill

import (
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

	got := DiscoverWorkspaceSkills(root)
	want := []SkillMeta{{Name: "alpha", Description: "Alpha description."}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DiscoverWorkspaceSkills() = %#v, want %#v", got, want)
	}
	for _, meta := range got {
		if strings.Contains(meta.Name, "SECRET") || strings.Contains(meta.Description, "SECRET") {
			t.Fatalf("DiscoverWorkspaceSkills() leaked a skill body: %#v", got)
		}
	}
}

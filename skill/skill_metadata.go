package skill

import (
	"io/fs"
	"os"
	"sort"
)

// DiscoverWorkspaceSkills returns validated metadata for direct
// .skills/<name>/SKILL.md candidates beneath root. Discovery is intentionally
// metadata-only and fail-soft: malformed, unreadable, empty, mismatched, or
// unsafe candidates are omitted, and filesystem errors are not exposed to the
// caller. Skill bodies remain available only through the gated Skill tool.
func DiscoverWorkspaceSkills(root string) []SkillMeta {
	osRoot, err := os.OpenRoot(root)
	if err != nil {
		return nil
	}
	defer func() { _ = osRoot.Close() }()

	info, err := osRoot.Lstat(workspaceSkillsDir)
	if err != nil || info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return nil
	}
	skillsRoot, err := osRoot.OpenRoot(workspaceSkillsDir)
	if err != nil {
		return nil
	}
	defer func() { _ = skillsRoot.Close() }()
	openedInfo, err := skillsRoot.Stat(".")
	if err != nil || !os.SameFile(info, openedInfo) {
		return nil
	}

	dir, err := skillsRoot.Open(".")
	if err != nil {
		return nil
	}
	entries, err := dir.ReadDir(-1)
	_ = dir.Close()
	if err != nil {
		return nil
	}

	metas := make([]SkillMeta, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		// Validate before the candidate name is used to construct a path.
		if entry.Type()&fs.ModeSymlink != 0 || !entry.IsDir() || validateSkillName(name) != nil {
			continue
		}

		candidateInfo, err := skillsRoot.Lstat(name)
		if err != nil || candidateInfo.Mode()&fs.ModeSymlink != 0 || !candidateInfo.IsDir() {
			continue
		}
		candidateRoot, err := skillsRoot.OpenRoot(name)
		if err != nil {
			continue
		}
		openedCandidateInfo, statErr := candidateRoot.Stat(".")
		if statErr != nil || !os.SameFile(candidateInfo, openedCandidateInfo) {
			_ = candidateRoot.Close()
			continue
		}

		var meta SkillMeta
		artifactPath := workspaceSkillsDir + "/" + name + "/" + skillFileName
		_, err = loadWorkspaceSkillAtRoot(candidateRoot, skillFileName, artifactPath, name, &meta)
		_ = candidateRoot.Close()
		if err != nil {
			continue
		}
		// The directory is the loadable canonical identity. Requiring the parsed
		// name to match avoids advertising an identity the Skill tool cannot load.
		if meta.Name == "" || meta.Description == "" || meta.Name != name {
			continue
		}
		if _, exists := seen[meta.Name]; exists {
			continue
		}
		seen[meta.Name] = struct{}{}
		metas = append(metas, meta)
	}

	sort.Slice(metas, func(i, j int) bool { return metas[i].Name < metas[j].Name })
	return metas
}

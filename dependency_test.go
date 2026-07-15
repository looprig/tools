package tools

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestToolPackageLayout(t *testing.T) {
	for _, name := range []string{
		"askuser",
		"bash",
		"editfile",
		"fetch",
		"glob",
		"grep",
		"permission",
		"readfile",
		"skill",
		"todo",
		"websearch",
		"writefile",
	} {
		info, err := os.Stat(name)
		if err != nil {
			t.Errorf("required package directory %q: %v", name, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("required package path %q is not a directory", name)
		}
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".go" || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		if entry.Name() != "definitions.go" {
			t.Errorf("production implementation remains at module root: %s", entry.Name())
		}
	}
}

func TestProductionDependencyBoundary(t *testing.T) {
	forbidden := []string{
		"github.com/looprig/sandbox",
		"github.com/looprig/confinement",
		"github.com/looprig/coderig",
		"github.com/looprig/harness/internal/",
	}
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != "." && (entry.Name() == "vendor" || strings.HasPrefix(entry.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			for _, prefix := range forbidden {
				if strings.HasPrefix(importPath, prefix) {
					t.Errorf("%s imports forbidden optional or internal dependency %q", path, importPath)
				}
			}
			if path != "definitions.go" && strings.HasPrefix(importPath, "github.com/looprig/tools/") &&
				!strings.HasPrefix(importPath, "github.com/looprig/tools/internal/") {
				t.Errorf("%s imports sibling public tool package %q", path, importPath)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

package tools

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
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
		"task",
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
	if info, err := os.Stat("todo"); err == nil {
		if info.IsDir() {
			t.Errorf("removed package directory %q still exists", "todo")
		} else {
			t.Errorf("removed package path %q still exists and is not a directory", "todo")
		}
	} else if !os.IsNotExist(err) {
		t.Errorf("stat removed package path %q: %v", "todo", err)
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

func TestTodoRemoved(t *testing.T) {
	forbidden := []string{
		strings.Join([]string{"Todo", "Definition"}, ""),
		strings.Join([]string{"New", "Todo"}, ""),
		strings.Join([]string{"github.com/looprig/tools/", "todo"}, ""),
	}

	if info, err := os.Stat("todo"); err == nil {
		t.Errorf("removed package path %q still exists (directory=%t)", "todo", info.IsDir())
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat removed package path %q: %v", "todo", err)
	}

	files, err := trackedSourceAndDocumentation()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, needle := range forbidden {
			if strings.Contains(string(contents), needle) {
				t.Errorf("%s contains removed API reference %q", path, needle)
			}
		}
	}
}

func trackedSourceAndDocumentation() ([]string, error) {
	output, err := exec.Command("git", "ls-files", "-z", "--", "*.go", "*.md").Output()
	if err != nil {
		return nil, err
	}
	var files []string
	for _, path := range strings.Split(string(output), "\x00") {
		if path == "" || strings.HasPrefix(path, "docs/plans/") {
			continue
		}
		if filepath.Ext(path) == ".go" || filepath.Ext(path) == ".md" {
			files = append(files, path)
		}
	}
	return files, nil
}

func TestProductionDependencyBoundary(t *testing.T) {
	forbidden := []string{
		"github.com/looprig/sandbox",
		"github.com/looprig/confinement",
		"github.com/looprig/coderig",
		"github.com/looprig/carbon",
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
			// Tool packages stay independent of each other. The permission
			// package is deliberately NOT a tool: it is the shared workspace
			// rule library whose exported canonical requirement-Match
			// encodings (TreeMatch etc.) are the pinned contract between tool
			// preparation and stored-rule matching, so preparation code may
			// import it.
			//
			// bash is additionally permitted to import process: bash/
			// supervised.go routes a SUPERVISED Bash call through the shared,
			// runner-free process.Supervisor (Task 19's long-running-command-
			// supervision work). Like permission, process is a shared,
			// standalone mechanics library here, not a sibling tool package
			// Bash bundles — this exemption is scoped to the bash package
			// only and does not widen any other package's allowed imports,
			// and Sandbox/Harness-internal imports remain forbidden above.
			allowedSiblingImport := importPath == "github.com/looprig/tools/permission" ||
				(importPath == "github.com/looprig/tools/process" && filepath.Dir(path) == "bash")
			if path != "definitions.go" && strings.HasPrefix(importPath, "github.com/looprig/tools/") &&
				!strings.HasPrefix(importPath, "github.com/looprig/tools/internal/") &&
				!allowedSiblingImport {
				t.Errorf("%s imports sibling public tool package %q", path, importPath)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

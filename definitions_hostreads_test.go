package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/looprig/tools/editfile"
	"github.com/looprig/tools/glob"
	"github.com/looprig/tools/readfile"
	"github.com/looprig/tools/writefile"
)

// TestReadFileDefinitionThreadsOptions proves ReadFileDefinition's variadic
// options actually reach readfile.NewReadFile -- passing WithHostReads()
// changes the built tool's advertised description, which only happens if the
// option was threaded through rather than silently dropped.
func TestReadFileDefinitionThreadsOptions(t *testing.T) {
	t.Parallel()
	guard := &fakeReadGuard{maxBytes: 1024}

	def := ReadFileDefinition(guard, readfile.WithHostReads())
	built, err := def.Build(context.Background(), blueprintBindings())
	if err != nil {
		t.Fatal(err)
	}
	if len(built) != 1 {
		t.Fatalf("Build() returned %d tools, want 1", len(built))
	}
	info, err := built[0].Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(info.Desc, "Reads are confined to the workspace") {
		t.Errorf("ReadFileDefinition(guard, WithHostReads()).Desc = %q, still advertises workspace-only confinement", info.Desc)
	}
}

// TestGlobDefinitionThreadsOptions proves GlobDefinition's variadic options
// actually reach glob.NewGlob, mirroring TestReadFileDefinitionThreadsOptions.
func TestGlobDefinitionThreadsOptions(t *testing.T) {
	t.Parallel()
	guard := &fakeReadGuard{maxBytes: 1024}

	def := GlobDefinition(guard, glob.WithHostReads())
	built, err := def.Build(context.Background(), blueprintBindings())
	if err != nil {
		t.Fatal(err)
	}
	if len(built) != 1 {
		t.Fatalf("Build() returned %d tools, want 1", len(built))
	}
	info, err := built[0].Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(info.Desc, "Results are confined to the workspace") {
		t.Errorf("GlobDefinition(guard, WithHostReads()).Desc = %q, still advertises workspace-only confinement", info.Desc)
	}
}

// TestWriteFileDefinitionThreadsOptions proves WriteFileDefinition's variadic
// options actually reach writefile.New -- passing WithHostWrites() changes
// the built tool's advertised description, which only happens if the option
// was threaded through rather than silently dropped, mirroring
// TestReadFileDefinitionThreadsOptions.
func TestWriteFileDefinitionThreadsOptions(t *testing.T) {
	t.Parallel()

	def := WriteFileDefinition(writefile.WithHostWrites())
	built, err := def.Build(context.Background(), blueprintBindings())
	if err != nil {
		t.Fatal(err)
	}
	if len(built) != 1 {
		t.Fatalf("Build() returned %d tools, want 1", len(built))
	}
	info, err := built[0].Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(info.Desc, "confined to the workspace") {
		t.Errorf("WriteFileDefinition(WithHostWrites()).Desc = %q, still advertises workspace-only confinement", info.Desc)
	}
}

// TestEditFileDefinitionThreadsOptions proves EditFileDefinition's variadic
// options actually reach editfile.New -- passing WithHostWrites() changes
// the built tool's advertised description, which only happens if the option
// was threaded through rather than silently dropped, mirroring
// TestReadFileDefinitionThreadsOptions.
func TestEditFileDefinitionThreadsOptions(t *testing.T) {
	t.Parallel()

	def := EditFileDefinition(editfile.WithHostWrites())
	built, err := def.Build(context.Background(), blueprintBindings())
	if err != nil {
		t.Fatal(err)
	}
	if len(built) != 1 {
		t.Fatalf("Build() returned %d tools, want 1", len(built))
	}
	info, err := built[0].Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(info.Desc, "confined to the workspace") {
		t.Errorf("EditFileDefinition(WithHostWrites()).Desc = %q, still advertises workspace-only confinement", info.Desc)
	}
}

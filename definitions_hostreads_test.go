package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/looprig/tools/glob"
	"github.com/looprig/tools/readfile"
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

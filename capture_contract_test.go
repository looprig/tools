package tools

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"sort"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/bash"
)

// captureContractRow is one standard definition's audited capture behaviour:
// the materialized maximum and truncation point it has, and the declaration
// that follows from them.
type captureContractRow struct {
	constructor string
	definition  func(t *testing.T) (tool.Definition, tool.Bindings)
	want        tool.DeclaredCaptureSafety
	// bound documents the audit: the tool's own maximum materialized result
	// and where it truncates. It is not asserted; it is the reason for want.
	bound string
}

type stubToolResultReader struct{}

func (stubToolResultReader) ReadToolResult(context.Context, tool.ToolResultPageRequest) (tool.ToolResultPage, error) {
	return tool.ToolResultPage{}, &tool.ToolResultReadError{Kind: tool.ToolResultReadUnknownCapture}
}

func captureContract() []captureContractRow {
	guard := &fakeReadGuard{maxBytes: 1024}
	plain := func(definition tool.Definition) func(*testing.T) (tool.Definition, tool.Bindings) {
		return func(*testing.T) (tool.Definition, tool.Bindings) { return definition, blueprintBindings() }
	}
	withProcess := func(definition tool.Definition) func(*testing.T) (tool.Definition, tool.Bindings) {
		return func(t *testing.T) (tool.Definition, tool.Bindings) {
			return definition, blueprintBindingsWithProcess(&fakeProcessRegistry{dir: t.TempDir()})
		}
	}
	streaming := tool.DeclaredCaptureSafety{Streaming: true, HighOutput: true}
	materializedHigh := tool.DeclaredCaptureSafety{HighOutput: true}
	small := tool.DeclaredCaptureSafety{}
	return []captureContractRow{
		{constructor: "Bash", definition: plain(Bash(bash.WithRunner(&definitionRunner{}))), want: streaming,
			bound: "unbounded command output; streams every byte to the sink before its 32 KiB head/tail preview"},
		{constructor: "BashDefinition", definition: withProcess(BashDefinition(func(context.Context, uuid.UUID) (tool.AsyncProcessRunner, error) {
			return &fakeAsyncProcessRunner{}, nil
		})), want: streaming,
			bound: "synchronous path as Bash; supervised terminal output streams the retained spool (64 MiB default ceiling)"},
		{constructor: "ProcessOutputDefinition", definition: withProcess(ProcessOutputDefinition()), want: materializedHigh,
			bound: "materializes limit_bytes per process (model-chosen, up to the spool ceiling); paged by cursor"},
		{constructor: "ProcessInputDefinition", definition: withProcess(ProcessInputDefinition()), want: materializedHigh,
			bound: "returns the same materialized snapshot shape as ProcessOutput"},
		{constructor: "ProcessStopDefinition", definition: withProcess(ProcessStopDefinition()), want: small,
			bound: "fixed-size JSON status per process"},
		{constructor: "ReadFileDefinition", definition: plain(ReadFileDefinition(guard)), want: materializedHigh,
			bound: "materializes up to the read guard's MaxReadBytes, which the consumer sets"},
		{constructor: "GrepDefinition", definition: plain(GrepDefinition(guard)), want: materializedHigh,
			bound: "at most 200 matches of at most 64 KiB each (~12.5 MiB)"},
		{constructor: "GlobDefinition", definition: plain(GlobDefinition(guard)), want: small,
			bound: "at most 500 paths"},
		{constructor: "FetchDefinition", definition: plain(FetchDefinition(http.DefaultClient)), want: small,
			bound: "64 KiB body cap plus at most 16 header lines"},
		{constructor: "WebSearchDefinition", definition: plain(WebSearchDefinition(&definitionProvider{})), want: small,
			bound: "at most 10 results"},
		{constructor: "WriteFileDefinition", definition: plain(WriteFileDefinition()), want: small,
			bound: "a short confirmation"},
		{constructor: "EditFileDefinition", definition: plain(EditFileDefinition()), want: small,
			bound: "a short confirmation"},
		{constructor: "TaskDefinitions", definition: plain(TaskDefinitions()), want: small,
			bound: "a bounded in-memory task graph"},
		{constructor: "AskUserDefinition", definition: plain(AskUserDefinition()), want: small,
			bound: "the user's answer"},
		{constructor: "ReadToolResultDefinition", definition: func(*testing.T) (tool.Definition, tool.Bindings) {
			bindings := blueprintBindings()
			bindings.ToolResults = stubToolResultReader{}
			return ReadToolResultDefinition(), bindings
		}, want: small,
			bound: "one page, at most 64 KiB and fitted under the loop's preview budget by Harness"},
	}
}

// TestStandardDefinitionsDeclareCaptureSafety proves every standard
// definition declares its capture safety explicitly, and that a streaming
// declaration is honest: every tool the definition builds implements
// tool.CapturingInvokableTool, and no materialized definition claims to.
func TestStandardDefinitionsDeclareCaptureSafety(t *testing.T) {
	t.Parallel()
	for _, row := range captureContract() {
		t.Run(row.constructor, func(t *testing.T) {
			t.Parallel()
			definition, bindings := row.definition(t)
			declarer, ok := definition.(tool.CaptureSafetyDeclarer)
			if !ok {
				t.Fatalf("%s does not declare capture safety", row.constructor)
			}
			if got := declarer.DeclaredCaptureSafety(); got != row.want {
				t.Fatalf("%s declares %+v, want %+v (%s)", row.constructor, got, row.want, row.bound)
			}
			built, err := definition.Build(context.Background(), bindings)
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			for _, builtTool := range built {
				_, capturing := builtTool.(tool.CapturingInvokableTool)
				if capturing != row.want.Streaming {
					t.Fatalf("%s built %T: implements CapturingInvokableTool = %t, declared Streaming = %t", row.constructor, builtTool, capturing, row.want.Streaming)
				}
			}
		})
	}
}

// TestStandardAssemblyIsCaptureSafe projects the whole standard set the way a
// composition root would: safe under Harness's finite materialized maximum,
// and — with no finite maximum — unsafe exactly for the materialized
// high-output definitions, never for Bash.
func TestStandardAssemblyIsCaptureSafe(t *testing.T) {
	t.Parallel()
	var definitions []tool.Definition
	for _, row := range captureContract() {
		definition, _ := row.definition(t)
		definitions = append(definitions, definition)
	}
	if descriptor := tool.ProjectCaptureSafety(definitions, 32<<20); !descriptor.Safe() {
		t.Fatalf("standard assembly unsafe under a finite maximum: %v", descriptor.Unsafe())
	}
	unsafe := tool.ProjectCaptureSafety(definitions, 0).Unsafe()
	want := []string{"Grep", "ProcessInput", "ProcessOutput", "ReadFile"}
	if len(unsafe) != len(want) {
		t.Fatalf("unsafe without a finite maximum = %v, want %v", unsafe, want)
	}
	for i := range want {
		if unsafe[i] != want[i] {
			t.Fatalf("unsafe without a finite maximum = %v, want %v", unsafe, want)
		}
	}
}

// TestEveryExportedDefinitionHasACaptureContractRow fails when a new standard
// definition constructor is added to definitions.go without an audited row.
func TestEveryExportedDefinitionHasACaptureContractRow(t *testing.T) {
	t.Parallel()
	file, err := parser.ParseFile(token.NewFileSet(), "definitions.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var exported []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || !fn.Name.IsExported() || fn.Type.Results == nil || len(fn.Type.Results.List) != 1 {
			continue
		}
		if selector, ok := fn.Type.Results.List[0].Type.(*ast.SelectorExpr); ok && selector.Sel.Name == "Definition" {
			exported = append(exported, fn.Name.Name)
		}
	}
	rows := make(map[string]bool)
	for _, row := range captureContract() {
		rows[row.constructor] = true
	}
	sort.Strings(exported)
	for _, name := range exported {
		if !rows[name] {
			t.Errorf("definitions.go exports %s with no capture-safety contract row", name)
		}
		delete(rows, name)
	}
	for name := range rows {
		t.Errorf("capture-safety contract row %s names no exported definition constructor", name)
	}
	if len(exported) == 0 {
		t.Fatal("found no exported definition constructors; the guard is vacuous")
	}
}

// TestReadToolResultDefinition proves the reader definition declares the
// harness requirement bit, produces the harness-named tool, and fails closed
// without a reader.
func TestReadToolResultDefinition(t *testing.T) {
	t.Parallel()
	definition := ReadToolResultDefinition()
	if definition.Requirements() != tool.RequiresToolResultReader {
		t.Fatalf("Requirements() = %v, want RequiresToolResultReader only", definition.Requirements())
	}
	if names := definition.ProducedToolNames(); len(names) != 1 || names[0] != loop.ReadToolResultToolName {
		t.Fatalf("ProducedToolNames() = %v, want [%s]", names, loop.ReadToolResultToolName)
	}
	bindings := blueprintBindings()
	bindings.ToolResults = stubToolResultReader{}
	built, err := definition.Build(context.Background(), bindings)
	if err != nil || len(built) != 1 {
		t.Fatalf("Build() = %v, %v", built, err)
	}
	if _, ok := built[0].(tool.CallPreparer); !ok {
		t.Fatalf("built %T is not prepared", built[0])
	}
	if _, err := definition.Build(context.Background(), blueprintBindings()); err == nil {
		t.Fatal("Build() without a reader succeeded; want the harness binding refusal")
	}
}

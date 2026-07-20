package filemutation

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

type fakeReadGuard struct {
	maxBytes int64
	denied   map[string]bool
}

func newFakeReadGuard(maxBytes int64, denied ...string) *fakeReadGuard {
	guard := &fakeReadGuard{maxBytes: maxBytes, denied: make(map[string]bool)}
	for _, path := range denied {
		guard.denied[path] = true
	}
	return guard
}

func (guard *fakeReadGuard) DeniedRead(path string) bool { return guard.denied[path] }
func (guard *fakeReadGuard) MaxReadBytes() int64         { return guard.maxBytes }

func resolvedJoin(t *testing.T, root, relativePath string) string {
	t.Helper()
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	absolutePath, err := filepath.Abs(filepath.Join(resolvedRoot, relativePath))
	if err != nil {
		t.Fatal(err)
	}
	return absolutePath
}

// textBlock extracts the single text block from a tool result, failing the test
// on any structural surprise.
func textBlock(t *testing.T, res *tool.ToolResult) string {
	t.Helper()
	if res == nil || len(res.Content) != 1 {
		t.Fatalf("result = %v, want exactly 1 block", res)
	}
	tb, ok := res.Content[0].(*content.TextBlock)
	if !ok {
		t.Fatalf("block type = %T, want *content.TextBlock", res.Content[0])
	}
	return tb.Text
}

// prepareRun mirrors the runner's prepared-execution contract for a tool in
// tests: mint an execution ID, PrepareCall once, validate the request, install
// the prepared call on ctx, and execute. A preparation or validation failure is
// surfaced as the runner's fail-secure tool-result string so error-path tests
// keep asserting on "error:"-prefixed results.
func prepareRun(ctx context.Context, t *testing.T, tl tool.InvokableTool, argsJSON string) string {
	t.Helper()
	preparer, ok := tl.(tool.CallPreparer)
	if !ok {
		t.Fatalf("tool %T does not implement tool.CallPreparer; effectful tools fail closed", tl)
	}
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() error = %v", err)
	}
	req, art, err := preparer.PrepareCall(ctx, id, argsJSON)
	if err != nil {
		return "error: tool preparation failed: " + err.Error()
	}
	if err := tool.ValidateRequest(req); err != nil {
		return "error: tool preparation failed: " + err.Error()
	}
	ctx = loop.WithPreparedCall(ctx, tool.PreparedCall{ExecutionID: id, Request: req, Artifact: art})
	res, err := tl.InvokableRun(ctx, argsJSON)
	if err != nil {
		t.Fatalf("InvokableRun returned a Go error %v; tool failures are tool-result strings", err)
	}
	return textBlock(t, res)
}

// invokePrepared prepares the call and executes the tool under the given ctx,
// returning the raw tool result. Preparation must succeed (Fatalf otherwise) —
// for asserting on run-time behavior under a controlled context.
func invokePrepared(ctx context.Context, t *testing.T, tl tool.InvokableTool, argsJSON string) (*tool.ToolResult, error) {
	t.Helper()
	preparer, ok := tl.(tool.CallPreparer)
	if !ok {
		t.Fatalf("tool %T does not implement tool.CallPreparer", tl)
	}
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() error = %v", err)
	}
	req, art, err := preparer.PrepareCall(context.Background(), id, argsJSON)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	return tl.InvokableRun(loop.WithPreparedCall(ctx, tool.PreparedCall{ExecutionID: id, Request: req, Artifact: art}), argsJSON)
}

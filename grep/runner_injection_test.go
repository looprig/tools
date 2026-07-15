package grep

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/tool"
)

func rgOnPath() bool {
	_, err := exec.LookPath(rgBinary)
	return err == nil
}

type fakeArgvRunner struct {
	calls   int
	gotDir  string
	gotArgv []string
	out     []byte
	exit    int
	err     error
}

func (runner *fakeArgvRunner) RunArgv(_ context.Context, dir string, argv []string) ([]byte, int, error) {
	runner.calls++
	runner.gotDir = dir
	runner.gotArgv = append([]string(nil), argv...)
	return runner.out, runner.exit, runner.err
}

var _ tool.ArgvRunner = (*fakeArgvRunner)(nil)

func TestGrepWithArgvRunnerRoutesArgv(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.go"), "package main\nfunc target() {}\n")
	absA := resolvedJoin(t, root, "a.go")
	runner := &fakeArgvRunner{out: []byte(absA + ":2:func target() {}\n")}
	grep := newGrepWithBackend(root, newFakeReadGuard(1<<20), true, WithArgvRunner(runner))
	result, err := grep.InvokableRun(context.Background(), `{"pattern":"target"}`)
	if err != nil {
		t.Fatal(err)
	}
	block := result.Content[0].(*content.TextBlock)
	if runner.calls != 1 || runner.gotDir != root {
		t.Fatalf("runner calls=%d dir=%q", runner.calls, runner.gotDir)
	}
	if len(runner.gotArgv) == 0 || runner.gotArgv[0] != rgBinary {
		t.Fatalf("argv = %v, want %q first", runner.gotArgv, rgBinary)
	}
	assertAdjacent(t, runner.gotArgv, "--regexp", "target")
	if !strings.Contains(block.Text, "a.go") || !strings.Contains(block.Text, "func target") {
		t.Errorf("result %q did not contain the routed match", block.Text)
	}
}

func TestGrepWithArgvRunnerTimeout(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.go"), "// findme\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	grep := newGrepWithBackend(root, newFakeReadGuard(1<<20), true, WithArgvRunner(&fakeArgvRunner{err: context.Canceled}))
	result, err := grep.InvokableRun(ctx, `{"pattern":"findme"}`)
	if err != nil {
		t.Fatal(err)
	}
	if text := result.Content[0].(*content.TextBlock).Text; !strings.Contains(text, "timed out") {
		t.Errorf("result %q, want timeout", text)
	}
}

func TestGrepNilArgvRunnerDirectExec(t *testing.T) {
	t.Parallel()
	if !rgOnPath() {
		t.Skip("ripgrep not on PATH")
	}
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.go"), "package main\nfunc target() {}\n")
	grep := newGrepWithBackend(root, newFakeReadGuard(1<<20), true)
	if grep.argvRunner != nil {
		t.Fatal("nil runner should preserve direct execution")
	}
	result, err := grep.InvokableRun(context.Background(), `{"pattern":"target"}`)
	if err != nil {
		t.Fatal(err)
	}
	text := result.Content[0].(*content.TextBlock).Text
	if !strings.Contains(text, "a.go") || !strings.Contains(text, "func target") {
		t.Errorf("result %q did not contain the direct match", text)
	}
}

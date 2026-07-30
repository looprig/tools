package bash

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/looprig/harness/pkg/tool"
)

// fakeGrantedRunner implements BOTH tool.CommandRunner and tool.GrantedRunner. It
// records which method the Bash tool dispatched to and the grant tokens it was
// handed, so a test can assert the grant-aware routing (PreparedCall grants
// present + supported → RunCommandWithGrants; otherwise RunCommand).
type fakeGrantedRunner struct {
	ranPlain   bool
	ranGrants  bool
	gotCommand string
	gotDir     string
	gotGrants  []string
	out        []byte
	exit       int
	err        error
}

func (f *fakeGrantedRunner) RunCommand(_ context.Context, dir, command string) ([]byte, int, error) {
	f.ranPlain = true
	f.gotDir = dir
	f.gotCommand = command
	return f.out, f.exit, f.err
}

func (f *fakeGrantedRunner) RunCommandWithGrants(_ context.Context, dir, command string, grants []string) ([]byte, int, error) {
	f.ranGrants = true
	f.gotDir = dir
	f.gotCommand = command
	f.gotGrants = append([]string(nil), grants...)
	return f.out, f.exit, f.err
}

// Compile-time assertions: the fake satisfies both runner interfaces (so the
// sandbox Executor, which does too, is routed identically).
var (
	_ tool.CommandRunner = (*fakeGrantedRunner)(nil)
	_ tool.GrantedRunner = (*fakeGrantedRunner)(nil)
)

// TestBashGrantDispatch exercises the PreparedCall-grant + GrantedRunner
// routing: issued tokens travel ONLY on the prepared execution path (there is
// no model-facing grants argument and no ambient grant context).
func TestBashGrantDispatch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		grants      []string
		wantGranted bool
	}{
		{name: "prepared grants route to the grants method", grants: []string{"tok-a", "tok-b"}, wantGranted: true},
		{name: "no grants uses the plain RunCommand path", wantGranted: false},
		{name: "empty grants slice uses the plain path", grants: []string{}, wantGranted: false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			fake := &fakeGrantedRunner{out: []byte("ROUTED\n"), exit: 0}
			b := NewBash(root, WithRunner(fake))

			out := runPrepared(t, b, `{"command":"echo hi"}`, tt.grants)

			if fake.ranGrants != tt.wantGranted {
				t.Fatalf("ranGrants = %v, want %v", fake.ranGrants, tt.wantGranted)
			}
			if fake.ranPlain != !tt.wantGranted {
				t.Fatalf("ranPlain = %v, want %v", fake.ranPlain, !tt.wantGranted)
			}
			if tt.wantGranted && !slices.Equal(fake.gotGrants, tt.grants) {
				t.Errorf("grants handed to runner = %#v, want %#v", fake.gotGrants, tt.grants)
			}
			if fake.gotCommand != "echo hi" {
				t.Errorf("runner saw command %q, want %q", fake.gotCommand, "echo hi")
			}
			if fake.gotDir != root {
				t.Errorf("runner saw dir %q, want %q", fake.gotDir, root)
			}
			if !strings.Contains(out, "ROUTED") {
				t.Errorf("result %q missing the runner's output", out)
			}
		})
	}
}

// TestBashGrantsCommandRunnerOnlyFallsBack asserts that when grants are present
// but the injected runner implements ONLY tool.CommandRunner (no GrantedRunner),
// Bash falls back to RunCommand without panicking. The tokens are simply ignored
// at the exec layer (the gate already resolved the decision).
func TestBashGrantsCommandRunnerOnlyFallsBack(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	fake := &fakeCommandRunner{out: []byte("PLAIN\n"), exit: 0}
	b := NewBash(root, WithRunner(fake))

	out := runPrepared(t, b, `{"command":"echo hi"}`, []string{"tok-a"})

	if fake.calls != 1 {
		t.Fatalf("RunCommand calls = %d, want 1 (grants present but runner is CommandRunner-only → fall back)", fake.calls)
	}
	if fake.gotCommand != "echo hi" {
		t.Errorf("runner saw command %q, want %q", fake.gotCommand, "echo hi")
	}
	if !strings.Contains(out, "PLAIN") {
		t.Errorf("result %q missing the runner's output", out)
	}
}

// TestBashLegacyGrantDispatchUnaffectedByNewFields proves grant-aware
// dispatch (issued PreparedCall tokens routing to RunCommandWithGrants) is
// identical whether the call carries only the pre-existing fields or ALSO
// carries the new supervision fields at their legacy-equivalent zero values.
// Task 14 adds no runner routing change, so both calls must route to the
// grants method identically with the same tokens, dir, and command.
func TestBashLegacyGrantDispatchUnaffectedByNewFields(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		argsRaw string
	}{
		{name: "legacy-only fields", argsRaw: `{"command":"echo hi"}`},
		{name: "explicit-false supervision fields", argsRaw: `{"command":"echo hi","background":false,"tty":false}`},
	}
	grants := []string{"tok-a", "tok-b"}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			fake := &fakeGrantedRunner{out: []byte("ROUTED\n"), exit: 0}
			b := NewBash(root, WithRunner(fake))

			out := runPrepared(t, b, tt.argsRaw, grants)

			if !fake.ranGrants || fake.ranPlain {
				t.Fatalf("dispatch = grants:%v plain:%v, want the granted runner path", fake.ranGrants, fake.ranPlain)
			}
			if !slices.Equal(fake.gotGrants, grants) {
				t.Errorf("grants handed to runner = %#v, want %#v", fake.gotGrants, grants)
			}
			if fake.gotCommand != "echo hi" || fake.gotDir != root {
				t.Errorf("runner saw command/dir %q/%q, want %q/%q", fake.gotCommand, fake.gotDir, "echo hi", root)
			}
			if !strings.Contains(out, "ROUTED") {
				t.Errorf("result %q missing the runner's output", out)
			}
		})
	}
}

// TestBashGrantsNilRunnerDirectExec asserts grants present with NO injected
// runner (the bare-harness default) still direct-execs via sh -c without
// panicking; the grants are ignored at the exec layer.
func TestBashGrantsNilRunnerDirectExec(t *testing.T) {
	t.Parallel()
	requireSh(t)
	b := NewBash(t.TempDir()) // nil runner
	out := runPrepared(t, b, `{"command":"echo hello"}`, []string{"tok-a"})
	if !strings.Contains(out, "hello") || !strings.Contains(out, "[exit code: 0]") {
		t.Errorf("nil-runner Bash with grants did not direct-exec; got %q", out)
	}
}

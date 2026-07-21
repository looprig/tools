package permission

import (
	"testing"

	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/tool"
)

// The Store is consumed structurally by the harness gate evaluator as its
// durable rule matcher and writer. These assertions pin the contracts.
var (
	_ gate.RuleMatcher = (*Store)(nil)
	_ gate.RuleWriter  = (*Store)(nil)
)

// TestIdentifiersAlignWithHarness pins the identifier values shared with
// harness preparation so independent modules cannot drift silently.
func TestIdentifiersAlignWithHarness(t *testing.T) {
	if CapabilityCommandExecute != tool.CapabilityCommandExecute {
		t.Fatalf("CapabilityCommandExecute = %q, harness uses %q", CapabilityCommandExecute, tool.CapabilityCommandExecute)
	}
	if GrantClassCommandStart != tool.GrantClassCommandStart {
		t.Fatalf("GrantClassCommandStart = %q, harness uses %q", GrantClassCommandStart, tool.GrantClassCommandStart)
	}
}

// TestGrantClassesAlignWithSandbox pins every grant enforcement-class string
// shared with the sandbox enforcement side so the sandbox<->tools security seam
// cannot drift silently. Sandbox's validateGrantClass accepts these classes and
// sandbox exports them as named constants (GrantClass* / …) whose values are
// asserted by sandbox's own grant_class_test.go.
//
// tools must NOT depend on the sandbox module — the production dependency
// boundary (tools/dependency_test.go: TestProductionDependencyBoundary)
// forbids importing github.com/looprig/sandbox, and adding a test-only import
// would still pull sandbox into tools' module graph. So instead of comparing
// against the sandbox constants directly, both sides independently pin the same
// literal strings: this test pins the tools side, sandbox's value test pins the
// sandbox side, and any rename that changes a value fails on whichever side
// changed. The literals below MUST equal sandbox's exported constant values.
func TestGrantClassesAlignWithSandbox(t *testing.T) {
	cases := []struct {
		name string
		got  string
		// sandbox constant name — kept in the message so a mismatch points at
		// the exact seam partner to reconcile.
		sandboxConst string
		want         string
	}{
		{"GrantClassCommandStart", GrantClassCommandStart, "sandbox.GrantClassCommandStart", "command.start.v1"},
		{"GrantClassNetworkProxyTarget", GrantClassNetworkProxyTarget, "sandbox.GrantClassNetworkProxyTarget", "network.proxy-target.v1"},
		{"ClassNetworkBroad", ClassNetworkBroad, "sandbox.GrantClassNetworkBroad", "network.broad.v1"},
		{"ClassFilesystemPathRead", ClassFilesystemPathRead, "sandbox.GrantClassFilesystemPathRead", "filesystem.path.read.v1"},
		{"ClassFilesystemTreeRead", ClassFilesystemTreeRead, "sandbox.GrantClassFilesystemTreeRead", "filesystem.tree.read.v1"},
		{"ClassFilesystemHostRead", ClassFilesystemHostRead, "sandbox.GrantClassFilesystemHostRead", "filesystem.host.read.v1"},
		{"ClassFilesystemPathWrite", ClassFilesystemPathWrite, "sandbox.GrantClassFilesystemPathWrite", "filesystem.path.write.v1"},
		{"ClassFilesystemTreeWrite", ClassFilesystemTreeWrite, "sandbox.GrantClassFilesystemTreeWrite", "filesystem.tree.write.v1"},
		{"ClassFilesystemHostWrite", ClassFilesystemHostWrite, "sandbox.GrantClassFilesystemHostWrite", "filesystem.host.write.v1"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q (must equal %s)", c.name, c.got, c.want, c.sandboxConst)
		}
	}
}

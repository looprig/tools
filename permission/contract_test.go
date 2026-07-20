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

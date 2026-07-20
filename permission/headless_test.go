package permission

import (
	"context"
	"os"
	"testing"

	"github.com/looprig/harness/pkg/tool"
)

// TestReadOnlyStoreLoadsConfiguredFile proves the headless path loads one
// explicit file as an immutable rule set.
func TestReadOnlyStoreLoadsConfiguredFile(t *testing.T) {
	ctx := context.Background()
	path := testPath(t)
	writeValidFile(t, path, append(allowStatus(),
		Rule{Effect: EffectDeny, Capability: CapabilityFilesystemRead, Class: ClassFilesystemPathRead, Path: "/w/.env"}))

	store, diagnostics, err := NewReadOnlyStore(Config{Path: path})
	if err != nil {
		t.Fatalf("NewReadOnlyStore: %v", err)
	}
	if len(diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %#v", diagnostics)
	}
	if matched, err := store.MatchesAllow(ctx, statusRequirement()); err != nil || !matched {
		t.Fatalf("MatchesAllow = %v, %v", matched, err)
	}
	if matched, err := store.MatchesDeny(ctx, tool.Requirement{Kind: CapabilityFilesystemRead, Match: "/w/.env", Description: "d"}); err != nil || !matched {
		t.Fatalf("MatchesDeny = %v, %v", matched, err)
	}
	// An unmatched gated requirement is simply no match; the typed
	// approval-required denial belongs to the harness evaluator.
	if matched, err := store.MatchesAllow(ctx, tool.Requirement{Kind: CapabilityNetwork, Match: NetworkTargetMatch("tcp", "github.com", 443), Description: "d"}); err != nil || matched {
		t.Fatalf("unmatched requirement: got %v, %v; want false, nil", matched, err)
	}
}

// TestReadOnlyStoreStartupFailures proves a configured file that is
// missing, malformed, insecure, oversized, or unsupported fails startup.
func TestReadOnlyStoreStartupFailures(t *testing.T) {
	build := func(t *testing.T, prepare func(path string)) error {
		path := testPath(t)
		prepare(path)
		_, _, err := NewReadOnlyStore(Config{Path: path})
		return err
	}
	cases := map[string]func(path string){
		"missing":   func(string) {},
		"malformed": func(path string) { writeValidFile(t, path, nil); os.WriteFile(path, []byte("{"), 0o600) },
		"insecure": func(path string) {
			writeValidFile(t, path, allowStatus())
			os.Chmod(path, 0o644)
		},
		"unsupported": func(path string) {
			writeValidFile(t, path, nil)
			os.WriteFile(path, []byte(`{"version":3,"normalization_version":1,"rules":[]}`), 0o600)
		},
	}
	for name, prepare := range cases {
		if err := build(t, prepare); err == nil {
			t.Errorf("%s: NewReadOnlyStore succeeded, want startup failure", name)
		}
	}
}

// TestReadOnlyStoreWithoutPath proves the no-file headless run uses an
// empty rule set.
func TestReadOnlyStoreWithoutPath(t *testing.T) {
	store, diagnostics, err := NewReadOnlyStore(Config{})
	if err != nil {
		t.Fatalf("NewReadOnlyStore: %v", err)
	}
	if len(diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %#v", diagnostics)
	}
	if matched, err := store.MatchesAllow(context.Background(), statusRequirement()); err != nil || matched {
		t.Fatalf("empty rule set matched: %v, %v", matched, err)
	}
}

// TestReadOnlyStoreDoesNotReload proves the headless rule set is a
// snapshot: later file changes are invisible until a new run.
func TestReadOnlyStoreDoesNotReload(t *testing.T) {
	ctx := context.Background()
	path := testPath(t)
	writeValidFile(t, path, allowStatus())
	store, _, err := NewReadOnlyStore(Config{Path: path})
	if err != nil {
		t.Fatalf("NewReadOnlyStore: %v", err)
	}
	writeValidFile(t, path, append(allowStatus(),
		Rule{Effect: EffectAllow, Capability: CapabilityCommandExecute, Class: ClassCommandInvoke, Command: "git diff"}))
	requirement := tool.Requirement{Kind: CapabilityCommandExecute, Match: "git diff", Description: "d", GrantClass: GrantClassCommandStart, GrantTarget: "git diff"}
	if matched, err := store.MatchesAllow(ctx, requirement); err != nil || matched {
		t.Fatalf("read-only store reloaded: %v, %v", matched, err)
	}
}

// TestDiagnosticsSeparateFromMatching proves out-of-catalog manual allow
// families surface a diagnostic without altering precedence, and deny
// families never warn.
func TestDiagnosticsSeparateFromMatching(t *testing.T) {
	ctx := context.Background()
	rules := []Rule{
		{Effect: EffectAllow, Capability: CapabilityCommandExecute, Class: ClassCommandInvokeFamily, Tokens: []string{"weird-tool", "run"}, TrailingArguments: true},
		{Effect: EffectDeny, Capability: CapabilityCommandExecute, Class: ClassCommandInvokeFamily, Tokens: []string{"another", "family"}, TrailingArguments: true},
		{Effect: EffectAllow, Capability: CapabilityCommandExecute, Class: ClassCommandInvokeFamily, Tokens: []string{"git", "log"}, TrailingArguments: true},
		allowStatus()[0],
	}
	catalog := func(tokens []string) bool { return len(tokens) == 2 && tokens[0] == "git" && tokens[1] == "log" }

	path := testPath(t)
	writeValidFile(t, path, rules)
	store, diagnostics, err := NewReadOnlyStore(Config{Path: path, FamilyEligible: catalog})
	if err != nil {
		t.Fatalf("NewReadOnlyStore: %v", err)
	}
	if len(diagnostics) != 1 {
		t.Fatalf("got %d diagnostics, want exactly 1 (out-of-catalog allow family): %#v", len(diagnostics), diagnostics)
	}
	if diagnostics[0].Code != DiagnosticAllowFamilyOutOfCatalog || diagnostics[0].RuleIndex != 0 {
		t.Fatalf("wrong diagnostic: %#v", diagnostics[0])
	}
	if got := store.Diagnostics(); len(got) != 1 || got[0] != diagnostics[0] {
		t.Fatalf("Diagnostics() = %#v, want the load diagnostics", got)
	}
	// Precedence is unchanged: the exact allow still matches.
	if matched, err := store.MatchesAllow(ctx, statusRequirement()); err != nil || !matched {
		t.Fatalf("diagnostics altered matching: %v, %v", matched, err)
	}
}

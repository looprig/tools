package permission

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/looprig/harness/pkg/tool"
)

func testPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "store", "permissions.json")
}

func mustWorkspaceStore(t *testing.T, cfg Config) *Store {
	t.Helper()
	store, _, err := NewWorkspaceStore(cfg)
	if err != nil {
		t.Fatalf("NewWorkspaceStore: %v", err)
	}
	return store
}

func commandCandidate(command string) tool.RuleCandidate {
	return tool.RuleCandidate{
		Kind:        CapabilityCommandExecute,
		Match:       command,
		Description: "run " + command,
		GrantClass:  GrantClassCommandStart,
		GrantTarget: command,
	}
}

// TestWriteRulesPersistsDisplayedCandidates proves a workspace approval
// appends the exact displayed allow batch and that a later store instance
// (a later session) matches it.
func TestWriteRulesPersistsDisplayedCandidates(t *testing.T) {
	ctx := context.Background()
	path := testPath(t)
	store := mustWorkspaceStore(t, Config{Path: path})

	batch := []tool.RuleCandidate{
		commandCandidate("git status"),
		{Kind: CapabilityNetwork, Match: NetworkTargetMatch("tcp", "github.com", 443), Description: "d"},
	}
	if err := store.WriteRules(ctx, batch); err != nil {
		t.Fatalf("WriteRules: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat written file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %o, want 0600", info.Mode().Perm())
	}

	later := mustWorkspaceStore(t, Config{Path: path})
	for _, requirement := range []tool.Requirement{
		{Kind: CapabilityCommandExecute, Match: "git status", Description: "d", GrantClass: GrantClassCommandStart, GrantTarget: "git status"},
		{Kind: CapabilityNetwork, Match: NetworkTargetMatch("tcp", "github.com", 443), Description: "d"},
	} {
		matched, err := later.MatchesAllow(ctx, requirement)
		if err != nil {
			t.Fatalf("MatchesAllow(%s): %v", requirement.Kind, err)
		}
		if !matched {
			t.Fatalf("persisted candidate did not match %s requirement", requirement.Kind)
		}
	}
	if matched, err := later.MatchesDeny(ctx, tool.Requirement{Kind: CapabilityCommandExecute, Match: "git status", Description: "d", GrantClass: GrantClassCommandStart, GrantTarget: "git status"}); err != nil || matched {
		t.Fatalf("MatchesDeny = %v, %v; want false, nil", matched, err)
	}

	// Writing the same batch twice does not duplicate records.
	if err := later.WriteRules(ctx, batch); err != nil {
		t.Fatalf("idempotent WriteRules: %v", err)
	}
	rules := loadFileRules(t, path)
	if len(rules) != 2 {
		t.Fatalf("got %d rules after duplicate write, want 2", len(rules))
	}
}

func loadFileRules(t *testing.T, path string) []Rule {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	rules, err := decodeFile(data)
	if err != nil {
		t.Fatalf("decode written file: %v", err)
	}
	return rules
}

// TestWriteRulesMergePreservesForeignRules proves the locked re-read/merge
// keeps rules written by another process, including denies.
func TestWriteRulesMergePreservesForeignRules(t *testing.T) {
	ctx := context.Background()
	path := testPath(t)
	store := mustWorkspaceStore(t, Config{Path: path})
	if err := store.WriteRules(ctx, []tool.RuleCandidate{commandCandidate("git status")}); err != nil {
		t.Fatalf("first write: %v", err)
	}

	// A concurrent process (simulated) rewrites the file with a deny rule.
	foreign := append(loadFileRules(t, path), Rule{Effect: EffectDeny, Capability: CapabilityFilesystemRead, Class: ClassFilesystemPathRead, Path: "/w/.env"})
	encoded, err := encodeFile(foreign)
	if err != nil {
		t.Fatalf("encode foreign: %v", err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	if err := store.WriteRules(ctx, []tool.RuleCandidate{commandCandidate("git diff")}); err != nil {
		t.Fatalf("second write: %v", err)
	}
	rules := loadFileRules(t, path)
	if len(rules) != 3 {
		t.Fatalf("got %d rules, want 3 (merge dropped a foreign rule): %#v", len(rules), rules)
	}
	if matched, err := store.MatchesDeny(ctx, tool.Requirement{Kind: CapabilityFilesystemRead, Match: "/w/.env", Description: "d"}); err != nil || !matched {
		t.Fatalf("foreign deny lost after merge: %v, %v", matched, err)
	}
}

// TestWriteRulesRollback proves an injected write failure leaves the prior
// complete file intact, removes the temporary file, and reports an error so
// the approved call is blocked.
func TestWriteRulesRollback(t *testing.T) {
	ctx := context.Background()
	path := testPath(t)
	store := mustWorkspaceStore(t, Config{Path: path})
	if err := store.WriteRules(ctx, []tool.RuleCandidate{commandCandidate("git status")}); err != nil {
		t.Fatalf("initial write: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}

	injected := errors.New("injected failure")
	for name, breakStore := range map[string]func(*Store){
		"rename fails": func(s *Store) {
			s.renameFile = func(string, string) error { return injected }
		},
		"fsync fails": func(s *Store) {
			s.syncFile = func(interface{ Sync() error }) error { return injected }
		},
	} {
		broken := mustWorkspaceStore(t, Config{Path: path})
		breakStore(broken)
		err := broken.WriteRules(ctx, []tool.RuleCandidate{commandCandidate("rm -rf /")})
		if !errors.Is(err, injected) {
			t.Fatalf("%s: WriteRules error = %v, want injected failure", name, err)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: read after: %v", name, err)
		}
		if string(after) != string(before) {
			t.Fatalf("%s: failed write altered the permission file", name)
		}
		entries, err := os.ReadDir(filepath.Dir(path))
		if err != nil {
			t.Fatalf("%s: readdir: %v", name, err)
		}
		for _, entry := range entries {
			if name := entry.Name(); name != "permissions.json" && name != "permissions.json.lock" {
				t.Fatalf("%s: leftover file %q after failed write", name, entry.Name())
			}
		}
	}
}

// TestWriteRulesRejectsInvalidCandidates proves nothing is persisted when
// any candidate in the batch is invalid.
func TestWriteRulesRejectsInvalidCandidates(t *testing.T) {
	ctx := context.Background()
	path := testPath(t)
	store := mustWorkspaceStore(t, Config{Path: path})
	batches := map[string][]tool.RuleCandidate{
		"unknown kind":                     {{Kind: "tool.invoke", Match: "mcp:server/tool", Description: "d"}},
		"family with foreign capability":   {{Kind: CapabilityNetwork, Match: "Bash(git log:*)", Description: "d"}},
		"wildcard with foreign capability": {{Kind: CapabilityFilesystemRead, Match: "Bash(*)", Description: "d"}},
		"family with unsafe token":         {{Kind: CapabilityCommandExecute, Match: "Bash(git log;rm:*)", Description: "d", GrantClass: GrantClassCommandStart, GrantTarget: "git log"}},
		"relative filesystem path":         {{Kind: CapabilityFilesystemWrite, Match: "out.txt", Description: "d"}},
		"good then bad": {
			commandCandidate("git status"),
			{Kind: CapabilityNetwork, Match: "not a target", Description: "d"},
		},
	}
	for name, batch := range batches {
		if err := store.WriteRules(ctx, batch); err == nil {
			t.Errorf("%s: WriteRules succeeded, want error", name)
		}
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a rejected batch persisted a file: %v", err)
	}
}

// TestWriteRulesFamilyAndWildcardCandidates proves the Bash(...) display
// syntax converts to structured records, never raw prefixes.
func TestWriteRulesFamilyAndWildcardCandidates(t *testing.T) {
	ctx := context.Background()
	path := testPath(t)
	store := mustWorkspaceStore(t, Config{Path: path})
	batch := []tool.RuleCandidate{
		{Kind: CapabilityCommandExecute, Match: "Bash(git log:*)", Description: "d", GrantClass: GrantClassCommandStart, GrantTarget: "git log -n 3"},
		{Kind: CapabilityCommandExecute, Match: "Bash(*)", Description: "d", GrantClass: GrantClassCommandStart, GrantTarget: "true"},
	}
	if err := store.WriteRules(ctx, batch); err != nil {
		t.Fatalf("WriteRules: %v", err)
	}
	rules := loadFileRules(t, path)
	if len(rules) != 2 {
		t.Fatalf("got %d rules, want 2", len(rules))
	}
	family, wildcard := rules[0], rules[1]
	if family.Class != ClassCommandInvokeFamily || family.Tokens[0] != "git" || family.Tokens[1] != "log" || !family.TrailingArguments {
		t.Fatalf("family candidate stored wrong: %#v", family)
	}
	if wildcard.Class != ClassCommandInvokeWildcard {
		t.Fatalf("wildcard candidate stored wrong: %#v", wildcard)
	}
}

// TestReadOnlyStoreRejectsWrites proves headless stores never persist.
func TestReadOnlyStoreRejectsWrites(t *testing.T) {
	ctx := context.Background()
	store, _, err := NewReadOnlyStore(Config{})
	if err != nil {
		t.Fatalf("NewReadOnlyStore: %v", err)
	}
	if err := store.WriteRules(ctx, []tool.RuleCandidate{commandCandidate("git status")}); err == nil {
		t.Fatal("read-only store accepted a write")
	}
}

// TestConcurrentInterprocessMerge proves concurrent writers on the same
// path never lose a batch. Each goroutine uses its own Store instance with
// its own lock-file descriptor, so BSD flock semantics genuinely contend
// exactly as two CodeRig processes would (flock locks belong to the open
// file description, not the process).
func TestConcurrentInterprocessMerge(t *testing.T) {
	ctx := context.Background()
	path := testPath(t)
	const writers = 8

	var group sync.WaitGroup
	errs := make([]error, writers)
	for i := range writers {
		group.Add(1)
		go func() {
			defer group.Done()
			store, _, err := NewWorkspaceStore(Config{Path: path})
			if err != nil {
				errs[i] = err
				return
			}
			errs[i] = store.WriteRules(ctx, []tool.RuleCandidate{commandCandidate("cmd-" + strconv.Itoa(i))})
		}()
	}
	group.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}

	rules := loadFileRules(t, path)
	if len(rules) != writers {
		t.Fatalf("got %d rules, want %d (a concurrent batch was lost)", len(rules), writers)
	}
	store := mustWorkspaceStore(t, Config{Path: path})
	for i := range writers {
		command := "cmd-" + strconv.Itoa(i)
		matched, err := store.MatchesAllow(ctx, tool.Requirement{Kind: CapabilityCommandExecute, Match: command, Description: "d", GrantClass: GrantClassCommandStart, GrantTarget: command})
		if err != nil || !matched {
			t.Fatalf("writer %d batch lost: %v, %v", i, matched, err)
		}
	}
}

// TestStoreFamilyMatchingEndToEnd proves a persisted Bash(git log:*) approval
// satisfies later exact commands through MatchesAllow, never crosses a
// segment boundary, and composes with a later-approved exact segment.
func TestStoreFamilyMatchingEndToEnd(t *testing.T) {
	ctx := context.Background()
	path := testPath(t)
	store := mustWorkspaceStore(t, Config{Path: path})

	family := tool.RuleCandidate{Kind: CapabilityCommandExecute, Match: "Bash(git log:*)", Description: "d", GrantClass: GrantClassCommandStart, GrantTarget: "git log --graph"}
	if err := store.WriteRules(ctx, []tool.RuleCandidate{family}); err != nil {
		t.Fatalf("WriteRules(family): %v", err)
	}

	later := mustWorkspaceStore(t, Config{Path: path})
	for _, command := range []string{"git log", "git log --graph", "git log -n 3"} {
		if matched, err := later.MatchesAllow(ctx, commandRequirement(command)); err != nil || !matched {
			t.Fatalf("persisted family did not satisfy %q: %v, %v", command, matched, err)
		}
	}
	for _, command := range []string{"git status", "git catalog", "git log; rm -rf output"} {
		if matched, err := later.MatchesAllow(ctx, commandRequirement(command)); err != nil || matched {
			t.Fatalf("persisted family wrongly satisfied %q: %v, %v", command, matched, err)
		}
	}

	// A separately approved exact segment completes per-segment coverage.
	if err := store.WriteRules(ctx, []tool.RuleCandidate{commandCandidate("rm -rf output")}); err != nil {
		t.Fatalf("WriteRules(exact): %v", err)
	}
	if matched, err := later.MatchesAllow(ctx, commandRequirement("git log; rm -rf output")); err != nil || !matched {
		t.Fatalf("family plus exact segment did not cover the compound command: %v, %v", matched, err)
	}
}

// TestStoreDenyFamilySegments proves a deny family tightens any segment of a
// compound command even when a wildcard allow exists.
func TestStoreDenyFamilySegments(t *testing.T) {
	ctx := context.Background()
	path := testPath(t)
	writeValidFile(t, path, []Rule{
		{Effect: EffectAllow, Capability: CapabilityCommandExecute, Class: ClassCommandInvokeWildcard},
		{Effect: EffectDeny, Capability: CapabilityCommandExecute, Class: ClassCommandInvokeFamily, Tokens: []string{"git", "push"}, TrailingArguments: true},
	})
	store, _, err := NewReadOnlyStore(Config{Path: path})
	if err != nil {
		t.Fatalf("NewReadOnlyStore: %v", err)
	}
	if matched, err := store.MatchesDeny(ctx, commandRequirement("git log && git push origin main")); err != nil || !matched {
		t.Fatalf("deny family missed its segment: %v, %v", matched, err)
	}
	if matched, err := store.MatchesDeny(ctx, commandRequirement("git log")); err != nil || matched {
		t.Fatalf("deny family over-matched: %v, %v", matched, err)
	}
	if matched, err := store.MatchesAllow(ctx, commandRequirement("git log && git push origin main")); err != nil || !matched {
		t.Fatalf("wildcard allow should still answer allow (gate orders deny first): %v, %v", matched, err)
	}
}

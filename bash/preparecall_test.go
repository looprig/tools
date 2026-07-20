package bash

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/permission"
)

func mustUUID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() error = %v", err)
	}
	return id
}

// prepareBash runs PrepareCall and fails the test on an unexpected error.
func prepareBash(t *testing.T, b *BashTool, argsJSON string) (tool.Request, tool.PreparedArtifact) {
	t.Helper()
	req, art, err := b.PrepareCall(context.Background(), mustUUID(t), argsJSON)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	if err := tool.ValidateRequest(req); err != nil {
		t.Fatalf("ValidateRequest() error = %v", err)
	}
	return req, art
}

// TestBashPrepareCallCommandRequirement pins the always-present command-backed
// requirement: kind command.execute, empty scope, Match == GrantTarget ==
// Request.Command == the normalized command, grant class command.start.v1, and
// the populated grant-binding fields (working directory, expiry).
func TestBashPrepareCallCommandRequirement(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	b := NewBash(root)
	before := time.Now().UnixMilli()

	req, art := prepareBash(t, b, `{"command":"git status"}`)

	if req.Command != "git status" {
		t.Errorf("Request.Command = %q, want %q", req.Command, "git status")
	}
	if req.WorkingDirectory != root {
		t.Errorf("Request.WorkingDirectory = %q, want %q", req.WorkingDirectory, root)
	}
	if req.ExpiresAtUnixMilli <= before {
		t.Errorf("Request.ExpiresAtUnixMilli = %d, want a future expiry", req.ExpiresAtUnixMilli)
	}
	if len(req.Requirements) != 1 {
		t.Fatalf("Requirements = %d, want exactly the command requirement", len(req.Requirements))
	}
	r := req.Requirements[0]
	if r.Kind != tool.CapabilityCommandExecute || r.Scope != "" {
		t.Errorf("requirement kind/scope = %q/%q, want command.execute with empty scope", r.Kind, r.Scope)
	}
	if r.Match != "git status" || r.GrantClass != tool.GrantClassCommandStart || r.GrantTarget != "git status" {
		t.Errorf("requirement match/grant = %q %q/%q, want exact normalized command with command.start.v1", r.Match, r.GrantClass, r.GrantTarget)
	}
	if art == nil {
		t.Fatal("artifact = nil, want a typed bash artifact")
	}
}

// TestBashPrepareCallNormalizesCommand proves the normalized command (surrounding
// whitespace trimmed, interior bytes preserved verbatim) is used identically for
// Request.Command, Match, and GrantTarget.
func TestBashPrepareCallNormalizesCommand(t *testing.T) {
	t.Parallel()
	b := NewBash(t.TempDir())
	req, _ := prepareBash(t, b, `{"command":"  git  log --oneline\t"}`)
	const want = "git  log --oneline"
	r := req.Requirements[0]
	if req.Command != want || r.Match != want || r.GrantTarget != want {
		t.Errorf("command/match/target = %q/%q/%q, want all %q (trimmed, interior preserved)", req.Command, r.Match, r.GrantTarget, want)
	}
}

// TestBashPrepareCallCandidates pins the reusable command candidates: a family
// candidate only through the injected eligibility catalog, the exact command
// otherwise, and grant pairs identical to the requirement's.
func TestBashPrepareCallCandidates(t *testing.T) {
	t.Parallel()
	catalog := func(tokens []string) bool {
		return len(tokens) == 2 && tokens[0] == "git" && tokens[1] == "log"
	}
	tests := []struct {
		name      string
		options   []BashOption
		command   string
		wantMatch string
	}{
		{name: "eligible family", options: []BashOption{WithFamilyCatalog(catalog)}, command: "git log --oneline", wantMatch: "Bash(git log:*)"},
		{name: "ineligible exact fallback", options: []BashOption{WithFamilyCatalog(catalog)}, command: "rm -rf build", wantMatch: "rm -rf build"},
		{name: "no catalog exact fallback", command: "git log --oneline", wantMatch: "git log --oneline"},
		{name: "multi segment exact fallback", options: []BashOption{WithFamilyCatalog(catalog)}, command: "git log && rm x", wantMatch: "git log && rm x"},
		// A literal command living in the Bash(...) display-rule namespace
		// gets NO reusable candidate (wantMatch ""): its exact fallback would
		// be re-read by the store as a wildcard or family rule. Once-only
		// approval still works through the requirement itself.
		{name: "rule-syntax wildcard command", options: []BashOption{WithFamilyCatalog(catalog)}, command: "Bash(*)", wantMatch: ""},
		{name: "rule-syntax family command", options: []BashOption{WithFamilyCatalog(catalog)}, command: "Bash(rm:*)", wantMatch: ""},
		{name: "rule-syntax catalog command", options: []BashOption{WithFamilyCatalog(catalog)}, command: "Bash(git log:*)", wantMatch: ""},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := NewBash(t.TempDir(), tt.options...)
			args, err := json.Marshal(map[string]any{"command": tt.command})
			if err != nil {
				t.Fatal(err)
			}
			req, _ := prepareBash(t, b, string(args))
			requirement := req.Requirements[0]
			if requirement.Match != tt.command || requirement.GrantTarget != tt.command {
				t.Errorf("requirement match/target = %q/%q, want the exact command %q", requirement.Match, requirement.GrantTarget, tt.command)
			}
			candidates := requirement.Candidates
			if tt.wantMatch == "" {
				if len(candidates) != 0 {
					t.Fatalf("candidates = %+v, want none for a rule-syntax-colliding command", candidates)
				}
				return
			}
			if len(candidates) != 1 {
				t.Fatalf("candidates = %d, want 1", len(candidates))
			}
			c := candidates[0]
			if c.Match != tt.wantMatch {
				t.Errorf("candidate match = %q, want %q", c.Match, tt.wantMatch)
			}
			if c.Kind != tool.CapabilityCommandExecute || c.GrantClass != tool.GrantClassCommandStart || c.GrantTarget != tt.command {
				t.Errorf("candidate = %+v, want command.execute with the exact-command grant pair", c)
			}
		})
	}
}

// TestBashRuleSyntaxCommandCannotPersistWildcard is the end-to-end repro of
// the display-syntax collision: preparing the literal command `Bash(*)` and
// writing whatever candidates preparation emitted must never leave a durable
// rule that allows an unrelated command.
func TestBashRuleSyntaxCommandCannotPersistWildcard(t *testing.T) {
	t.Parallel()
	catalog := func(tokens []string) bool { return len(tokens) == 2 && tokens[0] == "git" && tokens[1] == "log" }
	for _, command := range []string{"Bash(*)", "Bash(rm:*)", "Bash(git log:*)"} {
		b := NewBash(t.TempDir(), WithFamilyCatalog(catalog))
		args, err := json.Marshal(map[string]any{"command": command})
		if err != nil {
			t.Fatal(err)
		}
		req, _ := prepareBash(t, b, string(args))
		store, _, err := permission.NewWorkspaceStore(permission.Config{Path: filepath.Join(t.TempDir(), "permissions.json")})
		if err != nil {
			t.Fatalf("NewWorkspaceStore() error = %v", err)
		}
		var candidates []tool.RuleCandidate
		for _, requirement := range req.Requirements {
			candidates = append(candidates, requirement.Candidates...)
		}
		if err := store.WriteRules(context.Background(), candidates); err != nil {
			t.Fatalf("WriteRules(%q candidates) error = %v", command, err)
		}
		for _, probe := range []string{"rm -rf /", "rm x", "git log --oneline"} {
			requirement := tool.Requirement{Kind: permission.CapabilityCommandExecute, Match: probe, Description: "d", GrantClass: permission.GrantClassCommandStart, GrantTarget: probe}
			matched, err := store.MatchesAllow(context.Background(), requirement)
			if err != nil {
				t.Fatalf("MatchesAllow(%q): %v", probe, err)
			}
			if matched {
				t.Errorf("preparing literal command %q durably allowed %q", command, probe)
			}
		}
	}
}

// TestBashPrepareCallDeclaredDeltas pins the requirement shape of every declared
// delta type combined with the command requirement into ONE request.
func TestBashPrepareCallDeclaredDeltas(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	b := NewBash(root)
	args := `{"command":"git push","access":{` +
		`"network":[{"transport":"tcp","host":"GitHub.COM.","port":443}],` +
		`"read":[{"scope":"path","path":"../shared/file.txt"},{"scope":"tree","path":"/opt/data"}],` +
		`"write":[{"scope":"host"}]}}`

	req, _ := prepareBash(t, b, args)
	if len(req.Requirements) != 5 {
		t.Fatalf("Requirements = %d, want command + 4 declared deltas in ONE request", len(req.Requirements))
	}
	wantPath := filepath.Clean(filepath.Join(root, "../shared/file.txt"))

	byMatch := map[string]tool.Requirement{}
	for _, r := range req.Requirements {
		byMatch[r.Kind+"\x00"+r.Match] = r
	}

	network, ok := byMatch[permission.CapabilityNetwork+"\x00"+permission.NetworkTargetMatch("tcp", "github.com", 443)]
	if !ok {
		t.Fatalf("missing normalized network target requirement; have %+v", req.Requirements)
	}
	if network.Scope != "" || network.GrantClass != permission.GrantClassNetworkProxyTarget || network.GrantTarget != "tcp:github.com:443" {
		t.Errorf("network requirement = %+v, want empty scope + proxy-target grant tcp:github.com:443", network)
	}
	if len(network.Candidates) != 1 || network.Candidates[0].Match != network.Match {
		t.Errorf("network candidates = %+v, want ONE reusable target-scoped candidate", network.Candidates)
	}

	pathRead, ok := byMatch[permission.CapabilityFilesystemRead+"\x00"+wantPath]
	if !ok {
		t.Fatalf("missing canonical path-read requirement for %q", wantPath)
	}
	if pathRead.Scope != wantPath || pathRead.GrantClass != permission.ClassFilesystemPathRead || pathRead.GrantTarget != wantPath {
		t.Errorf("path read requirement = %+v, want scope/target %q with %s", pathRead, wantPath, permission.ClassFilesystemPathRead)
	}

	treeRead, ok := byMatch[permission.CapabilityFilesystemRead+"\x00"+permission.TreeMatch("/opt/data")]
	if !ok {
		t.Fatal("missing tree-read requirement for /opt/data")
	}
	if treeRead.Scope != "tree:/opt/data" || treeRead.GrantClass != permission.ClassFilesystemTreeRead || treeRead.GrantTarget != "/opt/data" {
		t.Errorf("tree read requirement = %+v, want tree scope/grant for /opt/data", treeRead)
	}

	hostWrite, ok := byMatch[permission.CapabilityFilesystemWrite+"\x00"+permission.HostAccessMatch("git push")]
	if !ok {
		t.Fatal("missing command-bound host-write requirement")
	}
	if hostWrite.Scope != "host:*" || hostWrite.GrantClass != permission.ClassFilesystemHostWrite || hostWrite.GrantTarget != "host:*" {
		t.Errorf("host write requirement = %+v, want host:* scope/target", hostWrite)
	}
}

// TestBashPrepareCallBroadEgress pins the truthfully labeled, exact-command-bound
// broad egress delta a host-less network declaration produces.
func TestBashPrepareCallBroadEgress(t *testing.T) {
	t.Parallel()
	b := NewBash(t.TempDir())
	req, _ := prepareBash(t, b, `{"command":"git push origin main","access":{"network":[{"port":22}]}}`)
	if len(req.Requirements) != 2 {
		t.Fatalf("Requirements = %d, want command + broad egress", len(req.Requirements))
	}
	var broad tool.Requirement
	for _, r := range req.Requirements {
		if r.Kind == permission.CapabilityNetwork {
			broad = r
		}
	}
	wantMatch := permission.BroadEgressMatch("git push origin main", "tcp:*:22")
	if broad.Match != wantMatch || broad.Scope != "" {
		t.Errorf("broad match/scope = %q/%q, want %q with empty scope", broad.Match, broad.Scope, wantMatch)
	}
	if broad.GrantClass != permission.ClassNetworkBroad || broad.GrantTarget != "tcp:*:22" {
		t.Errorf("broad grant = %q/%q, want network.broad.v1 tcp:*:22", broad.GrantClass, broad.GrantTarget)
	}
	if !strings.Contains(broad.Description, "any host") {
		t.Errorf("broad description = %q, must make the broader destination scope explicit", broad.Description)
	}
	if len(broad.Candidates) != 1 || broad.Candidates[0].Match != wantMatch || broad.Candidates[0].GrantTarget != "tcp:*:22" {
		t.Errorf("broad candidates = %+v, want ONE exact-command-bound candidate", broad.Candidates)
	}
}

// TestBashPrepareCallRejects proves malformed input — including a declaration
// without a command — fails during preparation and never reaches evaluation.
func TestBashPrepareCallRejects(t *testing.T) {
	t.Parallel()
	b := NewBash(t.TempDir())
	for name, args := range map[string]string{
		"not json":                  `x`,
		"missing command":           `{}`,
		"whitespace command":        `{"command":"   "}`,
		"declaration without cmd":   `{"access":{"network":[{"host":"example.com","port":443}]}}`,
		"nul in command":            `{"command":"echo \u0000hi"}`,
		"network missing port":      `{"command":"c","access":{"network":[{"host":"example.com"}]}}`,
		"network port out of range": `{"command":"c","access":{"network":[{"host":"example.com","port":70000}]}}`,
		"network non-tcp transport": `{"command":"c","access":{"network":[{"transport":"udp","host":"example.com","port":53}]}}`,
		"network host with space":   `{"command":"c","access":{"network":[{"host":"bad host","port":443}]}}`,
		"unknown filesystem scope":  `{"command":"c","access":{"read":[{"scope":"glob","path":"*"}]}}`,
		"path scope without path":   `{"command":"c","access":{"read":[{"scope":"path"}]}}`,
		"tree scope without path":   `{"command":"c","access":{"write":[{"scope":"tree"}]}}`,
		"host scope with path":      `{"command":"c","access":{"write":[{"scope":"host","path":"/x"}]}}`,
		"escaping workdir":          `{"command":"echo x","workdir":"../.."}`,
		"nul in declared path":      `{"command":"c","access":{"read":[{"scope":"path","path":"/a\u0000b"}]}}`,
	} {
		name, args := name, args
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, _, err := b.PrepareCall(context.Background(), mustUUID(t), args); err == nil {
				t.Errorf("PrepareCall(%s) error = nil, want an error", args)
			}
		})
	}
}

// TestBashCandidatesRoundTripThroughStore proves every candidate Bash emits —
// family command, network target, broad egress, path, tree, and command-bound
// host — persists through the real permission store and that the stored rules
// then MATCH the very requirements the preparation emitted.
func TestBashCandidatesRoundTripThroughStore(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	catalog := func(tokens []string) bool { return len(tokens) == 2 && tokens[0] == "git" && tokens[1] == "push" }
	b := NewBash(root, WithFamilyCatalog(catalog))
	args := `{"command":"git push","access":{` +
		`"network":[{"host":"github.com","port":443},{"port":22}],` +
		`"read":[{"scope":"path","path":"/etc/hosts"},{"scope":"tree","path":"/opt/data"}],` +
		`"write":[{"scope":"host"}]}}`
	req, _ := prepareBash(t, b, args)

	store, _, err := permission.NewWorkspaceStore(permission.Config{Path: filepath.Join(t.TempDir(), "permissions.json")})
	if err != nil {
		t.Fatalf("NewWorkspaceStore() error = %v", err)
	}
	var candidates []tool.RuleCandidate
	for _, requirement := range req.Requirements {
		candidates = append(candidates, requirement.Candidates...)
	}
	if err := store.WriteRules(context.Background(), candidates); err != nil {
		t.Fatalf("WriteRules() error = %v (candidate encodings must persist)", err)
	}
	for _, requirement := range req.Requirements {
		matched, err := store.MatchesAllow(context.Background(), requirement)
		if err != nil {
			t.Fatalf("MatchesAllow(%s %q) error = %v", requirement.Kind, requirement.Match, err)
		}
		if !matched {
			t.Errorf("stored candidate does not match its own requirement %s %q", requirement.Kind, requirement.Match)
		}
	}
}

// TestBashRunConsumesArtifactAndGrants proves execution uses the PREPARED
// command and the PreparedCall grant tokens — the raw args are never reparsed —
// and fails closed without the artifact.
func TestBashRunConsumesArtifactAndGrants(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	fake := &fakeGrantedRunner{out: []byte("ROUTED\n")}
	b := NewBash(root, WithRunner(fake))
	id := mustUUID(t)

	req, art, err := b.PrepareCall(context.Background(), id, `{"command":"echo hi"}`)
	if err != nil {
		t.Fatalf("PrepareCall() error = %v", err)
	}
	ctx := loop.WithPreparedCall(context.Background(), tool.PreparedCall{
		ExecutionID: id, Request: req, Artifact: art, Grants: []string{"tok-1", "tok-2"},
	})
	res, err := b.InvokableRun(ctx, `{"command":"rm -rf /"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	if !strings.Contains(textOf(t, res), "ROUTED") {
		t.Errorf("result = %q, want the runner output", textOf(t, res))
	}
	if !fake.ranGrants || fake.ranPlain {
		t.Fatalf("dispatch = grants:%v plain:%v, want the granted runner path", fake.ranGrants, fake.ranPlain)
	}
	if !slices.Equal(fake.gotGrants, []string{"tok-1", "tok-2"}) {
		t.Errorf("grants handed to runner = %#v, want the PreparedCall tokens", fake.gotGrants)
	}
	if fake.gotCommand != "echo hi" {
		t.Errorf("runner saw command %q, want the PREPARED command, not the mutated raw args", fake.gotCommand)
	}

	// Without a prepared artifact the tool fails closed and never executes.
	fresh := &fakeGrantedRunner{}
	b2 := NewBash(root, WithRunner(fresh))
	res2, err := b2.InvokableRun(context.Background(), `{"command":"echo hi"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	if out := textOf(t, res2); !strings.HasPrefix(out, "error:") {
		t.Fatalf("result = %q, want fail-closed without an artifact", out)
	}
	if fresh.ranPlain || fresh.ranGrants {
		t.Fatal("runner was invoked without a prepared artifact")
	}
}

// TestBashSchemaDeclaresAccess asserts the JSON schema advertises the optional
// structured "access" declaration and no longer carries the removed model-facing
// "grants" token channel (tokens travel only in the PreparedCall).
func TestBashSchemaDeclaresAccess(t *testing.T) {
	t.Parallel()
	info, err := NewBash(t.TempDir()).Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(info.Schema, &schema); err != nil {
		t.Fatalf("Schema is not the expected JSON object: %v", err)
	}
	if _, ok := schema.Properties["access"]; !ok {
		t.Error("schema is missing the optional 'access' declaration property")
	}
	if _, ok := schema.Properties["grants"]; ok {
		t.Error("schema still advertises the removed 'grants' token channel")
	}
	if slices.Contains(schema.Required, "access") {
		t.Error("'access' must be optional (not in required)")
	}
}

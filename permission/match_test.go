package permission

import (
	"testing"

	"github.com/looprig/harness/pkg/tool"
)

// commandRequirement builds the requirement Bash preparation emits for one
// exact normalized command.
func commandRequirement(command string) tool.Requirement {
	return tool.Requirement{
		Kind:        CapabilityCommandExecute,
		Match:       command,
		Description: "execute " + command,
		GrantClass:  GrantClassCommandStart,
		GrantTarget: command,
	}
}

// TestMatchExactCommand proves command.invoke.v1 matches only its exact
// normalized command.
func TestMatchExactCommand(t *testing.T) {
	rule := Rule{Effect: EffectAllow, Capability: CapabilityCommandExecute, Class: ClassCommandInvoke, Command: "git status"}
	if !matchesRequirement(rule, commandRequirement("git status")) {
		t.Fatal("exact command did not match itself")
	}
	for _, other := range []string{"git status --short", "git", "git  status", "rm -rf /"} {
		if matchesRequirement(rule, commandRequirement(other)) {
			t.Fatalf("exact rule matched %q", other)
		}
	}
}

// TestMatchWildcardCommandOnly proves the Bash(*) representation satisfies
// any command.execute requirement and absolutely nothing else.
func TestMatchWildcardCommandOnly(t *testing.T) {
	rule := Rule{Effect: EffectAllow, Capability: CapabilityCommandExecute, Class: ClassCommandInvokeWildcard}
	if !matchesRequirement(rule, commandRequirement("anything at all")) {
		t.Fatal("wildcard did not match a command requirement")
	}
	network := tool.Requirement{Kind: CapabilityNetwork, Match: NetworkTargetMatch("tcp", "github.com", 443), Description: "d"}
	read := tool.Requirement{Kind: CapabilityFilesystemRead, Match: "/etc/hosts", Description: "d"}
	if matchesRequirement(rule, network) || matchesRequirement(rule, read) {
		t.Fatal("wildcard command rule implied a non-command capability")
	}
}

// TestMatchFamilyTokenSegments proves the token-aware family matcher: token
// equality on every shell segment, no string prefixes, no boundary crossing,
// and never another capability.
func TestMatchFamilyTokenSegments(t *testing.T) {
	rule := Rule{Effect: EffectAllow, Capability: CapabilityCommandExecute, Class: ClassCommandInvokeFamily, Tokens: []string{"git", "log"}, TrailingArguments: true}
	if !matchesRequirement(rule, commandRequirement("git log -n 3")) {
		t.Fatal("family rule did not match its own command with trailing arguments")
	}
	for _, other := range []string{"git status", "git catalog", "git", "git log; rm -rf output", "git log && rm x"} {
		if matchesRequirement(rule, commandRequirement(other)) {
			t.Fatalf("family rule matched %q", other)
		}
	}
	network := tool.Requirement{Kind: CapabilityNetwork, Match: NetworkTargetMatch("tcp", "github.com", 443), Description: "d"}
	if matchesRequirement(rule, network) {
		t.Fatal("family command rule implied a network capability")
	}
}

// TestMatchNetworkTarget proves target rules require every present
// constraint and reject different enforcement classes.
func TestMatchNetworkTarget(t *testing.T) {
	rule := Rule{Effect: EffectAllow, Capability: CapabilityNetwork, Class: ClassNetworkTarget, Transport: "tcp", Host: "github.com", Port: 443}
	fetch := tool.Requirement{Kind: CapabilityNetwork, Match: NetworkTargetMatch("tcp", "github.com", 443), Description: "d"}
	bash := tool.Requirement{Kind: CapabilityNetwork, Match: NetworkTargetMatch("tcp", "github.com", 443), Description: "d", GrantClass: GrantClassNetworkProxyTarget, GrantTarget: "tcp://github.com:443"}
	if !matchesRequirement(rule, fetch) {
		t.Fatal("target rule did not match the identical Fetch target")
	}
	if !matchesRequirement(rule, bash) {
		t.Fatal("target rule did not match the identical proxy-enforced Bash target")
	}
	for name, req := range map[string]tool.Requirement{
		"different host":  {Kind: CapabilityNetwork, Match: NetworkTargetMatch("tcp", "evil.example", 443), Description: "d"},
		"different port":  {Kind: CapabilityNetwork, Match: NetworkTargetMatch("tcp", "github.com", 22), Description: "d"},
		"different proto": {Kind: CapabilityNetwork, Match: NetworkTargetMatch("udp", "github.com", 443), Description: "d"},
		"broad class": {Kind: CapabilityNetwork, Match: BroadEgressMatch("git push", "tcp:*:22"), Description: "d",
			GrantClass: ClassNetworkBroad, GrantTarget: "tcp:*:22"},
		"command kind": commandRequirement("curl https://github.com"),
	} {
		if matchesRequirement(rule, req) {
			t.Fatalf("%s: target rule matched %#v", name, req)
		}
	}

	// A host-only rule constrains only the host.
	hostOnly := Rule{Effect: EffectDeny, Capability: CapabilityNetwork, Class: ClassNetworkTarget, Host: "github.com"}
	if !matchesRequirement(hostOnly, fetch) {
		t.Fatal("host-only rule did not match its host")
	}
	if !matchesRequirement(hostOnly, tool.Requirement{Kind: CapabilityNetwork, Match: NetworkTargetMatch("udp", "github.com", 53), Description: "d"}) {
		t.Fatal("host-only rule did not broaden over transport and port")
	}
}

// TestMatchBroadEgressCommandBound proves a broad egress rule matches only
// the same broad class, backend target, and exact bound command.
func TestMatchBroadEgressCommandBound(t *testing.T) {
	rule := Rule{Effect: EffectAllow, Capability: CapabilityNetwork, Class: ClassNetworkBroad, Command: "git push origin main", Target: "tcp:*:22"}
	same := tool.Requirement{Kind: CapabilityNetwork, Match: BroadEgressMatch("git push origin main", "tcp:*:22"), Description: "d", GrantClass: ClassNetworkBroad, GrantTarget: "tcp:*:22"}
	if !matchesRequirement(rule, same) {
		t.Fatal("broad rule did not match its exact bound command")
	}
	otherCommand := tool.Requirement{Kind: CapabilityNetwork, Match: BroadEgressMatch("git push origin other", "tcp:*:22"), Description: "d", GrantClass: ClassNetworkBroad, GrantTarget: "tcp:*:22"}
	if matchesRequirement(rule, otherCommand) {
		t.Fatal("broad rule matched a different command")
	}
	fetch := tool.Requirement{Kind: CapabilityNetwork, Match: NetworkTargetMatch("tcp", "github.com", 22), Description: "d"}
	if matchesRequirement(rule, fetch) {
		t.Fatal("command-bound broad egress satisfied a target-scoped request")
	}
}

// TestMatchFilesystem proves path, tree, and command-bound host matching.
func TestMatchFilesystem(t *testing.T) {
	path := Rule{Effect: EffectDeny, Capability: CapabilityFilesystemRead, Class: ClassFilesystemPathRead, Path: "/w/.env"}
	if !matchesRequirement(path, tool.Requirement{Kind: CapabilityFilesystemRead, Match: "/w/.env", Description: "d"}) {
		t.Fatal("path rule did not match its exact path")
	}
	if matchesRequirement(path, tool.Requirement{Kind: CapabilityFilesystemRead, Match: "/w/.env.example", Description: "d"}) {
		t.Fatal("path rule matched a sibling path")
	}
	if matchesRequirement(path, tool.Requirement{Kind: CapabilityFilesystemWrite, Match: "/w/.env", Description: "d"}) {
		t.Fatal("read rule matched a write requirement")
	}

	tree := Rule{Effect: EffectAllow, Capability: CapabilityFilesystemWrite, Class: ClassFilesystemTreeWrite, Root: "/w/out"}
	for _, match := range []string{"/w/out", "/w/out/a/b.txt", TreeMatch("/w/out")} {
		if !matchesRequirement(tree, tool.Requirement{Kind: CapabilityFilesystemWrite, Match: match, Description: "d"}) {
			t.Fatalf("tree rule did not cover %q", match)
		}
	}
	for _, match := range []string{"/w/output", "/w", TreeMatch("/w")} {
		if matchesRequirement(tree, tool.Requirement{Kind: CapabilityFilesystemWrite, Match: match, Description: "d"}) {
			t.Fatalf("tree rule escaped to %q", match)
		}
	}

	host := Rule{Effect: EffectAllow, Capability: CapabilityFilesystemRead, Class: ClassFilesystemHostRead, Command: "cat /etc/hosts"}
	bound := tool.Requirement{Kind: CapabilityFilesystemRead, Match: HostAccessMatch("cat /etc/hosts"), Description: "d", GrantClass: ClassFilesystemHostRead, GrantTarget: "host"}
	if !matchesRequirement(host, bound) {
		t.Fatal("host rule did not match its bound command")
	}
	direct := tool.Requirement{Kind: CapabilityFilesystemRead, Match: "/etc/hosts", Description: "d"}
	if matchesRequirement(host, direct) {
		t.Fatal("command-bound host rule satisfied a direct file-tool path request")
	}
}

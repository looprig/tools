package permission

import (
	"reflect"
	"strings"
	"testing"
)

// sampleRules covers every schema-v2 enforcement class once.
func sampleRules() []Rule {
	return []Rule{
		{Effect: EffectAllow, Capability: CapabilityCommandExecute, Class: ClassCommandInvoke, Command: "git status"},
		{Effect: EffectAllow, Capability: CapabilityCommandExecute, Class: ClassCommandInvokeWildcard},
		{Effect: EffectAllow, Capability: CapabilityCommandExecute, Class: ClassCommandInvokeFamily, Tokens: []string{"git", "log"}, TrailingArguments: true},
		{Effect: EffectAllow, Capability: CapabilityNetwork, Class: ClassNetworkTarget, Transport: "tcp", Host: "github.com", Port: 443},
		{Effect: EffectAllow, Capability: CapabilityNetwork, Class: ClassNetworkBroad, Command: "git push origin main", Target: "tcp:*:22"},
		{Effect: EffectDeny, Capability: CapabilityFilesystemRead, Class: ClassFilesystemPathRead, Path: "/home/u/secret.pem"},
		{Effect: EffectAllow, Capability: CapabilityFilesystemWrite, Class: ClassFilesystemTreeWrite, Root: "/home/u/project"},
		{Effect: EffectAllow, Capability: CapabilityFilesystemRead, Class: ClassFilesystemHostRead, Command: "cat /etc/hosts"},
	}
}

// TestFileRoundTrip proves every enforcement class encodes to the v2 schema
// and decodes back to an identical rule set.
func TestFileRoundTrip(t *testing.T) {
	rules := sampleRules()
	encoded, err := encodeFile(rules)
	if err != nil {
		t.Fatalf("encodeFile: %v", err)
	}
	for _, want := range []string{`"version": 2`, `"normalization_version": 1`, `"enforcement_class"`} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("encoded file missing %q:\n%s", want, encoded)
		}
	}
	decoded, err := decodeFile(encoded)
	if err != nil {
		t.Fatalf("decodeFile: %v", err)
	}
	if !reflect.DeepEqual(rules, decoded) {
		t.Fatalf("round trip mismatch:\n got %#v\nwant %#v", decoded, rules)
	}
}

// TestDecodeSpecExample proves the exact conceptual document from the
// specification decodes.
func TestDecodeSpecExample(t *testing.T) {
	const doc = `{
	  "version": 2,
	  "normalization_version": 1,
	  "rules": [
	    {
	      "effect": "allow",
	      "capability": "command.execute",
	      "enforcement_class": "command.invoke.shell-segment-glob.v1",
	      "match": {"tokens": ["git", "log"], "trailing_arguments": true}
	    },
	    {
	      "effect": "allow",
	      "capability": "network",
	      "enforcement_class": "network.target.v1",
	      "match": {"transport": "tcp", "host": "github.com", "port": 443}
	    }
	  ]
	}`
	rules, err := decodeFile([]byte(doc))
	if err != nil {
		t.Fatalf("decodeFile: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("got %d rules, want 2", len(rules))
	}
	if rules[0].Class != ClassCommandInvokeFamily || !rules[0].TrailingArguments {
		t.Fatalf("family rule decoded wrong: %#v", rules[0])
	}
	if rules[1].Host != "github.com" || rules[1].Port != 443 || rules[1].Transport != "tcp" {
		t.Fatalf("network rule decoded wrong: %#v", rules[1])
	}
}

// TestDecodeRejects proves the strict decoder rejects unsupported versions,
// unknown fields, invalid pairings, and capability-delta smuggling on
// wildcard/family records.
func TestDecodeRejects(t *testing.T) {
	valid := func(rule string) string {
		return `{"version": 2, "normalization_version": 1, "rules": [` + rule + `]}`
	}
	cases := map[string]string{
		"schema version 1": `{"version": 1, "normalization_version": 1, "rules": []}`,
		"schema version 3": `{"version": 3, "normalization_version": 1, "rules": []}`,
		"normalization 2":  `{"version": 2, "normalization_version": 2, "rules": []}`,
		"unknown top field": `{"version": 2, "normalization_version": 1, "rules": [],
			"extra": true}`,
		"not json":       `push the button`,
		"trailing bytes": `{"version": 2, "normalization_version": 1, "rules": []}{}`,
		"unknown effect": valid(`{"effect":"audit","capability":"network","enforcement_class":"network.target.v1","match":{"host":"a.example"}}`),
		"unknown capability": valid(`{"effect":"allow","capability":"time.travel",` +
			`"enforcement_class":"network.target.v1","match":{"host":"a.example"}}`),
		"unknown class":             valid(`{"effect":"allow","capability":"network","enforcement_class":"network.target.v2","match":{"host":"a.example"}}`),
		"class capability mismatch": valid(`{"effect":"allow","capability":"network","enforcement_class":"command.invoke.v1","match":{"command":"git status"}}`),
		"wildcard with host delta":  valid(`{"effect":"allow","capability":"command.execute","enforcement_class":"command.invoke.wildcard.v1","match":{"host":"github.com"}}`),
		"family with network delta": valid(`{"effect":"allow","capability":"command.execute","enforcement_class":"command.invoke.shell-segment-glob.v1","match":{"tokens":["git","log"],"trailing_arguments":true,"host":"github.com"}}`),
		"family without trailing":   valid(`{"effect":"allow","capability":"command.execute","enforcement_class":"command.invoke.shell-segment-glob.v1","match":{"tokens":["git","log"],"trailing_arguments":false}}`),
		"family with glob token":    valid(`{"effect":"allow","capability":"command.execute","enforcement_class":"command.invoke.shell-segment-glob.v1","match":{"tokens":["git","*"],"trailing_arguments":true}}`),
		"family with no tokens":     valid(`{"effect":"allow","capability":"command.execute","enforcement_class":"command.invoke.shell-segment-glob.v1","match":{"tokens":[],"trailing_arguments":true}}`),
		"port out of range":         valid(`{"effect":"allow","capability":"network","enforcement_class":"network.target.v1","match":{"host":"a.example","port":70000}}`),
		"network without host":      valid(`{"effect":"allow","capability":"network","enforcement_class":"network.target.v1","match":{"port":443}}`),
		"relative path":             valid(`{"effect":"deny","capability":"filesystem.read","enforcement_class":"filesystem.path.read.v1","match":{"path":"secret.pem"}}`),
		"traversal path":            valid(`{"effect":"allow","capability":"filesystem.write","enforcement_class":"filesystem.tree.write.v1","match":{"root":"/a/../b"}}`),
		"missing match":             valid(`{"effect":"allow","capability":"network","enforcement_class":"network.target.v1"}`),
	}
	for name, doc := range cases {
		if _, err := decodeFile([]byte(doc)); err == nil {
			t.Errorf("%s: decode succeeded, want error", name)
		}
	}
}

// TestEncodeRejectsInvalidRule proves the writer refuses to serialize a rule
// the loader would reject.
func TestEncodeRejectsInvalidRule(t *testing.T) {
	bad := []Rule{{Effect: EffectAllow, Capability: CapabilityCommandExecute, Class: ClassCommandInvokeFamily, Tokens: []string{"git", "log;rm"}, TrailingArguments: true}}
	if _, err := encodeFile(bad); err == nil {
		t.Fatal("encodeFile accepted an unsafe family token")
	}
	smuggled := []Rule{{Effect: EffectAllow, Capability: CapabilityCommandExecute, Class: ClassCommandInvokeWildcard, Host: "github.com"}}
	if _, err := encodeFile(smuggled); err == nil {
		t.Fatal("encodeFile accepted a wildcard rule carrying a network delta")
	}
}

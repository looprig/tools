package permission

import (
	"reflect"
	"strings"
	"testing"
)

// familyRule builds one allow token-prefix family rule.
func familyRule(tokens ...string) Rule {
	return Rule{Effect: EffectAllow, Capability: CapabilityCommandExecute, Class: ClassCommandInvokeFamily, Tokens: tokens, TrailingArguments: true}
}

// exactRule builds one allow exact-command rule.
func exactRule(command string) Rule {
	return Rule{Effect: EffectAllow, Capability: CapabilityCommandExecute, Class: ClassCommandInvoke, Command: command}
}

// seg describes one expected segment compactly for the segmentation table.
type seg struct {
	raw       string
	supported bool
	tokens    []string // token texts; nil when irrelevant/unsupported
}

// TestSplitShellCommand pins the segmentation grammar: every separator,
// subshell splicing, quote protection, and the unsupported classifications.
func TestSplitShellCommand(t *testing.T) {
	cases := map[string]struct {
		command string
		ok      bool
		want    []seg
	}{
		"simple":            {"git log", true, []seg{{"git log", true, []string{"git", "log"}}}},
		"tab whitespace":    {"git\tlog", true, []seg{{"git\tlog", true, []string{"git", "log"}}}},
		"and-and":           {"git log && git status", true, []seg{{"git log", true, nil}, {"git status", true, nil}}},
		"or-or":             {"a || b", true, []seg{{"a", true, nil}, {"b", true, nil}}},
		"semicolon":         {"a; b", true, []seg{{"a", true, nil}, {"b", true, nil}}},
		"pipe":              {"a | b", true, []seg{{"a", true, nil}, {"b", true, nil}}},
		"pipe-amp":          {"a |& b", true, []seg{{"a", true, nil}, {"b", true, nil}}},
		"background":        {"a & b", true, []seg{{"a", true, nil}, {"b", true, nil}}},
		"newline":           {"a\nb", true, []seg{{"a", true, nil}, {"b", true, nil}}},
		"trailing amp":      {"a &", true, []seg{{"a", true, nil}}},
		"trailing semi":     {"a;", true, []seg{{"a", true, nil}}},
		"empty":             {"", true, nil},
		"subshell spliced":  {"(git log; git status) && true", true, []seg{{"git log", true, nil}, {"git status", true, nil}, {"true", true, nil}}},
		"nested subshell":   {"((a))", true, []seg{{"a", true, nil}}},
		"subshell in pipe":  {"a | (b; c)", true, []seg{{"a", true, nil}, {"b", true, nil}, {"c", true, nil}}},
		"subshell trailer":  {"(a) b", true, []seg{{"(a) b", false, nil}}},
		"empty middle":      {"a; ; b", true, []seg{{"a", true, nil}, {"", false, nil}, {"b", true, nil}}},
		"dangling and-and":  {"a &&", true, []seg{{"a", true, nil}, {"", false, nil}}},
		"command subst":     {"a $(b)", true, []seg{{"a $(b)", false, nil}}},
		"bare subst":        {"$(a)", true, []seg{{"$(a)", false, nil}}},
		"backtick":          {"a `b`", true, []seg{{"a `b`", false, nil}}},
		"redirect out":      {"a > f", true, []seg{{"a > f", false, nil}}},
		"redirect in":       {"a < f", true, []seg{{"a < f", false, nil}}},
		"redirect append":   {"a >> f", true, []seg{{"a >> f", false, nil}}},
		"single quote arg":  {"git log 'a b'", true, []seg{{"git log 'a b'", true, []string{"git", "log", "a b"}}}},
		"double quote arg":  {`git log "x"`, true, []seg{{`git log "x"`, true, []string{"git", "log", "x"}}}},
		"dollar in dquote":  {`git log "$HOME"`, true, []seg{{`git log "$HOME"`, false, nil}}},
		"bare dollar":       {"git log $HOME", true, []seg{{"git log $HOME", false, nil}}},
		"dollar in squote":  {"echo '$HOME'", true, []seg{{"echo '$HOME'", true, []string{"echo", "$HOME"}}}},
		"assignment prefix": {"FOO=1 git log", true, []seg{{"FOO=1 git log", false, nil}}},
		"tilde command":     {"~/bin/x", true, []seg{{"~/bin/x", false, nil}}},
		"glob command":      {"* x", true, []seg{{"* x", false, nil}}},
		"comment":           {"#x", true, []seg{{"#x", false, nil}}},
		"quoted command":    {"'git' log", true, []seg{{"'git' log", false, nil}}},
		"quoted separator":  {"a ';' b", true, []seg{{"a ';' b", true, []string{"a", ";", "b"}}}},

		"unterminated squote":   {"a 'b", false, nil},
		"unterminated dquote":   {`a "b`, false, nil},
		"unterminated backtick": {"a `b", false, nil},
		"unbalanced open":       {"(a", false, nil},
		"unbalanced close":      {"a)", false, nil},
		"backslash":             {`a \; b`, false, nil},
		"control byte":          {"a\x01b", false, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			segments, ok := splitShellCommand(tc.command)
			if ok != tc.ok {
				t.Fatalf("split(%q) ok=%v, want %v (segments %#v)", tc.command, ok, tc.ok, segments)
			}
			if !ok {
				return
			}
			if len(segments) != len(tc.want) {
				t.Fatalf("split(%q) = %#v, want %d segments", tc.command, segments, len(tc.want))
			}
			for i, want := range tc.want {
				got := segments[i]
				if got.Raw != want.raw || got.Supported != want.supported {
					t.Fatalf("split(%q) segment %d = {%q supported=%v}, want {%q supported=%v}", tc.command, i, got.Raw, got.Supported, want.raw, want.supported)
				}
				if want.tokens == nil {
					continue
				}
				texts := make([]string, len(got.Tokens))
				for j, token := range got.Tokens {
					texts[j] = token.Text
				}
				if !reflect.DeepEqual(texts, want.tokens) {
					t.Fatalf("split(%q) segment %d tokens = %#v, want %#v", tc.command, i, texts, want.tokens)
				}
			}
		})
	}
}

// TestFamilyMatchesCommand pins the spec's own family examples and the
// injection boundaries: token equality per segment, never string prefixes.
func TestFamilyMatchesCommand(t *testing.T) {
	gitLog := familyRule("git", "log")
	matches := []string{
		"git log",
		"git log --oneline",
		"git log -n 5",
		"git  log",              // extra whitespace between identical tokens
		"git log; git log -p",   // every segment matches the family
		"(git log)",             // subshell of a matching segment
		"git log 'release/x y'", // quoted trailing argument
	}
	for _, command := range matches {
		if !familyMatchesCommand(gitLog, command) {
			t.Errorf("Bash(git log:*) did not match %q", command)
		}
	}
	rejects := []string{
		"",
		"git",
		"git status",
		"git catalog",
		"gitx log",
		"git logx",
		"git log; rm -rf output",
		"git log && rm x",
		"git log | head",
		"git log & rm x",
		"git log\nrm x",
		"git log > out",
		"git log $(x)",
		"git log `x`",
		"git log $HOME",
		`"git" log`,
		"'git' log",
		"git 'log'",
		"git \"log\" --stat",
		"FOO=1 git log",
		"git log; git status",
	}
	for _, command := range rejects {
		if familyMatchesCommand(gitLog, command) {
			t.Errorf("Bash(git log:*) wrongly matched %q", command)
		}
	}
}

// TestCommandCoverage pins per-segment allow coverage across the rule set and
// any-segment deny tightening.
func TestCommandCoverage(t *testing.T) {
	wildcard := Rule{Effect: EffectAllow, Capability: CapabilityCommandExecute, Class: ClassCommandInvokeWildcard}
	gitLog := familyRule("git", "log")

	t.Run("allow", func(t *testing.T) {
		cases := map[string]struct {
			rules   []Rule
			command string
			want    bool
		}{
			"family alone rejects injection":   {[]Rule{gitLog}, "git log; rm -rf output", false},
			"family plus exact covers both":    {[]Rule{gitLog, exactRule("rm -rf output")}, "git log; rm -rf output", true},
			"exact whole compound":             {[]Rule{exactRule("git log; rm -rf output")}, "git log; rm -rf output", true},
			"wildcard covers anything":         {[]Rule{wildcard}, "git log; rm -rf output", true},
			"wildcard covers unparseable":      {[]Rule{wildcard}, "a 'b", true},
			"family cannot cover unparseable":  {[]Rule{gitLog}, "git log 'b", false},
			"family covers repeated segments":  {[]Rule{gitLog}, "git log | git log -p", true},
			"exact segment inside compound":    {[]Rule{exactRule("head -n 1"), gitLog}, "git log | head -n 1", true},
			"uncovered second segment":         {[]Rule{gitLog, exactRule("head")}, "git log | head -n 1", false},
			"no command rules":                 {nil, "git log", false},
			"deny-effect rules never allow":    {[]Rule{{Effect: EffectDeny, Capability: CapabilityCommandExecute, Class: ClassCommandInvokeWildcard}}, "git log", false},
			"foreign capability never covers":  {[]Rule{{Effect: EffectAllow, Capability: CapabilityNetwork, Class: ClassNetworkTarget, Host: "github.com"}}, "git log", false},
			"empty command matches no family":  {[]Rule{gitLog, wildcard}, "", true},
			"empty command needs the wildcard": {[]Rule{gitLog}, "", false},
		}
		for name, tc := range cases {
			if got := commandAllowCovered(tc.rules, tc.command); got != tc.want {
				t.Errorf("%s: commandAllowCovered(%q) = %v, want %v", name, tc.command, got, tc.want)
			}
		}
	})

	t.Run("deny", func(t *testing.T) {
		denyPush := familyRule("git", "push")
		denyPush.Effect = EffectDeny
		denyExact := exactRule("rm -rf output")
		denyExact.Effect = EffectDeny
		cases := map[string]struct {
			rules   []Rule
			command string
			want    bool
		}{
			"family denies its segment":      {[]Rule{denyPush}, "git log && git push origin main", true},
			"family deny misses other":       {[]Rule{denyPush}, "git log", false},
			"exact deny hits inner segment":  {[]Rule{denyExact}, "git log; rm -rf output", true},
			"exact deny whole command":       {[]Rule{denyExact}, "rm -rf output", true},
			"exact deny whole unparseable":   {[]Rule{{Effect: EffectDeny, Capability: CapabilityCommandExecute, Class: ClassCommandInvoke, Command: "a 'b"}}, "a 'b", true},
			"allow-effect rules never deny":  {[]Rule{familyRule("git", "push")}, "git push", false},
			"deny misses unparseable other":  {[]Rule{denyPush}, "git push 'b", false},
			"subshell cannot hide a segment": {[]Rule{denyPush}, "(git push) & true", true},
		}
		for name, tc := range cases {
			if got := commandDenyMatched(tc.rules, tc.command); got != tc.want {
				t.Errorf("%s: commandDenyMatched(%q) = %v, want %v", name, tc.command, got, tc.want)
			}
		}
	})
}

// catalogOf builds a FamilyEligibility from joined token-prefix entries.
func catalogOf(entries ...string) FamilyEligibility {
	set := make(map[string]bool, len(entries))
	for _, entry := range entries {
		set[entry] = true
	}
	return func(tokens []string) bool { return set[strings.Join(tokens, " ")] }
}

// TestProposeCommandCandidate pins the positive-catalog proposal policy:
// catalog hit proposes a family, everything else falls back to the exact
// normalized command, and bare Bash(*) is never proposed.
func TestProposeCommandCandidate(t *testing.T) {
	catalog := catalogOf("git log", "git status", "git diff", "git show", "git push")
	cases := map[string]struct {
		command  string
		eligible FamilyEligibility
		want     string
	}{
		"catalog hit":               {"git log --oneline", catalog, "Bash(git log:*)"},
		"catalog hit bare":          {"git log", catalog, "Bash(git log:*)"},
		"catalog hit push":          {"git push origin main", catalog, "Bash(git push:*)"},
		"unknown subcommand":        {"git rebase -i main", catalog, "git rebase -i main"},
		"unknown command":           {"weird-tool run", catalog, "weird-tool run"},
		"shell":                     {"bash -c 'echo hi'", catalog, "bash -c 'echo hi'"},
		"interpreter":               {"python3 x.py", catalog, "python3 x.py"},
		"find":                      {"find . -delete", catalog, "find . -delete"},
		"xargs":                     {"xargs rm", catalog, "xargs rm"},
		"env wrapper":               {"env git log", catalog, "env git log"},
		"task runner":               {"npm run build", catalog, "npm run build"},
		"multi segment":             {"git status && git log", catalog, "git status && git log"},
		"pipe":                      {"git log | head", catalog, "git log | head"},
		"command substitution":      {"git log $(x)", catalog, "git log $(x)"},
		"redirect":                  {"git log > out", catalog, "git log > out"},
		"quoted command position":   {"'git' log", catalog, "'git' log"},
		"unparseable":               {"git log 'b", catalog, "git log 'b"},
		"nil catalog":               {"git log", nil, "git log"},
		"glob command":              {"*", catalog, "*"},
		"longest prefix wins":       {"git log -n 1", catalogOf("git", "git log"), "Bash(git log:*)"},
		"shorter prefix still hits": {"git fetch --all", catalogOf("git"), "Bash(git:*)"},
	}
	for name, tc := range cases {
		if got := ProposeCommandCandidate(tc.command, tc.eligible); got != tc.want {
			t.Errorf("%s: ProposeCommandCandidate(%q) = %q, want %q", name, tc.command, got, tc.want)
		}
	}
}

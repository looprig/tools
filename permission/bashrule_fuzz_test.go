package permission

import (
	"reflect"
	"strings"
	"testing"
)

// FuzzBashRule fuzzes the shell segmenter/tokenizer and the family matcher:
// no panics, deterministic and stable classification, family matches only
// when its tokens are exactly the leading bare tokens of every supported
// segment, and candidate proposal round-trips into a matching family.
func FuzzBashRule(f *testing.F) {
	seeds := []struct{ command, family string }{
		{"git log", "git log"},
		{"git log --oneline", "git log"},
		{"git log; rm -rf output", "git log"},
		{"(a && b) | c", "a"},
		{"a 'b", "git log"},
		{"git log $(x)", "git log"},
		{"a\nb & c |& d", "b"},
		{"FOO=1 git", "git"},
		{"git\tlog 'a b' \"c\"", "git log"},
		{"`x`; ~/bin/x; * ; #x", "x"},
		{"a && ", "a"},
		{"rm", "rm"},
	}
	for _, seed := range seeds {
		f.Add(seed.command, seed.family)
	}

	f.Fuzz(func(t *testing.T, command, family string) {
		tokens := strings.Fields(family)
		if validateFamilyTokens(tokens) != nil {
			tokens = []string{"git", "log"}
		}
		rule := Rule{Effect: EffectAllow, Capability: CapabilityCommandExecute, Class: ClassCommandInvokeFamily, Tokens: tokens, TrailingArguments: true}

		segments, ok := splitShellCommand(command)
		again, okAgain := splitShellCommand(command)
		if ok != okAgain || !reflect.DeepEqual(segments, again) {
			t.Fatalf("segmentation is not deterministic for %q", command)
		}

		if ok {
			for _, segment := range segments {
				if !segment.Supported {
					continue
				}
				if len(segment.Tokens) == 0 {
					t.Fatalf("supported segment %q has no tokens", segment.Raw)
				}
				for _, token := range segment.Tokens {
					// A '#' is only a comment at word start; mid-token it is
					// literal, so it is excluded from the byte invariant.
					if token.Bare && strings.ContainsAny(token.Text, " \t\n'\"\\$`<>()|&;") {
						t.Fatalf("bare token %q in segment %q contains shell-active bytes", token.Text, segment.Raw)
					}
					if token.Bare && strings.HasPrefix(token.Text, "#") {
						t.Fatalf("bare token %q in segment %q starts a comment", token.Text, segment.Raw)
					}
				}
				// Stability: a supported segment re-parses to itself.
				reparsed, reOK := splitShellCommand(segment.Raw)
				if !reOK || len(reparsed) != 1 || !reparsed[0].Supported || !reflect.DeepEqual(reparsed[0].Tokens, segment.Tokens) {
					t.Fatalf("supported segment %q is not reparse-stable: %#v", segment.Raw, reparsed)
				}
			}
		}

		matched := familyMatchesCommand(rule, command)
		if matched {
			// Independent recheck: every segment must be supported and lead
			// with exactly the family tokens as bare literals.
			if !ok || len(segments) == 0 {
				t.Fatalf("family matched unsegmentable command %q", command)
			}
			for _, segment := range segments {
				if !segment.Supported || len(segment.Tokens) < len(tokens) {
					t.Fatalf("family %v matched command %q with non-conforming segment %q", tokens, command, segment.Raw)
				}
				for i, want := range tokens {
					if !segment.Tokens[i].Bare || segment.Tokens[i].Text != want {
						t.Fatalf("family %v matched segment %q whose token %d is %#v", tokens, segment.Raw, i, segment.Tokens[i])
					}
				}
			}
		}
		// Single-family coverage equals single-family matching.
		if covered := commandAllowCovered([]Rule{rule}, command); covered != matched {
			t.Fatalf("coverage/matching divergence for %q: covered=%v matched=%v", command, covered, matched)
		}
		// Appending a new segment must defeat the family unless it also leads
		// with the family tokens.
		if matched && tokens[0] != "rm" {
			if familyMatchesCommand(rule, command+"; rm -rf /x") {
				t.Fatalf("family %v crossed a segment boundary on %q", tokens, command)
			}
		}

		eligible := func(candidate []string) bool { return reflect.DeepEqual(candidate, tokens) }
		proposed := ProposeCommandCandidate(command, eligible)
		if proposed == "Bash(*)" {
			t.Fatalf("bare wildcard proposed for %q", command)
		}
		if proposed != command {
			proposedRule, err := commandCandidateRule(0, Rule{Effect: EffectAllow, Capability: CapabilityCommandExecute}, proposed)
			if err != nil {
				t.Fatalf("proposal %q for %q does not parse: %v", proposed, command, err)
			}
			if proposedRule.Class != ClassCommandInvokeFamily {
				t.Fatalf("proposal %q for %q is not a family", proposed, command)
			}
			if !familyMatchesCommand(proposedRule, command) {
				t.Fatalf("proposal %q does not match its own command %q", proposed, command)
			}
		}
		if commandDenyMatched(nil, command) {
			t.Fatalf("empty rule set denied %q", command)
		}
	})
}

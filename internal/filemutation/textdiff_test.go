package filemutation

import (
	"strings"
	"testing"
)

func TestLCSOpsInterleavedChange(t *testing.T) {
	before := []string{"a", "b", "c"}
	after := []string{"a", "x", "c"}
	ops := lcsOps(before, after)
	want := []diffOp{
		{Kind: opEqual, Line: "a"},
		{Kind: opDelete, Line: "b"},
		{Kind: opInsert, Line: "x"},
		{Kind: opEqual, Line: "c"},
	}
	if len(ops) != len(want) {
		t.Fatalf("got %d ops, want %d: %+v", len(ops), len(want), ops)
	}
	for i := range want {
		if ops[i] != want[i] {
			t.Errorf("op %d = %+v, want %+v", i, ops[i], want[i])
		}
	}
}

func TestCoarseOpsCoarsensWholeInput(t *testing.T) {
	got := coarseOps([]string{"shared", "old"}, []string{"shared", "new"})
	want := []diffOp{
		{Kind: opDelete, Line: "shared"},
		{Kind: opDelete, Line: "old"},
		{Kind: opInsert, Line: "shared"},
		{Kind: opInsert, Line: "new"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d ops, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("op %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestLCSOpsPreservesUnchangedMiddle(t *testing.T) {
	// The case a prefix/suffix preview gets wrong: two changed regions with
	// unchanged lines between them must NOT collapse into one block.
	before := []string{"1", "same", "same", "2"}
	after := []string{"one", "same", "same", "two"}
	ops := lcsOps(before, after)
	equals := 0
	for _, op := range ops {
		if op.Kind == opEqual {
			equals++
		}
	}
	if equals != 2 {
		t.Fatalf("got %d equal ops, want 2 (the unchanged middle): %+v", equals, ops)
	}
}

func TestRetainedLineHelpers(t *testing.T) {
	lines := splitLines("first\nsecond\n")
	if len(lines) != 2 || lines[0] != "first" || lines[1] != "second" {
		t.Fatalf("splitLines = %#v, want [first second]", lines)
	}
	if lines := splitLines(""); lines != nil {
		t.Fatalf("splitLines(empty) = %#v, want nil", lines)
	}

	before := []string{"same", "old", "tail"}
	after := []string{"same", "new", "tail"}
	prefix := commonPrefixLen(before, after)
	if prefix != 1 {
		t.Fatalf("commonPrefixLen = %d, want 1", prefix)
	}
	if suffix := commonSuffixLen(before, after, prefix); suffix != 1 {
		t.Fatalf("commonSuffixLen = %d, want 1", suffix)
	}
	if suffix := commonSuffixLen([]string{"same"}, []string{"same"}, 1); suffix != 0 {
		t.Fatalf("commonSuffixLen overlapping prefix = %d, want 0", suffix)
	}
}

func TestBuildHunksSplitsDistantChanges(t *testing.T) {
	before := []string{"a", "1", "2", "3", "4", "5", "6", "7", "8", "b"}
	after := []string{"A", "1", "2", "3", "4", "5", "6", "7", "8", "B"}

	hunks := buildHunks(lcsOps(before, after), 2)

	if len(hunks) != 2 {
		t.Fatalf("got %d hunks, want 2 (changes are 8 lines apart): %+v", len(hunks), hunks)
	}
	if hunks[0].BeforeStart != 1 || hunks[1].BeforeStart != 8 {
		t.Errorf("hunk starts = %d and %d, want 1 and 8", hunks[0].BeforeStart, hunks[1].BeforeStart)
	}
}

func TestBuildHunksMergesOverlappingContext(t *testing.T) {
	before := []string{"a", "1", "b"}
	after := []string{"A", "1", "B"}

	hunks := buildHunks(lcsOps(before, after), 2)

	if len(hunks) != 1 {
		t.Fatalf("got %d hunks, want 1 (contexts overlap): %+v", len(hunks), hunks)
	}
}

func TestBuildHunksNoChangeYieldsNone(t *testing.T) {
	lines := []string{"a", "b"}
	if hunks := buildHunks(lcsOps(lines, lines), 3); len(hunks) != 0 {
		t.Fatalf("got %d hunks for identical input, want 0", len(hunks))
	}
}

func TestBuildHunksZeroLengthRanges(t *testing.T) {
	tests := []struct {
		name       string
		before     []string
		after      []string
		wantBefore [2]int
		wantAfter  [2]int
	}{
		{
			name:       "create at file start",
			after:      []string{"a"},
			wantBefore: [2]int{0, 0},
			wantAfter:  [2]int{1, 1},
		},
		{
			name:       "delete at file start",
			before:     []string{"a"},
			wantBefore: [2]int{1, 1},
			wantAfter:  [2]int{0, 0},
		},
		{
			name:       "append after two lines",
			before:     []string{"a", "b"},
			after:      []string{"a", "b", "c"},
			wantBefore: [2]int{2, 0},
			wantAfter:  [2]int{3, 1},
		},
		{
			name:       "remove EOF",
			before:     []string{"a", "b", "c"},
			after:      []string{"a", "b"},
			wantBefore: [2]int{3, 1},
			wantAfter:  [2]int{2, 0},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hunks := buildHunks(lcsOps(tt.before, tt.after), 0)
			if len(hunks) != 1 {
				t.Fatalf("got %d hunks, want 1: %+v", len(hunks), hunks)
			}
			got := hunks[0]
			if got.BeforeStart != tt.wantBefore[0] || got.BeforeLines != tt.wantBefore[1] {
				t.Errorf("before range = %d,%d, want %d,%d", got.BeforeStart, got.BeforeLines, tt.wantBefore[0], tt.wantBefore[1])
			}
			if got.AfterStart != tt.wantAfter[0] || got.AfterLines != tt.wantAfter[1] {
				t.Errorf("after range = %d,%d, want %d,%d", got.AfterStart, got.AfterLines, tt.wantAfter[0], tt.wantAfter[1])
			}
		})
	}
}

func TestBuildHunksMaxContextDoesNotOverflow(t *testing.T) {
	before := []string{"before", "old", "after"}
	after := []string{"before", "new", "after"}
	ops := lcsOps(before, after)

	hunks := buildHunks(ops, int(^uint(0)>>1))

	if len(hunks) != 1 {
		t.Fatalf("got %d hunks, want 1: %+v", len(hunks), hunks)
	}
	if len(hunks[0].Ops) != len(ops) {
		t.Errorf("hunk has %d ops, want all %d ops", len(hunks[0].Ops), len(ops))
	}
}

func TestRenderUnifiedDiffHeadersAndPrefixes(t *testing.T) {
	got := renderUnifiedDiff("a.go", "x\nold\nz\n", "x\nnew\nz\n", 1, 1<<20)

	for _, want := range []string{
		"--- a/a.go\n",
		"+++ b/a.go\n",
		"@@ -1,3 +1,3 @@\n",
		"\n-old\n",
		"\n+new\n",
		"\n x\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("diff missing %q:\n%s", want, got)
		}
	}
}

func TestRenderUnifiedDiffIdenticalIsEmpty(t *testing.T) {
	if got := renderUnifiedDiff("a.go", "same\n", "same\n", 3, 1<<20); got != "" {
		t.Fatalf("identical content rendered %q, want empty", got)
	}
}

func TestRenderUnifiedDiffFinalNewlineSemantics(t *testing.T) {
	tests := []struct {
		name         string
		before       string
		after        string
		contextLines int
		want         string
	}{
		{
			name:         "EOL removed only",
			before:       "line\n",
			after:        "line",
			contextLines: 1,
			want: "--- a/f\n+++ b/f\n@@ -1,1 +1,1 @@\n" +
				"-line\n+line\n\\ No newline at end of file\n",
		},
		{
			name:         "EOL added only",
			before:       "line",
			after:        "line\n",
			contextLines: 1,
			want: "--- a/f\n+++ b/f\n@@ -1,1 +1,1 @@\n" +
				"-line\n\\ No newline at end of file\n+line\n",
		},
		{
			name:         "append after unterminated final line",
			before:       "old",
			after:        "old\nnew\n",
			contextLines: 0,
			want: "--- a/f\n+++ b/f\n@@ -1,1 +1,2 @@\n" +
				"-old\n\\ No newline at end of file\n+old\n+new\n",
		},
		{
			name:         "truncate to an unterminated final line",
			before:       "old\nnew\n",
			after:        "old",
			contextLines: 0,
			want: "--- a/f\n+++ b/f\n@@ -1,2 +1,1 @@\n" +
				"-old\n-new\n+old\n\\ No newline at end of file\n",
		},
		{
			name:         "both sides no final EOL and content changes",
			before:       "old",
			after:        "new",
			contextLines: 1,
			want: "--- a/f\n+++ b/f\n@@ -1,1 +1,1 @@\n" +
				"-old\n\\ No newline at end of file\n+new\n\\ No newline at end of file\n",
		},
		{
			name:         "unchanged final no EOL remains context",
			before:       "old\nsame",
			after:        "new\nsame",
			contextLines: 1,
			want: "--- a/f\n+++ b/f\n@@ -1,2 +1,2 @@\n" +
				"-old\n+new\n same\n\\ No newline at end of file\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := renderUnifiedDiff("f", tt.before, tt.after, tt.contextLines, 1<<20); got != tt.want {
				t.Fatalf("rendered:\n%s\nwant:\n%s", got, tt.want)
			}
		})
	}
}

func TestRenderUnifiedDiffEscapesHeaderPath(t *testing.T) {
	path := "dir/\n@@ -9,9 +9,9 @@\n+forged\r\t\"\\\x01"
	got := renderUnifiedDiff(path, "old\n", "new\n", 0, 1<<20)

	beforeHeaders, afterHeaders, hunks := 0, 0, 0
	var headerLines []string
	for _, line := range strings.Split(got, "\n") {
		switch {
		case strings.HasPrefix(line, "--- "):
			beforeHeaders++
			headerLines = append(headerLines, line)
		case strings.HasPrefix(line, "+++ "):
			afterHeaders++
			headerLines = append(headerLines, line)
		case strings.HasPrefix(line, "@@ "):
			hunks++
		case strings.HasPrefix(line, "+forged"):
			t.Fatalf("forged added line in rendered diff:\n%s", got)
		}
	}
	if beforeHeaders != 1 || afterHeaders != 1 || hunks != 1 {
		t.Fatalf("headers/hunks = %d/%d/%d, want 1/1/1:\n%s", beforeHeaders, afterHeaders, hunks, got)
	}
	for _, header := range headerLines {
		if strings.ContainsAny(header, "\r\t") {
			t.Fatalf("raw control character in header %q", header)
		}
		for _, want := range []string{`\n@@ -9,9 +9,9 @@\n+forged`, `\r\t\"\\\x01`} {
			if !strings.Contains(header, want) {
				t.Errorf("header %q missing escaped path fragment %q", header, want)
			}
		}
	}
}

func TestRenderUnifiedDiffDropsWholeHunksToFitBudget(t *testing.T) {
	before := strings.Repeat("keep\n", 40) + "one\n" + strings.Repeat("keep\n", 40) + "two\n"
	after := strings.Repeat("keep\n", 40) + "ONE\n" + strings.Repeat("keep\n", 40) + "TWO\n"
	wantOmitted := "--- a/a.go\n+++ b/a.go\n@@ -40,3 +40,3 @@\n" +
		" keep\n-one\n+ONE\n keep\n... 1 hunks omitted\n"
	wantFull := wantOmitted[:len(wantOmitted)-len("... 1 hunks omitted\n")] +
		"@@ -81,2 +81,2 @@\n keep\n-two\n+TWO\n"

	got := renderUnifiedDiff("a.go", before, after, 1, 90)

	if len(got) > 90 {
		t.Fatalf("rendered %d bytes, want <= 90:\n%s", len(got), got)
	}
	if got != wantOmitted {
		t.Fatalf("rendered:\n%s\nwant:\n%s", got, wantOmitted)
	}
	if got := renderUnifiedDiff("a.go", before, after, 1, len(wantFull)); got != wantFull {
		t.Fatalf("exact complete budget rendered:\n%s\nwant:\n%s", got, wantFull)
	}
	if got := renderUnifiedDiff("a.go", before, after, 1, len(wantFull)-1); got != wantOmitted {
		t.Fatalf("one-byte-short budget rendered:\n%s\nwant:\n%s", got, wantOmitted)
	}
	if got := renderUnifiedDiff("a.go", before, after, 1, 0); got != "" {
		t.Fatalf("zero budget rendered %q, want empty", got)
	}
}

func TestEditPreviewKeepsUnchangedMiddleSeparate(t *testing.T) {
	// Regression for the retired prefix/suffix preview: two distant replace_all matches used
	// to collapse into one hunk spanning every unchanged line between them.
	before := "match\n" + strings.Repeat("keep\n", 30) + "match\n"
	after := "REPL\n" + strings.Repeat("keep\n", 30) + "REPL\n"

	got := editPreview("a.go", before, after)

	if strings.Count(got, "@@ -") != 2 {
		t.Fatalf("want 2 hunks, got:\n%s", got)
	}
	if strings.Contains(got, "-keep") || strings.Contains(got, "+keep") {
		t.Errorf("unchanged lines were marked as changed:\n%s", got)
	}
}

func TestEditPreviewNoChangeNote(t *testing.T) {
	if got := editPreview("a.go", "x\n", "x\n"); got != "edited a.go (no changes)" {
		t.Fatalf("got %q", got)
	}
}

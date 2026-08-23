package filemutation

import "testing"

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

func TestLCSOpsPreservesUnchangedMiddle(t *testing.T) {
	// The case today's diffPreview gets wrong: two changed regions with
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

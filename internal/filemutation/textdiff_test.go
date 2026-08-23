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

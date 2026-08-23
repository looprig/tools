package filemutation

type diffOpKind uint8

const (
	opEqual diffOpKind = iota
	opDelete
	opInsert
)

type diffOp struct {
	Kind diffOpKind
	Line string
}

// hunk is a changed edit-script region with surrounding context. Starts name
// the 1-based input line positions before the first operation in the hunk. A
// zero-length side starts at its preceding line, including 0 at file start.
type hunk struct {
	BeforeStart int
	BeforeLines int
	AfterStart  int
	AfterLines  int
	Ops         []diffOp
}

// maxDiffRegionLines bounds each input to keep the (n+1)*(m+1) int32 DP table
// around 16 MiB at worst. Larger regions fall back to a correct, coarse script
// of deletes followed by inserts rather than allocating an unbounded table.
const maxDiffRegionLines = 2000

// lcsOps returns a line-level edit script from before to after. It uses an LCS
// table whose entries hold the LCS length of each pair of suffixes.
func lcsOps(before, after []string) []diffOp {
	if len(before) == 0 && len(after) == 0 {
		return nil
	}
	if len(before) > maxDiffRegionLines || len(after) > maxDiffRegionLines {
		return coarseOps(before, after)
	}

	n, m := len(before), len(after)
	width := m + 1
	table := make([]int32, (n+1)*width)

	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if before[i] == after[j] {
				table[i*width+j] = table[(i+1)*width+j] + 1
				continue
			}
			down := table[(i+1)*width+j]
			right := table[i*width+j+1]
			if down >= right {
				table[i*width+j] = down
			} else {
				table[i*width+j] = right
			}
		}
	}

	ops := make([]diffOp, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		if before[i] == after[j] {
			ops = append(ops, diffOp{Kind: opEqual, Line: before[i]})
			i++
			j++
			continue
		}
		if table[(i+1)*width+j] >= table[i*width+j+1] {
			ops = append(ops, diffOp{Kind: opDelete, Line: before[i]})
			i++
		} else {
			ops = append(ops, diffOp{Kind: opInsert, Line: after[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{Kind: opDelete, Line: before[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{Kind: opInsert, Line: after[j]})
	}

	return ops
}

func coarseOps(before, after []string) []diffOp {
	ops := make([]diffOp, 0, len(before)+len(after))
	for _, line := range before {
		ops = append(ops, diffOp{Kind: opDelete, Line: line})
	}
	for _, line := range after {
		ops = append(ops, diffOp{Kind: opInsert, Line: line})
	}
	return ops
}

// buildHunks groups changed operations with contextLines unchanged operations
// on either side. Overlapping or adjacent context windows merge so an equal
// operation is never emitted by two hunks.
func buildHunks(ops []diffOp, contextLines int) []hunk {
	if contextLines < 0 {
		contextLines = 0
	}

	type span struct {
		lo int
		hi int
	}
	spans := make([]span, 0)
	for i, op := range ops {
		if op.Kind == opEqual {
			continue
		}

		lo := max(0, i-contextLines)
		hi := len(ops) - 1
		if contextLines < len(ops)-1-i {
			hi = i + contextLines
		}
		if n := len(spans); n > 0 && lo <= spans[n-1].hi+1 {
			if hi > spans[n-1].hi {
				spans[n-1].hi = hi
			}
			continue
		}
		spans = append(spans, span{lo: lo, hi: hi})
	}
	if len(spans) == 0 {
		return nil
	}

	// Record the 1-based line positions before each operation while walking the
	// full script once. Deletes advance only before; inserts advance only after.
	lineAt := make([][2]int, len(ops))
	beforeLine, afterLine := 1, 1
	for i, op := range ops {
		lineAt[i] = [2]int{beforeLine, afterLine}
		switch op.Kind {
		case opEqual:
			beforeLine++
			afterLine++
		case opDelete:
			beforeLine++
		case opInsert:
			afterLine++
		}
	}

	hunks := make([]hunk, 0, len(spans))
	for _, span := range spans {
		hunkOps := ops[span.lo : span.hi+1]
		h := hunk{
			BeforeStart: lineAt[span.lo][0],
			AfterStart:  lineAt[span.lo][1],
			Ops:         hunkOps,
		}
		for _, op := range hunkOps {
			switch op.Kind {
			case opEqual:
				h.BeforeLines++
				h.AfterLines++
			case opDelete:
				h.BeforeLines++
			case opInsert:
				h.AfterLines++
			}
		}
		if h.BeforeLines == 0 {
			h.BeforeStart--
		}
		if h.AfterLines == 0 {
			h.AfterStart--
		}
		hunks = append(hunks, h)
	}
	return hunks
}

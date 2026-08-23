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

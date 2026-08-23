package filemutation

import (
	"strconv"
	"strings"
	"unicode"
)

type diffOpKind uint8

const (
	opEqual diffOpKind = iota
	opDelete
	opInsert
)

type diffOp struct {
	Kind           diffOpKind
	Line           string
	NoFinalNewline bool
}

type sourceLine struct {
	Text           string
	NoFinalNewline bool
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

// diffContextLines is fixed at render time so every diff consumer receives
// identical context and text.
const diffContextLines = 3

func lcsOps(before, after []string) []diffOp {
	return lcsSourceOps(sourceLinesFromStrings(before), sourceLinesFromStrings(after))
}

func coarseOps(before, after []string) []diffOp {
	return coarseSourceOps(sourceLinesFromStrings(before), sourceLinesFromStrings(after))
}

// lcsSourceOps returns a line-level edit script from before to after. It uses
// an LCS table whose entries hold the LCS length of each pair of suffixes.
func lcsSourceOps(before, after []sourceLine) []diffOp {
	if len(before) == 0 && len(after) == 0 {
		return nil
	}
	if len(before) > maxDiffRegionLines || len(after) > maxDiffRegionLines {
		return coarseSourceOps(before, after)
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
			ops = append(ops, diffOpForSource(opEqual, before[i]))
			i++
			j++
			continue
		}
		if table[(i+1)*width+j] >= table[i*width+j+1] {
			ops = append(ops, diffOpForSource(opDelete, before[i]))
			i++
		} else {
			ops = append(ops, diffOpForSource(opInsert, after[j]))
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOpForSource(opDelete, before[i]))
	}
	for ; j < m; j++ {
		ops = append(ops, diffOpForSource(opInsert, after[j]))
	}

	return ops
}

func coarseSourceOps(before, after []sourceLine) []diffOp {
	ops := make([]diffOp, 0, len(before)+len(after))
	for _, line := range before {
		ops = append(ops, diffOpForSource(opDelete, line))
	}
	for _, line := range after {
		ops = append(ops, diffOpForSource(opInsert, line))
	}
	return ops
}

func diffOpForSource(kind diffOpKind, line sourceLine) diffOp {
	return diffOp{Kind: kind, Line: line.Text, NoFinalNewline: line.NoFinalNewline}
}

func sourceLinesFromStrings(lines []string) []sourceLine {
	source := make([]sourceLine, len(lines))
	for i, line := range lines {
		source[i].Text = line
	}
	return source
}

func sourceLines(s string) []sourceLine {
	source := sourceLinesFromStrings(splitLines(s))
	if s != "" && !strings.HasSuffix(s, "\n") {
		source[len(source)-1].NoFinalNewline = true
	}
	return source
}

func commonSourcePrefixLen(a, b []sourceLine) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

func commonSourceSuffixLen(a, b []sourceLine, prefix int) int {
	n := 0
	for n < len(a)-prefix && n < len(b)-prefix && a[len(a)-1-n] == b[len(b)-1-n] {
		n++
	}
	return n
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

// renderUnifiedDiff renders the changed line hunks between before and after as
// bounded unified-diff text. It omits whole trailing hunks when the complete
// representation would exceed maxBytes.
func renderUnifiedDiff(path, before, after string, contextLines, maxBytes int) string {
	if before == after {
		return ""
	}

	beforeLines := sourceLines(before)
	afterLines := sourceLines(after)
	prefix := commonSourcePrefixLen(beforeLines, afterLines)
	suffix := commonSourceSuffixLen(beforeLines, afterLines, prefix)

	beforeMiddleEnd := len(beforeLines) - suffix
	afterMiddleEnd := len(afterLines) - suffix
	ops := make([]diffOp, 0, prefix)
	for _, line := range beforeLines[:prefix] {
		ops = append(ops, diffOpForSource(opEqual, line))
	}
	ops = append(ops, lcsSourceOps(beforeLines[prefix:beforeMiddleEnd], afterLines[prefix:afterMiddleEnd])...)
	for _, line := range beforeLines[beforeMiddleEnd:] {
		ops = append(ops, diffOpForSource(opEqual, line))
	}

	hunks := buildHunks(ops, contextLines)
	if len(hunks) == 0 {
		return ""
	}

	header := "--- " + headerPath("a/", path) + "\n+++ " + headerPath("b/", path) + "\n"
	if maxBytes <= 0 || len(header) > maxBytes {
		return ""
	}

	var output strings.Builder
	output.WriteString(header)
	for i, h := range hunks {
		renderedHunk := renderHunk(h)
		remainingTrailer := omittedTrailer(len(hunks) - i - 1)
		if output.Len()+len(renderedHunk)+len(remainingTrailer) > maxBytes {
			trailer := omittedTrailer(len(hunks) - i)
			if output.Len()+len(trailer) > maxBytes {
				return ""
			}
			output.WriteString(trailer)
			return output.String()
		}
		output.WriteString(renderedHunk)
	}
	return output.String()
}

func renderHunk(h hunk) string {
	var output strings.Builder
	output.WriteString("@@ -")
	output.WriteString(strconv.Itoa(h.BeforeStart))
	output.WriteByte(',')
	output.WriteString(strconv.Itoa(h.BeforeLines))
	output.WriteString(" +")
	output.WriteString(strconv.Itoa(h.AfterStart))
	output.WriteByte(',')
	output.WriteString(strconv.Itoa(h.AfterLines))
	output.WriteString(" @@\n")

	for _, op := range h.Ops {
		switch op.Kind {
		case opEqual:
			output.WriteByte(' ')
		case opDelete:
			output.WriteByte('-')
		case opInsert:
			output.WriteByte('+')
		}
		output.WriteString(op.Line)
		output.WriteByte('\n')
		if op.NoFinalNewline {
			output.WriteString("\\ No newline at end of file\n")
		}
	}
	return output.String()
}

func headerPath(prefix, path string) string {
	fullPath := prefix + path
	for _, r := range fullPath {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == '"' || r == '\\' {
			return strconv.Quote(fullPath)
		}
	}
	return fullPath
}

func omittedTrailer(n int) string {
	if n <= 0 {
		return ""
	}
	return "... " + strconv.Itoa(n) + " hunks omitted\n"
}

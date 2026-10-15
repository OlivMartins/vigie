package pins

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
)

// Render pretty-prints a stored definition for review. Invisible and control
// characters, a classic way to hide instructions from a reviewer while the
// model still reads them, are shown as \u{XXXX}.
func Render(definition json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, definition, "", "  "); err != nil {
		buf.Reset()
		buf.Write(definition)
	}
	return revealInvisible(buf.String())
}

// DiffContext is the number of unchanged lines shown around each change.
const DiffContext = 2

// Diff returns a line diff between two definitions: lines prefixed with "-"
// were removed, "+" added, " " unchanged. Only DiffContext lines are kept
// around each change; a skipped stretch shows as "  …".
func Diff(from, to json.RawMessage) string {
	a := strings.Split(Render(from), "\n")
	b := strings.Split(Render(to), "\n")
	ops := diffLines(a, b)
	keep := make([]bool, len(ops))
	for i, op := range ops {
		if op.kind == ' ' {
			continue
		}
		for j := max(0, i-DiffContext); j <= min(len(ops)-1, i+DiffContext); j++ {
			keep[j] = true
		}
	}
	var out strings.Builder
	skipped := false
	for i, op := range ops {
		if !keep[i] {
			skipped = true
			continue
		}
		if skipped {
			out.WriteString("  …\n")
		}
		skipped = false
		out.WriteByte(op.kind)
		out.WriteByte(' ')
		out.WriteString(op.line)
		out.WriteByte('\n')
	}
	if skipped && out.Len() > 0 {
		out.WriteString("  …\n")
	}
	return out.String()
}

type diffOp struct {
	kind byte
	line string
}

// diffLines computes a shortest edit script with a longest common subsequence
// table. Tool definitions are a few hundred lines at most.
func diffLines(a, b []string) []diffOp {
	n, m := len(a), len(b)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	ops := make([]diffOp, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{' ', a[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, diffOp{'-', a[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{'-', a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{'+', b[j]})
	}
	return ops
}

func revealInvisible(s string) string {
	var out strings.Builder
	for _, r := range s {
		if r != '\n' && (unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Co, r)) {
			fmt.Fprintf(&out, `\u{%04X}`, r)
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}

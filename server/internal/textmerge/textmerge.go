// Package textmerge does line-based three-way merges, the same way the
// Obsidian plugin merges concurrent edits of a note.
package textmerge

import "strings"

// maxCells bounds the LCS table; bigger inputs are reported as a conflict
// rather than burning memory on a web request.
const maxCells = 16 << 20

// Merge combines the changes from base→ours and base→theirs. ok is false
// when both sides changed the same lines.
func Merge(base, ours, theirs string) (merged string, ok bool) {
	switch {
	case ours == theirs:
		return ours, true
	case ours == base:
		return theirs, true
	case theirs == base:
		return ours, true
	}
	o, a, b := lines(base), lines(ours), lines(theirs)
	ma, okA := match(o, a)
	mb, okB := match(o, b)
	if !okA || !okB {
		return "", false
	}
	var out strings.Builder
	io, ia, ib := 0, 0, 0
	for {
		// Stable run: base lines kept unchanged on both sides.
		k := 0
		for io+k < len(o) && ma[io+k] == ia+k && mb[io+k] == ib+k {
			k++
		}
		if k > 0 {
			for _, l := range o[io : io+k] {
				out.WriteString(l)
			}
			io, ia, ib = io+k, ia+k, ib+k
			continue
		}
		// Unstable chunk up to the next base line both sides kept.
		j := io
		for j < len(o) && (ma[j] < 0 || mb[j] < 0) {
			j++
		}
		ea, eb := len(a), len(b)
		if j < len(o) {
			ea, eb = ma[j], mb[j]
		}
		co, ca, cb := o[io:j], a[ia:ea], b[ib:eb]
		switch {
		case equal(ca, co):
			writeAll(&out, cb)
		case equal(cb, co), equal(ca, cb):
			writeAll(&out, ca)
		default:
			return "", false
		}
		if j >= len(o) {
			return out.String(), true
		}
		io, ia, ib = j, ea, eb
	}
}

// lines splits s keeping the line terminators, so joining is lossless.
func lines(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.SplitAfter(s, "\n")
	if parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

func equal(x, y []string) bool {
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

func writeAll(b *strings.Builder, ls []string) {
	for _, l := range ls {
		b.WriteString(l)
	}
}

// match returns, for every line of o, the index of the line of x it is
// paired with in a longest common subsequence, or -1.
func match(o, x []string) ([]int, bool) {
	m := make([]int, len(o))
	for i := range m {
		m[i] = -1
	}
	// Common prefix and suffix need no table.
	p := 0
	for p < len(o) && p < len(x) && o[p] == x[p] {
		m[p] = p
		p++
	}
	s := 0
	for s < len(o)-p && s < len(x)-p && o[len(o)-1-s] == x[len(x)-1-s] {
		m[len(o)-1-s] = len(x) - 1 - s
		s++
	}
	oo, xx := o[p:len(o)-s], x[p:len(x)-s]
	n, k := len(oo), len(xx)
	if n == 0 || k == 0 {
		return m, true
	}
	if (n+1)*(k+1) > maxCells {
		return nil, false
	}
	// dp[i][j] = LCS length of oo[i:] and xx[j:].
	w := k + 1
	dp := make([]int32, (n+1)*w)
	for i := n - 1; i >= 0; i-- {
		for j := k - 1; j >= 0; j-- {
			if oo[i] == xx[j] {
				dp[i*w+j] = dp[(i+1)*w+j+1] + 1
			} else if dp[(i+1)*w+j] >= dp[i*w+j+1] {
				dp[i*w+j] = dp[(i+1)*w+j]
			} else {
				dp[i*w+j] = dp[i*w+j+1]
			}
		}
	}
	for i, j := 0, 0; i < n && j < k; {
		switch {
		case oo[i] == xx[j]:
			m[p+i] = p + j
			i++
			j++
		case dp[(i+1)*w+j] >= dp[i*w+j+1]:
			i++
		default:
			j++
		}
	}
	return m, true
}

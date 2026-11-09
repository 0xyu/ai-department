package policy

import (
	"bytes"
	"strings"
)

// Line is one added or removed line of a diff, with its line number in the
// new file (added) or the old file (removed).
type Line struct {
	No   int
	Text string
}

// maxEdits bounds the Myers search. When two versions differ by more edits
// than this, every line between their common prefix and suffix counts as
// changed, which can only add findings, never hide one.
const maxEdits = 1000

// splitLines splits content into lines without any length limit. A final
// newline does not start an extra empty line; a trailing "\r" is dropped
// from each line's text.
func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	b = bytes.TrimSuffix(b, []byte{'\n'})
	parts := strings.Split(string(b), "\n")
	for i, p := range parts {
		parts[i] = strings.TrimSuffix(p, "\r")
	}
	return parts
}

// DiffLines compares two versions of a file's raw content and returns the
// added lines (numbered in the new version) and the removed lines (numbered
// in the old version). It works on the bytes as stored, so neither
// .gitattributes nor diff presentation settings can hide a line.
func DiffLines(oldContent, newContent []byte) (added, removed []Line) {
	a, b := splitLines(oldContent), splitLines(newContent)
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	s := 0
	for s < len(a)-p && s < len(b)-p && a[len(a)-1-s] == b[len(b)-1-s] {
		s++
	}
	ins, del := myers(a[p:len(a)-s], b[p:len(b)-s])
	for _, i := range ins {
		added = append(added, Line{No: p + i + 1, Text: b[p+i]})
	}
	for _, i := range del {
		removed = append(removed, Line{No: p + i + 1, Text: a[p+i]})
	}
	return added, removed
}

// myers returns the indexes of inserted lines of b and deleted lines of a in
// a shortest edit script, or every index when more than maxEdits edits are
// needed.
func myers(a, b []string) (ins, del []int) {
	n, m := len(a), len(b)
	all := func() ([]int, []int) {
		for i := 0; i < m; i++ {
			ins = append(ins, i)
		}
		for i := 0; i < n; i++ {
			del = append(del, i)
		}
		return ins, del
	}
	if n == 0 || m == 0 {
		return all()
	}
	limit := n + m
	if limit > maxEdits {
		limit = maxEdits
	}
	off := limit + 1
	v := make([]int, 2*limit+3)
	var trace [][]int
	for d := 0; d <= limit; d++ {
		trace = append(trace, append([]int(nil), v...))
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = v[off+k+1]
			} else {
				x = v[off+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[off+k] = x
			if x >= n && y >= m {
				return backtrack(trace, off, n, m)
			}
		}
	}
	return all()
}

func backtrack(trace [][]int, off, x, y int) (ins, del []int) {
	for d := len(trace) - 1; d > 0; d-- {
		v := trace[d]
		k := x - y
		var prevK int
		if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := v[off+prevK]
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			x--
			y--
		}
		if prevK == k+1 {
			ins = append(ins, prevY) // moved down: b[prevY] inserted
		} else {
			del = append(del, prevX) // moved right: a[prevX] deleted
		}
		x, y = prevX, prevY
	}
	reverse(ins)
	reverse(del)
	return ins, del
}

func reverse(s []int) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}

// Package textdiff computes the line diff that the admin review shows
// (design 0009 D7, D3.11): a Myers diff of two texts, line by line, and
// hunks with context lines around each change.
package textdiff

import "strings"

// Kind says whether a line is in both texts, only in the old one, or only
// in the new one.
type Kind byte

const (
	Same Kind = ' '
	Del  Kind = '-'
	Add  Kind = '+'
)

// Line is one line of a diff. A and B are its 1-based line numbers in the
// old and the new text, 0 where it has none. Text holds the line without
// its line feed; NoEOL marks the last line of a text that does not end in
// a line feed, so that "a" and "a\n" differ.
type Line struct {
	Kind  Kind
	A, B  int
	Text  string
	NoEOL bool
}

// Hunk is a run of lines around one or more changes: the changed lines and
// up to the context's number of unchanged lines before and after them.
type Hunk struct {
	// A and B are the first line numbers of the hunk in the old and the
	// new text, and ALen and BLen its lengths in each, as in the header of
	// a unified diff.
	A, ALen, B, BLen int
	Lines            []Line
}

// Limits on the work of one diff. Both texts are at most 256 KiB in the
// admin, but a text of many short lines can still make the greedy search
// slow; past these limits the remaining middle of the texts is shown as
// removed and added in full, which is still a correct diff.
const (
	maxEdits = 1000
	maxWork  = 40_000_000
)

// split cuts a text into lines, each with its line feed if it has one.
func split(s string) []string {
	if s == "" {
		return nil
	}
	out := make([]string, 0, strings.Count(s, "\n")+1)
	for s != "" {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			out = append(out, s)
			break
		}
		out = append(out, s[:i+1])
		s = s[i+1:]
	}
	return out
}

// Diff returns every line of the two texts in order, as a shortest edit
// script from a to b within the limits above: the unchanged lines, and for
// each change the removed lines before the added ones.
func Diff(a, b string) []Line {
	al, bl := split(a), split(b)
	// Lines become numbers, so the search compares integers.
	ids := map[string]int32{}
	num := func(ls []string) []int32 {
		out := make([]int32, len(ls))
		for i, l := range ls {
			id, ok := ids[l]
			if !ok {
				id = int32(len(ids))
				ids[l] = id
			}
			out[i] = id
		}
		return out
	}
	x, y := num(al), num(bl)

	// The common prefix and suffix need no search.
	pre := 0
	for pre < len(x) && pre < len(y) && x[pre] == y[pre] {
		pre++
	}
	suf := 0
	for suf < len(x)-pre && suf < len(y)-pre && x[len(x)-1-suf] == y[len(y)-1-suf] {
		suf++
	}
	mid := script(x[pre:len(x)-suf], y[pre:len(y)-suf])

	out := make([]Line, 0, len(al)+len(bl))
	i, j := 0, 0 // next line of a and b
	emit := func(k Kind) {
		l := Line{Kind: k}
		var s string
		switch k {
		case Same:
			s = al[i]
			i++
			j++
			l.A, l.B = i, j
		case Del:
			s = al[i]
			i++
			l.A = i
		case Add:
			s = bl[j]
			j++
			l.B = j
		}
		l.Text, l.NoEOL = strings.CutSuffix(s, "\n")
		l.NoEOL = !l.NoEOL
		out = append(out, l)
	}
	for range pre {
		emit(Same)
	}
	for _, k := range mid {
		emit(k)
	}
	for range suf {
		emit(Same)
	}
	return out
}

// script returns the edit script from x to y: Myers' greedy search for
// the furthest reaching path on each diagonal, keeping the state before
// each round for the trace back. Within a run of changes, removals come
// before additions.
func script(x, y []int32) []Kind {
	n, m := len(x), len(y)
	if n == 0 || m == 0 {
		return replace(n, m)
	}
	limit := min(n+m, maxEdits)
	off := limit + 1
	v := make([]int32, 2*limit+3) // furthest x on diagonal k at v[off+k]
	var trace [][]int32           // trace[d][k+d]: v before round d
	work := 0
	end := -1
	for d := 0; d <= limit && end < 0 && work <= maxWork; d++ {
		trace = append(trace, append([]int32(nil), v[off-d:off+d+1]...))
		for k := -d; k <= d; k += 2 {
			var px int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				px = int(v[off+k+1]) // down from diagonal k+1: an addition
			} else {
				px = int(v[off+k-1]) + 1 // right from diagonal k-1: a removal
			}
			py, sx := px-k, px
			for px < n && py < m && x[px] == y[py] {
				px++
				py++
			}
			work += 1 + px - sx
			v[off+k] = int32(px)
			if px >= n && py >= m {
				end = d
				break
			}
		}
	}
	if end < 0 {
		return replace(n, m)
	}
	out := make([]Kind, 0, n+m)
	px, py := n, m
	for d := end; d > 0; d-- {
		prev := trace[d]
		at := func(k int) int { return int(prev[k+d]) }
		k := px - py
		var pk, mx int // previous diagonal, and x after the step
		if k == -d || (k != d && at(k-1) < at(k+1)) {
			pk = k + 1
			mx = at(pk)
		} else {
			pk = k - 1
			mx = at(pk) + 1
		}
		for px > mx {
			out = append(out, Same)
			px--
			py--
		}
		if pk == k+1 {
			out = append(out, Add)
			py--
		} else {
			out = append(out, Del)
			px--
		}
	}
	for px > 0 {
		out = append(out, Same)
		px--
		py--
	}
	// The script was built backwards.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return removalsFirst(out)
}

// replace is the script that removes n lines and adds m.
func replace(n, m int) []Kind {
	out := make([]Kind, 0, n+m)
	for range n {
		out = append(out, Del)
	}
	for range m {
		out = append(out, Add)
	}
	return out
}

// removalsFirst orders each run of changes between unchanged lines so
// that its removals come first, as a unified diff shows them.
func removalsFirst(s []Kind) []Kind {
	for i := 0; i < len(s); {
		if s[i] == Same {
			i++
			continue
		}
		j := i
		dels := 0
		for j < len(s) && s[j] != Same {
			if s[j] == Del {
				dels++
			}
			j++
		}
		for k := i; k < j; k++ {
			if k-i < dels {
				s[k] = Del
			} else {
				s[k] = Add
			}
		}
		i = j
	}
	return s
}

// Hunks groups the lines of a diff into hunks with up to context
// unchanged lines before and after each change; changes closer than
// twice the context share a hunk. A diff without changes has no hunks.
func Hunks(lines []Line, context int) []Hunk {
	var out []Hunk
	for i := 0; i < len(lines); {
		if lines[i].Kind == Same {
			i++
			continue
		}
		start := max(i-context, 0)
		end := i // one past the last change of the hunk
		for j := i; j < len(lines); j++ {
			if lines[j].Kind != Same {
				end = j + 1
				continue
			}
			if j-end >= 2*context {
				break
			}
		}
		stop := min(end+context, len(lines))
		h := Hunk{Lines: lines[start:stop]}
		for _, l := range h.Lines {
			if l.Kind != Add {
				if h.A == 0 {
					h.A = l.A
				}
				h.ALen++
			}
			if l.Kind != Del {
				if h.B == 0 {
					h.B = l.B
				}
				h.BLen++
			}
		}
		// An empty side starts after the line before the hunk, as in a
		// unified diff header.
		if h.A == 0 {
			h.A = lineBefore(lines, start, func(l Line) int { return l.A })
		}
		if h.B == 0 {
			h.B = lineBefore(lines, start, func(l Line) int { return l.B })
		}
		out = append(out, h)
		i = stop
	}
	return out
}

func lineBefore(lines []Line, i int, n func(Line) int) int {
	for i--; i >= 0; i-- {
		if v := n(lines[i]); v > 0 {
			return v
		}
	}
	return 0
}

// Apply rebuilds the new text from the old one and the hunks of their
// diff: the check that a diff is complete. It returns false when a hunk
// does not match the old text.
func Apply(old string, hunks []Hunk) (string, bool) {
	al := split(old)
	var b strings.Builder
	next := 0 // next line of old, 0-based
	for _, h := range hunks {
		for _, l := range h.Lines {
			if l.Kind == Add {
				b.WriteString(text(l))
				continue
			}
			// Lines of old up to this one are unchanged.
			for next < l.A-1 {
				if next >= len(al) {
					return "", false
				}
				b.WriteString(al[next])
				next++
			}
			if next >= len(al) || al[next] != text(l) {
				return "", false
			}
			if l.Kind == Same {
				b.WriteString(al[next])
			}
			next++
		}
	}
	for ; next < len(al); next++ {
		b.WriteString(al[next])
	}
	return b.String(), true
}

func text(l Line) string {
	if l.NoEOL {
		return l.Text
	}
	return l.Text + "\n"
}

package textdiff

import (
	"math/rand/v2"
	"strings"
	"testing"
)

// lcs returns the length of a longest common subsequence of the lines of
// a and b, the oracle for a shortest edit script.
func lcs(a, b []string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for i := range a {
		for j := range b {
			if a[i] == b[j] {
				cur[j+1] = prev[j] + 1
			} else {
				cur[j+1] = max(cur[j], prev[j+1])
			}
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// check verifies one diff: the lines rebuild both texts with consecutive
// line numbers, the edit script is shortest when want is set, removals
// come before additions within a run, and the hunks with context 0 to 3
// rebuild the new text from the old one.
func check(t *testing.T, a, b string, shortest bool) []Line {
	t.Helper()
	lines := Diff(a, b)
	var oldText, newText strings.Builder
	na, nb, edits := 0, 0, 0
	for i, l := range lines {
		s := l.Text
		if !l.NoEOL {
			s += "\n"
		}
		if l.Kind != Add {
			na++
			if l.A != na {
				t.Fatalf("line %d: A %d, want %d", i, l.A, na)
			}
			oldText.WriteString(s)
		}
		if l.Kind != Del {
			nb++
			if l.B != nb {
				t.Fatalf("line %d: B %d, want %d", i, l.B, nb)
			}
			newText.WriteString(s)
		}
		if l.Kind != Same {
			edits++
		}
		if i > 0 && l.Kind == Del && lines[i-1].Kind == Add {
			t.Fatalf("line %d: a removal after an addition", i)
		}
	}
	if oldText.String() != a || newText.String() != b {
		t.Fatalf("the lines do not rebuild the texts:\n%q\n%q", oldText.String(), newText.String())
	}
	if shortest {
		al, bl := split(a), split(b)
		if want := len(al) + len(bl) - 2*lcs(al, bl); edits != want {
			t.Fatalf("%d edits, want %d for\n%q\n%q", edits, want, a, b)
		}
	}
	// Any context rebuilds it, also none, where a hunk may hold only
	// added lines (OI-0006 AA-R4).
	for _, c := range []int{0, 1, 2} {
		if got, ok := Apply(a, Hunks(lines, c)); !ok || got != b {
			t.Fatalf("the hunks with context %d do not rebuild the new text: %v\n%q\nwant %q", c, ok, got, b)
		}
	}
	hunks := Hunks(lines, 3)
	got, ok := Apply(a, hunks)
	if !ok || got != b {
		t.Fatalf("the hunks do not rebuild the new text: %v\n%q\nwant %q", ok, got, b)
	}
	for _, h := range hunks {
		for _, l := range h.Lines {
			if l.Kind != Same {
				goto changed
			}
		}
		t.Fatalf("a hunk without a change: %+v", h)
	changed:
	}
	return lines
}

// TestTextDiff checks the line diff of design 0009 D7 (W6): a shortest
// edit script, removals before additions, hunks with three lines of
// context that rebuild the new text from the old one, and the line feed
// at the end of a text, on fixed cases and on random texts from a small
// alphabet so that equal lines repeat.
func TestTextDiff(t *testing.T) {
	cases := []struct{ a, b string }{
		{"", ""},
		{"", "a\n"},
		{"a\n", ""},
		{"a\n", "a\n"},
		{"a", "a\n"},
		{"a\nb", "a\nb\n"},
		{"a\nb\nc\n", "a\nc\n"},
		{"a\nb\nc\n", "a\nx\nc\n"},
		{"a\nb\n", "a\nX\nb\n"},
		{"a\nb\n", "a\nb\nX\n"},
		{"x\r\ny\n", "x\ny\n"},
		{"1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n13\n14\n15\n", "1\n2\nX\n4\n5\n6\n7\n8\n9\n10\n11\n12\nY\n14\n15\n"},
	}
	for _, c := range cases {
		check(t, c.a, c.b, true)
	}
	// "a" and "a\n" differ; the last line of a says that it has no line
	// feed.
	if l := Diff("a", "a\n"); len(l) != 2 || l[0].Kind != Del || !l[0].NoEOL || l[1].Kind != Add || l[1].NoEOL {
		t.Errorf("a vs a\\n: %+v", l)
	}

	rnd := rand.New(rand.NewPCG(1, 2))
	text := func() string {
		var b strings.Builder
		for range rnd.IntN(30) {
			b.WriteString(string(rune('a' + rnd.IntN(4))))
			b.WriteByte('\n')
		}
		if rnd.IntN(5) == 0 {
			b.WriteString("z")
		}
		return b.String()
	}
	for range 3000 {
		check(t, text(), text(), true)
	}

	// Hunks: two changes six unchanged lines apart share a hunk; seven
	// apart do not.
	lines := func(n int, change ...int) string {
		var b strings.Builder
		for i := 1; i <= n; i++ {
			s := string(rune('a' + i%26))
			for _, c := range change {
				if c == i {
					s = "X"
				}
			}
			b.WriteString(s + "\n")
		}
		return b.String()
	}
	if h := Hunks(Diff(lines(30), lines(30, 10, 17)), 3); len(h) != 1 || h[0].A != 7 || h[0].ALen != 14 {
		t.Errorf("changes 6 lines apart: %d hunks %+v", len(h), h)
	}
	if h := Hunks(Diff(lines(30), lines(30, 10, 18)), 3); len(h) != 2 || h[0].A != 7 || h[0].ALen != 7 || h[1].A != 15 {
		t.Errorf("changes 7 lines apart: %d hunks %+v", len(h), h)
	}
	// An insertion at the start has an empty old side.
	if h := Hunks(Diff("a\n", "x\na\n"), 3); len(h) != 1 || h[0].A != 1 || h[0].ALen != 1 || h[0].B != 1 || h[0].BLen != 2 {
		t.Errorf("insertion at the start: %+v", h)
	}

	// Past the limits the diff stays correct: two texts of 3000 distinct
	// lines each need 6000 edits, more than the search does.
	var big1, big2 strings.Builder
	for i := range 3000 {
		big1.WriteString("old " + string(rune('A'+i%26)) + strings.Repeat("x", i%7) + "\n")
		big2.WriteString("new " + string(rune('A'+i%26)) + strings.Repeat("y", i%5) + "\n")
	}
	check(t, "head\n"+big1.String()+"tail\n", "head\n"+big2.String()+"tail\n", false)
}

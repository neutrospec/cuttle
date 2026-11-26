package config

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden files of the canonical form")

// validSamples returns every configuration file in testdata that Parse
// accepts, and the starter file, by name.
func validSamples(t testing.TB) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, pattern := range []string{"testdata/*/*.toml", "testdata/*/*/*.toml"} {
		paths, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range paths {
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if cfg, _ := Parse(data, p); cfg != nil {
				out[p] = data
			}
		}
	}
	starter, err := Starter()
	if err != nil {
		t.Fatal(err)
	}
	out["starter"] = starter
	return out
}

// sameConfig reports whether two configurations are equal apart from
// SHA256.
func sameConfig(a, b *Config) bool {
	x, y := *a, *b
	x.SHA256, y.SHA256 = "", ""
	return reflect.DeepEqual(x, y)
}

// checkCanonical checks the three properties of design 0009 D6 for one
// valid file: same configuration, canonical fixed point, and model
// fidelity.
func checkCanonical(t *testing.T, name string, data []byte) {
	t.Helper()
	doc, cfg, r := DocOf(data, name)
	if doc == nil {
		t.Fatalf("%s: DocOf refused a file that Parse accepts:\n%s", name, issues(r.Errors))
	}
	enc := doc.Encode()
	doc2, cfg2, r2 := DocOf(enc, name)
	if doc2 == nil {
		t.Fatalf("%s: the canonical form is invalid:\n%s\n%s", name, issues(r2.Errors), enc)
	}
	if !sameConfig(cfg, cfg2) {
		t.Errorf("%s: the canonical form parses to another configuration:\n%+v\n%+v\n%s", name, *cfg, *cfg2, enc)
	}
	if !reflect.DeepEqual(doc, doc2) {
		t.Errorf("%s: DocOf(Encode(d)) differs from d:\n%+v\n%+v", name, *doc, *doc2)
	}
	if again := doc2.Encode(); !bytes.Equal(again, enc) {
		t.Errorf("%s: the canonical form is not a fixed point:\n%s\n---\n%s", name, enc, again)
	}
}

// TestCanonicalRoundTrip checks W1 and W2 over every valid sample and
// the starter file: Parse(Encode(DocOf(f))) equals Parse(f), and the
// fixed point and model fidelity hold.
func TestCanonicalRoundTrip(t *testing.T) {
	samples := validSamples(t)
	if len(samples) < 15 {
		t.Fatalf("only %d valid samples", len(samples))
	}
	for name, data := range samples {
		checkCanonical(t, name, data)
	}
}

// FuzzCanonical checks the properties of TestCanonicalRoundTrip on inputs
// derived from every sample, valid or not; inputs that Parse refuses are
// skipped.
func FuzzCanonical(f *testing.F) {
	for _, pattern := range []string{"testdata/*/*.toml", "testdata/*/*/*.toml"} {
		paths, _ := filepath.Glob(pattern)
		for _, p := range paths {
			data, err := os.ReadFile(p)
			if err != nil {
				f.Fatal(err)
			}
			f.Add(data)
		}
	}
	starter, _ := Starter()
	f.Add(starter)
	f.Fuzz(func(t *testing.T, data []byte) {
		if cfg, _ := Parse(data, "fuzz"); cfg == nil {
			return
		}
		checkCanonical(t, "fuzz", data)
	})
}

// TestCanonicalGolden checks W2's fixed layout: a hand-written file that
// sets every key, with comments, literal and escaped strings, inline
// tables, and unnamed rules, encodes to the golden file byte for byte.
// Run with -update to rewrite the golden file, then review its diff.
func TestCanonicalGolden(t *testing.T) {
	data, err := os.ReadFile("testdata/canonical/handwritten.toml")
	if err != nil {
		t.Fatal(err)
	}
	doc, _, r := DocOf(data, "handwritten.toml")
	if doc == nil {
		t.Fatalf("handwritten.toml:\n%s", issues(r.Errors))
	}
	got := doc.Encode()
	const golden = "testdata/canonical/handwritten.golden.toml"
	if *updateGolden {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("canonical form differs from %s:\n%s", golden, got)
	}

	// The layout rules of D6, checked on the output itself.
	text := string(got)
	if !strings.HasSuffix(text, "\"\n") || strings.HasSuffix(text, "\n\n") || strings.Contains(text, "\n\n\n") {
		t.Errorf("one final newline and single empty lines between tables")
	}
	for i, l := range strings.Split(text, "\n") {
		if strings.ContainsAny(l, "\t\r") || strings.TrimRight(l, " ") != l {
			t.Errorf("line %d has a tab, a CR, or trailing spaces: %q", i+1, l)
		}
		if strings.HasPrefix(l, " ") && !strings.HasPrefix(l, "  \"") {
			t.Errorf("line %d is indented outside an array: %q", i+1, l)
		}
	}

	// An array stays on one line up to 80 columns.
	for _, c := range []struct {
		width int
		lines int
	}{{80, 1}, {81, 4}} {
		// hosts = ["<a>", "<b>"] is 16 columns plus the two names.
		a := strings.Repeat("a", (c.width-16)/2)
		b := strings.Repeat("b", c.width-16-len(a))
		d := &Doc{Rules: []DocRule{{Name: "x", Hosts: Some([]string{a, b})}}}
		out := string(d.Encode())
		start := strings.Index(out, "hosts = [")
		block := out[start : start+strings.Index(out[start:], "]\n")+1]
		if n := strings.Count(block, "\n") + 1; n != c.lines {
			t.Errorf("array of %d columns takes %d lines, want %d:\n%s", c.width, n, c.lines, block)
		}
	}
}

// TestCanonicalEscaping checks the string rules of D6 (W3): '"', '\',
// control characters, and non-ASCII characters survive the round trip,
// every control character, U+007F included, is written escaped, and
// nothing else is.
func TestCanonicalEscaping(t *testing.T) {
	// Through the encoder and the TOML parser, with values that no valid
	// configuration holds.
	values := []string{
		"quote \" and backslash \\",
		"tab\there, DEL\x7f, NUL\x00, ESC\x1b, US\x1f",
		"newline\n CR\r backspace\b form feed\f",
		"東京, café, emoji 🦑, U+0080 \u0080, U+2028 \u2028",
		"#not a comment, 'single' quotes, ''' and \"\"\"",
		"",
	}
	for _, v := range values {
		d := &Doc{Name: "cuttle-test", Proxy: DocProxy{Default: "direct"},
			Rules: []DocRule{{Name: "x", Note: Some(v), Hosts: Some([]string{v}), Action: "direct"}}}
		enc := d.Encode()
		if !utf8.Valid(enc) {
			t.Errorf("%q: output is not UTF-8", v)
		}
		for _, b := range enc {
			if (b < 0x20 && b != '\n') || b == 0x7f {
				t.Errorf("%q: raw control byte %#x in the output", v, b)
			}
		}
		var raw struct {
			Rule []struct {
				Note  string
				Hosts []string
			}
		}
		if _, err := toml.Decode(string(enc), &raw); err != nil {
			t.Errorf("%q: the output is not TOML: %v\n%s", v, err, enc)
			continue
		}
		if len(raw.Rule) != 1 || raw.Rule[0].Note != v || len(raw.Rule[0].Hosts) != 1 || raw.Rule[0].Hosts[0] != v {
			t.Errorf("%q: decoded as %+v", v, raw.Rule)
		}
	}
	for v, want := range map[string]string{
		"\x7f": `"\u007F"`, "\x00": `"\u0000"`, "\x1b": `"\u001B"`, "\t": `"\t"`, "\n": `"\n"`, "\r": `"\r"`,
		"\b": `"\b"`, "\f": `"\f"`, `"`: `"\""`, `\`: `"\\"`, "東": `"東"`, "\u0080": "\"\u0080\"",
		"\xff": "\"\uFFFD\"",
	} {
		if got := quote(v); got != want {
			t.Errorf("quote(%q) = %s, want %s", v, got, want)
		}
	}

	// Through DocOf, with a valid note.
	note := `say "hi" \ 東京 # 🦑`
	src := "name = \"cuttle-test\"\n[proxy]\ndefault = \"direct\"\n[[rule]]\nname = \"x\"\nnote = '" + note + "'\nhosts = [\"x.example\"]\naction = \"block\"\n"
	doc, _, r := DocOf([]byte(src), "note.toml")
	if doc == nil {
		t.Fatalf("note.toml:\n%s", issues(r.Errors))
	}
	if doc.Rules[0].Note != Some(note) {
		t.Fatalf("note %+v", doc.Rules[0].Note)
	}
	checkCanonical(t, "note.toml", []byte(src))
}

// TestNoteValidation checks the note limits of D6 (W3): at most 120 code
// points and no control character, on rules, upstreams, and interfaces,
// with messages that never repeat the note.
func TestNoteValidation(t *testing.T) {
	const head = "name = \"cuttle-test\"\n[proxy]\ndefault = \"direct\"\n"
	entity := map[string]string{
		"rule":      "[[rule]]\nname = \"x\"\nhosts = [\"x.example\"]\naction = \"upstream:u\"\n",
		"upstream":  "[[upstream]]\nname = \"u\"\naddress = \"192.0.2.1:3128\"\ndisclosure = \"via\"\n",
		"interface": "[[interface]]\nname = \"eth-a\"\ndevice = \"en7\"\nkind = \"wired\"\n",
	}
	// Each entity carries the note in turn; the others are there so that
	// the file is otherwise valid.
	build := func(kind, line string) string {
		var b strings.Builder
		b.WriteString(head)
		for _, k := range []string{"interface", "upstream", "rule"} {
			b.WriteString(entity[k])
			if k == kind {
				b.WriteString(line)
			}
		}
		return b.String()
	}
	long := strings.Repeat("東", MaxNoteLength)
	for kind := range entity {
		path := kind + "[1].note"
		for _, c := range []struct {
			name, line, msg string
			value           string // must not appear in the message
		}{
			{"120 non-ASCII characters", "note = \"" + long + "\"\n", "", ""},
			{"empty", "note = \"\"\n", "", ""},
			{"121 characters", "note = \"" + long + "x\"\n", "is 121 characters long; a note has at most 120", long},
			{"tab", "note = \"secret\\there\"\n", "contains a control character", "secret"},
			{"DEL", "note = \"secret\\u007Fhere\"\n", "contains a control character", "secret"},
			{"newline", "note = \"\"\"secret\nhere\"\"\"\n", "contains a control character", "secret"},
			{"integer", "note = 12345\n", "must be a string, not an integer", "12345"},
		} {
			cfg, r := Parse([]byte(build(kind, c.line)), "note.toml")
			if c.msg == "" {
				if cfg == nil {
					t.Errorf("%s, %s: refused:\n%s", kind, c.name, issues(r.Errors))
				}
				continue
			}
			if cfg != nil || len(r.Errors) != 1 || r.Errors[0].Key != path || !strings.HasPrefix(r.Errors[0].Msg, c.msg) {
				t.Errorf("%s, %s: want one error %s: %s; got:\n%s", kind, c.name, path, c.msg, issues(r.Errors))
				continue
			}
			if v := c.value[:min(len(c.value), 6)]; strings.Contains(r.Errors[0].String(), v) {
				t.Errorf("%s, %s: message repeats the note: %s", kind, c.name, r.Errors[0])
			}
		}
	}
	// note is not a key of the other tables.
	if cfg, r := Parse([]byte(head+"note = \"x\"\n"), "note.toml"); cfg != nil || r.Errors[0].Key != "proxy.note" {
		t.Errorf("proxy.note: %v", r.Err())
	}
}

// TestNoteSyntaxError checks that a TOML syntax error inside a note gives
// a fixed message that repeats none of it (OI-0006 Z-R3, design 0009 D6),
// while another syntax error keeps the parser's message.
func TestNoteSyntaxError(t *testing.T) {
	const head = "name = \"cuttle-test\"\n[proxy]\ndefault = \"direct\"\n"
	for _, c := range []struct{ name, text string }{
		{"rule, unquoted", head + "[[rule]]\nname = \"x\"\nhosts = [\"x.example\"]\naction = \"block\"\nnote = secretword here\n"},
		{"rule, unterminated", head + "[[rule]]\nname = \"x\"\nnote = \"secretword here\naction = \"block\"\n"},
		{"upstream, unquoted", head + "[[upstream]]\nname = \"u\"\nnote = secretword\naddress = \"192.0.2.1:3128\"\n"},
		{"upstream, bad escape", head + "[[upstream]]\nname = \"u\"\nnote = \"secretword \\q\"\n"},
		{"interface, unquoted", head + "[[interface]]\nname = \"eth-a\"\nnote = secretword\n"},
		{"inline rule", head + "rule = [{name = \"x\", note = secretword}]\n"},
	} {
		_, r := Parse([]byte(c.text), "note.toml")
		// Under [[interface]] the interface hint, as fixed, comes first.
		want := noteSyntaxHint
		if strings.HasPrefix(c.name, "interface") {
			want = interfaceSyntaxHint
		}
		if len(r.Errors) != 1 || r.Errors[0].Msg != want || strings.Contains(r.Errors[0].String(), "secret") {
			t.Errorf("%s: %s", c.name, issues(r.Errors))
		}
	}
	_, r := Parse([]byte(head+"[[rule]]\nname = secretword\n"), "x.toml")
	if len(r.Errors) != 1 || r.Errors[0].Msg == noteSyntaxHint || !strings.HasPrefix(r.Errors[0].Msg, "TOML syntax: ") {
		t.Errorf("a syntax error outside a note: %s", issues(r.Errors))
	}
}

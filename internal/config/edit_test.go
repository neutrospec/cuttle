package config

import (
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// editBase has three rules, two upstreams, and two interface definitions,
// with every kind of reference: rule actions and both defaults use the
// upstream lab, and proxy.interfaces, a rule, and an upstream list eth-a.
const editBase = `name = "cuttle-test"

[proxy]
default = "upstream:lab"
interfaces = ["wifi", "eth-a"]

[proxy.remote]
default = "upstream:lab"

[[interface]]
name = "eth-a"
mac = "00:00:5e:00:53:0a"

[[interface]]
name = "wifi"
device = "en0"
kind = "wifi"

[[upstream]]
name = "lab"
address = "192.0.2.6:3128"
disclosure = "via"
interfaces = ["eth-a"]

[[upstream]]
name = "egress"
address = "192.0.2.7:3128"
disclosure = "none"

[[rule]]
name = "a"
domains = ["a.example"]
action = "upstream:lab"

[[rule]]
name = "b"
hosts = ["b.example"]
action = "direct"
interfaces = ["eth-a"]

[[rule]]
name = "c"
domains = ["c.example"]
action = "upstream:egress"
`

func editDoc(t *testing.T) *Doc {
	t.Helper()
	d, _, r := DocOf([]byte(editBase), "edit.toml")
	if d == nil {
		t.Fatalf("edit base:\n%s", issues(r.Errors))
	}
	return d
}

func ruleNames(d *Doc) string {
	names := make([]string, len(d.Rules))
	for i, r := range d.Rules {
		names[i] = r.Name
	}
	return strings.Join(names, ",")
}

// mustValid encodes d and checks that the candidate is valid.
func mustValid(t *testing.T, what string, d *Doc) *Config {
	t.Helper()
	cfg, r := Parse(d.Encode(), "candidate.toml")
	if cfg == nil {
		t.Fatalf("%s: candidate invalid:\n%s\n%s", what, issues(r.Errors), d.Encode())
	}
	return cfg
}

// TestDocEdits checks W4: each op of design 0009 D7 gives the expected
// Doc, renames rewrite every reference, a used upstream or interface
// cannot be deleted, a stale target or order is refused, and a refused
// op leaves the Doc unchanged.
func TestDocEdits(t *testing.T) {
	newRule := DocRule{Name: "n", Hosts: Some([]string{"n.example"}), Action: "block"}

	t.Run("insert", func(t *testing.T) {
		for _, c := range []struct {
			name string
			at   func(d *Doc) int
			want string
		}{
			{"first", func(*Doc) int { return 0 }, "n,a,b,c"},
			{"before c", func(d *Doc) int { _, i := d.Rule("c"); return i }, "a,b,n,c"},
			{"last", func(d *Doc) int { return len(d.Rules) }, "a,b,c,n"},
		} {
			d := editDoc(t)
			if err := d.InsertRule(newRule, c.at(d)); err != nil || ruleNames(d) != c.want {
				t.Errorf("%s: %v %s", c.name, err, ruleNames(d))
			}
			mustValid(t, c.name, d)
		}
		d := editDoc(t)
		for _, at := range []int{-1, 4} {
			if err := d.InsertRule(newRule, at); err == nil || ruleNames(d) != "a,b,c" {
				t.Errorf("insert at %d: %v %s", at, err, ruleNames(d))
			}
		}
	})

	t.Run("replace and delete", func(t *testing.T) {
		d := editDoc(t)
		r, _ := d.Rule("b")
		r.Name, r.Domains = "b2", Some([]string{"b2.example"})
		if err := d.ReplaceRule("b", r); err != nil || ruleNames(d) != "a,b2,c" {
			t.Fatalf("replace: %v %s", err, ruleNames(d))
		}
		if got, _ := d.Rule("b2"); !slices.Equal(got.Domains.V, []string{"b2.example"}) || !slices.Equal(got.Hosts.V, []string{"b.example"}) {
			t.Errorf("replaced rule %+v", got)
		}
		if err := d.DeleteRule("a"); err != nil || ruleNames(d) != "b2,c" {
			t.Errorf("delete: %v %s", err, ruleNames(d))
		}
		mustValid(t, "replace and delete", d)
		before := d.Clone()
		for name, err := range map[string]error{
			"replace": d.ReplaceRule("gone", newRule),
			"delete":  d.DeleteRule("gone"),
		} {
			if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), `rule "gone" no longer exists`) {
				t.Errorf("%s of a missing rule: %v", name, err)
			}
		}
		if !reflect.DeepEqual(d, before) {
			t.Errorf("refused ops changed the Doc")
		}
	})

	t.Run("reorder", func(t *testing.T) {
		d := editDoc(t)
		if err := d.ReorderRules([]string{"c", "a", "b"}); err != nil || ruleNames(d) != "c,a,b" {
			t.Fatalf("reorder: %v %s", err, ruleNames(d))
		}
		if r, _ := d.Rule("b"); !slices.Equal(r.Interfaces.V, []string{"eth-a"}) {
			t.Errorf("rule b lost its keys: %+v", r)
		}
		for _, bad := range [][]string{{"c", "a"}, {"c", "a", "a"}, {"c", "a", "x"}, {"c", "a", "b", "b"}, nil} {
			if err := d.ReorderRules(bad); !errors.Is(err, ErrOrder) || ruleNames(d) != "c,a,b" {
				t.Errorf("reorder %q: %v %s", bad, err, ruleNames(d))
			}
		}
	})

	t.Run("duplicate", func(t *testing.T) {
		d := editDoc(t)
		cp, err := d.DuplicateRule("b")
		if err != nil || cp.Name != "b-copy" || !slices.Equal(cp.Hosts.V, []string{"b.example"}) || ruleNames(d) != "a,b,c" {
			t.Fatalf("duplicate: %v %+v %s", err, cp, ruleNames(d))
		}
		cp.Hosts.V[0] = "changed.example"
		if r, _ := d.Rule("b"); r.Hosts.V[0] != "b.example" {
			t.Errorf("the copy shares its lists with the original")
		}
		d.InsertRule(cp, 2)
		if cp2, _ := d.DuplicateRule("b"); cp2.Name != "b-copy-2" {
			t.Errorf("second copy %q", cp2.Name)
		}
		if n := d.FreeRuleName(strings.Repeat("x", 64)); n != strings.Repeat("x", 64) {
			t.Errorf("free name %q", n)
		}
		d.Rules[0].Name = strings.Repeat("x", 64)
		if n := d.FreeRuleName(strings.Repeat("x", 64)); n != strings.Repeat("x", 62)+"-2" {
			t.Errorf("free name of a used 64-character name %q", n)
		}
		if _, err := d.DuplicateRule("gone"); !errors.Is(err, ErrNotFound) {
			t.Errorf("duplicate of a missing rule: %v", err)
		}
	})

	t.Run("rename upstream", func(t *testing.T) {
		d := editDoc(t)
		u, _ := d.Upstream("lab")
		u.Name = "lab-b"
		if err := d.ReplaceUpstream("lab", u); err != nil {
			t.Fatal(err)
		}
		if d.Proxy.Default != "upstream:lab-b" || d.Remote.Default != Some("upstream:lab-b") ||
			d.Rules[0].Action != "upstream:lab-b" || d.Rules[1].Action != "direct" || d.Rules[2].Action != "upstream:egress" {
			t.Errorf("references after rename: %q %+v %q %q %q", d.Proxy.Default, d.Remote.Default, d.Rules[0].Action, d.Rules[1].Action, d.Rules[2].Action)
		}
		cfg := mustValid(t, "rename upstream", d)
		if _, ok := cfg.Upstream("lab-b"); !ok || cfg.Rules[0].Route.Upstream != "lab-b" {
			t.Errorf("config after rename: %+v", cfg.Rules[0].Route)
		}
		// Unset remote default stays unset.
		d = editDoc(t)
		d.Remote.Default = Opt[string]{}
		d.ReplaceUpstream("lab", u)
		if d.Remote.Default.Set {
			t.Errorf("an unset default became set: %+v", d.Remote.Default)
		}
	})

	t.Run("rename interface", func(t *testing.T) {
		d := editDoc(t)
		orig := d.Clone()
		x, _ := d.Interface("eth-a")
		x.Name = "eth-c"
		if err := d.ReplaceInterface("eth-a", x); err != nil {
			t.Fatal(err)
		}
		r, _ := d.Rule("b")
		u, _ := d.Upstream("lab")
		if !slices.Equal(d.Proxy.Interfaces.V, []string{"wifi", "eth-c"}) || !slices.Equal(r.Interfaces.V, []string{"eth-c"}) ||
			!slices.Equal(u.Interfaces.V, []string{"eth-c"}) || d.Upstreams[1].Interfaces.Set {
			t.Errorf("lists after rename: %v %v %v %+v", d.Proxy.Interfaces, r.Interfaces, u.Interfaces, d.Upstreams[1].Interfaces)
		}
		mustValid(t, "rename interface", d)
		if !reflect.DeepEqual(orig, editDoc(t)) {
			t.Errorf("the rename changed the Doc it was cloned from")
		}
	})

	t.Run("clone", func(t *testing.T) {
		d := editDoc(t)
		c := d.Clone()
		c.Proxy.Interfaces.V[0] = "x"
		c.Remote.Default.V = "x"
		c.Rules[0].Domains.V[0] = "x"
		c.Rules[1].Interfaces.V[0] = "x"
		c.Upstreams[0].Interfaces.V[0] = "x"
		c.Interfaces[0].Name = "x"
		if !reflect.DeepEqual(d, editDoc(t)) {
			t.Errorf("an edit of the clone changed the original")
		}
	})

	t.Run("guarded deletes", func(t *testing.T) {
		d := editDoc(t)
		before := d.Clone()
		var inUse *InUseError
		err := d.DeleteUpstream("lab")
		if !errors.As(err, &inUse) || !reflect.DeepEqual(inUse.Users, []Ref{
			{Kind: KindRule, Name: "a"}, {Kind: KindKey, Name: "proxy.default"}, {Kind: KindKey, Name: "proxy.remote.default"},
		}) {
			t.Errorf("delete used upstream: %v", err)
		}
		if err == nil || err.Error() != "upstream \"lab\" is used by rule `a`, proxy.default, proxy.remote.default" {
			t.Errorf("message: %v", err)
		}
		err = d.DeleteInterface("eth-a")
		if !errors.As(err, &inUse) || !reflect.DeepEqual(inUse.Users, []Ref{
			{Kind: KindKey, Name: "proxy.interfaces", Pos: 2}, {Kind: KindRule, Name: "b", Pos: 1}, {Kind: KindUpstream, Name: "lab", Pos: 1},
		}) {
			t.Errorf("delete used interface: %v", err)
		}
		if !reflect.DeepEqual(d, before) {
			t.Errorf("refused deletes changed the Doc")
		}
		if !errors.Is(d.DeleteUpstream("gone"), ErrNotFound) || !errors.Is(d.DeleteInterface("gone"), ErrNotFound) ||
			!errors.Is(d.ReplaceUpstream("gone", DocUpstream{}), ErrNotFound) || !errors.Is(d.ReplaceInterface("gone", DocInterface{}), ErrNotFound) {
			t.Errorf("ops on a missing upstream or interface")
		}

		// Once the uses are gone, the deletes work.
		d.Rules[0].Action = "block"
		d.Proxy.Default = "direct"
		d.Remote.Default = Opt[string]{}
		if err := d.DeleteUpstream("lab"); err != nil || len(d.Upstreams) != 1 {
			t.Errorf("delete unused upstream: %v", err)
		}
		d.Proxy.Interfaces = Some([]string{"wifi"})
		d.Rules[1].Interfaces = Opt[[]string]{}
		if err := d.DeleteInterface("eth-a"); err != nil || len(d.Interfaces) != 1 {
			t.Errorf("delete unused interface: %v", err)
		}
		mustValid(t, "deletes", d)
	})

	t.Run("add", func(t *testing.T) {
		d := editDoc(t)
		d.AddUpstream(DocUpstream{Name: "extra", Address: "192.0.2.9:3128", Disclosure: "via"})
		d.AddInterface(DocInterface{Name: "eth-b", MAC: Some("00:00:5e:00:53:0b")})
		if len(d.Upstreams) != 3 || d.Upstreams[2].Name != "extra" || len(d.Interfaces) != 3 || d.Interfaces[2].Name != "eth-b" {
			t.Errorf("add: %+v %+v", d.Upstreams, d.Interfaces)
		}
		mustValid(t, "add", d)
	})

	t.Run("route a host", func(t *testing.T) {
		d := editDoc(t)
		d.Rules[2].Ports = Some([]int64{443})
		d.InsertRule(DocRule{Name: "d", Hosts: Some([]string{"d.example"}), Action: "upstream:egress"}, 3)
		if got := d.RouteRules("upstream:egress"); !slices.Equal(got, []string{"d"}) {
			t.Errorf("RouteRules: %v", got)
		}
		if err := d.AddPattern("d", "upstream:egress", PatternDomains, "video.example"); err != nil {
			t.Fatal(err)
		}
		d.AddPattern("d", "upstream:egress", PatternDomains, "video.example")
		r, _ := d.Rule("d")
		if !slices.Equal(r.Domains.V, []string{"video.example"}) || !slices.Equal(r.Hosts.V, []string{"d.example"}) {
			t.Errorf("after AddPattern: %+v", r)
		}
		before := d.Clone()
		for _, c := range []struct {
			rule, action string
			want         error
		}{
			{"d", "direct", ErrRuleChanged},
			{"c", "upstream:egress", ErrRuleChanged},
			{"gone", "direct", ErrNotFound},
		} {
			if err := d.AddPattern(c.rule, c.action, PatternHosts, "x.example"); !errors.Is(err, c.want) {
				t.Errorf("AddPattern %s %s: %v", c.rule, c.action, err)
			}
		}
		if !reflect.DeepEqual(d, before) {
			t.Errorf("refused AddPattern changed the Doc")
		}
		mustValid(t, "route a host", d)
	})
}

// TestSummarize checks the change summary of design 0009 D7.
func TestSummarize(t *testing.T) {
	lines := func(old, new *Doc, h Hint) []string {
		var out []string
		for _, l := range Summarize(old, new, h) {
			out = append(out, l.String())
		}
		return out
	}
	check := func(name string, got []string, want ...string) {
		t.Helper()
		if !slices.Equal(got, want) {
			t.Errorf("%s:\n got %q\nwant %q", name, got, want)
		}
	}
	base := editDoc(t)

	d := base.Clone()
	check("no change", lines(base, d, Hint{}))

	d.Rules[0].Domains = Some([]string{"a.example", "a2.example"})
	d.Rules[0].Action = "block"
	d.Rules[1].Interfaces = Opt[[]string]{}
	check("rule keys", lines(base, d, Hint{}),
		"Rule `a`: domains +1, action upstream:lab → block",
		"Rule `b`: interfaces −1")

	d = base.Clone()
	d.ReorderRules([]string{"c", "a", "b"})
	check("one move", lines(base, d, Hint{}), "Moved rule `c` from 3 to 1")

	// Among thousands of rules, one move is still one line, and a reversed
	// order moves all but one.
	big := &Doc{}
	for i := range 5000 {
		big.Rules = append(big.Rules, DocRule{Name: "r" + strconv.Itoa(i+1)})
	}
	d = big.Clone()
	moved := d.Rules[4000]
	d.Rules = slices.Insert(slices.Delete(d.Rules, 4000, 4001), 1, moved)
	check("one move of 5000", lines(big, d, Hint{}), "Moved rule `r4001` from 4001 to 2")
	d = big.Clone()
	slices.Reverse(d.Rules)
	if n := len(Summarize(big, d, Hint{})); n != 4999 {
		t.Errorf("reversed 5000 rules: %d lines, want 4999", n)
	}

	d = base.Clone()
	d.InsertRule(DocRule{Name: "n", Hosts: Some([]string{"n.example"}), Action: "block"}, 0)
	d.DeleteRule("c")
	check("add and delete", lines(base, d, Hint{}), "Rule `n` added at position 1", "Rule `c` removed")

	d = base.Clone()
	u, _ := d.Upstream("lab")
	u.Name = "lab-b"
	d.ReplaceUpstream("lab", u)
	check("upstream rename", lines(base, d, Hint{Kind: KindUpstream, From: "lab", To: "lab-b"}),
		"Upstream renamed `lab` → `lab-b`; 1 rule, proxy.default, and proxy.remote.default updated")
	check("rename without a hint", lines(base, d, Hint{}),
		"Rule `a`: action upstream:lab → upstream:lab-b", "Upstream `lab-b` added", "Upstream `lab` removed",
		"`proxy.default`: upstream:lab → upstream:lab-b", "`proxy.remote.default`: upstream:lab → upstream:lab-b")

	d = base.Clone()
	x, _ := d.Interface("eth-a")
	x.Name = "eth-c"
	x.MAC = Some("00:00:5e:00:53:0c")
	d.ReplaceInterface("eth-a", x)
	got := lines(base, d, Hint{Kind: KindInterface, From: "eth-a", To: "eth-c"})
	check("interface rename", got,
		"Interface renamed `eth-a` → `eth-c`; 1 rule, 1 upstream, and proxy.interfaces updated",
		"Interface `eth-c`: hardware address changed")
	if strings.Contains(strings.Join(got, "\n"), "53:0") {
		t.Errorf("a summary shows a MAC: %q", got)
	}

	d = base.Clone()
	r, _ := d.Rule("b")
	r.Name, r.Note = "b2", Some("secret words")
	d.ReplaceRule("b", r)
	check("rule rename with a note", lines(base, d, Hint{Kind: KindRule, From: "b", To: "b2"}),
		"Rule renamed `b` → `b2`", "Rule `b2`: note changed")

	d = base.Clone()
	d.Proxy.ConnectTimeout = Some("20s")
	d.Proxy.Interfaces = Opt[[]string]{}
	d.Remote.Ports = Some([]int64{})
	d.Admin.Listen = Some("")
	d.Name = "cuttle-new"
	check("settings", lines(base, d, Hint{}),
		"`name`: cuttle-test → cuttle-new",
		"`proxy.connect_timeout`: default → 20s",
		"`proxy.interfaces`: wifi, eth-a → default",
		"`proxy.remote.ports`: default → []",
		"`admin.listen`: default → \"\"")

	// Code spans carry the names, so a page can mark them up.
	l := Summarize(base, d, Hint{})[0]
	if len(l) != 2 || !l[0].Code || l[0].Text != "name" || l[1].Code {
		t.Errorf("spans %+v", l)
	}
}

// TestHandComments checks the comment count of the canonical-form notice
// (design 0009 D7).
func TestHandComments(t *testing.T) {
	const src = `# one
  # two
name = "a # not" # three
[proxy] # four
listen = [
  "x", # five
  # six
]
s = '''
# not
''' # seven
t = """\"""" # eight
u = 'it"s'
"k#" = 1
`
	var raw map[string]any
	if _, err := toml.Decode(src, &raw); err != nil {
		t.Fatalf("sample is not TOML: %v", err)
	}
	if lines, eol := HandComments([]byte(src)); lines != 3 || eol != 5 {
		t.Errorf("counted %d comment lines and %d end-of-line comments, want 3 and 5", lines, eol)
	}
	d := editDoc(t)
	if lines, eol := HandComments(d.Encode()); lines != 0 || eol != 0 {
		t.Errorf("canonical form: %d %d", lines, eol)
	}
	starter, _ := Starter()
	if lines, eol := HandComments(starter); lines != 34 || eol != 3 {
		t.Errorf("starter: %d %d", lines, eol)
	}
}

package admin

import (
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/neutrospec/cuttle/internal/config"
)

// TestIssueFieldMap checks the key-path mapping of design 0009 D7 (W5):
// every key-path form the validator emits for the configuration samples
// splits into entity, key, and entry and names a page; issues of the
// edited entity go under their field with the entry, issues of other
// entities go to the list under the summary with a link, and file-level
// issues go to the top.
func TestIssueFieldMap(t *testing.T) {
	for key, want := range map[string]issueRef{
		"":                             {},
		"name":                         {Key: "name"},
		"rule":                         {Entity: "rule"},
		"rule[3]":                      {Entity: "rule", Index: 3},
		"rule[3].name":                 {Entity: "rule", Index: 3, Key: "name"},
		"rule[3].domains[2]":           {Entity: "rule", Index: 3, Key: "domains", Entry: 2},
		"rule[12].interfaces[1]":       {Entity: "rule", Index: 12, Key: "interfaces", Entry: 1},
		"upstream[1].address":          {Entity: "upstream", Index: 1, Key: "address"},
		"interface[2].mac":             {Entity: "interface", Index: 2, Key: "mac"},
		"proxy":                        {Entity: "proxy"},
		"proxy.listen[1]":              {Entity: "proxy", Key: "listen", Entry: 1},
		"proxy.default":                {Entity: "proxy", Key: "default"},
		"proxy.remote":                 {Entity: "proxy.remote"},
		"proxy.remote.sources[2]":      {Entity: "proxy.remote", Key: "sources", Entry: 2},
		"proxy.remote.ports":           {Entity: "proxy.remote", Key: "ports"},
		"admin.listen":                 {Entity: "admin", Key: "listen"},
		"stats.retention_days":         {Entity: "stats", Key: "retention_days"},
		"log.level":                    {Entity: "log", Key: "level"},
		"upstream[2].interfaces[3]":    {Entity: "upstream", Index: 2, Key: "interfaces", Entry: 3},
		"interface[1].note":            {Entity: "interface", Index: 1, Key: "note"},
		"proxy.interface_demotion":     {Entity: "proxy", Key: "interface_demotion"},
		"proxy.remote.allow_direct":    {Entity: "proxy.remote", Key: "allow_direct"},
		"upstream[1].unreachable_hold": {Entity: "upstream", Index: 1, Key: "unreachable_hold"},
		// Unknown keys are named as written.
		"interface[1].00:00:5e:00:53:21": {Entity: "interface", Index: 1, Key: "00:00:5e:00:53:21"},
		"proxy.a.b":                      {Entity: "proxy", Key: "a.b"},
	} {
		got, ok := parseIssueKey(key)
		if !ok || got != want {
			t.Errorf("parseIssueKey(%q) = %+v, %v; want %+v", key, got, ok, want)
		}
	}
	for _, bad := range []string{"rule[0].name", "rule[x]", "rule[1]name", "upstream[2", "interface[-1].mac"} {
		if got, ok := parseIssueKey(bad); ok {
			t.Errorf("parseIssueKey(%q) = %+v, want no match", bad, got)
		}
	}

	// Every key path that the validator emits for the samples of the
	// config package, valid and invalid, parses and names a page.
	var paths []string
	for _, pattern := range []string{"../config/testdata/*/*.toml", "../config/testdata/*/*/*.toml"} {
		m, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, m...)
	}
	if len(paths) < 60 {
		t.Fatalf("only %d samples", len(paths))
	}
	seen := 0
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		_, r := config.Parse(data, p)
		for _, is := range append(slices.Clone(r.Errors), r.Warnings...) {
			if strings.HasPrefix(is.Msg, "TOML syntax") {
				continue // a syntax error names the parser's last key; text ops list it by line
			}
			seen++
			ref, ok := parseIssueKey(is.Key)
			if !ok {
				t.Errorf("%s: key path %q does not parse (%s)", p, is.Key, is.Msg)
				continue
			}
			if href := entityHref(ref, nil); !strings.HasPrefix(href, "/") {
				t.Errorf("%s: key path %q names no page", p, is.Key)
			}
		}
	}
	t.Logf("%d issues from %d samples", seen, len(paths))

	// A rule form: the edited rule is the second of the candidate.
	doc, _, _ := config.DocOf([]byte(exampleConfig), "x")
	rep := &config.Report{
		Errors: []config.Issue{
			{Key: "rule[2].domains[2]", Msg: `"*.x" is not a domain`},
			{Key: "rule[2].ports[1]", Msg: "port 0 is out of range 1-65535"},
			{Key: "rule[2]", Msg: "rule has none of hosts, domains, cidrs, or ports"},
			{Key: "rule[2].interfaces", Msg: "applies only to direct rules"},
			{Key: "", Msg: "file is larger than 256 KiB"},
		},
		Warnings: []config.Issue{
			{Key: "rule[3].domains[1]", Msg: `"ads.example.net" repeats rule "restricted"`},
			{Key: "upstream[2].name", Msg: `upstream "lab" is not used by any action`},
			{Key: "proxy.interfaces", Msg: "warning on proxy"},
			{Key: "name", Msg: "warning on name"},
			{Key: "rule[2].note", Msg: "a warning of the edited rule"},
		},
	}
	f := newFormFrom(testRuleOp, map[string]string{}, "restricted", "", "/rules/restricted")
	f.mapIssues(rep, doc, 2)
	if got := f.Errors("domains"); !reflect.DeepEqual(got, []string{`Entry 2: "*.x" is not a domain`}) || f.Bad("domains") != "2" {
		t.Errorf("domains: %q, bad %q", got, f.Bad("domains"))
	}
	if got := f.Errors("ports"); !reflect.DeepEqual(got, []string{"Entry 1: port 0 is out of range 1-65535"}) || f.Bad("ports") != "1" {
		t.Errorf("ports: %q, bad %q", got, f.Bad("ports"))
	}
	// interfaces maps to the choice, whose list shows no entries.
	if got := f.Errors("ifaces"); !reflect.DeepEqual(got, []string{"applies only to direct rules"}) {
		t.Errorf("interfaces: %q", got)
	}
	if !reflect.DeepEqual(f.Top, []string{"rule has none of hosts, domains, cidrs, or ports", "file is larger than 256 KiB"}) ||
		!reflect.DeepEqual(f.Warnings("note"), []string{"a warning of the edited rule"}) {
		t.Errorf("top %q, note warnings %q", f.Top, f.Warnings("note"))
	}
	var summary []string
	for _, s := range f.Summary {
		summary = append(summary, segText(s.Label)+": "+s.Msg+" → "+s.Href)
	}
	if want := []string{`Domains, entry 2: "*.x" is not a domain → #f-domains`, "Ports, entry 1: port 0 is out of range 1-65535 → #f-ports",
		"Interfaces: applies only to direct rules → #f-ifaces"}; !reflect.DeepEqual(summary, want) {
		t.Errorf("summary:\n got %q\nwant %q", summary, want)
	}
	var others []string
	for _, o := range f.Others {
		others = append(others, segText(o.Label)+": "+o.Msg+" → "+o.Href)
	}
	if want := []string{`Rule ads: domains, entry 1: "ads.example.net" repeats rule "restricted" → /rules/ads`,
		`Upstream lab: name: upstream "lab" is not used by any action → /upstreams/lab`,
		"Proxy: interfaces: warning on proxy → /proxy", "General: name: warning on name → /general"}; !reflect.DeepEqual(others, want) {
		t.Errorf("others:\n got %q\nwant %q", others, want)
	}

	// A settings form owns its tables: an entry of the ports choice shows
	// on its list, other tables go to the list of other entries.
	g := newFormFrom(testRemoteOp, map[string]string{}, "", "", "/remote")
	g.mapIssues(&config.Report{Errors: []config.Issue{
		{Key: "proxy.remote.ports[2]", Msg: "port 70000 is out of range 1-65535"},
		{Key: "proxy.remote.sources[1]", Msg: "not a CIDR"},
		{Key: "proxy.remote", Msg: "remote listener without sources"},
		{Key: "proxy.listen[1]", Msg: "not loopback"},
	}}, doc, 0)
	if g.Errors("ports_list")[0] != "Entry 2: port 70000 is out of range 1-65535" || g.Bad("ports_list") != "2" ||
		g.Errors("sources")[0] != "Entry 1: not a CIDR" || !reflect.DeepEqual(g.Top, []string{"remote listener without sources"}) ||
		len(g.Others) != 1 || g.Others[0].Href != "/proxy" {
		t.Errorf("settings form: errors %v, top %q, others %+v", g.errs, g.Top, g.Others)
	}
}

func segText(s []seg) string {
	var b strings.Builder
	for _, x := range s {
		b.WriteString(x.Text)
	}
	return b.String()
}

// stateForm returns a posted form of op with each field's rendered value
// and the posted controls.
func stateForm(op *formOp, target string, rendered map[string]string, posted url.Values) *form {
	v := url.Values{"op": {op.Name}, "target": {target}}
	for k, r := range rendered {
		v.Set("r:"+k, r)
	}
	for k, p := range posted {
		v[k] = p
	}
	r := httptest.NewRequest("POST", "/config/draft", strings.NewReader(v.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.ParseForm()
	return postedForm(op, r)
}

// TestFieldState checks the field-state rule of design 0009 D4 (W5) for
// each control kind: a field posted with its rendered value keeps its key
// as the current file has it, set or unset, also when another writer
// changed it; a changed field sets the key; an emptied text field or
// list, or a cleared checkbox, unsets it; values compare parsed, so a
// list with CRLF line ends or commas that parses to its rendered list is
// untouched; a choice maps by the table, so a hidden sub-field keeps no
// key (OI-0006 Y-R4); a new entity writes its untouched prefilled
// fields; and the hidden state of a re-render is the posted rendered
// values, never recomputed.
func TestFieldState(t *testing.T) {
	set := config.Some[string]
	unset := config.Opt[string]{}
	rendered := map[string]string{"name": "r1", "note": "old note", "hosts": "a.example\nb.example", "domains": "",
		"ports": "443", "action": "direct", "allow_loopback": "false", "ifaces": "only", "ifaces_list": "eth-a\nwifi"}
	same := url.Values{}
	for k, v := range rendered {
		same.Set(k, v)
	}
	with := func(kv ...string) url.Values {
		v := url.Values{}
		for k, x := range same {
			v[k] = x
		}
		for i := 0; i < len(kv); i += 2 {
			v.Set(kv[i], kv[i+1])
		}
		return v
	}

	// Text: untouched keeps the file's value, set or unset, also when it
	// differs from the rendered one; changed sets; emptied unsets.
	f := stateForm(testRuleOp, "r1", rendered, same)
	if got := f.Text("note", set("changed elsewhere")); got != set("changed elsewhere") {
		t.Errorf("untouched note: %+v", got)
	}
	if got := f.Text("note", unset); got != unset {
		t.Errorf("untouched note, unset in the file: %+v", got)
	}
	if got := stateForm(testRuleOp, "r1", rendered, with("note", "new\r\nnote")).Text("note", set("old note")); got != set("new\nnote") {
		t.Errorf("changed note: %+v", got)
	}
	if got := stateForm(testRuleOp, "r1", rendered, with("note", "")).Text("note", set("old note")); got != unset {
		t.Errorf("emptied note: %+v", got)
	}
	if got := stateForm(testRuleOp, "r1", rendered, with("note", "old note\r\n")).Text("note", set("x")); got != set("old note\n") {
		t.Errorf("note with a line end added: %+v", got)
	}

	// Lists: CRLF, commas, and spaces that parse to the rendered list are
	// untouched.
	for _, posted := range []string{"a.example\r\nb.example", "a.example, b.example", " a.example b.example\n"} {
		other := config.Some([]string{"other.example"})
		if got := stateForm(testRuleOp, "r1", rendered, with("hosts", posted)).List("hosts", other); !reflect.DeepEqual(got, other) {
			t.Errorf("hosts %q: %+v, want the file's list", posted, got)
		}
	}
	if got := stateForm(testRuleOp, "r1", rendered, with("hosts", "b.example,a.example")).List("hosts", config.Opt[[]string]{}); !reflect.DeepEqual(got, config.Some([]string{"b.example", "a.example"})) {
		t.Errorf("reordered hosts: %+v", got)
	}
	if got := stateForm(testRuleOp, "r1", rendered, with("hosts", "  \r\n")).List("hosts", config.Some([]string{"x"})); got.Set {
		t.Errorf("emptied hosts: %+v", got)
	}
	// A list from repeated inputs of the same name.
	rv := with()
	rv["domains"] = []string{"c.example", "d.example"}
	if got := stateForm(testRuleOp, "r1", rendered, rv).List("domains", config.Opt[[]string]{}); !reflect.DeepEqual(got, config.Some([]string{"c.example", "d.example"})) {
		t.Errorf("domains from repeated inputs: %+v", got)
	}

	// Ports and integers compare as numbers; a value that is not one is an
	// issue of its entry and writes nothing.
	if got := stateForm(testRuleOp, "r1", rendered, with("ports", "0443")).Ports("ports", config.Some([]int64{8443})); !reflect.DeepEqual(got, config.Some([]int64{8443})) {
		t.Errorf("ports 0443: %+v", got)
	}
	g := stateForm(testRuleOp, "r1", rendered, with("ports", "443, https"))
	if g.Ports("ports", config.Some([]int64{443})); g.parseErrs != 1 || g.Errors("ports")[0] != `Entry 2: "https" must be an integer port` || g.Bad("ports") != "2" {
		t.Errorf("ports with a name: %d issues %q bad %q", g.parseErrs, g.Errors("ports"), g.Bad("ports"))
	}
	rem := map[string]string{"sources": "", "ports": "default", "ports_list": "", "default": "block", "allow_direct": "false", "max": "128"}
	remSame := func(kv ...string) url.Values {
		v := url.Values{}
		for k, x := range rem {
			v.Set(k, x)
		}
		for i := 0; i < len(kv); i += 2 {
			v.Set(kv[i], kv[i+1])
		}
		return v
	}
	if got := stateForm(testRemoteOp, "", rem, remSame("max", " 0128 ")).Int("max", config.Opt[int64]{}); got.Set {
		t.Errorf("max 0128 untouched: %+v", got)
	}
	if got := stateForm(testRemoteOp, "", rem, remSame("max", "")).Int("max", config.Some[int64](128)); got.Set {
		t.Errorf("max cleared: %+v", got)
	}
	h := stateForm(testRemoteOp, "", rem, remSame("max", "lots"))
	if h.Int("max", config.Some[int64](128)); h.parseErrs != 1 || h.Errors("max")[0] != "must be an integer" {
		t.Errorf("max lots: %q", h.Errors("max"))
	}

	// Checkboxes: untouched keeps unset and an explicit false; checked
	// sets; cleared unsets.
	if got := stateForm(testRuleOp, "r1", rendered, same).Check("allow_loopback", config.Opt[bool]{}); got.Set {
		t.Errorf("untouched checkbox set the key: %+v", got)
	}
	if got := stateForm(testRuleOp, "r1", rendered, same).Check("allow_loopback", config.Some(false)); got != config.Some(false) {
		t.Errorf("untouched checkbox dropped an explicit false: %+v", got)
	}
	if got := stateForm(testRuleOp, "r1", rendered, with("allow_loopback", "true")).Check("allow_loopback", config.Opt[bool]{}); got != config.Some(true) {
		t.Errorf("checked: %+v", got)
	}
	cleared := map[string]string{}
	for k, v := range rendered {
		cleared[k] = v
	}
	cleared["allow_loopback"] = "true"
	cv := with()
	cv.Del("allow_loopback") // an unchecked box posts nothing
	if got := stateForm(testRuleOp, "r1", cleared, cv).Check("allow_loopback", config.Some(true)); got.Set {
		t.Errorf("cleared checkbox: %+v", got)
	}

	// A choice maps by the table: "default" unsets the list although the
	// hidden list field still holds its entries.
	r0 := config.DocRule{Name: "r1", Action: "direct", Interfaces: config.Some([]string{"eth-a", "wifi"})}
	if got := editRule(r0, stateForm(testRuleOp, "r1", rendered, with("ifaces", "default"))); got.Interfaces.Set {
		t.Errorf("choice default kept the list: %+v", got.Interfaces)
	}
	if got := editRule(r0, stateForm(testRuleOp, "r1", rendered, with("ifaces_list", "wifi"))); !reflect.DeepEqual(got.Interfaces, config.Some([]string{"wifi"})) {
		t.Errorf("choice list changed: %+v", got.Interfaces)
	}
	// Untouched, the choice keeps what another writer wrote.
	other := r0
	other.Interfaces = config.Some([]string{"wifi", "eth-a"})
	if got := editRule(other, stateForm(testRuleOp, "r1", rendered, same)); !reflect.DeepEqual(got.Interfaces, other.Interfaces) {
		t.Errorf("untouched choice: %+v", got.Interfaces)
	}
	// A sub-field of an option that is not chosen is ignored: changing the
	// hidden list under "default" is no change.
	defRendered := map[string]string{}
	for k, v := range rendered {
		defRendered[k] = v
	}
	defRendered["ifaces"], defRendered["ifaces_list"] = "default", ""
	if f := stateForm(testRuleOp, "r1", defRendered, with("ifaces", "default", "ifaces_list", "eth-a")); f.touched("ifaces") {
		t.Error("a hidden sub-field counted as a change of its choice")
	}
	if _, changed := stateForm(testRuleOp, "r1", rendered, with("ifaces", "bogus")).Choice("ifaces"); changed {
		t.Error("an unknown option counted as a choice")
	}

	// A new entity writes its untouched prefilled fields; an empty field
	// stays unset.
	n := stateForm(testRuleOp, "", rendered, same)
	if !n.New {
		t.Fatal("a form without a target is not new")
	}
	got := editRule(config.DocRule{}, n)
	if got.Name != "r1" || got.Note != set("old note") || !reflect.DeepEqual(got.Hosts, config.Some([]string{"a.example", "b.example"})) ||
		got.Domains.Set || !reflect.DeepEqual(got.Ports, config.Some([]int64{443})) || got.AllowLoopback.Set ||
		!reflect.DeepEqual(got.Interfaces, config.Some([]string{"eth-a", "wifi"})) {
		t.Errorf("new rule: %+v", got)
	}

	// The hidden state carries the posted rendered values, not the
	// controls and not the file.
	carried := map[string]string{}
	for _, h := range stateForm(testRuleOp, "r1", rendered, with("hosts", "z.example", "note", "")).Hidden() {
		if k, ok := strings.CutPrefix(h.Name, "r:"); ok {
			carried[k] = h.Value
		}
	}
	if !reflect.DeepEqual(carried, rendered) {
		t.Errorf("carried rendered values:\n got %q\nwant %q", carried, rendered)
	}
}

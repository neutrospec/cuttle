package admin

import (
	"html"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/neutrospec/cuttle/internal/config"
)

// Test-only form ops that exercise the save pipeline of design 0009 D7
// the way the rule, upstream, and settings pages of tasks C and D will:
// a rule editor, a rule delete, a settings page with explicit choices,
// "Add to rule" of Route a host, and a reorder.

func boolText(o config.Opt[bool]) string {
	if o.Set && o.V {
		return "true"
	}
	return "false"
}

func intsText(o config.Opt[[]int64]) string {
	var out []string
	for _, n := range o.V {
		out = append(out, strconv.FormatInt(n, 10))
	}
	return strings.Join(out, "\n")
}

func intText(o config.Opt[int64]) string {
	if !o.Set {
		return ""
	}
	return strconv.FormatInt(o.V, 10)
}

var ruleFields = []formField{
	{Name: "name", Key: "name", Label: "Name", Kind: fieldText},
	{Name: "note", Key: "note", Label: "Note", Kind: fieldText},
	{Name: "hosts", Key: "hosts", Label: "Hosts", Kind: fieldList},
	{Name: "domains", Key: "domains", Label: "Domains", Kind: fieldList},
	{Name: "ports", Key: "ports", Label: "Ports", Kind: fieldPorts},
	{Name: "action", Key: "action", Label: "Action", Kind: fieldText},
	{Name: "allow_loopback", Key: "allow_loopback", Label: "Allow loopback", Kind: fieldCheck},
	{Name: "ifaces", Key: "interfaces", Label: "Interfaces", Kind: fieldChoice,
		Options: []choiceOption{{Value: "default"}, {Value: "only", Fields: []string{"ifaces_list"}}}},
	{Name: "ifaces_list", Key: "interfaces", Label: "Interfaces", Kind: fieldList, Under: "ifaces"},
}

func fieldNames(fs []formField) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Name
	}
	return out
}

func renderRule(d *config.Doc, target string) (map[string]string, bool) {
	r, i := d.Rule(target)
	if i < 0 {
		return nil, false
	}
	choice := "default"
	if r.Interfaces.Set {
		choice = "only"
	}
	return map[string]string{
		"name": r.Name, "note": r.Note.V, "hosts": strings.Join(r.Hosts.V, "\n"), "domains": strings.Join(r.Domains.V, "\n"),
		"ports": intsText(r.Ports), "action": r.Action, "allow_loopback": boolText(r.AllowLoopback),
		"ifaces": choice, "ifaces_list": strings.Join(r.Interfaces.V, "\n"),
	}, true
}

// editRule applies the rule form to r, the rule as the file has it.
func editRule(r config.DocRule, f *form) config.DocRule {
	r.Name = f.Required("name", r.Name)
	r.Note = f.Text("note", r.Note)
	r.Hosts = f.List("hosts", r.Hosts)
	r.Domains = f.List("domains", r.Domains)
	r.Ports = f.Ports("ports", r.Ports)
	r.Action = f.Required("action", r.Action)
	r.AllowLoopback = f.Check("allow_loopback", r.AllowLoopback)
	// The choice maps by the table: "default" unsets the list, whatever
	// the hidden list field holds.
	if opt, changed := f.Choice("ifaces"); changed {
		switch opt {
		case "default":
			r.Interfaces = config.Opt[[]string]{}
		case "only":
			r.Interfaces = config.Some(splitList(f.Value("ifaces_list")))
			if len(r.Interfaces.V) == 0 {
				r.Interfaces.V = []string{}
			}
		}
	}
	return r
}

var testRuleOp = &formOp{
	Name: "test.rule", Kind: config.KindRule, Page: "testform", Nav: "rules", PageTitle: "Rule",
	Fields: ruleFields,
	Render: renderRule,
	Edit: func(d *config.Doc, f *form) error {
		if f.New {
			return d.InsertRule(editRule(config.DocRule{}, f), 0)
		}
		cur, i := d.Rule(f.Target)
		if i < 0 {
			return config.ErrNotFound
		}
		return d.ReplaceRule(f.Target, editRule(cur, f))
	},
	Title: func(f *form) []seg {
		if f.New {
			return []seg{{Text: "add rule "}, {Text: f.Value("name"), Code: true}}
		}
		return []seg{{Text: "change rule "}, {Text: f.Target, Code: true}}
	},
	Hint: func(f *form) config.Hint {
		return config.Hint{Kind: config.KindRule, From: f.Target, To: f.Value("name")}
	},
	Entity: func(f *form) string { return f.Value("name") },
	View:   func(*config.Doc, *form) any { return fieldNames(ruleFields) },
}

var testDeleteOp = &formOp{
	Name: "test.rule.delete", Kind: config.KindRule, Page: "testform", Nav: "rules", PageTitle: "Rule", Delete: true,
	Fields: ruleFields,
	Render: renderRule,
	Edit:   func(d *config.Doc, f *form) error { return d.DeleteRule(f.Target) },
	Title:  func(f *form) []seg { return []seg{{Text: "delete rule "}, {Text: f.Target, Code: true}} },
	View:   func(*config.Doc, *form) any { return nil },
}

var remoteFields = []formField{
	{Name: "sources", Key: "proxy.remote.sources", Label: "Sources", Kind: fieldList},
	{Name: "ports", Key: "proxy.remote.ports", Label: "Ports", Kind: fieldChoice,
		Options: []choiceOption{{Value: "default"}, {Value: "these", Fields: []string{"ports_list"}}, {Value: "none"}}},
	{Name: "ports_list", Key: "proxy.remote.ports", Label: "Ports", Kind: fieldPorts, Under: "ports"},
	{Name: "default", Key: "proxy.remote.default", Label: "Default route", Kind: fieldText},
	{Name: "allow_direct", Key: "proxy.remote.allow_direct", Label: "Allow direct", Kind: fieldCheck},
	{Name: "max", Key: "proxy.remote.max_connections_per_source", Label: "Connections per source", Kind: fieldInt},
}

var testRemoteOp = &formOp{
	Name: "test.remote", Page: "testform", Nav: "remote", PageTitle: "Remote clients",
	Fields: remoteFields,
	Render: func(d *config.Doc, _ string) (map[string]string, bool) {
		r := d.Remote
		ports := "default"
		switch {
		case r.Ports.Set && len(r.Ports.V) == 0:
			ports = "none"
		case r.Ports.Set:
			ports = "these"
		}
		def := "block"
		if r.Default.Set {
			def = r.Default.V
		}
		return map[string]string{"sources": strings.Join(r.Sources.V, "\n"), "ports": ports, "ports_list": intsText(r.Ports),
			"default": def, "allow_direct": boolText(r.AllowDirect), "max": intText(r.MaxConnectionsPerSource)}, true
	},
	Edit: func(d *config.Doc, f *form) error {
		r := &d.Remote
		r.Sources = f.List("sources", r.Sources)
		if opt, changed := f.Choice("ports"); changed {
			switch opt {
			case "default":
				r.Ports = config.Opt[[]int64]{}
			case "none":
				r.Ports = config.Some([]int64{})
			case "these":
				r.Ports = f.PortsValue("ports_list")
				if !r.Ports.Set && f.parseErrs == 0 {
					f.Error("ports", 0, "List at least one port, or choose No ports")
				}
			}
		}
		if f.touched("default") {
			r.Default = config.Some(f.Value("default"))
		}
		r.AllowDirect = f.Check("allow_direct", r.AllowDirect)
		r.MaxConnectionsPerSource = f.Int("max", r.MaxConnectionsPerSource)
		return nil
	},
	Title: func(*form) []seg { return plain("change the remote client settings") },
	View:  func(*config.Doc, *form) any { return fieldNames(remoteFields) },
}

var addPatternFields = []formField{
	{Name: "domain", Key: "domains", Label: "Domain", Kind: fieldText},
	{Name: "action", Key: "action", Label: "Route", Kind: fieldText},
}

var testAddPatternOp = &formOp{
	Name: "test.addpattern", Kind: config.KindRule, Page: "testform", Nav: "rules", PageTitle: "Route a host",
	Fields: addPatternFields,
	Render: func(d *config.Doc, target string) (map[string]string, bool) {
		r, i := d.Rule(target)
		return map[string]string{"domain": "", "action": r.Action}, i >= 0
	},
	Edit: func(d *config.Doc, f *form) error {
		return d.AddPattern(f.Target, f.Value("action"), config.PatternDomains, f.Value("domain"))
	},
	Title:  func(f *form) []seg { return []seg{{Text: "add to rule "}, {Text: f.Target, Code: true}} },
	Entity: func(f *form) string { return f.Target },
	View:   func(*config.Doc, *form) any { return fieldNames(addPatternFields) },
}

var testOrderOp = &formOp{
	Name: "test.order", Page: "testform", Nav: "rules", PageTitle: "Rules",
	Fields: []formField{{Name: "order", Key: "rule", Label: "Order", Kind: fieldList}},
	Render: func(d *config.Doc, _ string) (map[string]string, bool) {
		var names []string
		for _, r := range d.Rules {
			names = append(names, r.Name)
		}
		return map[string]string{"order": strings.Join(names, "\n")}, true
	},
	Edit:  func(d *config.Doc, f *form) error { return d.ReorderRules(splitList(f.Value("order"))) },
	Title: func(*form) []seg { return plain("reorder the rules") },
	View:  func(*config.Doc, *form) any { return []string{"order"} },
}

// withTestOps registers the test ops and their page, and returns a
// handler that serves GET /test/<op>[/<target>] with the form as a GET
// renders it, ahead of the server's own routes.
func (f *fixture) withTestOps() http.Handler {
	f.t.Helper()
	src, err := os.ReadFile("testdata/testform.html")
	if err != nil {
		f.t.Fatal(err)
	}
	f.s.pages["testform"] = template.Must(template.Must(f.s.base.Clone()).Parse(string(src)))
	for _, op := range []*formOp{testRuleOp, testDeleteOp, testRemoteOp, testAddPatternOp, testOrderOp} {
		f.s.addOp(op)
	}
	mux := http.NewServeMux()
	mux.Handle("/", f.s)
	mux.Handle("GET /test/{op}/{target...}", f.s.authed(0, func(w http.ResponseWriter, r *http.Request, _ string, v session) {
		op := f.s.ops[r.PathValue("op")]
		file := f.s.readFile()
		if op == nil || !file.Valid() {
			http.NotFound(w, r)
			return
		}
		target := r.PathValue("target")
		vals := map[string]string{}
		if target != "" || op.Kind == "" {
			var ok bool
			if vals, ok = op.Render(file.Doc, target); !ok {
				http.NotFound(w, r)
				return
			}
		}
		fm := newFormFrom(op, vals, target, file.Digest, r.URL.Query().Get("return"))
		f.s.render(w, http.StatusOK, op.Page, pageData{Title: op.PageTitle, Token: v.csrf, Page: op.Nav,
			Data: &formPage{Form: fm, View: op.View(file.Doc, fm)}})
	}))
	return mux
}

var (
	hiddenRE   = regexp.MustCompile(`<input type="hidden" name="([^"]+)" value="([^"]*)">`)
	textareaRE = regexp.MustCompile(`(?s)<textarea id="[^"]*" name="([^"]+)"[^>]*>\n?(.*?)</textarea>`)
)

// formValues returns the fields of the first form in body whose start
// tag begins with start, as a browser would post them.
func formValues(t *testing.T, body, start string) url.Values {
	t.Helper()
	i := strings.Index(body, start)
	if i < 0 {
		t.Fatalf("no form %q in:\n%s", start, body)
	}
	body = body[i:]
	body = body[:strings.Index(body, "</form>")]
	v := url.Values{}
	for _, m := range hiddenRE.FindAllStringSubmatch(body, -1) {
		v.Add(html.UnescapeString(m[1]), html.UnescapeString(m[2]))
	}
	for _, m := range textareaRE.FindAllStringSubmatch(body, -1) {
		v.Add(html.UnescapeString(m[1]), html.UnescapeString(m[2]))
	}
	return v
}

// post posts form to path through h and returns the response and body.
func (b *browser) post(h http.Handler, path string, form url.Values, fragment bool) (*http.Response, string) {
	b.f.t.Helper()
	r := req{method: http.MethodPost, path: path, form: form}
	if fragment {
		r.header = http.Header{"Cuttle-Fragment": {"1"}}
	}
	res := b.doWith(h, r)
	return res, bodyOf(b.f.t, res)
}

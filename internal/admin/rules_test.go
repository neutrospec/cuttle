package admin

import (
	"html"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/neutrospec/cuttle/internal/config"
	"github.com/neutrospec/cuttle/internal/policy"
)

var (
	chainRowRE = regexp.MustCompile(`<li class="rule a-(\w+)( [\w ]+)?" data-name="([^"]+)"`)
	tagRE2     = regexp.MustCompile(`<[^>]+>`)
)

// chainRows returns the rows of the chain as name, action, and mark.
func chainRows(body string) [][3]string {
	var out [][3]string
	for _, m := range chainRowRE.FindAllStringSubmatch(body, -1) {
		out = append(out, [3]string{m[3], m[1], strings.TrimSpace(m[2])})
	}
	return out
}

func rowNames(body string) []string {
	var out []string
	for _, r := range chainRows(body) {
		out = append(out, r[0])
	}
	return out
}

// rowOf returns the markup of the chain row of rule name.
func rowOf(t *testing.T, body, name string) string {
	t.Helper()
	i := strings.Index(body, `data-name="`+name+`"`)
	if i < 0 {
		t.Fatalf("no row %q", name)
	}
	row := body[i:]
	return row[:strings.Index(row, "</li>")]
}

var inlineTagRE = regexp.MustCompile(`^</?(code|b|span|a|strong|em|i|svg|use|label)\b`)

// textOf returns the text of markup with tags removed and spaces folded:
// an inline tag goes without a trace, another tag becomes a space.
func textOf(s string) string {
	s = tagRE2.ReplaceAllStringFunc(s, func(tag string) string {
		if inlineTagRE.MatchString(tag) {
			return ""
		}
		return " "
	})
	return strings.Join(strings.Fields(html.UnescapeString(s)), " ")
}

// listConfig is exampleConfig with a fifth rule that has a note, ports,
// more than four patterns, and a pattern that rule ads already catches,
// which the validator warns about.
var listConfig = exampleConfig + `
[[rule]]
name = "late"
note = "Late <arrival> & co"
domains = ["tracker.example", "one.example", "two.example", "three.example", "four.example"]
ports = [443, 8443]
action = "direct"
`

// TestAdminRulesList checks the rules page of design 0009 D3.3 (W15):
// every rule in order with its action badge, patterns, ports, flags,
// warning badges, and 24-hour requests and bytes from RuleTotals,
// followed by the default row; ?q= filters by name, note, pattern, and
// action; the empty state; the page without statistics; and the
// read-only page of a file with errors, which shows the running rules.
func TestAdminRulesList(t *testing.T) {
	p := newPipeline(t)
	p.write(listConfig)
	body := p.get("/rules")
	checkMarkup(t, "rules", body, true, "/rules")
	rows := chainRows(body)
	want := [][3]string{{"loopback", "direct", ""}, {"restricted", "upstream", ""}, {"ads", "block", ""}, {"lab-net", "upstream", ""}, {"late", "direct", ""}}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("rows %q, want %q", rows, want)
	}
	for name, parts := range map[string][]string{
		"loopback":   {`<span class="node">1</span>`, `<span class="chip">localhost</span><span class="chip">127.0.0.0/8</span><span class="chip">::1/128</span>`, "allows loopback", "<b>1,204</b> req", "18.8 MiB", `<span class="badge direct">`},
		"restricted": {`<span class="node">2</span>`, `<span class="badge upstream"><span class="sw"></span>upstream <b>gateway</b></span>`, "<b>3,471</b> req"},
		"ads":        {`<span class="badge block">`, "<b>2,290</b> req", "0 B"},
		"late": {`<span class="node">5</span>`, `<div class="r-note">Late &lt;arrival&gt; &amp; co</div>`, `<span class="chip more">+1 more</span>`, "ports 443, 8443",
			`1 warning</a>`, `title="domains, entry 1: &#34;tracker.example&#34; repeats rule &#34;ads&#34;, which matches first, so it can never match"`,
			`href="/rules/new?from=late"`, `form="del-5"`},
	} {
		row := rowOf(t, body, name)
		for _, s := range parts {
			if !strings.Contains(row, s) {
				t.Errorf("row %s lacks %q:\n%s", name, s, row)
			}
		}
	}
	if n := strings.Count(body, "warning</a>"); n != 1 {
		t.Errorf("%d warning badges, want 1", n)
	}
	def := body[strings.Index(body, `<li class="rule default`):]
	def = def[:strings.Index(def, "</li>")]
	if got := textOf(def); !strings.Contains(got, "No rule matched The default route applies: local default, remote default. Local direct Remote block") || !strings.Contains(got, "10,784 req") {
		t.Errorf("default row: %q", got)
	}
	for _, s := range []string{"5 rules", "Traffic: last 24 hours", `<form id="order-form"`, `name="op" value="rules.order"`,
		`<input type="hidden" name="r:order" value="loopback`, `<a class="btn primary" href="/rules/new">`, `data-reorder`} {
		if !strings.Contains(body, s) {
			t.Errorf("page lacks %q", s)
		}
	}

	// A reorder that renders again keeps each warning badge on its rule.
	order := formByID(t, body, "order-form")
	order["order"] = []string{"late", "loopback", "restricted", "ads", "lab-net"}
	order["position"] = nil
	res, again := p.post("/config/draft", with(order, "intent", "edit"))
	if res.StatusCode != http.StatusOK || !slices.Equal(rowNames(again), order["order"]) ||
		!strings.Contains(rowOf(t, again, "late"), "1 warning</a>") || strings.Contains(rowOf(t, again, "lab-net"), "warning</a>") {
		t.Errorf("warning badges after a reorder: %d %q", res.StatusCode, rowNames(again))
	}

	// The filter: name, note, pattern, and action, in any case.
	for q, names := range map[string][]string{
		"ADS": {"ads"}, "arrival": {"late"}, "lab.example": {"lab-net"}, "198.51.100": {"lab-net"},
		"upstream:gateway": {"restricted"}, "block": {"ads"}, "example": {"restricted", "ads", "lab-net", "late"},
	} {
		body := p.get("/rules?q=" + url.QueryEscape(q))
		if got := rowNames(body); !slices.Equal(got, names) {
			t.Errorf("q=%s: rows %q, want %q", q, got, names)
		}
		if !strings.Contains(body, "rules match; reordering is off while filtering.") || strings.Contains(body, "data-reorder") ||
			strings.Contains(body, `<form id="order-form"`) && strings.Contains(body, `name="position"`) {
			t.Errorf("q=%s: reordering is offered while filtering", q)
		}
	}
	// The rule editor links the rule's hosts to /domains, filtered by
	// the rule (design 0009 D3.2, D9).
	if !strings.Contains(p.get("/rules/restricted"), `href="/domains?rule=restricted&amp;window=24h"`) {
		t.Error("the editor does not link the rule's hosts")
	}
	if got := hosts(p.get("/domains?rule=restricted&window=24h")); !slices.Equal(got, []string{"wiki.internal.example", "git.internal.example", "ci.build.example", "docs.internal.example"}) {
		t.Errorf("hosts of rule restricted: %q", got)
	}
	if body := p.get("/rules?q=nothing-matches"); !strings.Contains(body, "<h3>No rules match</h3>") || len(rowNames(body)) != 0 {
		t.Errorf("filter without a match:\n%s", mainText(body))
	}

	// No rules: the empty state with the defaults, and Add rule.
	p.write("name = \"cuttle-edge\"\n\n[proxy]\nlisten = [\"127.0.0.1:41728\"]\ndefault = \"block\"\n")
	body = p.get("/rules")
	if got := textOf(body[strings.Index(body, `<div class="empty section">`):]); !strings.HasPrefix(got,
		"No rules yet Every request takes the default route: block for local clients and block for remote clients.") ||
		!strings.Contains(body, `<a class="btn primary" href="/rules/new">`) || strings.Contains(body, `<ol class="chain`) {
		t.Errorf("empty state:\n%s", mainText(body))
	}
	checkMarkup(t, "rules without rules", body, true, "/rules")

	// Without statistics: no traffic, one muted line.
	p.write(listConfig)
	p.f.be.mu.Lock()
	p.f.be.st = nil
	p.f.be.mu.Unlock()
	body = p.get("/rules")
	if !strings.Contains(body, "Statistics unavailable") || strings.Contains(body, " req</div>") {
		t.Errorf("rules without statistics:\n%s", mainText(body))
	}

	// A file with errors: the running rules, read-only.
	p.write(strings.Replace(listConfig, `action = "block"`, `action = "sideways"`, 1))
	body = p.get("/rules")
	if got := rowNames(body); !slices.Equal(got, []string{"loopback", "restricted", "ads", "lab-net"}) {
		t.Errorf("read-only rows %q", got)
	}
	for _, s := range []string{`<form id="order-form"`, `href="/rules/new"`, `form="del-`, `data-reorder`, `name="position"`} {
		if strings.Contains(body, s) {
			t.Errorf("read-only page offers %q", s)
		}
	}
	if !strings.Contains(body, fileErrorsBanner) {
		t.Error("read-only page lacks the errors banner")
	}
	checkMarkup(t, "rules read-only", body, true, "/rules")
	if res := p.b.doWith(p.h, req{method: http.MethodGet, path: "/rules/restricted"}); res.StatusCode != http.StatusOK {
		t.Errorf("read-only editor: %d", res.StatusCode)
	} else if body := bodyOf(t, res); !strings.Contains(body, `<fieldset class="fsec" disabled>`) || strings.Contains(body, `data-primary`) {
		t.Errorf("read-only editor is not read-only")
	}
}

// ruleForm loads the rule editor at path and returns its form as a
// browser would post it.
func (p *pipeline) ruleForm(path string) url.Values {
	p.t.Helper()
	return formByID(p.t, p.get(path), "rule-form")
}

// docEdit returns the canonical form of the file with edit applied.
func (p *pipeline) docEdit(edit func(d *config.Doc)) string {
	p.t.Helper()
	return encodeWith(p.t, p.file(), edit)
}

// TestAdminRuleEdits checks the rule ops of design 0009 D7 through the
// pages' forms (W16): adding at each position, editing, changing the
// action, duplicating, deleting, and reordering with the script's order
// and with the position fields, each giving the expected candidate; a
// rename or delete started at /rules/{name} returns to /rules; a reorder
// changes nothing but the order; the delete review shows the rule's
// traffic and where it goes next.
func TestAdminRuleEdits(t *testing.T) {
	p := newPipeline(t)
	newRule := func(pos string) url.Values {
		return with(p.ruleForm("/rules/new"), "name", "media", "domains", "video.example\ncdn.video.example", "action", "block", "position", pos)
	}
	media := config.DocRule{Name: "media", Domains: config.Some([]string{"video.example", "cdn.video.example"}), Action: "block"}
	for pos, at := range map[string]int{"first": 0, "before:ads": 2, "last": 4} {
		body := p.review(newRule(pos), http.StatusOK)
		if want := p.docEdit(func(d *config.Doc) { d.InsertRule(media, at) }); applyForm(t, body).Get("text") != want {
			t.Errorf("new rule at %s:\n%s\nwant\n%s", pos, applyForm(t, mainText(body)).Get("text"), want)
		}
		if !strings.Contains(body, "Review: add rule <code>media</code>") {
			t.Errorf("new rule at %s: title:\n%s", pos, mainText(body))
		}
	}
	want := p.docEdit(func(d *config.Doc) { d.InsertRule(media, 4) })
	if res := p.apply(p.review(newRule("last"), http.StatusOK)); res.Header.Get("Location") != "/rules" || p.file() != want {
		t.Fatalf("apply of a new rule: %q\n%s", res.Header.Get("Location"), p.file())
	}
	// A position that names a rule that no longer exists.
	body := p.review(newRule("before:gone"), http.StatusUnprocessableEntity)
	if !strings.Contains(body, "rule &#34;gone&#34; no longer exists; choose another place") {
		t.Errorf("position of a rule that is gone:\n%s", mainText(body))
	}
	// An empty name: the review says it is required; a check stays quiet
	// about it and checks the rest.
	body = p.review(with(newRule("first"), "name", ""), http.StatusUnprocessableEntity)
	if !strings.Contains(body, `<a href="#f-name">Name: required</a>`) {
		t.Errorf("review without a name:\n%s", mainText(body))
	}
	res, body := p.fragment("/config/draft", with(newRule("first"), "name", "", "domains", "*.bad", "intent", "check"))
	if res.StatusCode != http.StatusUnprocessableEntity || strings.Contains(body, "required") || !strings.Contains(body, `data-issues-for="f-domains" data-bad="1" data-invalid`) {
		t.Errorf("check without a name: %d:\n%s", res.StatusCode, mainText(body))
	}

	// Other entries list the warnings that the edit causes, such as a
	// later rule that the new first rule now shadows, and not those that
	// the file has already.
	res, body = p.fragment("/config/draft", with(newRule("first"), "name", "fresh", "domains", "fresh.example", "hosts", "localhost", "intent", "check"))
	if others := textOf(body); res.StatusCode != http.StatusOK || !strings.Contains(others, `Rule loopback: hosts, entry 1: "localhost" repeats rule "fresh"`) ||
		strings.Contains(others, "has no interfaces") {
		t.Errorf("other entries of a check: %d: %q", res.StatusCode, others)
	}

	// Edit: domains and note; the editor returns to itself.
	form := with(p.ruleForm("/rules/restricted"), "domains", "internal.example\nbuild.example\ndocs.example.org", "note", "Gateway only")
	want = p.docEdit(func(d *config.Doc) {
		r := &d.Rules[1]
		r.Domains.V = append(r.Domains.V, "docs.example.org")
		r.Note = config.Some("Gateway only")
	})
	body = p.review(form, http.StatusOK)
	if !strings.Contains(body, "Rule <code>restricted</code>: note changed, domains &#43;1") || applyForm(t, body).Get("text") != want {
		t.Errorf("edit review: %s\n%s\nwant\n%s", summaryOf(mainText(body)), applyForm(t, body).Get("text"), want)
	}
	if res := p.apply(body); res.Header.Get("Location") != "/rules/restricted" || p.file() != want {
		t.Errorf("edit apply: %q", res.Header.Get("Location"))
	}
	// An untouched form changes nothing and offers no Apply (OI-0006
	// AA-R2).
	body = p.review(p.ruleForm("/rules/restricted"), http.StatusOK)
	if !strings.Contains(body, "No change to the configuration.") || strings.Contains(body, "Apply changes") {
		t.Errorf("untouched review:\n%s", mainText(body))
	}

	// The action: a direct rule with flags becomes a block rule without
	// them; a direct rule with an interface list becomes an upstream rule
	// without it.
	body = p.review(with(p.ruleForm("/rules/loopback"), "action", "block"), http.StatusOK)
	if got := applyForm(t, body).Get("text"); got != p.docEdit(func(d *config.Doc) {
		d.Rules[0].Action, d.Rules[0].AllowLoopback = "block", config.Opt[bool]{}
	}) {
		t.Errorf("direct to block:\n%s", got)
	}
	form = p.ruleForm("/rules/loopback")
	form.Del("allow_loopback")
	form.Set("allow_private", "true")
	form.Set("ifaces", "only")
	form.Set("ifaces_list", "wifi\neth-a")
	p.apply(p.review(form, http.StatusOK))
	d, _, _ := config.DocOf([]byte(p.file()), "x")
	if r := d.Rules[0]; r.AllowLoopback.Set || !r.AllowPrivate.V || !slices.Equal(r.Interfaces.V, []string{"wifi", "eth-a"}) {
		t.Fatalf("direct flags and list: %+v", r)
	}
	body = p.review(with(p.ruleForm("/rules/loopback"), "action", "upstream", "upstream", "lab"), http.StatusOK)
	nd, _, _ := config.DocOf([]byte(applyForm(t, body).Get("text")), "x")
	if r := nd.Rules[0]; r.Action != "upstream:lab" || r.AllowPrivate.Set || r.Interfaces.Set {
		t.Errorf("direct to upstream keeps direct keys: %+v", r)
	}

	// Duplicate: the new-rule form with the rule's fields and a free name.
	form = p.ruleForm("/rules/new?from=ads")
	if form.Get("name") != "ads-copy" || form.Get("domains") != "ads.example.net\ntracker.example\nmetrics.example.org\npixel.example.com" ||
		form.Get("action") != "block" || form.Get("op") != opRuleSave || form.Get("target") != "" {
		t.Errorf("duplicate form: %v", form)
	}
	body = p.review(form, http.StatusOK)
	ads, _ := d.Rule("ads")
	dup := ads
	dup.Name = "ads-copy"
	if got := applyForm(t, body).Get("text"); got != p.docEdit(func(d *config.Doc) { d.InsertRule(dup, 0) }) {
		t.Errorf("duplicate candidate:\n%s", got)
	}

	// Delete from the list: the review names the rule's traffic and where
	// its first pattern goes next; it returns to /rules.
	list := p.get("/rules")
	del := formByID(t, list, "del-3")
	if del.Get("op") != opRuleDelete || del.Get("target") != "ads" || del.Get("r:domains") == "" {
		t.Fatalf("delete form: %v", del)
	}
	body = p.review(del, http.StatusOK)
	for _, s := range []string{"Review: delete rule <code>ads</code>", "This rule decided 2,290 requests (0 B) in the last 24 hours.",
		"After the delete, requests for <code>ads.example.net</code> take the default route, <code>proxy.default</code>: <code>direct</code>."} {
		if !strings.Contains(body, s) {
			t.Errorf("delete review lacks %q:\n%s", s, mainText(body))
		}
	}
	if strings.Contains(body, "Back to editing") {
		t.Error("a delete review offers Back to editing")
	}
	// A delete answers only a review.
	if res, _ := p.post("/config/draft", with(del, "intent", "check")); res.StatusCode != http.StatusBadRequest {
		t.Errorf("check of a delete: %d", res.StatusCode)
	}
	want = p.docEdit(func(d *config.Doc) { d.DeleteRule("ads") })
	if res := p.apply(body); res.Header.Get("Location") != "/rules" || p.file() != want {
		t.Errorf("delete apply: %q", res.Header.Get("Location"))
	}
	// Delete and rename started at the editor return to the list.
	edit := p.get("/rules/lab-net")
	body = p.review(formByID(t, edit, "rule-delete"), http.StatusOK)
	if applyForm(t, body).Get("return") != "/rules/lab-net" {
		t.Errorf("delete from the editor returns to %q", applyForm(t, body).Get("return"))
	}
	if res := p.apply(body); res.Header.Get("Location") != "/rules" {
		t.Errorf("delete from the editor: %q", res.Header.Get("Location"))
	}
	body = p.review(with(p.ruleForm("/rules/restricted"), "name", "restricted-b"), http.StatusOK)
	if !strings.Contains(body, "Statistics keep traffic by rule name, so rule <code>restricted-b</code> shows no traffic") {
		t.Errorf("rename review:\n%s", mainText(body))
	}
	if res := p.apply(body); res.Header.Get("Location") != "/rules" {
		t.Errorf("rename from the editor: %q", res.Header.Get("Location"))
	}

	// Reorder with the script: the order of the rows, positions 1 to n.
	p.write(exampleConfig)
	order := formByID(t, p.get("/rules"), "order-form")
	if !slices.Equal(order["order"], []string{"loopback", "restricted", "ads", "lab-net"}) || !slices.Equal(order["position"], []string{"1", "2", "3", "4"}) {
		t.Fatalf("order form: %v", order)
	}
	scripted := with(order)
	scripted["order"] = []string{"ads", "loopback", "restricted", "lab-net"}
	body = p.review(scripted, http.StatusOK)
	if !strings.Contains(body, "Moved rule <code>ads</code> from 3 to 1") {
		t.Errorf("reorder review:\n%s", mainText(body))
	}
	reordered := applyForm(t, body).Get("text")
	if reordered != p.docEdit(func(d *config.Doc) { d.ReorderRules([]string{"ads", "loopback", "restricted", "lab-net"}) }) {
		t.Errorf("reorder candidate:\n%s", reordered)
	}
	sameRules(t, exampleConfig, reordered)
	// Without the script: the position fields. Each rule lands at the
	// position typed for it: a rule moved up goes before the unmoved rule
	// at that position, a rule moved down after it.
	plainOrder := with(order)
	for _, c := range []struct {
		pos  []string
		want string
	}{
		{[]string{"1", "2", "1", "4"}, "ads loopback restricted lab-net"},
		{[]string{"4", "2", "3", "4"}, "restricted ads lab-net loopback"},
		{[]string{"3", "2", "3", "4"}, "restricted ads loopback lab-net"},
		{[]string{"2", "2", "3", "4"}, "restricted loopback ads lab-net"},
		{[]string{"1", "2", "3", "1"}, "lab-net loopback restricted ads"},
		{[]string{"1", "2", "3", "2"}, "loopback lab-net restricted ads"},
		{[]string{"2", "1", "3", "4"}, "restricted loopback ads lab-net"},
		{[]string{"9", "2", "3", "4"}, "restricted ads lab-net loopback"},
	} {
		plainOrder["position"] = c.pos
		got := applyForm(t, p.review(plainOrder, http.StatusOK)).Get("text")
		if want := p.docEdit(func(d *config.Doc) { d.ReorderRules(strings.Fields(c.want)) }); got != want {
			t.Errorf("positions %v: want %s", c.pos, c.want)
		}
	}
	plainOrder["position"] = []string{"4", "3", "2", "1"}
	got := applyForm(t, p.review(plainOrder, http.StatusOK)).Get("text")
	if got != p.docEdit(func(d *config.Doc) { d.ReorderRules([]string{"lab-net", "ads", "restricted", "loopback"}) }) {
		t.Errorf("reverse by positions:\n%s", got)
	}
	sameRules(t, exampleConfig, got)
	if res := p.apply(p.review(plainOrder, http.StatusOK)); res.Header.Get("Location") != "/rules" || p.file() != got {
		t.Errorf("reorder apply: %q", res.Header.Get("Location"))
	}
	// A position that is not a number: 422 before the validator; a list
	// that does not name every rule once: 409.
	p.write(exampleConfig)
	order = formByID(t, p.get("/rules"), "order-form")
	bad := with(order)
	bad["position"] = []string{"1", "x", "3", "4"}
	if body := p.review(bad, http.StatusUnprocessableEntity); !strings.Contains(body, "the position of rule &#34;restricted&#34; must be a whole number from 1") {
		t.Errorf("bad position:\n%s", mainText(body))
	}
	for name, names := range map[string][]string{"missing": {"ads", "loopback", "restricted"}, "repeated": {"ads", "ads", "restricted", "lab-net"}} {
		bad := with(order)
		bad["order"], bad["position"] = names, nil
		if res, body := p.post("/config/draft", with(bad, "intent", "review")); res.StatusCode != http.StatusConflict ||
			!strings.Contains(body, "The order does not name every rule exactly once") {
			t.Errorf("order with a %s name: %d:\n%s", name, res.StatusCode, mainText(body))
		}
	}
	// Back to editing without the script renders the list in the new
	// order, with the rendered order kept.
	res, body = p.post("/config/draft", with(scripted, "intent", "edit"))
	if res.StatusCode != http.StatusOK || !slices.Equal(rowNames(body), scripted["order"]) || !strings.Contains(body, `name="r:order" value="loopback`) {
		t.Errorf("back to editing the order: %d %q", res.StatusCode, rowNames(body))
	}
	checkMarkup(t, "order back to editing", body, true, "/rules")
	if p.file() != exampleConfig {
		t.Fatal("a review changed the file")
	}
}

// sameRules fails unless the rules of a and b are the same apart from
// their order.
func sameRules(t *testing.T, a, b string) {
	t.Helper()
	da, _, _ := config.DocOf([]byte(a), "a")
	db, _, _ := config.DocOf([]byte(b), "b")
	if da == nil || db == nil || len(da.Rules) != len(db.Rules) {
		t.Fatal("rules differ in number")
	}
	for _, r := range da.Rules {
		o, i := db.Rule(r.Name)
		if i < 0 || !reflect.DeepEqual(r, o) {
			t.Errorf("rule %s changed: %+v → %+v", r.Name, r, o)
		}
	}
	for _, name := range []string{"Name", "Proxy", "Remote", "Admin", "Stats", "Log", "Interfaces", "Upstreams"} {
		if !reflect.DeepEqual(reflect.ValueOf(*da).FieldByName(name).Interface(), reflect.ValueOf(*db).FieldByName(name).Interface()) {
			t.Errorf("a reorder changed %s", name)
		}
	}
}

// fillDoc sets every key of a model value: strings and the values of
// optional keys to a value that Encode writes, nested tables in place, and
// one entry of each array of tables.
func fillDoc(v reflect.Value) {
	switch v.Kind() {
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int64:
		v.SetInt(1)
	case reflect.Slice:
		e := reflect.New(v.Type().Elem()).Elem()
		fillDoc(e)
		v.Set(reflect.Append(reflect.MakeSlice(v.Type(), 0, 1), e))
	case reflect.Struct:
		for i := range v.NumField() {
			fillDoc(v.Field(i))
		}
	}
}

// schemaPaths returns every key of the 0004 D3 schema as the canonical
// writer writes it for a model with every key set: "name",
// "proxy.listen", "proxy.remote.ports", "rule.domains", and so on, with
// an array of tables named by its key. The model mirrors the schema, which
// the round-trip tests of design 0009 W1 hold.
func schemaPaths(t *testing.T) []string {
	t.Helper()
	d := &config.Doc{}
	fillDoc(reflect.ValueOf(d).Elem())
	var raw map[string]any
	if _, err := toml.Decode(string(d.Encode()), &raw); err != nil {
		t.Fatal(err)
	}
	var out []string
	var walk func(prefix string, m map[string]any)
	walk = func(prefix string, m map[string]any) {
		for k, v := range m {
			switch x := v.(type) {
			case map[string]any:
				walk(prefix+k+".", x)
			case []map[string]any:
				walk(prefix+k+".", x[0])
			default:
				out = append(out, prefix+k)
			}
		}
	}
	walk("", raw)
	slices.Sort(out)
	return out
}

// pageOf returns the op of the page that the field catalogue of design
// 0009 D4 names for a key path.
func pageOf(path string) string {
	for _, p := range [][2]string{{"rule.", opRuleSave}, {"upstream.", opUpstreamSave}, {"interface.", "interface.save"},
		{"proxy.remote.", "remote.save"}, {"proxy.", "proxy.save"}} {
		if strings.HasPrefix(path, p[0]) {
			return p[1]
		}
	}
	return "general.save"
}

// TestFormCoverage checks W17 and W22 of design 0009 (and OI-0006
// AB-R9): every key of the 0004 D3 schema, note included, is controlled by
// exactly one form field, on the page that the D4 table names, and no
// field controls anything else. A choice controls its own key and the keys
// of its options' sub-fields. The rule and upstream editors also render a
// control for each of their fields.
func TestFormCoverage(t *testing.T) {
	f := newFixture(t, true)
	f.withExample()
	paths := schemaPaths(t)
	if len(paths) < 40 || !slices.Contains(paths, "rule.note") || !slices.Contains(paths, "proxy.remote.ports") ||
		!slices.Contains(paths, "interface.kind") {
		t.Fatalf("schema paths: %q", paths)
	}
	controls := map[string][]string{}
	for _, name := range []string{opRuleSave, opUpstreamSave, "interface.save", "proxy.save", "remote.save", "general.save"} {
		op := f.s.ops[name]
		if op == nil {
			t.Fatalf("op %s is not registered", name)
		}
		prefix := ""
		if op.Kind != "" {
			prefix = op.Kind + "."
		}
		for _, fd := range op.Fields {
			if fd.Under != "" {
				continue
			}
			keys := []string{fd.Key}
			for _, o := range fd.Options {
				for _, sub := range o.Fields {
					sd, _ := op.field(sub)
					keys = append(keys, sd.Key)
				}
			}
			slices.Sort(keys)
			for _, k := range slices.Compact(keys) {
				if k != "" {
					controls[prefix+k] = append(controls[prefix+k], name+"/"+fd.Name)
				}
			}
		}
	}
	for _, key := range paths {
		if c := controls[key]; len(c) != 1 || !strings.HasPrefix(c[0], pageOf(key)+"/") {
			t.Errorf("%s: controlled by %q, want one field of %s", key, c, pageOf(key))
		}
	}
	for key, c := range controls {
		if !slices.Contains(paths, key) {
			t.Errorf("%q controls %s, which is not a key of the schema", c, key)
		}
	}
	b := f.browser()
	b.login()
	for _, c := range []struct{ op, page, nav, path string }{
		{opRuleSave, "rule", "rules", "/rules/new"},
		{opUpstreamSave, "upstream", "upstreams", "/upstreams/new"},
	} {
		op := f.s.ops[c.op]
		if op.Page != c.page || op.Nav != c.nav {
			t.Errorf("%s: page %s, nav %s", c.op, op.Page, op.Nav)
		}
		body := bodyOf(t, b.get(c.path))
		for _, fd := range op.Fields {
			// A list of the interface list editor is its Add select while
			// it holds no entry.
			if !regexp.MustCompile(`<(input|textarea|select)\b[^>]* name="(add:)?`+fd.Name+`"`).MatchString(body) && fd.Name != "upstream" {
				t.Errorf("%s: no control for field %s", c.path, fd.Name)
			}
		}
	}
}

// testURLConfig has a direct rule over loopback addresses without
// allow_loopback, an upstream rule, and a direct rule with its own
// interface list, for TestAdminTestURL.
const testURLConfig = `name = "cuttle-test"

[proxy]
listen = ["127.0.0.1:41728", "192.168.1.2:3128"]
default = "direct"
interfaces = ["eth-a"]

[proxy.remote]
sources = ["192.168.1.0/24"]
ports = [443]
allow_direct = true
default = "direct"

[admin]
listen = "127.0.0.1:41729"

[[interface]]
name = "eth-a"
mac = "00:00:5e:00:53:0a"

[[interface]]
name = "wifi"
device = "en0"
kind = "wifi"

[[upstream]]
name = "gateway"
address = "192.0.2.6:3128"
disclosure = "via"

[[rule]]
name = "local-net"
cidrs = ["127.0.0.0/8"]
action = "direct"

[[rule]]
name = "restricted"
domains = ["internal.example"]
action = "upstream:gateway"

[[rule]]
name = "video"
domains = ["video.example"]
action = "direct"
interfaces = ["wifi"]
`

var resultRE = regexp.MustCompile(`(?s)<div class="test-res" data-block="rules-test"([^>]*)>(.*?)</div></div>`)

// TestAdminTestURL checks Test a URL of design 0009 D8 (W18) on the rules
// page: for a table of targets and both client classes, the result shows
// the fields of config.Explain, the same that cuttle explain prints, and
// the literal-address cases read "refused" with their class; a bare host
// is read as https://<host> with a note; a target that cuttle explain
// refuses gets its message; the chain marks the rows; the script's
// fragment names the match; a test of an unsaved order runs against that
// order; and the editor's test of a draft that an earlier rule shadows
// says so.
func TestAdminTestURL(t *testing.T) {
	p := newPipeline(t)
	p.write(testURLConfig)
	cfg, r := config.Parse([]byte(testURLConfig), "x")
	if cfg == nil {
		t.Fatalf("invalid sample: %v", r.Errors)
	}
	for _, c := range []struct {
		target, source, head string
		refused              bool
	}{
		{"wiki.internal.example:8443", "local", "Rule 2, restricted", false},
		{"http://video.example/watch", "local", "Rule 3, video", false},
		{"https://example.com/", "local", "default: no rule matched; proxy.default applies", false},
		{"https://example.com:8443/", "local", "default: no rule matched; proxy.default applies", false},
		{"198.51.100.7:443", "local", "default: no rule matched; proxy.default applies", false},
		{"[2001:db8::1]:443", "local", "default: no rule matched; proxy.default applies", false},
		{"wiki.internal.example:22", "remote", "port: port 22 is not in proxy.remote.ports, so no rule is tried", false},
		{"http://169.254.169.254/", "local", "default: no rule matched; proxy.default applies", true},
		{"127.0.0.1:8080", "local", "Rule 1, local-net", true},
		{"10.0.0.1:443", "remote", "remote-default: no rule matched; proxy.remote.default applies", true},
		{"10.0.0.1:443", "local", "default: no rule matched; proxy.default applies", false},
	} {
		src := policy.SourceLocal
		if c.source == "remote" {
			src = policy.SourceRemote
		}
		e, err := config.Explain(cfg, c.target, src)
		if err != nil {
			t.Fatal(err)
		}
		body := p.get("/rules?" + url.Values{"test": {c.target}, "source": {c.source}}.Encode())
		m := resultRE.FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("%s: no result:\n%s", c.target, mainText(body))
		}
		got := textOf(m[2])
		for _, want := range []string{c.head, "Target " + e.Target.String() + " (", e.Source.String() + " client"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s (%s): result %q lacks %q", c.target, c.source, got, want)
			}
		}
		if e.Address != nil && !strings.Contains(got, "Address "+e.AddressText()) || e.Address == nil && strings.Contains(got, "Address ") {
			t.Errorf("%s (%s): address in %q, want %q", c.target, c.source, got, e.AddressText())
		}
		if e.Action != policy.ActionBlock && !strings.Contains(got, "Interfaces "+e.InterfacesText()) {
			t.Errorf("%s (%s): interfaces in %q, want %q", c.target, c.source, got, e.InterfacesText())
		}
		if refused := strings.Contains(m[2], `<dd class="refused">refused: `); refused != c.refused {
			t.Errorf("%s (%s): refused %v, want %v: %q", c.target, c.source, refused, c.refused, got)
		}
		// The chain: rows before the match passed, the match matched, the
		// rest not reached; the default row when no rule matched.
		for i, row := range chainRows(body) {
			want := "passed"
			switch {
			case e.Rule == policy.RulePort || (e.Matched > 0 && i+1 > e.Matched):
				want = "unreached"
			case i+1 == e.Matched:
				want = "matched"
			}
			if row[2] != want {
				t.Errorf("%s (%s): row %s is %q, want %q", c.target, c.source, row[0], row[2], want)
			}
		}
		if dflt := strings.Contains(body, `<li class="rule default matched`); dflt != (e.Matched == 0 && e.Rule != policy.RulePort) {
			t.Errorf("%s (%s): default row matched %v", c.target, c.source, dflt)
		}
	}
	// The upstream's address, disclosure, and live state.
	body := p.get("/rules?test=" + url.QueryEscape("https://wiki.internal.example/"))
	if got := textOf(resultRE.FindStringSubmatch(body)[2]); !strings.Contains(got, "Upstream 192.0.2.6:3128 disclosure via Up since 11:11") {
		t.Errorf("upstream in the result: %q", got)
	}
	// A bare host and a refused target.
	if got := textOf(resultRE.FindStringSubmatch(p.get("/rules?test=Example.COM"))[2]); !strings.Contains(got, "Target example.com:443 (tunnel, local client)") ||
		!strings.Contains(got, "Read as https://Example.COM.") {
		t.Errorf("bare host: %q", got)
	}
	if got := textOf(resultRE.FindStringSubmatch(p.get("/rules?test=" + url.QueryEscape("ftp://example.com/")))[2]); got != "TARGET must be host:port, an http:// URL, or an https:// URL" {
		t.Errorf("refused target: %q", got)
	}
	// Markup in the input is escaped where the page repeats it.
	body = p.get("/rules?test=" + url.QueryEscape(`"><script>alert(1)</script>`))
	if strings.Contains(body, "<script>alert") {
		t.Error("the test input is not escaped")
	}
	// The script's fragment names the match.
	res := p.b.doWith(p.h, req{method: http.MethodGet, path: "/rules?test=127.0.0.1:80", header: http.Header{"Cuttle-Fragment": {"1"}}})
	if body := bodyOf(t, res); !strings.HasPrefix(body, `<div class="test-res" data-block="rules-test" data-match="local-net" data-action="direct">`) {
		t.Errorf("test fragment:\n%s", mainText(body))
	}
	// A test of an unsaved order runs against that order.
	order := formByID(t, p.get("/rules"), "order-form")
	order["order"] = []string{"video", "restricted", "local-net"}
	res2, body := p.fragment("/config/draft", with(order, "intent", "test", "test", "127.0.0.1:80"))
	if res2.StatusCode != http.StatusOK || !strings.Contains(body, `data-match="local-net"`) || !strings.Contains(textOf(body), "Rule 3, local-net") {
		t.Errorf("test of an unsaved order: %d:\n%s", res2.StatusCode, mainText(body))
	}

	// The editor: a draft that an earlier rule shadows.
	form := with(p.ruleForm("/rules/new"), "name", "loop-block", "cidrs", "127.0.0.0/16", "action", "block", "position", "last",
		"test", "http://127.0.0.1:3000/", "source", "local")
	res2, body = p.fragment("/config/draft", with(form, "intent", "test"))
	got := textOf(body)
	if res2.StatusCode != http.StatusOK || !strings.Contains(got, "This rule matches") || !strings.Contains(got, "Rule 1, local-net, matches first.") ||
		!strings.Contains(got, "refused: loopback address, 502 destination_ip_prohibited") {
		t.Errorf("editor test of a shadowed draft: %d: %q", res2.StatusCode, got)
	}
	res2, body = p.fragment("/config/draft", with(form, "intent", "test", "test", "https://example.com/"))
	if got := textOf(body); !strings.Contains(got, "This rule does not match") || strings.Contains(got, "matches first") {
		t.Errorf("editor test of another target: %q", got)
	}
	res2, body = p.fragment("/config/draft", with(form, "intent", "test", "cidrs", "not-a-cidr"))
	if !strings.Contains(textOf(body), "Fix the errors to test this draft.") {
		t.Errorf("editor test of a draft with errors: %q", textOf(body))
	}
	// Without the script, Test renders the editor with the result.
	res2, body = p.post("/config/draft", with(form, "intent", "test"))
	if res2.StatusCode != http.StatusOK || !strings.Contains(body, "<h1>Add rule</h1>") || !strings.Contains(body, "matches first.") {
		t.Errorf("editor test without the script: %d", res2.StatusCode)
	}
	checkMarkup(t, "editor test", body, true, "/rules")
	if p.file() != testURLConfig {
		t.Fatal("a test changed the file")
	}
}

// summaryOf returns the change summary of a review as text.
func summaryOf(body string) string {
	i := strings.Index(body, `<ul class="summary">`)
	if i < 0 {
		return "(no summary)"
	}
	return textOf(body[i : i+strings.Index(body[i:], "</ul>")])
}

// mainText returns the text of a page's main content, for messages.
func mainText(body string) string {
	if i := strings.Index(body, `<main id="main"`); i >= 0 {
		body = body[i:]
	}
	return textOf(body)
}

// TestAdminEditorInterfaceLists checks W21 of design 0009 on the real
// rule and upstream editors (OI-0006 AB-R10): task D's interface list
// editor adds, moves, and removes entries without the script, each
// button posting the form for editing with the change in the query, and
// as the script posts the list; "Only these interfaces" with the list
// sets the key, and "Use the default list" or "System routing" unsets it.
func TestAdminEditorInterfaceLists(t *testing.T) {
	p := newPipeline(t)
	for _, c := range []struct {
		path, form, unset, key string
		entity                 func(d *config.Doc) config.Opt[[]string]
	}{
		{"/rules/loopback", "rule-form", "default", "rule", func(d *config.Doc) config.Opt[[]string] { return d.Rules[0].Interfaces }},
		{"/upstreams/lab", "upstream-form", "system", "upstream", func(d *config.Doc) config.Opt[[]string] { return d.Upstreams[1].Interfaces }},
	} {
		p.write(exampleConfig)
		edit := func(form url.Values, do, at string, kv ...string) url.Values {
			t.Helper()
			q := url.Values{"list": {"ifaces_list"}, "do": {do}, "at": {at}}
			res, body := p.post("/config/draft?"+q.Encode(), with(form, append([]string{"intent", "edit"}, kv...)...))
			if res.StatusCode != http.StatusOK {
				t.Fatalf("%s: %s %s: %d: %s", c.path, do, at, res.StatusCode, mainText(body))
			}
			if !strings.Contains(body, `data-iflist="ifaces_list"`) || !strings.Contains(body, `<button type="submit" class="sr default-btn"`) {
				t.Fatalf("%s: the editor lacks the list editor or the default button", c.path)
			}
			return formByID(t, body, c.form)
		}
		list := func(form url.Values) string {
			return strings.Join(splitList(strings.Join(form["ifaces_list"], "\n")), ",")
		}
		page := p.get(c.path)
		checkMarkup(t, c.path, page, true, "/"+c.key+"s")
		form := formByID(t, page, c.form)
		if form.Get("ifaces") != c.unset || list(form) != "" {
			t.Fatalf("%s: form %v", c.path, form)
		}
		form = edit(form, "add", "", "ifaces", "only", "add:ifaces_list", "wifi")
		form = edit(form, "add", "", "add:ifaces_list", "eth-a")
		if got := list(form); got != "wifi,eth-a" || form.Get("ifaces") != "only" {
			t.Fatalf("%s: Add: %q %v", c.path, got, form["ifaces"])
		}
		form = edit(form, "up", "2")
		if got := list(form); got != "eth-a,wifi" {
			t.Errorf("%s: Move up: %q", c.path, got)
		}
		form = edit(form, "down", "1")
		form = edit(form, "remove", "1")
		if got := list(form); got != "eth-a" {
			t.Errorf("%s: Move down, Remove: %q", c.path, got)
		}
		form = edit(form, "add", "", "add:ifaces_list", "wifi")
		body := p.review(form, http.StatusOK)
		d, _, _ := config.DocOf([]byte(applyForm(t, body).Get("text")), "x")
		if got := c.entity(d); !got.Set || strings.Join(got.V, ",") != "eth-a,wifi" {
			t.Errorf("%s: candidate list %+v", c.path, got)
		}
		p.apply(body)
		// As the script posts it: the hidden inputs in their new order.
		form = formByID(t, p.get(c.path), c.form)
		form["ifaces_list"] = []string{"wifi", "eth-a"}
		d, _, _ = config.DocOf([]byte(applyForm(t, p.review(form, http.StatusOK)).Get("text")), "x")
		if got := c.entity(d); strings.Join(got.V, ",") != "wifi,eth-a" {
			t.Errorf("%s: the script's order %+v", c.path, got)
		}
		// The choice that leaves the key unset, with the list still posted.
		form = with(formByID(t, p.get(c.path), c.form), "ifaces", c.unset)
		d, _, _ = config.DocOf([]byte(applyForm(t, p.review(form, http.StatusOK)).Get("text")), "x")
		if got := c.entity(d); got.Set {
			t.Errorf("%s: %s kept the list %+v", c.path, c.unset, got)
		}
		// Only these with an empty list: the validator's error, in place.
		form = formByID(t, p.get(c.path), c.form)
		form.Del("ifaces_list")
		body = p.review(form, http.StatusUnprocessableEntity)
		if slot := body[strings.Index(body, `id="f-ifaces-issues"`):]; !strings.HasPrefix(slot, `id="f-ifaces-issues"><p class="err">`) {
			t.Errorf("%s: an empty list: %s", c.path, mainText(body))
		}
	}
}

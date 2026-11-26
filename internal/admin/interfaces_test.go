package admin

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/neutrospec/cuttle/internal/config"
)

// lockedBuffer is a log sink that tests read while the server writes.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// withLog sends the server's log to a buffer.
func (f *fixture) withLog() *lockedBuffer {
	buf := &lockedBuffer{}
	f.s.log = slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return buf
}

// macRE finds the test MACs in any case and with any separator or none.
var macRE = regexp.MustCompile(`(?i)00[^0-9a-z]?00[^0-9a-z]?5e[^0-9a-z]?00[^0-9a-z]?53`)

const editorStart = `<form method="post" action="/config/draft" class="if-form"`

// TestAdminInterfaces checks W20 of design 0009: with an injected
// interface table, the Interfaces page lists the detected rows with
// their matches and Define… for the others, and the definitions with
// their states, identity, and users; Define… prefills the D3.6 identity
// (device and kind for Wi-Fi, the hardware address otherwise) and
// ?device= fills nothing unless it names a detected BSD name; an
// untouched Define… form for a Wi-Fi row writes device and kind; a rename
// updates every list; a used definition offers no Delete and its delete
// op is refused, an unused one is deleted; the page reports a table it
// cannot read; and no log line of these flows holds a MAC.
func TestAdminInterfaces(t *testing.T) {
	p := newPipeline(t)
	p.f.withLinks()
	logs := p.f.withLog()

	body := p.get("/interfaces")
	for _, s := range []string{
		// Detected rows with their matches, and Define… for the others.
		`<td class="host lead-cell">en0</td>`, `<a class="badge" href="/interfaces/wifi">wifi</a>`,
		`<a class="badge" href="/interfaces/eth-a">eth-a</a>`,
		`<a class="btn sm" href="/interfaces/new?device=en8" data-fragment="editor-slot">`,
		`<a class="btn sm" href="/interfaces/new?device=en3" data-fragment="editor-slot">`,
		`<td class="host" data-label="Hardware address">00:00:5e:00:53:0b</td>`, `198.51.100.7`, `IPv4 primary`,
		// Definitions with state, identity, and users.
		`<span class="state online">Online</span>`, `<span>device <code>en0</code>, Wi-Fi</span>`, `<span>hardware address</span>`,
		`Used by <code>proxy.interfaces</code> position 1`, `Used by <code>proxy.interfaces</code> position 2`,
		`<div class="m-note">Used by proxy.interfaces. Remove it from those lists first.</div>`,
	} {
		if !strings.Contains(body, s) {
			t.Errorf("interfaces page lacks %q", s)
		}
	}
	for _, s := range []string{"lo0", "utun3", "fe80::1", `value="interface.delete"`} {
		if strings.Contains(body, s) {
			t.Errorf("interfaces page shows %q", s)
		}
	}
	// The definitions list says the identity in words, without the MAC.
	defs := body[strings.Index(body, `<ul class="ents">`):strings.Index(body, `id="detected-h"`)]
	if macRE.MatchString(defs) {
		t.Errorf("the definitions list shows a MAC:\n%s", defs)
	}
	// Refresh loads only the detected table.
	res := p.b.doWith(p.h, req{method: http.MethodGet, path: "/interfaces", header: http.Header{"Cuttle-Fragment": {"1"}}})
	frag := bodyOf(t, res)
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(frag, `<div class="stack" data-block="detected">`) || strings.Contains(frag, "<html") {
		t.Errorf("Refresh fragment: %d:\n%s", res.StatusCode, frag)
	}

	// Define… for a wired row: the hardware address, the next eth-<x>
	// name, and no kind guard; as a fragment, the editor block.
	form := p.pageForm("/interfaces/new?device=en8", editorStart)
	if got := []string{form.Get("name"), form.Get("ident"), form.Get("mac"), form.Get("device"), form.Get("kind")}; strings.Join(got, "|") !=
		"eth-b|mac|00:00:5e:00:53:0b|en8|" {
		t.Errorf("Define… en8: %q", got)
	}
	res = p.b.doWith(p.h, req{method: http.MethodGet, path: "/interfaces/new?device=en8", header: http.Header{"Cuttle-Fragment": {"1"}}})
	frag = bodyOf(t, res)
	if !strings.HasPrefix(frag, `<div class="editor-box in-dialog" data-block="editor">`) || !strings.Contains(frag, `<h2 id="editor-h">Define en8</h2>`) {
		t.Errorf("editor fragment:\n%s", frag)
	}
	// ?device= fills only a BSD name that the detected table holds.
	for _, dev := range []string{"en9", "EN8", "en8x", "en8;", "utun3", "lo0", "abcdefghijklmno1", "en8%0a", ""} {
		form := p.pageForm("/interfaces/new?device="+url.QueryEscape(dev), editorStart)
		if form.Get("name") != "" || form.Get("mac") != "" || form.Get("device") != "" {
			t.Errorf("?device=%q filled the form: %v", dev, form)
		}
	}

	// A rename updates every list, and returns to the list.
	withLists := encodeWith(t, exampleConfig, func(d *config.Doc) {
		d.Rules[0].Interfaces = config.Some([]string{"eth-a"})
		d.Upstreams[0].Interfaces = config.Some([]string{"wifi", "eth-a"})
	})
	p.write(withLists)
	rv := p.review(with(p.pageForm("/interfaces/eth-a", editorStart), "name", "eth-x"), http.StatusOK)
	cand := docOf(t, candidate(t, rv))
	if got := []string{strings.Join(cand.Proxy.Interfaces.V, ","), strings.Join(cand.Rules[0].Interfaces.V, ","),
		strings.Join(cand.Upstreams[0].Interfaces.V, ","), cand.Interfaces[0].Name}; strings.Join(got, "|") != "eth-x,wifi|eth-x|wifi,eth-x|eth-x" {
		t.Errorf("rename: %q", got)
	}
	if !strings.Contains(rv, "Review: change interface <code>eth-a</code>") {
		t.Errorf("rename review title:\n%s", rv)
	}
	res = p.apply(rv)
	if loc := res.Header.Get("Location"); loc != "/interfaces" {
		t.Errorf("after a rename: %q", loc)
	}

	// A used definition has no Delete, and its delete op is refused.
	body = p.get("/interfaces")
	if strings.Contains(body, `<input type="hidden" name="target" value="eth-x">`) {
		t.Errorf("a used definition offers Delete")
	}
	vals, _ := renderInterface(docOf(t, p.file()), "eth-x")
	del := url.Values{"csrf": {p.csrf}}
	for _, h := range newFormFrom(p.f.s.ops["interface.delete"], vals, "eth-x", config.Digest([]byte(p.file())), "/interfaces").Hidden() {
		del.Add(h.Name, h.Value)
	}
	res, body = p.post("/config/draft", with(del, "intent", "review"))
	if res.StatusCode != http.StatusConflict || !strings.Contains(body, "Interface &#34;eth-x&#34; is used by proxy.interfaces, rule `loopback`, upstream `gateway`") {
		t.Errorf("delete of a used definition: %d:\n%s", res.StatusCode, body)
	}

	// An untouched Define… form for the Wi-Fi row writes device and kind.
	p.write(encodeWith(t, exampleConfig, func(d *config.Doc) {
		d.Interfaces, d.Proxy.Interfaces = nil, config.Opt[[]string]{}
	}))
	body = p.get("/interfaces")
	if !strings.Contains(body, "No interfaces defined") || !strings.Contains(body, `href="/interfaces/new?device=en0"`) {
		t.Errorf("empty state:\n%s", body)
	}
	form = p.pageForm("/interfaces/new?device=en0", editorStart)
	if got := []string{form.Get("name"), form.Get("ident"), form.Get("kind")}; strings.Join(got, "|") != "wifi|device|wifi" {
		t.Errorf("Define… en0: %q", got)
	}
	rv = p.review(form, http.StatusOK)
	c := candidate(t, rv)
	if !strings.Contains(c, "[[interface]]\nname = \"wifi\"\ndevice = \"en0\"\nkind = \"wifi\"\n") || strings.Contains(c, "mac =") {
		t.Errorf("an untouched Define… form for Wi-Fi:\n%s", c)
	}
	p.apply(rv)
	// The other identities: the hardware address alone, and both.
	rv = p.review(p.pageForm("/interfaces/new?device=en8", editorStart), http.StatusOK)
	if c := candidate(t, rv); !strings.Contains(c, "name = \"eth-a\"\nmac = \"00:00:5e:00:53:0b\"\n\n") {
		t.Errorf("Define… en8:\n%s", c)
	}
	rv = p.review(with(p.pageForm("/interfaces/new?device=en8", editorStart), "ident", "both", "kind", "wired"), http.StatusOK)
	if c := candidate(t, rv); !strings.Contains(c, "name = \"eth-a\"\nmac = \"00:00:5e:00:53:0b\"\ndevice = \"en8\"\nkind = \"wired\"\n") {
		t.Errorf("Define… en8 with both:\n%s", c)
	}
	p.apply(rv)
	// An unused definition is deleted from its menu.
	body = p.get("/interfaces")
	start := strings.Index(body, `<input type="hidden" name="op" value="interface.delete">`)
	if start < 0 {
		t.Fatalf("no Delete for an unused definition:\n%s", body)
	}
	del = formValues(t, body[strings.LastIndex(body[:start], `<form`):], `<form method="post" action="/config/draft">`)
	rv = p.review(del, http.StatusOK)
	if !strings.Contains(rv, "Review: delete interface <code>wifi</code>") {
		t.Errorf("delete review:\n%s", rv)
	}
	p.apply(rv)
	if d := docOf(t, p.file()); len(d.Interfaces) != 1 || d.Interfaces[0].Name != "eth-a" {
		t.Errorf("after the delete: %+v", d.Interfaces)
	}

	// A table that cannot be read is reported; the definitions still show.
	p.f.be.mu.Lock()
	p.f.be.links, p.f.be.linksErr = nil, errors.New("route socket: operation not permitted")
	p.f.be.mu.Unlock()
	body = p.get("/interfaces")
	if !strings.Contains(body, "The interface table could not be read.") || !strings.Contains(body, "route socket: operation not permitted") ||
		!strings.Contains(body, `class="e-name" href="/interfaces/eth-a"`) {
		t.Errorf("unreadable table:\n%s", body)
	}

	if m := macRE.FindString(logs.String()); m != "" {
		t.Errorf("the log holds a MAC %q:\n%s", m, logs.String())
	}
	if !strings.Contains(logs.String(), "admin settings saved") {
		t.Errorf("the flows logged no save:\n%s", logs.String())
	}
}

// TestAdminInterfaceLists checks W21 of design 0009: the interface list
// editor adds, removes, and reorders entries of proxy.interfaces, of a
// rule's list, and of an upstream's list, both through its buttons
// without the script (POST /config/draft?list=… with intent=edit) and as
// the script posts them (the entries in order); an emptied
// proxy.interfaces, "default list" for a rule, and "system routing" for
// an upstream unset the key; and the editor draws its choice, its
// entries, and its buttons.
func TestAdminInterfaceLists(t *testing.T) {
	p := newPipeline(t)
	p.f.s.addOp(testUpstreamOp)
	// edit presses an editor button without the script and returns the
	// form as the page renders it again.
	edit := func(form url.Values, list, do, at string, kv ...string) url.Values {
		t.Helper()
		q := url.Values{"list": {list}, "do": {do}, "at": {at}}
		res, body := p.post("/config/draft?"+q.Encode(), with(form, append([]string{"intent", "edit"}, kv...)...))
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s %s: %d:\n%s", do, at, res.StatusCode, body)
		}
		return body2form(t, body)
	}
	listOf := func(form url.Values, name string) string {
		return strings.Join(splitList(strings.Join(form[name], "\n")), ",")
	}

	// proxy.interfaces, without the script.
	form := p.pageForm("/proxy", settingsFormStart)
	if got := listOf(form, "interfaces"); got != "eth-a,wifi" {
		t.Fatalf("proxy list: %q", got)
	}
	form = edit(form, "interfaces", "down", "1")
	if got := listOf(form, "interfaces"); got != "wifi,eth-a" {
		t.Errorf("Move down: %q", got)
	}
	form = edit(form, "interfaces", "up", "2")
	form = edit(form, "interfaces", "remove", "1")
	if got := listOf(form, "interfaces"); got != "wifi" {
		t.Errorf("Move up, Remove: %q", got)
	}
	form = edit(form, "interfaces", "add", "", "add:interfaces", "eth-a")
	if got := listOf(form, "interfaces"); got != "wifi,eth-a" {
		t.Errorf("Add: %q", got)
	}
	rv := p.review(form, http.StatusOK)
	if c := candidate(t, rv); !strings.Contains(c, `interfaces = ["wifi", "eth-a"]`) {
		t.Errorf("proxy list review:\n%s", c)
	}
	// Out-of-range buttons change nothing.
	if got := listOf(edit(form, "interfaces", "up", "1"), "interfaces"); got != "wifi,eth-a" {
		t.Errorf("Move up at the top: %q", got)
	}
	if got := listOf(edit(form, "interfaces", "remove", "9"), "interfaces"); got != "wifi,eth-a" {
		t.Errorf("Remove past the end: %q", got)
	}
	// As the script posts it, and emptied, which unsets the key.
	rv = p.review(with(p.pageForm("/proxy", settingsFormStart), "interfaces", "wifi\neth-a"), http.StatusOK)
	if c := candidate(t, rv); !strings.Contains(c, `interfaces = ["wifi", "eth-a"]`) {
		t.Errorf("proxy list as the script posts it:\n%s", c)
	}
	form = edit(edit(p.pageForm("/proxy", settingsFormStart), "interfaces", "remove", "1"), "interfaces", "remove", "1")
	rv = p.review(form, http.StatusOK)
	if c := candidate(t, rv); strings.Contains(c, "interfaces =") {
		t.Errorf("an emptied proxy.interfaces:\n%s", c)
	}

	// A rule's list (the test rule editor of ops_test.go): "only" with
	// entries added and moved, then "default", which unsets it.
	form = p.testForm("/test/test.rule/loopback?return=/rules")
	form = edit(with(form, "ifaces", "only"), "ifaces_list", "add", "", "add:ifaces_list", "wifi")
	form = edit(form, "ifaces_list", "add", "", "add:ifaces_list", "eth-a")
	form = edit(form, "ifaces_list", "up", "2")
	rv = p.review(form, http.StatusOK)
	if r := docOf(t, candidate(t, rv)).Rules[0]; strings.Join(r.Interfaces.V, ",") != "eth-a,wifi" {
		t.Errorf("rule list: %+v", r.Interfaces)
	}
	p.apply(rv)
	rv = p.review(with(p.testForm("/test/test.rule/loopback?return=/rules"), "ifaces", "default"), http.StatusOK)
	if r := docOf(t, candidate(t, rv)).Rules[0]; r.Interfaces.Set {
		t.Errorf("default list kept the rule's list: %+v", r.Interfaces)
	}
	// An upstream's list: "only" as the script posts it, then "system".
	rv = p.review(with(p.testForm("/test/test.upstream/gateway?return=/upstreams"), "ifaces", "only", "ifaces_list", "wifi"), http.StatusOK)
	if u := docOf(t, candidate(t, rv)).Upstreams[0]; strings.Join(u.Interfaces.V, ",") != "wifi" {
		t.Errorf("upstream list: %+v", u.Interfaces)
	}
	p.apply(rv)
	form = edit(p.testForm("/test/test.upstream/gateway?return=/upstreams"), "ifaces_list", "add", "", "add:ifaces_list", "eth-a")
	form = edit(form, "ifaces_list", "remove", "1")
	rv = p.review(form, http.StatusOK)
	if u := docOf(t, candidate(t, rv)).Upstreams[0]; strings.Join(u.Interfaces.V, ",") != "eth-a" {
		t.Errorf("upstream list after Add and Remove: %+v", u.Interfaces)
	}
	rv = p.review(with(p.testForm("/test/test.upstream/gateway?return=/upstreams"), "ifaces", "system"), http.StatusOK)
	if u := docOf(t, candidate(t, rv)).Upstreams[0]; u.Interfaces.Set {
		t.Errorf("system routing kept the upstream's list: %+v", u.Interfaces)
	}

	// The editor with a choice, as a rule or an upstream page draws it.
	d := docOf(t, p.file())
	op := p.f.s.ops["test.upstream"]
	vals, _ := op.Render(d, "gateway")
	f := newFormFrom(op, vals, "gateway", "", "/upstreams")
	v := f.IfList("ifaces_list", "ifaces", "only", p.f.s.ifStates(d), "Add at least one interface.")
	v.Choice.Options = []optionView{{Value: "system", Label: "System routing"}, {Value: "only", Label: "Only these interfaces, in this order", Checked: true}}
	var buf bytes.Buffer
	if err := p.f.s.base.ExecuteTemplate(&buf, "iflist", v); err != nil {
		t.Fatal(err)
	}
	html := buf.String()
	for _, s := range []string{
		`<input type="radio" name="ifaces" value="only" checked data-ifl-set>`,
		`<li data-n="wifi"><input type="hidden" name="ifaces_list" value="wifi">`,
		`formaction="/config/draft?list=ifaces_list&amp;do=up&amp;at=1" data-ifl="up" aria-label="Move wifi up" disabled>`,
		`formaction="/config/draft?list=ifaces_list&amp;do=remove&amp;at=1" data-ifl="remove" aria-label="Remove wifi">`,
		`<select class="input" name="add:ifaces_list" aria-label="Add an interface to the list" data-ifl-add><option value="">Add interface…</option><option value="eth-a"`,
		`<span class="state online">Online</span>`,
	} {
		if !strings.Contains(html, s) {
			t.Errorf("iflist lacks %q:\n%s", s, html)
		}
	}
}

// body2form returns the form of a rendered settings page or test form.
func body2form(t *testing.T, body string) url.Values {
	t.Helper()
	for _, start := range []string{settingsFormStart, `<form method="post" action="/config/draft" class="test-form">`} {
		if strings.Contains(body, start) {
			return pageForm(t, body, start)
		}
	}
	t.Fatalf("no form in:\n%s", body)
	return nil
}

// testUpstreamOp is a test-only upstream editor with the interface list
// of design 0009 D4: "System routing" unsets it, "Only these" sets it.
var testUpstreamOp = &formOp{
	Name: "test.upstream", Kind: config.KindUpstream, Page: "testform", Nav: "upstreams", PageTitle: "Upstream",
	Fields: []formField{
		{Name: "ifaces", Key: "interfaces", Label: "Interfaces", Kind: fieldChoice,
			Options: []choiceOption{{Value: "system"}, {Value: "only", Fields: []string{"ifaces_list"}}}},
		{Name: "ifaces_list", Key: "interfaces", Label: "Interfaces", Kind: fieldList, Under: "ifaces"},
	},
	Render: func(d *config.Doc, target string) (map[string]string, bool) {
		u, i := d.Upstream(target)
		choice := "system"
		if u.Interfaces.Set {
			choice = "only"
		}
		return map[string]string{"ifaces": choice, "ifaces_list": strings.Join(u.Interfaces.V, "\n")}, i >= 0
	},
	Edit: func(d *config.Doc, f *form) error {
		u, _ := d.Upstream(f.Target)
		if opt, changed := f.Choice("ifaces"); changed {
			switch opt {
			case "system":
				u.Interfaces = config.Opt[[]string]{}
			case "only":
				u.Interfaces = config.Some(append([]string{}, splitList(f.Value("ifaces_list"))...))
			}
		}
		return d.ReplaceUpstream(f.Target, u)
	},
	Title:  func(f *form) []seg { return []seg{{Text: "change upstream "}, {Text: f.Target, Code: true}} },
	Entity: func(f *form) string { return f.Target },
	View:   func(*config.Doc, *form) any { return []string{"ifaces", "ifaces_list"} },
}

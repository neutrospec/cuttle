package admin

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/neutrospec/cuttle/internal/config"
	"github.com/neutrospec/cuttle/internal/stats"
)

// routeForm loads Route a host for host and returns its form as a
// browser posts it untouched, and the page.
func (p *pipeline) routeForm(host string) (url.Values, string) {
	p.t.Helper()
	body := p.get("/rules/new?" + url.Values{"host": {host}, "return": {"/domains?window=24h"}}.Encode())
	return formByID(p.t, body, "route-form"), body
}

var (
	matchRE  = regexp.MustCompile(`<input type="radio" name="match" value="([^"]*)"( checked)?>`)
	actionRE = regexp.MustCompile(`<input type="radio" name="action" value="([^"]*)"( checked)?( disabled)?>`)
	whereRE  = regexp.MustCompile(`<option value="([^"]+)" data-route="([^"]+)"`)
)

// matchesOf lists the offered matches, the checked one marked with *.
func matchesOf(body string) string {
	var out []string
	for _, m := range matchRE.FindAllStringSubmatch(body, -1) {
		v := m[1]
		if m[2] != "" {
			v += "*"
		}
		out = append(out, v)
	}
	return strings.Join(out, ",")
}

// TestAdminRouteHost checks W25 of design 0009 (D3.12, D7, D12): Route…
// on the overview, domains, and live pages opens the prefilled form, as a
// page or as the dialog's block; it offers the exact host, preselected,
// each parent down to two labels, or the /32 or /128 of a literal, and
// shows the current decision with that route unavailable; a value that is
// not a host name or IP literal fills nothing; "Add to rule" lists only
// the rules of the chosen route without ports; the route.host op inserts
// a new first rule or adds the pattern to a rule, refuses a pattern that
// was not offered, and answers 409 when the rule's action or ports changed
// or it is gone (OI-0006 Y-R3); and /domains?rule= filters by rule with
// its select.
func TestAdminRouteHost(t *testing.T) {
	p := newPipeline(t)
	withPorts := exampleConfig + `
[[rule]]
name = "web-alt"
domains = ["alt.example"]
ports = [8443]
action = "block"
`
	p.write(withPorts)
	p.f.be.mu.Lock()
	p.f.be.cfg, _, _ = config.Load(p.f.be.path)
	p.f.be.mu.Unlock()

	// Route… on the three traffic pages, returning to the page.
	for _, c := range []struct{ path, want string }{
		{"/", `href="/rules/new?host=tracker.example&amp;return=%2F" data-fragment="route-editor"`},
		{"/domains?route=block", `href="/rules/new?host=tracker.example&amp;return=%2Fdomains%3Froute%3Dblock" data-fragment="route-editor" aria-label="Route tracker.example…"`},
		{"/live", `href="/rules/new?host=video.example&amp;return=%2Flive" data-fragment="route-editor"`},
	} {
		body := p.get(c.path)
		if !strings.Contains(body, c.want) || !strings.Contains(body, `<div id="route-editor" class="editor-slot">`) {
			t.Errorf("%s lacks Route… %s", c.path, c.want)
		}
	}

	// The matches, the current decision, and the routes.
	for _, c := range []struct{ host, want string }{
		{"cdn.video.example.com", "cdn.video.example.com*,video.example.com,example.com"},
		{"user.github.io", "user.github.io*,github.io"},
		{"example.com", "example.com*"},
		{"localhost", "localhost*"},
		{"CDN.Example.COM.", "cdn.example.com*,example.com"},
		{"198.51.100.7", "198.51.100.7/32*"},
		{"[2001:db8::1]", "2001:db8::1/128*"},
		{"::ffff:192.0.2.9", "192.0.2.9/32*"},
	} {
		_, body := p.routeForm(c.host)
		if got := matchesOf(body); got != c.want {
			t.Errorf("%s: matches %q, want %q", c.host, got, c.want)
		}
	}
	form, body := p.routeForm("wiki.internal.example")
	want := `<span class="v-lab">Currently</span> <span class="badge upstream"><span class="sw"></span>upstream <b>gateway</b></span> <span>by rule 2, <a class="name-code" href="/rules/restricted">restricted</a>, for HTTPS from a local client</span>`
	if !strings.Contains(body, want) || !strings.Contains(body, "<h1>Route <span class=\"name-code\">wiki.internal.example</span></h1>") {
		t.Errorf("current decision:\n%s", body)
	}
	var routes []string
	for _, m := range actionRE.FindAllStringSubmatch(body, -1) {
		routes = append(routes, m[1]+m[2]+m[3])
	}
	if got := strings.Join(routes, ","); got != "direct,upstream:gateway disabled,upstream:lab,block" {
		t.Errorf("routes %q: none chosen, the current one disabled", got)
	}
	if form.Get("action") != "" || form.Get("where") != "" || form.Get("host") != "wiki.internal.example" || form.Get("return") != "/domains?window=24h" {
		t.Errorf("form %v", form)
	}
	// "Add to rule" lists the rules without ports, by route; web-alt has
	// ports.
	var where []string
	for _, m := range whereRE.FindAllStringSubmatch(body, -1) {
		where = append(where, m[1]+"@"+m[2])
	}
	if got := strings.Join(where, ","); got != "direct loopback@direct,upstream:gateway restricted@upstream:gateway,upstream:lab lab-net@upstream:lab,block ads@block" {
		t.Errorf("Add to rule offers %q", got)
	}
	// As a fragment, the dialog's block.
	res := p.b.doWith(p.h, req{method: http.MethodGet, path: "/rules/new?host=wiki.internal.example&return=/", header: http.Header{"Cuttle-Fragment": {"1"}}})
	frag := bodyOf(t, res)
	if !strings.HasPrefix(frag, `<div class="up-editor route-editor" data-block="route-editor">`) || !strings.Contains(frag, `<h2 id="editor-h">Route`) || strings.Contains(frag, "<html") {
		t.Errorf("fragment:\n%s", frag)
	}

	// A value that is not a host name or IP literal fills nothing.
	for _, h := range []string{"*", "a b", "<script>", "http://x.example/", "x..example", "fe80::1%en0", "1.2.3", "x.example\n", "é.example"} {
		body := p.get("/rules/new?" + url.Values{"host": {h}}.Encode())
		if strings.Contains(body, `id="route-form"`) || !strings.Contains(body, "<h1>Add rule</h1>") || strings.Contains(body, "<script>") {
			t.Errorf("?host=%q filled the form", h)
		}
	}

	// A new rule, first in the chain, with the exact host; apply returns
	// to the page Route… started on.
	form, _ = p.routeForm("tracker.example")
	rv := p.review(with(form, "action", "direct"), http.StatusOK)
	d := docOf(t, candidate(t, rv))
	if r := d.Rules[0]; r.Name != "tracker.example" || strings.Join(r.Hosts.V, ",") != "tracker.example" || r.Action != "direct" || r.Domains.Set || len(d.Rules) != 6 {
		t.Errorf("new rule: %+v", r)
	}
	if !strings.Contains(rv, "Review: route <code>tracker.example</code> direct") ||
		!strings.Contains(rv, "After this change, requests for <code>tracker.example</code> from a local client take rule 1, <code>tracker.example</code>: <code>direct</code>.") {
		t.Errorf("review:\n%s", rv)
	}
	res = p.apply(rv)
	if loc := res.Header.Get("Location"); loc != "/domains?window=24h" {
		t.Errorf("after apply: %q", loc)
	}
	if body := p.get("/domains?window=24h"); !strings.Contains(body, "Saved and reloaded") {
		t.Errorf("no toast after Route a host:\n%s", body)
	}

	// A parent added to a rule of the chosen route; a later rule warns
	// that an earlier one still decides the host.
	form, _ = p.routeForm("img.cdn.example.org")
	rv = p.review(with(form, "action", "block", "match", "cdn.example.org", "where", "block ads"), http.StatusOK)
	if got := docOf(t, candidate(t, rv)).Rules[3]; got.Name != "ads" || strings.Join(got.Domains.V, ",") != "ads.example.net,tracker.example,metrics.example.org,pixel.example.com,cdn.example.org" {
		t.Errorf("Add to rule ads: %+v", got)
	}
	form, _ = p.routeForm("wiki.internal.example")
	rv = p.review(with(form, "action", "upstream:lab", "where", "upstream:lab lab-net"), http.StatusOK)
	if got := docOf(t, candidate(t, rv)).Rules[4]; got.Name != "lab-net" || !strings.Contains(strings.Join(got.Hosts.V, ","), "wiki.internal.example") {
		t.Errorf("Add to rule lab-net: %+v", got)
	}
	if !strings.Contains(rv, "take rule 3, <code>restricted</code>: <code>upstream:gateway</code>. That rule matches before the one you changed, so this change does not route the host through lab.") {
		t.Errorf("no shadow warning:\n%s", rv)
	}

	// Refusals: a pattern that was not offered, a missing route, a rule of
	// another route, and a forged host.
	before := p.file()
	form, _ = p.routeForm("cdn.video.example.com")
	for _, c := range []struct {
		kv   []string
		want string
	}{
		{[]string{"action", "block", "match", "com"}, "Match: choose one of the offered matches"},
		{[]string{"action", "block", "match", "other.example.com"}, "Match: choose one of the offered matches"},
		{[]string{"action", "block", "match", ""}, "Match: choose one of the offered matches"},
		{[]string{"action", ""}, "Route: choose where requests for this host go"},
		{[]string{"action", "sideways"}, "Route: choose one of the routes"},
		{[]string{"action", "direct", "where", "block ads"}, `Where: rule &#34;ads&#34; routes to block, not direct; choose a new rule or a rule that routes direct`},
		{[]string{"action", "block", "where", "block"}, "Where: choose a new rule or one of the offered rules"},
	} {
		_, body := p.post("/config/draft", with(form, append([]string{"intent", "review"}, c.kv...)...))
		if !strings.Contains(body, c.want) || strings.Contains(body, "review-form") {
			t.Errorf("%v: no %q:\n%s", c.kv, c.want, body)
		}
	}
	if res, _ := p.post("/config/draft", with(form, "intent", "review", "action", "block", "host", "*")); res.StatusCode != http.StatusBadRequest {
		t.Errorf("forged host: %d", res.StatusCode)
	}

	// The rule changed after the form was rendered (OI-0006 Y-R3): its
	// action, its ports, or the rule itself.
	form, _ = p.routeForm("cdn.video.example.com")
	form = with(form, "action", "block", "where", "block ads")
	for _, c := range []struct {
		name, file, want string
	}{
		{"action", strings.Replace(before, `action = "block"`, `action = "direct"`, 1), `Rule &#34;ads&#34;: the rule&#39;s action or ports changed; the file was changed outside this page.`},
		{"ports", strings.Replace(before, `action = "block"`, "action = \"block\"\nports = [443]", 1), `Rule &#34;ads&#34;: the rule&#39;s action or ports changed; the file was changed outside this page.`},
		{"gone", strings.Replace(before, `name = "ads"`, `name = "ads-2"`, 1), "Rule &#34;ads&#34; no longer exists; the file was changed outside this page."},
	} {
		p.write(c.file)
		for _, intent := range []string{"review", "edit"} {
			res, body := p.post("/config/draft", with(form, "intent", intent))
			if res.StatusCode != http.StatusConflict || !strings.Contains(body, c.want) {
				t.Errorf("%s changed, %s: %d:\n%s", c.name, intent, res.StatusCode, body)
			}
		}
		if p.file() != c.file {
			t.Errorf("%s: a refused draft wrote the file", c.name)
		}
	}
	p.write(before)

	// /domains?rule= filters by rule, with its select.
	body = p.get("/domains?rule=restricted")
	if got := strings.Join(hosts(body), ","); got != "wiki.internal.example,git.internal.example,ci.build.example,docs.internal.example" {
		t.Errorf("/domains?rule=restricted: %s", got)
	}
	for _, want := range []string{`<option value="restricted" selected>restricted</option>`, `<option value="all">all rules</option>`,
		`<option value="default">default: no rule matched</option>`, `<label for="f-rule">Rule</label>`,
		`<td class="opt rules" data-label="Rules"><a href="/rules/restricted">restricted</a></td>`} {
		if !strings.Contains(body, want) {
			t.Errorf("/domains?rule=restricted lacks %q", want)
		}
	}
	if body := p.get("/domains?rule=renamed-away"); !strings.Contains(body, `<option value="renamed-away" selected>renamed-away</option>`) {
		t.Error("a rule the configuration no longer names is not kept in the select")
	}
	if res := p.b.doWith(p.h, req{method: http.MethodGet, path: "/domains?rule=Bad%20Rule"}); res.StatusCode != http.StatusBadRequest {
		t.Errorf("/domains?rule=Bad Rule: %d", res.StatusCode)
	}
}

// TestRouteHero checks the route sentence of design 0009 D3.1 for each
// shape of the window's traffic.
func TestRouteHero(t *testing.T) {
	w := windowView{Window: stats.Window{Preset: "7d"}}
	text := func(h *routeHero) string {
		var b strings.Builder
		for _, s := range h.Sentence {
			b.WriteString(s.Text)
		}
		return b.String()
	}
	tot := func(action, up string, reqs, bytes int64) stats.Total {
		return stats.Total{Action: action, Upstream: up, Requests: reqs, BytesDown: bytes}
	}
	for _, c := range []struct {
		routes []stats.Total
		want   string
		bar    string
	}{
		{[]stats.Total{tot("direct", "", 3, 100)}, "In the last 7 days, all traffic went direct.", "g100"},
		{[]stats.Total{tot("direct", "", 3, 600), tot("upstream", "gw", 1, 400)}, "In the last 7 days, 60% of traffic went direct and 40% through gw.", "g60 g40"},
		{[]stats.Total{tot("direct", "", 3, 6), tot("upstream", "gw", 1, 3), tot("upstream", "lab", 1, 1), tot("block", "", 2310, 0)},
			"In the last 7 days, 60% of traffic went direct, 30% through gw, 10% through lab, and 2,310 requests were blocked.", "g60 g30 g10"},
		{[]stats.Total{tot("block", "", 1, 0)}, "In the last 7 days, no traffic went out and 1 request was blocked.", ""},
		{[]stats.Total{tot("direct", "", 3, 1000), tot("upstream", "gw", 1, 1)}, "In the last 7 days, 100% of traffic went direct and <1% through gw.", "g100 g1"},
		{[]stats.Total{tot("direct", "", 1, 1), tot("upstream", "a", 1, 1), tot("upstream", "b", 1, 1)}, "In the last 7 days, 34% of traffic went direct, 33% through a, and 33% through b.", "g34 g33 g33"},
	} {
		h := newRouteHero(w, c.routes)
		var grow []string
		for _, b := range h.Bar {
			grow = append(grow, b.Grow)
		}
		if got := text(h); got != c.want || strings.Join(grow, " ") != c.bar {
			t.Errorf("%v: %q %v, want %q %s", c.routes, got, grow, c.want, c.bar)
		}
	}
	if h := newRouteHero(w, []stats.Total{tot("direct", "", 1, 0)}); h != nil {
		t.Errorf("no bytes and no block: %+v", h)
	}
	h := newRouteHero(w, []stats.Total{tot("upstream", "a", 1, 1), tot("upstream", "b", 1, 1), tot("upstream", "c", 1, 1)})
	if h.Bar[0].Shade != "" || h.Bar[1].Shade != "shade2" || h.Bar[2].Shade != "shade3" {
		t.Errorf("upstream shades: %+v", h.Bar)
	}
}

// TestAdminOverviewStates checks the rest of W24: without statistics the
// overview shows the reason instead of the sentence, bar, and top hosts,
// and still the states and the configuration summary with its restart
// keys.
func TestAdminOverviewStates(t *testing.T) {
	f := newFixture(t, true)
	f.withExample()
	f.be.mu.Lock()
	f.be.st = nil
	f.be.restart = []string{"proxy.listen", "admin.listen"}
	f.be.mu.Unlock()
	b := f.browser()
	b.login()
	body := bodyOf(t, b.get("/"))
	for _, want := range []string{
		"Statistics are unavailable", "The proxy serves without them.",
		`<dt><a class="name-code" href="/upstreams/lab">lab</a></dt><dd><span class="state down">Down for 42 s, 17 failed fast</span></dd>`,
		`<dt><a class="name-code" href="/interfaces/eth-a">eth-a</a></dt><dd><span class="state online">Online</span></dd>`,
		`<dt>Rules</dt><dd><a class="num" href="/rules">4</a></dd>`,
		`<dt>Restart required</dt><dd><a href="/proxy"><code>proxy.listen</code></a>, <a href="/general"><code>admin.listen</code></a></dd>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("overview without statistics lacks %q", want)
		}
	}
	for _, not := range []string{"hero-sentence", "routebar", `class="hosts"`} {
		if strings.Contains(body, not) {
			t.Errorf("overview without statistics shows %s", not)
		}
	}
}

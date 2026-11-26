package admin

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/neutrospec/cuttle/internal/config"
)

// upstreamForm loads the upstream editor at path and returns its form.
func (p *pipeline) upstreamForm(path string) url.Values {
	p.t.Helper()
	return pageForm(p.t, p.get(path), "upstream-form")
}

// TestAdminUpstreams checks the upstreams page of design 0009 D3.5
// (W19): the list shows each upstream's live state, usage, and traffic;
// add, edit, and delete work through review and apply; a rename rewrites
// the actions and defaults that use it and returns to /upstreams; a used
// upstream has no Delete, and its delete op is refused with its users; a
// new upstream has no disclosure chosen, and a review without one shows
// the validator's message under the field.
func TestAdminUpstreams(t *testing.T) {
	p := newPipeline(t)
	// proxy.default also uses lab, so a rename has a default to rewrite.
	start := strings.Replace(exampleConfig, "default = \"direct\"\n", "default = \"upstream:lab\"\n", 1)
	start = strings.Replace(start, `name = "lab"`, "name = \"lab\"\nnote = \"Lab <box> & co\"", 1)
	p.write(start)
	body := p.get("/upstreams")
	checkMarkup(t, "upstreams", body, true, "/upstreams")
	for _, want := range []string{
		`<a class="e-name" href="/upstreams/gateway" data-fragment="upstream-editor">gateway</a>`,
		`<span class="state up">Up since 11:11</span>`, `<span class="badge">disclosure via</span>`, "Another cuttle instance; adds one Via entry",
		`<span class="state down">Down for 42 s, 17 failed fast</span><span class="hint">Last error: connection_timeout</span>`,
		`<a href="/rules/restricted">rule <code>restricted</code></a>`, `<a href="/rules/lab-net">rule <code>lab-net</code></a>, <code>proxy.default</code>`,
		"<b>3,471</b> req", "407.1 MiB", "Used by rule restricted. Change those first.", "Used by rule lab-net and proxy.default. Change those first.",
		`<span class="e-note">Lab &lt;box&gt; &amp; co</span>`, `href="/domains?route=upstream%3Agateway&amp;window=24h"`, "2 upstreams",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("upstreams page lacks %q", want)
		}
	}
	if strings.Contains(body, `form="updel-`) {
		t.Error("an upstream in use offers Delete")
	}

	// Add: no disclosure chosen; a review without one names the field.
	editor := p.get("/upstreams/new")
	checkMarkup(t, "new upstream", editor, true, "/upstreams")
	form := pageForm(t, editor, "upstream-form")
	if _, chosen := form["disclosure"]; chosen || strings.Contains(editor, `value="via" checked`) || strings.Contains(editor, `value="none" checked`) {
		t.Errorf("a disclosure is preselected: %v", form)
	}
	form = with(form, "name", "spare", "address", "192.0.2.50:3128")
	body = p.review(form, http.StatusUnprocessableEntity)
	slot := body[strings.Index(body, `id="f-disclosure-issues"`):]
	slot = slot[:strings.Index(slot, "</div>")]
	if !strings.Contains(slot, `<p class="err">required`) || !strings.Contains(body, `<a href="#f-disclosure">Disclosure: required`) {
		t.Errorf("review without a disclosure: %s\n%s", slot, mainText(body))
	}
	if res, frag := p.fragment("/config/draft", with(form, "intent", "review")); res.StatusCode != http.StatusUnprocessableEntity ||
		!strings.Contains(frag, `data-issues-for="f-disclosure" data-bad="" data-invalid><p class="err">required`) {
		t.Errorf("refused review as a fragment: %d:\n%s", res.StatusCode, frag)
	}
	body = p.review(with(form, "disclosure", "none"), http.StatusOK)
	spare := config.DocUpstream{Name: "spare", Address: "192.0.2.50:3128", Disclosure: "none"}
	if got := applyForm(t, body).Get("text"); got != p.docEdit(func(d *config.Doc) { d.AddUpstream(spare) }) {
		t.Errorf("new upstream candidate:\n%s", got)
	}
	if !strings.Contains(summaryOf(body), "Upstream spare added") || !strings.Contains(mainText(body), `upstream "spare" is not used by any action`) {
		t.Errorf("new upstream review: %s", mainText(body))
	}
	if res := p.apply(body); res.Header.Get("Location") != "/upstreams" {
		t.Errorf("after adding: %q", res.Header.Get("Location"))
	}

	// Delete the upstream that nothing uses.
	body = p.get("/upstreams")
	if !strings.Contains(body, `form="updel-2"`) || strings.Count(body, `form="updel-`) != 1 {
		t.Fatalf("delete offers:\n%s", mainText(body))
	}
	body = p.review(pageForm(t, body, "updel-2"), http.StatusOK)
	if !strings.Contains(summaryOf(body), "Upstream spare removed") || strings.Contains(body, "Back to editing") {
		t.Errorf("delete review: %s", mainText(body))
	}
	want := p.docEdit(func(d *config.Doc) { d.DeleteUpstream("spare") })
	if res := p.apply(body); res.Header.Get("Location") != "/upstreams" || p.file() != want {
		t.Errorf("after the delete: %q", res.Header.Get("Location"))
	}

	// Edit: a note, a timeout set and one cleared, and an interface list.
	form = p.upstreamForm("/upstreams/lab")
	if form.Get("unreachable_hold") != "10s" || form.Get("disclosure") != "via" || form.Get("ifaces") != "system" {
		t.Fatalf("lab form: %v", form)
	}
	form = with(form, "note", "Lab", "connect_timeout", " 5s ", "unreachable_hold", "", "ifaces", "only", "ifaces_list", "wifi, eth-a")
	body = p.review(form, http.StatusOK)
	want = p.docEdit(func(d *config.Doc) {
		u := &d.Upstreams[1]
		u.Note, u.ConnectTimeout, u.UnreachableHold, u.Interfaces = config.Some("Lab"), config.Some("5s"), config.Opt[string]{}, config.Some([]string{"wifi", "eth-a"})
	})
	if got := applyForm(t, body).Get("text"); got != want {
		t.Errorf("edit candidate:\n%s\nwant\n%s", got, want)
	}
	p.apply(body)
	body = p.review(with(p.upstreamForm("/upstreams/lab"), "ifaces", "system"), http.StatusOK)
	if nd, _, _ := config.DocOf([]byte(applyForm(t, body).Get("text")), "x"); nd.Upstreams[1].Interfaces.Set {
		t.Error("System routing kept the interface list")
	}

	// Rename: every action and default that used lab now names lab-b.
	body = p.review(with(p.upstreamForm("/upstreams/lab"), "name", "lab-b"), http.StatusOK)
	if s := summaryOf(body); !strings.Contains(s, "Upstream renamed lab → lab-b") {
		t.Errorf("rename summary: %s", s)
	}
	cand := applyForm(t, body).Get("text")
	nd, _, _ := config.DocOf([]byte(cand), "x")
	if nd == nil || nd.Proxy.Default != "upstream:lab-b" || nd.Rules[3].Action != "upstream:lab-b" || strings.Contains(cand, "upstream:lab\"") {
		t.Fatalf("rename candidate:\n%s", cand)
	}
	if res := p.apply(body); res.Header.Get("Location") != "/upstreams" || p.file() != cand {
		t.Errorf("after the rename: %q", res.Header.Get("Location"))
	}
	if cfg := p.f.be.Config(); cfg.Proxy.Default.Upstream != "lab-b" {
		t.Errorf("the reload did not take the rename: %v", cfg.Proxy.Default)
	}

	// A delete of an upstream in use is refused with its users.
	d, _, _ := config.DocOf([]byte(p.file()), "x")
	g, _ := d.Upstream("gateway")
	del := newFormFrom(p.f.s.ops[opUpstreamDelete], upstreamValues(g), "gateway", config.Digest([]byte(p.file())), "/upstreams")
	post := urlValues(del.Hidden())
	post.Set("csrf", p.csrf)
	before := p.file()
	res, out := p.post("/config/draft", with(post, "intent", "review"))
	if res.StatusCode != http.StatusConflict || !strings.Contains(out, "is used by rule `restricted`") || strings.Contains(out, "Apply changes") {
		t.Errorf("delete of a used upstream: %d: %s", res.StatusCode, mainText(out))
	}
	if p.file() != before {
		t.Fatal("a refused delete changed the file")
	}
	edit := p.get("/upstreams/gateway")
	if strings.Contains(edit, `form="upstream-delete"`) || !strings.Contains(edit, "Used by rule restricted, so it cannot be deleted.") {
		t.Errorf("editor of a used upstream: %s", mainText(edit))
	}

	// The editor as the dialog's fragment, and paths that name nothing.
	res = p.b.doWith(p.h, req{method: http.MethodGet, path: "/upstreams/new", header: http.Header{"Cuttle-Fragment": {"1"}}})
	if body := bodyOf(t, res); !strings.HasPrefix(body, `<div class="up-editor" data-block="upstream-editor">`) ||
		!strings.Contains(body, `<h2 id="editor-h">Add upstream</h2>`) || !strings.Contains(body, "data-close") || strings.Contains(body, "<h1") {
		t.Errorf("editor fragment:\n%s", body)
	}
	for _, path := range []string{"/upstreams/Bad", "/upstreams/nope", "/rules/Bad", "/rules/nope", "/rules/a%2Fb"} {
		if res := p.b.doWith(p.h, req{method: http.MethodGet, path: path}); res.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", path, res.StatusCode)
		}
	}

	// No upstreams: the empty state with Add upstream.
	var b strings.Builder
	for _, line := range strings.Split(exampleConfig, "\n") {
		if strings.Contains(line, "upstream:") {
			line = strings.Replace(line, line[strings.Index(line, `"`):], `"block"`, 1)
		}
		b.WriteString(line + "\n")
	}
	noUps := b.String()
	for _, name := range []string{"gateway", "lab"} {
		i := strings.Index(noUps, "[[upstream]]\nname = \""+name+"\"")
		j := i + strings.Index(noUps[i:], "\n\n") + 2
		noUps = noUps[:i] + noUps[j:]
	}
	if cfg, r := config.Parse([]byte(noUps), "x"); cfg == nil {
		t.Fatalf("sample without upstreams: %v", r.Errors)
	}
	p.write(noUps)
	body = p.get("/upstreams")
	if !strings.Contains(body, "<h3>No upstreams</h3>") || !strings.Contains(body, `<a class="btn primary" href="/upstreams/new" data-fragment="upstream-editor">`) {
		t.Errorf("empty state: %s", mainText(body))
	}
	checkMarkup(t, "upstreams empty", body, true, "/upstreams")
}

// urlValues returns hidden fields as a form.
func urlValues(hidden []hiddenField) url.Values {
	v := url.Values{}
	for _, h := range hidden {
		v.Add(h.Name, h.Value)
	}
	return v
}

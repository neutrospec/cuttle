package main

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/neutrospec/cuttle/internal/config"
	"github.com/neutrospec/cuttle/internal/testenv"
)

// chromeRules are the rules of the browser check besides loopback: an
// early rule that shadows a later one, and a second rule to move.
const chromeRules = `
[[rule]]
name = "early"
domains = ["shadow.example"]
action = "block"

[[rule]]
name = "second"
domains = ["two.example"]
action = "block"
`

// chromeNewPassword is the password the check changes to.
const chromeNewPassword = "a different and longer passphrase"

// formFlows drives the form flows of design 0009 W26 in a running
// instance with the script: add a rule by pasting three domains into the
// list input, see the live check flag an invalid entry and the live test
// shadowed by an earlier rule, apply and see the toast; reorder two rules
// by keyboard and review once; add an upstream; define an interface from
// the detected table; change the password and stay signed in; and route a
// host from the domains page in four activations, and once more without
// the script. Every flow ends in the file.
func formFlows(t *testing.T, chrome *testenv.Chrome, p *testenv.ChromePage, base, proxyAddr, cfgPath string,
	shoot func(string), clean func(string)) {
	t.Helper()
	// Two blocked requests, so that the domains page has hosts to route
	// once the statistics flush, every 10 s, while the other flows run.
	for _, host := range []string{"route-me.example", "noscript-me.example"} {
		blockedConnect(t, proxyAddr, host+":443")
	}

	eval := func(expr string, v any) {
		t.Helper()
		p.Eval(expr, v)
	}
	wait := func(what, expr string) {
		t.Helper()
		var ok bool
		eval(`new Promise((r) => { const t0 = Date.now(); (function poll() {
			let v = false; try { v = !!(`+expr+`); } catch (e) {}
			if (v || Date.now() - t0 > 8000) r(v); else setTimeout(poll, 50); })(); })`, &ok)
		if !ok {
			t.Fatalf("timed out waiting for %s: %s", what, expr)
		}
	}
	click := func(sel string) {
		t.Helper()
		eval(`(() => { const e = document.querySelector(`+jsString(sel)+`); if (!e) throw new Error("no " + `+jsString(sel)+`); e.click(); return true })()`, nil)
	}
	typeInto := func(sel, value string) {
		t.Helper()
		eval(`(() => { const i = document.querySelector(`+jsString(sel)+`); i.focus(); i.value = `+jsString(value)+`;
			i.dispatchEvent(new Event("input", {bubbles: true})); i.dispatchEvent(new Event("change", {bubbles: true})); return true })()`, nil)
	}
	text := func(sel string) string {
		t.Helper()
		var v string
		eval(`(document.querySelector(`+jsString(sel)+`) || {}).textContent || ""`, &v)
		return strings.Join(strings.Fields(v), " ")
	}
	press := func(key, code string, vk int) {
		t.Helper()
		for _, typ := range []string{"rawKeyDown", "keyUp"} {
			chrome.Call(p.Session, "Input.dispatchKeyEvent", map[string]any{"type": typ, "key": key, "code": code, "windowsVirtualKeyCode": vk}, nil)
		}
	}
	// apply presses Apply changes in the review dialog and returns the
	// toast of the page it returns to.
	apply := func(what string) string {
		t.Helper()
		wait(what+" review", `document.getElementById("review-dialog").open && document.querySelector("#review-dialog .dlg-foot .btn.primary")`)
		shoot("flow-" + what + "-review")
		click("#review-dialog .dlg-foot .btn.primary")
		p.WaitLoad()
		toast := text(".toast")
		if !strings.Contains(toast, "Saved and reloaded") {
			t.Errorf("%s: toast %q", what, toast)
		}
		clean(what)
		return toast
	}
	doc := func() *config.Doc {
		t.Helper()
		data, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		d, _, rep := config.DocOf(data, cfgPath)
		if d == nil {
			t.Fatalf("invalid file: %v", rep.Errors)
		}
		return d
	}
	ruleNames := func() string {
		var out []string
		for _, r := range doc().Rules {
			out = append(out, r.Name)
		}
		return strings.Join(out, " ")
	}

	// A rule from three pasted domains. An invalid entry is flagged by
	// the live check; the live test, with the rule last, shows that the
	// earlier rule early matches first; first in the chain, it applies.
	p.Navigate(base + "/rules/new")
	eval(`(() => { const i = document.getElementById("f-domains-entry"); i.focus();
		const dt = new DataTransfer(); dt.setData("text/plain", "a.example, b.example\nx.shadow.example *.bad.example");
		i.dispatchEvent(new ClipboardEvent("paste", {clipboardData: dt, bubbles: true, cancelable: true})); return true })()`, nil)
	wait("the live check", `document.querySelector("#f-domains-issues .err") && document.querySelector("#f-domains + .tokens .tok.bad")`)
	if got := text("#f-domains-issues .err"); !strings.Contains(got, "Entry 4") || !strings.Contains(got, "wildcards are not supported") {
		t.Errorf("live check: %q", got)
	}
	shoot("flow-rule-live-check")
	click(`#f-domains + .tokens .tok.bad button`)
	typeInto("#f-position", "last")
	typeInto("#rule-test-url", "https://x.shadow.example/")
	wait("the live test", `document.querySelector("#rule-test .shadow")`)
	if got := text("#rule-test .shadow"); got != "Rule 2, early, matches first." || !strings.Contains(text("#rule-test"), "This rule matches") {
		t.Errorf("live test: %q", text("#rule-test"))
	}
	shoot("flow-rule-live-test")
	wait("the check without the wildcard", `!document.querySelector("#f-domains-issues .err")`)
	typeInto("#f-position", "first")
	click(`.rule-form [data-primary]`)
	apply("rule")
	if r, i := doc().Rule("a.example"); i != 0 || strings.Join(r.Domains.V, ",") != "a.example,b.example,x.shadow.example" || r.Action != "direct" {
		t.Errorf("added rule at %d: %+v", i, r)
	}

	// Reorder by keyboard: second moves above early, and one review
	// applies it.
	p.Navigate(base + "/rules")
	eval(`document.querySelector('.rule[data-name="second"] .handle').focus()`, nil)
	press(" ", "Space", 32)
	press("ArrowUp", "ArrowUp", 38)
	press(" ", "Space", 32)
	wait("the save bar", `!document.getElementById("savebar").hidden`)
	click("#savebar-submit")
	apply("reorder")
	if got := ruleNames(); got != "a.example loopback second early" {
		t.Errorf("rules after the reorder: %s", got)
	}

	// An upstream from the dialog.
	p.Navigate(base + "/upstreams")
	click(`.page-head a[data-fragment="upstream-editor"]`)
	wait("the upstream dialog", `document.getElementById("editor-dialog").open && document.getElementById("upstream-form")`)
	typeInto(`#upstream-form input[name="name"]`, "gateway")
	typeInto(`#upstream-form input[name="address"]`, "127.0.0.1:9")
	click(`#upstream-form input[name="disclosure"][value="via"]`)
	shoot("flow-upstream-dialog")
	click(`#upstream-form [data-primary]`)
	apply("upstream")
	if u, i := doc().Upstream("gateway"); i < 0 || u.Address != "127.0.0.1:9" || u.Disclosure != "via" {
		t.Errorf("added upstream: %d %+v", i, u)
	}

	// An interface defined from the detected table of this Mac. Only its
	// name is logged.
	p.Navigate(base + "/interfaces")
	var define string
	eval(`(document.querySelector('a[data-fragment="editor-slot"][href^="/interfaces/new?device="]') || {}).textContent || ""`, &define)
	if define == "" {
		t.Error("the detected table offers no Define…")
	} else {
		click(`a[data-fragment="editor-slot"][href^="/interfaces/new?device="]`)
		wait("the define dialog", `document.getElementById("editor-dialog").open && document.querySelector("#editor-slot [data-block=editor]")`)
		click(`#editor-slot [data-primary]`)
		apply("interface")
		if d := doc(); len(d.Interfaces) != 1 {
			t.Errorf("%d interfaces defined", len(d.Interfaces))
		} else {
			t.Logf("defined interface %s from the detected table", d.Interfaces[0].Name)
		}
	}

	// The password, and the browser stays signed in.
	p.Navigate(base + "/account")
	typeInto("#pw-current", adminTestPassword)
	typeInto("#pw-new", chromeNewPassword)
	typeInto("#pw-confirm", chromeNewPassword)
	click(`.account-form [data-primary]`)
	p.WaitLoad()
	if s := readState(t, p); s.Path != "/account" || s.H1 != "Account" || !strings.Contains(text(".toast"), "Password changed.") {
		t.Errorf("after the password change: %+v, toast %q", s, text(".toast"))
	}
	clean("password")
	if s := navigateState(t, p, base+"/rules"); s.Path != "/rules" {
		t.Errorf("not signed in after the password change: %+v", s)
	}

	// Route a host from the domains page in four activations: Route…,
	// the route, Review changes, and Apply changes.
	routeLink := func(host string) string {
		return `a[data-fragment="route-editor"][aria-label="Route ` + host + `…"]`
	}
	deadline := time.Now().Add(25 * time.Second)
	for {
		p.Navigate(base + "/domains?route=block")
		var found bool
		eval(`!!document.querySelector(`+jsString(routeLink("route-me.example"))+`) && !!document.querySelector(`+jsString(routeLink("noscript-me.example"))+`)`, &found)
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the blocked hosts did not reach the domains page")
		}
		time.Sleep(time.Second)
	}
	activations := 0
	click(routeLink("route-me.example"))
	activations++
	wait("the route dialog", `document.getElementById("route-dialog").open && document.getElementById("route-form")`)
	if now := text("#route-form .verdict"); !strings.Contains(now, "Currently block by the default route") {
		t.Errorf("route dialog: %q", now)
	}
	click(`#route-form input[name="action"][value="direct"]`)
	activations++
	shoot("flow-route-dialog")
	click(`#route-form [data-primary]`)
	activations++
	activations++ // Apply changes
	apply("route")
	if s := readState(t, p); s.Path != "/domains" || activations != 4 {
		t.Errorf("after Route a host: %+v in %d activations", s, activations)
	}
	if r, i := doc().Rule("route-me.example"); i != 0 || strings.Join(r.Hosts.V, ",") != "route-me.example" || r.Action != "direct" {
		t.Errorf("routed host: rule %d %+v", i, r)
	}

	// Without the script: the page, the review page, and back.
	chrome.Call(p.Session, "Emulation.setScriptExecutionDisabled", map[string]any{"value": true}, nil)
	p.Navigate(base + "/domains?route=block")
	click(routeLink("noscript-me.example"))
	p.WaitLoad()
	if s := readState(t, p); s.Path != "/rules/new" || s.H1 != "Route noscript-me.example" {
		t.Errorf("Route… without the script: %+v", s)
	}
	click(`#route-form input[name="action"][value="upstream:gateway"]`)
	click(`#route-form [data-primary]`)
	p.WaitLoad()
	click(".review-form .btn.primary")
	p.WaitLoad()
	if s := readState(t, p); s.Path != "/domains" || !strings.Contains(text(".toast"), "Saved and reloaded") {
		t.Errorf("Route a host without the script: %+v", s)
	}
	if r, i := doc().Rule("noscript-me.example"); i != 0 || r.Action != "upstream:gateway" {
		t.Errorf("routed host without the script: rule %d %+v", i, r)
	}
	clean("route without the script")
	chrome.Call(p.Session, "Emulation.setScriptExecutionDisabled", map[string]any{"value": false}, nil)
}

// navigateState loads u and reads the page.
func navigateState(t *testing.T, p *testenv.ChromePage, u string) pageState {
	t.Helper()
	p.Navigate(u)
	return readState(t, p)
}

// blockedConnect sends a CONNECT for target through the proxy, whose
// block default refuses it before any lookup.
func blockedConnect(t *testing.T, proxyAddr, target string) {
	t.Helper()
	c, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	io.WriteString(c, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n\r\n")
	res, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: http.MethodConnect})
	if err != nil || res.StatusCode != http.StatusForbidden {
		t.Fatalf("CONNECT %s: %v %v", target, res, err)
	}
}

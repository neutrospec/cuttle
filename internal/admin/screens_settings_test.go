package admin

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neutrospec/cuttle/internal/config"
	"github.com/neutrospec/cuttle/internal/testenv"
)

// TestAdminScreensSettings is the browser check of the pages of task D of
// design 0009 (D3.6–D3.9): headless Chrome, as in TestAdminScreens, signs
// in to a server with the example configuration, the injected interface
// table, and statistics, and renders Interfaces, the Define… dialog,
// Proxy, Remote clients, General, and Account, the last also with a
// wrong current password, at 1280 px and 390 px in light and dark
// appearance, with no sideways scrolling, console error, or
// Content-Security-Policy violation; the overview with a long upstream
// address without hyphens stays within a phone's width (OI-0006 T-R1).
// It then drives the flows with the script: define an interface from the
// detected table in the dialog, reorder and extend the proxy interface
// list in place, choose "No ports", choose "On at" for the admin
// listener, and change the password and stay signed in; and the same
// flows again with scripts disabled, through the plain buttons and the
// review page.
//
// It starts a browser, so T0 and T1 skip it; scripts/admin-check runs it
// with CUTTLE_ADMIN_CHROME=1, and CUTTLE_ADMIN_CHROME_OUT names a
// directory for the screenshots.
func TestAdminScreensSettings(t *testing.T) {
	if os.Getenv("CUTTLE_ADMIN_CHROME") != "1" {
		t.Skip("starts headless Chrome; run scripts/admin-check")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := newFixtureAt(t, true, l.Addr().String())
	f.withExample()
	f.withLinks()
	srv := &http.Server{Handler: f.s}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	block := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "blocked by the test", http.StatusForbidden)
	}))
	t.Cleanup(block.Close)
	dir := t.TempDir()
	home, tmp := filepath.Join(dir, "home"), filepath.Join(dir, "tmp")
	for _, d := range []string{home, tmp} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	chrome := testenv.StartChrome(t, testenv.ChromeOptions{Dir: dir, Env: []string{"HOME=" + home, "TMPDIR=" + tmp, "PATH=/usr/bin:/bin"},
		Proxy: strings.TrimPrefix(block.URL, "http://")})
	p := chrome.NewPage()
	base := "http://" + l.Addr().String()
	out := os.Getenv("CUTTLE_ADMIN_CHROME_OUT")
	shoot := func(name string) {
		t.Helper()
		if out != "" {
			p.Eval(`Promise.all(document.getAnimations().map((a) => a.finished)).then(() => true)`, nil)
			p.Screenshot(filepath.Join(out, name+".png"))
		}
	}
	// Chrome logs a 4xx answer as a network error; 401 (a wrong current
	// password), 409, and 422 are answers these pages expect and show.
	clean := func(where string) {
		t.Helper()
		var probs []string
		for _, pr := range p.Problems() {
			if strings.HasPrefix(pr, "log network: Failed to load resource: the server responded with a status of 4") {
				code := strings.TrimPrefix(pr, "log network: Failed to load resource: the server responded with a status of ")
				if strings.HasPrefix(code, "401 ") || strings.HasPrefix(code, "409 ") || strings.HasPrefix(code, "422 ") {
					continue
				}
			}
			probs = append(probs, pr)
		}
		if len(probs) > 0 {
			t.Errorf("%s: console errors or policy violations: %q", where, probs)
		}
	}
	eval := func(expr string, v any) {
		t.Helper()
		p.Eval(expr, v)
	}
	type layout struct {
		Path   string `json:"path"`
		H1     string `json:"h1"`
		Width  int    `json:"width"`
		Scroll int    `json:"scroll"`
		Script bool   `json:"script"`
	}
	state := func() layout {
		t.Helper()
		var s layout
		eval(`({path: location.pathname, h1: (document.querySelector("h1") || {}).textContent || "",
			width: document.documentElement.clientWidth, scroll: document.documentElement.scrollWidth,
			script: document.documentElement.classList.contains("js")})`, &s)
		return s
	}
	// click presses the element sel names; nav waits for the page it loads.
	click := func(sel string, nav bool) {
		t.Helper()
		var ok bool
		eval(`(() => { const e = document.querySelector(`+fmt.Sprintf("%q", sel)+`); if (!e) return false; e.click(); return true; })()`, &ok)
		if !ok {
			t.Fatalf("no element %s on %s", sel, state().Path)
		}
		if nav {
			p.WaitLoad()
		}
	}
	// waitFor polls until the expression is true, for up to 5 s.
	waitFor := func(expr string) bool {
		t.Helper()
		var ok bool
		eval(`new Promise((r) => { const t0 = Date.now(); (function poll() { let v = false; try { v = !!(`+expr+`); } catch (e) {}
			if (v || Date.now() - t0 > 5000) r(v); else setTimeout(poll, 50); })(); })`, &ok)
		return ok
	}
	setValue := func(sel, value string) {
		t.Helper()
		eval(`(() => { const e = document.querySelector(`+fmt.Sprintf("%q", sel)+`); e.value = `+fmt.Sprintf("%q", value)+`;
			e.dispatchEvent(new Event("input", {bubbles: true})); e.dispatchEvent(new Event("change", {bubbles: true})); return true; })()`, nil)
	}
	reviewOpen := `document.getElementById("review-dialog").open && document.querySelector("#review-dialog [data-block]")`

	p.Viewport(1280, 900, false)
	p.ColorScheme("light")
	p.Navigate(base + "/login")
	eval(`(() => { document.getElementById("password").value = `+fmt.Sprintf("%q", testPassword)+
		`; document.querySelector("form").submit(); return true })()`, nil)
	p.WaitLoad()
	if s := state(); s.Path != "/" || !s.Script {
		t.Fatalf("after sign-in: %+v", s)
	}

	// An upstream address without hyphens, longer than a phone is wide
	// (OI-0006 T-R1).
	long := "averyveryverylongupstreamhostnamewithoutanyhyphensatall.internal.example:3128"
	f.be.mu.Lock()
	f.be.status.Upstreams[1].Address = long
	f.be.mu.Unlock()

	pages := []struct{ path, h1, name string }{
		{"/", "Overview", "overview-long-address"},
		{"/interfaces", "Interfaces", "interfaces"},
		{"/proxy", "Proxy", "proxy"},
		{"/remote", "Remote clients", "remote"},
		{"/general", "General", "general"},
		{"/account", "Account", "account"},
	}
	for _, scheme := range []string{"light", "dark"} {
		p.ColorScheme(scheme)
		for _, w := range []struct {
			px     int
			mobile bool
		}{{1280, false}, {390, true}} {
			p.Viewport(w.px, 900, w.mobile)
			suffix := fmt.Sprintf("-%d-%s", w.px, scheme)
			check := func(name, h1 string) {
				t.Helper()
				s := state()
				if s.H1 != h1 || !s.Script {
					t.Errorf("%s%s: %+v", name, suffix, s)
				}
				if s.Scroll > s.Width {
					t.Errorf("%s%s scrolls sideways: content %d px, viewport %d px", name, suffix, s.Scroll, s.Width)
				}
				// A table panel scrolls inside its wrapper rather than the
				// page (OI-0006 T-R1).
				var panels []string
				eval(`[...document.querySelectorAll(".tablewrap")].filter((e) => e.scrollWidth > e.clientWidth + 1)
					.map((e) => (e.closest("section, .section") || e).textContent.trim().slice(0, 40))`, &panels)
				if len(panels) > 0 {
					t.Errorf("%s%s: table panels scroll sideways: %q", name, suffix, panels)
				}
				clean(name + suffix)
				shoot(name + suffix)
			}
			for _, pg := range pages {
				p.Navigate(base + pg.path)
				check(pg.name, pg.h1)
			}
			// The Define… dialog over the Interfaces page.
			p.Navigate(base + "/interfaces")
			click(`a[href="/interfaces/new?device=en8"]`, false)
			if !waitFor(`document.getElementById("editor-dialog").open && document.querySelector("#editor-slot [data-block=editor]")`) {
				t.Fatalf("Define… opened no dialog%s", suffix)
			}
			var wide bool
			eval(`(() => { const d = document.getElementById("editor-dialog"); const b = d.querySelector(".dlg-body");
				return d.scrollWidth > d.clientWidth || b.scrollWidth > b.clientWidth; })()`, &wide)
			if wide {
				t.Errorf("Define… dialog%s scrolls sideways", suffix)
			}
			clean("define dialog" + suffix)
			shoot("interfaces-define" + suffix)
			p.Key("Escape", 27)
			// Account with a wrong current password; each counts toward the
			// shared backoff, which starts again here.
			f.s.loginMu.Lock()
			f.s.backoff.reset()
			f.s.loginMu.Unlock()
			p.Navigate(base + "/account")
			setValue("#pw-current", "not the password")
			setValue("#pw-new", "a new and longer passphrase")
			setValue("#pw-confirm", "a new and longer passphrase")
			click(".account-form [data-primary]", true)
			var msg string
			eval(`(document.querySelector("#field-current .err") || {}).textContent || ""`, &msg)
			if msg != "Invalid password." {
				t.Errorf("wrong current password%s: %q", suffix, msg)
			}
			check("account-wrong", "Account")
		}
	}
	// The wrong passwords above counted toward the shared backoff; the
	// flows below start without a wait.
	f.s.loginMu.Lock()
	f.s.backoff.reset()
	f.s.loginMu.Unlock()
	p.Viewport(1280, 900, false)
	p.ColorScheme("light")

	file := func() *config.Doc { return docOf(t, readFile(t, f.be.path)) }
	applyReview := func(where string) {
		t.Helper()
		if !waitFor(reviewOpen) {
			var issues string
			eval(`(document.getElementById("form-issues") || {}).textContent || ""`, &issues)
			t.Fatalf("%s: no review dialog; issues %q", where, issues)
		}
		shoot(where + "-review")
		click("#review-dialog .dlg-foot .btn.primary", true)
		var toast string
		eval(`(document.querySelector(".toast") || {}).textContent || ""`, &toast)
		if !strings.Contains(toast, "Saved and reloaded") {
			t.Errorf("%s: toast %q", where, toast)
		}
		clean(where)
	}

	// With the script. Define en8 in the dialog.
	p.Navigate(base + "/interfaces")
	click(`a[href="/interfaces/new?device=en8"]`, false)
	waitFor(`document.querySelector("#editor-slot [data-block=editor]")`)
	click(`#editor-slot [data-primary]`, false)
	applyReview("define")
	if s := state(); s.Path != "/interfaces" {
		t.Errorf("after define: %+v", s)
	}
	if x, i := file().Interface("eth-b"); i < 0 || x.MAC.V != "00:00:5e:00:53:0b" || x.Device.Set {
		t.Errorf("defined eth-b: %+v", x)
	}
	// The proxy interface list in place: move eth-a down, add eth-b.
	p.Navigate(base + "/proxy")
	click(`[data-iflist="interfaces"] li[data-n="eth-a"] [data-ifl="down"]`, false)
	setValue(`[data-iflist="interfaces"] select[data-ifl-add]`, "eth-b")
	var list struct {
		Names []string `json:"names"`
		Pos   []string `json:"pos"`
		Bar   bool     `json:"bar"`
		Path  string   `json:"path"`
	}
	eval(`({names: [...document.querySelectorAll('.settings-form input[type=hidden][name="interfaces"]')].map((i) => i.value),
		pos: [...document.querySelectorAll('[data-iflist="interfaces"] .pos')].map((s) => s.textContent),
		bar: !document.getElementById("savebar").hidden, path: location.pathname})`, &list)
	if strings.Join(list.Names, ",") != "wifi,eth-a,eth-b" || strings.Join(list.Pos, ",") != "1,2,3" || !list.Bar || list.Path != "/proxy" {
		t.Errorf("list editor with the script: %+v", list)
	}
	shoot("proxy-list-edited-1280-light")
	click(`.settings-form [data-primary]`, false)
	applyReview("proxy-list")
	if got := strings.Join(file().Proxy.Interfaces.V, ","); got != "wifi,eth-a,eth-b" {
		t.Errorf("proxy.interfaces after the review: %q", got)
	}
	// Remote clients: No ports.
	p.Navigate(base + "/remote")
	click(`input[name="ports"][value="none"]`, false)
	click(`.settings-form [data-primary]`, false)
	applyReview("remote-no-ports")
	if r := file().Remote.Ports; !r.Set || len(r.V) != 0 {
		t.Errorf("No ports: %+v", r)
	}
	// General: the admin listener On at, with its field shown only then.
	p.Navigate(base + "/general")
	var shown [2]bool
	eval(`[document.getElementById("f-admin_at").offsetParent !== null, false]`, &shown)
	click(`input[name="admin_listen"][value="disabled"]`, false)
	eval(`[document.getElementById("f-admin_at").offsetParent !== null, document.getElementById("f-admin_at").offsetParent !== null]`, &shown)
	click(`input[name="admin_listen"][value="at"]`, false)
	setValue("#f-admin_at", "127.0.0.1:41760")
	var visible bool
	eval(`document.getElementById("f-admin_at").offsetParent !== null`, &visible)
	if shown[0] || !visible {
		t.Errorf("On at field: shown with Disabled %v, shown with On at %v", shown[0], visible)
	}
	click(`.settings-form [data-primary]`, false)
	applyReview("general-admin-at")
	if a := file().Admin.Listen; a.V != "127.0.0.1:41760" {
		t.Errorf("admin.listen: %+v", a)
	}
	// The password change keeps this browser signed in.
	p.Navigate(base + "/account")
	setValue("#pw-current", testPassword)
	setValue("#pw-new", "a new and longer passphrase")
	setValue("#pw-confirm", "a new and longer passphrase")
	click(".account-form [data-primary]", true)
	var after struct {
		Path  string `json:"path"`
		Toast string `json:"toast"`
	}
	eval(`({path: location.pathname, toast: (document.querySelector(".toast") || {}).textContent || ""})`, &after)
	if after.Path != "/account" || !strings.Contains(after.Toast, "Password changed. Other sessions were signed out.") {
		t.Errorf("password change: %+v", after)
	}
	shoot("account-changed-1280-light")
	clean("password change")

	// Without the script, through the plain buttons and review pages.
	chrome.Call(p.Session, "Emulation.setScriptExecutionDisabled", map[string]any{"value": true}, nil)
	applyPage := func(where string) {
		t.Helper()
		if s := state(); s.Path != "/config/draft" || !strings.HasPrefix(s.H1, "Review: ") {
			t.Fatalf("%s: no review page: %+v", where, s)
		}
		shoot(where + "-review-noscript")
		click(".review-form .btn.primary", true)
		var toast string
		eval(`(document.querySelector(".toast") || {}).textContent || ""`, &toast)
		if !strings.Contains(toast, "Saved and reloaded") {
			t.Errorf("%s without the script: toast %q", where, toast)
		}
		clean(where + " without the script")
	}
	// Define en3 on the editor page.
	p.Navigate(base + "/interfaces")
	click(`a[href="/interfaces/new?device=en3"]`, true)
	if s := state(); s.Path != "/interfaces/new" || s.H1 != "Define en3" || s.Script {
		t.Errorf("Define… without the script: %+v", s)
	}
	shoot("interface-define-page-noscript")
	click(`.if-form [data-primary]`, true)
	applyPage("define")
	if _, i := file().Interface("eth-c"); i < 0 {
		t.Errorf("eth-c not defined:\n%s", readFile(t, f.be.path))
	}
	// The proxy list's buttons submit for editing.
	p.Navigate(base + "/proxy")
	click(`[data-iflist="interfaces"] li[data-n="eth-b"] [data-ifl="up"]`, true)
	click(`[data-iflist="interfaces"] li[data-n="wifi"] [data-ifl="remove"]`, true)
	setValue(`[data-iflist="interfaces"] select[data-ifl-add]`, "eth-c")
	click(`[data-iflist="interfaces"] .ifl-add`, true)
	eval(`({names: [...document.querySelectorAll('.settings-form input[type=hidden][name="interfaces"]')].map((i) => i.value),
		pos: [...document.querySelectorAll('[data-iflist="interfaces"] .pos')].map((s) => s.textContent),
		bar: false, path: location.pathname})`, &list)
	if strings.Join(list.Names, ",") != "eth-b,eth-a,eth-c" || list.Path != "/config/draft" {
		t.Errorf("list editor without the script: %+v", list)
	}
	shoot("proxy-list-noscript")
	click(`.settings-form [data-primary]`, true)
	applyPage("proxy-list")
	if got := strings.Join(file().Proxy.Interfaces.V, ","); got != "eth-b,eth-a,eth-c" {
		t.Errorf("proxy.interfaces without the script: %q", got)
	}
	// Remote clients: back to the default ports.
	p.Navigate(base + "/remote")
	click(`input[name="ports"][value="default"]`, false)
	click(`.settings-form [data-primary]`, true)
	applyPage("remote-default-ports")
	if file().Remote.Ports.Set {
		t.Errorf("Default ports without the script kept the key")
	}
	// General: the admin listener back to Default.
	p.Navigate(base + "/general")
	click(`input[name="admin_listen"][value="default"]`, false)
	click(`.settings-form [data-primary]`, true)
	applyPage("general-default")
	if file().Admin.Listen.Set {
		t.Errorf("Default admin listener without the script kept the key")
	}
	// The password change.
	p.Navigate(base + "/account")
	setValue("#pw-current", "a new and longer passphrase")
	setValue("#pw-new", testPassword)
	setValue("#pw-confirm", testPassword)
	click(".account-form [data-primary]", true)
	eval(`({path: location.pathname, toast: (document.querySelector(".toast") || {}).textContent || ""})`, &after)
	if after.Path != "/account" || !strings.Contains(after.Toast, "Password changed.") {
		t.Errorf("password change without the script: %+v", after)
	}
	clean("without the script")
	chrome.Call(p.Session, "Emulation.setScriptExecutionDisabled", map[string]any{"value": false}, nil)
	chrome.Close()
}

package admin

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/neutrospec/cuttle/internal/testenv"
)

// TestAdminScreens is the browser check of the shell, the visual system,
// and the script core of design 0009 (D2, D10, D11): headless Chrome, with
// a throwaway profile and the mock keychain as in TestAdminChrome, signs
// in to a server with the example configuration, traffic, and running
// state, and renders the overview, domains, live, and Advanced pages, the
// Advanced page after a save that needs a restart (a toast and a banner),
// the sign-in page, and the component gallery, each at 1280 px and 390 px
// in light and dark appearance, with no sideways scrolling, console
// error, or Content-Security-Policy violation. It then drives the script:
// list input chips from a paste, the unsaved-changes bar and Discard, a
// dialog that Escape closes with the focus back on its opener, toasts that
// close, the Validate fragment, and the phone menu; and, with scripts
// disabled, that the plain controls remain and work.
//
// It starts a browser, so T0 and T1 skip it; scripts/admin-check runs it
// with CUTTLE_ADMIN_CHROME=1, and CUTTLE_ADMIN_CHROME_OUT names a
// directory for the screenshots. Chrome's only proxy is a local listener
// that answers 403, and every host name but 127.0.0.1 is NOTFOUND.
func TestAdminScreens(t *testing.T) {
	if os.Getenv("CUTTLE_ADMIN_CHROME") != "1" {
		t.Skip("starts headless Chrome; run scripts/admin-check")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := newFixtureAt(t, true, l.Addr().String())
	f.withExample()
	srv := &http.Server{Handler: f.galleryPage()}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	var proxied atomic.Int64
	block := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Add(1)
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
			p.Screenshot(filepath.Join(out, name+".png"))
		}
	}
	// Chrome logs every 4xx answer to a fetch as a network error; 422 and
	// 409 are the answers the script expects for a refused draft or a
	// conflict, and it shows them, so only those lines are not problems.
	clean := func(where string) {
		t.Helper()
		var probs []string
		for _, pr := range p.Problems() {
			if strings.HasPrefix(pr, "log network: Failed to load resource: the server responded with a status of 422 ") ||
				strings.HasPrefix(pr, "log network: Failed to load resource: the server responded with a status of 409 ") {
				continue
			}
			probs = append(probs, pr)
		}
		if len(probs) > 0 {
			t.Errorf("%s: console errors or policy violations: %q", where, probs)
		}
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
		p.Eval(`({path: location.pathname, h1: (document.querySelector("h1") || {}).textContent || "",
			width: document.documentElement.clientWidth, scroll: document.documentElement.scrollWidth,
			script: document.documentElement.classList.contains("js")})`, &s)
		return s
	}
	eval := func(expr string, v any) {
		t.Helper()
		p.Eval(expr, v)
	}

	p.Viewport(1280, 900, false)
	p.ColorScheme("light")
	p.Navigate(base + "/")
	if s := state(); s.Path != "/login" || s.H1 != "Sign in" {
		t.Fatalf("before sign-in: %+v", s)
	}
	eval(`(() => { document.getElementById("password").value = `+fmt.Sprintf("%q", testPassword)+
		`; document.querySelector("form").submit(); return true })()`, nil)
	p.WaitLoad()
	if s := state(); s.Path != "/" || s.H1 != "Overview" || !s.Script {
		t.Fatalf("after sign-in: %+v", s)
	}

	// Every page at both widths in both appearances.
	f.be.mu.Lock()
	f.be.restart = []string{"proxy.listen"}
	f.be.mu.Unlock()
	pages := []struct{ path, h1, name string }{
		{"/", "Overview", "overview"},
		{"/domains", "Domains", "domains"},
		{"/live", "Live", "live"},
		{"/settings", "Advanced", "advanced"},
		{"/gallery", "Components", "gallery"},
	}
	listen := []string{`"127.0.0.1:41728"`, `"127.0.0.1:41730"`}
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
				clean(name + suffix)
				// Shoot the steady state, after toasts and dialogs move in.
				eval(`Promise.all(document.getAnimations().map((a) => a.finished)).then(() => true)`, nil)
				shoot(name + suffix)
			}
			for _, pg := range pages {
				p.Navigate(base + pg.path)
				check(pg.name, pg.h1)
			}
			// A save that needs a restart: a toast and the shell's banner.
			p.Navigate(base + "/settings")
			text := strings.Replace(readFile(t, f.be.path), listen[0], listen[1], 1)
			listen[0], listen[1] = listen[1], listen[0]
			eval(`(() => { document.getElementById("text").value = `+fmt.Sprintf("%q", text)+
				`; document.querySelector("[data-primary]").click(); return true })()`, nil)
			p.WaitLoad()
			var shown struct {
				Toast  string `json:"toast"`
				Banner string `json:"banner"`
			}
			eval(`({toast: (document.querySelector(".toast.warn") || {}).textContent || "",
				banner: (document.querySelector(".banners .banner.warn") || {}).textContent || ""})`, &shown)
			if !strings.Contains(shown.Toast, "Restart required for: proxy.listen.") || !strings.Contains(shown.Banner, "proxy.listen changes apply at the next start.") {
				t.Errorf("save%s: toast %q, banner %q", suffix, shown.Toast, shown.Banner)
			}
			check("advanced-saved", "Advanced")
		}
	}
	f.be.mu.Lock()
	f.be.restart = nil
	f.be.mu.Unlock()

	p.Viewport(1280, 900, false)
	p.ColorScheme("light")

	// List input: three chips from the textarea, the refused entry marked;
	// a paste adds three more, a removal takes one away, and Discard
	// restores the form. The bar names the form's primary action.
	p.Navigate(base + "/gallery")
	type listState struct {
		Chips   int    `json:"chips"`
		Bad     int    `json:"bad"`
		Value   string `json:"value"`
		Bar     bool   `json:"bar"`
		Message string `json:"message"`
		Action  string `json:"action"`
	}
	readList := `({chips: document.querySelectorAll("#g-domains + .tokens .tok").length,
		bad: document.querySelectorAll("#g-domains + .tokens .tok.bad").length,
		value: document.getElementById("g-domains").value,
		bar: !document.getElementById("savebar").hidden,
		message: document.getElementById("savebar-msg").textContent,
		action: document.getElementById("savebar-submit").textContent})`
	var ls listState
	eval(readList, &ls)
	if ls.Chips != 3 || ls.Bad != 1 || ls.Bar {
		t.Errorf("list input at load: %+v", ls)
	}
	eval(`(() => { const i = document.getElementById("g-domains-entry"); i.focus();
		const dt = new DataTransfer(); dt.setData("text/plain", "a.example, b.example\nc.example internal.example");
		i.dispatchEvent(new ClipboardEvent("paste", {clipboardData: dt, bubbles: true, cancelable: true})); return true })()`, nil)
	eval(readList, &ls)
	if ls.Chips != 6 || ls.Value != "internal.example\nbuild.example\n*.wiki.example\na.example\nb.example\nc.example" ||
		!ls.Bar || ls.Message != "Unsaved changes to the example rule" || ls.Action != "Review changes" {
		t.Errorf("list input after a paste: %+v", ls)
	}
	shoot("gallery-edited-1280-light")
	eval(`(() => { document.querySelector('#g-domains + .tokens .tok[data-v="b.example"] button').click(); return true })()`, nil)
	eval(readList, &ls)
	if ls.Chips != 5 || strings.Contains(ls.Value, "b.example") {
		t.Errorf("list input after a removal: %+v", ls)
	}
	eval(`(() => { document.getElementById("savebar-discard").click(); return new Promise((r) => setTimeout(() => r(true), 50)); })()`, nil)
	eval(readList, &ls)
	if ls.Chips != 3 || ls.Bad != 1 || ls.Bar || ls.Value != "internal.example\nbuild.example\n*.wiki.example" {
		t.Errorf("list input after Discard: %+v", ls)
	}

	// A dialog opens over the page; Escape closes it and returns the
	// focus to its opener.
	var open, back bool
	eval(`(() => { document.querySelector('[data-dialog="review-demo"]').click(); return document.getElementById("review-demo").open; })()`, &open)
	shoot("gallery-dialog-1280-light")
	p.Key("Escape", 27)
	// The close event, which returns the focus, is a task after Escape.
	eval(`new Promise((r) => setTimeout(() => r(!document.getElementById("review-demo").open &&
		document.activeElement === document.querySelector('[data-dialog="review-demo"]')), 100))`, &back)
	if !open || !back {
		t.Errorf("dialog: opened %v, closed with the focus on its opener %v", open, back)
	}

	// The ok toast closes after 8 s; the warn toast stays until closed.
	var toasts [2]int
	eval(`new Promise((r) => setTimeout(() => r([document.querySelectorAll(".toast.ok").length, document.querySelectorAll(".toast.warn").length]), 8600))`, &toasts)
	if toasts != [2]int{0, 1} {
		t.Errorf("toasts after 8.6 s: ok %d, warn %d; want 0 and 1", toasts[0], toasts[1])
	}
	var left int
	eval(`(() => { document.querySelector(".toast.warn button").click();
		return new Promise((r) => setTimeout(() => r(document.querySelectorAll(".toast").length), 400)); })()`, &left)
	if left != 0 {
		t.Errorf("%d toasts after Close", left)
	}

	// Validate loads only the result block: the page stays, keeps the
	// edited text, and marks it unsaved; Discard reloads the file.
	p.Navigate(base + "/settings")
	file := readFile(t, f.be.path)
	eval(`(() => { window.__stay = true; const a = document.getElementById("text"); a.value = "name = 1\n";
		a.dispatchEvent(new Event("input", {bubbles: true}));
		const old = document.querySelector("#settings-result > [data-block]");
		document.querySelector('[data-fragment="settings-result"]').click();
		return new Promise((r) => { const t0 = Date.now(); (function poll() {
			if (document.querySelector("#settings-result > [data-block]") !== old || Date.now() - t0 > 5000) r(true); else setTimeout(poll, 50); })(); }); })()`, nil)
	var v struct {
		Stay   bool   `json:"stay"`
		Path   string `json:"path"`
		Text   string `json:"text"`
		Errors string `json:"errors"`
		Bar    bool   `json:"bar"`
		Action string `json:"action"`
	}
	eval(`({stay: window.__stay === true, path: location.pathname, text: document.getElementById("text").value,
		errors: (document.querySelector("#settings-result") || {}).textContent || "",
		bar: !document.getElementById("savebar").hidden, action: document.getElementById("savebar-submit").textContent})`, &v)
	if !v.Stay || v.Path != "/settings" || v.Text != "name = 1\n" || !strings.Contains(v.Errors, "The configuration has errors; nothing was saved.") ||
		!v.Bar || v.Action != "Save and reload" {
		t.Errorf("Validate as a fragment: %+v", v)
	}
	clean("validate fragment")
	shoot("advanced-validated-1280-light")
	eval(`(() => { document.getElementById("savebar-discard").click(); return true; })()`, nil)
	p.WaitLoad()
	var reloaded string
	eval(`document.getElementById("text").value`, &reloaded)
	if reloaded != file {
		t.Errorf("Discard did not reload the file: %q", reloaded)
	}

	// The phone menu is a <details>: it opens, shows the navigation with
	// the current page, and Escape closes it.
	p.Viewport(390, 844, true)
	p.Navigate(base + "/domains")
	var menu [2]bool
	eval(`(() => { document.querySelector(".topbar summary").click();
		const a = document.querySelector('.topbar .nav a[aria-current="page"]');
		return [document.querySelector(".topbar details").open, !!a && a.offsetHeight > 0]; })()`, &menu)
	shoot("domains-menu-390-light")
	p.Key("Escape", 27)
	var closed bool
	eval(`!document.querySelector(".topbar details").open`, &closed)
	if menu != [2]bool{true, true} || !closed {
		t.Errorf("phone menu: open and current %v, closed by Escape %v", menu, closed)
	}
	clean("interactions")

	// Without the script every control is the plain one: the list input
	// is its textarea, toasts sit in the page above the banners, the save
	// bar stays hidden, the phone menu still opens, and Validate submits
	// the whole page.
	p.Viewport(1280, 900, false)
	chrome.Call(p.Session, "Emulation.setScriptExecutionDisabled", map[string]any{"value": true}, nil)
	p.Navigate(base + "/gallery")
	var plain struct {
		Script   bool   `json:"script"`
		List     bool   `json:"list"`
		Chips    int    `json:"chips"`
		Toasts   string `json:"toasts"`
		Closable int    `json:"closable"`
		Bar      bool   `json:"bar"`
	}
	eval(`({script: document.documentElement.classList.contains("js"),
		list: document.getElementById("g-domains").offsetHeight > 0,
		chips: document.querySelectorAll(".tokens").length,
		toasts: getComputedStyle(document.getElementById("toasts")).position,
		closable: document.querySelectorAll(".toast button").length,
		bar: !document.getElementById("savebar").hidden})`, &plain)
	if plain.Script || !plain.List || plain.Chips != 0 || plain.Toasts != "static" || plain.Closable != 0 || plain.Bar {
		t.Errorf("gallery without the script: %+v", plain)
	}
	shoot("gallery-noscript-1280-light")
	p.Navigate(base + "/settings")
	eval(`(() => { document.getElementById("text").value = "name = 1\n";
		document.querySelector('[formaction="/settings/validate"]').click(); return true; })()`, nil)
	p.WaitLoad()
	if s := state(); s.Path != "/settings/validate" || s.H1 != "Advanced" {
		t.Errorf("Validate without the script: %+v", s)
	}
	p.Viewport(390, 844, true)
	p.Navigate(base + "/live")
	var opened bool
	eval(`(() => { document.querySelector(".topbar summary").click(); return document.querySelector(".topbar details").open; })()`, &opened)
	if !opened {
		t.Error("the phone menu does not open without the script")
	}
	clean("without the script")
	chrome.Call(p.Session, "Emulation.setScriptExecutionDisabled", map[string]any{"value": false}, nil)
	p.Viewport(1280, 900, false)
	p.Navigate(base + "/live")

	// The sign-in page, signed out, at both widths and appearances.
	eval(`(() => { document.querySelector('.topbar form[action="/logout"]').submit(); return true; })()`, nil)
	p.WaitLoad()
	for _, scheme := range []string{"light", "dark"} {
		p.ColorScheme(scheme)
		for _, w := range []struct {
			px     int
			mobile bool
		}{{1280, false}, {390, true}} {
			p.Viewport(w.px, 900, w.mobile)
			p.Navigate(base + "/login")
			s := state()
			if s.H1 != "Sign in" || s.Scroll > s.Width {
				t.Errorf("sign-in at %d px, %s: %+v", w.px, scheme, s)
			}
			clean(fmt.Sprintf("sign-in at %d px, %s", w.px, scheme))
			shoot(fmt.Sprintf("login-%d-%s", w.px, scheme))
		}
	}
	chrome.Close()
	t.Logf("Chrome's requests through the blocking proxy: %d", proxied.Load())
}

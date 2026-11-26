package admin

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
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

	// Every page at both widths in both appearances. A previous version
	// exists, so the Advanced page offers it and /settings/backup shows
	// its diff.
	f.be.mu.Lock()
	f.be.restart = []string{"proxy.listen"}
	f.be.mu.Unlock()
	if err := os.WriteFile(f.be.path+".bak", []byte(strings.Replace(exampleConfig, `name = "ads"`, "name = \"ads\"\nnote = \"Before the last save\"", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	pages := []struct{ path, h1, name string }{
		{"/", "Overview", "overview"},
		{"/domains", "Domains", "domains"},
		{"/live", "Live", "live"},
		{"/settings", "Advanced", "advanced"},
		{"/settings/backup", "Previous version", "backup"},
		{"/gallery", "Components", "gallery"},
	}
	listenRE := regexp.MustCompile(`listen = \["127\.0\.0\.1:\d+"\]`)
	// dialog reports the open review dialog: whether it shows a diff with
	// added lines, its summary, and whether it or its body scrolls
	// sideways.
	type dialogState struct {
		Open    bool   `json:"open"`
		Adds    int    `json:"adds"`
		Summary string `json:"summary"`
		Notices string `json:"notices"`
		Wide    bool   `json:"wide"`
		Focus   string `json:"focus"`
	}
	readDialog := `(() => { const d = document.getElementById("review-dialog"); const b = d && d.querySelector(".dlg-body");
		return {open: !!d && d.open, adds: d ? d.querySelectorAll(".diff .dl.add").length : 0,
			summary: d && d.querySelector(".summary") ? d.querySelector(".summary").textContent : "",
			notices: d && d.querySelector(".notices") ? d.querySelector(".notices").textContent : "",
			wide: !!d && (d.scrollWidth > d.clientWidth || (!!b && b.scrollWidth > b.clientWidth)),
			focus: document.activeElement ? document.activeElement.textContent.trim() : ""}; })()`
	// openReview presses the element that sel names and waits until the
	// review dialog is open with its content.
	openReview := func(sel string) dialogState {
		t.Helper()
		var ds dialogState
		eval(`(() => { document.querySelector(`+fmt.Sprintf("%q", sel)+`).click();
			return new Promise((r) => { const t0 = Date.now(); (function poll() {
				const d = document.getElementById("review-dialog");
				if ((d && d.open && d.querySelector("[data-block]")) || Date.now() - t0 > 5000) r(true); else setTimeout(poll, 50); })(); }); })()`, nil)
		eval(`Promise.all(document.getAnimations().map((a) => a.finished)).then(() => true)`, nil)
		eval(readDialog, &ds)
		return ds
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
				clean(name + suffix)
				// Shoot the steady state, after toasts and dialogs move in.
				eval(`Promise.all(document.getAnimations().map((a) => a.finished)).then(() => true)`, nil)
				shoot(name + suffix)
			}
			for _, pg := range pages {
				p.Navigate(base + pg.path)
				// At desktop width a host stays on one line; on a phone a
				// stacked row starts each line at the same inset.
				var broken []string
				var offset []int
				var shown int
				eval(`[...document.querySelectorAll("td.host")].filter((td) => {
					const r = document.createRange(); r.selectNodeContents(td);
					return [...r.getClientRects()].filter((x) => x.width > 0).length > 1; }).map((td) => td.textContent)`, &broken)
				eval(`(() => {
					const inset = (td) => { const cs = getComputedStyle(td);
						return td.getBoundingClientRect().left + parseFloat(cs.borderLeftWidth) + parseFloat(cs.paddingLeft); };
					return [...document.querySelectorAll("table.resp tbody tr")].map((tr) => {
						const c = [...tr.children].filter((td) => getComputedStyle(td).display !== "none");
						return c.length < 2 ? 0 : Math.round(inset(c[0]) - inset(c[1]));
					}).filter((d) => d !== 0); })()`, &offset)
				if !w.mobile && pg.name != "gallery" && len(broken) > 0 {
					t.Errorf("%s%s: hosts break across lines: %q", pg.name, suffix, broken)
				}
				eval(`[...document.querySelectorAll("table.resp td.opt, table.resp td.barcol")].filter((td) => getComputedStyle(td).display !== "none").length`, &shown)
				if w.mobile && len(offset) > 0 {
					t.Errorf("%s%s: stacked rows start their lines at different insets: %v px", pg.name, suffix, offset)
				}
				if w.mobile && shown > 0 {
					t.Errorf("%s%s: %d cells that a phone hides are shown", pg.name, suffix, shown)
				}
				check(pg.name, pg.h1)
			}
			// A change that needs a restart, through the review dialog
			// (design 0009 D3.11): the editor with the pending change and
			// the save bar, the review with its summary, notices, and
			// diff, then Apply changes, the toast, and the banner. Each
			// save moves proxy.listen to a port other than the start one.
			p.Navigate(base + "/settings")
			cur := readFile(t, f.be.path)
			port := "41730"
			if strings.Contains(cur, "127.0.0.1:41730") {
				port = "41731"
			}
			text := listenRE.ReplaceAllString(cur, `listen = ["127.0.0.1:`+port+`"]`)
			eval(`(() => { const a = document.getElementById("text"); a.value = `+fmt.Sprintf("%q", text)+
				`; a.dispatchEvent(new Event("input", {bubbles: true})); return true })()`, nil)
			check("advanced-pending", "Advanced")
			ds := openReview(`.editor-form [data-primary]`)
			if !ds.Open || ds.Adds != 1 || !strings.Contains(ds.Summary, "Edited as text: 1 line added, 1 removed") ||
				!strings.Contains(ds.Notices, "proxy.listen changes apply at the next start.") || ds.Wide || ds.Focus != "Apply changes" {
				t.Errorf("review dialog%s: %+v", suffix, ds)
			}
			clean("review dialog" + suffix)
			shoot("review-dialog" + suffix)
			eval(`(() => { document.querySelector("#review-dialog .dlg-foot .btn.primary").click(); return true })()`, nil)
			p.WaitLoad()
			var shown struct {
				Toast  string `json:"toast"`
				Banner string `json:"banner"`
			}
			eval(`({toast: (document.querySelector(".toast.warn") || {}).textContent || "",
				banner: (document.querySelector(".banners .banner.warn") || {}).textContent || ""})`, &shown)
			if !strings.Contains(shown.Toast, "Restart required.") || !strings.Contains(shown.Toast, "proxy.listen changes apply at the next start.") ||
				!strings.Contains(shown.Banner, "proxy.listen changes apply at the next start.") {
				t.Errorf("save%s: toast %q, banner %q", suffix, shown.Toast, shown.Banner)
			}
			check("advanced-saved", "Advanced")
			if got := readFile(t, f.be.path); got != text {
				t.Errorf("apply%s wrote %q", suffix, got)
			}

			// The restore dialog from the previous version's page.
			p.Navigate(base + "/settings/backup")
			if ds := openReview(`.page-head form [value="review"]`); !ds.Open || !strings.Contains(ds.Summary, "Restores the previous version saved") || ds.Wide {
				t.Errorf("restore dialog%s: %+v", suffix, ds)
			}
			clean("restore dialog" + suffix)
			shoot("restore-dialog" + suffix)
			p.Key("Escape", 27)

			// The four file banners, in their order: the file has errors
			// (with the previous version to restore), or differs from the
			// running configuration; the last reload failed; restart
			// required.
			f.be.mu.Lock()
			f.be.lastErr = "line 31: rule[3].action: \"sideways\" is not \"direct\", \"block\", or \"upstream:<name>\""
			f.be.mu.Unlock()
			for _, b := range []struct{ name, file, first string }{
				{"banners-errors", strings.Replace(text, `action = "block"`, `action = "sideways"`, 1), "The configuration file has errors"},
				{"banners-differs", strings.Replace(text, `default = "direct"`, `default = "block"`, 1), "The file changed since cuttle loaded it."},
			} {
				if err := os.WriteFile(f.be.path, []byte(b.file), 0o600); err != nil {
					t.Fatal(err)
				}
				p.Navigate(base + "/live")
				var titles []string
				eval(`[...document.querySelectorAll(".banners .banner strong")].map((s) => s.textContent)`, &titles)
				if len(titles) != 3 || !strings.HasPrefix(titles[0], b.first) || !strings.HasPrefix(titles[1], "The last reload refused the file") ||
					!strings.Contains(titles[2], "changes apply at the next start.") {
					t.Errorf("%s%s: %q", b.name, suffix, titles)
				}
				check(b.name, "Live")
			}
			if err := os.WriteFile(f.be.path, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
			f.be.mu.Lock()
			f.be.lastErr = ""
			f.be.mu.Unlock()
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
		!v.Bar || v.Action != "Review changes" {
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

	// The review with the script (OI-0006 T-R3, design 0009 D3.10):
	// Command-Enter in the editor opens the review dialog without leaving
	// the page; Back to editing closes it, keeps the text, and returns the
	// focus; text with errors puts them in the result block and opens no
	// dialog; a review after the file changed warns that applying replaces
	// that change; Apply changes writes and the next page shows the toast
	// once.
	p.Navigate(base + "/settings")
	file = readFile(t, f.be.path)
	edited := strings.Replace(file, `default = "direct"`, `default = "block"`, 1)
	setText := func(s string) {
		t.Helper()
		eval(`(() => { window.__stay = true; const a = document.getElementById("text"); a.value = `+fmt.Sprintf("%q", s)+
			`; a.dispatchEvent(new Event("input", {bubbles: true})); a.focus(); return true })()`, nil)
	}
	setText(edited)
	for _, typ := range []string{"rawKeyDown", "keyUp"} {
		chrome.Call(p.Session, "Input.dispatchKeyEvent", map[string]any{"type": typ, "key": "Enter", "code": "Enter",
			"windowsVirtualKeyCode": 13, "modifiers": 4}, nil)
	}
	eval(`new Promise((r) => { const t0 = Date.now(); (function poll() { const d = document.getElementById("review-dialog");
		if ((d.open && d.querySelector("[data-block]")) || Date.now() - t0 > 5000) r(true); else setTimeout(poll, 50); })(); })`, nil)
	var ds dialogState
	eval(readDialog, &ds)
	var stay bool
	eval(`window.__stay === true`, &stay)
	if !ds.Open || ds.Adds != 1 || !stay || readFile(t, f.be.path) != file {
		t.Errorf("Command-Enter: dialog %+v, stayed %v", ds, stay)
	}
	shoot("review-cmd-enter-1280-light")
	var backTo struct {
		Open  bool   `json:"open"`
		Text  string `json:"text"`
		Focus string `json:"focus"`
	}
	eval(`(() => { document.querySelector("#review-dialog .dlg-foot [data-close]").click();
		return new Promise((r) => setTimeout(() => { const d = document.getElementById("review-dialog");
			r({open: d.open, text: document.getElementById("text").value, focus: document.activeElement.textContent.trim()}); }, 100)); })()`, &backTo)
	if backTo.Open || backTo.Text != edited || backTo.Focus != "Review changes" {
		t.Errorf("Back to editing: %+v", backTo)
	}
	setText(strings.Replace(edited, `action = "block"`, `action = "sideways"`, 1))
	var refusedReview struct {
		Open   bool   `json:"open"`
		Errors string `json:"errors"`
	}
	eval(`(() => { const old = document.querySelector("#settings-result > [data-block]");
		document.querySelector(".editor-form [data-primary]").click();
		return new Promise((r) => { const t0 = Date.now(); (function poll() {
			const now = document.querySelector("#settings-result > [data-block]");
			if (now !== old || Date.now() - t0 > 5000) r({open: document.getElementById("review-dialog").open,
				errors: now ? now.textContent : ""}); else setTimeout(poll, 50); })(); }); })()`, &refusedReview)
	if refusedReview.Open || !strings.Contains(refusedReview.Errors, "fix them, then review again") || !strings.Contains(refusedReview.Errors, "sideways") {
		t.Errorf("review of text with errors: %+v", refusedReview)
	}
	clean("refused review")
	setText(edited)
	if err := os.WriteFile(f.be.path, []byte(strings.Replace(file, `name = "cuttle-edge"`, `name = "cuttle-other"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	ds = openReview(`.editor-form [data-primary]`)
	if !strings.Contains(ds.Notices, "The file changed after you opened this page. Applying replaces that change with this text") {
		t.Errorf("review after a change: %+v", ds)
	}
	shoot("review-conflict-1280-light")
	for _, w := range []struct {
		px     int
		mobile bool
	}{{390, true}} {
		p.Viewport(w.px, 844, w.mobile)
		eval(`Promise.all(document.getAnimations().map((a) => a.finished)).then(() => true)`, nil)
		eval(readDialog, &ds)
		if ds.Wide {
			t.Errorf("review dialog at %d px scrolls sideways", w.px)
		}
		shoot("review-conflict-390-light")
	}
	p.Viewport(1280, 900, false)
	eval(`(() => { document.querySelector("#review-dialog .dlg-foot .btn.primary").click(); return true })()`, nil)
	p.WaitLoad()
	var after struct {
		Path  string `json:"path"`
		Toast string `json:"toast"`
	}
	eval(`({path: location.pathname, toast: (document.querySelector(".toast") || {}).textContent || ""})`, &after)
	if after.Path != "/settings" || !strings.Contains(after.Toast, "Saved and reloaded") || readFile(t, f.be.path) != edited {
		t.Errorf("Apply changes: %+v, file %q", after, readFile(t, f.be.path))
	}
	clean("review flow")
	p.Navigate(base + "/settings")
	eval(`document.querySelectorAll(".toast").length`, &left)
	if left != 0 {
		t.Errorf("the toast showed again: %d", left)
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

	// /live refreshes itself after 5 s, but not while its menu is open.
	p.Navigate(base + "/live")
	var kept [2]bool
	eval(`(() => { window.__stay = true; document.querySelector(".topbar summary").click();
		return new Promise((r) => setTimeout(() => r([window.__stay === true, document.querySelector(".topbar details").open]), 6500)); })()`, &kept)
	if kept != [2]bool{true, true} {
		t.Errorf("/live refreshed while its menu was open: page kept %v, menu open %v", kept[0], kept[1])
	}
	p.Key("Escape", 27)
	p.WaitLoad()
	var fresh bool
	eval(`window.__stay === undefined`, &fresh)
	if !fresh {
		t.Error("/live did not refresh after its menu closed")
	}
	clean("live refresh")

	// A refused fragment request says what happened, and keeps Try again:
	// a session that ended (403) offers to sign in; another refusal shows
	// the server's message.
	p.Viewport(1280, 900, false)
	refused := func(field, value string) (text string, signIn, again bool) {
		t.Helper()
		p.Navigate(base + "/settings")
		var r struct {
			Text   string `json:"text"`
			SignIn bool   `json:"signIn"`
			Again  bool   `json:"again"`
		}
		eval(`(() => { document.querySelector('.editor-form input[name="`+field+`"]').value = "`+value+`";
			document.querySelector('[data-fragment="settings-result"]').click();
			return new Promise((r) => { const t0 = Date.now(); (function poll() {
				const f = document.querySelector('#settings-result > [data-block="failed"]');
				if (f || Date.now() - t0 > 5000) r({text: f ? f.textContent : "", signIn: !!(f && f.querySelector('a[href="/login"]')),
					again: !!(f && [...f.querySelectorAll("button")].some((b) => b.textContent === "Try again"))});
				else setTimeout(poll, 50); })(); }); })()`, &r)
		// Chrome logs the refusal as a network error; it is the answer
		// under test here.
		p.Problems()
		return r.Text, r.SignIn, r.Again
	}
	if text, signIn, again := refused("csrf", "expired"); !strings.HasPrefix(text, "Your session ended or this page expired.") || !signIn || !again {
		t.Errorf("403 as a fragment: %q, sign-in link %v, Try again %v", text, signIn, again)
	}
	shoot("advanced-refused-1280-light")
	if text, signIn, again := refused("base", "zz"); !strings.HasPrefix(text, "cuttle refused the request: 400 Bad Request: missing or invalid base.") || signIn || !again {
		t.Errorf("400 as a fragment: %q, sign-in link %v, Try again %v", text, signIn, again)
	}

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
	// Review changes without the script is a page; Apply changes returns
	// to Advanced with the toast in the page.
	p.Navigate(base + "/settings")
	file = readFile(t, f.be.path)
	noscript := strings.Replace(file, `default = "block"`, `default = "direct"`, 1)
	eval(`(() => { document.getElementById("text").value = `+fmt.Sprintf("%q", noscript)+`;
		document.querySelector(".editor-form [data-primary]").click(); return true; })()`, nil)
	p.WaitLoad()
	var page struct {
		H1   string `json:"h1"`
		Adds int    `json:"adds"`
	}
	eval(`({h1: document.querySelector("h1").textContent, adds: document.querySelectorAll(".diff .dl.add").length})`, &page)
	if s := state(); s.Path != "/config/draft" || page.H1 != "Review: edit the configuration as text" || page.Adds != 1 {
		t.Errorf("review without the script: %+v %+v", s, page)
	}
	shoot("review-page-noscript-1280-light")
	eval(`(() => { document.querySelector(".review-form .btn.primary").click(); return true; })()`, nil)
	p.WaitLoad()
	var plainToast string
	eval(`(document.querySelector(".toast") || {}).textContent || ""`, &plainToast)
	if s := state(); s.Path != "/settings" || !strings.Contains(plainToast, "Saved and reloaded") || readFile(t, f.be.path) != noscript {
		t.Errorf("apply without the script: %+v, toast %q", s, plainToast)
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

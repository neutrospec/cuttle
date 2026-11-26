package admin

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/neutrospec/cuttle/internal/stats"
)

// TestAdminScreensRoute is the browser check of task E of design 0009
// (D3.1, D3.2, D3.12, D3.13): the overview with the route sentence and
// bar, the domains page with its rule filter, Route a host in the dialog
// of the domains and live pages, and the Account error summary, each at
// 1280 and 390 px in light and dark appearance without sideways scrolling
// or console errors; the overview of a window whose requests all failed
// (OI-0006 AD-R1); Route a host in four activations with the script,
// with "Where" showing only the chosen route's rules; /live not
// refreshing while the dialog is open; and Route a host as pages without
// the script.
//
// It starts a browser, so T0 and T1 skip it; scripts/admin-check runs it
// with CUTTLE_ADMIN_CHROME=1, and CUTTLE_ADMIN_CHROME_OUT names a
// directory for the screenshots.
func TestAdminScreensRoute(t *testing.T) {
	if os.Getenv("CUTTLE_ADMIN_CHROME") != "1" {
		t.Skip("starts headless Chrome; run scripts/admin-check")
	}
	s := newScreens(t)
	file := readFile(t, s.f.be.path)
	// openRoute presses the first Route… of the page and waits for the
	// dialog with its form.
	openRoute := func(sel string) {
		t.Helper()
		s.click(sel)
		s.wait("the route dialog", `document.getElementById("route-dialog").open && document.getElementById("route-form")`)
	}
	dialogWide := func() bool {
		var wide bool
		s.eval(`(() => { const d = document.getElementById("route-dialog"); const b = d.querySelector(".dlg-body");
			return d.scrollWidth > d.clientWidth || b.scrollWidth > b.clientWidth; })()`, &wide)
		return wide
	}
	for _, scheme := range []string{"light", "dark"} {
		s.p.ColorScheme(scheme)
		for _, w := range []struct {
			px     int
			mobile bool
		}{{1280, false}, {390, true}} {
			s.p.Viewport(w.px, 900, w.mobile)
			sfx := fmt.Sprintf("-%d-%s", w.px, scheme)
			s.p.Navigate(s.base + "/")
			if got := s.text(".hero-sentence"); !strings.HasPrefix(got, "In the last 24 hours, ") || !strings.Contains(got, "requests were blocked.") {
				t.Errorf("overview sentence%s: %q", sfx, got)
			}
			var parts int
			s.eval(`[...document.querySelectorAll(".routebar i")].filter((i) => i.getBoundingClientRect().width >= 2).length`, &parts)
			if parts != 3 {
				t.Errorf("overview bar%s: %d visible parts, want direct, gateway, and lab", sfx, parts)
			}
			s.check("overview"+sfx, "Overview")
			s.p.Navigate(s.base + "/domains?rule=restricted")
			if got := s.text("#f-rule option:checked"); got != "restricted" {
				t.Errorf("rule filter%s: %q", sfx, got)
			}
			s.check("domains-rule"+sfx, "Domains")
			s.p.Navigate(s.base + "/domains")
			openRoute(`a[data-fragment="route-editor"][aria-label="Route tracker.example…"]`)
			if dialogWide() {
				t.Errorf("route dialog from domains%s scrolls sideways", sfx)
			}
			s.clean("route dialog from domains" + sfx)
			s.shoot("route-domains" + sfx)
			s.press("Escape", "Escape", 27, 0)
			s.p.Navigate(s.base + "/live")
			openRoute(`a[data-fragment="route-editor"][aria-label="Route www.example.com…"]`)
			if dialogWide() {
				t.Errorf("route dialog from live%s scrolls sideways", sfx)
			}
			s.clean("route dialog from live" + sfx)
			s.shoot("route-live" + sfx)
			s.press("Escape", "Escape", 27, 0)
			s.p.Navigate(s.base + "/account")
			s.click(`.account-form [data-primary]`)
			s.p.WaitLoad()
			var focused bool
			s.eval(`document.activeElement === document.querySelector(".error-summary")`, &focused)
			if !focused || !strings.Contains(s.text(".error-summary"), "Current password: Enter the current password.") {
				t.Errorf("account error summary%s: focused %v, %q", sfx, focused, s.text(".error-summary"))
			}
			s.check("account-errors"+sfx, "Account")
		}
	}
	s.p.ColorScheme("light")
	s.p.Viewport(1280, 900, false)

	// Four activations from the domains page: Route…, the route, Review
	// changes, and Apply changes. "Where" follows the chosen route.
	s.p.Navigate(s.base + "/domains")
	openRoute(`a[data-fragment="route-editor"][aria-label="Route tracker.example…"]`)
	var form struct {
		Now      string   `json:"now"`
		Disabled []string `json:"disabled"`
		Match    string   `json:"match"`
		Where    []string `json:"where"`
		Focus    string   `json:"focus"`
	}
	read := func() {
		t.Helper()
		s.eval(`(() => { const f = document.getElementById("route-form");
			return {now: f.querySelector(".verdict.now").textContent.replace(/\s+/g, " ").trim(),
				disabled: [...f.querySelectorAll('input[name="action"]:disabled')].map((i) => i.value),
				match: (f.querySelector('input[name="match"]:checked') || {}).value || "",
				where: [...f.elements.where.options].map((o) => o.textContent),
				focus: document.activeElement ? document.activeElement.name || "" : ""}; })()`, &form)
	}
	read()
	if form.Now != "Currently block by rule 3, ads, for HTTPS from a local client" || strings.Join(form.Disabled, ",") != "block" ||
		form.Match != "tracker.example" || strings.Join(form.Where, "|") != "New rule, first in the chain" || form.Focus != "match" {
		t.Errorf("route form: %+v", form)
	}
	s.click(`#route-form input[name="action"][value="upstream:gateway"]`)
	read()
	if strings.Join(form.Where, "|") != "New rule, first in the chain|Add to rule restricted" {
		t.Errorf("where for upstream gateway: %q", form.Where)
	}
	s.click(`#route-form input[name="action"][value="direct"]`)
	read()
	if strings.Join(form.Where, "|") != "New rule, first in the chain|Add to rule loopback" {
		t.Errorf("where for direct: %q", form.Where)
	}
	s.shoot("route-domains-chosen-1280-light")
	s.click(`#route-form [data-primary]`)
	s.wait("the review over the route dialog", `document.getElementById("review-dialog").open && document.querySelector("#review-dialog .summary")`)
	var both [2]bool
	s.eval(`[document.getElementById("route-dialog").open, document.getElementById("review-dialog").open]`, &both)
	if sum := s.text("#review-dialog .summary"); both != [2]bool{true, true} || !strings.Contains(sum, "tracker.example") {
		t.Errorf("review: dialogs %v, summary %q", both, sum)
	}
	s.shoot("route-review-1280-light")
	toast := s.apply()
	if st := s.state(); st.Path != "/domains" || !strings.Contains(toast, "Saved and reloaded") {
		t.Errorf("after apply: %+v, toast %q", st, toast)
	}
	if got := s.fileRules(); got != "tracker.example loopback restricted ads lab-net" {
		t.Errorf("rules after Route a host: %s", got)
	}
	s.clean("route flow")
	s.write(file)

	// /live does not refresh while the dialog is open.
	s.p.Navigate(s.base + "/live")
	s.eval(`(() => { window.__stay = true; return true; })()`, nil)
	openRoute(`a[data-fragment="route-editor"][aria-label="Route video.example…"]`)
	var kept bool
	s.eval(`new Promise((r) => setTimeout(() => r(window.__stay === true && document.getElementById("route-dialog").open), 6500))`, &kept)
	if !kept {
		t.Error("/live refreshed while Route a host was open")
	}
	s.press("Escape", "Escape", 27, 0)
	s.clean("live with the dialog")

	// Without the script: Route… opens the page, the review is a page, and
	// Apply changes returns to the domains page.
	s.chrome.Call(s.p.Session, "Emulation.setScriptExecutionDisabled", map[string]any{"value": true}, nil)
	s.p.Navigate(s.base + "/domains")
	s.click(`a[data-fragment="route-editor"][aria-label="Route ads.example.net…"]`)
	s.p.WaitLoad()
	if st := s.state(); st.Path != "/rules/new" || st.H1 != "Route ads.example.net" {
		t.Errorf("Route… without the script: %+v", st)
	}
	var groups []string
	s.eval(`[...document.querySelectorAll("#f-where optgroup")].map((g) => g.label)`, &groups)
	if strings.Join(groups, "|") != "Rules that route direct|Rules that route through gateway|Rules that route through lab|Rules that route to block" {
		t.Errorf("where without the script: %q", groups)
	}
	s.check("route-page-noscript-1280-light", "Route")
	s.click(`#route-form input[name="action"][value="direct"]`)
	s.click(`#route-form input[name="match"][value="example.net"]`)
	s.click(`#route-form [data-primary]`)
	s.p.WaitLoad()
	if st := s.state(); st.Path != "/config/draft" || !strings.HasPrefix(st.H1, "Review: route example.net direct") {
		t.Errorf("review without the script: %+v", st)
	}
	s.click(".review-form .btn.primary")
	s.p.WaitLoad()
	if st := s.state(); st.Path != "/domains" || !strings.Contains(s.text(".toast"), "Saved and reloaded") {
		t.Errorf("apply without the script: %+v %q", st, s.text(".toast"))
	}
	if got := s.fileRules(); got != "example.net loopback restricted ads lab-net" {
		t.Errorf("rules after Route a host without the script: %s", got)
	}
	s.clean("route without the script")
	s.chrome.Call(s.p.Session, "Emulation.setScriptExecutionDisabled", map[string]any{"value": false}, nil)
	s.write(file)

	// A window whose requests all failed and moved no bytes (OI-0006
	// AD-R1): the sentence and the legend, not the empty state.
	st := s.f.withStats()
	record(st, stats.RowKey{Host: "video.example", Action: "direct", Rule: "default", Source: "local"}, 4, true, true, 0, 0)
	record(st, stats.RowKey{Host: "www.example.com", Action: "direct", Rule: "default", Source: "local"}, 1, true, true, 0, 0)
	record(st, stats.RowKey{Host: "wiki.internal.example", Action: "upstream", Upstream: "gateway", Rule: "restricted", Source: "local"}, 3, true, true, 0, 0)
	flush(t, st)
	for _, v := range []struct {
		px     int
		mobile bool
		scheme string
	}{{1280, false, "light"}, {390, true, "dark"}} {
		s.p.ColorScheme(v.scheme)
		s.p.Viewport(v.px, 900, v.mobile)
		s.p.Navigate(s.base + "/")
		sfx := fmt.Sprintf("-%d-%s", v.px, v.scheme)
		if got := s.text(".hero-sentence"); got != "In the last 24 hours, no traffic went out. 5 requests went direct and 3 through gateway but moved no bytes." {
			t.Errorf("overview without bytes%s: %q", sfx, got)
		}
		var empty, legend int
		s.eval(`document.querySelectorAll("main .empty").length`, &empty)
		s.eval(`document.querySelectorAll(".hero .legend li").length`, &legend)
		if empty != 0 || legend != 2 {
			t.Errorf("overview without bytes%s: %d empty states, %d legend entries", sfx, empty, legend)
		}
		s.check("overview-no-bytes"+sfx, "Overview")
	}
	s.chrome.Close()
}

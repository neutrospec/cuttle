package admin

import (
	"html/template"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// galleryPage adds the test-only component gallery (testdata/gallery.html)
// to the server's pages and returns a handler that serves it at /gallery
// with the D5 headers, ahead of the server's own routes.
func (f *fixture) galleryPage() http.Handler {
	f.t.Helper()
	src, err := os.ReadFile("testdata/gallery.html")
	if err != nil {
		f.t.Fatal(err)
	}
	f.s.pages["gallery"] = template.Must(template.Must(f.s.base.Clone()).Parse(string(src)))
	mux := http.NewServeMux()
	mux.Handle("/", f.s)
	mux.Handle("GET /gallery", f.s.headers(f.s.hostCheck(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.s.render(w, http.StatusOK, "gallery", pageData{Title: "Components", Token: "gallery", Page: "gallery",
			Toasts: []toast{
				{Kind: "ok", Title: "Saved and reloaded.", Lines: [][]seg{rich("Rule `restricted`: domains +1")}},
				{Kind: "warn", Title: "Saved and reloaded. Restart required.", Lines: [][]seg{rich("`proxy.listen` applies at the next start: `brew services restart cuttle`")}},
			}})
	}))))
	return mux
}

var (
	controlTagRE = regexp.MustCompile(`<(input|select|textarea)\b([^>]*)>`)
	attrRE       = regexp.MustCompile(`([a-zA-Z_:][-a-zA-Z0-9_:.]*)(?:="([^"]*)")?`)
	scriptRE     = regexp.MustCompile(`(?i)<script\b[^>]*>`)
	inlineRE     = regexp.MustCompile(`(?i)(<style\b|\sstyle=|\son[a-z]+=|javascript:)`)
)

func attrsOf(s string) map[string]string {
	m := map[string]string{}
	for _, a := range attrRE.FindAllStringSubmatch(s, -1) {
		m[strings.ToLower(a[1])] = a[2]
	}
	return m
}

// checkMarkup checks a rendered page against design 0009 W13: one h1,
// a label for every input, select, and textarea, nothing inline that the
// Content-Security-Policy refuses, and, on a session page, the skip link,
// aria-current on the navigation entry of current (empty for none), and
// the script as the only <script>, with a src.
func checkMarkup(t *testing.T, name, body string, session bool, current string) {
	t.Helper()
	if n := strings.Count(body, "<h1"); n != 1 {
		t.Errorf("%s: %d h1 elements, want 1", name, n)
	}
	if m := inlineRE.FindString(body); m != "" {
		t.Errorf("%s: inline style, handler, or javascript: URL %q", name, m)
	}
	for _, m := range controlTagRE.FindAllStringSubmatchIndex(body, -1) {
		tag := body[m[0]:m[1]]
		a := attrsOf(body[m[4]:m[5]])
		switch a["type"] {
		case "hidden", "submit", "button", "reset", "image":
			continue
		}
		labelled := a["aria-label"] != ""
		if ids := a["aria-labelledby"]; ids != "" {
			labelled = true
			for _, id := range strings.Fields(ids) {
				if !strings.Contains(body, `id="`+id+`"`) {
					t.Errorf("%s: %s names a missing label %q", name, tag, id)
				}
			}
		}
		if id := a["id"]; id != "" && strings.Contains(body, `<label for="`+id+`"`) {
			labelled = true
		}
		// A control inside an open <label> element is labelled by it.
		before := body[:m[0]]
		if open := strings.LastIndex(before, "<label"); open >= 0 && !strings.Contains(before[open:], "</label>") {
			labelled = true
		}
		if !labelled {
			t.Errorf("%s: no label for %s", name, tag)
		}
		if ids := a["aria-describedby"]; ids != "" {
			for _, id := range strings.Fields(ids) {
				if !strings.Contains(body, `id="`+id+`"`) {
					t.Errorf("%s: %s is described by a missing element %q", name, tag, id)
				}
			}
		}
	}
	scripts := scriptRE.FindAllString(body, -1)
	if !session {
		if len(scripts) != 0 {
			t.Errorf("%s: scripts %q on a page without a session", name, scripts)
		}
		return
	}
	if len(scripts) != 1 || scripts[0] != `<script src="/static/admin.js" defer>` || !strings.Contains(body, `<script src="/static/admin.js" defer></script>`) {
		t.Errorf("%s: scripts %q, want only the admin script", name, scripts)
	}
	if !strings.Contains(body, `<a class="skip" href="#main">`) || !strings.Contains(body, `<main id="main"`) {
		t.Errorf("%s: no skip link to the main content", name)
	}
	// The sidebar and the phone menu both list the pages; each marks the
	// current entry once.
	marked := regexp.MustCompile(`<a href="([^"]*)" aria-current="page">`).FindAllStringSubmatch(body, -1)
	want := 2
	if current == "" {
		want = 0
	}
	if len(marked) != want {
		t.Errorf("%s: %d entries with aria-current, want %d", name, len(marked), want)
	}
	for _, m := range marked {
		if m[1] != current {
			t.Errorf("%s: aria-current on %q, want %q", name, m[1], current)
		}
	}
}

// TestAdminMarkup checks W13 of design 0009 on every page of the shell
// in the states the M2 pages have: with and without statistics, with the
// shell's banners and a toast, after a bad request, after each settings
// action, and on the sign-in page with and without a password file.
func TestAdminMarkup(t *testing.T) {
	f := newFixture(t, true)
	b := f.browser()
	csrf := b.login()
	page := func(name, path string, code int, current string) {
		t.Helper()
		res := b.get(path)
		body := bodyOf(t, res)
		if res.StatusCode != code {
			t.Fatalf("%s: %d, want %d:\n%s", name, res.StatusCode, code, body)
		}
		checkMarkup(t, name, body, true, current)
	}
	// Without statistics.
	page("overview without statistics", "/", 200, "/")
	page("domains without statistics", "/domains", 200, "/domains")

	f.withExample()
	f.be.mu.Lock()
	f.be.restart = []string{"proxy.listen", "admin.listen"}
	f.be.lastErr = "line 3: proxy.default: unknown upstream \"nowhere\""
	f.be.mu.Unlock()
	for _, p := range []struct{ name, path, current string }{
		{"overview", "/", "/"},
		{"overview 7d", "/?window=7d", "/"},
		{"domains", "/domains", "/domains"},
		{"domains by route", "/domains?route=upstream:gateway&window=1h", "/domains"},
		{"domains by dates", "/domains?from=2026-11-25&to=2026-11-25", "/domains"},
		{"live", "/live", "/live"},
		{"advanced", "/settings", "/settings"},
	} {
		page(p.name, p.path, 200, p.current)
	}
	page("overview with a bad window", "/?window=2d", 400, "/")
	page("domains with a bad filter", "/domains?route=bogus", 400, "/domains")

	post := func(name, path string, text string, code int) string {
		t.Helper()
		_, base := b.settingsForm()
		res := b.do(req{method: http.MethodPost, path: path, form: url.Values{"csrf": {csrf}, "text": {text}, "base": {base}}})
		body := bodyOf(t, res)
		if res.StatusCode != code {
			t.Fatalf("%s: %d, want %d:\n%s", name, res.StatusCode, code, body)
		}
		checkMarkup(t, name, body, true, "/settings")
		return body
	}
	post("advanced validate", "/settings/validate", exampleConfig, 200)
	post("advanced invalid", "/settings/validate", "name = 1\n", 422)
	body := post("advanced saved", "/settings", strings.Replace(exampleConfig, `"127.0.0.1:41728"`, `"127.0.0.1:41730"`, 1), 200)
	if !strings.Contains(body, `class="toast warn" role="alert" data-toast="warn"`) {
		t.Errorf("a save that needs a restart shows no warn toast:\n%s", body)
	}

	// The gallery holds every component of the visual system.
	rec := httptest.NewRecorder()
	hr := httptest.NewRequest(http.MethodGet, "http://"+testHost+"/gallery", nil)
	f.galleryPage().ServeHTTP(rec, hr)
	if rec.Code != http.StatusOK {
		t.Fatalf("gallery: %d:\n%s", rec.Code, rec.Body)
	}
	checkMarkup(t, "gallery", rec.Body.String(), true, "")

	// The sign-in page, with and without a password file.
	res := f.browser().get("/login")
	checkMarkup(t, "login", bodyOf(t, res), false, "")
	g := newFixture(t, false)
	res = g.browser().get("/login")
	checkMarkup(t, "login without a password", bodyOf(t, res), false, "")
}

// TestShellNavigation checks the sidebar of design 0009 D2: the three
// groups in order, links for the pages that exist, unavailable entries
// for the pages that later stages add, the configuration counts, and
// the instance state at the bottom.
func TestShellNavigation(t *testing.T) {
	f := newFixture(t, true)
	f.withExample()
	b := f.browser()
	b.login()
	body := bodyOf(t, b.get("/live"))
	nav := body[strings.Index(body, `<nav class="nav"`):]
	nav = nav[:strings.Index(nav, "</nav>")]
	text := strings.Join(strings.Fields(tagRE.ReplaceAllString(nav, " ")), " ")
	want := "Traffic Overview Domains Live Configuration Rules 4 Upstreams 2 " +
		"Interfaces (not available yet) 2 Proxy (not available yet) Remote clients (not available yet) General (not available yet) " +
		"Admin Account (not available yet) Advanced"
	if text != want {
		t.Errorf("navigation:\n got %q\nwant %q", text, want)
	}
	for _, href := range []string{"/", "/domains", "/live", "/rules", "/upstreams", "/settings"} {
		if !strings.Contains(nav, `<a href="`+href+`"`) {
			t.Errorf("navigation lacks a link to %s", href)
		}
	}
	if n := strings.Count(nav, "<a "); n != 6 {
		t.Errorf("navigation has %d links, want 6", n)
	}
	for _, s := range []string{"<b>cuttle</b><span>cuttle-edge</span>", "Version 0.2.0, up 3 days", "Configuration loaded at 09:42"} {
		if !strings.Contains(body, s) {
			t.Errorf("shell lacks %q", s)
		}
	}
}

// TestShellBanners checks the banners that the instance state gives
// (design 0009 D2): the failed reload before restart required, each at
// most once, and neither while the state is clean.
func TestShellBanners(t *testing.T) {
	f := newFixture(t, true)
	b := f.browser()
	b.login()
	if body := bodyOf(t, b.get("/live")); !strings.Contains(body, `<div class="banners"></div>`) || strings.Contains(body, `class="banner `) {
		t.Errorf("banners without a reason:\n%s", body)
	}
	f.be.mu.Lock()
	f.be.restart = []string{"proxy.listen", "admin.listen", "proxy.remote.sources"}
	f.be.lastErr = "line 3: proxy.default: unknown upstream \"<nowhere>\""
	f.be.mu.Unlock()
	for _, p := range []string{"/", "/domains", "/live", "/settings"} {
		body := bodyOf(t, b.get(p))
		reload := strings.Index(body, "The last reload refused the file; the running configuration is unchanged.")
		restart := strings.Index(body, "<code>proxy.listen</code>, <code>admin.listen</code> and <code>proxy.remote.sources</code> changes apply at the next start.")
		if reload < 0 || restart < reload || strings.Count(body, "changes apply at the next start.") != 1 {
			t.Errorf("%s: banners out of order or missing (reload %d, restart %d)", p, reload, restart)
		}
		if !strings.Contains(body, "unknown upstream &#34;&lt;nowhere&gt;&#34;") || !strings.Contains(body, "<code>brew services restart cuttle</code>") {
			t.Errorf("%s: banner text not escaped or missing:\n%s", p, body)
		}
		if strings.Contains(body, `<a class="btn sm" href="/settings">Open Advanced</a>`) == (p == "/settings") {
			t.Errorf("%s: the Open Advanced action is wrong", p)
		}
	}
}

// TestAdminDomainsWindow checks the window of /domains (OI-0006 U-R3):
// the presets are links that drop a date range, the filter form keeps
// the preset in force, a date range marks no preset and replaces it, and
// clearing both dates returns to the 24h default.
func TestAdminDomainsWindow(t *testing.T) {
	f := newFixture(t, true)
	f.withExample()
	b := f.browser()
	b.login()
	get := func(q string) string {
		t.Helper()
		res := b.get("/domains?" + q)
		body := bodyOf(t, res)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("/domains?%s: %d:\n%s", q, res.StatusCode, body)
		}
		return body
	}
	current := regexp.MustCompile(`<a href="([^"]*)" aria-current="true">([^<]*)</a>`)
	hidden := regexp.MustCompile(`<input type="hidden" name="window" value="([^"]*)">`)

	// A preset: its link is current, and the form carries it.
	body := get("route=direct&window=7d")
	if m := current.FindStringSubmatch(body); m == nil || m[2] != "7d" || strings.Contains(m[1], "from=") {
		t.Errorf("7d: current preset %q", m)
	}
	if m := hidden.FindStringSubmatch(body); m == nil || m[1] != "7d" {
		t.Errorf("7d: the form carries %q", m)
	}
	if strings.Contains(body, `<select class="input" id="f-window"`) || strings.Contains(body, `<select name="window"`) {
		t.Error("the form still has a window select")
	}

	// A date range: no preset is current, the form has no window, and the
	// preset links drop the dates.
	body = get("route=direct&from=2026-11-25&to=2026-11-25")
	if m := current.FindStringSubmatch(body); m != nil {
		t.Errorf("date range: preset %q is current", m[2])
	}
	if hidden.MatchString(body) || !strings.Contains(body, `name="from" value="2026-11-25"`) {
		t.Error("date range: the form carries a window or loses the dates")
	}
	for _, m := range regexp.MustCompile(`<a href="(/domains\?[^"]*)"`).FindAllStringSubmatch(body, -1) {
		if strings.Contains(m[1], "from=") {
			t.Errorf("date range: link %s keeps the dates", m[1])
		}
	}
	if !strings.Contains(body, "2026-11-25 00:00 to 2026-11-25 13:00 UTC") {
		t.Error("date range: the exact range is missing")
	}

	// The form submitted from a preset with dates filled: the dates win.
	body = get("route=direct&interface=all&source=all&from=2026-11-25&to=2026-11-25&window=1h")
	if m := current.FindStringSubmatch(body); m != nil || !strings.Contains(body, "2026-11-25 00:00 to 2026-11-25 13:00 UTC") {
		t.Errorf("dates with a preset: current %q", m)
	}
	// The form submitted from a date range with both dates cleared: 24h.
	body = get("route=direct&interface=all&source=all&from=&to=")
	if m := current.FindStringSubmatch(body); m == nil || m[2] != "24h" {
		t.Errorf("cleared dates: current preset %q, want 24h", m)
	}
	// A preset link from a filtered date-range page keeps the filters.
	body = get("route=upstream:gateway&source=local&interface=all&from=2026-11-25&to=2026-11-25")
	if !strings.Contains(body, `<a href="/domains?interface=all&amp;route=upstream%3Agateway&amp;source=local&amp;window=1h">1h</a>`) {
		t.Errorf("date range: the 1h link does not keep the filters:\n%s", body)
	}
}

// TestAdminSaveToast checks the toast of a save on the Advanced page
// (design 0009 D7): validator warnings, which a file may keep for good,
// leave it an ok toast that closes by itself and names their count while
// the page lists them; restart-only keys make it a warn toast that stays,
// with the shell banner's wording.
func TestAdminSaveToast(t *testing.T) {
	f := newFixture(t, true)
	f.withExample()
	b := f.browser()
	csrf := b.login()
	save := func(text string) string {
		t.Helper()
		_, base := b.settingsForm()
		res := b.do(req{method: http.MethodPost, path: "/settings", form: url.Values{"csrf": {csrf}, "text": {text}, "base": {base}}})
		body := bodyOf(t, res)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("save: %d:\n%s", res.StatusCode, body)
		}
		return body
	}
	toastOf := func(body string) string {
		t.Helper()
		i := strings.Index(body, `<div class="toast `)
		if i < 0 {
			t.Fatalf("no toast:\n%s", body)
		}
		return body[i : i+strings.Index(body[i:], "</div></div>")]
	}
	// Two warnings: the example's upstreams have no interface list.
	body := save(strings.Replace(exampleConfig, `default = "direct"`, `default = "block"`, 1))
	if got := toastOf(body); !strings.HasPrefix(got, `<div class="toast ok" role="status" data-toast="ok">`) ||
		!strings.Contains(got, "<strong>Saved and reloaded with 2 warnings.</strong>") || !strings.Contains(got, "The file has 2 warnings; this page lists them.") {
		t.Errorf("save with warnings: toast %s", got)
	}
	if !strings.Contains(body, `<h2 id="warnings-h">Warnings <span class="count">2</span></h2>`) {
		t.Error("save with warnings: the page does not list them")
	}
	body = save(strings.Replace(readFile(t, f.be.path), `"127.0.0.1:41728"`, `"127.0.0.1:41730"`, 1))
	if got := toastOf(body); !strings.HasPrefix(got, `<div class="toast warn" role="alert" data-toast="warn">`) ||
		!strings.Contains(got, "<strong>Saved and reloaded. Restart required.</strong>") ||
		!strings.Contains(got, "<code>proxy.listen</code> changes apply at the next start. Restart with <code>brew services restart cuttle</code> to use the new values.") ||
		!strings.Contains(got, "The file has 2 warnings; this page lists them.") {
		t.Errorf("save that needs a restart: toast %s", got)
	}
}

// TestAdminLiveRefresh checks that /live refreshes itself every 5 s
// through the script, which can wait while a menu is open, and through a
// <noscript> meta refresh without it; no other page refreshes.
func TestAdminLiveRefresh(t *testing.T) {
	f := newFixture(t, true)
	b := f.browser()
	b.login()
	body := bodyOf(t, b.get("/live"))
	if !strings.Contains(body, `<noscript><meta http-equiv="refresh" content="5"></noscript>`) || !strings.Contains(body, `<body data-refresh="5">`) {
		t.Errorf("/live does not refresh:\n%s", body)
	}
	for _, p := range []string{"/", "/domains", "/settings"} {
		if body := bodyOf(t, b.get(p)); strings.Contains(body, "http-equiv") || strings.Contains(body, "data-refresh") {
			t.Errorf("%s refreshes", p)
		}
	}
}

// TestAdminFragment checks the fragment requests of design 0009 D10 on
// the Advanced page: with Cuttle-Fragment: 1, Validate answers only the
// result block, whose root carries data-block, with the same status,
// escaping, and security headers as the page; without it, the page.
func TestAdminFragment(t *testing.T) {
	f := newFixture(t, true)
	b := f.browser()
	csrf := b.login()
	_, base := b.settingsForm()
	validate := func(text string, fragment bool) (*http.Response, string) {
		t.Helper()
		r := req{method: http.MethodPost, path: "/settings/validate", form: url.Values{"csrf": {csrf}, "text": {text}, "base": {base}}}
		if fragment {
			r.header = http.Header{"Cuttle-Fragment": {"1"}}
		}
		res := b.do(r)
		return res, bodyOf(t, res)
	}
	res, body := validate(`name = "<b>"`+"\n[proxy]\ndefault = \"nowhere\"\n", true)
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.HasPrefix(body, `<div class="stack" data-block="settings-result">`) ||
		strings.Contains(body, "<html") || strings.Contains(body, "<textarea") || !strings.Contains(body, "The configuration has errors; nothing was saved.") ||
		!strings.Contains(body, "proxy.default: ") {
		t.Errorf("invalid text as a fragment: %d:\n%s", res.StatusCode, body)
	}
	if res.Header.Get("Vary") != "Cuttle-Fragment" || res.Header.Get("Content-Security-Policy") == "" || res.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("fragment headers: %v", res.Header)
	}
	res, body = validate(fakeConfig, true)
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(body, `<div class="stack" data-block="settings-result">`) ||
		!strings.Contains(body, "The configuration is valid. It has not been saved.") {
		t.Errorf("valid text as a fragment: %d:\n%s", res.StatusCode, body)
	}
	res, body = validate(fakeConfig, false)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "<!doctype html>") || !strings.Contains(body, `data-guard-dirty`) ||
		res.Header.Get("Vary") != "Cuttle-Fragment" {
		t.Errorf("validate without the header: %d %v", res.StatusCode, res.Header)
	}
	// A fragment request without a session gets the same 403 as a form.
	r := req{method: http.MethodPost, path: "/settings/validate", form: url.Values{"text": {fakeConfig}}, noCookies: true,
		header: http.Header{"Cuttle-Fragment": {"1"}}}
	if res := b.do(r); res.StatusCode != http.StatusForbidden {
		t.Errorf("fragment without a session: %d", res.StatusCode)
	}
	if body := bodyOf(t, b.get("/settings")); strings.Contains(body, "data-guard-dirty") {
		t.Error("the page as loaded is marked dirty")
	}
}

// TestShellFormats checks the shell's uptime and time formats.
func TestShellFormats(t *testing.T) {
	for d, want := range map[time.Duration]string{
		30 * time.Second:             "less than a minute",
		time.Minute:                  "1 minute",
		59 * time.Minute:             "59 minutes",
		time.Hour:                    "1 hour",
		25*time.Hour + 2*time.Minute: "25 hours 2 minutes",
		76 * time.Hour:               "3 days",
	} {
		if got := humanDuration(d); got != want {
			t.Errorf("humanDuration(%v) = %q, want %q", d, got, want)
		}
	}
	s := &Server{loc: time.UTC}
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	if got := s.when(now.Add(-2*time.Hour), now); got != "at 10:00" {
		t.Errorf("today: %q", got)
	}
	if got := s.when(now.Add(-13*time.Hour), now); got != "on 2026-11-24 at 23:00" {
		t.Errorf("yesterday: %q", got)
	}
	if got := rich("Restart with `brew services restart cuttle`."); len(got) != 3 || !got[1].Code || got[2].Text != "." {
		t.Errorf("rich: %+v", got)
	}
}

// TestContrastTokens checks the D11 contrast ratios of design 0009 in
// the stylesheet's light and dark tokens: text tokens 4.5:1 on every
// surface, each hue 4.5:1 on its soft background and on the surface,
// the hues and --line-strong 3:1 as UI colors, the primary button's
// text, and the banner and badge text on the soft backgrounds.
func TestContrastTokens(t *testing.T) {
	css, err := assets.ReadFile("static/admin.css")
	if err != nil {
		t.Fatal(err)
	}
	text := string(css)
	dark := strings.Index(text, "@media (prefers-color-scheme: dark)")
	if dark < 0 {
		t.Fatal("no dark tokens")
	}
	tokens := func(block string) map[string]string {
		m := map[string]string{}
		for _, v := range regexp.MustCompile(`--([a-z0-9-]+):\s*(#[0-9A-Fa-f]{6});`).FindAllStringSubmatch(block, -1) {
			m[v[1]] = v[2]
		}
		return m
	}
	light := tokens(text[:dark])
	end := strings.Index(text[dark:], "\n}\n")
	themes := map[string]map[string]string{"light": light, "dark": tokens(text[dark : dark+end])}
	hues := []string{"direct", "upstream", "block", "warn", "ok"}
	for name, tk := range themes {
		if len(tk) != 19 {
			t.Fatalf("%s: %d color tokens, want the 19 of design 0009 D11 and --on-ink", name, len(tk))
		}
		check := func(fg, bg string, min float64) {
			t.Helper()
			r := contrast(t, tk[fg], tk[bg])
			if r < min {
				t.Errorf("%s: --%s on --%s is %.2f:1, want %.1f:1", name, fg, bg, r, min)
			}
		}
		for _, fg := range []string{"ink", "ink-2", "muted"} {
			for _, bg := range []string{"paper", "surface", "surface-2"} {
				check(fg, bg, 4.5)
			}
		}
		for _, h := range hues {
			check(h, h+"-soft", 4.5)
			check(h, "surface", 4.5)
			check(h, "paper", 3)
			check("ink", h+"-soft", 4.5)
			check("ink-2", h+"-soft", 4.5)
		}
		check("line-strong", "surface", 3)
		check("line-strong", "paper", 3)
		check("on-ink", "ink", 4.5)
	}
}

// contrast is the WCAG 2 contrast ratio of two #RRGGBB colors.
func contrast(t *testing.T, a, b string) float64 {
	t.Helper()
	lum := func(hex string) float64 {
		if len(hex) != 7 {
			t.Fatalf("color %q", hex)
		}
		var c [3]float64
		for i := range c {
			v, err := strconv.ParseUint(hex[1+2*i:3+2*i], 16, 8)
			if err != nil {
				t.Fatal(err)
			}
			s := float64(v) / 255
			if s <= 0.03928 {
				c[i] = s / 12.92
			} else {
				c[i] = math.Pow((s+0.055)/1.055, 2.4)
			}
		}
		return 0.2126*c[0] + 0.7152*c[1] + 0.0722*c[2]
	}
	la, lb := lum(a), lum(b)
	return (math.Max(la, lb) + 0.05) / (math.Min(la, lb) + 0.05)
}

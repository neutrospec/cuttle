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

// screens is a signed-in headless Chrome on a server with the example
// configuration, traffic, and running state, for the browser checks of
// the configuration pages.
type screens struct {
	t      *testing.T
	f      *fixture
	chrome *testenv.Chrome
	p      *testenv.ChromePage
	base   string
	out    string
}

// layoutState is what a page check reads: the path, the h1, whether the
// page scrolls sideways, and whether the script ran.
type layoutState struct {
	Path   string `json:"path"`
	H1     string `json:"h1"`
	Width  int    `json:"width"`
	Scroll int    `json:"scroll"`
	Script bool   `json:"script"`
}

func newScreens(t *testing.T) *screens {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := newFixtureAt(t, true, l.Addr().String())
	f.withExample()
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
	s := &screens{t: t, f: f, chrome: chrome, p: chrome.NewPage(), base: "http://" + l.Addr().String(), out: os.Getenv("CUTTLE_ADMIN_CHROME_OUT")}
	s.p.Viewport(1280, 900, false)
	s.p.ColorScheme("light")
	s.p.Navigate(s.base + "/login")
	s.eval(`(() => { document.getElementById("password").value = `+fmt.Sprintf("%q", testPassword)+
		`; document.querySelector("form").submit(); return true })()`, nil)
	s.p.WaitLoad()
	if st := s.state(); st.Path != "/" || !st.Script {
		t.Fatalf("after sign-in: %+v", st)
	}
	return s
}

func (s *screens) eval(expr string, v any) {
	s.t.Helper()
	s.p.Eval(expr, v)
}

func (s *screens) state() layoutState {
	s.t.Helper()
	var st layoutState
	s.eval(`({path: location.pathname, h1: (document.querySelector("h1") || {}).textContent || "",
		width: document.documentElement.clientWidth, scroll: document.documentElement.scrollWidth,
		script: document.documentElement.classList.contains("js")})`, &st)
	return st
}

func (s *screens) shoot(name string) {
	s.t.Helper()
	if s.out != "" {
		s.eval(`Promise.all(document.getAnimations().map((a) => a.finished)).then(() => true)`, nil)
		s.p.Screenshot(filepath.Join(s.out, name+".png"))
	}
}

// clean reports console errors and policy violations, apart from the 422
// and 409 answers that the script expects for a refused draft.
func (s *screens) clean(where string) {
	s.t.Helper()
	var probs []string
	for _, pr := range s.p.Problems() {
		if strings.HasPrefix(pr, "log network: Failed to load resource: the server responded with a status of 422 ") ||
			strings.HasPrefix(pr, "log network: Failed to load resource: the server responded with a status of 409 ") {
			continue
		}
		probs = append(probs, pr)
	}
	if len(probs) > 0 {
		s.t.Errorf("%s: console errors or policy violations: %q", where, probs)
	}
}

// check checks the page in view: its h1, that it does not scroll
// sideways, and no console error; and shoots it.
func (s *screens) check(name, h1 string) {
	s.t.Helper()
	st := s.state()
	if !strings.HasPrefix(st.H1, h1) {
		s.t.Errorf("%s: h1 %q, want %q (%+v)", name, st.H1, h1, st)
	}
	if st.Scroll > st.Width {
		s.t.Errorf("%s scrolls sideways: content %d px, viewport %d px", name, st.Scroll, st.Width)
	}
	// On a phone the top bar keeps its height on a short page.
	var bar int
	s.eval(`(() => { const t = document.querySelector(".topbar"); return t && getComputedStyle(t).display !== "none" ? t.offsetHeight : 0; })()`, &bar)
	if bar > 60 {
		s.t.Errorf("%s: the top bar is %d px high", name, bar)
	}
	s.clean(name)
	s.shoot(name)
}

// wait polls the expression until it is true, for up to 5 s.
func (s *screens) wait(what, expr string) {
	s.t.Helper()
	var ok bool
	s.eval(`new Promise((r) => { const t0 = Date.now(); (function poll() {
		let v = false; try { v = !!(`+expr+`); } catch (e) {}
		if (v || Date.now() - t0 > 5000) r(v); else setTimeout(poll, 50); })(); })`, &ok)
	if !ok {
		s.t.Fatalf("timed out waiting for %s: %s", what, expr)
	}
}

// press presses a key with its DevTools code and key code.
func (s *screens) press(key, code string, vk, modifiers int) {
	s.t.Helper()
	for _, typ := range []string{"rawKeyDown", "keyUp"} {
		s.chrome.Call(s.p.Session, "Input.dispatchKeyEvent", map[string]any{"type": typ, "key": key, "code": code,
			"windowsVirtualKeyCode": vk, "modifiers": modifiers}, nil)
	}
}

// paste pastes text into the input with id, as a list input's entry.
func (s *screens) paste(id, text string) {
	s.t.Helper()
	s.eval(`(() => { const i = document.getElementById(`+fmt.Sprintf("%q", id)+`); i.focus();
		const dt = new DataTransfer(); dt.setData("text/plain", `+fmt.Sprintf("%q", text)+`);
		i.dispatchEvent(new ClipboardEvent("paste", {clipboardData: dt, bubbles: true, cancelable: true})); return true })()`, nil)
}

// typeInto sets the value of the element that sel names and fires input.
func (s *screens) typeInto(sel, value string) {
	s.t.Helper()
	s.eval(`(() => { const i = document.querySelector(`+fmt.Sprintf("%q", sel)+`); i.focus(); i.value = `+fmt.Sprintf("%q", value)+`;
		i.dispatchEvent(new Event("input", {bubbles: true})); return true })()`, nil)
}

// click clicks the element that sel names.
func (s *screens) click(sel string) {
	s.t.Helper()
	s.eval(`(() => { const e = document.querySelector(`+fmt.Sprintf("%q", sel)+`); if (!e) throw new Error("no "+`+fmt.Sprintf("%q", sel)+`); e.click(); return true })()`, nil)
}

func (s *screens) text(sel string) string {
	s.t.Helper()
	var v string
	s.eval(`(document.querySelector(`+fmt.Sprintf("%q", sel)+`) || {}).textContent || ""`, &v)
	return strings.Join(strings.Fields(v), " ")
}

// rowOrder returns the names of the chain's rows as the page shows them.
func (s *screens) rowOrder() string {
	s.t.Helper()
	var v []string
	s.eval(`[...document.querySelectorAll("#chain .rule[data-name]")].map((r) => r.dataset.name)`, &v)
	return strings.Join(v, " ")
}

// fileRules returns the names of the file's rules in order.
func (s *screens) fileRules() string {
	s.t.Helper()
	d, _, r := config.DocOf([]byte(readFile(s.t, s.f.be.path)), "x")
	if d == nil {
		s.t.Fatalf("invalid file: %v", r.Errors)
	}
	var names []string
	for _, x := range d.Rules {
		names = append(names, x.Name)
	}
	return strings.Join(names, " ")
}

// apply presses Apply changes in the open review dialog and waits for the
// page it returns to.
func (s *screens) apply() string {
	s.t.Helper()
	s.wait("the review dialog", `document.getElementById("review-dialog").open && document.querySelector("#review-dialog .dlg-foot .btn.primary")`)
	s.click("#review-dialog .dlg-foot .btn.primary")
	s.p.WaitLoad()
	return s.text(".toast")
}

func (s *screens) write(text string) {
	s.t.Helper()
	if err := os.WriteFile(s.f.be.path, []byte(text), 0o600); err != nil {
		s.t.Fatal(err)
	}
}

// TestAdminScreensRules is the browser check of the rules and upstreams
// pages of design 0009 (D3.3–D3.5, D10, D11): each page and state at 1280
// and 390 px in light and dark appearance without sideways scrolling or
// console errors (the rules list with a URL test, the rule editor with
// inline errors and a live test, reordering, the upstreams list, the add
// upstream dialog, and the empty states), and the flows with the script
// (Test a URL as you type, the filter, reordering by keyboard and by the
// row menu with one review, adding a rule with the live check and test,
// adding an upstream in the dialog, a rename that updates its users, the
// guarded delete) and without it (position fields, the editor's Test, the
// upstream page, the menu's delete).
//
// It starts a browser, so T0 and T1 skip it; scripts/admin-check runs it
// with CUTTLE_ADMIN_CHROME=1, and CUTTLE_ADMIN_CHROME_OUT names a
// directory for the screenshots.
func TestAdminScreensRules(t *testing.T) {
	if os.Getenv("CUTTLE_ADMIN_CHROME") != "1" {
		t.Skip("starts headless Chrome; run scripts/admin-check")
	}
	s := newScreens(t)
	pages := []struct{ path, h1, name string }{
		{"/rules", "Rules", "rules"},
		{"/rules?test=https://wiki.internal.example/", "Rules", "rules-tested"},
		{"/rules/restricted", "Edit rule", "rule-edit"},
		{"/rules/new", "Add rule", "rule-new"},
		{"/upstreams", "Upstreams", "upstreams"},
		{"/upstreams/new", "Add upstream", "upstream-new"},
		{"/upstreams/lab", "Edit upstream", "upstream-edit"},
	}
	noRules := "name = \"cuttle-edge\"\n\n[proxy]\nlisten = [\"127.0.0.1:41728\"]\ndefault = \"direct\"\n\n[admin]\nlisten = \"127.0.0.1:41729\"\n"
	for _, scheme := range []string{"light", "dark"} {
		s.p.ColorScheme(scheme)
		for _, w := range []struct {
			px     int
			mobile bool
		}{{1280, false}, {390, true}} {
			s.p.Viewport(w.px, 900, w.mobile)
			sfx := fmt.Sprintf("-%d-%s", w.px, scheme)
			for _, pg := range pages {
				s.p.Navigate(s.base + pg.path)
				s.check(pg.name+sfx, pg.h1)
			}

			// The rule editor with inline errors and a live test of a
			// draft that an earlier rule shadows.
			s.p.Navigate(s.base + "/rules/new")
			s.eval(`(() => { const p = document.getElementById("f-position"); p.value = "last"; p.dispatchEvent(new Event("change", {bubbles: true})); return true })()`, nil)
			s.paste("f-domains-entry", "media.example, *.bad.example")
			s.paste("f-hosts-entry", "localhost\nstream.example")
			s.typeInto("#rule-test-url", "http://localhost:3000/")
			s.wait("the live check", `document.querySelector("#f-domains-issues .err") && document.querySelector("#f-domains + .tokens .tok.bad")`)
			s.wait("the live test", `/Fix the errors to test this draft/.test(document.getElementById("rule-test").textContent)`)
			var name string
			s.eval(`document.getElementById("f-name").value`, &name)
			if name != "media.example" {
				t.Errorf("suggested name %q", name)
			}
			s.check("rule-new-errors"+sfx, "Add rule")
			// Without the bad entry: the test of the draft, which rule
			// loopback shadows, and the validator's warning in place.
			s.click(`#f-domains + .tokens .tok[data-v="*.bad.example"] button`)
			s.wait("the live test", `/matches first/.test(document.getElementById("rule-test").textContent) && document.querySelector("#f-hosts-issues .wrn")`)
			s.check("rule-new-test"+sfx, "Add rule")
			// Discard resets the form, so that leaving does not ask.
			s.click("#savebar-discard")
			s.p.WaitLoad()

			// Reordering: the lifted row by keyboard at desktop width, the
			// row menu on a phone, where the handles are hidden; then the
			// save bar.
			s.p.Navigate(s.base + "/rules")
			if !w.mobile {
				s.eval(`document.querySelector('.rule[data-name="ads"] .handle').focus()`, nil)
				s.press(" ", "Space", 32, 0)
				s.press("ArrowUp", "ArrowUp", 38, 0)
				s.check("rules-lifted"+sfx, "Rules")
				s.press(" ", "Space", 32, 0)
			} else {
				s.click(`.rule[data-name="ads"] details.r-menu > summary`)
				s.check("rules-menu"+sfx, "Rules")
				s.click(`.rule[data-name="ads"] [data-move="up"]`)
			}
			if got := s.rowOrder(); got != "loopback ads restricted lab-net" {
				t.Errorf("order after a move%s: %q", sfx, got)
			}
			s.wait("the save bar", `!document.getElementById("savebar").hidden`)
			if msg := s.text("#savebar-msg"); msg != "Rule order changed. Nothing is saved until you review and apply." {
				t.Errorf("save bar%s: %q", sfx, msg)
			}
			s.check("rules-reordered"+sfx, "Rules")
			s.click("#savebar-discard")
			s.p.WaitLoad()

			// The add-upstream dialog.
			s.p.Navigate(s.base + "/upstreams")
			s.click(`.page-head a[data-fragment="upstream-editor"]`)
			s.wait("the editor dialog", `document.getElementById("editor-dialog").open && document.getElementById("upstream-form")`)
			var wide bool
			s.eval(`(() => { const d = document.getElementById("editor-dialog"); return d.scrollWidth > d.clientWidth; })()`, &wide)
			if wide {
				t.Errorf("the upstream dialog%s scrolls sideways", sfx)
			}
			s.check("upstream-dialog"+sfx, "Upstreams")
			s.press("Escape", "Escape", 27, 0)

			// The rule and upstream editors with the interface list
			// editor: "Only these interfaces" and two entries added.
			for _, ed := range []struct{ path, name, h1 string }{{"/rules/loopback", "rule-iflist", "Edit rule"}, {"/upstreams/lab", "upstream-iflist", "Edit upstream"}} {
				s.p.Navigate(s.base + ed.path)
				s.click(`input[name="ifaces"][value="only"]`)
				for _, n := range []string{"wifi", "eth-a"} {
					s.eval(`(() => { const sel = document.querySelector("select[data-ifl-add]"); sel.value = `+fmt.Sprintf("%q", n)+`;
						sel.dispatchEvent(new Event("change", {bubbles: true})); return true })()`, nil)
				}
				s.wait("the list entries", `document.querySelectorAll('[data-iflist="ifaces_list"] li[data-n]').length === 2`)
				s.check(ed.name+sfx, ed.h1)
				s.click("#savebar-discard")
				s.p.WaitLoad()
			}

			// The empty states.
			file := readFile(t, s.f.be.path)
			s.write(noRules)
			s.p.Navigate(s.base + "/rules")
			s.check("rules-empty"+sfx, "Rules")
			s.p.Navigate(s.base + "/upstreams")
			s.check("upstreams-empty"+sfx, "Upstreams")
			s.write(file)
		}
	}
	s.p.ColorScheme("light")
	s.p.Viewport(1280, 900, false)

	// Test a URL as you type: the chain marks the rows.
	s.p.Navigate(s.base + "/rules")
	s.typeInto("#test-url", "https://wiki.internal.example/")
	s.wait("the test result", `document.querySelector("#rules-test .result") && document.querySelector("#chain.tested")`)
	var marks map[string]string
	s.eval(`Object.fromEntries([...document.querySelectorAll("#chain .rule")].map((r) => [r.dataset.name || "default",
		["passed", "matched", "unreached"].find((c) => r.classList.contains(c)) || ""]))`, &marks)
	if fmt.Sprint(marks) != "map[ads:unreached default:unreached lab-net:unreached loopback:passed restricted:matched]" {
		t.Errorf("marks after a test: %v", marks)
	}
	if got := s.text("#rules-test .res-head > span:last-child"); got != "Rule 2, restricted" {
		t.Errorf("test result head %q", got)
	}
	s.typeInto("#test-url", "example.com")
	s.wait("the default row", `document.querySelector("#chain .rule.default.matched")`)
	if got := s.text("#rules-test .res-note"); got != "Read as https://example.com." {
		t.Errorf("bare host note %q", got)
	}
	s.clean("test as you type")

	// The filter hides other rows and turns reordering off.
	s.typeInto("#rule-filter", "tracker")
	var filtered struct {
		Shown    []string `json:"shown"`
		Disabled bool     `json:"disabled"`
	}
	s.eval(`({shown: [...document.querySelectorAll("#chain .rule[data-name]")].filter((r) => r.offsetHeight > 0).map((r) => r.dataset.name),
		disabled: [...document.querySelectorAll("#chain .handle")].every((h) => h.disabled)})`, &filtered)
	if strings.Join(filtered.Shown, " ") != "ads" || !filtered.Disabled || !strings.Contains(s.text("#filter-meta"), "1 of 4 rules match; reordering is off") {
		t.Errorf("filter: %+v, %q", filtered, s.text("#filter-meta"))
	}
	s.shoot("rules-filtered-1280-light")
	s.typeInto("#rule-filter", "")

	// Reorder by keyboard and review once: two moves, one review.
	s.eval(`document.querySelector('.rule[data-name="lab-net"] .handle').focus()`, nil)
	s.press(" ", "Space", 32, 0)
	s.press("ArrowUp", "ArrowUp", 38, 0)
	s.press("ArrowUp", "ArrowUp", 38, 0)
	s.press(" ", "Space", 32, 0)
	var focused string
	s.eval(`document.activeElement.getAttribute("aria-label")`, &focused)
	if got := s.rowOrder(); got != "loopback lab-net restricted ads" || focused != "Move rule lab-net, position 2 of 4" {
		t.Errorf("keyboard move: %q, focus %q", got, focused)
	}
	// Escape puts a lifted rule back.
	s.press(" ", "Space", 32, 0)
	s.press("ArrowDown", "ArrowDown", 40, 0)
	s.press("Escape", "Escape", 27, 0)
	if got := s.rowOrder(); got != "loopback lab-net restricted ads" {
		t.Errorf("after Escape: %q", got)
	}
	// The test runs against the unsaved order.
	s.typeInto("#test-url", "http://lab.example.net/")
	s.wait("the test of the unsaved order", `document.querySelector('#chain .rule[data-name="lab-net"].matched')`)
	if got := s.text("#rules-test .res-head > span:last-child"); got != "Rule 2, lab-net" {
		t.Errorf("test against the unsaved order: %q", got)
	}
	s.click("#savebar-submit")
	s.wait("the review", `document.getElementById("review-dialog").open && document.querySelector("#review-dialog .summary")`)
	if got := s.text("#review-dialog .summary"); got != "Moved rule lab-net from 4 to 2" {
		t.Errorf("reorder summary %q", got)
	}
	s.shoot("rules-review-1280-light")
	if toast := s.apply(); !strings.Contains(toast, "Saved and reloaded") || s.fileRules() != "loopback lab-net restricted ads" {
		t.Errorf("reorder apply: toast %q, file %q", toast, s.fileRules())
	}
	s.clean("reorder")

	// Add a rule: chips from a paste, the live check, a fixed entry,
	// Command-Enter for the review, and apply.
	s.p.Navigate(s.base + "/rules/new")
	s.paste("f-domains-entry", "media.example, cdn.media.example *.bad.example")
	s.wait("the live check", `document.querySelector("#f-domains-issues .err")`)
	if got := s.text("#f-domains-issues"); !strings.Contains(got, "Entry 3:") {
		t.Errorf("live check: %q", got)
	}
	s.click(`#f-domains + .tokens .tok[data-v="*.bad.example"] button`)
	s.wait("the fixed field", `!document.querySelector("#f-domains-issues .err") && !document.querySelector("#form-issues .error-summary")`)
	s.click(`label:has(input[name="action"][value="block"])`)
	s.eval(`(() => { const p = document.getElementById("f-position"); p.value = "last"; p.dispatchEvent(new Event("change", {bubbles: true})); p.focus(); return true })()`, nil)
	s.press("Enter", "Enter", 13, 4)
	s.wait("the review", `document.getElementById("review-dialog").open && document.querySelector("#review-dialog .summary")`)
	if got := s.text("#review-dialog .summary"); got != "Rule media.example added at position 5" {
		t.Errorf("add review summary %q", got)
	}
	if toast := s.apply(); !strings.Contains(toast, "Saved and reloaded") || s.fileRules() != "loopback lab-net restricted ads media.example" {
		t.Errorf("add apply: toast %q, file %q", toast, s.fileRules())
	}
	s.clean("add a rule")

	// The interface list editor of a rule with the script (W21): add,
	// move, and remove in place, then one review.
	s.p.Navigate(s.base + "/rules/loopback")
	s.click(`input[name="ifaces"][value="only"]`)
	for _, n := range []string{"wifi", "eth-a"} {
		s.eval(`(() => { const sel = document.querySelector("select[data-ifl-add]"); sel.value = `+fmt.Sprintf("%q", n)+`;
			sel.dispatchEvent(new Event("change", {bubbles: true})); return true })()`, nil)
	}
	s.click(`[data-iflist="ifaces_list"] li[data-n="eth-a"] [data-ifl="up"]`)
	var entries []string
	s.eval(`[...document.querySelectorAll('[data-iflist="ifaces_list"] li[data-n] input')].map((i) => i.value)`, &entries)
	if strings.Join(entries, ",") != "eth-a,wifi" {
		t.Errorf("list after Add and Move up: %q", entries)
	}
	s.click(`[data-iflist="ifaces_list"] li[data-n="wifi"] [data-ifl="remove"]`)
	s.click(`[data-iflist="ifaces_list"] .iflist-add select`)
	s.eval(`(() => { const sel = document.querySelector("select[data-ifl-add]"); sel.value = "wifi";
		sel.dispatchEvent(new Event("change", {bubbles: true})); return true })()`, nil)
	s.click(`#rule-form [data-primary]`)
	s.wait("the review", `document.getElementById("review-dialog").open && document.querySelector("#review-dialog .summary")`)
	s.apply()
	if d, _, _ := config.DocOf([]byte(readFile(t, s.f.be.path)), "x"); d == nil || strings.Join(d.Rules[0].Interfaces.V, ",") != "eth-a,wifi" {
		t.Errorf("rule interfaces after apply: %+v", d.Rules[0].Interfaces)
	}
	s.clean("rule interface list")

	// A refused review leaves its errors only until a later review
	// succeeds (OI-0006 AA-R1): on Advanced, which has no live check.
	s.p.Navigate(s.base + "/settings")
	file := readFile(t, s.f.be.path)
	s.typeInto("#text", strings.Replace(file, `action = "block"`, `action = "sideways"`, 1))
	s.click(".editor-form [data-primary]")
	s.wait("the refusal", `/fix them, then review again/.test(document.getElementById("settings-result").textContent)`)
	s.typeInto("#text", strings.Replace(file, `name = "cuttle-edge"`, `name = "cuttle-other"`, 1))
	s.click(".editor-form [data-primary]")
	s.wait("the review", `document.getElementById("review-dialog").open && document.querySelector("#review-dialog .summary")`)
	if got := s.text("#settings-result"); strings.Contains(got, "fix them, then review again") || strings.Contains(got, "sideways") {
		t.Errorf("the refusal stays behind the dialog: %q", got)
	}
	s.click("#review-dialog .dlg-foot [data-close]")
	if got := s.text("#settings-result"); strings.Contains(got, "sideways") {
		t.Errorf("the refusal is back after Back to editing: %q", got)
	}
	s.click("#savebar-discard")
	s.p.WaitLoad()
	s.clean("AA-R1")

	// Add an upstream in the dialog: no disclosure first, then via.
	s.p.Navigate(s.base + "/upstreams")
	s.click(`.page-head a[data-fragment="upstream-editor"]`)
	s.wait("the editor dialog", `document.getElementById("editor-dialog").open && document.getElementById("f-name")`)
	s.typeInto("#f-name", "spare")
	s.typeInto("#f-address", "192.0.2.50:3128")
	s.click(`#upstream-form [data-primary]`)
	s.wait("the disclosure error", `document.querySelector("#f-disclosure-issues .err")`)
	var dlgOpen [2]bool
	s.eval(`[document.getElementById("editor-dialog").open, document.getElementById("review-dialog").open]`, &dlgOpen)
	if dlgOpen != [2]bool{true, false} || !strings.HasPrefix(s.text("#f-disclosure-issues"), "required") {
		t.Errorf("review without a disclosure: dialogs %v, %q", dlgOpen, s.text("#f-disclosure-issues"))
	}
	s.shoot("upstream-dialog-errors-1280-light")
	s.click(`#upstream-form input[name="disclosure"][value="via"]`)
	s.click(`#upstream-form [data-primary]`)
	s.wait("the review", `document.getElementById("review-dialog").open && document.querySelector("#review-dialog .summary")`)
	if got := s.text("#review-dialog .summary"); got != "Upstream spare added" {
		t.Errorf("add upstream summary %q", got)
	}
	s.shoot("upstream-review-1280-light")
	if toast := s.apply(); !strings.Contains(toast, "Saved and reloaded") || !strings.Contains(readFile(t, s.f.be.path), `name = "spare"`) {
		t.Errorf("add upstream apply: toast %q", toast)
	}

	// Rename with the reference update: lab becomes lab-b in rule lab-net.
	s.click(`.ent a.e-name[href="/upstreams/lab"]`)
	s.wait("the editor dialog", `document.getElementById("editor-dialog").open && document.getElementById("f-name")`)
	s.typeInto("#f-name", "lab-b")
	s.click(`#upstream-form [data-primary]`)
	s.wait("the review", `document.getElementById("review-dialog").open && document.querySelector("#review-dialog .summary")`)
	if got := s.text("#review-dialog .summary"); !strings.HasPrefix(got, "Upstream renamed lab → lab-b") {
		t.Errorf("rename summary %q", got)
	}
	s.apply()
	if f := readFile(t, s.f.be.path); !strings.Contains(f, `action = "upstream:lab-b"`) || strings.Contains(f, `"upstream:lab"`) {
		t.Errorf("rename did not update the rule:\n%s", f)
	}

	// Guarded delete: a used upstream's menu has no Delete and names its
	// users; the unused one deletes through the review.
	s.click(`.ent:has(a[href="/upstreams/gateway"]) details.r-menu > summary`)
	if got := s.text(`.ent:has(a[href="/upstreams/gateway"]) .m-note`); got != "Used by rule restricted. Change those first." {
		t.Errorf("used upstream's menu: %q", got)
	}
	var disabled bool
	s.eval(`document.querySelector('.ent:has(a[href="/upstreams/gateway"]) .menu button.danger').disabled`, &disabled)
	if !disabled {
		t.Error("the used upstream's Delete is enabled")
	}
	s.shoot("upstreams-menu-used-1280-light")
	s.press("Escape", "Escape", 27, 0)
	s.click(`.ent:has(a[href="/upstreams/spare"]) details.r-menu > summary`)
	s.click(`.ent:has(a[href="/upstreams/spare"]) .menu button.danger`)
	s.wait("the review", `document.getElementById("review-dialog").open && document.querySelector("#review-dialog .summary")`)
	if got := s.text("#review-dialog .summary"); got != "Upstream spare removed" {
		t.Errorf("delete summary %q", got)
	}
	s.apply()
	if strings.Contains(readFile(t, s.f.be.path), `name = "spare"`) {
		t.Error("the delete did not remove the upstream")
	}
	s.clean("upstreams")

	// Without the script: the position fields, the editor's Test button,
	// the upstream page, and the menu's delete.
	s.chrome.Call(s.p.Session, "Emulation.setScriptExecutionDisabled", map[string]any{"value": true}, nil)
	s.p.Navigate(s.base + "/rules")
	var plain struct {
		Script  bool `json:"script"`
		Pos     int  `json:"pos"`
		Handles int  `json:"handles"`
		Review  bool `json:"review"`
	}
	s.eval(`({script: document.documentElement.classList.contains("js"),
		pos: [...document.querySelectorAll('#chain input[name="position"]')].filter((i) => i.offsetHeight > 0).length,
		handles: [...document.querySelectorAll("#chain .handle")].filter((h) => h.offsetHeight > 0).length,
		review: document.querySelector(".order-foot [data-primary]").offsetHeight > 0})`, &plain)
	if plain.Script || plain.Pos != 5 || plain.Handles != 0 || !plain.Review {
		t.Errorf("rules without the script: %+v", plain)
	}
	// Each cell keeps its column without the handle: the name and the
	// position field stay clear of the route, and the main cell is wide.
	for _, scheme := range []string{"light", "dark"} {
		s.p.ColorScheme(scheme)
		for _, w := range []struct {
			px     int
			mobile bool
		}{{1280, false}, {390, true}} {
			s.p.Viewport(w.px, 900, w.mobile)
			s.p.Navigate(s.base + "/rules")
			var bad []string
			s.eval(`(() => { const hit = (a, b) => a.left < b.right && b.left < a.right && a.top < b.bottom && b.top < a.bottom;
				return [...document.querySelectorAll("#chain .rule[data-name]")].flatMap((r) => {
					const route = r.querySelector(".r-route").getBoundingClientRect();
					const main = r.querySelector(".r-main").getBoundingClientRect();
					const out = [];
					for (const sel of [".r-name", ".r-pos", ".chips", ".r-note"]) {
						const e = r.querySelector(sel);
						if (e && hit(e.getBoundingClientRect(), route)) out.push(r.dataset.name + " " + sel + " overlaps the route");
					}
					if (main.width < `+fmt.Sprint(min(w.px/4, 240))+`) out.push(r.dataset.name + " main cell " + Math.round(main.width) + " px");
					return out; }); })()`, &bad)
			if len(bad) > 0 {
				t.Errorf("rules without the script at %d px, %s: %q", w.px, scheme, bad)
			}
			s.check(fmt.Sprintf("rules-noscript-%d-%s", w.px, scheme), "Rules")
		}
	}
	s.p.ColorScheme("light")
	s.p.Viewport(1280, 900, false)
	s.p.Navigate(s.base + "/rules")
	// Move the first rule to the last position.
	s.eval(`(() => { document.getElementById("pos-1").value = "5"; document.querySelector(".order-foot [data-primary]").click(); return true })()`, nil)
	s.p.WaitLoad()
	if st := s.state(); st.Path != "/config/draft" || !strings.HasPrefix(st.H1, "Review: reorder the rules") {
		t.Errorf("order review without the script: %+v", st)
	}
	s.click(".review-form .btn.primary")
	s.p.WaitLoad()
	if st := s.state(); st.Path != "/rules" || s.fileRules() != "lab-net restricted ads media.example loopback" {
		t.Errorf("order apply without the script: %+v, file %q", st, s.fileRules())
	}
	s.p.Navigate(s.base + "/rules/restricted")
	s.eval(`(() => { document.getElementById("rule-test-url").value = "https://wiki.internal.example/";
		document.querySelector('button[name="intent"][value="test"]').click(); return true })()`, nil)
	s.p.WaitLoad()
	if got := s.text("#rule-test"); !strings.Contains(got, "This rule matches") {
		t.Errorf("editor test without the script: %q", got)
	}
	s.check("rule-test-noscript-1280-light", "Edit rule")
	s.p.Navigate(s.base + "/upstreams/new")
	s.eval(`(() => { document.getElementById("f-name").value = "plain"; document.getElementById("f-address").value = "192.0.2.60:3128";
		document.querySelector('input[name="disclosure"][value="none"]').checked = true;
		document.querySelector("#upstream-form [data-primary]").click(); return true })()`, nil)
	s.p.WaitLoad()
	if st := s.state(); st.H1 != "Review: add upstream plain" {
		t.Errorf("upstream review without the script: %+v", st)
	}
	s.click(".review-form .btn.primary")
	s.p.WaitLoad()
	if st := s.state(); st.Path != "/upstreams" || !strings.Contains(readFile(t, s.f.be.path), `name = "plain"`) {
		t.Errorf("upstream apply without the script: %+v", st)
	}
	s.click(`.ent:has(a[href="/upstreams/plain"]) details.r-menu > summary`)
	s.click(`.ent:has(a[href="/upstreams/plain"]) .menu button.danger`)
	s.p.WaitLoad()
	if st := s.state(); st.H1 != "Review: delete upstream plain" {
		t.Errorf("delete from the menu without the script: %+v", st)
	}
	s.clean("without the script")
	s.chrome.Call(s.p.Session, "Emulation.setScriptExecutionDisabled", map[string]any{"value": false}, nil)
	s.chrome.Close()
}

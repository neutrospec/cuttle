package admin

import (
	"html"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/neutrospec/cuttle/internal/config"
)

// pipeline is a signed-in browser on a server with the example
// configuration and the test-only form ops of ops_test.go.
type pipeline struct {
	t    *testing.T
	f    *fixture
	b    *browser
	h    http.Handler
	csrf string
}

func newPipeline(t *testing.T) *pipeline {
	t.Helper()
	f := newFixture(t, true)
	f.withExample()
	p := &pipeline{t: t, f: f, h: f.withTestOps()}
	p.b = f.browser()
	p.csrf = p.b.login()
	return p
}

// get loads a page and returns its body; the status must be 200.
func (p *pipeline) get(path string) string {
	p.t.Helper()
	res := p.b.doWith(p.h, req{method: http.MethodGet, path: path})
	body := bodyOf(p.t, res)
	if res.StatusCode != http.StatusOK {
		p.t.Fatalf("GET %s: %d:\n%s", path, res.StatusCode, body)
	}
	return body
}

// post posts form to path.
func (p *pipeline) post(path string, form url.Values) (*http.Response, string) {
	p.t.Helper()
	return p.b.post(p.h, path, form, false)
}

// fragment posts form to path as the script does.
func (p *pipeline) fragment(path string, form url.Values) (*http.Response, string) {
	p.t.Helper()
	return p.b.post(p.h, path, form, true)
}

// testForm loads the form of a test op as a GET renders it.
func (p *pipeline) testForm(path string) url.Values {
	p.t.Helper()
	return formValues(p.t, p.get(path), `<form method="post" action="/config/draft" class="test-form">`)
}

// editorForm loads the Advanced editor's form.
func (p *pipeline) editorForm() url.Values {
	p.t.Helper()
	return formValues(p.t, p.get("/settings"), `<form method="post" action="/config/draft" class="editor-form"`)
}

// with returns a copy of v with the pairs set.
func with(v url.Values, kv ...string) url.Values {
	out := url.Values{}
	for k, x := range v {
		out[k] = append([]string(nil), x...)
	}
	for i := 0; i < len(kv); i += 2 {
		out.Set(kv[i], kv[i+1])
	}
	return out
}

// review posts a form for its review and wants code.
func (p *pipeline) review(form url.Values, code int) string {
	p.t.Helper()
	res, body := p.post("/config/draft", with(form, "intent", "review"))
	if res.StatusCode != code {
		p.t.Fatalf("review: %d, want %d:\n%s", res.StatusCode, code, body)
	}
	return body
}

// applyForm returns the review's form, which Apply changes posts.
func applyForm(t *testing.T, body string) url.Values {
	t.Helper()
	return formValues(t, body, `<form method="post" action="/config/apply" class="review-form" data-leave>`)
}

// apply posts the review's form to /config/apply.
func (p *pipeline) apply(review string) *http.Response {
	p.t.Helper()
	res, body := p.post("/config/apply", applyForm(p.t, review))
	if res.StatusCode != http.StatusSeeOther {
		p.t.Fatalf("apply: %d:\n%s", res.StatusCode, body)
	}
	return res
}

func (p *pipeline) write(text string) {
	p.t.Helper()
	if err := os.WriteFile(p.f.be.path, []byte(text), 0o600); err != nil {
		p.t.Fatal(err)
	}
}

func (p *pipeline) file() string { return readFile(p.t, p.f.be.path) }

var dlRE = regexp.MustCompile(`<div class="dl( add| del)?"><span class="ln">(\d*)</span><span class="ln">(\d*)</span><span class="m">([^<]*)</span><span class="t">(.*?)(<span class="eol">[^<]*</span>)?</span></div>`)

// rebuild returns the old and the new text of the diff in a review, from
// its lines as the page shows them, collapsed stretches included, and
// checks the line numbers and marks.
func rebuild(t *testing.T, body string) (old, new string) {
	t.Helper()
	var o, n strings.Builder
	a, b := 0, 0
	for _, m := range dlRE.FindAllStringSubmatch(body, -1) {
		line := html.UnescapeString(m[5])
		if m[6] == "" {
			line += "\n"
		}
		if m[1] != " add" {
			a++
			if m[2] != strconv.Itoa(a) {
				t.Fatalf("old line number %q, want %d", m[2], a)
			}
			o.WriteString(line)
		}
		if m[1] != " del" {
			b++
			if m[3] != strconv.Itoa(b) {
				t.Fatalf("new line number %q, want %d", m[3], b)
			}
			n.WriteString(line)
		}
		if want := map[string]string{"": " ", " add": "+", " del": "−"}[m[1]]; html.UnescapeString(m[4]) != want {
			t.Fatalf("mark %q for %q", m[4], m[1])
		}
	}
	return o.String(), n.String()
}

// encodeWith returns the canonical form of text with edit applied to its
// model.
func encodeWith(t *testing.T, text string, edit func(d *config.Doc)) string {
	t.Helper()
	d, _, r := config.DocOf([]byte(text), "x")
	if d == nil {
		t.Fatalf("invalid: %v", r.Errors)
	}
	edit(d)
	return string(d.Encode())
}

func noBak(t *testing.T, f *fixture) {
	t.Helper()
	if _, err := os.Lstat(f.be.path + ".bak"); err == nil {
		t.Fatal(".bak written")
	}
}

// TestAdminDraftReview checks review of design 0009 D7 (W6, W9): a valid
// edit answers 200 with the change summary and a diff whose lines
// rebuild the file and the candidate, and leaves the file and .bak
// unchanged; an invalid edit answers 422 with each error inside its
// field's element; a non-integer port answers 422 without running the
// validator; a text review shows the plain-text diff and the one-line
// summary, and a script request gets the review block or, for errors,
// the page's result block; a file with comments written by hand gives a
// review that counts them, and after apply the file is the canonical
// form of the file with the edit; and a valid file whose canonical form
// exceeds 256 KiB is refused at review with the size (D6).
func TestAdminDraftReview(t *testing.T) {
	p := newPipeline(t)
	f := p.f

	// Text: the one-line summary and the plain-text diff.
	edited := strings.Replace(exampleConfig, `default = "direct"`, `default = "block"`, 1)
	form := with(p.editorForm(), "text", strings.ReplaceAll(edited, "\n", "\r\n"))
	body := p.review(form, http.StatusOK)
	for _, want := range []string{`<h1 id="review-h">Review: edit the configuration as text</h1>`, "Edited as text: 1 line added, 1 removed",
		"<code>config.toml.bak</code>", "1 added, 1 removed", `<details class="gap"><summary>`} {
		if !strings.Contains(body, want) {
			t.Errorf("text review lacks %q:\n%s", want, body)
		}
	}
	checkMarkup(t, "text review", body, true, "/settings")
	if o, n := rebuild(t, body); o != exampleConfig || n != edited {
		t.Errorf("text review diff:\nold %q\nnew %q", o, n)
	}
	if got := applyForm(t, body).Get("text"); got != edited {
		t.Errorf("the review carries %q", got)
	}
	if p.file() != exampleConfig || f.be.reloadCount() != 0 {
		t.Fatal("a review changed the file or reloaded")
	}
	noBak(t, f)
	res, body := p.fragment("/config/draft", with(form, "intent", "review"))
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(body, `<div class="review" data-block="review">`) ||
		!strings.Contains(body, `<h2 id="review-h">`) || strings.Contains(body, "<html") || !strings.Contains(body, "data-close") {
		t.Errorf("text review as a fragment: %d:\n%s", res.StatusCode, body)
	}

	// Invalid text: 422 with the errors in the result block.
	bad := strings.Replace(exampleConfig, `action = "block"`, `action = "sideways"`, 1)
	body = p.review(with(form, "text", bad), http.StatusUnprocessableEntity)
	if !strings.Contains(body, "The configuration has errors; fix them, then review again.") || !strings.Contains(body, "rule[3].action: ") ||
		!strings.Contains(body, `data-guard-dirty`) {
		t.Errorf("invalid text review:\n%s", body)
	}
	res, body = p.fragment("/config/draft", with(form, "text", bad, "intent", "review"))
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.HasPrefix(body, `<div class="stack" data-block="settings-result">`) ||
		!strings.Contains(body, "rule[3].action: ") {
		t.Errorf("invalid text review as a fragment: %d:\n%s", res.StatusCode, body)
	}

	// A form op: the change summary and the canonical candidate.
	rule := p.testForm("/test/test.rule/restricted?return=/rules/restricted")
	body = p.review(with(rule, "domains", "internal.example\nbuild.example\ndocs.example.org"), http.StatusOK)
	if !strings.Contains(body, "Review: change rule <code>restricted</code>") ||
		!strings.Contains(body, "<li><span>Rule <code>restricted</code>: domains &#43;1</span></li>") {
		t.Errorf("form review:\n%s", body)
	}
	checkMarkup(t, "form review", body, true, "/rules")
	want := encodeWith(t, exampleConfig, func(d *config.Doc) {
		d.Rules[1].Domains.V = append(d.Rules[1].Domains.V, "docs.example.org")
	})
	if o, n := rebuild(t, body); o != exampleConfig || n != want {
		t.Errorf("form review diff:\nold %q\nnew %q\nwant %q", o, n, want)
	}
	if !strings.Contains(body, "This save rewrites the file in canonical form; only the formatting changes.") {
		t.Errorf("form review lacks the canonical notice:\n%s", body)
	}
	if p.file() != exampleConfig || f.be.reloadCount() != 0 {
		t.Fatal("a form review changed the file or reloaded")
	}
	noBak(t, f)

	// An invalid edit: 422, the error inside its field's element.
	body = p.review(with(rule, "action", "upstream:nowhere"), http.StatusUnprocessableEntity)
	field := body[strings.Index(body, `<div class="field" id="field-action">`):]
	field = field[:strings.Index(field, "</div>")]
	if !strings.Contains(field, `<p class="err">`) || !strings.Contains(field, "nowhere") ||
		!strings.Contains(body, `<a href="#f-action">Action: `) {
		t.Errorf("invalid edit: field %s\n%s", field, body)
	}
	res, body = p.fragment("/config/draft", with(rule, "action", "upstream:nowhere", "intent", "review"))
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.HasPrefix(body, `<div class="stack" data-block="form-issues">`) ||
		!strings.Contains(body, `<a href="#f-action">Action: `) {
		t.Errorf("invalid edit as a fragment: %d:\n%s", res.StatusCode, body)
	}

	// Text from the file and from validator messages is escaped: in the
	// diff of a review, and under a field.
	hostile := strings.Replace(exampleConfig, `name = "ads"`, "name = \"ads\"\nnote = \"<script>alert(1)</script>\"", 1)
	body = p.review(with(form, "text", hostile), http.StatusOK)
	if strings.Contains(body, "<script>alert") || !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Errorf("a note with markup in the diff is not escaped:\n%s", body)
	}
	body = p.review(with(rule, "domains", `<img src=x onerror=alert(1)>`), http.StatusUnprocessableEntity)
	if strings.Contains(body, "<img src=x") || !strings.Contains(body, "&lt;img") {
		t.Errorf("a validator message with markup is not escaped:\n%s", body)
	}

	// A port that is not an integer: 422 without the validator.
	runs := f.s.validations.Load()
	body = p.review(with(rule, "ports", "443\nhttps"), http.StatusUnprocessableEntity)
	if f.s.validations.Load() != runs {
		t.Error("the validator ran for a port that is not an integer")
	}
	if !strings.Contains(body, `Entry 2: &#34;https&#34; must be an integer port`) || !strings.Contains(body, `data-bad="2"`) {
		t.Errorf("non-integer port:\n%s", body)
	}
	if p.file() != exampleConfig {
		t.Fatal("a refused review changed the file")
	}

	// Comments written by hand: counted in the review, and gone after
	// apply, which writes the canonical form of the file with the edit.
	commented := "# Edge instance.\n# Second line.\n" + strings.Replace(exampleConfig, `default = "direct"`, `default = "direct" # local clients`, 1)
	p.write(commented)
	rule = p.testForm("/test/test.rule/restricted?return=/rules/restricted")
	body = p.review(with(rule, "domains", "internal.example\nbuild.example\ndocs.example.org"), http.StatusOK)
	if !strings.Contains(body, "This save rewrites the file in canonical form: 2 comment lines and 1 end-of-line comment written by hand are removed.") {
		t.Errorf("commented file review:\n%s", body)
	}
	p.apply(body)
	if got := p.file(); got != encodeWith(t, commented, func(d *config.Doc) {
		d.Rules[1].Domains.V = append(d.Rules[1].Domains.V, "docs.example.org")
	}) {
		t.Errorf("after apply:\n%s", got)
	}
	if readFile(t, f.be.path+".bak") != commented {
		t.Error(".bak does not hold the commented file")
	}

	// Size (D6): a valid file under 256 KiB whose canonical form is not.
	var b strings.Builder
	b.WriteString("name = \"big\"\n[proxy]\ndefault = \"direct\"\n[[rule]]\nname = \"big\"\naction = \"block\"\ndomains = [")
	for i := 0; b.Len() < config.MaxFileSize-200; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"d` + strconv.Itoa(i) + `.example"`)
	}
	b.WriteString("]\n")
	big := b.String()
	if cfg, r := config.Parse([]byte(big), "big"); cfg == nil || len(big) > config.MaxFileSize {
		t.Fatalf("big sample: %d bytes, %v", len(big), r.Errors)
	}
	p.write(big)
	rule = p.testForm("/test/test.rule/big?return=/rules")
	body = p.review(with(rule, "note", "too big now"), http.StatusRequestEntityTooLarge)
	if !regexp.MustCompile(`In canonical form the file would be [0-9.]+ KiB, larger than the 256 KiB limit, so this change cannot be saved.`).MatchString(body) ||
		strings.Contains(body, "Apply changes") {
		t.Errorf("canonical form over the limit:\n%s", body)
	}
	if p.file() != big {
		t.Error("a refused review changed the file")
	}
}

// TestAdminApply checks apply of design 0009 D7 (W7, W14): apply writes
// the reviewed text with mode 0600 and the previous content in .bak,
// reloads, and answers 303 to return, where a toast reports the reload
// result once; a proxy.listen change shows restart required in the
// review, the toast, and the banner, and a later review that changes it
// back to its start value lists no restart key; a reviewed text with CRLF
// line ends is written with LF; a return outside the page list gets 400;
// a rename started at the editor path returns to the list; a directory
// that cannot be synced counts as saved with the warning, and the reload
// runs (OI-0006 U-R1), also when the reload then refuses the file
// (OI-0006 Z-R8).
func TestAdminApply(t *testing.T) {
	p := newPipeline(t)
	f := p.f
	moved := strings.Replace(exampleConfig, `"127.0.0.1:41728"`, `"127.0.0.1:41730"`, 1)
	body := p.review(with(p.editorForm(), "text", moved), http.StatusOK)
	if !strings.Contains(body, "<code>proxy.listen</code> changes apply at the next start. The other changes apply when you apply them.") {
		t.Errorf("review of a proxy.listen change:\n%s", body)
	}
	af := applyForm(t, body)
	af.Set("text", strings.ReplaceAll(af.Get("text"), "\n", "\r\n"))
	res, out := p.post("/config/apply", af)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/settings" {
		t.Fatalf("apply: %d %q:\n%s", res.StatusCode, res.Header.Get("Location"), out)
	}
	if got := p.file(); got != moved {
		t.Errorf("file after apply: %q", got)
	}
	if readFile(t, f.be.path+".bak") != exampleConfig || f.be.reloadCount() != 1 {
		t.Errorf("after apply: .bak or reloads %d", f.be.reloadCount())
	}
	for _, path := range []string{f.be.path, f.be.path + ".bak"} {
		if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: mode %v", path, fi.Mode())
		}
	}
	body = p.get("/settings")
	for _, want := range []string{`<div class="toast warn" role="alert" data-toast="warn">`, "<strong>Saved and reloaded. Restart required.</strong>",
		"<span>Edited as text: 1 line added, 1 removed.</span>", `<div class="banner warn" role="status">`,
		"<code>proxy.listen</code> changes apply at the next start."} {
		if !strings.Contains(body, want) {
			t.Errorf("page after apply lacks %q:\n%s", want, body)
		}
	}
	if body := p.get("/settings"); strings.Contains(body, `class="toast `) {
		t.Error("the toast showed twice")
	}
	// Back to the start value: no restart key at review.
	body = p.review(with(p.editorForm(), "text", exampleConfig), http.StatusOK)
	if strings.Contains(body, "The other changes apply when you apply them.") {
		t.Errorf("review back to the start value lists a restart key:\n%s", body)
	}
	p.apply(body)

	// A return outside the page list: 400 at draft and apply.
	for _, ret := range []string{"", "/evil", "//evil.example/", "http://evil.example/", "/settings#x", `/settings\x`, "/rules/Bad", "/rules/a/b",
		"/settings?%zz", "/static/admin.css"} {
		for _, path := range []string{"/config/draft", "/config/apply"} {
			form := with(applyForm(t, p.review(with(p.editorForm(), "text", moved), http.StatusOK)), "return", ret, "intent", "review")
			if res, _ := p.post(path, form); res.StatusCode != http.StatusBadRequest {
				t.Errorf("%s with return %q: %d", path, ret, res.StatusCode)
			}
		}
	}
	if p.file() != exampleConfig {
		t.Fatal("a refused apply changed the file")
	}
	// The other apply guards: a reviewed digest that is not a SHA-256 or
	// an unknown op gets 400, text over 256 KiB 413, and text the
	// validator refuses 422; none writes or reloads.
	good := applyForm(t, p.review(with(p.editorForm(), "text", moved), http.StatusOK))
	reloads := f.be.reloadCount()
	for name, c := range map[string]struct {
		form url.Values
		code int
	}{
		"no reviewed digest":       {with(good, "reviewed", ""), http.StatusBadRequest},
		"an upper-case digest":     {with(good, "reviewed", strings.ToUpper(good.Get("reviewed"))), http.StatusBadRequest},
		"an unknown op":            {with(good, "op", "rule.nonsense"), http.StatusBadRequest},
		"text over 256 KiB":        {with(good, "text", moved+"#"+strings.Repeat("x", config.MaxFileSize)+"\n"), http.StatusRequestEntityTooLarge},
		"text with errors":         {with(good, "text", strings.Replace(moved, `action = "block"`, `action = "sideways"`, 1)), http.StatusUnprocessableEntity},
		"text with a syntax error": {with(good, "text", moved+"[proxy\n"), http.StatusUnprocessableEntity},
	} {
		res, body := p.post("/config/apply", c.form)
		if res.StatusCode != c.code || strings.Contains(body, "Apply changes") {
			t.Errorf("apply with %s: %d, want %d:\n%s", name, res.StatusCode, c.code, body)
		}
	}
	if p.file() != exampleConfig || f.be.reloadCount() != reloads {
		t.Fatal("a refused apply changed the file or reloaded")
	}
	// A query is kept, re-encoded.
	form := with(p.editorForm(), "text", moved, "return", "/domains?window=1h&route=block")
	res = p.apply(p.review(form, http.StatusOK))
	if got := res.Header.Get("Location"); got != "/domains?route=block&window=1h" {
		t.Errorf("return with a query: %q", got)
	}
	// The toast on a page that does not list warnings names them.
	if body := p.get("/domains"); !strings.Contains(body, "Saved and reloaded. Restart required.") ||
		!strings.Contains(body, "has no interfaces, so its dials follow system routing") {
		t.Errorf("toast on /domains:\n%s", body)
	}

	// A rename started at the editor path returns to the list.
	rule := p.testForm("/test/test.rule/ads?return=/rules/ads")
	res = p.apply(p.review(with(rule, "name", "ads-b"), http.StatusOK))
	if got := res.Header.Get("Location"); got != "/rules" {
		t.Errorf("after a rename: %q", got)
	}
	if body := p.get("/settings"); !strings.Contains(body, "Rule <code>ads-b</code>") && !strings.Contains(body, "Saved and reloaded") {
		t.Errorf("toast after a rename:\n%s", body)
	}

	// The directory cannot be synced: saved with the warning, reloaded.
	if os.Geteuid() == 0 {
		t.Skip("root opens any directory")
	}
	dir := filepath.Dir(f.be.path)
	syncFail := func(text string) *http.Response {
		t.Helper()
		review := p.review(with(p.editorForm(), "text", text), http.StatusOK)
		if err := os.Chmod(dir, 0o300); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(dir, 0o700)
		res, body := p.post("/config/apply", applyForm(t, review))
		if res.StatusCode != http.StatusSeeOther {
			t.Fatalf("apply into a directory that cannot be synced: %d:\n%s", res.StatusCode, body)
		}
		return res
	}
	reloads = f.be.reloadCount()
	blocked := strings.Replace(exampleConfig, `default = "direct"`, `default = "block"`, 1)
	syncFail(blocked)
	if p.file() != blocked || f.be.reloadCount() != reloads+1 || f.be.Config().Proxy.Default.String() != "block" {
		t.Errorf("unsynced save: file %q, reloads %d", p.file(), f.be.reloadCount()-reloads)
	}
	if body := p.get("/settings"); !strings.Contains(body, "<strong>Saved and reloaded; the directory could not be synced.</strong>") {
		t.Errorf("toast after an unsynced save:\n%s", body)
	}
	// And the reload refuses the file too: the toast keeps both.
	f.be.mu.Lock()
	f.be.refuse = "the test refuses the file"
	f.be.mu.Unlock()
	syncFail(exampleConfig)
	body = p.get("/settings")
	if !strings.Contains(body, "<strong>Saved, but the reload refused the file; the running configuration is unchanged.</strong>") ||
		!strings.Contains(body, "<span>the test refuses the file</span>") || !strings.Contains(body, "the directory could not be synced") ||
		!strings.Contains(body, `<div class="toast error" role="alert" data-toast="error">`) {
		t.Errorf("toast after an unsynced save that the reload refused:\n%s", body)
	}
	// POST /settings says the same in its notice.
	_, base := p.b.settingsForm()
	if err := os.Chmod(dir, 0o300); err != nil {
		t.Fatal(err)
	}
	res, body = p.post("/settings", url.Values{"csrf": {p.csrf}, "text": {blocked}, "base": {base}})
	os.Chmod(dir, 0o700)
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "the test refuses the file") ||
		!strings.Contains(body, "The directory could not be synced either") {
		t.Errorf("POST /settings, unsynced and refused: %d:\n%s", res.StatusCode, body)
	}
}

// TestDiffView checks the collapsing of design 0009 D3.11: only an
// unchanged stretch longer than 6 lines collapses, so a change near the
// start shows every line before it, and a gap between hunks or a long
// stretch at the end collapses with its count.
func TestDiffView(t *testing.T) {
	var old strings.Builder
	for i := 1; i <= 30; i++ {
		old.WriteString("line " + strconv.Itoa(i) + "\n")
	}
	a := old.String()
	b := strings.Replace(strings.Replace(a, "line 5\n", "five\n", 1), "line 20\n", "twenty\n", 1)
	var shape []string
	for _, part := range newDiffView("f", a, b).Parts {
		if part.Gap {
			shape = append(shape, part.Label)
		} else {
			shape = append(shape, strconv.Itoa(len(part.Lines)))
		}
	}
	// Lines 1-4 before the first change stay (1 + 3 of context), the 8
	// lines 9-16 between the hunks collapse, and so do lines 24-30.
	if want := []string{"1", "8", "8 unchanged lines", "8", "7 unchanged lines"}; strings.Join(shape, ",") != strings.Join(want, ",") {
		t.Errorf("parts %q, want %q", shape, want)
	}
}

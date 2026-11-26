package admin

import (
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
)

const fileErrorsBanner = "The configuration file has errors, so the configuration pages are read-only."

// TestAdminInvalidFile checks design 0009 D3.13 and W10: with an invalid
// file on disk every page shows the errors banner, and every draft intent
// of a form op answers 409 and leaves the file unchanged; the same file
// is fixed from the UI in three ways, each leaving the file valid,
// reloaded, and the banner gone: Advanced → review → apply, restore of a
// valid .bak → review → apply, and POST /settings (A9).
func TestAdminInvalidFile(t *testing.T) {
	p := newPipeline(t)
	f := p.f
	invalid := strings.Replace(exampleConfig, `action = "block"`, `action = "sideways"`, 1)
	breakFile := func() {
		t.Helper()
		p.write(invalid)
		for _, path := range []string{"/", "/domains", "/live", "/settings", "/settings/backup"} {
			body := p.get(path)
			if !strings.Contains(body, fileErrorsBanner) || !strings.Contains(body, "rule[3].action: ") {
				t.Errorf("%s with an invalid file: no errors banner", path)
			}
			if strings.Contains(body, `href="/settings">Fix in Advanced</a>`) == strings.HasPrefix(path, "/settings") {
				t.Errorf("%s: the Fix in Advanced action is wrong", path)
			}
		}
		// Every intent of a form op: 409, the file unchanged.
		for _, intent := range []string{intentEdit, intentCheck, intentTest, intentReview} {
			form := url.Values{"csrf": {p.csrf}, "op": {"test.rule"}, "target": {"restricted"}, "base": {strings.Repeat("0", 64)},
				"return": {"/rules/restricted"}, "intent": {intent}, "note": {"x"}, "r:note": {""}}
			for _, frag := range []bool{false, true} {
				res, body := p.b.post(p.h, "/config/draft", form, frag)
				if res.StatusCode != http.StatusConflict || !strings.Contains(body, "The configuration file has errors, so this page cannot change it.") {
					t.Errorf("%s (fragment %v) with an invalid file: %d:\n%s", intent, frag, res.StatusCode, body)
				}
			}
		}
		if p.file() != invalid {
			t.Fatal("a draft changed the invalid file")
		}
	}
	fixed := func(how string, want string) {
		t.Helper()
		if got := p.file(); got != want {
			t.Errorf("%s: file %q", how, got)
		}
		if f.be.Config().SHA256 != f.s.readFile().Digest {
			t.Errorf("%s: not reloaded", how)
		}
		if body := p.get("/"); strings.Contains(body, fileErrorsBanner) || strings.Contains(body, "The file changed since cuttle loaded it.") {
			t.Errorf("%s: a file banner remains", how)
		}
	}

	// Advanced: review and apply valid text. The review says that the
	// current file has errors.
	breakFile()
	body := p.review(with(p.editorForm(), "text", exampleConfig), http.StatusOK)
	if !strings.Contains(body, "The current file has errors; applying replaces it.") {
		t.Errorf("review over an invalid file:\n%s", body)
	}
	p.apply(body)
	fixed("Advanced", exampleConfig)

	// Restore: the .bak now holds the invalid file; save a valid one
	// over it first, so that .bak is valid, then break the file.
	valid2 := strings.Replace(exampleConfig, `default = "direct"`, `default = "block"`, 1)
	p.apply(p.review(with(p.editorForm(), "text", valid2), http.StatusOK))
	breakFile()
	if body := p.get("/"); !strings.Contains(body, `<a class="btn sm" href="/settings/backup">Restore the previous version</a>`) {
		t.Errorf("the errors banner does not offer the valid previous version:\n%s", body)
	}
	body = p.get("/settings/backup")
	restore := formValues(t, body, `<form method="post" action="/config/draft" class="inline-form">`)
	body = p.review(restore, http.StatusOK)
	if !strings.Contains(body, "Review: restore the previous version") || !strings.Contains(body, "The current file has errors; applying replaces it.") {
		t.Errorf("restore review:\n%s", body)
	}
	p.apply(body)
	fixed("restore", exampleConfig)
	if readFile(t, f.be.path+".bak") != invalid {
		t.Error("restore did not keep the replaced file in .bak")
	}

	// POST /settings, the direct save of design 0005 D10.
	breakFile()
	_, base := p.b.settingsForm()
	res, body := p.post("/settings", url.Values{"csrf": {p.csrf}, "text": {valid2}, "base": {base}})
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "Saved and reloaded") {
		t.Errorf("POST /settings over an invalid file: %d:\n%s", res.StatusCode, body)
	}
	fixed("POST /settings", valid2)

	// A file that cannot be read: its own banner, and 409 for a text
	// review.
	if err := os.Chmod(f.be.path, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(f.be.path, 0o600)
	if os.Geteuid() != 0 {
		if body := p.get("/live"); !strings.Contains(body, "The configuration file cannot be read, so the configuration pages are read-only.") {
			t.Errorf("unreadable file: no banner")
		}
		form := url.Values{"csrf": {p.csrf}, "op": {"text"}, "text": {valid2}, "base": {base}, "return": {"/settings"}, "intent": {"review"}}
		if res, body := p.post("/config/draft", form); res.StatusCode != http.StatusConflict || !strings.Contains(body, "The configuration file cannot be read") {
			t.Errorf("review of an unreadable file: %d:\n%s", res.StatusCode, body)
		}
	}
}

// TestAdminReloadButton checks the banner of a file that differs from the
// running configuration (design 0009 D3.13, W10): it offers Reload now,
// which posts to /config/reload; the reload loads the file, returns to the
// page with a toast, and the banner is gone. A file the reload refuses
// gives the refusal toast and the reload banner.
func TestAdminReloadButton(t *testing.T) {
	p := newPipeline(t)
	f := p.f
	if body := p.get("/live"); strings.Contains(body, "The file changed since cuttle loaded it.") {
		t.Fatal("banner while the file is running")
	}
	changed := strings.Replace(exampleConfig, `default = "direct"`, `default = "block"`, 1)
	p.write(changed)
	body := p.get("/live")
	if !strings.Contains(body, "<strong>The file changed since cuttle loaded it.</strong>") ||
		!strings.Contains(body, `<form method="post" action="/config/reload">`) {
		t.Fatalf("no file-differs banner:\n%s", body)
	}
	form := formValues(t, body, `<form method="post" action="/config/reload">`)
	if form.Get("return") != "/live" || form.Get("csrf") != p.csrf {
		t.Errorf("Reload now posts %v", form)
	}
	res, _ := p.post("/config/reload", form)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/live" || f.be.reloadCount() != 1 ||
		f.be.Config().Proxy.Default.String() != "block" {
		t.Fatalf("reload: %d %q, reloads %d", res.StatusCode, res.Header.Get("Location"), f.be.reloadCount())
	}
	body = p.get("/live")
	if strings.Contains(body, "The file changed since cuttle loaded it.") || !strings.Contains(body, "<strong>Reloaded with 2 warnings.</strong>") {
		t.Errorf("after the reload:\n%s", body)
	}
	f.be.mu.Lock()
	f.be.refuse = "refused by the test"
	f.be.mu.Unlock()
	p.write(exampleConfig)
	res, _ = p.post("/config/reload", form)
	body = p.get("/live")
	if res.StatusCode != http.StatusSeeOther || !strings.Contains(body, "<strong>The reload refused the file; the running configuration is unchanged.</strong>") ||
		!strings.Contains(body, "The last reload refused the file") {
		t.Errorf("refused reload: %d:\n%s", res.StatusCode, body)
	}
	if res, _ := p.post("/config/reload", with(form, "return", "https://evil.example/")); res.StatusCode != http.StatusBadRequest {
		t.Errorf("reload with a foreign return: %d", res.StatusCode)
	}
}

// TestAdminPostProtection checks W11 of design 0009: every POST that the
// server registers, but the login itself, refuses a request without a
// session, without the session's CSRF token, with another token, and a
// cross-site request, with 403, before it reads or writes anything. The
// test enumerates the server's routes, so a new POST without the checks
// fails it.
func TestAdminPostProtection(t *testing.T) {
	f := newFixture(t, true)
	b := f.browser()
	csrf := b.login()
	before := readFile(t, f.be.path)
	var posts []string
	for _, r := range f.s.routes {
		if path, ok := strings.CutPrefix(r, "POST "); ok && path != "/login" {
			posts = append(posts, path)
		}
	}
	for _, want := range []string{"/logout", "/settings", "/settings/validate", "/config/draft", "/config/apply", "/config/reload"} {
		found := false
		for _, p := range posts {
			found = found || p == want
		}
		if !found {
			t.Errorf("POST %s is not registered", want)
		}
	}
	valid := url.Values{"text": {fakeConfig}, "base": {strings.Repeat("0", 64)}, "reviewed": {strings.Repeat("0", 64)}, "op": {"text"},
		"intent": {"review"}, "return": {"/settings"}}
	for _, path := range posts {
		for name, r := range map[string]req{
			"without a session":    {form: with(valid, "csrf", csrf), noCookies: true},
			"without a CSRF token": {form: valid},
			"with a wrong token":   {form: with(valid, "csrf", alterToken(csrf))},
			"cross-site":           {form: with(valid, "csrf", csrf), crossSite: true},
		} {
			r.method, r.path = http.MethodPost, path
			if res := b.do(r); res.StatusCode != http.StatusForbidden {
				t.Errorf("POST %s %s: %d, want 403", path, name, res.StatusCode)
			}
		}
	}
	if readFile(t, f.be.path) != before || f.be.reloadCount() != 0 {
		t.Fatal("a refused POST changed the file or reloaded")
	}
	// Positive control: the same reload with the session and the token.
	if res := b.do(req{method: http.MethodPost, path: "/config/reload", form: url.Values{"csrf": {csrf}, "return": {"/"}}}); res.StatusCode != http.StatusSeeOther || f.be.reloadCount() != 1 {
		t.Errorf("reload with the token: %d", res.StatusCode)
	}
}

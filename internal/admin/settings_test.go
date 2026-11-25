package admin

import (
	"bytes"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"

	"squid/internal/config"
)

var baseRE = regexp.MustCompile(`name="base" value="([0-9a-f]{64})"`)

// settingsForm loads /settings and returns the CSRF token and base.
func (b *browser) settingsForm() (csrf, base string) {
	b.f.t.Helper()
	res := b.get("/settings")
	body := bodyOf(b.f.t, res)
	c, m := csrfRE.FindStringSubmatch(body), baseRE.FindStringSubmatch(body)
	if res.StatusCode != http.StatusOK || c == nil || m == nil {
		b.f.t.Fatalf("GET /settings: %d:\n%s", res.StatusCode, body)
	}
	return c[1], m[1]
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestAdminSettingsAccess checks that the settings POSTs need a session,
// the session's CSRF token, and a same-origin request (design 0005 D8,
// D10): each refused variant gets 403, leaves the file and its .bak
// untouched, and runs no reload; the same request with all three saves.
func TestAdminSettingsAccess(t *testing.T) {
	f := newFixture(t, true)
	b := f.browser()
	b.login()
	csrf, base := b.settingsForm()
	other := f.browser()
	other.login()
	otherCSRF, _ := other.settingsForm()
	newText := strings.Replace(fakeConfig, `default = "direct"`, `default = "block"`, 1)
	before := readFile(t, f.be.path)

	for _, path := range []string{"/settings", "/settings/validate"} {
		refuse := func(name string, r req) {
			t.Helper()
			r.method, r.path = http.MethodPost, path
			res := b.do(r)
			if body := bodyOf(t, res); res.StatusCode != http.StatusForbidden {
				t.Errorf("%s %s: %d, want 403:\n%s", path, name, res.StatusCode, body)
			}
			if got := readFile(t, f.be.path); got != before {
				t.Fatalf("%s %s: the file changed", path, name)
			}
			if _, err := os.Stat(f.be.path + ".bak"); err == nil {
				t.Fatalf("%s %s: .bak written", path, name)
			}
			if n := f.be.reloadCount(); n != 0 {
				t.Fatalf("%s %s: %d reloads", path, name, n)
			}
		}
		form := func(token string) url.Values {
			v := url.Values{"text": {newText}, "base": {base}}
			if token != "" {
				v.Set("csrf", token)
			}
			return v
		}
		refuse("without a session", req{form: form(csrf), noCookies: true})
		refuse("with a foreign session cookie", req{form: form(csrf), cookies: map[string]string{SessionCookie: alterToken(b.cookies[SessionCookie])}})
		refuse("without the CSRF token", req{form: form("")})
		refuse("with a wrong CSRF token", req{form: form(alterToken(csrf))})
		refuse("with another session's CSRF token", req{form: form(otherCSRF)})
		refuse("cross-site with the token", req{form: form(csrf), crossSite: true})
		refuse("same-site with the token", req{form: form(csrf), header: http.Header{"Sec-Fetch-Site": {"same-site"}}})
		refuse("foreign Origin with the token", req{form: form(csrf), header: http.Header{"Sec-Fetch-Site": nil, "Origin": {"http://evil.example"}}})
	}

	// Without a session, GET /settings sends the browser to /login.
	if res := b.do(req{method: http.MethodGet, path: "/settings", noCookies: true}); res.StatusCode != http.StatusSeeOther ||
		res.Header.Get("Location") != "/login" {
		t.Errorf("GET /settings without a session: %d %q", res.StatusCode, res.Header.Get("Location"))
	}

	// Positive control: the same request with the session, the token, and
	// same-origin metadata saves and reloads.
	res := b.do(req{method: http.MethodPost, path: "/settings", form: url.Values{"text": {newText}, "base": {base}, "csrf": {csrf}}})
	if body := bodyOf(t, res); res.StatusCode != http.StatusOK || !strings.Contains(body, "Saved and reloaded.") {
		t.Fatalf("save: %d:\n%s", res.StatusCode, body)
	}
	if got := readFile(t, f.be.path); got != newText || readFile(t, f.be.path+".bak") != before || f.be.reloadCount() != 1 {
		t.Errorf("save: file %q, reloads %d", got, f.be.reloadCount())
	}
}

// TestAdminSettingsForm checks the D10 procedure on the fake backend:
// validation without saving, 422 with line numbers, 409 on a stale base
// with both texts shown, CRLF line breaks stored as LF, a leading newline
// kept, the size limit, the base check, the .bak and the file mode, and
// restart_required after a proxy.listen change.
func TestAdminSettingsForm(t *testing.T) {
	f := newFixture(t, true)
	b := f.browser()
	b.login()
	csrf, base := b.settingsForm()
	if base != config.Digest([]byte(fakeConfig)) {
		t.Fatalf("base %s is not the SHA-256 of the file", base)
	}
	post := func(path string, form url.Values) (*http.Response, string) {
		t.Helper()
		form.Set("csrf", csrf)
		res := b.do(req{method: http.MethodPost, path: path, form: form})
		return res, bodyOf(t, res)
	}
	unchanged := func(what string) {
		t.Helper()
		if got := readFile(t, f.be.path); got != fakeConfig {
			t.Fatalf("%s: the file changed:\n%s", what, got)
		}
		if f.be.reloadCount() != 0 {
			t.Fatalf("%s: reloaded", what)
		}
	}

	// The page shows the file in the editor, with a newline after the
	// textarea tag so that a leading newline survives.
	res := b.get("/settings")
	body := bodyOf(t, res)
	if !strings.Contains(body, "<textarea id=\"text\" name=\"text\" rows=\"32\" spellcheck=\"false\" autocomplete=\"off\" wrap=\"off\">\nname = &#34;cuttle-test&#34;") ||
		!strings.Contains(body, f.be.path) || !strings.Contains(body, "succeeded") {
		t.Errorf("settings page:\n%s", body)
	}

	// Invalid text: 422 with each error's line, the submitted text kept,
	// the file unchanged; the validate button says the same.
	invalid := fakeConfig + "\n[[rule]]\nname = \"bad\"\naction = \"upstream:nowhere\"\nhosts = [\"x.example\"]\n"
	for _, path := range []string{"/settings", "/settings/validate"} {
		res, body := post(path, url.Values{"text": {invalid}, "base": {base}})
		if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "line 22: rule[2].action: ") ||
			!strings.Contains(body, "upstream:nowhere") || !strings.Contains(body, "name = &#34;bad&#34;") {
			t.Errorf("%s invalid: %d:\n%s", path, res.StatusCode, body)
		}
		unchanged(path + " invalid")
	}
	res, body = post("/settings", url.Values{"text": {"name = \"x\"\n[proxy\n"}, "base": {base}})
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "line 3: TOML syntax: ") {
		t.Errorf("syntax error: %d:\n%s", res.StatusCode, body)
	}
	unchanged("syntax error")

	// Valid text through the validate button: 200, nothing saved.
	valid := strings.Replace(fakeConfig, `default = "direct"`, `default = "block"`, 1)
	if res, body := post("/settings/validate", url.Values{"text": {valid}, "base": {base}}); res.StatusCode != http.StatusOK ||
		!strings.Contains(body, "The configuration is valid. It has not been saved.") {
		t.Errorf("validate: %d:\n%s", res.StatusCode, body)
	}
	unchanged("validate")

	// The size limit and the base check.
	big := fakeConfig + "#" + strings.Repeat("x", config.MaxFileSize) + "\n"
	if res, _ := post("/settings", url.Values{"text": {big}, "base": {base}}); res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("text over 256 KiB: %d", res.StatusCode)
	}
	huge := strings.Repeat("\n", maxSettingsFormBytes/3+1) // %0A encodes each newline as three bytes
	if res, _ := post("/settings", url.Values{"text": {huge}, "base": {base}}); res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("body over the form limit: %d", res.StatusCode)
	}
	for _, bad := range []string{"", "abc", strings.ToUpper(base)} {
		if res, _ := post("/settings", url.Values{"text": {valid}, "base": {bad}}); res.StatusCode != http.StatusBadRequest {
			t.Errorf("base %q: %d", bad, res.StatusCode)
		}
	}
	unchanged("limits")

	// A stale base: someone else changed the file. 409 shows both texts;
	// the file keeps the other writer's content.
	other := strings.Replace(fakeConfig, `name = "cuttle-test"`, `name = "cuttle-other"`, 1)
	if err := os.WriteFile(f.be.path, []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}
	res, body = post("/settings", url.Values{"text": {valid}, "base": {base}})
	m := baseRE.FindStringSubmatch(body)
	if res.StatusCode != http.StatusConflict || !strings.Contains(body, "The file changed since this page loaded") ||
		!strings.Contains(body, "name = &#34;cuttle-other&#34;") || !strings.Contains(body, "default = &#34;block&#34;") ||
		m == nil || m[1] != config.Digest([]byte(other)) {
		t.Fatalf("stale base: %d:\n%s", res.StatusCode, body)
	}
	if got := readFile(t, f.be.path); got != other || f.be.reloadCount() != 0 {
		t.Fatalf("stale base: file %q, reloads %d", got, f.be.reloadCount())
	}
	if _, err := os.Stat(f.be.path + ".bak"); err == nil {
		t.Fatal("stale base: .bak written")
	}

	// Saving again with the base the 409 page offered replaces the file.
	// The browser sends CRLF line breaks, which are stored as LF, and a
	// leading newline is kept.
	crlf := strings.ReplaceAll("\n"+valid, "\n", "\r\n")
	res, body = post("/settings", url.Values{"text": {crlf}, "base": {m[1]}})
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "Saved and reloaded.") {
		t.Fatalf("save: %d:\n%s", res.StatusCode, body)
	}
	data, _ := os.ReadFile(f.be.path)
	if !bytes.Equal(data, []byte("\n"+valid)) {
		t.Errorf("saved %q", data)
	}
	if bak := readFile(t, f.be.path+".bak"); bak != other {
		t.Errorf(".bak %q", bak)
	}
	for _, p := range []string{f.be.path, f.be.path + ".bak"} {
		if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: mode %v", p, fi.Mode())
		}
	}
	if f.be.reloadCount() != 1 || f.be.Config().Proxy.Default.String() != "block" {
		t.Errorf("reload: %d, default %v", f.be.reloadCount(), f.be.Config().Proxy.Default)
	}

	// A proxy.listen change saves, reloads, and lists the key under
	// restart required.
	_, base = b.settingsForm()
	moved := strings.Replace("\n"+valid, "127.0.0.1:41728", "127.0.0.1:41730", 1)
	res, body = post("/settings", url.Values{"text": {moved}, "base": {base}})
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "Restart required for: proxy.listen.") {
		t.Errorf("proxy.listen change: %d:\n%s", res.StatusCode, body)
	}

	// A file that is invalid on disk is shown with its errors.
	if err := os.WriteFile(f.be.path, []byte(invalid), 0o600); err != nil {
		t.Fatal(err)
	}
	res = b.get("/settings")
	body = bodyOf(t, res)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "line 22: rule[2].action: ") || !baseRE.MatchString(body) {
		t.Errorf("invalid file on disk: %d:\n%s", res.StatusCode, body)
	}
}

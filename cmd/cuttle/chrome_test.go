package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neutrospec/cuttle/internal/testenv"
)

// pageState is what the check reads from a rendered page.
type pageState struct {
	Path  string `json:"path"`
	Title string `json:"title"`
	H1    string `json:"h1"`
	H2    string `json:"h2"`
	Text  string `json:"text"`
	// Styled reports whether the embedded stylesheet applied, and Script
	// whether the embedded script ran.
	Styled bool `json:"styled"`
	Script bool `json:"script"`
	Width  int  `json:"width"`
	Scroll int  `json:"scroll"`
}

const readPage = `JSON.stringify({
  path: location.pathname, title: document.title,
  h1: (document.querySelector("h1") || {}).textContent || "",
  h2: Array.from(document.querySelectorAll("h2")).map(e => e.textContent).join("|"),
  text: document.body.innerText.slice(0, 4000),
  styled: getComputedStyle(document.body).margin === "0px",
  script: document.documentElement.classList.contains("js"),
  width: document.documentElement.clientWidth, scroll: document.documentElement.scrollWidth
})`

func readState(t *testing.T, p *testenv.ChromePage) pageState {
	t.Helper()
	var raw string
	p.Eval(readPage, &raw)
	var s pageState
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestAdminChrome is the T2 admin check of design 0005 A10 and 0007 D7:
// headless Chrome, with a throwaway profile under a temporary directory and
// the mock keychain, signs in at http://127.0.0.1:<port>/ of a cuttle
// binary that runs with temporary XDG directories and test ports, and
// renders the overview, the domain table, the live page, and the
// Advanced page, each also at 390 px in light and dark appearance without
// sideways scrolling, a console error, or a Content-Security-Policy
// violation (design 0009 D11). Chrome opens no window and no debugging
// port; its proxy is the instance's own proxy listener, whose block
// default answers Chrome's background requests with 403 before any name
// lookup, and every host name maps to NOTFOUND, so nothing leaves the
// machine.
//
// It starts a browser, so T0 and T1 skip it; scripts/admin-check runs it
// with CUTTLE_ADMIN_CHROME=1. CUTTLE_CHROME overrides the Chrome binary,
// and CUTTLE_ADMIN_CHROME_OUT names a directory for screenshots.
func TestAdminChrome(t *testing.T) {
	if os.Getenv("CUTTLE_ADMIN_CHROME") != "1" {
		t.Skip("starts headless Chrome; run scripts/admin-check")
	}
	sb := testenv.NewSandbox(t, testenv.BuildCuttle(t))
	proxyAddr := testenv.Port(sb.FreePort())
	adminAddr := testenv.Port(sb.FreePort())
	writeConfig(t, sb, strings.Replace(serveConfig([]string{proxyAddr}, chromeRules), `[admin]
listen = ""`, fmt.Sprintf("[admin]\nlisten = %q", adminAddr), 1))
	if r := sb.RunInput(10*time.Second, adminTestPassword+"\n", "admin", "passwd", "--stdin"); r.Code != 0 {
		t.Fatalf("admin passwd: %d %s", r.Code, r.Stderr)
	}
	proc := sb.Start(proxyAddr, "serve")
	waitLog(t, sb, `msg="admin UI listening" address=`+adminAddr)

	// HOME and TMPDIR inside the sandbox; the instance's proxy listener
	// answers every request that is not for the admin UI.
	chrome := testenv.StartChrome(t, testenv.ChromeOptions{Dir: sb.Root, Env: sb.Env(), Proxy: proxyAddr})
	p := chrome.NewPage()
	navigate := func(u string) pageState {
		t.Helper()
		p.Navigate(u)
		return readState(t, p)
	}
	shoot := func(name string) {
		t.Helper()
		if dir := os.Getenv("CUTTLE_ADMIN_CHROME_OUT"); dir != "" {
			p.Screenshot(filepath.Join(dir, name+".png"))
		}
	}
	// Chrome logs every 4xx answer to a fetch as a network error; 422 is
	// the answer the live check expects for a draft with an invalid entry,
	// and it shows it, so only that line is not a problem.
	clean := func(where string) {
		t.Helper()
		var probs []string
		for _, pr := range p.Problems() {
			if !strings.HasPrefix(pr, "log network: Failed to load resource: the server responded with a status of 422 ") {
				probs = append(probs, pr)
			}
		}
		if len(probs) > 0 {
			t.Errorf("%s: console errors or policy violations: %q", where, probs)
		}
	}
	base := "http://" + adminAddr

	// Without a session the overview sends Chrome to the sign-in page.
	s := navigate(base + "/")
	if s.Path != "/login" || s.H1 != "Sign in" || !s.Styled {
		t.Fatalf("before sign-in: %+v", s)
	}
	clean("sign-in")
	shoot("login")
	p.Eval(`(() => { document.getElementById("password").value = `+jsString(adminTestPassword)+
		`; document.querySelector("form").submit(); return true })()`, nil)
	p.WaitLoad()
	s = readState(t, p)
	t.Logf("overview: path %s, title %q, sections %q", s.Path, s.Title, s.H2)
	if s.Path != "/" || s.Title != "Overview · cuttle" || s.H1 != "Overview" || !s.Styled || !s.Script ||
		!strings.Contains(s.H2, "Configuration") || !strings.Contains(s.H2, "Status") || !strings.Contains(s.Text, "Sign out") {
		t.Fatalf("overview after sign-in: %+v", s)
	}
	clean("overview")
	shoot("overview")

	// The session cookie keeps the D7 attributes in Chrome (assumption B1).
	var cookies struct {
		Cookies []struct {
			Name     string `json:"name"`
			HTTPOnly bool   `json:"httpOnly"`
			Secure   bool   `json:"secure"`
			SameSite string `json:"sameSite"`
			Session  bool   `json:"session"`
		} `json:"cookies"`
	}
	chrome.Call(p.Session, "Network.getCookies", map[string]any{"urls": []string{base + "/"}}, &cookies)
	found := false
	for _, ck := range cookies.Cookies {
		if ck.Name == "cuttle_session" {
			found = true
			if !ck.HTTPOnly || ck.Secure || ck.SameSite != "Strict" || !ck.Session {
				t.Errorf("cuttle_session in Chrome: %+v", ck)
			}
		}
	}
	if !found {
		t.Errorf("Chrome holds no cuttle_session cookie: %+v", cookies.Cookies)
	}

	pages := []struct{ path, h1, shot string }{
		{"/", "Overview", "overview"},
		{"/domains?route=block&window=1h", "Domains", "domains"},
		{"/live", "Live", "live"},
		{"/rules", "Rules", "rules"},
		{"/rules/new", "Add rule", "rule-new"},
		{"/upstreams", "Upstreams", "upstreams"},
		{"/interfaces", "Interfaces", "interfaces"},
		{"/proxy", "Proxy", "proxy"},
		{"/remote", "Remote clients", "remote"},
		{"/general", "General", "general"},
		{"/account", "Account", "account"},
		{"/settings", "Advanced", "advanced"},
	}
	for _, pg := range pages[1:] {
		s := navigate(base + pg.path)
		if s.H1 != pg.h1 || !s.Styled || !s.Script || s.Path != strings.SplitN(pg.path, "?", 2)[0] {
			t.Errorf("%s: %+v", pg.path, s)
		}
		clean(pg.path)
		shoot(pg.shot)
	}
	s = readState(t, p)
	if !strings.Contains(s.Text, "name = \"cuttle-test\"") && !strings.Contains(s.Text, "Configuration") {
		t.Errorf("Advanced page text: %q", s.Text)
	}

	formFlows(t, chrome, p, base, proxyAddr, sb.ConfigPath(), shoot, clean)

	// Phone width, in both appearances: no page scrolls sideways.
	p.Viewport(390, 844, true)
	for _, scheme := range []string{"light", "dark"} {
		p.ColorScheme(scheme)
		for _, pg := range pages {
			s := navigate(base + pg.path)
			if s.Scroll > s.Width || s.H1 != pg.h1 {
				t.Errorf("%s at 390 px, %s: content %d px, viewport %d px, h1 %q", pg.path, scheme, s.Scroll, s.Width, s.H1)
			}
			clean(pg.path + " at 390 px, " + scheme)
			shoot(pg.shot + "-phone-" + scheme)
		}
	}

	chrome.Close()
	if code := proc.Stop(); code != 0 {
		t.Fatalf("cuttle exit %d: %s", code, proc.Stderr())
	}
	log, _ := os.ReadFile(sb.LogPath())
	var sent []string
	for _, l := range strings.Split(string(log), "\n") {
		if strings.Contains(l, "msg=decision") {
			sent = append(sent, l)
		}
	}
	t.Logf("Chrome's requests through the proxy: %d decision lines", len(sent))
	for _, l := range sent {
		if !strings.Contains(l, "action=block") {
			t.Errorf("a request left through the proxy: %s", l)
		}
	}
	if strings.Contains(string(log), adminTestPassword) || strings.Contains(string(log), chromeNewPassword) {
		t.Error("the log holds a password")
	}
}

// jsString quotes s as a JavaScript string literal.
func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

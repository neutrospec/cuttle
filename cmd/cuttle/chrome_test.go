package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"squid/internal/testenv"
)

// defaultChrome is Chrome's binary on macOS (design 0007 D8).
const defaultChrome = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"

// cdp is a Chrome DevTools Protocol client over --remote-debugging-pipe:
// Chrome reads NUL-terminated JSON messages on its descriptor 3 and writes
// them on descriptor 4, so no debugging port is opened.
type cdp struct {
	t      *testing.T
	w      *os.File
	msgs   chan cdpMsg
	id     int
	events []cdpMsg
}

type cdpMsg struct {
	ID        int             `json:"id"`
	Method    string          `json:"method"`
	SessionID string          `json:"sessionId"`
	Params    json.RawMessage `json:"params"`
	Result    json.RawMessage `json:"result"`
	Error     *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func newCDP(t *testing.T, w, r *os.File) *cdp {
	c := &cdp{t: t, w: w, msgs: make(chan cdpMsg, 256)}
	go func() {
		defer close(c.msgs)
		br := bufio.NewReader(r)
		for {
			b, err := br.ReadBytes(0)
			if err != nil {
				return
			}
			var m cdpMsg
			if json.Unmarshal(bytes.TrimSuffix(b, []byte{0}), &m) == nil {
				c.msgs <- m
			}
		}
	}()
	return c
}

// next returns the next message within the deadline.
func (c *cdp) next(deadline time.Time) cdpMsg {
	c.t.Helper()
	select {
	case m, ok := <-c.msgs:
		if !ok {
			c.t.Fatal("Chrome closed the DevTools pipe")
		}
		return m
	case <-time.After(time.Until(deadline)):
		c.t.Fatal("no answer from Chrome")
	}
	return cdpMsg{}
}

// call sends a command and returns its result; events that arrive
// meanwhile are kept for wait.
func (c *cdp) call(session, method string, params any, result any) {
	c.t.Helper()
	c.id++
	msg := map[string]any{"id": c.id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	if session != "" {
		msg["sessionId"] = session
	}
	b, _ := json.Marshal(msg)
	if _, err := c.w.Write(append(b, 0)); err != nil {
		c.t.Fatalf("%s: %v", method, err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		m := c.next(deadline)
		if m.ID == 0 {
			c.events = append(c.events, m)
			continue
		}
		if m.ID != c.id {
			continue
		}
		if m.Error != nil {
			c.t.Fatalf("%s: %d %s", method, m.Error.Code, m.Error.Message)
		}
		if result != nil {
			if err := json.Unmarshal(m.Result, result); err != nil {
				c.t.Fatalf("%s: %v", method, err)
			}
		}
		return
	}
}

// wait returns once an event with this method arrived for the session,
// and forgets it and every event before it.
func (c *cdp) wait(session, method string) {
	c.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		for i, e := range c.events {
			if e.Method == method && e.SessionID == session {
				c.events = c.events[i+1:]
				return
			}
		}
		m := c.next(deadline)
		if m.ID == 0 {
			c.events = append(c.events, m)
		}
	}
}

// eval evaluates a JavaScript expression in the page through DevTools,
// which the page's Content-Security-Policy does not govern, and decodes
// its JSON value.
func (c *cdp) eval(session, expr string, v any) {
	c.t.Helper()
	var res struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	c.call(session, "Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true}, &res)
	if res.ExceptionDetails != nil {
		c.t.Fatalf("%s: %s", expr, res.ExceptionDetails.Text)
	}
	if v != nil {
		if err := json.Unmarshal(res.Result.Value, v); err != nil {
			c.t.Fatalf("%s: %v in %s", expr, err, res.Result.Value)
		}
	}
}

// pageState is what the check reads from a rendered page.
type pageState struct {
	Path  string `json:"path"`
	Title string `json:"title"`
	H1    string `json:"h1"`
	H2    string `json:"h2"`
	Text  string `json:"text"`
	// Styled reports whether the embedded stylesheet applied.
	Styled bool `json:"styled"`
	Width  int  `json:"width"`
	Scroll int  `json:"scroll"`
}

const readPage = `JSON.stringify({
  path: location.pathname, title: document.title,
  h1: (document.querySelector("h1") || {}).textContent || "",
  h2: Array.from(document.querySelectorAll("h2")).map(e => e.textContent).join("|"),
  text: document.body.innerText.slice(0, 4000),
  styled: getComputedStyle(document.body).margin === "0px",
  width: document.documentElement.clientWidth, scroll: document.documentElement.scrollWidth
})`

func (c *cdp) page(session string) pageState {
	c.t.Helper()
	var raw string
	c.eval(session, readPage, &raw)
	var p pageState
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		c.t.Fatal(err)
	}
	return p
}

// TestAdminChrome is the T2 admin check of design 0005 A10 and 0007 D7:
// headless Chrome, with a throwaway profile under a temporary directory and
// the mock keychain, signs in at http://127.0.0.1:<port>/ of a cuttle
// binary that runs with temporary XDG directories and test ports, and
// renders the overview, the domain table, the live page, and the settings
// page. Chrome opens no window and no debugging port; its proxy is the
// instance's own proxy listener, whose block default answers Chrome's
// background requests with 403 before any name lookup, and every host
// name maps to NOTFOUND, so nothing leaves the machine.
//
// It starts a browser, so T0 and T1 skip it; scripts/admin-check runs it
// with CUTTLE_ADMIN_CHROME=1. CUTTLE_CHROME overrides the Chrome binary,
// and CUTTLE_ADMIN_CHROME_OUT names a directory for screenshots.
func TestAdminChrome(t *testing.T) {
	if os.Getenv("CUTTLE_ADMIN_CHROME") != "1" {
		t.Skip("starts headless Chrome; run scripts/admin-check")
	}
	chrome := os.Getenv("CUTTLE_CHROME")
	if chrome == "" {
		chrome = defaultChrome
	}
	if _, err := os.Stat(chrome); err != nil {
		t.Fatalf("Chrome: %v", err)
	}
	sb := testenv.NewSandbox(t, testenv.BuildCuttle(t))
	proxyAddr := testenv.Port(sb.FreePort())
	adminAddr := testenv.Port(sb.FreePort())
	writeConfig(t, sb, strings.Replace(serveConfig([]string{proxyAddr}, ""), `[admin]
listen = ""`, fmt.Sprintf("[admin]\nlisten = %q", adminAddr), 1))
	if r := sb.RunInput(10*time.Second, adminTestPassword+"\n", "admin", "passwd", "--stdin"); r.Code != 0 {
		t.Fatalf("admin passwd: %d %s", r.Code, r.Stderr)
	}
	proc := sb.Start(proxyAddr, "serve")
	waitLog(t, sb, `msg="admin UI listening" address=`+adminAddr)

	profile := filepath.Join(sb.Root, "chrome-profile")
	if err := os.Mkdir(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	toChrome, chromeIn, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	chromeOut, fromChrome, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(chrome,
		"--headless=new",
		"--use-mock-keychain",
		"--user-data-dir="+profile,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-background-networking",
		"--disable-component-update",
		"--disable-default-apps",
		"--disable-extensions",
		"--disable-sync",
		"--proxy-server=http://"+proxyAddr,
		"--host-resolver-rules=MAP * ~NOTFOUND, EXCLUDE 127.0.0.1",
		"--remote-debugging-pipe",
		"--window-size=1280,1400",
		"about:blank")
	// HOME and TMPDIR inside the sandbox; the pipes become descriptors 3
	// and 4.
	cmd.Env = sb.Env()
	cmd.ExtraFiles = []*os.File{toChrome, fromChrome}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var chromeErr lockedBuffer
	cmd.Stderr = &chromeErr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	toChrome.Close()
	fromChrome.Close()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-exited
		}
		chromeIn.Close()
		chromeOut.Close()
		if t.Failed() {
			t.Logf("Chrome stderr:\n%s", chromeErr.String())
		}
	})
	c := newCDP(t, chromeIn, chromeOut)

	var target struct {
		TargetID string `json:"targetId"`
	}
	c.call("", "Target.createTarget", map[string]any{"url": "about:blank"}, &target)
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	c.call("", "Target.attachToTarget", map[string]any{"targetId": target.TargetID, "flatten": true}, &attached)
	s := attached.SessionID
	c.call(s, "Page.enable", nil, nil)
	navigate := func(u string) pageState {
		t.Helper()
		c.call(s, "Page.navigate", map[string]any{"url": u}, nil)
		c.wait(s, "Page.loadEventFired")
		return c.page(s)
	}
	shoot := func(name string) {
		t.Helper()
		dir := os.Getenv("CUTTLE_ADMIN_CHROME_OUT")
		if dir == "" {
			return
		}
		var shot struct {
			Data string `json:"data"`
		}
		c.call(s, "Page.captureScreenshot", map[string]any{"format": "png", "captureBeyondViewport": true}, &shot)
		png, err := base64.StdEncoding.DecodeString(shot.Data)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name+".png"), png, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	base := "http://" + adminAddr

	// Without a session the overview sends Chrome to the sign-in page.
	p := navigate(base + "/")
	if p.Path != "/login" || p.H1 != "Sign in" || !p.Styled {
		t.Fatalf("before sign-in: %+v", p)
	}
	shoot("login")
	c.eval(s, `(() => { document.getElementById("password").value = `+jsString(adminTestPassword)+
		`; document.querySelector("form").submit(); return true })()`, nil)
	c.wait(s, "Page.loadEventFired")
	p = c.page(s)
	t.Logf("overview: path %s, title %q, sections %q", p.Path, p.Title, p.H2)
	if p.Path != "/" || p.Title != "Overview · cuttle" || p.H1 != "Overview" || !p.Styled ||
		!strings.Contains(p.H2, "Routes") || !strings.Contains(p.H2, "Status") || !strings.Contains(p.Text, "Signed in") {
		t.Fatalf("overview after sign-in: %+v", p)
	}
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
	c.call(s, "Network.getCookies", map[string]any{"urls": []string{base + "/"}}, &cookies)
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

	for _, pg := range []struct{ path, h1, shot string }{
		{"/domains?route=block&window=1h", "Domains", "domains"},
		{"/live", "Live", "live"},
		{"/settings", "Settings", "settings"},
	} {
		p := navigate(base + pg.path)
		if p.H1 != pg.h1 || !p.Styled || p.Path != strings.SplitN(pg.path, "?", 2)[0] {
			t.Errorf("%s: %+v", pg.path, p)
		}
		shoot(pg.shot)
	}
	p = c.page(s)
	if !strings.Contains(p.Text, "name = \"cuttle-test\"") && !strings.Contains(p.Text, "Configuration") {
		t.Errorf("settings page text: %q", p.Text)
	}

	// Phone width: no horizontal scrolling on the overview.
	c.call(s, "Emulation.setDeviceMetricsOverride", map[string]any{"width": 390, "height": 844, "deviceScaleFactor": 1, "mobile": true}, nil)
	p = navigate(base + "/")
	if p.Scroll > p.Width {
		t.Errorf("overview at 390 px scrolls sideways: content %d px, viewport %d px", p.Scroll, p.Width)
	}
	shoot("overview-phone")

	c.call("", "Browser.close", nil, nil)
	select {
	case <-exited:
		stopped = true
	case <-time.After(15 * time.Second):
		t.Error("Chrome did not exit after Browser.close")
	}
	if out, err := exec.Command("/usr/bin/pgrep", "-f", profile).Output(); err == nil {
		t.Errorf("Chrome processes left with the throwaway profile: %s", out)
	}
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
	if strings.Contains(string(log), adminTestPassword) {
		t.Error("the log holds the password")
	}
}

// jsString quotes s as a JavaScript string literal.
func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

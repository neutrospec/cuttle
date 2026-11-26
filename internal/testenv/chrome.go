package testenv

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
)

// DefaultChrome is Chrome's binary on macOS (design 0007 D8).
const DefaultChrome = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"

// ChromeOptions configure StartChrome.
type ChromeOptions struct {
	// Dir is an existing test directory; the throwaway profile goes in a
	// new directory inside it.
	Dir string
	// Env is Chrome's environment, with HOME and TMPDIR inside the test.
	Env []string
	// Proxy is the HTTP proxy for every request but loopback, such as an
	// instance's block-by-default listener; Chrome reaches 127.0.0.1
	// directly.
	Proxy string
}

// Chrome is headless Chrome with a throwaway profile and the mock
// keychain, driven over the DevTools pipe (design 0007 D8): Chrome reads
// NUL-terminated JSON messages on its descriptor 3 and writes them on
// descriptor 4, so no debugging port is opened and no window opens.
// Every host name but 127.0.0.1 resolves to NOTFOUND.
type Chrome struct {
	t       testing.TB
	cmd     *exec.Cmd
	w       *os.File
	msgs    chan cdpMsg
	id      int
	events  []cdpMsg
	profile string
	exited  chan error
	stopped bool
	stderr  syncBuffer
	// problems are console errors, exceptions, and error log entries per
	// session, in arrival order.
	problems map[string][]string
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

// StartChrome starts headless Chrome; CUTTLE_CHROME overrides the
// binary. The test's cleanup kills Chrome if Close did not run.
func StartChrome(t testing.TB, opt ChromeOptions) *Chrome {
	t.Helper()
	bin := os.Getenv("CUTTLE_CHROME")
	if bin == "" {
		bin = DefaultChrome
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("Chrome: %v", err)
	}
	profile, err := os.MkdirTemp(opt.Dir, "chrome-profile-")
	if err != nil {
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
	args := []string{
		"--headless=new",
		"--use-mock-keychain",
		"--user-data-dir=" + profile,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-background-networking",
		"--disable-component-update",
		// Without it Chrome installs and wakes the user's own Google
		// updater (GoogleUpdater --install and --wake under the real home
		// directory, whatever HOME says) shortly after it starts (OI-0006
		// T-R2).
		"--disable-updater-scheduler",
		"--disable-default-apps",
		"--disable-extensions",
		"--disable-sync",
		"--host-resolver-rules=MAP * ~NOTFOUND, EXCLUDE 127.0.0.1",
		"--remote-debugging-pipe",
		"--window-size=1280,1400",
	}
	if opt.Proxy != "" {
		args = append(args, "--proxy-server=http://"+opt.Proxy)
	}
	c := &Chrome{t: t, w: chromeIn, msgs: make(chan cdpMsg, 256), profile: profile,
		exited: make(chan error, 1), problems: map[string][]string{}}
	c.cmd = exec.Command(bin, append(args, "about:blank")...)
	c.cmd.Env = opt.Env
	c.cmd.ExtraFiles = []*os.File{toChrome, fromChrome}
	c.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.cmd.Stderr = &c.stderr
	if err := c.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	toChrome.Close()
	fromChrome.Close()
	go func() { c.exited <- c.cmd.Wait() }()
	go func() {
		defer close(c.msgs)
		br := bufio.NewReader(chromeOut)
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
	t.Cleanup(func() {
		if !c.stopped {
			syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
			<-c.exited
		}
		chromeIn.Close()
		chromeOut.Close()
		if t.Failed() {
			t.Logf("Chrome stderr:\n%s", c.stderr.String())
		}
	})
	return c
}

// next returns the next message within the deadline and notes the
// problems that events report.
func (c *Chrome) next(deadline time.Time) cdpMsg {
	c.t.Helper()
	select {
	case m, ok := <-c.msgs:
		if !ok {
			c.t.Fatal("Chrome closed the DevTools pipe")
		}
		if m.ID == 0 {
			c.note(m)
		}
		return m
	case <-time.After(time.Until(deadline)):
		c.t.Fatal("no answer from Chrome")
	}
	return cdpMsg{}
}

// note keeps console errors, exceptions, and error log entries.
func (c *Chrome) note(m cdpMsg) {
	var p struct {
		Type string `json:"type"`
		Args []struct {
			Value       any    `json:"value"`
			Description string `json:"description"`
		} `json:"args"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception *struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
		Entry *struct {
			Level  string `json:"level"`
			Source string `json:"source"`
			Text   string `json:"text"`
			URL    string `json:"url"`
		} `json:"entry"`
	}
	switch m.Method {
	case "Runtime.consoleAPICalled", "Runtime.exceptionThrown", "Log.entryAdded":
	default:
		return
	}
	if json.Unmarshal(m.Params, &p) != nil {
		return
	}
	var msg string
	switch {
	case m.Method == "Runtime.consoleAPICalled" && (p.Type == "error" || p.Type == "assert"):
		var parts []string
		for _, a := range p.Args {
			if a.Description != "" {
				parts = append(parts, a.Description)
			} else {
				parts = append(parts, fmt.Sprint(a.Value))
			}
		}
		msg = "console." + p.Type + ": " + strings.Join(parts, " ")
	case m.Method == "Runtime.exceptionThrown" && p.ExceptionDetails != nil:
		msg = "exception: " + p.ExceptionDetails.Text
		if p.ExceptionDetails.Exception != nil {
			msg += " " + p.ExceptionDetails.Exception.Description
		}
	case m.Method == "Log.entryAdded" && p.Entry != nil && p.Entry.Level == "error":
		msg = "log " + p.Entry.Source + ": " + p.Entry.Text + " " + p.Entry.URL
	}
	if msg != "" {
		c.problems[m.SessionID] = append(c.problems[m.SessionID], msg)
	}
}

// Call sends a command and decodes its result into result unless nil;
// events that arrive meanwhile are kept for Wait.
func (c *Chrome) Call(session, method string, params any, result any) {
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

// Wait returns the parameters of the next event with this method for the
// session, and forgets it and every event before it.
func (c *Chrome) Wait(session, method string) json.RawMessage {
	c.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		for i, e := range c.events {
			if e.Method == method && e.SessionID == session {
				c.events = c.events[i+1:]
				return e.Params
			}
		}
		m := c.next(deadline)
		if m.ID == 0 {
			c.events = append(c.events, m)
		}
	}
}

// Close closes Chrome through DevTools, waits for it to exit, and checks
// that no process with the throwaway profile is left.
func (c *Chrome) Close() {
	c.t.Helper()
	c.Call("", "Browser.close", nil, nil)
	select {
	case <-c.exited:
		c.stopped = true
	case <-time.After(15 * time.Second):
		c.t.Error("Chrome did not exit after Browser.close")
	}
	if out, err := exec.Command("/usr/bin/pgrep", "-f", c.profile).Output(); err == nil {
		c.t.Errorf("Chrome processes left with the throwaway profile: %s", out)
	}
	// The updater, when Chrome starts it, logs to the stderr it inherits.
	if strings.Contains(c.stderr.String(), "chrome/updater/") {
		c.t.Error("Chrome started the Google updater (OI-0006 T-R2)")
	}
}

// ChromePage is one tab of a Chrome.
type ChromePage struct {
	c       *Chrome
	Session string
}

// cspProbe records Content-Security-Policy violations in the page; it
// runs through DevTools, which the page's policy does not govern.
const cspProbe = `window.__cspViolations = [];
document.addEventListener("securitypolicyviolation", (e) => {
  window.__cspViolations.push(e.violatedDirective + " " + (e.blockedURI || "inline") + " " + e.sourceFile + ":" + e.lineNumber);
});`

// NewPage opens a tab and attaches to it with the page, runtime, and log
// domains enabled, and the Content-Security-Policy probe installed.
func (c *Chrome) NewPage() *ChromePage {
	c.t.Helper()
	var target struct {
		TargetID string `json:"targetId"`
	}
	c.Call("", "Target.createTarget", map[string]any{"url": "about:blank"}, &target)
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	c.Call("", "Target.attachToTarget", map[string]any{"targetId": target.TargetID, "flatten": true}, &attached)
	p := &ChromePage{c: c, Session: attached.SessionID}
	for _, d := range []string{"Page.enable", "Runtime.enable", "Log.enable"} {
		c.Call(p.Session, d, nil, nil)
	}
	c.Call(p.Session, "Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": cspProbe}, nil)
	return p
}

// Navigate loads url and waits for its load event. A load event that
// arrived earlier, such as a page's own refresh, is dropped first.
func (p *ChromePage) Navigate(url string) {
	p.c.t.Helper()
	kept := p.c.events[:0]
	for _, e := range p.c.events {
		if e.Method != "Page.loadEventFired" || e.SessionID != p.Session {
			kept = append(kept, e)
		}
	}
	p.c.events = kept
	p.c.Call(p.Session, "Page.navigate", map[string]any{"url": url}, nil)
	p.WaitLoad()
}

// WaitLoad waits for the page's next load event.
func (p *ChromePage) WaitLoad() {
	p.c.t.Helper()
	p.c.Wait(p.Session, "Page.loadEventFired")
}

// Eval evaluates a JavaScript expression in the page, awaiting a promise,
// and decodes its JSON value into v unless nil. DevTools evaluation is
// not governed by the page's Content-Security-Policy.
func (p *ChromePage) Eval(expr string, v any) {
	p.c.t.Helper()
	var res struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception *struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	p.c.Call(p.Session, "Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true, "awaitPromise": true}, &res)
	if d := res.ExceptionDetails; d != nil {
		desc := d.Text
		if d.Exception != nil {
			desc += " " + d.Exception.Description
		}
		p.c.t.Fatalf("%s: %s", expr, desc)
	}
	if v != nil {
		if err := json.Unmarshal(res.Result.Value, v); err != nil {
			p.c.t.Fatalf("%s: %v in %s", expr, err, res.Result.Value)
		}
	}
}

// Viewport sets the page's width and height in CSS pixels; mobile
// emulates a phone.
func (p *ChromePage) Viewport(width, height int, mobile bool) {
	p.c.t.Helper()
	p.c.Call(p.Session, "Emulation.setDeviceMetricsOverride",
		map[string]any{"width": width, "height": height, "deviceScaleFactor": 1, "mobile": mobile}, nil)
}

// ColorScheme emulates prefers-color-scheme: light or dark.
func (p *ChromePage) ColorScheme(scheme string) {
	p.c.t.Helper()
	p.c.Call(p.Session, "Emulation.setEmulatedMedia",
		map[string]any{"features": []map[string]string{{"name": "prefers-color-scheme", "value": scheme}}}, nil)
}

// Key presses and releases a key, such as Escape, with the key code
// DevTools needs for keys that have a default action.
func (p *ChromePage) Key(key string, code int) {
	p.c.t.Helper()
	for _, typ := range []string{"keyDown", "keyUp"} {
		p.c.Call(p.Session, "Input.dispatchKeyEvent",
			map[string]any{"type": typ, "key": key, "code": key, "windowsVirtualKeyCode": code}, nil)
	}
}

// Screenshot writes a PNG of the whole page to path.
func (p *ChromePage) Screenshot(path string) {
	p.c.t.Helper()
	var m struct {
		Content struct {
			Width  float64 `json:"width"`
			Height float64 `json:"height"`
		} `json:"cssContentSize"`
	}
	p.c.Call(p.Session, "Page.getLayoutMetrics", nil, &m)
	var shot struct {
		Data string `json:"data"`
	}
	p.c.Call(p.Session, "Page.captureScreenshot", map[string]any{"format": "png", "captureBeyondViewport": true,
		"clip": map[string]any{"x": 0, "y": 0, "width": m.Content.Width, "height": m.Content.Height, "scale": 1}}, &shot)
	png, err := base64.StdEncoding.DecodeString(shot.Data)
	if err != nil {
		p.c.t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		p.c.t.Fatal(err)
	}
	if err := os.WriteFile(path, png, 0o600); err != nil {
		p.c.t.Fatal(err)
	}
}

// Problems returns the console errors, exceptions, error log entries, and
// Content-Security-Policy violations of the page since the last call.
func (p *ChromePage) Problems() []string {
	p.c.t.Helper()
	// Drain the events that arrived without a call.
	p.c.Call(p.Session, "Runtime.evaluate", map[string]any{"expression": "0"}, nil)
	var csp []string
	p.Eval(`(() => { const v = window.__cspViolations || []; window.__cspViolations = []; return v; })()`, &csp)
	out := append(p.c.problems[p.Session], csp...)
	p.c.problems[p.Session] = nil
	return out
}

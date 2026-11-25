package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"squid/internal/testenv"
)

var phcFile = regexp.MustCompile(`^\$argon2id\$v=19\$m=19456,t=2,p=1\$[A-Za-z0-9+/]{22}\$[A-Za-z0-9+/]{43}\n$`)

// checkPasswordFile checks the A2 file: one PHC string with the D6
// parameters, mode 0600, in a 0700 directory.
func checkPasswordFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !phcFile.Match(data) {
		t.Errorf("password file %q is not an Argon2id PHC string with m=19456,t=2,p=1", data)
	}
	fi, _ := os.Stat(path)
	di, _ := os.Stat(filepath.Dir(path))
	if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
		t.Errorf("modes: file %v, directory %v", fi.Mode(), di.Mode())
	}
	return string(data)
}

// TestAdminPassword checks the command part of A2: cuttle admin passwd
// --stdin writes a 0600 Argon2id PHC string with m=19456,t=2,p=1 and
// refuses an 11-byte password; it also refuses 129 bytes, and without
// --stdin it needs a terminal. It runs in-process for T0 and against the
// built binary for T1.
func TestAdminPassword(t *testing.T) {
	lookup, vars := tempEnv(t)
	path := filepath.Join(vars["XDG_DATA_HOME"], "cuttle", "admin-password")
	passwd := func(input string, args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := cmdAdmin(append([]string{"passwd"}, args...), lookup, strings.NewReader(input), &out, &errb)
		return code, out.String(), errb.String()
	}
	for _, in := range []string{"", "\n", "eleven-byte\n", "eleven-byte", strings.Repeat("x", 129) + "\n", strings.Repeat("x", 5000)} {
		if code, _, errOut := passwd(in, "--stdin"); code != exitFailure || !strings.Contains(errOut, "bytes") {
			t.Errorf("%d-byte input: exit %d %q", len(in), code, errOut)
		}
		if _, err := os.Stat(path); err == nil {
			t.Fatalf("%d-byte input wrote the file", len(in))
		}
	}
	if code, _, errOut := passwd("a long enough password\n"); code != exitFailure || !strings.Contains(errOut, "--stdin") {
		t.Errorf("without a terminal: exit %d %q", code, errOut)
	}
	code, out, errOut := passwd("twelve bytes\r\n", "--stdin")
	if code != exitOK || out != path+"\n" {
		t.Fatalf("12 bytes: exit %d %q %q", code, out, errOut)
	}
	first := checkPasswordFile(t, path)
	if code, _, _ := passwd(strings.Repeat("y", 128), "--stdin"); code != exitOK {
		t.Fatalf("128 bytes: exit %d", code)
	}
	if second := checkPasswordFile(t, path); second == first {
		t.Error("a new password left the file unchanged")
	}

	if testing.Short() {
		return
	}
	sb := testenv.NewSandbox(t, testenv.BuildCuttle(t))
	binPath := filepath.Join(sb.DataHome, "cuttle", "admin-password")
	if r := sb.RunInput(10*time.Second, "eleven-byte\n", "admin", "passwd", "--stdin"); r.Code != 1 || !strings.Contains(r.Stderr, "at least 12 bytes") {
		t.Errorf("binary, 11 bytes: exit %d %q", r.Code, r.Stderr)
	}
	if _, err := os.Stat(binPath); err == nil {
		t.Fatal("binary, 11 bytes: wrote the file")
	}
	if r := sb.RunInput(10*time.Second, "correct horse battery\n", "admin", "passwd", "--stdin"); r.Code != 0 || r.Stdout != binPath+"\n" {
		t.Fatalf("binary: exit %d %q %q", r.Code, r.Stdout, r.Stderr)
	}
	checkPasswordFile(t, binPath)
	if r := sb.Run(10*time.Second, "paths"); !strings.Contains(r.Stdout, binPath) || strings.Contains(r.Stdout, "missing") {
		t.Errorf("paths after passwd:\n%s", r.Stdout)
	}
}

// adminClient is a Go client with a cookie jar that does not follow
// redirects.
func adminClient(t *testing.T) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	tr := &http.Transport{Proxy: nil}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Jar: jar, Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

var adminTokenRE = regexp.MustCompile(`name="(login_token|csrf)" value="([^"]+)"`)

// adminGet fetches a page and returns the response, its body, and the
// form token it holds, if any.
func adminGet(t *testing.T, c *http.Client, u string) (*http.Response, string, string) {
	t.Helper()
	res, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	var tok string
	if m := adminTokenRE.FindSubmatch(body); m != nil {
		tok = string(m[2])
	}
	return res, string(body), tok
}

func adminPost(t *testing.T, c *http.Client, u string, form url.Values) (*http.Response, string) {
	t.Helper()
	res, err := c.PostForm(u, form)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res, string(body)
}

// rawAdmin sends one raw request and returns the status line.
func rawAdmin(t *testing.T, addr, request string) string {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	io.WriteString(c, request)
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("%q: %v", request, err)
	}
	return strings.TrimSpace(line)
}

// TestAdminServe runs the admin UI in the built binary on a port the test
// chose (A1–A3, A6, A7 end to end): without a password file the log and
// /login name cuttle admin passwd; a foreign Host, also in an
// absolute-form request, gets 403; after cuttle admin passwd --stdin a
// Go client signs in with the cookie attributes of D7 and gets the D5
// headers; a wrong password gets 401; a new password ends the session; and
// logout ends the next one. SIGTERM then exits 0.
func TestAdminServe(t *testing.T) {
	if testing.Short() {
		t.Skip("binary test")
	}
	sb := testenv.NewSandbox(t, testenv.BuildCuttle(t))
	proxyAddr := testenv.Port(sb.FreePort())
	adminPort := sb.FreePort()
	adminAddr := testenv.Port(adminPort)
	writeConfig(t, sb, strings.Replace(serveConfig([]string{proxyAddr}, ""), `[admin]
listen = ""`, fmt.Sprintf("[admin]\nlisten = %q", adminAddr), 1))
	proc := sb.Start(proxyAddr, "serve")
	waitLog(t, sb, `msg="admin UI listening" address=`+adminAddr)
	waitLog(t, sb, `msg="admin login disabled until cuttle admin passwd runs" reason="no password is set"`)
	base := "http://" + adminAddr
	c := adminClient(t)

	res, body, tok := adminGet(t, c, base+"/login")
	if res.StatusCode != 200 || !strings.Contains(body, "cuttle admin passwd") || tok != "" {
		t.Fatalf("/login without a password: %d %q\n%s", res.StatusCode, tok, body)
	}

	// A1 over TCP: the Host header and an absolute-form target.
	for _, r := range []string{
		"GET /login HTTP/1.1\r\nHost: evil.example:" + fmt.Sprint(adminPort) + "\r\n\r\n",
		"GET http://evil.example:" + fmt.Sprint(adminPort) + "/login HTTP/1.1\r\nHost: " + adminAddr + "\r\n\r\n",
		"GET /login HTTP/1.0\r\n\r\n",
	} {
		if line := rawAdmin(t, adminAddr, r); !strings.Contains(line, " 403 ") {
			t.Errorf("%q: %s", r, line)
		}
	}
	for _, host := range []string{adminAddr, fmt.Sprintf("localhost:%d", adminPort)} {
		if line := rawAdmin(t, adminAddr, "GET /login HTTP/1.1\r\nHost: "+host+"\r\n\r\n"); !strings.Contains(line, " 200 ") {
			t.Errorf("Host %s: %s", host, line)
		}
	}

	if r := sb.RunInput(10*time.Second, "correct horse battery\n", "admin", "passwd", "--stdin"); r.Code != 0 {
		t.Fatalf("admin passwd: %d %s", r.Code, r.Stderr)
	}
	login := func(pw string) *http.Response {
		t.Helper()
		res, body, tok := adminGet(t, c, base+"/login")
		if res.StatusCode != 200 || tok == "" {
			t.Fatalf("/login: %d\n%s", res.StatusCode, body)
		}
		res, _ = adminPost(t, c, base+"/login", url.Values{"login_token": {tok}, "password": {pw}})
		return res
	}
	if res := login("wrong password!"); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", res.StatusCode)
	}
	waitLog(t, sb, `msg="admin login failed" consecutive_failures=1`)
	res = login("correct horse battery")
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/" {
		t.Fatalf("login: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	var raw string
	for _, l := range res.Header.Values("Set-Cookie") {
		if strings.HasPrefix(l, "cuttle_session=") {
			raw = l
		}
	}
	if !regexp.MustCompile(`^cuttle_session=[A-Za-z0-9_-]{43}; Path=/; HttpOnly; SameSite=Strict$`).MatchString(raw) {
		t.Errorf("session cookie %q", raw)
	}
	res, body, csrf := adminGet(t, c, base+"/")
	if res.StatusCode != 200 || !strings.Contains(body, "Signed in") || csrf == "" {
		t.Fatalf("/: %d\n%s", res.StatusCode, body)
	}
	for k, v := range map[string]string{
		"Content-Security-Policy": "default-src 'self'; frame-ancestors 'none'; form-action 'self'; base-uri 'none'",
		"X-Frame-Options":         "DENY", "Referrer-Policy": "no-referrer", "X-Content-Type-Options": "nosniff",
		"Cache-Control": "no-store",
	} {
		if res.Header.Get(k) != v {
			t.Errorf("%s: %q", k, res.Header.Get(k))
		}
	}

	// A new password ends the session (A6).
	if r := sb.RunInput(10*time.Second, "another long password\n", "admin", "passwd", "--stdin"); r.Code != 0 {
		t.Fatalf("admin passwd: %d %s", r.Code, r.Stderr)
	}
	if res, _, _ := adminGet(t, c, base+"/"); res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/login" {
		t.Fatalf("/ after a new password: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	if res := login("another long password"); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("login with the new password: %d", res.StatusCode)
	}
	_, _, csrf = adminGet(t, c, base+"/")
	if res, _ := adminPost(t, c, base+"/logout", url.Values{}); res.StatusCode != http.StatusForbidden {
		t.Errorf("logout without the CSRF token: %d", res.StatusCode)
	}
	if res, _ := adminPost(t, c, base+"/logout", url.Values{"csrf": {csrf}}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("logout: %d", res.StatusCode)
	}
	if res, _, _ := adminGet(t, c, base+"/"); res.StatusCode != http.StatusSeeOther {
		t.Errorf("/ after logout: %d", res.StatusCode)
	}

	if code := proc.Stop(); code != 0 {
		t.Fatalf("exit %d: %s", code, proc.Stderr())
	}
	log := waitLog(t, sb, `msg=stopped`)
	if strings.Contains(log, "correct horse battery") || strings.Contains(log, "another long password") || strings.Contains(log, "wrong password!") {
		t.Error("the log holds a password")
	}
	if c, err := net.DialTimeout("tcp", adminAddr, time.Second); err == nil {
		c.Close()
		t.Error("admin listener still open after exit")
	}
}

// TestAdminListenerBusy checks that an admin port held by another program
// leaves cuttle serving the proxy without the admin UI, with an error in
// the log.
func TestAdminListenerBusy(t *testing.T) {
	if testing.Short() {
		t.Skip("binary test")
	}
	sb := testenv.NewSandbox(t, testenv.BuildCuttle(t))
	proxyAddr := testenv.Port(sb.FreePort())
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	adminAddr := busy.Addr().String()
	sb.MarkChosen(uint16(busy.Addr().(*net.TCPAddr).Port))
	writeConfig(t, sb, strings.Replace(serveConfig([]string{proxyAddr}, ""), `[admin]
listen = ""`, fmt.Sprintf("[admin]\nlisten = %q", adminAddr), 1))
	proc := sb.Start(proxyAddr, "serve")
	waitLog(t, sb, `msg="admin listener unavailable; running without the admin UI" address=`+adminAddr)
	if st := statusJSON(t, sb); len(st.Listeners) != 1 || st.Listeners[0].State != "bound" {
		t.Errorf("listeners %+v", st.Listeners)
	}
	if code := proc.Stop(); code != 0 {
		t.Fatalf("exit %d: %s", code, proc.Stderr())
	}
}

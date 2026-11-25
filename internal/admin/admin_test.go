package admin

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testPassword = "correct horse battery"
	testListen   = "127.0.0.1:41729"
	testHost     = "127.0.0.1:41729"
	// noHost sends a request without a Host value.
	noHost = "\x00"
)

// clock is the injected clock of the tests (design 0005: "Tests use an
// injected clock for hours and timeouts").
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fixture is a Server with a password file in a temporary directory and a
// fake backend.
type fixture struct {
	t      *testing.T
	s      *Server
	clock  *clock
	pwPath string
	be     *fakeBackend
}

func newFixture(t *testing.T, withPassword bool) *fixture {
	t.Helper()
	f := &fixture{
		t:      t,
		clock:  &clock{t: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)},
		pwPath: filepath.Join(t.TempDir(), "share", "cuttle", "admin-password"),
	}
	f.be = newFakeBackend(t)
	if withPassword {
		f.setPassword(testPassword)
	}
	f.s = New(Options{Listen: netip.MustParseAddrPort(testListen), PasswordPath: f.pwPath, Backend: f.be,
		Now: f.clock.Now, Location: time.UTC})
	return f
}

func (f *fixture) setPassword(pw string) {
	f.t.Helper()
	phc, err := HashPassword([]byte(pw))
	if err != nil {
		f.t.Fatal(err)
	}
	if err := WritePasswordFile(f.pwPath, phc); err != nil {
		f.t.Fatal(err)
	}
}

// browser keeps cookies like a browser on one origin and sends the fetch
// metadata a same-origin form submission carries.
type browser struct {
	f       *fixture
	host    string
	cookies map[string]string
}

func (f *fixture) browser() *browser {
	return &browser{f: f, host: testHost, cookies: map[string]string{}}
}

// req describes one request; zero fields take the browser's defaults.
type req struct {
	method, path string
	host         string
	form         url.Values
	header       http.Header
	// noCookies sends no cookies; cookies replaces the jar for this request.
	noCookies bool
	cookies   map[string]string
	// crossSite replaces the same-origin fetch metadata with headers of a
	// cross-site form submission.
	crossSite bool
}

func (b *browser) do(r req) *http.Response {
	b.f.t.Helper()
	host := r.host
	switch host {
	case "":
		host = b.host
	case noHost:
		host = ""
	}
	var body io.Reader
	if r.form != nil {
		body = strings.NewReader(r.form.Encode())
	}
	hr := httptest.NewRequest(r.method, "http://"+testHost+r.path, body)
	hr.Host = host
	if r.form != nil {
		hr.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if r.method == http.MethodPost {
		if r.crossSite {
			hr.Header.Set("Sec-Fetch-Site", "cross-site")
			hr.Header.Set("Origin", "http://evil.example")
		} else {
			hr.Header.Set("Sec-Fetch-Site", "same-origin")
			hr.Header.Set("Origin", "http://"+host)
		}
	}
	for k, vs := range r.header {
		hr.Header.Del(k)
		for _, v := range vs {
			hr.Header.Add(k, v)
		}
	}
	jar := b.cookies
	if r.cookies != nil {
		jar = r.cookies
	}
	if !r.noCookies {
		for k, v := range jar {
			hr.AddCookie(&http.Cookie{Name: k, Value: v})
		}
	}
	rec := httptest.NewRecorder()
	b.f.s.ServeHTTP(rec, hr)
	res := rec.Result()
	if r.cookies == nil && !r.noCookies {
		for _, c := range res.Cookies() {
			if c.MaxAge < 0 {
				delete(b.cookies, c.Name)
			} else {
				b.cookies[c.Name] = c.Value
			}
		}
	}
	return res
}

func (b *browser) get(path string) *http.Response {
	return b.do(req{method: http.MethodGet, path: path})
}

func bodyOf(t *testing.T, res *http.Response) string {
	t.Helper()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

var (
	loginTokenRE = regexp.MustCompile(`name="login_token" value="([^"]+)"`)
	csrfRE       = regexp.MustCompile(`name="csrf" value="([^"]+)"`)
)

// loginToken loads /login and returns the form's token.
func (b *browser) loginToken() string {
	b.f.t.Helper()
	res := b.get("/login")
	body := bodyOf(b.f.t, res)
	m := loginTokenRE.FindStringSubmatch(body)
	if res.StatusCode != http.StatusOK || m == nil {
		b.f.t.Fatalf("GET /login: %d, no login token:\n%s", res.StatusCode, body)
	}
	if b.cookies[LoginCookie] != m[1] {
		b.f.t.Fatalf("cuttle_login cookie %q differs from the form token %q", b.cookies[LoginCookie], m[1])
	}
	return m[1]
}

// tryLogin loads the form and submits pw.
func (b *browser) tryLogin(pw string) *http.Response {
	b.f.t.Helper()
	tok := b.loginToken()
	return b.do(req{method: http.MethodPost, path: "/login", form: url.Values{"login_token": {tok}, "password": {pw}}})
}

// login signs in and returns the CSRF token of the session.
func (b *browser) login() string {
	b.f.t.Helper()
	res := b.tryLogin(testPassword)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/" {
		b.f.t.Fatalf("login: %d %q:\n%s", res.StatusCode, res.Header.Get("Location"), bodyOf(b.f.t, res))
	}
	return b.csrf()
}

// csrf loads the session page and returns its CSRF token; it fails the
// test when the session has ended.
func (b *browser) csrf() string {
	b.f.t.Helper()
	res := b.get("/")
	body := bodyOf(b.f.t, res)
	m := csrfRE.FindStringSubmatch(body)
	if res.StatusCode != http.StatusOK || m == nil {
		b.f.t.Fatalf("GET /: %d %q, not signed in:\n%s", res.StatusCode, res.Header.Get("Location"), body)
	}
	return m[1]
}

// signedIn reports whether GET / serves the session page; a session that
// has ended redirects to /login.
func (b *browser) signedIn() bool {
	b.f.t.Helper()
	res := b.get("/")
	body := bodyOf(b.f.t, res)
	switch {
	case res.StatusCode == http.StatusOK && strings.Contains(body, "Signed in"):
		return true
	case res.StatusCode == http.StatusSeeOther && res.Header.Get("Location") == "/login":
		return false
	}
	b.f.t.Fatalf("GET /: unexpected %d %q:\n%s", res.StatusCode, res.Header.Get("Location"), body)
	return false
}

// TestAdminHostAllowlist checks A1: a Host other than 127.0.0.1:<port> and
// localhost:<port>, compared without regard to case, gets 403 before
// authentication, even with a valid session, CSRF token, login token, and
// password; the allowed values are served. On ::1, [::1]:<port> is
// allowed too.
func TestAdminHostAllowlist(t *testing.T) {
	f := newFixture(t, true)
	b := f.browser()
	csrf := b.login()
	// A login token and cookie that would sign in on an allowed Host.
	loginTok := f.browser().loginToken()

	refused := []string{
		"evil.example:41729", "evil.example", "127.0.0.1:41730", "localhost:41730",
		"127.0.0.1", "localhost", "127.0.0.2:41729", "[::1]:41729", "localhost.:41729",
		"127.0.0.1:41729.evil.example", noHost, "0x7f000001:41729",
	}
	for _, host := range refused {
		runs := f.s.argonRuns.Load()
		for _, r := range []req{
			{method: http.MethodGet, path: "/"},
			{method: http.MethodGet, path: "/login"},
			{method: http.MethodGet, path: "/static/admin.css"},
			{method: http.MethodPost, path: "/login", form: url.Values{"login_token": {loginTok}, "password": {testPassword}},
				cookies: map[string]string{LoginCookie: loginTok}},
			{method: http.MethodPost, path: "/logout", form: url.Values{"csrf": {csrf}}},
		} {
			r.host = host
			res := b.do(r)
			body := bodyOf(t, res)
			if res.StatusCode != http.StatusForbidden || !strings.Contains(body, "Host not allowed") {
				t.Errorf("Host %q, %s %s: %d, want 403 Host not allowed:\n%s", host, r.method, r.path, res.StatusCode, body)
			}
			if res.Header.Get("Set-Cookie") != "" {
				t.Errorf("Host %q, %s %s: set a cookie", host, r.method, r.path)
			}
		}
		if f.s.argonRuns.Load() != runs {
			t.Errorf("Host %q: Argon2id ran", host)
		}
	}
	// The session survived the refused logout.
	if !b.signedIn() {
		t.Fatal("session ended by a refused request")
	}

	for _, host := range []string{"127.0.0.1:41729", "localhost:41729", "LOCALHOST:41729", "LocalHost:41729"} {
		res := b.do(req{method: http.MethodGet, path: "/", host: host})
		if body := bodyOf(t, res); res.StatusCode != http.StatusOK || !strings.Contains(body, "Signed in") {
			t.Errorf("Host %q, GET /: %d:\n%s", host, res.StatusCode, body)
		}
		nb := f.browser()
		nb.host = host
		if res := nb.get("/login"); res.StatusCode != http.StatusOK {
			t.Errorf("Host %q, GET /login: %d", host, res.StatusCode)
		}
	}

	// A listener on ::1 also accepts [::1]:<port>.
	s6 := New(Options{Listen: netip.MustParseAddrPort("[::1]:41729"), PasswordPath: f.pwPath, Now: f.clock.Now})
	for host, want := range map[string]int{
		"[::1]:41729": http.StatusOK, "localhost:41729": http.StatusOK, "127.0.0.1:41729": http.StatusOK,
		"[::1]:41730": http.StatusForbidden, "evil.example:41729": http.StatusForbidden,
	} {
		r := httptest.NewRequest(http.MethodGet, "http://"+testHost+"/login", nil)
		r.Host = host
		rec := httptest.NewRecorder()
		s6.ServeHTTP(rec, r)
		if rec.Code != want {
			t.Errorf("::1 listener, Host %q: %d, want %d", host, rec.Code, want)
		}
	}
}

// TestAdminLogin checks the login part of A2: a correct password sets
// cuttle_session with HttpOnly, SameSite=Strict, Path=/, and neither Domain
// nor Max-Age; a wrong password gets 401 with the generic message and no
// session. Each login issues a new session ID.
func TestAdminLogin(t *testing.T) {
	f := newFixture(t, true)
	b := f.browser()

	res := b.tryLogin(testPassword + "x")
	body := bodyOf(t, res)
	if res.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "Invalid password.") {
		t.Fatalf("wrong password: %d:\n%s", res.StatusCode, body)
	}
	for _, c := range res.Cookies() {
		if c.Name == SessionCookie {
			t.Errorf("wrong password set %s", c.Raw)
		}
	}
	if b.signedIn() {
		t.Fatal("signed in after a wrong password")
	}

	tok := b.loginToken()
	res = b.do(req{method: http.MethodPost, path: "/login", form: url.Values{"login_token": {tok}, "password": {testPassword}}})
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/" {
		t.Fatalf("login: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	var raw string
	for _, line := range res.Header.Values("Set-Cookie") {
		if strings.HasPrefix(line, SessionCookie+"=") {
			raw = line
		}
	}
	attrs := map[string]bool{}
	for i, a := range strings.Split(raw, "; ") {
		if i > 0 {
			attrs[strings.SplitN(a, "=", 2)[0]] = true
		}
	}
	if !attrs["HttpOnly"] || !strings.Contains(raw, "; SameSite=Strict") || !strings.Contains(raw, "; Path=/") ||
		attrs["Domain"] || attrs["Max-Age"] || attrs["Expires"] || attrs["Secure"] {
		t.Errorf("session cookie %q", raw)
	}
	id1 := b.cookies[SessionCookie]
	if len(id1) != 43 {
		t.Errorf("session ID %q is not 256 bits in base64url", id1)
	}
	if _, ok := b.cookies[LoginCookie]; ok {
		t.Errorf("login cookie not cleared")
	}
	if !b.signedIn() {
		t.Fatal("not signed in after login")
	}

	// A login from a browser that still has a session issues a new ID and
	// ends the session it replaces.
	tok = f.browser().loginToken()
	res = b.do(req{method: http.MethodPost, path: "/login", form: url.Values{"login_token": {tok}, "password": {testPassword}},
		cookies: map[string]string{SessionCookie: id1, LoginCookie: tok}})
	var id2 string
	for _, c := range res.Cookies() {
		if c.Name == SessionCookie {
			id2 = c.Value
		}
	}
	if res.StatusCode != http.StatusSeeOther || id2 == id1 || id2 == "" {
		t.Errorf("second login: %d, kept the session ID", res.StatusCode)
	}
	old := f.browser()
	old.cookies[SessionCookie] = id1
	if old.signedIn() {
		t.Errorf("the replaced session still works")
	}
}

// TestAdminPasswordHash checks the PHC string of A2 and its verification:
// m=19456,t=2,p=1, a 16-byte salt and a 32-byte key in unpadded base64, a
// new salt each time, the right password verifies and others do not, and
// passwords outside 12–128 bytes are refused.
func TestAdminPasswordHash(t *testing.T) {
	phcRE := regexp.MustCompile(`^\$argon2id\$v=19\$m=19456,t=2,p=1\$[A-Za-z0-9+/]{22}\$[A-Za-z0-9+/]{43}$`)
	a, err := HashPassword([]byte(testPassword))
	if err != nil || !phcRE.MatchString(a) {
		t.Fatalf("HashPassword: %q %v", a, err)
	}
	b, _ := HashPassword([]byte(testPassword))
	if a == b {
		t.Error("two hashes share a salt")
	}
	h, err := parsePHC(a)
	if err != nil {
		t.Fatal(err)
	}
	if !h.verify([]byte(testPassword)) {
		t.Error("the right password does not verify")
	}
	for _, wrong := range []string{testPassword + " ", "Correct horse battery", testPassword[:len(testPassword)-1], ""} {
		if h.verify([]byte(wrong)) {
			t.Errorf("%q verifies", wrong)
		}
	}
	for _, n := range []int{0, 11, 129, 1000} {
		if _, err := HashPassword([]byte(strings.Repeat("x", n))); err == nil {
			t.Errorf("a %d-byte password was accepted", n)
		}
	}
	for _, n := range []int{12, 128} {
		if _, err := HashPassword([]byte(strings.Repeat("x", n))); err != nil {
			t.Errorf("a %d-byte password: %v", n, err)
		}
	}
	// Other parameters, versions, or lengths are refused, so a file cannot
	// raise the memory a verification takes.
	parts := strings.Split(a, "$")
	for _, bad := range []string{
		strings.Replace(a, "m=19456", "m=1048576", 1),
		strings.Replace(a, "t=2", "t=1", 1),
		strings.Replace(a, "argon2id", "argon2i", 1),
		strings.Replace(a, "v=19", "v=16", 1),
		strings.Join(append(parts[:4:4], parts[4][:20], parts[5]), "$"),
		strings.Join(append(parts[:5:5], parts[5]+"AA"), "$"),
		a + "=",
		"",
	} {
		if _, err := parsePHC(bad); err == nil {
			t.Errorf("parsePHC accepted %q", bad)
		}
	}
}

// TestAdminNoPassword checks A3: without a usable password file, /login
// names cuttle admin passwd and shows no form, and POST /login fails
// without running Argon2id, even with a valid login token. A file with
// another mode or invalid content counts as no password.
func TestAdminNoPassword(t *testing.T) {
	f := newFixture(t, false)
	b := f.browser()
	res := b.get("/login")
	body := bodyOf(t, res)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "cuttle admin passwd") || strings.Contains(body, "<form") {
		t.Fatalf("GET /login without a password: %d:\n%s", res.StatusCode, body)
	}
	res = b.do(req{method: http.MethodPost, path: "/login", form: url.Values{"password": {testPassword}}})
	if body := bodyOf(t, res); res.StatusCode != http.StatusForbidden || !strings.Contains(body, "cuttle admin passwd") {
		t.Errorf("POST /login without a password or token: %d:\n%s", res.StatusCode, body)
	}

	// Take a valid login token while a password exists, then make the file
	// unusable in each way and submit the right password.
	f.setPassword(testPassword)
	disable := map[string]func(){
		"missing": func() { os.Remove(f.pwPath) },
		"0644":    func() { os.Chmod(f.pwPath, 0o644) },
		"0400":    func() { os.Chmod(f.pwPath, 0o400) },
		"invalid": func() { os.WriteFile(f.pwPath, []byte("$argon2id$v=19$m=19456,t=2,p=1$AAAA$BBBB\n"), 0o600) },
		"bcrypt": func() {
			os.WriteFile(f.pwPath, []byte("$2b$10$abcdefghijklmnopqrstuuABCDEFGHIJKLMNOPQRSTUVWXYZ01234\n"), 0o600)
		},
		"dir": func() { os.Remove(f.pwPath); os.Mkdir(f.pwPath, 0o700) },
	}
	for name, fn := range disable {
		os.RemoveAll(f.pwPath)
		f.setPassword(testPassword)
		tok := b.loginToken()
		fn()
		if p := PasswordProblem(f.pwPath); p == "" {
			t.Errorf("%s: PasswordProblem reports none", name)
		}
		runs := f.s.argonRuns.Load()
		res := b.do(req{method: http.MethodPost, path: "/login", form: url.Values{"login_token": {tok}, "password": {testPassword}}})
		body := bodyOf(t, res)
		if res.StatusCode != http.StatusForbidden || !strings.Contains(body, "cuttle admin passwd") {
			t.Errorf("%s: POST /login: %d:\n%s", name, res.StatusCode, body)
		}
		if n := f.s.argonRuns.Load() - runs; n != 0 {
			t.Errorf("%s: Argon2id ran %d times", name, n)
		}
		if b.signedIn() {
			t.Errorf("%s: signed in", name)
		}
		if res := b.get("/login"); !strings.Contains(bodyOf(t, res), "cuttle admin passwd") {
			t.Errorf("%s: GET /login does not name the command", name)
		}
	}
	os.RemoveAll(f.pwPath)
	f.setPassword(testPassword)
	if PasswordProblem(f.pwPath) != "" {
		t.Fatal(PasswordProblem(f.pwPath))
	}
	b.login()
}

// TestAdminLoginBackoff checks A4: after 3 consecutive failures an
// immediate attempt gets 429 with Retry-After: 1; each further failure
// doubles the wait up to 300 s; refused attempts run no Argon2id and do
// not extend the wait; a success resets the count.
func TestAdminLoginBackoff(t *testing.T) {
	f := newFixture(t, true)
	b := f.browser()
	// Each attempt loads a new form, since the waits outlast a login token.
	attempt := func(pw string) *http.Response {
		t.Helper()
		delete(b.cookies, SessionCookie)
		return b.tryLogin(pw)
	}
	expect := func(step string, res *http.Response, code int, retry string) {
		t.Helper()
		body := bodyOf(t, res)
		if res.StatusCode != code || res.Header.Get("Retry-After") != retry {
			t.Fatalf("%s: %d Retry-After %q, want %d %q:\n%s", step, res.StatusCode, res.Header.Get("Retry-After"), code, retry, body)
		}
	}
	for i := 1; i <= 3; i++ {
		expect(fmt.Sprintf("failure %d", i), attempt("wrong password!"), http.StatusUnauthorized, "")
	}
	if n := f.s.argonRuns.Load(); n != 3 {
		t.Fatalf("argon runs %d", n)
	}
	// The right password is refused too while the wait lasts.
	expect("right password after 3 failures", attempt(testPassword), http.StatusTooManyRequests, "1")
	f.clock.Add(999 * time.Millisecond)
	expect("after 999 ms", attempt("wrong password!"), http.StatusTooManyRequests, "1")
	if n := f.s.argonRuns.Load(); n != 3 {
		t.Fatalf("refused attempts ran Argon2id: %d", n)
	}
	// The wait counts from the last failure, not from refused attempts.
	f.clock.Add(time.Millisecond)

	// Failures 4 to 13: the wait doubles from 1 s and stops at 300 s.
	waits := []int{2, 4, 8, 16, 32, 64, 128, 256, 300, 300}
	for i, w := range waits {
		n := i + 4
		expect(fmt.Sprintf("failure %d", n), attempt("wrong password!"), http.StatusUnauthorized, "")
		expect(fmt.Sprintf("right password after failure %d", n), attempt(testPassword), http.StatusTooManyRequests, strconv.Itoa(w))
		f.clock.Add(time.Duration(w)*time.Second - 500*time.Millisecond)
		expect("half a second before the end", attempt(testPassword), http.StatusTooManyRequests, "1")
		f.clock.Add(500 * time.Millisecond)
	}
	if n := f.s.argonRuns.Load(); n != 13 {
		t.Fatalf("argon runs %d, want 13", n)
	}

	// Success resets the count: three more failures are needed again.
	expect("success", attempt(testPassword), http.StatusSeeOther, "")
	for i := 0; i < 3; i++ {
		expect("failure after reset", attempt("wrong password!"), http.StatusUnauthorized, "")
	}
	expect("4th attempt after reset", attempt("wrong password!"), http.StatusTooManyRequests, "1")
}

// TestAdminLoginBackoffParallel checks that parallel attempts cannot pass
// the backoff together: with two failures counted, ten simultaneous wrong
// attempts run Argon2id once, and the other nine get 429.
func TestAdminLoginBackoffParallel(t *testing.T) {
	f := newFixture(t, true)
	b := f.browser()
	for range 2 {
		if res := b.tryLogin("wrong password!"); res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("failure: %d", res.StatusCode)
		}
	}
	const n = 10
	toks := make([]string, n)
	for i := range toks {
		toks[i] = b.loginToken()
	}
	runs := f.s.argonRuns.Load()
	codes := make(chan int, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, tok := range toks {
		wg.Go(func() {
			<-start
			res := b.do(req{method: http.MethodPost, path: "/login", form: url.Values{"login_token": {tok}, "password": {"wrong password!"}},
				cookies: map[string]string{LoginCookie: tok}})
			codes <- res.StatusCode
		})
	}
	close(start)
	wg.Wait()
	close(codes)
	count := map[int]int{}
	for c := range codes {
		count[c]++
	}
	if count[http.StatusUnauthorized] != 1 || count[http.StatusTooManyRequests] != n-1 {
		t.Errorf("parallel attempts: %v, want one 401 and %d 429", count, n-1)
	}
	if got := f.s.argonRuns.Load() - runs; got != 1 {
		t.Errorf("parallel attempts ran Argon2id %d times, want 1", got)
	}
}

// alterToken returns tok with one character changed, which always differs
// from tok. It changes a middle character: the last character of a
// base64url token can carry fewer than 6 bits, so replacing it with a fixed
// character sometimes leaves the token unchanged.
func alterToken(tok string) string {
	b := []byte(tok)
	if b[10] == 'A' {
		b[10] = 'B'
	} else {
		b[10] = 'A'
	}
	return string(b)
}

// TestAdminCSRF checks A5: a POST with Sec-Fetch-Site: cross-site or a
// foreign Origin, a POST without or with a wrong CSRF token, and a login
// without a matching, unexpired cuttle_login cookie get 403, and none of
// them changes state.
func TestAdminCSRF(t *testing.T) {
	f := newFixture(t, true)
	b := f.browser()
	csrf := b.login()
	other := f.browser()
	otherCSRF := other.login()

	logout := func(name string, r req) {
		t.Helper()
		r.method, r.path = http.MethodPost, "/logout"
		res := b.do(r)
		if body := bodyOf(t, res); res.StatusCode != http.StatusForbidden {
			t.Errorf("%s: %d, want 403:\n%s", name, res.StatusCode, body)
		}
		if !b.signedIn() {
			t.Fatalf("%s: the session ended", name)
		}
	}
	logout("cross-site with the token", req{form: url.Values{"csrf": {csrf}}, crossSite: true})
	logout("same-site with the token", req{form: url.Values{"csrf": {csrf}}, header: http.Header{"Sec-Fetch-Site": {"same-site"}}})
	logout("foreign Origin with the token", req{form: url.Values{"csrf": {csrf}},
		header: http.Header{"Sec-Fetch-Site": nil, "Origin": {"http://evil.example"}}})
	logout("no token", req{form: url.Values{}})
	logout("no token, no fetch metadata", req{form: url.Values{}, header: http.Header{"Sec-Fetch-Site": nil, "Origin": nil}})
	logout("wrong token", req{form: url.Values{"csrf": {alterToken(csrf)}}})
	logout("truncated token", req{form: url.Values{"csrf": {csrf[:20]}}})
	logout("another session's token", req{form: url.Values{"csrf": {otherCSRF}}})
	res := b.do(req{method: http.MethodPost, path: "/logout?csrf=" + url.QueryEscape(csrf), form: url.Values{}})
	if res.StatusCode != http.StatusForbidden || !b.signedIn() {
		t.Errorf("token in the query: %d", res.StatusCode)
	}

	// Login CSRF: every variant runs no Argon2id and starts no session.
	nb := f.browser()
	tok := nb.loginToken()
	tok2 := f.browser().loginToken()
	login := func(name string, r req) {
		t.Helper()
		r.method, r.path = http.MethodPost, "/login"
		runs := f.s.argonRuns.Load()
		res := nb.do(r)
		body := bodyOf(t, res)
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("%s: %d, want 403:\n%s", name, res.StatusCode, body)
		}
		if f.s.argonRuns.Load() != runs {
			t.Errorf("%s: Argon2id ran", name)
		}
		for _, c := range res.Cookies() {
			if c.Name == SessionCookie && c.MaxAge >= 0 {
				t.Errorf("%s: session cookie set", name)
			}
		}
	}
	// Each case sends its cookies explicitly, so a cookie that an earlier
	// 403 page set cannot decide the outcome.
	pw := url.Values{"password": {testPassword}}
	with := func(tok string) url.Values {
		return url.Values{"password": {testPassword}, "login_token": {tok}}
	}
	cookie := func(tok string) map[string]string { return map[string]string{LoginCookie: tok} }
	login("no cuttle_login cookie", req{form: with(tok), noCookies: true})
	login("no form token", req{form: pw, cookies: cookie(tok)})
	login("cookie and form differ", req{form: with(tok2), cookies: cookie(tok)})
	login("forged token in both", req{form: with("AAAA"), cookies: cookie("AAAA")})
	forged := alterToken(tok)
	login("tampered token in both", req{form: with(forged), cookies: cookie(forged)})
	login("cross-site", req{form: with(tok), cookies: cookie(tok), crossSite: true})
	login("same-site", req{form: with(tok), cookies: cookie(tok), header: http.Header{"Sec-Fetch-Site": {"same-site"}}})
	// Another server's process key: a token this process did not mint.
	foreign := New(Options{Listen: netip.MustParseAddrPort(testListen), PasswordPath: f.pwPath, Now: f.clock.Now}).login.mint(f.clock.Now())
	login("token from another process", req{form: with(foreign), cookies: cookie(foreign)})
	// Positive control: the same pair signs in one second before expiry.
	f.clock.Add(LoginTokenTTL - time.Second)
	if res := nb.do(req{method: http.MethodPost, path: "/login", form: with(tok), cookies: cookie(tok)}); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("valid login token pair: %d", res.StatusCode)
	}
	f.clock.Add(time.Second)
	login("expired token", req{form: with(tok), cookies: cookie(tok)})

	// A fresh token still works, and a proper logout ends the session.
	nb.login()
	res = b.do(req{method: http.MethodPost, path: "/logout", form: url.Values{"csrf": {csrf}}})
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/login" || b.signedIn() {
		t.Errorf("logout: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
}

// TestAdminSessionEnd checks A6: a session ends after 30 idle minutes,
// /live reloads included, after 12 hours in all, when the password file
// changes, and after logout, whose cookie value then no longer works.
func TestAdminSessionEnd(t *testing.T) {
	f := newFixture(t, true)

	t.Run("idle", func(t *testing.T) {
		b := f.browser()
		b.login()
		for i := 0; i < 3; i++ {
			f.clock.Add(IdleTimeout - time.Second)
			if !b.signedIn() {
				t.Fatalf("ended after %d active periods", i)
			}
		}
		f.clock.Add(IdleTimeout)
		if b.signedIn() {
			t.Fatal("alive after 30 idle minutes")
		}
		if b.signedIn() {
			t.Fatal("an ended session came back")
		}
	})

	// The live page reloads itself every 5 s; those reloads are not
	// activity, so a forgotten tab does not keep the session alive.
	t.Run("live refresh", func(t *testing.T) {
		b := f.browser()
		b.login()
		for elapsed := time.Duration(0); elapsed < IdleTimeout-5*time.Second; elapsed += 5 * time.Second {
			f.clock.Add(5 * time.Second)
			if res := b.get("/live"); res.StatusCode != http.StatusOK {
				t.Fatalf("/live after %v: %d", elapsed, res.StatusCode)
			}
		}
		f.clock.Add(5 * time.Second)
		if res := b.get("/live"); res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/login" {
			t.Fatalf("/live after 30 minutes of refreshes: %d %q", res.StatusCode, res.Header.Get("Location"))
		}
		if b.signedIn() {
			t.Fatal("alive after 30 minutes of /live refreshes")
		}
	})

	t.Run("absolute", func(t *testing.T) {
		b := f.browser()
		b.login()
		for elapsed := time.Duration(0); elapsed < AbsoluteTimeout-time.Minute; {
			step := min(25*time.Minute, AbsoluteTimeout-time.Minute-elapsed)
			f.clock.Add(step)
			elapsed += step
			if !b.signedIn() {
				t.Fatalf("ended after %v", elapsed)
			}
		}
		f.clock.Add(time.Minute)
		if b.signedIn() {
			t.Fatal("alive after 12 hours")
		}
	})

	t.Run("password change", func(t *testing.T) {
		b := f.browser()
		b.login()
		other := f.browser()
		other.login()
		f.setPassword("another long password")
		if b.signedIn() || other.signedIn() {
			t.Fatal("alive after the password file changed")
		}
		res := b.tryLogin(testPassword)
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("old password: %d", res.StatusCode)
		}
		res = b.tryLogin("another long password")
		if res.StatusCode != http.StatusSeeOther || !b.signedIn() {
			t.Fatalf("new password: %d", res.StatusCode)
		}
		// Rewriting the same password also ends sessions: the file differs.
		f.setPassword("another long password")
		if b.signedIn() {
			t.Fatal("alive after the password was set again")
		}
		// A file that becomes unusable ends sessions too.
		b.tryLogin("another long password")
		if !b.signedIn() {
			t.Fatal("login failed")
		}
		os.Chmod(f.pwPath, 0o644)
		if b.signedIn() {
			t.Fatal("alive after the password file became 0644")
		}
		os.Chmod(f.pwPath, 0o600)
		if b.signedIn() {
			t.Fatal("an ended session came back")
		}
		f.setPassword(testPassword)
	})

	t.Run("logout", func(t *testing.T) {
		b := f.browser()
		csrf := b.login()
		id := b.cookies[SessionCookie]
		res := b.do(req{method: http.MethodPost, path: "/logout", form: url.Values{"csrf": {csrf}}})
		if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/login" {
			t.Fatalf("logout: %d %q", res.StatusCode, res.Header.Get("Location"))
		}
		if _, ok := b.cookies[SessionCookie]; ok {
			t.Error("logout did not clear the cookie")
		}
		// The server forgot the ID: replaying the old cookie fails.
		b.cookies[SessionCookie] = id
		if b.signedIn() {
			t.Fatal("the old session ID still works")
		}
	})
}

// TestAdminHeaders checks A7: every response, whatever its status, carries
// the D5 security headers, and every page but the embedded styles carries
// Cache-Control: no-store.
func TestAdminHeaders(t *testing.T) {
	f := newFixture(t, true)
	b := f.browser()
	type check struct {
		name string
		r    func() *http.Response
		code int
	}
	var csrf string
	checks := []check{
		{"login page", func() *http.Response { return b.get("/login") }, 200},
		{"unknown Host", func() *http.Response { return b.do(req{method: "GET", path: "/", host: "evil.example:41729"}) }, 403},
		{"not signed in", func() *http.Response { return b.get("/") }, 303},
		{"wrong password", func() *http.Response { return b.tryLogin("wrong password!") }, 401},
		{"login without token", func() *http.Response {
			return b.do(req{method: "POST", path: "/login", form: url.Values{"password": {"x"}}})
		}, 403},
		{"cross-site", func() *http.Response {
			return b.do(req{method: "POST", path: "/login", form: url.Values{}, crossSite: true})
		}, 403},
		{"logout without session", func() *http.Response {
			return b.do(req{method: "POST", path: "/logout", form: url.Values{}})
		}, 403},
		{"login", func() *http.Response { return b.tryLogin(testPassword) }, 303},
		{"session page", func() *http.Response { return b.get("/") }, 200},
		{"login page while signed in", func() *http.Response { return b.get("/login") }, 303},
		{"logout without token", func() *http.Response {
			return b.do(req{method: "POST", path: "/logout", form: url.Values{}})
		}, 403},
		{"oversized form", func() *http.Response {
			return b.do(req{method: "POST", path: "/logout", form: url.Values{"x": {strings.Repeat("a", maxFormBytes)}}})
		}, 413},
		{"unknown path", func() *http.Response { return b.get("/nope") }, 404},
		{"wrong method", func() *http.Response { return b.do(req{method: "PUT", path: "/login"}) }, 405},
		{"styles", func() *http.Response { return b.get("/static/admin.css") }, 200},
		{"static directory", func() *http.Response { return b.get("/static/") }, 404},
		{"missing static file", func() *http.Response { return b.get("/static/missing.css") }, 404},
		{"logout", func() *http.Response {
			csrf = b.csrf()
			return b.do(req{method: "POST", path: "/logout", form: url.Values{"csrf": {csrf}}})
		}, 303},
	}
	// Three failures put the backoff in force for a 429.
	checks = append(checks, check{"backoff", func() *http.Response {
		for range 3 {
			b.tryLogin("wrong password!")
		}
		return b.tryLogin("wrong password!")
	}, 429})
	for _, c := range checks {
		res := c.r()
		bodyOf(t, res)
		if res.StatusCode != c.code {
			t.Errorf("%s: status %d, want %d", c.name, res.StatusCode, c.code)
		}
		for _, kv := range [][2]string{
			{"Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; form-action 'self'; base-uri 'none'"},
			{"X-Frame-Options", "DENY"},
			{"Referrer-Policy", "no-referrer"},
			{"X-Content-Type-Options", "nosniff"},
		} {
			if got := res.Header.Values(kv[0]); len(got) != 1 || got[0] != kv[1] {
				t.Errorf("%s: %s %q", c.name, kv[0], got)
			}
		}
		cc := res.Header.Get("Cache-Control")
		if strings.HasPrefix(c.name, "styles") || strings.HasPrefix(c.name, "static") || strings.HasPrefix(c.name, "missing static") {
			continue
		}
		if cc != "no-store" {
			t.Errorf("%s: Cache-Control %q", c.name, cc)
		}
	}
	if res := b.get("/static/admin.css"); res.Header.Get("Content-Type") != "text/css; charset=utf-8" {
		t.Errorf("styles Content-Type %q", res.Header.Get("Content-Type"))
	}
}

// TestPasswordFileCache checks that the password file is parsed once per
// version: the cache key follows inode, size, and modification time.
func TestPasswordFileCache(t *testing.T) {
	f := newFixture(t, true)
	p := &passwordFile{path: f.pwPath}
	a := p.load()
	if !a.usable() {
		t.Fatal(a.problem)
	}
	if b := p.load(); b.digest != a.digest {
		t.Fatal("digest changed without a change")
	}
	f.setPassword(testPassword)
	if b := p.load(); !b.usable() || b.digest == a.digest {
		t.Fatal("a rewritten file kept the cached digest")
	}
}

// BenchmarkPasswordHash measures one Argon2id verification with the D6
// parameters (design 0005 assumption B2: under 200 ms on the target Macs).
func BenchmarkPasswordHash(b *testing.B) {
	phc, err := HashPassword([]byte(testPassword))
	if err != nil {
		b.Fatal(err)
	}
	h, _ := parsePHC(phc)
	b.ResetTimer()
	for b.Loop() {
		h.verify([]byte(testPassword))
	}
}

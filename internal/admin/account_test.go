package admin

import (
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neutrospec/cuttle/internal/config"
)

const newPassword = "a new and longer passphrase"

// changePassword posts the password form.
func (b *browser) changePassword(csrf, current, next, confirm string) (*http.Response, string) {
	b.f.t.Helper()
	res := b.do(req{method: http.MethodPost, path: "/account/password",
		form: url.Values{"csrf": {csrf}, "current": {current}, "new": {next}, "confirm": {confirm}}})
	return res, bodyOf(b.f.t, res)
}

// TestAdminPasswordChange checks W23 of design 0009 (D12): a short or
// unconfirmed new password gets 422 without hashing; a wrong current
// password gets 401 and counts toward the backoff that login shares, so
// that after two failed logins it starts the wait, during which both the
// form and login answer 429 with Retry-After and run no Argon2id;
// success writes a 0600 Argon2id file, resets the shared count, ends
// every other session, keeps this browser signed in with a new session
// ID and CSRF token, shows the toast once, and logs no password; and
// "Sign out other sessions" ends the others only.
func TestAdminPasswordChange(t *testing.T) {
	f := newFixture(t, true)
	logs := f.withLog()
	b := f.browser()
	csrf := b.login()
	other := f.browser()
	other.login()
	third := f.browser()
	third.login()
	runs := f.s.argonRuns.Load()

	body := bodyOf(t, b.get("/account"))
	checkMarkup(t, "account", body, true, "/account")
	if !strings.Contains(body, `<dd class="num" id="other-sessions">2</dd>`) || !strings.Contains(body, f.pwPath) {
		t.Errorf("account page:\n%s", body)
	}

	// The new password is checked first, without any hash.
	for _, c := range []struct{ current, next, confirm, want string }{
		{testPassword, "short", "short", "The password must be at least 12 bytes; it has 5."},
		{testPassword, strings.Repeat("x", 129), strings.Repeat("x", 129), "The password must be at most 128 bytes; it has 129."},
		{testPassword, newPassword, newPassword + "!", "The passwords do not match."},
		{"", newPassword, newPassword, "Enter the current password."},
	} {
		res, body := b.changePassword(csrf, c.current, c.next, c.confirm)
		if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, `<p class="err">`+c.want+`</p>`) {
			t.Errorf("new %q: %d:\n%s", c.next, res.StatusCode, body)
		}
		checkMarkup(t, "account 422", body, true, "/account")
	}
	if n := f.s.argonRuns.Load(); n != runs {
		t.Errorf("a refused form ran Argon2id: %d → %d", runs, n)
	}

	// Two failed logins and a wrong current password make three failures:
	// the next attempt of either kind waits.
	for i := 0; i < 2; i++ {
		if res := f.browser().tryLogin("wrong password!"); res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("failed login: %d", res.StatusCode)
		}
	}
	res, body := b.changePassword(csrf, "wrong password!", newPassword, newPassword)
	if res.StatusCode != http.StatusUnauthorized || !strings.Contains(body, `<p class="err">Invalid password.</p>`) ||
		strings.Contains(body, "wrong password!") || strings.Contains(body, newPassword) {
		t.Fatalf("wrong current password: %d:\n%s", res.StatusCode, body)
	}
	if f.s.backoff.failures != 3 {
		t.Errorf("shared failure count %d, want 3", f.s.backoff.failures)
	}
	runs = f.s.argonRuns.Load()
	res, body = b.changePassword(csrf, testPassword, newPassword, newPassword)
	if res.StatusCode != http.StatusTooManyRequests || res.Header.Get("Retry-After") != "1" || !strings.Contains(body, "Too many failed attempts. Try again in 1 s.") {
		t.Errorf("during the wait: %d %q:\n%s", res.StatusCode, res.Header.Get("Retry-After"), body)
	}
	if res := f.browser().tryLogin(testPassword); res.StatusCode != http.StatusTooManyRequests {
		t.Errorf("login during the wait: %d", res.StatusCode)
	}
	if n := f.s.argonRuns.Load(); n != runs {
		t.Errorf("attempts during the wait ran Argon2id: %d → %d", runs, n)
	}
	if string(readFile(t, f.pwPath)) == "" || !b.signedIn() {
		t.Fatal("refused attempts changed the file or ended the session")
	}

	// Success.
	f.clock.Add(time.Second)
	oldID := b.cookies[SessionCookie]
	res, body = b.changePassword(csrf, testPassword, newPassword, newPassword)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/account" {
		t.Fatalf("change: %d %q:\n%s", res.StatusCode, res.Header.Get("Location"), body)
	}
	fi, err := os.Stat(f.pwPath)
	if err != nil || fi.Mode().Perm() != 0o600 || !strings.HasPrefix(readFile(t, f.pwPath), "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("password file: %v %v %q", err, fi.Mode(), readFile(t, f.pwPath))
	}
	if f.s.backoff.failures != 0 {
		t.Errorf("the shared failure count stayed %d", f.s.backoff.failures)
	}
	if b.cookies[SessionCookie] == oldID || b.cookies[SessionCookie] == "" {
		t.Errorf("the session ID did not change")
	}
	body = bodyOf(t, b.get("/account"))
	newCSRF := csrfRE.FindStringSubmatch(body)
	if newCSRF == nil || newCSRF[1] == csrf || !strings.Contains(body, "Password changed. Other sessions were signed out.") ||
		!strings.Contains(body, `<dd class="num" id="other-sessions">0</dd>`) {
		t.Errorf("after the change:\n%s", body)
	}
	if strings.Contains(bodyOf(t, b.get("/account")), "Password changed.") {
		t.Error("the toast showed twice")
	}
	if other.signedIn() || third.signedIn() {
		t.Error("another session survived the change")
	}
	// The old CSRF token is dead with its session.
	if res, _ := b.changePassword(csrf, newPassword, testPassword, testPassword); res.StatusCode != http.StatusForbidden {
		t.Errorf("the old CSRF token: %d", res.StatusCode)
	}
	if res := f.browser().tryLogin(testPassword); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("login with the old password: %d", res.StatusCode)
	}
	other.login2(newPassword)
	third.login2(newPassword)
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, testPassword) || strings.Contains(l, newPassword) || strings.Contains(l, "wrong password!") {
			t.Errorf("log line holds a password: %s", l)
		}
	}
	for _, want := range []string{"admin password changed", "admin password change refused; wrong current password"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("no log line %q:\n%s", want, logs.String())
		}
	}

	// Sign out other sessions ends the others only.
	csrf = newCSRF[1]
	res = b.do(req{method: http.MethodPost, path: "/account/sessions", form: url.Values{"csrf": {csrf}}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign out others: %d", res.StatusCode)
	}
	if body := bodyOf(t, b.get("/account")); !strings.Contains(body, "Other sessions signed out.") || !strings.Contains(body, "Ended 2 sessions.") {
		t.Errorf("sign out others toast:\n%s", body)
	}
	if !b.signedIn() || other.signedIn() || third.signedIn() {
		t.Error("Sign out other sessions: wrong sessions ended")
	}
}

// login2 signs in with pw.
func (b *browser) login2(pw string) {
	b.f.t.Helper()
	if res := b.tryLogin(pw); res.StatusCode != http.StatusSeeOther {
		b.f.t.Fatalf("login with %d bytes: %d", len(pw), res.StatusCode)
	}
}

// TestPasswordWriteNotSynced checks OI-0006 Z-R4: when the directory sync
// after the rename fails, the password file holds the new hash with mode
// 0600 and WritePasswordFile says so with config.ErrNotSynced; the
// password change then reports success with the warning, and the browser
// stays signed in. A directory with mode 0300 allows the rename but not
// the open of the directory.
func TestPasswordWriteNotSynced(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens any directory")
	}
	f := newFixture(t, true)
	b := f.browser()
	csrf := b.login()
	dir := filepath.Dir(f.pwPath)
	if err := os.Chmod(dir, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	phc, err := HashPassword([]byte(newPassword))
	if err != nil {
		t.Fatal(err)
	}
	err = WritePasswordFile(f.pwPath, phc)
	if !errors.Is(err, config.ErrNotSynced) {
		t.Fatalf("WritePasswordFile: %v, want ErrNotSynced", err)
	}
	if fi, err := os.Stat(f.pwPath); err != nil || fi.Mode().Perm() != 0o600 || readFile(t, f.pwPath) != phc+"\n" {
		t.Fatalf("file after an unsynced write: %v %v", err, fi)
	}
	// The write ended the session; sign in with the new password and
	// change it back through the form.
	b.login2(newPassword)
	csrf = b.csrf()
	res, body := b.changePassword(csrf, newPassword, testPassword, testPassword)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("change with an unsynced directory: %d:\n%s", res.StatusCode, body)
	}
	body = bodyOf(t, b.get("/account"))
	if !strings.Contains(body, `class="toast warn"`) || !strings.Contains(body, "its directory could not be synced") {
		t.Errorf("no sync warning:\n%s", body)
	}
	if res := f.browser().tryLogin(testPassword); res.StatusCode != http.StatusSeeOther {
		t.Errorf("the changed password does not sign in: %d", res.StatusCode)
	}
}

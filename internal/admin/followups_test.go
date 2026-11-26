package admin

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestAdminPasswordChangeUnderLock checks OI-0006 AB-R6: the password
// change reads the password file under loginMu, so a change that waited
// for the lock while another change replaced the password checks its
// current password against the new hash and is refused, instead of
// overwriting the other change.
func TestAdminPasswordChangeUnderLock(t *testing.T) {
	f := newFixture(t, true)
	b := f.browser()
	csrf := b.login()
	other, err := HashPassword([]byte("another long passphrase"))
	if err != nil {
		t.Fatal(err)
	}

	f.s.loginMu.Lock()
	done := make(chan int, 1)
	go func() {
		res := b.do(req{method: http.MethodPost, path: "/account/password",
			form: url.Values{"csrf": {csrf}, "current": {testPassword}, "new": {newPassword}, "confirm": {newPassword}}})
		res.Body.Close()
		done <- res.StatusCode
	}()
	// Let the request reach the lock, then let another change finish.
	time.Sleep(300 * time.Millisecond)
	if err := WritePasswordFile(f.pwPath, other); err != nil {
		f.s.loginMu.Unlock()
		t.Fatal(err)
	}
	f.s.loginMu.Unlock()
	if code := <-done; code != http.StatusUnauthorized {
		t.Errorf("the waiting change answered %d, want 401 against the new hash", code)
	}
	if got := strings.TrimSpace(readFile(t, f.pwPath)); got != other {
		t.Errorf("the waiting change replaced the other one: %q", got)
	}
}

// TestAdminSessionsEndedCount checks OI-0006 AB-R7: "Sign out other
// sessions" counts the live sessions it ended, as the page counts them,
// not the expired ones it also removes.
func TestAdminSessionsEndedCount(t *testing.T) {
	f := newFixture(t, true)
	b := f.browser()
	b.login()
	other := f.browser()
	other.login()
	idle := f.browser()
	idle.login()
	// The idle session passes its idle timeout; the other two stay live.
	f.clock.Add(20 * time.Minute)
	b.get("/account")
	other.get("/account")
	f.clock.Add(15 * time.Minute)
	body := bodyOf(t, b.get("/account"))
	if !strings.Contains(body, `<dd class="num" id="other-sessions">1</dd>`) {
		t.Fatalf("the page counts other than 1 live session:\n%s", body)
	}
	res := b.do(req{method: http.MethodPost, path: "/account/sessions", form: url.Values{"csrf": {b.csrf()}}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign out others: %d", res.StatusCode)
	}
	if body := bodyOf(t, b.get("/account")); !strings.Contains(body, "Ended 1 session.") {
		t.Errorf("toast after ending one live and one expired session:\n%s", body)
	}
	if other.signedIn() || !b.signedIn() {
		t.Error("wrong sessions ended")
	}
	if n := len(f.s.sessions.m); n != 1 {
		t.Errorf("%d sessions kept, want this one", n)
	}
}

// TestAdminInterfaceAddTitle checks OI-0006 AB-R8: a manual Add with
// both the hardware address and the device typed keeps the title "Add
// interface" when the form renders again, and a form that Define…
// prefilled keeps "Define <device>".
func TestAdminInterfaceAddTitle(t *testing.T) {
	p := newPipeline(t)
	p.f.withLinks()
	form := with(p.pageForm("/interfaces/new", editorStart), "name", "eth-z", "ident", "both",
		"mac", "00:00:5e:00:53:77", "device", "en5")
	res, body := p.post("/config/draft", with(form, "intent", "edit"))
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "<h1>Add interface</h1>") || strings.Contains(body, "Define en5") {
		t.Errorf("manual Add after a re-render: %d:\n%s", res.StatusCode, body)
	}
	form = with(p.pageForm("/interfaces/new?device=en8", editorStart), "name", "eth-y")
	res, body = p.post("/config/draft", with(form, "intent", "edit"))
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "<h1>Define en8</h1>") {
		t.Errorf("Define… after a re-render: %d:\n%s", res.StatusCode, body)
	}
}

// iflHintRE finds the interface list editor's body and the hint at its
// end, which hides with the body while the choice leaves the key unset.
var iflHintRE = regexp.MustCompile(`(?s)<div class="ifl-body">\n<ol class="iflist".*?</ol>\n<div class="iflist-add"[^\n]*</div>\n<div class="hint ifl-hint">([^<]*)</div>\n</div>`)

// TestAdminInterfaceListHint checks OI-0006 AC-R1: the interface list
// editor's hint sits inside the part that hides while "Use the default
// list" or "System routing" is chosen, and in the upstream editor it
// speaks of connections to the upstream.
func TestAdminInterfaceListHint(t *testing.T) {
	p := newPipeline(t)
	for _, c := range []struct{ path, want string }{
		{"/rules/restricted", "Requests use only these interfaces, in this order; the first one that is online carries the connection."},
		{"/upstreams/gateway", "Connections to the upstream use only these interfaces, in this order; the first one that is online carries the connection."},
		{"/proxy", "Requests use only these interfaces, in this order; the first one that is online carries the connection."},
	} {
		body := p.get(c.path)
		m := iflHintRE.FindAllStringSubmatch(body, -1)
		if len(m) != 1 || m[0][1] != c.want {
			t.Errorf("%s: hint %q, want one %q", c.path, m, c.want)
		}
		if n := strings.Count(body, "only these interfaces, in this order;"); n != 1 {
			t.Errorf("%s: the hint shows %d times", c.path, n)
		}
	}
}

// TestAdminDuplicateDiscard checks OI-0006 AC-R2: Discard on a duplicated
// rule loads the copy again, also after the form rendered again, and a
// forged source name is not used.
func TestAdminDuplicateDiscard(t *testing.T) {
	p := newPipeline(t)
	const want = `data-guard-discard="/rules/new?from=restricted"`
	body := p.get("/rules/new?from=restricted")
	if !strings.Contains(body, want) || !strings.Contains(body, "A copy of rule <code>restricted</code>.") {
		t.Fatalf("duplicate:\n%s", body)
	}
	form := with(formByID(t, body, "rule-form"), "note", "edited")
	res, body := p.post("/config/draft", with(form, "intent", "edit"))
	if res.StatusCode != http.StatusOK || !strings.Contains(body, want) {
		t.Errorf("after a re-render: %d:\n%s", res.StatusCode, body)
	}
	res, body = p.post("/config/draft", with(form, "intent", "edit", "r:from", "javascript:alert(1)"))
	if res.StatusCode != http.StatusOK || strings.Contains(body, "from=javascript") || !strings.Contains(body, `data-guard-discard="/rules/new"`) {
		t.Errorf("a forged source: %d:\n%s", res.StatusCode, body)
	}
	if body := p.get("/rules/new"); !strings.Contains(body, `data-guard-discard="/rules/new"`) {
		t.Errorf("a new rule's Discard:\n%s", body)
	}
}

// TestAdminAccountErrorSummary checks OI-0006 AB-R2 and design 0009
// D3.13: refused Account fields show an error summary at the top that
// links to each field and takes the focus; a form op's summary takes it
// on a full page and not in a fragment, which the script inserts while
// the operator types.
func TestAdminAccountErrorSummary(t *testing.T) {
	p := newPipeline(t)
	res, body := p.b.changePassword(p.csrf, "", "short", "short")
	summary := `<div class="error-summary" role="alert" tabindex="-1" autofocus><h2>2 errors</h2><ul>
<li><a href="#pw-current">Current password: Enter the current password.</a></li>
<li><a href="#pw-new">New password: The password must be at least 12 bytes; it has 5.</a></li>
</ul></div>`
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, summary) ||
		strings.Index(body, summary) > strings.Index(body, `class="account-form"`) {
		t.Errorf("account errors: %d:\n%s", res.StatusCode, body)
	}
	for _, id := range []string{"pw-current", "pw-new"} {
		if !strings.Contains(body, `id="`+id+`"`) {
			t.Errorf("no control %s for the summary's link", id)
		}
	}
	if body := p.get("/account"); strings.Contains(body, "error-summary") {
		t.Error("the page shows an error summary without errors")
	}

	form := with(p.ruleForm("/rules/new"), "name", "", "domains", "a.example")
	_, body = p.post("/config/draft", with(form, "intent", "review"))
	if !strings.Contains(body, `<div class="error-summary" role="alert" tabindex="-1" autofocus>`) {
		t.Errorf("a form's summary on a full page does not take the focus:\n%s", body)
	}
	_, body = p.fragment("/config/draft", with(form, "intent", "review"))
	if !strings.Contains(body, `<div class="error-summary" role="alert" tabindex="-1">`) {
		t.Errorf("a form's summary in a fragment:\n%s", body)
	}
}

package admin

import (
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/neutrospec/cuttle/internal/config"
)

// The Account page of design 0009 D3.9 and the password change of D12
// (task D): the current password, the new one and its confirmation, the
// sessions, and the password file.

// accountView is the data of the Account page.
type accountView struct {
	// SignedIn is when this browser's session started; Others counts the
	// other live sessions.
	SignedIn string
	Others   int
	// PasswordPath is the password file and Changed when it last changed.
	PasswordPath, Changed string
	// The password form's issues: one per field, and Message for the form
	// as a whole, such as a backoff wait or a write that failed.
	CurrentErr, NewErr, ConfirmErr string
	Message                        string
}

// Invalid reports whether the password form has an issue.
func (v *accountView) Invalid() bool {
	return v.CurrentErr != "" || v.NewErr != "" || v.ConfirmErr != "" || v.Message != ""
}

// others counts the live sessions other than id.
func (s *sessions) others(id string, now time.Time, pw passwordState) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k, v := range s.m {
		if k != id && !v.expired(now) && pw.usable() && v.digest == pw.digest {
			n++
		}
	}
	return n
}

// removeOthers ends every session but id and reports how many it ended.
func (s *sessions) removeOthers(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k := range s.m {
		if k != id {
			delete(s.m, k)
			n++
		}
	}
	return n
}

// removeAll ends every session.
func (s *sessions) removeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.m)
}

// renderAccount renders the Account page with the session and password
// file state.
func (s *Server) renderAccount(w http.ResponseWriter, code int, id string, v session, av *accountView) {
	now := s.now()
	av.SignedIn = s.when(v.created, now)
	av.Others = s.sessions.others(id, now, s.pw.load())
	av.PasswordPath = s.pw.path
	if fi, err := os.Stat(s.pw.path); err == nil {
		av.Changed = s.when(fi.ModTime(), now)
	}
	s.render(w, code, "account", pageData{Title: "Account", Token: v.csrf, Page: "account", Data: av})
}

func (s *Server) getAccount(w http.ResponseWriter, r *http.Request, id string, v session) {
	s.renderAccount(w, http.StatusOK, id, v, &accountView{})
}

// sentence makes an error message a sentence: a capital first letter and
// a full stop.
func sentence(msg string) string {
	if msg == "" {
		return ""
	}
	msg = strings.ToUpper(msg[:1]) + msg[1:]
	if !strings.HasSuffix(msg, ".") {
		msg += "."
	}
	return msg
}

// postPassword serves POST /account/password (design 0009 D12). A new
// password that is not 12–128 bytes or differs from its confirmation
// gets 422 before any hash. The rest shares loginMu, the backoff, and the
// failure count with the login: during a wait it answers 429 with
// Retry-After and runs no Argon2id; a wrong current password answers 401
// and counts as a failed login. A success writes the new hash, resets the
// count, ends every session, and starts a new one for this browser with a
// new ID and CSRF token. No password reaches the log or the page.
func (s *Server) postPassword(w http.ResponseWriter, r *http.Request, id string, v session) {
	pf := r.PostForm
	current, next, confirm := pf.Get("current"), pf.Get("new"), pf.Get("confirm")
	av := &accountView{}
	if current == "" {
		av.CurrentErr = "Enter the current password."
	}
	if err := CheckPasswordLength(len(next)); err != nil {
		av.NewErr = sentence(err.Error())
	} else if confirm != next {
		av.ConfirmErr = "The passwords do not match."
	}
	if av.Invalid() {
		s.renderAccount(w, http.StatusUnprocessableEntity, id, v, av)
		return
	}
	pw := s.pw.load()
	if !pw.usable() {
		// The session ended with the file; currentSession refuses such a
		// request before it gets here.
		http.Error(w, "403 Forbidden: login is disabled", http.StatusForbidden)
		return
	}

	s.loginMu.Lock()
	if wait := s.backoff.wait(s.now()); wait > 0 {
		s.loginMu.Unlock()
		secs := int((wait + time.Second - 1) / time.Second)
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		av.Message = "Too many failed attempts. Try again in " + strconv.Itoa(secs) + " s."
		s.renderAccount(w, http.StatusTooManyRequests, id, v, av)
		return
	}
	s.argonRuns.Add(1)
	if !pw.hash.verify([]byte(current)) {
		failures := s.backoff.fail(s.now())
		s.loginMu.Unlock()
		s.log.Warn("admin password change refused; wrong current password", "consecutive_failures", failures)
		av.CurrentErr = "Invalid password."
		s.renderAccount(w, http.StatusUnauthorized, id, v, av)
		return
	}
	// Hash under loginMu too, so at most one Argon2id computation runs at a
	// time (design 0005 D6).
	phc, err := HashPassword([]byte(next))
	if err == nil {
		err = WritePasswordFile(s.pw.path, phc)
	}
	unsynced := errors.Is(err, config.ErrNotSynced)
	if err != nil && !unsynced {
		s.loginMu.Unlock()
		s.log.Error("admin password change failed; the password file could not be written", "error", err.Error())
		av.Message = "The password file could not be written, so the password is unchanged: " + err.Error()
		s.renderAccount(w, http.StatusInternalServerError, id, v, av)
		return
	}
	s.backoff.reset()
	s.loginMu.Unlock()

	// The new digest ends every session (design 0005 D7); this browser
	// continues in a new one.
	s.sessions.removeAll()
	nid, nv := s.sessions.create(s.now(), s.pw.load().digest)
	setCookie(w, SessionCookie, nid, 0)
	t := toast{Kind: "ok", Title: "Password changed. Other sessions were signed out."}
	if unsynced {
		s.log.Warn("admin password changed, but the directory could not be synced", "error", err.Error())
		t.Kind = "warn"
		t.Lines = [][]seg{plain("The file holds the new password, but its directory could not be synced, so a power loss soon after could undo the change.")}
	} else {
		s.log.Info("admin password changed")
	}
	s.sessions.setFlash(nv.csrf, t)
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}

// postSessions serves POST /account/sessions: it ends every session but
// this one (design 0009 D12).
func (s *Server) postSessions(w http.ResponseWriter, r *http.Request, id string, v session) {
	n := s.sessions.removeOthers(id)
	s.log.Info("admin sessions ended", "count", n)
	t := toast{Kind: "ok", Title: "Other sessions signed out."}
	if n == 0 {
		t.Title = "No other session was signed in."
	} else {
		t.Lines = [][]seg{plain("Ended " + plural(n, "session") + ".")}
	}
	s.sessions.setFlash(v.csrf, t)
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}

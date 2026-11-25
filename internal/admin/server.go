// Package admin serves the password-protected admin UI on a loopback
// listener (design 0005 D5–D9): the Host allowlist, the security headers,
// the Argon2id password file, in-memory sessions, CSRF protection, and the
// login backoff.
package admin

import (
	"bytes"
	"embed"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Cookie names (design 0005 D7, D8).
const (
	SessionCookie = "cuttle_session"
	LoginCookie   = "cuttle_login"
)

// maxFormBytes bounds the body of the login and logout forms.
const maxFormBytes = 16 << 10

// securityHeaders go on every response (design 0005 D5).
var securityHeaders = [][2]string{
	{"Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; form-action 'self'; base-uri 'none'"},
	{"X-Frame-Options", "DENY"},
	{"Referrer-Policy", "no-referrer"},
	{"X-Content-Type-Options", "nosniff"},
}

//go:embed templates static
var assets embed.FS

// Options configure a Server.
type Options struct {
	// Listen is the address browsers reach the UI at; its port and, for
	// ::1, its address form the Host allowlist (design 0005 D5).
	Listen netip.AddrPort
	// PasswordPath is the admin password file (design 0006 D1).
	PasswordPath string
	Logger       *slog.Logger
	// Now replaces the clock in tests; nil means time.Now.
	Now func() time.Time
}

// Server is the admin UI.
type Server struct {
	log   *slog.Logger
	now   func() time.Time
	hosts []string
	pw    *passwordFile
	login *loginTokens
	pages map[string]*template.Template

	sessions sessions

	// loginMu serializes the backoff check, the verification, and the
	// failure count, so at most one Argon2id computation runs at a time
	// and parallel attempts cannot pass the backoff together (design 0005
	// D6, D8).
	loginMu sync.Mutex
	backoff backoff
	// argonRuns counts Argon2id verifications, for tests.
	argonRuns atomic.Int64

	handler http.Handler
}

// New returns a Server for the options.
func New(opt Options) *Server {
	s := &Server{
		log:   opt.Logger,
		now:   opt.Now,
		pw:    &passwordFile{path: opt.PasswordPath},
		login: newLoginTokens(),
		pages: map[string]*template.Template{},
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.now == nil {
		s.now = time.Now
	}
	port := strconv.Itoa(int(opt.Listen.Port()))
	s.hosts = []string{"127.0.0.1:" + port, "localhost:" + port}
	if own := strings.ToLower(opt.Listen.String()); !slices.Contains(s.hosts, own) {
		s.hosts = append(s.hosts, own)
	}
	base := template.Must(template.ParseFS(assets, "templates/layout.html"))
	for _, name := range []string{"login", "home"} {
		s.pages[name] = template.Must(template.Must(base.Clone()).ParseFS(assets, "templates/"+name+".html"))
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", s.getLogin)
	mux.HandleFunc("POST /login", s.postLogin)
	mux.HandleFunc("POST /logout", s.authed(s.postLogout))
	mux.HandleFunc("GET /{$}", s.authed(s.getHome))
	static, _ := fs.Sub(assets, "static")
	files := http.StripPrefix("/static/", http.FileServerFS(static))
	mux.HandleFunc("GET /static/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r) // no directory listings
			return
		}
		files.ServeHTTP(w, r)
	})
	cop := http.NewCrossOriginProtection()
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "403 Forbidden: cross-origin request", http.StatusForbidden)
	}))
	s.handler = s.headers(s.hostCheck(cop.Handler(mux)))
	return s
}

// ServeHTTP serves the admin UI.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

// Serve serves the UI on l until the returned server is closed. The
// timeouts follow design 0005 D5.
func (s *Server) Serve(l net.Listener) *http.Server {
	srv := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
	go srv.Serve(l)
	return srv
}

// headers sets the D5 security headers on every response, and
// Cache-Control: no-store on everything but the embedded styles.
func (s *Server) headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		for _, kv := range securityHeaders {
			h.Set(kv[0], kv[1])
		}
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// hostCheck answers 403 to any Host but 127.0.0.1:<port>, localhost:<port>,
// or the listen address itself, before anything else, which defeats DNS
// rebinding (design 0005 D5). The comparison ignores case.
func (s *Server) hostCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !slices.Contains(s.hosts, strings.ToLower(r.Host)) {
			http.Error(w, "403 Forbidden: Host not allowed", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// pageData is what the templates render.
type pageData struct {
	Title    string
	Message  string
	Disabled bool
	// Token is the login token on the login page and the CSRF token on
	// session pages.
	Token string
}

func (s *Server) render(w http.ResponseWriter, code int, page string, d pageData) {
	var buf bytes.Buffer
	if err := s.pages[page].ExecuteTemplate(&buf, "layout", d); err != nil {
		s.log.Error("admin page", "page", page, "error", err.Error())
		http.Error(w, "500 Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	w.Write(buf.Bytes())
}

// currentSession returns the live session the request's cookies name.
// Every cuttle_session cookie is tried, so a cookie that another server on
// 127.0.0.1 set for the same name cannot hide the real one.
func (s *Server) currentSession(r *http.Request) (string, session, bool) {
	cookies := r.CookiesNamed(SessionCookie)
	if len(cookies) == 0 {
		return "", session{}, false
	}
	pw := s.pw.load()
	now := s.now()
	for _, c := range cookies {
		if v, ok := s.sessions.get(c.Value, now, pw); ok {
			return c.Value, v, true
		}
	}
	return "", session{}, false
}

// authed serves h only within a live session. A GET without one is sent to
// /login; any other method gets 403. A POST must carry the session's CSRF
// token (design 0005 D8).
func (s *Server) authed(h func(http.ResponseWriter, *http.Request, string, session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, v, ok := s.currentSession(r)
		if !ok {
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
			http.Error(w, "403 Forbidden: not signed in", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost {
			if !parseForm(w, r) {
				return
			}
			if !tokenEqual(r.PostForm.Get("csrf"), v.csrf) {
				http.Error(w, "403 Forbidden: missing or invalid CSRF token", http.StatusForbidden)
				return
			}
		}
		h(w, r, id, v)
	}
}

// parseForm reads a URL-encoded body of at most maxFormBytes.
func parseForm(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "400 Bad Request: invalid form", http.StatusBadRequest)
		return false
	}
	return true
}

func setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
}

// getLogin shows the login form and sets the cuttle_login cookie, or
// names cuttle admin passwd while login is disabled (design 0005 D6, D8).
func (s *Server) getLogin(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.currentSession(r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if pw := s.pw.load(); !pw.usable() {
		s.render(w, http.StatusOK, "login", pageData{Title: "Sign in", Disabled: true})
		return
	}
	s.loginForm(w, http.StatusOK, "")
}

// loginForm renders the form with a new login token and sets its cookie.
func (s *Server) loginForm(w http.ResponseWriter, code int, msg string) {
	tok := s.login.mint(s.now())
	setCookie(w, LoginCookie, tok, int(LoginTokenTTL/time.Second))
	s.render(w, code, "login", pageData{Title: "Sign in", Message: msg, Token: tok})
}

// loginTokenOK reports whether the form's login token is valid and equals
// a cuttle_login cookie.
func (s *Server) loginTokenOK(r *http.Request) bool {
	tok := r.PostForm.Get("login_token")
	if !s.login.valid(tok, s.now()) {
		return false
	}
	for _, c := range r.CookiesNamed(LoginCookie) {
		if tokenEqual(c.Value, tok) {
			return true
		}
	}
	return false
}

// postLogin verifies the password and starts a session (design 0005 D7,
// D8). A request runs Argon2id only with a valid login token, a usable
// password file, and no backoff wait.
func (s *Server) postLogin(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) {
		return
	}
	tokenOK := s.loginTokenOK(r)
	pw := s.pw.load()
	switch {
	case !pw.usable():
		s.render(w, http.StatusForbidden, "login", pageData{Title: "Sign in", Disabled: true})
		return
	case !tokenOK:
		s.loginForm(w, http.StatusForbidden, "The sign-in form expired or is invalid. Try again.")
		return
	}
	tok := r.PostForm.Get("login_token")
	password := []byte(r.PostForm.Get("password"))

	s.loginMu.Lock()
	if wait := s.backoff.wait(s.now()); wait > 0 {
		s.loginMu.Unlock()
		secs := int((wait + time.Second - 1) / time.Second)
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		s.render(w, http.StatusTooManyRequests, "login", pageData{Title: "Sign in", Token: tok,
			Message: "Too many failed attempts. Try again in " + strconv.Itoa(secs) + " s."})
		return
	}
	s.argonRuns.Add(1)
	ok := pw.hash.verify(password)
	var failures int
	if ok {
		s.backoff.reset()
	} else {
		failures = s.backoff.fail(s.now())
	}
	s.loginMu.Unlock()

	if !ok {
		s.log.Warn("admin login failed", "consecutive_failures", failures)
		s.render(w, http.StatusUnauthorized, "login", pageData{Title: "Sign in", Token: tok, Message: "Invalid password."})
		return
	}
	// Login always issues a new ID; a session the browser still presents
	// ends.
	for _, c := range r.CookiesNamed(SessionCookie) {
		s.sessions.remove(c.Value)
	}
	id, _ := s.sessions.create(s.now(), pw.digest)
	setCookie(w, SessionCookie, id, 0)
	setCookie(w, LoginCookie, "", -1)
	s.log.Info("admin login")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// postLogout ends the session (design 0005 D7).
func (s *Server) postLogout(w http.ResponseWriter, r *http.Request, id string, _ session) {
	s.sessions.remove(id)
	setCookie(w, SessionCookie, "", -1)
	s.log.Info("admin logout")
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// getHome is the placeholder page that the M2.3 dashboard replaces.
func (s *Server) getHome(w http.ResponseWriter, r *http.Request, _ string, v session) {
	s.render(w, http.StatusOK, "home", pageData{Title: "Overview", Token: v.csrf})
}

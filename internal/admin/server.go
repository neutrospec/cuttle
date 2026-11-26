// Package admin serves the password-protected admin UI on a loopback
// listener (design 0005 D5–D10): the Host allowlist, the security headers,
// the Argon2id password file, in-memory sessions, CSRF protection, the
// login backoff, the dashboard, and the settings page, in the shell and
// visual system of design 0009 D2, D10, and D11.
package admin

import (
	"bytes"
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/neutrospec/cuttle/internal/config"
	"github.com/neutrospec/cuttle/internal/control"
	"github.com/neutrospec/cuttle/internal/iface"
	"github.com/neutrospec/cuttle/internal/stats"
)

// Cookie names (design 0005 D7, D8).
const (
	SessionCookie = "cuttle_session"
	LoginCookie   = "cuttle_login"
)

// maxFormBytes bounds the body of the login and logout forms.
const maxFormBytes = 16 << 10

// maxSettingsFormBytes bounds the body of the settings forms: a
// configuration of config.MaxFileSize bytes, each of which URL encoding
// may triple, and the other fields. The text itself is limited to
// config.MaxFileSize after decoding (design 0005 D10).
const maxSettingsFormBytes = 3*config.MaxFileSize + 16<<10

// Backend is the running instance that the dashboard shows and the
// settings page changes (design 0005 D9, D10).
type Backend interface {
	// Status returns the instance's state as cuttle status reports it
	// (design 0006 D4).
	Status() control.Status
	// Config returns the running configuration.
	Config() *config.Config
	// StartConfig returns the configuration the instance started with,
	// whose restart-only keys are in force until the next start; the
	// review lists the keys a candidate changes from it (design 0009 D7).
	StartConfig() *config.Config
	// ConfigPath returns the configuration file the instance reloads.
	ConfigPath() string
	// Stats returns the statistics service, or nil while statistics are
	// unavailable (design 0005 D4).
	Stats() *stats.Stats
	// Reload applies the configuration file as SIGHUP does (design 0004
	// D6); ok is false when the file was refused.
	Reload() (res control.ReloadResult, ok bool)
	// Interfaces reads the system interface table through the instance's
	// iface.Source, as cuttle interfaces does; it never runs another
	// program (design 0009 D3.6, 0008 D15).
	Interfaces() ([]iface.Link, error)
}

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
	// Backend is the running instance the pages show and change; it is
	// required.
	Backend Backend
	Logger  *slog.Logger
	// Now replaces the clock in tests; nil means time.Now.
	Now func() time.Time
	// Location is the time zone of dates and times on the pages; nil
	// means time.Local.
	Location *time.Location
}

// Server is the admin UI.
type Server struct {
	log   *slog.Logger
	now   func() time.Time
	loc   *time.Location
	hosts []string
	pw    *passwordFile
	login *loginTokens
	// base holds the layout and the shared blocks; each page is a clone
	// of it with the page's own templates.
	base    *template.Template
	pages   map[string]*template.Template
	backend Backend

	sessions sessions

	// settingsMu serializes settings saves, from the write through the
	// reload (design 0005 D10).
	settingsMu sync.Mutex

	// loginMu serializes the backoff check, the verification, and the
	// failure count, so at most one Argon2id computation runs at a time
	// and parallel attempts cannot pass the backoff together (design 0005
	// D6, D8).
	loginMu sync.Mutex
	backoff backoff
	// argonRuns counts Argon2id verifications, for tests.
	argonRuns atomic.Int64

	// ops are the form ops of design 0009 D7 that the pages register.
	ops map[string]*formOp
	// validations counts the validator runs on candidates, for tests.
	validations atomic.Int64
	// uid is this user's ID, which the previous version's owner must
	// have; tests replace it.
	uid func() int
	// routes lists the patterns of the mux, so that a test can check
	// that every POST is protected.
	routes []string

	handler http.Handler
}

// New returns a Server for the options.
func New(opt Options) *Server {
	s := &Server{
		log:     opt.Logger,
		now:     opt.Now,
		loc:     opt.Location,
		pw:      &passwordFile{path: opt.PasswordPath},
		login:   newLoginTokens(),
		pages:   map[string]*template.Template{},
		backend: opt.Backend,
		ops:     map[string]*formOp{},
		uid:     os.Getuid,
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.loc == nil {
		s.loc = time.Local
	}
	port := strconv.Itoa(int(opt.Listen.Port()))
	s.hosts = []string{"127.0.0.1:" + port, "localhost:" + port}
	if own := strings.ToLower(opt.Listen.String()); !slices.Contains(s.hosts, own) {
		s.hosts = append(s.hosts, own)
	}
	s.base = template.Must(template.New("").Funcs(s.funcs()).ParseFS(assets, "templates/layout.html", "templates/icons.html",
		"templates/pipeline.html", "templates/fields.html"))
	for _, name := range []string{"login", "overview", "domains", "live", "settings", "review", "backup"} {
		s.pages[name] = template.Must(template.Must(s.base.Clone()).ParseFS(assets, "templates/"+name+".html"))
	}

	mux := http.NewServeMux()
	handle := func(pattern string, h http.HandlerFunc) {
		s.routes = append(s.routes, pattern)
		mux.HandleFunc(pattern, h)
	}
	handle("GET /login", s.getLogin)
	handle("POST /login", s.postLogin)
	handle("POST /logout", s.authed(maxFormBytes, s.postLogout))
	handle("GET /{$}", s.authed(0, s.getOverview))
	handle("GET /domains", s.authed(0, s.getDomains))
	handle("GET /live", s.authed(0, s.getLive))
	handle("GET /settings", s.authed(0, s.getSettings))
	handle("GET /settings/backup", s.authed(0, s.getBackup))
	handle("POST /settings/validate", s.authed(maxSettingsFormBytes, s.postValidate))
	handle("POST /settings", s.authed(maxSettingsFormBytes, s.postSettings))
	handle("POST /config/draft", s.authed(maxSettingsFormBytes, s.postDraft))
	handle("POST /config/apply", s.authed(maxSettingsFormBytes, s.postApply))
	handle("POST /config/reload", s.authed(maxFormBytes, s.postReload))
	// The rules and upstreams pages (design 0009 task C).
	s.addConfigPages(handle)
	// Task D of design 0009: Interfaces, Proxy, Remote clients, General,
	// and Account.
	s.addPagesD(handle)
	static, _ := fs.Sub(assets, "static")
	files := http.StripPrefix("/static/", http.FileServerFS(static))
	handle("GET /static/", func(w http.ResponseWriter, r *http.Request) {
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
	Title string
	// Message is an error about the request, shown as the first banner
	// of a session page or on the sign-in form.
	Message  string
	Disabled bool
	// Token is the login token on the login page and the CSRF token on
	// session pages.
	Token string
	// Page names a session page by its navigation entry; the layout then
	// draws the shell with that entry marked (design 0009 D2).
	Page string
	// Refresh, when positive, reloads the page after that many seconds:
	// the script does, and waits while a menu or dialog is open; without
	// the script, a <noscript> meta refresh does.
	Refresh int
	// Data holds the page's own content.
	Data any

	// Shell is the shell around a session page; render fills it.
	Shell *shell
	// Banners are the page's own banners; render puts the shell's ahead
	// of them.
	Banners []banner
	// Toasts report the result of the request's action.
	Toasts []toast
	// status is the instance state the page shows, so that the shell
	// and the page agree; nil makes render read it.
	status *control.Status
}

// render renders a whole page. A session page gets the shell. Every page
// answers with Vary: Cuttle-Fragment, because a script request for the
// same URL may get one block of it (design 0009 D10).
func (s *Server) render(w http.ResponseWriter, code int, page string, d pageData) {
	if d.Page != "" {
		s.decorate(&d, d.status)
	}
	var buf bytes.Buffer
	if err := s.pages[page].ExecuteTemplate(&buf, "layout", d); err != nil {
		s.log.Error("admin page", "page", page, "error", err.Error())
		http.Error(w, "500 Internal Server Error", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Add("Vary", fragmentHeader)
	w.WriteHeader(code)
	w.Write(buf.Bytes())
}

// currentSession returns the live session the request's cookies name and
// marks it used. Every cuttle_session cookie is tried, so a cookie that
// another server on 127.0.0.1 set for the same name cannot hide the real
// one. A request for /live, which reloads itself every 5 s, does not mark
// the session used, so a forgotten tab cannot hold a session past its idle
// timeout (design 0005 D7, D9).
func (s *Server) currentSession(r *http.Request) (string, session, bool) {
	touch := r.URL.Path != "/live"
	cookies := r.CookiesNamed(SessionCookie)
	if len(cookies) == 0 {
		return "", session{}, false
	}
	pw := s.pw.load()
	now := s.now()
	for _, c := range cookies {
		if v, ok := s.sessions.get(c.Value, now, pw, touch); ok {
			return c.Value, v, true
		}
	}
	return "", session{}, false
}

// authed serves h only within a live session. A GET without one is sent to
// /login; any other method gets 403. A POST, whose body may hold at most
// limit bytes, must carry the session's CSRF token (design 0005 D8).
func (s *Server) authed(limit int64, h func(http.ResponseWriter, *http.Request, string, session)) http.HandlerFunc {
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
			if !parseForm(w, r, limit) {
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

// parseForm reads a URL-encoded body of at most limit bytes. A larger
// body gets 413, any other invalid form 400.
func parseForm(w http.ResponseWriter, r *http.Request, limit int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if err := r.ParseForm(); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "413 Request Entity Too Large", http.StatusRequestEntityTooLarge)
			return false
		}
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
	if !parseForm(w, r, maxFormBytes) {
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

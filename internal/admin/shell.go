package admin

import (
	"bytes"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/neutrospec/cuttle/internal/control"
)

// The shell of every session page (design 0009 D2): the sidebar with the
// three navigation groups and the instance state, the banners, and the
// toasts.

// navPage is one entry of the sidebar. Path is empty for a page that a
// later stage of design 0009 builds; the sidebar shows such an entry as
// unavailable instead of linking to a 404, and the stage that adds the
// page sets its path.
type navPage struct {
	group, id, label, icon, path string
	// count names the configuration list whose length the entry shows.
	count string
}

// navPages are the sidebar's entries in order (design 0009 D2).
var navPages = []navPage{
	{group: "Traffic", id: "overview", label: "Overview", icon: "overview", path: "/"},
	{group: "Traffic", id: "domains", label: "Domains", icon: "domains", path: "/domains"},
	{group: "Traffic", id: "live", label: "Live", icon: "live", path: "/live"},
	{group: "Configuration", id: "rules", label: "Rules", icon: "rules", count: "rules"},
	{group: "Configuration", id: "upstreams", label: "Upstreams", icon: "upstreams", count: "upstreams"},
	{group: "Configuration", id: "interfaces", label: "Interfaces", icon: "interfaces", count: "interfaces", path: "/interfaces"},
	{group: "Configuration", id: "proxy", label: "Proxy", icon: "proxy", path: "/proxy"},
	{group: "Configuration", id: "remote", label: "Remote clients", icon: "remote", path: "/remote"},
	{group: "Configuration", id: "general", label: "General", icon: "general", path: "/general"},
	{group: "Admin", id: "account", label: "Account", icon: "account", path: "/account"},
	{group: "Admin", id: "advanced", label: "Advanced", icon: "advanced", path: "/settings"},
}

// navItem is one sidebar entry as the layout draws it.
type navItem struct {
	Label, Icon, Href string
	// Count is the length of the list the page edits, or "".
	Count   string
	Current bool
}

type navGroup struct {
	Label string
	Items []navItem
}

// shell is the data of the shell around a session page.
type shell struct {
	// Name is the instance name; Version, Uptime, and Loaded describe the
	// running instance.
	Name, Version, Uptime, Loaded string
	Nav                           []navGroup
}

// seg is a piece of text; Code segments are set in <code>.
type seg struct {
	Text string
	Code bool
}

// rich splits s into segments, with each `quoted` part as code. It is
// for constant text; values from the file, the traffic, or the validator
// go in plain segments.
func rich(s string) []seg {
	var out []seg
	for i, part := range strings.Split(s, "`") {
		if part != "" {
			out = append(out, seg{Text: part, Code: i%2 == 1})
		}
	}
	return out
}

// plain is a single text segment.
func plain(s string) []seg { return []seg{{Text: s}} }

// banner is one notice in the banner area of the shell or of a page
// (design 0009 D2, D3.13).
type banner struct {
	// Kind is info, ok, warn, or error.
	Kind  string
	Title []seg
	Lines [][]seg
	// Pre is preformatted text, such as validator messages.
	Pre     string
	Actions []link
	// Post is an action that changes state, a button in a form with the
	// CSRF token, such as "Reload now".
	Post *postAction
}

// postAction is a button that posts to Action with the CSRF token and the
// page to return to.
type postAction struct {
	Label, Action, Token, Return string
}

// toast reports the result of an action (design 0009 D7). Kind ok closes
// by itself after 8 s with the script; warn and error stay until closed.
type toast struct {
	Kind  string
	Title string
	Lines [][]seg
}

// restartCommand restarts cuttle under brew services (design 0009 D2).
const restartCommand = "brew services restart cuttle"

// bannerIcon names the icon of a banner or toast kind.
func bannerIcon(kind string) string {
	switch kind {
	case "ok":
		return "check"
	case "info":
		return "info"
	}
	return "alert"
}

// decorate fills the shell, its banners, and the navigation state of a
// session page; st is the instance state the page shows, or nil.
func (s *Server) decorate(d *pageData, st *control.Status) {
	if st == nil {
		v := s.backend.Status()
		st = &v
	}
	sh := &shell{Name: st.Name, Version: st.Version, Uptime: "-", Loaded: "-"}
	now := s.now()
	if !st.StartedAt.IsZero() {
		sh.Uptime = humanDuration(now.Sub(st.StartedAt))
	}
	if t := st.Config.LoadedAt; !t.IsZero() {
		sh.Loaded = s.when(t, now)
	}
	counts := map[string]string{}
	if cfg := s.backend.Config(); cfg != nil {
		counts["rules"] = strconv.Itoa(len(cfg.Rules))
		counts["upstreams"] = strconv.Itoa(len(cfg.Upstreams))
		counts["interfaces"] = strconv.Itoa(len(cfg.Interfaces))
	}
	for _, p := range navPages {
		if n := len(sh.Nav); n == 0 || sh.Nav[n-1].Label != p.group {
			sh.Nav = append(sh.Nav, navGroup{Label: p.group})
		}
		g := &sh.Nav[len(sh.Nav)-1]
		g.Items = append(g.Items, navItem{Label: p.label, Icon: p.icon, Href: p.path, Count: counts[p.count],
			Current: p.id == d.Page && p.path != ""})
	}
	d.Shell = sh
	var b []banner
	if d.Message != "" {
		b = append(b, banner{Kind: "error", Title: plain(d.Message)})
	}
	d.Banners = append(append(b, s.shellBanners(st, d.Page, d.Token)...), d.Banners...)
	// The toast of an action that redirected here, shown once.
	d.Toasts = append(s.sessions.takeFlash(d.Token), d.Toasts...)
}

// shellBanners returns the banners of the instance state, at most one of
// each, in the order of design 0009 D2: the file on disk has errors (or
// cannot be read), the file differs from the running configuration, the
// last reload failed, and restart required. token is the session's CSRF
// token for "Reload now".
func (s *Server) shellBanners(st *control.Status, page, token string) []banner {
	var out []banner
	file := s.readFile()
	switch {
	case file.ReadErr != nil:
		b := banner{Kind: "error", Title: plain("The configuration file cannot be read, so the configuration pages are read-only."),
			Lines: [][]seg{plain(file.ReadErr.Error()), plain("The running configuration stays in force.")}}
		if page != "advanced" {
			b.Actions = []link{{Label: "Open Advanced", Href: "/settings"}}
		}
		out = append(out, b)
	case !file.Valid():
		b := banner{Kind: "error", Title: plain("The configuration file has errors, so the configuration pages are read-only."),
			Lines: [][]seg{plain("The running configuration, loaded " + s.when(st.Config.LoadedAt, s.now()) + ", stays in force until the file is fixed.")},
			Pre:   strings.Join(issueLines(file.Report.Errors), "\n")}
		if page != "advanced" {
			b.Actions = append(b.Actions, link{Label: "Fix in Advanced", Href: "/settings"})
		}
		if bak := s.readBackup(); bak.Offered() && bak.Valid {
			b.Actions = append(b.Actions, link{Label: "Restore the previous version", Href: "/settings/backup"})
		}
		out = append(out, b)
	case file.Digest != st.Config.SHA256:
		ret := "/"
		for _, p := range navPages {
			if p.id == page && p.path != "" {
				ret = p.path
			}
		}
		out = append(out, banner{Kind: "info", Title: plain("The file changed since cuttle loaded it."),
			Lines: [][]seg{plain("The running configuration is the one cuttle loaded; reload to use the file as it is now.")},
			Post:  &postAction{Label: "Reload now", Action: "/config/reload", Token: token, Return: ret}})
	}
	if e := st.Config.LastReloadError; e != "" {
		b := banner{Kind: "error", Title: plain("The last reload refused the file; the running configuration is unchanged."), Pre: e}
		if page != "advanced" {
			b.Actions = []link{{Label: "Open Advanced", Href: "/settings"}}
		}
		out = append(out, b)
	}
	if keys := st.Config.RestartRequired; len(keys) > 0 {
		out = append(out, banner{Kind: "warn", Title: restartTitle(keys), Lines: [][]seg{restartLine()}})
	}
	return out
}

// restartTitle says which restart-only keys wait for the next start,
// such as "`a`, `b` and `c` changes apply at the next start."
func restartTitle(keys []string) []seg {
	var out []seg
	for i, k := range keys {
		switch {
		case i == 0:
		case i == len(keys)-1:
			out = append(out, seg{Text: " and "})
		default:
			out = append(out, seg{Text: ", "})
		}
		out = append(out, seg{Text: k, Code: true})
	}
	return append(out, seg{Text: " changes apply at the next start."})
}

// restartLine says how to restart (design 0009 D2).
func restartLine() []seg {
	return rich("Restart with `" + restartCommand + "` to use the new values.")
}

// when formats a time for the shell: the time of day for today, the date
// and time otherwise.
func (s *Server) when(t, now time.Time) string {
	t, now = t.In(s.loc), now.In(s.loc)
	if y, m, d := t.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return "at " + t.Format("15:04")
	}
	return "on " + t.Format("2006-01-02") + " at " + t.Format("15:04")
}

// humanDuration formats an uptime in the largest whole unit that reads
// well: minutes, hours and minutes, or days.
func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "less than a minute"
	case d < time.Hour:
		return plural(int(d/time.Minute), "minute")
	case d < 48*time.Hour:
		h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
		if m == 0 {
			return plural(h, "hour")
		}
		return plural(h, "hour") + " " + plural(m, "minute")
	}
	return plural(int(d/(24*time.Hour)), "day")
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
}

// fragmentHeader marks a script request for one block of a page (design
// 0009 D10).
const fragmentHeader = "Cuttle-Fragment"

// wantsFragment reports whether the script asked for a fragment.
func wantsFragment(r *http.Request) bool { return r.Header.Get(fragmentHeader) == "1" }

// renderFragment renders only the named block of a page, whose root
// element carries data-block; the script replaces its target's
// children with that element. The block gets the pageData that the
// whole page gets, and html/template escapes it as in the page.
func (s *Server) renderFragment(w http.ResponseWriter, code int, page, block string, d pageData) {
	var buf bytes.Buffer
	if err := s.pages[page].ExecuteTemplate(&buf, block, d); err != nil {
		s.log.Error("admin fragment", "page", page, "block", block, "error", err.Error())
		http.Error(w, "500 Internal Server Error", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Add("Vary", fragmentHeader)
	w.WriteHeader(code)
	w.Write(buf.Bytes())
}

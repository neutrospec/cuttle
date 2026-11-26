package admin

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/neutrospec/cuttle/internal/control"
	"github.com/neutrospec/cuttle/internal/stats"
)

// Dashboard limits (design 0005 D9).
const (
	// queryTimeout bounds the statistics queries of one page.
	queryTimeout = 10 * time.Second
	// topHosts is the number of hosts the overview lists per route.
	topHosts = 10
	// liveRefresh is the reload interval of /live in seconds.
	liveRefresh = 5
	// defaultRetentionDays applies when no running configuration is known.
	defaultRetentionDays = 30
)

// liveCounters are the live counters the overview and /live show, in
// display order (design 0002 D7).
var liveCounters = []string{
	stats.TunnelsActive, stats.ConnectionsActive, stats.HTTPRequestsActive,
	stats.ConnectionsTotal, stats.ConnectionsRejectedTotal,
	stats.StatsWriteErrorsTotal, stats.StatsFoldedTotal,
}

// funcs are the template helpers.
func (s *Server) funcs() template.FuncMap {
	return template.FuncMap{
		"bytes": formatBytes,
		"num":   formatNum,
		"unix": func(sec int64) string {
			return time.Unix(sec, 0).In(s.loc).Format("2006-01-02 15:04")
		},
		"time": func(t time.Time) string {
			if t.IsZero() {
				return "-"
			}
			return t.In(s.loc).Format("2006-01-02 15:04:05 MST")
		},
		"bar":  barClass,
		"secs": func(n int64) string { return (time.Duration(n) * time.Second).String() },
		"dash": func(v string) string {
			if v == "" {
				return "-"
			}
			return v
		},
		"join":       strings.Join,
		"bannerIcon": bannerIcon,
		"plural":     plural,
		"rowIndex": func(first, i int) int64 {
			return int64(first + i)
		},
	}
}

// formatBytes prints a byte count in binary units with one decimal.
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

// formatNum prints an integer with thousands separators.
func formatNum(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// barClass returns the stylesheet class of a bar for v out of max, in
// steps of 5 %. The pages use classes because the Content-Security-Policy
// of design 0005 D5 refuses inline styles.
func barClass(v, max int64) string {
	if max <= 0 || v <= 0 {
		return "w0"
	}
	step := (v*20 + max - 1) / max
	return "w" + strconv.FormatInt(min(step, 20), 10)
}

// windowView describes the chosen window to a page.
type windowView struct {
	stats.Window
	// Label is the preset or the date range as chosen.
	Label string
	// Range is the exact range of whole hours the window covers.
	Range string
	// Query carries the window to links on other pages.
	Query   url.Values
	Presets []link
}

// link is a navigation link; Current marks the page or choice in force.
type link struct {
	Label, Href string
	Current     bool
}

// retentionDays returns stats.retention_days of the running configuration.
func (s *Server) retentionDays() int {
	if cfg := s.backend.Config(); cfg != nil && cfg.Stats.RetentionDays > 0 {
		return cfg.Stats.RetentionDays
	}
	return defaultRetentionDays
}

// window reads the window of a request: from and to as local dates, or
// window as a preset, 24h by default (design 0005 D9). The view holds the
// preset links even when the request's window is invalid.
func (s *Server) window(r *http.Request, path string, keep url.Values) (windowView, error) {
	now := s.now()
	q := r.URL.Query()
	var w stats.Window
	var err error
	from, to := q.Get("from"), q.Get("to")
	switch {
	case from != "" || to != "":
		if from == "" || to == "" {
			err = fmt.Errorf("a date range needs both from and to")
			break
		}
		w, err = stats.DateWindow(from, to, now, s.loc, s.retentionDays())
	default:
		preset := q.Get("window")
		if preset == "" {
			preset = stats.DefaultPreset
		}
		w, err = stats.PresetWindow(preset, now)
	}
	v := windowView{Query: url.Values{}}
	for _, p := range stats.Presets {
		lq := url.Values{}
		for k, vs := range keep {
			lq[k] = vs
		}
		lq.Set("window", p)
		v.Presets = append(v.Presets, link{Label: p, Href: path + "?" + lq.Encode(), Current: err == nil && p == w.Preset})
	}
	if err != nil {
		return v, err
	}
	v.Window = w
	if w.Preset != "" {
		v.Label = w.Preset
		v.Query.Set("window", w.Preset)
	} else {
		v.Label = w.FromDate + " to " + w.ToDate
		v.Query.Set("from", w.FromDate)
		v.Query.Set("to", w.ToDate)
	}
	v.Range = w.Start().In(s.loc).Format("2006-01-02 15:04") + " to " + w.End().In(s.loc).Format("2006-01-02 15:04 MST")
	return v, nil
}

// statsState is the statistics part of a page: enabled, or unavailable
// with the reason, and the last failed write (OI-0006 Q-R5).
type statsState struct {
	OK           bool
	Reason       string
	WriteError   string
	WriteErrorAt time.Time
	// QueryError is a failed dashboard query.
	QueryError string
}

func (s *Server) statsState(st control.Status) statsState {
	v := statsState{OK: s.backend.Stats() != nil, Reason: st.StatsErr, WriteError: st.StatsWriteError}
	if st.StatsWriteErrorAt != nil {
		v.WriteErrorAt = *st.StatsWriteErrorAt
	}
	return v
}

// counter is one live counter.
type counter struct {
	Name  string
	Value int64
}

func pickCounters(all map[string]int64) []counter {
	out := make([]counter, 0, len(liveCounters))
	for _, k := range liveCounters {
		out = append(out, counter{Name: k, Value: all[k]})
	}
	return out
}

// routeBlock is one route of the overview: its total and its top hosts.
// Bars show bytes, or requests on the block route, which moves none.
type routeBlock struct {
	stats.Total
	Top        []stats.HostRow
	ByRequests bool
	MaxTop     int64
	AllHref    string
}

// overview is the data of GET / (design 0005 D9).
type overview struct {
	Status        control.Status
	Uptime        string
	Window        windowView
	Stats         statsState
	Routes        []routeBlock
	MaxRouteBytes int64
	Interfaces    []stats.Total
	MaxIfBytes    int64
	Counters      []counter
}

// getOverview answers the operator's questions directly: the domains sent
// to each upstream, sent direct, and blocked, with totals per route and
// per interface, live counters, upstream and interface states, and the
// configuration and statistics status (design 0005 D9).
func (s *Server) getOverview(w http.ResponseWriter, r *http.Request, _ string, v session) {
	st := s.backend.Status()
	d := overview{Status: st, Stats: s.statsState(st), Counters: pickCounters(st.Counters)}
	d.Uptime = s.now().Sub(st.StartedAt).Round(time.Second).String()
	win, err := s.window(r, "/", nil)
	d.Window = win
	if err != nil {
		s.render(w, http.StatusBadRequest, "overview", pageData{Title: "Overview", Token: v.csrf, Page: "overview", Message: err.Error(), Data: d, status: &st})
		return
	}
	if sv := s.backend.Stats(); sv != nil {
		ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
		defer cancel()
		if err := s.fillOverview(ctx, sv, &d); err != nil {
			d.Stats.QueryError = err.Error()
			s.log.Warn("admin statistics query", "error", err.Error())
		}
	}
	s.render(w, http.StatusOK, "overview", pageData{Title: "Overview", Token: v.csrf, Page: "overview", Data: d, status: &st})
}

func (s *Server) fillOverview(ctx context.Context, sv *stats.Stats, d *overview) error {
	f := stats.Filter{Window: d.Window.Window}
	routes, err := sv.RouteTotals(ctx, f)
	if err != nil {
		return err
	}
	for _, t := range routes {
		b := routeBlock{Total: t, ByRequests: t.Action == "block"}
		rf := f
		rf.Route = t.Route()
		if b.Top, err = sv.Domains(ctx, rf, topHosts, 0); err != nil {
			return err
		}
		for _, h := range b.Top {
			if b.ByRequests {
				b.MaxTop = max(b.MaxTop, h.Requests)
			} else {
				b.MaxTop = max(b.MaxTop, h.Bytes())
			}
		}
		q := url.Values{"route": {t.Route()}}
		for k, vs := range d.Window.Query {
			q[k] = vs
		}
		b.AllHref = "/domains?" + q.Encode()
		d.MaxRouteBytes = max(d.MaxRouteBytes, t.Bytes())
		d.Routes = append(d.Routes, b)
	}
	if d.Interfaces, err = sv.InterfaceTotals(ctx, f); err != nil {
		return err
	}
	// Without interface definitions every row has an empty interface, and
	// the table would repeat the route totals.
	if len(d.Interfaces) == 1 && d.Interfaces[0].Interface == "" && len(d.Status.Interfaces) == 0 {
		d.Interfaces = nil
	}
	for _, t := range d.Interfaces {
		d.MaxIfBytes = max(d.MaxIfBytes, t.Bytes())
	}
	return nil
}

// option is one choice of a filter select.
type option struct {
	Value, Label string
	Selected     bool
}

// domainsPage is the data of GET /domains (design 0005 D9).
type domainsPage struct {
	Window     windowView
	Stats      statsState
	Route      []option
	Source     []option
	Interface  []option
	Rows       []stats.HostRow
	MaxBytes   int64
	First      int // 1-based index of the first row
	Prev, Next string
	// Rule is the rule filter, and AllRules the page without it.
	Rule, AllRules string
}

// getDomains shows the host table filtered by route, source, interface,
// and window, 500 rows a page (design 0005 D9).
func (s *Server) getDomains(w http.ResponseWriter, r *http.Request, _ string, v session) {
	q := r.URL.Query()
	f := stats.Filter{Route: q.Get("route"), Source: q.Get("source"), Interface: q.Get("interface")}
	for _, p := range []*string{&f.Route, &f.Source, &f.Interface} {
		if *p == "" {
			*p = "all"
		}
	}
	keep := url.Values{"route": {f.Route}, "source": {f.Source}, "interface": {f.Interface}}
	st := s.backend.Status()
	d := domainsPage{Stats: s.statsState(st)}
	if rule := q.Get("rule"); rule != "" && rule != "all" {
		all := url.Values{}
		for k, vs := range r.URL.Query() {
			if k != "rule" && k != "offset" {
				all[k] = vs
			}
		}
		f.Rule, d.Rule, d.AllRules = rule, rule, "/domains?"+all.Encode()
		keep.Set("rule", rule)
	}
	d.Route, d.Source, d.Interface = s.filterOptions(f)
	win, err := s.window(r, "/domains", keep)
	offset := 0
	if o := q.Get("offset"); err == nil && o != "" {
		if offset, err = strconv.Atoi(o); err != nil || offset < 0 || offset%stats.MaxDomainRows != 0 {
			err = fmt.Errorf("offset must be a multiple of %d", stats.MaxDomainRows)
		}
	}
	if err == nil {
		f.Window = win.Window
		err = f.Check()
	}
	d.Window = win
	page := pageData{Title: "Domains", Token: v.csrf, Page: "domains", Data: &d, status: &st}
	if err != nil {
		page.Message = err.Error()
		s.render(w, http.StatusBadRequest, "domains", page)
		return
	}
	if sv := s.backend.Stats(); sv != nil {
		ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
		defer cancel()
		rows, err := sv.Domains(ctx, f, stats.MaxDomainRows+1, offset)
		if err != nil {
			d.Stats.QueryError = err.Error()
			s.log.Warn("admin statistics query", "error", err.Error())
		}
		more := len(rows) > stats.MaxDomainRows
		if more {
			rows = rows[:stats.MaxDomainRows]
		}
		d.Rows, d.First = rows, offset+1
		for _, h := range rows {
			d.MaxBytes = max(d.MaxBytes, h.Bytes())
		}
		pq := url.Values{}
		for k, vs := range keep {
			pq[k] = vs
		}
		for k, vs := range win.Query {
			pq[k] = vs
		}
		if offset > 0 {
			pq.Set("offset", strconv.Itoa(offset-stats.MaxDomainRows))
			d.Prev = "/domains?" + pq.Encode()
		}
		if more {
			pq.Set("offset", strconv.Itoa(offset+stats.MaxDomainRows))
			d.Next = "/domains?" + pq.Encode()
		}
	}
	s.render(w, http.StatusOK, "domains", page)
}

// filterOptions lists the choices of the domain filters: the routes and
// interfaces of the running configuration, and a chosen value that it no
// longer names, such as an upstream that a reload removed.
func (s *Server) filterOptions(f stats.Filter) (routes, sources, ifaces []option) {
	rv := []string{"all", "direct", "upstream"}
	iv := []string{"all", "none", "other"}
	if cfg := s.backend.Config(); cfg != nil {
		for _, u := range cfg.Upstreams {
			rv = append(rv, "upstream:"+u.Name)
		}
		for _, i := range cfg.Interfaces {
			iv = append(iv, i.Name)
		}
	}
	rv = append(rv, "block")
	if !slices.Contains(rv, f.Route) {
		rv = append(rv, f.Route)
	}
	if !slices.Contains(iv, f.Interface) {
		iv = append(iv, f.Interface)
	}
	opts := func(values []string, chosen string, labels map[string]string) []option {
		out := make([]option, len(values))
		for i, v := range values {
			l := labels[v]
			if l == "" {
				l = v
			}
			out[i] = option{Value: v, Label: l, Selected: v == chosen}
		}
		return out
	}
	return opts(rv, f.Route, map[string]string{"all": "all routes", "upstream": "all upstreams"}),
		opts([]string{"all", "local", "remote"}, f.Source, map[string]string{"all": "all sources"}),
		opts(iv, f.Interface, map[string]string{"all": "all interfaces", "none": "no interface"})
}

// livePage is the data of GET /live.
type livePage struct {
	Status   control.Status
	Counters []counter
}

// getLive lists the active tunnels and reloads every 5 s (design 0005 D9).
func (s *Server) getLive(w http.ResponseWriter, r *http.Request, _ string, v session) {
	st := s.backend.Status()
	s.render(w, http.StatusOK, "live", pageData{Title: "Live", Token: v.csrf, Page: "live", Refresh: liveRefresh,
		Data: livePage{Status: st, Counters: pickCounters(st.Counters)}, status: &st})
}

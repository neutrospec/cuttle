package admin

import (
	"context"
	"html/template"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/neutrospec/cuttle/internal/config"
	"github.com/neutrospec/cuttle/internal/control"
	"github.com/neutrospec/cuttle/internal/policy"
	"github.com/neutrospec/cuttle/internal/stats"
)

// Shared parts of the configuration pages of design 0009 task C: the
// model a page shows, routes and upstream states as the pages draw them,
// the 24-hour traffic hints of D9, and Test a URL of D8.

// addConfigPages parses the templates of the rules and upstreams pages,
// registers their form ops, and adds their routes.
func (s *Server) addConfigPages(handle func(string, http.HandlerFunc)) {
	for _, name := range []string{"rules", "rule", "upstreams", "upstream"} {
		s.pages[name] = template.Must(template.Must(s.base.Clone()).ParseFS(assets, "templates/"+name+".html", "templates/testurl.html"))
	}
	s.addRuleOps()
	s.addUpstreamOps()
	handle("GET /rules", s.authed(0, s.getRules))
	handle("GET /rules/{name}", s.authed(0, s.getRule))
	handle("GET /upstreams", s.authed(0, s.getUpstreams))
	handle("GET /upstreams/{name}", s.authed(0, s.getUpstream))
}

// pagePath returns the path of a navigation entry, or "" for a page that
// a later stage builds, so that a page links only to pages that exist.
func pagePath(id string) string {
	for _, p := range navPages {
		if p.id == id {
			return p.path
		}
	}
	return ""
}

// routeView is a route as the route template draws it.
type routeView struct{ Action, Upstream string }

// routeOf reads an action or default: direct, block, or upstream:<name>.
func routeOf(action string) routeView {
	if name, ok := strings.CutPrefix(action, "upstream:"); ok {
		return routeView{Action: "upstream", Upstream: name}
	}
	return routeView{Action: action}
}

// confState is the configuration that a configuration page shows: the
// file's model while the file is valid, otherwise a model of the running
// configuration, which the page shows read-only (design 0009 D3.13).
type confState struct {
	file     *fileState
	doc      *config.Doc
	cfg      *config.Config
	readOnly bool
}

func (s *Server) confState() confState {
	f := s.readFile()
	if f.Valid() {
		return confState{file: f, doc: f.Doc, cfg: f.Cfg}
	}
	st := confState{file: f, readOnly: true, cfg: s.backend.Config()}
	if st.cfg != nil {
		st.doc = runningDoc(st.cfg)
	} else {
		st.doc, st.cfg = &config.Doc{}, &config.Config{}
	}
	return st
}

// warnings returns the file's validator warnings of the entities of kind,
// by their 1-based index, each with its key and entry.
func (c confState) warnings(kind string) map[int][]string {
	out := map[int][]string{}
	if c.readOnly || c.file.Report == nil {
		return out
	}
	for _, is := range c.file.Report.Warnings {
		if ref, ok := parseIssueKey(is.Key); ok && ref.Entity == kind && ref.Index > 0 {
			out[ref.Index] = append(out[ref.Index], issueText(ref, is.Msg))
		}
	}
	return out
}

// runningDoc builds a model of a validated configuration for the
// read-only pages: the values in force, which may include defaults that
// the file leaves unset, and no notes, which the configuration does not
// hold.
func runningDoc(cfg *config.Config) *config.Doc {
	d := &config.Doc{Name: cfg.Name}
	d.Proxy.Default = cfg.Proxy.Default.String()
	if len(cfg.Proxy.Interfaces) > 0 {
		d.Proxy.Interfaces = config.Some(cfg.Proxy.Interfaces)
	}
	d.Remote.Default = config.Some(cfg.Proxy.Remote.Default.String())
	for _, x := range cfg.Interfaces {
		d.Interfaces = append(d.Interfaces, config.DocInterface{Name: x.Name})
	}
	dur := func(t time.Duration) config.Opt[string] { return config.Some(t.String()) }
	for _, u := range cfg.Upstreams {
		du := config.DocUpstream{Name: u.Name, Address: u.Address, Disclosure: u.Disclosure,
			ConnectTimeout: dur(u.ConnectTimeout), ResponseTimeout: dur(u.ResponseTimeout), UnreachableHold: dur(u.UnreachableHold)}
		if u.Interfaces != nil {
			du.Interfaces = config.Some(u.Interfaces)
		}
		d.Upstreams = append(d.Upstreams, du)
	}
	list := func(l []string) config.Opt[[]string] {
		if len(l) == 0 {
			return config.Opt[[]string]{}
		}
		return config.Some(l)
	}
	flag := func(b bool) config.Opt[bool] {
		if !b {
			return config.Opt[bool]{}
		}
		return config.Some(true)
	}
	for _, r := range cfg.Rules {
		dr := config.DocRule{Name: r.Name, Hosts: list(r.Hosts), Domains: list(r.Domains), Action: r.Route.String(),
			AllowLoopback: flag(r.AllowLoopback), AllowPrivate: flag(r.AllowPrivate)}
		var cidrs []string
		for _, p := range r.CIDRs {
			cidrs = append(cidrs, p.String())
		}
		dr.CIDRs = list(cidrs)
		if len(r.Ports) > 0 {
			ports := make([]int64, len(r.Ports))
			for i, p := range r.Ports {
				ports[i] = int64(p)
			}
			dr.Ports = config.Some(ports)
		}
		if r.Interfaces != nil {
			dr.Interfaces = config.Some(r.Interfaces)
		}
		d.Rules = append(d.Rules, dr)
	}
	return d
}

// upState is an upstream's live state as the pages show it (design 0009
// D3.5): up, down for some time with the requests that failed fast, or
// probing, and the last error.
type upState struct {
	State, Text, LastError string
	Down                   bool
}

// Lower is the state's text as it reads inside a sentence.
func (u upState) Lower() string {
	if u.Text == "" {
		return ""
	}
	return strings.ToLower(u.Text[:1]) + u.Text[1:]
}

// upstreamStates returns the live state of each upstream of the running
// instance by name.
func (s *Server) upstreamStates(st control.Status) map[string]upState {
	out := map[string]upState{}
	for _, u := range st.Upstreams {
		v := upState{State: u.State, LastError: u.LastError}
		switch u.State {
		case "up":
			v.Text = "Up"
			if !u.Since.IsZero() {
				v.Text = "Up since " + strings.TrimPrefix(strings.TrimPrefix(s.when(u.Since, s.now()), "at "), "on ")
			}
		case "down":
			v.Down = true
			v.Text = "Down for " + humanSeconds(u.DownS)
			if u.FailedFast > 0 {
				v.Text += ", " + formatNum(u.FailedFast) + " failed fast"
			}
		case "probing":
			v.Text = "Probing after " + humanSeconds(u.DownS) + " down"
		default:
			v.Text = u.State
		}
		out[u.Name] = v
	}
	return out
}

// humanSeconds formats a duration in seconds as "42 s", "3 min", or
// "2 h 5 min".
func humanSeconds(sec float64) string {
	d := time.Duration(sec) * time.Second
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d/time.Second)) + " s"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + " min"
	}
	h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
	if m == 0 {
		return strconv.Itoa(h) + " h"
	}
	return strconv.Itoa(h) + " h " + strconv.Itoa(m) + " min"
}

// traffic is the 24-hour traffic of design 0009 D9: totals by rule and by
// route. ok is false without statistics or after a failed query; the
// pages then leave the traffic out and say why in one muted line.
type traffic struct {
	ok     bool
	reason string
	rules  map[string]stats.Total
	routes map[string]stats.Total
}

// line is the muted line about the traffic columns.
func (t traffic) line() string {
	if t.ok {
		return "Traffic: last 24 hours"
	}
	return "Statistics unavailable" + t.reason
}

func (s *Server) dayTraffic(ctx context.Context) traffic {
	sv := s.backend.Stats()
	if sv == nil {
		return traffic{}
	}
	t := traffic{ok: true, rules: map[string]stats.Total{}, routes: map[string]stats.Total{}}
	w, err := stats.PresetWindow("24h", s.now())
	if err != nil {
		return traffic{}
	}
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	rules, err := sv.RuleTotals(ctx, stats.Filter{Window: w})
	if err == nil {
		var routes []stats.Total
		if routes, err = sv.RouteTotals(ctx, stats.Filter{Window: w}); err == nil {
			for _, r := range rules {
				t.rules[r.Rule] = r
			}
			for _, r := range routes {
				t.routes[r.Route()] = r
			}
			return t
		}
	}
	s.log.Warn("admin statistics query", "error", err.Error())
	return traffic{reason: ": the query failed"}
}

// domainsHref links to the domains page filtered by key and value over
// the last 24 hours.
func domainsHref(key, value string) string {
	return "/domains?" + url.Values{key: {value}, "window": {"24h"}}.Encode()
}

// testView is the result of Test a URL (design 0009 D3.3, D3.4, D8), as
// the rules page and the rule editor show it.
type testView struct {
	Input  string
	Remote bool
	// Err says why the target was refused, Fix that the draft has errors,
	// and Note how a bare host was read.
	Err  string
	Fix  bool
	Note string
	// Head names what decided: "Rule 2, `restricted`", or the decision
	// that no rule made with its reason.
	Head  []seg
	Route routeView
	// Target is the normalized target, and Kind its kind and client.
	Target, Kind string
	// Upstream is the upstream of an upstream route with its live state.
	Upstream *testUpstream
	// Direct lists the direct flags, Answer the block answer, Interfaces
	// the list in force, and Address the address verdict.
	Direct, Answer, Interfaces, Address string
	Refused                             bool
	// The chain's marks: the rule that matched by position and name, or
	// the default row, or none when the port check refused the target.
	MatchPos  int
	MatchName string
	Default   bool
	Port      bool
	// Editor: whether the draft rule matches the target, and the rule
	// that matches first when an earlier one shadows it.
	Editor bool
	Self   bool
	Shadow []seg
}

type testUpstream struct {
	Address, Disclosure string
	State               upState
}

// testInput reads the operator's input of Test a URL: what `cuttle
// explain` accepts, and a bare host or IP address, which is read as
// https://<host> with a note that says so (design 0009 D8).
func testInput(in string) (target, note string) {
	in = strings.TrimSpace(in)
	if strings.Contains(in, "://") {
		return in, ""
	}
	if _, _, err := config.ParseTarget(in); err == nil {
		return in, ""
	}
	host := in
	if a, err := netip.ParseAddr(strings.Trim(in, "[]")); err == nil && a.Is6() && a.Zone() == "" {
		host = "[" + a.String() + "]"
	}
	if t := "https://" + host; host != "" {
		if _, _, err := config.ParseTarget(t); err == nil {
			return t, "Read as " + t + "."
		}
	}
	return in, ""
}

// explainView explains input against cfg for the page.
func (s *Server) explainView(cfg *config.Config, input string, remote bool, st control.Status) *testView {
	tv := &testView{Input: input, Remote: remote}
	target, note := testInput(input)
	src := policy.SourceLocal
	if remote {
		src = policy.SourceRemote
	}
	e, err := config.Explain(cfg, target, src)
	if err != nil {
		tv.Err = err.Error()
		return tv
	}
	tv.Note = note
	kind := "tunnel"
	if e.Kind == policy.KindHTTP {
		kind = "plain HTTP"
	}
	tv.Target, tv.Kind = e.Target.String(), kind+", "+e.Source.String()+" client"
	tv.Route = routeOf(e.Route().String())
	tv.MatchPos = e.Matched
	if e.Matched > 0 {
		tv.MatchName = cfg.Rules[e.Matched-1].Name
	}
	switch e.Rule {
	case policy.RuleDefault:
		tv.Default, tv.Head = true, rich("`default`: no rule matched; `proxy.default` applies")
	case policy.RuleRemoteDefault:
		tv.Default, tv.Head = true, rich("`remote-default`: no rule matched; `proxy.remote.default` applies")
	case policy.RulePort:
		tv.Port = true
		tv.Head = append(rich("`port`: "), plain("port "+strconv.Itoa(int(e.Target.Port))+" is not in ")...)
		tv.Head = append(tv.Head, rich("`proxy.remote.ports`, so no rule is tried")...)
	case policy.RuleRemoteDirect:
		tv.Default = e.Matched == 0
		if e.Matched > 0 {
			tv.Head = []seg{{Text: "Rule " + strconv.Itoa(e.Matched) + ", "}, {Text: tv.MatchName, Code: true}, {Text: ", routes direct, which remote clients may not use: "}}
		} else {
			tv.Head = rich("`proxy.remote.default` is direct, which remote clients may not use: ")
		}
		tv.Head = append(tv.Head, rich("`remote-direct` blocks it without `proxy.remote.allow_direct`")...)
	default:
		tv.Head = []seg{{Text: "Rule " + strconv.Itoa(e.RuleIndex) + ", "}, {Text: e.Rule, Code: true}}
	}
	switch e.Action {
	case policy.ActionUpstream:
		if e.Upstream != nil {
			tv.Upstream = &testUpstream{Address: e.Upstream.Address, Disclosure: e.Upstream.Disclosure}
			if u, ok := s.upstreamStates(st)[e.Upstream.Name]; ok {
				tv.Upstream.State = u
			}
		}
		tv.Interfaces = e.InterfacesText()
	case policy.ActionDirect:
		var flags []string
		if e.AllowLoopback {
			flags = append(flags, "allow_loopback")
		}
		if e.AllowPrivate {
			flags = append(flags, "allow_private")
		}
		tv.Direct = strings.Join(flags, ", ")
		tv.Interfaces = e.InterfacesText()
		tv.Address = e.AddressText()
		tv.Refused = e.Address != nil && e.Address.Literal && !e.Address.Allowed
	case policy.ActionBlock:
		tv.Answer = "403, no lookup and no connection"
	}
	return tv
}

// ruleTestView is the editor's test (design 0009 D3.4): the chain's
// decision on the candidate and whether the draft rule, the rule at
// index entity of the candidate, matches the target by itself.
func (s *Server) ruleTestView(d *draft, input string, remote bool) *testView {
	if strings.TrimSpace(input) == "" {
		return nil
	}
	if d.candCfg == nil || d.entity < 1 || d.entity > len(d.candCfg.Rules) {
		return &testView{Input: input, Remote: remote, Fix: true, Editor: true}
	}
	tv := s.explainView(d.candCfg, input, remote, s.backend.Status())
	tv.Editor = true
	if tv.Err != "" {
		return tv
	}
	target, _ := testInput(input)
	t, kind, _ := config.ParseTarget(target)
	draft := d.candCfg.Rules[d.entity-1]
	alone := policy.New(policy.Settings{Rules: []policy.Rule{draft}}).Decide(t, kind, policy.SourceLocal)
	tv.Self = alone.RuleIndex == 1
	if tv.Self && tv.MatchPos > 0 && tv.MatchPos < d.entity {
		tv.Shadow = []seg{{Text: "Rule " + strconv.Itoa(tv.MatchPos) + ", "}, {Text: tv.MatchName, Code: true}, {Text: ", matches first."}}
	}
	return tv
}

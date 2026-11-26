package admin

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/neutrospec/cuttle/internal/authority"
	"github.com/neutrospec/cuttle/internal/config"
	"github.com/neutrospec/cuttle/internal/policy"
)

// Route a host of design 0009 D3.12: "Route…" on the overview, domains,
// and live pages opens /rules/new?host=<host>, a dialog with the script
// and a page without it, which routes the host with a new rule or adds it
// to a rule of the chosen route. Its edit is the route.host op of D7.

const opRouteHost = "route.host"

// routeFields are the fields of Route a host: the host itself, which a
// hidden input carries and every request checks again; the match, one of
// the patterns that routeMatches offers; the route; and where the
// pattern goes, "" for a new rule or "<action> <rule>" for a rule that
// had that action when the form was rendered.
var routeFields = []formField{
	{Name: "host", Label: "Host", Kind: fieldText},
	{Name: "match", Label: "Match", Kind: fieldText},
	{Name: "action", Key: "action", Label: "Route", Kind: fieldText},
	{Name: "where", Label: "Where", Kind: fieldText},
}

// routeHost reads a host to route: a host name, made lowercase without a
// trailing dot, or an IP literal, with or without brackets. ok is false
// for anything else, such as the folded row *, so that a link or a
// prefill fills nothing (design 0009 D12).
func routeHost(raw string) (host string, ip netip.Addr, ok bool) {
	if raw == "" || len(raw) > 255 {
		return "", netip.Addr{}, false
	}
	if a, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(raw, "["), "]")); err == nil {
		if a.Zone() != "" {
			return "", netip.Addr{}, false
		}
		a = a.Unmap()
		return a.String(), a, true
	}
	name, err := authority.NormalizeName(raw)
	if err != nil || authority.CheckPattern(name) != nil {
		return "", netip.Addr{}, false
	}
	return name, netip.Addr{}, true
}

// hostOfTarget returns the host of a live tunnel's target, host:port.
func hostOfTarget(target string) string {
	if h, _, err := net.SplitHostPort(target); err == nil {
		return h
	}
	return target
}

// routeMatch is one choice of Match: the pattern and the rule key it goes
// in.
type routeMatch struct {
	Pattern, Key, Hint string
}

// routeMatches lists the patterns that Route a host offers for a host
// (design 0009 D3.12): the host itself, then each parent domain down to
// two labels, or for an IP literal its /32 or /128. The first is the
// default. Nothing broader than a two-label parent of the host is ever
// offered, and the op accepts only an offered pattern.
func routeMatches(host string, ip netip.Addr) []routeMatch {
	if ip.IsValid() {
		p := netip.PrefixFrom(ip, ip.BitLen())
		return []routeMatch{{Pattern: p.String(), Key: config.PatternCIDRs, Hint: "This address only"}}
	}
	out := []routeMatch{{Pattern: host, Key: config.PatternHosts, Hint: "This host only"}}
	labels := strings.Split(host, ".")
	for i := 1; i <= len(labels)-2; i++ {
		d := strings.Join(labels[i:], ".")
		out = append(out, routeMatch{Pattern: d, Key: config.PatternDomains, Hint: "And all of its subdomains"})
	}
	return out
}

// routeActions are the routes a host can take: direct, each upstream of
// the file, and block.
func routeActions(d *config.Doc) []string {
	out := []string{"direct"}
	for _, u := range d.Upstreams {
		out = append(out, "upstream:"+u.Name)
	}
	return append(out, "block")
}

var ruleNameChars = regexp.MustCompile(`[^a-z0-9._-]+`)

// routeRuleName names the new rule after its pattern, as the rule editor
// suggests a name from the first pattern, unused in d.
func routeRuleName(d *config.Doc, pattern string) string {
	base := strings.TrimLeft(ruleNameChars.ReplaceAllString(pattern, "-"), "-._")
	base = strings.TrimRight(base[:min(len(base), 56)], "-._")
	if !config.ValidName(config.KindRule, base) {
		base = strings.TrimRight(("route-" + base)[:min(len(base)+6, 56)], "-._")
	}
	if !config.ValidName(config.KindRule, base) {
		base = "route"
	}
	return d.FreeRuleName(base)
}

// errRouteHost refuses a posted host that is not a host name or IP
// literal; only a forged form sends one.
var errRouteHost = errors.New("the host is not a host name or IP literal")

// editRouteHost applies Route a host to d (design 0009 D3.12, D7): a new
// rule first in the chain with the chosen pattern and route, or the
// pattern added to the chosen rule, which must still have the route and
// no ports; otherwise config.ErrRuleChanged, or config.ErrNotFound for a
// rule that no longer exists, answers 409.
func editRouteHost(d *config.Doc, f *form) error {
	host, ip, ok := routeHost(f.Value("host"))
	if !ok {
		return errRouteHost
	}
	var m routeMatch
	for _, c := range routeMatches(host, ip) {
		if c.Pattern == f.Value("match") {
			m = c
		}
	}
	if m.Pattern == "" {
		f.Error("match", 0, "choose one of the offered matches")
	}
	action := f.Value("action")
	switch {
	case action == "":
		// Back to editing renders an unfinished form quietly.
		if f.intent == intentReview {
			f.Error("action", 0, "choose where requests for this host go")
		}
	case !validAction(action):
		f.Error("action", 0, "choose one of the routes")
	}
	where := f.Value("where")
	var target string
	if where != "" {
		was, name, ok := strings.Cut(where, " ")
		switch {
		case !ok || !validAction(was) || !config.ValidName(config.KindRule, name):
			f.Error("where", 0, "choose a new rule or one of the offered rules")
		case action != "" && was != action:
			f.Error("where", 0, fmt.Sprintf("rule %q routes %s, not %s; choose a new rule or a rule that routes %s",
				name, routeWords(was), routeWords(action), routeWords(action)))
		default:
			target = name
		}
	}
	if f.parseErrs > 0 || action == "" {
		return nil
	}
	if target == "" {
		r := config.DocRule{Name: routeRuleName(d, m.Pattern), Action: action}
		switch m.Key {
		case config.PatternHosts:
			r.Hosts = config.Some([]string{m.Pattern})
		case config.PatternDomains:
			r.Domains = config.Some([]string{m.Pattern})
		case config.PatternCIDRs:
			r.CIDRs = config.Some([]string{m.Pattern})
		}
		f.entity = 1
		return d.InsertRule(r, 0)
	}
	if err := d.AddPattern(target, action, m.Key, m.Pattern); err != nil {
		return err
	}
	_, i := d.Rule(target)
	f.entity = i + 1
	return nil
}

// validAction reports whether an action is direct, block, or
// upstream:<name> with a valid upstream name; whether the upstream exists
// is for the validator to say.
func validAction(a string) bool {
	if name, ok := strings.CutPrefix(a, "upstream:"); ok {
		return config.ValidName(config.KindUpstream, name)
	}
	return a == "direct" || a == "block"
}

// routeWords says a route in a sentence: "direct", "through gateway", or
// "to block".
func routeWords(action string) string {
	if name, ok := strings.CutPrefix(action, "upstream:"); ok {
		return "through " + name
	}
	if action == "block" {
		return "to block"
	}
	return action
}

// addRouteOp registers the route.host op.
func (s *Server) addRouteOp() {
	s.addOp(&formOp{
		Name: opRouteHost, Kind: config.KindRule, Page: "route", Nav: "rules", PageTitle: "Route a host",
		Fields: routeFields,
		Render: func(*config.Doc, string) (map[string]string, bool) { return map[string]string{}, true },
		Edit:   editRouteHost,
		Title: func(f *form) []seg {
			pat := f.Value("match")
			if was, name, ok := strings.Cut(f.Value("where"), " "); ok && was != "" {
				return []seg{{Text: "add "}, {Text: pat, Code: true}, {Text: " to rule "}, {Text: name, Code: true}}
			}
			switch a := f.Value("action"); {
			case a == "block":
				return []seg{{Text: "block "}, {Text: pat, Code: true}}
			case strings.HasPrefix(a, "upstream:"):
				return []seg{{Text: "route "}, {Text: pat, Code: true}, {Text: " through "}, {Text: strings.TrimPrefix(a, "upstream:"), Code: true}}
			}
			return []seg{{Text: "route "}, {Text: pat, Code: true}, {Text: " direct"}}
		},
		View:    func(d *config.Doc, f *form) any { return s.routeHostView(f) },
		Notices: s.routeNotices,
	})
}

// routeNotices says how the candidate routes the host for a local
// client, and warns when a rule before the edited one still decides it,
// as for a pattern added to a rule below the one that matches now.
func (s *Server) routeNotices(d *draft) []reviewNotice {
	host, _, ok := routeHost(d.form.Value("host"))
	if !ok || d.candCfg == nil || d.entity < 1 {
		return nil
	}
	target, _ := testInput(host)
	e, err := config.Explain(d.candCfg, target, policy.SourceLocal)
	if err != nil {
		return nil
	}
	route := e.Route().String()
	head := []seg{{Text: "After this change, requests for "}, {Text: host, Code: true}, {Text: " from a local client take "}}
	if e.RuleIndex > 0 {
		head = append(head, seg{Text: "rule " + strconv.Itoa(e.RuleIndex) + ", "}, seg{Text: e.Rule, Code: true})
	} else {
		head = append(head, rich("the default route, `proxy.default`")...)
	}
	head = append(head, seg{Text: ": "}, seg{Text: route, Code: true}, seg{Text: "."})
	if e.RuleIndex != d.entity || route != d.form.Value("action") {
		head = append(head, seg{Text: " That rule matches before the one you changed, so this change does not route the host " +
			routeWords(d.form.Value("action")) + "."})
		return []reviewNotice{{Kind: "warn", Text: head}}
	}
	return []reviewNotice{{Kind: "info", Text: head}}
}

// routeHostView is the data of Route a host.
type routeHostView struct {
	ReadOnly bool
	// Fragment renders the form for the dialog of a traffic page.
	Fragment bool
	Host     string
	Matches  []routeChoice
	Routes   []routeChoice
	// Where lists, by route, the rules that "Add to rule" offers: those
	// with no ports; the script shows only the chosen route's.
	Where []whereGroup
	// Current is how the running configuration routes the host now.
	Current *testView
	// Cancel is the page Route… started on.
	Cancel string
}

type routeChoice struct {
	Value, Label, Hint, Class string
	Checked, Disabled         bool
}

type whereGroup struct {
	Label   string
	Options []option
	Action  string
}

// routeHostView builds the form's data from the file as it is and the
// running configuration's decision for the host.
func (s *Server) routeHostView(f *form) *routeHostView {
	st := s.confState()
	host, ip, _ := routeHost(f.Value("host"))
	v := &routeHostView{ReadOnly: st.readOnly, Host: host, Cancel: f.Return}
	if cfg := s.backend.Config(); cfg != nil && host != "" {
		v.Current = s.explainView(cfg, host, false, s.backend.Status())
	}
	current := ""
	if v.Current != nil && v.Current.Err == "" {
		current = v.Current.Route.Action
		if current == "upstream" {
			current += ":" + v.Current.Route.Upstream
		}
	}
	for _, m := range routeMatches(host, ip) {
		v.Matches = append(v.Matches, routeChoice{Value: m.Pattern, Label: m.Pattern, Hint: m.Hint, Checked: f.Value("match") == m.Pattern})
	}
	for _, a := range routeActions(st.doc) {
		rv := routeOf(a)
		c := routeChoice{Value: a, Class: rv.Action, Checked: f.Value("action") == a, Disabled: a == current}
		switch rv.Action {
		case "direct":
			c.Label = "Direct"
		case "block":
			c.Label = "Block"
		default:
			c.Label = rv.Upstream
		}
		v.Routes = append(v.Routes, c)
	}
	for _, a := range routeActions(st.doc) {
		g := whereGroup{Action: a, Label: "Rules that route " + routeWords(a)}
		for _, name := range st.doc.RouteRules(a) {
			val := a + " " + name
			g.Options = append(g.Options, option{Value: val, Label: "Add to rule " + name, Selected: f.Value("where") == val})
		}
		if len(g.Options) > 0 {
			v.Where = append(v.Where, g)
		}
	}
	return v
}

// routeHref is the link of Route… for a host on a traffic page, which
// returns to ret after apply; "" for a host that cannot be routed, such
// as the folded row *.
func routeHref(host, ret string) string {
	h, _, ok := routeHost(host)
	if !ok || host == "*" {
		return ""
	}
	q := url.Values{"host": {h}}
	if r, ok := cleanReturn(ret); ok {
		q.Set("return", r)
	}
	return "/rules/new?" + q.Encode()
}

// getRouteHost serves /rules/new?host=: the form of Route a host,
// prefilled with the host and its exact match, or the dialog's block for
// a script request (design 0009 D3.12). ok is false when the host is not
// a host name or IP literal; the caller then shows the empty new rule
// form, which the prefill fills nothing of.
func (s *Server) getRouteHost(w http.ResponseWriter, r *http.Request, v session) bool {
	q := r.URL.Query()
	host, ip, ok := routeHost(q.Get("host"))
	if !ok {
		return false
	}
	ret, ok := cleanReturn(q.Get("return"))
	if !ok {
		ret = "/rules"
	}
	st := s.confState()
	op := s.ops[opRouteHost]
	vals := map[string]string{"host": host, "match": routeMatches(host, ip)[0].Pattern, "action": "", "where": ""}
	f := newFormFrom(op, vals, "", st.file.Digest, ret)
	view := s.routeHostView(f)
	page := pageData{Title: "Route " + host, Token: v.csrf, Page: "rules", Data: &formPage{Form: f, View: view}}
	if wantsFragment(r) {
		view.Fragment = true
		s.renderFragment(w, http.StatusOK, "route", "route-editor", page)
		return true
	}
	s.render(w, http.StatusOK, "route", page)
	return true
}

// trafficRet returns the request's own page as the return path of the
// Route… links on it.
func trafficRet(r *http.Request) string {
	ret, ok := cleanReturn(r.URL.RequestURI())
	if !ok {
		return r.URL.Path
	}
	return ret
}

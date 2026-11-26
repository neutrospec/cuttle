package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/neutrospec/cuttle/internal/config"
	"github.com/neutrospec/cuttle/internal/policy"
	"github.com/neutrospec/cuttle/internal/stats"
)

// The rules pages of design 0009 D3.3 and D3.4: the chain with Test a
// URL, the filter, reordering, and the row menu, and the rule editor with
// its test panel and traffic. Each edit is a form op of D7.

// Ops of the rules pages.
const (
	opRuleSave   = "rule.save"
	opRuleDelete = "rule.delete"
	opRulesOrder = "rules.order"
)

// ruleFormFields are the fields of the rule editor (design 0009 D4): one
// per key of [[rule]], the action as a choice whose options carry the
// upstream and the direct flags, the interface list as a choice, and the
// position of a new rule.
var ruleFormFields = []formField{
	{Name: "name", Key: "name", Label: "Name", Kind: fieldText},
	{Name: "note", Key: "note", Label: "Note", Kind: fieldText},
	{Name: "domains", Key: "domains", Label: "Domains", Kind: fieldList},
	{Name: "hosts", Key: "hosts", Label: "Hosts", Kind: fieldList},
	{Name: "cidrs", Key: "cidrs", Label: "CIDRs", Kind: fieldList},
	{Name: "ports", Key: "ports", Label: "Ports", Kind: fieldPorts},
	{Name: "action", Key: "action", Label: "Route", Kind: fieldChoice, Options: []choiceOption{
		{Value: "direct", Fields: []string{"allow_loopback", "allow_private"}},
		{Value: "upstream", Fields: []string{"upstream"}},
		{Value: "block"},
	}},
	{Name: "upstream", Key: "action", Label: "Upstream", Kind: fieldText, Under: "action"},
	{Name: "allow_loopback", Key: "allow_loopback", Label: "Allow loopback", Kind: fieldCheck, Under: "action"},
	{Name: "allow_private", Key: "allow_private", Label: "Allow private", Kind: fieldCheck, Under: "action"},
	{Name: "ifaces", Key: "interfaces", Label: "Interfaces", Kind: fieldChoice, Options: []choiceOption{
		{Value: "default"},
		{Value: "only", Fields: []string{"ifaces_list"}},
	}},
	{Name: "ifaces_list", Key: "interfaces", Label: "Interfaces", Kind: fieldList, Under: "ifaces"},
	{Name: "position", Label: "Position", Kind: fieldText},
}

// orderFields are the fields of the rule order: the names in the order
// the page shows them, and, without the script, a position for each.
var orderFields = []formField{
	{Name: "order", Key: "rule order", Label: "Order", Kind: fieldList},
	{Name: "position", Key: "rule order", Label: "Position", Kind: fieldList},
}

func optBool(o config.Opt[bool]) string {
	if o.Set && o.V {
		return "true"
	}
	return "false"
}

func optInts(o config.Opt[[]int64]) string {
	out := make([]string, len(o.V))
	for i, n := range o.V {
		out[i] = strconv.FormatInt(n, 10)
	}
	return strings.Join(out, "\n")
}

// ruleValues returns the rendered values of a rule's fields.
func ruleValues(r config.DocRule) map[string]string {
	route := routeOf(r.Action)
	ifaces := "default"
	if r.Interfaces.Set {
		ifaces = "only"
	}
	return map[string]string{
		"name": r.Name, "note": r.Note.V, "domains": strings.Join(r.Domains.V, "\n"), "hosts": strings.Join(r.Hosts.V, "\n"),
		"cidrs": strings.Join(r.CIDRs.V, "\n"), "ports": optInts(r.Ports), "action": route.Action, "upstream": route.Upstream,
		"allow_loopback": optBool(r.AllowLoopback), "allow_private": optBool(r.AllowPrivate),
		"ifaces": ifaces, "ifaces_list": strings.Join(r.Interfaces.V, "\n"), "position": "",
	}
}

func renderRuleForm(d *config.Doc, target string) (map[string]string, bool) {
	r, i := d.Rule(target)
	if i < 0 {
		return nil, false
	}
	return ruleValues(r), true
}

// editRuleForm applies the rule form to r, the rule as the file has it
// (design 0009 D4). A changed action sets the keys its option controls
// from that option alone, so a rule that stops being direct loses its
// direct flags and interface list, which apply only to direct rules.
func editRuleForm(r config.DocRule, f *form, d *config.Doc) config.DocRule {
	r.Name = f.Required("name", r.Name)
	if r.Name == "" {
		if f.intent == intentReview {
			f.Error("name", 0, "required")
		} else {
			// A check or test of an unfinished form runs under a name
			// that no rule has, so that the other fields are checked.
			r.Name = d.FreeRuleName("unnamed")
		}
	}
	r.Note = f.Text("note", r.Note)
	r.Domains = f.List("domains", r.Domains)
	r.Hosts = f.List("hosts", r.Hosts)
	r.CIDRs = f.List("cidrs", r.CIDRs)
	r.Ports = f.Ports("ports", r.Ports)
	if opt, changed := f.Choice("action"); changed {
		r.AllowLoopback, r.AllowPrivate = config.Opt[bool]{}, config.Opt[bool]{}
		switch opt {
		case "direct":
			r.Action = "direct"
			if f.Checked("allow_loopback") {
				r.AllowLoopback = config.Some(true)
			}
			if f.Checked("allow_private") {
				r.AllowPrivate = config.Some(true)
			}
		case "upstream":
			r.Action = "upstream:" + strings.TrimSpace(f.Value("upstream"))
		case "block":
			r.Action = "block"
		}
	}
	if r.Action != "direct" {
		r.Interfaces = config.Opt[[]string]{}
	} else if opt, changed := f.Choice("ifaces"); changed {
		switch opt {
		case "default":
			r.Interfaces = config.Opt[[]string]{}
		case "only":
			// An empty list stays empty, so the validator says so.
			l := splitList(f.Value("ifaces_list"))
			if l == nil {
				l = []string{}
			}
			r.Interfaces = config.Some(l)
		}
	}
	return r
}

// rulePosition returns where a new rule goes: first, last, or before a
// rule (design 0009 D3.4).
func rulePosition(d *config.Doc, f *form) int {
	switch p := f.Value("position"); {
	case p == "" || p == "first":
		return 0
	case p == "last":
		return len(d.Rules)
	case strings.HasPrefix(p, "before:"):
		name := strings.TrimPrefix(p, "before:")
		if _, i := d.Rule(name); i >= 0 {
			return i
		}
		f.Error("position", 0, "rule "+strconv.Quote(name)+" no longer exists; choose another place")
		return 0
	}
	f.Error("position", 0, "choose first, last, or a rule to go before")
	return 0
}

// errReviewOnly refuses a draft intent other than review of a delete.
var errReviewOnly = errors.New("a delete can only be reviewed")

// addRuleOps registers the ops of the rules pages.
func (s *Server) addRuleOps() {
	s.addOp(&formOp{
		Name: opRuleSave, Kind: config.KindRule, Page: "rule", Nav: "rules", PageTitle: "Rule",
		Fields: ruleFormFields,
		Render: renderRuleForm,
		Edit: func(d *config.Doc, f *form) error {
			if f.New {
				at := rulePosition(d, f)
				if err := d.InsertRule(editRuleForm(config.DocRule{}, f, d), at); err != nil {
					return err
				}
				f.entity = at + 1
				return nil
			}
			cur, i := d.Rule(f.Target)
			if i < 0 {
				return fmt.Errorf("rule %q %w", f.Target, config.ErrNotFound)
			}
			f.entity = i + 1
			return d.ReplaceRule(f.Target, editRuleForm(cur, f, d))
		},
		Title: func(f *form) []seg {
			if !f.New {
				return []seg{{Text: "change rule "}, {Text: f.Target, Code: true}}
			}
			if n := f.Value("name"); n != "" {
				return []seg{{Text: "add rule "}, {Text: n, Code: true}}
			}
			return plain("add a rule")
		},
		Hint: func(f *form) config.Hint {
			if f.New {
				return config.Hint{}
			}
			return config.Hint{Kind: config.KindRule, From: f.Target, To: f.Value("name")}
		},
		Entity: func(f *form) string { return f.Value("name") },
		View: func(d *config.Doc, f *form) any {
			return s.ruleEditView(context.Background(), d, f)
		},
		Test: func(s *Server, w http.ResponseWriter, r *http.Request, v session, d *draft) {
			view := s.ruleEditView(r.Context(), d.doc, d.form)
			view.TestInput, view.Remote = r.PostForm.Get("test"), r.PostForm.Get("source") == "remote"
			view.Test = s.ruleTestView(d, view.TestInput, view.Remote)
			page := pageData{Title: "Rule", Token: v.csrf, Page: "rules", Data: &formPage{Form: d.form, View: view}}
			if wantsFragment(r) {
				s.renderFragment(w, http.StatusOK, "rule", "rule-test", page)
				return
			}
			s.render(w, http.StatusOK, "rule", page)
		},
		Notices: func(d *draft) []reviewNotice {
			// Statistics are keyed by rule name (design 0009 D9).
			if f := d.form; !f.New && f.Value("name") != f.Target && f.Value("name") != "" {
				return []reviewNotice{{Kind: "info", Text: []seg{{Text: "Statistics keep traffic by rule name, so rule "},
					{Text: f.Value("name"), Code: true}, {Text: " shows no traffic until requests match it under its new name."}}}}
			}
			return nil
		},
	})
	s.addOp(&formOp{
		Name: opRuleDelete, Kind: config.KindRule, Page: "rule", Nav: "rules", PageTitle: "Rule", Delete: true,
		Fields: ruleFormFields,
		Render: renderRuleForm,
		Edit: func(d *config.Doc, f *form) error {
			if f.intent != intentReview {
				return errReviewOnly
			}
			return d.DeleteRule(f.Target)
		},
		Title:   func(f *form) []seg { return []seg{{Text: "delete rule "}, {Text: f.Target, Code: true}} },
		View:    func(d *config.Doc, f *form) any { return s.ruleEditView(context.Background(), d, f) },
		Notices: s.ruleDeleteNotices,
	})
	s.addOp(&formOp{
		Name: opRulesOrder, Page: "rules", Nav: "rules", PageTitle: "Rules",
		Fields: orderFields,
		Render: func(d *config.Doc, _ string) (map[string]string, bool) {
			names := make([]string, len(d.Rules))
			pos := make([]string, len(d.Rules))
			for i, r := range d.Rules {
				names[i], pos[i] = r.Name, strconv.Itoa(i+1)
			}
			return map[string]string{"order": strings.Join(names, "\n"), "position": strings.Join(pos, "\n")}, true
		},
		Edit:  editOrder,
		Title: func(*form) []seg { return plain("reorder the rules") },
		View: func(d *config.Doc, f *form) any {
			st := s.confState()
			return s.rulesView(context.Background(), st, d, "", nil)
		},
		Test: func(s *Server, w http.ResponseWriter, r *http.Request, v session, d *draft) {
			view := &rulesView{TestInput: r.PostForm.Get("test"), Remote: r.PostForm.Get("source") == "remote"}
			if d.candCfg != nil && strings.TrimSpace(view.TestInput) != "" {
				view.Test = s.explainView(d.candCfg, view.TestInput, view.Remote, s.backend.Status())
			}
			s.renderFragment(w, http.StatusOK, "rules", "rules-test", pageData{Title: "Rules", Token: v.csrf, Page: "rules",
				Data: &formPage{Form: d.form, View: view}})
		},
	})
}

// editOrder reorders the rules as the order form posts them (design 0009
// D3.3): the names in the order of the page, and without the script a
// position for each. A rule lands at the position typed for it: rules
// sort by position, and at a position that an unmoved rule also holds, a
// rule moved up goes before it and a rule moved down after it, so that
// moving the first of four rules to 4 makes it the last. The names must
// still be every rule exactly once.
func editOrder(d *config.Doc, f *form) error {
	names := f.Values("order")
	if pos := f.Values("position"); len(pos) > 0 {
		if len(pos) != len(names) {
			return config.ErrOrder
		}
		type item struct {
			name string
			pos  int
			// rank orders a position's rules: moved up, unmoved, moved
			// down.
			rank int
		}
		items := make([]item, 0, len(names))
		for i, p := range pos {
			n, err := strconv.Atoi(strings.TrimSpace(p))
			if err != nil || n < 1 {
				f.Error("position", 0, "the position of rule "+strconv.Quote(names[i])+" must be a whole number from 1")
				continue
			}
			rank := 1
			switch {
			case n < i+1:
				rank = 0
			case n > i+1:
				rank = 2
			}
			items = append(items, item{names[i], n, rank})
		}
		if f.parseErrs > 0 {
			return nil
		}
		sort.SliceStable(items, func(a, b int) bool {
			if items[a].pos != items[b].pos {
				return items[a].pos < items[b].pos
			}
			return items[a].rank < items[b].rank
		})
		names = names[:0:0]
		for _, it := range items {
			names = append(names, it.name)
		}
	}
	return d.ReorderRules(names)
}

// ruleDeleteNotices adds to a rule's delete review what the delete means
// for traffic (design 0009 D3.3): the rule's requests and bytes in the
// last 24 hours, and the route its first pattern takes after the delete.
func (s *Server) ruleDeleteNotices(d *draft) []reviewNotice {
	r, i := d.file.Doc.Rule(d.form.Target)
	if i < 0 || d.candCfg == nil {
		return nil
	}
	var text []seg
	if t := s.dayTraffic(context.Background()); t.ok {
		tot := t.rules[r.Name]
		reqs := formatNum(tot.Requests) + " requests"
		if tot.Requests == 1 {
			reqs = "1 request"
		}
		text = plain("This rule decided " + reqs + " (" + formatBytes(tot.Bytes()) + ") in the last 24 hours. ")
	}
	if target, pattern := patternTarget(r); target != "" {
		if e, err := config.Explain(d.candCfg, target, policy.SourceLocal); err == nil {
			text = append(text, seg{Text: "After the delete, requests for "}, seg{Text: pattern, Code: true}, seg{Text: " take "})
			if e.RuleIndex > 0 {
				text = append(text, seg{Text: "rule " + strconv.Itoa(e.RuleIndex) + ", "}, seg{Text: e.Rule, Code: true})
			} else {
				text = append(text, rich("the default route, `proxy.default`")...)
			}
			text = append(text, seg{Text: ": "}, seg{Text: e.Route().String(), Code: true}, seg{Text: "."})
		}
	}
	if len(text) == 0 {
		return nil
	}
	return []reviewNotice{{Kind: "info", Text: text}}
}

// patternTarget returns a tunnel target for a rule's first pattern, on
// its first port or 443, and the pattern; "" for a rule with ports only.
func patternTarget(r config.DocRule) (target, pattern string) {
	port := "443"
	if len(r.Ports.V) > 0 {
		port = strconv.FormatInt(r.Ports.V[0], 10)
	}
	switch {
	case len(r.Hosts.V) > 0:
		return r.Hosts.V[0] + ":" + port, r.Hosts.V[0]
	case len(r.Domains.V) > 0:
		return r.Domains.V[0] + ":" + port, r.Domains.V[0]
	case len(r.CIDRs.V) > 0:
		p, err := netip.ParsePrefix(r.CIDRs.V[0])
		if err != nil {
			return "", ""
		}
		return addrPort(p.Addr(), port), r.CIDRs.V[0]
	}
	return "", ""
}

func addrPort(a netip.Addr, port string) string {
	if a.Is6() {
		return "[" + a.String() + "]:" + port
	}
	return a.String() + ":" + port
}

// rulesView is the data of the rules page (design 0009 D3.3).
type rulesView struct {
	ReadOnly bool
	Rules    []ruleRow
	Total    int
	Query    string
	Default  defaultRow
	Traffic  bool
	// TrafficLine says what the traffic columns cover, or why they are
	// missing.
	TrafficLine string
	// Reorder reports that the order can change: the file is valid, no
	// filter is in force, and there are two rules or more.
	Reorder   bool
	Test      *testView
	TestInput string
	Remote    bool
}

// ruleRow is one rule of the chain.
type ruleRow struct {
	Pos, Total      int
	Name, Note      string
	Route           routeView
	Patterns        []string
	More            int
	Flags           []string
	Warnings        []string
	Requests, Bytes int64
	// Hay is the text the filter searches, in lower case.
	Hay string
	// Mark is the row's state after a test: passed, matched, or
	// unreached.
	Mark   string
	Delete *form
	Href   string
}

// defaultRow is the end of the chain: the routes for local and remote
// clients when no rule matched.
type defaultRow struct {
	Local, Remote         routeView
	Requests, Bytes       int64
	Mark                  string
	ProxyHref, RemoteHref string
}

// rulesView builds the rules page for doc, the rules to show, from the
// configuration state st, filtered by query and marked by a test.
func (s *Server) rulesView(ctx context.Context, st confState, doc *config.Doc, query string, tv *testView) *rulesView {
	v := &rulesView{ReadOnly: st.readOnly, Query: strings.TrimSpace(query), Total: len(doc.Rules), Test: tv}
	t := s.dayTraffic(ctx)
	v.Traffic, v.TrafficLine = t.ok, t.line()
	warn := st.warnings(config.KindRule)
	q := strings.ToLower(v.Query)
	delOp := s.ops[opRuleDelete]
	for i, r := range doc.Rules {
		row := ruleRow{Pos: i + 1, Total: len(doc.Rules), Name: r.Name, Note: r.Note.V, Route: routeOf(r.Action),
			Warnings: warn[r.Name], Href: "/rules/" + r.Name}
		pats := slices.Concat(r.Hosts.V, r.Domains.V, r.CIDRs.V)
		row.Patterns = pats[:min(4, len(pats))]
		row.More = len(pats) - len(row.Patterns)
		if len(r.Ports.V) > 0 {
			row.Flags = append(row.Flags, "ports "+strings.ReplaceAll(optInts(r.Ports), "\n", ", "))
		}
		if r.AllowLoopback.V {
			row.Flags = append(row.Flags, "allows loopback")
		}
		if r.AllowPrivate.V {
			row.Flags = append(row.Flags, "allows private")
		}
		if r.Interfaces.Set {
			row.Flags = append(row.Flags, "via "+strings.Join(r.Interfaces.V, ", "))
		}
		row.Hay = strings.ToLower(strings.Join(slices.Concat([]string{r.Name, r.Note.V, r.Action}, pats), " "))
		if q != "" && !strings.Contains(row.Hay, q) {
			continue
		}
		if tot, ok := t.rules[r.Name]; ok {
			row.Requests, row.Bytes = tot.Requests, tot.Bytes()
		}
		if tv != nil && tv.Err == "" {
			switch {
			case tv.Port || (tv.MatchPos > 0 && row.Pos > tv.MatchPos):
				row.Mark = "unreached"
			case row.Pos == tv.MatchPos:
				row.Mark = "matched"
			default:
				row.Mark = "passed"
			}
		}
		if !st.readOnly {
			row.Delete = newFormFrom(delOp, ruleValues(r), r.Name, st.file.Digest, "/rules")
		}
		v.Rules = append(v.Rules, row)
	}
	v.Reorder = !st.readOnly && q == "" && len(doc.Rules) > 1
	v.Default = defaultRow{Local: routeOf(doc.Proxy.Default), Remote: routeView{Action: "block"},
		ProxyHref: pagePath("proxy"), RemoteHref: pagePath("remote")}
	if doc.Remote.Default.Set {
		v.Default.Remote = routeOf(doc.Remote.Default.V)
	}
	for _, name := range []string{policy.RuleDefault, policy.RuleRemoteDefault} {
		if tot, ok := t.rules[name]; ok {
			v.Default.Requests += tot.Requests
			v.Default.Bytes += tot.Bytes()
		}
	}
	if tv != nil && tv.Err == "" {
		v.Default.Mark = "unreached"
		if tv.Default {
			// The node takes the color of the route that the default gave.
			v.Default.Mark = "matched a-" + tv.Route.Action
		}
	}
	return v
}

// getRules serves the rules page (design 0009 D3.3), with ?q= as the
// filter and ?test= and source= for Test a URL; a script request gets
// only the test result.
func (s *Server) getRules(w http.ResponseWriter, r *http.Request, _ string, v session) {
	q := r.URL.Query()
	st := s.confState()
	var fm *form
	if !st.readOnly {
		op := s.ops[opRulesOrder]
		vals, _ := op.Render(st.doc, "")
		fm = newFormFrom(op, vals, "", st.file.Digest, "/rules")
	}
	input, remote := q.Get("test"), q.Get("source") == "remote"
	var tv *testView
	if strings.TrimSpace(input) != "" {
		tv = s.explainView(st.cfg, input, remote, s.backend.Status())
	}
	view := s.rulesView(r.Context(), st, st.doc, q.Get("q"), tv)
	view.TestInput, view.Remote = input, remote
	page := pageData{Title: "Rules", Token: v.csrf, Page: "rules", Data: &formPage{Form: fm, View: view}}
	if wantsFragment(r) {
		s.renderFragment(w, http.StatusOK, "rules", "rules-test", page)
		return
	}
	s.render(w, http.StatusOK, "rules", page)
}

// ruleEditView is the data of the rule editor (design 0009 D3.4).
type ruleEditView struct {
	ReadOnly bool
	New      bool
	// Missing names a rule of the path that does not exist.
	Missing string
	// From names the rule a new rule duplicates.
	From       string
	Pos, Count int
	Upstreams  []upOption
	// Interfaces is the interface list editor with its choice.
	Interfaces     *ifListView
	DefaultList    string
	InterfacesHref string
	UpstreamsHref  string
	Positions      []option
	Test           *testView
	TestInput      string
	Remote         bool
	Traffic        *ruleTraffic
	TrafficLine    string
	Delete         *form
	// Action and Ifaces are the chosen options, so that the page shows
	// only the groups they use.
	Action, Ifaces string
}

type upOption struct {
	Name, Address string
	State         upState
}

// ruleTraffic is a rule's last 24 hours: its totals and top 10 hosts.
type ruleTraffic struct {
	Requests, Bytes int64
	Hosts           []stats.HostRow
	Max             int64
	AllHref         string
	Route           routeView
}

// ruleEditView builds the editor's data for the form f over d, the model
// the form edits.
func (s *Server) ruleEditView(ctx context.Context, d *config.Doc, f *form) *ruleEditView {
	st := s.confState()
	v := &ruleEditView{ReadOnly: st.readOnly, New: f.New, Action: f.Value("action"), Ifaces: f.Value("ifaces"),
		InterfacesHref: pagePath("interfaces"), UpstreamsHref: "/upstreams"}
	cur := st.doc
	status := s.backend.Status()
	states := s.upstreamStates(status)
	for _, u := range cur.Upstreams {
		v.Upstreams = append(v.Upstreams, upOption{Name: u.Name, Address: u.Address, State: states[u.Name]})
	}
	v.DefaultList = "system routing"
	if l := cur.Proxy.Interfaces.V; len(l) > 0 {
		v.DefaultList = strings.Join(l, ", ")
	}
	v.Interfaces = s.ifListChoice(f, cur, "default", "Use the default list ("+v.DefaultList+")",
		"Add at least one interface, or use the default list.")
	if f.New {
		chosen := f.Value("position")
		v.Positions = append(v.Positions, option{Value: "first", Label: "First", Selected: chosen == "" || chosen == "first"})
		for i, r := range cur.Rules {
			if i > 0 {
				val := "before:" + r.Name
				v.Positions = append(v.Positions, option{Value: val, Label: "Before " + r.Name, Selected: chosen == val})
			}
		}
		v.Positions = append(v.Positions, option{Value: "last", Label: "Last", Selected: chosen == "last"})
		return v
	}
	_, i := cur.Rule(f.Target)
	v.Pos, v.Count = i+1, len(cur.Rules)
	if i >= 0 && !st.readOnly {
		r := cur.Rules[i]
		v.Delete = newFormFrom(s.ops[opRuleDelete], ruleValues(r), r.Name, st.file.Digest, "/rules/"+r.Name)
	}
	t := s.dayTraffic(ctx)
	v.TrafficLine = t.line()
	if t.ok && i >= 0 {
		r := cur.Rules[i]
		tot := t.rules[r.Name]
		v.Traffic = &ruleTraffic{Requests: tot.Requests, Bytes: tot.Bytes(), AllHref: domainsHref("rule", r.Name), Route: routeOf(r.Action)}
		if sv := s.backend.Stats(); sv != nil {
			w, _ := stats.PresetWindow("24h", s.now())
			qctx, cancel := context.WithTimeout(ctx, queryTimeout)
			defer cancel()
			hosts, err := sv.Domains(qctx, stats.Filter{Window: w, Rule: r.Name}, topHosts, 0)
			if err != nil {
				s.log.Warn("admin statistics query", "error", err.Error())
			}
			v.Traffic.Hosts = hosts
			for _, h := range hosts {
				v.Traffic.Max = max(v.Traffic.Max, h.Requests)
			}
		}
	}
	return v
}

// getRule serves the rule editor: /rules/new for a new rule, with ?from=
// for a duplicate, and /rules/{name} for a rule of the file (design 0009
// D3.4). A name that does not match the schema or names no rule gets 404.
func (s *Server) getRule(w http.ResponseWriter, r *http.Request, _ string, v session) {
	name := r.PathValue("name")
	op := s.ops[opRuleSave]
	st := s.confState()
	digest := st.file.Digest
	var fm *form
	missing := ""
	from := ""
	switch {
	case name == "new":
		vals := map[string]string{"action": "direct", "ifaces": "default", "position": "first",
			"allow_loopback": "false", "allow_private": "false"}
		if f := r.URL.Query().Get("from"); config.ValidName(config.KindRule, f) {
			if dup, err := st.doc.DuplicateRule(f); err == nil {
				vals, from = ruleValues(dup), f
				vals["position"] = "first"
			}
		}
		fm = newFormFrom(op, vals, "", digest, "/rules")
	case !config.ValidName(config.KindRule, name):
		missing = name
	default:
		vals, ok := op.Render(st.doc, name)
		if !ok {
			missing = name
			break
		}
		fm = newFormFrom(op, vals, name, digest, "/rules/"+name)
	}
	if missing != "" {
		s.render(w, http.StatusNotFound, "rule", pageData{Title: "Rule not found", Token: v.csrf, Page: "rules",
			Data: &formPage{View: &ruleEditView{Missing: missing}}})
		return
	}
	view := s.ruleEditView(r.Context(), st.doc, fm)
	view.From = from
	title := "Add rule"
	if !fm.New {
		title = "Rule " + name
	}
	s.render(w, http.StatusOK, "rule", pageData{Title: title, Token: v.csrf, Page: "rules", Data: &formPage{Form: fm, View: view}})
}

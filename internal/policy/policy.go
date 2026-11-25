// Package policy compiles routing rules and decides the route of each
// request (design 0002 D4). It performs no I/O and never resolves a name.
package policy

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/neutrospec/cuttle/internal/authority"
)

// Kind is the request kind.
type Kind int

const (
	KindConnect Kind = iota
	KindHTTP
)

func (k Kind) String() string {
	if k == KindHTTP {
		return "http"
	}
	return "connect"
}

// SourceClass is the class of the client connection (design 0003 D10).
type SourceClass int

const (
	SourceLocal SourceClass = iota
	SourceRemote
)

func (s SourceClass) String() string {
	if s == SourceRemote {
		return "remote"
	}
	return "local"
}

// Action is what cuttle does with a request.
type Action int

const (
	ActionDirect Action = iota
	ActionBlock
	ActionUpstream
)

func (a Action) String() string {
	switch a {
	case ActionBlock:
		return "block"
	case ActionUpstream:
		return "upstream"
	default:
		return "direct"
	}
}

// Route is an action together with its upstream name.
type Route struct {
	Action   Action
	Upstream string // set when Action is ActionUpstream
}

func (r Route) String() string {
	if r.Action == ActionUpstream {
		return "upstream:" + r.Upstream
	}
	return r.Action.String()
}

// ParseRoute parses "direct", "block", or "upstream:<name>". It does not
// check that the upstream exists.
func ParseRoute(s string) (Route, error) {
	switch {
	case s == "direct":
		return Route{Action: ActionDirect}, nil
	case s == "block":
		return Route{Action: ActionBlock}, nil
	case strings.HasPrefix(s, "upstream:") && len(s) > len("upstream:"):
		return Route{Action: ActionUpstream, Upstream: s[len("upstream:"):]}, nil
	}
	return Route{}, fmt.Errorf("invalid action %q; use direct, block, or upstream:<name>", s)
}

// Rule names reserved for decisions that no rule made.
const (
	RuleDefault       = "default"
	RuleRemoteDefault = "remote-default"
	RulePort          = "port"
	RuleRemoteDirect  = "remote-direct"
)

// Rule is one configured rule in file order.
type Rule struct {
	Name          string
	Hosts         []string // normalized names
	Domains       []string // normalized names
	CIDRs         []netip.Prefix
	Ports         []uint16 // empty means any port
	Route         Route
	AllowLoopback bool
	AllowPrivate  bool
	// Interfaces is the rule's interface list, which replaces the default
	// list on a direct route; nil when the rule has none (design 0008 D6).
	Interfaces []string
}

// Settings is the routing part of a configuration.
type Settings struct {
	Rules             []Rule
	Default           Route
	RemoteDefault     Route
	RemotePorts       []uint16
	RemoteAllowDirect bool
	// Interfaces is proxy.interfaces, the default list of direct routes;
	// empty follows system routing (design 0008 D6).
	Interfaces []string
}

// Decision is the route chosen for one request.
type Decision struct {
	Target authority.Authority
	Kind   Kind
	Source SourceClass
	// Rule is the matching rule's name, or one of RuleDefault,
	// RuleRemoteDefault, RulePort, and RuleRemoteDirect.
	Rule string
	// RuleIndex is the 1-based position of the matching rule, or 0 when no
	// rule decided.
	RuleIndex     int
	Action        Action
	Upstream      string
	AllowLoopback bool
	AllowPrivate  bool
	// Interfaces is the list in force for a direct route: the rule's list,
	// else proxy.interfaces; empty follows system routing (design 0008 D6).
	Interfaces []string
}

// Route returns the decision's action and upstream.
func (d Decision) Route() Route { return Route{Action: d.Action, Upstream: d.Upstream} }

// Policy is an immutable compiled rule set.
type Policy struct {
	s Settings
}

// New compiles settings. The rules must already be validated.
func New(s Settings) *Policy {
	return &Policy{s: s}
}

// Decide chooses the route for a normalized target. It is pure: no I/O and
// no DNS.
func (p *Policy) Decide(t authority.Authority, k Kind, s SourceClass) Decision {
	d := Decision{Target: t, Kind: k, Source: s}
	if s == SourceRemote && !slices.Contains(p.s.RemotePorts, t.Port) {
		d.Rule, d.Action = RulePort, ActionBlock
		return d
	}
	matched := false
	var list []string
	for i := range p.s.Rules {
		r := &p.s.Rules[i]
		if r.matches(t) {
			d.Rule, d.RuleIndex = r.Name, i+1
			d.Action, d.Upstream = r.Route.Action, r.Route.Upstream
			if s == SourceLocal && r.Route.Action == ActionDirect {
				d.AllowLoopback, d.AllowPrivate = r.AllowLoopback, r.AllowPrivate
			}
			list = r.Interfaces
			matched = true
			break
		}
	}
	if !matched {
		def, name := p.s.Default, RuleDefault
		if s == SourceRemote {
			def, name = p.s.RemoteDefault, RuleRemoteDefault
		}
		d.Rule, d.Action, d.Upstream = name, def.Action, def.Upstream
	}
	if s == SourceRemote && d.Action == ActionDirect && !p.s.RemoteAllowDirect {
		d = Decision{Target: t, Kind: k, Source: s, Rule: RuleRemoteDirect, Action: ActionBlock}
	}
	if d.Action == ActionDirect {
		if list == nil {
			list = p.s.Interfaces
		}
		if len(list) > 0 {
			d.Interfaces = list
		}
	}
	return d
}

func (r *Rule) matches(t authority.Authority) bool {
	if len(r.Ports) > 0 && !slices.Contains(r.Ports, t.Port) {
		return false
	}
	if len(r.Hosts) == 0 && len(r.Domains) == 0 && len(r.CIDRs) == 0 {
		return true
	}
	if t.IsIP() {
		for _, c := range r.CIDRs {
			if c.Contains(t.Addr) {
				return true
			}
		}
		return false
	}
	if slices.Contains(r.Hosts, t.Host) {
		return true
	}
	for _, d := range r.Domains {
		if t.Host == d || strings.HasSuffix(t.Host, "."+d) {
			return true
		}
	}
	return false
}

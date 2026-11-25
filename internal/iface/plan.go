package iface

import (
	"net/netip"
	"slices"
	"strings"
)

// PlanState is what a plan needs to know about one listed interface.
type PlanState struct {
	Online   bool
	Reason   string // offline reason
	Families Families
	Index    int
	Demoted  bool
}

// PlanAddr is one allowed destination address. Unbound marks the classes
// that are dialed through system routing: loopback, self, and private
// (design 0008 D7), and for upstreams also every other non-public class.
type PlanAddr struct {
	Addr    netip.AddrPort
	Unbound bool
}

// Group is one step of a dial plan. Group 0 has no interface and is
// dialed unbound; every other group is bound to one interface.
type Group struct {
	Interface string // configured name; "" for group 0
	Index     int    // system interface index of a bound group
	Addrs     []netip.AddrPort
}

// Plan is the dial plan of one request under a list in force (design 0008
// D7).
type Plan struct {
	List   []string
	Groups []Group
	// Passed maps each listed interface that the plan passed over to its
	// reason: an offline reason, or no_family.
	Passed map[string]string
	// Demoted lists the demoted interfaces, which come last.
	Demoted map[string]bool
	// Public reports that the plan held a public address, so its listed
	// interfaces were considered.
	Public bool
}

// BuildPlan builds the plan for addrs, in the resolver's order, under
// list (design 0008 D7). Group 0 holds the unbound addresses. Each online
// interface gets a group with the public addresses of its families, in
// list order, demoted interfaces last in their relative order. A plan
// without a group answers D10.
func BuildPlan(list []string, state func(string) PlanState, addrs []PlanAddr) Plan {
	p := Plan{List: list, Passed: map[string]string{}, Demoted: map[string]bool{}}
	var g0 Group
	var public []netip.AddrPort
	for _, a := range addrs {
		if a.Unbound {
			g0.Addrs = append(g0.Addrs, a.Addr)
		} else {
			public = append(public, a.Addr)
		}
	}
	if len(g0.Addrs) > 0 {
		p.Groups = append(p.Groups, g0)
	}
	if len(public) == 0 {
		return p
	}
	p.Public = true
	var later []Group
	for _, name := range list {
		st := state(name)
		if !st.Online {
			p.Passed[name] = st.Reason
			continue
		}
		g := Group{Interface: name, Index: st.Index}
		for _, a := range public {
			if st.Families.Has(FamilyOf(a.Addr())) {
				g.Addrs = append(g.Addrs, a)
			}
		}
		if len(g.Addrs) == 0 {
			p.Passed[name] = ReasonNoFamily
			continue
		}
		if st.Demoted {
			p.Demoted[name] = true
			later = append(later, g)
			continue
		}
		p.Groups = append(p.Groups, g)
	}
	p.Groups = append(p.Groups, later...)
	return p
}

// Results of a bound group in one dial (design 0008 D15 skipped reasons).
const (
	ResultConnected   = "connected"
	ResultUnreachable = "unreachable" // every attempt ended with a failover error
	ResultTimeout     = "timeout"     // the group's deadline ended it
	ResultRefused     = "refused"     // ECONNREFUSED or ECONNRESET ended the dial
	ResultError       = "error"       // another error ended the dial
)

// Tried is the result of one bound group, in the order the groups ran.
type Tried struct {
	Interface string
	Result    string
}

// Skipped returns the decision log's skipped field (design 0008 D15) for
// a dial of this plan: name:reason pairs in list order, separated by
// commas. tried lists the bound groups that ran. carried names the
// interface that carried the connection, "" when the dial failed or group
// 0 carried it; failed reports a failed dial.
//
// A successful dial lists the interfaces before the carrying one in list
// order, and a failed dial those up to the one whose error ended it, or
// every interface when it ran out. Interfaces that were tried are listed
// wherever they are, since demotion can move them after the carrying one
// in list order. The field is empty when the plan held no public address
// or group 0 carried the connection.
func (p Plan) Skipped(tried []Tried, carried string, failed bool) string {
	if !p.Public || (!failed && carried == "") {
		return ""
	}
	result := map[string]string{}
	ended := ""
	for _, t := range tried {
		result[t.Interface] = t.Result
		if t.Result == ResultRefused || t.Result == ResultError {
			ended = t.Interface
		}
	}
	bound := len(p.List)
	switch {
	case carried != "":
		bound = slices.Index(p.List, carried)
	case ended != "":
		bound = slices.Index(p.List, ended) + 1
	}
	var out []string
	for i, name := range p.List {
		if name == carried {
			continue
		}
		r, ok := result[name]
		switch {
		case ok:
		case i >= bound:
			continue
		case p.Passed[name] != "":
			r = p.Passed[name]
		case p.Demoted[name]:
			r = "demoted"
		default:
			// Never started: the budget ended before its group.
			r = ResultTimeout
		}
		if r == ResultConnected {
			continue
		}
		out = append(out, name+":"+r)
	}
	return strings.Join(out, ",")
}

// Unusable lists every interface of the list with its reason, for the
// details of a D10 answer and its skipped field.
func (p Plan) Unusable() []string {
	out := make([]string, 0, len(p.List))
	for _, name := range p.List {
		out = append(out, name+":"+p.Passed[name])
	}
	return out
}

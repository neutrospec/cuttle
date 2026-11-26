package config

import (
	"errors"
	"fmt"
	"strings"

	"github.com/neutrospec/cuttle/internal/authority"
	"github.com/neutrospec/cuttle/internal/netx"
	"github.com/neutrospec/cuttle/internal/policy"
)

// Explanation is how a configuration routes one target, the decision that
// `cuttle explain` prints and the admin's Test a URL shows (design 0009
// D8). It reads only the configuration: it never resolves a name and never
// connects.
type Explanation struct {
	// Decision is the policy's decision for the target.
	policy.Decision
	// Matched is the 1-based position of the rule that matched the target,
	// also when the decision is remote-direct, which blocks what a rule or
	// the remote default routed direct; 0 when no rule matched or the port
	// check refused the target first.
	Matched int
	// Upstream is the upstream of an upstream route.
	Upstream *Upstream
	// InterfacesFrom names the source of the interface list in force:
	// "rule <name>", "proxy.interfaces", or "upstream <name>"; "" for
	// system routing.
	Interfaces     []string
	InterfacesFrom string
	// Address is the address verdict of a direct route (design 0003 D11);
	// nil for an upstream or a block.
	Address *AddressVerdict
}

// AddressVerdict is the destination address check of a direct route. For
// an IP literal it is the check the dial would make; for a name it says
// which address classes the dial allows after resolution. It does not know
// this host's own addresses (class self), since it reads only the file.
type AddressVerdict struct {
	// Literal reports an IP-literal target, Class its address class, and
	// Allowed whether the dial may connect to it.
	Literal bool
	Class   string
	Allowed bool
	// Classes lists the classes a name may resolve to, for a name.
	Classes []string
}

// ParseTarget reads a target as `cuttle explain` takes it (design 0006
// D3): host:port is a tunnel, an http:// URL a plain-HTTP request, and an
// https:// URL a tunnel to its port, 443 by default.
func ParseTarget(s string) (authority.Authority, policy.Kind, error) {
	lower := strings.ToLower(s)
	switch {
	case strings.HasPrefix(lower, "http://"):
		t, err := authority.ParseHTTP(s)
		if err != nil {
			return authority.Authority{}, 0, err
		}
		return t.Authority, policy.KindHTTP, nil
	case strings.HasPrefix(lower, "https://"):
		rest := s[len("https://"):]
		if i := strings.IndexAny(rest, "/?#"); i >= 0 {
			rest = rest[:i]
		}
		hasPort := strings.Contains(rest, ":")
		if strings.HasPrefix(rest, "[") {
			hasPort = strings.Contains(rest, "]:")
		}
		if !hasPort {
			rest += ":443"
		}
		a, err := authority.ParseConnect(rest)
		return a, policy.KindConnect, err
	case strings.Contains(s, "://"):
		return authority.Authority{}, 0, errors.New("TARGET must be host:port, an http:// URL, or an https:// URL")
	}
	a, err := authority.ParseConnect(s)
	return a, policy.KindConnect, err
}

// Explain decides how cfg routes target for a client of class source, as
// the proxy would, and adds what the decision implies: the upstream's
// address and disclosure, the interface list in force with its source, and
// the address verdict of a direct route (design 0009 D8). A target that
// ParseTarget refuses is an error.
func Explain(cfg *Config, target string, source policy.SourceClass) (Explanation, error) {
	t, kind, err := ParseTarget(target)
	if err != nil {
		return Explanation{}, err
	}
	p := policy.New(cfg.PolicySettings())
	e := Explanation{Decision: p.Decide(t, kind, source)}
	e.Matched = e.RuleIndex
	if e.Rule == policy.RuleRemoteDirect {
		// The rule that routed the target direct before remote-direct
		// blocked it: the chain decides the same for a local client.
		e.Matched = p.Decide(t, kind, policy.SourceLocal).RuleIndex
	}
	switch e.Action {
	case policy.ActionUpstream:
		if u, ok := cfg.Upstream(e.Decision.Upstream); ok {
			e.Upstream = &u
			if len(u.Interfaces) > 0 {
				e.Interfaces, e.InterfacesFrom = u.Interfaces, "upstream "+u.Name
			}
		}
	case policy.ActionDirect:
		if len(e.Decision.Interfaces) > 0 {
			e.Interfaces, e.InterfacesFrom = e.Decision.Interfaces, "proxy.interfaces"
			if e.RuleIndex > 0 && len(cfg.Rules[e.RuleIndex-1].Interfaces) > 0 {
				e.InterfacesFrom = "rule " + e.Rule
			}
		}
		e.Address = addressVerdict(e.Decision)
	}
	return e, nil
}

// addressVerdict applies the destination address table of design 0003 D11
// to a direct decision without resolving anything.
func addressVerdict(d policy.Decision) *AddressVerdict {
	t := d.Target
	if t.IsIP() {
		c := netx.Classify(t.Addr, nil)
		return &AddressVerdict{Literal: true, Class: c.String(), Allowed: netx.Allowed(c, d.Source, true, d)}
	}
	v := &AddressVerdict{}
	for _, c := range []netx.Class{netx.ClassPublic, netx.ClassPrivate, netx.ClassSelf, netx.ClassLoopback} {
		if netx.Allowed(c, d.Source, false, d) {
			v.Classes = append(v.Classes, c.String())
		}
	}
	return v
}

// RuleText is the rule line of `cuttle explain`: the matching rule with
// its position, or the decision that no rule made with its reason.
func (e Explanation) RuleText() string {
	switch e.Rule {
	case policy.RuleDefault:
		return e.Rule + " (proxy.default)"
	case policy.RuleRemoteDefault:
		return e.Rule + " (proxy.remote.default)"
	case policy.RulePort:
		return e.Rule + fmt.Sprintf(" (port %d is not in proxy.remote.ports)", e.Target.Port)
	case policy.RuleRemoteDirect:
		return e.Rule + " (direct needs proxy.remote.allow_direct)"
	}
	return e.Rule + fmt.Sprintf(" (#%d)", e.RuleIndex)
}

// ActionText is the action line: the route, with the upstream's address
// and disclosure, the direct flags, or the block status.
func (e Explanation) ActionText() string {
	action := e.Route().String()
	switch e.Action {
	case policy.ActionUpstream:
		if e.Upstream != nil {
			action += fmt.Sprintf(" -> %s (disclosure %s)", e.Upstream.Address, e.Upstream.Disclosure)
		}
	case policy.ActionDirect:
		var flags []string
		if e.AllowLoopback {
			flags = append(flags, "allow_loopback")
		}
		if e.AllowPrivate {
			flags = append(flags, "allow_private")
		}
		if len(flags) > 0 {
			action += " (" + strings.Join(flags, ", ") + ")"
		}
	case policy.ActionBlock:
		action += " (403)"
	}
	return action
}

// InterfacesText is the interfaces line: the list in force and its source,
// or system routing (design 0008 D15).
func (e Explanation) InterfacesText() string {
	if e.InterfacesFrom == "" {
		return "system routing"
	}
	return strings.Join(e.Interfaces, ", ") + " (" + e.InterfacesFrom + "); public addresses only"
}

// AddressText is the address line of a direct route (design 0009 D8): for
// an IP literal, "allowed: <class> address" or "refused: <class> address,
// 502 destination_ip_prohibited"; for a name, the classes its addresses
// may have, checked after resolution. It is "" for other routes.
func (e Explanation) AddressText() string {
	v := e.Address
	switch {
	case v == nil:
		return ""
	case v.Literal && v.Allowed:
		return "allowed: " + v.Class + " address"
	case v.Literal:
		return "refused: " + v.Class + " address, 502 " + netx.ErrProhibited
	}
	return "checked after resolution; allows " + joinAnd(v.Classes) + " addresses"
}

// TargetText is the target line: the normalized target, its kind, and the
// client class.
func (e Explanation) TargetText() string {
	return fmt.Sprintf("%s (%s, %s client)", e.Target, e.Kind, e.Source)
}

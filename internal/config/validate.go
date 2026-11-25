package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/neutrospec/cuttle/internal/authority"
	"github.com/neutrospec/cuttle/internal/iface"
	"github.com/neutrospec/cuttle/internal/netx"
	"github.com/neutrospec/cuttle/internal/policy"
)

var (
	instanceNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	upstreamNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	ruleNameRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	// Interface definitions (design 0008 D2, D6).
	interfaceNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	macRe           = regexp.MustCompile(`^[0-9A-Fa-f]{2}(:[0-9A-Fa-f]{2}){5}$`)
	deviceRe        = regexp.MustCompile(`^[a-z]+[0-9]+$`)
	loopbackDevRe   = regexp.MustCompile(`^lo[0-9]+$`)
)

// interfaceSyntaxHint replaces a TOML syntax message that could quote part
// of a hardware address (design 0008 D6, 0004 D4).
const interfaceSyntaxHint = "TOML syntax: invalid value; put it in double quotes"

var reservedRuleNames = []string{policy.RuleDefault, policy.RuleRemoteDefault, policy.RulePort, policy.RuleRemoteDirect}

// Defaults from design 0004 D3.
var (
	DefaultListen      = netip.MustParseAddrPort("127.0.0.1:3128")
	DefaultAdminListen = netip.MustParseAddrPort("127.0.0.1:3129")
)

const (
	minTimeout = 100 * time.Millisecond
	maxTimeout = 10 * time.Minute
	minHold    = 100 * time.Millisecond
	maxHold    = 5 * time.Minute
)

// Parse decodes and validates a configuration. file names the source in
// messages. The Config is nil when the report has errors.
func Parse(data []byte, file string) (*Config, *Report) {
	r := &Report{}
	var raw map[string]any
	if _, err := toml.Decode(string(data), &raw); err != nil {
		var pe toml.ParseError
		if errors.As(err, &pe) {
			r.Errors = append(r.Errors, syntaxIssue(pe, file))
		} else {
			r.Errors = append(r.Errors, Issue{File: file, Msg: "TOML: " + err.Error()})
		}
		return nil, r
	}
	v := &validator{file: file, idx: indexLines(string(data)), r: r}
	cfg := v.config(raw)
	if len(r.Errors) > 0 {
		return nil, r
	}
	cfg.SHA256 = Digest(data)
	return cfg, r
}

// syntaxIssue reports a TOML syntax error with its position and the
// parser's message. The parser quotes the start of an unquoted value, so
// under an [[interface]] key or an interfaces list, where that value may
// be a MAC, the message is a fixed hint instead (design 0008 D6). Other
// messages quote what the operator wrote, as every other message does:
// the operator is trusted, and a pattern that hid MAC-shaped text would
// also hide names and IPv6 addresses that the settings page must show.
func syntaxIssue(pe toml.ParseError, file string) Issue {
	msg := "TOML syntax: " + pe.Message
	if interfaceKey(pe.LastKey) {
		msg = interfaceSyntaxHint
	}
	return Issue{File: file, Line: pe.Position.Line, Key: pe.LastKey, Msg: msg}
}

// interfaceKey reports a key path of an [[interface]] table or an
// interfaces list.
func interfaceKey(k string) bool {
	return k == "interface" || strings.HasPrefix(k, "interface.") || strings.HasSuffix(k, "interfaces")
}

type validator struct {
	file string
	idx  lineIndex
	r    *Report
	// listenIdx holds, for each accepted proxy listener, its index in
	// proxy.listen as written. It is empty when proxy.listen is absent.
	listenIdx []int
	// upstreamIdx and ruleIdx hold, for each entry of Config.Upstreams and
	// Config.Rules, its index in the array as written, and ruleElems the
	// written indexes of each rule's accepted patterns, so that cross checks
	// name the elements the user wrote even after others were skipped.
	upstreamIdx []int
	ruleIdx     []int
	ruleElems   []patternIdx
	// listed reports, for each upstream and rule, whether it has its own
	// interfaces key; defined holds the names of the interface
	// definitions.
	upstreamListed []bool
	ruleListed     []bool
	defined        map[string]bool
}

// patternIdx holds the written indexes of a rule's accepted hosts,
// domains, and cidrs, parallel to the policy.Rule lists.
type patternIdx struct {
	hosts, domains, cidrs []int
}

// listenPath names the proxy.listen element of accepted listener i.
func (v *validator) listenPath(i int) string {
	if i < len(v.listenIdx) {
		return elem("proxy.listen", v.listenIdx[i])
	}
	return "proxy.listen"
}

func sub(base, key string) string {
	if base == "" {
		return key
	}
	return base + "." + key
}

func elem(base string, i int) string { return base + "[" + strconv.Itoa(i+1) + "]" }

func (v *validator) errf(path, format string, args ...any) {
	v.r.Errors = append(v.r.Errors, Issue{File: v.file, Line: v.idx.line(path), Key: path, Msg: fmt.Sprintf(format, args...)})
}

func (v *validator) warnf(path, format string, args ...any) {
	v.r.Warnings = append(v.r.Warnings, Issue{File: v.file, Line: v.idx.line(path), Key: path, Msg: fmt.Sprintf(format, args...)})
}

func typeName(x any) string {
	switch x.(type) {
	case string:
		return "a string"
	case int64:
		return "an integer"
	case float64:
		return "a float"
	case bool:
		return "a boolean"
	case []any:
		return "an array"
	case []map[string]any:
		return "an array of tables"
	case map[string]any:
		return "a table"
	}
	return fmt.Sprintf("a %T", x)
}

// known reports keys of m that the schema does not define.
func (v *validator) known(m map[string]any, base string, allowed ...string) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int { return v.idx.line(sub(base, a)) - v.idx.line(sub(base, b)) })
	for _, k := range keys {
		if !slices.Contains(allowed, k) {
			v.errf(sub(base, k), "unknown key")
		}
	}
}

func (v *validator) str(m map[string]any, base, key string) (string, bool) {
	x, ok := m[key]
	if !ok {
		return "", false
	}
	s, ok := x.(string)
	if !ok {
		v.errf(sub(base, key), "must be a string, not %s", typeName(x))
	}
	return s, ok
}

func (v *validator) boolean(m map[string]any, base, key string) (bool, bool) {
	x, ok := m[key]
	if !ok {
		return false, false
	}
	b, ok := x.(bool)
	if !ok {
		v.errf(sub(base, key), "must be true or false, not %s", typeName(x))
	}
	return b, ok
}

func (v *validator) integer(m map[string]any, base, key string, lo, hi int64) (int, bool) {
	x, ok := m[key]
	if !ok {
		return 0, false
	}
	n, ok := x.(int64)
	if !ok {
		v.errf(sub(base, key), "must be an integer, not %s", typeName(x))
		return 0, false
	}
	if n < lo || n > hi {
		v.errf(sub(base, key), "%d is out of range %d-%d", n, lo, hi)
		return 0, false
	}
	return int(n), true
}

// duration reads a Go duration string within [lo, hi]; zero is accepted
// when zeroOK.
func (v *validator) duration(m map[string]any, base, key string, def, lo, hi time.Duration, zeroOK bool) time.Duration {
	s, ok := v.str(m, base, key)
	if !ok {
		return def
	}
	path := sub(base, key)
	d, err := time.ParseDuration(s)
	if err != nil {
		v.errf(path, "invalid duration %q; use a Go duration such as \"10s\"", s)
		return def
	}
	if d == 0 && zeroOK {
		return 0
	}
	if d < lo || (hi > 0 && d > hi) {
		if hi > 0 {
			v.errf(path, "%s is out of range %s-%s", s, lo, hi)
		} else {
			v.errf(path, "%s is negative", s)
		}
		return def
	}
	return d
}

func (v *validator) array(m map[string]any, base, key string) ([]any, bool) {
	x, ok := m[key]
	if !ok {
		return nil, false
	}
	a, ok := x.([]any)
	if !ok {
		v.errf(sub(base, key), "must be an array, not %s", typeName(x))
	}
	return a, ok
}

// strElem is one string element of an array with its index in the file, so
// messages name the element the user wrote even after others were skipped.
type strElem struct {
	i int
	s string
}

func (v *validator) strings(m map[string]any, base, key string) ([]strElem, bool) {
	a, ok := v.array(m, base, key)
	if !ok {
		return nil, false
	}
	out := make([]strElem, 0, len(a))
	good := true
	for i, x := range a {
		s, ok := x.(string)
		if !ok {
			v.errf(elem(sub(base, key), i), "must be a string, not %s", typeName(x))
			good = false
			continue
		}
		out = append(out, strElem{i, s})
	}
	return out, good
}

func (v *validator) ports(m map[string]any, base, key string) ([]uint16, bool) {
	a, ok := v.array(m, base, key)
	if !ok {
		return nil, false
	}
	out := make([]uint16, 0, len(a))
	for i, x := range a {
		n, ok := x.(int64)
		if !ok {
			v.errf(elem(sub(base, key), i), "must be an integer port, not %s", typeName(x))
			continue
		}
		if n < 1 || n > 65535 {
			v.errf(elem(sub(base, key), i), "port %d is out of range 1-65535", n)
			continue
		}
		out = append(out, uint16(n))
	}
	return out, true
}

func (v *validator) table(m map[string]any, base, key string) (map[string]any, bool) {
	x, ok := m[key]
	if !ok {
		return nil, false
	}
	t, ok := x.(map[string]any)
	if !ok {
		v.errf(sub(base, key), "must be a table, not %s", typeName(x))
	}
	return t, ok
}

// tableElem is one table of an array of tables with its index in the file.
type tableElem struct {
	i int
	m map[string]any
}

func (v *validator) tables(m map[string]any, key string) []tableElem {
	x, ok := m[key]
	if !ok {
		return nil
	}
	switch t := x.(type) {
	case []map[string]any:
		out := make([]tableElem, len(t))
		for i, mt := range t {
			out[i] = tableElem{i, mt}
		}
		return out
	case []any:
		out := make([]tableElem, 0, len(t))
		for i, e := range t {
			if mt, ok := e.(map[string]any); ok {
				out = append(out, tableElem{i, mt})
			} else {
				v.errf(elem(key, i), "must be a table, not %s", typeName(e))
			}
		}
		return out
	}
	v.errf(key, "must be an array of tables ([[%s]]), not %s", key, typeName(x))
	return nil
}

func (v *validator) route(m map[string]any, base, key string, upstreams map[string]bool) (policy.Route, bool) {
	s, ok := v.str(m, base, key)
	if !ok {
		return policy.Route{}, false
	}
	r, err := policy.ParseRoute(s)
	if err != nil {
		v.errf(sub(base, key), "%v", err)
		return policy.Route{}, false
	}
	if r.Action == policy.ActionUpstream && !upstreams[r.Upstream] {
		v.errf(sub(base, key), "unknown upstream %q", r.Upstream)
		return policy.Route{}, false
	}
	return r, true
}

// prefixElem is one accepted CIDR with its index in the file.
type prefixElem struct {
	i int
	p netip.Prefix
}

func prefixes(list []prefixElem) []netip.Prefix {
	out := make([]netip.Prefix, len(list))
	for k, e := range list {
		out[k] = e.p
	}
	return out
}

func (v *validator) cidrs(m map[string]any, base, key string) []prefixElem {
	list, _ := v.strings(m, base, key)
	out := make([]prefixElem, 0, len(list))
	for _, e := range list {
		i, s := e.i, e.s
		p, err := netip.ParsePrefix(s)
		if err != nil {
			if a, aerr := netip.ParseAddr(s); aerr == nil {
				a = a.Unmap() // suggest the form that can match
				bits := 32
				if a.Is6() {
					bits = 128
				}
				v.errf(elem(sub(base, key), i), "%q is an address, not a CIDR; use %q", s, netip.PrefixFrom(a, bits).String())
			} else {
				v.errf(elem(sub(base, key), i), "invalid CIDR %q", s)
			}
			continue
		}
		if a := p.Masked().Addr(); a.Is4In6() && p.Bits() >= 96 {
			// Peers and targets are unmapped before they are compared, so an
			// IPv4-mapped prefix such as ::ffff:10.8.0.1/128 never matches.
			v.errf(elem(sub(base, key), i), "%q is an IPv4-mapped prefix, which never matches because cuttle compares IPv4 addresses in plain form; use %q",
				s, netip.PrefixFrom(a.Unmap(), p.Bits()-96).String())
			continue
		}
		if p != p.Masked() {
			v.errf(elem(sub(base, key), i), "CIDR %q has host bits set; use %q", s, p.Masked().String())
			continue
		}
		out = append(out, prefixElem{i, p})
	}
	return out
}

func (v *validator) config(raw map[string]any) *Config {
	c := &Config{}
	v.known(raw, "", "name", "proxy", "upstream", "rule", "admin", "stats", "log", "interface")
	v.interfaces(raw, c)

	if name, ok := v.str(raw, "", "name"); ok {
		if instanceNameRe.MatchString(name) {
			c.Name = name
		} else {
			v.errf("name", "%q does not match ^[a-z][a-z0-9-]{0,62}$", name)
		}
	} else if _, present := raw["name"]; !present {
		v.errf("name", "required; for example name = \"cuttle-edge\"")
	}

	// Upstreams first, so actions can refer to them.
	upstreamNames := map[string]bool{}
	for _, te := range v.tables(raw, "upstream") {
		um := te.m
		base := elem("upstream", te.i)
		v.known(um, base, "name", "address", "disclosure", "connect_timeout", "response_timeout", "unreachable_hold", "interfaces")
		u := Upstream{}
		if name, ok := v.str(um, base, "name"); ok {
			switch {
			case !upstreamNameRe.MatchString(name):
				v.errf(sub(base, "name"), "%q does not match ^[a-z][a-z0-9-]{0,31}$", name)
			case upstreamNames[name]:
				v.errf(sub(base, "name"), "duplicate upstream name %q", name)
			default:
				u.Name = name
				upstreamNames[name] = true
			}
		} else if _, present := um["name"]; !present {
			v.errf(sub(base, "name"), "required")
		}
		if addr, ok := v.str(um, base, "address"); ok {
			if a, err := authority.ParseConnect(addr); err != nil {
				v.errf(sub(base, "address"), "%q is not host:port (%s)", addr, err.(*authority.Error).Reason)
			} else {
				u.Address = a.String()
			}
		} else if _, present := um["address"]; !present {
			v.errf(sub(base, "address"), "required")
		}
		if d, ok := v.str(um, base, "disclosure"); ok {
			if d == DisclosureVia || d == DisclosureNone {
				u.Disclosure = d
			} else {
				v.errf(sub(base, "disclosure"), "%q is not \"via\" or \"none\"", d)
			}
		} else if _, present := um["disclosure"]; !present {
			v.errf(sub(base, "disclosure"), "required: \"via\" for another cuttle instance, \"none\" for an egress proxy")
		}
		u.ConnectTimeout = v.duration(um, base, "connect_timeout", 10*time.Second, minTimeout, maxTimeout, false)
		u.ResponseTimeout = v.duration(um, base, "response_timeout", 60*time.Second, minTimeout, maxTimeout, false)
		u.UnreachableHold = v.duration(um, base, "unreachable_hold", 5*time.Second, minHold, maxHold, true)
		list, listed := v.interfaceList(um, base, false)
		if listed {
			u.Interfaces = list
		}
		c.Upstreams = append(c.Upstreams, u)
		v.upstreamIdx = append(v.upstreamIdx, te.i)
		v.upstreamListed = append(v.upstreamListed, listed)
	}

	v.proxy(raw, c, upstreamNames)
	v.rules(raw, c, upstreamNames)
	v.admin(raw, c)

	c.Stats = Stats{RetentionDays: 30, MaxHostsPerHour: 1000}
	if sm, ok := v.table(raw, "", "stats"); ok {
		v.known(sm, "stats", "retention_days", "max_hosts_per_hour")
		if n, ok := v.integer(sm, "stats", "retention_days", 1, 90); ok {
			c.Stats.RetentionDays = n
		}
		if n, ok := v.integer(sm, "stats", "max_hosts_per_hour", 100, 100000); ok {
			c.Stats.MaxHostsPerHour = n
		}
	}
	c.Log = Log{Level: "info"}
	if lm, ok := v.table(raw, "", "log"); ok {
		v.known(lm, "log", "level")
		if s, ok := v.str(lm, "log", "level"); ok {
			switch s {
			case "debug", "info", "warn", "error":
				c.Log.Level = s
			default:
				v.errf("log.level", "%q is not debug, info, warn, or error", s)
			}
		}
	}

	v.crossChecks(c)
	return c
}

func (v *validator) proxy(raw map[string]any, c *Config, upstreams map[string]bool) {
	p := &c.Proxy
	p.Listen = []netip.AddrPort{DefaultListen}
	p.ConnectTimeout = 10 * time.Second
	p.ResponseTimeout = 60 * time.Second
	p.IdleTimeout = time.Hour
	p.MaxConnections = 1024
	p.InterfaceAttemptTimeout = 3 * time.Second
	p.InterfaceDemotion = 30 * time.Second
	p.Remote = Remote{Ports: []uint16{80, 443}, Default: policy.Route{Action: policy.ActionBlock}, MaxConnectionsPerSource: 128}

	pm, ok := v.table(raw, "", "proxy")
	if !ok {
		if _, present := raw["proxy"]; !present {
			v.errf("proxy.default", "required; add [proxy] with default = \"direct\", \"block\", or \"upstream:<name>\"")
		}
		return
	}
	v.known(pm, "proxy", "listen", "default", "connect_timeout", "response_timeout", "idle_timeout", "max_connections", "remote",
		"interfaces", "interface_attempt_timeout", "interface_demotion")

	if raw, present := pm["listen"]; present {
		// Check every string element, even when another one has the wrong
		// type, so each message names the element as written.
		list, _ := v.strings(pm, "proxy", "listen")
		p.Listen = nil
		if a, ok := raw.([]any); ok && len(a) == 0 {
			v.errf("proxy.listen", "needs at least one IP:port listener")
		}
		v.listenIdx = nil
		for _, e := range list {
			s := e.s
			path := elem("proxy.listen", e.i)
			ap, err := netip.ParseAddrPort(s)
			switch {
			case err != nil:
				v.errf(path, "%q is not an IP:port literal; host names are not allowed", s)
			case ap.Addr().Zone() != "":
				v.errf(path, "%q has a zone; use an address without one", s)
			case ap.Addr().Unmap().IsUnspecified():
				// Unmapped first: [::ffff:0.0.0.0] is not unspecified to
				// netip, but Go listens on it with a dual-stack wildcard
				// socket, on every IPv4 and IPv6 address.
				v.errf(path, "%q is a wildcard address; listen on explicit addresses only", s)
			case ap.Port() == 0:
				v.errf(path, "%q needs a port 1-65535", s)
			case slices.Contains(p.Listen, unmapAP(ap)):
				v.errf(path, "duplicate listener %q", s)
			default:
				p.Listen = append(p.Listen, unmapAP(ap))
				v.listenIdx = append(v.listenIdx, e.i)
			}
		}
	}
	if r, ok := v.route(pm, "proxy", "default", upstreams); ok {
		p.Default = r
	} else if _, present := pm["default"]; !present {
		v.errf("proxy.default", "required: \"direct\", \"block\", or \"upstream:<name>\"")
	}
	p.ConnectTimeout = v.duration(pm, "proxy", "connect_timeout", p.ConnectTimeout, minTimeout, maxTimeout, false)
	p.ResponseTimeout = v.duration(pm, "proxy", "response_timeout", p.ResponseTimeout, minTimeout, maxTimeout, false)
	p.IdleTimeout = v.duration(pm, "proxy", "idle_timeout", p.IdleTimeout, 0, 0, true)
	if n, ok := v.integer(pm, "proxy", "max_connections", 16, 65536); ok {
		p.MaxConnections = n
	}
	if list, ok := v.interfaceList(pm, "proxy", true); ok {
		p.Interfaces = list
	}
	p.InterfaceAttemptTimeout = v.duration(pm, "proxy", "interface_attempt_timeout", p.InterfaceAttemptTimeout, minTimeout, maxTimeout, false)
	p.InterfaceDemotion = v.duration(pm, "proxy", "interface_demotion", p.InterfaceDemotion, time.Second, maxTimeout, true)

	if rm, ok := v.table(pm, "proxy", "remote"); ok {
		base := "proxy.remote"
		v.known(rm, base, "sources", "ports", "default", "allow_direct", "max_connections_per_source")
		if _, present := rm["sources"]; present {
			sources := v.cidrs(rm, base, "sources")
			p.Remote.Sources = prefixes(sources)
			for _, e := range sources {
				if e.p.Bits() == 0 {
					v.warnf(elem(sub(base, "sources"), e.i), "%s admits every address that can reach a non-loopback listener", e.p)
				}
			}
		}
		if ports, ok := v.ports(rm, base, "ports"); ok {
			p.Remote.Ports = ports
		}
		if r, ok := v.route(rm, base, "default", upstreams); ok {
			p.Remote.Default = r
		}
		if b, ok := v.boolean(rm, base, "allow_direct"); ok {
			p.Remote.AllowDirect = b
		}
		if n, ok := v.integer(rm, base, "max_connections_per_source", 1, 65536); ok {
			p.Remote.MaxConnectionsPerSource = n
		}
		if p.Remote.Default.Action == policy.ActionDirect && !p.Remote.AllowDirect {
			v.errf(sub(base, "default"), "\"direct\" for remote clients needs proxy.remote.allow_direct = true")
		}
	}

	for i, ap := range p.Listen {
		if !ap.Addr().IsLoopback() && len(p.Remote.Sources) == 0 {
			v.errf(v.listenPath(i), "non-loopback listener %s needs proxy.remote.sources", ap)
		}
	}
}

func unmapAP(ap netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
}

func (v *validator) rules(raw map[string]any, c *Config, upstreams map[string]bool) {
	names := map[string]bool{}
	for _, te := range v.tables(raw, "rule") {
		rm := te.m
		base := elem("rule", te.i)
		v.known(rm, base, "name", "hosts", "domains", "cidrs", "ports", "action", "allow_loopback", "allow_private", "interfaces")
		r := policy.Rule{Name: "rule-" + strconv.Itoa(te.i+1)}
		var at patternIdx
		namePath := sub(base, "name")
		if name, ok := v.str(rm, base, "name"); ok {
			switch {
			case !ruleNameRe.MatchString(name):
				v.errf(namePath, "%q does not match ^[a-z0-9][a-z0-9._-]{0,63}$", name)
			case slices.Contains(reservedRuleNames, name):
				v.errf(namePath, "%q is reserved for decisions that no rule made", name)
			default:
				r.Name = name
			}
		}
		if names[r.Name] {
			v.errf(namePath, "duplicate rule name %q", r.Name)
		}
		names[r.Name] = true

		// An explicitly empty list would silently widen the rule, possibly to
		// every destination with allow_loopback or allow_private.
		for _, key := range []string{"hosts", "domains", "cidrs", "ports"} {
			if a, ok := rm[key].([]any); ok && len(a) == 0 {
				v.errf(sub(base, key), "empty list; list at least one entry or remove the key")
			}
		}
		_, hasCIDRs := rm["cidrs"]
		hosts, _ := v.strings(rm, base, "hosts")
		for _, e := range hosts {
			h := e.s
			if err := authority.CheckPattern(h); err != nil {
				v.errf(elem(sub(base, "hosts"), e.i), "%q: %s", h, err.(*authority.Error).Reason)
				continue
			}
			r.Hosts = append(r.Hosts, h)
			at.hosts = append(at.hosts, e.i)
		}
		domains, _ := v.strings(rm, base, "domains")
		for _, e := range domains {
			d := e.s
			if err := authority.CheckPattern(d); err != nil {
				v.errf(elem(sub(base, "domains"), e.i), "%q: %s", d, err.(*authority.Error).Reason)
				continue
			}
			r.Domains = append(r.Domains, d)
			at.domains = append(at.domains, e.i)
		}
		if hasCIDRs {
			for _, e := range v.cidrs(rm, base, "cidrs") {
				r.CIDRs = append(r.CIDRs, e.p)
				at.cidrs = append(at.cidrs, e.i)
			}
		}
		if ports, ok := v.ports(rm, base, "ports"); ok {
			r.Ports = ports
		}
		// Decided on the parsed lists, not on which keys are present.
		if len(r.Hosts) == 0 && len(r.Domains) == 0 && len(r.CIDRs) == 0 && len(r.Ports) == 0 {
			v.errf(base, "rule %q has none of hosts, domains, cidrs, or ports, so it would match every destination; a catch-all belongs in proxy.default", r.Name)
		}
		if rt, ok := v.route(rm, base, "action", upstreams); ok {
			r.Route = rt
		} else if _, present := rm["action"]; !present {
			v.errf(sub(base, "action"), "required: \"direct\", \"block\", or \"upstream:<name>\"")
		}
		for _, flag := range []string{"allow_loopback", "allow_private"} {
			b, ok := v.boolean(rm, base, flag)
			if !ok {
				continue
			}
			if b && r.Route.Action != policy.ActionDirect {
				v.errf(sub(base, flag), "applies only to rules whose action is \"direct\"")
			}
			if flag == "allow_loopback" {
				r.AllowLoopback = b
			} else {
				r.AllowPrivate = b
			}
		}
		list, listed := v.interfaceList(rm, base, false)
		if listed {
			if r.Route.Action != policy.ActionDirect {
				v.errf(sub(base, "interfaces"), "applies only to rules whose action is \"direct\"; an upstream route uses the upstream's interfaces")
			} else {
				r.Interfaces = list
			}
		}
		c.Rules = append(c.Rules, r)
		v.ruleIdx = append(v.ruleIdx, te.i)
		v.ruleElems = append(v.ruleElems, at)
		v.ruleListed = append(v.ruleListed, listed)
	}
}

func (v *validator) admin(raw map[string]any, c *Config) {
	c.Admin.Listen = DefaultAdminListen
	explicit := false
	if am, ok := v.table(raw, "", "admin"); ok {
		v.known(am, "admin", "listen")
		if s, ok := v.str(am, "admin", "listen"); ok {
			explicit = true
			if s == "" {
				c.Admin.Listen = netip.AddrPort{}
				return
			}
			ap, err := netip.ParseAddrPort(s)
			switch {
			case err != nil || ap.Port() == 0:
				v.errf("admin.listen", "%q is not an IP:port literal", s)
				return
			case !ap.Addr().Unmap().IsLoopback():
				v.errf("admin.listen", "%q is not a loopback address; the admin UI listens on loopback only", s)
				return
			}
			c.Admin.Listen = unmapAP(ap)
		}
	}
	// Compare the effective address, the default included.
	for i, l := range c.Proxy.Listen {
		if l != c.Admin.Listen {
			continue
		}
		if explicit {
			v.errf("admin.listen", "%s equals a proxy listener", l)
		} else {
			v.errf(v.listenPath(i), "%s equals the default admin.listen; listen elsewhere or set admin.listen", l)
		}
	}
}

// crossChecks covers errors and warnings that span tables.
func (v *validator) crossChecks(c *Config) {
	// An upstream equal to one of this instance's listeners.
	for i, u := range c.Upstreams {
		if u.Address == "" {
			continue
		}
		a, err := authority.ParseConnect(u.Address)
		if err != nil {
			continue
		}
		for _, l := range c.Proxy.Listen {
			same := a.Port == l.Port() &&
				((a.IsIP() && a.Addr == l.Addr()) || (a.Host == "localhost" && l.Addr().IsLoopback()))
			if same {
				v.errf(sub(elem("upstream", v.upstreamIdx[i]), "address"), "%s is this instance's own listener", u.Address)
			}
		}
	}

	// Patterns that a previous rule already catches on every port this rule
	// names.
	for j := range c.Rules {
		later := &c.Rules[j]
		at := v.ruleElems[j]
		path := func(key string, k int) string { return elem(sub(elem("rule", v.ruleIdx[j]), key), k) }
		for i := 0; i < j; i++ {
			earlier := &c.Rules[i]
			covers := len(earlier.Ports) == 0 ||
				(len(later.Ports) > 0 && !slices.ContainsFunc(later.Ports, func(p uint16) bool { return !slices.Contains(earlier.Ports, p) }))
			if !covers {
				continue
			}
			for k, h := range later.Hosts {
				if slices.Contains(earlier.Hosts, h) {
					v.warnf(path("hosts", at.hosts[k]), "%q repeats rule %q, which matches first, so it can never match", h, earlier.Name)
				}
			}
			for k, d := range later.Domains {
				if slices.Contains(earlier.Domains, d) {
					v.warnf(path("domains", at.domains[k]), "%q repeats rule %q, which matches first, so it can never match", d, earlier.Name)
				}
			}
			for k, p := range later.CIDRs {
				if slices.Contains(earlier.CIDRs, p) {
					v.warnf(path("cidrs", at.cidrs[k]), "%q repeats rule %q, which matches first, so it can never match", p.String(), earlier.Name)
				}
			}
		}
	}

	v.interfaceWarnings(c)

	// Upstreams that no action uses.
	used := map[string]bool{}
	for _, r := range c.Rules {
		used[r.Route.Upstream] = true
	}
	used[c.Proxy.Default.Upstream] = true
	used[c.Proxy.Remote.Default.Upstream] = true
	for i, u := range c.Upstreams {
		if u.Name != "" && !used[u.Name] {
			v.warnf(sub(elem("upstream", v.upstreamIdx[i]), "name"), "upstream %q is not used by any action", u.Name)
		}
	}
}

// interfaces reads the [[interface]] definitions (design 0008 D2, D6). No
// message repeats the value it is about: messages name key paths, lines,
// and the rule broken, so a MAC never reaches a message, the log, or
// last_reload_error.
func (v *validator) interfaces(raw map[string]any, c *Config) {
	v.defined = map[string]bool{}
	names := map[string]int{}
	macs := map[string]int{}
	devices := map[string]int{}
	for _, te := range v.tables(raw, "interface") {
		im := te.m
		base := elem("interface", te.i)
		v.known(im, base, "name", "mac", "device", "kind")
		d := Interface{}
		if name, ok := v.str(im, base, "name"); ok {
			path := sub(base, "name")
			switch {
			case !interfaceNameRe.MatchString(name):
				v.errf(path, "does not match ^[a-z][a-z0-9-]{0,31}$")
			case name == "other":
				v.errf(path, "is reserved for traffic of no defined interface")
			default:
				if i, dup := names[name]; dup {
					v.errf(path, "duplicate of %s", sub(elem("interface", i), "name"))
				} else {
					names[name] = te.i
					d.Name = name
					v.defined[name] = true
				}
			}
		} else if _, present := im["name"]; !present {
			v.errf(sub(base, "name"), "required")
		}
		_, hasMAC := im["mac"]
		_, hasDevice := im["device"]
		if s, ok := v.str(im, base, "mac"); ok {
			path := sub(base, "mac")
			if !macRe.MatchString(s) {
				v.errf(path, "not a hardware address; use six colon-separated pairs of hex digits as cuttle interfaces prints them")
			} else {
				mac, _ := net.ParseMAC(s)
				switch {
				case slices.Equal(mac, make(net.HardwareAddr, 6)):
					v.errf(path, "is all zero, which no interface has")
				case mac[0]&1 != 0:
					v.errf(path, "is a group address, which no interface has")
				default:
					key := mac.String()
					if i, dup := macs[key]; dup {
						v.errf(path, "duplicate of %s", sub(elem("interface", i), "mac"))
					} else {
						macs[key] = te.i
						d.MAC = mac
					}
				}
			}
		}
		if s, ok := v.str(im, base, "device"); ok {
			path := sub(base, "device")
			switch {
			case !deviceRe.MatchString(s) || len(s) > 15:
				v.errf(path, "not a BSD name such as en0")
			case loopbackDevRe.MatchString(s):
				v.errf(path, "names a loopback interface, which never carries traffic out")
			default:
				if i, dup := devices[s]; dup {
					v.errf(path, "duplicate of %s", sub(elem("interface", i), "device"))
				} else {
					devices[s] = te.i
					d.Device = s
				}
			}
		}
		if !hasMAC && !hasDevice {
			v.errf(base, "needs mac, device, or both")
		}
		if s, ok := v.str(im, base, "kind"); ok {
			if _, ok := iface.ParseKind(s); ok {
				d.Kind = s
			} else {
				v.errf(sub(base, "kind"), "must be wired, wifi, or cellular")
			}
		}
		if d.Device != "" && !hasMAC && d.Kind == "" {
			v.warnf(sub(base, "device"), "with neither mac nor kind, any interface that takes this BSD name matches; add mac or kind")
		}
		c.Interfaces = append(c.Interfaces, d)
	}
}

// interfaceList reads an interfaces list. It reports whether the key is
// present. Each element must name a definition, once; an empty list is an
// error unless allowEmpty, because in a rule or an upstream it would read
// as either no interface or the default (design 0008 D6).
func (v *validator) interfaceList(m map[string]any, base string, allowEmpty bool) ([]string, bool) {
	x, present := m["interfaces"]
	if !present {
		return nil, false
	}
	path := sub(base, "interfaces")
	list, _ := v.strings(m, base, "interfaces")
	if a, ok := x.([]any); ok && len(a) == 0 && !allowEmpty {
		v.errf(path, "empty list; list at least one interface, or remove the key")
	}
	out := []string{}
	seen := map[string]int{}
	for _, e := range list {
		ep := elem(path, e.i)
		if i, dup := seen[e.s]; dup {
			v.errf(ep, "repeats %s", elem(path, i))
			continue
		}
		seen[e.s] = e.i
		if !v.defined[e.s] {
			v.errf(ep, "not the name of an [[interface]] definition")
			continue
		}
		out = append(out, e.s)
	}
	return out, true
}

// interfaceWarnings reports the interface warnings of design 0008 D6 that
// span tables.
func (v *validator) interfaceWarnings(c *Config) {
	anyList := len(c.Proxy.Interfaces) > 0
	for j, r := range c.Rules {
		if !v.ruleListed[j] {
			continue
		}
		anyList = true
		base := elem("rule", v.ruleIdx[j])
		allNonPublic := len(r.CIDRs) > 0 && !slices.ContainsFunc(r.CIDRs, func(p netip.Prefix) bool { return !netx.NonPublic(p) })
		if r.AllowLoopback || r.AllowPrivate || allNonPublic {
			v.warnf(sub(base, "interfaces"), "does not apply to loopback and private addresses, which are dialed through system routing")
		}
	}
	for i, u := range c.Upstreams {
		base := elem("upstream", v.upstreamIdx[i])
		if !v.upstreamListed[i] {
			if len(c.Proxy.Interfaces) > 0 {
				v.warnf(base, "has no interfaces, so its dials follow system routing; proxy.interfaces applies only to direct routes")
			}
			continue
		}
		anyList = true
		if a, err := authority.ParseConnect(u.Address); err == nil && a.IsIP() && netx.Classify(a.Addr, nil) != netx.ClassPublic {
			v.warnf(sub(base, "interfaces"), "the address is a loopback or private literal, which is always dialed through system routing")
		}
	}
	if anyList && c.Proxy.InterfaceAttemptTimeout >= c.Proxy.ConnectTimeout {
		v.warnf("proxy.interface_attempt_timeout", "is at or above proxy.connect_timeout, so a timeout never moves to the next interface")
	}
}

func fileOwner(fi os.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

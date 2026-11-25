package policy

import (
	"net/netip"
	"testing"

	"squid/internal/authority"
)

func mustConnect(t *testing.T, s string) authority.Authority {
	t.Helper()
	a, err := authority.ParseConnect(s)
	if err != nil {
		t.Fatalf("ParseConnect(%q): %v", s, err)
	}
	return a
}

// TestPolicyTable checks rule order, hosts, domains label boundaries, cidrs,
// ports, and both defaults (C8), plus the remote steps of D4 that the policy
// decides.
func TestPolicyTable(t *testing.T) {
	gw := Route{Action: ActionUpstream, Upstream: "gateway"}
	p := New(Settings{
		Rules: []Rule{
			{Name: "loopback", Hosts: []string{"localhost"},
				CIDRs:         []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")},
				Route:         Route{Action: ActionDirect},
				AllowLoopback: true},
			{Name: "ads", Domains: []string{"ads.example.net"}, Route: Route{Action: ActionBlock}},
			{Name: "restricted", Domains: []string{"internal.example"}, Route: gw},
			// Shadowed by "restricted" for every port: first match wins.
			{Name: "never", Hosts: []string{"wiki.internal.example"}, Route: Route{Action: ActionBlock}},
			{Name: "exact", Hosts: []string{"exact.example"}, Route: Route{Action: ActionBlock}},
			{Name: "ssh-only", Domains: []string{"ports.example"}, Ports: []uint16{22}, Route: Route{Action: ActionBlock}},
			{Name: "lan", Domains: []string{"lan"}, Route: Route{Action: ActionDirect}, AllowPrivate: true},
			{Name: "net10", CIDRs: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}, Route: gw},
			{Name: "v6doc", CIDRs: []netip.Prefix{netip.MustParsePrefix("2001:db8::/32")}, Route: Route{Action: ActionBlock}},
			{Name: "port-8081", Ports: []uint16{8081}, Route: Route{Action: ActionBlock}},
			{Name: "public-direct", Hosts: []string{"public.example"}, Route: Route{Action: ActionDirect}},
		},
		Default:       Route{Action: ActionDirect},
		RemoteDefault: Route{Action: ActionBlock},
		RemotePorts:   []uint16{80, 443},
	})
	cases := []struct {
		target    string
		src       SourceClass
		rule      string
		index     int
		route     string
		loopback  bool
		private   bool
		nameMatch bool
	}{
		{"localhost:3000", SourceLocal, "loopback", 1, "direct", true, false, true},
		{"127.0.0.1:8443", SourceLocal, "loopback", 1, "direct", true, false, true},
		{"[::1]:22", SourceLocal, "loopback", 1, "direct", true, false, true},
		// Names never match cidrs; IP literals never match hosts or domains.
		{"127.0.0.1.nip.example:80", SourceLocal, "default", 0, "direct", false, false, false},
		{"ads.example.net:443", SourceLocal, "ads", 2, "block", false, false, true},
		{"x.ads.example.net:443", SourceLocal, "ads", 2, "block", false, false, true},
		{"badads.example.net:443", SourceLocal, "default", 0, "direct", false, false, false},
		{"example.net:443", SourceLocal, "default", 0, "direct", false, false, false},
		{"internal.example:443", SourceLocal, "restricted", 3, "upstream:gateway", false, false, true},
		{"wiki.internal.example:443", SourceLocal, "restricted", 3, "upstream:gateway", false, false, true},
		{"a.b.internal.example:8080", SourceLocal, "restricted", 3, "upstream:gateway", false, false, true},
		{"badinternal.example:443", SourceLocal, "default", 0, "direct", false, false, false},
		{"exact.example:443", SourceLocal, "exact", 5, "block", false, false, true},
		{"sub.exact.example:443", SourceLocal, "default", 0, "direct", false, false, false},
		{"ports.example:22", SourceLocal, "ssh-only", 6, "block", false, false, true},
		{"ports.example:443", SourceLocal, "default", 0, "direct", false, false, false},
		{"nas.lan:443", SourceLocal, "lan", 7, "direct", false, true, true},
		{"10.1.2.3:443", SourceLocal, "net10", 8, "upstream:gateway", false, false, true},
		{"11.1.2.3:443", SourceLocal, "default", 0, "direct", false, false, false},
		{"[2001:db8::1]:443", SourceLocal, "v6doc", 9, "block", false, false, true},
		{"anything.example:8081", SourceLocal, "port-8081", 10, "block", false, false, true},
		{"192.0.2.1:8081", SourceLocal, "port-8081", 10, "block", false, false, true},

		// Remote clients: port policy first, then rules, then the remote
		// default; direct needs allow_direct; flags never apply.
		{"wiki.internal.example:22", SourceRemote, "port", 0, "block", false, false, false},
		{"wiki.internal.example:443", SourceRemote, "restricted", 3, "upstream:gateway", false, false, true},
		{"unmatched.example:443", SourceRemote, "remote-default", 0, "block", false, false, false},
		{"public.example:443", SourceRemote, "remote-direct", 0, "block", false, false, false},
		{"localhost:80", SourceRemote, "remote-direct", 0, "block", false, false, false},
		{"ads.example.net:80", SourceRemote, "ads", 2, "block", false, false, true},
	}
	for _, c := range cases {
		d := p.Decide(mustConnect(t, c.target), KindConnect, c.src)
		if d.Rule != c.rule || d.RuleIndex != c.index || d.Route().String() != c.route ||
			d.AllowLoopback != c.loopback || d.AllowPrivate != c.private {
			t.Errorf("%s %s: got rule %q #%d %s loopback=%v private=%v; want %q #%d %s loopback=%v private=%v",
				c.src, c.target, d.Rule, d.RuleIndex, d.Route(), d.AllowLoopback, d.AllowPrivate,
				c.rule, c.index, c.route, c.loopback, c.private)
		}
		if d.Source != c.src || d.Kind != KindConnect || d.Target.String() != mustConnect(t, c.target).String() {
			t.Errorf("%s: decision does not carry its inputs: %+v", c.target, d)
		}
	}

	// The block default for local clients, and remote direct allowed.
	p2 := New(Settings{
		Rules: []Rule{
			{Name: "lan", Domains: []string{"lan"}, Route: Route{Action: ActionDirect}, AllowPrivate: true, AllowLoopback: true},
		},
		Default:           Route{Action: ActionBlock},
		RemoteDefault:     Route{Action: ActionDirect},
		RemotePorts:       []uint16{443},
		RemoteAllowDirect: true,
	})
	for _, c := range []struct {
		target string
		src    SourceClass
		rule   string
		route  string
		flags  bool
	}{
		{"example.com:443", SourceLocal, "default", "block", false},
		{"nas.lan:443", SourceLocal, "lan", "direct", true},
		{"example.com:443", SourceRemote, "remote-default", "direct", false},
		{"nas.lan:443", SourceRemote, "lan", "direct", false},
		{"nas.lan:80", SourceRemote, "port", "block", false},
	} {
		d := p2.Decide(mustConnect(t, c.target), KindHTTP, c.src)
		if d.Rule != c.rule || d.Route().String() != c.route || d.AllowPrivate != c.flags || d.AllowLoopback != c.flags {
			t.Errorf("p2 %s %s: got %q %s flags=%v/%v", c.src, c.target, d.Rule, d.Route(), d.AllowLoopback, d.AllowPrivate)
		}
		if d.Kind != KindHTTP {
			t.Errorf("kind not carried")
		}
	}
}

func TestParseRoute(t *testing.T) {
	for _, c := range []struct {
		in, want string
		ok       bool
	}{
		{"direct", "direct", true},
		{"block", "block", true},
		{"upstream:gateway", "upstream:gateway", true},
		{"upstream:", "", false},
		{"Direct", "", false},
		{"deny", "", false},
		{"", "", false},
	} {
		r, err := ParseRoute(c.in)
		if (err == nil) != c.ok || (err == nil && r.String() != c.want) {
			t.Errorf("ParseRoute(%q) = %v, %v", c.in, r, err)
		}
	}
}

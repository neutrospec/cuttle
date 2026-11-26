package config

import (
	"strings"
	"testing"

	"github.com/neutrospec/cuttle/internal/policy"
)

// explainConfig has a rule of each kind for TestExplain: a direct rule
// over loopback addresses without allow_loopback, an upstream rule, a
// direct rule with its own interface list, and a block rule over an IPv6
// prefix; remote clients may use port 443 only.
const explainConfig = `name = "cuttle-test"

[proxy]
listen = ["127.0.0.1:3128", "192.168.1.2:3128"]
default = "direct"
interfaces = ["eth-a"]

[proxy.remote]
sources = ["192.168.1.0/24"]
ports = [443]

[[interface]]
name = "eth-a"
mac = "00:00:5e:00:53:0a"

[[interface]]
name = "wifi"
device = "en0"
kind = "wifi"

[[upstream]]
name = "gateway"
address = "192.0.2.6:3128"
disclosure = "via"
interfaces = ["wifi"]

[[rule]]
name = "local-net"
cidrs = ["127.0.0.0/8"]
action = "direct"

[[rule]]
name = "restricted"
domains = ["internal.example"]
action = "upstream:gateway"

[[rule]]
name = "video"
domains = ["video.example"]
action = "direct"
interfaces = ["wifi"]

[[rule]]
name = "doc-v6"
cidrs = ["2001:db8::/32"]
action = "block"
`

// TestExplain checks the explanation of design 0009 D8 (W18) for a table
// of targets: the forms a target takes, IP literals of both families, a
// remote client on a closed port and on a direct route without
// allow_direct, a rule's interface list, and the literal-address
// verdicts, which read "refused" with their class.
func TestExplain(t *testing.T) {
	cfg, r := Parse([]byte(explainConfig), "explain.toml")
	if cfg == nil {
		t.Fatalf("invalid sample: %v", r.Errors)
	}
	direct := strings.Replace(explainConfig, "ports = [443]", "ports = [443]\nallow_direct = true\ndefault = \"direct\"", 1)
	remoteDirect, r := Parse([]byte(direct), "direct.toml")
	if remoteDirect == nil {
		t.Fatalf("invalid sample: %v", r.Errors)
	}
	type want struct {
		target, rule, action, ifaces, address string
		matched                               int
	}
	for _, c := range []struct {
		name   string
		cfg    *Config
		target string
		source policy.SourceClass
		want   want
	}{
		{"host:port", cfg, "wiki.internal.example:8443", policy.SourceLocal, want{"wiki.internal.example:8443 (connect, local client)", "restricted (#2)",
			"upstream:gateway -> 192.0.2.6:3128 (disclosure via)", "wifi (upstream gateway); public addresses only", "", 2}},
		{"http URL", cfg, "http://Video.Example/watch?v=1", policy.SourceLocal, want{"video.example:80 (http, local client)", "video (#3)",
			"direct", "wifi (rule video); public addresses only", "checked after resolution; allows public addresses", 3}},
		{"https URL", cfg, "https://example.com/a", policy.SourceLocal, want{"example.com:443 (connect, local client)", "default (proxy.default)",
			"direct", "eth-a (proxy.interfaces); public addresses only", "checked after resolution; allows public addresses", 0}},
		{"https URL with a port", cfg, "https://example.com:8443/", policy.SourceLocal, want{"example.com:8443 (connect, local client)", "default (proxy.default)",
			"direct", "eth-a (proxy.interfaces); public addresses only", "checked after resolution; allows public addresses", 0}},
		{"IPv4 literal", cfg, "198.51.100.7:443", policy.SourceLocal, want{"198.51.100.7:443 (connect, local client)", "default (proxy.default)",
			"direct", "eth-a (proxy.interfaces); public addresses only", "allowed: public address", 0}},
		{"IPv6 literal", cfg, "https://[2001:db8::1]/", policy.SourceLocal, want{"[2001:db8::1]:443 (connect, local client)", "doc-v6 (#4)",
			"block (403)", "system routing", "", 4}},
		{"private literal, local", cfg, "http://10.0.0.1/", policy.SourceLocal, want{"10.0.0.1:80 (http, local client)", "default (proxy.default)",
			"direct", "eth-a (proxy.interfaces); public addresses only", "allowed: private address", 0}},
		{"remote, closed port", cfg, "wiki.internal.example:22", policy.SourceRemote, want{"wiki.internal.example:22 (connect, remote client)",
			"port (port 22 is not in proxy.remote.ports)", "block (403)", "system routing", "", 0}},
		{"remote, direct without allow_direct", cfg, "https://video.example/", policy.SourceRemote, want{"video.example:443 (connect, remote client)",
			"remote-direct (direct needs proxy.remote.allow_direct)", "block (403)", "system routing", "", 3}},
		{"remote, upstream", cfg, "https://wiki.internal.example/", policy.SourceRemote, want{"wiki.internal.example:443 (connect, remote client)",
			"restricted (#2)", "upstream:gateway -> 192.0.2.6:3128 (disclosure via)", "wifi (upstream gateway); public addresses only", "", 2}},
		// The literal-address cases of design 0009 D8.
		{"link-local literal", cfg, "http://169.254.169.254/", policy.SourceLocal, want{"169.254.169.254:80 (http, local client)", "default (proxy.default)",
			"direct", "eth-a (proxy.interfaces); public addresses only", "refused: unusable address, 502 destination_ip_prohibited", 0}},
		{"loopback without allow_loopback", cfg, "127.0.0.1:8080", policy.SourceLocal, want{"127.0.0.1:8080 (connect, local client)", "local-net (#1)",
			"direct", "eth-a (proxy.interfaces); public addresses only", "refused: loopback address, 502 destination_ip_prohibited", 1}},
		{"remote with allow_direct to a private literal", remoteDirect, "10.0.0.1:443", policy.SourceRemote, want{"10.0.0.1:443 (connect, remote client)",
			"remote-default (proxy.remote.default)", "direct", "eth-a (proxy.interfaces); public addresses only",
			"refused: private address, 502 destination_ip_prohibited", 0}},
		{"remote with allow_direct to a name", remoteDirect, "example.com:443", policy.SourceRemote, want{"example.com:443 (connect, remote client)",
			"remote-default (proxy.remote.default)", "direct", "eth-a (proxy.interfaces); public addresses only",
			"checked after resolution; allows public addresses", 0}},
	} {
		e, err := Explain(c.cfg, c.target, c.source)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		got := want{e.TargetText(), e.RuleText(), e.ActionText(), e.InterfacesText(), e.AddressText(), e.Matched}
		if got != c.want {
			t.Errorf("%s:\n got %+v\nwant %+v", c.name, got, c.want)
		}
	}
	// The forms that cuttle explain refuses, a bare host among them.
	for _, bad := range []string{"example.com", "ftp://example.com/", "", "https://", "a b:443", "[::1]"} {
		if _, err := Explain(cfg, bad, policy.SourceLocal); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// TestReservedNewName checks that no rule, upstream, or interface is
// named new, the last part of the admin UI's page that adds one, such as
// /rules/new, so that the page never hides an entity's editor (OI-0006
// AB-R5).
func TestReservedNewName(t *testing.T) {
	for kind, text := range map[string]string{
		"rule":      "name = \"t\"\n[proxy]\ndefault = \"direct\"\n[[rule]]\nname = \"new\"\nhosts = [\"a.example\"]\naction = \"block\"\n",
		"upstream":  "name = \"t\"\n[proxy]\ndefault = \"upstream:new\"\n[[upstream]]\nname = \"new\"\naddress = \"192.0.2.1:3128\"\ndisclosure = \"via\"\n",
		"interface": "name = \"t\"\n[proxy]\ndefault = \"direct\"\n[[interface]]\nname = \"new\"\ndevice = \"en7\"\n",
	} {
		_, r := Parse([]byte(text), "x.toml")
		found := false
		for _, e := range r.Errors {
			if e.Key == kind+"[1].name" && strings.Contains(e.Msg, "is reserved for the admin UI's page that adds one") {
				found = true
			}
		}
		if !found {
			t.Errorf("%s named new: %v", kind, r.Errors)
		}
		if ValidName(kind, "new") {
			t.Errorf("ValidName(%s, new) is true", kind)
		}
		if !ValidName(kind, "news") {
			t.Errorf("ValidName(%s, news) is false", kind)
		}
	}
}

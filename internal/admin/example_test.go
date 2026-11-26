package admin

import (
	"os"
	"time"

	"github.com/neutrospec/cuttle/internal/config"
	"github.com/neutrospec/cuttle/internal/control"
	"github.com/neutrospec/cuttle/internal/stats"
)

// exampleConfig is the configuration of an edge instance as an operator
// might write it, with reserved names and addresses only.
const exampleConfig = `name = "cuttle-edge"

[proxy]
listen = ["127.0.0.1:41728"]
default = "direct"
interfaces = ["eth-a", "wifi"]

[admin]
listen = "127.0.0.1:41729"

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

[[upstream]]
name = "lab"
address = "192.0.2.40:3128"
disclosure = "via"
unreachable_hold = "10s"

[[rule]]
name = "loopback"
hosts = ["localhost"]
cidrs = ["127.0.0.0/8", "::1/128"]
action = "direct"
allow_loopback = true

[[rule]]
name = "restricted"
domains = ["internal.example", "build.example"]
action = "upstream:gateway"

[[rule]]
name = "ads"
domains = ["ads.example.net", "tracker.example", "metrics.example.org", "pixel.example.com"]
action = "block"

[[rule]]
name = "lab-net"
domains = ["lab.example.net"]
cidrs = ["198.51.100.0/24"]
action = "upstream:lab"
`

// withExample fills the fixture with exampleConfig, a running state with
// upstreams, interfaces, and tunnels, and statistics for the last day,
// so that every table and state of the dashboard has rows.
func (f *fixture) withExample() {
	f.t.Helper()
	if err := os.WriteFile(f.be.path, []byte(exampleConfig), 0o600); err != nil {
		f.t.Fatal(err)
	}
	cfg, _, err := config.Load(f.be.path)
	if err != nil {
		f.t.Fatal(err)
	}
	now := f.clock.Now()
	f.be.mu.Lock()
	f.be.cfg, f.be.start = cfg, cfg
	f.be.status.Name, f.be.status.Version = "cuttle-edge", "0.2.0"
	f.be.status.StartedAt = now.Add(-(3*24 + 4) * time.Hour)
	f.be.loadedAt = now.Add(-2*time.Hour - 18*time.Minute)
	f.be.status.Listeners = []control.Listener{{Address: "127.0.0.1:41728", State: "bound", Since: f.be.status.StartedAt}}
	f.be.status.Upstreams = []control.Upstream{
		{Name: "gateway", Address: "192.0.2.6:3128", State: "up", Since: now.Add(-49 * time.Minute)},
		{Name: "lab", Address: "192.0.2.40:3128", State: "down", Since: now.Add(-42 * time.Second), DownS: 42, FailedFast: 17, LastError: "connection_timeout"},
	}
	f.be.status.Interfaces = []control.Interface{
		{Name: "eth-a", Device: "en7", State: "online", Families: []string{"ipv4"}, Primary: []string{"ipv4"}, Since: now.Add(-5 * time.Hour)},
		{Name: "wifi", Device: "en0", State: "online", Families: []string{"ipv4", "ipv6"}, Since: now.Add(-3 * 24 * time.Hour)},
	}
	f.be.status.Tunnels = []control.Tunnel{
		{Target: "video.example:443", Kind: "connect", Action: "direct", Source: "local", Interface: "eth-a", AgeS: 1260, Up: 48 << 10, Down: 812 << 20},
		{Target: "wiki.internal.example:443", Kind: "connect", Action: "upstream", Upstream: "gateway", Source: "local", AgeS: 315, Up: 96 << 10, Down: 3 << 20},
		{Target: "git.internal.example:443", Kind: "connect", Action: "upstream", Upstream: "gateway", Source: "local", AgeS: 74, Up: 1 << 20, Down: 22 << 20},
		{Target: "www.example.com:443", Kind: "connect", Action: "direct", Source: "local", Interface: "wifi", AgeS: 12, Up: 6 << 10, Down: 410 << 10},
		{Target: "chat.example.org:80", Kind: "upgrade", Action: "direct", Source: "local", Interface: "eth-a", AgeS: 3605, Up: 220 << 10, Down: 1 << 20},
	}
	f.be.status.Counters = map[string]int64{
		stats.TunnelsActive: 5, stats.ConnectionsActive: 7, stats.HTTPRequestsActive: 1,
		stats.ConnectionsTotal: 18432, stats.ConnectionsRejectedTotal: 3,
	}
	f.be.status.Log = control.Log{Path: "~/.local/state/cuttle/cuttle.log"}
	f.be.mu.Unlock()

	st := f.withStats()
	key := func(host, action, upstream, rule, iface string) stats.RowKey {
		return stats.RowKey{Host: host, Action: action, Upstream: upstream, Rule: rule, Source: "local", Interface: iface}
	}
	for _, r := range []struct {
		k        stats.RowKey
		n        int
		connect  bool
		failed   bool
		up, down int64
	}{
		{key("video.example", "direct", "", "default", "eth-a"), 402, true, false, 31 << 10, 4 << 20},
		{key("www.example.com", "direct", "", "default", "wifi"), 6120, true, false, 6 << 10, 130 << 10},
		{key("cdn.example.net", "direct", "", "default", "eth-a"), 3310, true, false, 2 << 10, 200 << 10},
		{key("updates.example.com", "direct", "", "default", "eth-a"), 12, true, false, 40 << 10, 50 << 20},
		{key("fonts.example.org", "direct", "", "default", "wifi"), 940, true, false, 1 << 10, 32 << 10},
		{key("localhost", "direct", "", "loopback", ""), 1204, false, false, 2 << 10, 14 << 10},
		{key("wiki.internal.example", "upstream", "gateway", "restricted", ""), 1840, true, false, 9 << 10, 100 << 10},
		{key("git.internal.example", "upstream", "gateway", "restricted", ""), 1102, true, false, 20 << 10, 96 << 10},
		{key("ci.build.example", "upstream", "gateway", "restricted", ""), 388, true, false, 12 << 10, 160 << 10},
		{key("docs.internal.example", "upstream", "gateway", "restricted", ""), 141, true, false, 4 << 10, 150 << 10},
		{key("lab.example.net", "upstream", "lab", "lab-net", ""), 23, true, true, 2 << 10, 40 << 10},
		{key("tracker.example", "block", "", "ads", ""), 1204, true, false, 0, 0},
		{key("ads.example.net", "block", "", "ads", ""), 688, true, false, 0, 0},
		{key("metrics.example.org", "block", "", "ads", ""), 301, false, false, 0, 0},
		{key("pixel.example.com", "block", "", "ads", ""), 97, true, false, 0, 0},
	} {
		record(st, r.k, r.n, r.connect, r.failed, r.up, r.down)
	}
	flush(f.t, st)
}

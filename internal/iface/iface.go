// Package iface holds interface selection (design 0008): interface
// definitions and their matching to system interfaces, the online
// predicate, the notice classifiers, dial plans, demotion, and the monitor
// that keeps the interface table current. The system reader and the notice
// sockets are in the darwin files; everything else is portable logic that
// tests drive with injected rows, notices, and timers.
package iface

import (
	"bytes"
	"context"
	"net"
	"net/netip"
	"strings"
)

// Kind is an interface's functional type (design 0008 D3).
type Kind int

const (
	// KindOther is every functional type other than the three below. In a
	// definition it means that no kind guard is set.
	KindOther Kind = iota
	KindWired
	KindWifi
	KindCellular
)

func (k Kind) String() string {
	switch k {
	case KindWired:
		return "wired"
	case KindWifi:
		return "wifi"
	case KindCellular:
		return "cellular"
	}
	return "other"
}

// ParseKind parses a definition's kind guard: wired, wifi, or cellular.
func ParseKind(s string) (Kind, bool) {
	switch s {
	case "wired":
		return KindWired, true
	case "wifi":
		return KindWifi, true
	case "cellular":
		return KindCellular, true
	}
	return KindOther, false
}

// Functional types of SIOCGIFFUNCTIONALTYPE (net/if.h,
// IFRTYPE_FUNCTIONAL_*).
const (
	functionalWired     = 2 // IFRTYPE_FUNCTIONAL_WIRED
	functionalWifiInfra = 3 // IFRTYPE_FUNCTIONAL_WIFI_INFRA
	functionalCellular  = 5 // IFRTYPE_FUNCTIONAL_CELLULAR
)

// KindOf maps a functional type to a kind: 2 is wired, 3 (Wi-Fi
// infrastructure) wifi, 5 cellular, and every other value other.
func KindOf(functionalType uint32) Kind {
	switch functionalType {
	case functionalWired:
		return KindWired
	case functionalWifiInfra:
		return KindWifi
	case functionalCellular:
		return KindCellular
	}
	return KindOther
}

// Media is an interface's media status (design 0008 D4 check 3).
type Media int

const (
	// MediaUnknown means that the interface reports no media status.
	MediaUnknown Media = iota
	MediaActive
	MediaInactive
)

func (m Media) String() string {
	switch m {
	case MediaActive:
		return "active"
	case MediaInactive:
		return "inactive"
	}
	return "none"
}

// Route says whether an interface carries a default route of a family.
type Route int

const (
	RouteNone Route = iota
	// RouteScoped is a default route with RTF_IFSCOPE.
	RouteScoped
	// RoutePrimary is the unscoped default route of the primary interface.
	RoutePrimary
)

func (r Route) String() string {
	switch r {
	case RouteScoped:
		return "scoped"
	case RoutePrimary:
		return "primary"
	}
	return "none"
}

// Link is one row of the system interface table (design 0008 D5).
type Link struct {
	Index    int
	Device   string
	MAC      net.HardwareAddr
	Up       bool // IFF_UP
	Loopback bool // IFF_LOOPBACK; loopback rows never match a definition
	Kind     Kind
	Media    Media
	Addrs    []netip.Addr
	Default4 Route
	Default6 Route
}

// Source supplies the interface table and its change notices. On darwin,
// System uses system calls; tests supply rows and notices.
type Source interface {
	Read() ([]Link, error)
	// Watch opens the notice sockets for this call and sends one value per
	// accepted message or socket read error until ctx ends; it then closes
	// the sockets and the channel. A socket that cannot be opened is
	// logged and sends nothing.
	Watch(ctx context.Context) <-chan struct{}
}

// Def is one [[interface]] definition (design 0008 D2).
type Def struct {
	Name   string
	MAC    net.HardwareAddr // nil when unset
	Device string           // "" when unset
	Kind   Kind             // KindOther when unset
}

// Same reports whether two definitions identify the same device: equal
// mac, device, and kind (design 0008 D14).
func (d Def) Same(o Def) bool {
	return bytes.Equal(d.MAC, o.MAC) && d.Device == o.Device && d.Kind == o.Kind
}

// Families is a set of address families.
type Families uint8

const (
	IPv4 Families = 1 << iota
	IPv6
)

// Has reports whether f holds every family of g.
func (f Families) Has(g Families) bool { return f&g == g && g != 0 }

// List returns the families as "ipv4" and "ipv6", in that order.
func (f Families) List() []string {
	out := []string{}
	if f&IPv4 != 0 {
		out = append(out, "ipv4")
	}
	if f&IPv6 != 0 {
		out = append(out, "ipv6")
	}
	return out
}

func (f Families) String() string { return strings.Join(f.List(), ",") }

// FamilyOf returns the family of an address. A NAT64 address is IPv6.
func FamilyOf(a netip.Addr) Families {
	if a.Unmap().Is4() {
		return IPv4
	}
	return IPv6
}

// Offline reasons (design 0008 D4), and no_family for an online interface
// that has none of a plan's address families (D7).
const (
	ReasonAbsent       = "absent"
	ReasonAmbiguous    = "ambiguous"
	ReasonKindMismatch = "kind_mismatch"
	ReasonUnknown      = "unknown"
	ReasonAdminDown    = "admin_down"
	ReasonLinkDown     = "link_down"
	ReasonNoAddress    = "no_address"
	ReasonNoRoute      = "no_route"
	ReasonNoFamily     = "no_family"
)

// Match is a definition evaluated against the interface table: which row
// it matched and whether that row is online (design 0008 D3, D4).
type Match struct {
	Online   bool
	Reason   string   // the first failing check's reason when offline
	Families Families // families that pass checks 4 and 5
	Primary  Families // online families whose unscoped default uses the row
	// Link is the matched row; it is meaningful when exactly one row
	// matched (Reason is not absent, ambiguous, or unknown).
	Link Link
	// Devices lists the BSD names of every matching row when the
	// definition is ambiguous.
	Devices []string
	// OtherMAC reports that the definition's device exists with another
	// MAC.
	OtherMAC bool
}

// Matches reports whether the definition matches a row: every key it sets
// is equal, and loopback rows never match.
func (d Def) Matches(l Link) bool {
	if l.Loopback {
		return false
	}
	if d.MAC != nil && !bytes.Equal(d.MAC, l.MAC) {
		return false
	}
	if d.Device != "" && d.Device != l.Device {
		return false
	}
	return d.MAC != nil || d.Device != ""
}

var (
	v4Unusable = []netip.Prefix{netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("169.254.0.0/16")}
	v6LinkLoc  = netip.MustParsePrefix("fe80::/10")
)

// Usable reports whether an interface address counts for the online
// predicate (design 0008 D4 check 4): IPv4 outside 0.0.0.0/8 and
// 169.254.0.0/16, IPv6 outside fe80::/10.
func Usable(a netip.Addr) bool {
	a = a.Unmap().WithZone("")
	if a.Is4() {
		for _, p := range v4Unusable {
			if p.Contains(a) {
				return false
			}
		}
		return a.IsValid()
	}
	return a.IsValid() && !v6LinkLoc.Contains(a)
}

// Evaluate matches a definition against the table and applies the online
// predicate (design 0008 D3, D4). readable is false when the table has
// never been read, which makes every definition offline with reason
// unknown.
func Evaluate(d Def, table []Link, readable bool) Match {
	if !readable {
		return Match{Reason: ReasonUnknown}
	}
	var found []Link
	for _, l := range table {
		if d.Matches(l) {
			found = append(found, l)
		}
	}
	switch len(found) {
	case 0:
		m := Match{Reason: ReasonAbsent}
		if d.Device != "" && d.MAC != nil {
			for _, l := range table {
				if l.Device == d.Device && !l.Loopback {
					m.OtherMAC = true
				}
			}
		}
		return m
	case 1:
	default:
		m := Match{Reason: ReasonAmbiguous}
		for _, l := range found {
			m.Devices = append(m.Devices, l.Device)
		}
		return m
	}
	l := found[0]
	m := Match{Link: l}
	switch {
	case d.Kind != KindOther && l.Kind != d.Kind:
		m.Reason = ReasonKindMismatch
		return m
	case !l.Up:
		m.Reason = ReasonAdminDown
		return m
	case l.Media == MediaInactive:
		m.Reason = ReasonLinkDown
		return m
	}
	var addressed Families
	for _, a := range l.Addrs {
		if Usable(a) {
			addressed |= FamilyOf(a)
		}
	}
	if addressed&IPv4 != 0 && l.Default4 != RouteNone {
		m.Families |= IPv4
	}
	if addressed&IPv6 != 0 && l.Default6 != RouteNone {
		m.Families |= IPv6
	}
	if m.Families == 0 {
		m.Reason = ReasonNoRoute
		if addressed == 0 {
			m.Reason = ReasonNoAddress
		}
		return m
	}
	m.Online = true
	if m.Families&IPv4 != 0 && l.Default4 == RoutePrimary {
		m.Primary |= IPv4
	}
	if m.Families&IPv6 != 0 && l.Default6 == RoutePrimary {
		m.Primary |= IPv6
	}
	return m
}

// FirstUsable returns the first usable address of a family, or an invalid
// address.
func (l Link) FirstUsable(f Families) netip.Addr {
	for _, a := range l.Addrs {
		if Usable(a) && FamilyOf(a) == f {
			return a
		}
	}
	return netip.Addr{}
}

// UsableAddrs returns every usable address of a family.
func (l Link) UsableAddrs(f Families) []netip.Addr {
	out := []netip.Addr{}
	for _, a := range l.Addrs {
		if Usable(a) && FamilyOf(a) == f {
			out = append(out, a)
		}
	}
	return out
}

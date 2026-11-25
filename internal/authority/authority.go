// Package authority parses and normalizes proxy request targets (design 0002
// D3). The normalized authority is used for rule matching, for the direct
// dial, and in cuttle's own messages. Forwarded requests keep the client's
// raw target, which HTTPTarget carries alongside the normalized form.
package authority

import (
	"net/netip"
	"strconv"
	"strings"
)

// Authority is a normalized destination: a lowercase ASCII name without a
// trailing dot, or a canonical IP address, plus a port.
type Authority struct {
	// Host is the normalized name, or the canonical text of Addr for an IP
	// literal.
	Host string
	// Addr is valid only for IP-literal targets. IPv4-mapped IPv6 addresses
	// are unmapped.
	Addr netip.Addr
	Port uint16
}

// IsIP reports whether the target is an IP literal.
func (a Authority) IsIP() bool { return a.Addr.IsValid() }

// String returns host:port, with IPv6 literals in brackets.
func (a Authority) String() string {
	if a.Addr.Is6() {
		return "[" + a.Host + "]:" + strconv.Itoa(int(a.Port))
	}
	return a.Host + ":" + strconv.Itoa(int(a.Port))
}

// Error describes why a target was refused. Reason is safe to show to the
// client and to log: it never contains the target itself.
type Error struct {
	Reason string
}

func (e *Error) Error() string { return "invalid request target: " + e.Reason }

func fail(reason string) error { return &Error{Reason: reason} }

// ParseConnect parses a CONNECT request target, which must be host:port.
func ParseConnect(target string) (Authority, error) {
	if target == "" {
		return Authority{}, fail("empty target")
	}
	if strings.HasPrefix(target, "/") {
		return Authority{}, fail("CONNECT target is not host:port")
	}
	if strings.Contains(target, "@") {
		return Authority{}, fail("userinfo in target")
	}
	return parseHostPort(target, false)
}

// HTTPTarget is a parsed absolute-form http:// request target.
type HTTPTarget struct {
	Authority
	// RawAuthority, RawPath, and RawQuery are the client's bytes, unchanged.
	// RawPath is empty when the target has no path.
	RawAuthority string
	RawPath      string
	RawQuery     string
	// HasQuery reports whether the target contained "?", even with an empty
	// query.
	HasQuery bool
}

// ParseHTTP parses the request target of a non-CONNECT request. Only
// absolute-form http:// targets are accepted.
func ParseHTTP(requestURI string) (HTTPTarget, error) {
	switch {
	case requestURI == "":
		return HTTPTarget{}, fail("empty target")
	case requestURI == "*":
		return HTTPTarget{}, fail("asterisk-form target")
	case strings.HasPrefix(requestURI, "/"):
		return HTTPTarget{}, fail("origin-form target")
	}
	i := strings.Index(requestURI, "://")
	if i <= 0 {
		return HTTPTarget{}, fail("unsupported target form")
	}
	scheme := strings.ToLower(requestURI[:i])
	switch scheme {
	case "http":
	case "https":
		return HTTPTarget{}, fail("absolute-form https target; use CONNECT")
	default:
		return HTTPTarget{}, fail("unsupported scheme")
	}
	rest := requestURI[i+3:]
	end := strings.IndexAny(rest, "/?")
	if end < 0 {
		end = len(rest)
	}
	t := HTTPTarget{RawAuthority: rest[:end]}
	rest = rest[end:]
	if q := strings.IndexByte(rest, '?'); q >= 0 {
		t.RawPath = rest[:q]
		t.RawQuery = rest[q+1:]
		t.HasQuery = true
	} else {
		t.RawPath = rest
	}
	if strings.Contains(t.RawAuthority, "@") {
		return HTTPTarget{}, fail("userinfo in target")
	}
	a, err := parseHostPort(t.RawAuthority, true)
	if err != nil {
		return HTTPTarget{}, err
	}
	t.Authority = a
	return t, nil
}

// parseHostPort splits and normalizes host:port. With defaultPort, the port
// may be absent or empty and defaults to 80.
func parseHostPort(s string, defaultPort bool) (Authority, error) {
	var host, port string
	var bracketed, hasPort bool
	if strings.HasPrefix(s, "[") {
		end := strings.IndexByte(s, ']')
		if end < 0 {
			return Authority{}, fail("unterminated IPv6 literal")
		}
		host, bracketed = s[1:end], true
		rest := s[end+1:]
		switch {
		case rest == "":
		case rest[0] == ':':
			port, hasPort = rest[1:], true
		default:
			return Authority{}, fail("invalid authority")
		}
	} else {
		i := strings.LastIndexByte(s, ':')
		if i >= 0 {
			host, port, hasPort = s[:i], s[i+1:], true
			if strings.Contains(host, ":") {
				return Authority{}, fail("IPv6 literal without brackets")
			}
		} else {
			host = s
		}
	}
	var p uint16
	switch {
	case hasPort && port != "":
		var err error
		if p, err = ParsePort(port); err != nil {
			return Authority{}, err
		}
	case defaultPort:
		p = 80
	case hasPort:
		return Authority{}, fail("empty port")
	default:
		return Authority{}, fail("missing port")
	}
	a, err := normalizeHost(host, bracketed)
	if err != nil {
		return Authority{}, err
	}
	a.Port = p
	return a, nil
}

// ParsePort parses a decimal port 1-65535 without sign or leading zeros.
func ParsePort(s string) (uint16, error) {
	if s == "" || len(s) > 5 || s[0] == '0' {
		return 0, fail("invalid port")
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fail("invalid port")
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return 0, fail("invalid port")
	}
	return uint16(n), nil
}

func normalizeHost(host string, bracketed bool) (Authority, error) {
	if bracketed {
		if strings.Contains(host, "%") {
			return Authority{}, fail("zone in IPv6 literal")
		}
		addr, err := netip.ParseAddr(host)
		if err != nil || !addr.Is6() {
			return Authority{}, fail("invalid IPv6 literal")
		}
		addr = addr.Unmap()
		return Authority{Host: addr.String(), Addr: addr}, nil
	}
	if addr, err := netip.ParseAddr(host); err == nil && addr.Is4() {
		return Authority{Host: addr.String(), Addr: addr}, nil
	}
	name, err := NormalizeName(host)
	if err != nil {
		return Authority{}, err
	}
	return Authority{Host: name}, nil
}

// NormalizeName lowercases a host name, removes one trailing dot, and checks
// the D3 name syntax.
func NormalizeName(host string) (string, error) {
	for i := 0; i < len(host); i++ {
		if host[i] >= 0x80 {
			return "", fail("non-ASCII host name; send the A-label (xn--) form")
		}
	}
	name := strings.ToLower(host)
	name = strings.TrimSuffix(name, ".")
	if err := checkName(name); err != nil {
		return "", err
	}
	return name, nil
}

// checkName checks a lowercase name without a trailing dot.
func checkName(name string) error {
	if name == "" || len(name) > 253 {
		return fail("invalid host name")
	}
	labels := strings.Split(name, ".")
	for _, l := range labels {
		if l == "" || len(l) > 63 {
			return fail("invalid host name")
		}
		for i := 0; i < len(l); i++ {
			c := l[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return fail("invalid host name")
			}
		}
	}
	last := labels[len(labels)-1]
	if allDigits(last) || strings.HasPrefix(last, "0x") {
		return fail("numeric host that is not a dotted quad")
	}
	return nil
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

// CheckPattern checks a configured hosts or domains entry. It must already
// be a lowercase ASCII name in the D3 syntax. The returned error suggests the
// correct form.
func CheckPattern(p string) error {
	switch {
	case p == "":
		return fail("empty name")
	case strings.HasPrefix(p, "*."):
		return fail("wildcards are not supported; list " + strconv.Quote(p[2:]) + " in domains, which matches subdomains")
	case strings.HasPrefix(p, "."):
		return fail("leading dot; list " + strconv.Quote(strings.TrimLeft(p, ".")) + " in domains, which matches subdomains")
	}
	if addr, err := netip.ParseAddr(strings.Trim(p, "[]")); err == nil {
		bits := 32
		if addr.Is6() {
			bits = 128
		}
		return fail("IP address; list " + strconv.Quote(netip.PrefixFrom(addr.Unmap(), bits).String()) + " in cidrs")
	}
	for i := 0; i < len(p); i++ {
		if p[i] >= 0x80 {
			return fail("non-ASCII name; use the A-label (xn--) form")
		}
	}
	if p != strings.ToLower(p) {
		return fail("name must be lowercase; use " + strconv.Quote(strings.ToLower(p)))
	}
	if strings.HasSuffix(p, ".") {
		return fail("trailing dot; use " + strconv.Quote(strings.TrimSuffix(p, ".")))
	}
	if strings.Contains(p, "*") {
		return fail("wildcards are not supported")
	}
	return checkName(p)
}

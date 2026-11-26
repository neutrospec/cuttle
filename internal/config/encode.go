package config

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Generated comments of the canonical form (design 0009 D6). None holds a
// path, a time, or anything else that changes between saves, so encoding
// the same Doc twice gives the same bytes.
var (
	headerComment = []string{
		"# cuttle configuration. The admin UI writes this file in this form and",
		"# replaces comments with these generated ones; the previous version is",
		"# kept beside it with a .bak suffix. Check hand edits with `cuttle check`",
		"# and apply them with `cuttle reload`.",
	}
	proxyComment     = []string{"# Listeners and the route for requests that no rule matches."}
	remoteComment    = []string{"# Clients on other machines: allowed sources, ports, and their default."}
	adminComment     = []string{"# Admin UI; set the first password with `cuttle admin passwd`."}
	interfaceComment = []string{
		"# Network interfaces, named for interface lists; `cuttle interfaces`",
		"# lists this Mac's interfaces.",
	}
	upstreamComment = []string{
		`# Upstream proxies. disclosure: "via" for another cuttle instance,`,
		`# "none" for an egress proxy.`,
	}
	ruleComment = []string{"# Rules, tried in this order; the first match wins."}
)

// maxLineWidth is the width up to which an array stays on one line.
const maxLineWidth = 80

// Encode writes d in the canonical form of design 0009 D6: the header
// comment, name, [proxy], [proxy.remote], [admin], [stats], [log], then the
// [[interface]], [[upstream]], and [[rule]] entries, each table with its
// keys in schema order and only the keys d sets. A table without a set key
// is left out, but every entry of an array of tables is written, even an
// empty one, so that an incomplete draft still shows as an error rather
// than vanishing. A rule's name is always written, even when it is "".
// Strings are basic strings with every control character
// escaped, so the result is valid TOML for any Doc; whether it is a valid
// configuration is for Parse to say.
func (d *Doc) Encode() []byte {
	e := &encoder{}
	e.lines(headerComment...)
	if d.Name != "" {
		e.lines("", "name = "+quote(d.Name))
	}

	p := d.Proxy
	e.table(proxyComment, "[proxy]", func() {
		e.strs("listen", p.Listen)
		if p.Default != "" {
			e.str("default", p.Default)
		}
		e.optStr("connect_timeout", p.ConnectTimeout)
		e.optStr("response_timeout", p.ResponseTimeout)
		e.optStr("idle_timeout", p.IdleTimeout)
		e.optInt("max_connections", p.MaxConnections)
		e.strs("interfaces", p.Interfaces)
		e.optStr("interface_attempt_timeout", p.InterfaceAttemptTimeout)
		e.optStr("interface_demotion", p.InterfaceDemotion)
	})
	r := d.Remote
	e.table(remoteComment, "[proxy.remote]", func() {
		e.strs("sources", r.Sources)
		e.ints("ports", r.Ports)
		e.optStr("default", r.Default)
		e.optBool("allow_direct", r.AllowDirect)
		e.optInt("max_connections_per_source", r.MaxConnectionsPerSource)
	})
	e.table(adminComment, "[admin]", func() { e.optStr("listen", d.Admin.Listen) })
	e.table(nil, "[stats]", func() {
		e.optInt("retention_days", d.Stats.RetentionDays)
		e.optInt("max_hosts_per_hour", d.Stats.MaxHostsPerHour)
	})
	e.table(nil, "[log]", func() { e.optStr("level", d.Log.Level) })

	comment := interfaceComment
	for _, x := range d.Interfaces {
		e.entry(comment, "[[interface]]", func() {
			if x.Name != "" {
				e.str("name", x.Name)
			}
			e.optStr("note", x.Note)
			e.optStr("mac", x.MAC)
			e.optStr("device", x.Device)
			e.optStr("kind", x.Kind)
		})
		comment = nil
	}
	comment = upstreamComment
	for _, u := range d.Upstreams {
		e.entry(comment, "[[upstream]]", func() {
			if u.Name != "" {
				e.str("name", u.Name)
			}
			e.optStr("note", u.Note)
			if u.Address != "" {
				e.str("address", u.Address)
			}
			if u.Disclosure != "" {
				e.str("disclosure", u.Disclosure)
			}
			e.optStr("connect_timeout", u.ConnectTimeout)
			e.optStr("response_timeout", u.ResponseTimeout)
			e.optStr("unreachable_hold", u.UnreachableHold)
			e.strs("interfaces", u.Interfaces)
		})
		comment = nil
	}
	comment = ruleComment
	for _, x := range d.Rules {
		e.entry(comment, "[[rule]]", func() {
			e.str("name", x.Name)
			e.optStr("note", x.Note)
			e.strs("hosts", x.Hosts)
			e.strs("domains", x.Domains)
			e.strs("cidrs", x.CIDRs)
			e.ints("ports", x.Ports)
			if x.Action != "" {
				e.str("action", x.Action)
			}
			e.optBool("allow_loopback", x.AllowLoopback)
			e.optBool("allow_private", x.AllowPrivate)
			e.strs("interfaces", x.Interfaces)
		})
		comment = nil
	}
	return []byte(e.b.String())
}

type encoder struct {
	b strings.Builder
	// body collects the key lines of the table being written.
	body []string
}

func (e *encoder) lines(ls ...string) {
	for _, l := range ls {
		e.b.WriteString(l)
		e.b.WriteByte('\n')
	}
}

// table writes one table, after an empty line and its comment, when keys
// writes at least one key.
func (e *encoder) table(comment []string, header string, keys func()) {
	e.body = e.body[:0]
	keys()
	if len(e.body) > 0 {
		e.write(comment, header)
	}
}

// entry writes one entry of an array of tables, even an empty one, so
// that the number of entries never changes.
func (e *encoder) entry(comment []string, header string, keys func()) {
	e.body = e.body[:0]
	keys()
	e.write(comment, header)
}

func (e *encoder) write(comment []string, header string) {
	e.lines("")
	e.lines(comment...)
	e.lines(header)
	e.lines(e.body...)
}

func (e *encoder) key(k, v string) { e.body = append(e.body, k+" = "+v) }

func (e *encoder) str(k, v string) { e.key(k, quote(v)) }

func (e *encoder) optStr(k string, o Opt[string]) {
	if o.Set {
		e.str(k, o.V)
	}
}

func (e *encoder) optInt(k string, o Opt[int64]) {
	if o.Set {
		e.key(k, strconv.FormatInt(o.V, 10))
	}
}

func (e *encoder) optBool(k string, o Opt[bool]) {
	if o.Set {
		e.key(k, strconv.FormatBool(o.V))
	}
}

func (e *encoder) strs(k string, o Opt[[]string]) {
	if o.Set {
		items := make([]string, len(o.V))
		for i, s := range o.V {
			items[i] = quote(s)
		}
		e.array(k, items)
	}
}

func (e *encoder) ints(k string, o Opt[[]int64]) {
	if o.Set {
		items := make([]string, len(o.V))
		for i, n := range o.V {
			items[i] = strconv.FormatInt(n, 10)
		}
		e.array(k, items)
	}
}

// array writes an array on one line when the line fits in maxLineWidth
// columns, counted as code points, and otherwise one element per line with
// a trailing comma.
func (e *encoder) array(k string, items []string) {
	one := k + " = [" + strings.Join(items, ", ") + "]"
	if utf8.RuneCountInString(one) <= maxLineWidth {
		e.body = append(e.body, one)
		return
	}
	e.body = append(e.body, k+" = [")
	for _, it := range items {
		e.body = append(e.body, "  "+it+",")
	}
	e.body = append(e.body, "]")
}

// quote returns s as a TOML basic string. It escapes '"' and '\', writes
// every control character, U+0000 to U+001F and U+007F, as \b, \t, \n,
// \f, \r, or \uXXXX, and keeps every other character as UTF-8. Bytes that are not UTF-8 become U+FFFD, since
// a TOML file must be UTF-8.
func quote(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if isControl(r) {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

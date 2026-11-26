package config

import (
	"slices"
	"strconv"
)

// Opt is an optional key of the configuration file: Set reports whether
// the file sets it, and V holds the value as written. An unset key takes
// its default in the Config (design 0009 D6).
type Opt[T any] struct {
	V   T
	Set bool
}

// Some returns a set key with value v.
func Some[T any](v T) Opt[T] { return Opt[T]{V: v, Set: true} }

// Doc is the configuration file as the operator wrote it, the model that
// the admin forms edit and Encode writes in canonical form (design 0009
// D6). Each optional key is set or unset with its value as written:
// strings as strings, durations as their strings, lists as lists, integers
// as int64, and booleans as bool. Arrays of tables keep their order.
//
// A required string key is unset when it is "": Encode leaves it out, so
// the validator reports it as required. Doc never holds defaults, and it
// is not validated: Encode writes whatever it holds, and Parse of the
// result is the only judgment.
type Doc struct {
	Name       string
	Proxy      DocProxy
	Remote     DocRemote // [proxy.remote]
	Admin      DocAdmin
	Stats      DocStats
	Log        DocLog
	Interfaces []DocInterface
	Upstreams  []DocUpstream
	Rules      []DocRule
}

// DocProxy is the [proxy] table of a Doc.
type DocProxy struct {
	Listen                  Opt[[]string]
	Default                 string
	ConnectTimeout          Opt[string]
	ResponseTimeout         Opt[string]
	IdleTimeout             Opt[string]
	MaxConnections          Opt[int64]
	Interfaces              Opt[[]string]
	InterfaceAttemptTimeout Opt[string]
	InterfaceDemotion       Opt[string]
}

// DocRemote is the [proxy.remote] table of a Doc.
type DocRemote struct {
	Sources                 Opt[[]string]
	Ports                   Opt[[]int64]
	Default                 Opt[string]
	AllowDirect             Opt[bool]
	MaxConnectionsPerSource Opt[int64]
}

// DocAdmin is the [admin] table of a Doc. Listen set to "" disables the
// admin UI, which differs from unset.
type DocAdmin struct {
	Listen Opt[string]
}

// DocStats is the [stats] table of a Doc.
type DocStats struct {
	RetentionDays   Opt[int64]
	MaxHostsPerHour Opt[int64]
}

// DocLog is the [log] table of a Doc.
type DocLog struct {
	Level Opt[string]
}

// DocInterface is one [[interface]] entry of a Doc.
type DocInterface struct {
	Name   string
	Note   Opt[string]
	MAC    Opt[string]
	Device Opt[string]
	Kind   Opt[string]
}

// DocUpstream is one [[upstream]] entry of a Doc.
type DocUpstream struct {
	Name            string
	Note            Opt[string]
	Address         string
	Disclosure      string
	ConnectTimeout  Opt[string]
	ResponseTimeout Opt[string]
	UnreachableHold Opt[string]
	Interfaces      Opt[[]string]
}

// DocRule is one [[rule]] entry of a Doc. Name is always written, even
// when it is "", because the validator would otherwise give the rule the
// name rule-<n> of its position.
type DocRule struct {
	Name          string
	Note          Opt[string]
	Hosts         Opt[[]string]
	Domains       Opt[[]string]
	CIDRs         Opt[[]string]
	Ports         Opt[[]int64]
	Action        string
	AllowLoopback Opt[bool]
	AllowPrivate  Opt[bool]
	Interfaces    Opt[[]string]
}

// DocOf parses data as Parse does and, when it is valid, also returns its
// model, built from the same decoded TOML. A rule without a name gets the
// name rule-<n> that the validator gave it, so that the next Encode writes
// it and a later reorder does not rename the rule. Doc and Config are nil
// when the report has errors.
func DocOf(data []byte, file string) (*Doc, *Config, *Report) {
	cfg, raw, r := parse(data, file)
	if cfg == nil {
		return nil, nil, r
	}
	return docOf(raw), cfg, r
}

// docOf builds the model of a decoded file that the validator accepted, so
// every value has its schema type.
func docOf(raw map[string]any) *Doc {
	d := &Doc{}
	d.Name, _ = raw["name"].(string)
	if pm, ok := raw["proxy"].(map[string]any); ok {
		d.Proxy = DocProxy{
			Listen:                  optStrings(pm, "listen"),
			ConnectTimeout:          optStr(pm, "connect_timeout"),
			ResponseTimeout:         optStr(pm, "response_timeout"),
			IdleTimeout:             optStr(pm, "idle_timeout"),
			MaxConnections:          optInt(pm, "max_connections"),
			Interfaces:              optStrings(pm, "interfaces"),
			InterfaceAttemptTimeout: optStr(pm, "interface_attempt_timeout"),
			InterfaceDemotion:       optStr(pm, "interface_demotion"),
		}
		d.Proxy.Default, _ = pm["default"].(string)
		if rm, ok := pm["remote"].(map[string]any); ok {
			d.Remote = DocRemote{
				Sources:                 optStrings(rm, "sources"),
				Ports:                   optInts(rm, "ports"),
				Default:                 optStr(rm, "default"),
				AllowDirect:             optBool(rm, "allow_direct"),
				MaxConnectionsPerSource: optInt(rm, "max_connections_per_source"),
			}
		}
	}
	if am, ok := raw["admin"].(map[string]any); ok {
		d.Admin.Listen = optStr(am, "listen")
	}
	if sm, ok := raw["stats"].(map[string]any); ok {
		d.Stats = DocStats{RetentionDays: optInt(sm, "retention_days"), MaxHostsPerHour: optInt(sm, "max_hosts_per_hour")}
	}
	if lm, ok := raw["log"].(map[string]any); ok {
		d.Log.Level = optStr(lm, "level")
	}
	for _, m := range rawTables(raw, "interface") {
		i := DocInterface{Note: optStr(m, "note"), MAC: optStr(m, "mac"), Device: optStr(m, "device"), Kind: optStr(m, "kind")}
		i.Name, _ = m["name"].(string)
		d.Interfaces = append(d.Interfaces, i)
	}
	for _, m := range rawTables(raw, "upstream") {
		u := DocUpstream{
			Note:            optStr(m, "note"),
			ConnectTimeout:  optStr(m, "connect_timeout"),
			ResponseTimeout: optStr(m, "response_timeout"),
			UnreachableHold: optStr(m, "unreachable_hold"),
			Interfaces:      optStrings(m, "interfaces"),
		}
		u.Name, _ = m["name"].(string)
		u.Address, _ = m["address"].(string)
		u.Disclosure, _ = m["disclosure"].(string)
		d.Upstreams = append(d.Upstreams, u)
	}
	for i, m := range rawTables(raw, "rule") {
		r := DocRule{
			Note:          optStr(m, "note"),
			Hosts:         optStrings(m, "hosts"),
			Domains:       optStrings(m, "domains"),
			CIDRs:         optStrings(m, "cidrs"),
			Ports:         optInts(m, "ports"),
			AllowLoopback: optBool(m, "allow_loopback"),
			AllowPrivate:  optBool(m, "allow_private"),
			Interfaces:    optStrings(m, "interfaces"),
		}
		r.Name, _ = m["name"].(string)
		if _, named := m["name"]; !named {
			r.Name = "rule-" + strconv.Itoa(i+1)
		}
		r.Action, _ = m["action"].(string)
		d.Rules = append(d.Rules, r)
	}
	return d
}

// rawTables returns the tables of an array of tables, written as
// [[key]] or as an inline array of inline tables.
func rawTables(raw map[string]any, key string) []map[string]any {
	switch t := raw[key].(type) {
	case []map[string]any:
		return t
	case []any:
		out := make([]map[string]any, 0, len(t))
		for _, e := range t {
			if m, ok := e.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

func optStr(m map[string]any, key string) Opt[string] {
	s, ok := m[key].(string)
	return Opt[string]{V: s, Set: ok}
}

func optInt(m map[string]any, key string) Opt[int64] {
	n, ok := m[key].(int64)
	return Opt[int64]{V: n, Set: ok}
}

func optBool(m map[string]any, key string) Opt[bool] {
	b, ok := m[key].(bool)
	return Opt[bool]{V: b, Set: ok}
}

func optStrings(m map[string]any, key string) Opt[[]string] {
	a, ok := m[key].([]any)
	if !ok {
		return Opt[[]string]{}
	}
	out := make([]string, 0, len(a))
	for _, x := range a {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return Some(out)
}

func optInts(m map[string]any, key string) Opt[[]int64] {
	a, ok := m[key].([]any)
	if !ok {
		return Opt[[]int64]{}
	}
	out := make([]int64, 0, len(a))
	for _, x := range a {
		if n, ok := x.(int64); ok {
			out = append(out, n)
		}
	}
	return Some(out)
}

// Clone returns a deep copy of d, so that an edit of the copy leaves d
// unchanged.
func (d *Doc) Clone() *Doc {
	c := *d
	c.Proxy.Listen = cloneList(d.Proxy.Listen)
	c.Proxy.Interfaces = cloneList(d.Proxy.Interfaces)
	c.Remote.Sources = cloneList(d.Remote.Sources)
	c.Remote.Ports = cloneList(d.Remote.Ports)
	c.Interfaces = slices.Clone(d.Interfaces)
	c.Upstreams = slices.Clone(d.Upstreams)
	for i := range c.Upstreams {
		c.Upstreams[i] = c.Upstreams[i].clone()
	}
	c.Rules = slices.Clone(d.Rules)
	for i := range c.Rules {
		c.Rules[i] = c.Rules[i].clone()
	}
	return &c
}

func (u DocUpstream) clone() DocUpstream {
	u.Interfaces = cloneList(u.Interfaces)
	return u
}

func (r DocRule) clone() DocRule {
	r.Hosts = cloneList(r.Hosts)
	r.Domains = cloneList(r.Domains)
	r.CIDRs = cloneList(r.CIDRs)
	r.Ports = cloneList(r.Ports)
	r.Interfaces = cloneList(r.Interfaces)
	return r
}

func cloneList[T any](o Opt[[]T]) Opt[[]T] {
	o.V = slices.Clone(o.V)
	return o
}

// Rule returns the named rule and its index, or -1.
func (d *Doc) Rule(name string) (DocRule, int) {
	for i, r := range d.Rules {
		if r.Name == name {
			return r, i
		}
	}
	return DocRule{}, -1
}

// Upstream returns the named upstream and its index, or -1.
func (d *Doc) Upstream(name string) (DocUpstream, int) {
	for i, u := range d.Upstreams {
		if u.Name == name {
			return u, i
		}
	}
	return DocUpstream{}, -1
}

// Interface returns the named interface definition and its index, or -1.
func (d *Doc) Interface(name string) (DocInterface, int) {
	for i, x := range d.Interfaces {
		if x.Name == name {
			return x, i
		}
	}
	return DocInterface{}, -1
}

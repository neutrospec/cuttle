package config

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Edit operations of design 0009 D7. Each changes the Doc in place, or
// returns an error and leaves it unchanged. They check only what the edit
// needs, such as that its target exists; whether the result is a valid
// configuration is for Parse of the encoded Doc to say.

// ErrNotFound is wrapped by the error of an edit whose target no longer
// exists, such as a rule deleted by another writer (D7 step 2).
var ErrNotFound = errors.New("no longer exists")

// ErrOrder is returned by ReorderRules when the names are not every rule
// exactly once.
var ErrOrder = errors.New("the order does not name every rule exactly once")

// ErrRuleChanged is returned by AddPattern when the target rule no longer
// has the chosen action or now has ports (D3.12, OI-0006 Y-R3).
var ErrRuleChanged = errors.New("the rule's action or ports changed")

// Kinds of entities and references.
const (
	KindRule      = "rule"
	KindUpstream  = "upstream"
	KindInterface = "interface"
	// KindKey is a key of [proxy] or [proxy.remote], such as
	// proxy.remote.default.
	KindKey = "key"
)

// Ref is one use of an upstream or an interface definition.
type Ref struct {
	Kind string // KindRule, KindUpstream, or KindKey
	// Name is the rule or upstream name, or the key path such as
	// "proxy.remote.default" or "proxy.interfaces".
	Name string
	// Pos is the 1-based position in an interfaces list; 0 for an action
	// or a default.
	Pos int
}

// String names the reference as the admin pages do: rule `x`, upstream
// `y`, or the key path.
func (r Ref) String() string {
	if r.Kind == KindKey {
		return r.Name
	}
	return r.Kind + " `" + r.Name + "`"
}

// InUseError is returned by a delete of an upstream or interface
// definition that something still uses.
type InUseError struct {
	Kind, Name string
	Users      []Ref
}

func (e *InUseError) Error() string {
	users := make([]string, len(e.Users))
	for i, u := range e.Users {
		users[i] = u.String()
	}
	return fmt.Sprintf("%s %q is used by %s", e.Kind, e.Name, strings.Join(users, ", "))
}

func notFound(kind, name string) error {
	return fmt.Errorf("%s %q %w", kind, name, ErrNotFound)
}

// InsertRule inserts r so that it becomes the rule at index at: 0 for
// first, len(d.Rules) for last, or the index of the rule it goes before.
func (d *Doc) InsertRule(r DocRule, at int) error {
	if at < 0 || at > len(d.Rules) {
		return fmt.Errorf("rule position %d is out of range 1-%d", at+1, len(d.Rules)+1)
	}
	d.Rules = slices.Insert(d.Rules, at, r)
	return nil
}

// ReplaceRule replaces the rule named target with r, which may have another
// name. Rules are referred to by nothing in the file, so a rename changes
// only the rule.
func (d *Doc) ReplaceRule(target string, r DocRule) error {
	_, i := d.Rule(target)
	if i < 0 {
		return notFound(KindRule, target)
	}
	d.Rules[i] = r
	return nil
}

// DeleteRule removes the rule named target.
func (d *Doc) DeleteRule(target string) error {
	_, i := d.Rule(target)
	if i < 0 {
		return notFound(KindRule, target)
	}
	d.Rules = slices.Delete(d.Rules, i, i+1)
	return nil
}

// ReorderRules puts the rules in the order of names, which must name every
// rule exactly once.
func (d *Doc) ReorderRules(names []string) error {
	if len(names) != len(d.Rules) {
		return ErrOrder
	}
	out := make([]DocRule, 0, len(names))
	seen := map[string]bool{}
	for _, n := range names {
		r, i := d.Rule(n)
		if i < 0 || seen[n] {
			return ErrOrder
		}
		seen[n] = true
		out = append(out, r)
	}
	d.Rules = out
	return nil
}

// DuplicateRule returns a copy of the rule named target with an unused
// name, <name>-copy or <name>-copy-<n>, for a new-rule form. It does not
// insert the copy.
func (d *Doc) DuplicateRule(target string) (DocRule, error) {
	r, i := d.Rule(target)
	if i < 0 {
		return DocRule{}, notFound(KindRule, target)
	}
	r = r.clone()
	r.Name = d.FreeRuleName(r.Name + "-copy")
	return r, nil
}

// FreeRuleName returns base, or base-2, base-3, and so on, whichever no
// rule uses, cut so that it stays within the 64 characters of a rule name.
// Its result is a valid rule name when base is one.
func (d *Doc) FreeRuleName(base string) string {
	for n := 1; ; n++ {
		suffix := ""
		if n > 1 {
			suffix = "-" + strconv.Itoa(n)
		}
		name := base
		if len(name)+len(suffix) > 64 {
			name = name[:64-len(suffix)]
		}
		name += suffix
		if _, i := d.Rule(name); i < 0 {
			return name
		}
	}
}

// RouteRules returns the names of the rules that "Add to rule" of Route a
// host offers for action: those with that action and no ports (D3.12).
func (d *Doc) RouteRules(action string) []string {
	var out []string
	for _, r := range d.Rules {
		if r.Action == action && len(r.Ports.V) == 0 {
			out = append(out, r.Name)
		}
	}
	return out
}

// Pattern keys of a rule.
const (
	PatternHosts   = "hosts"
	PatternDomains = "domains"
	PatternCIDRs   = "cidrs"
)

// AddPattern appends pattern to the hosts, domains, or cidrs of the rule
// named target, unless the list holds it already: the "Add to rule" of
// Route a host (D3.12). The rule must still have action and no ports;
// otherwise the error wraps ErrRuleChanged.
func (d *Doc) AddPattern(target, action, key, pattern string) error {
	_, i := d.Rule(target)
	if i < 0 {
		return notFound(KindRule, target)
	}
	r := &d.Rules[i]
	if r.Action != action || len(r.Ports.V) > 0 {
		return fmt.Errorf("rule %q: %w", target, ErrRuleChanged)
	}
	var list *Opt[[]string]
	switch key {
	case PatternHosts:
		list = &r.Hosts
	case PatternDomains:
		list = &r.Domains
	case PatternCIDRs:
		list = &r.CIDRs
	default:
		return fmt.Errorf("unknown pattern key %q", key)
	}
	if !slices.Contains(list.V, pattern) {
		*list = Some(append(slices.Clone(list.V), pattern))
	}
	return nil
}

// AddUpstream appends u.
func (d *Doc) AddUpstream(u DocUpstream) { d.Upstreams = append(d.Upstreams, u) }

// ReplaceUpstream replaces the upstream named target with u. When u has
// another name, every upstream:<target> action and default becomes
// upstream:<new name> (D7).
func (d *Doc) ReplaceUpstream(target string, u DocUpstream) error {
	_, i := d.Upstream(target)
	if i < 0 {
		return notFound(KindUpstream, target)
	}
	d.Upstreams[i] = u
	if u.Name != target {
		from, to := "upstream:"+target, "upstream:"+u.Name
		for k := range d.Rules {
			if d.Rules[k].Action == from {
				d.Rules[k].Action = to
			}
		}
		if d.Proxy.Default == from {
			d.Proxy.Default = to
		}
		if d.Remote.Default.Set && d.Remote.Default.V == from {
			d.Remote.Default.V = to
		}
	}
	return nil
}

// DeleteUpstream removes the upstream named target. While an action or a
// default uses it, the error is an *InUseError that lists them.
func (d *Doc) DeleteUpstream(target string) error {
	_, i := d.Upstream(target)
	if i < 0 {
		return notFound(KindUpstream, target)
	}
	if users := d.UpstreamUsers(target); len(users) > 0 {
		return &InUseError{Kind: KindUpstream, Name: target, Users: users}
	}
	d.Upstreams = slices.Delete(d.Upstreams, i, i+1)
	return nil
}

// UpstreamUsers lists the rules, in order, and then the defaults whose
// action is upstream:<name>.
func (d *Doc) UpstreamUsers(name string) []Ref {
	action := "upstream:" + name
	var out []Ref
	for _, r := range d.Rules {
		if r.Action == action {
			out = append(out, Ref{Kind: KindRule, Name: r.Name})
		}
	}
	if d.Proxy.Default == action {
		out = append(out, Ref{Kind: KindKey, Name: "proxy.default"})
	}
	if d.Remote.Default.Set && d.Remote.Default.V == action {
		out = append(out, Ref{Kind: KindKey, Name: "proxy.remote.default"})
	}
	return out
}

// AddInterface appends x.
func (d *Doc) AddInterface(x DocInterface) { d.Interfaces = append(d.Interfaces, x) }

// ReplaceInterface replaces the definition named target with x. When x has
// another name, every interfaces list that names target names x instead
// (D7).
func (d *Doc) ReplaceInterface(target string, x DocInterface) error {
	_, i := d.Interface(target)
	if i < 0 {
		return notFound(KindInterface, target)
	}
	d.Interfaces[i] = x
	if x.Name != target {
		rename := func(o *Opt[[]string]) {
			if k := slices.Index(o.V, target); k >= 0 {
				o.V = slices.Clone(o.V)
				o.V[k] = x.Name
			}
		}
		rename(&d.Proxy.Interfaces)
		for k := range d.Rules {
			rename(&d.Rules[k].Interfaces)
		}
		for k := range d.Upstreams {
			rename(&d.Upstreams[k].Interfaces)
		}
	}
	return nil
}

// DeleteInterface removes the definition named target. While an
// interfaces list names it, the error is an *InUseError that lists them.
func (d *Doc) DeleteInterface(target string) error {
	_, i := d.Interface(target)
	if i < 0 {
		return notFound(KindInterface, target)
	}
	if users := d.InterfaceUsers(target); len(users) > 0 {
		return &InUseError{Kind: KindInterface, Name: target, Users: users}
	}
	d.Interfaces = slices.Delete(d.Interfaces, i, i+1)
	return nil
}

// InterfaceUsers lists the interfaces lists that name the definition:
// proxy.interfaces, then the rules and the upstreams in order, each with
// the definition's position in the list.
func (d *Doc) InterfaceUsers(name string) []Ref {
	var out []Ref
	if k := slices.Index(d.Proxy.Interfaces.V, name); k >= 0 {
		out = append(out, Ref{Kind: KindKey, Name: "proxy.interfaces", Pos: k + 1})
	}
	for _, r := range d.Rules {
		if k := slices.Index(r.Interfaces.V, name); k >= 0 {
			out = append(out, Ref{Kind: KindRule, Name: r.Name, Pos: k + 1})
		}
	}
	for _, u := range d.Upstreams {
		if k := slices.Index(u.Interfaces.V, name); k >= 0 {
			out = append(out, Ref{Kind: KindUpstream, Name: u.Name, Pos: k + 1})
		}
	}
	return out
}

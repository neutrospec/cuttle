package config

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Span is a piece of a summary line. Code spans hold names, key paths, and
// values, which a page shows as code.
type Span struct {
	Text string
	Code bool
}

// SummaryLine is one line of a change summary (design 0009 D7).
type SummaryLine []Span

// String returns the line as text with code spans in backticks.
func (l SummaryLine) String() string {
	var b strings.Builder
	for _, s := range l {
		if s.Code {
			b.WriteString("`" + s.Text + "`")
		} else {
			b.WriteString(s.Text)
		}
	}
	return b.String()
}

// line builds a SummaryLine from alternating text and code: the first
// argument is text, the second code, and so on.
func line(parts ...string) SummaryLine {
	var l SummaryLine
	for i, p := range parts {
		if p != "" {
			l = append(l, Span{Text: p, Code: i%2 == 1})
		}
	}
	return l
}

// Hint tells Summarize that the edit renamed the entity of Kind
// (KindRule, KindUpstream, or KindInterface) From To, which the two models
// alone cannot tell from a delete and an add.
type Hint struct {
	Kind     string
	From, To string
}

// Summarize describes the change from old to new, one line per changed
// entity or key (design 0009 D7): the rename of the hint with the
// references it rewrote; then rules, upstreams, and interfaces by name,
// added, removed, or with their changed keys; rule moves, from the longest
// common subsequence of the rule names, so that one move reads as one
// line; and each changed key of the other tables with its old and new
// value. A line never shows a MAC address or a note.
func Summarize(old, new *Doc, hint Hint) []SummaryLine {
	s := &summary{hint: hint}
	s.rules(old, new)
	s.upstreams(old, new)
	s.interfaces(old, new)
	s.settings(old, new)
	if l := s.renameLine(); l != nil {
		return append([]SummaryLine{l}, s.out...)
	}
	return s.out
}

type summary struct {
	hint Hint
	out  []SummaryLine
	// renamed is the line of the hint's rename, and the ref fields count
	// the references that the rename rewrote.
	renamed                SummaryLine
	refRules, refUpstreams int
	refKeys                []string
}

// oldName maps a name of the new model to the old one.
func (s *summary) oldName(kind, name string) string {
	if s.hint.Kind == kind && s.hint.To == name && s.hint.From != "" {
		return s.hint.From
	}
	return name
}

// renamedAction reports whether an action changed only by the hint's
// rename of an upstream.
func (s *summary) renamedAction(old, new string) bool {
	return s.hint.Kind == KindUpstream && s.hint.From != s.hint.To &&
		old == "upstream:"+s.hint.From && new == "upstream:"+s.hint.To
}

// renamedList reports whether an interfaces list changed only by the
// hint's rename of an interface definition.
func (s *summary) renamedList(old, new Opt[[]string]) bool {
	if s.hint.Kind != KindInterface || s.hint.From == s.hint.To || old.Set != new.Set || len(old.V) != len(new.V) {
		return false
	}
	k := slices.Index(old.V, s.hint.From)
	if k < 0 || new.V[k] != s.hint.To {
		return false
	}
	o := slices.Clone(old.V)
	o[k] = s.hint.To
	return slices.Equal(o, new.V)
}

// diff returns "old → new" for a changed key, or "" when it is unchanged.
func diff[T comparable](a, b Opt[T]) string {
	if a == b {
		return ""
	}
	return show(a) + " → " + show(b)
}

// listDiff is diff for a list.
func listDiff(a, b Opt[[]string]) string {
	if a.Set == b.Set && slices.Equal(a.V, b.V) {
		return ""
	}
	return show(a) + " → " + show(b)
}

// show writes a value of a summary line: "default" for an unset key.
func show[T any](o Opt[T]) string {
	if !o.Set {
		return "default"
	}
	switch v := any(o.V).(type) {
	case string:
		if v == "" {
			return `""`
		}
		return v
	case []string:
		if len(v) == 0 {
			return "[]"
		}
		return strings.Join(v, ", ")
	}
	return fmt.Sprint(o.V)
}

func intStrings(o Opt[[]int64]) Opt[[]string] {
	out := Opt[[]string]{Set: o.Set}
	for _, n := range o.V {
		out.V = append(out.V, strconv.FormatInt(n, 10))
	}
	return out
}

// keyDiff names the key of a changed value of an entity: "action direct →
// block".
func keyDiff(key, d string) string {
	if d == "" {
		return ""
	}
	return key + " " + d
}

// hidden names a changed key whose value a summary never shows.
func hidden(what, d string) string {
	if d == "" {
		return ""
	}
	return what + " changed"
}

// listChange describes a changed list of an entity as the entries added
// and removed, or as reordered.
func listChange(key string, a, b Opt[[]string]) string {
	if listDiff(a, b) == "" {
		return ""
	}
	add, rem := 0, 0
	for _, v := range b.V {
		if !slices.Contains(a.V, v) {
			add++
		}
	}
	for _, v := range a.V {
		if !slices.Contains(b.V, v) {
			rem++
		}
	}
	var parts []string
	if add > 0 {
		parts = append(parts, "+"+strconv.Itoa(add))
	}
	if rem > 0 {
		parts = append(parts, "−"+strconv.Itoa(rem))
	}
	switch {
	case len(parts) > 0:
		return key + " " + strings.Join(parts, " ")
	case a.Set == b.Set:
		return key + " reordered"
	}
	return key + " " + listDiff(a, b)
}

func ruleChanges(s *summary, a, b DocRule) []string {
	c := []string{
		hidden("note", diff(a.Note, b.Note)),
		listChange("hosts", a.Hosts, b.Hosts),
		listChange("domains", a.Domains, b.Domains),
		listChange("cidrs", a.CIDRs, b.CIDRs),
		listChange("ports", intStrings(a.Ports), intStrings(b.Ports)),
	}
	if s.renamedAction(a.Action, b.Action) {
		s.refRules++
	} else {
		c = append(c, keyDiff("action", diff(Some(a.Action), Some(b.Action))))
	}
	c = append(c,
		keyDiff("allow_loopback", diff(a.AllowLoopback, b.AllowLoopback)),
		keyDiff("allow_private", diff(a.AllowPrivate, b.AllowPrivate)))
	if s.renamedList(a.Interfaces, b.Interfaces) {
		s.refRules++
	} else {
		c = append(c, listChange("interfaces", a.Interfaces, b.Interfaces))
	}
	return c
}

func upstreamChanges(s *summary, a, b DocUpstream) []string {
	c := []string{
		hidden("note", diff(a.Note, b.Note)),
		keyDiff("address", diff(Some(a.Address), Some(b.Address))),
		keyDiff("disclosure", diff(Some(a.Disclosure), Some(b.Disclosure))),
		keyDiff("connect_timeout", diff(a.ConnectTimeout, b.ConnectTimeout)),
		keyDiff("response_timeout", diff(a.ResponseTimeout, b.ResponseTimeout)),
		keyDiff("unreachable_hold", diff(a.UnreachableHold, b.UnreachableHold)),
	}
	if s.renamedList(a.Interfaces, b.Interfaces) {
		s.refUpstreams++
	} else {
		c = append(c, listChange("interfaces", a.Interfaces, b.Interfaces))
	}
	return c
}

func interfaceChanges(_ *summary, a, b DocInterface) []string {
	return []string{
		hidden("note", diff(a.Note, b.Note)),
		hidden("hardware address", diff(a.MAC, b.MAC)),
		keyDiff("device", diff(a.Device, b.Device)),
		keyDiff("kind", diff(a.Kind, b.Kind)),
	}
}

// entities compares one array of tables by name.
func entities[T any](s *summary, kind, label string, old, new []T, name func(T) string, changes func(*summary, T, T) []string) {
	oldIdx := map[string]int{}
	for i, x := range old {
		oldIdx[name(x)] = i
	}
	newNames := map[string]bool{}
	for i, x := range new {
		n := name(x)
		on := s.oldName(kind, n)
		newNames[on] = true
		j, ok := oldIdx[on]
		if !ok {
			if kind == KindRule {
				s.out = append(s.out, line(label+" ", n, " added at position "+strconv.Itoa(i+1)))
			} else {
				s.out = append(s.out, line(label+" ", n, " added"))
			}
			continue
		}
		c := slices.DeleteFunc(changes(s, old[j], x), func(c string) bool { return c == "" })
		if len(c) > 0 {
			s.out = append(s.out, line(label+" ", n, ": "+strings.Join(c, ", ")))
		}
	}
	for _, x := range old {
		if !newNames[name(x)] {
			s.out = append(s.out, line(label+" ", name(x), " removed"))
		}
	}
	if s.hint.Kind == kind && s.hint.From != s.hint.To {
		if _, ok := oldIdx[s.hint.From]; ok {
			s.renamed = line(label+" renamed ", s.hint.From, " → ", s.hint.To)
		}
	}
}

func (s *summary) rules(old, new *Doc) {
	entities(s, KindRule, "Rule", old.Rules, new.Rules, func(r DocRule) string { return r.Name }, ruleChanges)
	// Moves: of the rules in both models, those outside a longest common
	// subsequence of the two orders moved. With distinct names, that is a
	// longest increasing subsequence of the old positions in new order,
	// which takes O(n log n) even for the thousands of rules that fit in
	// 256 KiB.
	oldPos := map[string]int{}
	for i, r := range old.Rules {
		oldPos[r.Name] = i
	}
	var common []string // old names of the rules in both, in new order
	newPos := map[string]int{}
	for i, r := range new.Rules {
		on := s.oldName(KindRule, r.Name)
		if _, ok := oldPos[on]; ok {
			common = append(common, on)
			newPos[on] = i
		}
	}
	stay := increasing(common, oldPos)
	for _, on := range common {
		if !stay[on] {
			n := new.Rules[newPos[on]].Name
			s.out = append(s.out, line("Moved rule ", n, " from "+strconv.Itoa(oldPos[on]+1)+" to "+strconv.Itoa(newPos[on]+1)))
		}
	}
}

// increasing returns the names of one longest subsequence of names whose
// positions in pos increase.
func increasing(names []string, pos map[string]int) map[string]bool {
	var tails []int // tails[k]: index of the last name of the best run of length k+1
	prev := make([]int, len(names))
	for i, n := range names {
		k := sort.Search(len(tails), func(j int) bool { return pos[names[tails[j]]] >= pos[n] })
		prev[i] = -1
		if k > 0 {
			prev[i] = tails[k-1]
		}
		if k == len(tails) {
			tails = append(tails, i)
		} else {
			tails[k] = i
		}
	}
	out := map[string]bool{}
	if len(tails) > 0 {
		for i := tails[len(tails)-1]; i >= 0; i = prev[i] {
			out[names[i]] = true
		}
	}
	return out
}

func (s *summary) upstreams(old, new *Doc) {
	entities(s, KindUpstream, "Upstream", old.Upstreams, new.Upstreams, func(u DocUpstream) string { return u.Name }, upstreamChanges)
}

func (s *summary) interfaces(old, new *Doc) {
	entities(s, KindInterface, "Interface", old.Interfaces, new.Interfaces, func(x DocInterface) string { return x.Name }, interfaceChanges)
}

// settings compares the keys of the top level, [proxy], [proxy.remote],
// [admin], [stats], and [log].
func (s *summary) settings(old, new *Doc) {
	key := func(path, d string) {
		if d != "" {
			s.out = append(s.out, line("", path, ": "+d))
		}
	}
	o, n := old.Proxy, new.Proxy
	key("name", diff(Some(old.Name), Some(new.Name)))
	key("proxy.listen", listDiff(o.Listen, n.Listen))
	if s.renamedAction(o.Default, n.Default) {
		s.refKeys = append(s.refKeys, "proxy.default")
	} else {
		key("proxy.default", diff(Some(o.Default), Some(n.Default)))
	}
	key("proxy.connect_timeout", diff(o.ConnectTimeout, n.ConnectTimeout))
	key("proxy.response_timeout", diff(o.ResponseTimeout, n.ResponseTimeout))
	key("proxy.idle_timeout", diff(o.IdleTimeout, n.IdleTimeout))
	key("proxy.max_connections", diff(o.MaxConnections, n.MaxConnections))
	if s.renamedList(o.Interfaces, n.Interfaces) {
		s.refKeys = append(s.refKeys, "proxy.interfaces")
	} else {
		key("proxy.interfaces", listDiff(o.Interfaces, n.Interfaces))
	}
	key("proxy.interface_attempt_timeout", diff(o.InterfaceAttemptTimeout, n.InterfaceAttemptTimeout))
	key("proxy.interface_demotion", diff(o.InterfaceDemotion, n.InterfaceDemotion))
	or, nr := old.Remote, new.Remote
	key("proxy.remote.sources", listDiff(or.Sources, nr.Sources))
	key("proxy.remote.ports", listDiff(intStrings(or.Ports), intStrings(nr.Ports)))
	if or.Default.Set && nr.Default.Set && s.renamedAction(or.Default.V, nr.Default.V) {
		s.refKeys = append(s.refKeys, "proxy.remote.default")
	} else {
		key("proxy.remote.default", diff(or.Default, nr.Default))
	}
	key("proxy.remote.allow_direct", diff(or.AllowDirect, nr.AllowDirect))
	key("proxy.remote.max_connections_per_source", diff(or.MaxConnectionsPerSource, nr.MaxConnectionsPerSource))
	key("admin.listen", diff(old.Admin.Listen, new.Admin.Listen))
	key("stats.retention_days", diff(old.Stats.RetentionDays, new.Stats.RetentionDays))
	key("stats.max_hosts_per_hour", diff(old.Stats.MaxHostsPerHour, new.Stats.MaxHostsPerHour))
	key("log.level", diff(old.Log.Level, new.Log.Level))
}

// renameLine returns the line of the hint's rename with the references it
// rewrote, or nil.
func (s *summary) renameLine() SummaryLine {
	if s.renamed != nil {
		var refs []string
		if s.refRules > 0 {
			refs = append(refs, plural(s.refRules, "rule"))
		}
		if s.refUpstreams > 0 {
			refs = append(refs, plural(s.refUpstreams, "upstream"))
		}
		refs = append(refs, s.refKeys...)
		if len(refs) > 0 {
			return append(s.renamed, Span{Text: "; " + joinAnd(refs) + " updated"})
		}
	}
	return s.renamed
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

// joinAnd joins items as "a", "a and b", or "a, b, and c".
func joinAnd(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + " and " + items[1]
	}
	return strings.Join(items[:len(items)-1], ", ") + ", and " + items[len(items)-1]
}

// generatedComments are the comment lines that Encode writes, which a
// count of comments written by hand leaves out.
var generatedComments = func() map[string]bool {
	m := map[string]bool{}
	for _, c := range [][]string{headerComment, proxyComment, remoteComment, adminComment, interfaceComment, upstreamComment, ruleComment} {
		for _, l := range c {
			m[l] = true
		}
	}
	return m
}()

// HandComments counts the comments written by hand in a TOML file: whole
// comment lines, other than the generated comments of the canonical form,
// and comments at the end of a line with a key or a table header. A form
// save removes them; the review says how many (design 0009 D7). The count
// is exact for any file that the TOML parser accepts, since it skips
// strings, quoted keys included.
func HandComments(data []byte) (lines, eol int) {
	src := string(data)
	lineStart := true // only blanks since the start of the line
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '\n':
			lineStart = true
			i++
		case c == ' ' || c == '\t' || c == '\r':
			i++
		case c == '#':
			end := strings.IndexByte(src[i:], '\n')
			if end < 0 {
				end = len(src) - i
			}
			text := strings.TrimRight(src[i:i+end], " \t\r")
			if !lineStart {
				eol++
			} else if !generatedComments[text] {
				lines++
			}
			i += end
		case c == '"' || c == '\'':
			lineStart = false
			i = skipString(src, i)
		default:
			lineStart = false
			i++
		}
	}
	return lines, eol
}

// skipString returns the index after the string that starts at i: a basic
// or literal string, single-line or multi-line.
func skipString(src string, i int) int {
	q := src[i]
	if strings.HasPrefix(src[i:], strings.Repeat(string(q), 3)) {
		delim := strings.Repeat(string(q), 3)
		for j := i + 3; j < len(src); j++ {
			if q == '"' && src[j] == '\\' {
				j++
				continue
			}
			if strings.HasPrefix(src[j:], delim) {
				// A closing delimiter may be followed by up to two quotes
				// that belong to the string.
				j += 3
				for k := 0; k < 2 && j < len(src) && src[j] == q; k++ {
					j++
				}
				return j
			}
		}
		return len(src)
	}
	for j := i + 1; j < len(src); j++ {
		switch src[j] {
		case '\\':
			if q == '"' {
				j++
			}
		case q, '\n':
			return j + 1
		}
	}
	return len(src)
}

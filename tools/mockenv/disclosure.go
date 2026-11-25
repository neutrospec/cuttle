package main

import (
	"bytes"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
)

// defaultTargets are the non-disclosure case targets of design 0007 D8.
var defaultTargets = []string{
	"https://" + hostOrigin + "/x",
	"http://" + hostPlain + "/x",
	"http://" + hostPlain + "/a{b}|c?x=1;y=2&z=%zz",
	"http://" + hostPlain + "/q?b=2&a=1;c=3",
}

// disclosureNames are the disclosure fields of design 0003 D5, lowercase,
// written out here from the design rather than taken from internal/proxy,
// so that the check does not share a mistake with the filter it checks.
// Every name that starts with x-forwarded is one as well.
var disclosureNames = []string{
	"via", "forwarded", "forwarded-for", "x-original-forwarded-for",
	"x-real-ip", "client-ip", "true-client-ip", "x-client-ip",
	"x-cluster-client-ip", "x-originating-ip", "cf-connecting-ip",
	"fastly-client-ip", "proxy-client-ip", "wl-proxy-client-ip",
	"x-proxyuser-ip", "x-remote-addr", "x-remote-ip",
	"x-envoy-external-address", "x-bluecoat-via", "cdn-loop", "proxy-status",
}

// isDisclosureField matches a field name as design 0003 D5 does: without
// regard to case, with "_" read as "-", and also behind a CGI-style http-
// prefix.
func isDisclosureField(name string) bool {
	n := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), "_", "-")
	listed := func(n string) bool { return strings.HasPrefix(n, "x-forwarded") || slices.Contains(disclosureNames, n) }
	return listed(n) || strings.HasPrefix(n, "http-") && listed(n[len("http-"):])
}

// disclosureOptions configure check-disclosure.
type disclosureOptions struct {
	Dir     string
	Names   []string // instance names
	Clients []string // original client addresses
	Targets []string // case target URLs
}

// caseKey is the key that matches a case target URL to recorded requests:
// CONNECT host:port for https, and the lenient canonical form of the
// absolute-form target for http, so that a client's own escaping, such as
// Chrome writing { as %7B, still matches. The exact targets are compared
// afterwards.
func caseKey(u string) (string, error) {
	scheme, rest, ok := strings.Cut(u, "://")
	if !ok {
		return "", fmt.Errorf("case target %q is not an absolute URL", u)
	}
	switch strings.ToLower(scheme) {
	case "https":
		auth := rest
		if i := strings.IndexAny(auth, "/?#"); i >= 0 {
			auth = auth[:i]
		}
		if !hasPort(auth) {
			auth += ":443"
		}
		return "CONNECT " + strings.ToLower(auth), nil
	case "http":
		return "HTTP " + canonicalHTTP(rest), nil
	}
	return "", fmt.Errorf("case target %q is neither http nor https", u)
}

// recordKey is caseKey for a recorded request; "" for a request that no
// case target can match, such as an origin-form request.
func recordKey(r record) string {
	if strings.EqualFold(r.Method, "CONNECT") {
		return "CONNECT " + strings.ToLower(r.Target)
	}
	if len(r.Target) > 7 && strings.EqualFold(r.Target[:7], "http://") {
		return "HTTP " + canonicalHTTP(r.Target[7:])
	}
	return ""
}

func hasPort(auth string) bool {
	i := strings.LastIndexByte(auth, ':')
	return i >= 0 && i > strings.LastIndexByte(auth, ']')
}

// canonicalHTTP lowercases the authority, drops the default port, and
// decodes every valid percent-escape in the rest, leaving invalid ones such
// as %zz as they are.
func canonicalHTTP(rest string) string {
	auth, pq := rest, ""
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		auth, pq = rest[:i], rest[i:]
	}
	auth = strings.TrimSuffix(strings.ToLower(auth), ":80")
	if pq == "" {
		pq = "/"
	}
	return auth + lenientUnescape(pq)
}

func lenientUnescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			b.WriteByte(unhex(s[i+1])<<4 | unhex(s[i+2]))
			i += 2
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case '0' <= c && c <= '9':
		return c - '0'
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10
	}
	return c - 'A' + 10
}

// checkDisclosure compares egress-chain.jsonl with egress-direct.jsonl in
// the session directory (design 0007 D8). Every egress-chain record must
// carry no disclosure field and no field name, value, or target containing
// an instance name or an original client address. Each case target must
// appear in both files, and each egress-chain request for it must have the
// method, target, and body of an egress-direct request for it. Requests
// outside the case targets, such as a browser's background requests, are
// not compared. Other header differences are printed for information. It
// prints every finding and returns the number of failures.
func checkDisclosure(opt disclosureOptions, w io.Writer) (int, error) {
	direct, err := readRecords(filepath.Join(opt.Dir, mockEgressDirect+".jsonl"))
	if err != nil {
		return 0, err
	}
	chain, err := readRecords(filepath.Join(opt.Dir, mockEgressChain+".jsonl"))
	if err != nil {
		return 0, err
	}
	targets := opt.Targets
	if len(targets) == 0 {
		targets = defaultTargets
	}
	keys := map[string]string{} // key → case target
	var order []string
	for _, t := range targets {
		k, err := caseKey(t)
		if err != nil {
			return 0, err
		}
		if _, dup := keys[k]; !dup {
			order = append(order, k)
		}
		keys[k] = t
	}

	fails := 0
	fail := func(format string, a ...any) {
		fails++
		fmt.Fprintf(w, "FAIL "+format+"\n", a...)
	}
	info := func(format string, a ...any) { fmt.Fprintf(w, "info "+format+"\n", a...) }

	needles := []string{}
	for _, n := range append(append([]string{}, opt.Names...), opt.Clients...) {
		if n = strings.TrimSpace(n); n != "" {
			needles = append(needles, n)
		}
	}
	containsNeedle := func(s string) string {
		ls := strings.ToLower(s)
		for _, n := range needles {
			if strings.Contains(ls, strings.ToLower(n)) {
				return n
			}
		}
		return ""
	}
	where := func(r record) string {
		return fmt.Sprintf("egress-chain #%d (%s %s)", r.Seq, r.Method, r.Target)
	}

	// Every request that reached the egress through the chain.
	var outside []string
	for _, r := range chain {
		for _, f := range append(append([]field{}, r.Fields...), r.Trailer...) {
			if isDisclosureField(f.Name) {
				fail("%s: disclosure field %s: %q", where(r), f.Name, f.Value)
			}
			if n := containsNeedle(f.Name); n != "" {
				fail("%s: field name %q contains %q", where(r), f.Name, n)
			}
			if n := containsNeedle(f.Value); n != "" {
				fail("%s: field %s: %q contains %q", where(r), f.Name, f.Value, n)
			}
		}
		if n := containsNeedle(r.Target); n != "" {
			fail("%s: target contains %q", where(r), n)
		}
		if _, ok := keys[recordKey(r)]; !ok {
			outside = append(outside, r.Method+" "+r.Target)
		}
	}

	// The case targets, compared with the direct run.
	for _, k := range order {
		t := keys[k]
		var d, c []record
		for _, r := range direct {
			if recordKey(r) == k {
				d = append(d, r)
			}
		}
		for _, r := range chain {
			if recordKey(r) == k {
				c = append(c, r)
			}
		}
		switch {
		case len(c) == 0 && len(d) == 0:
			fail("case %s: missing from both %s.jsonl and %s.jsonl", t, mockEgressDirect, mockEgressChain)
			continue
		case len(c) == 0:
			fail("case %s: missing from %s.jsonl", t, mockEgressChain)
			continue
		case len(d) == 0:
			fail("case %s: unmatched: %s.jsonl has it, %s.jsonl does not", t, mockEgressChain, mockEgressDirect)
			continue
		}
		for _, r := range c {
			ref, exact := pickDirect(r, d)
			if !exact {
				if r.Method != ref.Method {
					fail("case %s: %s: method %q, direct %q", t, where(r), r.Method, ref.Method)
				}
				if r.Target != ref.Target {
					fail("case %s: %s: target %q, direct %q", t, where(r), r.Target, ref.Target)
				}
				if !bytes.Equal(r.Body, ref.Body) {
					fail("case %s: %s: body %q, direct %q", t, where(r), r.Body, ref.Body)
				}
			}
			for _, diff := range fieldDiffs(ref.Fields, r.Fields) {
				info("case %s: %s: %s", t, where(r), diff)
			}
		}
		fmt.Fprintf(w, "case %s: %d direct, %d chain\n", t, len(d), len(c))
	}
	if len(outside) > 0 {
		info("%d egress-chain requests outside the case targets, checked for disclosure only: %s", len(outside), strings.Join(outside, "; "))
	}
	verdict := "pass"
	if fails > 0 {
		verdict = "FAIL"
	}
	fmt.Fprintf(w, "check-disclosure %s: %d case targets, %d egress-direct and %d egress-chain requests, %d failures\n",
		verdict, len(order), len(direct), len(chain), fails)
	return fails, nil
}

// pickDirect returns the direct request with the same method, target, and
// body as r, or else the closest one: the same method and target, then the
// same target, then the first.
func pickDirect(r record, d []record) (record, bool) {
	for _, x := range d {
		if x.Method == r.Method && x.Target == r.Target && bytes.Equal(x.Body, r.Body) {
			return x, true
		}
	}
	for _, x := range d {
		if x.Method == r.Method && x.Target == r.Target {
			return x, false
		}
	}
	for _, x := range d {
		if x.Target == r.Target {
			return x, false
		}
	}
	return d[0], false
}

// fieldDiffs lists the fields only one side has and the values that differ,
// by name without regard to case.
func fieldDiffs(direct, chain []field) []string {
	collect := func(fs []field) (map[string][]string, []string) {
		m := map[string][]string{}
		var names []string
		for _, f := range fs {
			k := strings.ToLower(f.Name)
			if _, ok := m[k]; !ok {
				names = append(names, k)
			}
			m[k] = append(m[k], f.Value)
		}
		return m, names
	}
	dm, dn := collect(direct)
	cm, cn := collect(chain)
	var out []string
	for _, k := range dn {
		switch cv, ok := cm[k]; {
		case !ok:
			out = append(out, fmt.Sprintf("field %s only in the direct request", k))
		case !slices.Equal(dm[k], cv):
			out = append(out, fmt.Sprintf("field %s: direct %q, chain %q", k, dm[k], cv))
		}
	}
	for _, k := range cn {
		if _, ok := dm[k]; !ok {
			out = append(out, fmt.Sprintf("field %s only in the chain request", k))
		}
	}
	return out
}

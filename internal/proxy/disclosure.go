package proxy

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"squid/internal/netx"
)

// disclosureFields are the fields that reveal an intermediary or the
// original client address (design 0003 D5), lowercase. Every field whose
// name starts with x-forwarded- is also one.
var disclosureFields = []string{
	"via",
	"forwarded",
	"x-real-ip",
	"client-ip",
	"true-client-ip",
	"x-client-ip",
	"x-cluster-client-ip",
	"x-originating-ip",
	"cf-connecting-ip",
	"fastly-client-ip",
	"proxy-status",
}

// isDisclosureField matches names without regard to case and with "_"
// read as "-", because some servers treat X_Forwarded_For like
// X-Forwarded-For.
func isDisclosureField(name string) bool {
	n := strings.ReplaceAll(strings.ToLower(name), "_", "-")
	return strings.HasPrefix(n, "x-forwarded-") || slices.Contains(disclosureFields, n)
}

// removeDisclosure is the disclosure filter: it deletes every disclosure
// field, the client's own included. It runs on direct routes and toward
// disclosure = "none" upstreams, from the plain-HTTP Rewrite and from the
// CONNECT head writer (design 0003 D5).
func removeDisclosure(h http.Header) {
	for k := range h {
		if isDisclosureField(k) {
			delete(h, k)
		}
	}
}

// viaEntry is cuttle's own Via list member (design 0003 D2).
func viaEntry(name string) string { return "1.1 " + name + " (cuttle)" }

// viaNames reports whether any Via member's received-by token, the
// client's and cuttle's alike, equals name without regard to case
// (design 0003 D2). It runs before any filtering.
func viaNames(h http.Header, name string) bool {
	for _, v := range h.Values("Via") {
		for _, m := range splitVia(v) {
			f := strings.Fields(m)
			if len(f) >= 2 && strings.EqualFold(f[1], name) {
				return true
			}
		}
	}
	return false
}

// splitVia splits a Via field value into its members on commas outside
// comments.
func splitVia(v string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(v); i++ {
		switch v[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				out = append(out, v[start:i])
				start = i + 1
			}
		}
	}
	return append(out, v[start:])
}

func isDialError(err error) bool {
	var de *netx.DialError
	return errors.As(err, &de)
}

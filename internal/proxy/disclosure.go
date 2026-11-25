package proxy

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/neutrospec/cuttle/internal/netx"
)

// disclosureFields are the fields that reveal an intermediary or the
// original client address (design 0003 D5), lowercase. Every field whose
// name starts with x-forwarded, with or without a following dash, is also
// one. The list cannot be complete; the guarantee covers these names.
var disclosureFields = []string{
	"via",
	"forwarded",
	"forwarded-for",
	"x-original-forwarded-for",
	"x-real-ip",
	"client-ip",
	"true-client-ip",
	"x-client-ip",
	"x-cluster-client-ip",
	"x-originating-ip",
	"cf-connecting-ip",
	"fastly-client-ip",
	"proxy-status",
	"cdn-loop",
	"proxy-client-ip",
	"wl-proxy-client-ip",
	"x-proxyuser-ip",
	"x-remote-addr",
	"x-remote-ip",
	"x-envoy-external-address",
	"x-bluecoat-via",
}

// cgiPrefix is the prefix CGI-style servers put before a field name, as in
// HTTP_X_FORWARDED_FOR; with "_" read as "-" it is "http-".
const cgiPrefix = "http-"

// isDisclosureField matches names without regard to case and with "_"
// read as "-", because some servers treat X_Forwarded_For like
// X-Forwarded-For, and also behind the CGI prefix, as in
// HTTP_X_FORWARDED_FOR.
func isDisclosureField(name string) bool {
	n := strings.ReplaceAll(strings.ToLower(name), "_", "-")
	return listedDisclosure(n) || (strings.HasPrefix(n, cgiPrefix) && listedDisclosure(n[len(cgiPrefix):]))
}

// listedDisclosure reports whether a normalized name is listed or has the
// x-forwarded prefix.
func listedDisclosure(n string) bool {
	return strings.HasPrefix(n, "x-forwarded") || slices.Contains(disclosureFields, n)
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

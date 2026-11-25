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
// field, the client's own included. It runs on direct routes and, from
// M1.3, toward disclosure = "none" upstreams.
func removeDisclosure(h http.Header) {
	for k := range h {
		if isDisclosureField(k) {
			delete(h, k)
		}
	}
}

func isDialError(err error) bool {
	var de *netx.DialError
	return errors.As(err, &de)
}

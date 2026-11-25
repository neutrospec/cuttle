package proxy

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"squid/internal/netx"
)

// disclosureFields are the fields that reveal an intermediary or the
// original client address (design 0003 D5), in canonical form. Every field
// whose name starts with X-Forwarded- is also one.
var disclosureFields = []string{
	"Via",
	"Forwarded",
	"X-Real-Ip",
	"Client-Ip",
	"True-Client-Ip",
	"X-Client-Ip",
	"X-Cluster-Client-Ip",
	"X-Originating-Ip",
	"Cf-Connecting-Ip",
	"Fastly-Client-Ip",
	"Proxy-Status",
}

// removeDisclosure is the disclosure filter: it deletes every disclosure
// field, the client's own included. It runs on direct routes and, from
// M1.3, toward disclosure = "none" upstreams.
func removeDisclosure(h http.Header) {
	for k := range h {
		ck := http.CanonicalHeaderKey(k)
		if strings.HasPrefix(ck, "X-Forwarded-") || slices.Contains(disclosureFields, ck) {
			delete(h, k)
		}
	}
}

func isDialError(err error) bool {
	var de *netx.DialError
	return errors.As(err, &de)
}

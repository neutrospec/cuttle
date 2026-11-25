package proxy

import (
	"context"
	"net/http"
	"net/http/httputil"
	"testing"

	"squid/internal/authority"
)

func TestRemoveDisclosure(t *testing.T) {
	h := http.Header{}
	for k, v := range clientDisclosure {
		h.Set(k, v)
	}
	h["x-forwarded-lowercase"] = []string{"1"} // set directly, not canonical
	h["X-Real-IP"] = []string{"1"}
	h.Set("X-Forwarded", "kept: not a prefix match")
	h.Set("X-Kept", "yes")
	h.Set("User-Agent", "ua")
	removeDisclosure(h)
	if len(h) != 3 || h.Get("X-Kept") != "yes" || h.Get("User-Agent") != "ua" || h.Get("X-Forwarded") == "" {
		t.Fatalf("after filter: %v", h)
	}
}

func TestSFString(t *testing.T) {
	if got := sfString(`blocked by rule "a\b"` + "\x00é"); got != `"blocked by rule \"a\\b\""` {
		t.Errorf("got %s", got)
	}
}

// TestRewriteDirect checks the raw-target table of design 0002 D5 without
// sockets: the URL fields that make the transport write the client's raw
// target.
func TestRewriteDirect(t *testing.T) {
	for _, c := range []struct {
		target string
		want   string // what URL.RequestURI() yields, as the transport writes it
	}{
		{"http://h/a{b}|c?x=1;y=2&z=%zz", "/a{b}|c?x=1;y=2&z=%zz"},
		{"http://h/q?b=2&a=1;c=3", "/q?b=2&a=1;c=3"},
		{"http://h/p?", "/p?"},
		{"http://h//x", "http://h//x"},
		{"http://h//x?y", "http://h//x?y"},
		{"http://h", "/"},
		{"http://h?q", "/?q"},
		{"http://Up.Example:8080/%2F%41", "/%2F%41"},
	} {
		ht, err := authority.ParseHTTP(c.target)
		if err != nil {
			t.Fatal(err)
		}
		in, err := http.NewRequest("GET", "http://placeholder/", nil)
		if err != nil {
			t.Fatal(err)
		}
		in.Host = ht.RawAuthority
		ex := &exchange{http: ht}
		in = in.WithContext(context.WithValue(in.Context(), exchangeKey{}, ex))
		out := in.Clone(in.Context())
		out.Header.Set("Via", "1.1 lab-gw")
		rewriteDirect(&httputil.ProxyRequest{In: in, Out: out})
		if got := out.URL.RequestURI(); got != c.want {
			t.Errorf("%s: RequestURI %q, want %q", c.target, got, c.want)
		}
		if out.URL.Host != ht.Authority.String() || out.Host != ht.RawAuthority || out.Header.Get("Via") != "" {
			t.Errorf("%s: host %q/%q via %q", c.target, out.URL.Host, out.Host, out.Header.Get("Via"))
		}
	}
}

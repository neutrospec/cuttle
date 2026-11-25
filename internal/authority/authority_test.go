package authority

import (
	"strings"
	"testing"
)

// TestAuthorityTable checks every row of the design 0002 D3 table that
// cuttle's own parser decides (C7). The rows answered by Go's server before
// the handler, the bracketless IPv6 CONNECT and an invalid escape in the
// path, are T1 checks in internal/proxy.
func TestAuthorityTable(t *testing.T) {
	connect := []struct {
		target string
		want   string // normalized authority, or "" for 400
	}{
		{"Example.COM:443", "example.com:443"},
		{"example.com.:443", "example.com:443"},
		{"[2001:DB8::1]:443", "[2001:db8::1]:443"},
		{"[::ffff:192.0.2.10]:443", "192.0.2.10:443"},
		{"192.0.2.10:443", "192.0.2.10:443"},
		{"a_b-c.example:8443", "a_b-c.example:8443"},
		{"[fe80::1%25en0]:443", ""},
		{"[fe80::1%en0]:443", ""},
		{"127.1:443", ""},
		{"2130706433:443", ""},
		{"0x7f.1:443", ""},
		{"0x7f:443", ""},
		{"example.0x1f:443", ""},
		{"example.123:443", ""},
		{"01.02.03.04:443", ""},
		{"bücher.example:443", ""},
		{"xn--bcher-kva.example:443", "xn--bcher-kva.example:443"},
		{"example.com", ""},
		{"example.com:", ""},
		{"example.com:0443", ""},
		{"example.com:0", ""},
		{"example.com:65536", ""},
		{"example.com:+443", ""},
		{"example.com:65535", "example.com:65535"},
		{"user@example.com:443", ""},
		{"/rpc", ""},
		{"2001:db8::1:443", ""},
		{"[2001:db8::1]", ""},
		{"[192.0.2.1]:443", ""},
		{":443", ""},
		{"a..b:443", ""},
		{"example.com..:443", ""},
		{strings.Repeat("a", 64) + ".example:443", ""},
		{strings.Repeat("a", 63) + ".example:443", strings.Repeat("a", 63) + ".example:443"},
		{strings.Repeat("abcdefghi.", 26) + "abc:443", ""}, // 263 bytes
		{"exa mple.com:443", ""},
		{"*.example.com:443", ""},
	}
	for _, c := range connect {
		a, err := ParseConnect(c.target)
		got := ""
		if err == nil {
			got = a.String()
		}
		if got != c.want {
			t.Errorf("CONNECT %q: got %q (err %v), want %q", c.target, got, err, c.want)
		}
	}

	httpTargets := []struct {
		target      string
		want        string // normalized authority, or "" for 400
		rawPath     string
		rawQuery    string
		hasQuery    bool
		rawAuthHost string
	}{
		{"http://Plain.Example/a", "plain.example:80", "/a", "", false, "Plain.Example"},
		{"HTTP://plain.example/a", "plain.example:80", "/a", "", false, "plain.example"},
		{"http://h/a{b}|c?x=1;y=2&z=%zz", "h:80", "/a{b}|c", "x=1;y=2&z=%zz", true, "h"},
		{"http://h/p?", "h:80", "/p", "", true, "h"},
		{"http://h//x", "h:80", "//x", "", false, "h"},
		{"http://h", "h:80", "", "", false, "h"},
		{"http://h?q=1", "h:80", "", "q=1", true, "h"},
		{"http://h:8080/", "h:8080", "/", "", false, "h:8080"},
		{"http://h:/", "h:80", "/", "", false, "h:"},
		{"http://[2001:DB8::1]:8080/", "[2001:db8::1]:8080", "/", "", false, "[2001:DB8::1]:8080"},
		{"http://192.0.2.1/", "192.0.2.1:80", "/", "", false, "192.0.2.1"},
		{"http://h:0080/", "", "", "", false, ""},
		{"https://example.com/", "", "", "", false, ""},
		{"ftp://example.com/", "", "", "", false, ""},
		{"/", "", "", "", false, ""},
		{"/index.html", "", "", "", false, ""},
		{"*", "", "", "", false, ""},
		{"http://user:pw@h/", "", "", "", false, ""},
		{"http://user@h/", "", "", "", false, ""},
		{"example.com:80", "", "", "", false, ""},
		{"http:///x", "", "", "", false, ""},
		{"http://127.1/", "", "", "", false, ""},
	}
	for _, c := range httpTargets {
		ht, err := ParseHTTP(c.target)
		got := ""
		if err == nil {
			got = ht.String()
		}
		if got != c.want {
			t.Errorf("GET %q: got %q (err %v), want %q", c.target, got, err, c.want)
			continue
		}
		if err != nil {
			continue
		}
		if ht.RawPath != c.rawPath || ht.RawQuery != c.rawQuery || ht.HasQuery != c.hasQuery || ht.RawAuthority != c.rawAuthHost {
			t.Errorf("GET %q: raw parts %q %q %q %v, want %q %q %q %v", c.target,
				ht.RawAuthority, ht.RawPath, ht.RawQuery, ht.HasQuery,
				c.rawAuthHost, c.rawPath, c.rawQuery, c.hasQuery)
		}
	}
}

func TestErrorReasonNeverEchoesTarget(t *testing.T) {
	for _, target := range []string{"/secret-path?token=abc", "https://secret.example/x", "http://user:secret@h/"} {
		_, err := ParseHTTP(target)
		if err == nil {
			t.Fatalf("%q accepted", target)
		}
		if strings.Contains(err.Error(), "secret") {
			t.Errorf("error for %q reflects the target: %v", target, err)
		}
	}
}

func TestCheckPattern(t *testing.T) {
	for _, c := range []struct {
		pattern string
		ok      bool
		hint    string
	}{
		{"example.com", true, ""},
		{"wiki.internal.example", true, ""},
		{"xn--bcher-kva.example", true, ""},
		{"lan", true, ""},
		{"*.example.com", false, `"example.com" in domains`},
		{".example.com", false, `"example.com" in domains`},
		{"bücher.example", false, "A-label"},
		{"192.0.2.1", false, `"192.0.2.1/32" in cidrs`},
		{"2001:db8::1", false, `"2001:db8::1/128" in cidrs`},
		{"Example.com", false, `"example.com"`},
		{"example.com.", false, "trailing dot"},
		{"127.1", false, "numeric"},
		{"", false, "empty"},
	} {
		err := CheckPattern(c.pattern)
		if (err == nil) != c.ok {
			t.Errorf("CheckPattern(%q) = %v, want ok=%v", c.pattern, err, c.ok)
			continue
		}
		if err != nil && !strings.Contains(err.Error(), c.hint) {
			t.Errorf("CheckPattern(%q) = %v, want hint %q", c.pattern, err, c.hint)
		}
	}
}

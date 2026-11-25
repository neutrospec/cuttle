package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// baseRecords returns matching direct and chain recordings of the case
// targets, as a client and the chain would leave them: the direct requests
// carry the client's Proxy-Connection, which cuttle removes; Chrome writes {
// and } escaped; and each file holds a request outside the case targets.
func baseRecords() (direct, chain []record) {
	ua := field{"User-Agent", "probe/1.0"}
	req := func(method, target string, body string, fs ...field) record {
		r := record{Method: method, Target: target, Proto: "HTTP/1.1", Fields: fs}
		if body != "" {
			r.Body = []byte(body)
		}
		return r
	}
	pc := field{"Proxy-Connection", "keep-alive"}
	host := field{"Host", hostPlain}
	direct = []record{
		req("CONNECT", "accounts.example:443", "", field{"Host", "accounts.example:443"}, ua, pc),
		req("CONNECT", hostOrigin+":443", "", field{"Host", hostOrigin + ":443"}, ua, pc),
		req("GET", "http://"+hostPlain+"/x", "", host, ua, pc),
		req("GET", "http://"+hostPlain+"/a%7Bb%7D|c?x=1;y=2&z=%zz", "", host, ua, pc),
		req("GET", "http://"+hostPlain+"/q?b=2&a=1;c=3", "", host, ua, pc),
		req("POST", "http://"+hostPlain+"/post", "k=v&n=1", host, ua, field{"Content-Length", "7"}, pc),
	}
	chain = []record{
		req("CONNECT", hostOrigin+":443", "", field{"Host", hostOrigin + ":443"}, ua),
		req("GET", "http://"+hostPlain+"/x", "", host, ua),
		req("GET", "http://"+hostPlain+"/a%7Bb%7D|c?x=1;y=2&z=%zz", "", host, ua),
		req("GET", "http://"+hostPlain+"/q?b=2&a=1;c=3", "", host, ua),
		req("POST", "http://"+hostPlain+"/post", "k=v&n=1", host, ua, field{"Content-Length", "7"}),
		req("GET", "http://"+hostPlain+"/favicon.ico", "", host, ua),
	}
	return direct, chain
}

var testTargets = append(append([]string{}, defaultTargets...), "http://"+hostPlain+"/post")

func writeSession(t *testing.T, direct, chain []record) string {
	t.Helper()
	dir := t.TempDir()
	for name, rs := range map[string][]record{mockEgressDirect: direct, mockEgressChain: chain} {
		var b bytes.Buffer
		for i, r := range rs {
			r.Seq = i + 1
			r.Conn = 1
			data, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			b.Write(append(data, '\n'))
		}
		if err := os.WriteFile(filepath.Join(dir, name+".jsonl"), b.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func findTarget(t *testing.T, rs []record, target string) int {
	t.Helper()
	for i, r := range rs {
		if r.Target == target {
			return i
		}
	}
	t.Fatalf("no record with target %s", target)
	return -1
}

func without(rs []record, i int) []record { return append(append([]record{}, rs[:i]...), rs[i+1:]...) }

// TestCheckDisclosure checks E7: check-disclosure reports and fails on each
// planted failure kind, and passes on identical recordings, on header
// differences that reveal nothing, and on requests outside the case targets
// that only one side has.
func TestCheckDisclosure(t *testing.T) {
	const client = "198.51.100.7"
	x := "http://" + hostPlain + "/x"
	braces := "http://" + hostPlain + "/a%7Bb%7D|c?x=1;y=2&z=%zz"
	q := "http://" + hostPlain + "/q?b=2&a=1;c=3"
	post := "http://" + hostPlain + "/post"
	addField := func(target string, f field) func(d, c []record) ([]record, []record) {
		return func(d, c []record) ([]record, []record) {
			i := findTarget(t, c, target)
			c[i].Fields = append(c[i].Fields, f)
			return d, c
		}
	}
	cases := []struct {
		name   string
		mutate func(d, c []record) ([]record, []record)
		want   string // "" expects a pass
	}{
		{"identical", nil, ""},
		{"header difference that reveals nothing", func(d, c []record) ([]record, []record) {
			c[findTarget(t, c, x)].Fields = append(c[findTarget(t, c, x)].Fields, field{"Accept", "*/*"})
			return d, c
		}, ""},
		{"Via", addField(hostOrigin+":443", field{"Via", "1.1 hop"}), "disclosure field Via"},
		{"X-Forwarded-For", addField(x, field{"X-Forwarded-For", "192.0.2.9"}), "disclosure field X-Forwarded-For"},
		{"bare X-Forwarded", addField(x, field{"X-Forwarded", "for=192.0.2.9"}), "disclosure field X-Forwarded"},
		{"underscore spelling", addField(x, field{"x_forwarded_for", "192.0.2.9"}), "disclosure field x_forwarded_for"},
		{"CGI spelling", addField(q, field{"HTTP_CLIENT_IP", "192.0.2.9"}), "disclosure field HTTP_CLIENT_IP"},
		{"Forwarded", addField(q, field{"Forwarded", "for=192.0.2.9"}), "disclosure field Forwarded"},
		{"Proxy-Status", addField(post, field{"Proxy-Status", "hop"}), "disclosure field Proxy-Status"},
		{"CDN-Loop", addField(post, field{"CDN-Loop", "hop"}), "disclosure field CDN-Loop"},
		{"X-Real-IP", addField(braces, field{"X-Real-IP", "192.0.2.9"}), "disclosure field X-Real-IP"},
		{"disclosure field in a trailer", func(d, c []record) ([]record, []record) {
			i := findTarget(t, c, post)
			c[i].Trailer = []field{{"Via", "1.1 hop"}}
			return d, c
		}, "disclosure field Via"},
		{"disclosure field outside the case targets", addField("http://"+hostPlain+"/favicon.ico", field{"Via", "1.1 hop"}), "disclosure field Via"},
		{"instance name in a value", addField(x, field{"User-Agent", "probe CUTTLE-EDGE/1"}), `contains "cuttle-edge"`},
		{"instance name in a field name", addField(x, field{"X-cuttle-gateway", "1"}), `field name "X-cuttle-gateway" contains "cuttle-gateway"`},
		{"client address in a value", addField(q, field{"X-Note", "from " + client + ":5000"}), `contains "` + client + `"`},
		{"changed method", func(d, c []record) ([]record, []record) {
			c[findTarget(t, c, x)].Method = "PUT"
			return d, c
		}, `method "PUT", direct "GET"`},
		{"changed target", func(d, c []record) ([]record, []record) {
			// Re-escaping | still matches the case, so the exact target
			// comparison must catch it.
			c[findTarget(t, c, braces)].Target = "http://" + hostPlain + "/a%7Bb%7D%7Cc?x=1;y=2&z=%zz"
			return d, c
		}, `target "http://plain.cuttle.test/a%7Bb%7D%7Cc?x=1;y=2&z=%zz", direct`},
		{"changed body", func(d, c []record) ([]record, []record) {
			c[findTarget(t, c, post)].Body = []byte("k=w&n=1")
			return d, c
		}, `body "k=w&n=1", direct "k=v&n=1"`},
		{"unmatched case target", func(d, c []record) ([]record, []record) {
			return without(d, findTarget(t, d, q)), c
		}, "unmatched: egress-chain.jsonl has it, egress-direct.jsonl does not"},
		{"case target missing from the chain", func(d, c []record) ([]record, []record) {
			return d, without(c, findTarget(t, c, hostOrigin+":443"))
		}, "missing from egress-chain.jsonl"},
		{"case target missing from both", func(d, c []record) ([]record, []record) {
			return without(d, findTarget(t, d, x)), without(c, findTarget(t, c, x))
		}, "missing from both"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, c := baseRecords()
			if tc.mutate != nil {
				d, c = tc.mutate(d, c)
			}
			dir := writeSession(t, d, c)
			var out bytes.Buffer
			fails, err := checkDisclosure(disclosureOptions{Dir: dir, Names: []string{nameEdge, nameGateway},
				Clients: []string{client}, Targets: testTargets}, &out)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if fails != 0 || !strings.Contains(out.String(), "check-disclosure pass") {
					t.Fatalf("%d failures, want a pass:\n%s", fails, out.String())
				}
				return
			}
			if fails == 0 || !strings.Contains(out.String(), "check-disclosure FAIL") {
				t.Fatalf("passed, want a failure with %q:\n%s", tc.want, out.String())
			}
			found := false
			for _, l := range strings.Split(out.String(), "\n") {
				if strings.HasPrefix(l, "FAIL ") && strings.Contains(l, tc.want) {
					found = true
				}
			}
			if !found {
				t.Errorf("no FAIL line with %q:\n%s", tc.want, out.String())
			}
		})
	}

	t.Run("command exit status", func(t *testing.T) {
		d, c := baseRecords()
		good := writeSession(t, d, c)
		c[findTarget(t, c, x)].Fields = append(c[findTarget(t, c, x)].Fields, field{"Via", "1.1 hop"})
		bad := writeSession(t, d, c)
		args := func(dir string) []string {
			a := []string{"check-disclosure", "-dir", dir}
			for _, tg := range testTargets {
				a = append(a, "-target", tg)
			}
			return a
		}
		var stdout, stderr bytes.Buffer
		if code := run(args(good), &stdout, &stderr, nil); code != 0 {
			t.Errorf("identical recordings: exit %d\n%s%s", code, stdout.String(), stderr.String())
		}
		if code := run(args(bad), &stdout, &stderr, nil); code != 1 {
			t.Errorf("planted Via: exit %d, want 1", code)
		}
		if code := run([]string{"check-disclosure", "-dir", filepath.Join(good, "none")}, &stdout, &stderr, nil); code != 2 {
			t.Errorf("missing directory: exit %d, want 2", code)
		}
		// Without -target, the D8 targets apply, so the curl-only POST
		// recorded on both sides is outside them and still passes.
		if code := run([]string{"check-disclosure", "-dir", good}, &stdout, &stderr, nil); code != 0 {
			t.Errorf("default targets: exit %d\n%s", code, stdout.String())
		}
	})
}

// TestDisclosureFieldNames checks the name matching of design 0003 D5 that
// check-disclosure applies.
func TestDisclosureFieldNames(t *testing.T) {
	for _, n := range []string{"Via", "VIA", "Forwarded", "Forwarded-For", "X-Forwarded", "X-Forwarded-Host",
		"x_forwarded_proto", "X-Forwardedfoo", "X-Real-IP", "Client-IP", "True-Client-IP", "X-Client-IP",
		"X-Cluster-Client-IP", "X-Originating-IP", "CF-Connecting-IP", "Fastly-Client-IP", "Proxy-Client-IP",
		"WL-Proxy-Client-IP", "X-ProxyUser-Ip", "X-Remote-Addr", "X-Remote-IP", "X-Envoy-External-Address",
		"X-BlueCoat-Via", "CDN-Loop", "Proxy-Status", "X-Original-Forwarded-For", "HTTP_X_FORWARDED_FOR",
		"http-client-ip", "HTTP_VIA"} {
		if !isDisclosureField(n) {
			t.Errorf("%s is not matched", n)
		}
	}
	for _, n := range []string{"Host", "User-Agent", "Accept", "Viaduct", "X-Forward", "Http-Foo", "Proxy-Connection", "X-Request-Id"} {
		if isDisclosureField(n) {
			t.Errorf("%s is matched", n)
		}
	}
}

// TestCaseKeys checks that case targets match a client's own escaping and
// the authority's case and default port, and nothing else.
func TestCaseKeys(t *testing.T) {
	key := func(u string) string {
		k, err := caseKey(u)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	same := [][2]string{
		{"http://plain.cuttle.test/a{b}|c?x=1;y=2&z=%zz", "http://PLAIN.cuttle.test:80/a%7Bb%7D%7cc?x=1;y=2&z=%zz"},
		{"http://plain.cuttle.test", "http://plain.cuttle.test/"},
		{"https://origin.cuttle.test/x", "https://ORIGIN.cuttle.test:443/other"},
	}
	for _, p := range same {
		if key(p[0]) != key(p[1]) {
			t.Errorf("%s and %s differ: %q, %q", p[0], p[1], key(p[0]), key(p[1]))
		}
	}
	differ := [][2]string{
		{"http://plain.cuttle.test/x", "http://plain.cuttle.test/y"},
		{"http://plain.cuttle.test/q?a=1", "http://plain.cuttle.test/q?a=2"},
		{"https://origin.cuttle.test/", "https://origin.cuttle.test:8443/"},
		{"https://origin.cuttle.test/", "http://origin.cuttle.test/"},
	}
	for _, p := range differ {
		if key(p[0]) == key(p[1]) {
			t.Errorf("%s and %s match: %q", p[0], p[1], key(p[0]))
		}
	}
	if k := recordKey(record{Method: "CONNECT", Target: "Origin.cuttle.test:443"}); k != key("https://origin.cuttle.test/x") {
		t.Errorf("CONNECT record key %q", k)
	}
	if k := recordKey(record{Method: "GET", Target: "/x"}); k != "" {
		t.Errorf("origin-form record key %q, want none", k)
	}
	for _, bad := range []string{"plain.cuttle.test/x", "ftp://plain.cuttle.test/"} {
		if _, err := caseKey(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

// TestEvidenceParsers checks the parsers the M1 client check reads its
// evidence with: decision log lines, including quoted values, and an
// element's text in a DOM dump.
func TestEvidenceParsers(t *testing.T) {
	line := `time=2026-11-25T01:01:02.345Z level=INFO msg=decision id=4f2a client=127.0.0.1:5 source=local kind=connect target=a.cuttle.test:443 rule=r action=upstream upstream="" interface="" outcome=ok status=200 detail="blocked by rule \"x\"" dur=1ms`
	f := parseLogfmt(line)
	for k, v := range map[string]string{"msg": "decision", "target": "a.cuttle.test:443", "upstream": "", "interface": "",
		"outcome": "ok", "status": "200", "detail": `blocked by rule "x"`, "dur": "1ms"} {
		if got, ok := f[k]; !ok || got != v {
			t.Errorf("%s = %q (%v), want %q", k, got, ok, v)
		}
	}
	if !(logLine{fields: f}).has(map[string]string{"kind": "connect", "upstream": ""}) || (logLine{fields: f}).has(map[string]string{"kind": "http"}) {
		t.Error("has")
	}
	dom := `<html><body><pre id="out">fetch a =&gt; response &amp; more</pre><script>const x = () => 1;</script></body></html>`
	if got := elementText(dom, `<pre id="out">`); got != "fetch a => response & more" {
		t.Errorf("elementText = %q", got)
	}
	if got := elementText(dom, `<pre id="none">`); got != "" {
		t.Errorf("missing element = %q", got)
	}
}

// TestProcessPatterns checks that the cleanup patterns match the processes
// the M1 client check starts and nothing that merely names a file in the
// evidence directory.
func TestProcessPatterns(t *testing.T) {
	out := "/tmp/x.y/cuttle-m1-check-1"
	started := regexp.MustCompile(startedPattern(out))
	for _, cmd := range []string{
		"/tmp/x.y/cuttle-m1-check-1/cuttle serve --config /tmp/x.y/cuttle-m1-check-1/curl/single.toml",
		"Google Chrome --headless=new --user-data-dir=/tmp/x.y/cuttle-m1-check-1/chrome/chrome/profile-01 --no-first-run",
		"chrome_crashpad_handler --database=/tmp/x.y/cuttle-m1-check-1/chrome/chrome/profile-01/Crashpad",
		"/usr/bin/curl -q --cacert /tmp/x.y/cuttle-m1-check-1/curl/origin-ca.pem https://origin.cuttle.test/",
	} {
		if !started.MatchString(cmd) {
			t.Errorf("not matched: %s", cmd)
		}
	}
	for _, cmd := range []string{
		"tee /tmp/x.y/cuttle-m1-check-1/run.log",
		"tail -f /tmp/x.y/cuttle-m1-check-1/report.txt",
		"/tmp/x.y/cuttle serve --config /tmp/x.y/cuttle-m1-check-10/curl/single.toml",
		"cuttle serve --config /tmp/xzy/cuttle-m1-check-1/curl/single.toml",
	} {
		if started.MatchString(cmd) {
			t.Errorf("matched: %s", cmd)
		}
	}
	profile := regexp.MustCompile(chromePattern("/tmp/s/chrome/profile-10"))
	if !profile.MatchString("Chrome --user-data-dir=/tmp/s/chrome/profile-10 --x") || !profile.MatchString("x --database=/tmp/s/chrome/profile-10/Crashpad") {
		t.Error("profile pattern misses its run")
	}
	if profile.MatchString("Chrome --user-data-dir=/tmp/s/chrome/profile-100 --x") || profile.MatchString("tail -f /tmp/s/chrome/profile-10.stderr") {
		t.Error("profile pattern matches another run or a reader")
	}
}

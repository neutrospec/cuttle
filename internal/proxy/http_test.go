package proxy

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"squid/internal/logx"
	"squid/internal/stats"
	"squid/internal/testenv"
)

// clientDisclosure are disclosure fields a client sends itself; none may
// reach the origin on a direct route (design 0003 D5).
var clientDisclosure = map[string]string{
	"Via":                 "1.1 lab-gw",
	"Forwarded":           "for=198.51.100.7",
	"X-Forwarded-For":     "198.51.100.7",
	"X-Forwarded-Host":    "front.example",
	"X-Forwarded-Proto":   "https",
	"X-Forwarded-Port":    "443",
	"X-Forwarded-Server":  "front",
	"X-Real-Ip":           "198.51.100.7",
	"Client-Ip":           "198.51.100.7",
	"True-Client-Ip":      "198.51.100.7",
	"X-Client-Ip":         "198.51.100.7",
	"X-Cluster-Client-Ip": "198.51.100.7",
	"X-Originating-Ip":    "198.51.100.7",
	"Cf-Connecting-Ip":    "198.51.100.7",
	"Fastly-Client-Ip":    "198.51.100.7",
	"Proxy-Status":        "lab-gw; error=connection_refused",
}

func checkNoDisclosure(t *testing.T, h http.Header) {
	t.Helper()
	for k := range h {
		ck := http.CanonicalHeaderKey(k)
		if _, bad := clientDisclosure[ck]; bad || strings.HasPrefix(ck, "X-Forwarded-") {
			t.Errorf("origin received disclosure field %s: %q", k, h[k])
		}
	}
}

// TestHTTPDirect checks C3 for Go and raw clients: the client's Host, other
// end-to-end fields, and body bytes arrive; no disclosure field arrives,
// even when the client sent one; no Accept-Encoding or User-Agent is added;
// the response keeps its status and body.
func TestHTTPDirect(t *testing.T) {
	e := newEnv(t, baseConfig)
	client, _ := e.goClient()

	t.Run("GET with client fields", func(t *testing.T) {
		e.http.Reset()
		req, _ := http.NewRequest("GET", "http://plain.cuttle.test/echo", nil)
		req.Header.Set("User-Agent", "test-agent/1.0")
		req.Header.Set("X-Custom", "kept")
		req.Header.Add("X-Multi", "one")
		req.Header.Add("X-Multi", "two")
		req.Header.Set("Cookie", "a=1; b=2")
		for k, v := range clientDisclosure {
			req.Header.Set(k, v)
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		rec := e.http.Requests()[0]
		if rec.Host != "plain.cuttle.test" || rec.URI != "/echo" {
			t.Errorf("host %q uri %q", rec.Host, rec.URI)
		}
		if rec.Header.Get("User-Agent") != "test-agent/1.0" || rec.Header.Get("X-Custom") != "kept" ||
			strings.Join(rec.Header["X-Multi"], ",") != "one,two" || rec.Header.Get("Cookie") != "a=1; b=2" {
			t.Errorf("client fields changed: %v", rec.Header)
		}
		checkNoDisclosure(t, rec.Header)
		if _, ok := rec.Header["Accept-Encoding"]; ok {
			t.Errorf("Accept-Encoding added: %q", rec.Header["Accept-Encoding"])
		}
	})

	t.Run("no User-Agent", func(t *testing.T) {
		e.http.Reset()
		req, _ := http.NewRequest("GET", "http://plain.cuttle.test/echo", nil)
		req.Header.Set("User-Agent", "") // Go's client then sends none
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		rec := e.http.Requests()[0]
		if _, ok := rec.Header["User-Agent"]; ok {
			t.Errorf("User-Agent added: %q", rec.Header["User-Agent"])
		}
		raw := fromLast(string(e.http.Raw(rec.Conn)), "GET /echo HTTP/1.1\r\n")
		if strings.Contains(strings.ToLower(raw), "user-agent") || strings.Contains(strings.ToLower(raw), "accept-encoding") {
			t.Errorf("raw request has an added field:\n%s", raw)
		}
	})

	t.Run("POST bodies", func(t *testing.T) {
		body := bytes.Repeat([]byte("0123456789"), 5000)
		for _, chunked := range []bool{false, true} {
			e.http.Reset()
			var rd io.Reader = bytes.NewReader(body)
			if chunked {
				rd = io.MultiReader(rd) // unknown length: Go sends it chunked
			}
			req, _ := http.NewRequest("POST", "http://plain.cuttle.test/echo?kind=post", rd)
			req.Header.Set("Content-Type", "application/octet-stream")
			res, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			rec := e.http.Requests()[0]
			if !bytes.Equal(rec.Body, body) || rec.Method != "POST" || rec.Header.Get("Content-Type") != "application/octet-stream" {
				t.Errorf("chunked=%v: body %d bytes, method %s", chunked, len(rec.Body), rec.Method)
			}
			raw := fromLast(string(e.http.Raw(rec.Conn)), "POST /echo?kind=post HTTP/1.1\r\n")
			if chunked != strings.Contains(raw, "Transfer-Encoding: chunked") {
				t.Errorf("chunked=%v: framing differs:\n%.300s", chunked, raw)
			}
		}
	})

	t.Run("response status, body, and trailer", func(t *testing.T) {
		res, err := client.Get("http://plain.cuttle.test/status?code=418")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 418 || string(b) != "status 418" || res.Header.Get("Proxy-Status") != "" || res.Header.Get("Via") != "" {
			t.Errorf("418: %d %q %v", res.StatusCode, b, res.Header)
		}
		res, err = client.Get("http://plain.cuttle.test/chunked")
		if err != nil {
			t.Fatal(err)
		}
		b, _ = io.ReadAll(res.Body)
		res.Body.Close()
		if string(b) != "chunk-0;chunk-1;chunk-2;" || res.Trailer.Get("X-Checksum") != "done" {
			t.Errorf("chunked: %q trailer %v", b, res.Trailer)
		}
	})

	t.Run("raw client disclosure fields", func(t *testing.T) {
		e.http.Reset()
		var req strings.Builder
		req.WriteString("GET http://plain.cuttle.test/echo HTTP/1.1\r\nHost: plain.cuttle.test\r\n")
		for k, v := range clientDisclosure {
			fmt.Fprintf(&req, "%s: %s\r\n", strings.ToLower(k), v)
		}
		req.WriteString("x-forwarded-anything: 1\r\nX-Kept: yes\r\nConnection: close\r\n\r\n")
		res, _ := e.raw(req.String())
		if res.StatusCode != 200 {
			t.Fatalf("status %d", res.StatusCode)
		}
		rec := e.http.Requests()[0]
		checkNoDisclosure(t, rec.Header)
		if rec.Header.Get("X-Kept") != "yes" {
			t.Errorf("end-to-end field lost")
		}
		if _, ok := rec.Header["User-Agent"]; ok {
			t.Errorf("User-Agent added for a client that sent none")
		}
	})

	t.Run("HTTP_PROXY in cuttle's environment", func(t *testing.T) {
		// Go reads proxy variables once per process, so the check runs in a
		// child test process whose environment points them at a trap.
		trap, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer trap.Close()
		hits := make(chan struct{}, 1)
		go func() {
			for {
				c, err := trap.Accept()
				if err != nil {
					return
				}
				c.Close()
				select {
				case hits <- struct{}{}:
				default:
				}
			}
		}()
		cmd := exec.Command(os.Args[0], "-test.run=^TestHTTPProxyEnvChild$", "-test.count=1", "-test.v")
		trapURL := "http://" + trap.Addr().String()
		cmd.Env = append(os.Environ(), "CUTTLE_TEST_PROXY_ENV_CHILD=1",
			"HTTP_PROXY="+trapURL, "http_proxy="+trapURL, "HTTPS_PROXY="+trapURL, "ALL_PROXY="+trapURL, "NO_PROXY=", "no_proxy=")
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "--- PASS: TestHTTPProxyEnvChild") {
			t.Fatalf("child: %v\n%s", err, out)
		}
		select {
		case <-hits:
			t.Fatal("cuttle connected to the HTTP_PROXY address")
		default:
		}
	})
}

// fromLast returns s from the last occurrence of line, the bytes of the
// latest request on a reused connection.
func fromLast(s, line string) string {
	if i := strings.LastIndex(s, line); i >= 0 {
		return s[i:]
	}
	return s
}

// TestHTTPProxyEnvChild runs only as the child of TestHTTPDirect, with
// HTTP_PROXY and friends pointing at a trap. The direct transport has Proxy
// nil, so the request must reach the origin in origin-form.
func TestHTTPProxyEnvChild(t *testing.T) {
	if os.Getenv("CUTTLE_TEST_PROXY_ENV_CHILD") != "1" {
		t.Skip("child process of TestHTTPDirect")
	}
	if p, _ := http.ProxyFromEnvironment(&http.Request{URL: mustURL("http://plain.cuttle.test/")}); p == nil {
		t.Fatal("the proxy environment is not visible to Go in this process")
	}
	e := newEnv(t, baseConfig)
	res, _ := e.raw("GET http://plain.cuttle.test/echo HTTP/1.1\r\nHost: plain.cuttle.test\r\n\r\n")
	if res.StatusCode != 200 {
		t.Fatalf("status %d", res.StatusCode)
	}
	rec := e.http.Requests()
	if len(rec) != 1 || rec[0].URI != "/echo" {
		t.Fatalf("origin saw %+v", rec)
	}
}

// TestHTTPRawTarget checks C3: the origin receives the client's raw path
// and query, origin-form, and absolute-form for a path starting with //;
// an invalid escape in the path gets Go's own 400.
func TestHTTPRawTarget(t *testing.T) {
	e := newEnv(t, baseConfig)
	for _, c := range []struct{ target, wantLine string }{
		{"http://h/a{b}|c?x=1;y=2&z=%zz", "GET /a{b}|c?x=1;y=2&z=%zz HTTP/1.1"},
		{"http://h/q?b=2&a=1;c=3", "GET /q?b=2&a=1;c=3 HTTP/1.1"},
		{"http://h/p?", "GET /p? HTTP/1.1"},
		{"http://h//x", "GET http://h//x HTTP/1.1"},
		{"http://h", "GET / HTTP/1.1"},
		{"http://h?only=query", "GET /?only=query HTTP/1.1"},
		{"http://H/MixedCase/%41?Q=%42", "GET /MixedCase/%41?Q=%42 HTTP/1.1"},
	} {
		e.http.Reset()
		res, _ := e.raw("GET " + c.target + " HTTP/1.1\r\nHost: h\r\nConnection: close\r\n\r\n")
		if res.StatusCode != 200 {
			t.Errorf("%s: status %d", c.target, res.StatusCode)
			continue
		}
		rec := e.http.Requests()
		if len(rec) != 1 {
			t.Fatalf("%s: %d requests", c.target, len(rec))
		}
		// Go's server keeps the request target exactly as sent.
		if line := rec[0].Method + " " + rec[0].URI + " " + rec[0].Proto; line != c.wantLine {
			t.Errorf("%s: origin got %q, want %q", c.target, line, c.wantLine)
		}
		if !strings.Contains(string(e.http.Raw(rec[0].Conn)), c.wantLine+"\r\n") {
			t.Errorf("%s: raw bytes lack %q", c.target, c.wantLine)
		}
	}
	e.waitDecisions(7)

	// Go rejects an invalid escape in the path before the handler.
	e.http.Reset()
	e.net.Reset()
	before := len(e.logs.decisions())
	res, _ := e.raw("GET http://h/a%zz HTTP/1.1\r\nHost: h\r\n\r\n")
	if res.StatusCode != 400 || res.Header.Get("Proxy-Status") != "" {
		t.Errorf("a%%zz: %d %q", res.StatusCode, res.Header.Get("Proxy-Status"))
	}
	time.Sleep(100 * time.Millisecond)
	if len(e.logs.decisions()) != before || len(e.http.Requests()) != 0 || len(e.net.Events()) != 0 {
		t.Errorf("a%%zz reached cuttle's handler")
	}
}

// TestHTTPReusedConnRoutesPerRequest checks C4 with Go and curl: two
// requests on one client connection, direct first because cuttle's own
// responses close the connection, then block, take their own routes.
func TestHTTPReusedConnRoutesPerRequest(t *testing.T) {
	t.Run("Go", func(t *testing.T) {
		e := newEnv(t, baseConfig)
		client, tr := e.goClient()
		tr.MaxConnsPerHost = 1
		var reused []bool
		trace := &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) { reused = append(reused, i.Reused) }}
		for _, u := range []string{"http://plain.cuttle.test/echo", "http://blocked.cuttle.test/"} {
			req, _ := http.NewRequest("GET", u, nil)
			req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
			res, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
		}
		if len(reused) != 2 || reused[0] || !reused[1] {
			t.Fatalf("connection reuse %v", reused)
		}
		checkReuse(t, e)
	})
	t.Run("curl", func(t *testing.T) {
		e := newEnv(t, baseConfig)
		r := testenv.Curl(t, "-sS", "--proxy", "http://"+e.addr, "-w", "%{http_code} %{num_connects}\n",
			"-o", "/dev/null", "http://plain.cuttle.test/echo", "-o", "/dev/null", "http://blocked.cuttle.test/")
		if r.Code != 0 || r.Stdout != "200 1\n403 0\n" {
			t.Fatalf("curl: exit %d out %q err %q", r.Code, r.Stdout, r.Stderr)
		}
		checkReuse(t, e)
	})
}

func checkReuse(t *testing.T, e *env) {
	t.Helper()
	ds := e.waitDecisions(2)
	if len(ds) != 2 {
		t.Fatalf("decisions %v", ds)
	}
	checkDecision(t, ds[0], map[string]string{"kind": "http", "target": "plain.cuttle.test:80", "action": "direct", "outcome": "ok", "status": "200"})
	checkDecision(t, ds[1], map[string]string{"kind": "http", "target": "blocked.cuttle.test:80", "rule": "blocked", "action": "block", "outcome": "blocked", "status": "403"})
	if ds[0]["client"] != ds[1]["client"] {
		t.Errorf("different client connections: %s %s", ds[0]["client"], ds[1]["client"])
	}
	if got := e.p.Live().Get(stats.ConnectionsTotal); got != 1 {
		t.Errorf("connections_total %d", got)
	}
}

// TestBlockRule checks C5 for a block rule: 403 with Proxy-Status, no lookup,
// no dial, and no counted bytes, for CONNECT and plain HTTP, from raw, Go,
// and curl clients.
func TestBlockRule(t *testing.T) {
	e := newEnv(t, baseConfig)
	checkBlocked(t, e, "blocked", "blocked.cuttle.test", "x.blocked.cuttle.test")
}

// TestBlockDefault checks C5 for a block default.
func TestBlockDefault(t *testing.T) {
	cfg := strings.Replace(baseConfig, `default = "direct"`, `default = "block"`, 1)
	e := newEnv(t, cfg)
	checkBlocked(t, e, "default", "origin.cuttle.test", "unknown.example")
}

func checkBlocked(t *testing.T, e *env, rule, host, other string) {
	t.Helper()
	ps := `cuttle-test; error=http_request_denied; details="blocked by rule ` + rule + `"`
	res, body := e.raw("CONNECT " + host + ":443 HTTP/1.1\r\nHost: " + host + ":443\r\n\r\n")
	generatedHeaders(t, res, 403, ps)
	if want := fmt.Sprintf("cuttle: blocked by rule %q (%s:443)\n", rule, host); string(body) != want {
		t.Errorf("body %q, want %q", body, want)
	}
	res, body = e.raw("POST http://" + other + "/upload?q=1 HTTP/1.1\r\nHost: " + other + "\r\nContent-Length: 5\r\n\r\nhello")
	generatedHeaders(t, res, 403, ps)
	if want := fmt.Sprintf("cuttle: blocked by rule %q (%s:80)\n", rule, other); string(body) != want {
		t.Errorf("body %q, want %q", body, want)
	}

	client, _ := e.goClient()
	var connectStatus int
	client.Transport.(*http.Transport).OnProxyConnectResponse = func(_ context.Context, _ *url.URL, _ *http.Request, res *http.Response) error {
		connectStatus = res.StatusCode
		return nil
	}
	if _, err := client.Get("https://" + host + "/"); err == nil {
		t.Errorf("Go: blocked CONNECT succeeded")
	}
	if connectStatus != 403 {
		t.Errorf("Go: CONNECT status %d", connectStatus)
	}
	r := testenv.Curl(t, "-sS", "--proxy", "http://"+e.addr, "-w", "%{http_connect}", "-o", "/dev/null", "https://"+host+"/")
	if r.Stdout != "403" {
		t.Errorf("curl: %%{http_connect} %q exit %d", r.Stdout, r.Code)
	}

	ds := e.waitDecisions(4)
	for _, d := range ds {
		checkDecision(t, d, map[string]string{"rule": rule, "action": "block", "outcome": "blocked", "status": "403",
			"up": "0", "down": "0", "error": "http_request_denied"})
	}
	if ev := e.net.Events(); len(ev) != 0 {
		t.Errorf("network events for blocked requests: %v", ev)
	}
	if n := e.bytesTotal(); n != 0 {
		t.Errorf("counted bytes %d", n)
	}
	if e.http.Connections()+e.https.Connections() != 0 {
		t.Errorf("an origin was contacted")
	}
}

// TestHTTPExpectAnd407 checks C10: with Expect: 100-continue the body
// reaches the origin, and an origin's 407 reaches the client as 502 with
// received-status=407. Only Go and /usr/bin/curl talk to the 407 origin
// (design 0007 D6).
func TestHTTPExpectAnd407(t *testing.T) {
	e := newEnvWith(t, baseConfig, envOptions{allow407: true})
	body := bytes.Repeat([]byte("x"), 64<<10)

	client, tr := e.goClient()
	tr.ExpectContinueTimeout = 5 * time.Second
	var got100 bool
	req, _ := http.NewRequest("POST", "http://plain.cuttle.test/sink", bytes.NewReader(body))
	req.Header.Set("Expect", "100-continue")
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{Got100Continue: func() { got100 = true }}))
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || string(b) != strconv.Itoa(len(body)) || !got100 {
		t.Errorf("Go Expect: %d %q 100=%v", res.StatusCode, b, got100)
	}

	file := filepath.Join(t.TempDir(), "body")
	os.WriteFile(file, body, 0o600)
	r := testenv.Curl(t, "-sS", "--proxy", "http://"+e.addr, "-H", "Expect: 100-continue", "--data-binary", "@"+file,
		"-w", " %{http_code}", "http://plain.cuttle.test/sink")
	if r.Code != 0 || r.Stdout != strconv.Itoa(len(body))+" 200" {
		t.Errorf("curl Expect: exit %d out %q err %q", r.Code, r.Stdout, r.Stderr)
	}
	for _, rec := range e.http.Requests() {
		if len(rec.Body) != len(body) {
			t.Errorf("origin body %d bytes", len(rec.Body))
		}
	}

	res, err = client.Get("http://plain.cuttle.test/status?code=407")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(res.Body)
	res.Body.Close()
	generatedHeaders(t, res, 502, "cuttle-test; received-status=407")
	if res.Header.Get("Proxy-Authenticate") != "" || strings.Contains(string(b), "status 407") {
		t.Errorf("407 leaked: %v %q", res.Header, b)
	}
	r = testenv.Curl(t, "-sS", "--proxy", "http://"+e.addr, "-D", "-", "-o", "/dev/null", "http://plain.cuttle.test/status?code=407")
	if !strings.HasPrefix(r.Stdout, "HTTP/1.1 502 Bad Gateway") || !strings.Contains(r.Stdout, "Proxy-Status: cuttle-test; received-status=407") ||
		strings.Contains(strings.ToLower(r.Stdout), "proxy-authenticate") {
		t.Errorf("curl 407: %q", r.Stdout)
	}
	ds := e.waitDecisions(4)
	checkDecision(t, ds[2], map[string]string{"outcome": "upstream_status", "status": "502", "down": "0"})
	checkDecision(t, ds[3], map[string]string{"outcome": "upstream_status", "status": "502", "down": "0"})
}

// TestFrontEndRejects checks C11: unsupported target forms get cuttle's 400
// with a decision line and no lookup or dial.
func TestFrontEndRejects(t *testing.T) {
	e := newEnv(t, baseConfig)
	reqs := []string{
		"GET / HTTP/1.1\r\nHost: plain.cuttle.test\r\n\r\n",
		"OPTIONS * HTTP/1.1\r\nHost: plain.cuttle.test\r\n\r\n",
		"GET https://origin.cuttle.test/ HTTP/1.1\r\nHost: origin.cuttle.test\r\n\r\n",
		"GET http://user:secret@plain.cuttle.test/ HTTP/1.1\r\nHost: plain.cuttle.test\r\n\r\n",
		"GET ftp://plain.cuttle.test/ HTTP/1.1\r\nHost: plain.cuttle.test\r\n\r\n",
		"CONNECT user@origin.cuttle.test:443 HTTP/1.1\r\nHost: origin.cuttle.test:443\r\n\r\n",
		"CONNECT origin.cuttle.test HTTP/1.1\r\nHost: origin.cuttle.test\r\n\r\n",
		"CONNECT [fe80::1%25en0]:443 HTTP/1.1\r\nHost: x\r\n\r\n",
		"CONNECT 127.1:443 HTTP/1.1\r\nHost: x\r\n\r\n",
	}
	for _, req := range reqs {
		res, body := e.raw(req)
		if res.StatusCode != 400 || !strings.HasPrefix(res.Header.Get("Proxy-Status"), "cuttle-test; error=http_request_error; details=") ||
			!strings.HasPrefix(string(body), "cuttle: bad request: ") {
			t.Errorf("%q: %d %q %q", firstLine(req), res.StatusCode, res.Header.Get("Proxy-Status"), body)
		}
		if strings.Contains(string(body), "secret") || strings.Contains(res.Header.Get("Proxy-Status"), "secret") {
			t.Errorf("response reflects the target: %q", body)
		}
	}
	ds := e.waitDecisions(len(reqs))
	for _, d := range ds {
		checkDecision(t, d, map[string]string{"outcome": "bad_request", "status": "400", "error": "http_request_error", "target": "", "action": ""})
	}
	if ev := e.net.Events(); len(ev) != 0 {
		t.Errorf("network events %v", ev)
	}
	if strings.Contains(e.logs.String(), "secret") {
		t.Errorf("log reflects the target")
	}
}

// TestHeadSizeLimit checks C11: a head of about 72 KiB gets Go's 431 and a
// head of about 60 KiB is served. MaxHeaderBytes is 64 KiB and Go reads up
// to MaxHeaderBytes + 4096 bytes. Go also refuses with 431 a head with more
// than 500 field values (http.DefaultMaxHeaderValueCount, exported from Go
// 1.27, so this go 1.26 module cannot name it), so both heads are built
// from 1 KiB fields, far fewer than 500 lines, and the 431 comes from the
// byte limit.
func TestHeadSizeLimit(t *testing.T) {
	e := newEnv(t, baseConfig)
	head := func(kib int) string {
		var b strings.Builder
		b.WriteString("GET http://plain.cuttle.test/echo HTTP/1.1\r\nHost: plain.cuttle.test\r\n")
		value := strings.Repeat("v", 1024-len("X-Pad-000: \r\n"))
		for i := range kib {
			fmt.Fprintf(&b, "X-Pad-%03d: %s\r\n", i, value)
		}
		b.WriteString("\r\n")
		return b.String()
	}
	small := head(60)
	if n := strings.Count(small, "\r\n"); n >= 500 {
		t.Fatalf("60 KiB head has %d lines", n)
	}
	res, _ := e.raw(small)
	if res.StatusCode != 200 {
		t.Errorf("60 KiB head (%d bytes): %d", len(small), res.StatusCode)
	}
	big := head(72)
	res, _ = e.raw(big)
	if res.StatusCode != 431 || res.Header.Get("Proxy-Status") != "" {
		t.Errorf("72 KiB head (%d bytes): %d %q", len(big), res.StatusCode, res.Header.Get("Proxy-Status"))
	}
	ds := e.waitDecisions(1)
	time.Sleep(100 * time.Millisecond)
	if len(e.logs.decisions()) != 1 {
		t.Errorf("decision lines %v", e.logs.decisions())
	}
	checkDecision(t, ds[0], map[string]string{"outcome": "ok", "status": "200"})
}

// TestDecisionLogFields checks C14: decision lines have the D8 fields and
// never contain a path, query, header value, or body.
func TestDecisionLogFields(t *testing.T) {
	e := newEnv(t, baseConfig)
	res, _ := e.raw("POST http://plain.cuttle.test/secret-path?secret-query=1 HTTP/1.1\r\nHost: plain.cuttle.test\r\n" +
		"X-Token: secret-header-value\r\nContent-Length: 11\r\nConnection: close\r\n\r\nsecret-body")
	if res.StatusCode != 200 {
		t.Fatalf("status %d", res.StatusCode)
	}
	c := e.connectRaw("origin.cuttle.test:443")
	if res, _ := tlsGet(t, c, e, "origin.cuttle.test", "/secret-tunnel-path?secret=1"); res.StatusCode != 200 {
		t.Fatal(res.StatusCode)
	}
	c.Close()
	e.raw("GET http://blocked.cuttle.test/secret-blocked?secret=2 HTTP/1.1\r\nHost: blocked.cuttle.test\r\n\r\n")
	e.raw("GET /secret-origin-form HTTP/1.1\r\nHost: plain.cuttle.test\r\n\r\n")
	e.net.Map("gone.cuttle.test")
	e.raw("GET http://gone.cuttle.test/secret-dns HTTP/1.1\r\nHost: gone.cuttle.test\r\n\r\n")

	ds := e.waitDecisions(5)
	fields := []string{"time", "level", "msg", "id", "client", "source", "kind", "target", "rule", "action", "upstream", "outcome", "status", "up", "down", "dur"}
	for _, d := range ds {
		for _, f := range fields {
			if _, ok := d[f]; !ok {
				t.Errorf("field %s missing: %v", f, d)
			}
		}
		if d["level"] != "INFO" || len(d["id"]) != 8 || !strings.HasPrefix(d["client"], "127.0.0.1:") || !strings.HasSuffix(d["time"], "Z") {
			t.Errorf("line %v", d)
		}
		if _, err := time.ParseDuration(d["dur"]); err != nil {
			t.Errorf("dur %q", d["dur"])
		}
	}
	checkDecision(t, ds[0], map[string]string{"kind": "http", "target": "plain.cuttle.test:80", "up": "11", "outcome": "ok"})
	if n, _ := strconv.Atoi(ds[0]["down"]); n == 0 {
		t.Errorf("plain HTTP down %q", ds[0]["down"])
	}
	checkDecision(t, byTarget(t, ds, "connect", "origin.cuttle.test:443"), map[string]string{"outcome": "ok"})
	checkDecision(t, byTarget(t, ds, "http", "gone.cuttle.test:80"), map[string]string{"outcome": "dns_error", "error": "dns_error", "status": "502"})
	if s := e.logs.String(); strings.Contains(s, "secret") {
		t.Errorf("log contains request content:\n%s", s)
	}
}

// TestLogRotation checks P6: with a 64 KiB threshold the decision log keeps
// exactly five rotated files, and no line is split.
func TestLogRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cuttle.log")
	w, err := logx.Open(path, 64<<10, logx.DefaultKeep)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	e := newEnvWith(t, baseConfig, envOptions{logWriter: w})
	client, _ := e.goClient()
	const requests = 3000
	for i := range requests {
		res, err := client.Get("http://plain.cuttle.test/bytes?n=" + strconv.Itoa(i%7))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
	}
	deadline := time.Now().Add(10 * time.Second)
	for e.p.Live().Sum(stats.DecisionsTotal) < requests && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := e.p.Live().Sum(stats.DecisionsTotal); got != requests {
		t.Fatalf("decisions %d", got)
	}
	names := []string{"cuttle.log", "cuttle.log.1", "cuttle.log.2", "cuttle.log.3", "cuttle.log.4", "cuttle.log.5"}
	entries, _ := os.ReadDir(dir)
	var have []string
	for _, en := range entries {
		have = append(have, en.Name())
	}
	if strings.Join(have, ",") != strings.Join(names, ",") {
		t.Fatalf("files %v, want %v", have, names)
	}
	lines := 0
	for _, n := range names {
		data, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			t.Fatal(err)
		}
		if len(data) > 64<<10 {
			t.Errorf("%s has %d bytes", n, len(data))
		}
		sc := bufio.NewScanner(bytes.NewReader(data))
		for sc.Scan() {
			lines++
			if m := parseLogLine(sc.Text()); m["msg"] != "decision" || m["time"] == "" || m["dur"] == "" {
				t.Fatalf("%s: broken line %q", n, sc.Text())
			}
		}
		if n != "cuttle.log" && (len(data) == 0 || data[len(data)-1] != '\n') {
			t.Errorf("%s does not end with a full line", n)
		}
	}
	if lines >= requests {
		t.Errorf("%d lines kept of %d; nothing was rotated away", lines, requests)
	}
}

// TestOriginRefuses407 checks the V10 guard: without Allow407 the mock
// origin refuses to answer 407.
func TestOriginRefuses407(t *testing.T) {
	e := newEnv(t, baseConfig)
	res, body := e.raw("GET http://plain.cuttle.test/status?code=407 HTTP/1.1\r\nHost: plain.cuttle.test\r\n\r\n")
	if res.StatusCode != 400 || !strings.Contains(string(body), "407 refused") {
		t.Fatalf("%d %q", res.StatusCode, body)
	}
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

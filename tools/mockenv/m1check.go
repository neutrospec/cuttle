package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/neutrospec/cuttle/internal/testenv"
)

// The M1 client check of design 0007 D8, as one runnable procedure. For each
// client it starts a fresh session, runs the single, edge, and gateway cuttle
// instances from the session's configurations with their own directories,
// runs every case with that client, checks the evidence, and ends with
// check-disclosure. Headless Chrome also runs the Local Network Access
// observation of OI-0006 V6.

const (
	defaultChrome = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
	defaultCurl   = "/usr/bin/curl"

	// Case names, in the order of design 0007 D8.
	caseDirectHTTPS   = "Direct HTTPS"
	caseDirectHTTP    = "Direct HTTP"
	caseBlock         = "Block"
	caseUpstream      = "Upstream"
	caseWebSocket     = "WebSocket"
	caseChain         = "Chain"
	caseNonDisclosure = "Non-disclosure"
	caseLNA           = "LNA observation"
)

// Result states. OBSERVED records what a client did where the check has no
// expected outcome (OI-0006 V6).
const (
	statusPass     = "PASS"
	statusFail     = "FAIL"
	statusObserved = "OBSERVED"
	statusNotRun   = "NOT RUN"
)

// m1Options configure the M1 client check.
type m1Options struct {
	Clients []string // chrome, curl
	Out     string   // evidence directory; "" creates one under $TMPDIR
	Cuttle  string   // cuttle binary; "" builds ./cmd/cuttle into Out
	Chrome  string
	Curl    string
	LNA     bool // run the V6 observation with Chrome

	// gatewayDisclosure replaces the gateway's disclosure toward
	// egress-chain; only tests set it.
	gatewayDisclosure string
}

// result is the outcome of one case for one client.
type result struct {
	Client, Case, Status string
	Notes                []string
}

// m1Check runs the check and writes its progress and report to w. It
// returns the results; a FAIL among them fails the check.
func m1Check(ctx context.Context, opt m1Options, w io.Writer) ([]result, error) {
	out := opt.Out
	var err error
	if out == "" {
		if out, err = os.MkdirTemp("", "cuttle-m1-check-"); err != nil {
			return nil, err
		}
	} else if err := os.MkdirAll(out, 0o700); err != nil {
		return nil, err
	}
	if out, err = filepath.Abs(out); err != nil {
		return nil, err
	}
	fmt.Fprintf(w, "M1 client check; evidence in %s\n", out)
	bin := opt.Cuttle
	if bin == "" {
		if bin, err = buildCuttle(ctx, out); err != nil {
			return nil, err
		}
	}
	var results []result
	for _, client := range opt.Clients {
		rs, err := runClient(ctx, opt, client, bin, filepath.Join(out, client), w)
		results = append(results, rs...)
		if err != nil {
			return results, fmt.Errorf("%s: %w", client, err)
		}
	}
	if left := leftovers(out); len(left) > 0 {
		results = append(results, result{Client: "all", Case: "process cleanup", Status: statusFail,
			Notes: []string{"processes left running were killed: " + strings.Join(left, "; ")}})
	}
	var rep bytes.Buffer
	report(&rep, results)
	os.WriteFile(filepath.Join(out, "report.txt"), rep.Bytes(), 0o600)
	w.Write(rep.Bytes())
	return results, nil
}

// buildCuttle builds ./cmd/cuttle of the module that holds the current
// directory into dir.
func buildCuttle(ctx context.Context, dir string) (string, error) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		return "", errors.New("go not found on PATH; pass -cuttle")
	}
	gomod, err := exec.CommandContext(ctx, goBin, "env", "GOMOD").Output()
	if err != nil || strings.TrimSpace(string(gomod)) == "" || strings.TrimSpace(string(gomod)) == os.DevNull {
		return "", errors.New("run m1-check inside the cuttle module, or pass -cuttle")
	}
	bin := filepath.Join(dir, "cuttle")
	cmd := exec.CommandContext(ctx, goBin, "build", "-o", bin, "./cmd/cuttle")
	cmd.Dir = filepath.Dir(strings.TrimSpace(string(gomod)))
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build ./cmd/cuttle: %v\n%s", err, out)
	}
	return bin, nil
}

// clientRun holds one client's session and cuttle processes.
type clientRun struct {
	ctx    context.Context
	opt    m1Options
	client string
	sess   *session
	procs  map[string]*cuttleProc
	w      io.Writer
	runs   int // Chrome runs, for profile names
	home   string
}

func runClient(ctx context.Context, opt m1Options, client, bin, dir string, w io.Writer) ([]result, error) {
	if client != "chrome" && client != "curl" {
		return nil, fmt.Errorf("unknown client %q", client)
	}
	path := opt.Curl
	if client == "chrome" {
		path = opt.Chrome
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("%s not found at %s", client, path)
	}
	sess, err := startSession(sessionOptions{Dir: dir, gatewayDisclosure: opt.gatewayDisclosure})
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	k := &clientRun{ctx: ctx, opt: opt, client: client, sess: sess, procs: map[string]*cuttleProc{}, w: w,
		home: filepath.Join(dir, "client-home")}
	if err := os.MkdirAll(k.home, 0o700); err != nil {
		return nil, err
	}
	defer k.stopAll()
	fmt.Fprintf(w, "\n== %s: session %s\n", client, sess.Dir)
	for _, role := range []string{"gateway", "edge", "single"} {
		p, err := startCuttle(bin, sess.Instances[role], sess.Dir)
		if err != nil {
			return nil, err
		}
		k.procs[role] = p
		fmt.Fprintf(w, "started cuttle %s (pid %d) on %s\n", role, p.cmd.Process.Pid, sess.Instances[role].Listen)
	}
	var results []result
	for _, c := range []struct {
		name string
		run  func() result
	}{
		{caseDirectHTTPS, k.directHTTPS},
		{caseDirectHTTP, k.directHTTP},
		{caseBlock, k.block},
		{caseUpstream, k.upstream},
		{caseWebSocket, k.webSocket},
		{caseChain, k.chain},
		{caseNonDisclosure, k.nonDisclosure},
		{caseLNA, k.lna},
	} {
		if ctx.Err() != nil {
			return results, ctx.Err()
		}
		r := c.run()
		r.Client, r.Case = client, c.name
		fmt.Fprintf(w, "%s %s: %s\n", client, c.name, r.Status)
		for _, n := range r.Notes {
			fmt.Fprintf(w, "    %s\n", n)
		}
		results = append(results, r)
	}
	for role, p := range k.procs {
		if code := p.stop(); code != 0 {
			results = append(results, result{Client: client, Case: "cuttle " + role + " stop", Status: statusFail,
				Notes: []string{fmt.Sprintf("exit status %d after SIGTERM; stderr: %s", code, tail(p.stderr(), 400))}})
		}
	}
	return results, nil
}

func (k *clientRun) stopAll() {
	for _, p := range k.procs {
		p.stop()
	}
}

// fetchOut is what a client returned for one URL.
type fetchOut struct {
	Text    string // curl's body or Chrome's DOM
	Connect string // curl's %{http_connect}
	Code    string // curl's %{http_code}
	Exit    int
	Stderr  string
	Cmd     string
}

func (f fetchOut) summary(client string) string {
	if client == "curl" {
		return fmt.Sprintf("curl exit %d, http_connect %s, http_code %s, %d body bytes%s", f.Exit, f.Connect, f.Code, len(f.Text), errNote(f.Stderr))
	}
	return fmt.Sprintf("chrome exit %d, DOM %d bytes: %s", f.Exit, len(f.Text), tail(oneLine(f.Text), 160)) +
		"; " + strings.TrimSuffix(strings.TrimPrefix(f.Stderr, "see "), ".stderr") + ".{dom.html,stderr}"
}

func errNote(s string) string {
	if s = strings.TrimSpace(s); s == "" {
		return ""
	}
	return "; stderr: " + tail(oneLine(s), 200)
}

// fetch loads target through the proxy at proxy. cacert is the root curl
// trusts; Chrome trusts the session's SPKI hashes.
// Every command line is appended to <session>/commands.txt, so a run can be
// repeated by hand.
func (k *clientRun) fetch(proxy, target, cacert string, extra ...string) fetchOut {
	var f fetchOut
	if k.client == "curl" {
		f = k.curl(proxy, target, cacert, extra...)
	} else {
		f = k.chrome(proxy, target)
	}
	if cf, err := os.OpenFile(filepath.Join(k.sess.Dir, "commands.txt"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600); err == nil {
		fmt.Fprintf(cf, "%s\n# exit %d\n", f.Cmd, f.Exit)
		cf.Close()
	}
	return f
}

// curl runs /usr/bin/curl as design 0007 D5 and D8 describe: an environment
// with only PATH and a temporary HOME, -q so no .curlrc is read, --proxy,
// --cacert, -sS, -g, and -w.
func (k *clientRun) curl(proxy, target, cacert string, extra ...string) fetchOut {
	args := []string{"-q", "--max-time", "20", "-sS", "-g", "--proxy", "http://" + proxy, "--cacert", cacert,
		"-w", `\n%{http_connect} %{http_code}`}
	args = append(append(args, extra...), target)
	ctx, cancel := context.WithTimeout(k.ctx, 40*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, k.opt.Curl, args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + k.home}
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	f := fetchOut{Exit: exitCode(err), Stderr: stderr.String(), Cmd: shellQuote(append([]string{k.opt.Curl}, args...))}
	body := stdout.String()
	if i := strings.LastIndexByte(body, '\n'); i >= 0 {
		if codes := strings.Fields(body[i+1:]); len(codes) == 2 {
			f.Connect, f.Code = codes[0], codes[1]
		}
		body = body[:i]
	}
	f.Text = body
	return f
}

// chrome runs headless Chrome as design 0007 D8 describes, with a new
// throwaway profile inside the session directory for every run, in its own
// process group so that no helper outlives the run.
func (k *clientRun) chrome(proxy, target string) fetchOut {
	k.runs++
	profile := filepath.Join(k.sess.Dir, "chrome", fmt.Sprintf("profile-%02d", k.runs))
	if err := os.MkdirAll(profile, 0o700); err != nil {
		return fetchOut{Exit: -1, Stderr: err.Error()}
	}
	args := []string{
		"--headless=new", "--use-mock-keychain", "--user-data-dir=" + profile, "--no-first-run",
		// Keeps Chrome from waking the user's Google updater (OI-0006 T-R2).
		"--disable-updater-scheduler",
		"--proxy-server=http://" + proxy, "--proxy-bypass-list=<-loopback>",
		"--ignore-certificate-errors-spki-list=" + strings.Join(k.sess.SPKI, ","),
		"--virtual-time-budget=5000", "--dump-dom", target,
	}
	ctx, cancel := context.WithTimeout(k.ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, k.opt.Chrome, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	cmd.Env = replaceEnv(os.Environ(), "HOME="+k.home, "TMPDIR="+k.home)
	var stdout bytes.Buffer
	stderrPath := profile + ".stderr"
	stderr, err := os.Create(stderrPath)
	if err != nil {
		return fetchOut{Exit: -1, Stderr: err.Error()}
	}
	cmd.Stdout, cmd.Stderr = &stdout, stderr
	err = cmd.Run()
	stderr.Close()
	if cmd.Process != nil {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // helpers left in the group
	}
	waitGone(chromePattern(profile), 5*time.Second)
	os.RemoveAll(profile)
	os.WriteFile(profile+".dom.html", stdout.Bytes(), 0o600)
	return fetchOut{Text: stdout.String(), Exit: exitCode(err), Stderr: "see " + stderrPath,
		Cmd: shellQuote(append([]string{k.opt.Chrome}, args...))}
}

// replaceEnv returns env with each KEY=value of set replacing that key.
func replaceEnv(env []string, set ...string) []string {
	out := []string{}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		keep := true
		for _, s := range set {
			if sk, _, _ := strings.Cut(s, "="); sk == k {
				keep = false
			}
		}
		if keep {
			out = append(out, kv)
		}
	}
	return append(out, set...)
}

func exitCode(err error) int {
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		return ee.ExitCode()
	}
	return -1
}

// echoed reports whether a client shows the origin's echo of path.
func echoed(text, path string) bool { return strings.Contains(text, `"uri":"`+path+`"`) }

// mark is a position in the recordings, taken before a case.
type mark struct {
	decisions map[string]int // log lines per instance role
	upstream  int
	inspect   int
	decrypted int
	https     int
	http      int
}

func (k *clientRun) mark() mark {
	m := mark{decisions: map[string]int{}}
	for role, in := range k.sess.Instances {
		ds, _ := readDecisions(in.Log)
		m.decisions[role] = len(ds)
	}
	m.upstream = len(k.sess.upstream.Requests())
	m.inspect = len(k.sess.inspect.Requests())
	m.decrypted = len(k.sess.inspect.Decrypted())
	m.https = len(k.sess.httpsOrigin.Requests())
	m.http = len(k.sess.httpOrigin.Requests())
	return m
}

// decision waits up to 5 s for a decision line of the instance, after the
// mark, whose fields include want; a CONNECT decision is logged when its
// tunnel closes.
func (k *clientRun) decision(m mark, role string, want map[string]string) (logLine, bool) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		ds, _ := readDecisions(k.sess.Instances[role].Log)
		for _, d := range ds[min(m.decisions[role], len(ds)):] {
			if d.has(want) {
				return d, true
			}
		}
		if time.Now().After(deadline) || k.ctx.Err() != nil {
			return logLine{}, false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (k *clientRun) expectDecision(r *result, m mark, role string, want map[string]string) {
	if d, ok := k.decision(m, role, want); ok {
		r.Notes = append(r.Notes, "decision ("+role+"): "+d.raw)
		return
	}
	r.Status = statusFail
	r.Notes = append(r.Notes, fmt.Sprintf("no decision line in %s's log with %v", role, want))
}

func newResult() result { return result{Status: statusPass} }

func (r *result) check(ok bool, note string) {
	if !ok {
		r.Status = statusFail
		note = "missing: " + note
	}
	r.Notes = append(r.Notes, note)
}

func (k *clientRun) directHTTPS() result {
	r, m := newResult(), k.mark()
	target := "https://" + k.sess.HTTPSOrigin + "/echo"
	f := k.fetch(k.sess.Instances["single"].Listen, target, k.sess.OriginCA)
	r.Notes = append(r.Notes, f.summary(k.client))
	r.check(echoed(f.Text, "/echo"), "the origin's echo of /echo")
	if k.client == "curl" {
		r.check(f.Exit == 0 && f.Connect == "200" && f.Code == "200", "curl exit 0 with 200 to CONNECT and to GET")
	}
	k.expectDecision(&r, m, "single", map[string]string{"kind": "connect", "target": k.sess.HTTPSOrigin,
		"rule": "loopback", "action": "direct", "outcome": "ok"})
	return r
}

func (k *clientRun) directHTTP() result {
	r, m := newResult(), k.mark()
	target := "http://" + k.sess.HTTPOrigin + "/echo"
	f := k.fetch(k.sess.Instances["single"].Listen, target, k.sess.OriginCA)
	r.Notes = append(r.Notes, f.summary(k.client))
	r.check(echoed(f.Text, "/echo"), "the origin's echo of /echo")
	if k.client == "curl" {
		r.check(f.Exit == 0 && f.Code == "200", "curl exit 0 with 200")
	}
	k.expectDecision(&r, m, "single", map[string]string{"kind": "http", "target": k.sess.HTTPOrigin,
		"rule": "loopback", "action": "direct", "outcome": "ok"})
	return r
}

func (k *clientRun) block() result {
	r, m := newResult(), k.mark()
	f := k.fetch(k.sess.Instances["single"].Listen, "https://"+hostBlocked+"/", k.sess.OriginCA)
	r.Notes = append(r.Notes, f.summary(k.client))
	if k.client == "curl" {
		r.check(f.Connect == "403", "curl %{http_connect} 403")
	} else {
		r.check(strings.TrimSpace(visibleText(f.Text)) == "", "an empty Chrome DOM")
	}
	k.expectDecision(&r, m, "single", map[string]string{"kind": "connect", "target": hostBlocked + ":443",
		"rule": "blocked", "action": "block", "outcome": "blocked", "status": "403"})
	note := "no mock received " + hostBlocked
	seen := k.mocksSaw(hostBlocked)
	if len(seen) > 0 {
		note += "; received: " + strings.Join(seen, "; ")
	}
	r.check(len(seen) == 0, note)
	return r
}

// mocksSaw lists every request any mock of the session received that names
// host in its target or Host field.
func (k *clientRun) mocksSaw(host string) []string {
	var out []string
	for name, u := range map[string]*testenv.RecordingUpstream{mockUpstream: k.sess.upstream,
		mockEgressDirect: k.sess.egressDirect, mockEgressChain: k.sess.egressChain} {
		for _, q := range u.Requests() {
			if strings.Contains(strings.ToLower(q.Target+" "+q.Get("Host")), host) {
				out = append(out, name+": "+q.Method+" "+q.Target)
			}
		}
	}
	for _, q := range k.sess.inspect.Requests() {
		if strings.Contains(strings.ToLower(q.Target+" "+q.Get("Host")), host) {
			out = append(out, "inspect: "+q.Method+" "+q.Target)
		}
	}
	for _, o := range []*testenv.Origin{k.sess.httpsOrigin, k.sess.httpOrigin} {
		for _, q := range o.Requests() {
			if strings.Contains(strings.ToLower(q.Host), host) {
				out = append(out, "origin: "+q.Method+" "+q.URI)
			}
		}
	}
	return out
}

func (k *clientRun) upstream() result {
	r, m := newResult(), k.mark()
	f := k.fetch(k.sess.Instances["single"].Listen, "https://"+hostOrigin+"/echo", k.sess.OriginCA)
	r.Notes = append(r.Notes, f.summary(k.client))
	r.check(echoed(f.Text, "/echo"), "the origin's echo of /echo")
	var connects int
	for _, q := range k.sess.upstream.Requests()[m.upstream:] {
		if q.Method != "CONNECT" || q.Target != hostOrigin+":443" {
			continue
		}
		connects++
		via := q.Values("Via")
		r.check(len(via) == 1 && via[0] == "1.1 "+nameSingle+" (cuttle)",
			fmt.Sprintf("one cuttle Via entry on upstream CONNECT #%d: got %q", q.Conn, via))
	}
	r.check(connects > 0, "a CONNECT "+hostOrigin+":443 recorded by the upstream mock")
	k.expectDecision(&r, m, "single", map[string]string{"kind": "connect", "target": hostOrigin + ":443",
		"rule": "cuttle-test", "action": "upstream", "upstream": "recording", "outcome": "ok"})
	return r
}

func (k *clientRun) webSocket() result {
	if k.client != "chrome" {
		return result{Status: statusNotRun, Notes: []string{"Chrome only (design 0007 D8)"}}
	}
	r, m := newResult(), k.mark()
	f := k.fetch(k.sess.Instances["single"].Listen, "http://"+hostWS+"/ws.html", k.sess.OriginCA)
	// The page's own status line is for information: Chrome may dump the
	// DOM before the echo arrives, so the mocks' records are the evidence.
	r.Notes = append(r.Notes, fmt.Sprintf("chrome exit %d; page status: %q", f.Exit, elementText(f.Text, `<pre id="out">`)))
	r.check(strings.Contains(f.Text, `<pre id="out">`), "the page ws.html in the DOM")
	connect := false
	for _, q := range k.sess.upstream.Requests()[m.upstream:] {
		if q.Method == "CONNECT" && q.Target == hostWS+":80" {
			connect = true
		}
	}
	r.check(connect, "CONNECT "+hostWS+":80 recorded by the upstream mock")
	msg := false
	for _, q := range k.sess.httpOrigin.Requests()[m.http:] {
		if q.Method == "WS" && string(q.Body) == "hello from ws.html" {
			msg = true
		}
	}
	r.check(msg, "the page's message recorded by the origin")
	k.expectDecision(&r, m, "single", map[string]string{"kind": "connect", "target": hostWS + ":80",
		"action": "upstream", "upstream": "recording", "outcome": "ok"})
	return r
}

func (k *clientRun) chain() result {
	r, m := newResult(), k.mark()
	f := k.fetch(k.sess.Instances["edge"].Listen, "https://"+hostInspected+"/chain", k.sess.InspectCA)
	r.Notes = append(r.Notes, f.summary(k.client))
	r.check(echoed(f.Text, "/chain"), "the origin's echo of /chain")
	if k.client == "curl" {
		r.check(f.Exit == 0 && f.Connect == "200" && f.Code == "200", "curl exit 0 trusting only the inspection root")
	}
	var connects int
	for _, q := range k.sess.inspect.Requests()[m.inspect:] {
		if q.Method != "CONNECT" || q.Target != hostInspected+":443" {
			continue
		}
		connects++
		var disclosed []string
		for _, fl := range q.Fields {
			if isDisclosureField(fl.Name) {
				disclosed = append(disclosed, fl.Name+": "+fl.Value)
			}
		}
		r.check(len(disclosed) == 0, fmt.Sprintf("no Via or other disclosure field on the inspecting mock's CONNECT #%d: got %q", q.Conn, disclosed))
	}
	r.check(connects > 0, "a CONNECT "+hostInspected+":443 recorded by the inspecting mock")
	dec := false
	for _, d := range k.sess.inspect.Decrypted()[m.decrypted:] {
		if d.URI == "/chain" {
			dec = true
		}
	}
	r.check(dec, "GET /chain decrypted by the inspecting mock")
	leaf := k.sess.inspect.Leaves()[hostInspected]
	r.check(leaf != nil && leaf.CheckSignatureFrom(k.sess.inspectRoot.Cert) == nil,
		"a leaf for "+hostInspected+" signed by the inspection root")
	k.expectDecision(&r, m, "edge", map[string]string{"kind": "connect", "target": hostInspected + ":443",
		"action": "upstream", "upstream": "gateway", "outcome": "ok"})
	k.expectDecision(&r, m, "gateway", map[string]string{"kind": "connect", "target": hostInspected + ":443",
		"action": "upstream", "upstream": "inspect", "outcome": "ok"})
	return r
}

// curlPost is the curl-only non-disclosure case with a body, so that the
// body comparison has bytes to compare.
const curlPost = "http://" + hostPlain + "/post"

func (k *clientRun) nonDisclosure() result {
	r := newResult()
	targets := append([]string{}, defaultTargets...)
	if k.client == "curl" {
		targets = append(targets, curlPost)
	}
	for _, pass := range []struct{ label, proxy string }{
		{"direct to " + mockEgressDirect, k.sess.Mocks[mockEgressDirect]},
		{"through edge and gateway to " + mockEgressChain, k.sess.Instances["edge"].Listen},
	} {
		for _, t := range targets {
			var extra []string
			if t == curlPost {
				extra = []string{"--data-binary", "mockenv body\tbytes&n=1"}
			}
			f := k.fetch(pass.proxy, t, k.sess.OriginCA, extra...)
			r.check(strings.Contains(f.Text, `"uri":`), fmt.Sprintf("%s %s: an echo (%s)", pass.label, t, f.summary(k.client)))
		}
	}
	var out bytes.Buffer
	opt := disclosureOptions{Dir: k.sess.Dir, Names: []string{nameEdge, nameGateway}, Clients: []string{"127.0.0.1"}, Targets: targets}
	fails, err := checkDisclosure(opt, &out)
	path := filepath.Join(k.sess.Dir, "check-disclosure.txt")
	os.WriteFile(path, out.Bytes(), 0o600)
	cmd := []string{"go", "run", "./tools/mockenv", "check-disclosure", "-dir", k.sess.Dir}
	if k.client == "curl" {
		for _, t := range targets {
			cmd = append(cmd, "-target", t)
		}
	}
	r.Notes = append(r.Notes, "rerun: "+shellQuote(cmd))
	switch {
	case err != nil:
		r.check(false, "check-disclosure ran: "+err.Error())
	default:
		r.check(fails == 0, fmt.Sprintf("check-disclosure exit 0: %d failures; output in %s: %s", fails, path, lastLine(out.String())))
	}
	if fails > 0 {
		for _, l := range strings.Split(out.String(), "\n") {
			if strings.HasPrefix(l, "FAIL ") {
				r.Notes = append(r.Notes, l)
			}
		}
	}
	return r
}

// lna is the observation of OI-0006 V6: a page from a public-looking
// origin, loaded through cuttle, fetches a private literal, a loopback
// literal, and a public literal. It records what Chrome did with each: sent
// it (the decision log shows it), blocked it, or held it for a permission
// that headless Chrome cannot show.
func (k *clientRun) lna() result {
	if k.client != "chrome" {
		return result{Status: statusNotRun, Notes: []string{"Chrome only (OI-0006 V6)"}}
	}
	if !k.opt.LNA {
		return result{Status: statusNotRun, Notes: []string{"disabled with -lna=false"}}
	}
	r, m := result{Status: statusObserved}, k.mark()
	type probe struct{ label, url, kind, target string }
	probes := []probe{
		{"private literal", "https://" + privateAddr + "/echo", "connect", privateAddr + ":443"},
		{"loopback literal", "http://" + k.sess.HTTPOrigin + "/echo", "http", k.sess.HTTPOrigin},
		{"public literal (control)", "https://" + originAddr + "/echo", "connect", originAddr + ":443"},
	}
	q := url.Values{}
	for _, p := range probes {
		q.Add("t", p.url)
	}
	page := "https://" + hostLNA + "/lna.html?" + q.Encode()
	f := k.fetch(k.sess.Instances["single"].Listen, page, k.sess.OriginCA)
	text := elementText(f.Text, `<pre id="out">`)
	r.Notes = append(r.Notes, "page "+page, fmt.Sprintf("chrome exit %d", f.Exit))
	for _, l := range strings.Split(strings.TrimSpace(text), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			r.Notes = append(r.Notes, "DOM: "+l)
		}
	}
	for _, p := range probes {
		outcome := "missing from the DOM"
		for _, l := range strings.Split(text, "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), "fetch "+p.url+" => ") {
				outcome = strings.TrimPrefix(strings.TrimSpace(l), "fetch "+p.url+" => ")
			}
		}
		d, sent := k.decision(m, "single", map[string]string{"kind": p.kind, "target": p.target})
		arrived := 0
		reqs := append(k.sess.httpsOrigin.Requests()[m.https:], k.sess.httpOrigin.Requests()[m.http:]...)
		for _, rq := range reqs {
			if rq.URI == "/echo" && strings.TrimSuffix(rq.Host, ":443") == strings.TrimSuffix(p.target, ":443") {
				arrived++
			}
		}
		var verdict string
		switch {
		case sent && strings.HasPrefix(outcome, "response"):
			verdict = "nothing: Chrome sent the request through cuttle without a permission request"
		case sent:
			verdict = "sent through cuttle, then failed in the page"
		case strings.HasPrefix(outcome, "pending"):
			verdict = "held: no request reached cuttle and the fetch never settled, as for a permission request headless Chrome cannot show"
		case strings.HasPrefix(outcome, "error"):
			verdict = "blocked: Chrome failed the fetch without sending it to cuttle"
		default:
			verdict = "unknown"
		}
		note := fmt.Sprintf("%s %s: page %q; decision line: %v; origin requests: %d; observed: %s", p.label, p.url, outcome, sent, arrived, verdict)
		if sent {
			note += " [" + d.raw + "]"
		}
		r.Notes = append(r.Notes, note)
	}
	return r
}

// elementText returns the visible text of the element that starts with the
// tag open, up to the next closing tag, from a DOM dump; "" without one.
func elementText(dom, open string) string {
	i := strings.Index(dom, open)
	if i < 0 {
		return ""
	}
	rest := dom[i+len(open):]
	if j := strings.Index(rest, "</"); j >= 0 {
		rest = rest[:j]
	}
	return visibleText(rest)
}

// visibleText strips tags from a DOM dump and decodes the common entities.
func visibleText(dom string) string {
	var b strings.Builder
	in := false
	for _, c := range dom {
		switch {
		case c == '<':
			in = true
		case c == '>':
			in = false
		case !in:
			b.WriteRune(c)
		}
	}
	return strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'").Replace(b.String())
}

// cuttleProc is one cuttle serve process.
type cuttleProc struct {
	cmd     *exec.Cmd
	done    chan struct{}
	err     error
	errPath string
}

// startCuttle starts cuttle serve with the instance's configuration and
// environment after checking that it is isolated as design 0007 D4 requires,
// and waits until it accepts connections.
func startCuttle(bin string, in instance, sessDir string) (*cuttleProc, error) {
	if err := checkIsolation(in, sessDir); err != nil {
		return nil, fmt.Errorf("refusing to start cuttle %s: %w", in.Role, err)
	}
	cmd := exec.Command(bin, "serve", "--config", in.Config)
	cmd.Env = in.Env()
	cmd.Dir = in.Home
	errPath := filepath.Join(sessDir, in.Role, "stderr.log")
	errf, err := os.Create(errPath)
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = errf, errf
	if err := cmd.Start(); err != nil {
		errf.Close()
		return nil, err
	}
	p := &cuttleProc{cmd: cmd, done: make(chan struct{}), errPath: errPath}
	go func() { p.err = cmd.Wait(); errf.Close(); close(p.done) }()
	deadline := time.Now().Add(15 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", in.Listen, 200*time.Millisecond)
		if err == nil {
			c.Close()
			return p, nil
		}
		select {
		case <-p.done:
			return nil, fmt.Errorf("cuttle %s exited before serving: %v: %s", in.Role, p.err, tail(p.stderr(), 600))
		default:
		}
		if time.Now().After(deadline) {
			p.stop()
			return nil, fmt.Errorf("cuttle %s did not serve on %s: %s", in.Role, in.Listen, tail(p.stderr(), 600))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// checkIsolation refuses an instance whose directories are not inside the
// session directory, whose runtime directory is not a 0700 cuttle-*
// directory under /tmp, or whose listen port is a live installation's.
func checkIsolation(in instance, sessDir string) error {
	for name, d := range map[string]string{"HOME": in.Home, "XDG_CONFIG_HOME": in.ConfigHome,
		"XDG_DATA_HOME": in.DataHome, "XDG_STATE_HOME": in.StateHome, "TMPDIR": in.TmpDir, "config": in.Config} {
		rel, err := filepath.Rel(sessDir, d)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
			return fmt.Errorf("%s=%s is outside the session directory", name, d)
		}
	}
	if home, _ := os.UserHomeDir(); home != "" && in.Home == home {
		return errors.New("HOME is the real home directory")
	}
	if filepath.Dir(in.RuntimeDir) != "/tmp" || !strings.HasPrefix(filepath.Base(in.RuntimeDir), "cuttle-") {
		return fmt.Errorf("XDG_RUNTIME_DIR=%s is not a cuttle-* directory under /tmp", in.RuntimeDir)
	}
	if fi, err := os.Stat(in.RuntimeDir); err != nil || fi.Mode().Perm() != 0o700 {
		return fmt.Errorf("XDG_RUNTIME_DIR=%s is missing or not 0700", in.RuntimeDir)
	}
	ap, err := netip.ParseAddrPort(in.Listen)
	if err != nil || !ap.Addr().IsLoopback() || ap.Port() == 3128 || ap.Port() == 3129 {
		return fmt.Errorf("listen address %s is not a chosen loopback port", in.Listen)
	}
	return nil
}

// stop sends SIGTERM, waits up to 10 s, then kills the process. It returns
// the exit status, or -1 if the process had to be killed.
func (p *cuttleProc) stop() int {
	select {
	case <-p.done:
		return exitCode(p.err)
	default:
	}
	p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
		return exitCode(p.err)
	case <-time.After(10 * time.Second):
		p.cmd.Process.Kill()
		<-p.done
		return -1
	}
}

func (p *cuttleProc) stderr() string {
	b, _ := os.ReadFile(p.errPath)
	return string(b)
}

// chromePattern matches the command lines of a Chrome run's processes: the
// browser and its helpers name the profile with --user-data-dir, and the
// crash handler names a directory inside it with --database.
func chromePattern(profile string) string {
	return "(--user-data-dir=|--database=)" + regexpQuote(profile) + "(/| |$)"
}

// startedPattern matches the command lines of every process the check
// starts under the evidence directory: cuttle serve with its session
// configuration, Chrome with its profile, and curl with a session root.
// It does not match other programs that merely name a file there, such as
// tee or tail on a report.
func startedPattern(out string) string {
	return "(serve --config |--user-data-dir=|--database=|--cacert )" + regexpQuote(out) + "/"
}

// waitGone waits until no process's command line matches the extended
// regular expression re, then kills any that remain.
func waitGone(re string, d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) && len(pgrep(re)) > 0 {
		time.Sleep(100 * time.Millisecond)
	}
	for _, pid := range pgrep(re) {
		syscall.Kill(pid, syscall.SIGKILL)
	}
}

// pgrep returns the PIDs of processes whose command line matches the
// extended regular expression re, without this process and its parent.
func pgrep(re string) []int {
	out, _ := exec.Command("/usr/bin/pgrep", "-f", "--", re).Output()
	var pids []int
	for _, f := range strings.Fields(string(out)) {
		pid, err := strconv.Atoi(f)
		if err == nil && pid != os.Getpid() && pid != os.Getppid() {
			pids = append(pids, pid)
		}
	}
	return pids
}

func regexpQuote(s string) string {
	var b strings.Builder
	for _, c := range s {
		if strings.ContainsRune(`\.+*?()|[]{}^$`, c) {
			b.WriteByte('\\')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// leftovers kills and lists the processes the check started that still
// run: cuttle, Chrome and its helpers, or curl.
func leftovers(out string) []string {
	var left []string
	for _, pid := range pgrep(startedPattern(out)) {
		cmd, _ := exec.Command("/bin/ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
		left = append(left, fmt.Sprintf("pid %d %s", pid, tail(oneLine(string(cmd)), 120)))
		syscall.Kill(pid, syscall.SIGKILL)
	}
	return left
}

// report writes the summary table and then every note.
func report(w io.Writer, results []result) {
	fmt.Fprintln(w, "\n== M1 client check results")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "client\tcase\tresult")
	failed := false
	for _, r := range results {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Client, r.Case, r.Status)
		failed = failed || r.Status == statusFail
	}
	tw.Flush()
	fmt.Fprintln(w, "\n== evidence")
	for _, r := range results {
		fmt.Fprintf(w, "%s %s: %s\n", r.Client, r.Case, r.Status)
		for _, n := range r.Notes {
			fmt.Fprintf(w, "    %s\n", n)
		}
	}
	if failed {
		fmt.Fprintln(w, "\nM1 client check: FAIL")
	} else {
		fmt.Fprintln(w, "\nM1 client check: pass")
	}
}

// logLine is one parsed log line.
type logLine struct {
	raw    string
	fields map[string]string
}

func (l logLine) has(want map[string]string) bool {
	for k, v := range want {
		if l.fields[k] != v {
			return false
		}
	}
	return true
}

// readDecisions returns the decision lines of a cuttle log.
func readDecisions(path string) ([]logLine, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []logLine
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		l := logLine{raw: sc.Text(), fields: parseLogfmt(sc.Text())}
		if l.fields["msg"] == "decision" {
			out = append(out, l)
		}
	}
	return out, sc.Err()
}

// parseLogfmt parses the key=value pairs of a log/slog text line.
func parseLogfmt(line string) map[string]string {
	m := map[string]string{}
	for i := 0; i < len(line); {
		for i < len(line) && line[i] == ' ' {
			i++
		}
		eq := strings.IndexByte(line[i:], '=')
		if eq < 0 {
			break
		}
		key := line[i : i+eq]
		i += eq + 1
		if i < len(line) && line[i] == '"' {
			j := i + 1
			for j < len(line) && line[j] != '"' {
				if line[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(line) {
				j = len(line) - 1
			}
			v, err := strconv.Unquote(line[i : j+1])
			if err != nil {
				v = line[i+1 : j]
			}
			m[key] = v
			i = j + 1
			continue
		}
		j := strings.IndexByte(line[i:], ' ')
		if j < 0 {
			j = len(line) - i
		}
		m[key] = line[i : i+j]
		i += j
	}
	return m
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// shellQuote renders a command line that a POSIX shell reads back as args.
func shellQuote(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		if a != "" && strings.Trim(a, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./:=,@%+") == "" {
			q[i] = a
			continue
		}
		q[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(q, " ")
}

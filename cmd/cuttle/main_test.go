package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"squid/internal/config"
	"squid/internal/paths"
	"squid/internal/testenv"
)

func TestMain(m *testing.M) {
	code := m.Run()
	testenv.CleanupBuild()
	os.Exit(code)
}

// tempEnv returns a lookup over a temporary HOME and XDG directories, with
// XDG_RUNTIME_DIR in a fresh 0700 directory under /tmp, so a control socket
// path stays short and never reaches a live one (design 0007 D4).
func tempEnv(t *testing.T) (env, map[string]string) {
	root := t.TempDir()
	rt, err := os.MkdirTemp("/tmp", "cuttle-")
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(rt, 0o700)
	t.Cleanup(func() { os.RemoveAll(rt) })
	vars := map[string]string{
		"HOME":            filepath.Join(root, "home"),
		"XDG_CONFIG_HOME": filepath.Join(root, "home", ".config"),
		"XDG_DATA_HOME":   filepath.Join(root, "home", ".local", "share"),
		"XDG_STATE_HOME":  filepath.Join(root, "home", ".local", "state"),
		"XDG_RUNTIME_DIR": rt,
		"TMPDIR":          filepath.Join(root, "tmp"),
	}
	os.MkdirAll(vars["HOME"], 0o700)
	return func(k string) (string, bool) { v, ok := vars[k]; return v, ok }, vars
}

func runCLI(lookup env, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run(args, lookup, &out, &errb)
	return code, out.String(), errb.String()
}

var starterName = regexp.MustCompile(`(?m)^name = "(cuttle-[0-9a-f]{6})"`)

func checkStarterFile(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m := starterName.FindSubmatch(data)
	if m == nil {
		t.Fatalf("no cuttle-<hex> name in %s", path)
	}
	if want := strings.Replace(config.StarterTemplate(), "NAME", string(m[1]), 1); string(data) != want {
		t.Errorf("%s is not the D5 starter file", path)
	}
	fi, _ := os.Stat(path)
	di, _ := os.Stat(filepath.Dir(path))
	if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
		t.Errorf("modes: file %v dir %v", fi.Mode(), di.Mode())
	}
}

// TestServeWritesStarter checks the T0 part of K2: serve's first-run steps
// write the same starter file as init when none exists. It binds nothing.
func TestServeWritesStarter(t *testing.T) {
	lookup, vars := tempEnv(t)
	s, err := prepareServe(lookup, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	path := filepath.Join(vars["XDG_CONFIG_HOME"], "cuttle", "config.toml")
	if s.cfgPath != path || s.cfg == nil {
		t.Fatalf("config %q %v", s.cfgPath, s.cfg)
	}
	checkStarterFile(t, path)
	log, _ := os.ReadFile(filepath.Join(vars["XDG_STATE_HOME"], "cuttle", "cuttle.log"))
	if !strings.Contains(string(log), `msg="wrote starter configuration"`) {
		t.Errorf("log %q", log)
	}

	// init writes the same file.
	lookup2, vars2 := tempEnv(t)
	code, out, _ := runCLI(lookup2, "init")
	path2 := filepath.Join(vars2["XDG_CONFIG_HOME"], "cuttle", "config.toml")
	if code != 0 || out != path2+"\n" {
		t.Fatalf("init: %d %q", code, out)
	}
	checkStarterFile(t, path2)
	a, _ := os.ReadFile(path)
	b, _ := os.ReadFile(path2)
	if starterName.ReplaceAll(a, nil) == nil || !bytes.Equal(starterName.ReplaceAll(a, nil), starterName.ReplaceAll(b, nil)) {
		t.Errorf("serve and init wrote different files")
	}
	// A second init leaves the file alone.
	if code, _, errOut := runCLI(lookup2, "init"); code != 0 || !strings.Contains(errOut, "already exists") {
		t.Errorf("second init: %d %q", code, errOut)
	}
	if c, _ := os.ReadFile(path2); !bytes.Equal(c, b) {
		t.Errorf("second init changed the file")
	}
}

func TestServeRefusesGroupWritable(t *testing.T) {
	lookup, vars := tempEnv(t)
	path := filepath.Join(vars["XDG_CONFIG_HOME"], "cuttle", "config.toml")
	if code, _, _ := runCLI(lookup, "init"); code != 0 {
		t.Fatal("init failed")
	}
	os.Chmod(path, 0o620)
	if _, err := prepareServe(lookup, "", io.Discard); err == nil || !strings.Contains(err.Error(), "writable by group or others") {
		t.Fatalf("prepareServe: %v", err)
	}
}

// copySample copies a sample configuration into dir with mode 0600, so the
// checkout's file modes do not matter.
func copySample(t *testing.T, dir, src string) string {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, filepath.Base(src))
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return dst
}

var sampleWant = regexp.MustCompile(`^# (want|warn): (\d+) ([^ :]*): (.+)$`)

// sampleIssues returns the header lines of a sample file as the lines
// cuttle check prints for them, without the message, and the message each
// must contain.
func sampleIssues(t *testing.T, path string, data []byte) (prefixes, msgs []string, warnings int) {
	t.Helper()
	for _, line := range strings.Split(string(data), "\n") {
		m := sampleWant.FindStringSubmatch(line)
		if m == nil {
			break
		}
		kind := "error"
		if m[1] == "warn" {
			kind = "warning"
			warnings++
		}
		loc := path
		if m[2] != "0" {
			loc += ":" + m[2]
		}
		p := kind + ": " + loc + ": "
		if m[3] != "" {
			p += m[3] + ": "
		}
		prefixes, msgs = append(prefixes, p), append(msgs, m[4])
	}
	if len(prefixes) == 0 {
		t.Fatalf("%s: no expectation header", path)
	}
	return prefixes, msgs, warnings
}

// hasLine reports whether out has a line that starts with prefix and
// contains msg.
func hasLine(out, prefix, msg string) bool {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, prefix) && strings.Contains(l[len(prefix):], msg) {
			return true
		}
	}
	return false
}

// TestCheckCommand checks cuttle check: each warning sample prints its
// warning with exit status 0 (K1), errors exit 1, and usage errors exit 2
// (P7). Every issue in a sample's header must appear with its line and key
// path. It runs in-process for T0 and also against the built binary for T1.
func TestCheckCommand(t *testing.T) {
	lookup, _ := tempEnv(t)
	dir := t.TempDir()

	warn, _ := filepath.Glob("../../internal/config/testdata/warn/*.toml")
	invalid, _ := filepath.Glob("../../internal/config/testdata/invalid/*.toml")
	if len(warn) != 4 || len(invalid) < 40 {
		t.Fatalf("samples: %d warn, %d invalid", len(warn), len(invalid))
	}
	for _, src := range append(warn, invalid...) {
		path := copySample(t, dir, src)
		data, _ := os.ReadFile(path)
		prefixes, msgs, warnings := sampleIssues(t, path, data)
		code, out, _ := runCLI(lookup, "check", "--config", path)
		wantCode, summary := 1, " error(s), "
		if strings.HasPrefix(prefixes[0], "warning: ") {
			wantCode, summary = 0, fmt.Sprintf(": ok, %d warning(s)", warnings)
		}
		if code != wantCode || !strings.Contains(out, summary) {
			t.Errorf("%s: exit %d, output:\n%s", src, code, out)
		}
		for i := range prefixes {
			if !hasLine(out, prefixes[i], msgs[i]) {
				t.Errorf("%s: no line %q...%q in output:\n%s", src, prefixes[i], msgs[i], out)
			}
		}
	}
	valid := filepath.Join(dir, "valid.toml")
	os.WriteFile(valid, []byte("name = \"cuttle-test\"\n[proxy]\ndefault = \"direct\"\n"), 0o600)
	if code, out, _ := runCLI(lookup, "check", "--config", valid); code != 0 || out != valid+": ok, 0 warning(s)\n" {
		t.Errorf("valid: %d %q", code, out)
	}
	if code, _, _ := runCLI(lookup, "check", "--config", filepath.Join(dir, "missing.toml")); code != 1 {
		t.Errorf("missing file: %d", code)
	}
	loose := filepath.Join(dir, "loose.toml")
	os.WriteFile(loose, []byte("name = \"cuttle-test\"\n[proxy]\ndefault = \"direct\"\n"), 0o600)
	os.Chmod(loose, 0o660)
	if code, out, _ := runCLI(lookup, "check", "--config", loose); code != 1 || !strings.Contains(out, "writable by group or others") {
		t.Errorf("group-writable: %d %q", code, out)
	}
	for _, args := range [][]string{{"check", "--bogus"}, {"check", "extra"}, {"check", "--config"}} {
		if code, _, _ := runCLI(lookup, args...); code != 2 {
			t.Errorf("%v: %d", args, code)
		}
	}

	if testing.Short() {
		return
	}
	// T1: the built binary gives the same statuses.
	sb := testenv.NewSandbox(t, testenv.BuildCuttle(t))
	os.MkdirAll(filepath.Dir(sb.ConfigPath()), 0o700)
	put := func(name, text string) string {
		p := filepath.Join(filepath.Dir(sb.ConfigPath()), name)
		os.WriteFile(p, []byte(text), 0o600)
		return p
	}
	cases := []struct {
		args []string
		code int
		out  string
	}{
		{[]string{"check", "--config", put("ok.toml", "name = \"cuttle-test\"\n[proxy]\ndefault = \"direct\"\n")}, 0, "ok, 0 warning(s)"},
		{[]string{"check", "--config", put("warn.toml", "name = \"cuttle-test\"\n[proxy]\ndefault = \"direct\"\n[[upstream]]\nname = \"gw\"\naddress = \"10.8.0.6:3128\"\ndisclosure = \"via\"\n")},
			0, ":5: upstream[1].name: upstream \"gw\" is not used by any action"},
		{[]string{"check", "--config", put("bad.toml", "name = \"cuttle-test\"\n[proxy]\ndefault = \"deny\"\n")}, 1, ":3: proxy.default: invalid action"},
		{[]string{"check", "--bogus"}, 2, ""},
		{[]string{"check", "extra"}, 2, ""},
	}
	for _, c := range cases {
		r := sb.Run(10*time.Second, c.args...)
		if r.Code != c.code || !strings.Contains(r.Stdout, c.out) {
			t.Errorf("binary %v: exit %d out %q err %q", c.args, r.Code, r.Stdout, r.Stderr)
		}
	}
}

// TestExplainCommand checks P7: cuttle explain prints the D3 fields for
// direct, upstream, block, and --remote targets.
func TestExplainCommand(t *testing.T) {
	if testing.Short() {
		t.Skip("binary test")
	}
	sb := testenv.NewSandbox(t, testenv.BuildCuttle(t))
	os.MkdirAll(filepath.Dir(sb.ConfigPath()), 0o700)
	os.WriteFile(sb.ConfigPath(), []byte(`name = "cuttle-edge"

[proxy]
default = "direct"

[[upstream]]
name = "gateway"
address = "10.8.0.6:3128"
disclosure = "via"

[[rule]]
name = "loopback"
hosts = ["localhost"]
cidrs = ["127.0.0.0/8", "::1/128"]
action = "direct"
allow_loopback = true

[[rule]]
name = "restricted"
domains = ["internal.example"]
action = "upstream:gateway"

[[rule]]
name = "ads"
domains = ["ads.example.net"]
action = "block"
`), 0o600)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"https://wiki.internal.example/"},
			"target      wiki.internal.example:443 (connect, local client)\nrule        restricted (#2)\naction      upstream:gateway -> 10.8.0.6:3128 (disclosure via)\ninterfaces  system routing\n"},
		{[]string{"http://localhost:3000/x?y"},
			"target      localhost:3000 (http, local client)\nrule        loopback (#1)\naction      direct (allow_loopback)\ninterfaces  system routing\n"},
		{[]string{"ads.example.net:443"},
			"target      ads.example.net:443 (connect, local client)\nrule        ads (#3)\naction      block (403)\ninterfaces  system routing\n"},
		{[]string{"Example.COM:8443"},
			"target      example.com:8443 (connect, local client)\nrule        default (proxy.default)\naction      direct\ninterfaces  system routing\n"},
		{[]string{"--remote", "https://wiki.internal.example/"},
			"target      wiki.internal.example:443 (connect, remote client)\nrule        restricted (#2)\naction      upstream:gateway -> 10.8.0.6:3128 (disclosure via)\ninterfaces  system routing\n"},
		{[]string{"--remote", "example.com:443"},
			"target      example.com:443 (connect, remote client)\nrule        remote-default (proxy.remote.default)\naction      block (403)\ninterfaces  system routing\n"},
		{[]string{"--remote", "wiki.internal.example:22"},
			"target      wiki.internal.example:22 (connect, remote client)\nrule        port (port 22 is not in proxy.remote.ports)\naction      block (403)\ninterfaces  system routing\n"},
		{[]string{"--remote", "http://localhost/"},
			"target      localhost:80 (http, remote client)\nrule        remote-direct (direct needs proxy.remote.allow_direct)\naction      block (403)\ninterfaces  system routing\n"},
	} {
		r := sb.Run(10*time.Second, append([]string{"explain"}, c.args...)...)
		if r.Code != 0 || r.Stdout != c.want {
			t.Errorf("explain %v: exit %d\n%s\nwant\n%s%s", c.args, r.Code, r.Stdout, c.want, r.Stderr)
		}
	}
	for _, args := range [][]string{{"explain"}, {"explain", "ftp://example.com/"}, {"explain", "example.com"}} {
		if r := sb.Run(10*time.Second, args...); r.Code != 2 {
			t.Errorf("%v: exit %d", args, r.Code)
		}
	}
}

// startServe writes a configuration on chosen ports from the starter and
// starts cuttle serve.
func startServe(t *testing.T, sb *testenv.Sandbox) (*testenv.Process, string) {
	t.Helper()
	data, err := os.ReadFile(sb.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	addr := testenv.Port(sb.FreePort())
	text := strings.Replace(string(data), `listen = ["127.0.0.1:3128"]`, `listen = ["`+addr+`"]`, 1)
	text = strings.Replace(text, `listen = "127.0.0.1:3129"`, `listen = ""`, 1)
	if text == string(data) {
		t.Fatal("starter listen lines not found")
	}
	if err := os.WriteFile(sb.ConfigPath(), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return sb.Start(addr, "serve"), addr
}

// TestFirstRun checks the T1 part of K2: cuttle init creates ~/.config/cuttle
// (0700) and the starter file (0600), which passes cuttle check; cuttle
// serve, with proxy.listen on a chosen port and admin.listen "", serves on
// that port.
func TestFirstRun(t *testing.T) {
	if testing.Short() {
		t.Skip("binary test")
	}
	sb := testenv.NewSandbox(t, testenv.BuildCuttle(t))
	r := sb.Run(10*time.Second, "init")
	if r.Code != 0 || r.Stdout != sb.ConfigPath()+"\n" {
		t.Fatalf("init: %d %q %q", r.Code, r.Stdout, r.Stderr)
	}
	if sb.ConfigPath() != filepath.Join(sb.Home, ".config", "cuttle", "config.toml") {
		t.Fatalf("config path %s", sb.ConfigPath())
	}
	checkStarterFile(t, sb.ConfigPath())
	if r := sb.Run(10*time.Second, "check"); r.Code != 0 {
		t.Fatalf("check: %d %q", r.Code, r.Stdout)
	}

	proc, addr := startServe(t, sb)
	ca := testenv.NewCA(t, "cuttle test origin root")
	plain := testenv.NewOrigin(t, testenv.OriginOptions{})
	secure := testenv.NewOrigin(t, testenv.OriginOptions{TLS: ca})
	tr := &http.Transport{
		Proxy:              http.ProxyURL(&url.URL{Scheme: "http", Host: addr}),
		TLSClientConfig:    &tls.Config{RootCAs: ca.Pool()},
		DisableCompression: true,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	for _, u := range []string{
		"http://" + plain.Addr.String() + "/echo",
		"https://" + secure.Addr.String() + "/echo",
	} {
		res, err := client.Get(u)
		if err != nil {
			t.Fatalf("%s: %v\n%s", u, err, proc.Stderr())
		}
		var rec testenv.Request
		json.NewDecoder(res.Body).Decode(&rec)
		res.Body.Close()
		if res.StatusCode != 200 || rec.URI != "/echo" {
			t.Errorf("%s: %d %+v", u, res.StatusCode, rec)
		}
	}
	tr.CloseIdleConnections()
	if code := proc.Stop(); code != 0 {
		t.Errorf("exit status %d after SIGTERM", code)
	}
	log, _ := os.ReadFile(sb.LogPath())
	for _, want := range []string{"msg=started", "listeners=" + addr, "msg=decision", "action=direct", "outcome=ok", "msg=stopped"} {
		if !strings.Contains(string(log), want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
}

// TestPermissions checks P3: after first run every created directory is
// 0700 and every file 0600, and a group-writable configuration stops cuttle
// serve with an error.
func TestPermissions(t *testing.T) {
	if testing.Short() {
		t.Skip("binary test")
	}
	sb := testenv.NewSandbox(t, testenv.BuildCuttle(t))
	if r := sb.Run(10*time.Second, "init"); r.Code != 0 {
		t.Fatal(r.Stderr)
	}
	proc, _ := startServe(t, sb)
	if code := proc.Stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var seen []string
	filepath.WalkDir(sb.Home, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == sb.Home {
			return err
		}
		fi, _ := d.Info()
		rel, _ := filepath.Rel(sb.Home, path)
		seen = append(seen, rel)
		want := os.FileMode(0o600)
		if d.IsDir() {
			want = 0o700
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s: mode %04o, want %04o", rel, fi.Mode().Perm(), want)
		}
		return nil
	})
	for _, want := range []string{".config/cuttle/config.toml", ".local/state/cuttle/cuttle.log", ".local/state/cuttle/stats.db"} {
		found := false
		for _, s := range seen {
			found = found || s == want
		}
		if !found {
			t.Errorf("%s not created; saw %v", want, seen)
		}
	}

	if err := os.Chmod(sb.ConfigPath(), 0o620); err != nil {
		t.Fatal(err)
	}
	r := sb.Run(10*time.Second, "serve")
	if r.Code != 1 || !strings.Contains(r.Stderr, "writable by group or others") {
		t.Errorf("group-writable serve: exit %d stderr %q", r.Code, r.Stderr)
	}
}

func TestPathsAndVersion(t *testing.T) {
	lookup, vars := tempEnv(t)
	code, out, _ := runCLI(lookup, "paths")
	if code != 0 || !strings.Contains(out, filepath.Join(vars["XDG_CONFIG_HOME"], "cuttle", "config.toml")) ||
		!strings.Contains(out, "missing") || !strings.HasPrefix(out, "config ") {
		t.Errorf("paths: %d %q", code, out)
	}
	code, out, _ = runCLI(lookup, "paths", "--json")
	var p paths.Paths
	if err := json.Unmarshal([]byte(out), &p); code != 0 || err != nil || p.Log.Path != filepath.Join(vars["XDG_STATE_HOME"], "cuttle", "cuttle.log") {
		t.Errorf("paths --json: %d %v %q", code, err, out)
	}
	if code, out, _ := runCLI(lookup, "version"); code != 0 || !strings.HasPrefix(out, "cuttle dev (go") {
		t.Errorf("version: %d %q", code, out)
	}
	if code, _, _ := runCLI(lookup, "--version"); code != 0 {
		t.Errorf("--version: %d", code)
	}
	for _, args := range [][]string{{}, {"bogus"}, {"paths", "extra"}, {"admin"}, {"admin", "bogus"}, {"admin", "passwd", "extra"}} {
		if code, _, _ := runCLI(lookup, args...); code != 2 {
			t.Errorf("%v: %d", args, code)
		}
	}
	for _, cmd := range []string{"reload", "status"} {
		if code, _, errOut := runCLI(lookup, cmd); code != 1 || errOut == "" {
			t.Errorf("%s: %d %q", cmd, code, errOut)
		}
	}
}

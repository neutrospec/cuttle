package config

import (
	"bytes"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/neutrospec/cuttle/internal/policy"
)

// expectation is one header line of a sample file:
// "# want: LINE KEY: MESSAGE" for an error or "# warn: LINE KEY: MESSAGE"
// for a warning. A sample starts with one or more such lines; the first
// names the issue the sample is for. An empty KEY is not checked.
type expectation struct {
	kind string // "want" or "warn"
	line int
	key  string
	msg  string
}

// A key may contain ":", as a quoted MAC key does; it ends at the first
// ": ", which no key path contains (OI-0006 U-R5).
var expectRe = regexp.MustCompile(`^# (want|warn): (\d+) (\S*): (.+)$`)

// readExpectations returns the header lines of a sample and its content.
func readExpectations(t *testing.T, path string) ([]expectation, []byte) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []expectation
	for _, line := range strings.Split(string(data), "\n") {
		m := expectRe.FindStringSubmatch(line)
		if m == nil {
			break
		}
		n, _ := strconv.Atoi(m[2])
		out = append(out, expectation{m[1], n, m[3], m[4]})
	}
	if len(out) == 0 {
		t.Fatalf("%s: first line does not document the expected issue", path)
	}
	return out, data
}

func findIssue(issues []Issue, e expectation) bool {
	for _, is := range issues {
		if is.Line == e.line && (e.key == "" || is.Key == e.key) && strings.Contains(is.Msg, e.msg) {
			return true
		}
	}
	return false
}

// checkExpectations looks up every header line of a sample in the report.
func checkExpectations(t *testing.T, path string, want []expectation, r *Report) {
	t.Helper()
	for _, e := range want {
		list, kind := r.Errors, "error"
		if e.kind == "warn" {
			list, kind = r.Warnings, "warning"
		}
		if !findIssue(list, e) {
			t.Errorf("%s: want %s line %d key %q message %q; got:\n%s", path, kind, e.line, e.key, e.msg, issues(list))
		}
	}
}

// TestConfigValidation checks that each design 0004 D4 error has a sample
// file that fails with the documented key path and line, and that each
// warning is reported without failing validation (K1). Samples whose
// header lists more issues also check those, such as warnings that must
// name an element as written even when an earlier element was refused.
func TestConfigValidation(t *testing.T) {
	invalid, _ := filepath.Glob("testdata/invalid/*.toml")
	if len(invalid) < 40 {
		t.Fatalf("only %d invalid samples", len(invalid))
	}
	for _, path := range invalid {
		want, data := readExpectations(t, path)
		if want[0].kind != "want" {
			t.Fatalf("%s: invalid sample with %q", path, want[0].kind)
		}
		cfg, r := Parse(data, path)
		if cfg != nil || len(r.Errors) == 0 {
			t.Errorf("%s: accepted", path)
			continue
		}
		checkExpectations(t, path, want, r)
		for _, e := range r.Errors {
			if !strings.HasPrefix(e.String(), path) {
				t.Errorf("%s: message does not name the file: %s", path, e)
			}
		}
	}
	warn, _ := filepath.Glob("testdata/warn/*.toml")
	if len(warn) != 4 {
		t.Fatalf("want 4 warning samples, have %d", len(warn))
	}
	for _, path := range warn {
		want, data := readExpectations(t, path)
		for _, e := range want {
			if e.kind != "warn" {
				t.Fatalf("%s: warning sample with %q", path, e.kind)
			}
		}
		cfg, r := Parse(data, path)
		if cfg == nil || len(r.Errors) > 0 {
			t.Errorf("%s: rejected:\n%s", path, issues(r.Errors))
			continue
		}
		checkExpectations(t, path, want, r)
	}
}

func issues(list []Issue) string {
	var b strings.Builder
	for _, i := range list {
		fmt.Fprintf(&b, "  %s\n", i)
	}
	return b.String()
}

func TestStarterIsValid(t *testing.T) {
	data, err := Starter()
	if err != nil {
		t.Fatal(err)
	}
	cfg, r := Parse(data, "starter")
	if cfg == nil {
		t.Fatalf("starter invalid:\n%s", issues(r.Errors))
	}
	if len(r.Warnings) != 0 {
		t.Errorf("starter warnings:\n%s", issues(r.Warnings))
	}
	if !regexp.MustCompile(`^cuttle-[0-9a-f]{6}$`).MatchString(cfg.Name) {
		t.Errorf("name %q", cfg.Name)
	}
	if want := strings.Replace(StarterTemplate(), "NAME", cfg.Name, 1); string(data) != want {
		t.Errorf("starter differs from the template")
	}
	if len(cfg.Proxy.Listen) != 1 || cfg.Proxy.Listen[0] != DefaultListen || cfg.Admin.Listen != DefaultAdminListen {
		t.Errorf("listeners %v %v", cfg.Proxy.Listen, cfg.Admin.Listen)
	}
	if cfg.Proxy.Default.Action != policy.ActionDirect || len(cfg.Rules) != 1 {
		t.Fatalf("default %v rules %d", cfg.Proxy.Default, len(cfg.Rules))
	}
	r0 := cfg.Rules[0]
	if r0.Name != "loopback" || !r0.AllowLoopback || r0.AllowPrivate || len(r0.CIDRs) != 2 || r0.Hosts[0] != "localhost" {
		t.Errorf("loopback rule %+v", r0)
	}
	other, _ := Starter()
	if bytes.Equal(data, other) {
		t.Errorf("two starters share a name")
	}
}

func TestDefaultsAndValues(t *testing.T) {
	src := `
name = "cuttle-edge"

[proxy]
listen = ["127.0.0.1:18080", "[::1]:18080"]
default = "block"
connect_timeout = "2s"
idle_timeout = "0s"

[proxy.remote]
sources = ["10.8.0.1/32"]
ports = [443]
allow_direct = true
default = "direct"

[[upstream]]
name = "gateway"
address = "GW.example:3128"
disclosure = "none"

[[rule]]
domains = ["internal.example"]
ports = [443, 8443]
action = "upstream:gateway"

[[rule]]
name = "lan"
cidrs = ["192.168.0.0/16", "fd00::/8"]
action = "direct"
allow_private = true

[admin]
listen = ""

[log]
level = "debug"
`
	cfg, r := Parse([]byte(src), "x.toml")
	if cfg == nil {
		t.Fatalf("errors:\n%s", issues(r.Errors))
	}
	p := cfg.Proxy
	if p.ConnectTimeout != 2*time.Second || p.ResponseTimeout != 60*time.Second || p.IdleTimeout != 0 || p.MaxConnections != 1024 {
		t.Errorf("timeouts %+v", p)
	}
	if len(p.Listen) != 2 || p.Listen[1] != netip.MustParseAddrPort("[::1]:18080") {
		t.Errorf("listen %v", p.Listen)
	}
	if !p.Remote.AllowDirect || p.Remote.Default.Action != policy.ActionDirect || len(p.Remote.Ports) != 1 || p.Remote.MaxConnectionsPerSource != 128 {
		t.Errorf("remote %+v", p.Remote)
	}
	u, ok := cfg.Upstream("gateway")
	if !ok || u.Address != "gw.example:3128" || u.Disclosure != "none" || u.UnreachableHold != 5*time.Second || u.ConnectTimeout != 10*time.Second {
		t.Errorf("upstream %+v", u)
	}
	if cfg.Rules[0].Name != "rule-1" || cfg.Rules[0].Route.Upstream != "gateway" || cfg.Rules[1].Name != "lan" {
		t.Errorf("rules %+v", cfg.Rules)
	}
	if cfg.Admin.Listen.IsValid() || cfg.Log.Level != "debug" || cfg.Stats.RetentionDays != 30 {
		t.Errorf("admin %v log %v stats %v", cfg.Admin.Listen, cfg.Log, cfg.Stats)
	}
	if cfg.SHA256 != Digest([]byte(src)) {
		t.Errorf("digest")
	}
}

func TestLoadSizeLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.toml")
	big := "name = \"cuttle-test\"\n[proxy]\ndefault = \"direct\"\n" + strings.Repeat("# padding\n", MaxFileSize/10+1)
	if err := os.WriteFile(path, []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	_, r, err := Load(path)
	if err == nil || len(r.Errors) != 1 || !strings.Contains(r.Errors[0].Msg, "256 KiB") {
		t.Fatalf("big file: %v %+v", err, r)
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error type %T", err)
	}
}

func TestCheckFileSecurity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckFileSecurity(path); err != nil {
		t.Fatalf("0600: %v", err)
	}
	for _, mode := range []os.FileMode{0o620, 0o602, 0o660} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if err := CheckFileSecurity(path); !errors.Is(err, ErrInsecureFile) {
			t.Errorf("%04o: %v", mode, err)
		}
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckFileSecurity(path); err != nil {
		t.Errorf("0644 is not writable by others: %v", err)
	}
}

// TestAtomicWrite checks design 0004 D7 (K7).
func TestAtomicWrite(t *testing.T) {
	t.Run("concurrent reader", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.toml")
		a := bytes.Repeat([]byte("a"), 200<<10)
		b := bytes.Repeat([]byte("b"), 150<<10)
		if err := WriteAtomic(path, a, WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		var stop atomic.Bool
		var bad atomic.Value
		var reads atomic.Int64
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				got, err := os.ReadFile(path)
				if err != nil {
					bad.Store(err.Error())
					return
				}
				if !bytes.Equal(got, a) && !bytes.Equal(got, b) {
					bad.Store(fmt.Sprintf("partial content of %d bytes", len(got)))
					return
				}
				reads.Add(1)
			}
		}()
		for i := range 60 {
			data := a
			if i%2 == 0 {
				data = b
			}
			if err := WriteAtomic(path, data, WriteOptions{Backup: true}); err != nil {
				t.Fatal(err)
			}
		}
		stop.Store(true)
		wg.Wait()
		if v := bad.Load(); v != nil {
			t.Fatalf("reader saw %v", v)
		}
		if reads.Load() == 0 {
			t.Fatal("reader never read")
		}
		left, _ := filepath.Glob(filepath.Join(dir, ".config.toml.tmp-*"))
		if len(left) != 0 {
			t.Errorf("temporary files left: %v", left)
		}
	})

	t.Run("symlink and backup", func(t *testing.T) {
		root := t.TempDir()
		dotfiles := filepath.Join(root, "dotfiles")
		cfgdir := filepath.Join(root, "config")
		for _, d := range []string{dotfiles, cfgdir} {
			if err := os.Mkdir(d, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		target := filepath.Join(dotfiles, "cuttle.toml")
		link := filepath.Join(cfgdir, "config.toml")
		if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../dotfiles/cuttle.toml", link); err != nil {
			t.Fatal(err)
		}
		before, _ := os.Stat(target)
		if err := WriteAtomic(link, []byte("new"), WriteOptions{ExpectSHA256: Digest([]byte("old")), Backup: true}); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Lstat(link)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("link replaced: %v %v", fi, err)
		}
		if dest, _ := os.Readlink(link); dest != "../dotfiles/cuttle.toml" {
			t.Errorf("link now points to %q", dest)
		}
		after, _ := os.Stat(target)
		if os.SameFile(before, after) {
			t.Errorf("target was rewritten in place, not replaced")
		}
		if got, _ := os.ReadFile(link); string(got) != "new" {
			t.Errorf("content %q", got)
		}
		if got, err := os.ReadFile(target + ".bak"); err != nil || string(got) != "old" {
			t.Errorf(".bak beside the target: %q %v", got, err)
		}
		if _, err := os.Stat(link + ".bak"); err == nil {
			t.Errorf(".bak written beside the link")
		}
		if after.Mode().Perm() != 0o600 {
			t.Errorf("mode %v", after.Mode())
		}
		// A temporary file is created beside the target, not the link: make
		// the link's directory read-only and write again.
		if err := os.Chmod(cfgdir, 0o500); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(cfgdir, 0o700)
		if err := WriteAtomic(link, []byte("newer"), WriteOptions{Backup: true}); err != nil {
			t.Fatalf("write through a read-only link directory: %v", err)
		}
		if got, _ := os.ReadFile(target + ".bak"); string(got) != "new" {
			t.Errorf(".bak holds %q, want the previous content", got)
		}
	})

	t.Run("stale digest", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte("current"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := WriteAtomic(path, []byte("mine"), WriteOptions{ExpectSHA256: Digest([]byte("what I read")), Backup: true})
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("stale digest: %v", err)
		}
		if got, _ := os.ReadFile(path); string(got) != "current" {
			t.Errorf("file changed to %q", got)
		}
		if _, err := os.Stat(path + ".bak"); err == nil {
			t.Errorf(".bak written on conflict")
		}
		left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".*tmp-*"))
		if len(left) != 0 {
			t.Errorf("temporary files left: %v", left)
		}
	})

	t.Run("failure leaves target", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.toml")
		if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(dir, 0o700)
		if err := WriteAtomic(path, []byte("lost"), WriteOptions{}); err == nil {
			t.Fatal("write into a read-only directory succeeded")
		}
		if got, _ := os.ReadFile(path); string(got) != "keep" {
			t.Errorf("target changed to %q", got)
		}
	})
}

// TestAtomicWriteDirSync checks W14 of design 0009 (OI-0006 U-R1): when
// the directory cannot be opened for the sync after the rename, the write
// reports ErrNotSynced, and the file and .bak already hold the new and the
// previous content. A directory with mode 0300 allows the renames but not
// the open.
func TestAtomicWriteDirSync(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens any directory")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o300); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	err := WriteAtomic(path, []byte("new"), WriteOptions{ExpectSHA256: Digest([]byte("old")), Backup: true})
	if !errors.Is(err, ErrNotSynced) || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("WriteAtomic: %v, want ErrNotSynced", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "new" {
		t.Errorf("file holds %q, want the new content", got)
	}
	if got, _ := os.ReadFile(path + ".bak"); string(got) != "old" {
		t.Errorf(".bak holds %q, want the previous content", got)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".config.toml.tmp-*")); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
	// Every other failure still means that the file is unchanged
	// (OI-0006 Z-R2): a conflict, and a directory with mode 0500, where
	// the temporary file for .bak cannot be created, before any rename.
	if err := WriteAtomic(path, []byte("newer"), WriteOptions{ExpectSHA256: Digest([]byte("old"))}); !errors.Is(err, ErrConflict) || errors.Is(err, ErrNotSynced) {
		t.Errorf("conflict: %v", err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	err = WriteAtomic(path, []byte("newer"), WriteOptions{ExpectSHA256: Digest([]byte("new")), Backup: true})
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err == nil || errors.Is(err, ErrNotSynced) {
		t.Errorf("write into a 0500 directory: %v, want an error other than ErrNotSynced", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "new" {
		t.Errorf("after the failed write the file holds %q", got)
	}
	if got, _ := os.ReadFile(path + ".bak"); string(got) != "old" {
		t.Errorf("after the failed write .bak holds %q", got)
	}
}

func TestEnsureStarter(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "xdg", "cuttle", "config.toml")
	wrote, err := EnsureStarter(path)
	if err != nil || !wrote {
		t.Fatalf("EnsureStarter: %v %v", wrote, err)
	}
	for _, d := range []string{filepath.Join(root, "xdg"), filepath.Join(root, "xdg", "cuttle")} {
		fi, err := os.Stat(d)
		if err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("%s: %v %v", d, fi.Mode(), err)
		}
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v %v", fi.Mode(), err)
	}
	first, _ := os.ReadFile(path)
	wrote, err = EnsureStarter(path)
	if err != nil || wrote {
		t.Fatalf("second EnsureStarter: %v %v", wrote, err)
	}
	if again, _ := os.ReadFile(path); !bytes.Equal(first, again) {
		t.Errorf("existing file replaced")
	}
}

func TestLineIndex(t *testing.T) {
	src := `# comment
name = "x" # trailing
proxy.default = "direct"
[proxy.remote]
sources = [
  "10.0.0.0/8", # first
  "0.0.0.0/0",
]
inline = { a = 1, b = [ { c = "]}" } ] }
[[rule]]
name = """multi
line"""
action = 'block'
[[rule]]
"quoted key" = 1
`
	ix := indexLines(src)
	for path, want := range map[string]int{
		"name": 2, "proxy": 3, "proxy.default": 3, "proxy.remote": 4,
		"proxy.remote.sources": 5, "proxy.remote.sources[1]": 6, "proxy.remote.sources[2]": 7,
		"proxy.remote.inline.a": 9, "proxy.remote.inline.b[1].c": 9,
		"rule": 10, "rule[1]": 10, "rule[1].name": 11, "rule[1].action": 13,
		"rule[2]": 14, "rule[2].quoted key": 15,
	} {
		if got := ix[path]; got != want {
			t.Errorf("%s: line %d, want %d", path, got, want)
		}
	}
	if ix.line("rule[1].missing") != 10 || ix.line("absent") != 0 {
		t.Errorf("fallback lines")
	}
	// Input the TOML parser would refuse never makes the scanner loop.
	for _, bad := range []string{"=", "a = [}", "a = {]", "[", "a = \"unterminated", "]]]"} {
		indexLines(bad)
	}
}

// TestConfigExposure checks 0003 N1: validation rejects wildcard and
// host-name listeners, an IPv4-mapped wildcard included, a non-loopback
// listener without proxy.remote.sources, proxy.remote.default = "direct"
// without allow_direct, an upstream without disclosure, and IPv4-mapped
// sources, which could never match (0004 D4); 0.0.0.0/0 and
// ::/0 in sources are warnings that cuttle check prints (TestCheckCommand
// runs the warning samples).
func TestConfigExposure(t *testing.T) {
	const head = "name = \"cuttle-test\"\n[proxy]\ndefault = \"direct\"\n"
	remote := func(listen string) string {
		return head + "listen = [\"127.0.0.1:18080\", " + listen + "]\n[proxy.remote]\nsources = [\"10.8.0.1/32\"]\n"
	}
	for _, c := range []struct{ name, src, key, msg string }{
		{"IPv4 wildcard", remote(`"0.0.0.0:18081"`), "proxy.listen[2]", "wildcard address"},
		{"IPv6 wildcard", remote(`"[::]:18081"`), "proxy.listen[2]", "wildcard address"},
		{"IPv4-mapped wildcard", remote(`"[::ffff:0.0.0.0]:18081"`), "proxy.listen[2]", "wildcard address"},
		{"host name", remote(`"gateway.internal.example:18081"`), "proxy.listen[2]", "host names are not allowed"},
		{"localhost", remote(`"localhost:18081"`), "proxy.listen[2]", "host names are not allowed"},
		{"no sources", head + "listen = [\"127.0.0.1:18080\", \"10.8.0.6:18081\"]\n", "proxy.listen[2]", "needs proxy.remote.sources"},
		{"no IPv6 sources", head + "listen = [\"[2001:db8::6]:18081\"]\n", "proxy.listen[1]", "needs proxy.remote.sources"},
		{"empty sources", head + "listen = [\"10.8.0.6:18081\"]\n[proxy.remote]\nsources = []\n", "proxy.listen[1]", "needs proxy.remote.sources"},
		{"remote direct default", head + "[proxy.remote]\nsources = [\"10.8.0.1/32\"]\ndefault = \"direct\"\n", "proxy.remote.default", "needs proxy.remote.allow_direct = true"},
		{"remote direct default, allow_direct false", head + "[proxy.remote]\ndefault = \"direct\"\nallow_direct = false\n", "proxy.remote.default", "needs proxy.remote.allow_direct = true"},
		{"upstream without disclosure", head + "[[upstream]]\nname = \"gw\"\naddress = \"10.8.0.6:3128\"\n", "upstream[1].disclosure", "required"},
		{"IPv4-mapped source", head + "listen = [\"10.8.0.6:18081\"]\n[proxy.remote]\nsources = [\"::ffff:10.8.0.1/128\"]\n", "proxy.remote.sources[1]", `IPv4-mapped prefix, which never matches because cuttle compares IPv4 addresses in plain form; use "10.8.0.1/32"`},
		{"IPv4-mapped source range", head + "[proxy.remote]\nsources = [\"::ffff:0:0/96\"]\n", "proxy.remote.sources[1]", `use "0.0.0.0/0"`},
	} {
		cfg, r := Parse([]byte(c.src), "x.toml")
		if cfg != nil {
			t.Errorf("%s: accepted", c.name)
			continue
		}
		found := false
		for _, e := range r.Errors {
			found = found || (e.Key == c.key && strings.Contains(e.Msg, c.msg))
		}
		if !found {
			t.Errorf("%s: want %s: %s; got:\n%s", c.name, c.key, c.msg, issues(r.Errors))
		}
	}

	for _, open := range []string{"0.0.0.0/0", "::/0"} {
		src := head + "listen = [\"10.8.0.6:18081\"]\n[proxy.remote]\nsources = [\"10.8.0.1/32\", \"" + open + "\"]\n"
		cfg, r := Parse([]byte(src), "x.toml")
		if cfg == nil {
			t.Fatalf("%s: rejected:\n%s", open, issues(r.Errors))
		}
		if len(r.Warnings) != 1 || r.Warnings[0].Key != "proxy.remote.sources[2]" || !strings.Contains(r.Warnings[0].Msg, open+" admits every address") {
			t.Errorf("%s: warnings:\n%s", open, issues(r.Warnings))
		}
	}

	// Accepted: explicit non-loopback listeners with sources, and a remote
	// direct default with allow_direct.
	src := head + "listen = [\"127.0.0.1:18080\", \"10.8.0.6:18081\", \"[2001:db8::6]:18081\"]\n" +
		"[proxy.remote]\nsources = [\"10.8.0.1/32\", \"2001:db8::1/128\"]\ndefault = \"direct\"\nallow_direct = true\n"
	cfg, r := Parse([]byte(src), "x.toml")
	if cfg == nil {
		t.Fatalf("remote listeners rejected:\n%s", issues(r.Errors))
	}
	if len(cfg.Proxy.Listen) != 3 || len(cfg.Proxy.Remote.Sources) != 2 || len(r.Warnings) != 0 {
		t.Errorf("listen %v sources %v warnings %v", cfg.Proxy.Listen, cfg.Proxy.Remote.Sources, r.Warnings)
	}
}

// TestConfigSelfUpstream checks the T0 half of 0003 U9: an upstream whose
// address equals one of this instance's listeners fails validation,
// comparing IP literals and treating localhost as loopback. Other names
// cannot be checked without resolving them; the dial-time check covers
// them (TestUpstreamSelfDial).
func TestConfigSelfUpstream(t *testing.T) {
	const head = "name = \"cuttle-test\"\n[proxy]\nlisten = [\"127.0.0.1:18080\", \"[::1]:18081\", \"10.8.0.6:18082\"]\ndefault = \"block\"\n" +
		"[proxy.remote]\nsources = [\"10.8.0.1/32\"]\n[admin]\nlisten = \"\"\n[[upstream]]\nname = \"up\"\ndisclosure = \"via\"\n"
	for _, c := range []struct {
		address string
		self    bool
	}{
		{"127.0.0.1:18080", true},
		{"[::1]:18081", true},
		{"10.8.0.6:18082", true},
		{"localhost:18080", true},
		{"localhost:18081", true},
		{"LOCALHOST.:18080", true},
		{"localhost:18082", false},
		{"127.0.0.1:18081", false},
		{"127.0.0.2:18080", false},
		{"10.8.0.6:18080", false},
		{"gateway.example:18080", false},
	} {
		cfg, r := Parse([]byte(head+"address = \""+c.address+"\"\n"), "self.toml")
		found := false
		for _, e := range r.Errors {
			if e.Key == "upstream[1].address" && strings.Contains(e.Msg, "this instance's own listener") {
				found = true
			}
		}
		if found != c.self || (cfg == nil) != c.self {
			t.Errorf("%s: self %v, errors:\n%s", c.address, found, issues(r.Errors))
		}
	}
}

// TestRestartRequired checks design 0004 D6 step 5: only proxy.listen and
// admin.listen are restart-only, each is listed when its value differs
// from the running one, and other changes list nothing.
func TestRestartRequired(t *testing.T) {
	parse := func(listen, admin, rest string) *Config {
		t.Helper()
		text := "name = \"cuttle-test\"\n[proxy]\nlisten = [" + listen + "]\ndefault = \"direct\"\n[admin]\nlisten = \"" + admin + "\"\n" + rest
		cfg, r := Parse([]byte(text), "test.toml")
		if cfg == nil {
			t.Fatalf("config: %v", r.Err())
		}
		return cfg
	}
	running := parse(`"127.0.0.1:41000"`, "127.0.0.1:41001", "")
	for _, c := range []struct {
		name string
		next *Config
		want []string
	}{
		{"unchanged", parse(`"127.0.0.1:41000"`, "127.0.0.1:41001", ""), []string{}},
		{"rules only", parse(`"127.0.0.1:41000"`, "127.0.0.1:41001", "[[rule]]\nname = \"x\"\nhosts = [\"x.example\"]\naction = \"block\"\n"), []string{}},
		{"proxy.listen", parse(`"127.0.0.1:41002"`, "127.0.0.1:41001", ""), []string{"proxy.listen"}},
		{"proxy.listen added", parse(`"127.0.0.1:41000", "[::1]:41000"`, "127.0.0.1:41001", ""), []string{"proxy.listen"}},
		{"admin.listen", parse(`"127.0.0.1:41000"`, "127.0.0.1:41003", ""), []string{"admin.listen"}},
		{"admin.listen off", parse(`"127.0.0.1:41000"`, "", ""), []string{"admin.listen"}},
		{"both", parse(`"127.0.0.1:41002"`, "", ""), []string{"proxy.listen", "admin.listen"}},
	} {
		if got := RestartRequired(running, c.next); !slices.Equal(got, c.want) || got == nil {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

package config

import (
	"bytes"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"squid/internal/policy"
)

// expectation is the first line of a sample file:
// "# want: LINE KEY: MESSAGE" or "# warn: LINE KEY: MESSAGE". An empty KEY
// is not checked.
type expectation struct {
	line int
	key  string
	msg  string
}

var expectRe = regexp.MustCompile(`^# (want|warn): (\d+) ([^ :]*): (.+)$`)

func readExpectation(t *testing.T, path string) (string, expectation, []byte) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	first, _, _ := strings.Cut(string(data), "\n")
	m := expectRe.FindStringSubmatch(first)
	if m == nil {
		t.Fatalf("%s: first line %q does not document the expected issue", path, first)
	}
	n, _ := strconv.Atoi(m[2])
	return m[1], expectation{n, m[3], m[4]}, data
}

func findIssue(issues []Issue, e expectation) bool {
	for _, is := range issues {
		if is.Line == e.line && (e.key == "" || is.Key == e.key) && strings.Contains(is.Msg, e.msg) {
			return true
		}
	}
	return false
}

// TestConfigValidation checks that each design 0004 D4 error has a sample
// file that fails with the documented key path and line, and that each
// warning is reported without failing validation (K1).
func TestConfigValidation(t *testing.T) {
	invalid, _ := filepath.Glob("testdata/invalid/*.toml")
	if len(invalid) < 40 {
		t.Fatalf("only %d invalid samples", len(invalid))
	}
	for _, path := range invalid {
		kind, want, data := readExpectation(t, path)
		if kind != "want" {
			t.Fatalf("%s: invalid sample with %q", path, kind)
		}
		cfg, r := Parse(data, path)
		if cfg != nil || len(r.Errors) == 0 {
			t.Errorf("%s: accepted", path)
			continue
		}
		if !findIssue(r.Errors, want) {
			t.Errorf("%s: want line %d key %q message %q; got:\n%s", path, want.line, want.key, want.msg, issues(r.Errors))
		}
		for _, e := range r.Errors {
			if !strings.HasPrefix(e.String(), path) {
				t.Errorf("%s: message does not name the file: %s", path, e)
			}
		}
	}
	warn, _ := filepath.Glob("testdata/warn/*.toml")
	if len(warn) != 3 {
		t.Fatalf("want 3 warning samples, have %d", len(warn))
	}
	for _, path := range warn {
		kind, want, data := readExpectation(t, path)
		if kind != "warn" {
			t.Fatalf("%s: warning sample with %q", path, kind)
		}
		cfg, r := Parse(data, path)
		if cfg == nil || len(r.Errors) > 0 {
			t.Errorf("%s: rejected:\n%s", path, issues(r.Errors))
			continue
		}
		if !findIssue(r.Warnings, want) {
			t.Errorf("%s: want warning line %d key %q message %q; got:\n%s", path, want.line, want.key, want.msg, issues(r.Warnings))
		}
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

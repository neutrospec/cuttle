package paths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envMap(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

// TestPaths checks each XDG variable unset, empty, relative, and absolute
// against design 0006 D1, and the runtime directory fallback to $TMPDIR
// (P1). It uses only temporary directories and never the real home.
func TestPaths(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	tmpdir := filepath.Join(root, "tmp")
	abs := filepath.Join(root, "xdg")

	defaults := map[string]string{
		"XDG_CONFIG_HOME": filepath.Join(home, ".config"),
		"XDG_DATA_HOME":   filepath.Join(home, ".local", "share"),
		"XDG_STATE_HOME":  filepath.Join(home, ".local", "state"),
	}
	type variant struct {
		name   string
		set    bool
		value  string
		useAbs bool
		source string
	}
	variants := []variant{
		{"unset", false, "", false, " unset"},
		{"empty", true, "", false, " empty"},
		{"relative", true, "rel/dir", false, " not absolute"},
		{"absolute", true, abs, true, ""},
	}
	files := map[string]func(Paths) []Item{
		"XDG_CONFIG_HOME": func(p Paths) []Item { return []Item{p.Config} },
		"XDG_DATA_HOME":   func(p Paths) []Item { return []Item{p.Password} },
		"XDG_STATE_HOME":  func(p Paths) []Item { return []Item{p.Stats, p.Log} },
	}
	names := map[string][]string{
		"XDG_CONFIG_HOME": {"cuttle/config.toml"},
		"XDG_DATA_HOME":   {"cuttle/admin-password"},
		"XDG_STATE_HOME":  {"cuttle/stats.db", "cuttle/cuttle.log"},
	}
	for v, def := range defaults {
		for _, va := range variants {
			env := map[string]string{"HOME": home, "TMPDIR": tmpdir}
			if va.set {
				env[v] = va.value
			}
			p, err := Resolve(envMap(env))
			if err != nil {
				t.Fatalf("%s %s: %v", v, va.name, err)
			}
			base := def
			if va.useAbs {
				base = abs
			}
			for i, it := range files[v](p) {
				want := filepath.Join(base, filepath.FromSlash(names[v][i]))
				if it.Path != want {
					t.Errorf("%s %s: path %q, want %q", v, va.name, it.Path, want)
				}
				if it.Source != v+va.source {
					t.Errorf("%s %s: source %q, want %q", v, va.name, it.Source, v+va.source)
				}
			}
			// The other variables stay at their defaults.
			if v != "XDG_CONFIG_HOME" && p.Config.Path != filepath.Join(defaults["XDG_CONFIG_HOME"], "cuttle", "config.toml") {
				t.Errorf("%s %s changed the config path: %q", v, va.name, p.Config.Path)
			}
		}
	}

	// The runtime directory: used only when absolute, owned, and 0700.
	good := filepath.Join(root, "run-good")
	loose := filepath.Join(root, "run-loose")
	if err := os.Mkdir(good, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(loose, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	fallback := filepath.Join(tmpdir, "cuttle", "control.sock")
	for _, c := range []struct {
		name   string
		set    bool
		value  string
		want   string
		source string
	}{
		{"unset", false, "", fallback, "TMPDIR"},
		{"empty", true, "", fallback, "TMPDIR"},
		{"relative", true, "run", fallback, "TMPDIR; XDG_RUNTIME_DIR not absolute"},
		{"missing", true, filepath.Join(root, "absent"), fallback, "TMPDIR; XDG_RUNTIME_DIR missing"},
		{"not 0700", true, loose, fallback, "TMPDIR; XDG_RUNTIME_DIR not 0700"},
		{"good", true, good, filepath.Join(good, "cuttle", "control.sock"), "XDG_RUNTIME_DIR"},
	} {
		env := map[string]string{"HOME": home, "TMPDIR": tmpdir}
		if c.set {
			env["XDG_RUNTIME_DIR"] = c.value
		}
		p, err := Resolve(envMap(env))
		if err != nil {
			t.Fatal(err)
		}
		if p.Control.Path != c.want || p.Control.Source != c.source {
			t.Errorf("runtime %s: %+v, want %q from %q", c.name, p.Control, c.want, c.source)
		}
	}

	// HOME is required and absolute.
	if _, err := Resolve(envMap(map[string]string{})); err == nil {
		t.Error("missing HOME accepted")
	}
	if _, err := Resolve(envMap(map[string]string{"HOME": "relative"})); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("relative HOME: %v", err)
	}
	// Without TMPDIR the socket falls back to /tmp.
	p, err := Resolve(envMap(map[string]string{"HOME": home}))
	if err != nil || p.Control.Path != "/tmp/cuttle/control.sock" {
		t.Errorf("no TMPDIR: %+v %v", p.Control, err)
	}
}

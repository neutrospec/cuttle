// Package paths resolves cuttle's files from the XDG Base Directory
// variables (design 0006 D1). cuttle resolves each variable itself and never
// calls os.UserConfigDir or os.UserCacheDir, which return ~/Library paths on
// darwin.
package paths

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// Item is one resolved path and where it came from.
type Item struct {
	Path   string `json:"path"`
	Source string `json:"source"`
}

// MaxSocketPath is the longest usable control socket path in bytes: the
// sun_path of a darwin sockaddr_un has 104 bytes, the terminating NUL
// included (design 0006 D1).
const MaxSocketPath = 103

// Paths are cuttle's resolved files.
type Paths struct {
	Config   Item `json:"config"`
	Password Item `json:"password"`
	Stats    Item `json:"stats"`
	Log      Item `json:"log"`
	Control  Item `json:"control"`
}

// Resolve computes the paths from the environment. lookupEnv is usually
// os.LookupEnv. HOME must be set and absolute.
func Resolve(lookupEnv func(string) (string, bool)) (Paths, error) {
	getenv := func(k string) string { v, _ := lookupEnv(k); return v }
	home := getenv("HOME")
	if home == "" {
		return Paths{}, errors.New("HOME is not set")
	}
	if !filepath.IsAbs(home) {
		return Paths{}, fmt.Errorf("HOME is not an absolute path: %q", home)
	}
	cfg, cfgSrc := xdgDir(lookupEnv, "XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	data, dataSrc := xdgDir(lookupEnv, "XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	state, stateSrc := xdgDir(lookupEnv, "XDG_STATE_HOME", filepath.Join(home, ".local", "state"))

	var p Paths
	p.Config = Item{filepath.Join(cfg, "cuttle", "config.toml"), cfgSrc}
	p.Password = Item{filepath.Join(data, "cuttle", "admin-password"), dataSrc}
	p.Stats = Item{filepath.Join(state, "cuttle", "stats.db"), stateSrc}
	p.Log = Item{filepath.Join(state, "cuttle", "cuttle.log"), stateSrc}

	runtime, why := getenv("XDG_RUNTIME_DIR"), ""
	switch {
	case runtime == "":
		why = "XDG_RUNTIME_DIR unset"
	case !filepath.IsAbs(runtime):
		why = "XDG_RUNTIME_DIR not absolute"
	default:
		why = checkRuntimeDir(runtime)
	}
	if why == "" {
		sock := filepath.Join(runtime, "cuttle", "control.sock")
		if n := len(sock); n > MaxSocketPath {
			why = fmt.Sprintf("XDG_RUNTIME_DIR path too long (%d bytes, limit %d)", n, MaxSocketPath)
		} else {
			p.Control = Item{sock, "XDG_RUNTIME_DIR"}
		}
	}
	if why != "" {
		tmp, src := getenv("TMPDIR"), "TMPDIR"
		if tmp == "" || !filepath.IsAbs(tmp) {
			tmp, src = "/tmp", "/tmp (TMPDIR unset or not absolute)"
		}
		p.Control = Item{filepath.Join(tmp, "cuttle", "control.sock"), src + "; " + why}
		if why == "XDG_RUNTIME_DIR unset" {
			p.Control.Source = src
		}
		if n := len(p.Control.Path); n > MaxSocketPath {
			p.Control.Source += fmt.Sprintf("; too long (%d bytes, limit %d): no control socket", n, MaxSocketPath)
		}
	}
	return p, nil
}

// ControlError reports why the control socket path cannot be used, or nil.
// A path longer than MaxSocketPath bytes can be neither bound nor dialed.
func (p Paths) ControlError() error {
	if n := len(p.Control.Path); n > MaxSocketPath {
		return fmt.Errorf("control socket path %s is %d bytes, longer than the %d-byte limit of a Unix socket path; "+
			"set XDG_RUNTIME_DIR or TMPDIR to a shorter directory", p.Control.Path, n, MaxSocketPath)
	}
	return nil
}

// WithPresence returns p with "; missing" added to the password source when
// the password file does not exist, as cuttle paths and cuttle status show
// it (design 0006 D1).
func (p Paths) WithPresence() Paths {
	if _, err := os.Stat(p.Password.Path); errors.Is(err, fs.ErrNotExist) {
		p.Password.Source += "; missing"
	}
	return p
}

// Items returns the paths with their names, in the order cuttle paths
// prints them.
func (p Paths) Items() []NamedItem {
	return []NamedItem{{"config", p.Config}, {"password", p.Password}, {"stats", p.Stats}, {"log", p.Log}, {"control", p.Control}}
}

// NamedItem is one path with its name.
type NamedItem struct {
	Name string
	Item
}

// xdgDir uses the variable when it is non-empty and absolute, and the
// specification's default otherwise.
func xdgDir(lookupEnv func(string) (string, bool), name, def string) (string, string) {
	v, set := lookupEnv(name)
	switch {
	case !set:
		return def, name + " unset"
	case v == "":
		return def, name + " empty"
	case !filepath.IsAbs(v):
		return def, name + " not absolute"
	}
	return v, name
}

// checkRuntimeDir returns "" when dir is owned by the user with mode 0700,
// and otherwise the reason it is not used.
func checkRuntimeDir(dir string) string {
	fi, err := os.Stat(dir)
	if err != nil {
		return "XDG_RUNTIME_DIR missing"
	}
	if !fi.IsDir() {
		return "XDG_RUNTIME_DIR not a directory"
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return "XDG_RUNTIME_DIR not owned by the user"
	}
	if fi.Mode().Perm() != 0o700 {
		return "XDG_RUNTIME_DIR not 0700"
	}
	return ""
}

// MkdirAll creates missing directories with mode 0700. Existing directories
// keep their modes, as the XDG specification requires.
func MkdirAll(dir string) error {
	return os.MkdirAll(dir, 0o700)
}

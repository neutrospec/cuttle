// Package paths resolves cuttle's files from the XDG Base Directory
// variables (design 0006 D1). cuttle resolves each variable itself and never
// calls os.UserConfigDir or os.UserCacheDir, which return ~/Library paths on
// darwin.
package paths

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Item is one resolved path and where it came from.
type Item struct {
	Path   string `json:"path"`
	Source string `json:"source"`
}

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
		p.Control = Item{filepath.Join(runtime, "cuttle", "control.sock"), "XDG_RUNTIME_DIR"}
	} else {
		tmp, src := getenv("TMPDIR"), "TMPDIR"
		if tmp == "" || !filepath.IsAbs(tmp) {
			tmp, src = "/tmp", "/tmp (TMPDIR unset or not absolute)"
		}
		p.Control = Item{filepath.Join(tmp, "cuttle", "control.sock"), src + "; " + why}
		if why == "XDG_RUNTIME_DIR unset" {
			p.Control.Source = src
		}
	}
	return p, nil
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

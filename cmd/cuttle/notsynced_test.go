package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWritesNotSynced checks OI-0006 Z-R4 on the command line: when the
// directory sync after the rename fails, cuttle admin passwd and cuttle
// init succeed with a warning, because the file is in place. A directory
// with mode 0300 allows the rename but not the open of the directory.
func TestWritesNotSynced(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens any directory")
	}
	lookup, vars := tempEnv(t)
	pwDir := filepath.Join(vars["XDG_DATA_HOME"], "cuttle")
	cfgDir := filepath.Join(vars["XDG_CONFIG_HOME"], "cuttle")
	for _, d := range []string{pwDir, cfgDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d, 0o300); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(d, 0o700)
	}
	var out, errb bytes.Buffer
	code := cmdAdmin([]string{"passwd", "--stdin"}, lookup, strings.NewReader("a long enough password\n"), &out, &errb)
	if code != exitOK || !strings.Contains(errb.String(), "warning: the password file was written, but") {
		t.Errorf("admin passwd: exit %d %q", code, errb.String())
	}
	code, _, errOut := runCLI(lookup, "init")
	if code != exitOK || !strings.Contains(errOut, "warning: wrote the starter configuration, but") {
		t.Errorf("init: exit %d %q", code, errOut)
	}
	for _, d := range []string{pwDir, cfgDir} {
		os.Chmod(d, 0o700)
	}
	checkPasswordFile(t, filepath.Join(pwDir, "admin-password"))
	checkStarterFile(t, filepath.Join(cfgDir, "config.toml"))
}

// TestServeStarterNotSynced checks OI-0006 AB-R3: when the directory sync
// after writing the starter file fails, the first-run steps of cuttle
// serve treat the file as written, log the warning, and go on to load it,
// as init does (OI-0006 Z-R4).
func TestServeStarterNotSynced(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens any directory")
	}
	lookup, vars := tempEnv(t)
	cfgDir := filepath.Join(vars["XDG_CONFIG_HOME"], "cuttle")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfgDir, 0o300); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(cfgDir, 0o700)
	s, err := prepareServe(lookup, "", io.Discard)
	if err != nil {
		t.Fatalf("prepareServe with an unsynced starter file: %v", err)
	}
	s.close()
	os.Chmod(cfgDir, 0o700)
	if s.cfg == nil {
		t.Fatal("no configuration loaded")
	}
	checkStarterFile(t, filepath.Join(cfgDir, "config.toml"))
	log, _ := os.ReadFile(filepath.Join(vars["XDG_STATE_HOME"], "cuttle", "cuttle.log"))
	if !strings.Contains(string(log), `msg="wrote starter configuration, but the directory could not be synced"`) {
		t.Errorf("log %q", log)
	}
}

package logx

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriterRotatesBeforeLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state", "cuttle", "cuttle.log")
	w, err := Open(path, 1000, 5)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	line := []byte(strings.Repeat("x", 99) + "\n")
	for range 100 {
		if _, err := w.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"cuttle.log", "cuttle.log.1", "cuttle.log.5"} {
		fi, err := os.Stat(filepath.Join(filepath.Dir(path), name))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() > 1000 || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: size %d mode %v", name, fi.Size(), fi.Mode())
		}
	}
	if _, err := os.Stat(path + ".6"); err == nil {
		t.Errorf(".6 exists")
	}
	fi, _ := os.Stat(filepath.Dir(path))
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", fi.Mode())
	}
}

func TestLoggerFormat(t *testing.T) {
	var buf bytes.Buffer
	NewLogger(&buf, slog.LevelInfo).Info("decision", "id", "4f2a")
	s := buf.String()
	if !strings.HasPrefix(s, "time=") || !strings.Contains(s, "Z level=INFO msg=decision id=4f2a") {
		t.Errorf("line %q", s)
	}
}

func countLines(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		n += strings.Count(string(data), "\n")
	}
	return n
}

// TestWriterSurvivesRotationFailure injects one failed rename: logging
// continues in the current file, the failure is reported once to Errors and
// counted, and rotation succeeds on a later attempt without losing a line.
func TestWriterSurvivesRotationFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cuttle.log")
	w, err := Open(path, 1000, 5)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	var errs bytes.Buffer
	w.Errors = &errs
	failed := false
	w.rename = func(oldpath, newpath string) error {
		if newpath == path+".1" && !failed {
			failed = true
			return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: os.ErrPermission}
		}
		return os.Rename(oldpath, newpath)
	}
	line := []byte(strings.Repeat("x", 99) + "\n")
	for i := range 30 {
		if _, err := w.Write(line); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if !failed || w.Failures() != 1 || strings.Count(errs.String(), "\n") != 1 || !strings.Contains(errs.String(), "logging continues") {
		t.Fatalf("failed=%v failures=%d errors=%q", failed, w.Failures(), errs.String())
	}
	if got := countLines(t, dir); got != 30 {
		t.Errorf("lines kept %d, want 30", got)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("rotation never succeeded: %v", err)
	}
}

// TestWriterReopensAfterOpenFailure injects one failed reopen during
// rotation: only that write is lost, and the next write opens the file.
func TestWriterReopensAfterOpenFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cuttle.log")
	w, err := Open(path, 1000, 5)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	var errs bytes.Buffer
	w.Errors = &errs
	failed := false
	w.openFile = func(name string, flag int, perm os.FileMode) (*os.File, error) {
		if !failed {
			failed = true
			return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrPermission}
		}
		return os.OpenFile(name, flag, perm)
	}
	line := []byte(strings.Repeat("y", 99) + "\n")
	lost := 0
	for range 30 {
		if _, err := w.Write(line); err != nil {
			lost++
		}
	}
	if lost != 1 || w.Failures() != 1 || strings.Count(errs.String(), "\n") != 1 {
		t.Fatalf("lost=%d failures=%d errors=%q", lost, w.Failures(), errs.String())
	}
	if got := countLines(t, dir); got != 29 {
		t.Errorf("lines kept %d, want 29", got)
	}
}

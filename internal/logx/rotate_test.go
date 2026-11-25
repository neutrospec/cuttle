package logx

import (
	"bytes"
	"fmt"
	"io/fs"
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

// TestWriterRecreatesDeletedLog checks that a log file deleted by someone
// else is recreated at the next rotation instead of growing unseen: the
// missing current file counts as already rotated away.
func TestWriterRecreatesDeletedLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cuttle.log")
	w, err := Open(path, 1000, 5)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	var errs bytes.Buffer
	w.Errors = &errs
	line := func(i int) []byte { return []byte(fmt.Sprintf("line-%03d %s\n", i, strings.Repeat("z", 89))) }
	for i := range 5 {
		w.Write(line(i))
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	// Past MaxBytes: the writer rotates, finds no current file, and opens a
	// new one.
	for i := 5; i < 25; i++ {
		if _, err := w.Write(line(i)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("log not recreated: %v", err)
	}
	if !strings.Contains(string(data), "line-024 ") {
		t.Errorf("recreated log lacks the newest line:\n%s", data)
	}
	if w.Failures() != 0 || errs.Len() != 0 {
		t.Errorf("failures %d, errors %q", w.Failures(), errs.String())
	}
}

// TestWriterRenameNotExist covers the race in which the current file
// vanishes between the existence check and the rename: ErrNotExist from that
// rename counts as rotated away, not as a failure.
func TestWriterRenameNotExist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cuttle.log")
	w, err := Open(path, 1000, 5)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	var errs bytes.Buffer
	w.Errors = &errs
	raced := false
	w.rename = func(oldpath, newpath string) error {
		if oldpath == path && !raced {
			raced = true
			if err := os.Remove(path); err != nil {
				return err
			}
			return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: fs.ErrNotExist}
		}
		return os.Rename(oldpath, newpath)
	}
	for i := range 15 {
		if _, err := w.Write([]byte(fmt.Sprintf("line-%03d %s\n", i, strings.Repeat("r", 89)))); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	data, err := os.ReadFile(path)
	if !raced || err != nil || !strings.Contains(string(data), "line-014 ") {
		t.Fatalf("raced=%v err=%v data=%q", raced, err, data)
	}
	if w.Failures() != 0 || errs.Len() != 0 {
		t.Errorf("failures %d, errors %q", w.Failures(), errs.String())
	}
}

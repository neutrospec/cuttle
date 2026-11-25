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

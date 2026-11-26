package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestAtomicWriteFinalRename checks the reverse of OI-0006 U-R1 at the
// last step (OI-0006 Z-R2): when the final rename fails, here because the
// target carries the user immutable flag, the error does not wrap
// ErrNotSynced and the file keeps its content. The .bak written before
// holds the same content, so nothing is lost.
func TestAtomicWriteFinalRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	const ufImmutable = 0x2 // UF_IMMUTABLE of <sys/stat.h>
	if err := syscall.Chflags(path, ufImmutable); err != nil {
		t.Skipf("chflags: %v", err)
	}
	t.Cleanup(func() { syscall.Chflags(path, 0) })
	err := WriteAtomic(path, []byte("lost"), WriteOptions{ExpectSHA256: Digest([]byte("keep")), Backup: true})
	if err == nil || errors.Is(err, ErrNotSynced) {
		t.Fatalf("WriteAtomic onto an immutable target: %v, want an error other than ErrNotSynced", err)
	}
	if !strings.Contains(err.Error(), "rename") {
		t.Errorf("WriteAtomic: %v, want the final rename to fail", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "keep" {
		t.Errorf("target changed to %q", got)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".config.toml.tmp-*")); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
}

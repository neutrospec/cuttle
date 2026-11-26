package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// ErrConflict is returned by WriteAtomic when the file changed since the
// caller read it.
var ErrConflict = errors.New("file changed since it was read")

// ErrNotSynced is wrapped by the error of WriteAtomic when the new content
// is in place but the directory could not be opened or synced after the
// rename (OI-0006 U-R1). The file holds the new content, so the caller
// treats the write as saved, logs a warning, and reloads.
var ErrNotSynced = errors.New("saved, but the directory could not be synced")

// WriteOptions control WriteAtomic.
type WriteOptions struct {
	// ExpectSHA256, when set, is the hex SHA-256 of the content the caller
	// started from. A different current content stops the write.
	ExpectSHA256 string
	// Backup writes the previous content to <base>.bak first.
	Backup bool
}

// WriteAtomic replaces the file at path with data so that the target always
// exists with the old or the new content (design 0004 D7). A symbolic link
// at path stays in place: the link's target is replaced, and the temporary
// file and .bak sit beside that target. An error that wraps ErrNotSynced
// means that the file holds data; any other error means that it is
// unchanged.
func WriteAtomic(path string, data []byte, opt WriteOptions) error {
	target, err := resolveLinks(path)
	if err != nil {
		return err
	}
	dir, base := filepath.Split(target)
	if dir == "" {
		dir = "."
	}
	old, err := os.ReadFile(target)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if opt.ExpectSHA256 != "" && (!exists || Digest(old) != opt.ExpectSHA256) {
		return ErrConflict
	}
	if opt.Backup && exists {
		if err := writeReplace(dir, base, base+".bak", old); err != nil {
			return fmt.Errorf("write backup: %w", err)
		}
		// A failure here is left to the sync after the next rename, which
		// covers this rename too.
		syncDir(dir)
	}
	if err := writeReplace(dir, base, base, data); err != nil {
		return err
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("%w: %w", ErrNotSynced, err)
	}
	return nil
}

// BackupPath returns the path of the previous version that WriteAtomic
// keeps for path: <target>.bak beside the target of the symbolic links at
// path (design 0004 D7, 0009 D12).
func BackupPath(path string) (string, error) {
	target, err := resolveLinks(path)
	if err != nil {
		return "", err
	}
	return target + ".bak", nil
}

// syncDir makes the renames in dir durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// writeReplace writes data to a new temporary file in dir and renames it to
// name. The caller syncs dir.
func writeReplace(dir, base, name string, data []byte) (err error) {
	var rnd [6]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+base+".tmp-"+hex.EncodeToString(rnd[:]))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, name))
}

// resolveLinks follows symbolic links at path itself, including a link whose
// target does not exist yet.
func resolveLinks(path string) (string, error) {
	p := path
	for range 40 {
		fi, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			return p, nil
		}
		if err != nil {
			return "", err
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			return p, nil
		}
		link, err := os.Readlink(p)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(link) {
			link = filepath.Join(filepath.Dir(p), link)
		}
		p = link
	}
	return "", fmt.Errorf("%s: too many levels of symbolic links", path)
}

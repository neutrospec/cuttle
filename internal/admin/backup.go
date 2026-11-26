package admin

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/neutrospec/cuttle/internal/config"
)

// backupFile is the previous version of the configuration file,
// <config>.bak, as the Advanced page offers it (design 0009 D3.10, D12).
type backupFile struct {
	Path, Name string
	Data       []byte
	Size       int64
	ModTime    time.Time
	// Reason says why the previous version is not offered; it is empty
	// when it is.
	Reason string
	// Missing marks a previous version that does not exist yet.
	Missing bool
	// Valid reports whether the validator accepts the previous version,
	// which restore needs; Errors are its errors.
	Valid  bool
	Errors []string
}

// Offered reports whether the page offers the previous version.
func (b *backupFile) Offered() bool { return b.Reason == "" }

// readBackup reads <config>.bak beside the resolved target of the
// configuration path, the file that WriteAtomic writes (design 0004 D7).
// It opens it with O_NOFOLLOW, so a symbolic link is refused, and with
// O_NONBLOCK, so a FIFO cannot hold the request; checks the open
// descriptor with Fstat: a regular file, owned by this user, not writable
// by group or others, at most 256 KiB; and reads from that descriptor, so
// nothing can be swapped in between the check and the read (design 0009
// D12).
func (s *Server) readBackup() *backupFile {
	b := &backupFile{}
	p, err := config.BackupPath(s.backend.ConfigPath())
	if err != nil {
		b.Reason = "Its path cannot be resolved: " + err.Error()
		return b
	}
	b.Path, b.Name = p, filepath.Base(p)
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		b.Missing = true
		b.Reason = "No previous version yet. Each save keeps the file it replaces as " + b.Name + "."
		return b
	case errors.Is(err, syscall.ELOOP):
		b.Reason = b.Name + " is a symbolic link, so it is not offered."
		return b
	case err != nil:
		b.Reason = b.Name + " cannot be opened: " + err.Error()
		return b
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		b.Reason = b.Name + " cannot be read: " + err.Error()
		return b
	}
	uid := -1
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		uid = int(st.Uid)
	}
	switch {
	case !fi.Mode().IsRegular():
		b.Reason = b.Name + " is not a regular file, so it is not offered."
	case fi.Mode().Perm()&0o022 != 0:
		b.Reason = fmt.Sprintf("%s is writable by group or others (mode %04o), so it is not offered; run chmod 600 on it.", b.Name, fi.Mode().Perm())
	case uid != s.uid():
		b.Reason = b.Name + " is owned by another user, so it is not offered."
	case fi.Size() > config.MaxFileSize:
		b.Reason = fmt.Sprintf("%s is larger than %d KiB, so it is not offered.", b.Name, config.MaxFileSize>>10)
	}
	if b.Reason != "" {
		return b
	}
	data, err := io.ReadAll(io.LimitReader(f, config.MaxFileSize+1))
	switch {
	case err != nil:
		b.Reason = b.Name + " cannot be read: " + err.Error()
		return b
	case len(data) > config.MaxFileSize:
		b.Reason = fmt.Sprintf("%s is larger than %d KiB, so it is not offered.", b.Name, config.MaxFileSize>>10)
		return b
	}
	b.Data, b.Size, b.ModTime = data, int64(len(data)), fi.ModTime()
	_, r := config.Parse([]byte(lf(string(data))), b.Name)
	b.Valid, b.Errors = len(r.Errors) == 0, issueLines(r.Errors)
	return b
}

// backupPage is the data of /settings/backup.
type backupPage struct {
	Backup *backupFile
	// Base is the SHA-256 of the current file, the base of a restore;
	// ReadError says why the current file cannot be read.
	Base, ReadError string
	Diff            *diffView
	Return          string
}

// getBackup shows the previous version: when it was saved, its size, and
// the diff from the current file to it, with Restore (design 0009 D3.10).
func (s *Server) getBackup(w http.ResponseWriter, r *http.Request, _ string, v session) {
	d := &backupPage{Backup: s.readBackup(), Return: "/settings"}
	cur := s.readFile()
	if cur.ReadErr != nil {
		d.ReadError = cur.ReadErr.Error()
	} else {
		d.Base = cur.Digest
	}
	if d.Backup.Offered() {
		d.Diff = newDiffView(filepath.Base(s.backend.ConfigPath())+" → "+d.Backup.Name, string(cur.Data), lf(string(d.Backup.Data)))
	}
	s.render(w, http.StatusOK, "backup", pageData{Title: "Previous version", Token: v.csrf, Page: "advanced", Data: d})
}

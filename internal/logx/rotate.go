// Package logx holds cuttle's rotating log writer (design 0006 D7). launchd
// does not rotate logs and newsyslog needs sudo, so cuttle rotates its own
// log without a dependency.
package logx

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
)

// Defaults from design 0006 D7: 10 MiB per file and five rotated files.
const (
	DefaultMaxBytes = 10 << 20
	DefaultKeep     = 5
)

// Writer appends to a log file and rotates it before a write would take it
// past MaxBytes: .4 becomes .5 and so on down to the current file becoming
// .1. A single write larger than MaxBytes goes into a file of its own.
//
// A failed rotation never stops logging: when a rename fails the writer
// keeps appending to the current file, which then grows past MaxBytes, and
// tries again after another MaxBytes; when the file cannot be reopened, that
// write is lost and the next write tries again. Each failure increments
// Failures, and the first failure of a run of failures is reported once to
// Errors. A current file deleted by someone else is not a failure: the next
// rotation opens a new one.
type Writer struct {
	// Errors receives one line for the first failure after a success. It is
	// os.Stderr from Open.
	Errors io.Writer

	mu       sync.Mutex
	path     string
	max      int64
	keep     int
	f        *os.File
	size     int64
	retryAt  int64 // size at which a failed rotation is tried again
	failing  bool
	failures atomic.Int64

	// Seams for tests.
	rename   func(oldpath, newpath string) error
	openFile func(name string, flag int, perm os.FileMode) (*os.File, error)
}

// Open opens path for appending, creating missing directories with mode
// 0700 and the file with mode 0600.
func Open(path string, maxBytes int64, keep int) (*Writer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	w := &Writer{Errors: os.Stderr, path: path, max: maxBytes, keep: keep, rename: os.Rename, openFile: os.OpenFile}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *Writer) open() error {
	f, err := w.openFile(w.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.f, w.size, w.retryAt = f, fi.Size(), 0
	return nil
}

// Failures returns how many rotation or reopen failures occurred.
func (w *Writer) Failures() int64 { return w.failures.Load() }

func (w *Writer) fail(err error) {
	w.failures.Add(1)
	if !w.failing && w.Errors != nil {
		fmt.Fprintf(w.Errors, "cuttle: log %s: %v; logging continues\n", w.path, err)
	}
	w.failing = true
}

// Write writes p as one unit; slog handlers write one record per call, so a
// line is never split across files.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.path == "" {
		return 0, fs.ErrClosed
	}
	if w.f == nil {
		if err := w.open(); err != nil {
			w.fail(err)
			return 0, err
		}
	}
	if w.size > 0 && w.size+int64(len(p)) > w.max && w.size >= w.retryAt {
		if err := w.rotate(); err != nil {
			w.fail(err)
			if w.f == nil {
				return 0, err
			}
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	if err != nil {
		w.fail(err)
	} else if w.failing && w.retryAt == 0 {
		w.failing = false
	}
	return n, err
}

// rotate shifts the rotated files and starts a new current file. When the
// current file cannot be renamed it stays open, and the next attempt waits
// for another MaxBytes. A current file that someone else deleted counts as
// rotated away: the rotated files stay as they are, and a new current file
// is opened, so writes never keep growing a deleted file.
func (w *Writer) rotate() error {
	if _, err := os.Lstat(w.path); errors.Is(err, fs.ErrNotExist) {
		return w.reopen(nil)
	}
	var shiftErr error
	for i := w.keep - 1; i >= 1; i-- {
		err := w.rename(w.path+"."+strconv.Itoa(i), w.path+"."+strconv.Itoa(i+1))
		if err != nil && !errors.Is(err, fs.ErrNotExist) && shiftErr == nil {
			shiftErr = err
		}
	}
	// ErrNotExist here means the file was deleted after the check above.
	if err := w.rename(w.path, w.path+".1"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		w.retryAt = w.size + w.max
		if shiftErr != nil {
			return shiftErr
		}
		return err
	}
	return w.reopen(shiftErr)
}

// reopen closes the current file and opens a new one at the path. It returns
// the open error, or else prev.
func (w *Writer) reopen(prev error) error {
	w.f.Close()
	w.f = nil
	if err := w.open(); err != nil {
		return err
	}
	return prev
}

// Close closes the current file.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.path = ""
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

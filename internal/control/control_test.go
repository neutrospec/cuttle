package control

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// shortDir returns a fresh 0700 directory under /tmp, so socket paths stay
// far below the Unix limit (design 0007 D4).
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "cuttle-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(d, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// TestListenRefuses checks the cases in which Listen must not create the
// socket (design 0006 D2, D4): a regular file at the socket path, which it
// must leave in place, a symbolically linked socket directory, and a
// directory with mode 0755. The cases that touch a socket skip under
// -short (design 0007 D1).
func TestListenRefuses(t *testing.T) {
	t.Run("regular file at the path", func(t *testing.T) {
		if testing.Short() {
			t.Skip("dials the path")
		}
		dir := filepath.Join(shortDir(t), "cuttle")
		os.Mkdir(dir, 0o700)
		path := filepath.Join(dir, "control.sock")
		if err := os.WriteFile(path, []byte("keep me"), 0o600); err != nil {
			t.Fatal(err)
		}
		l, err := Listen(path)
		if err == nil {
			l.Close()
			t.Fatal("Listen replaced a regular file")
		}
		if data, rerr := os.ReadFile(path); rerr != nil || string(data) != "keep me" {
			t.Errorf("regular file changed: %q %v", data, rerr)
		}
		if errors.Is(err, ErrRunning) {
			t.Errorf("error %v", err)
		}
	})

	t.Run("symbolically linked directory", func(t *testing.T) {
		root := shortDir(t)
		target := filepath.Join(root, "real")
		os.Mkdir(target, 0o700)
		dir := filepath.Join(root, "cuttle")
		if err := os.Symlink(target, dir); err != nil {
			t.Fatal(err)
		}
		l, err := Listen(filepath.Join(dir, "control.sock"))
		if err == nil {
			l.Close()
			t.Fatal("Listen accepted a symbolic link")
		}
		if !errors.Is(err, ErrInsecureDir) {
			t.Errorf("error %v", err)
		}
		if entries, _ := os.ReadDir(target); len(entries) != 0 {
			t.Errorf("socket created through the link: %v", entries)
		}
	})

	t.Run("directory mode 0755", func(t *testing.T) {
		dir := filepath.Join(shortDir(t), "cuttle")
		os.Mkdir(dir, 0o755)
		os.Chmod(dir, 0o755)
		l, err := Listen(filepath.Join(dir, "control.sock"))
		if err == nil {
			l.Close()
			t.Fatal("Listen accepted a 0755 directory")
		}
		if !errors.Is(err, ErrInsecureDir) {
			t.Errorf("error %v", err)
		}
	})

	t.Run("missing directory is created 0700", func(t *testing.T) {
		if testing.Short() {
			t.Skip("opens a socket")
		}
		dir := filepath.Join(shortDir(t), "cuttle")
		l, err := Listen(filepath.Join(dir, "control.sock"))
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
			t.Errorf("directory mode %v", fi.Mode())
		}
	})
}

// fakeService answers status with a fixed name and counts the calls.
type fakeService struct {
	name    string
	calls   atomic.Int64
	reloads atomic.Int64
}

func (f *fakeService) Status() Status {
	f.calls.Add(1)
	return Status{Name: f.name}
}

func (f *fakeService) Reload() (ReloadResult, bool) {
	f.reloads.Add(1)
	return ReloadResult{RestartRequired: []string{}, Warnings: []string{}}, true
}

// serveAt serves a fake service on a socket at path, created directly so
// that it can sit in a directory Listen would refuse.
func serveAt(t *testing.T, path, name string) *fakeService {
	t.Helper()
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeService{name: name}
	srv := Serve(l, f, slog.New(slog.DiscardHandler))
	t.Cleanup(func() { srv.Close() })
	return f
}

// TestListenSockets checks the socket cases of Listen (design 0006 D4): a
// stale socket is replaced and the new one has mode 0600, and a live socket
// gives ErrRunning and keeps serving.
func TestListenSockets(t *testing.T) {
	if testing.Short() {
		t.Skip("opens sockets")
	}
	t.Run("stale socket", func(t *testing.T) {
		dir := filepath.Join(shortDir(t), "cuttle")
		os.Mkdir(dir, 0o700)
		path := filepath.Join(dir, "control.sock")
		stale, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		stale.(*net.UnixListener).SetUnlinkOnClose(false)
		stale.Close()
		l, err := Listen(path)
		if err != nil {
			t.Fatalf("stale socket not replaced: %v", err)
		}
		defer l.Close()
		fi, err := os.Lstat(path)
		if err != nil || fi.Mode().Type() != os.ModeSocket || fi.Mode().Perm() != 0o600 {
			t.Errorf("socket %v %v", fi.Mode(), err)
		}
		Serve(l, &fakeService{name: "fresh"}, slog.New(slog.DiscardHandler))
		st, err := NewClient(path).Status(context.Background())
		if err != nil || st.Name != "fresh" {
			t.Errorf("status over the new socket: %+v %v", st, err)
		}
	})

	t.Run("live socket", func(t *testing.T) {
		dir := filepath.Join(shortDir(t), "cuttle")
		os.Mkdir(dir, 0o700)
		path := filepath.Join(dir, "control.sock")
		serveAt(t, path, "first")
		l, err := Listen(path)
		if err == nil {
			l.Close()
			t.Fatal("Listen took over a live socket")
		}
		if !errors.Is(err, ErrRunning) {
			t.Errorf("error %v", err)
		}
		st, err := NewClient(path).Status(context.Background())
		if err != nil || st.Name != "first" {
			t.Errorf("first instance after the refused Listen: %+v %v", st, err)
		}
	})
}

// TestClientChecksDirectory checks that the client refuses a socket in a
// directory that is not the user's 0700 directory before it dials (design
// 0006 D2), so such a socket can neither answer status nor receive a
// reload, and that it reaches a socket in a private directory.
func TestClientChecksDirectory(t *testing.T) {
	if testing.Short() {
		t.Skip("opens sockets")
	}
	t.Run("0755 directory", func(t *testing.T) {
		dir := filepath.Join(shortDir(t), "cuttle")
		os.Mkdir(dir, 0o755)
		os.Chmod(dir, 0o755)
		path := filepath.Join(dir, "control.sock")
		f := serveAt(t, path, "spoof")
		c := NewClient(path)
		if st, err := c.Status(context.Background()); err == nil || !errors.Is(err, ErrInsecureDir) {
			t.Errorf("status from a 0755 directory: %+v %v", st, err)
		}
		if _, _, err := c.Reload(context.Background()); err == nil || !errors.Is(err, ErrInsecureDir) {
			t.Errorf("reload to a 0755 directory: %v", err)
		}
		if n, r := f.calls.Load(), f.reloads.Load(); n != 0 || r != 0 {
			t.Errorf("the socket received %d status and %d reload requests", n, r)
		}
	})

	t.Run("symbolically linked directory", func(t *testing.T) {
		root := shortDir(t)
		target := filepath.Join(root, "real")
		os.Mkdir(target, 0o700)
		serveAt(t, filepath.Join(target, "control.sock"), "spoof")
		dir := filepath.Join(root, "cuttle")
		os.Symlink(target, dir)
		if _, err := NewClient(filepath.Join(dir, "control.sock")).Status(context.Background()); !errors.Is(err, ErrInsecureDir) {
			t.Errorf("status through a link: %v", err)
		}
	})

	t.Run("missing directory", func(t *testing.T) {
		path := filepath.Join(shortDir(t), "cuttle", "control.sock")
		_, err := NewClient(path).Status(context.Background())
		if err == nil || errors.Is(err, ErrInsecureDir) {
			t.Errorf("missing directory: %v", err)
		}
	})

	t.Run("private directory", func(t *testing.T) {
		dir := filepath.Join(shortDir(t), "cuttle")
		os.Mkdir(dir, 0o700)
		path := filepath.Join(dir, "control.sock")
		f := serveAt(t, path, "real")
		c := NewClient(path)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if st, err := c.Status(ctx); err != nil || st.Name != "real" {
			t.Errorf("status: %+v %v", st, err)
		}
		if _, ok, err := c.Reload(ctx); err != nil || !ok || f.reloads.Load() != 1 {
			t.Errorf("reload: %v %v", ok, err)
		}
	})
}

// TestServeRoutes checks that the control server answers only its two
// routes.
func TestServeRoutes(t *testing.T) {
	if testing.Short() {
		t.Skip("opens sockets")
	}
	dir := filepath.Join(shortDir(t), "cuttle")
	os.Mkdir(dir, 0o700)
	path := filepath.Join(dir, "control.sock")
	serveAt(t, path, "x")
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}}
	for _, c := range []struct {
		method, path string
		code         int
	}{{"GET", "/v1/status", 200}, {"POST", "/v1/reload", 200}, {"POST", "/v1/status", 405}, {"GET", "/v1/reload", 405}, {"GET", "/", 404}} {
		req, _ := http.NewRequest(c.method, "http://cuttle"+c.path, nil)
		res, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != c.code {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, res.StatusCode, c.code)
		}
	}
}

package testenv

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

var (
	buildOnce sync.Once
	buildDir  string
	buildPath string
	buildErr  error
)

// BuildCuttle builds ./cmd/cuttle once per test process and returns the
// binary's path. Call CleanupBuild from TestMain.
func BuildCuttle(t testing.TB) string {
	t.Helper()
	buildOnce.Do(func() {
		goBin, err := exec.LookPath("go")
		if err != nil {
			goBin = filepath.Join(runtime.GOROOT(), "bin", "go")
		}
		buildDir, buildErr = os.MkdirTemp("", "cuttle-bin-")
		if buildErr != nil {
			return
		}
		buildPath = filepath.Join(buildDir, "cuttle")
		cmd := exec.Command(goBin, "build", "-o", buildPath, "-ldflags", "-X main.version=0.0.0-test", "github.com/neutrospec/cuttle/cmd/cuttle")
		out, err := cmd.CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return buildPath
}

// CleanupBuild removes the binary built by BuildCuttle.
func CleanupBuild() {
	if buildDir != "" {
		os.RemoveAll(buildDir)
	}
}

// Sandbox runs a cuttle binary with test-owned directories (design 0007 D4
// and E6): HOME and the XDG directories under a test temporary directory,
// XDG_RUNTIME_DIR in a fresh 0700 directory under /tmp, and listen ports the
// test chose.
type Sandbox struct {
	t   testing.TB
	Bin string

	Root       string // test temporary directory holding everything else
	Home       string
	ConfigHome string
	DataHome   string
	StateHome  string
	TmpDir     string
	RuntimeDir string

	mu     sync.Mutex
	chosen map[uint16]bool
}

// NewSandbox prepares the directories. The XDG directories under HOME are
// left for cuttle to create.
func NewSandbox(t testing.TB, bin string) *Sandbox {
	t.Helper()
	root := t.TempDir()
	s := &Sandbox{
		t:          t,
		Bin:        bin,
		Root:       root,
		Home:       filepath.Join(root, "home"),
		TmpDir:     filepath.Join(root, "tmp"),
		chosen:     map[uint16]bool{},
		ConfigHome: filepath.Join(root, "home", ".config"),
		DataHome:   filepath.Join(root, "home", ".local", "share"),
		StateHome:  filepath.Join(root, "home", ".local", "state"),
	}
	for _, d := range []string{s.Home, s.TmpDir} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	rt, err := os.MkdirTemp("/tmp", "cuttle-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(rt, 0o700); err != nil {
		t.Fatal(err)
	}
	s.RuntimeDir = rt
	t.Cleanup(func() { os.RemoveAll(rt) })
	return s
}

// ConfigPath is the default configuration path inside the sandbox.
func (s *Sandbox) ConfigPath() string {
	return filepath.Join(s.ConfigHome, "cuttle", "config.toml")
}

// LogPath is the log path inside the sandbox.
func (s *Sandbox) LogPath() string {
	return filepath.Join(s.StateHome, "cuttle", "cuttle.log")
}

// Env is the complete environment cuttle runs with.
func (s *Sandbox) Env() []string {
	return s.envWith(nil)
}

// overridable are the variables RunEnv may change; each must still name a
// directory inside the sandbox.
var overridable = []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME"}

func (s *Sandbox) envWith(over map[string]string) []string {
	dirs := s.dirs(over)
	return []string{
		"PATH=/usr/bin:/bin",
		"HOME=" + dirs["HOME"],
		"XDG_CONFIG_HOME=" + dirs["XDG_CONFIG_HOME"],
		"XDG_DATA_HOME=" + dirs["XDG_DATA_HOME"],
		"XDG_STATE_HOME=" + dirs["XDG_STATE_HOME"],
		"XDG_RUNTIME_DIR=" + s.RuntimeDir,
		"TMPDIR=" + dirs["TMPDIR"],
	}
}

// dirs returns the sandbox directories by variable, with overrides applied.
func (s *Sandbox) dirs(over map[string]string) map[string]string {
	d := map[string]string{
		"HOME": s.Home, "XDG_CONFIG_HOME": s.ConfigHome, "XDG_DATA_HOME": s.DataHome,
		"XDG_STATE_HOME": s.StateHome, "TMPDIR": s.TmpDir,
	}
	for k, v := range over {
		d[k] = v
	}
	return d
}

// FreePort chooses a port by binding 127.0.0.1:0 and releasing it. It never
// returns 3128 or 3129.
func (s *Sandbox) FreePort() uint16 {
	s.t.Helper()
	for {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			s.t.Fatal(err)
		}
		port := uint16(l.Addr().(*net.TCPAddr).Port)
		l.Close()
		if port != 3128 && port != 3129 {
			s.MarkChosen(port)
			return port
		}
	}
}

// MarkChosen records a port as chosen by the test.
func (s *Sandbox) MarkChosen(port uint16) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chosen[port] = true
}

func within(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}

func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// Check refuses to start cuttle unless the sandbox is isolated (E6): HOME,
// every XDG directory, and TMPDIR are inside the test's temporary root,
// which is inside the system temporary directory; XDG_RUNTIME_DIR is a
// cuttle-* directory under /tmp with mode 0700 owned by the user; and, for
// serve, which binds, the configuration exists, proxy.listen avoids 3128
// and 3129, and admin.listen is "" or a port the test chose. Commands that
// bind nothing (init, check, explain, paths, version) may read any
// configuration in the sandbox.
func (s *Sandbox) Check(args []string) error {
	return s.check(args, nil)
}

// check is Check for a command run with environment overrides (RunEnv).
// Only the variables in overridable may change, and the new values are
// checked like the others.
func (s *Sandbox) check(args []string, over map[string]string) error {
	for k := range over {
		if !slices.Contains(overridable, k) {
			return fmt.Errorf("%s may not be overridden", k)
		}
	}
	tmpRoot := realPath(os.TempDir())
	root := realPath(s.Root)
	if !within(tmpRoot, root) {
		return fmt.Errorf("sandbox root %s is not inside the temporary directory %s", s.Root, tmpRoot)
	}
	realHome := os.Getenv("HOME")
	for name, dir := range s.dirs(over) {
		if !filepath.IsAbs(dir) || !within(root, realPath(dir)) && !within(s.Root, dir) {
			return fmt.Errorf("%s=%s is not inside the sandbox %s", name, dir, s.Root)
		}
		if realHome != "" && (realPath(dir) == realPath(realHome) || within(realPath(dir), realPath(realHome))) {
			return fmt.Errorf("%s=%s is the real home directory", name, dir)
		}
	}
	rt := s.RuntimeDir
	if !within(realPath("/tmp"), realPath(rt)) || !strings.HasPrefix(filepath.Base(rt), "cuttle-") {
		return fmt.Errorf("XDG_RUNTIME_DIR=%s is not a cuttle-* directory under /tmp", rt)
	}
	fi, err := os.Stat(rt)
	if err != nil {
		return fmt.Errorf("XDG_RUNTIME_DIR: %v", err)
	}
	if fi.Mode().Perm() != 0o700 {
		return fmt.Errorf("XDG_RUNTIME_DIR=%s has mode %04o, not 0700", rt, fi.Mode().Perm())
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("XDG_RUNTIME_DIR=%s is owned by another user", rt)
	}

	cfg := s.ConfigPath()
	for i, a := range args {
		switch {
		case (a == "--config" || a == "-config") && i+1 < len(args):
			cfg = args[i+1]
		case strings.HasPrefix(a, "--config="), strings.HasPrefix(a, "-config="):
			cfg = a[strings.IndexByte(a, '=')+1:]
		}
	}
	if !within(root, realPath(cfg)) && !within(s.Root, cfg) {
		return fmt.Errorf("configuration %s is not inside the sandbox", cfg)
	}
	if len(args) == 0 || args[0] != "serve" {
		return nil
	}
	data, err := os.ReadFile(cfg)
	if err != nil {
		return fmt.Errorf("serve needs a configuration in the sandbox; the starter would bind 3128: %v", err)
	}
	var raw struct {
		Proxy struct {
			Listen []string `toml:"listen"`
		} `toml:"proxy"`
		Admin struct {
			Listen *string `toml:"listen"`
		} `toml:"admin"`
	}
	if _, err := toml.Decode(string(data), &raw); err != nil {
		return fmt.Errorf("configuration: %v", err)
	}
	if len(raw.Proxy.Listen) == 0 {
		return errors.New("proxy.listen is unset, so cuttle would bind 127.0.0.1:3128")
	}
	for _, l := range raw.Proxy.Listen {
		ap, err := netip.ParseAddrPort(l)
		if err != nil {
			continue // cuttle refuses it itself
		}
		if ap.Port() == 3128 || ap.Port() == 3129 {
			return fmt.Errorf("proxy.listen %s uses a live installation's port", l)
		}
	}
	if raw.Admin.Listen == nil {
		return errors.New("admin.listen is unset, so it defaults to 127.0.0.1:3129")
	}
	if a := *raw.Admin.Listen; a != "" {
		ap, err := netip.ParseAddrPort(a)
		if err != nil {
			return fmt.Errorf("admin.listen %q is not IP:port", a)
		}
		s.mu.Lock()
		chosen := s.chosen[ap.Port()]
		s.mu.Unlock()
		if !chosen || ap.Port() == 3128 || ap.Port() == 3129 {
			return fmt.Errorf("admin.listen %s is not a port the test chose", a)
		}
	}
	return nil
}

func (s *Sandbox) command(args []string, over map[string]string) *exec.Cmd {
	s.t.Helper()
	if err := s.check(args, over); err != nil {
		s.t.Fatalf("refusing to start cuttle %s: %v", strings.Join(args, " "), err)
	}
	cmd := exec.Command(s.Bin, args...)
	cmd.Env = s.envWith(over)
	cmd.Dir = s.Root
	return cmd
}

// Result is the outcome of a finished command.
type Result struct {
	Stdout, Stderr string
	Code           int
}

// Run runs a command to completion within timeout and returns its output
// and exit status. A command still running at the timeout is killed and
// fails the test.
func (s *Sandbox) Run(timeout time.Duration, args ...string) Result {
	s.t.Helper()
	return s.RunEnv(timeout, nil, args...)
}

// RunEnv is Run with some XDG directories replaced by other directories
// inside the sandbox, such as another XDG_STATE_HOME.
func (s *Sandbox) RunEnv(timeout time.Duration, over map[string]string, args ...string) Result {
	s.t.Helper()
	return s.run(timeout, over, nil, args)
}

// RunInput is Run with input on standard input.
func (s *Sandbox) RunInput(timeout time.Duration, input string, args ...string) Result {
	s.t.Helper()
	return s.run(timeout, nil, strings.NewReader(input), args)
}

func (s *Sandbox) run(timeout time.Duration, over map[string]string, stdin io.Reader, args []string) Result {
	s.t.Helper()
	cmd := s.command(args, over)
	var out, errb bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, &out, &errb
	if err := cmd.Start(); err != nil {
		s.t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			s.t.Fatal(err)
		}
		return Result{Stdout: out.String(), Stderr: errb.String(), Code: code}
	case <-time.After(timeout):
		cmd.Process.Kill()
		<-done
		s.t.Fatalf("cuttle %s still running after %v; killed\nstdout: %s\nstderr: %s", strings.Join(args, " "), timeout, out.String(), errb.String())
		return Result{}
	}
}

// Process is a running cuttle.
type Process struct {
	cmd    *exec.Cmd
	out    *syncBuffer
	errb   *syncBuffer
	done   chan struct{}
	err    error
	t      testing.TB
	stopMu sync.Mutex
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// Start starts a long-running command, waits until addr accepts
// connections, and stops the process when the test ends.
func (s *Sandbox) Start(addr string, args ...string) *Process {
	s.t.Helper()
	cmd := s.command(args, nil)
	p := &Process{cmd: cmd, out: &syncBuffer{}, errb: &syncBuffer{}, done: make(chan struct{}), t: s.t}
	cmd.Stdout, cmd.Stderr = p.out, p.errb
	if err := cmd.Start(); err != nil {
		s.t.Fatal(err)
	}
	go func() { p.err = cmd.Wait(); close(p.done) }()
	s.t.Cleanup(func() { p.Stop() })
	deadline := time.Now().Add(15 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			return p
		}
		select {
		case <-p.done:
			s.t.Fatalf("cuttle exited before serving: %v\nstderr: %s", p.err, p.errb.String())
		default:
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("cuttle did not serve on %s\nstderr: %s", addr, p.errb.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Stop sends SIGTERM, waits up to 10 s, then kills the process. It returns
// the exit status, or -1 if the process had to be killed.
func (p *Process) Stop() int {
	p.stopMu.Lock()
	defer p.stopMu.Unlock()
	select {
	case <-p.done:
		return p.code()
	default:
	}
	p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
		return p.code()
	case <-time.After(10 * time.Second):
		p.cmd.Process.Kill()
		<-p.done
		return -1
	}
}

func (p *Process) code() int {
	var ee *exec.ExitError
	if errors.As(p.err, &ee) {
		return ee.ExitCode()
	}
	if p.err != nil {
		return -1
	}
	return 0
}

// Signal sends sig to the process.
func (p *Process) Signal(sig os.Signal) error { return p.cmd.Process.Signal(sig) }

// Wait waits up to timeout for the process to exit and returns its exit
// status; exited is false if it is still running.
func (p *Process) Wait(timeout time.Duration) (code int, exited bool) {
	select {
	case <-p.done:
		return p.code(), true
	case <-time.After(timeout):
		return 0, false
	}
}

// Stderr returns what the process wrote to stderr so far.
func (p *Process) Stderr() string { return p.errb.String() }

// Port formats a chosen port as a loopback address.
func Port(port uint16) string { return "127.0.0.1:" + strconv.Itoa(int(port)) }

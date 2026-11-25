package testenv

import (
	"io"
	"log"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"testing"
)

func newDiscardLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// SourceListener wraps ln so that every accepted connection reports peer as
// its remote address, with the real ephemeral port (design 0007 D4). An
// instance then classifies the client by that address.
func SourceListener(ln net.Listener, peer netip.Addr) net.Listener {
	return &sourceListener{Listener: ln, peer: peer}
}

type sourceListener struct {
	net.Listener
	peer netip.Addr
}

func (l *sourceListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	port := uint16(0)
	if ta, ok := c.RemoteAddr().(*net.TCPAddr); ok {
		port = uint16(ta.Port)
	}
	return &peerConn{Conn: c, remote: net.TCPAddrFromAddrPort(netip.AddrPortFrom(l.peer, port))}, nil
}

type peerConn struct {
	net.Conn
	remote net.Addr
}

func (c *peerConn) RemoteAddr() net.Addr { return c.remote }

func (c *peerConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Conn.Close()
}

// CurlResult is the outcome of one curl run.
type CurlResult struct {
	Stdout, Stderr string
	Code           int
}

// CurlPath returns the curl binary for tests: $CUTTLE_CURL or
// /usr/bin/curl. Without one the test is skipped, unless
// CUTTLE_REQUIRE_CURL=1, which the T1 gate sets.
func CurlPath(t testing.TB) string {
	t.Helper()
	path := os.Getenv("CUTTLE_CURL")
	if path == "" {
		path = "/usr/bin/curl"
	}
	if _, err := os.Stat(path); err != nil {
		if os.Getenv("CUTTLE_REQUIRE_CURL") == "1" {
			t.Fatalf("curl required but %s is missing: %v", path, err)
		}
		t.Skipf("curl not available at %s", path)
	}
	return path
}

// Curl runs curl with an environment holding only PATH and a temporary
// HOME, and with -q so no .curlrc is read (design 0007 D5).
func Curl(t testing.TB, args ...string) CurlResult {
	t.Helper()
	cmd := exec.Command(CurlPath(t), append([]string{"-q"}, args...)...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}
	var out, errb limitedBuffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("curl: %v", err)
		}
		code = ee.ExitCode()
	}
	return CurlResult{Stdout: out.String(), Stderr: errb.String(), Code: code}
}

type limitedBuffer struct{ b []byte }

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if len(l.b) < 8<<20 {
		l.b = append(l.b, p...)
	}
	return len(p), nil
}

func (l *limitedBuffer) String() string { return string(l.b) }

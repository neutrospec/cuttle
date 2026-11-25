// Package control serves cuttle's control socket and is its client (design
// 0006 D4): an HTTP/1.1 server on a Unix stream socket with mode 0600 in a
// directory that the user owns with mode 0700. File-system permissions are
// the access control; there is no password.
package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"squid/internal/paths"
)

// Errors from Listen.
var (
	// ErrRunning means another cuttle instance accepts connections on the
	// socket; cuttle serve then exits with status 1.
	ErrRunning = errors.New("another cuttle instance is running")
	// ErrInsecureDir means the socket directory is not a directory that the
	// user owns with mode 0700; cuttle then runs without the socket (design
	// 0006 D2).
	ErrInsecureDir = errors.New("control socket directory is not private")
)

// Listen prepares the socket at path and listens on it (design 0006 D2,
// D4). A missing directory is created with mode 0700. A socket that accepts
// a connection belongs to a running instance and gives ErrRunning; a stale
// socket is removed.
func Listen(path string) (net.Listener, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%w: %s is not a directory", ErrInsecureDir, dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return nil, fmt.Errorf("%w: %s is owned by another user (uid %d)", ErrInsecureDir, dir, st.Uid)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		return nil, fmt.Errorf("%w: %s has mode %04o, not 0700", ErrInsecureDir, dir, perm)
	}
	if fi, err := os.Lstat(path); err == nil {
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			c.Close()
			return nil, fmt.Errorf("%w: %s accepts connections", ErrRunning, path)
		}
		if fi.Mode().Type() != fs.ModeSocket {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale socket: %w", err)
		}
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// Status is the response of GET /v1/status (design 0006 D4).
type Status struct {
	Version   string      `json:"version"`
	Name      string      `json:"name"`
	PID       int         `json:"pid"`
	StartedAt time.Time   `json:"started_at"`
	Paths     paths.Paths `json:"paths"`
	Listeners []Listener  `json:"listeners"`
	Upstreams []Upstream  `json:"upstreams"`
	Config    Config      `json:"config"`
	// Counters holds every live counter by key, such as
	// bytes_total{action="direct",direction="up"} (design 0002 D7).
	Counters map[string]int64 `json:"counters"`
	// Tunnels lists up to 500 active tunnels and upgraded connections.
	Tunnels []Tunnel `json:"tunnels"`
	// Stats is the statistics state; "disabled" until M2.
	Stats        string `json:"stats"`
	Log          Log    `json:"log"`
	RlimitNofile uint64 `json:"rlimit_nofile"`
}

// Listener is one proxy listener (design 0003 D9).
type Listener struct {
	Address string    `json:"address"`
	State   string    `json:"state"` // bound or retrying
	Error   string    `json:"error,omitempty"`
	Since   time.Time `json:"since"`
}

// Upstream is one upstream's reachability (design 0003 D8).
type Upstream struct {
	Name    string    `json:"name"`
	Address string    `json:"address"`
	State   string    `json:"state"` // up, down, or probing
	Since   time.Time `json:"since"`
	// DownS is the time in seconds since the first failure of the current
	// outage while the state is down or probing, and 0 when up.
	DownS float64 `json:"down_s"`
	// FailedFast counts the requests answered at once during the current or
	// last outage.
	FailedFast int64  `json:"failed_fast"`
	LastError  string `json:"last_error"`
}

// Config is the state of the configuration (design 0004 D6).
type Config struct {
	Path            string    `json:"path"`
	SHA256          string    `json:"sha256"`
	LoadedAt        time.Time `json:"loaded_at"`
	RestartRequired []string  `json:"restart_required"`
	LastReloadError string    `json:"last_reload_error"`
}

// Tunnel is one active tunnel or upgraded connection (design 0002 D7).
type Tunnel struct {
	Target   string `json:"target"`
	Kind     string `json:"kind"` // connect, or upgrade after a 101
	Action   string `json:"action"`
	Upstream string `json:"upstream,omitempty"`
	Source   string `json:"source"`
	AgeS     int64  `json:"age_s"`
	Up       int64  `json:"up"`
	Down     int64  `json:"down"`
}

// Log is the state of the log writer (design 0006 D7).
type Log struct {
	Path string `json:"path"`
	// RotationFailures counts failed rotations and reopens since start.
	RotationFailures int64 `json:"rotation_failures"`
}

// ReloadResult is the response of POST /v1/reload: 200 with the
// restart-only keys that differ and the warnings, or 422 with the errors
// (design 0006 D4).
type ReloadResult struct {
	RestartRequired []string `json:"restart_required"`
	Warnings        []string `json:"warnings"`
	Errors          []string `json:"errors,omitempty"`
}

// Service is what the control socket exposes.
type Service interface {
	Status() Status
	// Reload reloads the configuration; ok is false when it was refused.
	Reload() (res ReloadResult, ok bool)
}

// Serve serves svc on l until the returned server is closed.
func Serve(l net.Listener, svc Service, log *slog.Logger) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, svc.Status())
	})
	mux.HandleFunc("POST /v1/reload", func(w http.ResponseWriter, r *http.Request) {
		res, ok := svc.Reload()
		code := http.StatusOK
		if !ok {
			code = http.StatusUnprocessableEntity
		}
		writeJSON(w, code, res)
	})
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	go srv.Serve(l)
	return srv
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

// Client talks to a running instance over its socket.
type Client struct {
	path string
	hc   *http.Client
}

// NewClient returns a client for the socket at path.
func NewClient(path string) *Client {
	tr := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
		DisableKeepAlives: true,
	}
	return &Client{path: path, hc: &http.Client{Transport: tr, Timeout: 30 * time.Second}}
}

// Status fetches the running instance's status.
func (c *Client) Status(ctx context.Context) (*Status, error) {
	var st Status
	if _, err := c.do(ctx, http.MethodGet, "/v1/status", &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// Reload asks the running instance to reload; ok is false when it refused
// the configuration.
func (c *Client) Reload(ctx context.Context) (res *ReloadResult, ok bool, err error) {
	var r ReloadResult
	code, err := c.do(ctx, http.MethodPost, "/v1/reload", &r)
	if err != nil {
		return nil, false, err
	}
	return &r, code == http.StatusOK, nil
}

func (c *Client) do(ctx context.Context, method, path string, v any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://cuttle"+path, nil)
	if err != nil {
		return 0, err
	}
	res, err := c.hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("cannot reach the running service at %s: %w", c.path, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if err != nil {
		return 0, err
	}
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusUnprocessableEntity {
		return 0, fmt.Errorf("%s %s: %s", method, path, res.Status)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return 0, fmt.Errorf("%s %s: %w", method, path, err)
	}
	return res.StatusCode, nil
}

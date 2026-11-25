package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"squid/internal/config"
	"squid/internal/control"
	"squid/internal/logx"
	"squid/internal/netx"
	"squid/internal/paths"
	"squid/internal/proxy"
)

// shutdownDrain is how long serve waits for in-flight plain-HTTP exchanges
// after SIGTERM or SIGINT (design 0006 D6).
const shutdownDrain = 5 * time.Second

// serveState is everything serve prepares before it binds a listener, and
// the running configuration's state for status and reload.
type serveState struct {
	paths   paths.Paths
	cfgPath string
	cfg     *config.Config
	logw    *logx.Writer
	log     *slog.Logger
	level   *slog.LevelVar

	// retryInterval overrides proxy.RetryInterval in tests.
	retryInterval time.Duration

	p         *proxy.Proxy
	startCfg  *config.Config // the configuration the listeners were bound with
	startedAt time.Time

	// reloadMu serializes reloads; mu guards the fields below.
	reloadMu        sync.Mutex
	mu              sync.Mutex
	loadedAt        time.Time
	restartRequired []string
	lastReloadError string
}

func (s *serveState) close() {
	if s.logw != nil {
		s.logw.Close()
	}
}

// prepareServe runs the first-run steps of design 0006 D5 that come before
// binding: resolve paths, open the log, write the starter configuration if
// none exists, and load and validate the configuration. The caller sets the
// umask first.
func prepareServe(lookup env, cfgFlag string, stderr io.Writer) (*serveState, error) {
	p, err := paths.Resolve(lookup)
	if err != nil {
		return nil, err
	}
	s := &serveState{paths: p, cfgPath: cfgFlag, level: new(slog.LevelVar)}
	if s.cfgPath == "" {
		s.cfgPath = p.Config.Path
	}
	s.logw, err = logx.Open(p.Log.Path, logx.DefaultMaxBytes, logx.DefaultKeep)
	if err != nil {
		return nil, fmt.Errorf("open log: %w", err)
	}
	var out io.Writer = s.logw
	if isTerminal(stderr) {
		out = io.MultiWriter(s.logw, stderr)
	}
	s.log = logx.NewLogger(out, s.level)

	wrote, err := config.EnsureStarter(s.cfgPath)
	if err != nil {
		s.close()
		return nil, fmt.Errorf("write starter configuration: %w", err)
	}
	if wrote {
		s.log.Info("wrote starter configuration", "path", s.cfgPath)
	}
	if err := config.CheckFileSecurity(s.cfgPath); err != nil {
		s.log.Error("configuration refused", "error", err.Error())
		s.close()
		return nil, err
	}
	cfg, report, err := config.Load(s.cfgPath)
	if err != nil {
		if report != nil {
			for _, e := range report.Errors {
				s.log.Error("configuration error", "error", e.String())
			}
		} else {
			s.log.Error("configuration unreadable", "error", err.Error())
		}
		s.close()
		return nil, err
	}
	for _, w := range report.Warnings {
		s.log.Warn("configuration warning", "warning", w.String())
	}
	if lvl, err := logx.ParseLevel(cfg.Log.Level); err == nil {
		s.level.Set(lvl)
	}
	s.cfg, s.startCfg, s.loadedAt, s.restartRequired = cfg, cfg, time.Now(), []string{}
	return s, nil
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func cmdServe(args []string, lookup env, stdout, stderr io.Writer) int {
	fl := newFlags("serve", stderr)
	cfgFlag := fl.String("config", "", "configuration file `PATH`")
	if fl.Parse(args) != nil || fl.NArg() != 0 {
		return exitUsage
	}
	syscall.Umask(0o077)
	s, err := prepareServe(lookup, *cfgFlag, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "cuttle: %v\n", err)
		return exitFailure
	}
	defer s.close()
	return s.serve(stderr)
}

// serve opens the control socket, binds the listeners, runs the proxy, and
// handles signals (design 0006 D5 steps 6-8 and D6).
func (s *serveState) serve(stderr io.Writer) int {
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(signals)
	return s.run(stderr, net.Listen, signals)
}

// run is serve with the bind function and the signal channel passed in, so
// a test can serve a non-loopback listener address on a loopback port and
// stop the proxy without signalling the test process.
//
// A loopback listener that cannot bind stops startup with exit status 1. A
// non-loopback listener that cannot bind is logged and retried every
// proxy.RetryInterval while the other listeners serve (design 0003 D9).
func (s *serveState) run(stderr io.Writer, listen func(network, address string) (net.Listener, error), signals <-chan os.Signal) int {
	fail := func(msg string, err error) int {
		s.log.Error(msg, "error", err.Error())
		fmt.Fprintf(stderr, "cuttle: %s: %v\n", msg, err)
		return exitFailure
	}
	s.startedAt = time.Now()
	// The configuration the listeners bind with; a reload replaces s.cfg
	// under s.mu once the control socket serves.
	cfg := s.cfg

	// D5 step 6: the control socket. Another running instance stops
	// startup; any other failure leaves cuttle running without the socket.
	ctl, err := control.Listen(s.paths.Control.Path)
	switch {
	case errors.Is(err, control.ErrRunning):
		return fail("control socket", err)
	case err != nil:
		s.log.Warn("control socket unavailable; running without it", "path", s.paths.Control.Path, "error", err.Error())
		ctl = nil
	}
	closeCtl := func() {
		if ctl != nil {
			ctl.Close()
		}
	}

	// D5 step 7: the listeners.
	retryInterval := s.retryInterval
	if retryInterval <= 0 {
		retryInterval = proxy.RetryInterval
	}
	var listeners []net.Listener
	var retry []proxy.RetryListener
	closeAll := func() {
		for _, l := range listeners {
			l.Close()
		}
		closeCtl()
	}
	for _, addr := range cfg.Proxy.Listen {
		l, err := listen("tcp", addr.String())
		if err != nil {
			if addr.Addr().Unmap().IsLoopback() {
				closeAll()
				return fail("bind "+addr.String(), err)
			}
			s.log.Warn("listener retrying", "address", addr.String(), "error", err.Error(), "retry", retryInterval.String())
			retry = append(retry, proxy.RetryListener{Address: addr.String(), Err: err})
			continue
		}
		listeners = append(listeners, l)
	}
	p, err := proxy.New(cfg, proxy.Deps{
		Network:       netx.System{},
		SelfAddrs:     netx.InterfaceAddrs(10 * time.Second),
		Listeners:     listeners,
		Retry:         retry,
		Bind:          listen,
		RetryInterval: retryInterval,
		Logger:        s.log,
	})
	if err != nil {
		closeAll()
		return fail("start", err)
	}
	s.p = p
	var ctlSrv *http.Server
	if ctl != nil {
		ctlSrv = control.Serve(ctl, s, s.log)
	}

	// D5 step 8.
	addrs := make([]string, len(listeners))
	for i, l := range listeners {
		addrs[i] = l.Addr().String()
	}
	retrying := make([]string, len(retry))
	for i, r := range retry {
		retrying[i] = r.Address
	}
	s.log.Info("started", "version", version, "go", runtime.Version(), "name", cfg.Name, "pid", os.Getpid(),
		"config", s.cfgPath, "log", s.paths.Log.Path, "control", s.paths.Control.Path,
		"listeners", strings.Join(addrs, ","), "retrying", strings.Join(retrying, ","))
	if cfg.Admin.Listen.IsValid() {
		s.log.Info("admin UI not available in this version", "admin.listen", cfg.Admin.Listen.String())
	}

	served := make(chan error, 1)
	go func() { served <- p.Serve() }()
	for {
		select {
		case err := <-served:
			if ctlSrv != nil {
				ctlSrv.Close()
			}
			if err != nil {
				return fail("serve", err)
			}
			return exitOK
		case sig := <-signals:
			if sig == syscall.SIGHUP {
				s.Reload()
				continue
			}
			s.log.Info("stopping", "signal", sig.String())
			go func() {
				for sig := range signals {
					if sig != syscall.SIGHUP {
						s.log.Warn("second signal; exiting at once", "signal", sig.String())
						os.Exit(exitFailure)
					}
				}
			}()
			// D6 step 1: the control listener closes with the proxy
			// listeners.
			if ctlSrv != nil {
				ctlSrv.Close()
			}
			ctx, cancel := context.WithTimeout(context.Background(), shutdownDrain)
			err := p.Shutdown(ctx)
			cancel()
			if err != nil && !errors.Is(err, context.DeadlineExceeded) {
				s.log.Warn("shutdown", "error", err.Error())
			}
			<-served
			s.log.Info("stopped")
			return exitOK
		}
	}
}

// Reload reads and validates the configuration file and applies it (design
// 0004 D6). It serves SIGHUP and the control socket's POST /v1/reload. On
// failure the running configuration stays, and the errors are logged and
// returned.
func (s *serveState) Reload() (control.ReloadResult, bool) {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	res := control.ReloadResult{RestartRequired: []string{}, Warnings: []string{}}
	refuse := func(errs []string) (control.ReloadResult, bool) {
		for _, e := range errs {
			s.log.Error("configuration error", "error", e)
		}
		s.log.Error("reload failed; keeping the running configuration", "config", s.cfgPath, "errors", len(errs))
		s.mu.Lock()
		s.lastReloadError = strings.Join(errs, "\n")
		s.mu.Unlock()
		res.Errors = errs
		return res, false
	}
	if err := config.CheckFileSecurity(s.cfgPath); err != nil {
		return refuse([]string{err.Error()})
	}
	cfg, report, err := config.Load(s.cfgPath)
	if err != nil {
		if report == nil {
			return refuse([]string{err.Error()})
		}
		errs := make([]string, len(report.Errors))
		for i, e := range report.Errors {
			errs[i] = e.String()
		}
		return refuse(errs)
	}
	if err := s.p.Reload(cfg); err != nil {
		return refuse([]string{err.Error()})
	}
	if lvl, err := logx.ParseLevel(cfg.Log.Level); err == nil {
		s.level.Set(lvl)
	}
	for _, w := range report.Warnings {
		s.log.Warn("configuration warning", "warning", w.String())
		res.Warnings = append(res.Warnings, w.String())
	}
	res.RestartRequired = config.RestartRequired(s.startCfg, cfg)
	s.mu.Lock()
	s.cfg, s.loadedAt, s.restartRequired, s.lastReloadError = cfg, time.Now(), res.RestartRequired, ""
	s.mu.Unlock()
	s.log.Info("reloaded", "config", s.cfgPath, "sha256", cfg.SHA256, "name", cfg.Name,
		"restart_required", strings.Join(res.RestartRequired, ","))
	return res, true
}

// Status reports the running instance for GET /v1/status (design 0006 D4).
func (s *serveState) Status() control.Status {
	s.mu.Lock()
	cfg, loadedAt, rr, lastErr := s.cfg, s.loadedAt, s.restartRequired, s.lastReloadError
	s.mu.Unlock()
	now := time.Now()
	st := control.Status{
		Version:   version,
		Name:      cfg.Name,
		PID:       os.Getpid(),
		StartedAt: s.startedAt.UTC(),
		Paths:     s.paths.WithPresence(),
		Listeners: []control.Listener{},
		Upstreams: []control.Upstream{},
		Config: control.Config{
			Path: s.cfgPath, SHA256: cfg.SHA256, LoadedAt: loadedAt.UTC(),
			RestartRequired: append([]string{}, rr...), LastReloadError: lastErr,
		},
		Counters: s.p.Live().Snapshot(),
		Tunnels:  []control.Tunnel{},
		Stats:    "disabled",
		Log:      control.Log{Path: s.paths.Log.Path, RotationFailures: s.logw.Failures()},
	}
	for _, l := range s.p.Listeners() {
		st.Listeners = append(st.Listeners, control.Listener{Address: l.Address, State: l.State, Error: l.Error, Since: l.Since.UTC()})
	}
	for _, u := range s.p.Upstreams() {
		st.Upstreams = append(st.Upstreams, control.Upstream{Name: u.Name, Address: u.Address, State: u.State, Since: u.Since.UTC(),
			DownS: u.DownFor.Round(time.Millisecond).Seconds(), FailedFast: u.FailedFast, LastError: u.LastError})
	}
	for _, t := range s.p.Tunnels() {
		st.Tunnels = append(st.Tunnels, control.Tunnel{Target: t.Target, Kind: t.Kind, Action: t.Action, Upstream: t.Upstream,
			Source: t.Source, AgeS: int64(now.Sub(t.Started) / time.Second), Up: t.Up, Down: t.Down})
	}
	var rl syscall.Rlimit
	if syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl) == nil {
		st.RlimitNofile = rl.Cur
	}
	return st
}

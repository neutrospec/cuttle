package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"squid/internal/config"
	"squid/internal/logx"
	"squid/internal/netx"
	"squid/internal/paths"
	"squid/internal/proxy"
)

// shutdownDrain is how long serve waits for in-flight plain-HTTP exchanges
// after SIGTERM or SIGINT (design 0006 D6).
const shutdownDrain = 5 * time.Second

// serveState is everything serve prepares before it binds a listener.
type serveState struct {
	paths   paths.Paths
	cfgPath string
	cfg     *config.Config
	logw    *logx.Writer
	log     *slog.Logger
	level   *slog.LevelVar
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
	s.cfg = cfg
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

// serve binds the listeners, runs the proxy, and handles signals (design
// 0006 D5 steps 7-8 and D6).
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
// Every configured listener, loopback or not, must bind, or startup stops
// with exit status 1. Retrying a non-loopback listener that cannot bind
// (design 0003 D9) arrives in M1.4.
func (s *serveState) run(stderr io.Writer, listen func(network, address string) (net.Listener, error), signals <-chan os.Signal) int {
	fail := func(msg string, err error) int {
		s.log.Error(msg, "error", err.Error())
		fmt.Fprintf(stderr, "cuttle: %s: %v\n", msg, err)
		return exitFailure
	}
	var listeners []net.Listener
	closeAll := func() {
		for _, l := range listeners {
			l.Close()
		}
	}
	for _, addr := range s.cfg.Proxy.Listen {
		l, err := listen("tcp", addr.String())
		if err != nil {
			closeAll()
			return fail("bind "+addr.String(), err)
		}
		listeners = append(listeners, l)
	}
	p, err := proxy.New(s.cfg, proxy.Deps{
		Network:   netx.System{},
		SelfAddrs: netx.InterfaceAddrs(10 * time.Second),
		Listeners: listeners,
		Logger:    s.log,
	})
	if err != nil {
		closeAll()
		return fail("start", err)
	}
	addrs := make([]string, len(listeners))
	for i, l := range listeners {
		addrs[i] = l.Addr().String()
	}
	s.log.Info("started", "version", version, "go", runtime.Version(), "name", s.cfg.Name, "pid", os.Getpid(),
		"config", s.cfgPath, "log", s.paths.Log.Path, "control", s.paths.Control.Path,
		"listeners", strings.Join(addrs, ","))
	if s.cfg.Admin.Listen.IsValid() {
		s.log.Info("admin UI not available in this version", "admin.listen", s.cfg.Admin.Listen.String())
	}

	served := make(chan error, 1)
	go func() { served <- p.Serve() }()
	for {
		select {
		case err := <-served:
			if err != nil {
				return fail("serve", err)
			}
			return exitOK
		case sig := <-signals:
			if sig == syscall.SIGHUP {
				s.log.Warn("reload is not available in this version; restart cuttle to apply changes")
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

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/neutrospec/cuttle/internal/authority"
	"github.com/neutrospec/cuttle/internal/config"
	"github.com/neutrospec/cuttle/internal/paths"
	"github.com/neutrospec/cuttle/internal/policy"
)

// configPath returns --config or the resolved default.
func configPath(flagValue string, lookup env) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	p, err := paths.Resolve(lookup)
	if err != nil {
		return "", err
	}
	return p.Config.Path, nil
}

func cmdInit(args []string, lookup env, stdout, stderr io.Writer) int {
	fl := newFlags("init", stderr)
	cfgFlag := fl.String("config", "", "configuration file `PATH`")
	if fl.Parse(args) != nil || fl.NArg() != 0 {
		return exitUsage
	}
	path, err := configPath(*cfgFlag, lookup)
	if err != nil {
		fmt.Fprintln(stderr, "cuttle:", err)
		return exitFailure
	}
	wrote, err := config.EnsureStarter(path)
	if wrote && errors.Is(err, config.ErrNotSynced) {
		// The file is in place (OI-0006 Z-R4).
		fmt.Fprintln(stderr, "cuttle: warning: wrote the starter configuration, but", err)
		err = nil
	}
	if err != nil {
		fmt.Fprintln(stderr, "cuttle:", err)
		return exitFailure
	}
	if !wrote {
		fmt.Fprintf(stderr, "cuttle: %s already exists; left unchanged\n", path)
	}
	fmt.Fprintln(stdout, path)
	return exitOK
}

func cmdCheck(args []string, lookup env, stdout, stderr io.Writer) int {
	fl := newFlags("check", stderr)
	cfgFlag := fl.String("config", "", "configuration file `PATH`")
	if fl.Parse(args) != nil || fl.NArg() != 0 {
		return exitUsage
	}
	path, err := configPath(*cfgFlag, lookup)
	if err != nil {
		fmt.Fprintln(stderr, "cuttle:", err)
		return exitFailure
	}
	_, report, err := config.Load(path)
	if report == nil {
		fmt.Fprintln(stderr, "cuttle:", err)
		return exitFailure
	}
	for _, e := range report.Errors {
		fmt.Fprintln(stdout, "error:", e)
	}
	if err := config.CheckFileSecurity(path); err != nil {
		fmt.Fprintln(stdout, "error:", strings.TrimPrefix(err.Error(), config.ErrInsecureFile.Error()+": "))
		report.Errors = append(report.Errors, config.Issue{File: path, Msg: err.Error()})
	}
	for _, w := range report.Warnings {
		fmt.Fprintln(stdout, "warning:", w)
	}
	if len(report.Errors) > 0 {
		fmt.Fprintf(stdout, "%s: %d error(s), %d warning(s)\n", path, len(report.Errors), len(report.Warnings))
		return exitFailure
	}
	fmt.Fprintf(stdout, "%s: ok, %d warning(s)\n", path, len(report.Warnings))
	return exitOK
}

func cmdExplain(args []string, lookup env, stdout, stderr io.Writer) int {
	fl := newFlags("explain", stderr)
	cfgFlag := fl.String("config", "", "configuration file `PATH`")
	remote := fl.Bool("remote", false, "explain for a remote client")
	if fl.Parse(args) != nil || fl.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: cuttle explain [--config PATH] [--remote] TARGET")
		return exitUsage
	}
	target, kind, err := parseExplainTarget(fl.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "cuttle: %v\n", err)
		return exitUsage
	}
	path, err := configPath(*cfgFlag, lookup)
	if err != nil {
		fmt.Fprintln(stderr, "cuttle:", err)
		return exitFailure
	}
	cfg, report, err := config.Load(path)
	if err != nil {
		if report != nil {
			for _, e := range report.Errors {
				fmt.Fprintln(stderr, "error:", e)
			}
		} else {
			fmt.Fprintln(stderr, "cuttle:", err)
		}
		return exitFailure
	}
	src := policy.SourceLocal
	if *remote {
		src = policy.SourceRemote
	}
	d := policy.New(cfg.PolicySettings()).Decide(target, kind, src)

	w := tabwriter.NewWriter(stdout, 0, 8, 2, ' ', 0)
	fmt.Fprintf(w, "target\t%s (%s, %s client)\n", d.Target, d.Kind, d.Source)
	rule := d.Rule
	switch d.Rule {
	case policy.RuleDefault:
		rule += " (proxy.default)"
	case policy.RuleRemoteDefault:
		rule += " (proxy.remote.default)"
	case policy.RulePort:
		rule += fmt.Sprintf(" (port %d is not in proxy.remote.ports)", d.Target.Port)
	case policy.RuleRemoteDirect:
		rule += " (direct needs proxy.remote.allow_direct)"
	default:
		rule += fmt.Sprintf(" (#%d)", d.RuleIndex)
	}
	fmt.Fprintf(w, "rule\t%s\n", rule)
	action := d.Route().String()
	switch d.Action {
	case policy.ActionUpstream:
		if u, ok := cfg.Upstream(d.Upstream); ok {
			action += fmt.Sprintf(" -> %s (disclosure %s)", u.Address, u.Disclosure)
		}
	case policy.ActionDirect:
		var flags []string
		if d.AllowLoopback {
			flags = append(flags, "allow_loopback")
		}
		if d.AllowPrivate {
			flags = append(flags, "allow_private")
		}
		if len(flags) > 0 {
			action += " (" + strings.Join(flags, ", ") + ")"
		}
	case policy.ActionBlock:
		action += " (403)"
	}
	fmt.Fprintf(w, "action\t%s\n", action)
	fmt.Fprintf(w, "interfaces\t%s\n", explainInterfaces(cfg, d))
	w.Flush()
	return exitOK
}

// explainInterfaces names the interface list in force and its source: a
// rule, proxy.interfaces, or an upstream's own list, or system routing
// when no list applies (design 0008 D15). It reads only the file.
func explainInterfaces(cfg *config.Config, d policy.Decision) string {
	const public = "; public addresses only"
	switch d.Action {
	case policy.ActionDirect:
		if len(d.Interfaces) == 0 {
			break
		}
		src := "proxy.interfaces"
		if d.RuleIndex > 0 && len(cfg.Rules[d.RuleIndex-1].Interfaces) > 0 {
			src = "rule " + d.Rule
		}
		return strings.Join(d.Interfaces, ", ") + " (" + src + ")" + public
	case policy.ActionUpstream:
		if u, ok := cfg.Upstream(d.Upstream); ok && len(u.Interfaces) > 0 {
			return strings.Join(u.Interfaces, ", ") + " (upstream " + u.Name + ")" + public
		}
	}
	return "system routing"
}

// parseExplainTarget maps host:port to a tunnel, an http:// URL to a
// plain-HTTP request, and an https:// URL to a tunnel to its port, 443 by
// default (design 0006 D3).
func parseExplainTarget(s string) (authority.Authority, policy.Kind, error) {
	lower := strings.ToLower(s)
	switch {
	case strings.HasPrefix(lower, "http://"):
		t, err := authority.ParseHTTP(s)
		if err != nil {
			return authority.Authority{}, 0, err
		}
		return t.Authority, policy.KindHTTP, nil
	case strings.HasPrefix(lower, "https://"):
		rest := s[len("https://"):]
		if i := strings.IndexAny(rest, "/?#"); i >= 0 {
			rest = rest[:i]
		}
		hasPort := strings.Contains(rest, ":")
		if strings.HasPrefix(rest, "[") {
			hasPort = strings.Contains(rest, "]:")
		}
		if !hasPort {
			rest += ":443"
		}
		a, err := authority.ParseConnect(rest)
		return a, policy.KindConnect, err
	case strings.Contains(s, "://"):
		return authority.Authority{}, 0, errors.New("TARGET must be host:port, an http:// URL, or an https:// URL")
	}
	a, err := authority.ParseConnect(s)
	return a, policy.KindConnect, err
}

func cmdPaths(args []string, lookup env, stdout, stderr io.Writer) int {
	fl := newFlags("paths", stderr)
	asJSON := fl.Bool("json", false, "print JSON")
	if fl.Parse(args) != nil || fl.NArg() != 0 {
		return exitUsage
	}
	p, err := paths.Resolve(lookup)
	if err != nil {
		fmt.Fprintln(stderr, "cuttle:", err)
		return exitFailure
	}
	p = p.WithPresence()
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.Encode(p)
		return exitOK
	}
	w := tabwriter.NewWriter(stdout, 0, 8, 2, ' ', 0)
	for _, it := range p.Items() {
		fmt.Fprintf(w, "%s\t%s\t%s\n", it.Name, it.Path, it.Source)
	}
	w.Flush()
	return exitOK
}

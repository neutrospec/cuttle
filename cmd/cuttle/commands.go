package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

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
	if _, _, err := config.ParseTarget(fl.Arg(0)); err != nil {
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
	// The same explanation as the admin's Test a URL (design 0009 D8).
	e, err := config.Explain(cfg, fl.Arg(0), src)
	if err != nil {
		fmt.Fprintf(stderr, "cuttle: %v\n", err)
		return exitUsage
	}
	w := tabwriter.NewWriter(stdout, 0, 8, 2, ' ', 0)
	fmt.Fprintf(w, "target\t%s\n", e.TargetText())
	fmt.Fprintf(w, "rule\t%s\n", e.RuleText())
	fmt.Fprintf(w, "action\t%s\n", e.ActionText())
	fmt.Fprintf(w, "interfaces\t%s\n", e.InterfacesText())
	if a := e.AddressText(); a != "" {
		fmt.Fprintf(w, "address\t%s\n", a)
	}
	w.Flush()
	return exitOK
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

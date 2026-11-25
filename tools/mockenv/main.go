// Command mockenv runs the mocks of design 0007 for the client checks
// (design 0007 D8). It is a development tool: it listens only on loopback,
// resolves names only through its own test network, and never answers 407.
//
//	mockenv serve [-dir DIR] [-mode NAME=MODE ...]
//
// starts the controlled origins, the recording upstreams upstream,
// egress-direct, and egress-chain, and the inspecting egress mock on
// 127.0.0.1 with fresh roots; writes the single, edge, and gateway cuttle
// configurations, the root certificates, and session.json into the session
// directory; prints them with the SPKI hashes for Chrome; and records every
// request as one JSON line in <dir>/<mock>.jsonl until SIGINT or SIGTERM.
// MODE is tunnel, buffered, silent, reset, or a status code; 407 is refused.
//
//	mockenv check-disclosure -dir DIR [-names a,b] [-client addr,...] [-target URL ...]
//
// compares egress-chain.jsonl with egress-direct.jsonl and exits 1 on any
// disclosure field, any instance name or client address, any changed
// method, target, or body, and any case target that is missing or
// unmatched.
//
//	mockenv m1-check [-clients chrome,curl] [-out DIR] [-cuttle PATH] [-chrome PATH] [-curl PATH] [-lna=false]
//
// runs the M1 client check end to end, one session per client, and exits 1
// if any case fails.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, waitForSignal))
}

const usage = `usage:
  mockenv serve [-dir DIR] [-mode NAME=MODE ...]
  mockenv check-disclosure -dir DIR [-names a,b] [-client addr,...] [-target URL ...]
  mockenv m1-check [-clients chrome,curl] [-out DIR] [-cuttle PATH] [-chrome PATH] [-curl PATH] [-lna=false]
`

// run executes one subcommand and returns the exit status: 0 on success, 1
// when a check fails, and 2 for a usage or setup error. wait blocks a serve
// session until it should stop.
func run(args []string, stdout, stderr io.Writer, wait func()) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "serve":
		return runServe(args[1:], stdout, stderr, wait)
	case "check-disclosure":
		return runCheckDisclosure(args[1:], stdout, stderr)
	case "m1-check":
		return runM1Check(args[1:], stdout, stderr)
	case "-h", "-help", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	}
	fmt.Fprintf(stderr, "mockenv: unknown command %q\n%s", args[0], usage)
	return 2
}

func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("mockenv "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func runServe(args []string, stdout, stderr io.Writer, wait func()) int {
	fs := newFlags("serve", stderr)
	dir := fs.String("dir", "", "session directory to create (default: a new mockenv-* directory under $TMPDIR)")
	modes := modeFlags{}
	fs.Var(modes, "mode", "NAME=MODE for every recording upstream: tunnel, buffered, silent, reset, or a status code other than 407 (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "mockenv serve: unexpected arguments %q\n", fs.Args())
		return 2
	}
	s, err := startSession(sessionOptions{Dir: *dir, Modes: modes})
	if err != nil {
		fmt.Fprintf(stderr, "mockenv serve: %v\n", err)
		return 2
	}
	s.print(stdout)
	fmt.Fprintln(stdout, "\nserving; stop with Ctrl-C or SIGTERM")
	wait()
	s.Close()
	fmt.Fprintf(stdout, "mockenv: stopped; recordings kept in %s\n", s.Dir)
	return 0
}

func waitForSignal() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	<-ch
	signal.Stop(ch)
}

func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, " ") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

func runCheckDisclosure(args []string, stdout, stderr io.Writer) int {
	fs := newFlags("check-disclosure", stderr)
	dir := fs.String("dir", "", "session directory (required)")
	names := fs.String("names", nameEdge+","+nameGateway, "instance names that must not appear, comma-separated")
	client := fs.String("client", "127.0.0.1", "original client addresses that must not appear, comma-separated")
	var targets listFlag
	fs.Var(&targets, "target", "case target URL (repeatable; default: the non-disclosure targets of design 0007 D8)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dir == "" || fs.NArg() > 0 {
		fmt.Fprint(stderr, "mockenv check-disclosure: -dir is required and takes no arguments\n")
		return 2
	}
	fails, err := checkDisclosure(disclosureOptions{Dir: *dir, Names: splitList(*names), Clients: splitList(*client), Targets: targets}, stdout)
	if err != nil {
		fmt.Fprintf(stderr, "mockenv check-disclosure: %v\n", err)
		return 2
	}
	if fails > 0 {
		return 1
	}
	return 0
}

func runM1Check(args []string, stdout, stderr io.Writer) int {
	fs := newFlags("m1-check", stderr)
	clients := fs.String("clients", "chrome,curl", "clients to check, comma-separated: chrome, curl")
	out := fs.String("out", "", "evidence directory (default: a new cuttle-m1-check-* directory under $TMPDIR)")
	cuttle := fs.String("cuttle", "", "cuttle binary (default: build ./cmd/cuttle into the evidence directory)")
	chrome := fs.String("chrome", defaultChrome, "Google Chrome binary")
	curl := fs.String("curl", defaultCurl, "curl binary")
	lna := fs.Bool("lna", true, "run the Local Network Access observation with Chrome (OI-0006 V6)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "mockenv m1-check: unexpected arguments %q\n", fs.Args())
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	results, err := m1Check(ctx, m1Options{Clients: splitList(*clients), Out: *out, Cuttle: *cuttle,
		Chrome: *chrome, Curl: *curl, LNA: *lna}, stdout)
	if err != nil {
		fmt.Fprintf(stderr, "mockenv m1-check: %v\n", err)
		if errors.Is(err, context.Canceled) {
			return 1
		}
		return 2
	}
	for _, r := range results {
		if r.Status == statusFail {
			return 1
		}
	}
	return 0
}

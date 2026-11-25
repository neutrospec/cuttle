// Command cuttle is an explicit HTTP and HTTPS forward proxy with rule-based
// routing (design 0006 D3).
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

// Exit statuses (design 0006 D3).
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

const usage = `usage: cuttle <command> [flags]

commands:
  serve [--config PATH]                     run the proxy in the foreground
  init [--config PATH]                      write the starter configuration if none exists
  check [--config PATH]                     validate the configuration
  explain [--config PATH] [--remote] TARGET show how the configuration routes TARGET
                                            (host:port, http://URL, or https://URL)
  paths [--json]                            print resolved paths
  status [--json]                           show the running service's listeners,
                                            upstreams, interfaces, counters, and
                                            tunnels
  interfaces [--json] [--all] [--watch] [--config PATH]
                                            list the system interfaces and how they
                                            match the [[interface]] definitions
  reload                                    ask the running service to reload
                                            its configuration
  version                                   print the version

admin passwd arrives in a later version.
`

func main() {
	os.Exit(run(os.Args[1:], os.LookupEnv, os.Stdout, os.Stderr))
}

// env looks up environment variables; it is os.LookupEnv outside tests.
type env func(string) (string, bool)

func run(args []string, lookup env, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "serve":
		return cmdServe(rest, lookup, stdout, stderr)
	case "init":
		return cmdInit(rest, lookup, stdout, stderr)
	case "check":
		return cmdCheck(rest, lookup, stdout, stderr)
	case "explain":
		return cmdExplain(rest, lookup, stdout, stderr)
	case "paths":
		return cmdPaths(rest, lookup, stdout, stderr)
	case "status":
		return cmdStatus(rest, lookup, stdout, stderr)
	case "reload":
		return cmdReload(rest, lookup, stdout, stderr)
	case "interfaces":
		return cmdInterfaces(rest, lookup, stdout, stderr)
	case "version", "--version", "-version":
		if len(rest) != 0 {
			fmt.Fprint(stderr, usage)
			return exitUsage
		}
		fmt.Fprintf(stdout, "cuttle %s (%s)\n", version, runtime.Version())
		return exitOK
	case "admin":
		fmt.Fprintln(stderr, "cuttle: the admin UI and cuttle admin passwd arrive in a later version")
		return exitFailure
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return exitOK
	}
	fmt.Fprintf(stderr, "cuttle: unknown command %q\n\n%s", cmd, usage)
	return exitUsage
}

// newFlags returns a flag set that reports errors to stderr without
// exiting.
func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("cuttle "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// Command cuttle is an explicit HTTP and HTTPS forward proxy with rule-based
// routing (design 0006 D3).
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
)

// version is set at build time with -ldflags "-X main.version=...", as the
// Homebrew formula does. A plain go build leaves it at "dev", and init adds
// the VCS revision that the go command recorded, if any.
var version = "dev"

func init() {
	info, _ := debug.ReadBuildInfo() // nil when the binary has none
	version = buildVersion(version, info)
}

// buildVersion returns the version cuttle reports. A version set at link
// time is kept. The default "dev" becomes "dev+<revision>", the first 12
// hex digits of the recorded vcs.revision, with ".dirty" added when the
// checkout had uncommitted changes; without a recorded revision, such as
// in test binaries or a build from a source tarball, it stays "dev".
func buildVersion(v string, info *debug.BuildInfo) string {
	if v != "dev" || info == nil {
		return v
	}
	var rev string
	dirty := false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return v
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	v += "+" + rev
	if dirty {
		v += ".dirty"
	}
	return v
}

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
  admin passwd [--stdin]                    set the admin password; --stdin reads
                                            one line from standard input
  version                                   print the version
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
		return cmdAdmin(rest, lookup, os.Stdin, stdout, stderr)
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

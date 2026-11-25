package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/neutrospec/cuttle/internal/control"
	"github.com/neutrospec/cuttle/internal/paths"
)

// controlTimeout bounds a status or reload request over the control socket.
const controlTimeout = 30 * time.Second

func cmdStatus(args []string, lookup env, stdout, stderr io.Writer) int {
	fl := newFlags("status", stderr)
	asJSON := fl.Bool("json", false, "print JSON")
	if fl.Parse(args) != nil || fl.NArg() != 0 {
		return exitUsage
	}
	p, err := controlPaths(lookup)
	if err != nil {
		fmt.Fprintln(stderr, "cuttle:", err)
		return exitFailure
	}
	ctx, cancel := context.WithTimeout(context.Background(), controlTimeout)
	defer cancel()
	st, err := control.NewClient(p.Control.Path).Status(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "cuttle:", err)
		return exitFailure
	}
	warnPathMismatch(stderr, st.Paths, p)
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.Encode(st)
		return exitOK
	}
	printStatus(stdout, st, time.Now())
	return exitOK
}

// controlPaths resolves the paths and refuses a control socket path that no
// Unix socket can use (design 0006 D1).
func controlPaths(lookup env) (paths.Paths, error) {
	p, err := paths.Resolve(lookup)
	if err != nil {
		return p, err
	}
	return p, p.ControlError()
}

// warnPathMismatch warns about each path that the service resolved
// differently from this command, such as a log under another
// XDG_STATE_HOME (design 0006 D1).
func warnPathMismatch(stderr io.Writer, service, own paths.Paths) {
	theirs := map[string]paths.Item{}
	for _, it := range service.Items() {
		theirs[it.Name] = it.Item
	}
	for _, it := range own.Items() {
		if s := theirs[it.Name]; s.Path != it.Path {
			fmt.Fprintf(stderr, "cuttle: warning: the service's %s path is %s (%s), but this command resolves %s (%s)\n",
				it.Name, s.Path, s.Source, it.Path, it.Source)
		}
	}
}

func printStatus(w io.Writer, st *control.Status, now time.Time) {
	tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	fmt.Fprintf(tw, "name\t%s\n", st.Name)
	fmt.Fprintf(tw, "version\t%s (pid %d)\n", st.Version, st.PID)
	fmt.Fprintf(tw, "started\t%s (up %s)\n", st.StartedAt.Format(time.RFC3339), now.Sub(st.StartedAt).Round(time.Second))
	fmt.Fprintf(tw, "config\t%s\n", st.Config.Path)
	fmt.Fprintf(tw, "\tsha256 %s, loaded %s\n", st.Config.SHA256, st.Config.LoadedAt.Format(time.RFC3339))
	if len(st.Config.RestartRequired) > 0 {
		fmt.Fprintf(tw, "\trestart required: %s\n", strings.Join(st.Config.RestartRequired, ", "))
	}
	if st.Config.LastReloadError != "" {
		for _, l := range strings.Split(st.Config.LastReloadError, "\n") {
			fmt.Fprintf(tw, "\tlast reload error: %s\n", l)
		}
	}
	fmt.Fprintf(tw, "log\t%s (rotation failures %d)\n", st.Log.Path, st.Log.RotationFailures)
	if st.StatsErr != "" {
		fmt.Fprintf(tw, "stats\t%s: %s\n", st.Stats, st.StatsErr)
	} else {
		fmt.Fprintf(tw, "stats\t%s\n", st.Stats)
	}
	if st.StatsWriteError != "" && st.StatsWriteErrorAt != nil {
		fmt.Fprintf(tw, "\tlast write error at %s: %s\n", st.StatsWriteErrorAt.Format(time.RFC3339), st.StatsWriteError)
	}
	switch st.Admin.State {
	case control.AdminListening:
		fmt.Fprintf(tw, "admin\t%s listening since %s\n", st.Admin.Address, st.Admin.Since.Format(time.RFC3339))
	case control.AdminUnavailable:
		fmt.Fprintf(tw, "admin\t%s unavailable: %s\n", st.Admin.Address, st.Admin.Error)
	case control.AdminDisabled:
		fmt.Fprintln(tw, "admin\tdisabled")
	}
	fmt.Fprintf(tw, "open files\t%d\n", st.RlimitNofile)
	tw.Flush()

	fmt.Fprintln(w, "\nlisteners")
	tw = tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	for _, l := range st.Listeners {
		line := fmt.Sprintf("  %s\t%s\tsince %s", l.Address, l.State, l.Since.Format(time.RFC3339))
		if l.Error != "" {
			line += "\t" + l.Error
		}
		fmt.Fprintln(tw, line)
	}
	tw.Flush()

	if len(st.Upstreams) > 0 {
		fmt.Fprintln(w, "\nupstreams")
		tw = tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
		for _, u := range st.Upstreams {
			line := fmt.Sprintf("  %s\t%s\t%s\tsince %s", u.Name, u.Address, u.State, u.Since.Format(time.RFC3339))
			if u.State != "up" {
				line += fmt.Sprintf("\tdown for %s", time.Duration(u.DownS*float64(time.Second)).Round(time.Millisecond))
			}
			line += fmt.Sprintf("\tfailed fast %d", u.FailedFast)
			if u.LastError != "" {
				line += "\tlast error " + u.LastError
			}
			fmt.Fprintln(tw, line)
		}
		tw.Flush()
	}

	if len(st.Interfaces) > 0 {
		fmt.Fprintln(w, "\ninterfaces")
		tw = tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
		for _, i := range st.Interfaces {
			device := i.Device
			if device == "" {
				device = "-"
			}
			line := fmt.Sprintf("  %s\t%s\t%s", i.Name, device, i.State)
			if i.Reason != "" {
				line += " (" + i.Reason + ")"
			}
			line += fmt.Sprintf("\tsince %s", i.Since.Format(time.RFC3339))
			if len(i.Families) > 0 {
				line += "\tfamilies " + strings.Join(i.Families, ",")
			}
			if len(i.Primary) > 0 {
				line += "\tprimary " + strings.Join(i.Primary, ",")
			}
			if i.DemotedUntil != "" {
				line += "\tdemoted until " + i.DemotedUntil
			}
			fmt.Fprintln(tw, line)
		}
		tw.Flush()
	}

	fmt.Fprintln(w, "\ncounters")
	keys := make([]string, 0, len(st.Counters))
	for k := range st.Counters {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	tw = tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	for _, k := range keys {
		fmt.Fprintf(tw, "  %s\t%d\n", k, st.Counters[k])
	}
	tw.Flush()

	fmt.Fprintf(w, "\ntunnels (%d)\n", len(st.Tunnels))
	tw = tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	for _, t := range st.Tunnels {
		action := t.Action
		if t.Upstream != "" {
			action += ":" + t.Upstream
		}
		ifname := t.Interface
		if ifname == "" {
			ifname = "-"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\tinterface %s\tage %ds\tup %d\tdown %d\n", t.Target, t.Kind, action, t.Source, ifname, t.AgeS, t.Up, t.Down)
	}
	tw.Flush()

	fmt.Fprintln(w, "\npaths")
	tw = tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	for _, it := range st.Paths.Items() {
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", it.Name, it.Path, it.Source)
	}
	tw.Flush()
}

func cmdReload(args []string, lookup env, stdout, stderr io.Writer) int {
	fl := newFlags("reload", stderr)
	if fl.Parse(args) != nil || fl.NArg() != 0 {
		return exitUsage
	}
	p, err := controlPaths(lookup)
	if err != nil {
		fmt.Fprintln(stderr, "cuttle:", err)
		return exitFailure
	}
	ctx, cancel := context.WithTimeout(context.Background(), controlTimeout)
	defer cancel()
	res, ok, err := control.NewClient(p.Control.Path).Reload(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "cuttle:", err)
		return exitFailure
	}
	if !ok {
		for _, e := range res.Errors {
			fmt.Fprintln(stderr, "error:", e)
		}
		fmt.Fprintf(stderr, "cuttle: reload refused with %d error(s); the running configuration is unchanged\n", len(res.Errors))
		return exitFailure
	}
	for _, w := range res.Warnings {
		fmt.Fprintln(stdout, "warning:", w)
	}
	if len(res.RestartRequired) > 0 {
		fmt.Fprintf(stdout, "restart required: %s\n", strings.Join(res.RestartRequired, ", "))
	}
	fmt.Fprintln(stdout, "reloaded")
	return exitOK
}

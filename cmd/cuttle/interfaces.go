package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"squid/internal/config"
	"squid/internal/iface"
)

// interfaceSource is the interface table that cuttle interfaces reads; tests
// replace it.
var interfaceSource iface.Source = iface.System{}

// cmdInterfaces lists the system interfaces and how they match the
// configuration's definitions, reading the system directly without a
// running service (design 0008 D15). It never runs another program.
func cmdInterfaces(args []string, lookup env, stdout, stderr io.Writer) int {
	fl := newFlags("interfaces", stderr)
	asJSON := fl.Bool("json", false, "print JSON")
	all := fl.Bool("all", false, "list every interface, lo0 and utun included")
	watch := fl.Bool("watch", false, "print a line for each change until interrupted")
	cfgFlag := fl.String("config", "", "configuration file `PATH`")
	if fl.Parse(args) != nil || fl.NArg() != 0 {
		return exitUsage
	}
	if *asJSON && *watch {
		fmt.Fprintln(stderr, "cuttle: --json and --watch cannot be combined")
		return exitUsage
	}
	defs := interfaceDefs(*cfgFlag, lookup, stderr)
	ctx := context.Background()
	if *watch {
		var stop context.CancelFunc
		ctx, stop = signal.NotifyContext(ctx, os.Interrupt)
		defer stop()
	}
	return listInterfaces(ctx, interfaceSource, iface.RealTimers{}, time.Now, defs, *all, *asJSON, *watch, stdout, stderr)
}

// listInterfaces prints the table and, with watch, a line per change until
// ctx ends. With watch it opens the notice sockets before the first read,
// so that no change falls between them (design 0008 D5).
func listInterfaces(ctx context.Context, src iface.Source, timers iface.Timers, now func() time.Time, defs []iface.Def,
	all, asJSON, watch bool, stdout, stderr io.Writer) int {
	var notices <-chan struct{}
	if watch {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		notices = src.Watch(ctx)
	}
	links, err := src.Read()
	if err != nil {
		fmt.Fprintln(stderr, "cuttle: cannot read the interface table:", err)
		return exitFailure
	}
	rows := interfaceRows(links, defs, all)
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.Encode(rows)
		return exitOK
	}
	printInterfaces(stdout, rows)
	if watch {
		watchInterfaces(ctx, notices, src, timers, now, links, all, stdout, stderr)
	}
	return exitOK
}

// interfaceDefs loads the definitions from the configuration, if it can be
// read; without one, match, state, and reason stay empty. A file named
// with --config that cannot be read is reported.
func interfaceDefs(cfgFlag string, lookup env, stderr io.Writer) []iface.Def {
	path, err := configPath(cfgFlag, lookup)
	if err != nil {
		return nil
	}
	cfg, _, err := config.Load(path)
	if err != nil {
		if cfgFlag != "" || !errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(stderr, "cuttle: %s is not readable or not valid; match, state, and reason are left empty\n", path)
		}
		return nil
	}
	return cfg.Defs()
}

// interfaceRow is one row of cuttle interfaces (design 0008 D15).
type interfaceRow struct {
	Index     int                 `json:"index"`
	Device    string              `json:"device"`
	Kind      string              `json:"kind"`
	MAC       string              `json:"mac"`
	Media     string              `json:"media"`
	Addresses map[string][]string `json:"addresses"`
	Default   map[string]string   `json:"default"`
	Match     string              `json:"match"`
	State     string              `json:"state"`
	Reason    string              `json:"reason"`
}

// interfaceRows builds the rows: by default the interfaces with a six-byte
// MAC, with all every interface.
func interfaceRows(links []iface.Link, defs []iface.Def, all bool) []interfaceRow {
	rows := []interfaceRow{}
	for _, l := range links {
		if !all && len(l.MAC) != 6 {
			continue
		}
		r := interfaceRow{
			Index:  l.Index,
			Device: l.Device,
			Kind:   l.Kind.String(),
			Media:  l.Media.String(),
			Addresses: map[string][]string{
				"ipv4": addrStrings(l.UsableAddrs(iface.IPv4)),
				"ipv6": addrStrings(l.UsableAddrs(iface.IPv6)),
			},
			Default: map[string]string{"ipv4": l.Default4.String(), "ipv6": l.Default6.String()},
		}
		if len(l.MAC) > 0 {
			r.MAC = l.MAC.String()
		}
		var names, states, reasons []string
		for _, d := range defs {
			if !d.Matches(l) {
				continue
			}
			m := iface.Evaluate(d, links, true)
			names = append(names, d.Name)
			if m.Online {
				states = append(states, iface.StateOnline)
				reasons = append(reasons, "")
			} else {
				states = append(states, iface.StateOffline)
				reasons = append(reasons, m.Reason)
			}
		}
		r.Match, r.State, r.Reason = strings.Join(names, ","), strings.Join(states, ","), strings.Join(reasons, ",")
		rows = append(rows, r)
	}
	return rows
}

func addrStrings[T fmt.Stringer](list []T) []string {
	out := make([]string, len(list))
	for i, a := range list {
		out[i] = a.String()
	}
	return out
}

func firstOr(list []string) string {
	if len(list) == 0 {
		return "-"
	}
	return list[0]
}

// printInterfaces prints the text columns of design 0008 D15.
func printInterfaces(w io.Writer, rows []interfaceRow) {
	tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "index\tdevice\tkind\tmac\tmedia\tipv4\tipv6\tdefault4\tdefault6\tmatch")
	for _, r := range rows {
		mac := r.MAC
		if mac == "" {
			mac = "-"
		}
		match := "-"
		if r.Match != "" {
			match = r.Match
			if r.State != iface.StateOnline {
				match += " (" + r.Reason + ")"
			}
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Index, r.Device, r.Kind, mac, r.Media,
			firstOr(r.Addresses["ipv4"]), firstOr(r.Addresses["ipv6"]), r.Default["ipv4"], r.Default["ipv6"], match)
	}
	tw.Flush()
}

// watchInterfaces re-reads the table as the monitor of design 0008 D5
// does, Debounce after the first notice since the last re-read and every
// SafetyInterval, and prints one line per change: a timestamp, the device,
// and each changed field. It does not decode notices. As in the monitor,
// the debounce ends before its read starts, so a notice that arrives
// during a read schedules the next one. It returns when the notice channel
// closes after ctx ends.
func watchInterfaces(ctx context.Context, notices <-chan struct{}, src iface.Source, timers iface.Timers, now func() time.Time,
	prev []iface.Link, all bool, stdout, stderr io.Writer) {
	var mu sync.Mutex     // guards pending
	var readMu sync.Mutex // serializes re-reads and guards prev
	pending := false
	reread := func() {
		readMu.Lock()
		defer readMu.Unlock()
		links, err := src.Read()
		if err != nil {
			fmt.Fprintln(stderr, "cuttle: cannot read the interface table:", err)
			return
		}
		ts := now().UTC().Format("2006-01-02T15:04:05.000Z")
		for _, line := range tableChanges(prev, links, all) {
			fmt.Fprintln(stdout, ts, line)
		}
		prev = links
	}
	var safety func()
	safety = func() {
		if ctx.Err() != nil {
			return
		}
		reread()
		timers.AfterFunc(iface.SafetyInterval, safety)
	}
	stopSafety := timers.AfterFunc(iface.SafetyInterval, safety)
	defer stopSafety()
	for range notices {
		mu.Lock()
		if !pending {
			pending = true
			timers.AfterFunc(iface.Debounce, func() {
				mu.Lock()
				pending = false
				mu.Unlock()
				if ctx.Err() == nil {
					reread()
				}
			})
		}
		mu.Unlock()
	}
}

// tableChanges lists the changes between two reads, one line per device:
// "en7 media active->inactive ipv4 192.0.2.20->-", "en9 attached", or
// "en9 detached".
func tableChanges(prev, next []iface.Link, all bool) []string {
	shown := func(l iface.Link) bool { return all || len(l.MAC) == 6 }
	before := map[string]iface.Link{}
	for _, l := range prev {
		if shown(l) {
			before[l.Device] = l
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, l := range next {
		if !shown(l) {
			continue
		}
		seen[l.Device] = true
		old, ok := before[l.Device]
		if !ok {
			out = append(out, l.Device+" attached")
			continue
		}
		var fields []string
		diff := func(name, a, b string) {
			if a != b {
				fields = append(fields, name+" "+a+"->"+b)
			}
		}
		diff("index", strconv.Itoa(old.Index), strconv.Itoa(l.Index))
		diff("mac", macString(old), macString(l))
		diff("up", strconv.FormatBool(old.Up), strconv.FormatBool(l.Up))
		diff("kind", old.Kind.String(), l.Kind.String())
		diff("media", old.Media.String(), l.Media.String())
		diff("ipv4", addrList(old, iface.IPv4), addrList(l, iface.IPv4))
		diff("ipv6", addrList(old, iface.IPv6), addrList(l, iface.IPv6))
		diff("default4", old.Default4.String(), l.Default4.String())
		diff("default6", old.Default6.String(), l.Default6.String())
		if len(fields) > 0 {
			out = append(out, l.Device+" "+strings.Join(fields, " "))
		}
	}
	var gone []string
	for d := range before {
		if !seen[d] {
			gone = append(gone, d+" detached")
		}
	}
	slices.Sort(gone)
	return append(out, gone...)
}

// addrList joins the usable addresses of a family, or gives "-".
func addrList(l iface.Link, f iface.Families) string {
	list := addrStrings(l.UsableAddrs(f))
	if len(list) == 0 {
		return "-"
	}
	return strings.Join(list, ",")
}

func macString(l iface.Link) string {
	if len(l.MAC) == 0 {
		return "-"
	}
	return l.MAC.String()
}

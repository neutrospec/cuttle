package stats

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Window presets of the dashboard (design 0005 D9) and the hours each
// covers. A preset covers whole hourly buckets, the current one included.
var presetHours = map[string]int64{"1h": 1, "24h": 24, "7d": 7 * 24, "30d": 30 * 24}

// Presets lists the window presets in display order.
var Presets = []string{"1h", "24h", "7d", "30d"}

// DefaultPreset is the window the dashboard shows without a choice.
const DefaultPreset = "24h"

// DateLayout is the form of the from and to dates of a date range.
const DateLayout = "2006-01-02"

// Window is a range of hourly buckets: rows with From <= bucket < To.
type Window struct {
	// Preset is the preset that chose the window, or "" for a date range.
	Preset string
	// FromDate and ToDate are the inclusive local dates of a date range.
	FromDate, ToDate string
	From, To         int64
}

// Start and End return the window's bounds as times.
func (w Window) Start() time.Time { return time.Unix(w.From, 0) }
func (w Window) End() time.Time   { return time.Unix(w.To, 0) }

// PresetWindow returns the window of a preset at now: the current hour and
// the hours before it, as many as the preset covers.
func PresetWindow(preset string, now time.Time) (Window, error) {
	h, ok := presetHours[preset]
	if !ok {
		return Window{}, fmt.Errorf("unknown window %q; use 1h, 24h, 7d, or 30d", preset)
	}
	to := Bucket(now) + 3600
	return Window{Preset: preset, From: to - h*3600, To: to}, nil
}

// DateWindow returns the window of the local calendar dates from and to,
// both included, in loc. It holds the hourly buckets that start within
// those dates, limited to the retention period and the current hour; a
// range wholly outside them is an error (design 0005 D9).
func DateWindow(from, to string, now time.Time, loc *time.Location, retentionDays int) (Window, error) {
	f, err := time.ParseInLocation(DateLayout, from, loc)
	if err != nil {
		return Window{}, fmt.Errorf("from %q is not a date such as 2026-11-25", from)
	}
	t, err := time.ParseInLocation(DateLayout, to, loc)
	if err != nil {
		return Window{}, fmt.Errorf("to %q is not a date such as 2026-11-25", to)
	}
	if t.Before(f) {
		return Window{}, errors.New("to is before from")
	}
	end := time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc)
	w := Window{FromDate: from, ToDate: to, From: ceilHour(f.Unix()), To: ceilHour(end.Unix())}
	oldest := Bucket(now) - int64(retentionDays)*86400
	w.From = max(w.From, oldest)
	w.To = min(w.To, Bucket(now)+3600)
	if w.From >= w.To {
		return Window{}, fmt.Errorf("the dates lie outside the %d days that statistics keep", retentionDays)
	}
	return w, nil
}

// ceilHour rounds Unix seconds up to the next hour boundary.
func ceilHour(s int64) int64 {
	if r := s % 3600; r != 0 {
		return s - r + 3600
	}
	return s
}

var filterNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// Filter selects the rows of a query (design 0005 D9).
type Filter struct {
	Window
	// Route is all, direct, block, upstream for every upstream, or
	// upstream:<name>; "" means all.
	Route string
	// Source is all, local, or remote; "" means all.
	Source string
	// Interface is all, none for rows without an interface, other, or a
	// configured interface name; "" means all.
	Interface string
}

// Check reports a value the filter does not accept.
func (f Filter) Check() error {
	switch r := f.Route; {
	case r == "", r == "all", r == "direct", r == "block", r == "upstream":
	case strings.HasPrefix(r, "upstream:") && filterNameRe.MatchString(strings.TrimPrefix(r, "upstream:")):
	default:
		return fmt.Errorf("unknown route %q; use all, direct, block, upstream, or upstream:<name>", r)
	}
	switch f.Source {
	case "", "all", "local", "remote":
	default:
		return fmt.Errorf("unknown source %q; use all, local, or remote", f.Source)
	}
	if i := f.Interface; i != "" && i != "all" && i != "none" && !filterNameRe.MatchString(i) {
		return fmt.Errorf("unknown interface %q; use all, none, other, or an interface name", i)
	}
	if f.From >= f.To {
		return errors.New("empty window")
	}
	return nil
}

// where returns the WHERE clause of the filter and its arguments.
func (f Filter) where() (string, []any) {
	var b strings.Builder
	b.WriteString("bucket >= ? AND bucket < ?")
	args := []any{f.From, f.To}
	switch r := f.Route; {
	case r == "direct", r == "block", r == "upstream":
		b.WriteString(" AND action = ?")
		args = append(args, r)
	case strings.HasPrefix(r, "upstream:"):
		b.WriteString(" AND action = 'upstream' AND upstream = ?")
		args = append(args, strings.TrimPrefix(r, "upstream:"))
	}
	if f.Source == "local" || f.Source == "remote" {
		b.WriteString(" AND source = ?")
		args = append(args, f.Source)
	}
	switch i := f.Interface; i {
	case "", "all":
	case "none":
		b.WriteString(" AND interface = ''")
	default:
		b.WriteString(" AND interface = ?")
		args = append(args, i)
	}
	return b.String(), args
}

// RouteOf returns the route label of an action and upstream: direct,
// block, or upstream:<name>.
func RouteOf(action, upstream string) string {
	if action == "upstream" {
		return "upstream:" + upstream
	}
	return action
}

// HostRow is one line of the domain table: a host and one route it took
// in the window (design 0005 D9).
type HostRow struct {
	Host     string
	Action   string
	Upstream string
	// Rules lists the rules that chose the route, sorted.
	Rules     []string
	Requests  int64 // connects plus http_requests
	Failures  int64
	BytesUp   int64
	BytesDown int64
	FirstSeen int64
	LastSeen  int64
}

// Route returns the row's route label.
func (r HostRow) Route() string { return RouteOf(r.Action, r.Upstream) }

// Bytes returns the bytes in both directions.
func (r HostRow) Bytes() int64 { return r.BytesUp + r.BytesDown }

// MaxDomainRows is the page size of the domain table (design 0005 D2).
const MaxDomainRows = 500

// Domains returns the hosts of the filter, one row per host and route,
// ordered by bytes in both directions, then requests, then host, at most
// limit rows from offset.
func (s *Store) Domains(ctx context.Context, f Filter, limit, offset int) ([]HostRow, error) {
	if err := f.Check(); err != nil {
		return nil, err
	}
	where, args := f.where()
	q := `SELECT host, action, upstream, group_concat(DISTINCT rule),
  sum(connects) + sum(http_requests), sum(failures), sum(bytes_up), sum(bytes_down),
  min(first_seen), max(last_seen)
FROM host_hour
WHERE ` + where + `
GROUP BY host, action, upstream
ORDER BY sum(bytes_up) + sum(bytes_down) DESC, sum(connects) + sum(http_requests) DESC, host, action, upstream
LIMIT ? OFFSET ?`
	rs, err := s.reader.QueryContext(ctx, q, append(args, limit, offset)...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := []HostRow{}
	for rs.Next() {
		var r HostRow
		var rules string
		if err := rs.Scan(&r.Host, &r.Action, &r.Upstream, &rules, &r.Requests, &r.Failures,
			&r.BytesUp, &r.BytesDown, &r.FirstSeen, &r.LastSeen); err != nil {
			return nil, err
		}
		r.Rules = strings.Split(rules, ",")
		slices.Sort(r.Rules)
		out = append(out, r)
	}
	return out, rs.Err()
}

// Total sums the rows of one route or one interface (design 0005 D9).
type Total struct {
	// Action and Upstream are set for a route total; Interface for an
	// interface total, "" for rows without an interface.
	Action, Upstream, Interface string
	Hosts                       int64 // distinct hosts, * included
	Requests                    int64
	Failures                    int64
	BytesUp                     int64
	BytesDown                   int64
}

// Route returns a route total's label.
func (t Total) Route() string { return RouteOf(t.Action, t.Upstream) }

// Bytes returns the bytes in both directions.
func (t Total) Bytes() int64 { return t.BytesUp + t.BytesDown }

// RouteTotals sums the filter's rows per route, in the order direct,
// upstreams by name, block.
func (s *Store) RouteTotals(ctx context.Context, f Filter) ([]Total, error) {
	return s.totals(ctx, f, "action, upstream",
		"CASE action WHEN 'direct' THEN 0 WHEN 'upstream' THEN 1 ELSE 2 END, upstream",
		func(t *Total) []any { return []any{&t.Action, &t.Upstream} })
}

// InterfaceTotals sums the filter's rows per interface, ordered by name
// with rows without an interface last.
func (s *Store) InterfaceTotals(ctx context.Context, f Filter) ([]Total, error) {
	return s.totals(ctx, f, "interface", "interface = '', interface",
		func(t *Total) []any { return []any{&t.Interface} })
}

func (s *Store) totals(ctx context.Context, f Filter, group, order string, keys func(*Total) []any) ([]Total, error) {
	if err := f.Check(); err != nil {
		return nil, err
	}
	where, args := f.where()
	q := `SELECT ` + group + `, count(DISTINCT host), sum(connects) + sum(http_requests), sum(failures),
  sum(bytes_up), sum(bytes_down)
FROM host_hour WHERE ` + where + ` GROUP BY ` + group + ` ORDER BY ` + order
	rs, err := s.reader.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := []Total{}
	for rs.Next() {
		var t Total
		if err := rs.Scan(append(keys(&t), &t.Hosts, &t.Requests, &t.Failures, &t.BytesUp, &t.BytesDown)...); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rs.Err()
}

// Domains, RouteTotals, and InterfaceTotals query the written rows over a
// read-only connection, so a dashboard query never waits for a flush.
func (s *Stats) Domains(ctx context.Context, f Filter, limit, offset int) ([]HostRow, error) {
	return s.store.Domains(ctx, f, limit, offset)
}

func (s *Stats) RouteTotals(ctx context.Context, f Filter) ([]Total, error) {
	return s.store.RouteTotals(ctx, f)
}

func (s *Stats) InterfaceTotals(ctx context.Context, f Filter) ([]Total, error) {
	return s.store.InterfaceTotals(ctx, f)
}

package admin

import (
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/neutrospec/cuttle/internal/control"
	"github.com/neutrospec/cuttle/internal/stats"
)

// The overview of design 0009 D3.1 (task E): one sentence that answers
// where traffic went, the stacked route bar with its legend, the top
// hosts of each route with Route…, compact upstream and interface
// states, and a configuration summary.

// heroSeg is a part of the route sentence; a part with a Class names a
// route and carries its color swatch.
type heroSeg struct {
	Text, Class, Shade string
}

// heroBar is one part of the stacked bar or its legend.
type heroBar struct {
	// Class is the route's action; Shade tells the second and later
	// upstreams apart in the one upstream hue (design 0009 D11).
	Name, Class, Shade string
	// Grow is the stylesheet class of the part's width, g1 to g100; the
	// policy refuses inline styles (design 0005 D5).
	Grow  string
	Bytes int64
	Share string
}

// routeHero is the sentence, the bar, and the legend.
type routeHero struct {
	Sentence []heroSeg
	Bar      []heroBar
	// NoBytes are the routes other than block whose requests moved no
	// bytes, such as requests that all failed: the legend counts their
	// requests, and a sentence of their own names them (OI-0006 AD-R1).
	NoBytes []heroReqs
	// BarLabel reads the bar for a screen reader.
	BarLabel string
	// Blocked counts the requests of the block route, which moves no
	// bytes and so has no part of the bar.
	Blocked int64
}

// heroReqs is a legend entry counted in requests.
type heroReqs struct {
	Name, Class, Shade string
	Requests           int64
}

// windowPhrase starts the sentence with the window: "In the last 24
// hours" or "From 2026-11-01 to 2026-11-03".
func windowPhrase(w windowView) string {
	switch w.Preset {
	case "1h":
		return "In the last hour"
	case "24h":
		return "In the last 24 hours"
	case "7d":
		return "In the last 7 days"
	case "30d":
		return "In the last 30 days"
	}
	if w.FromDate == w.ToDate {
		return "On " + w.FromDate
	}
	return "From " + w.FromDate + " to " + w.ToDate
}

// shares splits 100 % over values in whole percents by the largest
// remainder, so that the shares add up to 100.
func shares(values []int64) []int {
	var total int64
	for _, v := range values {
		total += v
	}
	out := make([]int, len(values))
	if total <= 0 {
		return out
	}
	type rem struct {
		i int
		r int64
	}
	var rems []rem
	left := 100
	for i, v := range values {
		out[i] = int(v * 100 / total)
		left -= out[i]
		rems = append(rems, rem{i, v * 100 % total})
	}
	slices.SortStableFunc(rems, func(a, b rem) int {
		switch {
		case a.r > b.r:
			return -1
		case a.r < b.r:
			return 1
		}
		return 0
	})
	for k := 0; k < left; k++ {
		out[rems[k].i]++
	}
	return out
}

// joinItems joins the parts of a sentence: "a", "a and b", or "a, b,
// and c".
func joinItems(items [][]heroSeg) []heroSeg {
	var out []heroSeg
	for i, it := range items {
		switch {
		case i == 0:
		case len(items) == 2:
			out = append(out, heroSeg{Text: " and "})
		case i == len(items)-1:
			out = append(out, heroSeg{Text: ", and "})
		default:
			out = append(out, heroSeg{Text: ", "})
		}
		out = append(out, it...)
	}
	return out
}

// newRouteHero builds the sentence, bar, and legend from the route
// totals, which come as direct, the upstreams by name, and block (design
// 0005 D9); nil only when the window has no route at all, which the page
// shows as the empty state.
func newRouteHero(w windowView, routes []stats.Total) *routeHero {
	h := &routeHero{}
	var moved, noBytes []stats.Total
	for _, t := range routes {
		switch {
		case t.Action == "block":
			h.Blocked += t.Requests
		case t.Bytes() > 0:
			moved = append(moved, t)
		case t.Requests > 0:
			noBytes = append(noBytes, t)
		}
	}
	if len(moved) == 0 && len(noBytes) == 0 && h.Blocked == 0 {
		return nil
	}
	values := make([]int64, len(moved))
	for i, t := range moved {
		values[i] = t.Bytes()
	}
	pct := shares(values)
	// Rounding gives 100 % to the largest route when the others are
	// below half a percent; they show "<1%", so it shows 99 % (OI-0006
	// AD-R2).
	if len(moved) > 1 {
		for i := range pct {
			pct[i] = min(pct[i], 99)
		}
	}
	ups := 0
	// name returns a route's name and the shade that tells the second
	// and later upstreams apart.
	name := func(t stats.Total) (string, string) {
		if t.Action != "upstream" {
			return t.Action, ""
		}
		ups++
		if ups > 1 {
			return t.Upstream, "shade" + strconv.Itoa(min(ups, 3))
		}
		return t.Upstream, ""
	}
	var items [][]heroSeg
	var labels []string
	for i, t := range moved {
		bar := heroBar{Class: t.Action, Bytes: t.Bytes(), Grow: "g" + strconv.Itoa(max(pct[i], 1))}
		bar.Name, bar.Shade = name(t)
		bar.Share = strconv.Itoa(pct[i]) + "%"
		if pct[i] == 0 {
			bar.Share = "<1%"
		}
		h.Bar = append(h.Bar, bar)
		labels = append(labels, bar.Name+" "+bar.Share)
		lead := bar.Share + " "
		switch {
		case len(moved) == 1:
			lead = "all traffic went "
		case i == 0:
			lead += "of traffic went "
		}
		if t.Action == "upstream" {
			lead += "through "
		}
		items = append(items, []heroSeg{{Text: lead}, {Text: bar.Name, Class: bar.Class, Shade: bar.Shade}})
	}
	if len(moved) == 0 {
		items = append(items, []heroSeg{{Text: "no traffic went out"}})
	}
	if h.Blocked > 0 {
		verb := " requests were "
		if h.Blocked == 1 {
			verb = " request was "
		}
		items = append(items, []heroSeg{{Text: formatNum(h.Blocked) + verb}, {Text: "blocked", Class: "block"}})
	}
	h.Sentence = append([]heroSeg{{Text: windowPhrase(w) + ", "}}, joinItems(items)...)
	h.Sentence = append(h.Sentence, heroSeg{Text: "."})
	// The routes whose requests moved no bytes get a sentence of their
	// own: "5 requests went direct and 3 through gw but moved no bytes."
	items = nil
	for i, t := range noBytes {
		r := heroReqs{Class: t.Action, Requests: t.Requests}
		r.Name, r.Shade = name(t)
		h.NoBytes = append(h.NoBytes, r)
		lead := formatNum(t.Requests) + " "
		if i == 0 {
			lead += "requests went "
			if t.Requests == 1 {
				lead = "1 request went "
			}
		}
		if t.Action == "upstream" {
			lead += "through "
		}
		items = append(items, []heroSeg{{Text: lead}, {Text: r.Name, Class: r.Class, Shade: r.Shade}})
	}
	if len(items) > 0 {
		h.Sentence = append(h.Sentence, heroSeg{Text: " "})
		h.Sentence = append(h.Sentence, joinItems(items)...)
		h.Sentence = append(h.Sentence, heroSeg{Text: " but moved no bytes."})
	}
	if len(labels) > 0 {
		h.BarLabel = "Bytes by route: " + strings.Join(labels, ", ")
	}
	return h
}

// stateRow is one compact row of the overview's upstream or interface
// states: the name with a link to its page, and the state in words.
type stateRow struct {
	Name, Href, State, Text string
}

// confSummary is the overview's configuration summary (design 0009
// D3.1): the counts with links, when the file was last saved, and the
// keys that wait for a restart.
type confSummary struct {
	Rules, Upstreams, Interfaces int
	Saved                        string
	Restart                      []keyLink
}

type keyLink struct{ Key, Href string }

// keyPage returns the settings page of a key path.
func keyPage(key string) string {
	switch {
	case strings.HasPrefix(key, "proxy.remote."):
		return "/remote"
	case strings.HasPrefix(key, "proxy."):
		return "/proxy"
	}
	return "/general"
}

// fillStates fills the compact rows and the configuration summary.
func (s *Server) fillStates(d *overview, st control.Status) {
	states := s.upstreamStates(st)
	for _, u := range st.Upstreams {
		us := states[u.Name]
		d.UpRows = append(d.UpRows, stateRow{Name: u.Name, Href: "/upstreams/" + u.Name, State: us.State, Text: us.Text})
	}
	for _, i := range st.Interfaces {
		d.IfRows = append(d.IfRows, stateRow{Name: i.Name, Href: "/interfaces/" + i.Name, State: i.State, Text: stateText(i, s.loc)})
	}
	cs := s.confState()
	d.Conf = confSummary{Rules: len(cs.doc.Rules), Upstreams: len(cs.doc.Upstreams), Interfaces: len(cs.doc.Interfaces)}
	if fi, err := os.Stat(s.backend.ConfigPath()); err == nil {
		d.Conf.Saved = savedWhen(fi.ModTime(), s.now(), s.loc)
	}
	for _, k := range st.Config.RestartRequired {
		d.Conf.Restart = append(d.Conf.Restart, keyLink{Key: k, Href: keyPage(k)})
	}
}

// savedWhen says when the file was saved: "Today at 09:41" or
// "2026-11-24 at 09:41".
func savedWhen(t, now time.Time, loc *time.Location) string {
	t, now = t.In(loc), now.In(loc)
	if y, m, d := t.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return "Today at " + t.Format("15:04")
	}
	return t.Format("2006-01-02") + " at " + t.Format("15:04")
}

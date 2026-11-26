package admin

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/neutrospec/cuttle/internal/config"
	"github.com/neutrospec/cuttle/internal/control"
)

// The field components of the settings pages and editors of design 0009
// D3.6–D3.8 (task D): a field as the templates in fields.html draw it,
// the action control, the interface list editor with its page-free
// fallback, and the running configuration as a model for the read-only
// state of design 0009 D3.13.

// fieldInfo is how a page presents one field beyond its label: the hint
// under it, the default as placeholder, the Restart badge, and the input
// style. fieldInfos holds them by "<op>/<field>".
type fieldInfo struct {
	Hint, Placeholder string
	// Restart marks a key that applies at the next start (design 0009
	// D3.7).
	Restart bool
	// Mono sets the value in the monospaced face, for values the operator
	// types or copies.
	Mono bool
	// Numeric asks phones for the number keyboard.
	Numeric bool
}

var fieldInfos = map[string]fieldInfo{}

// fieldView is one field as a template draws it.
type fieldView struct {
	Name, ID, Label, Value string
	fieldInfo
	Errors, Warnings []string
	Invalid          bool
	// Bad lists the 1-based entries of a list input with errors.
	Bad string
	// Checked is a checkbox's state.
	Checked bool
	// Disabled renders the control disabled, as in the read-only state.
	Disabled bool
	// Options are the choices of an action control.
	Options []optionView
}

// optionView is one option of an action control or a choice.
type optionView struct {
	Value, Label, Class string
	Checked, Disabled   bool
}

// HintID, MsgID, and Described name the hint, the issues, and the
// aria-describedby list of the field; an element that the page does not
// draw is left out of the list.
func (v fieldView) HintID() string { return v.ID + "-h" }
func (v fieldView) MsgID() string  { return v.ID + "-e" }
func (v fieldView) HasMsgs() bool  { return len(v.Errors)+len(v.Warnings) > 0 }
func (v fieldView) Described() string {
	var ids []string
	if v.Hint != "" {
		ids = append(ids, v.HintID())
	}
	if v.HasMsgs() {
		ids = append(ids, v.MsgID())
	}
	return strings.Join(ids, " ")
}

// Field returns the view of a field of the form: its value, its issues,
// and how the page presents it.
func (f *form) Field(name string) fieldView {
	fd, ok := f.op.field(name)
	if !ok {
		panic("admin: op " + f.Op + " has no field " + name)
	}
	v := fieldView{Name: name, ID: "f-" + name, Label: fd.Label, Value: f.Value(name), fieldInfo: fieldInfos[f.Op+"/"+name],
		Errors: f.Errors(name), Warnings: f.Warnings(name), Invalid: f.Invalid(name), Bad: f.Bad(name), Checked: f.Checked(name)}
	return v
}

// Action returns the action control of a field (design 0009 D3.4):
// Direct, each upstream, and Block, with the posted value checked. A value
// that names no option, such as an upstream that is gone, gets its own
// option, so the control always shows what the form holds. noDirect
// disables Direct unless it is the value.
func (f *form) Action(name string, upstreams []string, noDirect bool) fieldView {
	v := f.Field(name)
	cur := v.Value
	add := func(value, label, class string) {
		v.Options = append(v.Options, optionView{Value: value, Label: label, Class: class, Checked: value == cur,
			Disabled: value == "direct" && noDirect && cur != "direct"})
	}
	add("direct", "Direct", "direct")
	seen := cur == "direct" || cur == "block"
	for _, u := range upstreams {
		add("upstream:"+u, u, "upstream")
		seen = seen || cur == "upstream:"+u
	}
	if !seen && strings.HasPrefix(cur, "upstream:") {
		add(cur, strings.TrimPrefix(cur, "upstream:"), "upstream")
	}
	add("block", "Block", "block")
	return v
}

// Is reports whether an option of a choice is the posted one.
func (f *form) Is(name, value string) bool { return f.Value(name) == value }

// ifState is one interface definition with its running state, as the
// interface list editor and the interfaces page show it.
type ifState struct {
	Name string
	// State is online, demoted, offline, or unknown; Text says it in
	// words, such as "Offline: absent".
	State, Text string
	Device      string
}

// ifStates returns the state of each definition of d from the running
// instance's status (design 0008 D15); a definition that the running
// configuration does not have yet is unknown.
func (s *Server) ifStates(d *config.Doc) []ifState {
	st := s.backend.Status()
	byName := map[string]control.Interface{}
	for _, i := range st.Interfaces {
		byName[i.Name] = i
	}
	out := make([]ifState, 0, len(d.Interfaces))
	for _, x := range d.Interfaces {
		is := ifState{Name: x.Name, State: "unknown", Text: "Not loaded yet"}
		if c, ok := byName[x.Name]; ok {
			is.Device = c.Device
			is.State, is.Text = c.State, stateText(c, s.loc)
		}
		out = append(out, is)
	}
	return out
}

// stateText says an interface's state in words: "Online", "Demoted until
// 10:42", or "Offline: absent".
func stateText(c control.Interface, loc *time.Location) string {
	switch c.State {
	case "online":
		return "Online"
	case "demoted":
		if t, err := time.Parse(time.RFC3339, c.DemotedUntil); err == nil {
			return "Demoted until " + t.In(loc).Format("15:04:05")
		}
		return "Demoted"
	case "offline":
		if c.Reason != "" {
			return "Offline: " + strings.ReplaceAll(c.Reason, "_", " ")
		}
		return "Offline"
	}
	return c.State
}

// ifListView is the interface list editor of design 0009 D3.8: the chosen
// interfaces in priority order, each with its state and Move up, Move
// down, and Remove buttons, and an Add select of the remaining
// definitions. The entries are hidden inputs named like the field, so the
// form posts them in order. Without the script each button submits the
// form for editing with the change in the query (see listAction); the
// script makes the same changes in place.
type ifListView struct {
	fieldView
	// Choice is the choice that controls the list, such as "Use the
	// default list" / "Only these interfaces"; nil when the list itself
	// is the value, as proxy.interfaces is.
	Choice *fieldView
	// SetOption is the choice's option that uses the list.
	SetOption string
	Entries   []ifEntry
	Available []ifState
	// Defined is false without definitions; the editor then links to the
	// Interfaces page instead.
	Defined bool
	// Empty says what an empty list means.
	Empty string
}

// ifEntry is one entry of the list.
type ifEntry struct {
	ifState
	Pos         int
	First, Last bool
	// Up, Down, and Remove are the formaction URLs of the buttons
	// without the script.
	Up, Down, Remove string
}

// IfList returns the interface list editor of a list field. choice names
// the choice that controls it and set the option that uses the list, or
// both are "".
func (f *form) IfList(name, choice, set string, states []ifState, empty string) *ifListView {
	v := &ifListView{fieldView: f.Field(name), SetOption: set, Defined: len(states) > 0, Empty: empty}
	if choice != "" {
		c := f.Field(choice)
		v.Choice = &c
	}
	byName := map[string]ifState{}
	for _, s := range states {
		byName[s.Name] = s
	}
	list := splitList(f.Value(name))
	act := func(do string, at int) string {
		return "/config/draft?list=" + name + "&do=" + do + "&at=" + strconv.Itoa(at)
	}
	for i, n := range list {
		st, ok := byName[n]
		if !ok {
			st = ifState{Name: n, State: "unknown", Text: "Not defined"}
		}
		v.Entries = append(v.Entries, ifEntry{ifState: st, Pos: i + 1, First: i == 0, Last: i == len(list)-1,
			Up: act("up", i+1), Down: act("down", i+1), Remove: act("remove", i+1)})
	}
	for _, s := range states {
		if !slices.Contains(list, s.Name) {
			v.Available = append(v.Available, s)
		}
	}
	return v
}

// AddAction is the formaction URL of the Add button without the script.
func (v *ifListView) AddAction() string { return "/config/draft?list=" + v.Name + "&do=add" }

// listAction applies a change of an interface list that a button of the
// editor asked for without the script: POST /config/draft?list=<field>&
// do=up|down|remove&at=<n>, or do=add with the add:<field> select, with
// intent=edit. The form then renders again from the changed values; its
// rendered values stay, so the change counts at the next save (design
// 0009 D4).
func (f *form) listAction(r *http.Request) {
	q := r.URL.Query()
	name := q.Get("list")
	if name == "" || r.PostForm.Get("intent") != intentEdit {
		return
	}
	if fd, ok := f.op.field(name); !ok || fd.Kind != fieldList {
		return
	}
	list := splitList(f.Value(name))
	at, _ := strconv.Atoi(q.Get("at"))
	switch q.Get("do") {
	case "up":
		if at >= 2 && at <= len(list) {
			list[at-2], list[at-1] = list[at-1], list[at-2]
		}
	case "down":
		if at >= 1 && at < len(list) {
			list[at-1], list[at] = list[at], list[at-1]
		}
	case "remove":
		if at >= 1 && at <= len(list) {
			list = slices.Delete(list, at-1, at)
		}
	case "add":
		if v := strings.TrimSpace(r.PostForm.Get("add:" + name)); v != "" && !slices.Contains(list, v) {
			list = append(list, v)
		}
	}
	f.values[name] = list
}

// durText is a duration as the file would hold it, without the zero units
// that time.Duration.String adds: 1h, not 1h0m0s.
func durText(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// runningDoc returns the running configuration as a model, for the
// read-only state of the form pages while the file cannot be read or has
// errors (design 0009 D3.13): every key holds its value in force, so the
// pages show what the proxy does now. Notes are not part of the running
// configuration and stay empty.
func runningDoc(c *config.Config) *config.Doc {
	if c == nil {
		return &config.Doc{}
	}
	d := &config.Doc{Name: c.Name}
	var listen []string
	for _, l := range c.Proxy.Listen {
		listen = append(listen, l.String())
	}
	p := &d.Proxy
	p.Listen = config.Some(listen)
	p.Default = c.Proxy.Default.String()
	p.ConnectTimeout = config.Some(durText(c.Proxy.ConnectTimeout))
	p.ResponseTimeout = config.Some(durText(c.Proxy.ResponseTimeout))
	p.IdleTimeout = config.Some(durText(c.Proxy.IdleTimeout))
	p.MaxConnections = config.Some(int64(c.Proxy.MaxConnections))
	if len(c.Proxy.Interfaces) > 0 {
		p.Interfaces = config.Some(slices.Clone(c.Proxy.Interfaces))
	}
	p.InterfaceAttemptTimeout = config.Some(durText(c.Proxy.InterfaceAttemptTimeout))
	p.InterfaceDemotion = config.Some(durText(c.Proxy.InterfaceDemotion))
	rm := c.Proxy.Remote
	var sources []string
	for _, s := range rm.Sources {
		sources = append(sources, s.String())
	}
	if len(sources) > 0 {
		d.Remote.Sources = config.Some(sources)
	}
	ports := []int64{}
	for _, n := range rm.Ports {
		ports = append(ports, int64(n))
	}
	d.Remote.Ports = config.Some(ports)
	d.Remote.Default = config.Some(rm.Default.String())
	d.Remote.AllowDirect = config.Some(rm.AllowDirect)
	d.Remote.MaxConnectionsPerSource = config.Some(int64(rm.MaxConnectionsPerSource))
	d.Admin.Listen = config.Some("")
	if c.Admin.Listen.IsValid() {
		d.Admin.Listen = config.Some(c.Admin.Listen.String())
	}
	d.Stats.RetentionDays = config.Some(int64(c.Stats.RetentionDays))
	d.Stats.MaxHostsPerHour = config.Some(int64(c.Stats.MaxHostsPerHour))
	d.Log.Level = config.Some(c.Log.Level)
	for _, x := range c.Interfaces {
		di := config.DocInterface{Name: x.Name}
		if x.MAC != nil {
			di.MAC = config.Some(x.MAC.String())
		}
		if x.Device != "" {
			di.Device = config.Some(x.Device)
		}
		if x.Kind != "" {
			di.Kind = config.Some(x.Kind)
		}
		d.Interfaces = append(d.Interfaces, di)
	}
	for _, u := range c.Upstreams {
		du := config.DocUpstream{Name: u.Name, Address: u.Address, Disclosure: u.Disclosure,
			ConnectTimeout: config.Some(durText(u.ConnectTimeout)), ResponseTimeout: config.Some(durText(u.ResponseTimeout)),
			UnreachableHold: config.Some(durText(u.UnreachableHold))}
		if u.Interfaces != nil {
			du.Interfaces = config.Some(slices.Clone(u.Interfaces))
		}
		d.Upstreams = append(d.Upstreams, du)
	}
	list := func(l []string) config.Opt[[]string] {
		if len(l) == 0 {
			return config.Opt[[]string]{}
		}
		return config.Some(slices.Clone(l))
	}
	flag := func(b bool) config.Opt[bool] {
		if !b {
			return config.Opt[bool]{}
		}
		return config.Some(true)
	}
	for _, r := range c.Rules {
		dr := config.DocRule{Name: r.Name, Action: r.Route.String(), Hosts: list(r.Hosts), Domains: list(r.Domains),
			AllowLoopback: flag(r.AllowLoopback), AllowPrivate: flag(r.AllowPrivate)}
		var cidrs []string
		for _, p := range r.CIDRs {
			cidrs = append(cidrs, p.String())
		}
		dr.CIDRs = list(cidrs)
		if len(r.Ports) > 0 {
			ports := make([]int64, len(r.Ports))
			for i, p := range r.Ports {
				ports[i] = int64(p)
			}
			dr.Ports = config.Some(ports)
		}
		if r.Interfaces != nil {
			dr.Interfaces = config.Some(slices.Clone(r.Interfaces))
		}
		d.Rules = append(d.Rules, dr)
	}
	return d
}

// pageDoc returns the model a form page renders from: the file's, or,
// while the file cannot be read or has errors, the running
// configuration's, with readOnly set (design 0009 D3.13).
func (s *Server) pageDoc() (file *fileState, d *config.Doc, readOnly bool) {
	file = s.readFile()
	if file.Valid() {
		return file, file.Doc, false
	}
	return file, runningDoc(s.backend.Config()), true
}

// upstreamNames lists the upstreams of d for an action control.
func upstreamNames(d *config.Doc) []string {
	out := make([]string, len(d.Upstreams))
	for i, u := range d.Upstreams {
		out[i] = u.Name
	}
	return out
}

// Dirty reports whether a field's posted value differs from its rendered
// value: a form that renders again after an edit holds changes that are
// not saved, so the page's unsaved-changes guard starts marked (design
// 0009 D3.13). A GET renders every field with its rendered value.
func (f *form) Dirty() bool {
	for _, fd := range f.op.Fields {
		if fd.Under == "" && f.comparable(fd, f.Value) != f.comparable(fd, f.Rendered) {
			return true
		}
	}
	return false
}

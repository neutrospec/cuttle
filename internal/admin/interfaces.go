package admin

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/neutrospec/cuttle/internal/config"
	"github.com/neutrospec/cuttle/internal/iface"
	"github.com/neutrospec/cuttle/internal/stats"
)

// The Interfaces page of design 0009 D3.6 (task D): the interface
// definitions with their state, users, and traffic, the interfaces
// detected on this Mac with Define…, and the definition editor.

var interfaceFields = []formField{
	{Name: "name", Key: "name", Label: "Name", Kind: fieldText},
	{Name: "note", Key: "note", Label: "Note", Kind: fieldText},
	{Name: "ident", Key: "mac", Label: "Identify it by", Kind: fieldChoice,
		Options: []choiceOption{{Value: identMAC, Fields: []string{"mac"}}, {Value: identDevice, Fields: []string{"device"}},
			{Value: identBoth, Fields: []string{"mac", "device"}}}},
	{Name: "mac", Key: "mac", Label: "Hardware address", Kind: fieldText, Under: "ident"},
	{Name: "device", Key: "device", Label: "Device", Kind: fieldText, Under: "ident"},
	{Name: "kind", Key: "kind", Label: "Kind guard", Kind: fieldText},
}

// The options of the identity choice (design 0009 D3.6, 0008 D2).
const (
	identMAC    = "mac"
	identDevice = "device"
	identBoth   = "both"
)

// kinds are the kind guards of a definition, "" for none.
var kinds = []struct{ Value, Label string }{
	{"", "No guard"}, {"wired", "Wired"}, {"wifi", "Wi-Fi"}, {"cellular", "Cellular"},
}

func init() {
	for k, v := range map[string]fieldInfo{
		"interface.save/name":   {Hint: "Lowercase letters, digits, and hyphens. Lists, logs, and statistics use it.", Mono: true},
		"interface.save/note":   {Hint: "Optional, one line, shown only on these pages."},
		"interface.save/mac":    {Hint: "Six colon-separated hex pairs, as cuttle interfaces shows it.", Mono: true},
		"interface.save/device": {Hint: "The BSD name, such as en0.", Mono: true},
		"interface.save/kind":   {Hint: "With a guard, the definition stays offline while its device is another kind of interface."},
	} {
		fieldInfos[k] = v
	}
}

func renderInterface(d *config.Doc, target string) (map[string]string, bool) {
	x, i := d.Interface(target)
	if i < 0 {
		return nil, false
	}
	ident := identMAC
	switch {
	case x.MAC.Set && x.Device.Set:
		ident = identBoth
	case x.Device.Set:
		ident = identDevice
	}
	return map[string]string{"name": x.Name, "note": x.Note.V, "ident": ident, "mac": x.MAC.V, "device": x.Device.V, "kind": x.Kind.V}, true
}

// textOpt is a typed value as a key: unset when empty.
func textOpt(s string) config.Opt[string] {
	if s = strings.TrimSpace(s); s != "" {
		return config.Some(s)
	}
	return config.Opt[string]{}
}

// editInterface applies the editor to x, the definition as the file has
// it. The identity choice sets mac and device from the chosen option
// alone (design 0009 D4).
func editInterface(x config.DocInterface, f *form) config.DocInterface {
	x.Name = f.Required("name", x.Name)
	x.Note = f.Text("note", x.Note)
	if opt, changed := f.Choice("ident"); changed {
		mac, dev := textOpt(f.Value("mac")), textOpt(f.Value("device"))
		switch opt {
		case identMAC:
			x.MAC, x.Device = mac, config.Opt[string]{}
		case identDevice:
			x.MAC, x.Device = config.Opt[string]{}, dev
		case identBoth:
			x.MAC, x.Device = mac, dev
		}
	}
	x.Kind = f.Text("kind", x.Kind)
	return x
}

// detectedRow is one row of the detected table (design 0009 D3.6), as
// cuttle interfaces lists it.
type detectedRow struct {
	Device, Kind, Media, IPv4, IPv6, Default, MAC string
	Active                                        bool
	// Match names the definitions that match the row.
	Match []string
	// Define is the editor link that Define… opens.
	Define string
	link   iface.Link
}

// detectedView is the detected table, or the error that kept it from
// being read.
type detectedView struct {
	Rows   []detectedRow
	Err    string
	ReadAt string
}

// kindText says a kind as the pages do.
func kindText(k string) string {
	switch k {
	case "wifi":
		return "Wi-Fi"
	case "":
		return ""
	}
	return k
}

// detect reads the interface table through the instance's source and
// matches it against defs: the interfaces with a six-byte hardware
// address, as cuttle interfaces lists them by default (design 0008 D15).
func (s *Server) detect(defs []iface.Def) detectedView {
	v := detectedView{ReadAt: s.now().In(s.loc).Format("15:04:05")}
	links, err := s.backend.Interfaces()
	if err != nil {
		v.Err = err.Error()
		return v
	}
	for _, l := range links {
		if len(l.MAC) != 6 || l.Loopback {
			continue
		}
		r := detectedRow{Device: l.Device, Kind: l.Kind.String(), Media: l.Media.String(), MAC: l.MAC.String(),
			Active: l.Media != iface.MediaInactive && l.Up, link: l}
		r.IPv4, r.IPv6 = "-", "-"
		if a := l.FirstUsable(iface.IPv4); a.IsValid() {
			r.IPv4 = a.String()
		}
		if a := l.FirstUsable(iface.IPv6); a.IsValid() {
			r.IPv6 = a.String()
		}
		var routes []string
		if l.Default4 != iface.RouteNone {
			routes = append(routes, "IPv4 "+l.Default4.String())
		}
		if l.Default6 != iface.RouteNone {
			routes = append(routes, "IPv6 "+l.Default6.String())
		}
		r.Default = "none"
		if len(routes) > 0 {
			r.Default = strings.Join(routes, ", ")
		}
		for _, d := range defs {
			if d.Matches(l) {
				r.Match = append(r.Match, d.Name)
			}
		}
		if len(r.Match) == 0 {
			r.Define = "/interfaces/new?" + url.Values{"device": {l.Device}}.Encode()
		}
		v.Rows = append(v.Rows, r)
	}
	return v
}

// refView is one use of a definition: an interfaces list of a rule, an
// upstream, or proxy.interfaces, with its position.
type refView struct {
	Before, Name, Href, After string
}

func refViews(users []config.Ref) []refView {
	out := make([]refView, 0, len(users))
	for _, u := range users {
		pos := ""
		if u.Pos > 0 {
			pos = " position " + strconv.Itoa(u.Pos)
		}
		switch u.Kind {
		case config.KindKey:
			out = append(out, refView{Name: u.Name, After: pos})
		default:
			out = append(out, refView{Before: u.Kind + " ", Name: u.Name, Href: "/" + u.Kind + "s/" + url.PathEscape(u.Name), After: pos})
		}
	}
	return out
}

// usersText names the users in plain text for the menu note.
func usersText(users []config.Ref) string {
	out := make([]string, len(users))
	for i, u := range users {
		out[i] = strings.ReplaceAll(u.String(), "`", "")
	}
	return joinAnd(out)
}

// ifDefView is one definition of the list.
type ifDefView struct {
	Name, Note string
	Identity   []seg
	ifState
	Users []refView
	// UsedBy names the users for the menu, which offers no Delete while
	// anything uses the definition (design 0009 D3.6).
	UsedBy string
	// Traffic is the definition's last 24 hours, when statistics are on.
	Traffic               *stats.Total
	TrafficHref, EditHref string
	Delete                []hiddenField
}

// interfacesView is the data of GET /interfaces.
type interfacesView struct {
	ReadOnly bool
	Defs     []ifDefView
	Detected detectedView
	// Stats is false while statistics are unavailable; the traffic
	// column then gives way to one muted line (design 0009 D3.13).
	Stats bool
}

// identity says a definition's identity keys in words: "hardware
// address", "device en0, Wi-Fi". The hardware address itself stays out
// of the list (design 0009 D3.6).
func identity(x config.DocInterface) []seg {
	var out []seg
	sep := func() {
		if len(out) > 0 {
			out = append(out, seg{Text: ", "})
		}
	}
	if x.MAC.Set {
		out = append(out, seg{Text: "hardware address"})
	}
	if x.Device.Set {
		sep()
		out = append(out, seg{Text: "device "}, seg{Text: x.Device.V, Code: true})
	}
	if x.Kind.Set {
		sep()
		out = append(out, seg{Text: kindText(x.Kind.V)})
	}
	return out
}

// interfaceTraffic returns the last 24 hours per interface, nil without
// statistics.
func (s *Server) interfaceTraffic(ctx context.Context) map[string]stats.Total {
	sv := s.backend.Stats()
	if sv == nil {
		return nil
	}
	w, err := stats.PresetWindow("24h", s.now())
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	totals, err := sv.InterfaceTotals(ctx, stats.Filter{Window: w})
	if err != nil {
		s.log.Warn("admin statistics query", "error", err.Error())
		return nil
	}
	out := map[string]stats.Total{}
	for _, t := range totals {
		out[t.Interface] = t
	}
	return out
}

// defsOf returns the definitions of d for matching.
func defsOf(d *config.Doc) []iface.Def {
	var out []iface.Def
	for _, x := range d.Interfaces {
		def := iface.Def{Name: x.Name, Device: x.Device.V}
		if x.MAC.Set {
			def.MAC, _ = net.ParseMAC(x.MAC.V)
		}
		def.Kind, _ = iface.ParseKind(x.Kind.V)
		out = append(out, def)
	}
	return out
}

// getInterfaces serves GET /interfaces; the Refresh button's script
// request gets only the detected table.
func (s *Server) getInterfaces(w http.ResponseWriter, r *http.Request, _ string, v session) {
	file, d, ro := s.pageDoc()
	page := pageData{Title: "Interfaces", Token: v.csrf, Page: "interfaces"}
	view := &interfacesView{ReadOnly: ro, Detected: s.detect(defsOf(d)), Stats: s.backend.Stats() != nil}
	page.Data = view
	if wantsFragment(r) {
		s.renderFragment(w, http.StatusOK, "interfaces", "detected", page)
		return
	}
	traffic := s.interfaceTraffic(r.Context())
	states := s.ifStates(d)
	del := s.ops["interface.delete"]
	for i, x := range d.Interfaces {
		users := d.InterfaceUsers(x.Name)
		dv := ifDefView{Name: x.Name, Note: x.Note.V, Identity: identity(x), ifState: states[i], Users: refViews(users),
			UsedBy: usersText(users), EditHref: "/interfaces/" + url.PathEscape(x.Name),
			TrafficHref: "/domains?" + url.Values{"interface": {x.Name}, "window": {"24h"}}.Encode()}
		if t, ok := traffic[x.Name]; ok {
			dv.Traffic = &t
		} else if traffic != nil {
			dv.Traffic = &stats.Total{Interface: x.Name}
		}
		if !ro && len(users) == 0 {
			vals, _ := renderInterface(d, x.Name)
			dv.Delete = newFormFrom(del, vals, x.Name, file.Digest, "/interfaces").Hidden()
		}
		view.Defs = append(view.Defs, dv)
	}
	s.render(w, http.StatusOK, "interfaces", page)
}

// ifEditorView is the page data of the definition editor.
type ifEditorView struct {
	ReadOnly bool
	// InPage renders the editor as a page; the script loads it into the
	// Interfaces page's dialog instead.
	InPage bool
	// Title and Sub head the editor, such as "Define en8" and "Wired
	// interface, active, 198.51.100.7".
	Title, Sub string
	// Row is the detected row that Define… started from.
	Row   *detectedRow
	Kinds []optionView
	// Users of an existing definition, which a rename updates.
	Users []refView
}

// interfaceView returns the editor's data for a render of the form.
func (s *Server) interfaceView(d *config.Doc, f *form) *ifEditorView {
	v := &ifEditorView{InPage: true}
	for _, k := range kinds {
		v.Kinds = append(v.Kinds, optionView{Value: k.Value, Label: k.Label, Checked: f.Value("kind") == k.Value})
	}
	if f.New {
		v.Title = "Add interface"
		v.Sub = "A name for one of this Mac's network interfaces, for interface lists."
		if dev := f.Value("device"); dev != "" && f.Value("mac") != "" {
			// A form that Define… prefilled names the device it started from.
			v.Title = "Define " + dev
		}
	} else {
		v.Title = "Edit interface " + f.Target
		v.Sub = "Renaming also updates every interface list that names it."
		if d != nil {
			v.Users = refViews(d.InterfaceUsers(f.Target))
		}
	}
	return v
}

// deviceRE is the form of a BSD name that ?device= may name (design 0009
// D12).
var deviceRE = regexp.MustCompile(`^[a-z]+[0-9]+$`)

// suggestName suggests a definition name for a detected row (design 0009
// D3.6): wifi for Wi-Fi, eth-a, eth-b, … for wired, if-<device>
// otherwise, each unused in d.
func suggestName(d *config.Doc, l iface.Link) string {
	free := func(n string) bool { _, i := d.Interface(n); return i < 0 }
	switch l.Kind {
	case iface.KindWifi:
		for n := 1; ; n++ {
			name := "wifi"
			if n > 1 {
				name += "-" + strconv.Itoa(n)
			}
			if free(name) {
				return name
			}
		}
	case iface.KindWired:
		for c := 'a'; c <= 'z'; c++ {
			if name := "eth-" + string(c); free(name) {
				return name
			}
		}
	}
	base := "if-" + l.Device
	for n := 1; ; n++ {
		name := base
		if n > 1 {
			name += "-" + strconv.Itoa(n)
		}
		if free(name) {
			return name
		}
	}
}

// defineValues prefills the editor from a detected row (design 0009
// D3.6, 0008 D2): device and kind for Wi-Fi, the hardware address
// otherwise. Both values are filled, so that another identity choice
// needs no typing; only the chosen one is written.
func defineValues(d *config.Doc, row detectedRow) map[string]string {
	vals := map[string]string{"name": suggestName(d, row.link), "note": "", "ident": identMAC, "mac": row.MAC, "device": row.Device, "kind": ""}
	if row.link.Kind == iface.KindWifi {
		vals["ident"], vals["kind"] = identDevice, "wifi"
	}
	return vals
}

// getInterfaceEditor serves GET /interfaces/new, with the ?device= of
// Define…, and GET /interfaces/{name}. A script request gets only the
// editor, which the Interfaces page shows in a dialog.
func (s *Server) getInterfaceEditor(w http.ResponseWriter, r *http.Request, _ string, v session) {
	op := s.ops["interface.save"]
	file, d, ro := s.pageDoc()
	target := r.PathValue("name")
	vals := map[string]string{"ident": identMAC}
	var row *detectedRow
	if target != "" {
		var ok bool
		if !config.ValidName(config.KindInterface, target) {
			http.NotFound(w, r)
			return
		}
		if vals, ok = renderInterface(d, target); !ok {
			http.NotFound(w, r)
			return
		}
	} else if dev := r.URL.Query().Get("device"); dev != "" && len(dev) <= 15 && deviceRE.MatchString(dev) {
		// ?device= fills the form only for a BSD name that the detected
		// table holds (design 0009 D12).
		for _, rw := range s.detect(defsOf(d)).Rows {
			if rw.Device == dev {
				vals = defineValues(d, rw)
				row = &rw
				break
			}
		}
	}
	f := newFormFrom(op, vals, target, file.Digest, "/interfaces")
	ev := s.interfaceView(d, f)
	ev.ReadOnly, ev.Row = ro, row
	if row != nil {
		k := row.KindLabel()
		ev.Sub = strings.ToUpper(k[:1]) + k[1:] + " interface, " + row.Media
		if row.IPv4 != "-" {
			ev.Sub += ", " + row.IPv4
		}
		ev.Sub += "."
	}
	page := pageData{Title: ev.Title, Token: v.csrf, Page: "interfaces", Data: &formPage{Form: f, View: ev}}
	if wantsFragment(r) {
		ev.InPage = false
		s.renderFragment(w, http.StatusOK, "interface", "editor", page)
		return
	}
	s.render(w, http.StatusOK, "interface", page)
}

// addInterfacePages registers the Interfaces page, the editor, and the
// interface.save and interface.delete ops.
func (s *Server) addInterfacePages(handle func(string, http.HandlerFunc)) {
	view := func(d *config.Doc, f *form) any { return s.interfaceView(d, f) }
	title := func(verb string) func(f *form) []seg {
		return func(f *form) []seg {
			if f.New {
				return []seg{{Text: "define interface "}, {Text: f.Value("name"), Code: true}}
			}
			return []seg{{Text: verb + " interface "}, {Text: f.Target, Code: true}}
		}
	}
	s.addOp(&formOp{
		Name: "interface.save", Kind: config.KindInterface, Page: "interface", Nav: "interfaces", PageTitle: "Interface",
		Fields: interfaceFields, Render: renderInterface,
		Edit: func(d *config.Doc, f *form) error {
			if f.New {
				d.AddInterface(editInterface(config.DocInterface{}, f))
				return nil
			}
			cur, _ := d.Interface(f.Target)
			return d.ReplaceInterface(f.Target, editInterface(cur, f))
		},
		Title: title("change"),
		Hint: func(f *form) config.Hint {
			return config.Hint{Kind: config.KindInterface, From: f.Target, To: f.Value("name")}
		},
		Entity: func(f *form) string { return f.Value("name") },
		View:   view,
	})
	s.addOp(&formOp{
		Name: "interface.delete", Kind: config.KindInterface, Page: "interface", Nav: "interfaces", PageTitle: "Interface",
		Fields: interfaceFields, Render: renderInterface, Delete: true,
		Edit:  func(d *config.Doc, f *form) error { return d.DeleteInterface(f.Target) },
		Title: title("delete"),
		View:  view,
	})
	handle("GET /interfaces", s.authed(0, s.getInterfaces))
	handle("GET /interfaces/new", s.authed(0, s.getInterfaceEditor))
	handle("GET /interfaces/{name}", s.authed(0, s.getInterfaceEditor))
}

// KindLabel is a detected row's kind in words.
func (r detectedRow) KindLabel() string {
	if r.Kind == "wifi" {
		return "Wi-Fi"
	}
	return r.Kind
}

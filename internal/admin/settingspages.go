package admin

import (
	"html/template"
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"github.com/neutrospec/cuttle/internal/config"
)

// The settings pages of design 0009 D3.7 (task D): Proxy, Remote clients,
// and General. Each is one form op over the keys of its tables, with the
// field-state rule of D4 and the explicit choices of proxy.remote.ports
// and admin.listen.

// Field names of the settings pages are the TOML keys within their table,
// apart from the sub-fields of the explicit choices.
var (
	proxyFormFields = []formField{
		{Name: "listen", Key: "proxy.listen", Label: "Listen on", Kind: fieldList},
		{Name: "default", Key: "proxy.default", Label: "Route", Kind: fieldText},
		{Name: "connect_timeout", Key: "proxy.connect_timeout", Label: "Connect", Kind: fieldText},
		{Name: "response_timeout", Key: "proxy.response_timeout", Label: "Response", Kind: fieldText},
		{Name: "idle_timeout", Key: "proxy.idle_timeout", Label: "Idle tunnel", Kind: fieldText},
		{Name: "max_connections", Key: "proxy.max_connections", Label: "Connection limit", Kind: fieldInt},
		{Name: "interfaces", Key: "proxy.interfaces", Label: "Default interface list", Kind: fieldList},
		{Name: "interface_attempt_timeout", Key: "proxy.interface_attempt_timeout", Label: "Attempt timeout", Kind: fieldText},
		{Name: "interface_demotion", Key: "proxy.interface_demotion", Label: "Demotion", Kind: fieldText},
	}
	remoteFormFields = []formField{
		{Name: "sources", Key: "proxy.remote.sources", Label: "Sources", Kind: fieldList},
		{Name: "ports", Key: "proxy.remote.ports", Label: "Ports", Kind: fieldChoice,
			Options: []choiceOption{{Value: "default"}, {Value: "these", Fields: []string{"ports_list"}}, {Value: "none"}}},
		{Name: "ports_list", Key: "proxy.remote.ports", Label: "Ports", Kind: fieldPorts, Under: "ports"},
		{Name: "default", Key: "proxy.remote.default", Label: "Route", Kind: fieldText},
		{Name: "allow_direct", Key: "proxy.remote.allow_direct", Label: "Allow direct", Kind: fieldCheck},
		{Name: "max_connections_per_source", Key: "proxy.remote.max_connections_per_source", Label: "Connections per source", Kind: fieldInt},
	}
	generalFormFields = []formField{
		{Name: "name", Key: "name", Label: "Instance name", Kind: fieldText},
		{Name: "admin_listen", Key: "admin.listen", Label: "Admin listener", Kind: fieldChoice,
			Options: []choiceOption{{Value: "default"}, {Value: "at", Fields: []string{"admin_at"}}, {Value: "disabled"}}},
		{Name: "admin_at", Key: "admin.listen", Label: "Admin listener", Kind: fieldText, Under: "admin_listen"},
		{Name: "retention_days", Key: "stats.retention_days", Label: "Retention", Kind: fieldInt},
		{Name: "max_hosts_per_hour", Key: "stats.max_hosts_per_hour", Label: "Hosts per hour", Kind: fieldInt},
		{Name: "log_level", Key: "log.level", Label: "Log level", Kind: fieldText},
	}
)

// The explicit choices of design 0009 D4 and their form errors.
const (
	portsDefault, portsThese, portsNone = "default", "these", "none"
	adminDefault, adminAt, adminOff     = "default", "at", "disabled"
	msgPortsEmpty                       = "List at least one port, or choose No ports"
	msgAdminEmpty                       = "Enter an IP:port, or choose Default or Disabled"
	// Unset defaults that a control shows as its choice (design 0009 D4).
	defaultRemoteRoute = "block"
	defaultLogLevel    = "info"
)

// logLevels are the values of log.level, in the order the control shows.
var logLevels = []string{"debug", "info", "warn", "error"}

func init() {
	for k, v := range map[string]fieldInfo{
		"proxy.save/listen":                      {Hint: "IP:port literals, one per line. A listener that is not a loopback address also needs remote sources.", Placeholder: "127.0.0.1:3128, the default", Restart: true, Mono: true},
		"proxy.save/connect_timeout":             {Hint: "Name lookup and connect, all addresses and interfaces together; 100ms to 10m.", Placeholder: "10s, the default", Mono: true},
		"proxy.save/response_timeout":            {Hint: "The response head of a plain-HTTP request; 100ms to 10m.", Placeholder: "60s, the default", Mono: true},
		"proxy.save/idle_timeout":                {Hint: "How long a tunnel may stay idle. 0s keeps idle tunnels open.", Placeholder: "1h, the default", Mono: true},
		"proxy.save/max_connections":             {Hint: "Client connections at once, 16 to 65536.", Placeholder: "1024, the default", Mono: true, Numeric: true},
		"proxy.save/interface_attempt_timeout":   {Hint: "Connect time one interface gets before the next one of the list is tried.", Placeholder: "3s, the default", Mono: true},
		"proxy.save/interface_demotion":          {Hint: "How long an interface that failed stays behind the others. 0s disables demotion.", Placeholder: "30s, the default", Mono: true},
		"remote.save/sources":                    {Hint: "CIDRs of the machines that may connect, one per line, such as 192.0.2.0/24. Empty: no remote client is accepted.", Mono: true},
		"remote.save/ports_list":                 {Hint: "Destination ports, one per line.", Mono: true},
		"remote.save/max_connections_per_source": {Hint: "Connections at once from one source address, 1 to 65536.", Placeholder: "128, the default", Mono: true, Numeric: true},
		"general.save/name":                      {Hint: "Lowercase letters, digits, and hyphens. It appears in the Via header, so the other instances of a chain see it, and in logs.", Mono: true},
		"general.save/admin_at":                  {Hint: "A loopback IP:port, such as 127.0.0.1:3129.", Mono: true},
		"general.save/retention_days":            {Hint: "How many days of hourly statistics to keep, 1 to 90.", Placeholder: "30, the default", Mono: true, Numeric: true},
		"general.save/max_hosts_per_hour":        {Hint: "Distinct hosts kept per hour; the rest are counted together as *. 100 to 100000.", Placeholder: "1000, the default", Mono: true, Numeric: true},
	} {
		fieldInfos[k] = v
	}
}

func lines(list []string) string { return strings.Join(list, "\n") }

// optIntText, optIntsText, and optBoolText render an optional key as its
// field holds it: an unset integer as "", ports one per line, and a
// boolean as "true" or "false".
func optIntText(o config.Opt[int64]) string {
	if !o.Set {
		return ""
	}
	return strconv.FormatInt(o.V, 10)
}

func optIntsText(o config.Opt[[]int64]) string {
	out := make([]string, len(o.V))
	for i, n := range o.V {
		out[i] = strconv.FormatInt(n, 10)
	}
	return lines(out)
}

func optBoolText(o config.Opt[bool]) string {
	if o.Set && o.V {
		return "true"
	}
	return "false"
}

func renderProxy(d *config.Doc, _ string) (map[string]string, bool) {
	p := d.Proxy
	return map[string]string{
		"listen": lines(p.Listen.V), "default": p.Default,
		"connect_timeout": p.ConnectTimeout.V, "response_timeout": p.ResponseTimeout.V, "idle_timeout": p.IdleTimeout.V,
		"max_connections": optIntText(p.MaxConnections), "interfaces": lines(p.Interfaces.V),
		"interface_attempt_timeout": p.InterfaceAttemptTimeout.V, "interface_demotion": p.InterfaceDemotion.V,
	}, true
}

func editProxy(d *config.Doc, f *form) error {
	p := &d.Proxy
	p.Listen = f.List("listen", p.Listen)
	p.Default = f.Required("default", p.Default)
	p.ConnectTimeout = f.Text("connect_timeout", p.ConnectTimeout)
	p.ResponseTimeout = f.Text("response_timeout", p.ResponseTimeout)
	p.IdleTimeout = f.Text("idle_timeout", p.IdleTimeout)
	p.MaxConnections = f.Int("max_connections", p.MaxConnections)
	p.Interfaces = f.List("interfaces", p.Interfaces)
	p.InterfaceAttemptTimeout = f.Text("interface_attempt_timeout", p.InterfaceAttemptTimeout)
	p.InterfaceDemotion = f.Text("interface_demotion", p.InterfaceDemotion)
	return nil
}

// portsChoice returns the option of proxy.remote.ports: unset is Default
// (80 and 443), [] is No ports, and a list is These ports.
func portsChoice(o config.Opt[[]int64]) string {
	switch {
	case !o.Set:
		return portsDefault
	case len(o.V) == 0:
		return portsNone
	}
	return portsThese
}

func renderRemote(d *config.Doc, _ string) (map[string]string, bool) {
	r := d.Remote
	def := defaultRemoteRoute
	if r.Default.Set {
		def = r.Default.V
	}
	return map[string]string{
		"sources": lines(r.Sources.V), "ports": portsChoice(r.Ports), "ports_list": optIntsText(r.Ports),
		"default": def, "allow_direct": optBoolText(r.AllowDirect), "max_connections_per_source": optIntText(r.MaxConnectionsPerSource),
	}, true
}

func editRemote(d *config.Doc, f *form) error {
	r := &d.Remote
	r.Sources = f.List("sources", r.Sources)
	// The choice sets the key from the chosen option alone (OI-0006 Y-R4).
	if opt, changed := f.Choice("ports"); changed {
		switch opt {
		case portsDefault:
			r.Ports = config.Opt[[]int64]{}
		case portsNone:
			r.Ports = config.Some([]int64{})
		case portsThese:
			before := f.parseErrs
			r.Ports = f.PortsValue("ports_list")
			if !r.Ports.Set && f.parseErrs == before {
				f.Error("ports", 0, msgPortsEmpty)
			}
		}
	}
	// The default route is a control that always has a value; an untouched
	// one leaves an unset key unset (design 0009 D4).
	if f.touched("default") {
		r.Default = config.Some(f.Value("default"))
	}
	r.AllowDirect = f.Check("allow_direct", r.AllowDirect)
	r.MaxConnectionsPerSource = f.Int("max_connections_per_source", r.MaxConnectionsPerSource)
	return nil
}

// adminChoice returns the option of admin.listen: unset is Default
// (127.0.0.1:3129), "" is Disabled, and an address is On at.
func adminChoice(o config.Opt[string]) string {
	switch {
	case !o.Set:
		return adminDefault
	case o.V == "":
		return adminOff
	}
	return adminAt
}

func renderGeneral(d *config.Doc, _ string) (map[string]string, bool) {
	level := defaultLogLevel
	if d.Log.Level.Set {
		level = d.Log.Level.V
	}
	return map[string]string{
		"name": d.Name, "admin_listen": adminChoice(d.Admin.Listen), "admin_at": d.Admin.Listen.V,
		"retention_days": optIntText(d.Stats.RetentionDays), "max_hosts_per_hour": optIntText(d.Stats.MaxHostsPerHour),
		"log_level": level,
	}, true
}

func editGeneral(d *config.Doc, f *form) error {
	d.Name = f.Required("name", d.Name)
	if opt, changed := f.Choice("admin_listen"); changed {
		switch opt {
		case adminDefault:
			d.Admin.Listen = config.Opt[string]{}
		case adminOff:
			d.Admin.Listen = config.Some("")
		case adminAt:
			if v := strings.TrimSpace(f.Value("admin_at")); v != "" {
				d.Admin.Listen = config.Some(v)
			} else {
				f.Error("admin_listen", 0, msgAdminEmpty)
			}
		}
	}
	d.Stats.RetentionDays = f.Int("retention_days", d.Stats.RetentionDays)
	d.Stats.MaxHostsPerHour = f.Int("max_hosts_per_hour", d.Stats.MaxHostsPerHour)
	if f.touched("log_level") {
		d.Log.Level = config.Some(f.Value("log_level"))
	}
	return nil
}

// settingsView is the page's own data of a settings form.
type settingsView struct {
	// ReadOnly is set while the file cannot be read or has errors; the
	// page then shows the running configuration (design 0009 D3.13).
	ReadOnly  bool
	Upstreams []string
	// Interfaces is the interface list editor of proxy.interfaces, with
	// Defined the number of definitions and Summary the one-line summary
	// of the collapsed interface fields.
	Interfaces *ifListView
	Defined    int
	Summary    string
	// NoDirect disables Direct as the remote default while allow_direct
	// is off; RemoteOff says that no remote client can connect.
	NoDirect  bool
	RemoteOff bool
	// LogLevels are the options of log.level; AdminStart is the admin
	// address the instance started with.
	LogLevels  []optionView
	AdminStart string
}

// settingsOp returns the form op of a settings page.
func (s *Server) settingsOp(name, page, title string, fields []formField,
	render func(*config.Doc, string) (map[string]string, bool), edit func(*config.Doc, *form) error, what string) *formOp {
	return &formOp{
		Name: name, Page: page, Nav: page, PageTitle: title, Fields: fields, Render: render, Edit: edit,
		Title: func(*form) []seg { return plain("change the " + what) },
		View:  func(d *config.Doc, f *form) any { return s.settingsView(d, f, false) },
	}
}

// settingsView fills the page data of a settings form from the model it
// renders.
func (s *Server) settingsView(d *config.Doc, f *form, readOnly bool) *settingsView {
	v := &settingsView{ReadOnly: readOnly, Upstreams: upstreamNames(d), Defined: len(d.Interfaces)}
	switch f.Op {
	case "proxy.save":
		v.Interfaces = f.IfList("interfaces", "", "", s.ifStates(d), "Empty: direct connections follow macOS routing.")
		list := splitList(f.Value("interfaces"))
		switch {
		case len(list) == 0:
			v.Summary = "System routing"
		default:
			v.Summary = strings.Join(list, ", ")
		}
	case "remote.save":
		v.NoDirect = !f.Checked("allow_direct")
		v.RemoteOff = len(splitList(f.Value("sources"))) == 0 && allLoopback(d.Proxy.Listen)
	case "general.save":
		for _, l := range logLevels {
			v.LogLevels = append(v.LogLevels, optionView{Value: l, Label: strings.ToUpper(l[:1]) + l[1:], Checked: f.Value("log_level") == l})
		}
		if st := s.backend.StartConfig(); st != nil && st.Admin.Listen.IsValid() {
			v.AdminStart = st.Admin.Listen.String()
		}
	}
	return v
}

// allLoopback reports whether every listener is a loopback address; an
// unset list is the loopback default.
func allLoopback(listen config.Opt[[]string]) bool {
	for _, l := range listen.V {
		ap, err := netip.ParseAddrPort(l)
		if err != nil || !ap.Addr().IsLoopback() {
			return false
		}
	}
	return true
}

// getSettingsForm serves GET of a settings page: the form as the file
// has it, or, while the file cannot be read or has errors, the running
// configuration, read-only.
func (s *Server) getSettingsForm(op *formOp, path string) func(http.ResponseWriter, *http.Request, string, session) {
	return func(w http.ResponseWriter, r *http.Request, _ string, v session) {
		file, d, ro := s.pageDoc()
		vals, _ := op.Render(d, "")
		f := newFormFrom(op, vals, "", file.Digest, path)
		fp := &formPage{Form: f, View: s.settingsView(d, f, ro)}
		s.render(w, http.StatusOK, op.Page, pageData{Title: op.PageTitle, Token: v.csrf, Page: op.Nav, Data: fp})
	}
}

// addPagesD registers the pages, routes, and ops of task D of design
// 0009: Interfaces, Proxy, Remote clients, General, and Account.
func (s *Server) addPagesD(handle func(string, http.HandlerFunc)) {
	for _, name := range []string{"interfaces", "interface", "proxy", "remote", "general", "account"} {
		s.pages[name] = template.Must(template.Must(s.base.Clone()).ParseFS(assets, "templates/"+name+".html"))
	}
	proxy := s.settingsOp("proxy.save", "proxy", "Proxy", proxyFormFields, renderProxy, editProxy, "proxy settings")
	remote := s.settingsOp("remote.save", "remote", "Remote clients", remoteFormFields, renderRemote, editRemote, "remote client settings")
	general := s.settingsOp("general.save", "general", "General", generalFormFields, renderGeneral, editGeneral, "general settings")
	for _, op := range []*formOp{proxy, remote, general} {
		s.addOp(op)
	}
	handle("GET /proxy", s.authed(0, s.getSettingsForm(proxy, "/proxy")))
	handle("GET /remote", s.authed(0, s.getSettingsForm(remote, "/remote")))
	handle("GET /general", s.authed(0, s.getSettingsForm(general, "/general")))
	s.addInterfacePages(handle)
	handle("GET /account", s.authed(0, s.getAccount))
	handle("POST /account/password", s.authed(maxFormBytes, s.postPassword))
	handle("POST /account/sessions", s.authed(maxFormBytes, s.postSessions))
}

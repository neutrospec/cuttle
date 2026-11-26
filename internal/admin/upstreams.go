package admin

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/neutrospec/cuttle/internal/config"
)

// The upstreams page of design 0009 D3.5: each upstream with its live
// state, its users, and its traffic, and the editor, a dialog over the
// list with the script and a page without it. A rename rewrites every
// action and default that uses the upstream; an upstream in use cannot be
// deleted.

// Ops of the upstreams page.
const (
	opUpstreamSave   = "upstream.save"
	opUpstreamDelete = "upstream.delete"
)

// upstreamFormFields are the fields of the upstream editor (design 0009
// D4): one per key of [[upstream]], with the interface list as a choice
// between system routing and a list of its own.
var upstreamFormFields = []formField{
	{Name: "name", Key: "name", Label: "Name", Kind: fieldText},
	{Name: "note", Key: "note", Label: "Note", Kind: fieldText},
	{Name: "address", Key: "address", Label: "Address", Kind: fieldText},
	{Name: "disclosure", Key: "disclosure", Label: "Disclosure", Kind: fieldText},
	{Name: "connect_timeout", Key: "connect_timeout", Label: "Connect timeout", Kind: fieldText},
	{Name: "response_timeout", Key: "response_timeout", Label: "Response timeout", Kind: fieldText},
	{Name: "unreachable_hold", Key: "unreachable_hold", Label: "Unreachable hold", Kind: fieldText},
	{Name: "ifaces", Key: "interfaces", Label: "Interfaces", Kind: fieldChoice, Options: []choiceOption{
		{Value: "system"},
		{Value: "only", Fields: []string{"ifaces_list"}},
	}},
	{Name: "ifaces_list", Key: "interfaces", Label: "Interfaces", Kind: fieldList, Under: "ifaces"},
}

// Upstream timeouts in force when a key is unset (design 0004 D3).
const (
	defaultUpConnect  = "10s"
	defaultUpResponse = "60s"
	defaultUpHold     = "5s"
)

func upstreamValues(u config.DocUpstream) map[string]string {
	ifaces := "system"
	if u.Interfaces.Set {
		ifaces = "only"
	}
	return map[string]string{
		"name": u.Name, "note": u.Note.V, "address": u.Address, "disclosure": u.Disclosure,
		"connect_timeout": u.ConnectTimeout.V, "response_timeout": u.ResponseTimeout.V, "unreachable_hold": u.UnreachableHold.V,
		"ifaces": ifaces, "ifaces_list": strings.Join(u.Interfaces.V, "\n"),
	}
}

func renderUpstreamForm(d *config.Doc, target string) (map[string]string, bool) {
	u, i := d.Upstream(target)
	if i < 0 {
		return nil, false
	}
	return upstreamValues(u), true
}

// editUpstreamForm applies the upstream form to u, the upstream as the
// file has it (design 0009 D4).
func editUpstreamForm(u config.DocUpstream, f *form) config.DocUpstream {
	u.Name = strings.TrimSpace(f.Required("name", u.Name))
	u.Note = f.Text("note", u.Note)
	u.Address = strings.TrimSpace(f.Required("address", u.Address))
	u.Disclosure = f.Required("disclosure", u.Disclosure)
	u.ConnectTimeout = trimOpt(f.Text("connect_timeout", u.ConnectTimeout))
	u.ResponseTimeout = trimOpt(f.Text("response_timeout", u.ResponseTimeout))
	u.UnreachableHold = trimOpt(f.Text("unreachable_hold", u.UnreachableHold))
	if opt, changed := f.Choice("ifaces"); changed {
		switch opt {
		case "system":
			u.Interfaces = config.Opt[[]string]{}
		case "only":
			// An empty list stays empty, so the validator says so.
			l := splitList(f.Value("ifaces_list"))
			if l == nil {
				l = []string{}
			}
			u.Interfaces = config.Some(l)
		}
	}
	return u
}

// trimOpt drops the spaces around a duration, and a value of spaces only.
func trimOpt(o config.Opt[string]) config.Opt[string] {
	if !o.Set {
		return o
	}
	if v := strings.TrimSpace(o.V); v != "" {
		return config.Some(v)
	}
	return config.Opt[string]{}
}

// addUpstreamOps registers the ops of the upstreams page.
func (s *Server) addUpstreamOps() {
	s.addOp(&formOp{
		Name: opUpstreamSave, Kind: config.KindUpstream, Page: "upstream", Nav: "upstreams", PageTitle: "Upstream",
		Fields: upstreamFormFields,
		Render: renderUpstreamForm,
		Edit: func(d *config.Doc, f *form) error {
			if f.New {
				d.AddUpstream(editUpstreamForm(config.DocUpstream{}, f))
				f.entity = len(d.Upstreams)
				return nil
			}
			cur, i := d.Upstream(f.Target)
			if i < 0 {
				return fmt.Errorf("upstream %q %w", f.Target, config.ErrNotFound)
			}
			f.entity = i + 1
			return d.ReplaceUpstream(f.Target, editUpstreamForm(cur, f))
		},
		Title: func(f *form) []seg {
			if !f.New {
				return []seg{{Text: "change upstream "}, {Text: f.Target, Code: true}}
			}
			if n := strings.TrimSpace(f.Value("name")); n != "" {
				return []seg{{Text: "add upstream "}, {Text: n, Code: true}}
			}
			return plain("add an upstream")
		},
		Hint: func(f *form) config.Hint {
			if f.New {
				return config.Hint{}
			}
			return config.Hint{Kind: config.KindUpstream, From: f.Target, To: strings.TrimSpace(f.Value("name"))}
		},
		Entity: func(f *form) string { return strings.TrimSpace(f.Value("name")) },
		View: func(d *config.Doc, f *form) any {
			return s.upstreamEditView(d, f)
		},
	})
	s.addOp(&formOp{
		Name: opUpstreamDelete, Kind: config.KindUpstream, Page: "upstream", Nav: "upstreams", PageTitle: "Upstream", Delete: true,
		Fields: upstreamFormFields,
		Render: renderUpstreamForm,
		Edit: func(d *config.Doc, f *form) error {
			if f.intent != intentReview {
				return errReviewOnly
			}
			return d.DeleteUpstream(f.Target)
		},
		Title: func(f *form) []seg { return []seg{{Text: "delete upstream "}, {Text: f.Target, Code: true}} },
		View:  func(d *config.Doc, f *form) any { return s.upstreamEditView(d, f) },
	})
}

// userRef is one use of an upstream, linked to its page where it has
// one.
type userRef struct {
	Label []seg
	Href  string
}

// upstreamUsers lists the rules and defaults that route to the upstream.
func upstreamUsers(d *config.Doc, name string) []userRef {
	var out []userRef
	for _, u := range d.UpstreamUsers(name) {
		switch u.Kind {
		case config.KindRule:
			out = append(out, userRef{Label: []seg{{Text: "rule "}, {Text: u.Name, Code: true}}, Href: "/rules/" + u.Name})
		default:
			page := "proxy"
			if u.Name == "proxy.remote.default" {
				page = "remote"
			}
			out = append(out, userRef{Label: []seg{{Text: u.Name, Code: true}}, Href: pagePath(page)})
		}
	}
	return out
}

// userRefsText names the users in a sentence, for the menu's note.
func userRefsText(users []userRef) string {
	parts := make([]string, len(users))
	for i, u := range users {
		var b strings.Builder
		for _, sg := range u.Label {
			b.WriteString(sg.Text)
		}
		parts[i] = b.String()
	}
	return joinAnd(parts)
}

// upstreamsView is the data of the upstreams page (design 0009 D3.5).
type upstreamsView struct {
	ReadOnly    bool
	Rows        []upstreamRow
	Traffic     bool
	TrafficLine string
}

type upstreamRow struct {
	Name, Note, Address, Disclosure string
	State                           upState
	Known                           bool
	Users                           []userRef
	UsersText                       string
	Warnings                        []string
	Requests, Bytes                 int64
	TrafficHref                     string
	Delete                          *form
}

func (s *Server) upstreamsView(ctx context.Context, st confState) *upstreamsView {
	v := &upstreamsView{ReadOnly: st.readOnly}
	t := s.dayTraffic(ctx)
	v.Traffic, v.TrafficLine = t.ok, t.line()
	states := s.upstreamStates(s.backend.Status())
	warn := st.warnings(config.KindUpstream)
	delOp := s.ops[opUpstreamDelete]
	for i, u := range st.doc.Upstreams {
		row := upstreamRow{Name: u.Name, Note: u.Note.V, Address: u.Address, Disclosure: u.Disclosure,
			Users: upstreamUsers(st.doc, u.Name), Warnings: warn[i+1], TrafficHref: domainsHref("route", "upstream:"+u.Name)}
		row.State, row.Known = states[u.Name]
		row.UsersText = userRefsText(row.Users)
		if tot, ok := t.routes["upstream:"+u.Name]; ok {
			row.Requests, row.Bytes = tot.Requests, tot.Bytes()
		}
		if !st.readOnly && len(row.Users) == 0 {
			row.Delete = newFormFrom(delOp, upstreamValues(u), u.Name, st.file.Digest, "/upstreams")
		}
		v.Rows = append(v.Rows, row)
	}
	return v
}

// getUpstreams serves the upstreams page.
func (s *Server) getUpstreams(w http.ResponseWriter, r *http.Request, _ string, v session) {
	st := s.confState()
	s.render(w, http.StatusOK, "upstreams", pageData{Title: "Upstreams", Token: v.csrf, Page: "upstreams",
		Data: &formPage{View: s.upstreamsView(r.Context(), st)}})
}

// upstreamEditView is the data of the upstream editor.
type upstreamEditView struct {
	ReadOnly bool
	New      bool
	Missing  string
	// Fragment renders the editor as the content of the list's dialog.
	Fragment       bool
	Defined        []string
	InterfacesHref string
	Users          []userRef
	UsersText      string
	Delete         *form
	// Timeouts sums up the timeouts in force for the collapsed group, and
	// the Def fields are the defaults that the empty fields show.
	Timeouts                     string
	Ifaces                       string
	DefConnect, DefResp, DefHold string
}

func (s *Server) upstreamEditView(_ *config.Doc, f *form) *upstreamEditView {
	st := s.confState()
	v := &upstreamEditView{ReadOnly: st.readOnly, New: f.New, InterfacesHref: pagePath("interfaces"), Ifaces: f.Value("ifaces"),
		DefConnect: defaultUpConnect, DefResp: defaultUpResponse, DefHold: defaultUpHold}
	for _, x := range st.doc.Interfaces {
		v.Defined = append(v.Defined, x.Name)
	}
	or := func(val, def string) string {
		if strings.TrimSpace(val) == "" {
			return def
		}
		return strings.TrimSpace(val)
	}
	v.Timeouts = "connect " + or(f.Value("connect_timeout"), defaultUpConnect) + ", response " + or(f.Value("response_timeout"), defaultUpResponse) +
		", unreachable hold " + or(f.Value("unreachable_hold"), defaultUpHold)
	if !f.New {
		if u, i := st.doc.Upstream(f.Target); i >= 0 {
			v.Users = upstreamUsers(st.doc, u.Name)
			v.UsersText = userRefsText(v.Users)
			if !st.readOnly && len(v.Users) == 0 {
				v.Delete = newFormFrom(s.ops[opUpstreamDelete], upstreamValues(u), u.Name, st.file.Digest, "/upstreams")
			}
		}
	}
	return v
}

// getUpstream serves the upstream editor, /upstreams/new or
// /upstreams/{name}: a page, or for a script request the editor block
// that the list's dialog shows.
func (s *Server) getUpstream(w http.ResponseWriter, r *http.Request, _ string, v session) {
	name := r.PathValue("name")
	op := s.ops[opUpstreamSave]
	st := s.confState()
	var fm *form
	missing := ""
	switch {
	case name == "new":
		fm = newFormFrom(op, map[string]string{"ifaces": "system"}, "", st.file.Digest, "/upstreams")
	case !config.ValidName(config.KindUpstream, name):
		missing = name
	default:
		vals, ok := op.Render(st.doc, name)
		if !ok {
			missing = name
			break
		}
		fm = newFormFrom(op, vals, name, st.file.Digest, "/upstreams")
	}
	if missing != "" {
		s.render(w, http.StatusNotFound, "upstream", pageData{Title: "Upstream not found", Token: v.csrf, Page: "upstreams",
			Data: &formPage{View: &upstreamEditView{Missing: missing}}})
		return
	}
	view := s.upstreamEditView(st.doc, fm)
	title := "Add upstream"
	if !fm.New {
		title = "Upstream " + name
	}
	page := pageData{Title: title, Token: v.csrf, Page: "upstreams", Data: &formPage{Form: fm, View: view}}
	if wantsFragment(r) {
		view.Fragment = true
		s.renderFragment(w, http.StatusOK, "upstream", "upstream-editor", page)
		return
	}
	s.render(w, http.StatusOK, "upstream", page)
}

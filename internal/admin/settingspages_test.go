package admin

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/neutrospec/cuttle/internal/config"
)

// docOf returns the model of a valid file.
func docOf(t *testing.T, text string) *config.Doc {
	t.Helper()
	d, _, r := config.DocOf([]byte(text), "x")
	if d == nil {
		t.Fatalf("invalid file: %v\n%s", r.Errors, text)
	}
	return d
}

// candidate returns the text a review would apply.
func candidate(t *testing.T, review string) string {
	t.Helper()
	return applyForm(t, review).Get("text")
}

// save reviews a settings page's form with the pairs set, applies it, and
// returns the review.
func (p *pipeline) save(path string, kv ...string) string {
	p.t.Helper()
	body := p.review(with(p.pageForm(path, settingsFormStart), kv...), http.StatusOK)
	p.apply(body)
	return body
}

// TestAdminSettingsForms checks W22 of design 0009: the Proxy, Remote
// clients, and General forms save each of their keys; clearing a text
// field removes its key and the default applies; restart-only keys carry
// the badge; the explicit choices of proxy.remote.ports and admin.listen
// render and map as D4 says, with their form errors before the
// validator; unset keys that a control shows as a default stay unset; a
// choice and a removed list entry survive Back to editing, check, a 422
// review, and Review again after a 409; another writer's ports = [] stays
// when the ports field is untouched; and the same-key warning on a
// settings page names its key (OI-0006 AA-R7).
func TestAdminSettingsForms(t *testing.T) {
	p := newPipeline(t)
	doc := func() *config.Doc { return docOf(t, p.file()) }

	// Proxy: every key, then every optional one cleared.
	body := p.get("/proxy")
	for _, s := range []string{`Listen on <span class="badge restart">Restart</span>`, `placeholder="10s, the default"`,
		`placeholder="1024, the default"`, `<input type="radio" name="default" value="direct" checked>`} {
		if !strings.Contains(body, s) {
			t.Errorf("proxy page lacks %q", s)
		}
	}
	rv := p.save("/proxy", "listen", "127.0.0.1:41728\n127.0.0.1:41740", "default", "upstream:gateway", "connect_timeout", "15s",
		"response_timeout", "90s", "idle_timeout", "0s", "max_connections", "2048", "interfaces", "wifi\neth-a",
		"interface_attempt_timeout", "2s", "interface_demotion", "0s")
	if !strings.Contains(rv, "<code>proxy.listen</code> changes apply at the next start.") {
		t.Errorf("the review does not list the restart-only key:\n%s", rv)
	}
	px := doc().Proxy
	if got := []string{strings.Join(px.Listen.V, ","), px.Default, px.ConnectTimeout.V, px.ResponseTimeout.V, px.IdleTimeout.V,
		optIntText(px.MaxConnections), strings.Join(px.Interfaces.V, ","), px.InterfaceAttemptTimeout.V, px.InterfaceDemotion.V}; !slices.Equal(got,
		[]string{"127.0.0.1:41728,127.0.0.1:41740", "upstream:gateway", "15s", "90s", "0s", "2048", "wifi,eth-a", "2s", "0s"}) {
		t.Errorf("proxy keys: %q", got)
	}
	p.save("/proxy", "listen", "", "connect_timeout", "", "response_timeout", "", "idle_timeout", "", "max_connections", "",
		"interfaces", "", "interface_attempt_timeout", "", "interface_demotion", "")
	px = doc().Proxy
	if px.Listen.Set || px.ConnectTimeout.Set || px.ResponseTimeout.Set || px.IdleTimeout.Set || px.MaxConnections.Set ||
		px.Interfaces.Set || px.InterfaceAttemptTimeout.Set || px.InterfaceDemotion.Set || px.Default != "upstream:gateway" {
		t.Errorf("cleared proxy keys: %+v", px)
	}
	if c := p.f.be.Config().Proxy; c.ConnectTimeout != 10*time.Second || c.MaxConnections != 1024 || c.IdleTimeout != time.Hour {
		t.Errorf("defaults after clearing: %+v", c)
	}

	// Remote clients: every key; an unset default, allow_direct, and
	// max_connections_per_source stay unset when another field changes.
	p.save("/remote", "max_connections_per_source", "64")
	rm := doc().Remote
	if rm.Default.Set || rm.AllowDirect.Set || rm.Ports.Set || rm.Sources.Set || rm.MaxConnectionsPerSource.V != 64 {
		t.Errorf("remote after one change: %+v", rm)
	}
	p.save("/remote", "sources", "192.0.2.0/24\n198.51.100.0/24", "ports", "these", "ports_list", "8080\n443", "default", "direct",
		"allow_direct", "true", "max_connections_per_source", "")
	rm = doc().Remote
	if got := []string{strings.Join(rm.Sources.V, ","), optIntsText(rm.Ports), rm.Default.V, optBoolText(rm.AllowDirect),
		optIntText(rm.MaxConnectionsPerSource)}; !slices.Equal(got, []string{"192.0.2.0/24,198.51.100.0/24", "8080\n443", "direct", "true", ""}) {
		t.Errorf("remote keys: %q", got)
	}
	// "No ports" writes ports = [], renders as No ports, and survives a
	// save of another field; "Default" removes the key.
	p.save("/remote", "ports", "none")
	if !strings.Contains(p.file(), "ports = []") {
		t.Errorf("No ports did not write ports = []:\n%s", p.file())
	}
	if body := p.get("/remote"); !strings.Contains(body, `<input type="radio" name="ports" value="none" checked>`) {
		t.Errorf("ports = [] does not render as No ports")
	}
	p.save("/remote", "allow_direct", "", "default", "block")
	if d := doc(); !strings.Contains(p.file(), "ports = []") || d.Remote.AllowDirect.Set || d.Remote.Default.V != "block" {
		t.Errorf("another field's save lost ports = [] or kept allow_direct:\n%s", p.file())
	}
	p.save("/remote", "ports", "default")
	if doc().Remote.Ports.Set {
		t.Errorf("Default kept proxy.remote.ports:\n%s", p.file())
	}
	// "These ports" without a port is the form's error, before the
	// validator; an entry that is not a port is an entry's error.
	runs := p.f.s.validations.Load()
	body = p.review(with(p.pageForm("/remote", settingsFormStart), "ports", "these", "ports_list", ""), http.StatusUnprocessableEntity)
	if !strings.Contains(body, msgPortsEmpty) || p.f.s.validations.Load() != runs {
		t.Errorf("These ports without a port: validator runs %d→%d:\n%s", runs, p.f.s.validations.Load(), body)
	}
	body = p.review(with(p.pageForm("/remote", settingsFormStart), "ports", "these", "ports_list", "443\nhttps"), http.StatusUnprocessableEntity)
	if !strings.Contains(body, "Entry 2: &#34;https&#34; must be an integer port") || strings.Contains(body, msgPortsEmpty) {
		t.Errorf("a port that is not an integer:\n%s", body)
	}

	// General: every key; the explicit choice of admin.listen.
	p.save("/general", "retention_days", "14")
	if d := doc(); d.Log.Level.Set || d.Admin.Listen.V != "127.0.0.1:41729" || d.Stats.RetentionDays.V != 14 {
		t.Errorf("general after one change: %+v %+v", d.Log, d.Admin)
	}
	if body := p.get("/general"); !strings.Contains(body, `<input type="radio" name="admin_listen" value="at" checked>`) ||
		!strings.Contains(body, `Listener <span class="badge restart">Restart</span>`) || !strings.Contains(body, `value="info" checked`) {
		t.Errorf("general page:\n%s", body)
	}
	p.save("/general", "name", "cuttle-west", "admin_listen", "at", "admin_at", "127.0.0.1:41750", "retention_days", "",
		"max_hosts_per_hour", "500", "log_level", "debug")
	d := doc()
	if got := []string{d.Name, d.Admin.Listen.V, optIntText(d.Stats.RetentionDays), optIntText(d.Stats.MaxHostsPerHour), d.Log.Level.V}; !slices.Equal(got,
		[]string{"cuttle-west", "127.0.0.1:41750", "", "500", "debug"}) {
		t.Errorf("general keys: %q", got)
	}
	rv = p.review(with(p.pageForm("/general", settingsFormStart), "admin_listen", "disabled"), http.StatusOK)
	if !strings.Contains(rv, "after the next start this page is no longer served") || !strings.Contains(candidate(t, rv), "listen = \"\"\n") {
		t.Errorf("Disabled: no warning or no listen = \"\":\n%s", rv)
	}
	p.apply(rv)
	if body := p.get("/general"); !strings.Contains(body, `<input type="radio" name="admin_listen" value="disabled" checked>`) {
		t.Errorf("admin.listen = \"\" does not render as Disabled")
	}
	p.save("/general", "max_hosts_per_hour", "")
	if !strings.Contains(p.file(), "listen = \"\"\n") || doc().Stats.MaxHostsPerHour.Set {
		t.Errorf("another field's save lost admin.listen = \"\":\n%s", p.file())
	}
	p.save("/general", "admin_listen", "default")
	if doc().Admin.Listen.Set {
		t.Errorf("Default kept admin.listen:\n%s", p.file())
	}
	runs = p.f.s.validations.Load()
	body = p.review(with(p.pageForm("/general", settingsFormStart), "admin_listen", "at", "admin_at", " "), http.StatusUnprocessableEntity)
	if !strings.Contains(body, msgAdminEmpty) || p.f.s.validations.Load() != runs {
		t.Errorf("On at without an address:\n%s", body)
	}
	// An invalid value maps to its field.
	body = p.review(with(p.pageForm("/general", settingsFormStart), "log_level", "loud"), http.StatusUnprocessableEntity)
	if !strings.Contains(body, `<a href="#f-log_level">Log level: &#34;loud&#34; is not debug, info, warn, or error</a>`) {
		t.Errorf("log level error:\n%s", body)
	}
}

// TestAdminSettingsCarry checks the W22 rows on re-renders: a "No ports"
// choice and a source removed from two survive Back to editing, check, a
// 422 review, and Review again after a 409, each followed by a change of
// another field; and a ports = [] that another writer saved while the
// form was open stays when the ports field is untouched. A key that both
// the form and another writer changed is named in the warning (OI-0006
// AA-R7).
func TestAdminSettingsCarry(t *testing.T) {
	p := newPipeline(t)
	two := encodeWith(t, exampleConfig, func(d *config.Doc) {
		d.Remote.Sources = config.Some([]string{"192.0.2.0/24", "198.51.100.0/24"})
	})
	type step struct {
		name string
		// rerender returns the form after one round trip with edits.
		rerender func(form url.Values) url.Values
	}
	page := func(body string) url.Values { return pageForm(t, body, settingsFormStart) }
	steps := []step{
		{"Back to editing", func(form url.Values) url.Values {
			_, body := p.post("/config/draft", with(form, "intent", "edit"))
			return page(body)
		}},
		{"check", func(form url.Values) url.Values {
			_, body := p.post("/config/draft", with(form, "intent", "check"))
			return page(body)
		}},
		{"a 422 review", func(form url.Values) url.Values {
			res, body := p.post("/config/draft", with(form, "intent", "review", "max_connections_per_source", "many"))
			if res.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("422 review: %d", res.StatusCode)
			}
			return with(page(body), "max_connections_per_source", "")
		}},
	}
	edits := []struct {
		name string
		edit []string
		ok   func(cand string) bool
	}{
		{"No ports", []string{"ports", "none"}, func(c string) bool { return strings.Contains(c, "ports = []") }},
		{"one source removed", []string{"sources", "192.0.2.0/24"}, func(c string) bool {
			return strings.Contains(c, `sources = ["192.0.2.0/24"]`)
		}},
	}
	for _, e := range edits {
		for _, s := range steps {
			p.write(two)
			form := s.rerender(with(p.pageForm("/remote", settingsFormStart), e.edit...))
			rv := p.review(with(form, "max_connections_per_source", "32"), http.StatusOK)
			if c := candidate(t, rv); !e.ok(c) || !strings.Contains(c, "max_connections_per_source = 32") {
				t.Errorf("%s through %s: candidate\n%s", e.name, s.name, c)
			}
		}
		// Review again after a 409 at apply.
		p.write(two)
		rv := p.review(with(p.pageForm("/remote", settingsFormStart), e.edit...), http.StatusOK)
		p.write(strings.Replace(two, `name = "cuttle-edge"`, `name = "cuttle-other"`, 1))
		res, body := p.post("/config/apply", applyForm(t, rv))
		if res.StatusCode != http.StatusConflict || !strings.Contains(body, "Review again") {
			t.Fatalf("apply after a change: %d:\n%s", res.StatusCode, body)
		}
		_, again := p.post("/config/draft", with(applyForm(t, body), "intent", "review"))
		if c := candidate(t, again); !e.ok(c) || !strings.Contains(c, `name = "cuttle-other"`) {
			t.Errorf("%s through Review again: candidate\n%s", e.name, c)
		}
	}

	// Another writer's ports = [] stays when the field is untouched.
	p.write(two)
	form := p.pageForm("/remote", settingsFormStart)
	other := encodeWith(t, two, func(d *config.Doc) { d.Remote.Ports = config.Some([]int64{}) })
	p.write(other)
	rv := p.review(with(form, "max_connections_per_source", "32"), http.StatusOK)
	if c := candidate(t, rv); !strings.Contains(c, "ports = []") || !strings.Contains(rv, "The changes below are yours; the other change stays") {
		t.Errorf("another writer's ports = []:\n%s", rv)
	}
	// Both changed ports: the warning names the key (OI-0006 AA-R7).
	p.write(two)
	form = p.pageForm("/remote", settingsFormStart)
	p.write(encodeWith(t, two, func(d *config.Doc) { d.Remote.Ports = config.Some([]int64{8080}) }))
	rv = p.review(with(form, "ports", "none"), http.StatusOK)
	if !strings.Contains(rv, "<span><code>proxy.remote.ports</code> was also changed elsewhere. Applying replaces that change with yours") {
		t.Errorf("same-key warning on a settings page:\n%s", rv)
	}
}

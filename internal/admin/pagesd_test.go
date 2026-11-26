package admin

import (
	"html"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/neutrospec/cuttle/internal/iface"
)

// Helpers and markup checks of the pages of task D of design 0009:
// Interfaces, the interface editor, Proxy, Remote clients, General, and
// Account.

func mustMAC(s string) net.HardwareAddr {
	m, err := net.ParseMAC(s)
	if err != nil {
		panic(err)
	}
	return m
}

func addrs(list ...string) []netip.Addr {
	out := make([]netip.Addr, len(list))
	for i, a := range list {
		out[i] = netip.MustParseAddr(a)
	}
	return out
}

// testLinks is an interface table with reserved addresses and the
// documentation MACs of RFC 7042: built-in Wi-Fi en0 and wired en7, which
// the example configuration defines, two wired interfaces without a
// definition, and loopback and tunnel rows that the page leaves out.
var testLinks = []iface.Link{
	{Index: 1, Device: "lo0", Up: true, Loopback: true, Addrs: addrs("127.0.0.1", "::1")},
	{Index: 4, Device: "en0", MAC: mustMAC("00:00:5e:00:53:01"), Up: true, Kind: iface.KindWifi, Media: iface.MediaActive,
		Addrs: addrs("192.0.2.21", "fe80::1", "2001:db8::21"), Default4: iface.RouteScoped, Default6: iface.RouteScoped},
	{Index: 7, Device: "en7", MAC: mustMAC("00:00:5e:00:53:0a"), Up: true, Kind: iface.KindWired, Media: iface.MediaActive,
		Addrs: addrs("192.0.2.20"), Default4: iface.RoutePrimary},
	{Index: 8, Device: "en8", MAC: mustMAC("00:00:5e:00:53:0b"), Up: true, Kind: iface.KindWired, Media: iface.MediaActive,
		Addrs: addrs("198.51.100.7"), Default4: iface.RouteScoped},
	{Index: 3, Device: "en3", MAC: mustMAC("00:00:5e:00:53:03"), Kind: iface.KindWired, Media: iface.MediaInactive},
	{Index: 20, Device: "utun3", Up: true, Addrs: addrs("fd00::3")},
}

// withLinks injects testLinks into the fake backend.
func (f *fixture) withLinks() {
	f.be.mu.Lock()
	f.be.links = testLinks
	f.be.mu.Unlock()
}

var (
	inputTagRE  = regexp.MustCompile(`<input\b([^>]*)>`)
	selectRE    = regexp.MustCompile(`(?s)<select\b([^>]*)>(.*?)</select>`)
	optionRE    = regexp.MustCompile(`<option\b([^>]*)>`)
	textareaTRE = regexp.MustCompile(`(?s)<textarea\b([^>]*)>\n?(.*?)</textarea>`)
)

// pageForm returns what a browser posts for the first form in body whose
// start tag begins with start: hidden, text, and password inputs, the
// checked radios and checkboxes, the selected option of each select, and
// each textarea, as typed by nobody. Submit buttons are left out.
func pageForm(t *testing.T, body, start string) url.Values {
	t.Helper()
	i := strings.Index(body, start)
	if i < 0 {
		t.Fatalf("no form %q in:\n%s", start, body)
	}
	body = body[i:]
	body = body[:strings.Index(body, "</form>")]
	v := url.Values{}
	for _, m := range inputTagRE.FindAllStringSubmatch(body, -1) {
		a := attrsOf(m[1])
		name := html.UnescapeString(a["name"])
		if name == "" {
			continue
		}
		_, checked := a["checked"]
		switch a["type"] {
		case "radio", "checkbox":
			if checked {
				v.Add(name, html.UnescapeString(a["value"]))
			}
		case "submit", "button":
		default:
			v.Add(name, html.UnescapeString(a["value"]))
		}
	}
	for _, m := range selectRE.FindAllStringSubmatch(body, -1) {
		name := html.UnescapeString(attrsOf(m[1])["name"])
		value := ""
		for j, o := range optionRE.FindAllStringSubmatch(m[2], -1) {
			oa := attrsOf(o[1])
			if _, sel := oa["selected"]; sel || j == 0 {
				value = html.UnescapeString(oa["value"])
			}
		}
		v.Add(name, value)
	}
	for _, m := range textareaTRE.FindAllStringSubmatch(body, -1) {
		v.Add(html.UnescapeString(attrsOf(m[1])["name"]), html.UnescapeString(m[2]))
	}
	return v
}

// settingsForm loads a settings page and returns its form as a browser
// posts it untouched.
func (p *pipeline) pageForm(path, start string) url.Values {
	p.t.Helper()
	return pageForm(p.t, p.get(path), start)
}

const settingsFormStart = `<form method="post" action="/config/draft" class="settings-form"`

// TestAdminPagesDMarkup checks W13 of design 0009 on the pages of task D,
// with an example configuration and the detected table, in the read-only
// state of an invalid file, after a refused review, and without
// definitions; and that a page whose file cannot be read still renders.
func TestAdminPagesDMarkup(t *testing.T) {
	p := newPipeline(t)
	p.f.withLinks()
	pages := []struct{ path, current string }{
		{"/interfaces", "/interfaces"},
		{"/interfaces/new", "/interfaces"},
		{"/interfaces/new?device=en8", "/interfaces"},
		{"/interfaces/new?device=en0", "/interfaces"},
		{"/interfaces/eth-a", "/interfaces"},
		{"/proxy", "/proxy"},
		{"/remote", "/remote"},
		{"/general", "/general"},
		{"/account", "/account"},
	}
	for _, pg := range pages {
		checkMarkup(t, pg.path, p.get(pg.path), true, pg.current)
	}
	// A refused review renders the page with its errors.
	form := p.pageForm("/proxy", settingsFormStart)
	res, body := p.post("/config/draft", with(form, "intent", "review", "max_connections", "many", "connect_timeout", "soon"))
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("refused review: %d:\n%s", res.StatusCode, body)
	}
	checkMarkup(t, "proxy refused", body, true, "/proxy")
	// The read-only state.
	p.write(strings.Replace(exampleConfig, `default = "direct"`, `default = "sideways"`, 1))
	for _, pg := range pages {
		if pg.path == "/interfaces/new?device=en8" {
			continue
		}
		body := p.get(pg.path)
		checkMarkup(t, pg.path+" read-only", body, true, pg.current)
		if pg.path != "/account" && !strings.Contains(body, "is read-only until the file on disk is fixed") {
			t.Errorf("%s: no read-only notice", pg.path)
		}
	}
	// A file that cannot be read.
	if err := os.Remove(p.f.be.path); err != nil {
		t.Fatal(err)
	}
	for _, pg := range pages[:8] {
		res := p.b.doWith(p.h, req{method: http.MethodGet, path: pg.path})
		body := bodyOf(t, res)
		if pg.path == "/interfaces/eth-a" {
			continue
		}
		if res.StatusCode != http.StatusOK {
			t.Errorf("%s without a file: %d", pg.path, res.StatusCode)
		}
		checkMarkup(t, pg.path+" without a file", body, true, pg.current)
	}
}

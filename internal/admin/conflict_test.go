package admin

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/neutrospec/cuttle/internal/config"
)

// TestAdminDraftConflict checks each row of the concurrency table of
// design 0009 D7 (W8): the normal review; a file changed elsewhere in
// keys the operator did not touch, which the review names and keeps
// (OI-0006 Y-R5), and whose apply writes, since apply checks the reviewed
// digest (Y-R1); a key also changed elsewhere, which the review warns
// about; a new entity; a text review after the file changed; an entity
// that no longer exists (409 at draft); a file that changes between
// review and apply (409 at apply, file unchanged, Review again), also when
// it changes back to the base content (Y-R1); a delete of an entity that
// changed elsewhere (Y-R2); "Add to rule" of a rule whose action or ports
// changed (Y-R3); and a reorder with stale names.
func TestAdminDraftConflict(t *testing.T) {
	p := newPipeline(t)
	f := p.f
	other := func(old, new string) string {
		t.Helper()
		cur := p.file()
		if !strings.Contains(cur, old) {
			t.Fatalf("the file lacks %q", old)
		}
		next := strings.Replace(cur, old, new, 1)
		p.write(next)
		return next
	}
	addDocs := func(form url.Values) url.Values {
		return with(form, "domains", "internal.example\nbuild.example\ndocs.example.org")
	}
	const stays = "The file changed after you opened this page. The changes below are yours; the other change stays"

	// The base equals the file: no conflict notice.
	rule := p.testForm("/test/test.rule/restricted?return=/rules/restricted")
	if body := p.review(addDocs(rule), http.StatusOK); strings.Contains(body, "The file changed after you opened this page") {
		t.Errorf("review without a conflict:\n%s", body)
	}

	// Other keys changed: of this rule (note, which the review names), and
	// of another rule. The review keeps both, and apply writes, because it
	// checks the reviewed digest and not the base (Y-R1).
	other(`name = "restricted"`, "name = \"restricted\"\nnote = \"theirs\"")
	other(`name = "ads"`, "name = \"ads\"\nnote = \"theirs too\"")
	body := p.review(addDocs(rule), http.StatusOK)
	if !strings.Contains(body, stays+": Rule <code>restricted</code>: note was changed elsewhere and keeps that change.") ||
		strings.Contains(body, "Applying replaces that change") {
		t.Errorf("review after other changes:\n%s", body)
	}
	if applyForm(t, body).Get("base") != rule.Get("base") {
		t.Error("the review does not carry the base")
	}
	p.apply(body)
	d, _, _ := config.DocOf([]byte(p.file()), "x")
	if r, _ := d.Rule("restricted"); r.Note.V != "theirs" || len(r.Domains.V) != 3 {
		t.Errorf("after apply: %+v", r)
	}
	if r, _ := d.Rule("ads"); r.Note.V != "theirs too" {
		t.Errorf("the other rule's change was lost: %+v", r)
	}

	// The same key changed elsewhere: the review warns that applying
	// replaces it.
	rule = p.testForm("/test/test.rule/restricted?return=/rules/restricted")
	other(`"docs.example.org"`, `"docs.example.org", "theirs.example"`)
	body = p.review(with(rule, "domains", "internal.example"), http.StatusOK)
	if !strings.Contains(body, "Rule <code>restricted</code>: domains was also changed elsewhere. Applying replaces that change with yours; the diff shows what is lost.") {
		t.Errorf("review after a change of the same key:\n%s", body)
	}

	// A new entity: the other change always stays.
	newRule := p.testForm("/test/test.rule/?return=/rules/new")
	other(`note = "theirs too"`, `note = "changed again"`)
	body = p.review(with(newRule, "name", "fresh", "domains", "fresh.example", "action", "block", "ifaces", "default"), http.StatusOK)
	if !strings.Contains(body, stays+".") || !strings.Contains(body, "Rule <code>fresh</code> added at position 1") {
		t.Errorf("review of a new rule after a change:\n%s", body)
	}

	// Text after a change: the replace warning.
	text := p.editorForm()
	other(`note = "changed again"`, `note = "and again"`)
	body = p.review(with(text, "text", exampleConfig), http.StatusOK)
	if !strings.Contains(body, "The file changed after you opened this page. Applying replaces that change with this text; the diff shows what is lost.") {
		t.Errorf("text review after a change:\n%s", body)
	}

	// The entity no longer exists: 409 at the draft step, for every
	// intent, with a link to reload.
	rule = p.testForm("/test/test.rule/restricted?return=/rules/restricted")
	other(`name = "restricted"`, `name = "restricted-b"`)
	before := p.file()
	for _, intent := range []string{intentEdit, intentCheck, intentReview} {
		res, body := p.post("/config/draft", with(addDocs(rule), "intent", intent))
		if res.StatusCode != http.StatusConflict || !strings.Contains(body, "Rule <code>restricted</code> no longer exists; it was changed outside this page.") {
			t.Errorf("%s of a rule that no longer exists: %d:\n%s", intent, res.StatusCode, body)
		}
	}
	if p.file() != before {
		t.Fatal("a refused draft changed the file")
	}
	other(`name = "restricted-b"`, `name = "restricted"`)

	// The file changes between review and apply: 409, the file keeps the
	// other change, and Review again rebuilds the review from the form.
	rule = p.testForm("/test/test.rule/restricted?return=/rules/restricted")
	body = p.review(with(rule, "note", "mine"), http.StatusOK)
	after := other(`name = "lab-net"`, "name = \"lab-net\"\nnote = \"late\"")
	res, body := p.post("/config/apply", applyForm(t, body))
	if res.StatusCode != http.StatusConflict || !strings.Contains(body, "The file changed after this review") ||
		!strings.Contains(body, `formaction="/config/draft" name="intent" value="review">Review again</button>`) ||
		strings.Contains(body, "Apply changes") {
		t.Fatalf("apply after a change: %d:\n%s", res.StatusCode, body)
	}
	if p.file() != after {
		t.Fatal("a refused apply changed the file")
	}
	again := with(applyForm(t, body), "intent", "review")
	res, body = p.post("/config/draft", again)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "Rule <code>restricted</code>: note changed") || !strings.Contains(body, stays) {
		t.Errorf("Review again: %d:\n%s", res.StatusCode, body)
	}

	// Y-R1: the file changed after the review and back to the base
	// content. The base matches, but the reviewed digest does not: 409.
	rule = p.testForm("/test/test.rule/restricted?return=/rules/restricted")
	baseText := p.file()
	other(`note = "late"`, `note = "later"`)
	body = p.review(with(rule, "note", "mine"), http.StatusOK)
	p.write(baseText)
	if res, _ := p.post("/config/apply", applyForm(t, body)); res.StatusCode != http.StatusConflict {
		t.Errorf("apply after the file returned to the base content: %d", res.StatusCode)
	}
	if p.file() != baseText {
		t.Fatal("a refused apply changed the file")
	}

	// Y-R2: a delete posts the rule's rendered values; if the rule changed
	// elsewhere, the review says that deleting removes that change too.
	del := p.testForm("/test/test.rule.delete/ads?return=/rules")
	if body := p.review(del, http.StatusOK); strings.Contains(body, "Deleting removes that change") || !strings.Contains(body, "Rule <code>ads</code> removed") {
		t.Errorf("delete without a change:\n%s", body)
	}
	other(`"ads.example.net"`, `"ads.example.net", "more-ads.example"`)
	body = p.review(del, http.StatusOK)
	if !strings.Contains(body, "Rule <code>ads</code>: domains was changed after you opened this page. Deleting removes that change too; the diff shows it.") {
		t.Errorf("delete after a change:\n%s", body)
	}

	// Y-R3: "Add to rule" when the rule no longer has the chosen action or
	// now has ports: 409.
	add := p.testForm("/test/test.addpattern/ads?return=/domains")
	add.Set("domain", "new-ads.example")
	if res, body := p.post("/config/draft", with(add, "intent", "review")); res.StatusCode != http.StatusOK || !strings.Contains(body, "Rule <code>ads</code>: domains &#43;1") {
		t.Fatalf("add to rule: %d:\n%s", res.StatusCode, body)
	}
	for _, change := range [][2]string{{`action = "block"`, `action = "direct"`}, {`action = "block"`, "action = \"block\"\nports = [443]"}} {
		before := p.file()
		other(change[0], change[1])
		res, body := p.post("/config/draft", with(add, "intent", "review"))
		if res.StatusCode != http.StatusConflict || !strings.Contains(body, "the rule&#39;s action or ports changed") {
			t.Errorf("add to a rule after %q: %d:\n%s", change[1], res.StatusCode, body)
		}
		p.write(before)
	}

	// A reorder whose names no longer match the rules: 409.
	order := p.testForm("/test/test.order/?return=/rules")
	other(`name = "lab-net"`, `name = "lab-net-2"`)
	if res, body := p.post("/config/draft", with(order, "intent", "review", "order", "ads restricted loopback lab-net")); res.StatusCode != http.StatusConflict ||
		!strings.Contains(body, "does not name every rule exactly once") {
		t.Errorf("stale reorder: %d:\n%s", res.StatusCode, body)
	}
	if f.be.reloadCount() != 1 {
		t.Errorf("%d reloads, want the one apply", f.be.reloadCount())
	}
}

// remoteConfig has remote clients with two sources and unset ports.
const remoteConfig = `name = "cuttle-edge"

[proxy]
listen = ["127.0.0.1:41728", "10.8.0.6:41728"]
default = "direct"

[proxy.remote]
sources = ["10.8.0.0/24", "10.9.0.0/24"]

[admin]
listen = "127.0.0.1:41729"
`

// TestAdminFieldStateRoundTrip checks the field-state rule through every
// re-render of design 0009 D4 (the W22 case on the test-only remote
// clients form): choosing "No ports" and removing one of two sources,
// then Back to editing, changing another field, and reviewing gives a
// candidate with ports = [] and without the removed source; the same
// holds through a check, a 422 review, and Review again after a 409; and
// a ports = [] that another writer saved while the form was open survives
// a review in which the ports field was untouched.
func TestAdminFieldStateRoundTrip(t *testing.T) {
	p := newPipeline(t)
	p.write(remoteConfig)
	want := func(body string, ports bool) {
		t.Helper()
		d, _, r := config.DocOf([]byte(applyForm(t, body).Get("text")), "x")
		if d == nil {
			t.Fatalf("candidate invalid: %v", r.Errors)
		}
		if ports != (d.Remote.Ports.Set && len(d.Remote.Ports.V) == 0) || len(d.Remote.Sources.V) != 1 || d.Remote.Sources.V[0] != "10.8.0.0/24" ||
			d.Remote.MaxConnectionsPerSource != config.Some[int64](64) {
			t.Errorf("candidate: ports %+v sources %+v max %+v", d.Remote.Ports, d.Remote.Sources, d.Remote.MaxConnectionsPerSource)
		}
	}
	page := func(body string) url.Values {
		t.Helper()
		return formValues(t, body, `<form method="post" action="/config/draft" class="test-form">`)
	}
	start := p.testForm("/test/test.remote/?return=/remote")
	edited := with(start, "ports", "none", "sources", "10.8.0.0/24")

	// Back to editing re-renders the form with the controls as posted and
	// the rendered values unchanged.
	res, body := p.post("/config/draft", with(edited, "intent", "edit"))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("edit: %d:\n%s", res.StatusCode, body)
	}
	back := page(body)
	if back.Get("ports") != "none" || back.Get("r:ports") != "default" || back.Get("r:sources") != "10.8.0.0/24\n10.9.0.0/24" {
		t.Fatalf("after Back to editing: %v", back)
	}
	want(p.review(with(back, "max", "64"), http.StatusOK), true)

	// Through a check.
	res, body = p.post("/config/draft", with(edited, "intent", "check"))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("check: %d:\n%s", res.StatusCode, body)
	}
	want(p.review(with(page(body), "max", "64"), http.StatusOK), true)

	// Through a 422 review: an invalid default first.
	body = p.review(with(edited, "default", "sideways"), http.StatusUnprocessableEntity)
	want(p.review(with(page(body), "default", "block", "max", "64"), http.StatusOK), true)

	// Through Review again after a 409.
	body = p.review(with(edited, "max", "64"), http.StatusOK)
	p.write(strings.Replace(remoteConfig, `name = "cuttle-edge"`, `name = "cuttle-other"`, 1))
	res, body = p.post("/config/apply", applyForm(t, body))
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("apply after a change: %d", res.StatusCode)
	}
	want(p.review(applyForm(t, body), http.StatusOK), true)

	// Another writer saved ports = [] while the form was open; a review
	// that leaves the ports untouched keeps it.
	p.write(remoteConfig)
	start = p.testForm("/test/test.remote/?return=/remote")
	p.write(strings.Replace(remoteConfig, "[proxy.remote]\n", "[proxy.remote]\nports = []\n", 1))
	body = p.review(with(start, "sources", "10.8.0.0/24", "max", "64"), http.StatusOK)
	want(body, true)
	// And a choice back to Default unsets it, whatever the hidden list
	// holds.
	start = p.testForm("/test/test.remote/?return=/remote")
	if start.Get("ports") != "none" {
		t.Fatalf("ports = [] renders %q", start.Get("ports"))
	}
	body = p.review(with(start, "ports", "default", "ports_list", "8080", "sources", "10.8.0.0/24", "max", "64"), http.StatusOK)
	want(body, false)
	if d, _, _ := config.DocOf([]byte(applyForm(t, body).Get("text")), "x"); d.Remote.Ports.Set {
		t.Errorf("Default kept ports: %+v", d.Remote.Ports)
	}
	// "These ports" with an empty list is a form error.
	if body := p.review(with(start, "ports", "these", "ports_list", ""), http.StatusUnprocessableEntity); !strings.Contains(body, "List at least one port, or choose No ports") {
		t.Errorf("These ports without ports:\n%s", body)
	}
}

package admin

import (
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/neutrospec/cuttle/internal/config"
)

// TestAddOpReservedNames checks OI-0006 AA-R5: a form op whose field is
// named like a hidden input of the pipeline, or with the prefix of the
// rendered values, is refused when it is registered.
func TestAddOpReservedNames(t *testing.T) {
	f := newFixture(t, true)
	for _, name := range append(append([]string{}, reservedFields...), "r:name", "") {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("a field named %q was registered", name)
				}
			}()
			f.s.addOp(&formOp{Name: "test.reserved." + name, Fields: []formField{{Name: "fine"}, {Name: name}}})
		}()
	}
	f.s.addOp(&formOp{Name: "test.fine", Fields: []formField{{Name: "test"}, {Name: "source"}, {Name: "position"}}})
	// The pages' own ops passed the same check in New.
	for _, name := range []string{opRuleSave, opRuleDelete, opRulesOrder, opUpstreamSave, opUpstreamDelete} {
		if f.s.ops[name] == nil {
			t.Errorf("op %s is not registered", name)
		}
	}
}

// TestAdminNoChange checks OI-0006 AA-R2: a review that finds no change
// offers no Apply, and an apply of a candidate equal to the file writes
// nothing, so the previous version in .bak stays.
func TestAdminNoChange(t *testing.T) {
	p := newPipeline(t)
	if err := os.WriteFile(p.f.be.path+".bak", []byte(fakeConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	reloads := p.f.be.reloadCount()
	body := p.review(with(p.editorForm(), "text", exampleConfig), http.StatusOK)
	if !strings.Contains(body, "No change to the configuration.") || strings.Contains(body, "Apply changes") {
		t.Errorf("text review without a change: %s", mainText(body))
	}
	// A crafted apply of the file itself.
	form := url.Values{"csrf": {p.csrf}, "op": {opText}, "return": {"/settings"}, "base": {config.Digest([]byte(exampleConfig))},
		"reviewed": {config.Digest([]byte(exampleConfig))}, "text": {exampleConfig}}
	res, out := p.post("/config/apply", form)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/settings" {
		t.Fatalf("apply without a change: %d: %s", res.StatusCode, mainText(out))
	}
	if readFile(t, p.f.be.path+".bak") != fakeConfig || p.file() != exampleConfig || p.f.be.reloadCount() != reloads {
		t.Error("an apply without a change wrote the file or .bak, or reloaded")
	}
	if body := p.get("/settings"); !strings.Contains(body, "No change; the file was not written.") {
		t.Errorf("toast after an apply without a change: %s", mainText(body))
	}
	// A form op on a file that is not canonical: no change to the model,
	// but the file's form changes, so Apply stays.
	body = p.review(p.ruleForm("/rules/restricted"), http.StatusOK)
	if !strings.Contains(body, "No change to the configuration; applying rewrites the file in canonical form.") || !strings.Contains(body, "Apply changes") {
		t.Errorf("untouched form on a hand-written file: %s", mainText(body))
	}
	// The previous version equal to the file: no Apply.
	if err := os.WriteFile(p.f.be.path+".bak", []byte(exampleConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	body = p.review(url.Values{"csrf": {p.csrf}, "op": {opRestore}, "return": {"/settings"}, "base": {config.Digest([]byte(exampleConfig))}}, http.StatusOK)
	if !strings.Contains(body, "the previous version is the same as the current file") || strings.Contains(body, "Apply changes") {
		t.Errorf("restore of an equal file: %s", mainText(body))
	}
}

// TestAdminApplyAfterReviewAgain checks OI-0006 AA-R3: after a 409 at
// apply, Review again builds a review against the file as it is now, and
// its apply writes the edit and keeps the other change.
func TestAdminApplyAfterReviewAgain(t *testing.T) {
	p := newPipeline(t)
	form := with(p.ruleForm("/rules/restricted"), "note", "mine")
	body := p.review(form, http.StatusOK)
	other := strings.Replace(exampleConfig, `name = "ads"`, "name = \"ads\"\nnote = \"theirs\"", 1)
	p.write(other)
	res, body := p.post("/config/apply", applyForm(t, body))
	if res.StatusCode != http.StatusConflict || !strings.Contains(body, ">Review again</button>") {
		t.Fatalf("apply after a change: %d: %s", res.StatusCode, mainText(body))
	}
	again := with(applyForm(t, body), "intent", "review")
	res, body = p.post("/config/draft", again)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "Apply changes") {
		t.Fatalf("Review again: %d: %s", res.StatusCode, mainText(body))
	}
	if res := p.apply(body); res.Header.Get("Location") != "/rules/restricted" {
		t.Errorf("apply after Review again returns to %q", res.Header.Get("Location"))
	}
	d, _, _ := config.DocOf([]byte(p.file()), "x")
	if r, _ := d.Rule("restricted"); r.Note.V != "mine" {
		t.Errorf("the edit is missing: %+v", r)
	}
	if r, _ := d.Rule("ads"); r.Note.V != "theirs" {
		t.Errorf("the other change is lost: %+v", r)
	}
}

// TestAdminSettingsConflictNotice checks OI-0006 AA-R6: the 409 notice of
// POST /settings names what the page's button does now.
func TestAdminSettingsConflictNotice(t *testing.T) {
	p := newPipeline(t)
	_, base := p.b.settingsForm()
	p.write(strings.Replace(exampleConfig, `name = "ads"`, `name = "ads-b"`, 1))
	res, body := p.post("/settings", url.Values{"csrf": {p.csrf}, "text": {exampleConfig}, "base": {base}})
	if res.StatusCode != http.StatusConflict ||
		!strings.Contains(body, "Review changes shows your text against the file as it is now; applying it replaces that change.") ||
		strings.Contains(body, "saving again replaces it") {
		t.Errorf("409 notice: %d: %s", res.StatusCode, mainText(body))
	}
}

package admin

import (
	"html"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

var (
	formTagRE   = regexp.MustCompile(`<form\b[^>]*>`)
	inputElemRE = regexp.MustCompile(`<input\b([^>]*)>`)
	areaTagRE   = regexp.MustCompile(`(?s)<textarea\b([^>]*)>(.*?)</textarea>`)
	selectTagRE = regexp.MustCompile(`(?s)<select\b([^>]*)>(.*?)</select>`)
	optionTagRE = regexp.MustCompile(`(?s)<option\b([^>]*)>`)
)

// attrs returns the attributes of a tag with their values unescaped.
func attrs(s string) map[string]string {
	m := attrsOf(s)
	for k, v := range m {
		m[k] = html.UnescapeString(v)
	}
	return m
}

// formByID returns what a browser posts for the form with id in body
// without a submit button: its hidden, text, and number inputs, checked
// radios and checkboxes, selects, and textareas, also those outside it
// that name it with form=, and none that is disabled or names another
// form.
func formByID(t *testing.T, body, id string) url.Values {
	t.Helper()
	start := -1
	for _, m := range formTagRE.FindAllStringIndex(body, -1) {
		if attrs(body[m[0]:m[1]])["id"] == id {
			start = m[0]
			break
		}
	}
	if start < 0 {
		t.Fatalf("no form %q in:\n%s", id, body)
	}
	end := start + strings.Index(body[start:], "</form>")
	inside := func(i int) bool { return i > start && i < end }
	owned := func(a map[string]string, at int) bool {
		if f, ok := a["form"]; ok {
			return f == id
		}
		return inside(at)
	}
	v := url.Values{}
	for _, m := range inputElemRE.FindAllStringSubmatchIndex(body, -1) {
		a := attrs(body[m[2]:m[3]])
		if !owned(a, m[0]) || a["name"] == "" {
			continue
		}
		if _, off := a["disabled"]; off {
			continue
		}
		switch a["type"] {
		case "submit", "button", "reset", "image":
			continue
		case "radio", "checkbox":
			if _, on := a["checked"]; !on {
				continue
			}
			if _, ok := a["value"]; !ok {
				a["value"] = "on"
			}
		}
		v.Add(a["name"], a["value"])
	}
	for _, m := range areaTagRE.FindAllStringSubmatchIndex(body, -1) {
		a := attrs(body[m[2]:m[3]])
		if !owned(a, m[0]) || a["name"] == "" {
			continue
		}
		text := html.UnescapeString(body[m[4]:m[5]])
		v.Add(a["name"], strings.TrimPrefix(text, "\n"))
	}
	for _, m := range selectTagRE.FindAllStringSubmatchIndex(body, -1) {
		a := attrs(body[m[2]:m[3]])
		if !owned(a, m[0]) || a["name"] == "" {
			continue
		}
		first, chosen := "", ""
		for i, o := range optionTagRE.FindAllStringSubmatch(body[m[4]:m[5]], -1) {
			oa := attrs(o[1])
			if i == 0 {
				first = oa["value"]
			}
			if _, sel := oa["selected"]; sel && chosen == "" {
				chosen = oa["value"]
			}
		}
		if chosen == "" {
			chosen = first
		}
		v.Add(a["name"], chosen)
	}
	return v
}

// TestFormByID checks the form serializer of the tests on a sample.
func TestFormByID(t *testing.T) {
	body := `<form id="a" method="post"><input type="hidden" name="h" value="1&amp;2">
<input class="input" name="t" value="x"><input type="radio" name="r" value="no"><input type="radio" name="r" value="yes" checked>
<input type="checkbox" name="c" value="true" checked><input type="checkbox" name="d" value="true">
<textarea name="ta">
a
b</textarea><select name="s"><option value="one">One</option><option value="two" selected>Two</option></select>
<select name="s2"><option value="first">F</option></select><input name="off" value="z" disabled>
<button type="submit" name="intent" value="review">R</button><input name="o" form="b" value="other"></form>
<input name="out" form="a" value="outside"><form id="b"><input name="b" value="1"></form>`
	got := formByID(t, body, "a")
	want := url.Values{"h": {"1&2"}, "t": {"x"}, "r": {"yes"}, "c": {"true"}, "ta": {"a\nb"}, "s": {"two"}, "s2": {"first"}, "out": {"outside"}}
	if got.Encode() != want.Encode() {
		t.Errorf("got %v\nwant %v", got, want)
	}
}

package admin

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/neutrospec/cuttle/internal/config"
)

// Forms of design 0009 D4 and D7. A page of tasks C and D registers a
// formOp for each edit it offers; the pipeline in config.go runs every op
// through the same steps: read the file, build its model, apply the op
// with the field-state rule, encode, validate, map the issues to the
// form, and review or render.

// fieldKind says how a field's posted text is parsed and compared.
type fieldKind int

const (
	// fieldText is a text input; values compare after CRLF becomes LF.
	fieldText fieldKind = iota
	// fieldList is a list input: entries split on newlines, commas, and
	// spaces, from one textarea or from repeated inputs of the same name.
	fieldList
	// fieldInt is an integer input.
	fieldInt
	// fieldPorts is a list input of integer ports.
	fieldPorts
	// fieldCheck is a checkbox; it posts "true" when checked and nothing
	// otherwise.
	fieldCheck
	// fieldChoice is an explicit choice, such as Default / These ports /
	// No ports, whose options may each use sub-fields (OI-0006 Y-R4).
	fieldChoice
)

// formField is one field of a form.
type formField struct {
	// Name is the control's name. Its rendered value travels in the hidden
	// input r:<Name>.
	Name string
	// Key is the key path the field edits, as the validator names it: for
	// an op on a rule, upstream, or interface, the key within the entity
	// ("domains"); for a settings page, the path from the top
	// ("proxy.remote.ports").
	Key   string
	Label string
	Kind  fieldKind
	// Options of a choice, each with the sub-fields it uses.
	Options []choiceOption
	// Under names the choice that a sub-field belongs to. A sub-field is
	// compared and written only through its choice, so a sub-field of an
	// option that is not chosen keeps no key.
	Under string
}

// choiceOption is one option of a choice field.
type choiceOption struct {
	Value  string
	Fields []string
}

// formOp is an op of design 0009 D7 that edits the configuration model
// from a form. Tasks C and D register theirs with Server.addOp; text and
// restore, whose candidate is text, are built into the pipeline.
type formOp struct {
	// Name is the op's value of the "op" field, such as "rule.save".
	Name string
	// Kind is the kind of entity the op edits (config.KindRule,
	// config.KindUpstream, or config.KindInterface), or "" for the keys of
	// a settings page. With a kind, an empty target is a new entity.
	Kind string
	// Page is the template page that renders the form, Nav the
	// navigation entry it belongs to, and PageTitle its title.
	Page, Nav, PageTitle string
	// Block is the fragment block of the form's issues, which a check and
	// a refused review answer to a script request; "form-issues" when
	// empty.
	Block string
	// Fields are the form's fields.
	Fields []formField
	// Render returns each field's rendered value as a GET renders it from
	// d (design 0009 D4); ok is false when the target no longer exists. A
	// settings op ignores target.
	Render func(d *config.Doc, target string) (vals map[string]string, ok bool)
	// Edit applies the op to d, which is a copy of the current file's
	// model. An error that wraps config.ErrNotFound, config.ErrOrder, or
	// config.ErrRuleChanged, or an *config.InUseError, answers 409 (D7
	// step 2); another error answers 400.
	Edit func(d *config.Doc, f *form) error
	// Title names the edit after "Review: ", such as "change rule
	// `restricted`".
	Title func(f *form) []seg
	// Hint names a rename for the change summary; nil for none.
	Hint func(f *form) config.Hint
	// Entity returns the name of the entity the form edits in the edited
	// model, for the issue mapping; nil for a settings page or a delete.
	Entity func(f *form) string
	// Delete marks an op that removes its target: it posts the entity's
	// rendered values, and the review warns when the entity changed
	// elsewhere (OI-0006 Y-R2).
	Delete bool
	// View returns the page's own data for a render of the form, such as
	// the upstreams an action select offers; nil for none.
	View func(d *config.Doc, f *form) any
	// Test answers intent=test, Test a URL against the candidate (D8);
	// nil answers 400.
	Test func(s *Server, w http.ResponseWriter, r *http.Request, v session, d *draft)
	// Notices returns the op's own notices of a review, such as where a
	// deleted rule's traffic goes next; nil for none.
	Notices func(d *draft) []reviewNotice
}

func (op *formOp) field(name string) (formField, bool) {
	for _, f := range op.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return formField{}, false
}

func (op *formOp) block() string {
	if op.Block == "" {
		return "form-issues"
	}
	return op.Block
}

// reservedFields are the names of the hidden inputs that a form op's form
// and its review post besides the fields: a field of the same name would
// be overwritten or carried as one of them (OI-0006 AA-R5). The review's
// form also carries every other posted name, and the rendered values use
// the prefix r:.
var reservedFields = []string{"csrf", "intent", "op", "base", "return", "target", "text", "reviewed"}

// addOp registers a form op. It panics on a second op of the same name
// and on a field whose name a hidden input of the pipeline uses.
func (s *Server) addOp(op *formOp) {
	if _, dup := s.ops[op.Name]; dup || op.Name == opText || op.Name == opRestore {
		panic("admin: op registered twice: " + op.Name)
	}
	for _, f := range op.Fields {
		if slices.Contains(reservedFields, f.Name) || strings.HasPrefix(f.Name, "r:") || f.Name == "" {
			panic("admin: op " + op.Name + " has a field with a reserved name: " + strconv.Quote(f.Name))
		}
	}
	s.ops[op.Name] = op
}

// form is the state of a form op (design 0009 D4, D7): the value of each
// control, the rendered value of each field from its hidden r:<name>
// input, and the issues found. A GET fills both from the file; every later
// render, the review included, carries the rendered values forward
// unchanged, so that a change made before a re-render still counts at the
// next save and a key that another writer changed is not mistaken for an
// edit.
type form struct {
	op     *formOp
	Op     string
	Target string
	Base   string
	Return string
	// New marks a form for an entity that is not in the file yet, whose
	// save writes every field that holds a value.
	New bool
	// intent is the draft intent of a posted form, "" for a GET, so that
	// an op can leave a check of an unfinished form quiet where a review
	// is not.
	intent string
	// entity is the 1-based index of the edited entity in the edited
	// model, which an op's Edit may set where the name cannot find it,
	// such as a new rule named like an existing one; 0 leaves it to the
	// op's Entity.
	entity int

	values   url.Values
	rendered map[string]string

	errs, warns map[string][]string
	bad         map[string][]int
	// Summary lists the errors, each linking to its field; Top and
	// TopWarnings hold the issues of the edited entity or the file that
	// have no field; Others holds the issues of other entities.
	Summary     []formIssue
	Top         []string
	TopWarnings []string
	Others      []formIssue
	// parseErrs counts the issues found before validation: values that are
	// not integers and choices without their value (D7 step 3).
	parseErrs int
}

// formIssue is one entry of the error summary or the list of other
// entities' issues.
type formIssue struct {
	Label []seg
	Msg   string
	Href  string
	Warn  bool
}

// newFormFrom returns the form of a GET: each control holds its rendered
// value.
func newFormFrom(op *formOp, vals map[string]string, target, base, ret string) *form {
	f := &form{op: op, Op: op.Name, Target: target, Base: base, Return: ret, New: op.Kind != "" && target == "",
		values: url.Values{}, rendered: map[string]string{}}
	for k, v := range vals {
		f.rendered[k] = v
		fd, _ := op.field(k)
		if fd.Kind == fieldList || fd.Kind == fieldPorts {
			f.values[k] = splitList(v)
		} else {
			f.values.Set(k, v)
		}
	}
	return f
}

// postedForm reads a form op's state from a request.
func postedForm(op *formOp, r *http.Request) *form {
	pf := r.PostForm
	f := &form{op: op, Op: op.Name, Target: pf.Get("target"), Base: pf.Get("base"), Return: pf.Get("return"),
		values: url.Values{}, rendered: map[string]string{}}
	f.New = op.Kind != "" && f.Target == ""
	for k, vs := range pf {
		if name, ok := strings.CutPrefix(k, "r:"); ok {
			if _, known := op.field(name); known && len(vs) > 0 {
				f.rendered[name] = vs[0]
			}
			continue
		}
		if _, known := op.field(k); known {
			f.values[k] = vs
		}
	}
	// A button of the interface list editor without the script.
	f.listAction(r)
	return f
}

// Value returns a control's value; a list's entries one per line.
func (f *form) Value(name string) string { return strings.Join(f.values[name], "\n") }

// Values returns every value posted for a control.
func (f *form) Values(name string) []string { return f.values[name] }

// Checked reports whether a checkbox is checked.
func (f *form) Checked(name string) bool { return normalize(fieldCheck, f.Value(name)) == "true" }

// Rendered returns a field's rendered value.
func (f *form) Rendered(name string) string { return f.rendered[name] }

// Errors and Warnings return the issues of a field, Invalid whether it
// has an error, and Bad the 1-based entries of a list input with errors,
// for its data-bad attribute.
func (f *form) Errors(name string) []string   { return f.errs[name] }
func (f *form) Warnings(name string) []string { return f.warns[name] }
func (f *form) Invalid(name string) bool      { return len(f.errs[name]) > 0 }
func (f *form) Bad(name string) string {
	var out []string
	for _, n := range f.bad[name] {
		out = append(out, strconv.Itoa(n))
	}
	return strings.Join(out, " ")
}

// Fields returns the view of every field of the form, for the hidden
// holders of the form-issues block, from which the script copies each
// field's issues under it after a check or a refused review: into the
// element with id <ID>-issues where the page draws one.
func (f *form) Fields() []fieldView {
	out := make([]fieldView, len(f.op.Fields))
	for i, fd := range f.op.Fields {
		out[i] = f.Field(fd.Name)
	}
	return out
}

// HasErrors reports whether the form has any error, and ErrorCount how
// many.
func (f *form) HasErrors() bool { return f.ErrorCount() > 0 }
func (f *form) ErrorCount() int { return len(f.Summary) + len(f.Top) }

// hiddenField is one hidden input.
type hiddenField struct{ Name, Value string }

// Hidden returns the hidden inputs of the form: the op, the target, the
// base, the return path, and each field's rendered value. The page adds
// the CSRF token.
func (f *form) Hidden() []hiddenField {
	out := []hiddenField{{"op", f.Op}, {"base", f.Base}, {"return", f.Return}}
	if f.Target != "" {
		out = append(out, hiddenField{"target", f.Target})
	}
	names := make([]string, 0, len(f.rendered))
	for k := range f.rendered {
		names = append(names, k)
	}
	slices.Sort(names)
	for _, k := range names {
		out = append(out, hiddenField{"r:" + k, f.rendered[k]})
	}
	return out
}

// normalize returns the form of a posted or rendered value that the
// field-state rule compares (design 0009 D4): text with LF line ends,
// lists after splitting, integers as numbers, and checkboxes as true or
// false.
func normalize(kind fieldKind, s string) string {
	switch kind {
	case fieldList:
		return strings.Join(splitList(s), "\n")
	case fieldInt:
		if n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
			return strconv.FormatInt(n, 10)
		}
		return "\x00" + s
	case fieldPorts:
		var out []string
		for _, e := range splitList(s) {
			if n, err := strconv.ParseInt(e, 10, 64); err == nil {
				out = append(out, strconv.FormatInt(n, 10))
			} else {
				out = append(out, "\x00"+e)
			}
		}
		return strings.Join(out, "\n")
	case fieldCheck:
		if s == "true" || s == "on" {
			return "true"
		}
		return "false"
	}
	return lf(s)
}

func lf(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }

var listSep = regexp.MustCompile(`[\s,]+`)

// splitList splits a list input on newlines, commas, and spaces, as the
// script does (design 0009 D5).
func splitList(s string) []string {
	var out []string
	for _, e := range listSep.Split(s, -1) {
		if e != "" {
			out = append(out, e)
		}
	}
	return out
}

// current returns the comparable value of a field from vals and get: a
// choice's option with the values of the option's sub-fields.
func (f *form) comparable(fd formField, get func(string) string) string {
	if fd.Kind != fieldChoice {
		return normalize(fd.Kind, get(fd.Name))
	}
	opt := get(fd.Name)
	parts := []string{opt}
	for _, o := range fd.Options {
		if o.Value != opt {
			continue
		}
		for _, sub := range o.Fields {
			sd, _ := f.op.field(sub)
			parts = append(parts, normalize(sd.Kind, get(sub)))
		}
	}
	return strings.Join(parts, "\x00")
}

// touched reports whether the operator changed a field: its posted value
// differs from its rendered value. Every field of a new entity counts as
// changed.
func (f *form) touched(name string) bool {
	if f.New {
		return true
	}
	fd, ok := f.op.field(name)
	if !ok {
		panic("admin: op " + f.Op + " has no field " + name)
	}
	return f.comparable(fd, f.Value) != f.comparable(fd, f.Rendered)
}

// changedFields lists the top-level fields that the operator changed.
func (f *form) changedFields() []formField {
	var out []formField
	for _, fd := range f.op.Fields {
		if fd.Under == "" && f.touched(fd.Name) {
			out = append(out, fd)
		}
	}
	return out
}

// differs lists the top-level fields whose rendered value differs from
// now, the rendered values of the current file: the fields that another
// writer changed since the form was rendered.
func (f *form) differs(now map[string]string) []formField {
	var out []formField
	get := func(n string) string { return now[n] }
	for _, fd := range f.op.Fields {
		if fd.Under == "" && f.comparable(fd, f.Rendered) != f.comparable(fd, get) {
			out = append(out, fd)
		}
	}
	return out
}

// The field helpers apply the field-state rule of design 0009 D4 to one
// key: given the key's value in the current file, they return the value
// the save writes. A field posted with its rendered value keeps the key as
// the file has it, set or unset; a changed field sets the key; an emptied
// text field or list, or a cleared checkbox, unsets it. A field of a new
// entity writes whatever it holds.

// Text applies the rule to an optional string key.
func (f *form) Text(name string, cur config.Opt[string]) config.Opt[string] {
	if !f.touched(name) {
		return cur
	}
	if v := lf(f.Value(name)); v != "" {
		return config.Some(v)
	}
	return config.Opt[string]{}
}

// Required applies the rule to a required string key, which "" leaves
// out of the file so that the validator reports it.
func (f *form) Required(name, cur string) string {
	if !f.touched(name) {
		return cur
	}
	return lf(f.Value(name))
}

// List applies the rule to a list key.
func (f *form) List(name string, cur config.Opt[[]string]) config.Opt[[]string] {
	if !f.touched(name) {
		return cur
	}
	return f.ListValue(name)
}

// ListValue returns the posted entries of a list, unset when empty.
func (f *form) ListValue(name string) config.Opt[[]string] {
	if l := splitList(f.Value(name)); len(l) > 0 {
		return config.Some(l)
	}
	return config.Opt[[]string]{}
}

// Int applies the rule to an integer key. A value that is not an integer
// is an issue of the field, and the save writes nothing.
func (f *form) Int(name string, cur config.Opt[int64]) config.Opt[int64] {
	if !f.touched(name) {
		return cur
	}
	v := strings.TrimSpace(f.Value(name))
	if v == "" {
		return config.Opt[int64]{}
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		f.parseError(name, 0, "must be an integer")
		return cur
	}
	return config.Some(n)
}

// Ports applies the rule to a list of ports.
func (f *form) Ports(name string, cur config.Opt[[]int64]) config.Opt[[]int64] {
	if !f.touched(name) {
		return cur
	}
	return f.PortsValue(name)
}

// PortsValue returns the posted ports, unset when empty; an entry that is
// not an integer is an issue of its entry.
func (f *form) PortsValue(name string) config.Opt[[]int64] {
	var out []int64
	for i, e := range splitList(f.Value(name)) {
		n, err := strconv.ParseInt(e, 10, 64)
		if err != nil {
			f.parseError(name, i+1, strconv.Quote(e)+" must be an integer port")
			continue
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return config.Opt[[]int64]{}
	}
	return config.Some(out)
}

// Check applies the rule to a boolean key. Every boolean key defaults to
// false, so a cleared checkbox unsets the key.
func (f *form) Check(name string, cur config.Opt[bool]) config.Opt[bool] {
	if !f.touched(name) {
		return cur
	}
	if f.Checked(name) {
		return config.Some(true)
	}
	return config.Opt[bool]{}
}

// Choice returns the chosen option of an explicit choice and whether the
// operator changed the choice or a sub-field of the chosen option. When
// it changed, the caller sets every key the choice controls from the
// chosen option alone, as the field catalogue's table says (OI-0006
// Y-R4); when it did not, the keys stay as the file has them. A posted
// value that is no option is an issue of the field.
func (f *form) Choice(name string) (string, bool) {
	fd, ok := f.op.field(name)
	if !ok || fd.Kind != fieldChoice {
		panic("admin: op " + f.Op + " has no choice " + name)
	}
	opt := f.Value(name)
	valid := slices.ContainsFunc(fd.Options, func(o choiceOption) bool { return o.Value == opt })
	if !f.touched(name) {
		return opt, false
	}
	if !valid {
		f.parseError(name, 0, "choose one of the options")
	}
	return opt, valid
}

// Error records an issue of a field that the form itself finds, such as
// an explicit choice without its value (D7 step 3); the save then writes
// nothing and the validator does not run. entry is 1-based, or 0.
func (f *form) Error(name string, entry int, msg string) { f.parseError(name, entry, msg) }

func (f *form) parseError(name string, entry int, msg string) {
	f.parseErrs++
	f.addIssue(name, entry, msg, false)
}

// addIssue puts an issue under its field and, for an error, in the
// summary.
func (f *form) addIssue(name string, entry int, msg string, warn bool) {
	fd, _ := f.op.field(name)
	if fd.Under != "" && entry == 0 {
		// An issue of a sub-field without an entry shows under its choice.
		if c, ok := f.op.field(fd.Under); ok {
			fd = c
		}
	}
	text := msg
	label := fd.Label
	if entry > 0 {
		text = "Entry " + strconv.Itoa(entry) + ": " + msg
		label += ", entry " + strconv.Itoa(entry)
	}
	if f.errs == nil {
		f.errs, f.warns, f.bad = map[string][]string{}, map[string][]string{}, map[string][]int{}
	}
	if warn {
		f.warns[fd.Name] = append(f.warns[fd.Name], text)
		return
	}
	f.errs[fd.Name] = append(f.errs[fd.Name], text)
	if entry > 0 {
		f.bad[name] = append(f.bad[name], entry)
	}
	f.Summary = append(f.Summary, formIssue{Label: plain(label), Msg: msg, Href: "#f-" + fd.Name})
}

// issueRef is a validator key path split into its parts (design 0009
// D7): rule[3].domains[2] is entity rule, index 3, key domains, entry 2.
type issueRef struct {
	// Entity is rule, upstream, interface, proxy, proxy.remote, admin,
	// stats, log, or "" for the top level.
	Entity string
	// Index is the 1-based index of a rule, upstream, or interface; 0 for
	// a table or for the whole array.
	Index int
	// Key is the key within the entity, "" for the entity itself.
	Key string
	// Entry is the 1-based entry of a list key, or 0.
	Entry int
}

var (
	keyPartRE = regexp.MustCompile(`^(.+?)(?:\[([1-9][0-9]*)\])?$`)
	arrays    = []string{config.KindRule, config.KindUpstream, config.KindInterface}
	tables    = []string{"proxy.remote", "proxy", "admin", "stats", "log"}
)

// parseIssueKey splits a key path that the validator emits. The key may
// be any name, since an unknown key's message names it as written. ok is
// false for a malformed index of a rule, upstream, or interface, which
// the form shows at its top.
func parseIssueKey(key string) (issueRef, bool) {
	var ref issueRef
	if key == "" {
		return ref, true
	}
	rest := key
	matched := false
	for _, a := range arrays {
		if rest == a {
			return issueRef{Entity: a}, true
		}
		if tail, ok := strings.CutPrefix(rest, a+"["); ok {
			end := strings.IndexByte(tail, ']')
			if end < 0 {
				return ref, false
			}
			n, err := strconv.Atoi(tail[:end])
			if err != nil || n < 1 {
				return ref, false
			}
			ref.Entity, ref.Index, rest, matched = a, n, tail[end+1:], true
			break
		}
	}
	if !matched {
		for _, t := range tables {
			if rest == t {
				return issueRef{Entity: t}, true
			}
			if tail, ok := strings.CutPrefix(rest, t+"."); ok {
				ref.Entity, rest = t, "."+tail
				matched = true
				break
			}
		}
	}
	if matched {
		if rest == "" {
			return ref, true
		}
		var ok bool
		if rest, ok = strings.CutPrefix(rest, "."); !ok {
			return ref, false
		}
	}
	m := keyPartRE.FindStringSubmatch(rest)
	if m == nil {
		return ref, false
	}
	ref.Key = m[1]
	if m[2] != "" {
		ref.Entry, _ = strconv.Atoi(m[2])
	}
	return ref, true
}

// path returns the key path of the reference's key from the top, without
// the entry: the form of formField.Key on a settings page.
func (r issueRef) path() string {
	if r.Entity == "" {
		return r.Key
	}
	if r.Key == "" {
		return r.Entity
	}
	return r.Entity + "." + r.Key
}

// entityLabel and entityHref name the page of the entity an issue belongs
// to (design 0009 D7): rule `x` on /rules/x, and the settings pages for
// the tables.
func entityLabel(r issueRef, d *config.Doc) []seg {
	switch r.Entity {
	case config.KindRule, config.KindUpstream, config.KindInterface:
		name := entityName(r, d)
		label := strings.ToUpper(r.Entity[:1]) + r.Entity[1:]
		if name == "" {
			return plain(label + "s")
		}
		return []seg{{Text: label + " "}, {Text: name, Code: true}}
	case "proxy":
		return plain("Proxy")
	case "proxy.remote":
		return plain("Remote clients")
	}
	return plain("General")
}

func entityHref(r issueRef, d *config.Doc) string {
	switch r.Entity {
	case config.KindRule, config.KindUpstream, config.KindInterface:
		if name := entityName(r, d); name != "" {
			return "/" + r.Entity + "s/" + url.PathEscape(name)
		}
		return "/" + r.Entity + "s"
	case "proxy":
		return "/proxy"
	case "proxy.remote":
		return "/remote"
	}
	return "/general"
}

func entityName(r issueRef, d *config.Doc) string {
	if d == nil || r.Index < 1 {
		return ""
	}
	i := r.Index - 1
	switch r.Entity {
	case config.KindRule:
		if i < len(d.Rules) {
			return d.Rules[i].Name
		}
	case config.KindUpstream:
		if i < len(d.Upstreams) {
			return d.Upstreams[i].Name
		}
	case config.KindInterface:
		if i < len(d.Interfaces) {
			return d.Interfaces[i].Name
		}
	}
	return ""
}

// entityIndex returns the 1-based index of the named entity of the op's
// kind in d, or 0.
func entityIndex(kind, name string, d *config.Doc) int {
	var i int
	switch kind {
	case config.KindRule:
		_, i = d.Rule(name)
	case config.KindUpstream:
		_, i = d.Upstream(name)
	case config.KindInterface:
		_, i = d.Interface(name)
	default:
		return 0
	}
	return i + 1
}

// fieldFor returns the field of the op that shows an issue: the field
// whose key is the issue's key, preferring a list for an entry.
func (op *formOp) fieldFor(key string, entry int) (formField, bool) {
	var found []formField
	for _, f := range op.Fields {
		if f.Key == key {
			found = append(found, f)
		}
	}
	for _, f := range found {
		if entry == 0 || f.Kind == fieldList || f.Kind == fieldPorts {
			return f, true
		}
	}
	if len(found) > 0 {
		return found[0], true
	}
	return formField{}, false
}

// ownsTable reports whether a settings op has a field in the table.
func (op *formOp) ownsTable(entity string) bool {
	for _, f := range op.Fields {
		if r, ok := parseIssueKey(f.Key); ok && r.Entity == entity {
			return true
		}
	}
	return false
}

// mapIssues sorts the validator's issues of the candidate into the form
// (design 0009 D7): issues of the edited entity, at index entity of d, or
// of the settings page's tables go under their field, with "Entry n" for
// an entry of a list; those without a field go to the top; issues of
// other entities go to Others with a link to their page.
func (f *form) mapIssues(rep *config.Report, d *config.Doc, entity int) {
	each := func(list []config.Issue, warn bool) {
		for _, is := range list {
			ref, ok := parseIssueKey(is.Key)
			msg := is.Msg
			own := ok && f.owns(ref, entity)
			if !own && ok && ref.Entity == "" && ref.Key == "" {
				own = true // the file as a whole
			}
			if !ok {
				own, msg = true, is.Key+": "+is.Msg
			}
			if !own {
				f.Others = append(f.Others, formIssue{Label: entityLabel(ref, d), Msg: issueText(ref, is.Msg), Href: entityHref(ref, d), Warn: warn})
				continue
			}
			key := ref.Key
			if f.op.Kind == "" {
				key = ref.path()
			}
			if fd, found := f.op.fieldFor(key, ref.Entry); found && ok && ref.Key != "" {
				f.addIssue(fd.Name, ref.Entry, msg, warn)
				continue
			}
			if warn {
				f.TopWarnings = append(f.TopWarnings, msg)
			} else {
				f.Top = append(f.Top, msg)
			}
		}
	}
	each(rep.Errors, false)
	each(rep.Warnings, true)
}

// dropKnownWarnings removes from Others the warnings of other entities
// that the current file, rep of model d, has already, so that the list
// names what the edit causes, such as a later rule that the edited rule
// now shadows. Errors stay, since they block the save. Entities are
// compared by name, so an edit that moves them keeps the match.
func (f *form) dropKnownWarnings(rep *config.Report, d *config.Doc) {
	if rep == nil || len(f.Others) == 0 {
		return
	}
	key := func(label []seg, msg string) string {
		var b strings.Builder
		for _, sg := range label {
			b.WriteString(sg.Text)
		}
		return b.String() + "\x00" + msg
	}
	known := map[string]bool{}
	for _, is := range rep.Warnings {
		if ref, ok := parseIssueKey(is.Key); ok {
			known[key(entityLabel(ref, d), issueText(ref, is.Msg))] = true
		}
	}
	out := f.Others[:0]
	for _, o := range f.Others {
		if !o.Warn || !known[key(o.Label, o.Msg)] {
			out = append(out, o)
		}
	}
	f.Others = out
}

// owns reports whether an issue belongs to the form: the edited entity
// of an entity op, or a table of a settings op.
func (f *form) owns(r issueRef, entity int) bool {
	if f.op.Kind != "" {
		return r.Entity == f.op.Kind && entity > 0 && r.Index == entity
	}
	return f.op.ownsTable(r.Entity)
}

// issueText returns an issue message of another entity with its key and
// entry, such as "domains, entry 2: …".
func issueText(r issueRef, msg string) string {
	switch {
	case r.Key == "":
		return msg
	case r.Entry > 0:
		return fmt.Sprintf("%s, entry %d: %s", r.Key, r.Entry, msg)
	}
	return r.Key + ": " + msg
}

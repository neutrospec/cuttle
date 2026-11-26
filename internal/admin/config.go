package admin

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/neutrospec/cuttle/internal/config"
	"github.com/neutrospec/cuttle/internal/control"
	"github.com/neutrospec/cuttle/internal/policy"
	"github.com/neutrospec/cuttle/internal/textdiff"
)

// The save pipeline of design 0009 D7: POST /config/draft checks, tests,
// reviews, or re-renders a draft and never writes; POST /config/apply
// writes a reviewed candidate and reloads; POST /config/reload reloads
// the file as it is. Every edit of the configuration from the admin UI
// goes through one review, and every write through save, which POST
// /settings shares.

// The ops whose candidate is text (design 0009 D7).
const (
	// opText is the Advanced editor's text as written.
	opText = "text"
	// opRestore is the previous version, <config>.bak.
	opRestore = "restore"
)

// Draft intents (design 0009 D7).
const (
	intentEdit   = "edit"
	intentCheck  = "check"
	intentTest   = "test"
	intentReview = "review"
)

// fileState is the configuration file on disk.
type fileState struct {
	Path    string
	Data    []byte
	Digest  string
	ReadErr error
	// Doc and Cfg are nil when the file is invalid; Report holds its
	// issues.
	Doc    *config.Doc
	Cfg    *config.Config
	Report *config.Report
}

// Valid reports whether the file was read and the validator accepts it.
func (f *fileState) Valid() bool { return f.ReadErr == nil && f.Cfg != nil }

// readFile reads and validates the configuration file.
func (s *Server) readFile() *fileState {
	f := &fileState{Path: s.backend.ConfigPath()}
	data, err := readConfigFile(f.Path)
	if err != nil {
		f.ReadErr = err
		return f
	}
	f.Data, f.Digest = data, config.Digest(data)
	f.Doc, f.Cfg, f.Report = config.DocOf(data, f.Path)
	return f
}

// returnPages are the pages a form may return to after apply (design 0009
// D7), besides the editor paths /rules/{name}, /upstreams/{name}, and
// /interfaces/{name}.
var returnPages = []string{"/", "/domains", "/live", "/rules", "/rules/new", "/upstreams", "/upstreams/new",
	"/interfaces", "/interfaces/new", "/proxy", "/remote", "/general", "/account", "/settings", "/settings/backup"}

var editorPathRE = regexp.MustCompile(`^/(rules|upstreams|interfaces)/([^/]+)$`)

// editorPath returns the kind and name of an editor path such as
// /rules/x.
func editorPath(p string) (kind, name string, ok bool) {
	m := editorPathRE.FindStringSubmatch(p)
	if m == nil || m[2] == "new" {
		return "", "", false
	}
	kind = strings.TrimSuffix(m[1], "s")
	return kind, m[2], config.ValidName(kind, m[2])
}

// cleanReturn checks the return path of a form: one of the configuration
// or traffic pages, with an optional query that parses; it returns the
// path with the query re-encoded. ok is false for anything else, which
// gets 400 (design 0009 D7).
func cleanReturn(raw string) (string, bool) {
	if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") || strings.ContainsAny(raw, "\\#") {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" || u.RawPath != "" {
		return "", false
	}
	if _, _, ok := editorPath(u.Path); !ok && !slices.Contains(returnPages, u.Path) {
		return "", false
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", false
	}
	if len(q) == 0 {
		return u.Path, true
	}
	return u.Path + "?" + q.Encode(), true
}

// navOf returns the navigation entry of a page path, so that the review
// page marks the page the edit started on.
func navOf(path string) string {
	path, _, _ = strings.Cut(path, "?")
	if path == "/settings/backup" {
		path = "/settings"
	}
	for _, p := range navPages {
		if p.path == "" {
			// A page of a later stage: match its list path.
			if strings.HasPrefix(path, "/"+p.id) {
				return p.id
			}
			continue
		}
		// An editor path such as /interfaces/eth-a belongs to its list.
		if p.path == path || (p.path != "/" && strings.HasPrefix(path, p.path+"/")) {
			return p.id
		}
	}
	return ""
}

// draft is one request of the pipeline.
type draft struct {
	opName, intent, base, ret string
	// op and form are set for a form op.
	op   *formOp
	form *form
	// text is the posted text of the text op.
	text string
	file *fileState
	// doc is the edited model of a form op and entity the 1-based index
	// of the edited entity in it.
	doc    *config.Doc
	entity int
	bak    *backupFile
	// cand is the candidate, the bytes apply writes, with its validation.
	cand    []byte
	candCfg *config.Config
	candRep *config.Report
}

// postDraft serves POST /config/draft (design 0009 D7).
func (s *Server) postDraft(w http.ResponseWriter, r *http.Request, _ string, v session) {
	pf := r.PostForm
	d := &draft{opName: pf.Get("op"), intent: pf.Get("intent"), base: pf.Get("base")}
	ret, ok := cleanReturn(pf.Get("return"))
	if !ok {
		http.Error(w, "400 Bad Request: missing or invalid return path", http.StatusBadRequest)
		return
	}
	d.ret = ret
	if !sha256Hex.MatchString(d.base) {
		http.Error(w, "400 Bad Request: missing or invalid base", http.StatusBadRequest)
		return
	}
	switch d.intent {
	case intentEdit, intentCheck, intentTest, intentReview:
	default:
		http.Error(w, "400 Bad Request: missing or invalid intent", http.StatusBadRequest)
		return
	}
	switch d.opName {
	case opText:
		s.draftText(w, r, v, d)
	case opRestore:
		s.draftRestore(w, r, v, d)
	default:
		op := s.ops[d.opName]
		if op == nil {
			http.Error(w, "400 Bad Request: unknown op", http.StatusBadRequest)
			return
		}
		d.op, d.form = op, postedForm(op, r)
		d.form.Base, d.form.Return = d.base, d.ret
		s.draftForm(w, r, v, d)
	}
}

// validate runs the validator on the candidate; validations counts the
// runs, so a test can tell that a draft with a form error skipped it.
func (s *Server) validate(d *draft) bool {
	s.validations.Add(1)
	d.candCfg, d.candRep = config.Parse(d.cand, d.file.Path)
	return len(d.candRep.Errors) == 0
}

// draftText serves the text op: the Advanced editor's text as written,
// with CRLF line ends made LF (D7 step 4).
func (s *Server) draftText(w http.ResponseWriter, r *http.Request, v session, d *draft) {
	d.text = lf(r.PostForm.Get("text"))
	page := s.settingsBase()
	page.Text, page.Base, page.Editable, page.Dirty = d.text, d.base, true, true
	if len(d.text) > config.MaxFileSize {
		page.Text, page.Editable = "", false
		page.ReadError = fmt.Sprintf("The submitted configuration is larger than %d KiB; nothing was saved.", config.MaxFileSize>>10)
		s.renderSettings(w, r, http.StatusRequestEntityTooLarge, v, page)
		return
	}
	switch d.intent {
	case intentEdit:
		page.validate()
		s.renderSettings(w, r, http.StatusOK, v, page)
		return
	case intentCheck:
		if !page.validate() {
			page.Notice = "The configuration has errors; nothing was saved."
			s.renderSettings(w, r, http.StatusUnprocessableEntity, v, page)
			return
		}
		page.Notice, page.NoticeOK = "The configuration is valid. It has not been saved.", true
		s.renderSettings(w, r, http.StatusOK, v, page)
		return
	case intentTest:
		http.Error(w, "400 Bad Request: the text editor has no test", http.StatusBadRequest)
		return
	}
	d.file = s.readFile()
	if d.file.ReadErr != nil {
		s.refuseReview(w, r, v, d, http.StatusConflict, plain("Review: edit the configuration as text"),
			plain("The configuration file cannot be read, so nothing can be applied: "+d.file.ReadErr.Error()))
		return
	}
	d.cand = []byte(d.text)
	if !s.validate(d) {
		page.Errors, page.Warnings = issueLines(d.candRep.Errors), issueLines(d.candRep.Warnings)
		page.Notice = "The configuration has errors; fix them, then review again. Nothing was saved."
		s.renderSettings(w, r, http.StatusUnprocessableEntity, v, page)
		return
	}
	rv := s.newReview(d, plain("Review: edit the configuration as text"))
	added, removed := rv.Diff.Added, rv.Diff.Removed
	rv.Summary = [][]seg{plain("Edited as text: " + plural(added, "line") + " added, " + strconv.Itoa(removed) + " removed")}
	if added == 0 && removed == 0 {
		rv.Summary = [][]seg{plain("No change to the configuration.")}
	}
	rv.Back = true
	s.textNotices(d, rv, "this text")
	s.renderReview(w, r, http.StatusOK, v, rv, d.ret)
}

// draftRestore serves the restore op: the previous version as the
// candidate, read with the D12 checks (design 0009 D3.10).
func (s *Server) draftRestore(w http.ResponseWriter, r *http.Request, v session, d *draft) {
	title := plain("Review: restore the previous version")
	if d.intent != intentReview {
		http.Error(w, "400 Bad Request: a restore can only be reviewed", http.StatusBadRequest)
		return
	}
	d.file = s.readFile()
	if d.file.ReadErr != nil {
		s.refuseReview(w, r, v, d, http.StatusConflict, title,
			plain("The configuration file cannot be read, so nothing can be applied: "+d.file.ReadErr.Error()))
		return
	}
	d.bak = s.readBackup()
	if !d.bak.Offered() {
		s.refuseReview(w, r, v, d, http.StatusConflict, title, plain("The previous version cannot be restored. "+d.bak.Reason))
		return
	}
	d.cand = []byte(lf(string(d.bak.Data)))
	if !s.validate(d) {
		rv := s.refusal(d, title, plain("The previous version has errors, so it cannot be restored."))
		rv.Errors = issueLines(d.candRep.Errors)
		s.renderReview(w, r, http.StatusUnprocessableEntity, v, rv, d.ret)
		return
	}
	rv := s.newReview(d, title)
	rv.Summary = [][]seg{plain("Restores the previous version saved " + s.when(d.bak.ModTime, s.now()) + ".")}
	s.textNotices(d, rv, "the previous version")
	s.renderReview(w, r, http.StatusOK, v, rv, d.ret)
}

// textNotices adds the notices of a text candidate: the conflict warning
// when the file changed since the page loaded, and the current file's
// errors (design 0009 D7).
func (s *Server) textNotices(d *draft, rv *reviewView, what string) {
	var n []reviewNotice
	if d.base != d.file.Digest {
		n = append(n, reviewNotice{Kind: "warn", Text: plain("The file changed after you opened this page. Applying replaces that change with " +
			what + "; the diff shows what is lost.")})
	}
	if !d.file.Valid() && d.file.Report != nil {
		n = append(n, reviewNotice{Kind: "warn", Text: plain("The current file has errors; applying replaces it."),
			List: issueLines(d.file.Report.Errors)})
	}
	rv.Notices = append(n, rv.Notices...)
}

// draftForm serves a form op through the steps of design 0009 D7.
func (s *Server) draftForm(w http.ResponseWriter, r *http.Request, v session, d *draft) {
	op, f := d.op, d.form
	// Step 1: the current file. A form edit needs its model.
	d.file = s.readFile()
	if d.file.ReadErr != nil {
		s.refuseForm(w, r, v, d, http.StatusConflict, plain("The configuration file cannot be read, so this page cannot change it: "+d.file.ReadErr.Error()))
		return
	}
	if !d.file.Valid() {
		s.refuseForm(w, r, v, d, http.StatusConflict, plain("The configuration file has errors, so this page cannot change it. Fix it in Advanced or restore the previous version."))
		return
	}
	// Step 2: the op on a copy of the model.
	if !f.New && op.Kind != "" {
		if _, ok := op.Render(d.file.Doc, f.Target); !ok {
			s.refuseForm(w, r, v, d, http.StatusConflict, gone(op.Kind, f.Target))
			return
		}
	}
	d.doc = d.file.Doc.Clone()
	if err := op.Edit(d.doc, f); err != nil {
		var inUse *config.InUseError
		switch {
		case errors.Is(err, config.ErrNotFound):
			s.refuseForm(w, r, v, d, http.StatusConflict, gone(op.Kind, f.Target))
		case errors.Is(err, config.ErrOrder), errors.Is(err, config.ErrRuleChanged), errors.As(err, &inUse):
			s.refuseForm(w, r, v, d, http.StatusConflict, plain(changedElsewhere(err)))
		default:
			http.Error(w, "400 Bad Request: "+err.Error(), http.StatusBadRequest)
		}
		return
	}
	// Step 3: values that are not integers and choices without their value
	// stop here, before the validator.
	if f.parseErrs > 0 {
		code := http.StatusUnprocessableEntity
		if d.intent == intentEdit {
			code = http.StatusOK
		}
		s.renderForm(w, r, code, v, d, true)
		return
	}
	// Steps 4 and 5: the candidate, its validation, and the issues in the
	// form.
	d.cand = d.doc.Encode()
	if op.Entity != nil {
		d.entity = entityIndex(op.Kind, op.Entity(f), d.doc)
	}
	if len(d.cand) > config.MaxFileSize {
		msg := fmt.Sprintf("In canonical form the file would be %s, larger than the %d KiB limit, so this change cannot be saved.",
			formatBytes(int64(len(d.cand))), config.MaxFileSize>>10)
		if d.intent == intentReview {
			rv := s.refusal(d, s.formTitle(d), plain(msg))
			rv.Back = true
			s.renderReview(w, r, http.StatusRequestEntityTooLarge, v, rv, d.ret)
			return
		}
		f.Top = append(f.Top, msg)
		s.renderForm(w, r, http.StatusRequestEntityTooLarge, v, d, true)
		return
	}
	valid := s.validate(d)
	f.mapIssues(d.candRep, d.doc, d.entity)
	switch d.intent {
	case intentEdit:
		s.renderForm(w, r, http.StatusOK, v, d, false)
		return
	case intentCheck:
		code := http.StatusOK
		if !valid {
			code = http.StatusUnprocessableEntity
		}
		s.renderForm(w, r, code, v, d, true)
		return
	case intentTest:
		if op.Test == nil {
			http.Error(w, "400 Bad Request: this form has no test", http.StatusBadRequest)
			return
		}
		op.Test(s, w, r, v, d)
		return
	}
	if !valid {
		s.renderForm(w, r, http.StatusUnprocessableEntity, v, d, true)
		return
	}
	rv := s.newReview(d, s.formTitle(d))
	var hint config.Hint
	if op.Hint != nil {
		hint = op.Hint(f)
	}
	for _, l := range config.Summarize(d.file.Doc, d.doc, hint) {
		rv.Summary = append(rv.Summary, summarySegs(l))
	}
	if len(rv.Summary) == 0 {
		rv.Summary = [][]seg{plain("No change to the configuration.")}
	}
	rv.Back = true
	s.formNotices(d, rv)
	s.renderReview(w, r, http.StatusOK, v, rv, d.ret)
}

func (s *Server) formTitle(d *draft) []seg {
	return append(plain("Review: "), d.op.Title(d.form)...)
}

// gone is the message for an op whose target no longer exists (D7 step
// 2).
func gone(kind, name string) []seg {
	label := strings.ToUpper(kind[:1]) + kind[1:]
	return []seg{{Text: label + " "}, {Text: name, Code: true}, {Text: " no longer exists; it was changed outside this page. Reload the page to see the file as it is."}}
}

// changedElsewhere is the message for an op that the current file no
// longer allows.
func changedElsewhere(err error) string {
	msg := err.Error()
	return strings.ToUpper(msg[:1]) + msg[1:] + "; the file was changed outside this page. Reload the page to see it as it is."
}

// formNotices adds the notices of a form op (design 0009 D7): the
// concurrency rows, the canonical rewrite with the comments it removes,
// and, for a delete, the change it also removes.
func (s *Server) formNotices(d *draft, rv *reviewView) {
	op, f := d.op, d.form
	var n []reviewNotice
	now, _ := op.Render(d.file.Doc, f.Target)
	elsewhere := f.differs(now)
	who := entityRef(op.Kind, f.Target)
	switch {
	case op.Delete:
		if len(elsewhere) > 0 {
			n = append(n, reviewNotice{Kind: "warn", Text: append(who, plain(keyList(elsewhere)+" "+was(elsewhere)+
				" changed after you opened this page. Deleting removes that change too; the diff shows it.")...)})
		} else if d.base != d.file.Digest {
			n = append(n, reviewNotice{Kind: "info", Text: plain("The file changed after you opened this page. The delete is yours; the other change stays.")})
		}
	case d.base != d.file.Digest && f.New:
		n = append(n, reviewNotice{Kind: "info", Text: plain("The file changed after you opened this page. The changes below are yours; the other change stays.")})
	case d.base != d.file.Digest:
		mine := f.changedFields()
		var both, kept []formField
		for _, e := range elsewhere {
			if slices.ContainsFunc(mine, func(m formField) bool { return m.Name == e.Name }) {
				both = append(both, e)
			} else {
				kept = append(kept, e)
			}
		}
		if len(both) > 0 {
			n = append(n, reviewNotice{Kind: "warn", Text: append(fieldSubject(op, who, both), plain(" "+was(both)+
				" also changed elsewhere. Applying replaces that change with yours; the diff shows what is lost.")...)})
		}
		text := plain("The file changed after you opened this page. The changes below are yours; the other change stays.")
		if len(kept) > 0 {
			// The fields of this form that changed elsewhere and stay
			// (OI-0006 Y-R5).
			text = append(plain("The file changed after you opened this page. The changes below are yours; the other change stays: "),
				append(fieldSubject(op, who, kept), plain(" "+was(kept)+" changed elsewhere and keeps that change.")...)...)
		}
		if len(both) == 0 || len(kept) > 0 {
			n = append(n, reviewNotice{Kind: "info", Text: text})
		}
	}
	if enc := d.file.Doc.Encode(); !bytes.Equal(enc, d.file.Data) {
		lines, eol := config.HandComments(d.file.Data)
		text := "This save rewrites the file in canonical form: " + plural(lines, "comment line") + " and " +
			plural(eol, "end-of-line comment") + " written by hand are removed. A note on a rule, upstream, or interface keeps a description."
		if lines == 0 && eol == 0 {
			text = "This save rewrites the file in canonical form; only the formatting changes."
		}
		n = append(n, reviewNotice{Kind: "info", Text: plain(text)})
	}
	rv.Notices = append(n, rv.Notices...)
}

// entityRef names the edited entity at the start of a notice, such as
// "Rule `restricted`: "; nothing for a settings page.
func entityRef(kind, name string) []seg {
	if kind == "" || name == "" {
		return nil
	}
	return []seg{{Text: strings.ToUpper(kind[:1]) + kind[1:] + " "}, {Text: name, Code: true}, {Text: ": "}}
}

// fieldSubject names the fields that a concurrency notice is about, as
// the start of a sentence: "Rule `x`: domains and ports" for an entity,
// and the key paths for a settings page, such as "`proxy.remote.ports`",
// so that the sentence has a subject either way (OI-0006 AA-R7).
func fieldSubject(op *formOp, who []seg, fields []formField) []seg {
	if op.Kind != "" {
		return append(slices.Clone(who), plain(keyList(fields))...)
	}
	var out []seg
	for i, f := range fields {
		switch {
		case i == 0:
		case i == len(fields)-1 && len(fields) == 2:
			out = append(out, seg{Text: " and "})
		case i == len(fields)-1:
			out = append(out, seg{Text: ", and "})
		default:
			out = append(out, seg{Text: ", "})
		}
		out = append(out, seg{Text: f.Key, Code: true})
	}
	return out
}

// keyList names fields by their keys: "domains and ports".
func keyList(fields []formField) string {
	keys := make([]string, len(fields))
	for i, f := range fields {
		k := f.Key
		if j := strings.LastIndexByte(k, '.'); j >= 0 {
			k = k[j+1:]
		}
		keys[i] = k
	}
	return joinAnd(keys)
}

func was(fields []formField) string {
	if len(fields) == 1 {
		return "was"
	}
	return "were"
}

func joinAnd(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + " and " + items[1]
	}
	return strings.Join(items[:len(items)-1], ", ") + ", and " + items[len(items)-1]
}

func summarySegs(l config.SummaryLine) []seg {
	out := make([]seg, len(l))
	for i, sp := range l {
		out[i] = seg{Text: sp.Text, Code: sp.Code}
	}
	return out
}

// reviewView is the review of design 0009 D3.11, as a dialog or a page,
// and also a refusal: a review whose candidate cannot be applied.
type reviewView struct {
	// InPage renders the review as a page with an h1; otherwise it is a
	// dialog's content.
	InPage bool
	Title  []seg
	// BakName is the name of the previous version's file.
	BakName string
	Summary [][]seg
	Notices []reviewNotice
	Diff    *diffView
	// Message and Errors say why the candidate cannot be applied.
	Message []seg
	Errors  []string
	// The form that Apply, Back to editing, and Review again post: the
	// fields of the request, and for Apply the candidate and the digest of
	// the file the review compared against (design 0009 D7).
	Carry    []hiddenField
	Text     string
	Reviewed string
	Return   string
	CanApply bool
	Back     bool
	// Again offers Review again after a 409 at apply; Reload offers to
	// reload the page after a 409 at the draft step.
	Again  bool
	Reload string
}

// reviewNotice is one notice of a review: info or warn.
type reviewNotice struct {
	Kind string
	Text []seg
	List []string
}

// carried returns the request's fields that the review's form posts on,
// so that Back to editing and Review again rebuild the form: every field
// but the CSRF token, the intent, the candidate, and the digests of a
// review.
func carried(r *http.Request, keepText bool) []hiddenField {
	var out []hiddenField
	keys := make([]string, 0, len(r.PostForm))
	for k := range r.PostForm {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		switch k {
		case "csrf", "intent", "reviewed":
			continue
		case "text":
			if !keepText {
				continue
			}
		}
		for _, v := range r.PostForm[k] {
			out = append(out, hiddenField{k, v})
		}
	}
	return out
}

// newReview starts the review of a valid candidate with the notices that
// every op shares: the file holds changes that are not running yet, the
// restart-only keys, the admin listener that the next start disables,
// and the validator's warnings.
func (s *Server) newReview(d *draft, title []seg) *reviewView {
	rv := s.refusal(d, title, nil)
	rv.CanApply = true
	rv.Text, rv.Reviewed = string(d.cand), d.file.Digest
	rv.Diff = newDiffView(filepath.Base(d.file.Path), string(d.file.Data), string(d.cand))
	if running := s.backend.Config(); d.file.Valid() && running != nil && d.file.Digest != running.SHA256 {
		rv.Notices = append(rv.Notices, reviewNotice{Kind: "info",
			Text: plain("The file holds changes that are not running yet; applying also loads them.")})
	}
	if start := s.backend.StartConfig(); start != nil {
		if keys := config.RestartRequired(start, d.candCfg); len(keys) > 0 {
			rv.Notices = append(rv.Notices, reviewNotice{Kind: "warn", Text: append(restartTitle(keys), seg{Text: " The other changes apply when you apply them."})})
		}
		if start.Admin.Listen.IsValid() && !d.candCfg.Admin.Listen.IsValid() {
			rv.Notices = append(rv.Notices, reviewNotice{Kind: "warn",
				Text: rich("`admin.listen` is empty: after the next start this page is no longer served.")})
		}
	}
	if w := d.candRep.Warnings; len(w) > 0 {
		rv.Notices = append(rv.Notices, reviewNotice{Kind: "warn", Text: plain("The configuration has " + plural(len(w), "warning") + "."),
			List: issueLines(w)})
	}
	return rv
}

// refusal is a review that cannot be applied, with msg as its reason.
func (s *Server) refusal(d *draft, title, msg []seg) *reviewView {
	bak, err := config.BackupPath(s.backend.ConfigPath())
	if err != nil {
		bak = s.backend.ConfigPath() + ".bak"
	}
	return &reviewView{Title: title, BakName: filepath.Base(bak), Message: msg, Return: d.ret}
}

// refuseReview answers a review that cannot be built because of the file
// as it is now, such as a file that cannot be read: 409 with a link that
// loads the page again.
func (s *Server) refuseReview(w http.ResponseWriter, r *http.Request, v session, d *draft, code int, title, msg []seg) {
	rv := s.refusal(d, title, msg)
	rv.Reload = d.ret
	s.renderReview(w, r, code, v, rv, d.ret)
}

// renderReview renders a review, as the block that the script shows in
// the page's review dialog, or as a page. Its form carries the request's
// fields.
func (s *Server) renderReview(w http.ResponseWriter, r *http.Request, code int, v session, rv *reviewView, ret string) {
	rv.Carry = carried(r, rv.Again && r.PostForm.Get("op") == opText)
	page := pageData{Title: "Review changes", Token: v.csrf, Page: navOf(ret), Data: rv}
	if wantsFragment(r) {
		s.renderFragment(w, code, "review", "review", page)
		return
	}
	rv.InPage = true
	s.render(w, code, "review", page)
}

// formPage is the data of a form op's page.
type formPage struct {
	Form *form
	// View is the op's own data for the page.
	View any
	// Message says why the draft was refused, with Reload, the page to
	// load again.
	Message []seg
	Reload  string
}

// renderForm renders a form op's page from the posted values, or, for a
// script request with issues, only its issues block.
func (s *Server) renderForm(w http.ResponseWriter, r *http.Request, code int, v session, d *draft, block bool) {
	fp := &formPage{Form: d.form}
	if d.op.View != nil && d.doc != nil {
		fp.View = d.op.View(d.doc, d.form)
	}
	page := pageData{Title: d.op.PageTitle, Token: v.csrf, Page: d.op.Nav, Data: fp}
	if block && wantsFragment(r) {
		s.renderFragment(w, code, d.op.Page, d.op.block(), page)
		return
	}
	s.render(w, code, d.op.Page, page)
}

// refuseForm answers 409 for a form op (D7 steps 1 and 2): in the review
// for a review, in the form otherwise.
func (s *Server) refuseForm(w http.ResponseWriter, r *http.Request, v session, d *draft, code int, msg []seg) {
	if d.intent == intentReview {
		rv := s.refusal(d, append(plain("Review: "), d.op.Title(d.form)...), msg)
		rv.Reload = d.ret
		if k, _, ok := editorPath(d.ret); ok {
			rv.Reload = "/" + k + "s"
		}
		s.renderReview(w, r, code, v, rv, d.ret)
		return
	}
	fp := &formPage{Form: d.form, Message: msg, Reload: d.ret}
	if d.op.View != nil && d.file != nil && d.file.Doc != nil {
		fp.View = d.op.View(d.file.Doc, d.form)
	}
	page := pageData{Title: d.op.PageTitle, Token: v.csrf, Page: d.op.Nav, Data: fp}
	if wantsFragment(r) {
		s.renderFragment(w, code, d.op.Page, d.op.block(), page)
		return
	}
	s.render(w, code, d.op.Page, page)
}

// diffView is the unified diff of a review (design 0009 D3.11): the
// changed lines with three lines of context and both line numbers, and
// the unchanged stretches between them collapsed.
type diffView struct {
	File           string
	Added, Removed int
	Parts          []diffPart
}

// diffPart is a run of shown lines, or a collapsed gap of unchanged lines.
type diffPart struct {
	Gap   bool
	Label string
	Lines []diffLine
}

type diffLine struct {
	Class, A, B, Mark, Text string
	NoEOL                   bool
}

func newDiffView(file, old, new string) *diffView {
	lines := textdiff.Diff(old, new)
	dv := &diffView{File: file}
	conv := func(ls []textdiff.Line) []diffLine {
		out := make([]diffLine, len(ls))
		for i, l := range ls {
			dl := diffLine{Text: l.Text, NoEOL: l.NoEOL, Mark: " "}
			if l.A > 0 {
				dl.A = strconv.Itoa(l.A)
			}
			if l.B > 0 {
				dl.B = strconv.Itoa(l.B)
			}
			switch l.Kind {
			case textdiff.Add:
				dl.Class, dl.Mark = "add", "+"
				dv.Added++
			case textdiff.Del:
				dl.Class, dl.Mark = "del", "−"
				dv.Removed++
			}
			out[i] = dl
		}
		return out
	}
	// Only an unchanged stretch longer than 6 lines collapses: between two
	// hunks that is any gap, since closer changes share a hunk; at either
	// end of the file, a gap of more than 3 lines past the hunk's 3 lines
	// of context.
	gap := func(ls []textdiff.Line, edge bool) {
		switch {
		case len(ls) == 0:
		case edge && len(ls) <= 3:
			dv.Parts = append(dv.Parts, diffPart{Lines: conv(ls)})
		default:
			dv.Parts = append(dv.Parts, diffPart{Gap: true, Label: plural(len(ls), "unchanged line"), Lines: conv(ls)})
		}
	}
	hunks := textdiff.Hunks(lines, 3)
	at := 0 // next line of lines
	for i, h := range hunks {
		start := indexOf(lines, h.Lines[0])
		gap(lines[at:start], i == 0)
		dv.Parts = append(dv.Parts, diffPart{Lines: conv(h.Lines)})
		at = start + len(h.Lines)
	}
	gap(lines[at:], len(hunks) > 0)
	return dv
}

// indexOf finds a line of a hunk in the diff by its line numbers.
func indexOf(lines []textdiff.Line, l textdiff.Line) int {
	for i := range lines {
		if lines[i].A == l.A && lines[i].B == l.B && lines[i].Kind == l.Kind {
			return i
		}
	}
	return len(lines)
}

// saveResult is what save did.
type saveResult struct {
	// conflict: the file changed since the caller read it; writeErr: the
	// write failed. Either leaves the file unchanged.
	conflict bool
	writeErr error
	// unsynced: the file holds the text, but the directory sync after the
	// rename failed (OI-0006 U-R1).
	unsynced bool
	// old is the content the write replaced, when save read it.
	old      []byte
	digest   string
	reload   control.ReloadResult
	reloaded bool
}

func (r saveResult) saved() bool { return !r.conflict && r.writeErr == nil }

// save writes a candidate that the caller validated, if the file still
// has the content of digest expect, keeps the previous content in .bak,
// and reloads: the [0005 D10] steps that POST /settings and POST
// /config/apply share, under the settings mutex, one save at a time
// (design 0009 D7).
func (s *Server) save(text []byte, expect, op string) saveResult {
	path := s.backend.ConfigPath()
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	var res saveResult
	if old, err := readConfigFile(path); err == nil && config.Digest(old) == expect {
		res.old = old
	}
	err := config.WriteAtomic(path, text, config.WriteOptions{ExpectSHA256: expect, Backup: true})
	// The file holds the new text even when the directory sync after the
	// rename failed, so that counts as saved and the reload runs (OI-0006
	// U-R1).
	if errors.Is(err, config.ErrNotSynced) {
		s.log.Warn("admin settings saved, but the directory could not be synced", "config", path, "op", op, "error", err.Error())
		res.unsynced, err = true, nil
	}
	switch {
	case errors.Is(err, config.ErrConflict):
		s.log.Warn("admin settings conflict; the file changed since it was read", "config", path, "op", op)
		res.conflict = true
		return res
	case err != nil:
		s.log.Error("admin settings write failed", "config", path, "op", op, "error", err.Error())
		res.writeErr = err
		return res
	}
	res.digest = config.Digest(text)
	s.log.Info("admin settings saved", "config", path, "op", op, "sha256", res.digest)
	res.reload, res.reloaded = s.backend.Reload()
	return res
}

// saveToast reports a save (design 0009 D7). first is the change
// summary's first line and more the number of other lines; listed says
// that the page shows the file's warnings. A reload that refused the file
// gives an error toast that stays and keeps the sync warning (OI-0006
// Z-R8); restart-only keys and an unsynced directory give a warn toast
// that stays; otherwise an ok toast that closes by itself.
func saveToast(res saveResult, first []seg, more int, listed bool) toast {
	unsynced := plain("The file holds the new text, but the directory could not be synced, so a power loss soon after the save could undo it.")
	if !res.reloaded {
		t := toast{Kind: "error", Title: "Saved, but the reload refused the file; the running configuration is unchanged."}
		for i, e := range res.reload.Errors {
			if i == 5 {
				t.Lines = append(t.Lines, plain("and "+plural(len(res.reload.Errors)-5, "more error")))
				break
			}
			t.Lines = append(t.Lines, plain(e))
		}
		if res.unsynced {
			t.Lines = append(t.Lines, unsynced)
		}
		return t
	}
	t := toast{Kind: "ok", Title: "Saved and reloaded."}
	if len(first) > 0 {
		line := first
		if more > 0 {
			line = append(slices.Clone(first), seg{Text: " and " + plural(more, "more change")})
		}
		t.Lines = append(t.Lines, line)
	}
	if res.unsynced {
		t.Kind, t.Title = "warn", "Saved and reloaded; the directory could not be synced."
		t.Lines = append(t.Lines, unsynced)
	}
	if keys := res.reload.RestartRequired; len(keys) > 0 {
		if t.Kind == "ok" {
			t.Title = "Saved and reloaded. Restart required."
		}
		t.Kind = "warn"
		t.Lines = append(t.Lines, append(append(restartTitle(keys), seg{Text: " "}), restartLine()...))
	}
	if n := len(res.reload.Warnings); n > 0 {
		if t.Title == "Saved and reloaded." {
			t.Title = "Saved and reloaded with " + plural(n, "warning") + "."
		}
		if listed {
			t.Lines = append(t.Lines, plain("The file has "+plural(n, "warning")+"; this page lists them."))
		} else {
			for i, w := range res.reload.Warnings {
				if i == 3 {
					t.Lines = append(t.Lines, plain("and "+plural(n-3, "more warning")))
					break
				}
				t.Lines = append(t.Lines, plain(w))
			}
		}
	}
	return t
}

// postApply serves POST /config/apply (design 0009 D7): it writes the
// reviewed candidate, if the file still has the content the review
// compared against, and reloads, then returns to the page the edit
// started on, where a toast reports the result.
func (s *Server) postApply(w http.ResponseWriter, r *http.Request, _ string, v session) {
	pf := r.PostForm
	ret, ok := cleanReturn(pf.Get("return"))
	if !ok {
		http.Error(w, "400 Bad Request: missing or invalid return path", http.StatusBadRequest)
		return
	}
	opName := pf.Get("op")
	op := s.ops[opName]
	if op == nil && opName != opText && opName != opRestore {
		http.Error(w, "400 Bad Request: unknown op", http.StatusBadRequest)
		return
	}
	reviewed := pf.Get("reviewed")
	if !sha256Hex.MatchString(reviewed) {
		http.Error(w, "400 Bad Request: missing or invalid reviewed digest", http.StatusBadRequest)
		return
	}
	// The review's hidden text arrives with CRLF line ends.
	text := []byte(lf(pf.Get("text")))
	d := &draft{opName: opName, ret: ret, base: pf.Get("base"), cand: text, file: &fileState{Path: s.backend.ConfigPath()}}
	title := plain("Review changes")
	if len(text) > config.MaxFileSize {
		rv := s.refusal(d, title, plain(fmt.Sprintf("The configuration is larger than %d KiB; nothing was written.", config.MaxFileSize>>10)))
		s.renderReview(w, r, http.StatusRequestEntityTooLarge, v, rv, ret)
		return
	}
	if !s.validate(d) {
		rv := s.refusal(d, title, plain("The configuration has errors; nothing was written."))
		rv.Errors = issueLines(d.candRep.Errors)
		s.renderReview(w, r, http.StatusUnprocessableEntity, v, rv, ret)
		return
	}
	res := s.save(text, reviewed, opName)
	switch {
	case res.conflict:
		rv := s.refusal(d, plain("The file changed after this review"),
			plain("Another change reached the file after you reviewed this one, so nothing was written. Review again to see your change against the file as it is now."))
		rv.Again = true
		s.renderReview(w, r, http.StatusConflict, v, rv, ret)
		return
	case res.writeErr != nil:
		rv := s.refusal(d, title, plain("The file could not be written and is unchanged: "+res.writeErr.Error()))
		s.renderReview(w, r, http.StatusInternalServerError, v, rv, ret)
		return
	}
	var first []seg
	more := 0
	switch {
	case res.old == nil:
	case opName == opText:
		dv := newDiffView("", string(res.old), string(text))
		first = plain("Edited as text: " + plural(dv.Added, "line") + " added, " + strconv.Itoa(dv.Removed) + " removed.")
	case opName == opRestore:
		first = plain("Restored the previous version.")
	default:
		oldDoc, _, _ := config.DocOf(res.old, "")
		newDoc, _, _ := config.DocOf(text, "")
		if oldDoc != nil && newDoc != nil {
			var hint config.Hint
			if op.Hint != nil {
				hint = op.Hint(postedForm(op, r))
			}
			if lines := config.Summarize(oldDoc, newDoc, hint); len(lines) > 0 {
				first, more = summarySegs(lines[0]), len(lines)-1
			}
		}
	}
	// An edit that renamed or deleted the entity of an editor path returns
	// to the entity's list.
	if kind, name, ok := editorPath(strings.SplitN(ret, "?", 2)[0]); ok && !hasEntity(d.candCfg, kind, name) {
		ret = "/" + kind + "s"
	}
	path, _, _ := strings.Cut(ret, "?")
	s.sessions.setFlash(v.csrf, saveToast(res, first, more, path == "/settings"))
	http.Redirect(w, r, ret, http.StatusSeeOther)
}

// hasEntity reports whether cfg has the named rule, upstream, or
// interface.
func hasEntity(cfg *config.Config, kind, name string) bool {
	switch kind {
	case config.KindRule:
		return slices.ContainsFunc(cfg.Rules, func(r policy.Rule) bool { return r.Name == name })
	case config.KindUpstream:
		_, ok := cfg.Upstream(name)
		return ok
	case config.KindInterface:
		_, ok := cfg.Interface(name)
		return ok
	}
	return false
}

// postReload serves POST /config/reload, the "Reload now" of the banner
// for a file that differs from the running configuration (design 0009
// D3.13): it runs the [0004 D6] reload and returns with a toast.
func (s *Server) postReload(w http.ResponseWriter, r *http.Request, _ string, v session) {
	ret, ok := cleanReturn(r.PostForm.Get("return"))
	if !ok {
		http.Error(w, "400 Bad Request: missing or invalid return path", http.StatusBadRequest)
		return
	}
	s.settingsMu.Lock()
	res, reloaded := s.backend.Reload()
	s.settingsMu.Unlock()
	s.log.Info("admin reload", "ok", reloaded)
	t := saveToast(saveResult{reload: res, reloaded: reloaded}, nil, 0, false)
	switch t.Title {
	case "Saved, but the reload refused the file; the running configuration is unchanged.":
		t.Title = "The reload refused the file; the running configuration is unchanged."
	default:
		t.Title = strings.Replace(t.Title, "Saved and reloaded", "Reloaded", 1)
	}
	s.sessions.setFlash(v.csrf, t)
	http.Redirect(w, r, ret, http.StatusSeeOther)
}

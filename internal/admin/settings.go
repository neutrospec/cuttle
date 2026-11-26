package admin

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/neutrospec/cuttle/internal/config"
)

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// settingsPage is the data of the settings page (design 0005 D10).
type settingsPage struct {
	Path string
	// Text is the editor's content and Base the SHA-256 of the file
	// content it started from.
	Text, Base string
	// Editable is false when the file cannot be read or is too large.
	Editable bool
	// ReadError explains why the file is not editable.
	ReadError string
	// Errors and Warnings are the validator's messages for Text.
	Errors, Warnings []string
	// Notice reports what a POST did; NoticeOK marks success.
	Notice   string
	NoticeOK bool
	// Dirty marks submitted text that was not saved, so that the page's
	// script asks before the operator leaves it (design 0009 D3.13).
	Dirty bool
	// Conflict holds the file's current content after a 409.
	Conflict bool
	DiskText string
	// Backup is the previous version (design 0009 D3.10).
	Backup *backupFile
	// The running configuration's state.
	LoadedAt        time.Time
	Running         string
	StartRestart    []string
	LastReloadError []string
}

// issueLines formats validator messages with their lines, without the
// file name that every message would repeat.
func issueLines(list []config.Issue) []string {
	out := make([]string, len(list))
	for i, is := range list {
		var b strings.Builder
		if is.Line > 0 {
			b.WriteString("line " + strconv.Itoa(is.Line) + ": ")
		}
		if is.Key != "" {
			b.WriteString(is.Key + ": ")
		}
		b.WriteString(is.Msg)
		out[i] = b.String()
	}
	return out
}

// readConfigFile reads the configuration file up to its size limit.
func readConfigFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, config.MaxFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > config.MaxFileSize {
		return nil, fmt.Errorf("the file is larger than %d KiB; edit it in a text editor", config.MaxFileSize>>10)
	}
	return data, nil
}

// settingsBase fills the parts of the page that describe the running
// configuration: the path, when it was loaded, the restart-only keys that
// differ, and the last reload result.
func (s *Server) settingsBase() *settingsPage {
	st := s.backend.Status()
	d := &settingsPage{Path: s.backend.ConfigPath(), LoadedAt: st.Config.LoadedAt, Running: st.Config.SHA256,
		StartRestart: st.Config.RestartRequired, Backup: s.readBackup()}
	if st.Config.LastReloadError != "" {
		d.LastReloadError = strings.Split(st.Config.LastReloadError, "\n")
	}
	return d
}

// validate fills Errors and Warnings for d.Text and reports whether the
// text is valid.
func (d *settingsPage) validate() bool {
	_, r := config.Parse([]byte(d.Text), d.Path)
	d.Errors, d.Warnings = issueLines(r.Errors), issueLines(r.Warnings)
	return len(r.Errors) == 0
}

// renderSettings renders the Advanced page (design 0009 D3.10), or only
// its result block for a script request (design 0009 D10).
func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, code int, v session, d *settingsPage, toasts ...toast) {
	page := pageData{Title: "Advanced", Token: v.csrf, Page: "advanced", Data: d, Toasts: toasts}
	if wantsFragment(r) {
		s.renderFragment(w, code, "settings", "settings-result", page)
		return
	}
	s.render(w, code, "settings", page)
}

// getSettings shows the configuration file in an editor with its SHA-256
// as base, its errors and warnings, and the running configuration's state
// (design 0005 D10). An invalid file is shown with its errors; the running
// configuration stays in force until a valid save.
func (s *Server) getSettings(w http.ResponseWriter, r *http.Request, _ string, v session) {
	d := s.settingsBase()
	data, err := readConfigFile(d.Path)
	if err != nil {
		d.ReadError = err.Error()
		s.renderSettings(w, r, http.StatusOK, v, d)
		return
	}
	d.Text, d.Base, d.Editable = string(data), config.Digest(data), true
	d.validate()
	s.renderSettings(w, r, http.StatusOK, v, d)
}

// submitted reads the editor's text and base from a settings form. The
// browser sends line breaks as CRLF, which become LF. A text over
// config.MaxFileSize gets 413, a base that is not a SHA-256 400; ok is
// false after either answer.
func (s *Server) submitted(w http.ResponseWriter, r *http.Request, v session) (*settingsPage, bool) {
	d := s.settingsBase()
	d.Text = strings.ReplaceAll(r.PostForm.Get("text"), "\r\n", "\n")
	d.Base = r.PostForm.Get("base")
	d.Editable = true
	if len(d.Text) > config.MaxFileSize {
		d.Text, d.Editable = "", false
		d.ReadError = fmt.Sprintf("The submitted configuration is larger than %d KiB; nothing was saved.", config.MaxFileSize>>10)
		s.renderSettings(w, r, http.StatusRequestEntityTooLarge, v, d)
		return nil, false
	}
	if !sha256Hex.MatchString(d.Base) {
		http.Error(w, "400 Bad Request: missing or invalid base", http.StatusBadRequest)
		return nil, false
	}
	d.Dirty = true
	return d, true
}

// postValidate validates the submitted text without saving it.
func (s *Server) postValidate(w http.ResponseWriter, r *http.Request, _ string, v session) {
	d, ok := s.submitted(w, r, v)
	if !ok {
		return
	}
	if !d.validate() {
		d.Notice = "The configuration has errors; nothing was saved."
		s.renderSettings(w, r, http.StatusUnprocessableEntity, v, d)
		return
	}
	d.Notice, d.NoticeOK = "The configuration is valid. It has not been saved.", true
	s.renderSettings(w, r, http.StatusOK, v, d)
}

// postSettings validates the submitted text, writes it atomically if the
// file still has the base content, and reloads (design 0005 D10, 0004 D6,
// D7). Invalid text gets 422 and a changed file 409; neither touches the
// file.
func (s *Server) postSettings(w http.ResponseWriter, r *http.Request, _ string, v session) {
	d, ok := s.submitted(w, r, v)
	if !ok {
		return
	}
	if !d.validate() {
		d.Notice = "The configuration has errors; the file is unchanged."
		s.renderSettings(w, r, http.StatusUnprocessableEntity, v, d)
		return
	}
	res := s.save([]byte(d.Text), d.Base, opText)
	switch {
	case res.conflict:
		d.Conflict = true
		d.Notice = "The file changed since this page loaded, so nothing was saved. Compare it with your text below. Review changes shows your text against the file as it is now; applying it replaces that change."
		if disk, rerr := readConfigFile(d.Path); rerr == nil {
			d.DiskText, d.Base = string(disk), config.Digest(disk)
		} else {
			d.DiskText, d.Editable = rerr.Error(), false
		}
		s.renderSettings(w, r, http.StatusConflict, v, d)
		return
	case res.writeErr != nil:
		d.Notice = "The file could not be written and is unchanged: " + res.writeErr.Error()
		s.renderSettings(w, r, http.StatusInternalServerError, v, d)
		return
	}
	d.Base = res.digest
	running := s.settingsBase()
	d.LoadedAt, d.Running, d.StartRestart, d.LastReloadError, d.Backup = running.LoadedAt, running.Running, running.StartRestart, running.LastReloadError, running.Backup
	d.Dirty = false
	if !res.reloaded {
		d.Notice = "Saved, but the reload refused the file; the running configuration is unchanged: " + strings.Join(res.reload.Errors, "; ")
		if res.unsynced {
			// Both problems stay visible (OI-0006 Z-R8).
			d.Notice += ". The directory could not be synced either, so a power loss soon after the save could undo it."
		}
		s.renderSettings(w, r, http.StatusUnprocessableEntity, v, d)
		return
	}
	s.renderSettings(w, r, http.StatusOK, v, d, saveToast(res, nil, 0, true))
}

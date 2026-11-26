package admin

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/neutrospec/cuttle/internal/config"
)

// TestAdminBackup checks the previous version of design 0009 D3.10 and
// D12 (W12): without .bak the page says so; after a save it offers the
// size, the time, Compare with current, and Restore; /settings/backup
// shows the diff from the current file to it; restore goes through review
// and apply and leaves the replaced file in .bak; a .bak that is a
// symbolic link, a FIFO, writable by group, owned by another user, or
// larger than 256 KiB is not offered, and a restore of it answers 409;
// and a restore review after the file changed shows the replace warning.
func TestAdminBackup(t *testing.T) {
	p := newPipeline(t)
	f := p.f
	bak := f.be.path + ".bak"
	if body := p.get("/settings"); !strings.Contains(body, "No previous version yet. Each save keeps the file it replaces as config.toml.bak.") ||
		strings.Contains(body, `value="restore"`) {
		t.Errorf("without .bak:\n%s", body)
	}
	if body := p.get("/settings/backup"); !strings.Contains(body, "No previous version yet.") || strings.Contains(body, `class="diff"`) {
		t.Errorf("/settings/backup without .bak:\n%s", body)
	}

	// A save makes the previous version.
	edited := strings.Replace(exampleConfig, `default = "direct"`, `default = "block"`, 1)
	p.apply(p.review(with(p.editorForm(), "text", edited), http.StatusOK))
	body := p.get("/settings")
	for _, want := range []string{`<h2 id="bak-h">Previous version</h2>`, `<a class="btn sm" href="/settings/backup">Compare with current</a>`,
		`<input type="hidden" name="op" value="restore">`, "<dt>Size</dt><dd>" + formatBytes(int64(len(exampleConfig))) + "</dd>"} {
		if !strings.Contains(body, want) {
			t.Errorf("/settings after a save lacks %q:\n%s", want, body)
		}
	}
	checkMarkup(t, "advanced with a previous version", body, true, "/settings")
	body = p.get("/settings/backup")
	checkMarkup(t, "previous version", body, true, "/settings")
	if o, n := rebuild(t, body); o != edited || n != exampleConfig {
		t.Errorf("compare with current:\nold %q\nnew %q", o, n)
	}

	// Restore: review, then apply; the replaced file is in .bak.
	restore := formValues(t, body, `<form method="post" action="/config/draft" class="inline-form">`)
	body = p.review(restore, http.StatusOK)
	if !strings.Contains(body, "Restores the previous version saved ") {
		t.Errorf("restore review:\n%s", body)
	}
	if strings.Contains(body, "Back to editing") || !strings.Contains(body, "Apply changes") {
		t.Errorf("restore review buttons:\n%s", body)
	}
	if o, n := rebuild(t, body); o != edited || n != exampleConfig {
		t.Errorf("restore diff:\nold %q\nnew %q", o, n)
	}
	p.apply(body)
	if p.file() != exampleConfig || readFile(t, bak) != edited {
		t.Errorf("after restore: file %q, .bak %q", p.file(), readFile(t, bak))
	}
	if body := p.get("/settings"); !strings.Contains(body, "Restored the previous version.") {
		t.Errorf("toast after restore:\n%s", body)
	}

	// A restore review after the file changed: the replace warning.
	restore = formValues(t, p.get("/settings"), `<form method="post" action="/config/draft" class="inline-form">`)
	p.write(strings.Replace(exampleConfig, `name = "cuttle-edge"`, `name = "cuttle-other"`, 1))
	if body := p.review(restore, http.StatusOK); !strings.Contains(body, "The file changed after you opened this page. Applying replaces that change with the previous version") {
		t.Errorf("restore after a change:\n%s", body)
	}
	p.write(exampleConfig)

	// Files that are not offered. Each case sets .bak up, checks that the
	// pages do not offer it and say why, that a restore answers 409, and
	// that the file is unchanged.
	refused := func(name, reason string) {
		t.Helper()
		body := p.get("/settings")
		if !strings.Contains(body, reason) || strings.Contains(body, `value="restore"`) {
			t.Errorf("%s: /settings offers it or does not say why:\n%s", name, body)
		}
		if body := p.get("/settings/backup"); !strings.Contains(body, reason) || strings.Contains(body, `class="diff"`) {
			t.Errorf("%s: /settings/backup shows it", name)
		}
		res, body := p.post("/config/draft", with(restore, "intent", "review", "base", config.Digest([]byte(exampleConfig))))
		if res.StatusCode != http.StatusConflict || !strings.Contains(body, "The previous version cannot be restored.") {
			t.Errorf("%s: restore: %d:\n%s", name, res.StatusCode, body)
		}
		if p.file() != exampleConfig {
			t.Fatalf("%s: the file changed", name)
		}
	}
	reset := func() {
		t.Helper()
		os.Remove(bak)
	}

	// A symbolic link to a valid file.
	reset()
	target := filepath.Join(t.TempDir(), "elsewhere.toml")
	if err := os.WriteFile(target, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, bak); err != nil {
		t.Fatal(err)
	}
	refused("symbolic link", "config.toml.bak is a symbolic link, so it is not offered.")

	// A FIFO, which an open without O_NONBLOCK would wait on.
	reset()
	if err := syscall.Mkfifo(bak, 0o600); err != nil {
		t.Fatal(err)
	}
	refused("FIFO", "config.toml.bak is not a regular file, so it is not offered.")

	// Writable by group.
	reset()
	if err := os.WriteFile(bak, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bak, 0o620); err != nil {
		t.Fatal(err)
	}
	refused("group-writable", "config.toml.bak is writable by group or others (mode 0620), so it is not offered;")

	// Owned by another user, as Fstat reports it: the server's own user ID
	// stands in for another one.
	if err := os.Chmod(bak, 0o600); err != nil {
		t.Fatal(err)
	}
	f.s.uid = func() int { return os.Getuid() + 1 }
	refused("another owner", "config.toml.bak is owned by another user, so it is not offered.")
	f.s.uid = os.Getuid

	// Over 256 KiB.
	if err := os.WriteFile(bak, []byte(edited+"#"+strings.Repeat("x", config.MaxFileSize)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	refused("too large", "config.toml.bak is larger than 256 KiB, so it is not offered.")

	// An invalid previous version is shown, but not offered for restore.
	if err := os.WriteFile(bak, []byte("name = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body = p.get("/settings")
	if !strings.Contains(body, "It has errors, so it cannot be restored.") || strings.Contains(body, `value="restore"`) {
		t.Errorf("invalid previous version:\n%s", body)
	}
	if body := p.get("/settings/backup"); !strings.Contains(body, "The previous version has errors, so it cannot be restored.") {
		t.Errorf("/settings/backup of an invalid previous version:\n%s", body)
	}
	res, body := p.post("/config/draft", with(restore, "intent", "review", "base", config.Digest([]byte(exampleConfig))))
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "The previous version has errors, so it cannot be restored.") ||
		strings.Contains(body, "Apply changes") {
		t.Errorf("restore of an invalid previous version: %d:\n%s", res.StatusCode, body)
	}
}

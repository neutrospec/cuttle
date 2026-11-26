package admin

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAdminSettingsDirSync checks OI-0006 U-R1 (design 0009 W14) on the
// settings page: when the directory sync after the rename fails, the save
// counts as saved, the reload runs, and the page says that the directory
// could not be synced. A configuration directory with mode 0300 allows the
// renames but not the open of the directory.
func TestAdminSettingsDirSync(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens any directory")
	}
	f := newFixture(t, true)
	b := f.browser()
	b.login()
	csrf, base := b.settingsForm()
	dir := filepath.Dir(f.be.path)
	if err := os.Chmod(dir, 0o300); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	valid := strings.Replace(fakeConfig, `default = "direct"`, `default = "block"`, 1)
	res := b.do(req{method: http.MethodPost, path: "/settings", form: url.Values{"csrf": {csrf}, "text": {valid}, "base": {base}}})
	body := bodyOf(t, res)
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "Saved and reloaded; the directory could not be synced.") {
		t.Errorf("save: %d:\n%s", res.StatusCode, body)
	}
	if got := readFile(t, f.be.path); got != valid {
		t.Errorf("file %q", got)
	}
	if f.be.reloadCount() != 1 || f.be.Config().Proxy.Default.String() != "block" {
		t.Errorf("reload: %d, default %v", f.be.reloadCount(), f.be.Config().Proxy.Default)
	}
}

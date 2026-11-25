package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

// macInSample finds hardware addresses in the sample files in every form
// they use: colon, dash, dotted, and twelve hex digits.
var macInSample = regexp.MustCompile(`(?i)\b[0-9a-f]{2}([:-][0-9a-f]{2}){5}\b|\b[0-9a-f]{4}\.[0-9a-f]{4}\.[0-9a-f]{4}\b|\b[0-9a-f]{12}\b`)

// macFragments returns every five-character piece of every spelling of
// the MACs in the files: colon, dash, and dotted forms and bare hex digits,
// in lower case. A message holding one quotes part of a MAC.
func macFragments(t *testing.T, paths []string) []string {
	t.Helper()
	seen := map[string]bool{}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range macInSample.FindAllString(string(data), -1) {
			hex := strings.ToLower(strings.NewReplacer(":", "", "-", "", ".", "").Replace(m))
			pairs := make([]string, 6)
			for i := range pairs {
				pairs[i] = hex[2*i : 2*i+2]
			}
			for _, form := range []string{hex, strings.Join(pairs, ":"), strings.Join(pairs, "-"),
				hex[0:4] + "." + hex[4:8] + "." + hex[8:12]} {
				for i := 0; i+5 <= len(form); i++ {
					seen[form[i:i+5]] = true
				}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for f := range seen {
		out = append(out, f)
	}
	if len(out) == 0 {
		t.Fatal("no MAC found in the samples")
	}
	return out
}

// TestConfigInterfaces checks the interface errors and warnings of design
// 0008 D6 (I1): each error sample fails with its key path and line, each
// warning sample passes with its warning, and no message about an
// [[interface]] key or an interfaces list carries any part of a sample's
// MAC in any case or separator form, TOML syntax errors included
// (TASK-0008 K-R2). Messages about other keys echo what the operator
// wrote, MAC-shaped text included (OI-0006 O-R1).
func TestConfigInterfaces(t *testing.T) {
	invalid, _ := filepath.Glob("testdata/interfaces/invalid/*.toml")
	warn, _ := filepath.Glob("testdata/interfaces/warn/*.toml")
	valid, _ := filepath.Glob("testdata/interfaces/valid/*.toml")
	if len(invalid) < 30 || len(warn) < 7 || len(valid) < 1 {
		t.Fatalf("samples: %d invalid, %d warn, %d valid", len(invalid), len(warn), len(valid))
	}
	fragments := macFragments(t, append(append(append([]string{}, invalid...), warn...), valid...))
	noMAC := func(path string, list []Issue) {
		t.Helper()
		for _, is := range list {
			if !interfaceKey(is.Key) && !strings.HasPrefix(is.Key, "interface[") && !strings.Contains(is.Key, "interfaces[") {
				continue
			}
			text := strings.ToLower(is.Msg)
			for _, f := range fragments {
				if strings.Contains(text, f) {
					t.Errorf("%s: message holds MAC fragment %q: %s", path, f, is)
				}
			}
		}
	}
	for _, path := range invalid {
		want, data := readExpectations(t, path)
		if want[0].kind != "want" {
			t.Fatalf("%s: invalid sample with %q", path, want[0].kind)
		}
		cfg, r := Parse(data, path)
		if cfg != nil || len(r.Errors) == 0 {
			t.Errorf("%s: accepted", path)
			continue
		}
		checkExpectations(t, path, want, r)
		noMAC(path, r.Errors)
		noMAC(path, r.Warnings)
	}
	for _, path := range warn {
		want, data := readExpectations(t, path)
		cfg, r := Parse(data, path)
		if cfg == nil || len(r.Errors) > 0 {
			t.Errorf("%s: rejected:\n%s", path, issues(r.Errors))
			continue
		}
		checkExpectations(t, path, want, r)
		noMAC(path, r.Warnings)
	}

	// Two upstreams with one address and different names and lists pass.
	data, _ := os.ReadFile(valid[0])
	cfg, r := Parse(data, valid[0])
	if cfg == nil {
		t.Fatalf("%s: rejected:\n%s", valid[0], issues(r.Errors))
	}
	noMAC(valid[0], r.Warnings)
	wired, _ := cfg.Upstream("proxy-wired")
	anyIf, _ := cfg.Upstream("proxy-any")
	if wired.Address != anyIf.Address || strings.Join(wired.Interfaces, ",") != "eth-a,eth-b" ||
		strings.Join(anyIf.Interfaces, ",") != "eth-a,eth-b,wifi" {
		t.Errorf("upstreams %+v %+v", wired, anyIf)
	}
	if len(cfg.Interfaces) != 3 || cfg.Interfaces[1].MAC.String() != "00:00:5e:00:53:0b" || cfg.Interfaces[2].Device != "en0" ||
		cfg.Interfaces[2].Kind != "wifi" || cfg.Interfaces[0].Kind != "wired" {
		t.Errorf("definitions %+v", cfg.Interfaces)
	}
	if strings.Join(cfg.Proxy.Interfaces, ",") != "eth-a,eth-b,wifi" || cfg.Proxy.InterfaceAttemptTimeout != 3*time.Second ||
		cfg.Proxy.InterfaceDemotion != 30*time.Second {
		t.Errorf("proxy %v %v %v", cfg.Proxy.Interfaces, cfg.Proxy.InterfaceAttemptTimeout, cfg.Proxy.InterfaceDemotion)
	}
	if strings.Join(cfg.Rules[0].Interfaces, ",") != "eth-b" || cfg.Rules[1].Interfaces != nil {
		t.Errorf("rule lists %v %v", cfg.Rules[0].Interfaces, cfg.Rules[1].Interfaces)
	}
	if ps := cfg.PolicySettings(); strings.Join(ps.Interfaces, ",") != "eth-a,eth-b,wifi" {
		t.Errorf("policy list %v", ps.Interfaces)
	}
	if !wired.Equal(wired) || wired.Equal(anyIf) {
		t.Errorf("Upstream.Equal")
	}

	// interface_demotion = "0s" disables demotion.
	cfg, r = Parse([]byte("name = \"cuttle-test\"\n[proxy]\ndefault = \"direct\"\ninterface_demotion = \"0s\"\n"), "zero.toml")
	if cfg == nil || cfg.Proxy.InterfaceDemotion != 0 {
		t.Errorf("demotion 0s: %v", r.Err())
	}

	// Other syntax messages keep their position and the parser's text.
	_, r = Parse([]byte("name = \"cuttle-test\"\n[proxy]\ndefault = direct\n"), "plain.toml")
	if len(r.Errors) != 1 || r.Errors[0].Line != 3 || r.Errors[0].Key != "proxy.default" ||
		!strings.HasPrefix(r.Errors[0].Msg, "TOML syntax: ") || r.Errors[0].Msg == interfaceSyntaxHint {
		t.Errorf("plain syntax error: %v", issues(r.Errors))
	}
	// The parser itself quotes the start of an unquoted MAC; the samples
	// above show that cuttle passes it on only outside the interface keys,
	// as misspelled-table-unquoted-mac.toml does.
	var raw map[string]any
	_, err := toml.Decode("[[interfce]]\nmac = 00:00:5e:00:53:0f\n", &raw)
	if err == nil || !strings.Contains(err.Error(), "00:00:5") {
		t.Logf("parser message changed: %v", err)
	}
}

// TestConfigEchoesOperatorText checks OI-0006 O-R1: since the operator who
// writes the configuration is trusted, messages about keys other than the
// interface keys show names, instance names, IPv6 addresses, and even
// MAC-shaped values as written, where the earlier blanket redaction hid
// them (design 0008 D6).
func TestConfigEchoesOperatorText(t *testing.T) {
	head := "name = \"cuttle-test\"\n[proxy]\ndefault = \"direct\"\n"
	for _, c := range []struct{ text, key, want string }{
		{"name = \"Cuttle-0123456789ab\"\n[proxy]\ndefault = \"direct\"\n", "name", `"Cuttle-0123456789ab"`},
		{head + "[[upstream]]\nname = \"u\"\naddress = \"00:11:22:33:44:55::1\"\ndisclosure = \"none\"\n",
			"upstream[1].address", `"00:11:22:33:44:55::1"`},
		{head + "[[upstream]]\nname = \"u\"\naddress = \"00:00:5e:00:53:28\"\ndisclosure = \"none\"\n",
			"upstream[1].address", `"00:00:5e:00:53:28"`},
		{head + "[[rule]]\nname = \"x\"\nhosts = [\"00-00-5e-00-53-2a.example\", \"0000.5E00.5326\"]\naction = \"direct\"\n",
			"rule[1].hosts[2]", `"0000.5E00.5326"`},
		{head + "[[rule]]\nname = \"0123456789ab\"\nhosts = [\"a.example\"]\naction = \"upstream:00005e005326\"\n",
			"rule[1].action", "00005e005326"},
		{head + "\"00:00:5e:00:53:23\" = 1\n", "proxy.00:00:5e:00:53:23", "unknown key"},
		{head + "[notes]\nadapter = 00-00-5e-00-53-10\n", "notes.adapter", "TOML syntax: "},
	} {
		_, r := Parse([]byte(c.text), "echo.toml")
		found := false
		for _, is := range r.Errors {
			if is.Key == c.key && strings.Contains(is.Msg, c.want) {
				found = true
			}
		}
		if !found {
			t.Errorf("%q: want %s with %q; got:\n%s", c.text, c.key, c.want, issues(r.Errors))
		}
	}
	// A repeated pattern keeps the rule names and the pattern as written.
	_, r := Parse([]byte(head+"[[rule]]\nname = \"0123456789ab\"\ndomains = [\"00-00-5e-00-53-29.example\"]\naction = \"direct\"\n"+
		"[[rule]]\nname = \"b\"\ndomains = [\"00-00-5e-00-53-29.example\"]\naction = \"block\"\n"), "echo.toml")
	if len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0].Msg, `"00-00-5e-00-53-29.example" repeats rule "0123456789ab"`) {
		t.Errorf("repeat warning: %s", issues(r.Warnings))
	}
}

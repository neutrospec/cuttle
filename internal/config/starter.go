package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// starterTemplate is the starter configuration of design 0004 D5. NAME is
// replaced with cuttle-<6 random hex digits>.
const starterTemplate = `# cuttle configuration. Check changes with ` + "`cuttle check`" + ` and apply them
# with ` + "`cuttle reload`" + `. Rules are tried in order; the first match wins.

name = "NAME"  # unique per instance; appears in Via headers

[proxy]
listen = ["127.0.0.1:3128"]
default = "direct"  # direct, block, or upstream:<name>
# Interfaces for direct routes, in priority order; list them with
# ` + "`cuttle interfaces`" + `. Unset, macOS chooses as for other applications.
# interfaces = ["eth-a", "wifi"]

[admin]
listen = "127.0.0.1:3129"  # set a password with ` + "`cuttle admin passwd`" + `

# localhost and loopback addresses, on any port. Other names that resolve
# to loopback or to this Mac's own addresses stay refused.
[[rule]]
name = "loopback"
hosts = ["localhost"]
cidrs = ["127.0.0.0/8", "::1/128"]
action = "direct"
allow_loopback = true

# Devices on the home network by name, such as a NAS. Private IP literals
# and this Mac's own address work without a rule; names that resolve to
# private addresses need one.
# [[rule]]
# name = "home-lan"
# domains = ["lan", "local", "home.arpa"]
# action = "direct"
# allow_private = true

# Another cuttle instance as an upstream:
# [[upstream]]
# name = "gateway"
# address = "10.8.0.6:3128"
# disclosure = "via"  # "none" for an egress proxy
#
# [[rule]]
# name = "restricted"
# domains = ["internal.example"]
# action = "upstream:gateway"

# Network interfaces named for proxy.interfaces and rule lists:
# [[interface]]
# name = "eth-a"
# mac = "00:00:5e:00:53:0a"  # from ` + "`cuttle interfaces`" + `
#
# [[interface]]
# name = "wifi"
# device = "en0"
# kind = "wifi"
`

// Starter returns the starter configuration with a new random instance
// name.
func Starter() ([]byte, error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	return []byte(strings.Replace(starterTemplate, "NAME", "cuttle-"+hex.EncodeToString(b[:]), 1)), nil
}

// StarterTemplate returns the template with NAME in place of the instance
// name, for tests.
func StarterTemplate() string { return starterTemplate }

// EnsureStarter writes the starter configuration to path if no file exists
// there. It reports whether it wrote one. Missing directories are created
// with mode 0700; the file gets mode 0600 through the atomic write. When
// the directory sync after the rename fails, the file is in place: it
// reports true with an error that wraps ErrNotSynced, which the caller
// reports as a warning (OI-0006 Z-R4).
func EnsureStarter(path string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	target, err := resolveLinks(path)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return false, err
	}
	data, err := Starter()
	if err != nil {
		return false, err
	}
	if err := WriteAtomic(path, data, WriteOptions{}); err != nil {
		return errors.Is(err, ErrNotSynced), err
	}
	return true, nil
}

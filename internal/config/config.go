// Package config holds the configuration schema, its validation, the
// starter file, and the atomic write procedure (design 0004).
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/neutrospec/cuttle/internal/iface"
	"github.com/neutrospec/cuttle/internal/policy"
)

// MaxFileSize is the largest configuration file cuttle reads.
const MaxFileSize = 256 << 10

// Config is a validated configuration with defaults applied.
type Config struct {
	Name      string
	Proxy     Proxy
	Upstreams []Upstream
	Rules     []policy.Rule
	Admin     Admin
	Stats     Stats
	Log       Log
	// Interfaces are the [[interface]] definitions in file order (design
	// 0008 D2).
	Interfaces []Interface

	// SHA256 is the hex digest of the file content.
	SHA256 string
}

// Proxy is the [proxy] table.
type Proxy struct {
	Listen          []netip.AddrPort
	Default         policy.Route
	ConnectTimeout  time.Duration
	ResponseTimeout time.Duration
	IdleTimeout     time.Duration
	MaxConnections  int
	Remote          Remote
	// Interfaces is the default interface list of direct routes; empty
	// follows system routing (design 0008 D6).
	Interfaces []string
	// InterfaceAttemptTimeout is the connect time each interface group but
	// the last gets (design 0008 D8).
	InterfaceAttemptTimeout time.Duration
	// InterfaceDemotion is how long a demoted interface stays behind the
	// others; 0 disables demotion (design 0008 D9).
	InterfaceDemotion time.Duration
}

// Remote is the [proxy.remote] table.
type Remote struct {
	Sources                 []netip.Prefix
	Ports                   []uint16
	Default                 policy.Route
	AllowDirect             bool
	MaxConnectionsPerSource int
}

// Disclosure modes of an upstream (design 0003 D1).
const (
	DisclosureVia  = "via"
	DisclosureNone = "none"
)

// Upstream is one [[upstream]] entry.
type Upstream struct {
	Name            string
	Address         string // host:port
	Disclosure      string // DisclosureVia or DisclosureNone
	ConnectTimeout  time.Duration
	ResponseTimeout time.Duration
	UnreachableHold time.Duration
	// Interfaces is the upstream's own interface list; nil follows system
	// routing (design 0008 D11).
	Interfaces []string
}

// Equal reports whether two upstream entries have the same settings.
func (u Upstream) Equal(o Upstream) bool {
	return u.Name == o.Name && u.Address == o.Address && u.Disclosure == o.Disclosure &&
		u.ConnectTimeout == o.ConnectTimeout && u.ResponseTimeout == o.ResponseTimeout &&
		u.UnreachableHold == o.UnreachableHold && slices.Equal(u.Interfaces, o.Interfaces) &&
		(u.Interfaces == nil) == (o.Interfaces == nil)
}

// Interface is one [[interface]] definition (design 0008 D2).
type Interface struct {
	Name   string
	MAC    net.HardwareAddr // nil when unset
	Device string           // "" when unset
	Kind   string           // wired, wifi, cellular, or "" when unset
}

// Def returns the definition for the interface monitor.
func (i Interface) Def() iface.Def {
	k, _ := iface.ParseKind(i.Kind)
	return iface.Def{Name: i.Name, MAC: i.MAC, Device: i.Device, Kind: k}
}

// Defs returns every definition for the interface monitor.
func (c *Config) Defs() []iface.Def {
	out := make([]iface.Def, len(c.Interfaces))
	for i, d := range c.Interfaces {
		out[i] = d.Def()
	}
	return out
}

// Interface returns the named definition.
func (c *Config) Interface(name string) (Interface, bool) {
	for _, d := range c.Interfaces {
		if d.Name == name {
			return d, true
		}
	}
	return Interface{}, false
}

// Admin is the [admin] table. An empty Listen disables the admin UI.
type Admin struct {
	Listen netip.AddrPort
}

// Stats is the [stats] table.
type Stats struct {
	RetentionDays   int
	MaxHostsPerHour int
}

// Log is the [log] table.
type Log struct {
	Level string
}

// PolicySettings returns the routing settings for policy.New.
func (c *Config) PolicySettings() policy.Settings {
	return policy.Settings{
		Rules:             c.Rules,
		Default:           c.Proxy.Default,
		RemoteDefault:     c.Proxy.Remote.Default,
		RemotePorts:       c.Proxy.Remote.Ports,
		RemoteAllowDirect: c.Proxy.Remote.AllowDirect,
		Interfaces:        c.Proxy.Interfaces,
	}
}

// RestartRequired lists the restart-only keys whose values in next differ
// from the running ones (design 0004 D3, D6 step 5): proxy.listen and
// admin.listen.
func RestartRequired(running, next *Config) []string {
	out := []string{}
	if !slices.Equal(running.Proxy.Listen, next.Proxy.Listen) {
		out = append(out, "proxy.listen")
	}
	if running.Admin.Listen != next.Admin.Listen {
		out = append(out, "admin.listen")
	}
	return out
}

// Upstream returns the named upstream.
func (c *Config) Upstream(name string) (Upstream, bool) {
	for _, u := range c.Upstreams {
		if u.Name == name {
			return u, true
		}
	}
	return Upstream{}, false
}

// Issue is one validation error or warning.
type Issue struct {
	File string
	Line int    // 0 when unknown
	Key  string // key path such as "rule[2].action"; empty for the file
	Msg  string
}

func (i Issue) String() string {
	var b strings.Builder
	if i.File != "" {
		b.WriteString(i.File)
		if i.Line > 0 {
			fmt.Fprintf(&b, ":%d", i.Line)
		}
		b.WriteString(": ")
	} else if i.Line > 0 {
		fmt.Fprintf(&b, "line %d: ", i.Line)
	}
	if i.Key != "" {
		b.WriteString(i.Key)
		b.WriteString(": ")
	}
	b.WriteString(i.Msg)
	return b.String()
}

// Report holds the issues found in one configuration.
type Report struct {
	Errors   []Issue
	Warnings []Issue
}

// Err returns an error listing every validation error, or nil.
func (r *Report) Err() error {
	if len(r.Errors) == 0 {
		return nil
	}
	msgs := make([]string, len(r.Errors))
	for i, e := range r.Errors {
		msgs[i] = e.String()
	}
	return &ValidationError{Issues: r.Errors, msg: strings.Join(msgs, "\n")}
}

// ValidationError is returned by Load when the file has errors.
type ValidationError struct {
	Issues []Issue
	msg    string
}

func (e *ValidationError) Error() string { return e.msg }

// Load reads and validates the file at path. It returns the report even
// when the configuration is invalid; the Config is nil then.
func Load(path string) (*Config, *Report, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxFileSize+1))
	if err != nil {
		return nil, nil, err
	}
	if len(data) > MaxFileSize {
		r := &Report{Errors: []Issue{{File: path, Msg: fmt.Sprintf("file is larger than %d KiB", MaxFileSize>>10)}}}
		return nil, r, r.Err()
	}
	cfg, r := Parse(data, path)
	return cfg, r, r.Err()
}

// Digest returns the hex SHA-256 of data.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ErrInsecureFile is returned by CheckFileSecurity.
var ErrInsecureFile = errors.New("insecure configuration file")

// CheckFileSecurity refuses a configuration file that another user could
// change (design 0006 D2): writable by group or others, or owned by another
// user.
func CheckFileSecurity(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%w: %s is writable by group or others (mode %04o); run chmod 600 %s", ErrInsecureFile, path, fi.Mode().Perm(), path)
	}
	if uid, ok := fileOwner(fi); ok && uid != os.Getuid() {
		return fmt.Errorf("%w: %s is owned by another user (uid %d)", ErrInsecureFile, path, uid)
	}
	return nil
}

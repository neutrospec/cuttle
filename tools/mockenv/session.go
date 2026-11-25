package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"squid/internal/testenv"
)

// Names and addresses of the session's test network (design 0007 D3). Every
// name resolves to originAddr, and the test literals route to the origins,
// so no request ever leaves loopback.
const (
	originAddr  = "203.0.113.10" // public test literal; every name below
	privateAddr = "10.20.0.5"    // private test literal for the LNA step

	hostOrigin    = "origin.cuttle.test"
	hostPlain     = "plain.cuttle.test"
	hostWS        = "ws.cuttle.test"
	hostInspected = "inspected.cuttle.test"
	hostLNA       = "lna.cuttle.test"
	hostBlocked   = "blocked.cuttle.test"
)

var sessionNames = []string{"cuttle.test", hostOrigin, hostPlain, hostWS, hostInspected, hostLNA, hostBlocked}

// Instance names of the three cuttle configurations.
const (
	nameSingle  = "cuttle-single"
	nameEdge    = "cuttle-edge"
	nameGateway = "cuttle-gateway"
)

// Mock names; each records into <dir>/<name>.jsonl.
const (
	mockUpstream     = "upstream"      // the single instance's recording upstream
	mockEgressDirect = "egress-direct" // clients use it directly
	mockEgressChain  = "egress-chain"  // the gateway's egress upstream
	mockInspect      = "inspect"       // the inspecting egress mock, outside TLS
	mockDecrypted    = "inspect-decrypted"
	mockOrigin       = "origin" // both origins
)

// sessionOptions configure a session.
type sessionOptions struct {
	// Dir is the session directory, which must not exist yet. "" creates
	// a new mockenv-* directory under $TMPDIR.
	Dir string
	// Modes are recording-upstream modes by target name, applied to every
	// recording upstream. parseMode refuses 407 (design 0007 D6, E4).
	Modes map[string]testenv.Mode
	// gatewayDisclosure replaces the gateway's disclosure = "none" toward
	// egress-chain; only tests set it, to show that check-disclosure
	// catches a disclosing chain.
	gatewayDisclosure string
}

// instance is one cuttle configuration of the session with its own
// directories (design 0007 D4).
type instance struct {
	Role       string `json:"role"` // single, edge, gateway
	Name       string `json:"name"` // the instance name
	Listen     string `json:"listen"`
	Config     string `json:"config"`
	Home       string `json:"home"`
	ConfigHome string `json:"xdg_config_home"`
	DataHome   string `json:"xdg_data_home"`
	StateHome  string `json:"xdg_state_home"`
	TmpDir     string `json:"tmpdir"`
	RuntimeDir string `json:"xdg_runtime_dir"`
	Log        string `json:"log"`
}

// Env is the complete environment the instance runs with.
func (in instance) Env() []string {
	return []string{
		"PATH=/usr/bin:/bin",
		"HOME=" + in.Home,
		"XDG_CONFIG_HOME=" + in.ConfigHome,
		"XDG_DATA_HOME=" + in.DataHome,
		"XDG_STATE_HOME=" + in.StateHome,
		"XDG_RUNTIME_DIR=" + in.RuntimeDir,
		"TMPDIR=" + in.TmpDir,
	}
}

// session is one running mockenv session (design 0007 D8).
type session struct {
	Dir           string              `json:"dir"`
	OriginCA      string              `json:"origin_ca"`
	InspectCA     string              `json:"inspection_ca"`
	SPKI          []string            `json:"spki"`
	Mocks         map[string]string   `json:"mocks"` // name → 127.0.0.1:port
	Instances     map[string]instance `json:"instances"`
	HTTPSOrigin   string              `json:"https_origin"`
	HTTPOrigin    string              `json:"http_origin"`
	RecordedFiles []string            `json:"recordings"`

	net          *testenv.Net
	httpsOrigin  *testenv.Origin
	httpOrigin   *testenv.Origin
	upstream     *testenv.RecordingUpstream
	egressDirect *testenv.RecordingUpstream
	egressChain  *testenv.RecordingUpstream
	inspect      *testenv.InspectingProxy
	inspectRoot  *testenv.CA
	writers      []*jsonl
	closeOnce    sync.Once
}

// startSession generates fresh roots, starts the mocks on loopback, chooses
// cuttle's ports, and writes the configurations, root certificates, and
// session.json into the session directory.
func startSession(opt sessionOptions) (s *session, err error) {
	dir := opt.Dir
	if dir == "" {
		if dir, err = os.MkdirTemp("", "mockenv-"); err != nil {
			return nil, err
		}
	} else if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, fmt.Errorf("session directory: %w", err)
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return nil, err
	}
	s = &session{Dir: dir, Mocks: map[string]string{}, Instances: map[string]instance{}, net: testenv.NewNet()}
	defer func() {
		if err != nil {
			s.Close()
		}
	}()

	originCA, err := newSessionCA("mockenv origin root")
	if err != nil {
		return nil, err
	}
	s.inspectRoot, err = newSessionCA("mockenv inspection root")
	if err != nil {
		return nil, err
	}
	s.OriginCA = filepath.Join(dir, "origin-ca.pem")
	s.InspectCA = filepath.Join(dir, "inspection-ca.pem")
	for path, ca := range map[string]*testenv.CA{s.OriginCA: originCA, s.InspectCA: s.inspectRoot} {
		if err := os.WriteFile(path, ca.PEM, 0o600); err != nil {
			return nil, err
		}
	}
	for _, ca := range []*testenv.CA{originCA, s.inspectRoot} {
		for _, pub := range []any{ca.Cert.PublicKey, &ca.LeafKey.PublicKey} {
			h, err := spkiHash(pub)
			if err != nil {
				return nil, err
			}
			s.SPKI = append(s.SPKI, h)
		}
	}

	rec := func(name string) (*jsonl, error) {
		w, err := newJSONL(filepath.Join(dir, name+".jsonl"))
		if err != nil {
			return nil, err
		}
		s.writers = append(s.writers, w)
		s.RecordedFiles = append(s.RecordedFiles, w.path)
		return w, nil
	}
	originRec, err := rec(mockOrigin)
	if err != nil {
		return nil, err
	}
	leafNames := []string{"*.cuttle.test", "cuttle.test", "localhost", "127.0.0.1", originAddr, privateAddr}
	s.httpsOrigin, err = testenv.StartOrigin(testenv.OriginOptions{
		TLS: originCA, Names: leafNames, Record: originRecorder(originRec, "https"),
	})
	if err != nil {
		return nil, err
	}
	s.httpOrigin, err = testenv.StartOrigin(testenv.OriginOptions{Record: originRecorder(originRec, "http")})
	if err != nil {
		return nil, err
	}
	s.HTTPSOrigin, s.HTTPOrigin = s.httpsOrigin.Addr.String(), s.httpOrigin.Addr.String()
	for _, name := range sessionNames {
		s.net.Map(name, originAddr)
	}
	for _, a := range []string{originAddr, privateAddr} {
		s.net.Route(a+":443", s.httpsOrigin.Addr)
		s.net.Route(a+":80", s.httpOrigin.Addr)
	}

	startUpstream := func(name string) (*testenv.RecordingUpstream, error) {
		w, err := rec(name)
		if err != nil {
			return nil, err
		}
		u, err := testenv.StartRecordingUpstream(testenv.UpstreamOptions{Net: s.net, Record: proxyRecorder(w)})
		if err != nil {
			return nil, err
		}
		// The test literals pass so that the LNA step reaches the origins;
		// every other name outside cuttle.test gets 403 (OI-0006 V4).
		u.SetMode(originAddr, testenv.Pass())
		u.SetMode(privateAddr, testenv.Pass())
		for host, m := range opt.Modes {
			u.SetMode(host, m)
		}
		s.Mocks[name] = u.Addr.String()
		return u, nil
	}
	if s.upstream, err = startUpstream(mockUpstream); err != nil {
		return nil, err
	}
	if s.egressDirect, err = startUpstream(mockEgressDirect); err != nil {
		return nil, err
	}
	if s.egressChain, err = startUpstream(mockEgressChain); err != nil {
		return nil, err
	}
	inspectRec, err := rec(mockInspect)
	if err != nil {
		return nil, err
	}
	decRec, err := rec(mockDecrypted)
	if err != nil {
		return nil, err
	}
	s.inspect, err = testenv.StartInspectingProxy(testenv.InspectingOptions{
		Root: s.inspectRoot, Origins: originCA.Pool(), Net: s.net,
		Record: proxyRecorder(inspectRec), RecordDecrypted: decryptedRecorder(decRec),
	})
	if err != nil {
		return nil, err
	}
	s.Mocks[mockInspect] = s.inspect.Addr.String()

	for _, role := range []string{"single", "edge", "gateway"} {
		in, err := s.newInstance(role)
		if err != nil {
			return nil, err
		}
		s.Instances[role] = in
	}
	gwDisclosure := opt.gatewayDisclosure
	if gwDisclosure == "" {
		gwDisclosure = "none"
	}
	configs := map[string]string{
		"single":  singleConfig(s.Instances["single"].Listen, s.Mocks[mockUpstream]),
		"edge":    edgeConfig(s.Instances["edge"].Listen, s.Instances["gateway"].Listen),
		"gateway": gatewayConfig(s.Instances["gateway"].Listen, s.Mocks[mockEgressChain], s.Mocks[mockInspect], gwDisclosure),
	}
	for role, text := range configs {
		if err := os.WriteFile(s.Instances[role].Config, []byte(text), 0o600); err != nil {
			return nil, err
		}
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "session.json"), append(data, '\n'), 0o600); err != nil {
		return nil, err
	}
	return s, nil
}

// newSessionCA generates a root whose leaves share one key, so Chrome can
// accept them through --ignore-certificate-errors-spki-list.
func newSessionCA(name string) (*testenv.CA, error) {
	ca, err := testenv.GenerateCA(name)
	if err != nil {
		return nil, err
	}
	if ca.LeafKey, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
		return nil, err
	}
	return ca, nil
}

// spkiHash is the base64 SHA-256 of a public key's SubjectPublicKeyInfo,
// the form Chrome's --ignore-certificate-errors-spki-list takes.
func spkiHash(pub any) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(der)
	return base64.StdEncoding.EncodeToString(sum[:]), nil
}

// newInstance chooses a listen port and creates the instance's directories
// inside the session directory, with XDG_RUNTIME_DIR in a fresh 0700
// cuttle-* directory under /tmp, as binary tests do (design 0007 D4).
func (s *session) newInstance(role string) (instance, error) {
	port, err := freePort()
	if err != nil {
		return instance{}, err
	}
	base := filepath.Join(s.Dir, role)
	in := instance{
		Role:       role,
		Name:       map[string]string{"single": nameSingle, "edge": nameEdge, "gateway": nameGateway}[role],
		Listen:     netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port).String(),
		Config:     filepath.Join(s.Dir, role+".toml"),
		Home:       filepath.Join(base, "home"),
		ConfigHome: filepath.Join(base, "home", ".config"),
		DataHome:   filepath.Join(base, "home", ".local", "share"),
		StateHome:  filepath.Join(base, "home", ".local", "state"),
		TmpDir:     filepath.Join(base, "tmp"),
	}
	in.Log = filepath.Join(in.StateHome, "cuttle", "cuttle.log")
	for _, d := range []string{in.Home, in.TmpDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return instance{}, err
		}
	}
	rt, err := os.MkdirTemp("/tmp", "cuttle-")
	if err != nil {
		return instance{}, err
	}
	in.RuntimeDir = rt
	return in, os.Chmod(rt, 0o700)
}

// freePort chooses a port by binding 127.0.0.1:0 and releasing it. It never
// returns 3128 or 3129, the ports of a live installation.
func freePort() (uint16, error) {
	for {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return 0, err
		}
		port := uint16(l.Addr().(*net.TCPAddr).Port)
		l.Close()
		if port != 3128 && port != 3129 {
			return port, nil
		}
	}
}

// The configurations use default = "block" with rules only for cuttle.test
// and loopback, so a client's background requests to other names never
// leave cuttle (OI-0006 V4). The single instance adds one rule for the two
// test literals of the LNA step, which reach only the mock origins.

func singleConfig(listen, upstream string) string {
	return `# mockenv: the single instance of the M1 client check (design 0007 D8).
name = "` + nameSingle + `"

[proxy]
listen = ["` + listen + `"]
default = "block"

[admin]
listen = ""

[[upstream]]
name = "recording"
address = "` + upstream + `"
disclosure = "via"

[[rule]]
name = "loopback"
hosts = ["localhost"]
cidrs = ["127.0.0.0/8", "::1/128"]
action = "direct"
allow_loopback = true

[[rule]]
name = "blocked"
hosts = ["` + hostBlocked + `"]
action = "block"

[[rule]]
name = "cuttle-test"
domains = ["cuttle.test"]
action = "upstream:recording"

# The LNA step's public and private test literals; the recording upstream
# passes them to the mock origins (OI-0006 V6).
[[rule]]
name = "test-literals"
cidrs = ["` + originAddr + `/32", "` + privateAddr + `/32"]
action = "upstream:recording"
`
}

func edgeConfig(listen, gateway string) string {
	return `# mockenv: the edge instance of the M1 client check (design 0007 D8).
name = "` + nameEdge + `"

[proxy]
listen = ["` + listen + `"]
default = "block"

[admin]
listen = ""

[[upstream]]
name = "gateway"
address = "` + gateway + `"
disclosure = "via"

[[rule]]
name = "cuttle-test"
domains = ["cuttle.test"]
action = "upstream:gateway"
`
}

func gatewayConfig(listen, egress, inspect, disclosure string) string {
	return `# mockenv: the gateway instance of the M1 client check (design 0007 D8).
name = "` + nameGateway + `"

[proxy]
listen = ["` + listen + `"]
default = "block"

[admin]
listen = ""

# egress-chain, the recording upstream that check-disclosure compares with
# egress-direct.
[[upstream]]
name = "egress"
address = "` + egress + `"
disclosure = "` + disclosure + `"

# The inspecting egress mock, for the Chain case.
[[upstream]]
name = "inspect"
address = "` + inspect + `"
disclosure = "none"

[[rule]]
name = "inspected"
hosts = ["` + hostInspected + `"]
action = "upstream:inspect"

[[rule]]
name = "cuttle-test"
domains = ["cuttle.test"]
action = "upstream:egress"
`
}

// Close stops the mocks, closes the recordings, and removes the runtime
// directories. The session directory stays as evidence.
func (s *session) Close() {
	s.closeOnce.Do(func() {
		for _, u := range []*testenv.RecordingUpstream{s.upstream, s.egressDirect, s.egressChain} {
			if u != nil {
				u.Close()
			}
		}
		if s.inspect != nil {
			s.inspect.Close()
		}
		for _, o := range []*testenv.Origin{s.httpsOrigin, s.httpOrigin} {
			if o != nil {
				o.Close()
			}
		}
		for _, w := range s.writers {
			w.Close()
		}
		for _, in := range s.Instances {
			if in.RuntimeDir != "" {
				os.RemoveAll(in.RuntimeDir)
			}
		}
	})
}

// field is one header field line as a mock received it.
type field struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// record is one request as a mock proxy received it: one JSON line in
// <dir>/<mock>.jsonl with the method, the target exactly as sent, the
// header fields in received order, and the body after transfer decoding
// (design 0007 D8). Byte slices are base64 in JSON.
type record struct {
	Seq     int     `json:"seq"`
	Conn    int     `json:"conn"`
	Method  string  `json:"method"`
	Target  string  `json:"target"`
	Proto   string  `json:"proto"`
	Fields  []field `json:"fields"`
	Body    []byte  `json:"body"`
	Trailer []field `json:"trailer,omitempty"`
}

func toFields(fs []testenv.Field) []field {
	out := make([]field, 0, len(fs))
	for _, f := range fs {
		out = append(out, field{Name: f.Name, Value: f.Value})
	}
	return out
}

func proxyRecorder(w *jsonl) func(testenv.ProxyRequest) {
	return func(r testenv.ProxyRequest) {
		w.write(func(seq int) any {
			return record{Seq: seq, Conn: r.Conn, Method: r.Method, Target: r.Target, Proto: r.Proto,
				Fields: toFields(r.Fields), Body: r.Body, Trailer: toFields(r.Trailer)}
		})
	}
}

// originRecord is one request as an origin received it.
type originRecord struct {
	Seq    int         `json:"seq"`
	Origin string      `json:"origin"` // https or http
	Conn   int         `json:"conn"`
	Method string      `json:"method"` // WS for a WebSocket message
	URI    string      `json:"uri"`
	Host   string      `json:"host"`
	Header http.Header `json:"header,omitempty"`
	Body   []byte      `json:"body"`
}

func originRecorder(w *jsonl, kind string) func(testenv.Request) {
	return func(r testenv.Request) {
		w.write(func(seq int) any {
			return originRecord{Seq: seq, Origin: kind, Conn: r.Conn, Method: r.Method, URI: r.URI,
				Host: r.Host, Header: r.Header, Body: r.Body}
		})
	}
}

// decryptedRecord is one request the inspecting egress mock read inside a
// tunnel.
type decryptedRecord struct {
	Seq    int         `json:"seq"`
	Conn   int         `json:"conn"`
	Method string      `json:"method"`
	URI    string      `json:"uri"`
	Host   string      `json:"host"`
	Header http.Header `json:"header"`
}

func decryptedRecorder(w *jsonl) func(testenv.Decrypted) {
	return func(d testenv.Decrypted) {
		w.write(func(seq int) any {
			return decryptedRecord{Seq: seq, Conn: d.Conn, Method: d.Method, URI: d.URI, Host: d.Host, Header: d.Header}
		})
	}
}

// jsonl appends one JSON value per line to a file and syncs nothing: each
// line is one write, so a reader sees whole lines.
type jsonl struct {
	path string
	mu   sync.Mutex
	f    *os.File
	seq  int
	err  error
}

func newJSONL(path string) (*jsonl, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &jsonl{path: path, f: f}, nil
}

func (w *jsonl) write(build func(seq int) any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return
	}
	w.seq++
	data, err := json.Marshal(build(w.seq))
	if err == nil {
		_, err = w.f.Write(append(data, '\n'))
	}
	if err != nil && w.err == nil {
		w.err = err
		fmt.Fprintf(os.Stderr, "mockenv: recording to %s failed: %v\n", w.path, err)
	}
}

func (w *jsonl) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f != nil {
		w.f.Close()
		w.f = nil
	}
}

// readRecords reads a proxy mock's JSONL file.
func readRecords(path string) ([]record, error) {
	return readJSONL[record](path)
}

func readJSONL[T any](path string) ([]T, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []T
	for i, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var v T
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, i+1, err)
		}
		out = append(out, v)
	}
	return out, nil
}

// parseMode parses a recording-upstream mode: tunnel, buffered, silent,
// reset, or a status code from 200 to 599, which answers with that status
// without any outbound connection. Any 407 is refused, because a browser or
// an Apple-framework client may use a mockenv session and must never see a
// proxy challenge (design 0007 D6, E4).
func parseMode(s string) (testenv.Mode, error) {
	v := strings.ToLower(strings.TrimSpace(s))
	switch v {
	case "tunnel", "pass":
		return testenv.Pass(), nil
	case "buffered":
		return testenv.Buffered([]byte("early")), nil
	case "silent":
		return testenv.Silent(), nil
	case "reset":
		return testenv.Reset(), nil
	}
	code, err := strconv.Atoi(strings.TrimPrefix(v, "status:"))
	if err != nil {
		return testenv.Mode{}, fmt.Errorf("unknown mode %q: want tunnel, buffered, silent, reset, or a status code", s)
	}
	if code == http.StatusProxyAuthRequired {
		return testenv.Mode{}, errors.New("mode 407 refused: no mockenv session ever answers 407 (design 0007 D6)")
	}
	if code < 200 || code > 599 {
		return testenv.Mode{}, fmt.Errorf("status %d outside 200-599", code)
	}
	if code/100 == 2 {
		return testenv.PassStatus(code, ""), nil
	}
	return testenv.Status(code, ""), nil
}

// modeFlags collects repeated -mode name=MODE flags.
type modeFlags map[string]testenv.Mode

func (m modeFlags) String() string { return "" }

func (m modeFlags) Set(v string) error {
	name, mode, ok := strings.Cut(v, "=")
	name = strings.ToLower(strings.TrimSpace(name))
	if !ok || name == "" {
		return fmt.Errorf("want name=MODE, got %q", v)
	}
	md, err := parseMode(mode)
	if err != nil {
		return err
	}
	m[name] = md
	return nil
}

// print writes what a person needs to run the M1 client check by
// hand: the roots, the SPKI hashes, the mocks, and each cuttle instance
// with its environment and command.
func (s *session) print(w io.Writer) {
	fmt.Fprintf(w, "mockenv session %s\n\n", s.Dir)
	fmt.Fprintf(w, "origin root:     %s\ninspection root: %s\n", s.OriginCA, s.InspectCA)
	fmt.Fprintf(w, "Chrome: --ignore-certificate-errors-spki-list=%s\n\n", strings.Join(s.SPKI, ","))
	fmt.Fprintf(w, "origins: https %s, http %s; every *.cuttle.test name in use, %s, and %s route to them\n",
		s.HTTPSOrigin, s.HTTPOrigin, originAddr, privateAddr)
	for _, m := range []string{mockUpstream, mockEgressDirect, mockEgressChain, mockInspect} {
		fmt.Fprintf(w, "mock %-14s http://%s  records %s\n", m, s.Mocks[m], filepath.Join(s.Dir, m+".jsonl"))
	}
	fmt.Fprintln(w)
	for _, role := range []string{"single", "edge", "gateway"} {
		in := s.Instances[role]
		fmt.Fprintf(w, "cuttle %s (%s) listens on %s; config %s\n", role, in.Name, in.Listen, in.Config)
		fmt.Fprintf(w, "  env -i %s <cuttle> serve --config %s\n", strings.Join(in.Env(), " "), in.Config)
	}
	fmt.Fprintf(w, "\nafter the non-disclosure runs: go run ./tools/mockenv check-disclosure -dir %s\n", s.Dir)
}

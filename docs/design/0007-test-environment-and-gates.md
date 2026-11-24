# 0007 — Test environment and gates

**Status** — Proposed (TASK-0001, 2026-11-25; revised in iteration 2).

## Context

There is no local test network, corporate proxy, or second site to test
against. The project must therefore provide controlled origins and
upstream proxies, including a TLS-intercepting one, and run chained cuttle
instances against them. A request reaching the corporate hop must carry no
trace of the cuttle hops ([0003 D5][d3-5]), so the mocks record exactly
what they receive. Research found two hazards: a mock that answers `407` to an
Apple-framework client opens a credential dialog on the user's desktop, and
browser checks can open windows ([LESSONS](../LESSONS.md),
[research 0002 §6][r2-6]).

This record defines the test layers, the mocks, name-resolution control,
the chain and loop setups, the clients, safety rules, the T0 to T3 gates,
and the client checks at M1 and M3 close.

## Decisions

### D1. Test layers

| Layer | Contents | Tier |
| --- | --- | --- |
| Unit | Authority parsing, `Via` editing, policy, configuration validation, paths, aggregation, rotation, password hashing; no sockets | T0 (`go test -short`) |
| Integration | In-process cuttle instances with mocks on `127.0.0.1`, driven by Go, raw-socket, curl, and WebSocket clients | T1 |
| Binary | Tests that build `./cmd/cuttle` and run it as a process: first run, CLI, control socket, signals | T1 |
| Client | Headless Chrome and curl against cuttle binaries and `tools/mockenv` at M1 close; Chrome, Safari, and curl at M3 close | T2 |
| Performance | Throughput, latency, and resource measurements | T3 |

Integration and binary tests call `testing.Short()` and skip under `-short`.

### D2. Mocks

All mocks live in `internal/testenv` and are started by tests on
`127.0.0.1:0`. Every mock records the bytes it receives exactly.

| Mock | Behavior | Records |
| --- | --- | --- |
| CA set | Per test run: an origin root and a corporate root, ECDSA P-256. Leaves have a SAN (names, and `127.0.0.1` where needed), the serverAuth EKU, and 24-hour validity. Kept in memory; written as PEM to `t.TempDir()` only for curl. | — |
| HTTPS origin | TLS with a leaf for `*.cuttle.test`. `/echo` returns method, path, field lines, and body; `/bytes?n=` sends n bytes; `/sink` counts an upload; `/slow?ms=` delays the response; `/chunked` answers with a chunked body and a trailer; `/ws` is a WebSocket echo. | Raw request bytes; connections |
| HTTP origin | The same endpoints over plain HTTP, `/ws` for `ws://`, and `/ws.html`, a page that opens `ws://<host>/ws` and sends one message. | Same |
| Recording upstream | A raw-socket HTTP proxy. Per target name the test selects `tunnel` (`200`), a status (`201`, `299`, `200` with `Content-Length: 3`, `403`, `407`, `502`, `504`), `buffered` (`200` followed at once by server bytes), `silent` (reads the head, never answers), or `reset`. Absolute-form requests are forwarded to the origin unchanged, or answered with the chosen status. | Every request head and body, byte for byte, per connection and in order |
| Intercepting upstream | Accepts plain-HTTP proxy connections. On `CONNECT` it answers `200`, terminates TLS with a leaf for the SNI name signed by the corporate root, and forwards the decrypted HTTP/1.1 requests to the origin, trusting the origin root. Names on its refuse list get `403`. Absolute-form `http://` is proxied normally. | Raw `CONNECT` heads; decrypted request lines and `Host` |

A refused upstream is an address whose port the test bound and closed.

### D3. Name resolution and dial control

`testenv.Net` implements the `Network` seam of
[0002](0002-proxy-core-and-routing.md#interfaces):

- `LookupNetIP` answers from a map of names to addresses. An unknown name
  returns `*net.DNSError` with `IsNotFound`. A name can be given a delay or
  an error.
- `DialContext` translates a test address and port to a real mock listener
  on `127.0.0.1`. An unmapped address is refused; a `blackhole` address
  blocks until the context deadline.
- Every lookup and dial is recorded in order, so a test can assert that a
  block or an upstream failure made zero direct dials.

| Addresses | Class | Use |
| --- | --- | --- |
| `203.0.113.0/24`, `2001:db8::/32` | public | Origins on direct routes, the bracketless IPv6 case |
| `10.8.0.1`, `10.8.0.6` | private | Home and office WireGuard addresses |
| `10.20.0.0/16` | private | Private destination tests |
| `127.0.0.1`, `::1` | loopback | Rebinding tests and the `loopback` rule |
| `169.254.1.1`, `fe80::1`, `0.0.0.0` | unusable | Prohibited destination tests |
| `192.0.2.50` | self | Returned by the injected `SelfAddrs` |

Only `Network` and `SelfAddrs` are injected. The address classifier and all
policy code are production code. `cmd/cuttle` wires the real resolver and
dialer; internal packages have no default `Network`, so a test cannot reach
real DNS by accident. T0 and T1 use no real DNS, internet, or proxy.

### D4. Instances, chains, and loops

- Tests create cuttle instances in-process with `proxy.New(cfg, deps)` and
  pass listeners on `127.0.0.1:0`. `testenv.SourceListener` wraps a
  listener and reports a chosen peer address, such as `10.8.0.1`, so the
  instance classifies that client as remote.
- Chain: client → home (local client; `office` upstream with
  `trace = "via"`) → office (sees home as `10.8.0.1`; `corp` upstream
  with `trace = "none"`) → intercepting or recording upstream → origin.
  Upstream addresses in the configurations are test addresses
  (`10.8.0.6:3128`) that `testenv.Net` routes to the real listeners.
- No-trace chain ([0003 U12][d3-ac]): the home client is a raw-socket
  client behind a `SourceListener` with a distinctive address, so the test
  can search every recorded header value for that address and both
  instance names, and can send fields that Go's client would not, such as
  its own `Via` and no `User-Agent`.
- Loop: home routes `loop.cuttle.test` to office, and office routes it to
  home, both with `trace = "via"`; home's second listener reports
  office's address. The expected result is `502 proxy_loop_detected` after
  exactly two `CONNECT` requests ([0003 U8][d3-ac]).
- Binary tests cannot inject `Network`. They use `block` rules, the
  `loopback` rule, and routes to a recording upstream on `127.0.0.1`,
  which needs no address check and reaches origins through its own test
  resolver.
- Binary tests never touch a live installation:
  - `HOME`, `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, and `XDG_STATE_HOME` point
    to directories under `t.TempDir()`.
  - `XDG_RUNTIME_DIR` points to a directory made with
    `os.MkdirTemp("/tmp", "cuttle-")`, mode 0700, so the socket path stays
    under the 104-byte limit and never resolves to the live
    `$TMPDIR/cuttle/control.sock`.
  - Listen addresses use ports the test chose by binding `127.0.0.1:0` and
    releasing it; no test uses 3128 or 3129. `admin.listen` is `""` unless
    the test exercises the admin UI.

### D5. Clients

- **Go**: `http.Transport` with a fixed `Proxy`, `OnProxyConnectResponse`
  to capture `CONNECT` status codes, and the test roots in `RootCAs`.
- **Raw socket**: writes and reads exact bytes, for the exact `200`,
  pipelining, the no-trace chain, and byte counters.
- **curl**: `/usr/bin/curl`, or the path in `CUTTLE_CURL`, run with an
  environment holding only `PATH` and a temporary `HOME`, and with
  `--proxy http://127.0.0.1:<port>`, `--cacert`, `-sS`, and `-w` for
  `%{http_connect}` and `%{http_code}`. curl sends host names to the proxy,
  so cuttle resolves them through `testenv.Net`. Without curl, tests skip,
  unless `CUTTLE_REQUIRE_CURL=1`, which the T1 gate sets.
- **WebSocket**: a raw `CONNECT` to port 80 or 443, then
  `golang.org/x/net/websocket` over the tunnel, with TLS for `wss`; and an
  absolute-form upgrade on a direct route.
- **Headless Chrome**: only in the client checks of D8.
- No Apple-framework client runs in T0, T1, or the M1 client check.

### D6. Safety rules

- A mock answers `407` only in T1 tests with Go or curl clients.
  `tools/mockenv` refuses any `407` mode, so no browser session ever sees a
  proxy challenge ([LESSONS](../LESSONS.md)).
- T0 and T1 never start a browser or another GUI process.
- The user approved the M1 client check as specified in D8: headless
  Chrome with `--headless=new`, `--use-mock-keychain`, and a throwaway
  profile under a temporary directory, which opens no window, plus
  `/usr/bin/curl`. It uses no Safari, no system proxy setting, and no
  keychain change.
- M3 browser steps need user approval first. Safari steps change system
  proxy settings only after approval, and the tester restores them
  afterwards.
- Generated keys and certificates exist only in memory, in `t.TempDir()`,
  or in mockenv's directory under `$TMPDIR`. `.gitignore` excludes `*.pem`,
  `*.key`, `*.crt`, and `*.p12`, and T0 fails if Git tracks such a file.

### D7. Gates

T0, from the repository root:

```sh
go version
test -z "$(gofmt -l .)"
go mod tidy -diff
go vet ./...
go build ./...
go test -short -count=1 ./...
test -z "$(git ls-files '*.pem' '*.key' '*.crt' '*.p12')"
test -z "$(go list -deps ./cmd/cuttle | grep -E 'internal/testenv|x/net/websocket')"
```

T1:

```sh
CUTTLE_REQUIRE_CURL=1 go test -race -count=1 -timeout 10m ./...
```

- The M1 implementer adds `scripts/gate`, a POSIX `sh` script whose `t0`
  and `t1` subcommands run exactly these commands, and documents it in the
  README. Evidence records the macOS version, `go version`, and
  `/usr/bin/curl --version`.
- T0 applies to source, dependency, toolchain, and build changes. T1
  applies to proxy, routing, configuration, upstream, statistics, admin,
  and CLI behavior. Docs-only changes record `gates: none`.
- T2 at M1 close: the D8 M1 client check with headless Chrome and curl.
  This is the milestone acceptance that AGENTS.md requires.
- T2 at M2 close: the headless Chrome admin check
  ([0005](0005-statistics-and-admin.md#acceptance-criteria) A10).
- T2 at M3 close and every later milestone: the D8 M3 matrix, the Homebrew
  checks ([0006](0006-packaging-paths-and-lifecycle.md#acceptance-criteria)
  P8–P12), and the deployment check.
- T3 applies to any claim about throughput, latency, or resource use, and
  before numeric targets are set. A Go load client run against
  `tools/mockenv` measures tunnel throughput, `CONNECT` setup latency (p50
  and p99), resident memory, and open descriptors, under `brew services`
  with `ProcessType` unset and `Interactive`. Raw results are kept with the
  inputs.

### D8. Client checks

`go run ./tools/mockenv` starts the D2 mocks with fresh roots in a new
directory under `$TMPDIR`, writes every recorded request to files there,
and prints cuttle configurations for a home and an office instance, the
root certificate paths, and the SPKI hashes for Chrome. Each cuttle binary
gets its own `XDG_*` directories and test-chosen ports, as in D4.

Headless Chrome runs as
`"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"` with
`--headless=new --use-mock-keychain --user-data-dir=<new temporary
directory> --no-first-run --proxy-server=http://127.0.0.1:<port>
--proxy-bypass-list="<-loopback>"
--ignore-certificate-errors-spki-list=<hashes> --virtual-time-budget=5000
--dump-dom <url>`. `<-loopback>` removes Chrome's implicit loopback bypass
([research 0002 §5][r2-5]). curl uses `--proxy`, `--cacert`, and `-w` as in
D5.

M1 client check, against mocks only:

| Case | Target | Route | Evidence |
| --- | --- | --- | --- |
| Direct | `https://127.0.0.1:<origin port>/echo` | The `loopback` rule | Chrome's DOM and curl show the echo; decision `ok`, action `direct` |
| Block | `https://blocked.cuttle.test/` | `block` rule | Chrome's DOM is empty; curl's `%{http_connect}` is `403`; decision `blocked`; no mock received anything |
| Upstream | `https://origin.cuttle.test/echo` | Recording upstream, `trace = "via"` | Echo shown; the mock recorded the `CONNECT` with one cuttle `Via` entry |
| WebSocket | `http://ws.cuttle.test/ws.html` | Recording upstream | Chrome only: the mock recorded `CONNECT ws.cuttle.test:80`, and the origin recorded the page's message |
| Chain | `https://origin.cuttle.test/chain` | Home → office (`none`) → intercepting upstream | Echo shown through the corporate root; the intercepting mock's `CONNECT` head has no `Via` |
| No trace | `https://origin.cuttle.test/x` and `http://plain.cuttle.test/x` | Client → recording upstream directly, then client → home → office (`none`) → recording upstream | For Chrome and for curl, the chain's requests carry no header from the [0003 D5][d3-5] list and no value containing either instance name or the home client's address, and have the same method, target, and body bytes as the direct run. Other header differences are reported for information. |

M3 matrix, with Chrome, Safari after approval, and `/usr/bin/curl`, and with
both "Web proxy (HTTP)" and "Secure web proxy (HTTPS)" set to
`127.0.0.1:3128` for Safari ([research 0002 §5][r2-5]):

| Case | Target | Route | Expected |
| --- | --- | --- | --- |
| Direct HTTPS | `https://example.com/` | `direct` | Loads with its public certificate; decision `ok` |
| Direct HTTP | `http://example.com/` | `direct` | Loads; plain-HTTP decision |
| Upstream HTTPS | `https://example.org/` | Recording upstream passing allowlisted public names through | Loads with its public certificate; the mock recorded the `CONNECT`; no direct dial |
| WebSocket | `http://ws.cuttle.test/ws.html` | Recording upstream | The origin recorded the page's message, and the mock recorded the `CONNECT` to port 80 or an absolute-form upgrade, whichever the client sent |
| Block | `https://example.net/` | `block` | Chrome `ERR_TUNNEL_CONNECTION_FAILED`, Safari generic failure, curl `403`; decision `blocked`; no dial |
| Chain | `https://origin.cuttle.test/` | Home → office (`none`) → intercepting | Loads only when the corporate root is trusted (Chrome SPKI flag, Safari keychain after approval, curl `--cacert`) |
| No trace | As at M1 | As at M1 | The M1 properties, now including Safari |
| IPv6 literal | `https://[2001:db8::1]/` | Recording upstream, status `403` | The recorded `CONNECT` spelling ([OI-0002][oi]) |
| Admin UI | `http://127.0.0.1:3129/` | — | Login and dashboard work |

Results go into [COMPATIBILITY.md](../COMPATIBILITY.md) with client and
macOS versions, the tested commit, and evidence links.

## Acceptance criteria

| ID | Criterion | Check |
| --- | --- | --- |
| E1 | Mock self-tests pass: the intercepting upstream's leaves have a SAN, the serverAuth EKU, and at most 825 days of validity; the recording upstream records requests byte for byte per connection; `testenv.Net` records lookups and dials. | T1 `go test ./internal/testenv/...` |
| E2 | `cmd/cuttle` links neither `internal/testenv` nor `x/net/websocket`. | T0 command above |
| E3 | Git tracks no key or certificate file. | T0 command above |
| E4 | `tools/mockenv` rejects a `407` mode. | T1 `TestMockenvRefuses407` |
| E5 | The full T1 run completes within its 10-minute timeout. | T1 command above |
| E6 | The binary-test helper refuses to start cuttle unless `HOME`, every `XDG_*` directory, and `XDG_RUNTIME_DIR` are test-owned temporary directories and the configuration avoids ports 3128 and 3129. | T0 `TestBinaryHelperIsolation` |

## Alternatives considered

- **A real DNS server in tests.** Needs a DNS library or privileged
  resolver changes; the `Network` seam covers the same paths.
- **mitmproxy as the intercepting upstream.** Research used it, but it is a
  Python dependency outside Go tests; a Go mock is enough for a blind
  tunnel.
- **Docker or VM networks.** Heavier than the project allows, and not
  needed while cuttle's code paths take an injected network.
- **Chrome against public sites at M1.** Mocks keep the M1 check offline and
  let it compare recorded bytes; public sites wait for M3.

[d3-5]: 0003-upstream-chaining-and-exposure.md#d5-no-trace-toward-the-corporate-upstream
[d3-ac]: 0003-upstream-chaining-and-exposure.md#acceptance-criteria
[oi]: ../OPEN_ISSUES.md
[r2-5]: ../research/0002-proxy-chaining-and-interception.md#5-how-macos-clients-choose-and-talk-to-the-proxy
[r2-6]: ../research/0002-proxy-chaining-and-interception.md#6-what-clients-show-for-proxy-generated-failures

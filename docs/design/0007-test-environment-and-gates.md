# 0007 — Test environment and gates

**Status** — Proposed (TASK-0001, 2026-11-25; revised in iterations 2
and 3).

## Context

There is no local test network, egress proxy, or second site to test
against. The project must therefore provide controlled origins and
upstream proxies, including a TLS-inspecting one, and run chained cuttle
instances against them. A request reaching the egress hop must disclose no
intermediary ([0003 D5][d3-5]), so the mocks record exactly what they
receive. Research found two hazards: a mock that answers `407` to an
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
| CA set | Per test run: an origin root and an inspection root CA, ECDSA P-256. Leaves have a SAN (names, and `127.0.0.1` where needed), the serverAuth EKU, and 24-hour validity. Kept in memory; written as PEM to `t.TempDir()` only for curl. | — |
| HTTPS origin | TLS with a leaf for `*.cuttle.test`. `/echo` returns method, path, field lines, and body; `/bytes?n=` sends n bytes; `/sink` counts an upload; `/slow?ms=` delays the response; `/status?code=` answers with that status, and with `407` only in a test that enables it, which `tools/mockenv` never does (D6); `/chunked` answers with a chunked body and a trailer; `/hints` sends a `103 Early Hints` response before its `200`; `/ws` is a WebSocket echo. | Raw request bytes; connections |
| HTTP origin | The same endpoints over plain HTTP, `/ws` for `ws://`, and `/ws.html`, a page that opens `ws://<host>/ws` and sends one message. | Same |
| Recording upstream | A raw-socket HTTP proxy. Per target name the test selects `tunnel` (`200`), a status (`201`, `299`, `200` with `Content-Length: 3`, `403`, `407`, `502`, `504`), `buffered` (`200` followed at once by server bytes), `silent` (reads the head, never answers), or `reset`. A name without a chosen mode is tunnelled when it is `cuttle.test` or below it and gets `403` otherwise, as the research recorder did. Absolute-form requests, upgrades included, are forwarded to the origin unchanged, or answered with the chosen status. The origin's informational responses, such as `100 Continue` and `103 Early Hints`, are relayed before its final response. In `tools/mockenv` only, it can also pass an allowlist of public names (default `example.org`) through to the internet for the M3 matrix; tests never enable this. | Every request head and body, byte for byte, per connection and in order |
| Inspecting egress mock | Accepts plain-HTTP proxy connections. On `CONNECT` it answers `200`, terminates TLS with a leaf for the SNI name signed by the inspection root CA, and forwards the decrypted HTTP/1.1 requests to the origin, trusting the origin root. Names on its refuse list get `403`. Absolute-form `http://` is proxied normally. | Raw `CONNECT` heads; decrypted request lines and `Host` |

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
- `DialContext` takes the interface binding of [0008 D7][d8-7] and records
  it with the dial. A test can give an interface and address its own
  behavior: route, refuse, fail with a chosen errno, or blackhole. Without
  one, a bound dial behaves like an unbound dial to that address.

`testenv.Interfaces` is the injected interface source of
[0008](0008-interface-selection.md#interfaces): the test sets its rows,
sends change notices, and can make its `Read` fail. Tests also inject the
timer that drives the notice debounce and the safety re-read.

| Addresses | Class | Use |
| --- | --- | --- |
| `203.0.113.0/24`, `2001:db8::/32` | public | Origins on direct routes, the bracketless IPv6 case |
| `10.8.0.1`, `10.8.0.6` | private | Edge-side and gateway private-link addresses |
| `10.20.0.0/16` | private | Private destination tests |
| `127.0.0.1`, `::1` | loopback | Rebinding tests and the `loopback` rule |
| `169.254.1.1`, `fe80::1`, `0.0.0.0` | unusable | Prohibited destination tests |
| `192.0.2.50` | self | Returned by the injected `SelfAddrs` |
| `198.18.0.0/15` | public | Addresses of injected interface rows, outside `SelfAddrs` ([0008 acceptance criteria][d8-ac]) |
| `198.51.100.0/24` | — | Rejected remote sources: peers outside every allowlist, reported by listener wrappers and never dialed; `198.51.100.7` in single-source tests |

Only `Network`, `SelfAddrs`, the interface source, and its timer are
injected. The address classifier and all
policy code are production code. `cmd/cuttle` wires the real resolver and
dialer; internal packages have no default `Network`, so a test cannot reach
real DNS by accident. T0 and T1 use no real DNS, internet, or proxy.

### D4. Instances, chains, and loops

- Tests create cuttle instances in-process with `proxy.New(cfg, deps)` and
  pass listeners on `127.0.0.1:0`. `testenv.SourceListener` wraps a
  listener and reports a chosen peer address, such as `10.8.0.1`, so the
  instance classifies that client as remote.
- Chain: client → edge (local client; `gateway` upstream with
  `disclosure = "via"`) → gateway (sees the edge as `10.8.0.1`; `egress`
  upstream with `disclosure = "none"`) → inspecting egress mock or
  recording upstream → origin.
  Upstream addresses in the configurations are test addresses
  (`10.8.0.6:3128`) that `testenv.Net` routes to the real listeners.
- Non-disclosure chain ([0003 U12][d3-ac]): the original client is a
  raw-socket client behind a `SourceListener` with a distinctive address,
  so the test can search every recorded header value for that address and
  both instance names. It sends fields that Go's client would not, such as its
  own `Via` and `X-Real-IP`, which must not arrive, and a request without
  `User-Agent`, and targets that Go would re-escape.
- Loop: the edge routes `loop.cuttle.test` to the gateway, and the gateway
  routes it to the edge, both with `disclosure = "via"`; the edge's second
  listener reports the gateway's address. The expected result is
  `502 proxy_loop_detected` after exactly two `CONNECT` requests
  ([0003 U8][d3-ac]).
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
  pipelining, the non-disclosure chain, and byte counters.
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

- A mock, origin or upstream, answers `407` only in T1 tests whose clients
  are cuttle, Go, or `/usr/bin/curl`: [0003 U3][d3-ac] and
  [0002 C10][d2-ac]. No `407` variant is ever enabled in a `tools/mockenv`
  session, which is what browser and Apple-framework clients use;
  `tools/mockenv` refuses any such mode (E4), so no browser or
  Apple-framework session ever sees a proxy challenge
  ([LESSONS](../LESSONS.md)).
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
out=$(gofmt -l .)
test -z "$out"
go mod tidy -diff
go vet ./...
go build ./...
go test -short -count=1 ./...
out=$(git ls-files '*.pem' '*.key' '*.crt' '*.p12')
test -z "$out"
deps=$(mktemp)
go list -deps ./cmd/cuttle >"$deps"
rc=0
grep -E 'internal/testenv|^golang\.org/x/net/' "$deps" || rc=$?
test "$rc" -eq 1
CGO_ENABLED=0 go build -o /dev/null ./cmd/cuttle
```

Output that must be empty is captured first, by an assignment or into a
file that the script removes on exit: under `set -e` both fail with their
command, while a failure inside `test -z "$(...)"` or on the left of `&&`
would not stop the gate. The dependency check passes only on `grep`'s exit
status 1, no match; status 0 lists a forbidden package, and status 2 means
`grep` itself failed. The pattern forbids every package of the
`golang.org/x/net` module, while the standard library's own
`vendor/golang.org/x/net/...` packages, which every `net/http` program
links, do not start with `golang.org` and pass. The last command shows
that cuttle still builds without cgo ([0008 D16][d8-16]).

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
- T2 at M1 close: the D8 M1 client check with headless Chrome and curl,
  ending with
  `go run ./tools/mockenv check-disclosure -dir <session directory>` for
  each client, and the [0008 device check][d8-dc] on the user's Mac with
  the user's interfaces. This is the milestone acceptance that AGENTS.md
  requires.
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

`tools/mockenv` has two subcommands:

- `go run ./tools/mockenv serve` starts the D2 mocks with fresh roots in a
  new session directory under `$TMPDIR` and prints cuttle configurations
  for an edge and a gateway instance, the root certificate paths, and the
  SPKI hashes for Chrome. It runs two identical recording upstreams,
  `egress-direct` and `egress-chain`; clients use the first directly, and
  the gateway's `egress` upstream (`disclosure = "none"`) uses the second.
  Each records every request as one JSON line in `<dir>/<mock>.jsonl`:
  method, target, header fields with names and values in the received
  order, and the body.
- `go run ./tools/mockenv check-disclosure -dir <dir> -names
  cuttle-edge,cuttle-gateway -client <original client address>` compares
  the two files. For each target in `egress-chain.jsonl` it prints every
  disclosure field of [0003 D5][d3-5], every value containing an instance
  name or the client address, and any difference in method, target, or
  body from the `egress-direct.jsonl` request with the same target. Other
  header differences are printed for information. It exits 1 if any check
  fails.

Each cuttle binary gets its own `XDG_*` directories and test-chosen ports,
as in D4. One `serve` session is used per client.

Headless Chrome runs as
`"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"` with
`--headless=new --use-mock-keychain --user-data-dir=<new temporary
directory> --no-first-run --proxy-server=http://127.0.0.1:<port>
--proxy-bypass-list="<-loopback>"
--ignore-certificate-errors-spki-list=<hashes> --virtual-time-budget=5000
--dump-dom <url>`. `<-loopback>` removes Chrome's implicit loopback bypass
([research 0002 §5][r2-5]). curl uses `--proxy`, `--cacert`, `-w`, and
`-g`, which keeps `{` and `}` in a URL literal, as in D5.

M1 client check, against mocks only:

| Case | Target | Route | Evidence |
| --- | --- | --- | --- |
| Direct HTTPS | `https://127.0.0.1:<origin port>/echo` | The `loopback` rule | Chrome's DOM and curl show the echo; decision `ok`, action `direct` |
| Direct HTTP | `http://127.0.0.1:<origin port>/echo` | The `loopback` rule | Chrome's DOM and curl show the echo; plain-HTTP decision `ok`, action `direct` |
| Block | `https://blocked.cuttle.test/` | `block` rule | Chrome's DOM is empty; curl's `%{http_connect}` is `403`; decision `blocked`; no mock received anything |
| Upstream | `https://origin.cuttle.test/echo` | Recording upstream, `disclosure = "via"` | Echo shown; the mock recorded the `CONNECT` with one cuttle `Via` entry |
| WebSocket | `http://ws.cuttle.test/ws.html` | Recording upstream | Chrome only: the mock recorded `CONNECT ws.cuttle.test:80`, and the origin recorded the page's message |
| Chain | `https://origin.cuttle.test/chain` | Edge → gateway (`none`) → inspecting egress mock | Echo shown through the inspection root CA; the inspecting egress mock's `CONNECT` head has no `Via` |
| Non-disclosure | `https://origin.cuttle.test/x`, `http://plain.cuttle.test/x`, `http://plain.cuttle.test/a{b}|c?x=1;y=2&z=%zz`, and `http://plain.cuttle.test/q?b=2&a=1;c=3` | Client → `egress-direct`, then client → edge → gateway (`none`) → `egress-chain` | `check-disclosure` exits 0 for Chrome and for curl: no disclosure field of [0003 D5][d3-5], no value containing either instance name or the original client address, and the same method, target, and body bytes as the direct run |

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
| Chain | `https://origin.cuttle.test/` | Edge → gateway (`none`) → inspecting egress mock | Loads only when the inspection root CA is trusted (Chrome SPKI flag, Safari keychain after approval, curl `--cacert`) |
| Non-disclosure | As at M1 | As at M1 | The M1 properties, now including Safari |
| IPv6 literal | `https://[2001:db8::1]/` | Recording upstream, status `403` | The recorded `CONNECT` spelling ([OI-0002][oi]) |
| Admin UI | `http://127.0.0.1:3129/` | — | Login and dashboard work |

Results go into [COMPATIBILITY.md](../COMPATIBILITY.md) with client and
macOS versions, the tested commit, and evidence links.

## Acceptance criteria

| ID | Criterion | Check |
| --- | --- | --- |
| E1 | Mock self-tests pass: the inspecting egress mock's leaves have a SAN, the serverAuth EKU, and at most 825 days of validity; the recording upstream records requests byte for byte per connection; `testenv.Net` records lookups and dials. | T1 `go test ./internal/testenv/...` |
| E2 | `cmd/cuttle` links neither `internal/testenv` nor any package of the `golang.org/x/net` module, `x/net/websocket` included, and builds with `CGO_ENABLED=0`. | T0 commands above |
| E3 | Git tracks no key or certificate file. | T0 command above |
| E4 | `tools/mockenv` rejects any `407` mode. | T1 `TestMockenvRefuses407` |
| E5 | The full T1 run completes within its 10-minute timeout. | T1 command above |
| E6 | The binary-test helper refuses to start cuttle unless `HOME`, every `XDG_*` directory, and `XDG_RUNTIME_DIR` are test-owned temporary directories, and, for `cuttle serve`, the only command that binds, the configuration exists, `proxy.listen` avoids 3128 and 3129, and `admin.listen` is `""` or a port the test chose. Commands that bind nothing may read the unchanged starter file, as K2's `cuttle check` does. | T0 `TestBinaryHelperIsolation` |
| E7 | `mockenv check-disclosure` reports and fails on a planted disclosure field, a value with an instance name or the client address, a changed target, and a changed body, and passes on identical records. | T1 `TestCheckDisclosure` |

## Alternatives considered

- **A real DNS server in tests.** Needs a DNS library or privileged
  resolver changes; the `Network` seam covers the same paths.
- **mitmproxy as the inspecting egress mock.** Research used it, but it is a
  Python dependency outside Go tests; a Go mock is enough for a blind
  tunnel.
- **Docker or VM networks.** Heavier than the project allows, and not
  needed while cuttle's code paths take an injected network.
- **Chrome against public sites at M1.** Mocks keep the M1 check offline and
  let it compare recorded bytes; public sites wait for M3.

[d2-ac]: 0002-proxy-core-and-routing.md#acceptance-criteria
[d3-5]: 0003-upstream-chaining-and-exposure.md#d5-non-disclosure-toward-an-egress-proxy
[d3-ac]: 0003-upstream-chaining-and-exposure.md#acceptance-criteria
[d8-7]: 0008-interface-selection.md#d7-dial-plan
[d8-16]: 0008-interface-selection.md#d16-dependencies-and-platform-code
[d8-ac]: 0008-interface-selection.md#acceptance-criteria
[d8-dc]: 0008-interface-selection.md#device-check-i20
[oi]: ../OPEN_ISSUES.md
[r2-5]: ../research/0002-proxy-chaining-and-interception.md#5-how-macos-clients-choose-and-talk-to-the-proxy
[r2-6]: ../research/0002-proxy-chaining-and-interception.md#6-what-clients-show-for-proxy-generated-failures

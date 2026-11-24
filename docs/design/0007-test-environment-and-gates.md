# 0007 — Test environment and gates

**Status** — Proposed (TASK-0001, 2026-11-25).

## Context

There is no local test network, corporate proxy, or second site to test
against. The project must therefore provide controlled origins and
upstream proxies, including a TLS-intercepting one, and run chained cuttle
instances against them. Research found two hazards: a mock that answers
`407` to an Apple-framework client opens a credential dialog on the user's
desktop, and browser checks can open windows
([LESSONS](../LESSONS.md), [research 0002 §6][r2-6]).

This record defines the test layers, the mocks, name-resolution control,
the chain and loop setups, the clients, safety rules, and the concrete T0
to T3 gates.

## Decisions

### D1. Test layers

| Layer | Contents | Tier |
| --- | --- | --- |
| Unit | Authority parsing, policy, configuration validation, paths, aggregation, rotation, password hashing; no sockets | T0 (`go test -short`) |
| Integration | In-process cuttle instances with mocks on `127.0.0.1`, driven by Go, raw-socket, curl, and WebSocket clients | T1 |
| Binary | Tests that build `./cmd/cuttle` and run it as a process: first run, CLI, control socket, signals | T1 |
| Client | Chrome, Safari, and curl against a cuttle binary and `tools/mockenv` | T2 |
| Performance | Throughput, latency, and resource measurements | T3 |

Integration and binary tests call `testing.Short()` and skip under `-short`.

### D2. Mocks

All mocks live in `internal/testenv` and are started by tests on
`127.0.0.1:0`.

| Mock | Behavior | Records |
| --- | --- | --- |
| CA set | Per test run: an origin root and a corporate root, ECDSA P-256. Leaves have a SAN, the serverAuth EKU, and 24-hour validity. Kept in memory; written as PEM to `t.TempDir()` only for curl. | — |
| HTTPS origin | TLS with a leaf for `*.cuttle.test`. `/echo` returns method, path, header fields, and body; `/bytes?n=` sends n bytes; `/sink` counts an upload; `/slow?ms=` delays the response; `/ws` is a WebSocket echo. | Requests with `Host` and `Via`; connections |
| HTTP origin | The same endpoints over plain HTTP, with `/ws` for `ws://`. | Same |
| Recording upstream | A raw-socket HTTP proxy. Per target name the test selects `tunnel` (`200`), a status (`201`, `299`, `200` with `Content-Length: 3`, `403`, `407`, `502`, `504`), `buffered` (`200` followed at once by server bytes), `silent` (reads the head, never answers), or `reset`. Absolute-form requests are forwarded to the origin with `Via` appended, or answered with the chosen status. | Every request head byte for byte |
| Intercepting upstream | Accepts plain-HTTP proxy connections. On `CONNECT` it answers `200`, terminates TLS with a leaf for the SNI name signed by the corporate root, and forwards the decrypted HTTP/1.1 requests to the origin, trusting the origin root. Names on its refuse list get `403`. Absolute-form `http://` is proxied normally. | `CONNECT` heads with `Via`; decrypted request lines and `Host` |

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
| `203.0.113.0/24` | public | Origins on direct routes |
| `10.8.0.1`, `10.8.0.6` | private | Home and office VPN addresses |
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
- Chain: client → home (local client) → office (sees home as `10.8.0.1`,
  allowed) → intercepting upstream as `corp` → HTTPS origin. Upstream
  addresses in the configurations are test addresses (`10.8.0.6:3128`)
  that `testenv.Net` routes to the real listeners.
- Loop: home routes `loop.cuttle.test` to office, and office routes it to
  home, whose second listener reports office's address. The expected result
  is `502 proxy_loop_detected` after exactly two `CONNECT` requests
  ([0003](0003-upstream-chaining-and-exposure.md#acceptance-criteria) U8).
- Binary tests cannot inject `Network`. They use `block` rules, the
  `loopback` rule, and routes to a recording upstream on `127.0.0.1`,
  which needs no address check and reaches origins through its own test
  resolver. Each binary test sets `HOME` and the `XDG_*` variables to
  directories under `t.TempDir()`.

### D5. Clients

- **Go**: `http.Transport` with a fixed `Proxy`, `OnProxyConnectResponse`
  to capture `CONNECT` status codes, and the test roots in `RootCAs`.
- **Raw socket**: writes and reads exact bytes, for framing, pipelining,
  and byte counters.
- **curl**: `/usr/bin/curl`, or the path in `CUTTLE_CURL`, run with an
  environment holding only `PATH` and a temporary `HOME`, and with
  `--proxy http://127.0.0.1:<port>`, `--cacert`, `-sS`, and `-w` for
  `%{http_connect}` and `%{http_code}`. curl sends host names to the proxy,
  so cuttle resolves them through `testenv.Net`. Without curl, tests skip,
  unless `CUTTLE_REQUIRE_CURL=1`, which the T1 gate sets.
- **WebSocket**: a raw `CONNECT` to port 80 or 443, then
  `golang.org/x/net/websocket` over the tunnel, with TLS for `wss`.
- No Apple-framework client runs in T0 or T1.

### D6. Safety rules

- A mock answers `407` only in T1 tests with Go or curl clients.
  `tools/mockenv` refuses any `407` mode, so no browser session ever sees a
  proxy challenge ([LESSONS](../LESSONS.md)).
- T0 and T1 never start a browser or another GUI process. T2 browser steps
  need user approval first. Headless Chrome uses a throwaway
  `--user-data-dir` and `--use-mock-keychain`. Safari steps change system
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
- T2-lite closes M2: the headless Chrome admin check
  ([0005](0005-statistics-and-admin.md#acceptance-criteria) A10). Full T2
  closes M3 and every later milestone: the D8 matrix, the Homebrew checks
  ([0006](0006-packaging-paths-and-lifecycle.md#acceptance-criteria)
  P8–P12), and the deployment check.
- T3 applies to any claim about throughput, latency, or resource use, and
  before numeric targets are set. A Go load client run against
  `tools/mockenv` measures tunnel throughput, `CONNECT` setup latency (p50
  and p99), resident memory, and open descriptors, under `brew services`
  with `ProcessType` unset and `Interactive`. Raw results are kept with the
  inputs.

### D8. T2 procedure

`go run ./tools/mockenv` starts the following and prints a cuttle
configuration for a home and an office instance, the paths of the
generated roots, and the SPKI hash for Chrome:

- a recording upstream on `127.0.0.1:18100` that also passes allowlisted
  public names (`example.org`) through to the internet;
- an intercepting upstream on `127.0.0.1:18200` with a fresh corporate
  root;
- HTTP, HTTPS, and WebSocket origins for `*.cuttle.test`, reachable only
  through those upstreams.

Each cuttle instance gets its own `XDG_*` directories. Client
configuration: Chrome with `--proxy-server=http://127.0.0.1:3128` and a
throwaway profile; Safari through System Settings, with "Web proxy (HTTP)"
and "Secure web proxy (HTTPS)" both set to `127.0.0.1:3128`
([research 0002 §5][r2-5]); curl with `--proxy`.

| Case | Target | Route | Expected |
| --- | --- | --- | --- |
| Direct HTTPS | `https://example.com/` | `direct` | Loads with its public certificate; decision `ok` |
| Direct HTTP | `http://example.com/` | `direct` | Loads; plain-HTTP decision |
| Upstream HTTPS | `https://example.org/` | recording upstream | Loads with its public certificate; the mock recorded `CONNECT example.org:443` with `Via`; no direct dial |
| WebSocket | `http://ws.cuttle.test/ws.html` | recording upstream | The page reports an echo; the mock recorded `CONNECT ws.cuttle.test:80` |
| Block | `https://example.net/` | `block` | Chrome `ERR_TUNNEL_CONNECTION_FAILED`, Safari generic failure, curl `403`; decision `blocked`; no dial |
| Chain | `https://origin.cuttle.test/` | home → office → intercepting | Loads only when the corporate root is trusted (Chrome SPKI flag, Safari keychain after approval, curl `--cacert`); the mock logged the decrypted request with both `Via` entries |
| IPv6 literal | `https://[2001:db8::1]/` | recording upstream, status `403` | The recorded `CONNECT` spelling ([OI-0002][oi]) |
| Admin UI | `http://127.0.0.1:3129/` | — | Login and dashboard work |

Results go into [COMPATIBILITY.md](../COMPATIBILITY.md) with client and
macOS versions, the tested commit, and evidence links.

## Acceptance criteria

| ID | Criterion | Check |
| --- | --- | --- |
| E1 | Mock self-tests pass: the intercepting upstream's leaves have a SAN, the serverAuth EKU, and at most 825 days of validity; the recording upstream records heads byte for byte; `testenv.Net` records lookups and dials. | T1 `go test ./internal/testenv/...` |
| E2 | `cmd/cuttle` links neither `internal/testenv` nor `x/net/websocket`. | T0 command above |
| E3 | Git tracks no key or certificate file. | T0 command above |
| E4 | `tools/mockenv` rejects a `407` mode. | T1 `TestMockenvRefuses407` |
| E5 | The full T1 run completes within its 10-minute timeout. | T1 command above |

## Alternatives considered

- **A real DNS server in tests.** Needs a DNS library or privileged
  resolver changes; the `Network` seam covers the same paths.
- **mitmproxy as the intercepting upstream.** Research used it, but it is a
  Python dependency outside Go tests; a Go mock is enough for a blind
  tunnel.
- **Docker or VM networks.** Heavier than the project allows, and not
  needed while cuttle's code paths take an injected network.

[oi]: ../OPEN_ISSUES.md
[r2-5]: ../research/0002-proxy-chaining-and-interception.md#5-how-macos-clients-choose-and-talk-to-the-proxy
[r2-6]: ../research/0002-proxy-chaining-and-interception.md#6-what-clients-show-for-proxy-generated-failures

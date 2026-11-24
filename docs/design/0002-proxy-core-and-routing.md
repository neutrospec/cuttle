# 0002 — Proxy core and routing

**Status** — Proposed (TASK-0001, 2026-11-25). Together with
[0003](0003-upstream-chaining-and-exposure.md) it supersedes
[0001](0001-https-connect-proxy.md).

## Context

cuttle is an explicit forward proxy for Chrome, Safari, and terminal clients
on macOS. The repository is named `squid`; the formula, binary, service, and
XDG directories are named `cuttle`. Each HTTPS `CONNECT` tunnel and each plain
`http://` request is routed by the same rules to `direct`, an upstream proxy,
or `block`, and the traffic is counted.

This record defines the proxy listener, request parsing, authority
normalization, rule semantics, the responses cuttle generates, byte
accounting, live counters, and the decision log. Upstream forwarding, loop
detection, and exposure controls are in
[0003](0003-upstream-chaining-and-exposure.md). The configuration syntax is
in [0004](0004-configuration-and-reload.md). The control socket and CLI that
read live counters are in [0006](0006-packaging-paths-and-lifecycle.md).

Decisions fixed before this record: Go with the standard library first;
tunnels are blind and cuttle never terminates client TLS; an upstream route
never falls back to direct; the proxy listener has no authentication and
cuttle never answers `407`.

## Decisions

### D1. Toolchain, dependencies, and layout

The module path is `squid`. Nothing installs cuttle with `go install`, so
the path does not need to name a host. The command is `./cmd/cuttle`. The
`go` directive is `1.26.0`, the minimum that
modernc.org/sqlite requires and below the Homebrew `go` 1.27.4 that builds
the formula ([research 0001 §1][r1-1], [§6][r1-6]). Gates run with the
Homebrew Go toolchain and record its version.

| Module | Use | First milestone | Justification |
| --- | --- | --- | --- |
| `github.com/BurntSushi/toml` | Configuration parsing | M1 | [0004 D1](0004-configuration-and-reload.md#d1-file-and-format) |
| `modernc.org/sqlite` | Statistics store | M2 | Pure Go, no cgo, WAL verified ([research 0001 §6][r1-6]) |
| `golang.org/x/crypto/argon2` | Admin password hash | M2 | The standard library has no Argon2id |
| `golang.org/x/term` | Password prompt without echo | M2 | The standard library cannot switch terminal modes |
| `golang.org/x/net/websocket` | Test-only WebSocket origin and client | M1 | The standard library has no WebSocket; never linked into `cmd/cuttle` |

Any other dependency needs a design change.

| Package | Responsibility |
| --- | --- |
| `cmd/cuttle` | CLI entry point and subcommands |
| `internal/paths` | XDG resolution |
| `internal/config` | Schema, validation, starter file, atomic write |
| `internal/authority` | Request-target parsing and normalization |
| `internal/policy` | Rule compilation and matching; no I/O |
| `internal/netx` | `Network` seam, address classes, direct dialer |
| `internal/proxy` | Listener, front end, tunnels, HTTP forwarding, chaining |
| `internal/stats` | Live counters, aggregator, SQLite store |
| `internal/admin` | Admin HTTP server, sessions, templates |
| `internal/control` | Control socket server and client |
| `internal/logx` | Rotating log writer |
| `internal/testenv` | Mocks for tests; never imported by `cmd/cuttle` |
| `tools/mockenv` | Development command that runs the mocks for T2 |

### D2. Own HTTP/1.1 front end on the proxy listener

cuttle reads request heads itself instead of serving the proxy port with
`net/http.Server`.

- Go's server rejects CFNetwork's bracketless IPv6 `CONNECT` with `400`
  before any handler runs ([research 0002 §5][r2-5]).
- `CONNECT` must branch before any mux, because `ServeMux` answers 404 and
  `ReverseProxy` answers 405 ([research 0002 §1][r2-1]).
- Every request, malformed ones included, then gets cuttle's error format,
  a log line, and a counter, and per-request routing on a reused connection
  is explicit in one loop.

The front end reuses standard-library parsers and adds no HTTP framing code
of its own:

1. Wait for the request line with a 30 s head timeout. Peek at most 8 KiB in
   a 16 KiB `bufio.Reader`. A first byte of `0x16` (a TLS ClientHello) is
   logged as `tls_on_plain_listener` and the connection is closed. A line
   that is not `METHOD SP target SP HTTP/1.x` gets `400`.
2. For `CONNECT`, consume the line, parse the target with D3, and read the
   header section with `textproto.Reader.ReadMIMEHeader`. `Host` is ignored.
   A `CONNECT` with `Content-Length` other than 0 or with
   `Transfer-Encoding` gets `400`.
3. For any other method, call `http.ReadRequest` on the same reader, which
   applies RFC 9112 message framing. The target must be absolute-form with
   scheme `http`; D3 normalizes its authority. Origin-form, asterisk-form,
   absolute-form `https://`, other schemes, and userinfo get `400`.
4. The whole head is limited to 64 KiB; a larger head gets `431`.

Connection rules:

- A `CONNECT` is the last request on its connection: a tunnel or a
  cuttle-generated error follows, and then the connection closes.
- Plain HTTP connections persist per RFC 9112 §9.3. Requests are handled in
  order; pipelined requests wait. The keep-alive idle timeout is 120 s.
- Every cuttle-generated error response closes the client connection, so
  cuttle never has to drain an unread request body.
- `Expect: 100-continue`: cuttle writes `100 Continue` when it first reads
  the body to forward it, as `net/http.Server` does. A refused request gets
  its final status without reading the body.

### D3. Authority normalization

The normalized authority is used for matching, dialing, the upstream
`CONNECT` target, the forwarded absolute-form target, and `Host`. It is a
lowercase ASCII name without a trailing dot, or a canonical IP address from
`net/netip`, plus a port. IPv6 is always written with brackets on the wire.

| Request target | Normalized | Note |
| --- | --- | --- |
| `CONNECT Example.COM:443` | `example.com:443` | ASCII lowercase |
| `CONNECT example.com.:443` | `example.com:443` | One trailing dot removed |
| `CONNECT [2001:DB8::1]:443` | `[2001:db8::1]:443` | `netip` canonical form |
| `CONNECT 2001:db8::1:443` | `[2001:db8::1]:443` | CFNetwork form ([research 0002 §5][r2-5]) |
| `CONNECT [::ffff:192.0.2.10]:443` | `192.0.2.10:443` | IPv4-mapped is unmapped |
| `GET http://Plain.Example:80/a` | `plain.example:80` | Port defaults to 80 |
| `CONNECT [fe80::1%en0]:443` | `400` | Zones are refused |
| `CONNECT 127.1:443`, `2130706433:443`, `0x7f.1:443` | `400` | Numeric host that is not a dotted quad |
| `CONNECT` raw UTF-8 name | `400` | Send the A-label (`xn--`) form |
| `CONNECT example.com` | `400` | `CONNECT` requires a port |
| `CONNECT example.com:0443` | `400` | Port is 1–65535 without leading zeros |

Rules:

- A bracketless target with two or more colons is split at the last colon.
  The left part must parse as IPv6 and the right part is the port. This is
  unambiguous because `CONNECT` requires a port.
- A name has 1–253 bytes in labels of 1–63 bytes from `[a-z0-9_-]` after
  lowercasing. A name whose last label is all digits or starts with `0x` is
  refused, so resolver spellings of IP addresses never reach a rule or an
  upstream ([research 0002 §7][r2-7]).
- A-labels pass through without IDNA validation. U-labels are not converted,
  so curl 8.7.1's raw UTF-8 `CONNECT` gets `400`
  ([research 0002 §5][r2-5]).

### D4. Rules and evaluation order

Each request is evaluated independently, including successive plain-HTTP
requests on one reused client connection
([research 0002 §2][r2-2]). Evaluation never resolves a name.

1. Accept: check the source address ([0003 D8][d3-8]).
2. Parse and normalize the target (D3) → `400` on failure.
3. Port policy: the port must be in `proxy.ports` → else `403`, rule `port`.
4. Rules in file order; the first match wins. With no match, use
   `proxy.default` for local clients or `proxy.remote.default` for remote
   ones.
5. A remote client whose action is `direct` gets `403`, rule
   `remote-direct`, unless `proxy.remote.allow_direct` is true
   ([0003 D8][d3-8]).
6. Loop check: unless the action is `block`, a request whose `Via` names
   this instance gets `502` ([0003 D5][d3-5]).
7. Execute the action.

A rule matches when its destination condition and its port condition both
hold.

| Field | Matches | Example |
| --- | --- | --- |
| `hosts` | The normalized name exactly | `"wiki.example.com"` |
| `domains` | The name or any subdomain, on a label boundary | `"example.com"` matches `example.com` and `a.b.example.com`, not `badexample.com` |
| `cidrs` | IP-literal targets inside a prefix | `"10.0.0.0/8"`, `"::1/128"` |
| `ports` | The target port is listed | `[443]` |

- The destination condition holds when any entry of `hosts`, `domains`, or
  `cidrs` matches. A rule with none of the three matches every destination.
- The port condition holds when `ports` is absent or contains the port.
- Names never match `cidrs`, and IP literals never match `hosts` or
  `domains`. A rule that should cover a site's literal addresses lists them
  in `cidrs`.
- Actions are `direct`, `block`, or `upstream:<name>`.
- `allow_loopback = true` on a `direct` rule lets local clients reach
  loopback and this host's own addresses through that rule
  ([0003 D9][d3-9]).

First match in file order is chosen over most-specific match because the
order an operator reads is the order cuttle applies. `cuttle explain`
([0006 D3][d6-3]) shows the matching rule for a target.

### D5. Actions

- **`block`** — answer `403` with no outbound DNS lookup or connection.
- **`direct`, `CONNECT`** — resolve and check the addresses
  ([0003 D9][d3-9]), dial, then write exactly
  `HTTP/1.1 200 Connection established\r\n\r\n` and relay. Bytes the client
  pipelined after its head are relayed first ([research 0002 §1][r2-1]).
- **`direct`, plain HTTP** — forward in origin-form with `Host` set to the
  normalized authority, omitting port 80
  ([research 0002 §2][r2-2]). The response is relayed with its status.
- **`upstream:<name>`** — [0003 D2][d3-2] and [D3][d3-3].

Plain HTTP forwarding, for both direct and upstream routes:

- Remove `Connection` and every field it names, plus `Proxy-Connection`,
  `Keep-Alive`, `TE`, `Trailer`, `Transfer-Encoding`, `Upgrade`,
  `Proxy-Authorization`, and `Proxy-Authenticate`, in both directions
  ([research 0002 §2][r2-2]).
- Append `1.1 <name>` to `Via` on the forwarded request and on the relayed
  response. Never add `Forwarded` or `X-Forwarded-*`; client-sent copies
  pass through unchanged.
- Use `http.Transport.RoundTrip` with `Proxy: nil`,
  `DisableCompression: true`, HTTP/2 off, and a 120 s response-header
  timeout. Never read proxy settings from the environment. Redirects are
  relayed, not followed.
- `Upgrade` is stripped, so an absolute-form WebSocket request reaches the
  origin as a plain `GET`. WebSocket works through `CONNECT`, which Chrome
  uses for `ws://` on port 80 and curl uses with `--proxytunnel`
  ([research 0002 §2][r2-2]). The default `proxy.ports` is `[80, 443]` for
  that reason.

Outbound connection pools:

- Direct transports exist per policy snapshot and per client class (local or
  remote). A pooled connection is reused only for requests with the same
  snapshot, class, host, and port, so a later request never inherits a dial
  that a different policy allowed.
- Upstream transports exist per upstream and are replaced when that
  upstream's settings change ([0004 D6][d4-6]).
- A tunnel never shares its outbound connection.

Tunnel relay:

- Copy both directions with 32 KiB buffers. On EOF in one direction,
  half-close the write side of the other connection and keep copying the
  remaining direction. Close both connections when both directions finish,
  on any error, or after `proxy.idle_timeout` with no bytes in either
  direction.

### D6. Responses generated by cuttle

Every response cuttle generates has a reason phrase, `Proxy-Status`
([RFC 9209][rfc9209]), `Content-Type: text/plain; charset=utf-8`,
`Cache-Control: no-store`, `X-Content-Type-Options: nosniff`,
`Connection: close`, and a one-line body such as
`cuttle: blocked by rule "ads" (ads.example.net:443)`. Only the normalized
authority and the rule name are reflected. Browsers discard the body of a
failed `CONNECT` and show a generic error
([research 0002 §6][r2-6]); the decision log and the admin UI explain it.

| Case | Status | `Proxy-Status` error | Outcome |
| --- | --- | --- | --- |
| Malformed request or target, unsupported form | 400 | `http_request_error` | `bad_request` |
| Request head over 64 KiB | 431 | `http_request_error` | `bad_request` |
| Port not allowed, rule or default `block`, remote `direct` | 403 | `http_request_denied` | `blocked` |
| Loop detected | 502 | `proxy_loop_detected` | `loop` |
| Name resolution failed | 502 | `dns_error` | `dns_error` |
| Every resolved address prohibited | 502 | `destination_ip_prohibited` | `prohibited` |
| Connection refused or reset | 502 | `connection_refused` | `refused` |
| TCP connect timed out | 504 | `connection_timeout` | `timeout` |
| Upstream gave no `CONNECT` response in time | 504 | `http_response_timeout` | `timeout` |
| Upstream held as unreachable ([0003 D12][d3-12]) | 503 | `destination_unavailable` | `unavailable` |
| Upstream answered non-2xx | 502, or 504 when it sent 504 | none; `received-status=<code>` | `upstream_status` |
| Upstream response unparsable | 502 | `http_protocol_error` | `upstream_protocol` |
| Internal error | 500 | `proxy_internal_error` | `internal` |

- `403` always means cuttle's own policy. A refusal by a later hop arrives
  as `502` with `received-status`, so a curl user can tell the two apart
  ([0003 D4][d3-4]).
- cuttle never sends `407` and never relays one
  ([research 0002 §1][r2-1]).
- Member format: `Proxy-Status: <name>; error=<type>; details="<reason>"`,
  plus `next-hop="<upstream>"` on upstream failures.

### D7. Byte accounting and live counters

Counted bytes are relayed bytes measured on the client side, once per
direction. `up` is client → destination and `down` is destination → client.

| Kind | `up` | `down` |
| --- | --- | --- |
| Tunnel | Bytes received from the client after the `CONNECT` head and written to the next hop | Bytes written to the client after the `200` head |
| Plain HTTP | Size of the request as received from the client: line, header section, and content as framed by the client | Size of the response as written to the client: line, header section, and framed content |
| Response generated by cuttle | 0 | 0 |

Tunnel totals include the TLS handshake and records. Counters advance during
transfer: a tunnel adds to its atomic counters as each write completes, so
active tunnels contribute before they close.

Live counters are in memory, start at zero with the process, and are read
through the control socket (`cuttle status`, [0006 D4][d6-4]) and the admin
API (`/api/live`, [0005 D9][d5-9]).

| Counter | Labels |
| --- | --- |
| `connections_active`, `tunnels_active`, `http_requests_active` | — |
| `connections_total` | — |
| `connections_rejected_total` | `reason` = `source`, `limit` |
| `decisions_total` | `kind` = `connect`, `http`; `action` |
| `outcomes_total` | `outcome` (D6, plus `ok`, `client_closed`, `shutdown`) |
| `bytes_total` | `action`, `direction` = `up`, `down` |
| `upstream_outcomes_total` | `upstream`, `outcome` |
| `upstream_state` | `upstream`; value `up`, `down`, or `probing` with its start time ([0003 D12][d3-12]) |

Labels are bounded: hosts never appear in counters. Per-host data lives in
the log and in statistics ([0005](0005-statistics-and-admin.md)). The status
response also lists active tunnels with authority, route, source, age, and
bytes, capped at 500 entries.

### D8. Decision log

cuttle writes one `log/slog` text line per completed decision at level
`info` ([0006 D7][d6-7]):

```text
time=2026-11-25T10:01:02.345Z level=INFO msg=decision id=4f2a
client=10.8.0.1:51234 source=remote kind=connect
target=wiki.corp.example.com:443 rule=office-only action=upstream
upstream=corp outcome=ok status=200 up=5321 down=88410 dur=12.402s
```

The line is wrapped here for width; it is one line in the log. Errors add
`error=<Proxy-Status type>` and `detail`. The log never contains paths,
queries, header values, bodies, or TLS contents.

## Interfaces

```go
// Network is the only path from the proxy to name resolution and sockets.
// Tests replace it (0007 D3).
type Network interface {
    LookupNetIP(ctx context.Context, host string) ([]netip.Addr, error)
    DialContext(ctx context.Context, addr netip.AddrPort) (net.Conn, error)
}

type Decision struct {
    Target        authority.Authority // normalized host and port
    Kind          Kind                // Connect or HTTP
    Source        SourceClass         // Local or Remote
    Rule          string              // rule name, "default", "remote-default",
                                      // "port", or "remote-direct"
    Action        Action              // Direct, Upstream, Block
    Upstream      string              // set when Action is Upstream
    AllowLoopback bool
}

// Decide is pure: no I/O and no DNS.
func (p *Policy) Decide(t authority.Authority, k Kind, s SourceClass) Decision
```

`proxy.New(cfg, deps)` takes the listeners as `net.Listener` values, so
tests can supply listeners that report chosen remote addresses
([0007 D4][d7-4]).

## Failure behavior

- Parsing or policy failures answer before any DNS lookup or dial.
- Direct-route failures map to D6 and never retry another route.
- After `200` has been written, a failure can only close the tunnel; the
  outcome is logged with the bytes relayed so far.
- A client that closes before the decision completes is logged as
  `client_closed` and is not a failure.

## Acceptance criteria

All checks are T1 tests run with the mocks in
[0007](0007-test-environment-and-gates.md) unless marked T0.

| ID | Criterion | Check |
| --- | --- | --- |
| C1 | `CONNECT` with a `direct` rule reaches the HTTPS origin; Go and curl verify the test-CA certificate; a raw client receives exactly `HTTP/1.1 200 Connection established\r\n\r\n` and nothing else before origin bytes. | `TestConnectDirect`, `TestConnectExact200` |
| C2 | Bytes pipelined after the `CONNECT` head reach the origin. | `TestConnectPipelinedBytes` |
| C3 | Plain HTTP `GET` and `POST` with a `direct` rule arrive at the origin in origin-form with `Host` from the target, `Via: 1.1 <name>`, no hop-by-hop fields, no added `X-Forwarded-For`, and an unchanged body; the client's response carries `Via`. | `TestHTTPDirect`, `TestHTTPHopByHop` |
| C4 | Two plain-HTTP requests on one client connection to hosts with different rules take different routes, and each produces its own decision line. | `TestHTTPReusedConnRoutesPerRequest` (Go and curl) |
| C5 | A `block` rule, the default `block`, and a port outside `proxy.ports` answer `403` with `Proxy-Status: <name>; error=http_request_denied`, zero lookups and dials in the test network, and zero counted bytes. | `TestBlockRule`, `TestBlockDefault`, `TestBlockPort` |
| C6 | Every row of the D3 table normalizes or fails as listed. | T0 `TestAuthorityTable` |
| C7 | A bracketless IPv6 `CONNECT` (`CONNECT 2001:db8::1:443`) is routed by a `cidrs` rule and reaches an upstream as `CONNECT [2001:db8::1]:443`. | `TestConnectBracketlessIPv6` |
| C8 | Rule order, `hosts`, `domains` label boundaries, `cidrs`, `ports`, and both defaults decide as specified. | T0 `TestPolicyTable` |
| C9 | A WebSocket echo round trip succeeds over `CONNECT` to port 80 and to port 443 (`wss`). | `TestWebSocketOverConnect` |
| C10 | Absolute-form `https://`, origin-form, `OPTIONS *`, userinfo, a TLS ClientHello, and a 65 KiB head each get the D2 or D6 result and a decision line or counter. | `TestFrontEndRejects` |
| C11 | A controlled transfer of 1,048,576 bytes down and 65,536 bytes up through a tunnel shows partial `bytes_total` progress while open and exact totals after close; `tunnels_active` returns to 0. | `TestTunnelByteCounters` |
| C12 | Plain HTTP `up` and `down` equal the bytes a raw client wrote and read for that exchange. | `TestHTTPByteCounters` |
| C13 | A decision line has the D8 fields and contains no path or query from the request. | `TestDecisionLogFields` |

## Non-goals

- **Response caching.** cuttle never terminates client TLS, so it cannot
  see, let alone cache, HTTPS content. Caching only plain `http://` would
  need storage, freshness rules, `Vary` and `Cache-Control` handling for a
  shared cache, and a privacy model, for little gain, because browsers keep
  their own caches. Caching HTTPS content would require TLS interception,
  which is out of scope ([0001 alternatives][d1-alt]).
- **TLS interception, transparent proxying, and filtering on paths,
  headers, or bodies.** The tunnel stays blind.

## Consequences

- One `CONNECT` tunnel can carry many HTTPS requests; cuttle sees only the
  authority and the stream sizes.
- The front end is cuttle's own code. Its framing relies on
  `http.ReadRequest` and `textproto`, and the C-series tests cover the
  connection rules.
- Operators write rules by name. A rule never matches a site reached by IP
  literal unless its addresses are listed in `cidrs`.

## Alternatives considered

- **`net/http.Server` with `httputil.ReverseProxy`.** Less code for plain
  HTTP, but CFNetwork's IPv6 form fails before cuttle sees it, and errors
  bypass cuttle's logging.
- **Glob or regular-expression patterns.** `*.example.com` versus
  `.example.com` conventions differ between tools; separate `hosts` and
  `domains` lists have no ambiguity.
- **Matching resolved addresses against `cidrs`.** This would put DNS on the
  path of upstream routes, which must not resolve locally.
- **Converting U-labels with `golang.org/x/net/idna`.** Chrome already sends
  A-labels; the conversion is deferred until a client needs it.
- **Relaying absolute-form `Upgrade`.** Not needed by the target clients,
  and after `101` the stream needs separate accounting.

[d1-alt]: 0001-https-connect-proxy.md#alternatives-considered
[d3-12]: 0003-upstream-chaining-and-exposure.md#d12-unreachable-upstream-hold
[d3-2]: 0003-upstream-chaining-and-exposure.md#d2-connect-through-an-upstream
[d3-3]: 0003-upstream-chaining-and-exposure.md#d3-plain-http-through-an-upstream
[d3-4]: 0003-upstream-chaining-and-exposure.md#d4-upstream-failures-and-proxy-status
[d3-5]: 0003-upstream-chaining-and-exposure.md#d5-via-and-loop-detection
[d3-8]: 0003-upstream-chaining-and-exposure.md#d8-source-classes-and-remote-policy
[d3-9]: 0003-upstream-chaining-and-exposure.md#d9-destination-address-policy-for-direct-routes
[d4-6]: 0004-configuration-and-reload.md#d6-reload
[d5-9]: 0005-statistics-and-admin.md#d9-dashboard-and-api
[d6-3]: 0006-packaging-paths-and-lifecycle.md#d3-command-line
[d6-4]: 0006-packaging-paths-and-lifecycle.md#d4-control-socket
[d6-7]: 0006-packaging-paths-and-lifecycle.md#d7-logging-and-rotation
[d7-4]: 0007-test-environment-and-gates.md#d4-instances-chains-and-loops
[r1-1]: ../research/0001-homebrew-launchd-xdg.md#1-homebrew-formula-for-a-go-program-in-a-personal-tap
[r1-6]: ../research/0001-homebrew-launchd-xdg.md#6-go-sqlite-drivers
[r2-1]: ../research/0002-proxy-chaining-and-interception.md#1-connect-through-chained-http-proxies
[r2-2]: ../research/0002-proxy-chaining-and-interception.md#2-plain-http-forwarding-in-absolute-form
[r2-5]: ../research/0002-proxy-chaining-and-interception.md#5-how-macos-clients-choose-and-talk-to-the-proxy
[r2-6]: ../research/0002-proxy-chaining-and-interception.md#6-what-clients-show-for-proxy-generated-failures
[r2-7]: ../research/0002-proxy-chaining-and-interception.md#7-exposure-risks
[rfc9209]: https://www.rfc-editor.org/rfc/rfc9209.html#section-2.3

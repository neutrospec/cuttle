# 0002 — Proxy core and routing

**Status** — Proposed (TASK-0001, 2026-11-25; revised in iterations 2
and 3).
Together with [0003](0003-upstream-chaining-and-exposure.md) it supersedes
[0001](0001-https-connect-proxy.md).

## Context

cuttle is an explicit forward proxy for Chrome, Safari, and terminal clients
on macOS. The repository is named `squid`; the formula, binary, service, and
XDG directories are named `cuttle`. Each HTTPS `CONNECT` tunnel and each plain
`http://` request is routed by the same rules to `direct`, an upstream proxy,
or `block`, and the traffic is counted.

An egress proxy at the end of an edge → gateway → egress chain must not
see that a request passed through an intermediary. A request toward it
discloses no intermediary, not even through disclosure fields the client
sent itself ([0003 D5][d3-5]). Beyond that, cuttle follows standard
`net/http` handling. Terms are defined in the [glossary](../GLOSSARY.md).

This record defines the proxy listener, authority normalization, rule
semantics, forwarding, the responses cuttle generates, byte accounting, live
counters, and the decision log. Upstream disclosure modes, loop detection,
and exposure controls are in [0003](0003-upstream-chaining-and-exposure.md).
The configuration syntax is in [0004](0004-configuration-and-reload.md). The
control socket and CLI that read live counters are in
[0006](0006-packaging-paths-and-lifecycle.md).

Decisions fixed before this record: Go with the standard library first;
tunnels are blind and cuttle never terminates client TLS; an upstream route
never falls back to direct; the proxy listener has no authentication and
cuttle never answers `407`; the default action is a required setting.

## Decisions

### D1. Toolchain, dependencies, and layout

The module path is `squid`. Nothing installs cuttle with `go install`, so
the path does not need to name a host. The command is `./cmd/cuttle`. The
`go` directive is `1.26.0`, the minimum that modernc.org/sqlite requires and
below the Homebrew `go` 1.27.4 that builds the formula
([research 0001 §1][r1-1], [§6][r1-6]). Gates run with the Homebrew Go
toolchain and record its version.

| Module | Use | First milestone | Status |
| --- | --- | --- | --- |
| `github.com/BurntSushi/toml` | Configuration parsing | M1 | Approved ([0004 D1](0004-configuration-and-reload.md#d1-file-and-format)) |
| `modernc.org/sqlite` | Statistics store | M2 | Allowed: pure Go, no cgo, WAL verified ([research 0001 §6][r1-6]) |
| `golang.org/x/crypto/argon2` | Admin password hash | M2 | Allowed: the standard library has no Argon2id |
| `golang.org/x/term` | Password prompt without echo | M2 | Allowed: the standard library cannot switch terminal modes |
| `golang.org/x/net/websocket` | Test-only WebSocket origin and client | M1 | Allowed: never linked into `cmd/cuttle` |

Any other dependency needs a design change.

| Package | Responsibility |
| --- | --- |
| `cmd/cuttle` | CLI entry point and subcommands |
| `internal/paths` | XDG resolution |
| `internal/config` | Schema, validation, starter file, atomic write |
| `internal/authority` | Request-target parsing and normalization |
| `internal/policy` | Rule compilation and matching; no I/O |
| `internal/netx` | `Network` seam, address classes, direct dialer |
| `internal/proxy` | Listener, handler, tunnels, forwarding, chaining |
| `internal/stats` | Live counters, aggregator, SQLite store |
| `internal/admin` | Admin HTTP server, sessions, templates |
| `internal/control` | Control socket server and client |
| `internal/logx` | Rotating log writer |
| `internal/testenv` | Mocks for tests; never imported by `cmd/cuttle` |
| `tools/mockenv` | Development command that runs the mocks for client checks |

### D2. Front end on `net/http.Server`

The proxy port is served by `net/http.Server` with one root handler and no
`ServeMux` ([research 0002 §1][r2-1]):

- `CONNECT` is handled by the root handler, which parses the target from
  `r.RequestURI` with D3, decides the route, and takes the connection with
  `http.NewResponseController(w).Hijack()`. Bytes the client pipelined
  after its head stay in the returned reader and are relayed first.
- An absolute-form `http://` request is decided and forwarded (D5).
- Origin-form, asterisk-form, absolute-form `https://`, other schemes, and
  userinfo get `400` from the handler.

Server settings:

| Setting | Value | Reason |
| --- | --- | --- |
| `ReadHeaderTimeout` | 30 s | Head timeout |
| `IdleTimeout` | 120 s | Keep-alive idle timeout |
| `MaxHeaderBytes` | 64 KiB | Bounds a head. Go reads up to `MaxHeaderBytes` + 4096 bytes before it answers `431` (`server.go` l.956–957 in Go 1.27.4), so the effective limit is 69,632 bytes. Go also answers `431` to a head with more than 500 field values (`DefaultMaxHeaderValueCount`, `server.go` l.940, exported from Go 1.27; `MaxHeaderValueCount` stays at its default) |
| `ReadTimeout`, `WriteTimeout` | 0 | Tunnels and long bodies; idle limits apply instead |
| `DisableGeneralOptionsHandler` | `true` | Go would otherwise answer `OPTIONS *` itself; the handler answers `400` |
| `ErrorLog` | Adapter to the cuttle log at `warn` | Server-level errors are logged |
| `ConnContext`, `ConnState` | Source class per connection; connection counters | [0003 D10][d3-10] and D7 |

The listener is wrapped so that the source check and connection limits run
on accept, before `net/http` reads a byte ([0003 D10][d3-10], [D12][d3-12]).

Known limitation: Go's server rejects CFNetwork's bracketless IPv6
`CONNECT` (`CONNECT 2001:db8::1:443`) with its own `400` before any handler
runs ([research 0002 §5][r2-5]). cuttle cannot route, log, or count such a
request beyond the connection counters. Whether Safari sends this form is
open ([OI-0002][oi]); research open question 8 is answered by this
decision until that check says otherwise.

The admin UI and the control socket also use `net/http`.

### D3. Authority normalization

The normalized authority is used for matching, for the direct dial, and in
cuttle's own messages. It is a lowercase ASCII name without a trailing dot,
or a canonical IP address from `net/netip`, plus a port. Forwarded requests
keep the client's raw target: the `CONNECT` target as sent, and the raw path
and query of a plain-HTTP request (D5).

| Request target | Normalized | Note |
| --- | --- | --- |
| `CONNECT Example.COM:443` | `example.com:443` | ASCII lowercase |
| `CONNECT example.com.:443` | `example.com:443` | One trailing dot removed |
| `CONNECT [2001:DB8::1]:443` | `[2001:db8::1]:443` | `netip` canonical form |
| `CONNECT [::ffff:192.0.2.10]:443` | `192.0.2.10:443` | IPv4-mapped is unmapped |
| `GET http://Plain.Example/a` | `plain.example:80` | Port defaults to 80 |
| `CONNECT [fe80::1%25en0]:443` | `400` | Zones are refused |
| `CONNECT 127.1:443`, `2130706433:443`, `0x7f.1:443` | `400` | Numeric host that is not a dotted quad |
| `CONNECT` raw UTF-8 name | `400` | Send the A-label (`xn--`) form |
| `CONNECT example.com` | `400` | `CONNECT` requires a port |
| `CONNECT example.com:0443` | `400` | Port is 1–65535 without leading zeros |
| `CONNECT example.com:` | `400` | An empty port is refused |
| `GET http://h:/a` | `h:80` | An empty port in a URL means the default (RFC 3986 §6.2.3) |
| `GET https://example.com/`, `GET /`, `OPTIONS *`, userinfo | `400` | Only absolute-form `http://` and `CONNECT` |
| `CONNECT 2001:db8::1:443` | Go's `400` | Known limitation (D2) |
| `GET http://h/a%zz` | Go's `400` | Go's URL parser rejects an invalid escape in the path before the handler; in the query it is kept |

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

1. Accept: classify the source ([0003 D10][d3-10]).
2. Normalize the target (D3).
3. Port policy for remote clients only: the port must be in
   `proxy.remote.ports` (default `[80, 443]`), or the request gets `403`
   with rule `port`. Local clients have the same reach as without a proxy
   and face no port policy.
4. Rules in file order; the first match wins. With no match, use
   `proxy.default` for local clients or `proxy.remote.default` for remote
   ones.
5. A remote client whose action is `direct` gets `403` with rule
   `remote-direct`, unless `proxy.remote.allow_direct` is true
   ([0003 D10][d3-10]).
6. Loop check: unless the action is `block`, a request whose `Via` names
   this instance gets `502` ([0003 D2][d3-2]).
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
  `domains`.
- Actions are `direct`, `block`, or `upstream:<name>`.
- On a `direct` rule, `allow_loopback` and `allow_private` let local
  clients reach loopback, own, or private addresses that the destination
  policy would otherwise refuse ([0003 D11][d3-11]).

First match in file order is chosen over most-specific match because the
order an operator reads is the order cuttle applies. `cuttle explain`
([0006 D3][d6-3]) shows the matching rule for a target.

### D5. Actions and forwarding

- **`block`** — answer `403` with no outbound DNS lookup or connection.
- **`direct`, `CONNECT`** — resolve and check the addresses
  ([0003 D11][d3-11]), dial, then write exactly
  `HTTP/1.1 200 Connection established\r\n\r\n` and relay.
- **`upstream:<name>`, `CONNECT`** — cuttle writes the upstream request head
  itself ([0003 D3][d3-3]).
- **Plain HTTP, any route** — `httputil.ReverseProxy` with a `Rewrite`
  function and the route's `http.Transport`. `Rewrite` keeps the client's
  `Host`, restores the client's raw target (below), and applies the
  disclosure rule of [0003 D5][d3-5]: on `direct` and
  `disclosure = "none"` routes it removes the disclosure fields, and on
  `disclosure = "via"` routes it adds cuttle's `Via` entry
  ([0003 D2][d3-2]). The direct transport writes origin-form;
  an upstream transport, whose `Proxy` is fixed to that upstream, writes
  absolute-form ([research 0002 §2][r2-2]).
- **Responses** pass through `ReverseProxy`'s standard handling. A `407`
  becomes cuttle's own `502` in `ModifyResponse`
  ([0003 D6][d3-6]), because a `407` must never reach a client
  ([research 0002 §6][r2-6]). cuttle adds no `Via` to responses.
- **Upgrades.** `ReverseProxy` splices the connections after a `101`, so
  absolute-form WebSocket works on every route. Chrome tunnels `ws://`
  with `CONNECT` to port 80 instead ([research 0002 §2][r2-2]).

Request target. Left alone, Go would change it: `ReverseProxy` re-encodes
a query that contains `;` or an invalid `%` escape before calling `Rewrite`
(`cleanQueryParams`, `reverseproxy.go` l.497 and l.933–958), and the
transport writes `URL.RequestURI()`, which re-escapes the path (`url.go`
l.1171–1186, `request.go` l.656–658, Go 1.27.4). For example,
`GET http://h/a{b}|c?x=1;y=2&z=%zz` would reach the origin as
`GET /a%7Bb%7D%7Cc` without its query. `RequestURI()` returns `URL.Opaque`
unchanged when it is set, and the transport then writes it as the target.
So `Rewrite` splits the client's raw target, `r.RequestURI`, into authority,
path, and query, and sets:

| Route | `Out.URL.Opaque` | Target written |
| --- | --- | --- |
| `direct` | The raw path; empty when the path is empty | `<raw path>?<raw query>` in origin-form, or `/` for an empty path |
| `direct`, raw path starting with `//` | `//<raw authority><raw path>` | Absolute-form, which origins must accept (RFC 9112 §3.2.2); `RequestURI()` would otherwise read `//x` as an authority |
| `upstream:<name>` | `//<raw authority><raw path>` | `http://<raw authority><raw path>?<raw query>`, the client's absolute-form |

`Out.URL.RawQuery` gets the raw query, and `Out.URL.ForceQuery` is set when
the client sent `?` with an empty query. `Out.URL.Host` stays set for
dialing. A scratch check against Go 1.27.4 wrote
`GET /a{b}|c?x=1;y=2&z=%zz HTTP/1.1` direct and
`GET http://h/a{b}|c?x=1;y=2&z=%zz HTTP/1.1` through a proxy with these
settings.

Standard `net/http` handling applies to everything else: header-name
canonicalization and order, removal of hop-by-hop fields including
`Proxy-Connection`, and the body framing that Go chooses. These do not
disclose an intermediary in the sense of [0003 D5][d3-5]. The defaults that
would disclose one or change the client's content are switched off:

| Standard behavior | Effect if left on | cuttle |
| --- | --- | --- |
| `http.Transport` sends `User-Agent: Go-http-client/1.1` when the request has none | Names a non-browser client | `ReverseProxy` sends an empty `User-Agent`, which the transport omits, when the client sent none. The `CONNECT` head carries only client fields. cuttle never sends a `User-Agent` of its own. |
| `http.Transport` adds `Accept-Encoding: gzip` and decodes the body | Changes the request and the body | `DisableCompression: true` ([research 0002 §2][r2-2]) |
| `http.Transport` sends `Proxy-Authorization` from proxy URL userinfo | Credentials | Upstream URLs never carry userinfo |
| `http.ProxyFromEnvironment` | Environment proxies | The direct transport has `Proxy: nil`; an upstream transport has a fixed URL |
| `ReverseProxy` `Director`, or `SetXForwarded` in `Rewrite` | Adds `X-Forwarded-For` with the client's address | Only `Rewrite`; `SetXForwarded` is never called |
| `ReverseProxy` with `Rewrite` drops the client's `Forwarded`, `X-Forwarded-For`, `X-Forwarded-Host`, and `X-Forwarded-Proto` (`reverseproxy.go` l.491–494) | Differs between plain HTTP and `CONNECT` | On `direct` and `disclosure = "none"` routes the disclosure filter removes them on both paths anyway. On `disclosure = "via"` routes `Rewrite` copies the client's own back from `pr.In`, so both paths pass client fields alike ([0003 D5][d3-5]). |
| `cleanQueryParams` and `URL.RequestURI()` | Change the target | `Rewrite` restores the raw target (above) |
| `http.Transport` HTTP/2 | Different protocol on the wire | `ForceAttemptHTTP2: false`; plain HTTP only |
| `Max-Forwards` processing on `TRACE` and `OPTIONS` (RFC 9110 §7.6.2) | Decrementing or answering reveals a hop | Not implemented: forwarded unchanged ([0003 D5][d3-5]) |

Every row has a check in C3, C9, or [0003 U5 and U12][d3-ac].

Outbound connection pools:

- Direct transports exist per policy snapshot and per client class (local or
  remote). A pooled connection is reused only for requests with the same
  snapshot, class, host, and port, so a later request never inherits a dial
  that a different policy allowed.
- Each upstream has one transport, kept across reloads while its settings
  are unchanged ([0004 D6][d4-6]). Its connections are shared by all
  clients.
- A tunnel never shares its outbound connection.

Tunnel relay: copy both directions with 32 KiB buffers. On EOF in one
direction, half-close the write side of the other connection and keep
copying the remaining direction. Close both connections when both
directions finish, on any error, or after `proxy.idle_timeout` with no
bytes in either direction.

### D6. Responses generated by cuttle

Every response cuttle generates has a reason phrase, `Proxy-Status`
([RFC 9209][rfc9209]), `Content-Type: text/plain; charset=utf-8`,
`Cache-Control: no-store`, `X-Content-Type-Options: nosniff`,
`Connection: close`, and a one-line body such as
`cuttle: blocked by rule "ads" (ads.example.net:443)`. Only the normalized
authority and the rule name are reflected. These responses go to the client
only. Browsers discard the body of a failed `CONNECT` and show a generic
error ([research 0002 §6][r2-6]); the decision log and the admin UI explain
it.

| Case | Status | `Proxy-Status` error | Outcome |
| --- | --- | --- | --- |
| Unsupported target form or authority (D3) | 400 | `http_request_error` | `bad_request` |
| Remote port, rule or default `block`, remote `direct` | 403 | `http_request_denied` | `blocked` |
| Loop detected | 502 | `proxy_loop_detected` | `loop` |
| Name resolution failed | 502 | `dns_error` | `dns_error` |
| Every resolved address prohibited | 502 | `destination_ip_prohibited` | `prohibited` |
| Connection refused or reset | 502 | `connection_refused` | `refused` |
| TCP connect timed out | 504 | `connection_timeout` | `timeout` |
| No response head within the response timeout | 504 | `http_response_timeout` | `timeout` |
| Upstream held as unreachable ([0003 D8][d3-8]) | 503 | `destination_unavailable` | `unavailable` |
| Upstream answered `CONNECT` with non-2xx, or any `407` | 502, or 504 when it sent 504 | none; `received-status=<code>` | `upstream_status` |
| Response unparsable | 502 | `http_protocol_error` | `upstream_protocol` |
| Internal error | 500 | `proxy_internal_error` | `internal` |

- Requests that Go's server rejects before the handler, such as an
  oversized head (`431`), a malformed request line, or an invalid escape in
  the path (`400`), get Go's own response without `Proxy-Status` and no
  decision line.
- `403` always means cuttle's own policy. A refusal by a later hop arrives
  as `502` with `received-status`, so a curl user can tell the two apart
  ([0003 D6][d3-6]).
- cuttle never sends `407` and never relays one.
- Member format: `Proxy-Status: <name>; error=<type>; details="<reason>"`,
  plus `next-hop="<upstream>"` on upstream failures.

### D7. Byte accounting and live counters

Counted bytes are relayed bytes measured on the client side, once per
direction. `up` is client → destination and `down` is destination → client.

| Kind | `up` | `down` |
| --- | --- | --- |
| Tunnel | Bytes received from the client after the `CONNECT` head and written to the next hop | Bytes written to the client after the `200` head |
| Plain HTTP | Request body bytes forwarded, after transfer decoding | Response body bytes the client socket accepted, after transfer decoding |
| After a `101` | Bytes written to the next hop | Bytes written to the client |
| Response generated by cuttle | 0 | 0 |

Heads and framing are excluded in every case. Tunnel totals include the TLS
handshake and records. Counters advance during transfer: a tunnel adds to
its atomic counters as each write completes, so active tunnels contribute
before they close.

A byte counts when a socket write accepted it, never while it is still
buffered: `down` never includes bytes still in cuttle's buffers. Tunnels
add the result of each write. Plain-HTTP responses are flushed after each
body write and counted only when the flush succeeds; a failed write or
flush counts nothing for that call. Plain-HTTP `up` still counts the
request body bytes the transport read from the client, and after a `101`
cuttle counts the bytes it reads from and writes to the client connection.
Socket-level counting of uploads and the exact totals after a `101` are
left for C13 at M1.4.

Live counters are in memory, start at zero with the process, and are read
through the control socket (`cuttle status`, [0006 D4][d6-4]) and the admin
overview and live pages ([0005 D9][d5-9]).

| Counter | Labels |
| --- | --- |
| `connections_active`, `tunnels_active`, `http_requests_active` | — |
| `connections_total` | — |
| `connections_rejected_total` | `reason` = `source`, `limit` |
| `decisions_total` | `kind` = `connect`, `http`; `action`, or `none` for a refused target (D3) |
| `outcomes_total` | `outcome` (D6, plus `ok`, `client_closed`, `shutdown`) |
| `bytes_total` | `action`, `direction` = `up`, `down` |
| `upstream_outcomes_total` | `upstream`, `outcome` |
| `upstream_state` | `upstream`; value `up`, `down`, or `probing` with its start time ([0003 D8][d3-8]) |
| `stats_write_errors_total`, `stats_folded_total` | — (M2, [0005 D4][d5-4]) |

Labels are bounded: hosts never appear in counters. Per-host data lives in
the log and in statistics ([0005](0005-statistics-and-admin.md)). The status
response also lists active tunnels with authority, route, source, age, and
bytes, capped at 500 entries.

### D8. Decision log

cuttle writes one `log/slog` text line per completed decision at level
`info` ([0006 D7][d6-7]):

```text
time=2026-11-25T01:01:02.345Z level=INFO msg=decision id=4f2a
client=10.8.0.1:51234 source=remote kind=connect
target=wiki.internal.example:443 rule=restricted action=upstream
upstream=egress outcome=ok status=200 up=5321 down=88410 dur=12.402s
```

The line is wrapped here for width; it is one line in the log. Errors add
`error=<Proxy-Status type>` and `detail`. A request whose target is refused
(D3) has empty `target`, `rule`, `action`, and `upstream` fields, so nothing
of the target is logged. The log never contains paths, queries, header
values, bodies, or TLS contents.

## Non-goals

- **Response caching.** cuttle never terminates client TLS, so it cannot
  see, let alone cache, HTTPS content. Caching only plain `http://` would
  need storage, freshness rules, `Vary` and `Cache-Control` handling for a
  shared cache, and a privacy model, for little gain, because browsers keep
  their own caches. Caching HTTPS content would require TLS interception,
  which is out of scope ([0001 alternatives][d1-alt]).
- **TLS interception, transparent proxying, and filtering on paths,
  headers, or bodies.** The tunnel stays blind.

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
    AllowPrivate  bool
}

// Decide is pure: no I/O and no DNS.
func (p *Policy) Decide(t authority.Authority, k Kind, s SourceClass) Decision
```

The handler stores the decision in the request context, so the direct
transport's dial function applies the matching address policy.
`proxy.New(cfg, deps)` takes the listeners as `net.Listener` values, so
tests can supply listeners that report chosen remote addresses
([0007 D4][d7-4]).

## Failure behavior

- Policy failures answer before any DNS lookup or dial.
- Direct-route failures map to D6 and never retry another route.
- Once the `200` or a response head has reached the client, the status
  cannot change and a failure can only close the connection. A tunnel that
  then ends with an error still logs outcome `ok`, because a tunnel has no
  framing by which to judge completeness. A plain-HTTP response whose body
  then fails logs `upstream_protocol`, or `client_closed` when writing to
  the client failed, with the status that was sent. Both log the bytes the
  client socket accepted (D7).
- A request on a connection that the listener did not classify gets `500`
  and is never routed, so a missing source class fails closed. Its
  decision line has `source=unknown`, empty target fields, outcome
  `internal`, and error `proxy_internal_error`, and it is counted in
  `outcomes_total`.
- A client that closes before the decision completes is logged as
  `client_closed` and is not a failure.

## Acceptance criteria

All checks are T1 tests run with the mocks in
[0007](0007-test-environment-and-gates.md) unless marked T0.

| ID | Criterion | Check |
| --- | --- | --- |
| C1 | `CONNECT` with a `direct` rule reaches the HTTPS origin; Go and curl verify the test-CA certificate; a raw client receives exactly `HTTP/1.1 200 Connection established\r\n\r\n` and nothing else before origin bytes. | `TestConnectDirect`, `TestConnectExact200` |
| C2 | Bytes pipelined after the `CONNECT` head reach the origin. | `TestConnectPipelinedBytes` |
| C3 | Plain-HTTP `GET` requests for `http://h/a{b}|c?x=1;y=2&z=%zz`, `http://h/q?b=2&a=1;c=3`, `http://h/p?`, and `http://h//x`, and `POST` requests with a `Content-Length` body and with a chunked body, reach the origin with the client's raw path and query (origin-form, and absolute-form for `//x`), the client's `Host`, the client's other end-to-end fields and values, and the same body bytes. The origin receives no disclosure field of [0003 D5][d3-5], even when the client sent one, and no `Accept-Encoding` or `User-Agent` the client did not send. The response reaches the client with its status and body. `GET http://h/a%zz` gets Go's `400`. With `HTTP_PROXY`, `HTTPS_PROXY`, and `ALL_PROXY` pointing at a trap in cuttle's environment, a direct request still reaches the origin in origin-form and the trap gets no connection. | `TestHTTPDirect`, `TestHTTPRawTarget` |
| C4 | Two plain-HTTP requests on one client connection, to hosts with a `direct` and a `block` rule, in that order because cuttle's own responses close the connection, take those routes and produce two decision lines. | `TestHTTPReusedConnRoutesPerRequest` (Go and curl) |
| C5 | A `block` rule and a `block` default answer `403` with `Proxy-Status: <name>; error=http_request_denied`, zero lookups and dials in the test network, and zero counted bytes. | `TestBlockRule`, `TestBlockDefault` |
| C6 | A local client reaches `localhost:3000`, `127.0.0.1:8443`, and a public test address on port 8443 through `direct`; no port policy applies to local clients. | `TestLocalAnyPort` |
| C7 | Every row of the D3 table normalizes or fails as listed, including Go's own `400` for the bracketless IPv6 form. | T0 `TestAuthorityTable`; T1 `TestConnectBracketlessIPv6Rejected` |
| C8 | Rule order, `hosts`, `domains` label boundaries, `cidrs`, `ports`, and both defaults decide as specified. | T0 `TestPolicyTable` |
| C9 | A WebSocket echo round trip succeeds over `CONNECT` to port 80 and to port 443 (`wss`), and over an absolute-form `GET` with `Upgrade: websocket` on a `direct` route. | `TestWebSocket` |
| C10 | With `Expect: 100-continue`, the request body reaches the origin. An origin's `407`, from the mock origin's `/status?code=407`, reaches the client as `502` with `received-status=407`. | `TestHTTPExpectAnd407` |
| C11 | Origin-form, asterisk-form (`OPTIONS *`), absolute-form `https://`, and userinfo get cuttle's `400` with a decision line. A head of about 72 KiB gets Go's `431`, and a head of about 60 KiB is served; both are built from fewer than 500 field lines, so the byte limit decides (D2). | `TestFrontEndRejects`, `TestHeadSizeLimit` |
| C12 | A controlled transfer of 1,048,576 bytes down and 65,536 bytes up through a tunnel shows partial `bytes_total` progress while open and exact totals after close; `tunnels_active` returns to 0. | `TestTunnelByteCounters` |
| C13 | Plain-HTTP `up` and `down` equal the request and response body sizes, for `Content-Length` and chunked bodies. After a `101`, the bytes of an absolute-form WebSocket exchange of known size are added to the exchange's counts. A blocked request adds zero bytes. | `TestHTTPByteCounters` |
| C14 | A decision line has the D8 fields and contains no path or query from the request. | `TestDecisionLogFields` |
| C15 | A direct connection pooled for a local client is not reused for a remote client's request to the same host and port (remote client with `allow_direct`). | `TestDirectPoolSeparation` |

## Consequences

- One `CONNECT` tunnel can carry many HTTPS requests; cuttle sees only the
  authority and the stream sizes.
- Request parsing and framing are Go's well-tested code. cuttle's own
  parsing is limited to the `CONNECT` target, the split of the raw
  plain-HTTP target, and the disclosure fields.
- CFNetwork clients that use IPv6 literals fail until OI-0002 shows whether
  Safari needs that form.
- Operators write rules by name. A rule never matches a site reached by IP
  literal unless its addresses are listed in `cidrs`.

## Alternatives considered

- **A front end that reads and forwards raw head bytes.** It would keep
  header case and order and accept the bracketless IPv6 form, but it puts
  custom parsing and framing code on the attack surface. The user confirmed
  that case and order may change; only disclosure of an intermediary
  matters.
- **Glob or regular-expression patterns.** `*.example.com` versus
  `.example.com` conventions differ between tools; separate `hosts` and
  `domains` lists have no ambiguity.
- **Matching resolved addresses against `cidrs`.** This would put DNS on the
  path of upstream routes, which must not resolve locally.
- **Converting U-labels with `golang.org/x/net/idna`.** Chrome already sends
  A-labels; the conversion is deferred until a client needs it.

[d1-alt]: 0001-https-connect-proxy.md#alternatives-considered
[d3-2]: 0003-upstream-chaining-and-exposure.md#d2-via-entries-and-loop-detection
[d3-3]: 0003-upstream-chaining-and-exposure.md#d3-connect-through-an-upstream
[d3-5]: 0003-upstream-chaining-and-exposure.md#d5-non-disclosure-toward-an-egress-proxy
[d3-6]: 0003-upstream-chaining-and-exposure.md#d6-upstream-failures-and-proxy-status
[d3-8]: 0003-upstream-chaining-and-exposure.md#d8-unreachable-upstream-hold
[d3-10]: 0003-upstream-chaining-and-exposure.md#d10-source-classes-and-remote-policy
[d3-11]: 0003-upstream-chaining-and-exposure.md#d11-destination-address-policy-for-direct-routes
[d3-12]: 0003-upstream-chaining-and-exposure.md#d12-connection-limits
[d3-ac]: 0003-upstream-chaining-and-exposure.md#acceptance-criteria
[d4-6]: 0004-configuration-and-reload.md#d6-reload
[d5-4]: 0005-statistics-and-admin.md#d4-write-path
[d5-9]: 0005-statistics-and-admin.md#d9-dashboard
[d6-3]: 0006-packaging-paths-and-lifecycle.md#d3-command-line
[d6-4]: 0006-packaging-paths-and-lifecycle.md#d4-control-socket
[d6-7]: 0006-packaging-paths-and-lifecycle.md#d7-logging-and-rotation
[d7-4]: 0007-test-environment-and-gates.md#d4-instances-chains-and-loops
[oi]: ../OPEN_ISSUES.md
[r1-1]: ../research/0001-homebrew-launchd-xdg.md#1-homebrew-formula-for-a-go-program-in-a-personal-tap
[r1-6]: ../research/0001-homebrew-launchd-xdg.md#6-go-sqlite-drivers
[r2-1]: ../research/0002-proxy-chaining-and-interception.md#1-connect-through-chained-http-proxies
[r2-2]: ../research/0002-proxy-chaining-and-interception.md#2-plain-http-forwarding-in-absolute-form
[r2-5]: ../research/0002-proxy-chaining-and-interception.md#5-how-macos-clients-choose-and-talk-to-the-proxy
[r2-6]: ../research/0002-proxy-chaining-and-interception.md#6-what-clients-show-for-proxy-generated-failures
[r2-7]: ../research/0002-proxy-chaining-and-interception.md#7-exposure-risks
[rfc9209]: https://www.rfc-editor.org/rfc/rfc9209.html#section-2.3

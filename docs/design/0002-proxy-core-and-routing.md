# 0002 — Proxy core and routing

**Status** — Proposed (TASK-0001, 2026-11-25; revised in iteration 2).
Together with [0003](0003-upstream-chaining-and-exposure.md) it supersedes
[0001](0001-https-connect-proxy.md).

## Context

cuttle is an explicit forward proxy for Chrome, Safari, and terminal clients
on macOS. The repository is named `squid`; the formula, binary, service, and
XDG directories are named `cuttle`. Each HTTPS `CONNECT` tunnel and each plain
`http://` request is routed by the same rules to `direct`, an upstream proxy,
or `block`, and the traffic is counted.

The corporate proxy at the end of the home → office chain is picky: it must
receive exactly what Chrome on the office Mac would send it. cuttle therefore
forwards request heads as it received them, byte for byte, on upstream
routes ([0003 D1][d3-1]). That requirement shapes the front end below.

This record defines the proxy listener, request parsing, authority
normalization, rule semantics, forwarding, the responses cuttle generates,
byte accounting, live counters, and the decision log. Upstream forwarding
modes, loop detection, and exposure controls are in
[0003](0003-upstream-chaining-and-exposure.md). The configuration syntax is
in [0004](0004-configuration-and-reload.md). The control socket and CLI that
read live counters are in [0006](0006-packaging-paths-and-lifecycle.md).

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
| `internal/h1` | Raw HTTP/1.1 heads, framing, body relay, head edits (D2) |
| `internal/netx` | `Network` seam, address classes, direct dialer |
| `internal/proxy` | Listener, exchanges, tunnels, routes, chaining |
| `internal/stats` | Live counters, aggregator, SQLite store |
| `internal/admin` | Admin HTTP server, sessions, templates |
| `internal/control` | Control socket server and client |
| `internal/logx` | Rotating log writer |
| `internal/testenv` | Mocks for tests; never imported by `cmd/cuttle` |
| `tools/mockenv` | Development command that runs the mocks for client checks |

### D2. A front end that keeps the raw head

Upstream routes must forward the client's request head byte for byte,
including the case and order of field names ([0003 D1][d3-1]).
`net/http.Server` parses a head into a canonicalized `Header` map and
discards the original bytes, case, and order, and `http.Transport` writes
fields in its own order and case. That is the concrete requirement that
`net/http` cannot meet, so cuttle serves the proxy port with a small front
end of its own. It also accepts CFNetwork's bracketless IPv6 `CONNECT`,
which Go's server rejects before any handler runs
([research 0002 §5][r2-5], open question 8). The admin UI and the control
socket still use `net/http`.

`internal/h1` is the only HTTP parsing code in cuttle:

1. **Head reader.** Reads the start line and field lines from a 16 KiB
   `bufio.Reader` into a byte slice and stops after the empty line. Nothing
   after the head is consumed. The slice is the head as received.
2. **Start-line parsers.** A request line is `method SP target SP version`;
   a status line is `version SP code SP reason`. Versions are `HTTP/1.1`
   and `HTTP/1.0`.
3. **Field view.** `net/textproto` parses the recorded field lines into a
   map for case-insensitive lookups. Forwarding always uses the recorded
   bytes, never the map.
4. **Framing.** Decides the body length from the fields by RFC 9112 §6,
   with the strict rules below.
5. **Body relay.** Copies exactly the framed body: N bytes for
   `Content-Length`, or the chunked stream with its chunk extensions and
   trailers. The chunked end is found with `httputil.NewChunkedReader` over
   a byte-wise tee, so nothing past the body is read.
6. **Head edits.** Line operations on the recorded head: replace the start
   line, delete field lines by name, insert one field line before the empty
   line, and delete the trailing run of cuttle `Via` lines
   ([0003 D2][d3-2]).

Security-sensitive rules, each with its T0 test:

| Piece | Rule | Result | Test |
| --- | --- | --- | --- |
| Head size | Start line at most 8 KiB, head at most 64 KiB, at most 128 field lines | `431` for a request; `502` for a response | `TestHeadLimits` |
| Line syntax | CRLF line endings only. Bare CR, bare LF, or NUL anywhere in the head; obs-fold (a line starting with SP or HTAB); whitespace between a field name and its colon; a field name with non-token bytes | `400`; `502` for a response | `TestHeadSyntax` |
| Request line | Exactly two single spaces; the method is a token; the target is one of the D3 forms | `400` | `TestRequestLine` |
| `Host` | An HTTP/1.1 request has exactly one `Host`. For absolute-form, `Host` must equal the target authority after D3 normalization | `400` | `TestHostRules` |
| Request framing | `Transfer-Encoding` with `Content-Length`; `Transfer-Encoding` in HTTP/1.0; several `Content-Length` values that differ; a `Content-Length` that is not 1–18 digits; a `CONNECT` with a body | `400` | `TestRequestFramingSmuggling` |
| Transfer coding | `Transfer-Encoding` other than exactly `chunked` | `501` | `TestRequestFramingSmuggling` |
| Chunked body | Chunk size at most 16 hexadecimal digits; chunk line at most 8 KiB; trailers count against the head limits; bytes after the final CRLF stay for the next request | `400`, or close once the body has started | `TestChunkedRelay` |
| Response framing | The same field rules. `Transfer-Encoding` with `Content-Length`, an invalid `Content-Length`, or a coding other than `chunked` fails. Body length follows RFC 9112 §6.3 for `HEAD`, 1xx, 204, 304, and close-delimited bodies | `502 http_protocol_error` | `TestResponseFraming` |
| Ordering | One exchange at a time: the next request head is read only after the previous response is relayed; pipelined bytes wait in the buffer | — | `TestPipelinedRequests` |

Rejecting `Transfer-Encoding` with `Content-Length` outright, instead of
removing `Content-Length` as RFC 9112 §6.3 allows, keeps the head unchanged
and closes the classic smuggling case.

Connection rules:

- The head timeout is 30 s, and the keep-alive idle timeout is 120 s. A
  first byte of `0x16` (a TLS ClientHello) is logged as
  `tls_on_plain_listener`, and the connection is closed.
- A `CONNECT` is the last request on its connection: a tunnel or a
  cuttle-generated error follows, and then the connection closes.
- The request body and the response are relayed concurrently, so an
  interim `100 Continue` from the next hop reaches the client. cuttle never
  sends a `100` of its own. If the final response ends before the request
  body, cuttle closes both connections after relaying it.
- 1xx interim responses are relayed as received. A `101` turns the client
  connection and its outbound connection into a tunnel (D5).
- cuttle closes the client connection after relaying a response that ends
  its connection: one with `Connection: close`, an HTTP/1.0 response
  without keep-alive, or a close-delimited body.
- Every cuttle-generated error response closes the client connection, so
  cuttle never has to drain an unread request body.

### D3. Authority normalization

The normalized authority is used for matching, for the direct dial, and in
cuttle's own messages. It is a lowercase ASCII name without a trailing dot,
or a canonical IP address from `net/netip`, plus a port. Upstream routes
forward the client's request line unchanged ([0003 D3][d3-3]).

| Request target | Normalized | Note |
| --- | --- | --- |
| `CONNECT Example.COM:443` | `example.com:443` | ASCII lowercase |
| `CONNECT example.com.:443` | `example.com:443` | One trailing dot removed |
| `CONNECT [2001:DB8::1]:443` | `[2001:db8::1]:443` | `netip` canonical form |
| `CONNECT 2001:db8::1:443` | `[2001:db8::1]:443` | CFNetwork form ([research 0002 §5][r2-5]) |
| `CONNECT [::ffff:192.0.2.10]:443` | `192.0.2.10:443` | IPv4-mapped is unmapped |
| `GET http://Plain.Example/a` | `plain.example:80` | Port defaults to 80 |
| `CONNECT [fe80::1%en0]:443` | `400` | Zones are refused |
| `CONNECT 127.1:443`, `2130706433:443`, `0x7f.1:443` | `400` | Numeric host that is not a dotted quad |
| `CONNECT` raw UTF-8 name | `400` | Send the A-label (`xn--`) form |
| `CONNECT example.com` | `400` | `CONNECT` requires a port |
| `CONNECT example.com:0443` | `400` | Port is 1–65535 without leading zeros |
| `GET https://example.com/`, `GET /`, `OPTIONS *`, userinfo | `400` | Only absolute-form `http://` and `CONNECT` |

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

1. Accept: classify the source ([0003 D10][d3-10]).
2. Read and check the head (D2), and normalize the target (D3).
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
  `HTTP/1.1 200 Connection established\r\n\r\n` and relay. Bytes the client
  pipelined after its head are relayed first ([research 0002 §1][r2-1]).
- **`direct`, plain HTTP** — forward the recorded head with these edits and
  no others: the request line becomes origin-form with the same method and
  version; `Proxy-Connection` and `Proxy-Authorization` lines are deleted;
  the trailing run of cuttle `Via` lines is deleted ([0003 D2][d3-2]); a
  `Host` line is inserted if an HTTP/1.0 request has none. Field order and
  case, `Connection`, `Keep-Alive`, `TE`, `Upgrade`, and the body framing
  are kept.
- **`upstream:<name>`** — [0003 D3][d3-3] and [D4][d3-4]: the head as
  received, with only the forwarding mode's `Via` edit.
- **Responses** are relayed unchanged on every route, with one exception:
  a `407` becomes cuttle's own `502` ([0003 D6][d3-6]), because a `407`
  must never reach a client ([research 0002 §6][r2-6]).
- **Upgrades.** After a `101` response, the client connection and its
  outbound connection become a tunnel, so absolute-form WebSocket and other
  upgrades work on every route. Chrome tunnels `ws://` with `CONNECT` to
  port 80 instead ([research 0002 §2][r2-2]).

Connection mapping:

- Each client connection owns its outbound connections: at most one per
  upstream, and at most one direct connection, for the most recent direct
  authority. A request reuses the owned connection for its route; a request
  for another direct authority replaces the direct connection.
- An owned connection is reused only while the settings it was opened with
  are unchanged in the current policy snapshot ([0004 D6][d4-6]).
- Owned connections close with their client connection. If an idle upstream
  connection closes, cuttle closes the client connection once no exchange is
  in flight, so the client sees the event it would see from that proxy. An
  idle direct connection that closes is dropped and redialed when needed.
- So connection-level fields such as `Connection`, `Keep-Alive`, and
  `Proxy-Connection` still describe the connection they travel on, and an
  upstream sees each client connection's requests in order on one
  connection ([0003 D5][d3-5]).

Deviation from RFC 9110: intermediaries remove hop-by-hop fields
(§7.6.1) and add `Via` to forwarded messages (§7.6.3)
([research 0002 §2][r2-2], [§3][r2-3]). cuttle keeps connection-level
fields because each client connection maps onto one outbound connection per
route, adds `Via` only on hops between cuttle instances, and leaves
responses unchanged. Exact forwarding requires this, and one forwarding path
then serves every route. cuttle never adds `Forwarded` or `X-Forwarded-*`.

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
authority and the rule name are reflected. Browsers discard the body of a
failed `CONNECT` and show a generic error
([research 0002 §6][r2-6]); the decision log and the admin UI explain it.

| Case | Status | `Proxy-Status` error | Outcome |
| --- | --- | --- | --- |
| Malformed request, target, or framing (D2, D3) | 400 | `http_request_error` | `bad_request` |
| Unsupported transfer coding | 501 | `http_request_error` | `bad_request` |
| Request head over the limits | 431 | `http_request_error` | `bad_request` |
| Remote port, rule or default `block`, remote `direct` | 403 | `http_request_denied` | `blocked` |
| Loop detected | 502 | `proxy_loop_detected` | `loop` |
| Name resolution failed | 502 | `dns_error` | `dns_error` |
| Every resolved address prohibited | 502 | `destination_ip_prohibited` | `prohibited` |
| Connection refused or reset | 502 | `connection_refused` | `refused` |
| TCP connect timed out | 504 | `connection_timeout` | `timeout` |
| No response head within the response timeout | 504 | `http_response_timeout` | `timeout` |
| Upstream held as unreachable ([0003 D8][d3-8]) | 503 | `destination_unavailable` | `unavailable` |
| Upstream answered `CONNECT` with non-2xx, or any `407` | 502, or 504 when it sent 504 | none; `received-status=<code>` | `upstream_status` |
| Response head or framing invalid | 502 | `http_protocol_error` | `upstream_protocol` |
| Internal error | 500 | `proxy_internal_error` | `internal` |

- `403` always means cuttle's own policy. A refusal by a later hop arrives
  as `502` with `received-status`, so a curl user can tell the two apart
  ([0003 D6][d3-6]).
- cuttle never sends `407` and never relays one.
- Member format: `Proxy-Status: <name>; error=<type>; details="<reason>"`,
  plus `next-hop="<upstream>"` on upstream failures.
- After part of a response has been relayed, a failure can only close the
  client connection.

### D7. Byte accounting and live counters

Counted bytes are relayed bytes measured on the client side, once per
direction. `up` is client → destination and `down` is destination → client.

| Kind | `up` | `down` |
| --- | --- | --- |
| Tunnel | Bytes received from the client after the `CONNECT` head and written to the next hop | Bytes written to the client after the `200` head |
| Plain HTTP | The request as received from the client: head and framed body | The response as written to the client: interim responses, head, and framed body |
| After a `101` | Added to the exchange's `up` | Added to the exchange's `down` |
| Response generated by cuttle | 0 | 0 |

Tunnel totals include the TLS handshake and records. Counters advance during
transfer: a tunnel adds to its atomic counters as each write completes, so
active tunnels contribute before they close.

Live counters are in memory, start at zero with the process, and are read
through the control socket (`cuttle status`, [0006 D4][d6-4]) and the admin
overview and live pages ([0005 D9][d5-9]).

| Counter | Labels |
| --- | --- |
| `connections_active`, `tunnels_active`, `http_requests_active` | — |
| `connections_total` | — |
| `connections_rejected_total` | `reason` = `source`, `limit` |
| `decisions_total` | `kind` = `connect`, `http`; `action` |
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
time=2026-11-25T10:01:02.345Z level=INFO msg=decision id=4f2a
client=10.8.0.1:51234 source=remote kind=connect
target=wiki.corp.example.com:443 rule=office-only action=upstream
upstream=corp outcome=ok status=200 up=5321 down=88410 dur=12.402s
```

The line is wrapped here for width; it is one line in the log. Errors add
`error=<Proxy-Status type>` and `detail`. The log never contains paths,
queries, header values, bodies, or TLS contents.

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

// Head is a request or response head exactly as received.
type Head struct {
    Raw    []byte               // start line, field lines, final CRLF
    Fields textproto.MIMEHeader // parsed view, for lookups only
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

`proxy.New(cfg, deps)` takes the listeners as `net.Listener` values, so
tests can supply listeners that report chosen remote addresses
([0007 D4][d7-4]).

## Failure behavior

- Parsing or policy failures answer before any DNS lookup or dial.
- Direct-route failures map to D6 and never retry another route.
- After `200` or part of a response has been relayed, a failure can only
  close the connection; the outcome is logged with the bytes relayed so far.
- A client that closes before the decision completes is logged as
  `client_closed` and is not a failure.

## Acceptance criteria

All checks are T1 tests run with the mocks in
[0007](0007-test-environment-and-gates.md) unless marked T0.

| ID | Criterion | Check |
| --- | --- | --- |
| C1 | `CONNECT` with a `direct` rule reaches the HTTPS origin; Go and curl verify the test-CA certificate; a raw client receives exactly `HTTP/1.1 200 Connection established\r\n\r\n` and nothing else before origin bytes. | `TestConnectDirect`, `TestConnectExact200` |
| C2 | Bytes pipelined after the `CONNECT` head reach the origin. | `TestConnectPipelinedBytes` |
| C3 | A raw client's plain-HTTP `GET`, and `POST` with a `Content-Length` body and with a chunked body and trailer, arrive at the origin with the origin-form request line, the client's other field lines in their original case and order minus `Proxy-Connection` and `Proxy-Authorization`, no `Via` or `X-Forwarded-For`, and the body bytes unchanged. The origin's response reaches the client byte for byte. | `TestHTTPDirect` |
| C4 | Two plain-HTTP requests on one client connection to hosts with different rules take different routes and produce two decision lines; requests on one client connection for one direct authority share one origin connection. | `TestHTTPReusedConnRoutesPerRequest` (Go and curl) |
| C5 | A `block` rule and a `block` default answer `403` with `Proxy-Status: <name>; error=http_request_denied`, zero lookups and dials in the test network, and zero counted bytes. | `TestBlockRule`, `TestBlockDefault` |
| C6 | A local client reaches `localhost:3000`, `127.0.0.1:8443`, and a public test address on port 8443 through `direct`; no port policy applies to local clients. | `TestLocalAnyPort` |
| C7 | A bracketless IPv6 `CONNECT` (`CONNECT 2001:db8::1:443`) is routed by a `cidrs` rule to `direct`, and the test network records a dial to `[2001:db8::1]:443`. | `TestConnectBracketlessIPv6` |
| C8 | Every row of the D3 table normalizes or fails as listed. | T0 `TestAuthorityTable` |
| C9 | Rule order, `hosts`, `domains` label boundaries, `cidrs`, `ports`, and both defaults decide as specified. | T0 `TestPolicyTable` |
| C10 | Each row of the D2 rules table holds. | T0 `TestHeadLimits`, `TestHeadSyntax`, `TestRequestLine`, `TestHostRules`, `TestRequestFramingSmuggling`, `TestChunkedRelay`, `TestResponseFraming`, `TestPipelinedRequests` |
| C11 | A WebSocket echo round trip succeeds over `CONNECT` to port 80 and to port 443 (`wss`), and over an absolute-form `GET` with `Upgrade: websocket` on a `direct` route. | `TestWebSocket` |
| C12 | With `Expect: 100-continue`, the origin's `100 Continue` reaches the client before the body and cuttle sends none of its own. An origin's `407` reaches the client as `502` with `received-status=407`. | `TestHTTPInterimAnd407` |
| C13 | A TLS ClientHello on the proxy port and each D3 `400` form produce the D6 response or a close, and a decision line or counter. | `TestFrontEndRejects` |
| C14 | A controlled transfer of 1,048,576 bytes down and 65,536 bytes up through a tunnel shows partial `bytes_total` progress while open and exact totals after close; `tunnels_active` returns to 0. | `TestTunnelByteCounters` |
| C15 | Plain HTTP `up` and `down` equal the bytes a raw client wrote and read for that exchange, for `Content-Length` and chunked bodies. | `TestHTTPByteCounters` |
| C16 | A decision line has the D8 fields and contains no path or query from the request. | `TestDecisionLogFields` |

## Consequences

- One `CONNECT` tunnel can carry many HTTPS requests; cuttle sees only the
  authority and the stream sizes.
- `internal/h1` is cuttle's own code on the attack surface. It is kept to
  the seven pieces in D2, and every rule there has a test.
- Plain HTTP pays for exactness: there is no connection pool across client
  connections, so a client that switches direct hosts on one connection
  causes a new dial each time. Most traffic is HTTPS through `CONNECT`.
- Operators write rules by name. A rule never matches a site reached by IP
  literal unless its addresses are listed in `cidrs`.

## Alternatives considered

- **`net/http.Server` with `httputil.ReverseProxy` or `http.Transport`.**
  Less code, but header names are canonicalized and their order is lost,
  which breaks exact forwarding, and CFNetwork's IPv6 form fails before
  cuttle sees it.
- **`net/http.Server` plus a listener that records raw bytes.** The server
  does not expose where one request ends and the next begins, so the raw
  head cannot be matched to a request reliably.
- **Glob or regular-expression patterns.** `*.example.com` versus
  `.example.com` conventions differ between tools; separate `hosts` and
  `domains` lists have no ambiguity.
- **Matching resolved addresses against `cidrs`.** This would put DNS on the
  path of upstream routes, which must not resolve locally.
- **Converting U-labels with `golang.org/x/net/idna`.** Chrome already sends
  A-labels; the conversion is deferred until a client needs it.

[d1-alt]: 0001-https-connect-proxy.md#alternatives-considered
[d3-1]: 0003-upstream-chaining-and-exposure.md#d1-upstreams-and-forwarding-modes
[d3-2]: 0003-upstream-chaining-and-exposure.md#d2-head-edits-via-and-loop-detection
[d3-3]: 0003-upstream-chaining-and-exposure.md#d3-connect-through-an-upstream
[d3-4]: 0003-upstream-chaining-and-exposure.md#d4-plain-http-through-an-upstream
[d3-5]: 0003-upstream-chaining-and-exposure.md#d5-what-an-exact-mode-upstream-sees
[d3-6]: 0003-upstream-chaining-and-exposure.md#d6-upstream-failures-and-proxy-status
[d3-8]: 0003-upstream-chaining-and-exposure.md#d8-unreachable-upstream-hold
[d3-10]: 0003-upstream-chaining-and-exposure.md#d10-source-classes-and-remote-policy
[d3-11]: 0003-upstream-chaining-and-exposure.md#d11-destination-address-policy-for-direct-routes
[d4-6]: 0004-configuration-and-reload.md#d6-reload
[d5-4]: 0005-statistics-and-admin.md#d4-write-path
[d5-9]: 0005-statistics-and-admin.md#d9-dashboard
[d6-3]: 0006-packaging-paths-and-lifecycle.md#d3-command-line
[d6-4]: 0006-packaging-paths-and-lifecycle.md#d4-control-socket
[d6-7]: 0006-packaging-paths-and-lifecycle.md#d7-logging-and-rotation
[d7-4]: 0007-test-environment-and-gates.md#d4-instances-chains-and-loops
[r1-1]: ../research/0001-homebrew-launchd-xdg.md#1-homebrew-formula-for-a-go-program-in-a-personal-tap
[r1-6]: ../research/0001-homebrew-launchd-xdg.md#6-go-sqlite-drivers
[r2-1]: ../research/0002-proxy-chaining-and-interception.md#1-connect-through-chained-http-proxies
[r2-2]: ../research/0002-proxy-chaining-and-interception.md#2-plain-http-forwarding-in-absolute-form
[r2-3]: ../research/0002-proxy-chaining-and-interception.md#3-loop-detection-across-chained-proxies
[r2-5]: ../research/0002-proxy-chaining-and-interception.md#5-how-macos-clients-choose-and-talk-to-the-proxy
[r2-6]: ../research/0002-proxy-chaining-and-interception.md#6-what-clients-show-for-proxy-generated-failures
[r2-7]: ../research/0002-proxy-chaining-and-interception.md#7-exposure-risks
[rfc9209]: https://www.rfc-editor.org/rfc/rfc9209.html#section-2.3

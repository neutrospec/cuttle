# 0003 — Upstream chaining and exposure

**Status** — Proposed (TASK-0001, 2026-11-25). Together with
[0002](0002-proxy-core-and-routing.md) it supersedes
[0001](0001-https-connect-proxy.md).

## Context

cuttle runs at home and at the office. The office Mac keeps a VPN connected
to the home network. Home cuttle hands office-only sites to office cuttle,
which hands them to the corporate proxy. The corporate proxy accepts plain
HTTP proxy connections without authentication and re-signs TLS with a
private root CA. The office listener is reachable over the VPN, so it needs
exposure controls that a loopback-only proxy does not.

This record defines upstreams, forwarding through them, failure reporting,
loop detection, timeouts, listeners, source and destination checks, and the
home → office → corporate configuration. Evidence is in
[research 0002][r2].

## Decisions

### D1. Upstreams

- An upstream is a named plain-HTTP proxy at `host:port`. cuttle speaks
  HTTP/1.1 to it without TLS and without credentials, and never answers or
  relays `407`.
- cuttle resolves an upstream's own host name through the `Network` seam
  ([0002 Interfaces][d2-if]). The destination checks of D9 do not apply to
  upstream addresses, which are often loopback or private.
- A failed upstream ends the request. cuttle never tries `direct` or another
  upstream. Failover lists are a later topic. An upstream that could not be
  reached is held briefly so later requests fail at once (D12).
- Configuration validation rejects an upstream whose address equals one of
  this instance's listener addresses, comparing IP literals and treating
  `localhost` as loopback. At dial time, an upstream whose resolved address
  and port equal a bound listener fails with `502 proxy_loop_detected`.
  Apache Traffic Server performs the same next-hop check
  ([research 0002 §3][r2-3]).

### D2. CONNECT through an upstream

1. Dial the upstream within its `connect_timeout`.
2. Write a fresh request head built from the normalized authority
   ([0002 D3][d2-3]):

   ```text
   CONNECT wiki.corp.example.com:443 HTTP/1.1
   Host: wiki.corp.example.com:443
   Via: 1.1 cuttle-home
   User-Agent: <copied only when the client sent one>
   ```

   `Via` holds the received entries plus this instance's entry (D5). No
   other client field is copied ([research 0002 §1][r2-1]).
3. Read the response head with `http.ReadResponse` within the upstream's
   `handshake_timeout`.
4. On any 2xx, ignore `Content-Length` and `Transfer-Encoding`, never read a
   body, and write exactly `HTTP/1.1 200 Connection established\r\n\r\n` to
   the client. Relay bytes already buffered from the upstream after its head
   to the client, and bytes the client pipelined to the upstream, before
   the normal relay ([research 0002 §1][r2-1]).
5. On anything else, answer the client per D4 and close both connections.

Every hop answers its client only after its own upstream answered, so the
home client receives `200` only after the whole chain did. Chrome accepts
only an exact `200`, while curl accepts any 2xx
([research 0002 §6][r2-6]).

### D3. Plain HTTP through an upstream

- Keep absolute-form with the normalized authority, because the next hop is
  a proxy ([research 0002 §2][r2-2]). Apply the hop-by-hop and `Via` rules
  of [0002 D5][d2-5].
- Use one `http.Transport` per upstream with `Proxy` fixed to that upstream
  and `DisableCompression: true`. Connections to an upstream are pooled
  across destinations.
- Relay the upstream's response unchanged apart from hop-by-hop fields and
  `Via`, because it may come from the origin. An upstream `407` becomes
  `502` with `received-status=407`, and `Proxy-Authenticate` is never
  relayed. A corporate block page with another status is relayed as-is.

### D4. Upstream failures and Proxy-Status

| Upstream event | Client status | Own `Proxy-Status` member |
| --- | --- | --- |
| Upstream host name not resolvable | 502 | `error=dns_error; next-hop="<upstream>"` |
| Refused, reset, or unreachable | 502 | `error=connection_refused; next-hop="<upstream>"` |
| TCP connect timeout | 504 | `error=connection_timeout; next-hop="<upstream>"` |
| No response head within `handshake_timeout` | 504 | `error=http_response_timeout; next-hop="<upstream>"` |
| Response head unparsable | 502 | `error=http_protocol_error; next-hop="<upstream>"` |
| Non-2xx `CONNECT` answer other than 504, `407` included | 502 | `received-status=<code>; next-hop="<upstream>"` |
| `504` | 504 | `received-status=504; next-hop="<upstream>"` |
| Held as unreachable (D12) | 503 | `error=destination_unavailable; next-hop="<upstream>"` |
| Closed after the `200` | Close the client connection | — |

- `403` from cuttle always means its own policy
  ([0002 D6][d2-6]). A refusal further along the chain arrives as `502`,
  with the refusing hop's code in `received-status`.
- `504` is relayed as `504` so that timeouts stay visible through a chain.
- When an upstream's non-2xx answer carries `Proxy-Status`, cuttle copies
  those members, up to 1 KiB of field value, and appends its own member
  last. RFC 9209 lists the member closest to the origin first
  ([RFC 9209 §2][rfc9209]). Example after the corporate proxy refuses:

  ```text
  Proxy-Status: cuttle-office; received-status=403; next-hop="corp",
                cuttle-home; received-status=502; next-hop="office"
  ```

  This is one field line on the wire. In the research chain tests, `home`
  relayed `502` with `received-status` in the same way
  ([research 0002 §1][r2-1]).

### D5. Via and loop detection

- The top-level `name` is the instance pseudonym. It is required, matches
  `^[a-z][a-z0-9-]{0,62}$`, and must be unique among chained instances. The
  starter configuration generates `cuttle-<6 random hex digits>`, and the
  deployment guide recommends names such as `cuttle-home` and
  `cuttle-office` ([research 0002 §3][r2-3]).
- cuttle appends `1.1 <name>` to `Via` on every message it forwards:
  `CONNECT` and plain HTTP to an upstream, plain HTTP to an origin, and every
  relayed response. A direct `CONNECT` forwards no message and adds no `Via`.
- On every request, cuttle parses all `Via` field lines, takes the
  `received-by` token of each member, and compares it with its own name
  without regard to case. A match fails the request with
  `502 proxy_loop_detected` unless the policy blocks it
  ([0002 D4][d2-4] step 6).
- An upstream with `via = false` receives no `Via` field at all. It exists
  only for a non-cuttle upstream that mishandles `Via`
  ([OI-0001][oi]). Instances that chain to each other keep it on, so loop
  detection between them is unaffected.

`Via` is used because RFC 9110 requires it on forwarded messages, names loop
avoidance as a purpose, and covers `CONNECT`, while `Max-Forwards` does not
apply to `CONNECT`. Squid and Apache Traffic Server use it the same way. In
the research harness, a loop stopped after 2 `CONNECT` requests with the
check and ran to 40 without it ([research 0002 §3][r2-3]). Backstops are
the listener check in D1, the connection limits in D10, and the timeouts in
D6.

### D6. Timeouts

| Timeout | Default | Key | Scope |
| --- | --- | --- | --- |
| Request head | 30 s | fixed | Reading a client request head |
| Keep-alive idle | 120 s | fixed | Between plain-HTTP requests on a client connection |
| Direct connect | 10 s | `proxy.connect_timeout` | TCP connect for a direct route, all addresses |
| Upstream connect | 10 s | `connect_timeout` in `[[upstream]]` | TCP connect to the upstream |
| Upstream handshake | 30 s | `handshake_timeout` in `[[upstream]]` | From the `CONNECT` head written to the response head read |
| Response header | 120 s | fixed | Plain HTTP, direct and upstream |
| Tunnel idle | 1 h | `proxy.idle_timeout` | No bytes in either direction; `0s` disables it |
| Shutdown drain | 5 s | fixed | [0006 D6][d6-6] |

Chain budget: an upstream that is itself a proxy must get a
`handshake_timeout` longer than that proxy's slowest answer. For an
upstream cuttle, that is its own upstream's `connect_timeout` plus
`handshake_timeout`, plus a 10 s margin. Otherwise the earlier hop reports
`504` while the later hop still succeeds and holds an orphaned tunnel
([research 0002 §1][r2-1]). With defaults at the office (10 s + 30 s), home
uses `handshake_timeout = "50s"` for its `office` upstream. cuttle cannot
see another instance's settings, so the deployment guide states this rule.

### D7. Listeners

- `proxy.listen` is a list of `IP:port` literals. Host names and the
  wildcard addresses `0.0.0.0` and `::` are configuration errors. A wildcard
  on an office laptop would expose an unauthenticated proxy on the
  corporate LAN ([research 0002 §7][r2-7]).
- A loopback listener that fails to bind stops startup with exit status 1.
- A non-loopback listener requires a non-empty `proxy.remote.sources`. If it
  fails to bind, for example because the VPN is down at login, cuttle logs
  a warning and retries every 30 s while the other listeners serve.
  `cuttle status` shows the listener as `retrying` with the error.
- Listener changes take effect at the next start
  ([0004 D6][d4-6]).

### D8. Source classes and remote policy

- On accept, cuttle unmaps the peer address and classifies it. Loopback
  sources are `local`. Sources inside `proxy.remote.sources` are `remote`.
  Anything else is closed at once, before any byte is read or written, and
  counted in `connections_rejected_total{reason="source"}`; a warning is
  logged at most once per source per minute.
- The source is checked again before each request on a kept-alive
  connection. After a reload removes a source, that connection is closed
  before its next request is read.
- Rules apply to remote clients as to local ones. When no rule matches,
  `proxy.remote.default` applies; its default is `block`.
- A remote client whose action is `direct` gets `403` with rule
  `remote-direct`, unless `proxy.remote.allow_direct` is true. Then D9
  limits it to public addresses. `proxy.remote.default = "direct"` is a
  configuration error unless `allow_direct` is true.

Home needs only the office's upstream route. A remote `direct` route would
give the home side the office Mac's network position, including its
loopback admin UI and other local services ([research 0002 §7][r2-7]).

### D9. Destination address policy for direct routes

1. An IP-literal target yields its address. A name is resolved through
   `Network.LookupNetIP`; a failure answers `502 dns_error`.
2. Unmap IPv4-mapped addresses. For the NAT64 prefix `64:ff9b::/96`, classify
   the embedded IPv4 address.
3. Classify each address and drop the prohibited ones for this client and
   rule. If none remain, answer `502 destination_ip_prohibited` and log the
   classes that were refused.
4. Dial only the remaining addresses, as literals, through
   `Network.DialContext`. Attempts follow the resolver's order, starting the
   next address 300 ms after the previous one if it has not connected, as
   in RFC 8305. There is no second lookup, so a rebinding answer cannot
   slip in between the check and the connection
   ([research 0002 §7][r2-7]).

| Class | Addresses | Local client | Remote client with `allow_direct` |
| --- | --- | --- | --- |
| `loopback` | `127.0.0.0/8`, `::1` | Only through a rule with `allow_loopback` | Denied |
| `self` | This host's interface addresses, refreshed at most every 10 s | Only through a rule with `allow_loopback` | Denied |
| `unusable` | `0.0.0.0/8`, `::`, `169.254.0.0/16`, `fe80::/10`, `224.0.0.0/4`, `ff00::/8`, `240.0.0.0/4` | Denied | Denied |
| `private` | `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `100.64.0.0/10`, `fc00::/7` | Allowed | Denied |
| `public` | Everything else | Allowed | Allowed |

- A browser applies its local-network protections to addresses it
  resolves. Through a proxy it never sees the address, so a page whose name
  rebinds to `127.0.0.1` could reach local services through cuttle. cuttle
  therefore refuses loopback and its own addresses unless a rule names them.
- The starter configuration has a `loopback` rule with
  `hosts = ["localhost"]`, `cidrs = ["127.0.0.0/8", "::1/128"]`, and
  `allow_loopback = true`. curl proxies `localhost` and `127.0.0.1`, which
  Chrome never does ([research 0002 §5][r2-5]), so terminal clients keep
  working.
- The admin port is reachable through cuttle only by a local client through
  such a rule, which equals local access and still meets the password and
  Host checks of [0005](0005-statistics-and-admin.md). Remote clients
  cannot reach loopback or `self` addresses at all.
- Upstream routes do no local resolution and no address check; the next
  proxy resolves.

### D10. Connection limits

- `proxy.max_connections` (default 1024) caps client connections.
  `proxy.remote.max_connections_per_source` (default 128) caps each remote
  source. A connection over a cap is closed right after accept and counted
  in `connections_rejected_total{reason="limit"}`.
- Each connection or tunnel uses at most two file descriptors. Go raises the
  soft `RLIMIT_NOFILE` to the hard limit at start; `cuttle status` reports
  the effective limit. Assumption A5 covers launchd's limits.

### D11. Home, office, and corporate deployment

```text
home browser or curl
  -> home cuttle 127.0.0.1:3128 (name cuttle-home)
     office-only domains -> upstream "office" 10.8.0.6:3128 over the VPN
  -> office cuttle 10.8.0.6:3128 (name cuttle-office; source 10.8.0.1)
     office-only domains -> upstream "corp" proxy.corp.example:8080
  -> corporate proxy (plain HTTP; re-signs TLS with a private root)
  -> origin
```

The addresses are placeholders. `sources` on the office side must hold the
address the office sees for the home Mac. A rejected attempt logs it.

Home `~/.config/cuttle/config.toml`:

```toml
name = "cuttle-home"

[proxy]
listen = ["127.0.0.1:3128"]
default = "direct"

[[upstream]]
name = "office"
address = "10.8.0.6:3128"
handshake_timeout = "50s"  # office corp connect + handshake + 10 s

[[rule]]
name = "loopback"
hosts = ["localhost"]
cidrs = ["127.0.0.0/8", "::1/128"]
action = "direct"
allow_loopback = true

[[rule]]
name = "office-only"
domains = ["corp.example", "intranet.example.net"]
action = "upstream:office"
```

Office `~/.config/cuttle/config.toml`:

```toml
name = "cuttle-office"

[proxy]
listen = ["127.0.0.1:3128", "10.8.0.6:3128"]
default = "upstream:corp"  # local office clients use the corporate proxy

[proxy.remote]
sources = ["10.8.0.1/32"]  # the home Mac as seen over the VPN
default = "block"

[[upstream]]
name = "corp"
address = "proxy.corp.example:8080"

[[rule]]
name = "loopback"
hosts = ["localhost"]
cidrs = ["127.0.0.0/8", "::1/128"]
action = "direct"
allow_loopback = true

[[rule]]
name = "office-only"  # the only route open to the home instance
domains = ["corp.example", "intranet.example.net"]
action = "upstream:corp"
```

Trust. cuttle needs no certificate or CA ([research 0002 §4][r2-4]). The
home client must trust the corporate root for office-only sites:

- Chrome and Safari on macOS: add the root to the login keychain and set
  "Secure Sockets Layer (SSL)" to "Always Trust". Chrome's verifier uses
  keychain trust for locally added roots. This has not been observed
  through the chain ([OI-0005][oi]).
- `/usr/bin/curl`: pass `--cacert corp-root.pem` or set `CURL_CA_BUNDLE`.
  Whether its default path honors keychain trust is unknown
  ([OI-0005][oi]).
- Safari also requires the re-signed leaves to have a SAN, the serverAuth
  EKU, and at most 825 days of validity ([OI-0001][oi]).

The operator accepts these consequences:

- The trusted corporate root can vouch for any name on that Mac, not only
  for routed sites.
- The corporation sees decrypted requests, cookies, and credentials for
  routed sites, and the `Via` pseudonyms in its logs.
- Client-certificate authentication and apps that pin keys fail on that
  path, as they do inside the office.
- Chrome may still look up office-only names with the home resolver
  ([research 0002 §5][r2-5]).

### D12. Unreachable upstream hold

Adopted. When the VPN to the office is down, packets to office cuttle are
often dropped rather than refused, so each request would wait the full
`connect_timeout` (10 s by default). A browser opens several connections
per page and retries, so the user waits repeatedly for the same answer.
cuttle therefore remembers briefly that an upstream cannot be reached and
fails at once. Fail-closed still holds: a held upstream is never replaced
by `direct` or by another upstream.

- **Trigger.** Only a failure to reach the upstream itself: TCP connect
  refused, reset, unreachable, or timed out, or failure to resolve the
  upstream's own host name. A non-2xx answer, a handshake timeout, and
  errors after the `200` do not trigger it, because the upstream was
  reachable and the fault may lie further along the chain. A dial
  abandoned because the client left is not a failure.
- **Hold.** The upstream is `down` for `unreachable_hold` in its
  `[[upstream]]` table, default `"5s"`, range 100 ms–5 min; `"0s"`
  disables the hold ([0004 D3][d4-3]). Every request routed to it during
  the hold gets `503` with
  `Proxy-Status: <name>; error=destination_unavailable;
  next-hop="<upstream>"; details="unreachable for 42s"` without a dial.
  RFC 9209 defines `destination_unavailable` for a next hop that recent
  attempts found down and recommends `503` ([RFC 9209 §2.3][rfc9209-types]).
  Plain-HTTP requests and tunnels are held alike, because both dial through
  the same upstream dialer.
- **Recovery.** When the hold expires, the next request becomes the single
  probe and dials normally; other requests keep failing at once while the
  probe is in flight. A successful connect marks the upstream `up` for all
  requests. A failed probe starts a new hold. A probe abandoned by its
  client leaves the upstream due for another probe on the next request. Any
  successful connect, including one that started before the hold, marks
  the upstream `up`. cuttle sends no background probes.
- **Reload.** An upstream whose address and timeouts are unchanged keeps
  its state; a changed or new upstream starts `up`. `cuttle reload`
  therefore does not clear a hold unless the upstream changed.
- **Logs and counters.** The transition to `down` logs one warning:
  `msg="upstream unreachable" upstream=office address=10.8.0.6:3128
  error=connection_timeout hold=5s`. Recovery logs one `info` line with
  the time spent down and the number of requests failed fast. Each held
  request still gets its normal decision line, with outcome
  `unavailable`. `outcomes_total` and `upstream_outcomes_total` count
  outcome `unavailable`, and `upstream_state` shows `up`, `down`, or
  `probing` with its start time ([0002 D7][d2-7]). `cuttle status` and the
  admin overview list each upstream's state, since when, and the last
  error. Statistics count held requests as failures
  ([0005 D1][d5-1]).

In a chain each hop holds only its own upstream. If the office side cannot
reach the corporate proxy, office answers `503` at once and home relays it
as `502` with `received-status=503` and office's `Proxy-Status` member
(D4), so the whole chain fails fast without home holding office.

Cost: one small state machine per upstream. After the VPN returns, requests
can fail for at most the hold time plus one connect before the probe
succeeds.

## Acceptance criteria

T1 tests with the mocks of [0007](0007-test-environment-and-gates.md)
unless marked T0.

| ID | Criterion | Check |
| --- | --- | --- |
| U1 | The recording upstream receives `CONNECT <authority> HTTP/1.1` with `Host: <authority>`, `Via: 1.1 <name>`, the client's `User-Agent`, and no `Proxy-Connection`. For upstream answers `200`, `201`, `299`, and `200` with `Content-Length: 3`, the client receives exactly `HTTP/1.1 200 Connection established` and completes TLS with the origin. | `TestUpstreamConnect2xx` |
| U2 | Bytes the upstream sends right after its 2xx head reach the client after cuttle's `200`. | `TestUpstreamBufferedBytes` |
| U3 | Upstream `403`, `407`, and `502` reach the client as `502` with `received-status`; `504` as `504`; no `Proxy-Authenticate` is relayed; the test network records zero direct dials. Checked with Go (`OnProxyConnectResponse`) and curl. | `TestUpstreamStatusMapping` |
| U4 | A refused upstream gives `502 connection_refused`; one that accepts and never answers gives `504 http_response_timeout` within `handshake_timeout` + 1 s; neither makes a direct dial. | `TestUpstreamRefusedAndTimeout` |
| U5 | Plain HTTP through an upstream arrives in absolute-form with `Via`; an upstream `407` becomes `502`; a `403` page from the upstream is relayed unchanged. | `TestUpstreamHTTP` |
| U6 | Home → office → intercepting mock: a client trusting the generated corporate root gets `200` and a leaf for the requested name issued by that root; the mock records the decrypted request with `Via: 1.1 cuttle-home, 1.1 cuttle-office`; a client trusting only the origin CA fails verification while both instances log `ok` tunnels. | `TestChainIntercepting` |
| U7 | When the intercepting mock refuses a name, the home client gets `502` and a `Proxy-Status` with the office member before the home member. | `TestChainRefusal` |
| U8 | Home routes `loop.cuttle.test` to office and office routes it back to home: the client gets `502` with `error=proxy_loop_detected`, after exactly 2 upstream `CONNECT` requests in total. The same holds for plain HTTP. | `TestChainLoop` |
| U9 | An upstream equal to a listener address fails validation (T0); an upstream `localhost:<listener port>` fails at dial time with `502 proxy_loop_detected`. | `TestConfigSelfUpstream`, `TestUpstreamSelfDial` |
| U10 | With office's corp `handshake_timeout` at 300 ms and home's office `handshake_timeout` at 2 s, a silent corporate mock yields `504` at the home client with `received-status=504` from office. | `TestChainTimeoutPropagates` |
| U11 | With an injected clock, `connect_timeout` 300 ms, and `unreachable_hold` 1 s on a blackholed upstream: the first request gets `504 connection_timeout` after one dial; four more within the hold get `503 destination_unavailable` without a dial, and `upstream_state` is `down`; while a probe is in flight after the hold, a concurrent request gets `503` and only one dial is recorded; once the address routes to the recording upstream, the probe succeeds, the state returns to `up`, and one recovery line is logged. A silent upstream (handshake timeout) and an upstream answering `502` are never held. A plain-HTTP request to a held upstream also gets `503`. With `unreachable_hold = "0s"`, every request dials. | `TestUpstreamUnreachableHold` |
| N1 | Validation rejects a wildcard or host-name listener, a non-loopback listener without `proxy.remote.sources`, and `proxy.remote.default = "direct"` without `allow_direct`. | T0 `TestConfigExposure` |
| N2 | A connection from a source outside the allowlist is closed with no byte read or written, increments `connections_rejected_total{reason="source"}`, and produces no decision. | `TestRemoteSourceRejected` |
| N3 | A remote client gets the upstream rule's route, `403` with rule `remote-default` for unmatched names, and `403` with rule `remote-direct` for a `direct` rule. With `allow_direct`, it reaches a public test address and gets `502 destination_ip_prohibited` for private, loopback, and `self` addresses. | `TestRemotePolicy` |
| N4 | A local client's direct request for a name resolving to `127.0.0.1`, `169.254.1.1`, `fe80::1`, or `0.0.0.0` gets `502 destination_ip_prohibited` with zero dials. `localhost` and `127.0.0.1` reach a loopback mock through the starter `loopback` rule. A private address is allowed. | `TestDirectAddressPolicy` |
| N5 | After a reload removes a remote source, its kept-alive connection is closed before the next request. | `TestReloadRemovesSource` |
| N6 | Connections over `max_connections` or `max_connections_per_source` are closed after accept and counted with `reason="limit"`. | `TestConnectionLimits` |
| N7 | A non-loopback listener on an address absent from the host (`192.0.2.1`) shows `retrying` in status while the loopback listener serves. | `TestListenerBindRetry` |

The real corporate proxy and VPN are checked in M3 ([OI-0001][oi]).

## Assumptions

| ID | Assumption | Smallest check |
| --- | --- | --- |
| A1 | The corporate proxy accepts `Via` on `CONNECT`, and its own Via text differs from cuttle's names. | One `CONNECT` from the office Mac with and without `Via: 1.1 cuttle-office` ([OI-0001][oi]). |
| A2 | A listener bound to the VPN address keeps accepting after the VPN reconnects with the same address. | Bind office cuttle, reconnect the VPN, and connect from home ([OI-0001][oi]). |
| A3 | With the macOS application firewall on, the first non-loopback bind shows an allow prompt, possibly again after an upgrade changes the binary. | Bind once with the firewall on, then after `brew upgrade` ([OI-0001][oi]). |
| A4 | The corporate proxy's re-signed leaves meet Safari's requirements. | `openssl s_client -proxy` from the office Mac ([OI-0001][oi]). |
| A5 | The launchd hard limit on open files allows at least 4,096 descriptors. | `cuttle status` under `brew services` in M3 ([OI-0003][oi]). |

## Alternatives considered

- **Relaying the upstream status unchanged.** A relayed `403` would look
  like cuttle's own policy, and a relayed `407` would prompt for proxy
  credentials ([research 0002 §6][r2-6]).
- **A private hop-count header.** It duplicates `Via` and is invisible to
  non-cuttle proxies.
- **A wildcard listener with a source allowlist.** It survives address
  changes, but advertises an open port on the corporate LAN.
- **Remote `direct` by default.** Not needed for the chain, and it exposes
  the office Mac's network position.
- **Checking the socket address in `net.Dialer.ControlContext`.** Equivalent
  protection, but it keeps Go's resolver on the dial path, which tests could
  not replace without a fake DNS server.

[d2-3]: 0002-proxy-core-and-routing.md#d3-authority-normalization
[d2-4]: 0002-proxy-core-and-routing.md#d4-rules-and-evaluation-order
[d2-5]: 0002-proxy-core-and-routing.md#d5-actions
[d2-6]: 0002-proxy-core-and-routing.md#d6-responses-generated-by-cuttle
[d2-7]: 0002-proxy-core-and-routing.md#d7-byte-accounting-and-live-counters
[d2-if]: 0002-proxy-core-and-routing.md#interfaces
[d4-3]: 0004-configuration-and-reload.md#d3-schema
[d4-6]: 0004-configuration-and-reload.md#d6-reload
[d5-1]: 0005-statistics-and-admin.md#d1-recorded-data
[d6-6]: 0006-packaging-paths-and-lifecycle.md#d6-signals-and-shutdown
[oi]: ../OPEN_ISSUES.md
[r2]: ../research/0002-proxy-chaining-and-interception.md
[r2-1]: ../research/0002-proxy-chaining-and-interception.md#1-connect-through-chained-http-proxies
[r2-2]: ../research/0002-proxy-chaining-and-interception.md#2-plain-http-forwarding-in-absolute-form
[r2-3]: ../research/0002-proxy-chaining-and-interception.md#3-loop-detection-across-chained-proxies
[r2-4]: ../research/0002-proxy-chaining-and-interception.md#4-tls-intercepting-upstream-and-client-trust
[r2-5]: ../research/0002-proxy-chaining-and-interception.md#5-how-macos-clients-choose-and-talk-to-the-proxy
[r2-6]: ../research/0002-proxy-chaining-and-interception.md#6-what-clients-show-for-proxy-generated-failures
[r2-7]: ../research/0002-proxy-chaining-and-interception.md#7-exposure-risks
[rfc9209]: https://www.rfc-editor.org/rfc/rfc9209.html#section-2
[rfc9209-types]: https://www.rfc-editor.org/rfc/rfc9209.html#section-2.3

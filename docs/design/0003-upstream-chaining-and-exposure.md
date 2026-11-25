# 0003 — Upstream chaining and exposure

**Status** — Proposed (TASK-0001, 2026-11-25; revised in iterations 2
and 3).
Together with [0002](0002-proxy-core-and-routing.md) it supersedes
[0001](0001-https-connect-proxy.md).

## Context

cuttle instances can be chained over a private link. An edge instance, the
one clients point at, hands restricted destinations to a gateway instance
inside another network, which hands them to that network's egress proxy.
The egress proxy accepts plain HTTP proxy connections without
authentication and may be a TLS-inspecting proxy that re-signs TLS with its
inspection root CA. The user requires that a request reaching it disclose
no intermediary. The gateway's listener is reachable over the private link,
so it also needs exposure controls that a loopback-only proxy does not.
Terms are defined in the [glossary](../GLOSSARY.md).

This record defines upstreams and their disclosure modes, `Via` handling
and loop detection between cuttle instances, the non-disclosure rule,
failure reporting, timeouts, the unreachable-upstream hold, listeners,
source and destination checks, and the edge → gateway → egress
configuration. Evidence is in [research 0002][r2]. The non-disclosure
requirement supersedes that research's recommendation to send `Via` to the
egress proxy ([research 0002 §3][r2-3], open question 6).

## Decisions

### D1. Upstreams and disclosure modes

- An upstream is a named plain-HTTP proxy at `host:port`. cuttle speaks
  HTTP/1.1 to it without TLS and without credentials, and never answers or
  relays `407`.
- Every upstream states its disclosure mode in `disclosure`, which is
  required:

| Mode | Use for | Effect on requests sent to it |
| --- | --- | --- |
| `via` | Another cuttle instance | The client's fields pass, and one cuttle `Via` entry is added for loop detection (D2) |
| `none` | An egress proxy, or any proxy that must not see an intermediary | The disclosure filter removes every disclosure field, the client's own included, and nothing that reveals a hop is added (D5) |

- `disclosure` has no default because either wrong default fails: `via`
  toward an egress proxy discloses an intermediary, and `none` toward
  another cuttle hides the chain from loop detection.
- Apart from the disclosure fields, both modes forward the same way, with
  the standard handling of [0002 D5][d2-5]. `direct` routes apply the same
  disclosure filter as `none`.
- cuttle resolves an upstream's own host name through the `Network` seam
  ([0002 Interfaces][d2-if]). The destination checks of D11 do not apply to
  upstream addresses, which are often loopback or private.
- A failed upstream ends the request. cuttle never tries `direct` or another
  upstream. An upstream that could not be reached is held briefly so later
  requests fail at once (D8). Failover lists are a later topic.
- Configuration validation rejects an upstream whose address equals one of
  this instance's listener addresses, comparing IP literals and treating
  `localhost` as loopback. At dial time, an upstream whose resolved address
  and port equal a bound listener fails with `502 proxy_loop_detected`.
  Apache Traffic Server performs the same next-hop check
  ([research 0002 §3][r2-3]).

### D2. Via entries and loop detection

- **Cuttle entry.** A `Via` list member exactly of the form
  `1.1 <name> (cuttle)`, where `<name>` has the instance-name syntax. RFC
  9110 §7.6.3 allows a comment after `received-by`; `(cuttle)` marks the
  member as cuttle's own.
- **Adding (`disclosure = "via"`).** cuttle appends one cuttle entry as a
  new `Via` field value. `Via` members that the client or earlier hops sent
  stay, in their order.
- **Removing (`disclosure = "none"` and every `direct` route).** The
  disclosure filter of D5 removes every `Via` field, cuttle's entries and
  the client's own alike.
- **Loop detection.** On every request, before any filtering, cuttle takes
  the `received-by` token of every `Via` member, the client's and cuttle's
  alike, and compares it with its own name without regard to case. A match
  fails the request with `502 proxy_loop_detected` unless the policy blocks
  it ([0002 D4][d2-4] step 6). An edge → gateway → edge misroute stops in
  one hop; in the research harness a loop stopped after 2 `CONNECT` requests
  with the check and ran to 40 without it ([research 0002 §3][r2-3]).
- A `none` hop adds no entry, so a loop through a `none` hop toward another
  cuttle is invisible. `none` is therefore only for proxies that are not
  cuttle. Backstops are the listener check in D1, the connection limits in
  D12, and the timeouts in D7.

Instance names: the top-level `name` is required, matches
`^[a-z][a-z0-9-]{0,62}$`, and must be unique among chained instances. The
starter configuration generates `cuttle-<6 random hex digits>`; the
deployment in D13 uses `cuttle-edge` and `cuttle-gateway`.

### D3. CONNECT through an upstream

1. Check the hold (D8), then dial the upstream within its
   `connect_timeout`.
2. Write the request head. `net/http` does not offer an outgoing `CONNECT`
   to a proxy with a chosen head, so cuttle writes it itself from the
   client's request:
   - the request line `CONNECT <r.RequestURI> HTTP/1.1`, where
     `r.RequestURI` is the target as the client sent it;
   - `Host`, set to that target;
   - the client's other header fields from `r.Header`, minus the
     hop-by-hop fields that `ReverseProxy` also removes (`Connection` and
     the fields it names, `Proxy-Connection`, `Keep-Alive`,
     `Proxy-Authenticate`, `Proxy-Authorization`, `TE`, `Trailer`,
     `Transfer-Encoding`, `Upgrade`);
   - for `disclosure = "none"`, the D5 disclosure filter applied to those
     fields; for `disclosure = "via"`, cuttle's `Via` entry added (D2).

   Nothing else is written. `http.Header.Write` writes the fields in sorted
   order.
3. Read the response head with `http.ReadResponse` within the upstream's
   `response_timeout`.
4. On any 2xx, ignore `Content-Length` and `Transfer-Encoding`, never read a
   body, and write exactly `HTTP/1.1 200 Connection established\r\n\r\n` to
   the client. Relay bytes already buffered from the upstream after its head
   to the client, and bytes the client pipelined to the upstream, before
   the normal relay ([research 0002 §1][r2-1]).
5. On anything else, answer the client per D6 and close both connections.

Every hop answers its client only after its own upstream answered, so the
original client receives `200` only after the whole chain did. Chrome accepts
only an exact `200`, while curl accepts any 2xx
([research 0002 §6][r2-6]).

### D4. Plain HTTP through an upstream

- `ReverseProxy` forwards the request with the upstream's transport, which
  writes the client's raw absolute-form target ([0002 D5][d2-5],
  [research 0002 §2][r2-2]). `Rewrite` applies the disclosure rule for the
  upstream's mode: the D5 filter for `none`, cuttle's `Via` entry for
  `via`.
- The transport pools connections to the upstream across clients
  ([0002 D5][d2-5]). Its dial function applies the hold (D8).
- Responses pass through standard handling and keep their status. Only a
  `407` becomes `502` with `received-status=407`. Any other non-2xx
  response, such as an egress proxy's block page or an upstream cuttle's own
  `502`, `503`, or `504` with its `Proxy-Status`, reaches the client as the
  upstream sent it; cuttle adds no `Proxy-Status` member to it.
- Absolute-form WebSocket works through an upstream: `ReverseProxy`
  splices the connections after the upstream's `101`.

### D5. Non-disclosure toward an egress proxy

Principle: toward a `disclosure = "none"` upstream, a request discloses no
intermediary. The egress proxy sees what it would see if the client used it
directly, apart from the limits below. The same rule applies on every
`direct` route toward origins.

Disclosure fields are `Via`, `Forwarded`, every field whose name starts
with `X-Forwarded-`, `X-Real-IP`, `Client-IP`, `True-Client-IP`,
`X-Client-IP`, `X-Cluster-Client-IP`, `X-Originating-IP`,
`CF-Connecting-IP`, `Fastly-Client-IP`, and `Proxy-Status`. One function in
`internal/proxy`, the disclosure filter, deletes them all from a header
set. It is called from one place for each path: the plain-HTTP `Rewrite`
and the `CONNECT` head writer (D3), on `direct` and `disclosure = "none"`
routes. It removes the client's own copies as well, because any of these
fields reaching the egress proxy would disclose an intermediary, whoever
added it. On `disclosure = "via"` routes the filter does not run: the
client's fields pass and cuttle adds its own `Via` entry, because the last
hop before the egress proxy filters them anyway.

The list above is the minimum. Each other way a proxy can disclose itself
is decided:

| Possible disclosure | Decision |
| --- | --- |
| Disclosure fields | Removed by the disclosure filter on `direct` and `none` routes; never added by cuttle except its own `Via` entry on `via` routes |
| cuttle-specific headers | None exist |
| cuttle instance names in any value | Appear only in cuttle `Via` entries, which the filter removes, and in responses to clients |
| The original client address | Never added to any field; client-sent address fields are disclosure fields |
| `User-Agent` | Only the client's own. If the client sent none, none is sent: `ReverseProxy` suppresses Go's default, and the `CONNECT` head carries only client fields ([0002 D5][d2-5]) |
| Request target | The client's raw target, `CONNECT` and plain HTTP alike ([0002 D5][d2-5]) |
| `Accept-Encoding` and body encoding | Not added; bodies are not decoded (`DisableCompression`) |
| `Proxy-Authorization` | Never generated; upstream URLs carry no userinfo |
| `Max-Forwards` on `TRACE` and `OPTIONS` | Forwarded unchanged on every route. cuttle never decrements it and never answers such a request itself, even at `Max-Forwards: 0`, because either would reveal a hop. This deviates from RFC 9110 §7.6.2. |
| `TRACE` | Forwarded like any method; its echo shows only what the upstream received |
| Requests of cuttle's own | None: no health checks, probes, or keep-alive requests; the hold (D8) learns only from client requests |
| Error responses with cuttle's name | Sent to the client, never upstream |

Standard handling that changes a request without revealing a hop stays, as
the user accepted: header-name case and order, removal of hop-by-hop fields
including `Proxy-Connection`, `Host` written as the target, and the body
framing Go chooses. Body bytes and the client's content encodings are kept.

Limits:

- The client is who it is. A curl client of the edge instance still looks
  like curl, in its `User-Agent` and in its TLS ClientHello inside the
  tunnel. The TLS inside a tunnel belongs to the client, so Chrome's own
  handshake reaches the egress proxy unchanged.
- The source address is the gateway machine's. The egress proxy attributes
  all traffic from the gateway instance, including every request relayed
  from an edge instance, to the gateway machine.
- Connection reuse, timing, and the TCP/IP stack are cuttle's and the
  gateway machine's. Anything beyond the request contents is out of scope.

### D6. Upstream failures and Proxy-Status

| Upstream event | Client status | Own `Proxy-Status` member |
| --- | --- | --- |
| Upstream host name not resolvable | 502 | `error=dns_error; next-hop="<upstream>"` |
| Refused, reset, or unreachable | 502 | `error=connection_refused; next-hop="<upstream>"` |
| TCP connect timeout | 504 | `error=connection_timeout; next-hop="<upstream>"` |
| Held as unreachable (D8) | 503 | `error=destination_unavailable; next-hop="<upstream>"` |
| No response head within `response_timeout` | 504 | `error=http_response_timeout; next-hop="<upstream>"` |
| Response unparsable | 502 | `error=http_protocol_error; next-hop="<upstream>"` |
| Non-2xx `CONNECT` answer other than 504, `407` included | 502 | `received-status=<code>; next-hop="<upstream>"` |
| `504` answer to `CONNECT` | 504 | `received-status=504; next-hop="<upstream>"` |
| Plain-HTTP `407` | 502 | `received-status=407; next-hop="<upstream>"` |
| Any other plain-HTTP response | Relayed unchanged (D4) | None added |
| Closed after the `200` or mid-response | Close the client connection | — |

- `403` from cuttle always means its own policy
  ([0002 D6][d2-6]). A refusal further along the chain arrives as `502`,
  with the refusing hop's code in `received-status`.
- A `504` answer to `CONNECT` is relayed as `504`, so timeouts stay visible
  through a chain.
- Status mapping and `Proxy-Status` chaining apply to `CONNECT` answers
  and to failures to reach the upstream or read its response, on either
  path. A plain-HTTP response other than `407` is a whole response that
  cuttle relays: the edge instance relays the gateway's `502`, `503`, or
  `504` unchanged, with the gateway's `Proxy-Status` member only.
- When an upstream's non-2xx `CONNECT` answer carries `Proxy-Status`,
  cuttle copies those members, up to 1 KiB of field value, and appends its
  own member last. RFC 9209 lists the member closest to the origin first
  ([RFC 9209 §2][rfc9209]). Example after the egress proxy refuses:

  ```text
  Proxy-Status: cuttle-gateway; received-status=403; next-hop="egress",
                cuttle-edge; received-status=502; next-hop="gateway"
  ```

  This is one field line on the wire, sent toward the client. In the
  research chain tests, the first hop relayed `502` with `received-status`
  in the same way ([research 0002 §1][r2-1]).

### D7. Timeouts

| Timeout | Default | Key | Scope |
| --- | --- | --- | --- |
| Request head | 30 s | fixed | Reading a client request head |
| Keep-alive idle | 120 s | fixed | Between requests on a client connection |
| Direct connect | 10 s | `proxy.connect_timeout` | TCP connect for a direct route, all addresses |
| Direct response | 60 s | `proxy.response_timeout` | Plain-HTTP response head from an origin |
| Upstream connect | 10 s | `connect_timeout` in `[[upstream]]` | TCP connect to the upstream |
| Upstream response | 60 s | `response_timeout` in `[[upstream]]` | Response head for `CONNECT` and plain HTTP |
| Tunnel idle | 1 h | `proxy.idle_timeout` | No bytes in either direction; `0s` disables it |
| Shutdown drain | 5 s | fixed | [0006 D6][d6-6] |

Chain budget: an upstream that is itself a cuttle instance must get a
`response_timeout` longer than that instance's slowest answer, which is its
longest connect timeout plus response timeout over the routes it may use,
plus 10 s. Then a timeout at the gateway reaches the original client as the
gateway's `504` before the edge's own timer fires: for `CONNECT`, the edge
answers `504` with `received-status=504` and both members; for plain HTTP,
the edge relays the gateway's `504` unchanged with the gateway's member
only (D6). Otherwise the edge reports its own `504` while the gateway still
succeeds and holds an orphaned tunnel ([research 0002 §1][r2-1]). With
defaults at the gateway (10 s + 60 s), the edge uses
`response_timeout = "80s"` for its `gateway` upstream. cuttle cannot see
another instance's settings, so the deployment guide states this rule.

### D8. Unreachable upstream hold

When the private link to a gateway instance is down, packets to it are
often dropped rather than refused, so each request would wait the full
`connect_timeout`. A browser opens several connections per page and retries,
so the user would wait repeatedly for the same answer. cuttle therefore
remembers briefly that an upstream cannot be reached and fails at once.
Fail-closed still holds: a held upstream is never replaced by `direct` or
by another upstream.

- **Trigger.** Only a failure to reach the upstream itself: TCP connect
  refused, reset, unreachable, or timed out, or failure to resolve the
  upstream's own host name. A non-2xx answer, a response timeout, and errors
  after the `200` do not trigger it, because the upstream was reachable and
  the fault may lie further along the chain. A dial abandoned because the
  client left is not a failure.
- **Hold.** The upstream is `down` for `unreachable_hold` in its
  `[[upstream]]` table, default `"5s"`, range 100 ms–5 min; `"0s"` disables
  the hold ([0004 D3][d4-3]). Every request routed to it during the hold
  gets `503` with `Proxy-Status: <name>; error=destination_unavailable;
  next-hop="<upstream>"; details="unreachable for 42s"` without a dial.
  RFC 9209 defines `destination_unavailable` for a next hop that recent
  attempts found down and recommends `503`
  ([RFC 9209 §2.3][rfc9209-types]). Tunnels and plain HTTP are held alike.
- **Recovery.** When the hold expires, the next request becomes the single
  probe and dials normally; other requests keep failing at once while the
  probe is in flight. A successful connect marks the upstream `up` for all
  requests; a failed probe starts a new hold. A probe abandoned by its
  client leaves the upstream due for another probe. Any successful connect,
  including one that started before the hold, marks the upstream `up`.
  cuttle sends no background probes (D5).
- **Reload.** An upstream whose settings are unchanged keeps its state; a
  changed or new upstream starts `up`.
- **Logs and counters.** The change to `down` logs one warning:
  `msg="upstream unreachable" upstream=gateway address=10.8.0.6:3128
  error=connection_timeout hold=5s`. Recovery logs one `info` line with the
  time spent down and the number of requests failed fast. Each held request
  still gets its own decision line, with outcome `unavailable`.
  `outcomes_total` and `upstream_outcomes_total` count outcome
  `unavailable`, and `upstream_state` shows `up`, `down`, or `probing` with
  its start time ([0002 D7][d2-7]). `cuttle status` and the admin overview
  list each upstream's state, since when, and the last error. Statistics
  count held requests as failures ([0005 D1][d5-1]).

In a chain each hop holds only its own upstream. If the gateway cannot
reach the egress proxy, the gateway answers `503` at once. For `CONNECT`,
the edge answers `502` with `received-status=503` and both members; for
plain HTTP, the edge relays the gateway's `503` unchanged (D6).

### D9. Listeners

- `proxy.listen` is a list of `IP:port` literals. Host names and the
  wildcard addresses `0.0.0.0` and `::` are configuration errors. A wildcard
  on a gateway machine would expose an unauthenticated proxy on that
  machine's LAN ([research 0002 §7][r2-7]).
- A loopback listener that fails to bind stops startup with exit status 1.
- A non-loopback listener requires a non-empty `proxy.remote.sources`. If it
  fails to bind, for example because the private link is down at login,
  cuttle logs a warning and retries every 30 s while the other listeners
  serve.
  `cuttle status` shows the listener as `retrying` with the error.
- Listener changes take effect at the next start
  ([0004 D6][d4-6]).

### D10. Source classes and remote policy

- On accept, before `net/http` reads a byte, cuttle unmaps the peer address
  and classifies it. Loopback sources are `local`. Sources inside
  `proxy.remote.sources` are `remote`. Anything else is closed at once and
  counted in `connections_rejected_total{reason="source"}`. A warning
  naming the source address is logged at most once per source per minute.
- The handler checks the source again for each request, so after a reload
  removes a source, the next request on a kept-alive connection gets no
  response and the connection is closed.
- Remote clients may use only the ports in `proxy.remote.ports` (default
  `[80, 443]`); RFC 9110 asks for `CONNECT` port limits, and only a remote
  instance is exposed ([research 0002 §7][r2-7]). Other ports get `403`
  with rule `port`.
- Rules apply to remote clients as to local ones. When no rule matches,
  `proxy.remote.default` applies; its default is `block`.
- A remote client whose action is `direct` gets `403` with rule
  `remote-direct`, unless `proxy.remote.allow_direct` is true. Then D11
  limits it to public addresses. `proxy.remote.default = "direct"` is a
  configuration error unless `allow_direct` is true.

An edge instance needs only the gateway's upstream route. A remote `direct`
route would give the edge side the gateway machine's network position,
including its loopback admin UI and other local services
([research 0002 §7][r2-7]).

### D11. Destination address policy for direct routes

1. An IP-literal target yields its address. A name is resolved through
   `Network.LookupNetIP`; a failure answers `502 dns_error`.
2. Unmap IPv4-mapped addresses. For the NAT64 prefix `64:ff9b::/96`, classify
   the embedded IPv4 address.
3. Classify each address, taking the first class that matches in the order
   of the table below, and drop the addresses this client and rule may not
   reach. If none remain, answer `502 destination_ip_prohibited` and log the
   refused classes.
4. Dial only the remaining addresses, as literals, through
   `Network.DialContext`. Attempts follow the resolver's order, starting the
   next address 300 ms after the previous one if it has not connected, as
   in RFC 8305. There is no second lookup, so a rebinding answer cannot
   slip in between the check and the connection
   ([research 0002 §7][r2-7]).

| Class | Addresses | Local client, IP literal | Local client, name | Remote client with `allow_direct` |
| --- | --- | --- | --- | --- |
| `unusable` | `0.0.0.0/8`, `::`, `169.254.0.0/16`, `fe80::/10`, `224.0.0.0/4`, `ff00::/8`, `240.0.0.0/4` | Refused | Refused | Refused |
| `loopback` | `127.0.0.0/8`, `::1` | Through a rule with `allow_loopback` | Through a rule with `allow_loopback` | Refused |
| `self` | This host's interface addresses, refreshed at most every 10 s | Allowed | Through a rule with `allow_loopback` | Refused |
| `private` | `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `100.64.0.0/10`, `fc00::/7` | Allowed | Through a rule with `allow_private` | Refused |
| `public` | Everything else | Allowed | Allowed | Allowed |

How the pieces compose:

- `allow_loopback` and `allow_private` are flags on `direct` rules and
  apply only to the rule that matched. `allow_loopback` covers `loopback`
  addresses and names that resolve to `self`; `allow_private` covers names
  that resolve to `private` addresses. Neither implies the other,
  neither ever admits `unusable`, and both are ignored for remote clients.
- A name with several addresses keeps the allowed ones; for example, a name
  with a public and a private address is dialed on the public one.
- Private ranges are RFC 1918, carrier-grade NAT `100.64.0.0/10`, and IPv6
  unique local `fc00::/7`. Carrier-grade NAT is included because overlay
  networks place private devices there; Tailscale, for example, assigns
  addresses in `100.64.0.0/10`.

Why names and literals differ:

- A browser applies its local-network protections to addresses it
  resolves. Through a proxy it never sees the address, so a page whose name
  resolves or rebinds to a LAN device or to this Mac would reach it through
  cuttle. Names that resolve to `loopback`, `self`, or `private` addresses
  therefore need a rule that names them, as research 0002 §7 mitigation 4
  recommends ([research 0002 §7][r2-7]).
- A private IP literal is a destination the user typed, such as
  `http://192.168.0.1/`, and gives the client the reach it has without a
  proxy. The user decided that such literals are not affected. This Mac's
  own address typed as a literal is treated the same way.
- Loopback literals still need `allow_loopback`, because `127.0.0.1` and
  `::1` reach services that listen only on loopback, such as the admin UI,
  and a page can name them without any DNS. The starter `loopback` rule
  provides it
  for `localhost`, `127.0.0.0/8`, and `::1` on any port, so terminal clients
  that proxy `localhost`, which curl does and Chrome never does
  ([research 0002 §5][r2-5]), keep working.

Example:

```toml
[[rule]]
name = "home-lan"            # NAS and printers by name
domains = ["lan", "local", "home.arpa"]
action = "direct"
allow_private = true
```

- The admin port listens on loopback, so it is reachable through cuttle
  only by a local client through an `allow_loopback` rule. That equals
  local access and still meets the password and Host checks of
  [0005](0005-statistics-and-admin.md). Remote clients cannot reach
  `loopback`, `self`, or `private` addresses at all.
- Upstream routes do no local resolution and no address check; the next
  proxy resolves.

### D12. Connection limits

- `proxy.max_connections` (default 1024) caps client connections.
  `proxy.remote.max_connections_per_source` (default 128) caps each remote
  source. A connection over a cap is closed right after accept and counted
  in `connections_rejected_total{reason="limit"}`.
- Each tunnel uses two descriptors, and pooled outbound connections add
  more. Go raises the soft `RLIMIT_NOFILE` to the hard limit at start;
  `cuttle status` reports the effective limit. Assumption A5 covers
  launchd's limits.

### D13. Edge, gateway, and egress deployment

An edge instance sends restricted destinations over a private link to a
gateway instance, which sends them through its network's egress proxy. A
private link is any network path between the two sides, such as a VPN,
WireGuard, Tailscale, or an SSH tunnel. In the cases below, one end of the
link is a server or router on the edge machine's LAN, and the other end is
the gateway machine.

```text
client (browser or curl) on the edge machine
  -> edge instance 127.0.0.1:3128 (name cuttle-edge)
     restricted domains -> upstream "gateway" 10.8.0.6:3128,
                           disclosure = "via"
  -> private link (edge-side server or router <-> gateway machine)
  -> gateway instance 10.8.0.6:3128 (name cuttle-gateway)
     restricted domains -> upstream "egress" proxy.internal.example:8080,
                           disclosure = "none"
  -> egress proxy (plain HTTP; may re-sign TLS with an inspection root CA)
  -> origin
```

The addresses are placeholders: `10.8.0.6` is the gateway machine's
private-link address. The egress proxy receives requests that disclose no
intermediary (D5), and it attributes all of this traffic to the gateway
machine. Operators must make sure that the network's policy allows
relaying requests from other machines through the gateway machine.

Which source address the gateway instance sees depends on the edge-side
server or router:

| Case | Source seen by the gateway instance | `proxy.remote.sources` | Consequence |
| --- | --- | --- | --- |
| Routed: the server forwards edge LAN traffic into the link unchanged | The edge machine's own LAN address, such as `192.168.1.20` | That address as `/32` | Only that machine is admitted. Keep its address fixed, for example with a DHCP reservation. Inference: the gateway side of the link must route the edge LAN back into the link, or replies will not return; with WireGuard, for example, the gateway machine's peer entry lists the edge LAN in `AllowedIPs`. |
| NAT: the server masquerades edge LAN traffic | The server's link address, such as `10.8.0.1`, for every device on the edge LAN | That address as `/32` | Every device on the edge LAN can use the gateway instance's remote routes, which reach only the egress proxy for restricted destinations, attributed to the gateway machine. Restrict this at the server's firewall or switch to routing. |

The observation that tells them apart: start the gateway instance with a
placeholder `sources = ["192.0.2.1/32"]`, send one request from the edge
machine through the edge instance, for example
`curl -x http://127.0.0.1:3128 https://wiki.internal.example/`, and read
the gateway log's rejected-source warning. A `source=` with the edge
machine's LAN address means routed; the server's link address means NAT. A
second device on the edge LAN shows the same address only under NAT. Then
set `sources` and run `cuttle reload`.

Edge `~/.config/cuttle/config.toml`:

```toml
name = "cuttle-edge"

[proxy]
listen = ["127.0.0.1:3128"]
default = "direct"

[[upstream]]
name = "gateway"
address = "10.8.0.6:3128"
disclosure = "via"        # cuttle to cuttle: Via for loop detection
response_timeout = "80s"  # gateway's egress connect + response + 10 s

[[rule]]
name = "loopback"
hosts = ["localhost"]
cidrs = ["127.0.0.0/8", "::1/128"]
action = "direct"
allow_loopback = true

[[rule]]
name = "restricted"
domains = ["internal.example", "intranet.example.net"]
action = "upstream:gateway"
```

Gateway `~/.config/cuttle/config.toml`:

```toml
name = "cuttle-gateway"

[proxy]
listen = ["127.0.0.1:3128", "10.8.0.6:3128"]
default = "upstream:egress"  # local gateway clients use the egress proxy

[proxy.remote]
sources = ["192.168.1.20/32"]  # routed case; for NAT, the server, e.g. 10.8.0.1/32
default = "block"

[[upstream]]
name = "egress"
address = "proxy.internal.example:8080"
disclosure = "none"  # requests reach the egress proxy with no intermediary

[[rule]]
name = "loopback"
hosts = ["localhost"]
cidrs = ["127.0.0.0/8", "::1/128"]
action = "direct"
allow_loopback = true

[[rule]]
name = "restricted"  # the only route open to the edge instance
domains = ["internal.example", "intranet.example.net"]
action = "upstream:egress"
```

Trust. cuttle needs no certificate or CA ([research 0002 §4][r2-4]). When
the egress proxy is TLS-inspecting, clients of the edge instance must trust
its inspection root CA for restricted destinations:

- Chrome and Safari on macOS: add the root to the login keychain and set
  "Secure Sockets Layer (SSL)" to "Always Trust". Chrome's verifier uses
  keychain trust for locally added roots. This has not been observed
  through the chain ([OI-0005][oi]).
- `/usr/bin/curl`: pass `--cacert inspection-root.pem` or set
  `CURL_CA_BUNDLE`. Whether its default path honors keychain trust is
  unknown ([OI-0005][oi]).
- Safari also requires the re-signed leaves to have a SAN, the serverAuth
  EKU, and at most 825 days of validity ([OI-0001][oi]).

The operator accepts these consequences:

- The trusted inspection root CA can vouch for any name on the client's
  machine, not only for routed sites.
- The egress proxy's operator sees decrypted requests, cookies, and
  credentials for routed sites, and attributes them to the gateway machine.
- Client-certificate authentication and apps that pin keys fail on that
  path, as they do inside that network.
- Chrome may still look up restricted names with the edge machine's
  resolver ([research 0002 §5][r2-5]).

#### Example: reaching an office network from home

The maintainer's own setup is one instance of this deployment:

| General term | In this example |
| --- | --- |
| Edge instance | cuttle on a Mac at home |
| Gateway instance | cuttle on a Mac at the office |
| Private link | A WireGuard VPN between a home server or router and the office Mac |
| Egress proxy | The corporate proxy: plain HTTP without authentication, TLS-inspecting |
| Inspection root CA | The corporate root, which clients at home must trust |
| Restricted destinations | Office-only sites, such as the intranet |
| Original client | Chrome, Safari, or curl on the home Mac |

The configurations above apply with this setup's names and addresses.
Whether the home server routes or masquerades home LAN traffic, and
assumptions A1–A4 about the corporate proxy and the VPN, are checked on
this setup in [OI-0001][oi].

## Acceptance criteria

T1 tests with the mocks of [0007](0007-test-environment-and-gates.md)
unless marked T0.

| ID | Criterion | Check |
| --- | --- | --- |
| U1 | Through a `via` upstream, the recording upstream receives `CONNECT <target as sent> HTTP/1.1` with `Host` equal to the target, the client's `User-Agent`, no hop-by-hop field, and exactly one cuttle `Via` entry. For upstream answers `200`, `201`, `299`, and `200` with `Content-Length: 3`, the client receives exactly `HTTP/1.1 200 Connection established` and completes TLS with the origin. | `TestUpstreamConnectVia` |
| U2 | Bytes the upstream sends right after its 2xx head reach the client after cuttle's `200`. | `TestUpstreamBufferedBytes` |
| U3 | Upstream `403`, `407`, and `502` reach the client as `502` with `received-status`; `504` as `504`; no `Proxy-Authenticate` is relayed; the test network records zero direct dials. Checked with Go (`OnProxyConnectResponse`) and curl. | `TestUpstreamStatusMapping` |
| U4 | A refused upstream gives `502 connection_refused`; one that accepts and never answers gives `504 http_response_timeout` within `response_timeout` + 1 s; neither makes a direct dial. | `TestUpstreamRefusedAndTimeout` |
| U5 | Plain HTTP through a `via` upstream arrives with the client's raw absolute-form target, including `http://h/a{b}|c?x=1;y=2&z=%zz`, one cuttle `Via` entry, and the client's body bytes; an upstream `407` becomes `502`; a `403` page is relayed unchanged. An absolute-form WebSocket upgrade through the upstream completes an echo round trip. | `TestUpstreamHTTP` |
| U6 | Edge (`via`) → gateway (`none`) → inspecting egress mock: the mock's recorded `CONNECT` head has no disclosure field (D5). A client trusting the generated inspection root CA gets `200` and a leaf for the requested name issued by that root, and the mock's decrypted request has the expected `Host` and path. A client trusting only the origin CA fails verification while both instances log `ok` tunnels. | `TestChainInspecting` |
| U7 | When the inspecting egress mock refuses a name, the original client gets `502` and a `Proxy-Status` with the gateway member before the edge member. | `TestChainRefusal` |
| U8 | The edge routes `loop.cuttle.test` to the gateway and the gateway routes it back to the edge, both with `disclosure = "via"`: the client gets `502` with `error=proxy_loop_detected`, after exactly 2 upstream `CONNECT` requests in total. The same holds for plain HTTP. | `TestChainLoop` |
| U9 | An upstream equal to a listener address fails validation (T0). In-process tests pass their own listeners, and an upstream name that the test network resolves to the listener's address fails at dial time with `502 proxy_loop_detected`; validation cannot see that case. | `TestConfigSelfUpstream`, `TestUpstreamSelfDial` |
| U10 | With the gateway's `egress` `response_timeout` at 300 ms and the edge's `gateway` `response_timeout` at 2 s, a silent egress mock yields at the original client, for `CONNECT`, `504` with `received-status=504` and the gateway member before the edge member, and for plain HTTP, the gateway's `504` unchanged with the gateway's member only. | `TestChainTimeoutPropagates` |
| U11 | With an injected clock, `connect_timeout` 300 ms, and `unreachable_hold` 1 s on a blackholed upstream: the first request gets `504 connection_timeout` after one dial; four more within the hold get `503 destination_unavailable` without a dial, and `upstream_state` is `down`; while a probe is in flight after the hold, a concurrent request gets `503` and only one dial is recorded; once the address routes to the recording upstream, the probe succeeds, the state returns to `up`, and one recovery line is logged. A silent upstream and an upstream answering `502` are never held. A plain-HTTP request to a held upstream also gets `503`. With `unreachable_hold = "0s"`, every request dials. | `TestUpstreamUnreachableHold` |
| U12 | Non-disclosure: an original client with a distinctive source address sends a `CONNECT` and plain-HTTP requests (a `POST` with a chunked body, and `GET` targets `http://h/a{b}|c?x=1;y=2&z=%zz` and `http://h/q?b=2&a=1;c=3`) through edge (`via`) → gateway (`none`) → recording mock. The client also sends `Via: 1.1 lab-gw` and `X-Real-IP`, and one request has no `User-Agent`. The mock receives no field or value that cuttle added: every field it receives, apart from `Content-Length`, `Transfer-Encoding`, and `Trailer`, was sent by the client with the same value; no disclosure field arrives, the client's own included; no value contains `cuttle-edge`, `cuttle-gateway`, or the original client address; and method, target, and body bytes equal the client's. `OPTIONS` and `TRACE` with `Max-Forwards: 0` arrive with the value unchanged and are answered by the mock. | `TestNoDisclosureChain` |
| N1 | Validation rejects a wildcard or host-name listener, a non-loopback listener without `proxy.remote.sources`, `proxy.remote.default = "direct"` without `allow_direct`, and an upstream without `disclosure`; `cuttle check` warns about `0.0.0.0/0` or `::/0` in `sources`. | T0 `TestConfigExposure` |
| N2 | A connection from a source outside the allowlist is closed with no byte read or written, increments `connections_rejected_total{reason="source"}`, logs the source address, and produces no decision. | `TestRemoteSourceRejected` |
| N3 | A remote client gets the upstream rule's route, `403` with rule `remote-default` for unmatched names, `403` with rule `remote-direct` for a `direct` rule, and `403` with rule `port` for port 22. With `allow_direct`, it reaches a public test address and gets `502 destination_ip_prohibited` for private, loopback, and `self` addresses even through rules with `allow_private` or `allow_loopback`. | `TestRemotePolicy` |
| N4 | A local client's direct request for a name resolving to `127.0.0.1`, `192.0.2.50` (`self`), `169.254.1.1`, `fe80::1`, or `0.0.0.0` gets `502 destination_ip_prohibited` with zero dials. A name resolving to `10.20.0.5` is refused without `allow_private` and dialed with it; the literals `10.20.0.5` and `192.0.2.50` are dialed without any rule flag. `localhost:3000` and `127.0.0.1:8443` reach loopback mocks through the starter `loopback` rule. | `TestDirectAddressPolicy` |
| N5 | After a reload removes a remote source, its kept-alive connection is closed at the next request. | `TestReloadRemovesSource` |
| N6 | Connections over `max_connections` or `max_connections_per_source` are closed after accept and counted with `reason="limit"`. | `TestConnectionLimits` |
| N7 | A non-loopback listener on an address absent from the host (`192.0.2.1`) shows `retrying` in `cuttle status` while the loopback listener serves. | `TestListenerBindRetry` |

A real egress proxy and private link are checked in M3 on the example
deployment of D13 ([OI-0001][oi]).

## Assumptions

| ID | Assumption | Smallest check |
| --- | --- | --- |
| A1 | The egress proxy accepts requests that disclose no intermediary, with standard header handling, as it accepts direct requests from Chrome on the gateway machine. This replaces the earlier assumption that it accepts `Via`. | From the edge machine through the chain, load restricted destinations that Chrome on the gateway machine loads directly ([OI-0001][oi]). |
| A2 | A listener bound to the private-link address keeps accepting after the link reconnects with the same address. | Bind the gateway instance, restart the link, and connect from the edge machine ([OI-0001][oi]). |
| A3 | With the macOS application firewall on, the first non-loopback bind shows an allow prompt, possibly again after an upgrade changes the binary. | Bind once with the firewall on, then after `brew upgrade` ([OI-0001][oi]). |
| A4 | A TLS-inspecting egress proxy's re-signed leaves meet Safari's requirements. | `openssl s_client -proxy` from the gateway machine ([OI-0001][oi]). |
| A5 | The launchd hard limit on open files allows at least 4,096 descriptors. | `cuttle status` under `brew services` in M3 ([OI-0003][oi]). |

## Alternatives considered

- **`Via` toward the egress proxy with an opt-out.** Research 0002
  recommended it; the user's non-disclosure requirement makes
  `disclosure = "none"` the documented setting instead.
- **Forwarding request heads byte for byte.** Considered in this iteration
  and withdrawn: the user confirmed that header case, order, and standard
  hop-by-hop handling are fine, and it needed a custom front end.
- **Marking cuttle hops with a private field instead of `Via`.** It would
  still be an added field between cuttle instances, and loop detection
  would no longer follow RFC 9110.
- **Decrementing `Max-Forwards`.** RFC 9110 requires it of proxies, but a
  client can then discover the hop by sending `Max-Forwards: 0`.
- **Relaying the upstream status unchanged.** A relayed `403` would look
  like cuttle's own policy, and a relayed `407` would prompt for proxy
  credentials ([research 0002 §6][r2-6]).
- **A wildcard listener with a source allowlist.** It survives address
  changes, but advertises an open port on the gateway machine's LAN.
- **Remote `direct` by default.** Not needed for the chain, and it exposes
  the gateway machine's network position.
- **Checking the socket address in `net.Dialer.ControlContext`.** Equivalent
  protection, but it keeps Go's resolver on the dial path, which tests could
  not replace without a fake DNS server.

[d2-4]: 0002-proxy-core-and-routing.md#d4-rules-and-evaluation-order
[d2-5]: 0002-proxy-core-and-routing.md#d5-actions-and-forwarding
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

# 0002 — Proxy chaining, intercepting upstreams, and admin exposure

> **Terminology.** This note keeps its original wording, such as corporate
> proxy, home and office cuttle, and `trace`. The
> [glossary mapping table](../GLOSSARY.md#mapping-from-earlier-wording)
> gives the current terms.

**Task** — TASK-0001. **Milestone** — M1, with deployment input for M2.

This note covers how cuttle should relay CONNECT and absolute-form requests
through a chain of HTTP proxies that ends at a TLS-intercepting corporate
proxy, and how macOS clients behave around it. It draws on the standards,
Go 1.27.4 source, Chromium source, vendor documentation, and controlled
127.0.0.1 experiments. A forwarding proxy sends a fresh `CONNECT host:port`
with `Host` set to that authority and waits for the next hop's 2xx. It then
answers its client with exactly `200`, because Chrome accepts nothing else.
Browsers see every tunnel failure as the same generic error, so cuttle's
logs and admin UI must explain a policy block; curl and `Proxy-Status` see
the status code. Plain `http://` stays in absolute form toward an upstream
and becomes origin-form when direct. Hop-by-hop fields are removed,
including the `Proxy-Connection` that every tested client sends. Clients
reuse one proxy connection for several destinations, so routing is decided
per request. A unique `Via` pseudonym on every forwarded message, CONNECT
included, plus a check for that pseudonym stops a home ↔ office loop in one
hop; Squid and ATS use the same signal. The tunnelling hops need no
certificates. The home client must trust the corporate root, and the
corporation sees the decrypted traffic. Clients send hostnames, not
resolved addresses, but differ on IDNs, case, trailing dots, and IPv6
literals. CFNetwork omits IPv6 brackets, and Go's server rejects that form.
The VPN-reachable office listener needs an explicit bind address, a source
allowlist, port limits, and checks on the resolved address. Otherwise its
direct route exposes the office's loopback admin UI. That UI also needs
Host validation against DNS rebinding, on top of cross-origin protection.
Safari itself, 407 handling in headed browsers, keychain-trusted roots, and
the real corporate proxy were not tested.

## 1. CONNECT through chained HTTP proxies

**Controlled setup used in every section.** Observations used endpoints on
127.0.0.1 started from the scratch directory `<scratch>/research-0002/`,
which is outside Git and may be cleaned up. A Go harness (`harness-bin`)
provided:

- `origin`: an HTTPS echo origin on :18443 and an HTTP echo origin on :18080.
  Its leaf certificate for `*.cuttle.test` came from a throwaway test CA.
- `recorder`: a raw-socket proxy on :18100 that logs each request head byte
  for byte. It answers by the first DNS label of a `*.cuttle.test`
  destination. `s403`, `s502`, `s504`, and `s407` get that status with no
  outbound connection. Other test names are tunnelled to the origin or echoed
  in absolute form. Names outside `cuttle.test` get 403.
- `mini`: a chained proxy built on Go `net/http`. Two instances ran as `home`
  (:18101) and `office` (:18102), and three more ran self-looping variants.
- mitmproxy 12.3.1 (`mitmdump`, :18200) played the TLS-intercepting corporate
  proxy. An addon mapped every upstream connection to the local origins and
  refused everything else.

Clients were macOS 27.2.1 (26B131, arm64) with:

- headless Chrome 157.0.8103.50, a throwaway profile,
  `--proxy-server=http://127.0.0.1:18100`, and a net-log;
- `/usr/bin/curl` 8.7.1 (LibreSSL 3.3.6, SecureTransport also built in);
- a Swift `URLSession` probe with a per-session proxy dictionary
  (CFNetwork 3896.200.61);
- Go 1.27.4 `net/http`.

Observations were made on 2026-11-25. No system proxy setting, keychain,
or trust setting was changed. Safari itself was not run.

**Semantics.** Any 2xx response to CONNECT means that the sender and all
inbound proxies switch to tunnel mode right after the response header
section. Any other response means that the tunnel has not been formed
([RFC 9110 §9.3.6][9110-connect]). A server MUST NOT send `Content-Length`
or `Transfer-Encoding` in a 2xx response to CONNECT, and a client MUST ignore
them ([RFC 9110 §9.3.6][9110-connect]; [RFC 9112 §6.3][9112-length], rule 2).
A CONNECT request has no content. When either side of a tunnel closes, the
intermediary MUST try to deliver outstanding data from the closed side, then
close both connections ([RFC 9110 §9.3.6][9110-connect]).

**Request target and Host.** A client MUST send only the host and port of the
tunnel destination in authority-form, `authority-form = uri-host ":" port`.
The specification's example is `CONNECT www.example.com:80 HTTP/1.1` with
`Host: www.example.com` ([RFC 9112 §3.2.3][9112-authority]). Every HTTP/1.1
request needs exactly one valid `Host`, and a server answers 400 otherwise
([RFC 9112 §3.2][9112-target]).

Observed CONNECT heads:

| Sender | Request line | Other fields |
| --- | --- | --- |
| Chrome 157 | `CONNECT origin.cuttle.test:18443 HTTP/1.1` | `Host` with port, `Proxy-Connection: keep-alive`, `User-Agent` |
| curl 8.7.1 | `CONNECT origin.cuttle.test:18443 HTTP/1.1` | `Host` with port, `User-Agent`, `Proxy-Connection: Keep-Alive` |
| CFNetwork 3896.200.61 | `CONNECT s403.cuttle.test:443 HTTP/1.1` | `Host: s403.cuttle.test` without port 443, `Proxy-Connection` and `Connection: keep-alive`, no `User-Agent` |
| Go `Transport` 1.27.4 | `CONNECT origin.cuttle.test:18443 HTTP/1.1` | `Host` equal to the authority, `User-Agent: Go-http-client/1.1` |

CFNetwork includes a non-default port in `Host` and drops 443.

Inference: cuttle should take the destination from the request target and
ignore `Host` for CONNECT. Go already does this. A CONNECT target is parsed
as a bare authority ([request.go L1128][go-req-connect]). The usual
missing-`Host` 400 skips CONNECT ([server.go L1067][go-srv-host]). Observed
in check G3, a CONNECT without `Host` reached the handler with `r.Host` set
from the target.

The upstream CONNECT should be a fresh `CONNECT <authority> HTTP/1.1` with
`Host: <authority>`, using the authority that the policy matched. Client
fields such as `Proxy-Connection` are hop-by-hop and are not copied. `Via` is
discussed in section 3. The harness `mini` sent exactly this. mitmproxy
accepted it and tunnelled (chain test X2 returned 200).

**What counts as success, and Go pitfalls.**

- The RFC accepts any 2xx. Go's `Transport` accepts only 200 when it dials
  through a proxy and returns the status text as the error
  ([transport.go L2036][go-tr-200]). Check G1 returned
  `Get "https://s403.cuttle.test/g1": Forbidden`.
  `Transport.OnProxyConnectResponse` runs before that check and can capture
  the code ([transport.go L132][go-tr-onproxy]); G1 observed it for 403, 502,
  and 407.
- Chrome also accepts only an exact `200`, and rejects a 200 that has extra
  bytes after the head (section 6). curl accepts any 2xx. So cuttle must
  always send exactly `200` downstream, even when its upstream answered
  another 2xx.
- Inference: chaining is simplest with a hand-written handshake. Dial the
  upstream, write the CONNECT, read with `http.ReadResponse`, and accept any
  2xx. The harness `mini` did this.
- `http.ReadResponse` does not apply the CONNECT framing rule (check G6):
  - For `200 Connection established` followed by tunnel bytes, it returned
    `ContentLength=-1` and a non-`NoBody` body. The 12 trailing bytes stayed
    in the `bufio.Reader`.
  - For a 200 carrying `Content-Length: 3`, it reported a 3-byte body, which
    RFC 9110 says to ignore.
  - So the proxy must never read `resp.Body` of a 2xx. It must forward
    `br.Buffered()` bytes as tunnel data first.
- On the client side, hijack after deciding the route. `Hijacker` works only
  on HTTP/1.x connections ([server.go L184][go-srv-hijack]).
  `http.NewResponseController(w).Hijack()` behaves the same. Bytes that the
  client pipelined after the CONNECT head stay in the returned reader
  (G5: 21 bytes) and must be relayed first.
- `ServeMux` answers 404 to CONNECT because the request has an empty path
  ([server.go L2740][go-srv-mux]; G4 returned 404 with a `"/"` pattern). The
  proxy handler must be the server's root `Handler`, or the server must
  branch on the method before any mux.
- `httputil.ReverseProxy` refuses CONNECT with 405
  ([reverseproxy.go L521][go-rp-connect]; G8).

**Relaying upstream failures.**

- When the upstream answers non-2xx, cuttle answers the client with its own
  status and closes. RFC 9209 `Proxy-Status` can say which hop failed. It is
  a Structured Fields list with one member per intermediary
  ([RFC 9209 §2][9209]). Useful parameters are `received-status`, which
  carries the upstream code (§2.1.4), and `next-hop` (§2.1.2).
- Chain test X4: the interceptor refused, `office` relayed 502, and `home`
  relayed 502 with `Proxy-Status: home; received-status=502`. curl reported
  `CONNECT tunnel failed, response 502`.
- RFC 9209 error types with their recommended status
  ([RFC 9209 §2.3][9209-types]):

  | Situation | Error type | Status |
  | --- | --- | --- |
  | Policy block | `http_request_denied` (§2.3.17) | 403 |
  | Forbidden destination address | `destination_ip_prohibited` (§2.3.5) | 502 |
  | Upstream refused the connection | `connection_refused` (§2.3.7) | 502 |
  | Upstream timed out | `connection_timeout` (§2.3.9) | 504 |
  | Loop detected | `proxy_loop_detected` (§2.3.32) | 502 |
  | Bad configuration | `proxy_configuration_error` (§2.3.31) | 500 |
  | Name resolution failed | `dns_error` (§2.3.2) | — |

  `destination_not_found` is meant for gateways, not forward proxies.
- Do not relay an upstream 407. `Proxy-Authenticate` "applies only to the
  next outbound client on the response chain"
  ([RFC 9110 §11.7.1][9110-proxy-auth]). Relaying it makes the client treat
  cuttle as asking for credentials. CFNetwork raised a proxy-authentication
  challenge to the application (section 6). This research also triggered a
  real authentication dialog on the user's screen. Map the 407 to 502 with
  `Proxy-Status`; that mapping is an inference.
- After cuttle sends 2xx, it can report a failure only by closing the
  connection, because both sides are in tunnel mode. The design rule "report
  success after the selected path is established" therefore holds per hop.
  `home` must wait for `office`'s 2xx, which itself waits for the corporate
  proxy's 2xx.
- Inference: each hop's timeout for the upstream CONNECT should exceed the
  next hop's. Otherwise `home` reports 504 while `office` still succeeds and
  holds an orphaned upstream tunnel.

[9110-connect]: https://www.rfc-editor.org/rfc/rfc9110.html#section-9.3.6
[9112-length]: https://www.rfc-editor.org/rfc/rfc9112.html#section-6.3
[9112-authority]: https://www.rfc-editor.org/rfc/rfc9112.html#section-3.2.3
[9112-target]: https://www.rfc-editor.org/rfc/rfc9112.html#section-3.2
[9110-proxy-auth]: https://www.rfc-editor.org/rfc/rfc9110.html#section-11.7.1
[9209]: https://www.rfc-editor.org/rfc/rfc9209.html#section-2
[9209-types]: https://www.rfc-editor.org/rfc/rfc9209.html#section-2.3
[go-req-connect]: https://github.com/golang/go/blob/go1.27.4/src/net/http/request.go#L1128
[go-srv-host]: https://github.com/golang/go/blob/go1.27.4/src/net/http/server.go#L1067
[go-srv-hijack]: https://github.com/golang/go/blob/go1.27.4/src/net/http/server.go#L184
[go-srv-mux]: https://github.com/golang/go/blob/go1.27.4/src/net/http/server.go#L2740
[go-tr-200]: https://github.com/golang/go/blob/go1.27.4/src/net/http/transport.go#L2036
[go-tr-onproxy]: https://github.com/golang/go/blob/go1.27.4/src/net/http/transport.go#L132
[go-rp-connect]: https://github.com/golang/go/blob/go1.27.4/src/net/http/httputil/reverseproxy.go#L521

## 2. Plain HTTP forwarding in absolute form

**Direct route.** A client MUST send absolute-form to a proxy, except for
CONNECT and server-wide OPTIONS. A proxy that receives absolute-form MUST
ignore the received `Host`, replace it with the host from the request target,
and generate a new `Host` when it forwards ([RFC 9112 §3.2.2][9112-absolute]).
Going direct, cuttle rewrites the request to origin-form: path and query, plus
`Host` ([RFC 9112 §3.2.1][9112-origin]).

Go already applies the `Host` rule (check G3). For
`GET http://Plain.Cuttle.TEST:80/a?b=1` with `Host: different.example`, the
handler saw `r.Host="Plain.Cuttle.TEST:80"` and `r.URL.Host` equal to it, so
the stray `Host` was ignored. Case and an explicit default port survive, so
rule matching needs the normalization in section 5. An absolute-form request
without `Host` got a 400 from Go before the handler ran (G3). That follows
RFC 9112 §3.2 but differs from CONNECT.

**Upstream route.** cuttle keeps absolute-form, because the next proxy is
again a proxy. Go's `Transport` writes absolute-form whenever a proxy is in
use ([request.go L657][go-req-absolute]). Check G2 and chain test X3 recorded
`GET http://plain.cuttle.test/x3 HTTP/1.1` at each hop. The origin received
`Via: 1.1 home, 1.1 office`, and mitmproxy, acting as an ordinary proxy for
`http://`, forwarded it unchanged.

**Hop-by-hop fields.**

- Intermediaries MUST remove every field named in `Connection`. They SHOULD
  remove or replace `Proxy-Connection`, `Keep-Alive`, `TE`,
  `Transfer-Encoding`, and `Upgrade` even when `Connection` does not list them
  ([RFC 9110 §7.6.1][9110-connection]).
- All three observed clients send the non-standard `Proxy-Connection` on
  absolute-form requests: Chrome (`keep-alive`), curl (`Keep-Alive`), and
  CFNetwork (`keep-alive`, plus `Connection: keep-alive`).
- `ReverseProxy` removes `Connection`, `Proxy-Connection`, `Keep-Alive`,
  `Proxy-Authenticate`, `Proxy-Authorization`, `Te`, `Trailer`,
  `Transfer-Encoding`, and `Upgrade`, plus anything `Connection` lists
  ([reverseproxy.go L364][go-rp-hop]). It then restores `Te: trailers` when
  the client sent it ([L476][go-rp-te]). Check G7 confirmed that a field
  named in `Connection` (`X-Secret`), `Keep-Alive`, and `Proxy-Connection`
  were removed and `Te: trailers` was kept.

**Using `httputil.ReverseProxy` as the forward path.** This works with a
`Rewrite` function that copies the absolute URL and sets `Out.Host` from it.
Pitfalls observed in G7:

- With `Rewrite`, `ReverseProxy` strips inbound `Forwarded` and
  `X-Forwarded-*` fields before calling the function
  ([reverseproxy.go L121][go-rp-rewrite]). The origin received no
  `X-Forwarded-For`.
- With the deprecated `Director`, `X-Forwarded-For` is preserved and the
  client IP is appended ([L186][go-rp-director], [L242][go-rp-xff]). The
  origin saw `X-Forwarded-For: 203.0.113.9, 127.0.0.1`. A forward proxy that
  should not disclose the client address must use `Rewrite` and must not call
  `SetXForwarded`.
- A default `Transport` adds `Accept-Encoding: gzip` when the client sent
  none, and transparently decodes the response. The origin saw
  `Accept-Encoding: gzip` with `Director` and none with `Rewrite` plus
  `DisableCompression: true`. Inference: byte accounting and
  content-preserving relay need `DisableCompression: true`.

**Upgrade and WebSocket over `http://`.**

- `Upgrade` is hop-by-hop. Its sender MUST also send `Connection: upgrade`
  ([RFC 9110 §7.8][9110-upgrade]).
- Chrome 157 does not send an absolute-form `Upgrade` request for `ws://`.
  It tunnels it with CONNECT to port 80. A page at
  `http://plain.cuttle.test/ws.html` opening `ws://wsdemo.cuttle.test/chat`
  produced `CONNECT wsdemo.cuttle.test:80 HTTP/1.1` at the recorder, and
  Chrome reported `ERR_TUNNEL_CONNECTION_FAILED` for the 403.
- So a CONNECT port policy that allows only 443 blocks plain WebSocket
  through cuttle. curl `-p`/`--proxytunnel` also sends CONNECT to port 80 for
  `http://` (C5b: `CONNECT plain.cuttle.test:80`).
- Other clients may send an absolute-form `GET` with `Upgrade: websocket`.
  `ReverseProxy` re-adds `Upgrade` for such requests ([L482][go-rp-upgrade])
  and splices a 101 response through `handleUpgradeResponse`
  ([L822][go-rp-upgrade-resp]).
- Inference: after a 101 the stream bypasses per-request accounting, so
  cuttle must count those bytes explicitly.

**Connection reuse.**

- Clients reuse one keep-alive connection to the proxy for requests to
  *different* destinations. curl sent `http://plain.cuttle.test/c2a` and
  `http://other.cuttle.test/c2b` on one connection (`num_connects` 1 then 0).
  Go's `Transport` did the same in G2.
- Consequence: routing and policy must be evaluated per request, not per
  client connection.
- Pool outbound connections per destination for `direct` and per upstream
  proxy for `upstream`.
- The design's per-tunnel accounting holds for CONNECT only. An `http://`
  client connection can carry several decisions.

**Other targets.** Go also parses `GET https://origin.cuttle.test/` in
absolute form (G3). That would ask cuttle itself to originate TLS. Browsers
send CONNECT for `https://`, so inference: cuttle should reject absolute-form
`https` targets with 400 or 501 rather than open TLS on the client's behalf.

[9112-absolute]: https://www.rfc-editor.org/rfc/rfc9112.html#section-3.2.2
[9112-origin]: https://www.rfc-editor.org/rfc/rfc9112.html#section-3.2.1
[9110-connection]: https://www.rfc-editor.org/rfc/rfc9110.html#section-7.6.1
[9110-upgrade]: https://www.rfc-editor.org/rfc/rfc9110.html#section-7.8
[go-req-absolute]: https://github.com/golang/go/blob/go1.27.4/src/net/http/request.go#L657
[go-rp-hop]: https://github.com/golang/go/blob/go1.27.4/src/net/http/httputil/reverseproxy.go#L364
[go-rp-te]: https://github.com/golang/go/blob/go1.27.4/src/net/http/httputil/reverseproxy.go#L476
[go-rp-rewrite]: https://github.com/golang/go/blob/go1.27.4/src/net/http/httputil/reverseproxy.go#L121
[go-rp-director]: https://github.com/golang/go/blob/go1.27.4/src/net/http/httputil/reverseproxy.go#L186
[go-rp-xff]: https://github.com/golang/go/blob/go1.27.4/src/net/http/httputil/reverseproxy.go#L242
[go-rp-upgrade]: https://github.com/golang/go/blob/go1.27.4/src/net/http/httputil/reverseproxy.go#L482
[go-rp-upgrade-resp]: https://github.com/golang/go/blob/go1.27.4/src/net/http/httputil/reverseproxy.go#L822

## 3. Loop detection across chained proxies

**Via.**

- A proxy MUST send an appropriate `Via` in each message that it forwards.
- `received-by` may be a pseudonym, and entries with the same protocol may be
  combined.
- RFC 9110 lists "avoiding request loops" among Via's uses
  ([RFC 9110 §7.6.3][9110-via]).
- The rule names no method exemption. Inference: it covers a forwarded
  CONNECT.
- `Max-Forwards` is defined only for TRACE and OPTIONS, and recipients may
  ignore it on other methods ([RFC 9110 §7.6.2][9110-maxfwd]). So it cannot
  be relied on to bound a CONNECT loop.

**squid-cache** (master `76d8131`; SQUID_7_8 has the same defaults).

- Detection: `clientInterpretRequestHeaders()` checks whether the received
  `Via` list contains this instance's `" <unique_hostname> (<version>)"`.
  It logs `WARNING: Forwarding loop detected` and sets
  `flags.loopDetected` ([client_side_request.cc L935–966][squid-detect]).
- Plain HTTP: the request is denied with 403 `ERR_ACCESS_DENIED`
  ([client_side_reply.cc L742–750][squid-deny]).
- CONNECT: the tunnel code never reads the flag. Peer selection only forces
  a direct route ([peer_select.cc L633–636][squid-peer]). Unless
  `never_direct` applies, a looping CONNECT is logged and goes direct, with
  no error to the client.
- Squid adds `Via` to a CONNECT that it sends to a `cache_peer`.
  `Http::Tunneler::writeRequest()` builds the header through the same
  `httpBuildRequestHeader()`, which calls `addVia`
  ([HttpTunneler.cc L146–154][squid-tunneler], [http.cc L1966][squid-via]).
- Its documentation requires distinct `unique_hostname` values "so forwarding
  loops can be detected" ([unique_hostname][squid-unique]).

**Apache Traffic Server** (master `691b733`).

- `HttpTransact::will_this_request_self_loop()` flags a loop in two cases.
  One is that the next hop is one of the proxy's own addresses and ports. The
  other is that its process UUID appears in the incoming `Via` more than
  `proxy.config.http.max_proxy_cycles` times
  ([HttpTransact.cc L7149–7230][ats-loop];
  [max_proxy_cycles][ats-cycles]).
- It answers 400 `Cycle Detected` or `Multi-Hop Cycle Detected`.

**Enterprise gateway.**

- Skyhigh Secure Web Gateway, formerly McAfee Web Gateway, returns 502 for
  a proxy loop when the incoming `Via` contains its own configured Via text.
  The article recommends a single word such as "Proxy" for that text.
- The documented case is two chained gateways. CONNECT is not mentioned.
- The check is the appliance option "HTTP(S): Inspect Via header to detect
  proxy loops" ([Skyhigh Via article][skyhigh-via];
  [Skyhigh deactivate][skyhigh-off]).
- No vendor document was found saying that an intercepting proxy rejects a
  CONNECT merely because it carries `Via`.

**Controlled observations.**

- Chain test X2 sent CONNECT `home` → `office` → mitmproxy 12.3.1 with
  `Via: 1.1 home, 1.1 office`. mitmproxy logged the header and established
  and intercepted the tunnel normally.
- A self-looping `mini` without detection (`loopy2`, no Via) recursed until
  the harness cap of 40 concurrent tunnels, then failed with 503 at 41
  CONNECTs.
- Adding `Via` without checking it (`loopy3`) behaved the same. The last
  request carried 40 `Via` entries.
- With a check for its own pseudonym in `Via` (`loopy`), the second hop
  answered 502 with `Proxy-Status: loopy; error=proxy_loop_detected`.
  Two CONNECTs were made in total.

**Assessment for the home → office → corporate chain** (inferences unless
cited).

- Use a unique pseudonym per instance, such as `cuttle-home` and
  `cuttle-office`, configured or derived from a random per-install ID.
- Add `Via: 1.1 <pseudonym>` to every forwarded request, CONNECT included,
  and reject a request whose `Via` already names this instance. Answer 502
  with `Proxy-Status: <pseudonym>; error=proxy_loop_detected`
  ([RFC 9209 §2.3.32][9209-loop]).
- This catches home → office → home misroutes in one hop. It also matches
  RFC 9110's MUST and Squid's own behavior toward peers.
- Risk to the corporate proxy is low but not zero:
  - A compliant corporate proxy must accept `Via`.
  - Products that detect loops match their own Via text. That text could
    collide with a generic cuttle name, hence the distinctive pseudonym.
  - The pseudonyms reveal the chain to the corporate proxy's logs.
- Smallest safe option: a per-upstream setting that omits `Via` on CONNECT
  to a non-cuttle upstream if the corporate proxy misbehaves. The default
  stays on.
- Backstops that work without `Via`:
  - Reject an upstream whose address is one of cuttle's own listener
    addresses at config load. ATS performs the same next-hop check.
  - Cap concurrent tunnels and upstream dials.
  - Bound each hop's upstream timeout.
  - A private hop-count header is possible but duplicates `Via`.

[9110-via]: https://www.rfc-editor.org/rfc/rfc9110.html#section-7.6.3
[9110-maxfwd]: https://www.rfc-editor.org/rfc/rfc9110.html#section-7.6.2
[9209-loop]: https://www.rfc-editor.org/rfc/rfc9209.html#section-2.3.32
[squid-detect]: https://github.com/squid-cache/squid/blob/76d813169215a82dea60b8a0820d0b100120263e/src/client_side_request.cc#L935-L966
[squid-deny]: https://github.com/squid-cache/squid/blob/76d813169215a82dea60b8a0820d0b100120263e/src/client_side_reply.cc#L742-L750
[squid-peer]: https://github.com/squid-cache/squid/blob/76d813169215a82dea60b8a0820d0b100120263e/src/peer_select.cc#L633-L636
[squid-tunneler]: https://github.com/squid-cache/squid/blob/76d813169215a82dea60b8a0820d0b100120263e/src/clients/HttpTunneler.cc#L146-L154
[squid-via]: https://github.com/squid-cache/squid/blob/76d813169215a82dea60b8a0820d0b100120263e/src/http.cc#L1966
[squid-unique]: https://www.squid-cache.org/Doc/config/unique_hostname/
[ats-loop]: https://github.com/apache/trafficserver/blob/691b7330a6989f3510cfcd8306d015d974e8e731/src/proxy/http/HttpTransact.cc#L7149-L7230
[ats-cycles]: https://docs.trafficserver.apache.org/en/latest/admin-guide/files/records.yaml.en.html#proxy.config.http.max_proxy_cycles
[skyhigh-via]: https://success.skyhighsecurity.com/docs/via-and-x-forwarded-for-headers-proxy-loop-prevention.md
[skyhigh-off]: https://success.skyhighsecurity.com/docs/deactivate-proxy-loop-detection.md

## 4. TLS-intercepting upstream and client trust

**A tunnelling proxy needs no CA or certificate handling.** After the 2xx,
cuttle only relays bytes ([RFC 9110 §9.3.6][9110-connect]). The TLS
handshake runs between the client and whichever party terminates TLS, here
the corporate proxy. Chain tests ran curl → `home` → `office` → mitmproxy →
origin, with no certificate configuration in either `mini`:

- **X1.** The client trusted only the origin's test CA. curl exited 60 with
  `SSL certificate problem: unable to get local issuer certificate`.
  mitmproxy logged `The client does not trust the proxy's certificate for
  origin.cuttle.test (tlsv1 alert unknown ca)`. Both `mini` instances logged
  a normal tunnel.
- **X2.** The client trusted mitmproxy's CA through `--cacert`. The request
  returned 200 with `subject: CN=origin.cuttle.test` and
  `issuer: CN=mitmproxy; O=mitmproxy`. mitmproxy negotiated `h2` with the
  client and logged the decrypted `GET https://origin.cuttle.test:18443/x2`.

The interceptor's certificate, ALPN choice, and policy reach cuttle only as
tunnel bytes.

**What the home client sees for an office-only site.** The chain is home
client → home cuttle → office cuttle → corporate proxy → origin.

1. A `200` to its CONNECT, and only after all three upstream hops answered
   2xx (section 1).
2. A certificate for the requested name, issued by the corporate private
   root instead of the origin's public chain. Without trust in that root,
   curl exits 60 (observed in X1). Browsers show a certificate error
   interstitial; that is an inference and was not observed here.
3. Decrypted traffic at the corporation. The corporate proxy sees requests,
   cookies, and credentials for those sites, as mitmproxy did in X2.
   - The corporate proxy can refuse a site with a non-2xx CONNECT response,
     which cuttle relays as 502.
   - Inference: it can instead return an HTTP block page inside the
     re-signed TLS session, which the client renders as the site.
4. Inference: client-certificate authentication and apps that pin public
   keys fail on the intercepted path, as they do inside the office.

Inference: trusting the corporate root on a home machine lets that root
vouch for any name, not only the routed sites. That is an operator decision
the design should state.

**Chrome on macOS.**

- The Chrome Certificate Verifier, the default on macOS since Chrome 108,
  uses certificates added to the Default and System keychains whose SSL trust
  is "Always Trust" ([Chrome Root Store FAQ][crs-local]).
- Certificate Transparency is not needed for manually installed CAs that
  were never publicly trusted ([Chromium CT doc][cr-ct]).
- Chrome does not check pins when the chain ends at a private trust anchor
  ([Chromium security FAQ][cr-pin]).
- For QUIC, the verifier uses local trust only to remove trust
  ([Chrome Root Store FAQ][crs-quic]). Inference: this does not matter
  through an HTTP proxy, because the tunnel is TCP.
- Not observed: the Chrome runs accepted the test leaf through
  `--ignore-certificate-errors-spki-list`, not through keychain trust.

**Safari.**

- Trust is set per certificate in Keychain Access. Under "Secure Sockets
  Layer (SSL)", choose "Always Trust" ([trust settings][kc-trust];
  [policy values][kc-values]).
- Apple's requirements apply to "All TLS server certificates": RSA keys of
  at least 2048 bits, SHA-2 signatures, and the DNS name in the SAN.
  Certificates issued after 2019-07-01 also need EKU `id-kp-serverAuth` and
  at most 825 days of validity ([Apple 103769][apple-req]). The page does
  not mention user-added roots explicitly.
- The 398-day limit does not affect certificates from user-added or
  administrator-added roots ([Apple 102028][apple-398]).
- The CT policy covers publicly trusted certificates
  ([Apple 103214][apple-ct]).
- Inference: re-signed leaves from the corporate proxy need a SAN, the
  serverAuth EKU, and at most 825 days of validity for Safari.

**`/usr/bin/curl` 8.7.1.**

- The default backend is LibreSSL with `CAfile: /etc/ssl/cert.pem` and
  `CApath: none` (observed with `-v`). Apple's build sets that path
  ([apple-oss curl_config.h][apple-curl]).
- Observed overrides that worked: `--cacert`, `CURL_CA_BUNDLE`,
  `SSL_CERT_FILE`, `--capath`, and `SSL_CERT_DIR`. The last two worked with
  an OpenSSL-hashed directory; a plain directory failed.
- `CURL_SSL_BACKEND=secure-transport` switches to Secure Transport, as
  Apple's README documents. It failed with `Invalid certificate chain` for
  the untrusted CA and succeeded when combined with `CURL_CA_BUNDLE`.
- No primary source says whether the default LibreSSL path consults the
  keychain, and that was not tested (open question).
- The manual documents the variables ([curl manpage][curl-man]):
  `CURL_CA_BUNDLE` and `SSL_CERT_FILE` act as `--cacert`, and
  `SSL_CERT_DIR` acts as `--capath`.

**Homebrew curl** (not installed here; formula reading only).

- The current formula (curl 8.23.0) builds against `openssl@4` with
  `--with-apple-sectrust` and `--with-ca-fallback` and no bundled CA path, so
  Apple SecTrust handles default verification
  ([curl.rb][brew-curl]; [sslcerts][curl-sslcerts]).
- `--cacert` alone disables SecTrust; `--ca-native --cacert` uses both.
  `--ca-native` with SecTrust exists since curl 8.17.0
  ([curl manpage][curl-native]).
- The `ca-certificates` bundle used by `openssl@4` is an install-time
  snapshot. It takes certificates from `System.keychain` and
  `SystemRootCertificates.keychain` that pass `security verify-cert`.
  A root trusted only in the login keychain is not included
  ([ca-certificates.rb][brew-ca]).

[crs-local]: https://chromium.googlesource.com/chromium/src/+/main/net/data/ssl/chrome_root_store/faq.md#how-does-the-chrome-certificate-verifier-integrate-with-platform-trust-stores-for-local-trust-decisions
[crs-quic]: https://chromium.googlesource.com/chromium/src/+/main/net/data/ssl/chrome_root_store/faq.md#how-does-the-chrome-certificate-verifier-consider-local-trust-decisions
[cr-ct]: https://chromium.googlesource.com/chromium/src/+/main/net/docs/certificate-transparency.md#Locally_trusted-CAs
[cr-pin]: https://chromium.googlesource.com/chromium/src/+/main/docs/security/faq.md#How-does-key-pinning-interact-with-local-proxies-and-filters
[kc-trust]: https://support.apple.com/guide/keychain-access/change-the-trust-settings-of-a-certificate-kyca11871/mac
[kc-values]: https://support.apple.com/guide/keychain-access/change-certificate-trust-policies-on-mac-mchlp2824/11.0/mac/27
[apple-req]: https://support.apple.com/en-us/103769
[apple-398]: https://support.apple.com/en-us/102028
[apple-ct]: https://support.apple.com/en-us/103214
[apple-curl]: https://github.com/apple-oss-distributions/curl/blob/curl-166.0.0.0.1/config_mac/curl_config.h
[curl-man]: https://curl.se/docs/manpage.html
[curl-native]: https://curl.se/docs/manpage.html#--ca-native
[curl-sslcerts]: https://curl.se/docs/sslcerts.html
[brew-curl]: https://github.com/Homebrew/homebrew-core/blob/main/Formula/c/curl.rb
[brew-ca]: https://github.com/Homebrew/homebrew-core/blob/main/Formula/c/ca-certificates.rb

## 5. How macOS clients choose and talk to the proxy

**System settings.**

- macOS 27 offers these entries: Auto proxy discovery, Automatic proxy
  configuration (a PAC URL), "Web proxy (HTTP)", "Secure web proxy (HTTPS)",
  and SOCKS proxy. It also has a comma-separated "Bypass proxy settings for
  these hosts & domains" list and "Exclude simple hostnames"
  ([Apple proxy settings][apple-proxy]).
- Chrome reads them with `SCDynamicStoreCopyProxies`
  ([proxy_config_service_mac.cc][cr-mac-config]):
  - `HTTPEnable` sets the proxy for `http://` URLs.
  - `HTTPSEnable` sets the proxy for `https://` URLs.
  - `ExceptionsList` becomes bypass rules.
  - `ExcludeSimpleHostnames` bypasses dotless names.
- Chromium's code says: "The "HTTPS" on the Mac side here means "proxy
  applies to https:// URLs"; the proxy itself is still expected to be an
  HTTP proxy" ([proxy_chain_util_apple.cc][cr-mac-scheme]). For cuttle, both
  entries name the same plain-HTTP listener.
- For `ws://`, Chrome takes "proxies for HTTPS" before "proxies for HTTP"
  ([proxy.md][cr-proxy-ws]). It forces a CONNECT tunnel for WebSockets
  ([client_socket_pool.cc][cr-ws-connect]), as observed in section 2.
- PAC is supported. A PAC fetch must return exactly 200, stay under 1 MB,
  and never goes through a proxy ([proxy.md][cr-proxy]).
- `URLSessionConfiguration.connectionProxyDictionary` defaults to the system
  settings ([Apple][apple-cpd]). Inference: Safari uses the same settings
  through CFNetwork. Safari was not run, because that needs a system proxy
  change.

**Implicit bypass in Chrome.**

- Chrome never proxies localhost names (including `*.localhost`), loopback,
  link-local, or unspecified IP literals.
- Manual settings can override this with `<-loopback>`; PAC cannot
  ([proxy.md][cr-bypass]).
- Observed: with `--proxy-server` set, `http://localhost:18080/` went
  `DIRECT` in the net-log, and so did the `dec` case, logged as
  `http://127.0.0.1:18080/`. The tag indicates a decimal-IPv4 input, but the
  exact command line was not preserved.
- Test fixtures therefore need non-loopback names, as `*.cuttle.test`
  provided here.

**curl.**

- `-x host:port` without a scheme means an HTTP proxy on default port 1080
  and overrides the proxy environment variables.
- The lowercase variable wins, and `http_proxy` exists only in lowercase
  ([curl manpage][curl-x]).
- Observed with 8.7.1 in a clean environment:
  - `HTTP_PROXY` was ignored and the request went direct. `http_proxy`,
    `HTTPS_PROXY`, `https_proxy`, and `ALL_PROXY` were honored.
  - `NO_PROXY=cuttle.test` and `no_proxy=.cuttle.test` bypassed
    `plain.cuttle.test`. `no_proxy=*.cuttle.test` did not, because only a
    lone `*` is a wildcard.
  - `no_proxy='*'` also bypassed a proxy given with `-x`, as documented, and
    `--noproxy '*'` overrode `http_proxy`.
- curl has no implicit loopback bypass. `http://127.0.0.1:18080/` and
  `http://localhost:18080/` were sent to the proxy.
- Go's `ProxyFromEnvironment` differs: it exempts `localhost` and loopback
  IPs (G9; [transport.go L517][go-env], [httpproxy L178][go-env-loop]).

**Names, not resolved IPs.** Every client sent the hostname, in CONNECT and
in absolute-form, whenever the URL had one: Chrome, curl, CFNetwork, and Go
(recorder heads). IP literals appeared only when the URL contained one.
Observed authority spellings:

| URL host | Chrome 157 | curl 8.7.1 | CFNetwork |
| --- | --- | --- | --- |
| IPv6 literal | `CONNECT [2001:db8::1]:443` | `CONNECT [2001:db8::1]:8443` | `CONNECT 2001:db8::1:443`, `Host: 2001:db8::1` (no brackets) |
| IPv4 literal | `CONNECT 192.0.2.10:443` | `CONNECT 192.0.2.10:443` | `CONNECT 192.0.2.10:443`, `Host: 192.0.2.10` |
| IPv6 zone `[fe80::1%25lo0]` | not run | `CONNECT [fe80::1]:8443` (zone dropped) | not run |
| IDN `bücher.cuttle.test` | `CONNECT xn--bcher-kva.cuttle.test:443` | raw UTF-8 `b\303\274cher.cuttle.test:443` | not run |
| Mixed case | lowercase on the wire (`upper` case) | `CONNECT ORIGIN.Cuttle.TEST:18443` | not run |
| Trailing dot | `origin.cuttle.test.:18443` | `origin.cuttle.test.:18443` | not run |
| Decimal IPv4 | bypassed as loopback | `GET http://127.0.0.1:18080/` | not run |

Go's `net/http` server rejects CFNetwork's bracketless form,
`CONNECT 2001:db8::1:443`, with 400 before any handler runs. `url.Parse`
reports `invalid port ":db8::1:443" after host` (check `v6check`). So a
listener built on `net/http` cannot serve CFNetwork clients that use IPv6
literals, unless it parses the request line itself.

Inference for the design: normalize before rule matching.

- Lowercase ASCII names and define trailing-dot handling.
- Accept only A-label names, or convert U-labels, and reject raw non-ASCII
  bytes.
- Canonicalize IP literals with `net/netip`.
- Require a port.
- Match IP-literal rules separately from name rules.

**DNS lookups by Chrome.** Chrome's net-log recorded host-resolver requests
for the destination, such as `https://origin.cuttle.test:18443`, under the
network-quality-estimator source while the request itself was proxied. They
failed with `ERR_NAME_NOT_RESOLVED` and did not affect the load. Their
purpose is not established (open question). Inference: names of office-only
sites may reach the home resolver even when cuttle routes them.

[apple-proxy]: https://support.apple.com/guide/mac-help/change-proxy-settings-on-mac-mchlp2591/mac
[apple-cpd]: https://developer.apple.com/documentation/foundation/urlsessionconfiguration/connectionproxydictionary
[cr-mac-config]: https://chromium.googlesource.com/chromium/src/+/HEAD/net/proxy_resolution/proxy_config_service_mac.cc
[cr-mac-scheme]: https://chromium.googlesource.com/chromium/src/+/HEAD/net/proxy_resolution/proxy_chain_util_apple.cc
[cr-proxy]: https://chromium.googlesource.com/chromium/src/+/HEAD/net/docs/proxy.md
[cr-proxy-ws]: https://chromium.googlesource.com/chromium/src/+/HEAD/net/docs/proxy.md#Mapping-WebSockets-URLs-to-a-proxy
[cr-bypass]: https://chromium.googlesource.com/chromium/src/+/HEAD/net/docs/proxy.md#Implicit-bypass-rules
[cr-ws-connect]: https://chromium.googlesource.com/chromium/src/+/HEAD/net/socket/client_socket_pool.cc
[curl-x]: https://curl.se/docs/manpage.html#-x
[go-env]: https://github.com/golang/go/blob/go1.27.4/src/net/http/transport.go#L517
[go-env-loop]: https://github.com/golang/go/blob/go1.27.4/src/vendor/golang.org/x/net/http/httpproxy/proxy.go#L178

## 6. What clients show for proxy-generated failures

| Proxy answer | Chrome 157 (headless) | curl 8.7.1 | CFNetwork (`URLSession`) |
| --- | --- | --- | --- |
| CONNECT 403 | `ERR_TUNNEL_CONNECTION_FAILED`, body not shown | exit 56, `CONNECT tunnel failed, response 403` | error 310, stream domain 4, code -2096 |
| CONNECT 502 | same as 403 | exit 56, `... response 502` | same as 403 |
| CONNECT 504 | same as 403 | exit 56, `... response 504` | same as 403 |
| CONNECT 407 | 407 logged, final `ERR_TUNNEL_CONNECTION_FAILED` | exit 56, `... response 407` | proxy Basic challenge sent to the app, stall until the 8 s timeout, then 310 with -2102 |
| `http://` 403 with HTML body | body rendered as the page | body printed, exit 0 (`-f`: exit 22) | status 403 and body delivered |

CFNetwork error 310 is `kCFErrorHTTPSProxyConnectionFailure`
(`CFNetworkErrors.h` line 93 in the macOS 27.2 SDK). Stream domain 4 is
`kCFStreamErrorDomainHTTP`.

**Chromium source** ([http_proxy_client_socket.cc][cr-tunnel]):

- Only an exact `200` establishes a tunnel. Every other code, including
  other 2xx, becomes `ERR_TUNNEL_CONNECTION_FAILED`.
- A 200 followed by extra bytes after the head also fails.
- A 407 goes into proxy authentication.
- The body of a failed CONNECT is ignored "to avoid letting the proxy
  impersonate the target server" (crbug 137891).
- `ERR_PROXY_CONNECTION_FAILED` (-130) means that the proxy itself was
  unreachable and excludes CONNECT failures ([net_error_list.h][cr-errors]).
- Headed Chrome was not run, and by instruction no further 407 tests were
  run (open questions).

**curl.**

- curl accepts any 2xx for CONNECT.
- 8.7.1 exits 56 on a non-2xx CONNECT. Current master returns 7,
  `CURLE_COULDNT_CONNECT`, from `lib/cf-h1-proxy.c`. Scripts should not
  depend on 56 across versions ([curl exit codes][curl-exit]).

**Safari** was not run. Inference: CFNetwork exposes neither the status nor
the body of a failed CONNECT, so Safari can show only a generic failure.

**The 407 incident.** The CFNetwork probe ran:

```text
./cfnet/probe certs/ca.pem ... https://s407.cuttle.test/cf-c407 ...
```

It is a command-line binary that uses a per-session proxy dictionary and
default handling for authentication challenges. It received 407 with
`Proxy-Authenticate: Basic` and stalled until its 8-second timeout, which the
recorder's next request, 8 seconds later, confirms. The orchestrator then
reported a proxy authentication dialog on the user's screen. Inference:
CFNetwork's default handling of a proxy challenge can raise a system
credential prompt, even for a non-GUI process.

The earlier 407 runs cannot show a macOS dialog. They ran before the
CFNetwork probe, in this order: Go `Transport`, curl, and headless Chrome
with `--use-mock-keychain`.

**Implications for reporting a policy block.**

- A blocked HTTPS destination looks the same as an upstream failure in
  browsers. Chrome shows `ERR_TUNNEL_CONNECTION_FAILED` for 403, 502, and
  504, and CFNetwork returns error 310. Only curl shows the code. Operators
  will explain blocks through cuttle's log, metrics, and admin UI.
- Distinct codes still help curl, logs, and `Proxy-Status`: 403 with
  `http_request_denied` for policy, and 502 or 504 for upstream failures.
- Keep CONNECT error bodies short, because browsers discard them.
- On success, send exactly `HTTP/1.1 200` with no bytes after the head, even
  when the upstream answered another 2xx. That follows from Chrome's rule.
- Never send 407.
- A blocked `http://` request shows the proxy's body to the user, under the
  destination's origin. Inference: make it plain text, or escape anything
  reflected from the request. Add `Cache-Control: no-store` and give no
  script a chance to run in that origin.

[cr-tunnel]: https://chromium.googlesource.com/chromium/src/+/HEAD/net/http/http_proxy_client_socket.cc
[cr-errors]: https://chromium.googlesource.com/chromium/src/+/HEAD/net/base/net_error_list.h
[curl-exit]: https://curl.se/docs/manpage.html#EXIT

## 7. Exposure risks

**An unauthenticated forward proxy reachable over the VPN.**

- Squid's default policy allows only localhost clients, "because a more
  permissive Squid installation could introduce new attack vectors". Its
  `allow localnet` line ships commented out. Its default rules deny
  `!Safe_ports`, `CONNECT !SSL_ports`, `to_localhost` (127.0.0.0/8,
  0.0.0.0/32, ::1, ::), and `to_linklocal` (169.254.0.0/16, fe80::/10)
  ([cf.data.pre L2002–2037][squid-default]; ACLs at
  [L1115–1116][squid-acl]).
- The Squid wiki warns: "If your proxy allows access from untrusted hosts or
  users, you can be sure that people will find and abuse your service"
  ([Squid security pitfalls][squid-open]).
- RFC 9110 says CONNECT SHOULD be limited to known ports or a configurable
  list of safe targets ([RFC 9110 §9.3.6][9110-connect]).

Inferences about the office instance:

- Anything that can route to the listening address can use it: the home
  network across the VPN, and the office LAN or corporate network too if it
  binds every interface.
- A user of office cuttle acts with the office machine's network position,
  and the corporate proxy attributes the traffic to the employee.
- Its direct route can reach the office machine itself. A CONNECT to
  `127.0.0.1:<admin port>` arrives from loopback, so the loopback-only admin
  UI becomes reachable from the VPN. Other loopback services, the office
  LAN, and link-local addresses are exposed the same way. This is the case
  Squid's `to_localhost` and `to_linklocal` defaults exist for.

Spellings defeat string checks. Go's resolver returned `127.0.0.1` for
`2130706433`, `0x7f.1`, and `127.1`, and `net.ParseIP` returned nil for each
(check G10). `localhost` and `LOCALHOST.` resolved to `::1` and `127.0.0.1`.

OWASP says to validate the *resolved* addresses and connect only to them,
because "A second, unchecked DNS lookup between validation and connection
can bypass these checks". It says to normalize, such as unmapping
`::ffff:0:0/96`, before comparing. Its deny list includes loopback, `0/8`,
RFC 1918, link-local, `fc00::/7`, multicast, and NAT64
([OWASP SSRF][owasp-ssrf]; [deny list][owasp-ssrf-deny]).

Mitigations for the design (inferences consistent with these sources):

1. Bind to explicit addresses. Use loopback at home. At the office, use
   loopback plus the VPN interface address, never the wildcard by default.
2. Check a source CIDR allowlist, such as the home VPN peer, on accept,
   before reading the request.
3. Give remote sources a narrower policy than local ones. For example, allow
   remote clients only the corporate upstream route or only listed domains,
   with a default of `block`.
4. For direct routes, check the actual socket address in
   `net.Dialer.ControlContext`. Deny loopback, unspecified, link-local, and
   the host's own addresses. Deny private ranges unless a rule explicitly
   allows them. Upstream routes do no local resolution, because the next
   proxy resolves.
5. Restrict CONNECT ports: 443, plus 80 if `ws://` must work (section 2).
6. Cap connections per source, and log the source address with every
   decision.
7. Use loop detection (section 3) to stop home ↔ office amplification.

**A loopback-only admin web UI.**

- **The proxy is the largest exposure.** A CONNECT to the admin port through
  a VPN-reachable cuttle arrives from loopback and carries
  `Host: 127.0.0.1:<port>`, so a Host check passes. Only the destination
  checks above stop it (inference).
- **DNS rebinding.** A page whose name rebinds to 127.0.0.1 can send
  same-origin requests to the admin port with `Host: attacker.example:<port>`.
  - Go's documentation says handlers "should validate that the Host header
    has a value for which the Handler considers itself authoritative"
    ([Request.Host][go-host]).
  - The server does not check `Host` by default. That issue was closed with
    documentation only ([golang/go#23993][go-23993]).
  - NCC Group's guidance is to accept only `127.0.0.1:<port>` and
    `localhost:<port>` ([NCC Group][ncc-rebind]). Inference: also accept
    `[::1]:<port>` if the UI listens there.
- **Chrome Local Network Access** has been stable since Chrome 142. It
  prompts when a public site requests loopback
  ([Chrome 142 notes][cr-lna]). Chrome 145 split the permission into
  `local-network` and `loopback-network`, and Chrome 147 added WebSockets.
  The specification says that an administration interface must defend
  against CSRF "on its own" and should not rely on the browser
  ([LNA spec][lna-scope]). It is defense in depth only.
- **CSRF.**
  - Go 1.25 and later provide `http.CrossOriginProtection`. It rejects
    non-safe cross-origin browser requests, judged by `Sec-Fetch-Site` or
    by `Origin` against `Host`. It always allows GET, HEAD, and OPTIONS. It
    allows requests that carry neither header
    ([pkg.go.dev][go-cop]; [csrf.go L15][go-csrf]).
  - OWASP accepts Fetch Metadata as the main defense for modern browsers,
    with an Origin fallback, and notes that browsers send it to localhost,
    127.0.0.0/8, and ::1. It says to reject a state-changing request when
    neither header establishes trust and no token succeeds, which is
    stricter than Go's default ([OWASP CSRF][owasp-csrf-fm]).
  - Login CSRF calls for a pre-session token on the login form
    ([OWASP CSRF][owasp-csrf-login]).
  - Inference: `CrossOriginProtection` does not stop DNS rebinding, because
    the rebound `Origin` and `Host` match. Use it with the Host allowlist,
    POST-only state changes, a login-form token, and `SameSite=Strict`.
- **Cookies and sessions.**
  - OWASP: `SameSite=Strict` is preferred, and `Secure` and `HttpOnly` are
    mandatory. `__Host-` requires `Secure`, no `Domain`, and `Path=/`.
    Regenerate the session ID at login. An idle timeout of 15–30 minutes
    suits low-risk applications. Set an absolute timeout as well
    ([OWASP sessions][owasp-session]).
  - On `http://` loopback, the cookie draft leaves "secure" to the user
    agent ([rfc6265bis §5.8.3][6265bis]).
  - Chrome accepts `Secure` cookies on localhost
    ([httpwg issue 2605][httpwg-2605]). Current Chromium source also accepts
    `__Host-` there ([cookie_util.cc][cr-cookie]); the shipping release is
    unknown.
  - Safari 27.0 added `Secure` cookies on loopback hosts over HTTP
    ([WebKit blog][webkit-27]), and earlier Safari rejected them. Whether
    Safari accepts `__Host-`, and whether "loopback hosts" includes the name
    `localhost`, is not documented (open question).
- **Login.**
  - Return generic errors.
  - Account lockout can become a denial of service
    ([OWASP authentication][owasp-auth-lock]). Inference: for a single local
    admin, use per-account exponential backoff rather than a hard lockout.
  - Hash with Argon2id at no less than m=19 MiB, t=2, p=1. Fall back to
    scrypt; bcrypt is for legacy systems only, needs a work factor of at
    least 10, and is limited to 72 bytes ([OWASP passwords][owasp-pw]).
    Go has `golang.org/x/crypto/argon2` ([IDKey][go-argon2]).
- Other local users on the same Mac can reach loopback, which the decided
  password covers.

[go-host]: https://pkg.go.dev/net/http#Request
[go-23993]: https://github.com/golang/go/issues/23993
[ncc-rebind]: https://github.com/nccgroup/singularity/wiki/Preventing-DNS-Rebinding-Attacks
[cr-lna]: https://developer.chrome.com/release-notes/142#local_network_access_restrictions
[lna-scope]: https://wicg.github.io/local-network-access/#scope-of-mitigation
[go-cop]: https://pkg.go.dev/net/http#CrossOriginProtection
[go-csrf]: https://github.com/golang/go/blob/go1.27.4/src/net/http/csrf.go#L15
[owasp-csrf-fm]: https://cheatsheetseries.owasp.org/cheatsheets/Cross-Site_Request_Forgery_Prevention_Cheat_Sheet.html#fetch-metadata-headers
[owasp-csrf-login]: https://cheatsheetseries.owasp.org/cheatsheets/Cross-Site_Request_Forgery_Prevention_Cheat_Sheet.html#possible-csrf-vulnerabilities-in-login-forms
[owasp-session]: https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html#cookie-name-prefixes
[6265bis]: https://httpwg.org/http-extensions/draft-ietf-httpbis-rfc6265bis.html#section-5.8.3
[httpwg-2605]: https://github.com/httpwg/http-extensions/issues/2605
[cr-cookie]: https://github.com/chromium/chromium/blob/main/net/cookies/cookie_util.cc
[webkit-27]: https://webkit.org/blog/18325/webkit-features-for-safari-27-0/
[owasp-auth-lock]: https://cheatsheetseries.owasp.org/cheatsheets/Authentication_Cheat_Sheet.html#account-lockout
[owasp-pw]: https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html#argon2id
[go-argon2]: https://pkg.go.dev/golang.org/x/crypto/argon2#IDKey

[squid-default]: https://github.com/squid-cache/squid/blob/76d813169215a82dea60b8a0820d0b100120263e/src/cf.data.pre#L2002-L2037
[squid-acl]: https://github.com/squid-cache/squid/blob/76d813169215a82dea60b8a0820d0b100120263e/src/cf.data.pre#L1115-L1116
[squid-open]: https://wiki.squid-cache.org/SquidFaq/SecurityPitfalls#open-access-proxies
[owasp-ssrf]: https://cheatsheetseries.owasp.org/cheatsheets/Server_Side_Request_Forgery_Prevention_Cheat_Sheet.html#domain-name
[owasp-ssrf-deny]: https://cheatsheetseries.owasp.org/cheatsheets/Server_Side_Request_Forgery_Prevention_Cheat_Sheet.html#deny-list-last-resort

## Open questions

Each item names the smallest controlled check that would settle it.

1. **Safari's CONNECT and error UI.** How does Safari spell an IPv6 literal
   in CONNECT? Does it match CFNetwork's bracketless form? What does it show
   for CONNECT 403, 502, and 504? Which proxy entry does it use for `ws://`?
   Check: in a disposable macOS VM or test account, set "Secure web proxy" to
   the recorder with the 407 case removed. Load `https://[2001:db8::1]/`,
   `https://s403.cuttle.test/`, `s502`, and `s504`. Record the request heads
   and a screenshot. Not run, because it needs a system proxy change.
2. **407 in headed browsers.** Not run, by instruction, after the dialog
   incident. cuttle never sends 407, so this is low priority. If needed,
   test only in a VM with no user session, never on the user's desktop.
3. **`/usr/bin/curl` and the keychain.** Does the default LibreSSL path
   honor a root that is trusted in the keychain? Check: in a VM, mark a test
   CA "Always Trust" in the System keychain and run `/usr/bin/curl` with no
   CA options against the controlled origin.
4. **Browsers with a keychain-trusted private root through the chain.**
   Check: in a VM, trust the mitmproxy CA. Run Chrome without
   `--ignore-certificate-errors-spki-list`, and Safari, through
   `home` → `office` → mitmproxy.
5. **Chrome's destination-name lookups while proxied.** Check: rerun the
   `ok` case with `--net-log-capture-mode=Everything` and a unique test
   name. Watch `log stream --predicate 'process == "mDNSResponder"'` for
   that name.
6. **The real corporate proxy.** Does it accept `Via` on CONNECT, and what
   is its own Via text? Does it refuse with a CONNECT status or with a block
   page? What is its CONNECT timeout? Do its re-signed leaves carry a SAN,
   the serverAuth EKU, and at most 825 days of validity?
   Check: the user runs, from the office machine, one CONNECT to an allowed
   site with and without `Via: 1.1 cuttle-office`, then
   `openssl s_client -proxy <corp>:<port> -connect <site>:443 -showcerts`.
   Not run, because the brief forbids contacting real proxies.
7. **Admin cookies on loopback.** Do `Secure` and `__Host-` cookies work on
   `http://127.0.0.1` and `http://localhost` in current Chrome and Safari
   27.2? Check: a loopback Go server that sets each variant, read back in
   headless Chrome, and in Safari only with the user's consent.
8. **Accepting CFNetwork's bracketless IPv6 CONNECT.** Should cuttle parse
   the request line itself to accept it, or document the limitation? Decide
   after item 1 shows whether Safari sends that form.

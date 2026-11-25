# Client compatibility

This matrix records outcomes observed on real clients with cuttle. For each
check, record the client and macOS versions, the proxy configuration, the
case from the [client checks][cc], the tested commit and tree, and a link
to evidence. Verify both the client outcome and the route, using the
decision log and the mock records. For a block, record the client-visible
error and confirm that no outbound connection was made. Account for each
client's proxy bypass rules. Only the tester updates rows, and only for
checks actually performed.

## Proxy

"Non-disclosure" means that `mockenv check-disclosure` passes: requests a
mock records through edge → gateway with `disclosure = "none"` carry no
disclosure field of [design 0003 D5][nd], not even one the client sent,
and no value with an instance name or the original client address, and they
match the direct run in method, target, and body bytes.

| Client | Check | Proxy configuration | Direct HTTPS | Direct HTTP | Upstream HTTPS | WebSocket | Block | Chain through a TLS-inspecting proxy | Non-disclosure | IPv6 literal | Evidence |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Chrome on macOS, headless | M1 | `--headless=new --use-mock-keychain`, throwaway profile, `--proxy-server=http://127.0.0.1:<port>`, `--proxy-bypass-list="<-loopback>"` | PASS | PASS | PASS | PASS | PASS | PASS | PASS | Not in M1 | [M1 run](#m1-client-check-run-2026-11-25) |
| `/usr/bin/curl` | M1 | `--proxy http://127.0.0.1:<port>`, `-g` | PASS | PASS | PASS | NOT RUN, Chrome only | PASS | PASS | PASS | Not in M1 | [M1 run](#m1-client-check-run-2026-11-25) |
| Chrome on macOS | M3 | `--proxy-server=http://127.0.0.1:3128` with a throwaway profile, or the macOS proxy settings | Pending | Pending | Pending | Pending | Pending | Pending | Pending | Pending | — |
| Safari on macOS | M3, with approval | System Settings → Network → Proxies: "Web proxy (HTTP)" and "Secure web proxy (HTTPS)" both `127.0.0.1:3128` | Pending | Pending | Pending | Pending | Pending | Pending | Pending | Pending | — |
| `/usr/bin/curl` | M3 | `--proxy http://127.0.0.1:3128`, or lowercase `http_proxy` and `https_proxy` | Pending | Pending | Pending | Not run | Pending | Pending | Pending | Pending | — |

### M1 client check run, 2026-11-25

TASK-0014 ran the [M1 client check][cc] once, from the repository root,
against mocks only:

```sh
go run ./tools/mockenv m1-check -out <evidence directory>
```

It exited 0 and printed `M1 client check: pass`, so no rerun was needed.
Every case passed with both clients. WebSocket and the LNA observation run
with Chrome only: curl's are `NOT RUN`, and Chrome's LNA observation is
`OBSERVED`. The evidence directory, with each client's session records,
configurations, decision logs, Chrome DOMs, and `report.txt`, stays outside
Git.

| Item | Value |
| --- | --- |
| Tested commit | `7f246072e0d0256df0ebd439dd816b03f4359581`, tree `22a5a09f4c4c169e465fd5f9c73c6ed09ef80236`. It differs from the TASK-0012 landing `169df68` (tree `607aa63`) only in `docs/TASKS.md`. |
| Chrome | Google Chrome 158.0.8127.37, headless; it sent `HeadlessChrome/158.0.0.0` in `User-Agent` |
| macOS | 27.2.1 (26B131) |
| curl | `curl 8.7.1 (x86_64-apple-darwin26.0) libcurl/8.7.1 (SecureTransport) LibreSSL/3.3.6 zlib/1.2.12 nghttp2/1.69.0` |
| Cleanup | After the run, `pgrep` found no cuttle, mockenv, or Chrome process of the run, and no new `cuttle-*` directory remained under `/tmp`. |

What the check verified, per case:

- Direct HTTPS and Direct HTTP: the origin's echo in Chrome's DOM and in
  curl's body; decision `rule=loopback action=direct outcome=ok`.
- Block: Chrome's DOM was empty; curl printed
  `curl: (56) CONNECT tunnel failed, response 403` with
  `%{http_connect}` `403`; decision `outcome=blocked status=403`; no mock
  received `blocked.cuttle.test`.
- Upstream: the echo, and the recording upstream's
  `CONNECT origin.cuttle.test:443` with one
  `Via: 1.1 cuttle-single (cuttle)`.
- WebSocket (Chrome): `CONNECT ws.cuttle.test:80` recorded by the
  recording upstream, the page's message recorded by the origin, and the
  page status `echo: hello from ws.html`.
- Chain: the echo of `/chain`; curl trusted only the inspection root; the
  inspecting egress mock decrypted `GET /chain` with a leaf signed by the
  inspection root CA, and its `CONNECT` heads carried no `Via` or other
  disclosure field.
- Non-disclosure: `check-disclosure` exited 0 for Chrome (4 case targets,
  48 `egress-direct` and 6 `egress-chain` requests, 0 failures) and for
  curl (5 case targets, 5 and 5 requests, 0 failures).

#### LNA observation

The page `https://lna.cuttle.test/lna.html`, loaded through
`cuttle-single`, fetched three literals in `no-cors` mode and printed these
permission states:

```text
permission local-network-access => prompt
permission local-network => prompt
permission loopback-network => prompt
```

| Fetch | Page outcome | Decision by `cuttle-single` | Origin received it |
| --- | --- | --- | --- |
| `https://10.20.0.5/echo`, private | `response type=opaque` | Yes, `rule=test-literals action=upstream upstream=recording outcome=ok` | Yes, one `GET /echo` for `10.20.0.5` |
| `http://127.0.0.1:<origin port>/echo`, loopback | `response type=opaque` | Yes, `rule=loopback action=direct outcome=ok` | Yes, one `GET /echo` |
| `https://203.0.113.10/echo`, public control | `response type=opaque` | Yes, `rule=test-literals action=upstream upstream=recording outcome=ok` | Yes, one `GET /echo` for `203.0.113.10` |

The decision lines, in the order of the table:

```text
time=2026-11-25T12:56:02.573Z level=INFO msg=decision id=b8f04e48 client=127.0.0.1:58441 source=local kind=connect target=10.20.0.5:443 rule=test-literals action=upstream upstream=recording interface="" outcome=ok status=200 up=2558 down=2938 dur=7ms
time=2026-11-25T12:56:02.567Z level=INFO msg=decision id=0f55f34e client=127.0.0.1:58442 source=local kind=http target=127.0.0.1:58183 rule=loopback action=direct upstream="" interface="" outcome=ok status=200 up=0 down=642 dur=1ms
time=2026-11-25T12:56:02.572Z level=INFO msg=decision id=8363bcc5 client=127.0.0.1:58444 source=local kind=connect target=203.0.113.10:443 rule=test-literals action=upstream upstream=recording interface="" outcome=ok status=200 up=2625 down=2941 dur=7ms
```

Chrome 158 headless sent the private-literal fetch through cuttle with no
permission request: cuttle logged a decision, the recording upstream
recorded `CONNECT 10.20.0.5:443`, the origin received the request with
`Sec-Fetch-Site: cross-site` and `Sec-Fetch-Mode: no-cors`, the page got a
response, and the permission states stayed `prompt`. The origin recorded
no `OPTIONS` preflight. This is the observation that assumption A6 of
[design 0003][a6] and OI-0006 V6 call for. The check has no run without a
proxy, so it does not show whether this Chrome build would ask for the
permission on a direct connection.

## Admin UI

| Client | Check | Login and session | Dashboard | Settings save | Evidence |
| --- | --- | --- | --- | --- | --- |
| Chrome on macOS, headless | M2 | Pending | Pending | Pending | — |
| Chrome on macOS | M3 | Pending | Pending | Pending | — |
| Safari on macOS | M3, with approval | Pending | Pending | Pending | — |

## Interfaces

The M1.5 device check of [design 0008][dc] runs on the user's Mac with the
user typing the commands. Rows record outcomes only: yes, no, or short
notes, and no MAC address, IP address, network name, or serial number.
Step numbers refer to the check.

| Check | Steps | Built-in Wi-Fi | Ethernet adapters | Phones over USB | Evidence |
| --- | --- | --- | --- | --- | --- |
| Kind, media, and default route shown by `cuttle interfaces` | 1 | Pending | Pending | Pending | — |
| Distinct BSD names and MACs for devices of one kind | 1 | Not applicable | Pending | Pending | — |
| MAC and BSD name kept across replugging, port change, and reboot | 2, 3 | Not applicable | Pending | Pending | — |
| Same-model replacement takes over a BSD name; the MAC match stays `absent` | 4 | Not applicable | Optional | Not run | — |
| Time to a state change on cable pull, unplug, sharing off, and Wi-Fi off; index after replugging | 5 | Pending | Pending | Pending | — |
| First listed interface carries the flow, by address and byte count | 6, 8 | Not applicable | Pending | Not applicable | — |
| Bound flow leaves through a non-primary interface, by address and byte count | 9, 10 | Not applicable | Pending | Pending | — |
| `link_down` skip, and `503 no_interface` for a pinned interface that is gone | 7, 10 | Not applicable | Pending | Pending | — |
| Phone without cellular data: failover reason and demotion | 11 | Not applicable | Not applicable | Pending | — |
| Dead uplink behind the primary: `dns_error` for names, failover for an address | 12 | Not applicable | Optional | Not applicable | — |

## Known client behavior

From [research 0002](research/0002-proxy-chaining-and-interception.md);
these are inputs to the design, not recorded compatibility checks.

| Behavior | Clients | Design response |
| --- | --- | --- |
| Only an exact `200` opens a tunnel | Chrome | cuttle always sends exactly `200` |
| IPv6 literal sent without brackets in `CONNECT` | CFNetwork; Safari unverified ([OI-0002][oi-2]) | Go's server answers `400`; a documented limitation until OI-0002 |
| Raw UTF-8 host for an IDN | curl 8.7.1 | `400`; use the A-label |
| `ws://` tunnelled with `CONNECT` to port 80 | Chrome | Local clients have no port policy; remote clients need port 80 in `proxy.remote.ports`, which it is by default |
| localhost, loopback, and link-local never proxied | Chrome | The admin UI is reached directly; the M1 check removes the bypass with `<-loopback>` |
| No implicit loopback bypass | curl | The starter `loopback` rule allows `localhost`, `127.0.0.0/8`, and `::1` on any port |
| `Proxy-Connection` sent on every request | Chrome, curl, CFNetwork | Removed by standard hop-by-hop handling on every route |
| Exit status for a failed `CONNECT` is 56 in 8.7.1 and 7 later | curl | Tests check `%{http_connect}` |
| A `407` raises a proxy credential prompt | CFNetwork and Safari | cuttle never sends or relays `407`; mocks never send it to Apple clients |

Configuration references: [Chromium proxy support](https://chromium.googlesource.com/chromium/src/+/HEAD/net/docs/proxy.md#manual-proxy-settings), [macOS proxy settings](https://support.apple.com/en-am/guide/mac-help/mchlp2591/27/mac/27), [curl `--proxy`](https://curl.se/docs/manpage.html#-x).

[a6]: design/0003-upstream-chaining-and-exposure.md#assumptions
[cc]: design/0007-test-environment-and-gates.md#d8-client-checks
[dc]: design/0008-interface-selection.md#device-check-i20
[nd]: design/0003-upstream-chaining-and-exposure.md#d5-non-disclosure-toward-an-egress-proxy
[oi-2]: OPEN_ISSUES.md#oi-0002--safari-and-cfnetwork-behavior-unverified

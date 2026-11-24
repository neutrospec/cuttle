# Client compatibility

This matrix records outcomes observed on real clients with cuttle. For each
check, record the client and macOS versions, the proxy configuration, the
case from the [T2 procedure][t2], the tested commit and tree, and a link to
evidence. Verify both the client outcome and the route, using the decision
log and the mock records. For a block, record the client-visible error and
confirm that no outbound connection was made. Account for each client's
proxy bypass rules. Only the tester updates rows, and only for checks
actually performed.

## Proxy

| Client | Proxy configuration | Direct HTTPS | Direct HTTP | Upstream HTTPS | WebSocket over `CONNECT :80` | Block | Chain through an intercepting proxy | Evidence |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Chrome on macOS | `--proxy-server=http://127.0.0.1:3128` with a throwaway profile, or the macOS proxy settings | Pending | Pending | Pending | Pending | Pending | Pending | — |
| Safari on macOS | System Settings → Network → Proxies: "Web proxy (HTTP)" and "Secure web proxy (HTTPS)" both `127.0.0.1:3128` | Pending | Pending | Pending | Pending | Pending | Pending | — |
| `/usr/bin/curl` | `--proxy http://127.0.0.1:3128`, or lowercase `http_proxy` and `https_proxy` | Pending | Pending | Pending | Pending | Pending | Pending | — |

## Admin UI

| Client | Login and session | Dashboard | Settings save | Evidence |
| --- | --- | --- | --- | --- |
| Chrome on macOS | Pending | Pending | Pending | — |
| Safari on macOS | Pending | Pending | Pending | — |

## Known client behavior

From [research 0002](research/0002-proxy-chaining-and-interception.md);
these are inputs to the design, not recorded compatibility checks.

| Behavior | Clients | Design response |
| --- | --- | --- |
| Only an exact `200` opens a tunnel | Chrome | cuttle always sends exactly `200` |
| IPv6 literal sent without brackets in `CONNECT` | CFNetwork; Safari unverified ([OI-0002](OPEN_ISSUES.md#oi-0002--safari-and-cfnetwork-behavior-unverified)) | cuttle's own parser accepts it |
| Raw UTF-8 host for an IDN | curl 8.7.1 | `400`; use the A-label |
| `ws://` tunnelled with `CONNECT` to port 80 | Chrome | Port 80 is in the default `proxy.ports` |
| localhost, loopback, and link-local never proxied | Chrome | The admin UI is reached directly |
| No implicit loopback bypass | curl | The starter `loopback` rule |
| Exit status for a failed `CONNECT` is 56 in 8.7.1 and 7 later | curl | Tests check `%{http_connect}` |
| A `407` raises a proxy credential prompt | CFNetwork and Safari | cuttle never sends `407`; mocks never send it to Apple clients |

Configuration references: [Chromium proxy support](https://chromium.googlesource.com/chromium/src/+/HEAD/net/docs/proxy.md#manual-proxy-settings), [macOS proxy settings](https://support.apple.com/en-am/guide/mac-help/mchlp2591/27/mac/27), [curl `--proxy`](https://curl.se/docs/manpage.html#-x).

[t2]: design/0007-test-environment-and-gates.md#d8-t2-procedure

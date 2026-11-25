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
| Chrome on macOS, headless | M1 | `--headless=new --use-mock-keychain`, throwaway profile, `--proxy-server=http://127.0.0.1:<port>`, `--proxy-bypass-list="<-loopback>"` | Pending | Pending | Pending | Pending | Pending | Pending | Pending | Not in M1 | — |
| `/usr/bin/curl` | M1 | `--proxy http://127.0.0.1:<port>`, `-g` | Pending | Pending | Pending | Not in M1 | Pending | Pending | Pending | Not in M1 | — |
| Chrome on macOS | M3 | `--proxy-server=http://127.0.0.1:3128` with a throwaway profile, or the macOS proxy settings | Pending | Pending | Pending | Pending | Pending | Pending | Pending | Pending | — |
| Safari on macOS | M3, with approval | System Settings → Network → Proxies: "Web proxy (HTTP)" and "Secure web proxy (HTTPS)" both `127.0.0.1:3128` | Pending | Pending | Pending | Pending | Pending | Pending | Pending | Pending | — |
| `/usr/bin/curl` | M3 | `--proxy http://127.0.0.1:3128`, or lowercase `http_proxy` and `https_proxy` | Pending | Pending | Pending | Not run | Pending | Pending | Pending | Pending | — |

## Admin UI

| Client | Check | Login and session | Dashboard | Settings save | Evidence |
| --- | --- | --- | --- | --- | --- |
| Chrome on macOS, headless | M2 | Pending | Pending | Pending | — |
| Chrome on macOS | M3 | Pending | Pending | Pending | — |
| Safari on macOS | M3, with approval | Pending | Pending | Pending | — |

## Interfaces

The M1.5 device check of [design 0008][dc] runs on the user's Mac with the
user present. Rows record outcomes only: no MAC address, IP address,
network name, or serial number. Step numbers refer to the check.

| Check | Steps | Built-in Wi-Fi | Ethernet adapters | Phones over USB | Evidence |
| --- | --- | --- | --- | --- | --- |
| Kind, media, and port name shown by `cuttle interfaces` | 1 | Pending | Pending | Pending | — |
| Distinct BSD names and MACs for devices of one kind | 1 | Not applicable | Pending | Pending | — |
| MAC and BSD name kept across replugging, port change, and reboot | 2, 3 | Not applicable | Pending | Pending | — |
| Same-model replacement takes over a BSD name; the MAC match stays `absent` | 4 | Not applicable | Optional | Not run | — |
| Notices and time to offline on cable pull, detach, and sharing off | 6 | Pending | Pending | Pending | — |
| Flow leaves through the chosen interface | 7 | Pending | Pending | Pending | — |
| `link_down` skip and pinned `503` | 8, 9 | Not applicable | Pending | Pending | — |
| Phone without cellular data: failover reason and demotion | 10 | Not applicable | Not applicable | Pending | — |

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

[cc]: design/0007-test-environment-and-gates.md#d8-client-checks
[dc]: design/0008-interface-selection.md#device-check-i18
[nd]: design/0003-upstream-chaining-and-exposure.md#d5-non-disclosure-toward-an-egress-proxy
[oi-2]: OPEN_ISSUES.md#oi-0002--safari-and-cfnetwork-behavior-unverified

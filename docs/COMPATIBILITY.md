# Client compatibility

For each recorded check, include the client and OS versions, proxy
configuration, origin fixture, tested commit, and a link to evidence. Verify
both the client outcome and the selected route. Record the client-visible error
for a blocked request and verify that it produced no outbound connection.
Account for client bypass settings and confirm that `squid` receives the
expected `CONNECT` for each test case.

| Client | Proxy configuration path | Direct | Upstream | Block | Evidence |
| --- | --- | --- | --- | --- | --- |
| Chrome on macOS | `--proxy-server` with an HTTP proxy address | Pending | Pending | Pending | — |
| Safari on macOS | macOS HTTPS traffic proxy setting | Pending | Pending | Pending | — |
| `curl` | `--proxy http://127.0.0.1:<port>` | Pending | Pending | Pending | — |

Configuration references. [Chromium proxy support](https://chromium.googlesource.com/chromium/src/+/HEAD/net/docs/proxy.md#manual-proxy-settings), [macOS proxy settings](https://support.apple.com/en-am/guide/mac-help/mchlp2591/27/mac/27), [curl `--proxy`](https://curl.se/docs/manpage.html#-x).

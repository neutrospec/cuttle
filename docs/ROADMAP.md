# Roadmap

Each milestone lists observable outcomes. The detailed acceptance criteria
and their checks are in the [design records](design/README.md), and the
gate tiers are defined in [AGENTS.md](../AGENTS.md#evidence-by-git-tree) and
[design 0007](design/0007-test-environment-and-gates.md#d7-gates). A
milestone closes when every stage has evidence on the same candidate tree
and an independent review is complete.

## M1 — Routing proxy core with the mock environment

cuttle routes `CONNECT` tunnels and plain `http://` requests to `direct`,
an upstream proxy, or `block`; protects a remote listener; reaches the
corporate proxy through a home → office chain without leaving a trace of
proxying; and reports live traffic. Acceptance uses T0 and T1 with Go,
raw-socket, and curl clients against the mocks, plus the T2 M1 client check
with headless Chrome and `/usr/bin/curl` against cuttle binaries and the
mocks ([design 0007 D8][d7-8]). No installation, no Safari, no system proxy
setting, and no keychain change.

Each stage uses only features of its own or earlier stages. Live counters
exist in memory from M1.1, and tests read them through the Go API; M1.4
exposes them through the control socket and proves exact byte accounting.

| Stage | Done when |
| --- | --- |
| M1.1 — Direct and block for local clients | The starter configuration is created, and `cuttle check`, `explain`, and `paths` work. `net/http` serves the proxy port. `CONNECT` and plain HTTP reach controlled origins on `direct` rules with certificates verified and the client's raw target kept, local clients reach any port, and `block` rules and defaults answer `403` without any lookup or dial. Direct routes refuse loopback, own, and private addresses reached by name unless a rule allows them, and always refuse link-local addresses. Targets are normalized, and CFNetwork's bracketless IPv6 form is a documented limitation. Each request on a reused connection is routed on its own. WebSocket works over `CONNECT` and absolute-form upgrades. Forwarded requests carry no trace field and no added `Accept-Encoding` or default `User-Agent`. Every decision is logged, and the log rotates. Criteria: 0002 C1–C11 and C14; 0003 N4; 0004 K1, K2, and K7; 0006 P1, P3, P6, and P7. |
| M1.2 — Exposure | Listeners accept only explicit addresses, and a non-loopback listener that cannot bind is retried. Sources are classified on accept: remote sources need the allowlist, get their own ports and default, and cannot go `direct` unless allowed. Direct connection pools are kept apart per client class. Connection limits apply. Criteria: 0002 C15; 0003 N1, N2, N3, and N6. |
| M1.3 — Upstreams and chaining | `trace = "via"` upstreams receive one cuttle `Via` entry; `trace = "none"` upstreams receive no trace of proxying, shown by the no-trace chain test. Any upstream 2xx yields exactly `200`; `CONNECT` failures map to `502`, `503`, or `504` with `Proxy-Status`, plain-HTTP responses pass through, and nothing falls back to `direct`. An unreachable upstream is held so later requests fail at once. Home → office → intercepting mock delivers a corporate-root leaf, and a home ↔ office loop stops after two `CONNECT` requests. Criteria: 0003 U1–U12. |
| M1.4 — Live operation | Live counters show exact byte deltas during and after a controlled transfer. `cuttle status` and `cuttle reload` work over the control socket. A reload keeps established tunnels; after it removes a source, that source's kept-alive connections close at their next request, while its established tunnels survive until they close or reach the idle timeout. A listener that cannot bind shows as retrying. `SIGTERM` drains and exits within 8 s. Criteria: 0002 C12–C13; 0003 N5 and N7; 0004 K3–K6; 0006 P2, P4, and P5. |

M1 also needs the test environment criteria 0007 E1–E7 and the M1 client
check: direct HTTPS and HTTP, block, upstream, WebSocket over `CONNECT` to
port 80, the home → office chain, and `mockenv check-trace` comparing
Chrome and curl requests sent directly to a mock and through home → office
with `trace = "none"`.

Changes from the suggested split, with reasons:

- The decision log and its rotation are in M1, not M3. Browsers show the
  same error for every failed tunnel, so the log is the only per-request
  explanation before the M2 dashboard, and an unrotated log would grow
  during M1 use.
- Reload is in M1. Hand edits are the only way to change settings before
  M2, and a restart drops tunnels. M2's settings page reuses the same
  reload.
- The M1 client check moves headless Chrome and curl coverage to M1 close,
  as the user approved, so M1 meets the AGENTS.md T2 trigger for milestone
  acceptance.
- Exposure comes before upstreams. The chain tests need remote source
  classes, the allowlist, and the remote default, so the stage that
  delivers them goes first.

## M2 — Statistics and admin

The operator sees which domains went to each upstream, which went direct,
and which were blocked over a chosen period, and edits the configuration as
TOML text in a password-protected page reachable only from the same Mac.
Acceptance uses T0, T1, and the T2 headless Chrome admin check.

| Stage | Done when |
| --- | --- |
| M2.1 — Statistics | Hourly per-host rows in SQLite match the traffic, writes never delay forwarding, retention and folding bound the size, and the proxy runs without the database. Criteria: 0005 S1–S7. |
| M2.2 — Admin access | Only `127.0.0.1` and `localhost` Host values reach the loopback admin listener; the password is set through the CLI and hashed with Argon2id; sessions, CSRF protection, and login backoff behave as specified. Criteria: 0005 A1–A7. |
| M2.3 — Dashboard | Domain tables per upstream, direct, and blocked over `1h`, `24h`, `7d`, `30d`, or a date range; live tunnels and upstream states. Criteria: 0005 A8 and A10. |
| M2.4 — Settings | The configuration is edited as text, validated, written atomically with conflict detection, and reloaded without dropping tunnels. Criteria: 0005 A9. |

## M3 — Install and deploy

cuttle installs from a Homebrew tap, runs under user-level
`brew services`, works in Chrome, Safari, and curl, and has a verified
home and office setup. Acceptance uses T0, T1, and full T2.

| Stage | Done when |
| --- | --- |
| M3.1 — Homebrew tap and service | `brew install`, `brew test`, and `brew audit` pass; `brew services` start, stop, and restart behave as designed, including `SIGTERM` handling, relaunch, the `.env` override, and log locations. Criteria: 0006 P8–P12. Depends on [OI-0003][oi-3] and [OI-0004][oi-4]. |
| M3.2 — Client verification | The [design 0007 D8][d7-8] M3 matrix is recorded in the [compatibility matrix](COMPATIBILITY.md) for Chrome, Safari, and `/usr/bin/curl`, with admin login in Chrome and Safari. Depends on [OI-0002][oi-2] and [OI-0005][oi-5]. |
| M3.3 — Home and office deployment | A deployment guide gives the [design 0003 D13][d3-13] configuration, both WireGuard cases, the trust steps and their consequences, the timeout budget, and verification commands. The two-instance chain is verified with `tools/mockenv`. The user confirms on the real corporate proxy that requests through office cuttle with `trace = "none"` work as office Chrome's direct requests do, or defers that with the user's consent. Depends on [OI-0001][oi-1]. |

## Later

- T3 baselines for tunnel throughput, `CONNECT` latency, memory, and open
  descriptors, including the `ProcessType` comparison, then numeric
  targets.
- Upstream failover lists, TLS connections to upstream proxies, and
  upstream authentication, if a need appears.
- Statistics grouped by registrable domain, and metrics export.
- Converting U-labels, and remote `direct` routes to private ranges by
  explicit rule.
- Closing established tunnels that a reload newly blocks.
- A PAC file, SOCKS, and Linux support.

## Non-goals

- **Response caching.** cuttle never terminates client TLS, so it cannot
  cache HTTPS content. Caching only plain `http://` would add storage,
  freshness, and privacy rules for a shared cache (`Vary`,
  `Cache-Control`) for little gain, because browsers keep their own caches.
  Caching HTTPS content would require TLS interception.
- **TLS interception** and filtering on URL paths, headers, or bodies.
- **Hiding anything beyond the request contents** from the corporate
  proxy, such as timing, connection reuse, the office Mac's address, or the
  client's own identity ([design 0003 D5][d3-5]).
- **Transparent proxying.** Clients configure cuttle explicitly.
- **Authentication on the proxy listener.** cuttle never answers `407`.
- **System-wide installation.** No `sudo brew services` and no
  LaunchDaemon.

[d3-5]: design/0003-upstream-chaining-and-exposure.md#d5-no-trace-toward-the-corporate-upstream
[d3-13]: design/0003-upstream-chaining-and-exposure.md#d13-home-office-and-corporate-deployment
[d7-8]: design/0007-test-environment-and-gates.md#d8-client-checks
[oi-1]: OPEN_ISSUES.md#oi-0001--real-corporate-proxy-and-office-vpn-behavior
[oi-2]: OPEN_ISSUES.md#oi-0002--safari-and-cfnetwork-behavior-unverified
[oi-3]: OPEN_ISSUES.md#oi-0003--homebrew-and-launchd-behavior-unverified
[oi-4]: OPEN_ISSUES.md#oi-0004--tap-hosting-repository-remote-and-licence
[oi-5]: OPEN_ISSUES.md#oi-0005--private-root-trust-through-the-chain

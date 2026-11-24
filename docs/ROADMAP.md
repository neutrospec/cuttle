# Roadmap

Each milestone lists observable outcomes. The detailed acceptance criteria
and their checks are in the [design records](design/README.md), and the
gate tiers are defined in [AGENTS.md](../AGENTS.md#evidence-by-git-tree) and
[design 0007](design/0007-test-environment-and-gates.md#d7-gates). A
milestone closes when every stage has evidence on the same candidate tree
and an independent review is complete.

## M1 — Routing proxy core with the mock environment

cuttle routes `CONNECT` tunnels and plain `http://` requests to `direct`,
an upstream proxy, or `block`, chains through other proxies, protects a
remote listener, and reports live traffic. Acceptance uses T0 and T1 with
Go, raw-socket, and curl clients against the mocks; no installation and no
browser.

| Stage | Done when |
| --- | --- |
| M1.1 — Direct and block | The starter configuration is created on first run, and `cuttle check`, `explain`, and `paths` work. `CONNECT` and plain HTTP reach controlled origins on `direct` rules with certificates verified, and `block` rules, the default, and the port policy answer `403` without any lookup or dial. Targets are normalized, including CFNetwork's bracketless IPv6. Each request on a reused connection is routed on its own. WebSocket works over `CONNECT` to port 80. Every decision is logged, and the log rotates. Criteria: 0002 C1–C10 and C13; 0004 K1, K2, and K7; 0006 P1, P3, P6, and P7. |
| M1.2 — Upstreams and chaining | Upstream `CONNECT` and absolute-form forwarding work, any upstream 2xx yields exactly `200`, and failures map to `502`, `503`, or `504` with `Proxy-Status` and no direct attempt. An unreachable upstream is held so later requests fail at once. Home → office → intercepting mock delivers a corporate-root leaf, and a home ↔ office loop stops after two `CONNECT` requests. Criteria: 0003 U1–U11. |
| M1.3 — Exposure | Listeners accept only explicit addresses; remote sources need the allowlist, get their own default, and cannot go `direct` unless allowed; direct routes refuse loopback, own, and link-local addresses reached by name; connection limits apply. Criteria: 0003 N1–N7. |
| M1.4 — Live operation | Live counters show exact byte deltas during and after a controlled transfer. `cuttle status` and `cuttle reload` work over the control socket; a reload keeps established tunnels; `SIGTERM` drains and exits within 8 s. Criteria: 0002 C11–C12; 0004 K3–K6; 0006 P2, P4, and P5. |

M1 also needs the test environment criteria 0007 E1–E5.

Changes from the suggested split, with reasons:

- The decision log and its rotation are in M1, not M3. Browsers show the
  same error for every failed tunnel, so the log is the only per-request
  explanation before the M2 dashboard, and an unrotated log would grow
  during M1 use.
- Reload is in M1. Hand edits are the only way to change settings before
  M2, and a restart drops tunnels. M2's settings page reuses the same
  reload.

## M2 — Statistics and admin

The operator sees which domains went to each upstream, which went direct,
and which were blocked over a chosen period, and edits the configuration
in a password-protected page reachable only from the same Mac. Acceptance
uses T0, T1, and T2-lite (headless Chrome).

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
| M3.1 — Homebrew tap and service | `brew install`, `brew test`, and `brew audit` pass; `brew services` start, stop, and restart behave as designed, including `SIGTERM` handling, relaunch, the `.env` override, and log locations. Criteria: 0006 P8–P12. Depends on [OI-0003](OPEN_ISSUES.md#oi-0003--homebrew-and-launchd-behavior-unverified) and [OI-0004](OPEN_ISSUES.md#oi-0004--tap-hosting-repository-remote-and-licence). |
| M3.2 — Client verification | The [design 0007 D8](design/0007-test-environment-and-gates.md#d8-t2-procedure) matrix is recorded in the [compatibility matrix](COMPATIBILITY.md) for Chrome, Safari, and `/usr/bin/curl`, with admin login in Chrome and Safari. Depends on [OI-0002](OPEN_ISSUES.md#oi-0002--safari-and-cfnetwork-behavior-unverified) and [OI-0005](OPEN_ISSUES.md#oi-0005--private-root-trust-through-the-chain). |
| M3.3 — Home and office deployment | A deployment guide gives the [design 0003 D11](design/0003-upstream-chaining-and-exposure.md#d11-home-office-and-corporate-deployment) configuration, the trust steps and their consequences, the timeout budget, and verification commands. The two-instance chain is verified with `tools/mockenv`, and the real corporate proxy and VPN checks are done by the user or deferred with the user's consent. Depends on [OI-0001](OPEN_ISSUES.md#oi-0001--real-corporate-proxy-and-office-vpn-behavior). |

## Later

- T3 baselines for tunnel throughput, `CONNECT` latency, memory, and open
  descriptors, including the `ProcessType` comparison, then numeric
  targets.
- Upstream failover lists, TLS connections to upstream proxies, and
  upstream authentication, if a need appears.
- Statistics grouped by registrable domain, and metrics export.
- Relaying absolute-form `Upgrade`, converting U-labels, and remote
  `direct` routes to private ranges by explicit rule.
- Closing established tunnels that a reload newly blocks.
- A PAC file, SOCKS, and Linux support.

## Non-goals

- **Response caching.** cuttle never terminates client TLS, so it cannot
  cache HTTPS content. Caching only plain `http://` would add storage,
  freshness, and privacy rules for a shared cache (`Vary`,
  `Cache-Control`) for little gain, because browsers keep their own caches.
  Caching HTTPS content would require TLS interception.
- **TLS interception** and filtering on URL paths, headers, or bodies.
- **Transparent proxying.** Clients configure cuttle explicitly.
- **Authentication on the proxy listener.** cuttle never answers `407`.
- **System-wide installation.** No `sudo brew services` and no
  LaunchDaemon.

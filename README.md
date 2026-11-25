# squid — the cuttle proxy

This repository builds `cuttle`, a small explicit forward proxy for macOS
clients such as Chrome, Safari, and command-line tools. The repository keeps
the name `squid`; the Homebrew formula, binary, service, and XDG directories
are named `cuttle`, because homebrew/core already ships a `squid` formula.

cuttle routes each HTTPS `CONNECT` tunnel and each plain `http://` request by
ordered rules on host name, domain, IP prefix, and port:

| Action | Behavior |
| --- | --- |
| `direct` | cuttle connects to the destination itself. |
| `upstream:<name>` | A configured HTTP proxy, possibly another cuttle, carries the request. A failed upstream never falls back to `direct`. |
| `block` | cuttle answers `403` without contacting the destination. |

- Tunnels stay end-to-end TLS; cuttle never decrypts traffic and caches
  nothing.
- Chains such as edge instance → gateway instance → egress proxy are
  supported, with loop detection and source and destination controls for
  listeners reachable over a private link. Requests toward an egress proxy
  disclose no intermediary.
- Where connection costs differ, cuttle can choose the network interface of
  each connection: named Wi-Fi, Ethernet, or tethered-phone interfaces in
  priority order, with per-rule lists that pin a destination to one of
  them.
- Live counters, a decision log, hourly per-domain statistics in SQLite, and
  a password-protected dashboard and settings page on loopback show where
  traffic went.
- cuttle installs from a Homebrew tap, runs under `brew services`, and
  follows the XDG Base Directory specification.

## Status

Milestone stages M1.1 to M1.5 are implemented: `direct`, `block`, and
`upstream:<name>` routes, the starter configuration, the decision log with
rotation, and `cuttle init`, `check`, `explain`, and `paths`, for local
clients and for remote clients on non-loopback listeners. Remote clients
need an address in `proxy.remote.sources` and get their own ports, default,
and connection limits
([0003 D9–D12](docs/design/0003-upstream-chaining-and-exposure.md#d9-listeners)).
Upstreams are plain HTTP proxies without authentication. Toward another
cuttle instance, `disclosure = "via"` adds one cuttle `Via` entry, which
stops loops between instances; toward an egress proxy,
`disclosure = "none"` sends requests that disclose no intermediary. An
upstream that cannot be reached is held for `unreachable_hold`, and
requests routed to it fail at once with `503`; nothing falls back to
`direct`
([0003 D1–D8](docs/design/0003-upstream-chaining-and-exposure.md#d1-upstreams-and-disclosure-modes)).
cuttle tells remote from local clients only by the peer address, so a
relay that hands remote connections to cuttle from loopback, such as an
`ssh -L` forward or a VPN client that delivers connections from loopback,
gives them a local client's full reach; such setups are unsupported
([0003 D13](docs/design/0003-upstream-chaining-and-exposure.md#d13-edge-gateway-and-egress-deployment)).
`cuttle status` shows the listeners, upstreams, live counters, and open
tunnels over a control socket under `XDG_RUNTIME_DIR` or `$TMPDIR`, and
`cuttle reload` or `SIGHUP` applies an edited configuration without
dropping tunnels
([0006 D4](docs/design/0006-packaging-paths-and-lifecycle.md#d4-control-socket),
[0004 D6](docs/design/0004-configuration-and-reload.md#d6-reload)). A
non-loopback listener that cannot bind, for example while a private link
is down, is retried every 30 s and shows as `retrying`. `SIGTERM` drains
in-flight requests for up to 5 s and closes tunnels. The dashboard and
the admin UI arrive in later stages of the [roadmap](docs/ROADMAP.md).

Stage M2.1 records hourly per-host statistics in
`~/.local/state/cuttle/stats.db`: requests, failures, and bytes per host,
route, rule, source, and interface, written every 10 s and at shutdown,
and kept for `stats.retention_days`
([0005 D1–D4](docs/design/0005-statistics-and-admin.md#d1-recorded-data)).
The proxy never waits for the database; when it cannot be opened,
`cuttle status` shows `stats  unavailable` with the reason and the proxy
serves without statistics.

Interface selection is implemented for stage M1.5
([design 0008](docs/design/0008-interface-selection.md)); its check on
real adapters and tethered phones is still to be run with the user
([OI-0008](docs/OPEN_ISSUES.md#oi-0008--interface-behavior-on-the-users-devices)).
The configuration names interfaces in `[[interface]]` tables by MAC
address or BSD name, which `cuttle interfaces` lists, and orders them in
`proxy.interfaces`; a `direct` rule or an upstream can carry its own
`interfaces` list. An interface that is not online is skipped, a fast
local connect failure or a 3 s attempt timeout moves to the next
interface of the list, and `cuttle status` shows each interface's state.
Two limits matter in practice. Name lookups
still go through the interface that macOS treats as primary, so set the
macOS network service order to match `proxy.interfaces`. When that primary
interface keeps its link but loses its internet connection, names fail to
resolve before cuttle can move to another interface
([0008 D13](docs/design/0008-interface-selection.md#d13-name-resolution)).
With `proxy.interfaces` or a rule list set, `direct` connections to public
addresses leave through the listed interface and bypass full-tunnel and
split-tunnel VPN routes; private addresses keep system routing
([0008 D7](docs/design/0008-interface-selection.md#d7-dial-plan)).

Try it from a checkout:

```sh
go build -o cuttle ./cmd/cuttle
./cuttle init                 # writes ~/.config/cuttle/config.toml
./cuttle check
./cuttle explain https://example.com/
./cuttle interfaces           # network interfaces and their MACs
./cuttle serve                # proxy on 127.0.0.1:3128; Ctrl-C stops it
curl -x http://127.0.0.1:3128 https://example.com/
./cuttle status               # in another terminal
./cuttle reload               # after editing the configuration
```

The log is `~/.local/state/cuttle/cuttle.log`; `cuttle paths` lists every
path. Go's server rejects CFNetwork's bracketless IPv6 `CONNECT` form before
cuttle sees it, a documented limitation
([0002 D2](docs/design/0002-proxy-core-and-routing.md#d2-front-end-on-nethttpserver)).

## Development

Building needs Go 1.26 or later; the gates use the Homebrew `go`. Run the
gate tiers of [AGENTS.md](AGENTS.md#evidence-by-git-tree) from the
repository root with `scripts/gate`, which runs exactly the commands of
[design 0007 D7](docs/design/0007-test-environment-and-gates.md#d7-gates):

```sh
scripts/gate t0   # gofmt, go mod tidy -diff, vet, build, unit tests,
                  # no tracked keys, cmd/cuttle free of test-only packages
                  # and golang.org/x/net, no cgo files in its packages,
                  # and a build without cgo
scripts/gate t1   # every test with -race, including integration and
                  # binary tests; needs /usr/bin/curl (or CUTTLE_CURL)
```

Tests use only loopback, generated certificates in temporary directories,
mock origins, and a test network instead of DNS
([design 0007](docs/design/0007-test-environment-and-gates.md)). They never
bind 3128 or 3129 or touch `~/.config/cuttle`. Record the macOS version,
`go version`, and `/usr/bin/curl --version` with gate evidence.

## Documentation

- [Roadmap and milestones](docs/ROADMAP.md)
- [Design records](docs/design/README.md), starting with
  [0002 — Proxy core and routing](docs/design/0002-proxy-core-and-routing.md)
- [Glossary](docs/GLOSSARY.md) of project terms
- [Development workflow and agent roles](AGENTS.md)
- [Documentation index](docs/README.md)

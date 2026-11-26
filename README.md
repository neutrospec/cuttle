# cuttle

`cuttle` is a small explicit forward proxy for macOS clients such as Chrome,
Safari, and command-line tools. It is released under the
[MIT licence](LICENSE).

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
  a password-protected admin UI on loopback show where traffic went. The
  admin UI also edits the whole configuration and shows each change for
  review before it is applied.
- cuttle installs from a Homebrew tap, runs under `brew services`, and
  follows the XDG Base Directory specification ([Install](#install)).

## Install

cuttle needs macOS. It installs from the `neutrospec/tap` Homebrew tap
and runs as a user-level LaunchAgent under `brew services`; no step needs
`sudo`. On Apple Silicon with macOS 15 or later, Homebrew pours a prebuilt
bottle, including on a macOS newer than any bottle. Elsewhere, such as on
Intel Macs, it builds cuttle from source with Homebrew's `go`.

```sh
brew install neutrospec/tap/cuttle
brew services start cuttle    # starts now and at every login
cuttle status                 # listeners, upstreams, counters, and paths
cuttle admin passwd           # then sign in at http://127.0.0.1:3129/
```

The first start writes the starter configuration to
`~/.config/cuttle/config.toml`, with the proxy on `127.0.0.1:3128` and the
admin UI on `http://127.0.0.1:3129/`; point clients at
`http://127.0.0.1:3128`. Edit the configuration on the admin settings
pages, or by hand followed by `cuttle check` and `cuttle reload`. Sign-in
stays disabled until `cuttle admin passwd` has set a password; a new
password needs no restart. `cuttle paths` lists every file the CLI uses.

The service sees no `XDG_*` variable from your shell. If your shell sets
`XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_STATE_HOME`, or `XDG_RUNTIME_DIR`
to a value other than the default, put the same `KEY=value` lines in
Homebrew's per-service environment file `cuttle.env`, which must not be
writable by group or others, and restart the service:

```sh
# $XDG_CONFIG_HOME/homebrew/services when XDG_CONFIG_HOME is set,
# otherwise ~/.homebrew/services
dir=${XDG_CONFIG_HOME:+$XDG_CONFIG_HOME/homebrew}
dir=${dir:-$HOME/.homebrew}/services
mkdir -p "$dir"
printf 'XDG_STATE_HOME=%s\n' "$XDG_STATE_HOME" >>"$dir/cuttle.env"
brew services restart cuttle
cuttle status                 # warns when the service's paths differ
```

Logs:

- `~/.local/state/cuttle/cuttle.log` is cuttle's own log, with the
  decision log; cuttle rotates it at 10 MiB and keeps five old files.
- `$(brew --prefix)/var/log/cuttle.log` receives what launchd captures:
  configuration errors at start, output from before cuttle's own log
  opens, and crashes. launchd restarts cuttle after a crash and, while the
  configuration is invalid, every 10 s until the file is fixed.

After an upgrade, check the configuration before restarting the service,
because a new version can refuse a file that the old one accepted; the
[changelog](CHANGELOG.md) lists such changes under "Upgrading":

```sh
brew upgrade cuttle
cuttle check                  # fix what it reports before restarting
brew services restart cuttle
```

To remove cuttle and everything it created:

```sh
brew services stop cuttle
brew uninstall cuttle
brew services cleanup
rm -rf ~/.config/cuttle ~/.local/share/cuttle ~/.local/state/cuttle
rm -f "$(brew --prefix)/var/log/cuttle.log"
# with non-default XDG_* values, their cuttle directories and cuttle.env
```

Stop the service before uninstalling: `brew uninstall` leaves it running
from the deleted files, and `brew services cleanup` then skips it. If that
happened, run `launchctl bootout gui/$(id -u)/sh.brew.cuttle` and then
`brew services cleanup`. Uninstalling alone also leaves the configuration,
password, statistics, and logs in place.

## Status

Milestones M1, M2, and M4 and stage M3.1 are implemented, with the
device check of M1.5 still open; Safari and the client matrix (M3.2) and
a verified chained deployment (M3.3) remain ([roadmap](docs/ROADMAP.md)).
The [changelog](CHANGELOG.md) lists the changes of each release.

Stages M1.1 to M1.5 provide `direct`, `block`, and `upstream:<name>`
routes, the starter configuration, the decision log with rotation, and
`cuttle init`, `check`, `explain`, and `paths`, for local clients and for
remote clients on non-loopback listeners. Remote clients
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
in-flight requests for up to 5 s and closes tunnels.

The admin UI covers stages M2.2 to M2.4 and milestone M4
([0005 D5–D10](docs/design/0005-statistics-and-admin.md#d5-admin-listener-and-host-allowlist),
[0009](docs/design/0009-web-configuration.md)).
`cuttle admin passwd` stores an Argon2id hash of the admin password in
`~/.local/share/cuttle/admin-password`, and `cuttle serve` answers on
`admin.listen`, `http://127.0.0.1:3129/` by default, with a sign-in page.
Once signed in, the overview says in one sentence where traffic went over
the last hour, 24 hours, 7 days, 30 days, or a date range, splits the
bytes by route in a bar, and lists the top hosts sent direct, through each
upstream, and blocked, with totals per interface, upstream and interface
states, a configuration summary, and the statistics status. The domain
table filters by route, rule, source, and interface, and the live page
lists open tunnels. "Route…" beside a host on these pages sends it direct,
through an upstream, or to block, with a new rule or by adding it to an
existing one.

The settings pages edit everything the configuration holds: rules, which
are added, edited, duplicated, deleted, and reordered, with "Test a URL"
showing the matching rule as `cuttle explain` does; upstreams with their
live state; interfaces defined from the ones detected on this Mac; the
proxy, remote-client, and general settings; and, on the Account page, the
admin password, changed with the current one. Every change opens a review
with the diff of the file, and nothing is written before Apply changes.
A save rewrites the whole file in canonical form with short generated
comments, so comments written by hand are not kept. It is written
atomically with the previous file kept as `config.toml.bak`, refused when
the file changed after the review, and reloaded without dropping tunnels.
The Advanced page edits the file as TOML text, validates it with line
numbers, applies it through the same review, and restores the previous
version. The pages are rendered on the server; one same-origin script
adds dialogs and other conveniences, and every page works without it.

Only the Host values `127.0.0.1:<port>` and `localhost:<port>` are
served. `cuttle status` shows whether the admin listener is listening,
unavailable with the bind error, or disabled. Sessions live in memory
and end after 30 idle minutes, after 12 hours, at logout, when the
password changes, and when cuttle restarts. After three failed sign-ins
in a row, each attempt must wait, from 1 s doubling up to 300 s.

Stage M2.1 records hourly per-host statistics in
`~/.local/state/cuttle/stats.db`: requests, failures, and bytes per host,
route, rule, source, and interface, written every 10 s and at shutdown,
and kept for `stats.retention_days`
([0005 D1–D4](docs/design/0005-statistics-and-admin.md#d1-recorded-data)).
The proxy never waits for the database; when it cannot be opened,
`cuttle status` shows `stats  unavailable` with the reason and the proxy
serves without statistics. A failed write is retried at the next flush,
and `cuttle status` and the dashboard show the last one with its time.

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

Try it from a checkout without installing:

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
./cuttle admin passwd         # then open http://127.0.0.1:3129/ and sign in
```

The log is `~/.local/state/cuttle/cuttle.log`; `cuttle paths` lists every
path. Go's server rejects CFNetwork's bracketless IPv6 `CONNECT` form before
cuttle sees it, a documented limitation
([0002 D2](docs/design/0002-proxy-core-and-routing.md#d2-front-end-on-nethttpserver)).

## Development

The module path is `github.com/neutrospec/cuttle`. Building needs Go 1.26
or later; the gates use the Homebrew `go`. A plain `go build` reports its
version as `dev+<revision>` (`.dirty` with uncommitted changes); release
builds set it with `-ldflags "-X main.version=..."`. The Homebrew formula's
source is [packaging/homebrew/cuttle.rb](packaging/homebrew/cuttle.rb),
which is copied into the tap at each release; the tap's CI builds the
bottles ([design 0006 D8](docs/design/0006-packaging-paths-and-lifecycle.md#release-procedure)
has the release procedure). Run the
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
scripts/admin-check  # T2: headless Chrome signs in to the admin UI of a
                     # sandboxed cuttle (0005 A10) and checks every admin
                     # page in both appearances and at phone width (0009)
```

Tests use only loopback, generated certificates in temporary directories,
mock origins, and a test network instead of DNS
([design 0007](docs/design/0007-test-environment-and-gates.md)). They never
bind 3128 or 3129 or touch `~/.config/cuttle`. Record the macOS version,
`go version`, and `/usr/bin/curl --version` with gate evidence.

The M1 client check (T2) runs headless Chrome and `/usr/bin/curl` against
cuttle binaries and the mocks of `tools/mockenv`, on loopback only, with
throwaway profiles and no system setting changed
([design 0007 D8](docs/design/0007-test-environment-and-gates.md#d8-client-checks)):

```sh
go run ./tools/mockenv m1-check                  # both clients, all cases
go run ./tools/mockenv m1-check -clients curl    # one client
go run ./tools/mockenv serve                     # the mocks alone, for manual checks
```

## Documentation

- [Changelog](CHANGELOG.md), with the upgrade steps of each release
- [Roadmap and milestones](docs/ROADMAP.md)
- [Design records](docs/design/README.md), starting with
  [0002 — Proxy core and routing](docs/design/0002-proxy-core-and-routing.md)
- [Glossary](docs/GLOSSARY.md) of project terms
- [Development workflow and agent roles](AGENTS.md)
- [Documentation index](docs/README.md)

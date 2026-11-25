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
- Live counters, a decision log, hourly per-domain statistics in SQLite, and
  a password-protected dashboard and settings page on loopback show where
  traffic went.
- cuttle installs from a Homebrew tap, runs under `brew services`, and
  follows the XDG Base Directory specification.

## Status

Milestone stage M1.1 is implemented: `direct` and `block` routes for local
clients on loopback listeners, the starter configuration, the decision log
with rotation, and `cuttle init`, `check`, `explain`, and `paths`. Upstream
routes, listeners for remote clients, `cuttle reload` and `status`, the
statistics, and the admin UI arrive in later stages of the
[roadmap](docs/ROADMAP.md); `cuttle serve` refuses a configuration that
needs them.

Try it from a checkout:

```sh
go build -o cuttle ./cmd/cuttle
./cuttle init                 # writes ~/.config/cuttle/config.toml
./cuttle check
./cuttle explain https://example.com/
./cuttle serve                # proxy on 127.0.0.1:3128; Ctrl-C stops it
curl -x http://127.0.0.1:3128 https://example.com/
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

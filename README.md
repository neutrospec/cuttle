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
- Chains such as home cuttle → office cuttle → corporate proxy are supported,
  with loop detection and source and destination controls for listeners
  reachable over a VPN. Toward a picky corporate proxy, cuttle forwards each
  request head byte for byte as the client sent it.
- Live counters, a decision log, hourly per-domain statistics in SQLite, and
  a password-protected dashboard and settings page on loopback show where
  traffic went.
- cuttle installs from a Homebrew tap, runs under `brew services`, and
  follows the XDG Base Directory specification.

The project is in the design stage. See:

- [Roadmap and milestones](docs/ROADMAP.md)
- [Design records](docs/design/README.md), starting with
  [0002 — Proxy core and routing](docs/design/0002-proxy-core-and-routing.md)
- [Development workflow and agent roles](AGENTS.md)
- [Documentation index](docs/README.md)

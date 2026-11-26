# Changelog

Notable changes to cuttle. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions
follow [Semantic Versioning](https://semver.org/).

## [0.2.0] - 2026-11-26

The whole configuration can now be edited in the admin UI, without
opening `config.toml` (roadmap milestone M4,
[design 0009](docs/design/0009-web-configuration.md)).

### Added

- Settings pages for rules, upstreams, interfaces, the proxy, remote
  clients, and general settings. Rules are added, edited, duplicated,
  deleted, and reordered; upstreams show their live state; interfaces are
  defined from the ones detected on this Mac.
- Review before apply: every change opens a review with the diff of the
  file, and nothing is written until Apply changes. Applying checks that
  the file did not change since the review, writes it atomically, keeps
  the previous file as `config.toml.bak`, and reloads without dropping
  tunnels.
- Restore: the Advanced page shows the previous version and restores it
  through the same review.
- The domains page filters by rule as well as by route, source, and
  interface, and each rule name links to its page.
- Route a host: "Route…" on the overview, domains, and live pages sends a
  host direct, through an upstream, or to block with a new rule or by
  adding it to an existing rule.
- Test a URL on the rules page and in the rule editor shows the matching
  rule and route, with the same logic as `cuttle explain`.
- `cuttle explain` prints an `address` line: for an IP literal on a direct
  route, whether the address is allowed or refused, as the dial would
  decide; for a name, which address classes are allowed after
  resolution.
- An optional `note` key on rules, upstreams, and interfaces: one line of
  up to 120 characters for the operator, shown in the admin UI.
- The account page changes the admin password with the current password.
  The first password is still set with `cuttle admin passwd`.

### Changed

- The overview opens with one sentence and a bar that show where traffic
  went, followed by the top hosts of each route.
- The admin UI has one visual system with light and dark themes and
  layouts for phone widths. A small same-origin script adds dialogs and
  other conveniences; every page also works without it.
- A save from a settings form rewrites the whole file in cuttle's
  canonical form with short generated comments. Comments written by hand
  are not kept; the previous file stays in `config.toml.bak`.
- The settings page is now the Advanced page. It still edits the file as
  TOML text, a save goes through the review, and the text is written as
  typed, so comments written there are kept.
- `new` is reserved as a rule, upstream, and interface name, because
  `/rules/new`, `/upstreams/new`, and `/interfaces/new` add one.

### Fixed

- When the directory sync after an atomic write fails, the file already
  holds the new content. cuttle now reports the configuration, the admin
  password, and the starter configuration as saved with a warning, and
  the admin UI reloads the saved configuration, instead of reporting a
  failure.

### Upgrading

- Run `cuttle check` after `brew upgrade cuttle` and before
  `brew services restart cuttle`. A configuration that names a rule,
  upstream, or interface `new` is now invalid, and `cuttle serve` refuses
  to start with it; `cuttle check` names the entry to rename.
- cuttle 0.1.0 refuses a file that contains a `note` key, as an unknown
  key. Before downgrading to 0.1.0, remove every `note` line from the
  configuration.

## [0.1.0] - 2026-11-26

The first release.

### Added

- An explicit forward proxy for macOS clients: HTTPS `CONNECT` tunnels,
  plain `http://` requests, and WebSocket upgrades. Tunnels stay
  end-to-end TLS; cuttle never decrypts traffic and caches nothing.
- Routing by ordered rules on host name, domain, IP prefix, and port, to
  `direct`, `upstream:<name>`, or `block`. Direct routes reach loopback
  addresses, and private addresses reached by name, only through a rule
  that allows them; they always refuse link-local addresses; remote
  clients reach only public addresses; and a failed upstream never falls
  back to `direct`.
- Upstream chaining through plain HTTP proxies, such as an edge instance,
  a gateway instance, and an egress proxy. `disclosure = "via"` adds one
  cuttle `Via` entry, which stops loops between instances;
  `disclosure = "none"` sends requests that disclose no intermediary. An
  unreachable upstream is held, so later requests fail at once with
  `503`.
- Listeners for remote clients over a private link, with a source
  allowlist, their own ports and default, and connection limits.
- Interface selection: named Wi-Fi, Ethernet, or tethered-phone
  interfaces in priority order, with per-rule and per-upstream lists and
  failover to the next interface.
- `cuttle init`, `check`, `explain`, `paths`, `interfaces`, `status`, and
  `reload`; reload keeps established tunnels, and `SIGTERM` drains
  in-flight requests.
- A decision log with rotation, live counters, and hourly per-host
  statistics in SQLite with retention.
- A password-protected admin UI on loopback: a dashboard of domains per
  route over a chosen window, live tunnels, and a settings page that edits
  the configuration as TOML text. The password is set with
  `cuttle admin passwd` and stored as an Argon2id hash.
- A Homebrew formula in the `neutrospec/tap` tap that runs cuttle as a
  user-level service under `brew services`, with paths that follow the
  XDG Base Directory specification.

[0.2.0]: https://github.com/neutrospec/cuttle/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/neutrospec/cuttle/releases/tag/v0.1.0

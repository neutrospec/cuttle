# 0004 — Configuration and reload

**Status** — Proposed (TASK-0001, 2026-11-25; revised in iterations 2
and 3).

## Context

Operators edit routing rules by hand and, from M2, as TOML text in the
admin settings page. Both paths must produce the same validated
configuration, and a change must apply without dropping existing tunnels.
This record defines the file format, the source of truth, the schema,
validation, the starter file, reload, and the atomic write procedure.

## Decisions

### D1. File and format

- Path: `$XDG_CONFIG_HOME/cuttle/config.toml`, by default
  `~/.config/cuttle/config.toml` ([0006 D1][d6-1]). Every command accepts
  `--config PATH` to override it.
- Format: TOML 1.0, parsed with `github.com/BurntSushi/toml` v1. The
  orchestrator approved this dependency in iteration 2.
- Durations are Go duration strings such as `"10s"`. The file is limited to
  256 KiB.

Why TOML and this library:

- Operators annotate rule groups, so the format needs comments. JSON has
  none.
- The library is pure Go under the MIT licence, reports line numbers in
  parse errors, and exposes undecoded keys, so cuttle can reject unknown
  keys and typos. The implementer records `go mod graph` in the first T0
  evidence to show what it adds.
- cuttle never encodes TOML. The settings page edits the file as text
  ([0005 D10][d5-10]), so comments and order survive edits.
- YAML brings a larger dependency and implicit typing. A custom line format
  needs its own parser, error reporting, and documentation.

### D2. Source of truth

The configuration file is the only source of truth. SQLite holds statistics
only.

- The proxy must run before any admin UI exists (M1) and with the admin
  listener disabled.
- Hand edits, dotfile repositories, diffs, and offline `cuttle check` work
  on a file.
- The statistics database is state that may be deleted
  ([0005 D3][d5-3]); deleting it must not lose routing.
- One writer procedure (D7) serves the CLI and the UI, and nothing has to
  keep two stores in step.

cuttle does not watch the file. A change applies on reload (D6).

### D3. Schema

Unknown keys are errors. "Restart" marks keys that apply only at the next
start. M1 validates `[admin]` and `[stats]` but serves no admin UI and
writes no statistics until M2.

Top level:

| Key | Type | Default | Applies | Meaning |
| --- | --- | --- | --- | --- |
| `name` | string | required | reload | Instance pseudonym for `Via` and logs, `^[a-z][a-z0-9-]{0,62}$` ([0003 D2][d3-2]) |

`[proxy]`:

| Key | Type | Default | Applies | Meaning |
| --- | --- | --- | --- | --- |
| `listen` | array of `IP:port` | `["127.0.0.1:3128"]` | restart | Proxy listeners ([0003 D9][d3-9]) |
| `default` | action | required | reload | Action for local clients when no rule matches |
| `connect_timeout` | duration | `"10s"` | reload | Direct name lookup and TCP connect together, 100 ms–10 min |
| `response_timeout` | duration | `"60s"` | reload | Direct plain-HTTP response head, 100 ms–10 min |
| `idle_timeout` | duration | `"1h"` | reload | Tunnel idle limit; `"0s"` disables |
| `max_connections` | integer | `1024` | reload | Client connection cap, 16–65536 |

`[proxy.remote]`:

| Key | Type | Default | Applies | Meaning |
| --- | --- | --- | --- | --- |
| `sources` | array of CIDR | `[]` | reload | Allowed remote sources; required for a non-loopback listener |
| `ports` | array of integers | `[80, 443]` | reload | Destination ports open to remote clients ([0003 D10][d3-10]) |
| `default` | action | `"block"` | reload | Action for remote clients when no rule matches |
| `allow_direct` | boolean | `false` | reload | Allow `direct` for remote clients |
| `max_connections_per_source` | integer | `128` | reload | Per-source cap, 1–65536 |

Local clients have no port policy.

`[[upstream]]`, zero or more:

| Key | Type | Default | Applies | Meaning |
| --- | --- | --- | --- | --- |
| `name` | string | required | reload | `^[a-z][a-z0-9-]{0,31}$`, unique |
| `address` | `host:port` | required | reload | Plain-HTTP proxy address |
| `disclosure` | string | required | reload | `via` for another cuttle instance; `none` for an egress proxy or any proxy that must not see an intermediary ([0003 D1][d3-1]) |
| `connect_timeout` | duration | `"10s"` | reload | 100 ms–10 min |
| `response_timeout` | duration | `"60s"` | reload | Response head for `CONNECT` and plain HTTP, 100 ms–10 min |
| `unreachable_hold` | duration | `"5s"` | reload | Fail fast after a connect failure, 100 ms–5 min; `"0s"` disables ([0003 D8][d3-8]) |

`[[rule]]`, zero or more, evaluated in file order:

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `name` | string | `rule-<n>` (1-based) | Shown in logs, statistics, and errors; `^[a-z0-9][a-z0-9._-]{0,63}$`, unique |
| `hosts` | array of names | `[]` | Exact names ([0002 D4][d2-4]) |
| `domains` | array of names | `[]` | Names and their subdomains |
| `cidrs` | array of CIDR | `[]` | IP-literal targets |
| `ports` | array of integers | `[]` | Port condition; empty means any port |
| `action` | action | required | `direct`, `block`, or `upstream:<name>` |
| `allow_loopback` | boolean | `false` | `direct` only: local clients may reach loopback addresses, and names that resolve to this Mac's own addresses ([0003 D11][d3-11]) |
| `allow_private` | boolean | `false` | `direct` only: local clients may reach private addresses that a name resolves to ([0003 D11][d3-11]) |

All rule keys apply on reload.

`[admin]` (M2):

| Key | Type | Default | Applies | Meaning |
| --- | --- | --- | --- | --- |
| `listen` | `IP:port` | `"127.0.0.1:3129"` | restart | Loopback literal only; `""` disables the admin UI |

`[stats]` (M2):

| Key | Type | Default | Applies | Meaning |
| --- | --- | --- | --- | --- |
| `retention_days` | integer | `30` | reload | 1–90 ([0005 D3][d5-3]) |
| `max_hosts_per_hour` | integer | `1000` | reload | 100–100000 |

`[log]`:

| Key | Type | Default | Applies | Meaning |
| --- | --- | --- | --- | --- |
| `level` | string | `"info"` | reload | `debug`, `info`, `warn`, or `error`; decisions log at `info` |

### D4. Validation

`cuttle check`, startup, reload, and the settings page run the same
validator. Each message names the key path and, where the parser reports
it, the line.

Errors:

- TOML syntax errors, unknown keys, wrong types, and values out of range.
- A missing `name`, `proxy.default`, rule `action`, or upstream
  `disclosure`, or a `disclosure` other than `via` or `none`.
- Duplicate upstream or rule names, or an action naming an unknown
  upstream. The rule names `default`, `remote-default`, `port`, and
  `remote-direct` are reserved for decisions that no rule made.
- A rule with none of `hosts`, `domains`, `cidrs`, or `ports`, judged by
  the entries the lists hold, and any of these keys set to an empty list,
  which would otherwise widen the rule. A catch-all belongs in `default`.
- A pattern that is not a lowercase ASCII name in the D3 syntax of
  [0002][d2-3], such as `*.example.com`, `.example.com`, a U-label, or an IP
  address in `hosts` or `domains`. The message suggests the correct field
  or the A-label form.
- An invalid CIDR, or a CIDR with host bits set.
- An IPv4-mapped CIDR, such as `::ffff:10.8.0.1/128` or `::ffff:0:0/96`, in
  `proxy.remote.sources` or a rule's `cidrs`. Peers and targets are
  compared in plain IPv4 form, so it would never match. The message
  suggests the IPv4 prefix, such as `10.8.0.1/32`, and an IP address given
  where a CIDR belongs gets the same plain form in its suggestion.
- `allow_loopback` or `allow_private` on a rule whose action is not
  `direct`.
- The exposure errors of [0003 D9][d3-9] and [D10][d3-10].
- An upstream address equal to a listener ([0003 D1][d3-1]).
- An `admin.listen` that is not a loopback literal, or whose effective
  value, the default `127.0.0.1:3129` included, equals a proxy listener.

Warnings, printed by `cuttle check` and the settings page:

- A later rule repeats a pattern of an earlier rule whose port condition
  covers the later rule's ports, because it has no `ports` or a superset.
  That entry of the later rule can never match.
- `proxy.remote.sources` contains `0.0.0.0/0` or `::/0`, which admits every
  address that can reach a non-loopback listener.
- An upstream that no action uses.

### D5. Starter configuration

When `cuttle serve` or `cuttle init` finds no configuration file, it
creates the directory with mode 0700 and writes this file with mode 0600
(D7). `<hex>` is 6 random hexadecimal digits.

```toml
# cuttle configuration. Check changes with `cuttle check` and apply them
# with `cuttle reload`. Rules are tried in order; the first match wins.

name = "cuttle-<hex>"  # unique per instance; appears in Via headers

[proxy]
listen = ["127.0.0.1:3128"]
default = "direct"  # direct, block, or upstream:<name>

[admin]
listen = "127.0.0.1:3129"  # set a password with `cuttle admin passwd`

# localhost and loopback addresses, on any port. Other names that resolve
# to loopback or to this Mac's own addresses stay refused.
[[rule]]
name = "loopback"
hosts = ["localhost"]
cidrs = ["127.0.0.0/8", "::1/128"]
action = "direct"
allow_loopback = true

# Devices on the home network by name, such as a NAS. Private IP literals
# and this Mac's own address work without a rule; names that resolve to
# private addresses need one.
# [[rule]]
# name = "home-lan"
# domains = ["lan", "local", "home.arpa"]
# action = "direct"
# allow_private = true

# Another cuttle instance as an upstream:
# [[upstream]]
# name = "gateway"
# address = "10.8.0.6:3128"
# disclosure = "via"  # "none" for an egress proxy
#
# [[rule]]
# name = "restricted"
# domains = ["internal.example"]
# action = "upstream:gateway"
```

With the starter default `direct` on a loopback listener, a local client
has the reach it has without a proxy, on any port, with three exceptions
([0003 D11][d3-11]):

- Loopback literals need a rule with `allow_loopback`; the starter
  `loopback` rule provides it.
- Names that resolve to loopback, to this Mac's own addresses, or to
  private addresses need a rule with `allow_loopback` or `allow_private`.
- Unspecified, link-local, multicast, and reserved addresses are always
  refused.

Tests never serve this file unchanged: they replace `proxy.listen` with a
port they chose and set `admin.listen` to `""` or another port they chose
([0007 D4][d7-4]).

### D6. Reload

Triggers: `SIGHUP`, `cuttle reload` through the control socket
([0006 D4][d6-4]), and saving on the settings page
([0005 D10][d5-10]).

1. Read and validate the file, and refuse it as startup does when it is
   writable by group or others or owned by another user
   ([0006 D2][d6-2]). On failure, keep the running configuration, log the
   errors, and return them to the caller. `cuttle status` shows them as
   `last_reload_error` until a reload succeeds.
2. Build a new immutable policy snapshot: compiled rules, remote settings,
   upstream table, timeouts, and name.
3. Keep the transport and reachability state of each upstream whose
   settings, `address`, `disclosure`, the timeouts, and
   `unreachable_hold`, are unchanged under the same name. Create new ones
   for the rest, close the idle connections of the replaced transports,
   and create new direct transports for the snapshot
   ([0003 D8][d3-8]).
4. Swap the snapshot pointer atomically. Apply `log.level` and `[stats]`
   settings.
5. Record which restart-only keys, `proxy.listen` and `admin.listen`,
   differ from the values the instance started with. `cuttle status`,
   `cuttle reload`, and the settings page list them as
   `restart_required`.

In-flight work:

- A request already decided keeps its snapshot. Established tunnels are
  never re-evaluated or closed by a reload, even when the new rules would
  block them; they end normally or on idle timeout.
- The next request on a kept-alive plain-HTTP connection uses the new
  snapshot, and its source is checked again ([0003 D10][d3-10]). Pooled
  outbound connections are reused only within their snapshot's transports
  ([0002 D5][d2-5]), except that an upstream kept in step 3 keeps its
  transport and pool.

### D7. Atomic write

`cuttle init`, first start, the settings page, and `cuttle admin passwd`
write files this way:

1. Resolve symbolic links in the target path. All later steps work in the
   resolved target's directory, so a dotfile manager's link stays intact,
   and the temporary file and `.bak` sit beside the link's target, not
   beside the link.
2. If the caller supplies the SHA-256 of the content it started from,
   compare it with the current file and stop with a conflict on mismatch.
   A narrow race remains between this check and the rename; it is accepted
   for a single local user.
3. Create `.<base>.tmp-<random>` in that directory with `O_EXCL` and mode
   0600, write, `fsync`, and close.
4. For the configuration file, write the previous content to `<base>.bak`
   in that directory the same way, then rename it into place.
5. Rename the temporary file over the target, then `fsync` the directory.
6. On any failure, remove the temporary file and leave the target
   unchanged.

The target always exists, holding the old or the new content.

## Acceptance criteria

| ID | Criterion | Check |
| --- | --- | --- |
| K1 | Each D4 error has a sample file that fails with the documented key path and line, and each warning appears in `cuttle check` output with exit status 0. | T0 `TestConfigValidation`, `TestCheckCommand` |
| K2 | With `HOME` and the `XDG_*` variables set to temporary directories, `cuttle init` creates `~/.config/cuttle` (0700) and the D5 file (0600), which passes `cuttle check` and has a `cuttle-<hex>` name. `cuttle serve`, given that file with `proxy.listen` changed to a port the test chose and `admin.listen` set to `""`, serves on that port. A unit test shows that `serve` writes the same starter file when none exists. No test binds 3128 or 3129. | T1 `TestFirstRun`; T0 `TestServeWritesStarter` |
| K3 | A tunnel opened before a reload keeps transferring after it. A request after the reload follows the new rules. | T1 `TestReloadKeepsTunnels` |
| K4 | Reloading an invalid file keeps the previous policy, logs the errors, and returns them through `cuttle reload` with a non-zero exit status. `SIGHUP` behaves the same apart from the return path. | T1 `TestReloadInvalid`, `TestReloadSIGHUP` |
| K5 | Changing `proxy.listen` and reloading leaves the listeners unchanged and lists `proxy.listen` under `restart_required`. | T1 `TestReloadRestartRequired` |
| K6 | Changing an upstream's address and reloading lets an existing tunnel through the old address continue while new requests use the new one. | T1 `TestReloadUpstreamChange` |
| K7 | Atomic write: a concurrent reader never sees partial content; a symlinked configuration keeps its link, gets a new target, and has its temporary file and `.bak` beside the target; `.bak` holds the previous content; a stale SHA-256 leaves the file unchanged. | T0 `TestAtomicWrite` |

## Alternatives considered

- **Configuration in SQLite, edited only through the UI.** It hides the
  configuration from text tools and ties routing to a state file.
- **File watching with `fsnotify`.** It adds a dependency and applies
  half-saved editor states; an explicit reload is predictable.
- **Structured settings forms that rewrite the file.** They would lose
  comments without a comment-preserving TOML encoder. The user chose text
  editing.
- **A global destination port list for every client.** It made
  `curl http://localhost:3000` fail and gave local clients less reach than
  no proxy; ports are limited for remote clients only.

[d2-3]: 0002-proxy-core-and-routing.md#d3-authority-normalization
[d2-4]: 0002-proxy-core-and-routing.md#d4-rules-and-evaluation-order
[d2-5]: 0002-proxy-core-and-routing.md#d5-actions-and-forwarding
[d3-1]: 0003-upstream-chaining-and-exposure.md#d1-upstreams-and-disclosure-modes
[d3-2]: 0003-upstream-chaining-and-exposure.md#d2-via-entries-and-loop-detection
[d3-8]: 0003-upstream-chaining-and-exposure.md#d8-unreachable-upstream-hold
[d3-9]: 0003-upstream-chaining-and-exposure.md#d9-listeners
[d3-10]: 0003-upstream-chaining-and-exposure.md#d10-source-classes-and-remote-policy
[d3-11]: 0003-upstream-chaining-and-exposure.md#d11-destination-address-policy-for-direct-routes
[d5-3]: 0005-statistics-and-admin.md#d3-retention-and-size
[d5-10]: 0005-statistics-and-admin.md#d10-settings
[d6-1]: 0006-packaging-paths-and-lifecycle.md#d1-paths
[d6-2]: 0006-packaging-paths-and-lifecycle.md#d2-permissions
[d6-4]: 0006-packaging-paths-and-lifecycle.md#d4-control-socket
[d7-4]: 0007-test-environment-and-gates.md#d4-instances-chains-and-loops

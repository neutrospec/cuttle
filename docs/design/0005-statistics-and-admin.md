# 0005 — Statistics and admin

**Status** — Proposed (TASK-0001, 2026-11-25; revised in iteration 2).

## Context

The operator wants a dashboard of traffic over a chosen period: which
domains went to each upstream proxy, which went direct, and which were
blocked. The dashboard and the settings page require a password and are
reachable from the same Mac only. Storage stays at SQLite or lighter.

This record defines the statistics data, schema, write path, retention, the
admin listener, authentication, sessions, CSRF protection, the dashboard,
and settings management. Live counters are defined in
[0002 D7][d2-7] and configuration editing rules in
[0004](0004-configuration-and-reload.md).

## Decisions

### D1. Recorded data

cuttle aggregates per hour, host, action, upstream, rule, source class, and
interface.

| Field | Meaning |
| --- | --- |
| `host` | Normalized name or IP literal ([0002 D3][d2-3]); IPv6 without brackets; `*` for folded rows (D4) |
| `action`, `upstream` | Route taken; `upstream` is empty unless the action is `upstream` |
| `rule` | Rule name, or `default`, `remote-default`, `port`, `remote-direct` |
| `source` | `local` or `remote` ([0003 D10][d3-10]) |
| `interface` | The interface that carried the outbound connection: a configured interface name, `other`, or empty without one, with the decision-log value of [0008 D15][d8-15] |
| `connects` | `CONNECT` decisions |
| `http_requests` | Plain-HTTP decisions |
| `failures` | Decisions with outcome `loop`, `dns_error`, `prohibited`, `refused`, `timeout`, `unavailable`, `no_interface`, `upstream_status`, `upstream_protocol`, or `internal` |
| `bytes_up`, `bytes_down` | Bytes per [0002 D7][d2-7] |
| `first_seen`, `last_seen` | Unix seconds of the first and last decision or byte transfer in the hour |

Not recorded: malformed requests and rejected connections, which have no
host and appear only in live counters and the log; ports; client
addresses; paths and other request content. The database is a host-level
browsing history for the retention period and is kept with mode 0600.

### D2. Schema

The database is `$XDG_STATE_HOME/cuttle/stats.db`
([0006 D1][d6-1]), opened with modernc.org/sqlite
([research 0001 §6][r1-6]).

```sql
PRAGMA journal_mode = WAL;     -- persistent; set when the file is created
PRAGMA auto_vacuum = INCREMENTAL;  -- set before the first table
PRAGMA synchronous = NORMAL;   -- per connection
PRAGMA busy_timeout = 5000;    -- per connection
PRAGMA user_version = 1;       -- schema version

CREATE TABLE host_hour (
  bucket        INTEGER NOT NULL,  -- start of the UTC hour, Unix seconds
  host          TEXT    NOT NULL,
  action        TEXT    NOT NULL CHECK (action IN ('direct', 'upstream', 'block')),
  upstream      TEXT    NOT NULL DEFAULT '',
  rule          TEXT    NOT NULL,
  source        TEXT    NOT NULL CHECK (source IN ('local', 'remote')),
  interface     TEXT    NOT NULL DEFAULT '',
  connects      INTEGER NOT NULL DEFAULT 0,
  http_requests INTEGER NOT NULL DEFAULT 0,
  failures      INTEGER NOT NULL DEFAULT 0,
  bytes_up      INTEGER NOT NULL DEFAULT 0,
  bytes_down    INTEGER NOT NULL DEFAULT 0,
  first_seen    INTEGER NOT NULL,
  last_seen     INTEGER NOT NULL,
  PRIMARY KEY (bucket, host, action, upstream, rule, source, interface)
) STRICT, WITHOUT ROWID;
```

Each flushed key is one upsert, with the values in column order:
`bucket`, `host`, `action`, `upstream`, `rule`, `source`, `interface`,
then the counts and times:

```sql
INSERT INTO host_hour VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (bucket, host, action, upstream, rule, source, interface)
DO UPDATE SET
  connects      = connects      + excluded.connects,
  http_requests = http_requests + excluded.http_requests,
  failures      = failures      + excluded.failures,
  bytes_up      = bytes_up      + excluded.bytes_up,
  bytes_down    = bytes_down    + excluded.bytes_down,
  first_seen    = min(first_seen, excluded.first_seen),
  last_seen     = max(last_seen,  excluded.last_seen);
```

A dashboard window is a range scan on the primary key prefix:

```sql
SELECT host, sum(connects) + sum(http_requests), sum(failures),
       sum(bytes_up), sum(bytes_down), min(first_seen), max(last_seen)
FROM host_hour
WHERE bucket >= :from AND bucket < :to
  AND action = 'upstream' AND upstream = :name
GROUP BY host
ORDER BY sum(bytes_up) + sum(bytes_down) DESC
LIMIT 500 OFFSET :offset;
```

Migrations run at start inside a transaction and raise `user_version`. A
database with a newer `user_version` than the binary knows is left
untouched.

### D3. Retention and size

- At start and every hour, cuttle deletes rows with `bucket` older than
  `stats.retention_days` (default 30, range 1–90) in batches of 5,000 rows,
  then runs `PRAGMA incremental_vacuum(1000)`. A row is older when its
  `bucket` is before the start of the current hour minus the retention,
  so the oldest kept hour began exactly `retention_days` days before the
  current one.
- `stats.max_hosts_per_hour` (default 1,000) bounds distinct hosts per hour
  (D4).

Expected size, assuming about 110 bytes per row on disk (assumption B3),
which includes a short interface name:

| Case | Rows | Size |
| --- | --- | --- |
| Typical: 300 hosts an hour for 12 hours a day, 30 days | 108,000 | about 12 MB |
| Cap: 1,000 hosts every hour, 30 days | 720,000 | about 79 MB |
| Cap with 90-day retention | 2,160,000 | about 238 MB |

A host that used several interfaces in one hour, for example after a
failover ([0008 D8][d8-8]), has one row per interface, so the typical case
can grow slightly; the cap bounds hosts, not rows.

The WAL file is checkpointed automatically and stays small at this write
rate.

### D4. Write path

- The forwarding path updates an in-memory aggregator under a short
  mutex: once when a tunnel, an upgraded connection, or a plain-HTTP
  exchange starts relaying, which registers it for sampling, and once
  when its decision completes. It never calls SQLite.
- Bytes use the counters behind the decision line and the live counters.
  On each flush, the flusher takes the deltas of every registered
  exchange and adds them to the current hour; a completing exchange adds
  its final delta. A long tunnel therefore spreads its bytes over the
  hours in which they moved, with 10-second resolution.
- A decision counts in the hour in which it completes, as its decision
  line does, so a tunnel open across an hour boundary has bytes in both
  hours and its `connects` in the later one. `first_seen` and
  `last_seen` are the times at which the aggregator recorded a
  completion or a flush found bytes.
- Every 10 s and at shutdown, the flusher swaps the map and writes all keys
  in one transaction over a single writer connection. The shutdown flush
  has 1 s ([0006 D6][d6-6]); a write that is still waiting for a lock then
  gives up, and its rows are lost.
- If a write fails, the batch is merged back and retried at the next flush,
  `stats_write_errors_total` increases, and an error is logged at most once
  a minute. `cuttle status` and the dashboard show the last failed write
  since start with its time and message, as `stats_last_write_error` and
  `stats_last_write_error_at`; a later successful flush leaves it in place
  ([OI-0006][oi] Q-R5).
- More than 20,000 pending keys, or a host beyond `max_hosts_per_hour` for
  its hour, folds into host `*` with the same hour, action, upstream, rule,
  source, and interface; `stats_folded_total` counts it. The host cap
  counts hosts seen since start, so after a restart within an hour it may
  admit up to twice the cap for that hour.
- If the database cannot be opened, migrated, or written at start, cuttle
  logs the reason and runs without statistics. `cuttle status` and the
  dashboard show `stats: unavailable` with the reason. Forwarding never
  depends on SQLite. The first retention run at start (D3) is the write
  check.
- Dashboard data lags traffic by at most one flush interval.

### D5. Admin listener and Host allowlist

- The admin UI is a `net/http.Server` with a `ServeMux` on `admin.listen`,
  which must be a loopback literal (default `127.0.0.1:3129`); `""`
  disables it ([0004 D3][d4-3]). Header read timeout 10 s, idle timeout
  60 s.
- Before anything else, every request's `Host` must be `127.0.0.1:<port>`
  or `localhost:<port>`, or `[::1]:<port>` when listening on `::1`, with
  the listen port and without regard to case. Anything else gets `403`.
  This defeats DNS rebinding, which Go's server does not check by default
  ([research 0002 §7][r2-7]).
- Remote proxy clients cannot reach the admin port: it listens on loopback,
  and they have no route to loopback ([0003 D11][d3-11]).
- Every response carries `Content-Security-Policy: default-src 'self';
  frame-ancestors 'none'; form-action 'self'; base-uri 'none'`,
  `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`, and
  `X-Content-Type-Options: nosniff`. Authenticated pages also carry
  `Cache-Control: no-store`.
- Pages are rendered with `html/template`. Styles are embedded with
  `embed`; the pages load nothing from other origins and need no
  JavaScript.

### D6. Password

- `cuttle admin passwd` is the only way to set the password. On a terminal
  it prompts twice without echo (`golang.org/x/term`); with `--stdin` it
  reads one line. Passwords are 12–128 bytes.
- It writes `$XDG_DATA_HOME/cuttle/admin-password` with the atomic write of
  [0004 D7][d4-7] and mode 0600. The file holds one PHC string:
  `$argon2id$v=19$m=19456,t=2,p=1$<salt>$<key>`, with a 16-byte random
  salt and a 32-byte key, both unpadded base64. The parameters are OWASP's
  minimum for Argon2id ([research 0002 §7][r2-7]).
- The service reads the file at each login attempt, so a new password
  needs no restart.
- If the file is missing, unreadable, not mode 0600, or owned by another
  user, login is disabled and `/login` shows the command to run. There is
  no default password.
- Verification compares in constant time. At most one Argon2id computation
  runs at a time, which bounds memory to about 19 MiB.

### D7. Sessions

- Sessions live in memory with 256-bit random IDs. A restart ends them,
  which removes the session-key file that
  [research 0001 §3][r1-3] proposed.
- Cookie `cuttle_session`: `HttpOnly`, `SameSite=Strict`, `Path=/`, no
  `Domain`, no `Max-Age`. It has no `Secure` attribute and no `__Host-`
  prefix, because the UI is plain HTTP on loopback and Safari's handling of
  both there is not established ([research 0002 §7][r2-7],
  [OI-0002][oi]).
- Login always issues a new ID. The idle timeout is 30 minutes and the
  absolute timeout 12 hours. Logout deletes the session. A request for
  `/live`, which reloads itself every 5 s (D9), does not restart the idle
  timeout, so a forgotten tab cannot keep a session alive.
- A session stores the SHA-256 of the password file at login. When the
  file changes, every session ends. The hash is cached by inode, size, and
  modification time.
- Browsers scope cookies by host, not port, so a browser sends the cookie
  to any other server on `127.0.0.1` it visits. The short idle timeout
  limits the exposure; the risk is accepted for a single-user Mac.
- Port squatting is the same exposure from the other side. A program that
  binds the admin port before cuttle does, for example while cuttle is
  stopped, receives the browser's requests to that address, with the
  session cookie of an earlier session and whatever the operator types
  into a look-alike sign-in form. cuttle cannot reserve the port. When
  its own bind fails it logs an error and serves the proxy without the
  admin UI, and `cuttle status` shows the admin listener as `unavailable`
  with the bind error, so the operator learns that something else answers
  on that port ([0006 D4][d6-4], [OI-0006][oi] P-R2). The risk is accepted
  for a single-user Mac.

### D8. CSRF and login limits

- State changes use `POST` only.
- `http.CrossOriginProtection` wraps the mux and rejects cross-origin
  non-safe requests by `Sec-Fetch-Site` or `Origin`
  ([research 0002 §7][r2-7]).
- Every form also carries the session's CSRF token, compared in constant
  time, so a request without either header still needs the token, as
  OWASP recommends.
- `GET /login` sets a 10-minute `cuttle_login` cookie and puts the same
  random value in the form. `POST /login` requires both to match, which
  stops login CSRF.
- `CrossOriginProtection` does not stop DNS rebinding, because the rebound
  `Origin` and `Host` match; the Host allowlist does (D5).
- A failed login answers `401` with "Invalid password." and nothing more.
  After n consecutive failures, with n of 3 or more, an attempt sooner than
  2^(n−3) seconds after the last failure gets `429` with `Retry-After`; the
  wait is capped at 300 s. Refused attempts run no Argon2id and do not
  extend the delay. Success resets the count. Each failure is logged
  without the password.

### D9. Dashboard

Windows are `1h`, `24h` (default), `7d`, and `30d`, or `from` and `to` as
local calendar dates, inclusive, within the retention period. A preset
covers whole hourly buckets, including the current one; the page shows the
exact range.

| Method | Path | Access | Purpose |
| --- | --- | --- | --- |
| `GET` | `/login` | Host check | Login form; sets `cuttle_login` |
| `POST` | `/login` | Host check, login token | Verify the password, start a session |
| `POST` | `/logout` | Session, CSRF | End the session |
| `GET` | `/` | Session | Overview: totals per route and per interface, top hosts, live counters, upstream and interface states, configuration and statistics status |
| `GET` | `/domains` | Session | Host table filtered by `route`, `source`, `interface`, and window |
| `GET` | `/live` | Session | Active tunnels; refreshes every 5 s with `<meta http-equiv="refresh">` |
| `GET` | `/settings` | Session | Configuration editor (D10) |
| `POST` | `/settings/validate` | Session, CSRF | Validate text without saving |
| `POST` | `/settings` | Session, CSRF | Validate, save, reload |
| `GET` | `/static/*` | Host check | Embedded styles |

`route` is `direct`, `upstream` (all upstreams), `upstream:<name>`,
`block`, or `all`. `source` is `local`, `remote`, or `all`. `interface` is
a configured interface name, `other`, `none` for rows without an
interface, or `all`. The filter selects list the running configuration's
upstreams and interfaces; a well-formed name that it no longer has, such
as an upstream a reload removed, still selects its rows. Any other value,
an unknown preset, an incomplete or reversed date range, a range wholly
outside the retention period, and an offset that is not a multiple of 500
get `400` with the reason.

A date range covers the hourly buckets that start within its local
dates, so in a time zone with a half-hour offset each day starts at the
next whole UTC hour. It is cut to the retention period and to the end of
the current hour, and the page shows the exact hours it covers.

The overview answers the operator's questions directly: domains sent to
each upstream, domains sent direct, and domains blocked. It shows the
totals per route, in the order `direct`, upstreams by name, `block`, the
totals per interface when the configuration defines interfaces, and the
ten hosts with the most bytes on each route, or the most requests on
`block`, with a link to that route's full table.
The domain table shows host, route, rules, requests (`connects` plus
`http_requests`), failures, bytes in each direction, and first and last
seen, ordered by bytes in both directions, then requests, then host, 500
rows a page. A host that took several routes appears once per route.
Bars drawn with CSS widths stand in for charts; the widths come from
classes in the embedded stylesheet in steps of 5 %, because the D5
`Content-Security-Policy` refuses inline styles.

The queries run on their own read-only SQLite connections, so a page
never waits for a flush that waits for a lock (D4).

The pages are rendered on the server; there is no JSON API and no
`/explain` page, because no consumer needs them. Operators use
`cuttle explain` and `cuttle status` from the terminal
([0006 D3][d6-3]), and tests exercise the query functions in
`internal/stats` directly.

### D10. Settings

- `/settings` shows the configuration path, its text in an editor, the
  SHA-256 of that text as `base`, when the running configuration was loaded,
  `restart_required` keys, the last reload result, and validator warnings.
- Browsers submit a text area's line breaks as CRLF; the page stores them
  as LF. The editor starts on a new line after its tag, so a leading empty
  line of the file survives the round trip.
- `POST /settings` checks the CSRF token and the 256 KiB size limit, which
  gets `413`, and that `base` is a lowercase hex SHA-256, which gets `400`
  otherwise, then:
  1. validates the text with the [0004 D4][d4-4] validator; on errors it
     answers `422` with the messages and keeps the submitted text, and the
     file is unchanged;
  2. writes it with the [0004 D7][d4-7] procedure and the `base` hash; on a
     conflict it answers `409` and shows both the file and the submitted
     text, with the file's current hash as the new `base`, so saving again
     deliberately replaces the other change;
  3. reloads ([0004 D6][d4-6]) and shows the result and any
     `restart_required` keys. If the reload refuses the file it was just
     given, for example because the file changed again in between, the page
     answers `422` with the errors, and the running configuration stays.
- One save at a time runs from the write through the reload.
- `POST /settings/validate` runs the same checks and the validator on the
  submitted text and never writes: `200` when the text is valid, `422`
  with the messages otherwise.
- The whole file is editable. Validation already refuses unsafe values,
  such as a non-loopback admin listener or a non-loopback proxy listener
  without sources. The password and statistics are not settings.
- If the file on disk is invalid, the page shows it with the errors, and
  the running configuration stays in force until a valid save.

## Acceptance criteria

T1 tests unless marked otherwise. Tests use an injected clock for hours and
timeouts.

| ID | Criterion | Check |
| --- | --- | --- |
| S1 | After a scripted mix of direct `CONNECT` to two hosts, one of them through a test interface of [0008][d8], plain HTTP through upstream `gateway`, and blocked requests, `host_hour` rows match the expected counts, bytes, and interfaces and agree with the live counters. | `TestStatsRowsMatchTraffic` |
| S2 | While another connection holds `BEGIN EXCLUSIVE` on the database, 100 proxied requests complete before the lock is released; the pending batch is written afterwards. | `TestStatsNeverBlocksForwarding` |
| S3 | A tunnel open across an hour boundary splits its bytes between the two hours within one flush interval of data. | `TestStatsHourAttribution` |
| S4 | Rows older than the retention are deleted; hosts beyond `max_hosts_per_hour` and keys beyond 20,000 pending fold into `*`. | `TestStatsRetentionAndFolding` |
| S5 | With an unopenable database or a newer `user_version`, the proxy serves and status reports `stats: unavailable`. | `TestStatsUnavailable` |
| S6 | 100,000 synthetic rows, each with an 8-byte interface name, take at most 160 bytes per row after a checkpoint; the test reports the figure. | `TestStatsRowSize` |
| S7 | Bytes of tunnels closed by shutdown are in the database after the process exits. | `TestStatsShutdownFlush` |
| A1 | `Host: evil.example:3129` gets `403` before authentication; `127.0.0.1:<port>` and `localhost:<port>` are served. | `TestAdminHostAllowlist` |
| A2 | `cuttle admin passwd --stdin` writes a 0600 Argon2id PHC string with `m=19456,t=2,p=1` and refuses an 11-byte password. Login with it sets `cuttle_session` with `HttpOnly` and `SameSite=Strict`; a wrong password gets `401` with the generic message. | `TestAdminPassword`, `TestAdminLogin` |
| A3 | Without a password file, `/login` names `cuttle admin passwd`, and `POST /login` fails without running Argon2id. | `TestAdminNoPassword` |
| A4 | After 3 consecutive failures, an immediate attempt gets `429` with `Retry-After: 1`; each further failure doubles the wait up to 300 s; a success resets the count. | `TestAdminLoginBackoff` |
| A5 | A `POST` with `Sec-Fetch-Site: cross-site`, without the CSRF token, or a login without the `cuttle_login` cookie gets `403`. | `TestAdminCSRF` |
| A6 | A session ends after 30 idle minutes, after `cuttle admin passwd` changes the file, and after logout. | `TestAdminSessionEnd` |
| A7 | Every admin response carries the D5 security headers. | `TestAdminHeaders` |
| A8 | After S1's traffic, the query for `upstream:gateway`, `direct`, and `block` over `24h` returns exactly the expected hosts, and `1h` excludes rows from earlier hours. `/domains` renders those rows, and `/live` lists an open tunnel. | `TestStatsDomainsQuery`, `TestAdminPages` |
| A9 | A valid save replaces the file (0600, `.bak` written), reloads, and an open tunnel survives; invalid text gets `422` with line numbers and leaves the file; a stale `base` gets `409` and leaves the file; a `proxy.listen` change shows `restart_required`. | `TestAdminSettings` |
| A10 | Headless Chrome with a throwaway profile and `--use-mock-keychain` logs in at `http://127.0.0.1:<port>/` and renders the overview. No window opens. | T2 at M2 close: `scripts/admin-check` runs `TestAdminChrome`, [0007 D7][d7-7] |

## Assumptions

| ID | Assumption | Smallest check |
| --- | --- | --- |
| B1 | Chrome and Safari keep a non-`Secure`, `SameSite=Strict` cookie for `http://127.0.0.1`. | A10 for Chrome; Safari login in M3 ([OI-0002][oi]) |
| B2 | Argon2id with these parameters takes under 200 ms on the target Macs. | T0 benchmark `BenchmarkPasswordHash`, reported in M2 evidence |
| B3 | A row takes about 110 bytes on disk, including the interface name. | S6 |

## Alternatives considered

- **One row per decision.** Size grows with traffic instead of with hosts
  and hours.
- **Grouping by registrable domain** with `golang.org/x/net/publicsuffix`.
  Useful for CDN-heavy sites; deferred until hosts prove too noisy.
- **Signed stateless cookies with a key file.** They survive restarts, but
  add a secret file; re-login after a restart is acceptable.
- **bcrypt or scrypt.** OWASP prefers Argon2id, and bcrypt truncates at 72
  bytes.
- **A JavaScript chart library or a Prometheus endpoint.** Not needed for
  the stated views; later topics.
- **Editing settings through forms that rewrite the file.** Rejected in
  [0004](0004-configuration-and-reload.md#alternatives-considered).

[d2-3]: 0002-proxy-core-and-routing.md#d3-authority-normalization
[d2-7]: 0002-proxy-core-and-routing.md#d7-byte-accounting-and-live-counters
[d3-10]: 0003-upstream-chaining-and-exposure.md#d10-source-classes-and-remote-policy
[d3-11]: 0003-upstream-chaining-and-exposure.md#d11-destination-address-policy-for-direct-routes
[d4-3]: 0004-configuration-and-reload.md#d3-schema
[d4-4]: 0004-configuration-and-reload.md#d4-validation
[d4-6]: 0004-configuration-and-reload.md#d6-reload
[d4-7]: 0004-configuration-and-reload.md#d7-atomic-write
[d6-1]: 0006-packaging-paths-and-lifecycle.md#d1-paths
[d6-4]: 0006-packaging-paths-and-lifecycle.md#d4-control-socket
[d6-6]: 0006-packaging-paths-and-lifecycle.md#d6-signals-and-shutdown
[d6-3]: 0006-packaging-paths-and-lifecycle.md#d3-command-line
[d7-7]: 0007-test-environment-and-gates.md#d7-gates
[d8]: 0008-interface-selection.md
[d8-8]: 0008-interface-selection.md#d8-failover
[d8-15]: 0008-interface-selection.md#d15-status-command-line-logging-counters-and-statistics
[oi]: ../OPEN_ISSUES.md
[r1-3]: ../research/0001-homebrew-launchd-xdg.md#3-xdg-base-directory-specification
[r1-6]: ../research/0001-homebrew-launchd-xdg.md#6-go-sqlite-drivers
[r2-7]: ../research/0002-proxy-chaining-and-interception.md#7-exposure-risks

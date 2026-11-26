# 0006 — Packaging, paths, and lifecycle

**Status** — Proposed (TASK-0001, 2026-11-25; revised in iteration 2).
D8 and the C2–C4 assumptions revised in TASK-0015 (M3.1) after checks with
the installed Homebrew.

## Context

cuttle installs from a personal Homebrew tap and runs as a user-level
`brew services` LaunchAgent. It follows the XDG Base Directory
specification. launchd passes no `XDG_*` variables, Go's directory helpers
return `~/Library` paths on darwin, and launchd neither rotates logs nor
waits long after `SIGTERM` by default ([research 0001][r1]). This record
defines paths, permissions, the CLI, the control socket, first run,
signals, logging, and the formula.

## Decisions

### D1. Paths

cuttle resolves each XDG variable itself: it uses the value when it is
non-empty and absolute, and otherwise the specification's `$HOME` default
([research 0001 §3][r1-3], [§4][r1-4]). `HOME` must be set. cuttle never
calls `os.UserConfigDir` or `os.UserCacheDir`.

| Item | Path | Mode |
| --- | --- | --- |
| Configuration | `$XDG_CONFIG_HOME/cuttle/config.toml`, plus `config.toml.bak` beside it, or beside the link's target when the path is a symbolic link ([0004 D7][d4-7]) | 0600 |
| Admin password hash | `$XDG_DATA_HOME/cuttle/admin-password` | 0600 |
| Statistics | `$XDG_STATE_HOME/cuttle/stats.db`, plus `-wal` and `-shm` | 0600 |
| Log | `$XDG_STATE_HOME/cuttle/cuttle.log`, plus `.1` to `.5` | 0600 |
| Control socket | `$XDG_RUNTIME_DIR/cuttle/control.sock`, else `$TMPDIR/cuttle/control.sock` | directory 0700 |
| launchd stdout and stderr | `$(brew --prefix)/var/log/cuttle.log` | set by launchd |

- `XDG_RUNTIME_DIR` is used only when it is absolute, owned by the user,
  and mode 0700, and when the socket path under it fits the Unix socket
  path limit: 103 bytes, because the 104-byte `sun_path` of a darwin
  `sockaddr_un` ends with a NUL. macOS does not set it; `$TMPDIR` is the
  per-user 0700 directory that the shell and the LaunchAgent share
  ([research 0001 §2][r1-2]). When the fallback is taken, the source
  names the reason, such as
  `TMPDIR; XDG_RUNTIME_DIR path too long (120 bytes, limit 103)`.
- When the `$TMPDIR` socket path is too long as well, the source adds
  `too long (N bytes, limit 103): no control socket`. `cuttle serve` then
  logs one error and runs without the socket, and `cuttle status` and
  `cuttle reload` exit 1 with the same cause before they dial.
- Missing directories are created with mode 0700. Existing directories keep
  their modes, as the specification requires.
- The service sees only default XDG paths unless the user copies non-default
  `XDG_*` values into `$HOMEBREW_USER_CONFIG_HOME/services/cuttle.env`,
  which Homebrew merges into the plist ([research 0001 §2][r1-2]). The
  service logs its paths at start, `cuttle status` reports them, and the
  CLI warns when they differ from its own.

`cuttle paths` prints the resolved paths and where each came from:

```text
config    /Users/me/.config/cuttle/config.toml        XDG_CONFIG_HOME unset
password  /Users/me/.local/share/cuttle/admin-password  missing
stats     /Users/me/.local/state/cuttle/stats.db      XDG_STATE_HOME unset
log       /Users/me/.local/state/cuttle/cuttle.log    XDG_STATE_HOME unset
control   /var/folders/<id>/T/cuttle/control.sock     TMPDIR
```

`--json` prints the same as an object.

### D2. Permissions

- `cuttle serve` sets umask 077. Files it creates are 0600 and directories
  0700.
- Startup fails if the configuration file is writable by group or others,
  or owned by another user, since that user could reroute traffic.
- The password file rules are in [0005 D6][d5-6].
- The control socket directory must be a directory, not a symbolic link,
  owned by the user with mode 0700. Otherwise cuttle warns and runs
  without the control socket. `cuttle status` and `cuttle reload` check
  the directory the same way before they dial and exit 1 otherwise, so
  another user's directory, such as a planted `/tmp/cuttle`, can neither
  answer a status request nor receive a reload.

### D3. Command line

| Command | Purpose |
| --- | --- |
| `cuttle serve [--config PATH]` | Run the proxy, admin UI, and control socket in the foreground |
| `cuttle init [--config PATH]` | Write the starter configuration if none exists ([0004 D5][d4-5]) and print its path |
| `cuttle check [--config PATH]` | Validate the configuration and print errors and warnings |
| `cuttle explain [--config PATH] [--remote] TARGET` | Show how the configuration routes `TARGET` |
| `cuttle reload` | Ask the running service to reload ([0004 D6][d4-6]) |
| `cuttle status [--json]` | Show the running service's listeners, upstreams, interfaces, counters, tunnels, and paths |
| `cuttle paths [--json]` | Print resolved paths (D1) |
| `cuttle interfaces [--json] [--all] [--watch] [--config PATH]` | List the system interfaces with their kind, MAC, media, addresses, and default routes, and how they match the configuration's `[[interface]]` definitions; `--watch` prints state changes ([0008 D15][d8-15]) |
| `cuttle admin passwd [--stdin]` | Set the admin password ([0005 D6][d5-6]) |
| `cuttle version`, `cuttle --version` | Print the version and Go version |

- Exit status 0 means success, 1 failure, and 2 a usage error. Warnings
  from `cuttle check` leave the status at 0. `cuttle check` reports a file
  that `cuttle serve` would refuse under D2, writable by group or others or
  owned by another user, as an error.
- `explain` reads the file, not the running service. `TARGET` is
  `host:port` for a tunnel, or an `http://` or `https://` URL, which maps to
  a plain-HTTP request or a tunnel to port 443. Example:

  ```text
  $ cuttle explain https://wiki.internal.example/
  target      wiki.internal.example:443 (connect, local client)
  rule        restricted (#2)
  action      upstream:gateway -> 10.8.0.6:3128 (disclosure via)
  interfaces  system routing
  ```

  The `interfaces` line names the interface list in force and its
  source: a rule, `proxy.interfaces`, or an upstream's own list, or
  `system routing` when none applies ([0008 D15][d8-15]).

  A direct route adds an `address` line, the destination address check
  of [0003 D11][d3-11] as far as the file decides it, without DNS
  ([0009 D8][d9-8]): for an IP literal, `allowed: <class> address` or
  `refused: <class> address, 502 destination_ip_prohibited`; for a name,
  the address classes that the dial accepts after resolution. The admin
  UI's Test a URL shows the same explanation, from `config.Explain`.

  ```text
  $ cuttle explain http://169.254.169.254/
  target      169.254.169.254:80 (http, local client)
  rule        default (proxy.default)
  action      direct
  interfaces  system routing
  address     refused: unusable address, 502 destination_ip_prohibited
  ```

### D4. Control socket

- An HTTP/1.1 server on a Unix stream socket, socket mode 0600 in a 0700
  directory. File system permissions are the access control; there is no
  password. The package is `internal/control`.
- At start, if the socket accepts a connection, another instance is running
  and `cuttle serve` exits with status 1 before it binds a listener. A
  stale socket is removed. If the socket cannot be created, or its path
  exists and is not a socket, cuttle logs a warning and runs without it.
  The socket closes, and its file is removed, at the first step of
  shutdown (D6).

| Method | Path | Response |
| --- | --- | --- |
| `GET` | `/v1/status` | Version, PID, start time, name, paths, listeners with state, the admin listener with state, upstreams with state, interfaces with state ([0008 D15][d8-15]), configuration state, counters ([0002 D7][d2-7]), up to 500 active tunnels, statistics state with the last failed write, log state with the rotation failure count, open-file limit |
| `POST` | `/v1/reload` | `200` with `restart_required` and warnings, or `422` with errors |

```json
{"version": "0.1.0", "name": "cuttle-edge", "pid": 4242,
 "started_at": "2026-11-25T00:00:00Z",
 "paths": {"config": {"path": "...", "source": "XDG_CONFIG_HOME unset"}},
 "listeners": [{"address": "127.0.0.1:3128", "state": "bound",
                "since": "2026-11-25T00:00:00Z"},
               {"address": "10.8.0.6:3128", "state": "retrying",
                "error": "listen tcp 10.8.0.6:3128: bind: can't assign requested address",
                "since": "2026-11-25T00:00:00Z"}],
 "admin": {"address": "127.0.0.1:3129", "state": "listening",
           "since": "2026-11-25T00:00:00Z"},
 "upstreams": [{"name": "gateway", "address": "10.8.0.6:3128",
                "state": "down", "since": "2026-11-25T00:58:00Z",
                "down_s": 42.3, "failed_fast": 17,
                "last_error": "connection_timeout"}],
 "interfaces": [{"name": "eth-a", "device": "en7", "state": "online",
                 "reason": "", "families": ["ipv4"],
                 "primary": ["ipv4"], "since": "2026-11-25T00:00:00Z",
                 "demoted_until": ""}],
 "config": {"path": "/Users/me/.config/cuttle/config.toml",
            "sha256": "9f2c...", "loaded_at": "2026-11-25T00:00:00Z",
            "restart_required": [], "last_reload_error": ""},
 "counters": {"tunnels_active": 3, "connections_active": 5},
 "tunnels": [{"target": "example.com:443", "kind": "connect",
              "action": "direct", "source": "local",
              "interface": "eth-a", "age_s": 42,
              "up": 3120, "down": 88000}],
 "stats": "enabled",
 "stats_last_write_error": "database is locked",
 "stats_last_write_error_at": "2026-11-25T00:41:10Z",
 "log": {"path": "/Users/me/.local/state/cuttle/cuttle.log",
         "rotation_failures": 0},
 "rlimit_nofile": 10240}
```

- `paths` equals the output of `cuttle paths --json` for the service's
  environment (D1). `counters` holds every live counter by its key, such
  as `bytes_total{action="direct",direction="up"}` ([0002 D7][d2-7]).
- An upstream's `down_s` is the time since the first failure of the
  current outage while it is `down` or `probing`, and 0 when it is `up`;
  `failed_fast` counts the requests answered at once during the current
  or last outage ([0003 D8][d3-8]). A tunnel's `kind` is `connect`, or
  `upgrade` for a connection upgraded after a `101`, and `upstream` names
  the upstream of an upstream route. A tunnel's `interface` is the
  decision-log value of [0008 D15][d8-15]: a configured interface name,
  `other`, or empty. `interfaces` lists the `[[interface]]` definitions in
  configuration order with their state, and is empty without
  definitions; it never contains a MAC address. `stats` is `enabled`,
  or `unavailable` when the database could not be opened, migrated, or
  written at start, with the reason in `stats_error`; the text form
  prints `stats  unavailable: <reason>` ([0005 D4][d5-4]).
  `stats_last_write_error` and `stats_last_write_error_at` hold the last
  failed flush since start and are absent while none has failed; the text
  form prints `last write error at <time>: <message>` under `stats`.
- `admin` is the admin listener of [0005 D5][d5-5] as the instance
  started it: `listening` on `address`, `unavailable` with the bind error
  in `error`, or `disabled` with an empty `address` when `admin.listen` is
  `""`. The text form prints `admin  <address> listening since <time>`,
  `admin  <address> unavailable: <error>`, or `admin  disabled`. A
  changed `admin.listen` applies at the next start and shows under
  `restart_required` meanwhile.
- `log.rotation_failures` counts the failed rotations and reopens since
  start (D7), so an operator sees a log that cannot rotate without reading
  the launchd file.
- `cuttle status` prints these fields as text, and `--json` prints the
  object. When a path the service reports differs from the one the command
  resolves, the command writes a warning per path to stderr and still
  exits 0.
- `cuttle reload` exits 0 after a reload and prints `restart required:`
  with the keys of `restart_required` and each warning. When the service
  refuses the file it prints each error to stderr and exits 1.
- Without a reachable socket, `cuttle status` and `cuttle reload` exit 1
  and name the socket path.
- `listeners` follows the order of `proxy.listen`, retrying listeners
  included.

### D5. First run

`cuttle serve`:

1. Resolves paths and sets umask 077.
2. Opens the log (D7).
3. Writes the starter configuration if none exists and logs its path.
4. Loads and validates the configuration. On errors it writes them to the
   log and stderr and exits with status 1. Under launchd it is relaunched
   about every 10 s and starts by itself once the file is fixed.
5. Opens the statistics database (M2); failure is not fatal
   ([0005 D4][d5-4]).
6. Opens the control socket; failure is not fatal.
7. Binds listeners ([0003 D9][d3-9]).
8. Logs the version, name, paths, and listeners. Without a password file it
   logs that admin login stays disabled until `cuttle admin passwd` runs.

### D6. Signals and shutdown

On `SIGTERM` or `SIGINT`:

1. Close the proxy, admin, and control listeners.
2. Close idle client connections and stop reading new requests.
3. Wait up to 5 s for in-flight plain-HTTP exchanges, and stop retrying
   listeners that have not bound ([0003 D9][d3-9]).
4. Close every tunnel, every connection upgraded after a `101`
   ([0002 D5][d2-5]), and every remaining connection with outcome
   `shutdown`. An exchange still in flight when the drain ends also logs
   `shutdown`. Then wait, for at most 1 s, until every request handler
   has returned, so every decision line is written before `stopped`.
5. Flush statistics within 1 s and close SQLite. The flush writes every
   pending row, including the final bytes and decisions of the tunnels
   that step 4 closed, so it cannot start before step 4 ends; a write
   still waiting for a lock when the second ends gives up, and its rows
   are lost ([0005 D4][d5-4]).
6. Log `stopped` and exit with status 0.

The steps take at most 5 s + 1 s + 1 s = 7 s, which leaves 1 s of P5's
8 s for closing the log and exiting, and stays below the formula's
`stop_timeout 15`; launchd's default of 5 s observed in research is too
short ([research 0001 §2][r1-2]). A flush of the 20,000 keys that
statistics hold at most before folding (0005 D4) took 72 ms in
`TestStatsRowSize` without the race detector, so 1 s is ample. A second `SIGTERM` or `SIGINT` exits at once
with status 1. `SIGHUP` reloads the configuration. A Go panic exits with
status 2 and writes to stderr, which launchd sends to
`$(brew --prefix)/var/log/cuttle.log`.

### D7. Logging and rotation

- `log/slog` text lines go to `$XDG_STATE_HOME/cuttle/cuttle.log`, and also
  to stderr when stderr is a terminal.
- cuttle rotates its own log: before a write would take the file past
  10 MiB, it renames `.4` to `.5` and so on down to the current file to
  `.1`, then opens a new file. Five rotated files bound the total at
  60 MiB. The writer is in-house and needs no dependency
  ([research 0001 §7][r1-7]).
- A failed rotation never stops logging. When a rename keeps failing, the
  writer keeps appending to the current file, which then grows past
  10 MiB, and tries again after each further 10 MiB. When the file cannot
  be reopened, that line is lost and the next write tries again. Every
  failure is counted, `cuttle status` shows the count as
  `log.rotation_failures` (D4), and the first failure of each run of
  failures is written to stderr, which under launchd is the launchd file.
  A current file deleted by someone else is not a failure: the next
  rotation opens a new one without shifting the rotated files. Until that
  rotation, lines go to the deleted file and are lost. A deleted log
  directory is recreated only at the next start: until then the rotation
  cannot open a new file, and each later line is lost and counted as a
  failure.
- The launchd file receives only output from before the log opens,
  configuration errors at start (D5 step 4), the first failure of each run
  of log failures, and runtime panics, so it stays small without rotation.
- Levels: `debug` adds tunnel-open and connection events; `info` has
  decisions, lifecycle events, and interfaces coming online or leaving
  demotion; `warn` has rejected sources and connection limits, retrying
  listeners, statistics write errors, interfaces going offline or demoted,
  and the interface identity warnings ([0008 D3][d8-3]);
  `error` has failures that stop a component. Rejected-source and limit
  warnings come at most once per source per minute and at most 60 per
  minute in all. One `warnings suppressed` line per minute then gives the
  `count` of rejected or limited connections that got no warning line and
  were not covered by a line of the same kind for the same source within
  the last minute, which also shows the size of a flood
  ([0003 D12][d3-12]).
- Privacy rules are in [0002 D8][d2-8].

### D8. Tap and formula

- The tap is the public GitHub repository `neutrospec/homebrew-tap`, tapped
  as `neutrospec/tap`, with `Formula/cuttle.rb`. The source repository is
  the public `neutrospec/cuttle`, MIT-licensed ([OI-0004][oi]). Since
  Homebrew 6, users install with the fully qualified name or trust the tap
  first ([research 0001 §1][r1-1]); `brew install neutrospec/tap/cuttle`
  also records the formula in the user's trust store, so later
  `brew services` and `brew test` commands need no separate trust step.
- The formula's source of truth is
  [packaging/homebrew/cuttle.rb](../../packaging/homebrew/cuttle.rb). At
  each release it is copied into the tap with `url` naming the new tag and
  `sha256` set to that tag tarball's checksum: a tag's tarball cannot
  contain its own checksum, so the repository copy holds a placeholder.
- Releases are tags `vX.Y.Z` with GitHub's source tarball. `head` tracks
  `main` for development.

```ruby
class Cuttle < Formula
  desc "Explicit HTTP and HTTPS forward proxy with rule-based routing"
  homepage "https://github.com/neutrospec/cuttle"
  url "https://github.com/neutrospec/cuttle/archive/refs/tags/v0.1.0.tar.gz"
  sha256 "<sha256 of that tarball>"
  license "MIT"
  head "https://github.com/neutrospec/cuttle.git", branch: "main"

  depends_on "go" => :build
  depends_on :macos

  allow_network_access! :test

  def fetch
    system "go", "mod", "download"
  end

  def install
    ENV["CGO_ENABLED"] = "0"
    system "go", "build", *std_go_args(ldflags: "-X main.version=#{version}"), "./cmd/cuttle"
  end

  def caveats
    # Paths, the admin URL, the cuttle.env override, stopping before
    # uninstalling, and the files uninstalling leaves; see the source file.
  end

  service do
    run [opt_bin/"cuttle", "serve"]
    keep_alive true
    process_type :interactive
    stop_timeout 15
    log_path var/"log/cuttle.log"
    error_log_path var/"log/cuttle.log"
  end

  test do
    # XDG_RUNTIME_DIR in testpath, a block-default config on free_port,
    # `cuttle check`, then under with_test_process a raw CONNECT over
    # TCPSocket must get "HTTP/1.1 403", and `cuttle version` must print
    # the formula version; see the source file.
  end
end
```

The Homebrew 7.0 API the formula uses, checked against the installed
Homebrew and in TASK-0015 with a throwaway tap ([OI-0003][oi]):

- `fetch` is a public formula method. It runs after unpacking, with
  network access, and `go mod download` fills Homebrew's package-manager
  cache (`GOPATH` and `GOCACHE` under `$(brew --cache)`). `install` then
  runs with all network denied and only those caches writable, so
  `go build` works offline from the downloaded modules; no `GOFLAGS` or
  `GOPROXY` setting is needed. The `go` directive must not exceed the
  Homebrew `go`, because Homebrew sets `GOTOOLCHAIN=local`.
- `std_go_args(ldflags: ...)` expands to `-trimpath -o=<bin>/cuttle
  -ldflags=-s -w -X main.version=<version>`.
- `CGO_ENABLED=0` makes the build independent of the C toolchain; cuttle
  has no cgo files ([0008 D16][d8-16]), and the built binary records
  `CGO_ENABLED=0`. modernc.org/sqlite is pure Go and builds this way.
- `depends_on :macos`, because cuttle does not build for Linux (interface
  binding is darwin-only).
- Network access is allowed in every phase unless the formula restricts
  it. `allow_network_access! :test` allows it for `test` and denies it for
  `build` and `postinstall`; `fetch` keeps network access regardless. The
  test needs it: with `deny_network_access! :test`, the sandbox refuses
  even Homebrew's own `free_port` (`Errno::EPERM` from `bind(2)`), so
  loopback TCP is network access to the test sandbox.
- `free_port` and `with_test_process` are public test helpers;
  `with_test_process` sends `SIGTERM` when the block ends and `SIGKILL`
  after 5 s. The test sandbox sets `HOME` and `XDG_CONFIG_HOME` inside
  `testpath` and `TMPDIR` to `HOMEBREW_TEMP`; the test sets
  `XDG_RUNTIME_DIR` to a 0700 directory in `testpath`, so the control
  socket does not land in the shared `$HOMEBREW_TEMP/cuttle`.
- The test sends the `CONNECT` itself over `TCPSocket` instead of running
  curl: it then depends on no curl version or proxy environment variable,
  and `brew audit --strict` rejects curl's `-w '%{http_connect}'` under
  RuboCop's `Style/FormatStringToken`.
- `service do` accepts `run`, `keep_alive`, `process_type`,
  `stop_timeout`, `log_path`, and `error_log_path`. `brew services start`
  writes `~/Library/LaunchAgents/sh.brew.cuttle.plist` with label
  `sh.brew.cuttle` and merges `cuttle.env` into `EnvironmentVariables`.
- The per-service environment file is
  `$HOMEBREW_USER_CONFIG_HOME/services/cuttle.env`:
  `$XDG_CONFIG_HOME/homebrew/services/cuttle.env` when the shell that runs
  `brew` sets `XDG_CONFIG_HOME`, otherwise `~/.homebrew/services/cuttle.env`.
  Homebrew skips a group- or world-writable file. It applies at the next
  `brew services start` or `restart`.

The service block decisions:

- No `--config` and no `XDG_*` in the block. cuttle resolves paths at run
  time, and the `.env` file covers custom values
  ([research 0001 §5][r1-5]).
- `keep_alive true` restarts after crashes and failed starts. A Go panic
  exits with status 2, not a signal, so `keep_alive crashed: true` would
  miss it. `brew services stop` uses `bootout`, which launchd does not undo.
  Observed: a clean exit after `SIGTERM` (`last exit code = 0`) and a
  `SIGKILL` are relaunched at once, and an invalid configuration exits 1
  and is relaunched every 10 s until the file is fixed.
- `stop_timeout 15` covers the D6 budget; `launchctl print` reports
  `exit timeout = 15`.
- `process_type :interactive`, because an unset `ProcessType` lets launchd
  throttle CPU and I/O, and cuttle carries interactive browser traffic
  ([research 0001 §2][r1-2]); `launchctl print` reports
  `spawn type = interactive`. Assumption C1.
- No `require_root`; `sudo brew services` is unsupported.

Uninstalling leaves the XDG files, the `var/log` file, and the
`cuttle.env` file. `brew uninstall` does not stop the service: the job
stays loaded and running from the deleted keg, its plist stays in
`~/Library/LaunchAgents`, and `brew services cleanup` skips it ("not
managed by `brew services`"), because it stops only running jobs whose
formula is still installed and removes only plists of jobs that are not
running. The caveats and the README therefore say to stop first:
`brew services stop cuttle`, `brew uninstall cuttle`,
`brew services cleanup`, then removal of the three XDG directories, the
log, and any `cuttle.env`. After an uninstall without a stop,
`launchctl bootout gui/$(id -u)/sh.brew.cuttle` stops the job and
`brew services cleanup` then removes its plist. `brew services start` also
runs `launchctl enable`, which leaves an `sh.brew.cuttle => enabled`
record in launchd's per-user override database; it is launchd's default
state, harmless, and removing it needs root.

## Acceptance criteria

| ID | Criterion | Check |
| --- | --- | --- |
| P1 | For each XDG variable unset, empty, relative, and absolute, the resolved paths match D1; the runtime directory falls back to `$TMPDIR` when `XDG_RUNTIME_DIR` is unset or not 0700. | T0 `TestPaths` |
| P2 | `cuttle paths --json` equals the `paths` of `/v1/status` for the same environment; with a different `XDG_STATE_HOME` the CLI prints the mismatch warning. | T1 `TestPathsMatchService` |
| P3 | After first run, every created directory is 0700 and every file 0600. A group-writable configuration stops `cuttle serve` with an error. | T1 `TestPermissions` |
| P4 | `cuttle status` and `cuttle reload` work over the socket; a second `cuttle serve` exits with status 1; a stale socket is replaced; a 0755 socket directory leaves the service running without a control socket and logs a warning. | T1 `TestControlSocket` |
| P5 | The built binary, sent `SIGTERM` with an open tunnel and an in-flight 2 s plain-HTTP response, completes the response, closes the tunnel, and exits 0 within 8 s; a second signal exits at once. | T1 `TestServeSIGTERM` |
| P6 | With a test threshold of 64 KiB, rotation keeps exactly five rotated files. | T1 `TestLogRotation` |
| P7 | `cuttle explain` prints the D3 fields for direct, upstream, block, and `--remote` targets, and `cuttle check` exits 0, 1, or 2 as specified. | T1 `TestExplainCommand`, `TestCheckCommand` |
| P8 | `brew install --build-from-source neutrospec/tap/cuttle`, `brew test cuttle`, and `brew audit --strict neutrospec/tap/cuttle` pass. | T2 at M3 |
| P9 | `brew services start cuttle` writes `~/Library/LaunchAgents/sh.brew.cuttle.plist` with `KeepAlive`, `ExitTimeOut` 15, `ProcessType` `Interactive`, and the log paths; the service serves on `127.0.0.1:3128`; `cuttle status` reaches it and its paths equal `cuttle paths`. | T2 at M3 |
| P10 | `brew services stop cuttle` logs `stopped` within 15 s without `SIGKILL`; `restart` works; after `kill -9` launchd relaunches it; with an invalid configuration it is relaunched with errors in the log and serves once the file is fixed. | T2 at M3 |
| P11 | With `XDG_STATE_HOME` in `cuttle.env`, the service uses that state directory after `brew services restart`. | T2 at M3 |
| P12 | Under the service, the application log rotates and `var/log/cuttle.log` holds only start-up output. | T2 at M3 |

## Assumptions

| ID | Assumption | Smallest check |
| --- | --- | --- |
| C1 | `process_type :interactive` avoids throttling that the default would apply. | T3: latency under `brew services` with the key unset and set ([research 0001, open question 3][r1-oq]) |
| C2 | `brew test` can use loopback TCP, possibly with `allow_network_access! :test`. | Settled in TASK-0015: loopback TCP needs network access in the test phase, which the default and `allow_network_access! :test` give and `deny_network_access! :test` takes away (D8). |
| C3 | launchd sends `SIGTERM` and waits `ExitTimeOut` from `stop_timeout`. | Settled in TASK-0015: `brew services stop` delivers `SIGTERM` and `launchctl print` reports `exit timeout = 15`. cuttle exits within milliseconds, so the `SIGKILL` at 15 s was not reached. |
| C4 | The `.env` override reaches the agent's environment. | Settled in TASK-0015 with P11. |

## Alternatives considered

- **Homebrew's `etc` and `var` for configuration and state.** Conventional
  for formulae, but shared across users and contrary to the XDG
  requirement ([research 0001 §5][r1-5]).
- **`github.com/adrg/xdg`.** It maps to `~/Library` on darwin
  ([research 0001 §4][r1-4]).
- **newsyslog rotation.** Needs sudo, which the install avoids
  ([research 0001 §7][r1-7]).
- **A PID file.** launchd tracks the process; the control socket guards
  against a second instance.

[d2-5]: 0002-proxy-core-and-routing.md#d5-actions-and-forwarding
[d2-7]: 0002-proxy-core-and-routing.md#d7-byte-accounting-and-live-counters
[d2-8]: 0002-proxy-core-and-routing.md#d8-decision-log
[d3-8]: 0003-upstream-chaining-and-exposure.md#d8-unreachable-upstream-hold
[d3-9]: 0003-upstream-chaining-and-exposure.md#d9-listeners
[d3-11]: 0003-upstream-chaining-and-exposure.md#d11-destination-address-policy-for-direct-routes
[d3-12]: 0003-upstream-chaining-and-exposure.md#d12-connection-limits
[d4-5]: 0004-configuration-and-reload.md#d5-starter-configuration
[d4-6]: 0004-configuration-and-reload.md#d6-reload
[d4-7]: 0004-configuration-and-reload.md#d7-atomic-write
[d5-4]: 0005-statistics-and-admin.md#d4-write-path
[d5-5]: 0005-statistics-and-admin.md#d5-admin-listener-and-host-allowlist
[d5-6]: 0005-statistics-and-admin.md#d6-password
[d8-3]: 0008-interface-selection.md#d3-matching-system-interfaces
[d8-15]: 0008-interface-selection.md#d15-status-command-line-logging-counters-and-statistics
[d8-16]: 0008-interface-selection.md#d16-dependencies-and-platform-code
[d9-8]: 0009-web-configuration.md#d8-test-a-url
[oi]: ../OPEN_ISSUES.md
[r1]: ../research/0001-homebrew-launchd-xdg.md
[r1-1]: ../research/0001-homebrew-launchd-xdg.md#1-homebrew-formula-for-a-go-program-in-a-personal-tap
[r1-2]: ../research/0001-homebrew-launchd-xdg.md#2-service-do-and-brew-services-without-sudo
[r1-3]: ../research/0001-homebrew-launchd-xdg.md#3-xdg-base-directory-specification
[r1-4]: ../research/0001-homebrew-launchd-xdg.md#4-xdg-on-macos
[r1-5]: ../research/0001-homebrew-launchd-xdg.md#5-homebrew-conventions-versus-xdg
[r1-7]: ../research/0001-homebrew-launchd-xdg.md#7-log-handling-under-launchd
[r1-oq]: ../research/0001-homebrew-launchd-xdg.md#open-questions
[r2-6]: ../research/0002-proxy-chaining-and-interception.md#6-what-clients-show-for-proxy-generated-failures

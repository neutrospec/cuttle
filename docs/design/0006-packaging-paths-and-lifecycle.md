# 0006 — Packaging, paths, and lifecycle

**Status** — Proposed (TASK-0001, 2026-11-25; revised in iteration 2).

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
  and mode 0700. macOS does not set it; `$TMPDIR` is the per-user 0700
  directory that the shell and the LaunchAgent share
  ([research 0001 §2][r1-2]). The socket path fits the 104-byte limit.
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
control   /var/folders/xy/abc123/T/cuttle/control.sock  TMPDIR
```

`--json` prints the same as an object.

### D2. Permissions

- `cuttle serve` sets umask 077. Files it creates are 0600 and directories
  0700.
- Startup fails if the configuration file is writable by group or others,
  or owned by another user, since that user could reroute traffic.
- The password file rules are in [0005 D6][d5-6].
- The control socket directory must be owned by the user with mode 0700.
  Otherwise cuttle warns and runs without the control socket.

### D3. Command line

| Command | Purpose |
| --- | --- |
| `cuttle serve [--config PATH]` | Run the proxy, admin UI, and control socket in the foreground |
| `cuttle init [--config PATH]` | Write the starter configuration if none exists ([0004 D5][d4-5]) and print its path |
| `cuttle check [--config PATH]` | Validate the configuration and print errors and warnings |
| `cuttle explain [--config PATH] [--remote] TARGET` | Show how the configuration routes `TARGET` |
| `cuttle reload` | Ask the running service to reload ([0004 D6][d4-6]) |
| `cuttle status [--json]` | Show the running service's listeners, counters, tunnels, and paths |
| `cuttle paths [--json]` | Print resolved paths (D1) |
| `cuttle admin passwd [--stdin]` | Set the admin password ([0005 D6][d5-6]) |
| `cuttle version`, `cuttle --version` | Print the version and Go version |

- Exit status 0 means success, 1 failure, and 2 a usage error. Warnings
  from `cuttle check` leave the status at 0.
- `explain` reads the file, not the running service. `TARGET` is
  `host:port` for a tunnel, or an `http://` or `https://` URL, which maps to
  a plain-HTTP request or a tunnel to port 443. Example:

  ```text
  $ cuttle explain https://wiki.corp.example/
  target  wiki.corp.example:443 (connect, local client)
  rule    office-only (#2)
  action  upstream:office -> 10.8.0.6:3128 (forward via)
  ```

### D4. Control socket

- An HTTP/1.1 server on a Unix stream socket, socket mode 0600 in a 0700
  directory. File system permissions are the access control; there is no
  password.
- At start, if the socket accepts a connection, another instance is running
  and `cuttle serve` exits with status 1. A stale socket is removed. If the
  socket cannot be created, cuttle logs a warning and runs without it.

| Method | Path | Response |
| --- | --- | --- |
| `GET` | `/v1/status` | Version, PID, start time, name, paths, listeners with state, upstreams with state, configuration state, counters ([0002 D7][d2-7]), up to 500 active tunnels, statistics state, open-file limit |
| `POST` | `/v1/reload` | `200` with `restart_required` and warnings, or `422` with errors |

```json
{"version": "0.1.0", "name": "cuttle-home", "pid": 4242,
 "started_at": "2026-11-25T09:00:00Z",
 "listeners": [{"address": "127.0.0.1:3128", "state": "bound"}],
 "upstreams": [{"name": "office", "state": "down",
                "since": "2026-11-25T09:58:00Z",
                "last_error": "connection_timeout"}],
 "config": {"path": "/Users/me/.config/cuttle/config.toml",
            "sha256": "9f2c...", "loaded_at": "2026-11-25T09:00:00Z",
            "restart_required": [], "last_reload_error": ""},
 "counters": {"tunnels_active": 3, "connections_active": 5},
 "tunnels": [{"target": "example.com:443", "action": "direct",
              "source": "local", "age_s": 42, "up": 3120, "down": 88000}],
 "stats": "ok", "rlimit_nofile": 10240}
```

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
3. Wait up to 5 s for in-flight plain-HTTP exchanges.
4. Close every tunnel and remaining connection with outcome `shutdown`.
5. Flush statistics within 2 s and close SQLite.
6. Log `stopped` and exit with status 0.

The budget is 8 s, below the formula's `stop_timeout 15`; launchd's default
of 5 s observed in research is too short
([research 0001 §2][r1-2]). A second `SIGTERM` or `SIGINT` exits at once
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
- The launchd file receives only output from before the log opens and
  runtime panics, so it stays small without rotation.
- Levels: `debug` adds tunnel-open and connection events; `info` has
  decisions and lifecycle events; `warn` has rejected sources (rate
  limited), retrying listeners, and statistics write errors; `error` has
  failures that stop a component.
- Privacy rules are in [0002 D8][d2-8].

### D8. Tap and formula

- The tap is a GitHub repository `<owner>/homebrew-tap` with
  `Formula/cuttle.rb`. Since Homebrew 6, users install with the fully
  qualified name or trust the tap first ([research 0001 §1][r1-1]). The
  owner and hosting are open ([OI-0004][oi]).
- Releases are tags `vX.Y.Z` with a source tarball and its SHA-256. `head`
  tracks `main` for development.

```ruby
class Cuttle < Formula
  desc "Explicit HTTP and HTTPS forward proxy with rule-based routing"
  homepage "https://github.com/<owner>/squid"
  url "https://github.com/<owner>/squid/archive/refs/tags/v0.1.0.tar.gz"
  sha256 "<sha256>"
  license "<SPDX id>"
  head "https://github.com/<owner>/squid.git", branch: "main"

  depends_on "go" => :build

  def fetch
    system "go", "mod", "download"
  end

  def install
    system "go", "build", *std_go_args(ldflags: "-X main.version=#{version}"),
           "./cmd/cuttle"
  end

  def caveats
    <<~EOS
      Configuration: ~/.config/cuttle/config.toml (created on first start)
      Admin UI:      http://127.0.0.1:3129/ after `cuttle admin passwd`
      Paths:         `cuttle paths`
      Non-default XDG_* values must also be set for the service in
        $HOMEBREW_USER_CONFIG_HOME/services/cuttle.env
      Uninstalling leaves ~/.config/cuttle, ~/.local/share/cuttle, and
        ~/.local/state/cuttle in place.
    EOS
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
    port = free_port
    (testpath/".config/cuttle/config.toml").write <<~EOS
      name = "cuttle-test"
      [proxy]
      listen = ["127.0.0.1:#{port}"]
      default = "block"
      [admin]
      listen = ""
    EOS
    with_test_process bin/"cuttle", "serve" do
      sleep 1
      code = shell_output("curl -s -o /dev/null -w '%{http_connect}' " \
                          "-x http://127.0.0.1:#{port} " \
                          "https://blocked.invalid/; true")
      assert_equal "403", code
    end
    assert_match version.to_s, shell_output("#{bin}/cuttle version")
  end
end
```

The `fetch`, `with_test_process`, and `std_go_args` usage follows the
research draft ([research 0001 §1][r1-1]). The service block decisions:

- No `--config` and no `XDG_*` in the block. cuttle resolves paths at run
  time, and the `.env` file covers custom values
  ([research 0001 §5][r1-5]).
- `keep_alive true` restarts after crashes and failed starts. A Go panic
  exits with status 2, not a signal, so `keep_alive crashed: true` would
  miss it. `brew services stop` uses `bootout`, which launchd does not undo.
- `stop_timeout 15` covers the D6 budget.
- `process_type :interactive`, because an unset `ProcessType` lets launchd
  throttle CPU and I/O, and cuttle carries interactive browser traffic
  ([research 0001 §2][r1-2]). Assumption C1.
- No `require_root`; `sudo brew services` is unsupported.
- The test needs no outbound connection because a block makes none. It
  checks only curl's `%{http_connect}`, since curl's exit status for a
  failed `CONNECT` differs between versions ([research 0002 §6][r2-6]).
  Whether the sandbox needs `allow_network_access! :test` for loopback is
  open ([OI-0003][oi]).

Uninstalling leaves the XDG files, the `var/log` file, and a loaded agent
unless the service was stopped first ([research 0001 §5][r1-5]). The README
gives the cleanup steps: `brew services stop cuttle`, `brew uninstall
cuttle`, `brew services cleanup`, and removal of the three XDG directories
and the log.

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
| P8 | `brew install --build-from-source <owner>/tap/cuttle`, `brew test cuttle`, and `brew audit --strict <owner>/tap/cuttle` pass. | T2 at M3 |
| P9 | `brew services start cuttle` writes `~/Library/LaunchAgents/sh.brew.cuttle.plist` with `KeepAlive`, `ExitTimeOut` 15, `ProcessType` `Interactive`, and the log paths; the service serves on `127.0.0.1:3128`; `cuttle status` reaches it and its paths equal `cuttle paths`. | T2 at M3 |
| P10 | `brew services stop cuttle` logs `stopped` within 15 s without `SIGKILL`; `restart` works; after `kill -9` launchd relaunches it; with an invalid configuration it is relaunched with errors in the log and serves once the file is fixed. | T2 at M3 |
| P11 | With `XDG_STATE_HOME` in `cuttle.env`, the service uses that state directory after `brew services restart`. | T2 at M3 |
| P12 | Under the service, the application log rotates and `var/log/cuttle.log` holds only start-up output. | T2 at M3 |

## Assumptions

| ID | Assumption | Smallest check |
| --- | --- | --- |
| C1 | `process_type :interactive` avoids throttling that the default would apply. | T3: latency under `brew services` with the key unset and set ([research 0001, open question 3][r1-oq]) |
| C2 | `brew test` can use loopback TCP, possibly with `allow_network_access! :test`. | Open question 5 ([OI-0003][oi]) |
| C3 | launchd sends `SIGTERM` and waits `ExitTimeOut` from `stop_timeout`. | Open question 1 ([OI-0003][oi]) |
| C4 | The `.env` override reaches the agent's environment. | Open question 2 ([OI-0003][oi]); P11 |

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

[d2-7]: 0002-proxy-core-and-routing.md#d7-byte-accounting-and-live-counters
[d2-8]: 0002-proxy-core-and-routing.md#d8-decision-log
[d3-9]: 0003-upstream-chaining-and-exposure.md#d9-listeners
[d4-5]: 0004-configuration-and-reload.md#d5-starter-configuration
[d4-6]: 0004-configuration-and-reload.md#d6-reload
[d4-7]: 0004-configuration-and-reload.md#d7-atomic-write
[d5-4]: 0005-statistics-and-admin.md#d4-write-path
[d5-6]: 0005-statistics-and-admin.md#d6-password
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

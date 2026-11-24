# 0001 — Homebrew tap, launchd service, XDG paths, and Go SQLite for `cuttle`

- Task: TASK-0001 (milestone M1; packaging and storage inputs for the
  revised design).
- Date: 2026-11-25.
- Environment: macOS 27.2.1 (build 26B131), arm64, Apple M5 Max (18 CPUs);
  Homebrew 7.0.15-111-ge40c695 at `/opt/homebrew`; Go 1.27.4 darwin/arm64
  from the Homebrew `go` 1.27.4 bottle; Apple clang 21.0.0.
- Path abbreviations: `HB/` is `/opt/homebrew/Library/Homebrew` (upstream
  <https://github.com/Homebrew/brew/tree/main/Library/Homebrew>), `HBD/` is
  `/opt/homebrew/docs` (published at <https://docs.brew.sh>), and `GOSRC/` is
  `/opt/homebrew/Cellar/go/1.27.4/libexec/src`
  (<https://github.com/golang/go/tree/go1.27.4/src>). Line numbers refer to
  the local copies at the versions above.
- Observations of running LaunchAgents were read-only (`launchctl print`,
  `ps -E`) on two existing Homebrew-generated agents of the observation
  account. No service was started, loaded, or stopped.

## Summary

A personal tap is a GitHub repository named `<owner>/homebrew-tap` that
holds `Formula/cuttle.rb`; the formula declares `depends_on "go" => :build`,
downloads modules in `fetch`, and builds with `std_go_args`, and since
Homebrew 6.0 the user must trust the tap or install by the fully qualified
name. `brew services start cuttle` without sudo copies a generated plist to
`~/Library/LaunchAgents/sh.brew.cuttle.plist` and bootstraps it into
`gui/<uid>`; `stop` and `restart` use `launchctl bootout`, so launchd sends
SIGTERM and then SIGKILL after `ExitTimeOut`, which defaulted to 5 seconds on
the observed agents and is set by the service block's `stop_timeout`.
launchd gives the agent `HOME`, `USER`, `LOGNAME`, `SHELL`, `TMPDIR`, and
`PATH=/usr/bin:/bin:/usr/sbin:/sbin`, but none of the `XDG_*` variables the
login shell exports, and Go's `os.UserConfigDir` and `os.UserCacheDir` return
`~/Library/...` on darwin. cuttle therefore has to implement XDG resolution
itself with the specification's `$HOME` defaults. Users with non-default
`XDG_*` values can give the same values to the service through Homebrew's
per-service override file `$HOMEBREW_USER_CONFIG_HOME/services/cuttle.env`.
The recommended mapping is config in `$XDG_CONFIG_HOME/cuttle`, the admin
password hash in `$XDG_DATA_HOME/cuttle`, the session key, statistics
database, and logs in `$XDG_STATE_HOME/cuttle`, and any socket in
`$XDG_RUNTIME_DIR/cuttle` or else the per-user `$TMPDIR/cuttle`. launchd does
not rotate `StandardOutPath` files, so cuttle should rotate its own log.
All three SQLite drivers built here and ran in WAL mode with SQLite 3.54.1.
modernc.org/sqlite is pure Go, BSD-3-Clause, and adds 4.7 MB to a stripped
binary. mattn/go-sqlite3 adds only 2.0 MB but needs cgo, and with cgo off it
still builds into a binary that panics at run time.

## 1. Homebrew formula for a Go program in a personal tap

### Tap naming and install commands

- `brew tap <user>/<repo>` clones `https://github.com/<user>/homebrew-<repo>`;
  the one-argument form requires the `homebrew-` prefix on GitHub, and a
  non-GitHub URL can be given as a second argument (`HBD/Taps.md:25,45,55-62`,
  <https://docs.brew.sh/Taps>). The tap guide recommends
  `<owner>/homebrew-tap` and shows `brew tap-new` scaffolding with bottle CI
  (`HBD/How-to-Create-and-Maintain-a-Tap.md:14-41`).
- Formulae go in a `Formula/` subdirectory (same guide, line 71).
- Tap trust is required by default since Homebrew 6.0.0. Installing the fully
  qualified name trusts only that formula; installing by short name needs an
  explicit trust first (`HBD/Tap-Trust.md`, reviewed 2026-10-25,
  <https://docs.brew.sh/Tap-Trust>):

  ```sh
  brew install <owner>/tap/cuttle
  # or
  brew tap <owner>/tap
  brew trust --formula <owner>/tap/cuttle
  brew install cuttle
  ```

- A tap formula with the same name as a homebrew-core formula cannot be
  installed side by side (`HBD/How-to-Create-and-Maintain-a-Tap.md:98-100`).
  Observed: `brew info --formula squid` shows the core `squid` 7.8, while
  `brew info --formula cuttle` and `brew info --cask cuttle` find nothing, so
  `cuttle` is free today.

### Minimal formula

Draft for the designer, assembled from the cited documentation and the
`caddy` formula (homebrew-core `Formula/c/caddy.rb` at `b87759317dc2`):

```ruby
class Cuttle < Formula
  desc "Explicit HTTP and HTTPS forward proxy with rule-based routing"
  homepage "https://github.com/<owner>/squid"
  url "https://github.com/<owner>/squid/archive/refs/tags/v0.1.0.tar.gz"
  sha256 "<sha256 of that tarball>"
  license "<SPDX id>"
  head "https://github.com/<owner>/squid.git", branch: "main"

  depends_on "go" => :build

  def fetch
    system "go", "mod", "download"
  end

  def install
    ldflags = "-X main.version=#{version}"
    system "go", "build", *std_go_args(ldflags:), "./cmd/cuttle"
  end

  service do
    run [opt_bin/"cuttle", "serve"]
    keep_alive true
    stop_timeout 15
    log_path var/"log/cuttle.log"
    error_log_path var/"log/cuttle.log"
  end

  test do
    port = free_port
    # HOME is testpath, so the XDG defaults resolve inside the sandbox.
    (testpath/".config/cuttle/config.toml").write <<~EOS
      listen = "127.0.0.1:#{port}"
      default = "block"
    EOS
    with_test_process bin/"cuttle", "serve" do
      sleep 1
      out = shell_output("curl -s -o /dev/null -w '%{http_connect}' " \
                         "-x http://127.0.0.1:#{port} " \
                         "https://blocked.invalid/ 2>&1", 56)
      assert_match "403", out
    end
    assert_match version.to_s, shell_output("#{bin}/cuttle --version")
  end
end
```

The config keys, subcommands, and curl exit code in the test are
placeholders for the design to define.

- Stable `url` with `sha256` versus `head`: a Git `head` must specify
  `branch:` (`HBD/Formula-Cookbook.md:876-886`, reviewed 2026-04-05,
  <https://docs.brew.sh/Formula-Cookbook#unstable-versions-head>). A
  HEAD-only formula is flagged by audit only in the core tap
  (`HB/formula_auditor.rb:860`). It installs with `brew install --HEAD`
  (`HB/cmd/install.rb:92`), and `brew upgrade` checks a HEAD install for new
  commits only with `--fetch-HEAD` (`HB/cmd/upgrade.rb:109-112`). Inference:
  tagged releases with `url` and `sha256` give the user normal
  `brew upgrade` behaviour, and `head` remains useful for development.
- `depends_on "go" => :build` is the documented Go dependency, and Homebrew
  sets `GOTOOLCHAIN=local` (`HBD/Language-Specific-Formulae.md:203-230`,
  reviewed 2026-07-18, <https://docs.brew.sh/Language-Specific-Formulae#go>;
  `HB/extend/ENV/super.rb:181`). The `go` directive in `go.mod` must
  therefore not exceed the Homebrew `go` version, which is 1.27.4 today
  (`brew info --json=v2 go`).
- `fetch` runs with network access after unpacking. When it is defined,
  `install` runs without network access and with the module cache read-only
  (`HB/formula.rb:1715-1740`). Network access is otherwise allowed for every
  phase unless `deny_network_access!` is declared (`HB/formula.rb:4320-4352`).
- `std_go_args` expands to `-trimpath -o=<bin/name>` plus
  `-ldflags=-s -w <extra>` (stripping is skipped for debug-symbol builds),
  with optional `-tags` and `-gcflags`. `ldflags: :goreleaser` sets
  `main.version`, `main.commit`, `main.date`, and `main.builtBy` from formula
  metadata (`HB/formula.rb:2336-2390`;
  `HBD/Language-Specific-Formulae.md:232-257`). Version injection is
  therefore `-X main.version=#{version}`. For head builds, `caddy` passes
  `version.commit` when `build.head?` is true.
- `test do` runs in a temporary `testpath` with `HOME=testpath`. A
  `--version` check counts as a bad test, though better than none.
  `with_test_process` runs a background process and sends TERM and then KILL
  after 5 seconds. The macOS test sandbox allows Unix sockets under
  `testpath` (`HBD/Formula-Cookbook.md:332-349`;
  `HB/formula_assertions.rb:47`; `free_port` in
  `HB/formula_free_port.rb:12`). Inference: a policy block needs no outbound
  connection (design 0001), so a block-rule `CONNECT` returning `403` makes
  a meaningful offline test. `caddy` declares `allow_network_access! :test`
  for its loopback server.
- On Linux arm64, Homebrew sets `CGO_ENABLED=0` for every build
  (`HB/extend/os/linux/extend/ENV/super.rb:51`). This only matters if the
  design picks a cgo SQLite driver (section 6).

## 2. `service do` and `brew services` without sudo

### What the DSL generates

`Homebrew::Service#to_plist` (`HB/service.rb:609-671`) maps the block as
follows. The defaults come from `HB/service.rb:42-70`, and the method table
is in `HBD/Formula-Cookbook.md:1339-1405`.

| DSL | launchd key | Default if omitted |
| --- | --- | --- |
| `name macos:` | `Label` | `sh.brew.cuttle` (`service.rb:96-98`) |
| `run` | `ProgramArguments` | required; `~` args expanded (`568-570`) |
| `run_at_load` | `RunAtLoad` | `true` |
| `keep_alive true` | `KeepAlive` = true | key omitted |
| `keep_alive crashed:` / `successful_exit:` / `path:` | `KeepAlive` dict | — |
| `environment_variables` | `EnvironmentVariables` | omitted, unless a user `.env` exists |
| `working_dir` | `WorkingDirectory` | omitted |
| `log_path` | `StandardOutPath` | omitted |
| `error_log_path` | `StandardErrorPath` | omitted |
| `stop_timeout` | `ExitTimeOut` | omitted (system default) |
| `restart_delay` | `TimeOut` | omitted |
| `throttle_interval` | `ThrottleInterval` | omitted (launchd: 10 s) |
| `process_type` | `ProcessType` | omitted |
| `run_type :interval` / `:cron` | `StartInterval` / `StartCalendarInterval` | `:immediate` |
| always | `LimitLoadToSessionType` | Aqua, Background, LoginWindow, StandardIO, System |

Notes:

- The older label `homebrew.mxcl.<name>` is still recognised when looking
  up and stopping a service (`HB/service.rb:91-105`). One existing agent on
  the observation machine still uses that label.
- `log_path` and `error_log_path` are optional. The cookbook table gives no
  default. Inference: without them launchd sends stdout and stderr to
  `/dev/null`.
- launchd.plist(5) (`/usr/share/man/man5/launchd.plist.5`, dated 30 July
  2019, shipped with macOS 27.2.1) says `TimeOut` "is no longer
  implemented". Inference: `restart_delay` therefore has no effect on macOS.
- launchd.plist(5) says an unset `ProcessType` makes the system "apply light
  resource limits to the job, throttling its CPU usage and I/O bandwidth",
  that `Standard` is the same as unset, and that `Interactive` applies no
  limits. This matters for a proxy (see Open questions).
- In launchd.plist(5), `KeepAlive` true implies `RunAtLoad`, and quick,
  repeated exits are throttled. `ThrottleInterval` defaults to 10 seconds.

### Start, stop, restart, kill

- `start` (`HB/services/cli.rb:181-218`) regenerates the plist from the
  formula (`HB/services/formula_wrapper.rb:390-398`), writes it to
  `~/Library/LaunchAgents/sh.brew.cuttle.plist` with mode 0644
  (`cli.rb:518-562`; `HB/services/system.rb:80-88`), and creates the
  working directory and the parent directories of the log paths
  (`cli.rb:493`; `service.rb:578-589`). It then runs
  `launchctl enable gui/<uid>/sh.brew.cuttle` and
  `launchctl bootstrap gui/<uid> <plist>` (`cli.rb:454-462`). The domain is
  `gui/<uid>`, or `user/<uid>` when brew runs over SSH without owning
  `/dev/console` or through sudo as the user (`system.rb:97-119`). A copy of
  the plist is also written into the keg at install time
  (`HB/formula_installer.rb:1447-1468`; `HB/formula.rb:1594-1601`).
- `stop` (`cli.rb:230-346`) runs `launchctl bootout gui/<uid>/<label>`,
  retries once a second while the job is still running, waits up to
  `--max-wait` (default 60 s; 0 waits indefinitely;
  `HB/services/subcommand/stop.rb:18-20,37`), falls back to
  `launchctl stop`, and then deletes the plist from `~/Library/LaunchAgents`
  unless `--keep` is given (`cli.rb:333-337`).
- `restart` stops with `max_wait` 0, which waits until the job is gone, and
  then starts again. It is also how a new plist is applied after
  `brew upgrade` (`HB/services/subcommand/restart.rb:30-46`). `brew upgrade`
  itself does not restart the service. The install caveat tells the user to
  run `brew services restart cuttle` (`HB/caveats.rb:304-308`).
- `kill` runs `launchctl stop`. It refuses a `keep_alive` service, which
  launchd would restart (`cli.rb:350-375`).
- Signals: per launchd.plist(5), `ExitTimeOut` is "the amount of time
  launchd waits between sending the SIGTERM signal and before sending a
  SIGKILL signal when the job is to be stopped. The default value is
  system-defined." Zero means infinity and should not be used. When a job
  dies, launchd also kills the remaining processes in its process group
  unless `AbandonProcessGroup` is set.
- Observed default: `launchctl print gui/<uid>/<label>` reported
  `exit timeout = 5` and `minimum runtime = 10` for two Homebrew agents whose
  plists have no `ExitTimeOut` key (macOS 27.2.1).

Design consequences (inference): cuttle should treat SIGTERM as the stop
signal and finish draining tunnels, flushing counters, and closing SQLite
within `ExitTimeOut`. The service block should set `stop_timeout`
explicitly. With `keep_alive true`, a startup failure such as an invalid
config is relaunched about every 10 s, so the design should choose between
`keep_alive true`, `crashed: true`, and `successful_exit: false` on
purpose.

### Environment of the LaunchAgent

Observed with `launchctl print` on a running `sh.brew.*` agent and
`ps -E -ww -o command= -p <pid>` (variable names only):

- Default environment: `PATH=/usr/bin:/bin:/usr/sbin:/sbin`.
- Set at spawn: `HOME`, `USER`, `LOGNAME`, `SHELL`,
  `TMPDIR=/var/folders/<per-user>/T/`, `XPC_SERVICE_NAME`, `XPC_FLAGS`, and
  `OSLogRateLimit`.
- The plist's `EnvironmentVariables` (`HOMEBREW_PREFIX` in that plist).
- Variables set domain-wide with `launchctl setenv` ("inherited
  environment"; two such variables were present).
- No `XDG_*` and no `LANG`, although the login shell exports
  `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_STATE_HOME`, and `XDG_CACHE_HOME`,
  and `launchctl getenv XDG_CONFIG_HOME` prints nothing.

The agent's `TMPDIR` equals the shell's `TMPDIR` and
`getconf DARWIN_USER_TEMP_DIR`, which is a 0700 directory owned by the user.

Homebrew merges a per-service user override file into
`EnvironmentVariables` when it generates the plist. The file is
`$HOMEBREW_USER_CONFIG_HOME/services/<formula>.env` (`KEY=value` lines, `#`
comments) and takes effect on the next `brew services restart`
(`HB/service.rb:486-531`; `HB/cmd/services.rb:26-30`).
`HOMEBREW_USER_CONFIG_HOME` is `$XDG_CONFIG_HOME/homebrew` when that variable
is set and `~/.homebrew` otherwise (`/opt/homebrew/bin/brew:165-173`).
Homebrew skips the file when it is world- or group-writable, and for root
services.

### What changes under `sudo brew services`

- The plist goes to `/Library/LaunchDaemons` and loads into the `system`
  domain. It starts at boot instead of login (`HB/services/system.rb:68-99`;
  `HB/cmd/services.rb:18-24`).
- The job runs as root unless `--sudo-service-user=<user>` sets `UserName`
  (`cli.rb:530-536`). Without that flag, brew chowns the plist and the keg's
  `opt`, `bin`, and `sbin` paths to `root:admin` with the sticky bit, and
  `brew upgrade` and `brew uninstall` then need `sudo rm`
  (`cli.rb:379-445`).
- User `.env` overrides are ignored for root services
  (`HB/service.rb:497-500`).
- Inference: as root, `HOME` and the XDG defaults would resolve under root's
  home, so the CLI and the service would use different paths. The design
  should leave `require_root` false and not support sudo mode.

## 3. XDG Base Directory specification

Source: <https://specifications.freedesktop.org/basedir-spec/latest/>,
version 0.8, 08 May 2021, fetched 2026-11-25.

| Variable | Default if unset or empty | Purpose |
| --- | --- | --- |
| `XDG_CONFIG_HOME` | `$HOME/.config` | user-specific configuration files |
| `XDG_DATA_HOME` | `$HOME/.local/share` | user-specific data files |
| `XDG_STATE_HOME` | `$HOME/.local/state` | state that persists across restarts but is not important or portable enough for data, such as logs, history, and reusable application state |
| `XDG_CACHE_HOME` | `$HOME/.cache` | user-specific non-essential cached data |
| `XDG_RUNTIME_DIR` | no default | runtime files and objects such as sockets and named pipes |

The `XDG_CONFIG_DIRS` (`/etc/xdg`) and `XDG_DATA_DIRS`
(`/usr/local/share/:/usr/share/`) search paths exist but cuttle has no
system-wide files.

Rules:

- Paths in these variables must be absolute. A relative path is invalid and
  must be ignored.
- When writing, create a missing destination directory with mode 0700 and do
  not change the permissions of an existing one. Handle failure to create or
  write. When reading, skip unreadable locations.
- `XDG_RUNTIME_DIR` must be owned by the user with mode 0700. Its lifetime is
  bound to the login session, and its files must not survive a reboot or a
  full logout. It must be on a local file system that supports `AF_UNIX`
  sockets and locking. Files in it may be cleaned periodically unless their
  access time is updated every 6 hours or they have the sticky bit. If it is
  unset, applications should fall back to a replacement directory with
  similar capabilities and print a warning. Only small files belong there.

### Mapping of cuttle files (recommendation, inference)

| File | Location | Justification |
| --- | --- | --- |
| Config (rules, upstreams) | `$XDG_CONFIG_HOME/cuttle/config.<ext>`, 0600 | User-edited configuration. If the admin UI writes it, write a temporary file in the same directory and rename it. |
| Admin password hash | `$XDG_DATA_HOME/cuttle/admin-password`, 0600 | The program writes it (CLI or UI) and it must persist. A separate file keeps it out of dotfile repositories that track `~/.config`. Alternative: a separate 0600 file next to the config. |
| Session signing secret | `$XDG_STATE_HOME/cuttle/session-key`, 0600 | Generated, machine-specific, and regenerable. Losing it only ends sessions. |
| Statistics database | `$XDG_STATE_HOME/cuttle/stats.db` (+ `-wal`, `-shm`) | Bounded-retention history fits the spec's "actions history" under state. WAL needs a local file system and write access to the directory (section 6). |
| Logs | `$XDG_STATE_HOME/cuttle/cuttle.log` | The specification names logs as state data. |
| launchd stdout/stderr | `$(brew --prefix)/var/log/cuttle.log` | Homebrew convention for crash and early-start output (section 5). |
| Control socket or PID file | `$XDG_RUNTIME_DIR/cuttle/`, else `$TMPDIR/cuttle/` (0700) | macOS sets no `XDG_RUNTIME_DIR`, and `$TMPDIR` is the closest per-user 0700 local directory, the same for the CLI and the agent. A PID file is unnecessary because launchd tracks the PID. |
| Cache | none | No cached data is planned. |

All directories are created with mode 0700. The longest socket path,
`$TMPDIR` (49 bytes here) plus `cuttle/control.sock`, fits the 104-byte
`sun_path` limit (`sys/un.h:79` in the macOS SDK).

## 4. XDG on macOS

- Go 1.27.4 on darwin (`GOSRC/os/file.go`):
  - `os.UserConfigDir` returns `$HOME/Library/Application Support` and
    ignores `XDG_CONFIG_HOME` (lines 557-595).
  - `os.UserCacheDir` returns `$HOME/Library/Caches` (lines 504-542).
  - `os.UserHomeDir` returns `$HOME` and fails if it is unset (lines
    605-624).
  - The `XDG_*` variables are honoured only in the generic Unix branch.
  - The `os` package has no data, state, or runtime directory helper.

  cuttle must resolve XDG paths itself. Per variable: read the value, use it
  only if it is non-empty and absolute, and otherwise use the `$HOME`
  default from section 3.
- Library caveat: `github.com/adrg/xdg` (`paths_darwin.go` at
  `393abc7503a2`, latest tag v0.5.3) defaults the config, data, and state
  homes to `~/Library/Application Support` and the cache to
  `~/Library/Caches` on darwin. It does not produce `~/.config` without
  explicit variables.
- XDG variables do not reach a LaunchAgent (section 2, observed). launchd
  passes only the plist's `EnvironmentVariables`, domain-wide
  `launchctl setenv` values, and its own defaults. launchctl(1) describes
  `setenv` as applying to "all future processes launched by launchd in the
  caller's context". Inference: `launchctl setenv` does not persist across
  login, so it is not a product mechanism.
- How the CLI and the service can agree (options; inference):
  1. Both resolve paths with the spec defaults. This is identical whenever
     the user's `XDG_*` are unset or equal the defaults, as on the
     observation machine, where all four equal the defaults.
  2. Users with non-default values put the same `XDG_*` lines into
     `$HOMEBREW_USER_CONFIG_HOME/services/cuttle.env`, which Homebrew merges
     into the plist (section 2). The caveats and docs should say so.
  3. Add a `cuttle paths` command that prints the resolved paths, and have
     the service log its resolved paths at startup. The admin UI can also
     report them, so a mismatch is visible.
  4. Do not compute XDG paths in the service block (section 5).

  Recommendation: 1 + 2 + 3.
- Prior art for XDG paths on macOS:
  - Git reads `$XDG_CONFIG_HOME/git/config` and uses `$HOME/.config/` when
    the variable is unset or empty (git-config(1), Apple Git 2.54.0, local
    man page; <https://git-scm.com/docs/git-config#FILES>).
  - GitHub CLI (`go-gh` `pkg/config/config.go` at `2ce792192e43`, latest
    tag v2.17.0) uses `XDG_CONFIG_HOME`, `XDG_STATE_HOME`, `XDG_DATA_HOME`,
    and `XDG_CACHE_HOME` and falls back to `~/.config/gh`,
    `~/.local/state/gh`, `~/.local/share/gh`, and `~/.cache/gh` on macOS
    too (lines 270-337).
  - Homebrew keeps user configuration in `$XDG_CONFIG_HOME/homebrew` when
    that variable is set (`/opt/homebrew/bin/brew:165-173`).

## 5. Homebrew conventions versus XDG

- Prefix directories: `etc` is `$HOMEBREW_PREFIX/etc`, `pkgetc` is
  `$HOMEBREW_PREFIX/etc/<name>`, and `var` is `$HOMEBREW_PREFIX/var`. They
  sit outside the Cellar and persist across upgrades
  (`HB/formula.rb:1448-1465`; `HBD/Formula-Cookbook.md:1288-1292`). Service
  logs conventionally go to `var/"log/<name>.log"` (caddy, kanata, minidlna,
  and both observed agents). The prefix belongs to one user and is shared by
  everyone who uses it. Inference: this conflicts with the decision to keep
  per-user state under XDG paths in `$HOME`.
- homebrew-core has precedent for user-home paths in service blocks:
  - `kanata` (at `feaf9d5e9aee`) runs with
    `--cfg #{Dir.home}/.config/kanata/kanata.kbd` and `working_dir Dir.home`.
  - `minidlna` (at `6554855a4091`) uses
    `#{Dir.home}/.config/minidlna/minidlna.conf` and a PID file there.
  - `caddy` points its service at `etc` and sets
    `XDG_DATA_HOME` and `HOME` to `var/lib` through
    `environment_variables`, with a caveat explaining the change.
- `Dir.home` in a tap formula: the Ruby is evaluated on the user's machine,
  at install for the keg plist and again at each `brew services start`
  (`HB/formula_installer.rb:1447-1465`; `HB/services/cli.rb:527-528`).
  Inference: `Dir.home` is therefore the invoking user's home. For
  API-installed core formulae, the home directory is serialised as the
  placeholder `/$HOME` and substituted on load (`HB/global.rb:47`;
  `HB/service.rb:882-887`).
- A service block cannot see the user's `XDG_*` reliably. `bin/brew`
  renames `XDG_CONFIG_HOME` and related variables to
  `HOMEBREW_XDG_CONFIG_HOME` and so on, then re-executes with `env -i` and a
  filtered list (`/opt/homebrew/bin/brew:245-258,301-344`). Inference: a
  formula could read the renamed `ENV["HOMEBREW_XDG_CONFIG_HOME"]`, but that
  is undocumented and is captured only when `start` runs.
  Recommendation (inference): pass no `--config` and no `XDG_*` from the
  service block. Let cuttle resolve paths at run time and use the `.env`
  override for non-default setups. If the design wants an explicit flag,
  `--config "#{Dir.home}/.config/cuttle/config.<ext>"` follows the kanata
  precedent but hard-codes the default and ignores `XDG_CONFIG_HOME`.
- Service rubocop: use `opt_bin` rather than `bin` in service blocks
  (`HB/rubocops/service.rb`, `CELLAR_PATH_AUDIT_CORRECTIONS`).
- `caveats`: a `def caveats` string is shown after install
  (`HBD/Formula-Cookbook.md:408-416`). Homebrew already appends the
  `brew services start cuttle` hint and the manual command
  (`HB/caveats.rb:291-328`). The cuttle caveat should therefore list the
  XDG file locations, the first-run password step, the `.env` override, and
  the uninstall cleanup steps.
- `brew uninstall cuttle` removes the keg and its links
  (`HB/uninstall.rb:79-81`). It warns about, but leaves, files under
  `etc/cuttle` and `etc/cuttle*` (`HB/uninstall.rb:96-133`), and it says
  nothing about `var`. The code contains no service handling.
  Inference: uninstalling therefore leaves the following behind:
  - `var/log/cuttle.log`.
  - Every XDG file in `$HOME`.
  - `~/Library/LaunchAgents/sh.brew.cuttle.plist` and the loaded job,
    unless the user ran `brew services stop` first. `brew services cleanup`
    removes such orphans (`HB/services/subcommand/cleanup.rb`).
  - The `.env` override file.

  `brew uninstall --zap` applies only to casks (`brew uninstall --help`).

## 6. Go SQLite drivers

### Comparison

| | modernc.org/sqlite | github.com/mattn/go-sqlite3 | github.com/ncruces/go-sqlite3 |
| --- | --- | --- | --- |
| Version tested | v1.63.0 (2026-11-22) | v1.14.53 (2026-11-05) | v0.38.0 (2026-11-21) |
| Approach | SQLite transpiled to Go | cgo, bundled amalgamation | Wasm build translated to Go by wasm2go |
| cgo / C compiler | no | yes (`CGO_ENABLED=1` and a C compiler) | no |
| `go` directive | 1.26.0 | 1.21 | 1.26.0 |
| Licence | BSD-3-Clause; SQLite public domain; vendored sqlite-vec MIT | MIT; SQLite public domain | MIT; translation MIT-0 with original licences kept |
| Published versions | 175 | 72 | 114 (pre-1.0) |
| Canonical repository | gitlab.com/cznic/sqlite (GitHub mirror) | github.com/mattn/go-sqlite3 | github.com/ncruces/go-sqlite3 |
| WAL observed | yes | yes | yes |
| Stripped binary (bytes) | 6,470,514 | 3,788,738 | 7,813,522 |
| Increase over baseline | +4.68 MB | +2.00 MB | +6.02 MB |
| Cold build | 4 s | 12 s | 6 s |

Sources:

- Release times and repositories come from the Go module proxy
  (`https://proxy.golang.org/<module>/@latest` and `/@v/list`, queried
  2026-11-25).
- Licences and approach come from each module's `LICENSE*` and `README.md`
  in the module cache.
- mattn's cgo requirement is in its README at lines 80-83 and 196-198.
- ncruces's macOS support (amd64, arm64; README line 79), OFD locks, WAL
  shared memory, and macOS blocking WAL locks are documented in
  `vfs/README.md` at lines 20-82.
- SQLite 3.54.1 is bundled in all three (`sqlite_version()` output; mattn
  `sqlite3-binding.c:471`).

Observations:

- `otool -L` shows that every binary links only `/usr/lib/libSystem.B.dylib`
  and `/usr/lib/libresolv.9.dylib`. mattn compiles SQLite in statically, so
  no Homebrew `sqlite` dependency is needed.
- mattn built with `CGO_ENABLED=0` succeeds (3,213,170 bytes unstripped) but
  panics on first use with "Binary was compiled with 'CGO_ENABLED=0',
  go-sqlite3 requires cgo to work. This is a stub". Combined with Homebrew's
  `CGO_ENABLED=0` on Linux arm64 (section 1), a mattn-based formula would
  build cleanly there and fail at run time (inference).
- modernc and ncruces built with `CGO_ENABLED=0` are byte-identical in size
  to their `CGO_ENABLED=1` builds and ran WAL successfully.

WAL constraints from <https://sqlite.org/wal.html> (fetched 2026-11-25):

- All processes that use the database must be on the same host, and WAL
  does not work over a network file system.
- WAL mode persists in the database file.
- WAL adds `-wal` and `-shm` files. Opening the database needs write access
  to the `-shm` file, or to the directory when that file is absent. Since
  3.22.0, read-only opening has conditions.
- Checkpoints run automatically by default.

Inference: `$XDG_STATE_HOME` on the local APFS home volume meets these
constraints. A CLI that reads statistics while the service writes them is
fine on one host.

Recommendation (inference): modernc.org/sqlite. It needs no cgo, is v1.x
with frequent releases, and its 4.7 MB increase is acceptable for a
Homebrew-built CLI. Choose mattn only if binary size outweighs a cgo build
and the stub hazard. ncruces is the largest and still pre-1.0.

### Build check (reproducible commands)

Each program is a 40-line `main.go` that imports one driver for side effects
and does the following:

1. Opens `t.db` in `os.MkdirTemp`.
2. Runs `PRAGMA journal_mode=WAL` and `SELECT sqlite_version()`.
3. Creates a `STRICT` table.
4. Upserts with `ON CONFLICT ... DO UPDATE`.
5. Reads the value back.

The baseline imports only `database/sql`. Driver names are `sqlite`
(modernc) and `sqlite3` (mattn, ncruces `driver` package). All three
printed `journal_mode=wal sqlite_version=3.54.1 bytes=42`; the baseline
printed `[]`.

```sh
R=<scratch>/research-0001
export GOMODCACHE=$R/gomodcache GOCACHE=$R/gocache \
  GOFLAGS=-mod=mod GOTOOLCHAIN=local
cd $R/sqlite/<driver>        # baseline, modernc, mattn, ncruces
CGO_ENABLED=1 go build -o $R/bin2/<driver>-default .
CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" \
  -o $R/bin2/<driver>-stripped .
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
  -o $R/bin2/<driver>-nocgo-stripped .   # modernc, ncruces
CGO_ENABLED=0 go build -o $R/bin2/mattn-nocgo .
$R/bin2/<driver>-default
# Cold build: empty GOCACHE, warm module cache, GOPROXY=off, timed with
# `date +%s` (1 s resolution), using the stripped build command.
```

`-trimpath -ldflags="-s -w"` matches what `std_go_args` passes, so the
stripped sizes approximate a Homebrew-built binary.

| Build | default (bytes) | stripped (bytes) |
| --- | --- | --- |
| baseline | 2,738,466 | 1,792,626 |
| modernc v1.63.0 | 9,989,410 | 6,470,514 |
| mattn v1.14.53 | 7,118,306 | 3,788,738 |
| ncruces v0.38.0 | 13,939,762 | 7,813,522 |

The scratch directory is outside the repository and may be cleaned. The
earlier interrupted run produced the same sizes from the same inputs.

## 7. Log handling under launchd

- launchd.plist(5) defines `StandardOutPath` and `StandardErrorPath` only as
  file mappings, created if missing with the job's user and `Umask`. It
  lists no rotation or size key. Inference: launchd does not rotate these
  files.
- Homebrew does not rotate service logs either: a search of `HB/` for
  `newsyslog`, `logrotate`, and `rotat` found nothing related to logs.
- macOS's rotator is newsyslog(8). launchd runs it hourly at minute 30
  (`/System/Library/LaunchDaemons/com.apple.newsyslog.plist`). It reads
  `/etc/newsyslog.conf` and `/etc/newsyslog.d/*.conf`, a root-owned
  directory, so adding a rule needs sudo. Rules can rotate by size or time,
  compress the old file, signal a process through a PID file, or use `N` for
  no signal. Rotated files default to `root:admin` unless `owner:group` is
  given (newsyslog.conf(5), newsyslog(8), local man pages).
- Inference: launchd opens the stdout and stderr files once at spawn. After
  newsyslog renames the file, the process keeps writing to the renamed file
  until it restarts, unless it reopens the path itself and `dup2`s the
  result onto fd 1 and 2.
- Options:
  1. **App-managed rotation (recommended, inference).** cuttle writes its
     own log to `$XDG_STATE_HOME/cuttle/cuttle.log`, rotates by size, keeps
     a bounded number of files, and writes almost nothing to stdout or
     stderr. The service block's `var/log/cuttle.log` then receives only
     panics and pre-logger errors and stays small. This needs no root and
     works the same for the CLI and the service.
  2. A newsyslog rule with the `N` flag (or a signal and PID file) and an
     explicit `owner:group`. This needs sudo and conflicts with the no-sudo
     installation.
  3. No rotation. The file grows without bound.

## Open questions

Each item names the smallest controlled check that would settle it. None of
them was run because the brief forbids loading launchd jobs and installing
formulae.

1. **SIGTERM, grace period, and `stop_timeout` end to end.** Bootstrap a
   scratch LaunchAgent (label `test.cuttle.sigterm`, plist in a scratch
   directory, `ExitTimeOut` unset and then 15) whose program logs signals
   and sleeps 30 s before exiting. Run `launchctl bootout` and record the
   SIGTERM time, the SIGKILL time, and the exit status from
   `launchctl print`. This confirms the 5 s default and the effect of
   `stop_timeout`. It needs user approval because it changes launchd state.
2. **Environment and `.env` override under `brew services`.** Install a
   throwaway formula from a local tap with
   `run ["/usr/bin/env"]` and `log_path`. Run `brew services run` (no login
   registration) with and without
   `~/.homebrew/services/<name>.env` containing `XDG_CONFIG_HOME=/tmp/x`,
   and diff the logged environment.
3. **`ProcessType` and proxy throughput.** In the M2 T3 baseline, measure
   cuttle throughput and latency under `brew services` with `process_type`
   unset and with `:interactive`, plus a foreground run as control.
4. **Uninstall leftovers.** With the throwaway formula's service running,
   run `brew uninstall <name>` and record whether the plist, the launchd
   job, and `var/log` remain. Then run `brew services cleanup`.
5. **Loopback TCP in the test sandbox.** Run `brew test` on a formula whose
   test binds `127.0.0.1:free_port`, with and without
   `deny_network_access! :test`. This decides whether the test needs
   `allow_network_access! :test` or a Unix socket.
6. **`sudo` and `Dir.home`.** Run `sudo ruby -e 'puts Dir.home'` and
   `sudo brew services info --json <name>` with a throwaway service. This
   only matters if sudo mode is ever supported.
7. **Tap hosting and source download.** The repository has no remote yet.
   If it becomes private, `url` needs an authenticated download strategy
   (`HBD/Homebrew-Fleet-Management.md`, private taps). The check is to
   create the tap and run `brew fetch --build-from-source <owner>/tap/cuttle`.
8. **Homebrew build of the chosen driver.** Run
   `brew install --build-from-source` on a tap formula that links
   modernc.org/sqlite, and record the build time and binary size with the
   `fetch` and offline `install` split.

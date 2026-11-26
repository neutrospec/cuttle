# Lessons

Repeated failures and how to avoid them. Read this file before starting any
task. The [lesson rules](../AGENTS.md#lessons) say who writes, edits, and
removes entries. Append new entries at the end, oldest first.

```text
### YYYY-MM-DD — role — short task name

- Task: Task ID if any, and what the agent was trying to do.
- Attempts: Number of attempts.
- What failed: Failure and the relevant command or tool call.
- What to do instead: The working approach.
```

## Entries

### 2026-11-25 — researcher — Homebrew, launchd, and XDG research

- Task: TASK-0001, running shell commands for research note 0001.
- Attempts: 2.
- What failed: The Bash tool runs zsh, which aborts a command when an
  unquoted glob matches nothing. `grep -rn PATTERN DIR --include=*.rb`
  failed with `no matches found: --include=*.rb`, and
  `curl -sSf https://api.github.com/repos/OWNER/REPO/tags?per_page=1`
  failed with `no matches found: https://...?per_page=1`. zsh also does not
  word-split unquoted variables, so `set -- $q` inside a loop put the whole
  string in `$1`.
- What to do instead: Quote every argument that contains `*`, `?`, or `[`,
  for example `--include='*.rb'` and `"https://...?per_page=1"`. For loops
  that split strings into fields, write a POSIX script file and run it with
  `sh script.sh`.

### 2026-11-25 — researcher — proxy chaining and interception research

- Task: TASK-0001, client tests of proxy failure responses for research note
  0002.
- Attempts: 1 visible failure, recorded because it reached the user's screen.
- What failed: A command-line Swift probe used `URLSession` with a per-session
  proxy and default authentication handling
  (`./cfnet/probe certs/ca.pem ... https://s407.cuttle.test/cf-c407`). The mock
  proxy answered `407` with `Proxy-Authenticate: Basic`, and macOS showed a
  proxy sign-in dialog on the user's desktop. Go `Transport`,
  `/usr/bin/curl`, and headless Chrome with `--use-mock-keychain` showed no
  dialog for the same response.
- What to do instead: Never let a mock proxy answer `407` to an Apple-framework
  client (`URLSession`, `CFNetwork`, Safari) on the user's desktop. If the case
  must be tested, refuse the challenge in the client's authentication handler.
  Get user approval before any client test that can open a window.

### 2026-11-25 — researcher — WebKit sources for Safari behavior

- Task: TASK-0001, Safari proxy behavior for research note 0002.
- Attempts: 2.
- What failed: `bugs.webkit.org` timed out with `curl -sSL -m 60` (exit 28)
  and with the web fetch tool.
- What to do instead: Use the Safari release notes on `webkit.org` or the
  WebKit source on GitHub. Do not retry `bugs.webkit.org` more than once.

### 2026-11-25 — reviewer — publication scan in the Bash tool

- Task: TASK-0003, checking that the history scan finds every hit.
- Attempts: 1 hidden failure, recorded because it can make a leak scan
  report 0.
- What failed: In the agent Bash tool, `grep` is a shell function that runs
  ugrep with `-I --ignore-files`, so a plain `grep` over files silently
  skips binary and gitignored files. `git grep -I` also skips binary files
  without a message.
- What to do instead: For scans that must not miss anything, call
  `/usr/bin/grep` or run the script with `sh`, and do not pass `-I` to
  `git grep`. Add a positive control that plants a known hit.

### 2026-11-25 — implementer — strict gate script under `sh`

- Task: TASK-0004, making `scripts/gate` fail when a command inside a check
  fails.
- Attempts: 1, recorded because the obvious fix silently does nothing.
- What failed: Under `sh` with `set -e`, a failing command on the left of
  `&&`, as in `out=$(false) && test -z "$out"`, does not stop the script,
  and neither does one inside `test -z "$(cmd)"`.
- What to do instead: Capture with a plain assignment, `out=$(cmd)`, which
  takes the command's exit status, then run `test -z "$out"` on its own
  line. Verify by injecting a failing command and checking that the script
  exits non-zero.

### 2026-11-25 — reviewer — injecting failures into a gate script

- Task: TASK-0004, proving that `scripts/gate` fails when a command fails.
- Attempts: 1, recorded because it exhausted the machine's process slots.
- What failed: Wrapper scripts placed first on `PATH` called the real tool
  through `command -v <tool>`. For `git`, that resolved to another wrapper
  that looks up `git` on `PATH` again, so the two wrappers called each other
  in a loop until `fork: Resource temporarily unavailable`.
- What to do instead: Do not shadow tools on `PATH`. Inject failures by
  editing a scratch copy of the script with `sed`, for example
  `go list -deps ./cmd/no-such-pkg`. If a wrapper is unavoidable, point it
  at an absolute path that you have checked is the real binary.

### 2026-11-25 — implementer — unquoted grep patterns again

- Task: TASK-0005, searching the tree for identifiers while implementing
  M1.2.
- Attempts: 2, recorded because the 2026-11-25 researcher entry already
  covers the failure and it still recurred.
- What failed: `grep -rn PATTERN docs README.md --include=*.md` and
  `grep -rn PATTERN --include=*.go .` both stopped with
  `(eval):1: no matches found: --include=*.md` before grep ran, because
  the Bash tool's zsh expands the unquoted glob in the current directory.
- What to do instead: Quote every `--include` value, as in
  `grep -rn 'PATTERN' --include='*.go' .`, every time, also in quick
  one-off searches.

### 2026-11-25 — implementer — mutation checks in a scratch copy

- Task: TASK-0006, proving the M1.3 tests by applying one mutation per
  scratch copy of the worktree with a Python script and running the
  targeted test.
- Attempts: 3 for one mutation, recorded because the script reported
  "PATTERN NOT FOUND" twice and a silently skipped mutation looks like a
  passing check.
- What failed: The first pattern was written from memory and did not
  match the code. The fix was then applied with
  `python3 - <<EOF ... s.replace('''"\\tcase ...''', ...) EOF`: the
  unquoted heredoc let the shell rewrite the backslashes, so the script
  file was left unchanged and the run reported the same miss.
- What to do instead: Copy the pattern from the file, for example with
  `/usr/bin/grep -n -A12 '^func (h \*hold) admit' FILE | od -c`, and edit
  helper scripts with a quoted heredoc (`<<'EOF'`), passing paths as
  arguments. Make the mutation script fail loudly when a pattern is
  missing, and count a mutation as checked only when the targeted test
  fails. A mutation that survives may also mean that the test does not
  reach the code: Go's `Transport.CloseIdleConnections` also closes
  connections that become idle later, until the next request queues on
  the transport, so a pooled-connection test needs such a request.

### 2026-11-25 — reviewer — stopping a backgrounded binary in `sh`

- Task: TASK-0007, probing `cuttle serve` shutdown from a script.
- Attempts: 1, recorded because it left a `cuttle serve` process running.
- What failed: With `run()` defined as a shell function, `run serve &` made
  `$!` the PID of a subshell, so `kill -TERM $!` stopped the subshell and
  left the `cuttle` process running.
- What to do instead: Background the binary itself (`./cuttle serve &`), or
  record its PID another way, and confirm with `pgrep` that no `cuttle`
  process remains before finishing.

### 2026-11-25 — orchestrator — scripted edits of the task board

- Task: TASK-0001 to TASK-0008, updating and closing entries in
  docs/TASKS.md with short Python scripts.
- Attempts: 2 failures of the same kind.
- What failed: The scripts located text with `s.index(...)` and assumed an
  order. Once, `s.index('- Status: design')` matched the entry template
  above the real entry and rewrote the template. Later, closing TASK-0007
  sliced from `### TASK-0007` to the first `### TASK-0008`, which came
  earlier in the file, so `s[:start] + s[end:]` duplicated two entries.
- What to do instead: Search from the start of the target entry, assert
  that each start index is below its end index and that each heading
  occurs once, and print the board's headings after every edit before
  committing.

### 2026-11-25 — implementer — mutations that do not compile

- Task: TASK-0009, proving each M1.5 test against a mutation in a scratch
  copy of the worktree.
- Attempts: 2; five mutations and one test had to be redone.
- What failed: Mutations that replaced a condition with `false`, or a
  function body with a constant, left a loop variable, a local, or an
  import unused, for example `if i < len(groups)-1 {` → `if false {` and
  `return strings.Join(list, ",")` → `return ""`. The scratch copy failed
  with `declared and not used` or `imported and not used`, `go test`
  exited 1, and only the script's check for `[build failed]` kept those
  runs from counting as proofs. Separately, a test whose failure path read
  a result channel a second time blocked until `-timeout 120s` instead of
  failing with its message.
- What to do instead: Keep the original expression inside the mutation,
  as in `if false && i < len(groups)-1 {` or
  `return strings.Join(nil, ",")`, and count a run as a proof only when
  the targeted test fails with its own message, not a build error or a
  timeout. Make every failure path of a test finish, for example by
  remembering a value already received from a channel.

### 2026-11-25 — reviewer — probing `cuttle serve` by hand

- Task: TASK-0011, a live probe of the admin listener with a built binary.
- Attempts: 1, recorded because it touched the real control socket path.
- What failed: The probe set `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, and
  `XDG_STATE_HOME` but not `XDG_RUNTIME_DIR`, so the control socket fell
  back to `$TMPDIR/cuttle/control.sock`, the path a real instance uses.
- What to do instead: Set `XDG_RUNTIME_DIR` to a short 0700 directory under
  `/tmp` as well, together with `HOME` and the other `XDG_*` variables,
  whenever a probe runs `cuttle serve` outside the test helpers.

### 2026-11-25 — implementer — mutation run against the wrong package

- Task: TASK-0012, proving `tools/mockenv` tests with one mutation per
  scratch copy, where the mutation script ran
  `go test -run <Test> ./<directory of the mutated file>`.
- Attempts: 2 for one mutation, recorded because the failure looks like a
  test gap rather than a script error.
- What failed: A mutation in `internal/testenv/upstream.go` meant for
  `TestServeSession` in `tools/mockenv` ran
  `go test -run TestServeSession ./internal/testenv`. No test matched, so
  `go test` printed `ok ... [no tests to run]`, exited 0, and the script
  reported the mutation as surviving. A second mutation was killed, but
  the script's expected-message pattern used spaces where Go prints
  subtest names with underscores, so it was reported as failing without
  the expected message.
- What to do instead: Give each mutation the package of the test that
  should catch it, and treat `no tests to run` in the output as an
  invalid run. Match expected messages against the test's own failure
  text, and for subtests use the underscore form, as in
  `TestCheckDisclosure/disclosure_field_outside_the_case_targets`.

### 2026-11-26 — infra — Homebrew checks leave state outside the tap

- Task: TASK-0015, installing a formula from a throwaway tap and running
  `brew services` checks that had to leave nothing behind.
- Attempts: 1, recorded because the obvious cleanup (`brew services stop`,
  `brew uninstall`, `brew untap`) misses three side effects, and one of
  them defeats `brew services cleanup`.
- What failed: `brew install --dry-run <user>/<tap>/<formula>` already
  added the formula to `trustedformulae` in
  `$HOMEBREW_USER_CONFIG_HOME/trust.json`. `brew uninstall` with the
  service running left the job running from the deleted keg, with its
  plist in `~/Library/LaunchAgents`, and `brew services cleanup` printed
  `Service sh.brew.cuttle not managed by brew services => skipping`. The
  build created `$(brew --cache)/go_mod_cache` and `go_cache`, about
  500 MB, and `brew services start` left `"sh.brew.cuttle" => enabled`
  under `disabled services` in `launchctl print gui/$(id -u)`, which only
  root can remove.
- What to do instead: Before the check, record `shasum -a 256` of
  `trust.json`, `launchctl print gui/$(id -u) | grep <name>`, and the
  birth times (`stat -f %SB`) of the cache directories. Always run
  `brew services stop` before `brew uninstall`; for a job left running,
  run `launchctl bootout gui/$(id -u)/<label>` and then
  `brew services cleanup`. `brew untap` removes the trust entry. Remove the
  Go caches only when their birth time shows the check created them, with
  `GOMODCACHE=<cache>/go_mod_cache/pkg/mod go clean -modcache` first,
  because module files are read-only. Report the launchd record.

### 2026-11-26 — reviewer — identity controls in a shell harness

- Task: TASK-0015, positive controls for `scripts/publish-scan`'s
  identity check, written as POSIX `sh` functions in a scratch harness.
- Attempts: 2 for two controls.
- What failed: `GIT_AUTHOR_NAME=Bob as_alice git commit …` did not make
  Bob the author, because `as_alice` set `GIT_AUTHOR_*` as a prefix on its
  own `git` command, and that inner prefix overrides the caller's. The
  control looked like a missed identity (the scan exited 0).
- What to do instead: Set the identity variables directly on each `git`
  command of a control, and check the planted value with `git log
  --format='%an <%ae>'` before counting a pass as a miss.

### 2026-11-26 — implementer — TOML samples inside Go string literals

- Task: TASK-0018, changing one line of a TOML sample written as a
  double-quoted Go string in `internal/config/edit_test.go`.
- Attempts: 3 for one sample line.
- What failed: `sed -i '' "s/.../.../"` with the Go escapes of TOML
  quotes (`\"`, `\\`) matched nothing and exited 0, so the file was
  unchanged. A Python replacement then wrote `'it"s'` into the Go string,
  which ended the literal (`expected ';', found s`). After that, the
  sample `t = """\""""` had been written as `\"\"\"\\\"\"\"`, which is
  `"""\"""` in TOML, an unterminated string; the comment counter under
  test still returned a count, and only a check added later showed that
  the sample was not TOML.
- What to do instead: Write TOML samples in tests as Go raw strings
  (backticks) where they hold no backtick, edit them with the exact-text
  Edit tool rather than `sed`, and assert in the test that the sample
  decodes with `toml.Decode` before using it, so an invalid sample fails
  loudly.

### 2026-11-26 — implementer — waiting on a DOM change in a browser check

- Task: TASK-0019, checking in `TestAdminScreens` that Validate replaces
  the Advanced page's result block through a fragment request.
- Attempts: 3.
- What failed: The check clicked `[data-fragment="settings-result"]` and
  polled for `#settings-result .issue-list`. The poll returned at once,
  because the page already listed the file's warnings, and the check read
  the old block. After that was fixed, the selector matched the block's
  own root element, which also carried `data-fragment`, before the
  button, so the click did nothing and the poll timed out.
- What to do instead: Capture the node that the request replaces
  (`const old = document.querySelector("#target > [data-block]")`) and
  poll until a different node is there. Keep trigger and content markers
  apart: triggers carry `data-fragment="<target id>"`, and fragment roots
  carry `data-block="<name>"`.

### 2026-11-26 — implementer — the first match in a page with a shell

- Task: TASK-0019 iteration 2, browser checks in `TestAdminScreens` for
  the refused-fragment messages and the phone table rows.
- Attempts: 2 failures of the same kind as the entry above, and one
  check that a mutation survived.
- What failed: `document.querySelector('input[name="csrf"]')` returned the
  sidebar's sign-out form, which comes before the editor in the page, so
  the probe changed the wrong token, the request succeeded, and the check
  saw no refusal. Separately, a check for misaligned phone rows compared
  `getBoundingClientRect().left` of the cells; the defect was padding
  inside the cell, so the check passed with the defect restored.
- What to do instead: The admin shell repeats the navigation and the
  sign-out form (sidebar and phone menu), so scope every selector to the
  element under test, such as `.editor-form input[name="csrf"]`. Measure
  what the operator sees: the content edge (`left` plus border and
  padding) or the text's `Range` rects, and prove each layout check by
  restoring the defect once.

### 2026-11-26 — implementer — plus signs in html/template output

- Task: TASK-0020, asserting on the review's diff marks and change
  summary in the rendered admin pages.
- Attempts: 2 failures of the same kind.
- What failed: `html/template` writes `+` in text as `&#43;`. A regular
  expression over the diff rows compared the mark cell with `"+"`
  (`mark "&#43;" for " add"`), and after that was fixed, a check for
  `"Rule <code>restricted</code>: domains +1"` failed the same way.
- What to do instead: Unescape captured text with `html.UnescapeString`
  before comparing it, or write the expected literal in its escaped form
  (`domains &#43;1`). `&#34;` and `&#39;` replace `"` and `'` the same
  way.

### 2026-11-26 — reviewer — browser checks from two worktrees at once

- Task: TASK-0021 and TASK-0022, run in parallel worktrees, each running
  `scripts/admin-check`.
- Attempts: 2 (the TASK-0021 implementer and the TASK-0022 reviewer each
  hit it once).
- What failed: the script ends with `/usr/bin/pgrep -f
  'cuttle-bin-|TestAdminChrome|TestAdminScreens'`, which matched the other
  worktree's concurrent run, so it printed `admin-check: processes left:
  <pid>` and exited 1 although every test passed.
- What to do instead: before counting that failure, check whose checkout
  the process belongs to with `lsof -a -p <pid> -d cwd` and its start
  time, and rerun when `pgrep` finds nothing. OI-0006 AB-R4 tracks
  scoping the pattern to the run's own temporary directories.

### 2026-11-26 — reviewer — mutation run against a package that does not build the file

- Task: TASK-0022, a mutation of `cmd/cuttle/serve.go`.
- Attempts: 2.
- What failed: `go test -run '^TestEnsureStarterNotSynced$'
  ./internal/config` ran an existing test that never compiles
  `cmd/cuttle`, so it passed and looked like a surviving mutation; the
  `no tests to run` check from the 2026-11-25 entry does not catch this.
- What to do instead: run each mutation against the package that contains
  the mutated file, or one that imports it, and confirm with `go list
  -deps <pkg> | grep <mutated package>`.

### 2026-11-26 — implementer — multi-file edit scripts that stop halfway

- Task: TASK-0021 iteration 2, editing several files with one
  `python3 - a b c <<'EOF'` script that asserted each pattern just before
  replacing it in that file.
- Attempts: 2 failures of the same kind.
- What failed: a pattern copied from memory did not match (`gofmt` had
  written `"`+fd.Name+`"` without the spaces I typed, and one test line had
  changed in an earlier step), so the script raised `AssertionError` after
  it had already written the files before it. The first time nothing was
  written; the second time three of four files were changed and the run
  looked like a total failure, so the partial state was easy to miss.
- What to do instead: read every pattern from the file first (`grep -n`
  or `sed -n`), and in the script check all patterns of all files before
  writing any file; after a failure, run `git diff --stat` to see what
  was written.

### 2026-11-26 — implementer — html/template and `data-*action*` attributes

- Task: TASK-0023, a Route… button carrying the chosen route in
  `data-action="upstream:gateway"`.
- Attempts: 1, recorded because the failure is silent.
- What failed: `html/template` treats any attribute whose name contains
  `action` (also `src`, `href`, `uri`, `url`) as a URL, so a value such as
  `upstream:gateway` reads as an unsafe scheme and is replaced with
  `#ZgotmplZ`; nothing fails at build or render time.
- What to do instead: name such attributes without those words (here
  `data-route`), and assert the rendered attribute value in a test.


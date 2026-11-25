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

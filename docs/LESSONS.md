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

# Open issues

Record significant work that carries beyond the three-iteration limit or
blocks a milestone. Give each item an `OI-NNNN` ID, origin, affected milestone,
iterations used, responsible step, status, blocking reason, and next action.
Move resolved items to Closed with their resolving commit or evidence.

## Open

### OI-0001 — Real corporate proxy and office VPN behavior

- Origin: TASK-0001 design, [0003 assumptions][d3-a] A1–A4; research 0002
  [open question 6][r2-oq].
- Affected milestone: M3.3.
- Iterations used: none; recorded in TASK-0001 iteration 1.
- Responsible step: test, run by the user on the office Mac.
- Status: open.
- Blocking reason: Only the user can reach the corporate proxy and the VPN,
  and research was not allowed to contact a real proxy. The design assumes
  the proxy accepts `Via` on `CONNECT`, its re-signed leaves meet Safari's
  requirements, a listener bound to the VPN address survives a VPN
  reconnect, and the macOS firewall prompt appears on the first
  non-loopback bind.
- Next action: tester — give the user commands to send one `CONNECT` to an
  allowed site with and without `Via: 1.1 cuttle-office`; record the
  answers, the proxy's own `Via` text, how it refuses a site, and its
  `CONNECT` timeout; run
  `openssl s_client -proxy <corp>:<port> -connect <site>:443 -showcerts`
  and record SAN, EKU, and validity; reconnect the VPN under a bound office
  listener and connect from home; note any firewall prompt, also after an
  upgrade.

### OI-0002 — Safari and CFNetwork behavior unverified

- Origin: research 0002 [open questions 1, 7, and 8][r2-oq];
  [0005 assumption B1][d5-a].
- Affected milestone: M3.2. M1 accepts CFNetwork's bracketless IPv6
  `CONNECT` on CFNetwork evidence alone.
- Iterations used: none; recorded in TASK-0001 iteration 1.
- Responsible step: test.
- Status: open.
- Blocking reason: Safari checks change the macOS system proxy settings and
  can open windows, which needs user approval.
- Next action: tester — with user approval, run the Safari rows of the
  [0007 D8 matrix][d7-8], recording the IPv6 `CONNECT` spelling, the error
  UI for `403`, `502`, and `504`, the proxy entry used for `ws://`, whether
  Safari bypasses loopback, and whether the admin session cookie is kept;
  restore the settings.

### OI-0003 — Homebrew and launchd behavior unverified

- Origin: research 0001 [open questions 1, 2, 4, 5, and 8][r1-oq];
  [0006 assumptions][d6-a] C2–C4; [0003 assumption A5][d3-a].
- Affected milestone: M3.1.
- Iterations used: none; recorded in TASK-0001 iteration 1.
- Responsible step: test, with infra for the tap tooling.
- Status: open.
- Blocking reason: The checks load launchd jobs and install formulae, which
  needs user approval.
- Next action: tester — with approval and a throwaway formula, record
  `SIGTERM` and `ExitTimeOut` timing, the `.env` override, uninstall
  leftovers, loopback TCP in `brew test`, the modernc.org/sqlite build under
  Homebrew, and the open-file limit of the agent, before the M3.1 formula
  is final.

### OI-0004 — Tap hosting, repository remote, and licence

- Origin: research 0001 [open question 7][r1-oq]; [0006 D8][d6-8].
- Affected milestone: M3.1.
- Iterations used: none; recorded in TASK-0001 iteration 1.
- Responsible step: user decision, then infra.
- Status: open.
- Blocking reason: The repository has no remote. The GitHub owner of the
  repository and the tap, public or private hosting, and the licence are
  undecided. A private repository needs an authenticated download strategy
  in the formula.
- Next action: orchestrator — ask the user for the owner, visibility, and
  licence before M3.1 starts.

### OI-0005 — Private-root trust through the chain

- Origin: research 0002 [open questions 3 and 4][r2-oq]; [0003 D11][d3-11].
- Affected milestones: M3.2 and M3.3.
- Iterations used: none; recorded in TASK-0001 iteration 1.
- Responsible step: test.
- Status: open.
- Blocking reason: Trusting a root in the keychain changes the user's trust
  settings and should happen only in a disposable account or VM, with user
  approval.
- Next action: tester — in a disposable account or VM, mark the mockenv
  corporate root "Always Trust", load the chain case in Chrome without the
  SPKI flag and in Safari, and run `/usr/bin/curl` without CA options;
  record the results for the deployment guide.

## Closed

[d3-11]: design/0003-upstream-chaining-and-exposure.md#d11-home-office-and-corporate-deployment
[d3-a]: design/0003-upstream-chaining-and-exposure.md#assumptions
[d5-a]: design/0005-statistics-and-admin.md#assumptions
[d6-8]: design/0006-packaging-paths-and-lifecycle.md#d8-tap-and-formula
[d6-a]: design/0006-packaging-paths-and-lifecycle.md#assumptions
[d7-8]: design/0007-test-environment-and-gates.md#d8-t2-procedure
[r1-oq]: research/0001-homebrew-launchd-xdg.md#open-questions
[r2-oq]: research/0002-proxy-chaining-and-interception.md#open-questions

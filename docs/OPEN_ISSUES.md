# Open issues

Record significant work that carries beyond the three-iteration limit or
blocks a milestone. Give each item an `OI-NNNN` ID, origin, affected milestone,
iterations used, responsible step, status, blocking reason, and next action.
Move resolved items to Closed with their resolving commit or evidence.

## Open

### OI-0001 — Real egress proxy and private link in the example deployment

- Origin: TASK-0001 design, [0003 assumptions][d3-a] A1–A4 and the
  [private-link cases of 0003 D13][d3-13]; research 0002
  [open question 6][r2-oq].
- Affected milestone: M3.3.
- Iterations used: none; recorded in TASK-0001 iteration 1 and revised in
  iteration 2.
- Responsible step: test, run by the user on the gateway and edge machines
  of the example deployment in [0003 D13][d3-13].
- Status: open.
- Blocking reason: Only the user can reach that egress proxy and private
  link, and research was not allowed to contact a real proxy. The user
  requires that requests reach the egress proxy without disclosing an
  intermediary, so the design uses `disclosure = "none"` toward it and
  assumes the proxy accepts such requests with standard header handling.
  It also assumes the re-signed leaves meet Safari's requirements, a
  listener bound to the private-link address survives a link restart, and
  the macOS firewall prompt appears on the first non-loopback bind. Whether
  the edge-side link server routes or masquerades edge LAN traffic is
  unknown.
- Next action: tester — give the user steps to:
  1. Start the gateway instance with the placeholder
     `sources = ["192.0.2.1/32"]`, send one request from the edge machine
     through the edge instance, and read the gateway log's rejected-source
     warning: the edge machine's LAN address means routed, the link
     server's address means NAT. Set `sources` accordingly.
  2. Confirm `disclosure = "none"` against the real egress proxy: load a
     few restricted destinations in Chrome on the gateway machine directly
     through the egress proxy, then from Chrome and curl on the edge
     machine through the edge → gateway chain, and record that each
     succeeds or fails the same way. Before that, point the gateway
     instance's `egress` upstream at a local recording mock once and
     confirm that the recorded requests disclose neither cuttle hop.
  3. Record how the egress proxy refuses a site and its `CONNECT` timeout,
     and run
     `openssl s_client -proxy <egress>:<port> -connect <site>:443 -showcerts`
     to record SAN, EKU, and validity.
  4. Restart the private link under a bound gateway listener and connect
     from the edge machine; note any firewall prompt, also after an
     upgrade.

  The step inputs come from the root `.env` (see
  [`.env.example`](../.env.example)).

### OI-0002 — Safari and CFNetwork behavior unverified

- Origin: research 0002 [open questions 1, 7, and 8][r2-oq];
  [0005 assumption B1][d5-a].
- Affected milestone: M3.2. In M1, Go's server rejects CFNetwork's
  bracketless IPv6 `CONNECT` with `400`, a documented limitation. The
  Safari check decides whether cuttle must accept that form (research open
  question 8).
- Iterations used: none; recorded in TASK-0001 iteration 1.
- Responsible step: test.
- Status: open.
- Blocking reason: Safari checks change the macOS system proxy settings and
  can open windows, which needs user approval.
- Next action: tester — with user approval, run the Safari rows of the
  [0007 D8 M3 matrix][d7-8], recording the IPv6 `CONNECT` spelling, the
  error UI for `403`, `502`, and `504`, for `ws://` both the proxy entry
  Safari uses and whether it sends `CONNECT` to port 80 or an absolute-form
  upgrade, whether Safari bypasses loopback, whether the non-disclosure
  properties hold for Safari, and whether the admin session cookie is kept;
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
  licence before M3.1 starts. Publication follows these rules, because the
  TASK-0003 history rewrite left pre-rewrite history in local refs and
  files:
  1. Before the first push, run `git config --get-regexp '^remote\.'` and
     confirm that no remote has `mirror` set and that there is no push or
     mirror refspec whose source is `refs/*` or `refs/t3/*` (for example
     `+refs/*:refs/*`). The default fetch refspec,
     `+refs/heads/*:refs/remotes/<name>/*`, is fine.
  2. Push only `refs/heads/main` and tags, never `--mirror`, a `refs/*`
     refspec, or anything else that syncs `refs/t3/*`.
  3. Keep `refs/t3/*` private permanently. They contain pre-rewrite
     history, and T3 keeps adding checkpoints that can capture untracked
     files.
  4. The reflogs, `.git/filter-repo/`, the backup bundle, and the commit
     map hold pre-rewrite history. Never copy or share the working folder
     or the bundle.
  5. After pushing, check that `git ls-remote <remote>` lists only `HEAD`,
     `refs/heads/main`, and tags. Then scan a fresh clone of the remote:
     the file contents of every commit, the commit messages without the
     author and committer lines (the author email is allowed), the tag
     messages, and the tip tree. The patterns are local values, so they
     live in `CUTTLE_PUBLISH_SCAN_PATTERNS` in the untracked root `.env`
     (see [`.env.example`](../.env.example)). Run the commands from the
     main checkout's root. Each count must be 0; leave out `| wc -l` to see
     the matching lines.

     ```sh
     set -a; . ./.env; set +a
     p=$CUTTLE_PUBLISH_SCAN_PATTERNS
     d=$(mktemp -d)
     git clone -q <remote-url> "$d/clone" && cd "$d/clone"
     git rev-list --all | while read -r r; do
       git grep -I -i -E -e "$p" "$r" --
     done | wc -l
     git log --all --format=%B | grep -i -E -e "$p" | wc -l
     git for-each-ref --format='%(contents)' refs/tags |
       grep -i -E -e "$p" | wc -l
     git grep -I -i -E -e "$p" HEAD -- | wc -l
     cd - >/dev/null && rm -rf "$d"
     ```
  6. Delete or securely store the bundle and the commit map once the user
     approves.

### OI-0005 — Private-root trust through the chain

- Origin: research 0002 [open questions 3 and 4][r2-oq]; [0003 D13][d3-13].
- Affected milestones: M3.2 and M3.3.
- Iterations used: none; recorded in TASK-0001 iteration 1.
- Responsible step: test.
- Status: open.
- Blocking reason: Trusting a root in the keychain changes the user's trust
  settings and should happen only in a disposable account or VM, with user
  approval.
- Next action: tester — in a disposable account or VM, mark the mockenv
  inspection root CA "Always Trust", load the chain case in Chrome without
  the SPKI flag and in Safari, and run `/usr/bin/curl` without CA options;
  record the results for the deployment guide.

### OI-0006 — Design follow-ups from the TASK-0001 final review

- Origin: TASK-0001 iteration 3 review of candidate `a1e1e61`, verdict
  "approve with minor fixes", findings V1–V12. Landed in `dc52251`. P1
  comes from the TASK-0002 review.
- Affected milestones: M1.2, M1.3, and M1 close; V5 also M3.2; P1 M3.3.
- Iterations used: 3 of 3, so the fixes move to the first M1 task instead of
  another design loop.
- Responsible step: design for wording and criteria, implementer for tests.
- Status: open.
- Blocking reason: None of the findings blocks approval, but each must be
  settled before the stage it affects closes.
- Items:
  - V1 (design): 0003 N3 asserts an upstream route in M1.2, which is
    delivered in M1.3; limit N3 to decisions M1.2 can show. Move the
    roadmap's non-loopback bind retry sentence from M1.2 to M1.4.
  - V2 (implementer): add checks that client-sent `Forwarded` and
    `X-Forwarded-*` fields pass unchanged on `disclosure = "via"` routes,
    on the CONNECT head and the plain-HTTP copy-back (U1, U5). Add an
    `HTTP_PROXY` environment check to C3 or drop the 0002 D5 claim that
    every row has a check.
  - V3 (design, implementer): check the unreachable hold in the handler
    before `ReverseProxy`, as for CONNECT, and close idle pooled connections
    when an upstream goes down (0003 D4, D8, U11).
  - V4 (implementer, before M1 close): isolate headless Chrome's background
    requests in the M1 client check. mockenv configs use
    `default = "block"` with rules only for `cuttle.test` and loopback,
    recording upstreams answer 403 outside `cuttle.test`,
    `check-disclosure` compares only the case targets and fails on an
    unmatched one, and the Block evidence names `blocked.cuttle.test`.
  - V5 (design): Go rejects the bracketless IPv6 CONNECT before any handler,
    so the M3 IPv6 row points the client at the recording mock directly, as
    research 0002 did. Tracked with OI-0002.
  - V6 (user decision, 2026-11-25): private IP literals and this Mac's own
    address stay allowed for local clients without a rule. Record in 0003
    D11 that this is an assumption, and add to the M1 headless Chrome check
    whether a public-origin page's fetch of a private literal through cuttle
    still triggers Chrome's Local Network Access permission. If it does not,
    return the decision to the user.
  - V7 (implementer): mention Go's 500 header-value cap
    (`DefaultMaxHeaderValueCount`) in 0002 D2 and build the C11 60 KiB head
    from fewer than 500 field lines.
  - V8 (design, docs): wording fixes in 0002 D5 (the sentence that
    `RequestURI()` returns `URL.Opaque` unchanged, although it prefixes
    `scheme:` for `//` opaque values), 0003 D5 (where the disclosure filter
    is called), the 0003 alternative "Forwarding request heads byte for
    byte", OI-0001 step 2, the 0007 D7 `check-disclosure` flags, the 0007
    D8 chain evidence, 0005 A10 "T2-lite", the COMPATIBILITY curl M3
    WebSocket cell, and ragged wraps in 0003.
  - V9 (implementer): C4 sends the direct request before the block request,
    because cuttle's own responses close the connection.
  - V10 (implementer): add a `/status?code=` endpoint to the mock origin for
    C10; mockenv refuses 407 on it.
  - V11 (docs): add Consequences sections to records 0003–0007 or relax the
    convention in the design README.
  - V12 (design): note `proxy.remote.ports` in the 0003 D13 deployment notes
    for restricted destinations on ports other than 80 and 443.
  - P1 (from the TASK-0002 review; design): a private link can drop a
    packet whose inner source address it does not expect; WireGuard, for
    example, drops one that is not in the peer's `AllowedIPs`. In the
    routed case of 0003 D13 without the edge LAN listed, the request then
    never reaches the gateway instance, and the D13 observation step shows
    no rejected-source warning. This is an inference and was not tested.
    Do not change the design until the inference is confirmed. Settle it
    with a researcher from the private link's published routing rules
    before M3.3.
  - W1–W3 (from the TASK-0002 re-check; docs): in 0003 D5, word "so a
    strict upstream has nothing to reject" as a definition rather than a
    guarantee, since assumption A1 is unconfirmed, and rewrap that
    paragraph. Optionally mention the start-up log line in the glossary
    Instance entry (0004 D3, 0006 D5). In P1, say the edge LAN must be in
    the gateway side's `AllowedIPs` or the link's equivalent, and add to
    OI-0001 step 1: if no warning appears, see OI-0006 P1.
- Next action: orchestrator — include V1–V12, P1, and W1–W3 in the first M1
  task brief; the producer closes each item with the commit that settles
  it.

## Closed

[d3-13]: design/0003-upstream-chaining-and-exposure.md#d13-edge-gateway-and-egress-deployment
[d3-a]: design/0003-upstream-chaining-and-exposure.md#assumptions
[d5-a]: design/0005-statistics-and-admin.md#assumptions
[d6-8]: design/0006-packaging-paths-and-lifecycle.md#d8-tap-and-formula
[d6-a]: design/0006-packaging-paths-and-lifecycle.md#assumptions
[d7-8]: design/0007-test-environment-and-gates.md#d8-client-checks
[r1-oq]: research/0001-homebrew-launchd-xdg.md#open-questions
[r2-oq]: research/0002-proxy-chaining-and-interception.md#open-questions

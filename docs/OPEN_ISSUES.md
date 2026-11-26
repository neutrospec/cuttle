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

### OI-0007 — Local network privacy for cuttle under launchd

- Origin: research 0003 [local network privacy][r3-lnp], TASK-0008.
- Affected milestones: M1.5, M3.1, and M3.3.
- Iterations used: none; recorded in TASK-0008.
- Responsible step: research, then design.
- Status: open.
- Blocking reason: macOS 15 and later apply local network privacy to
  outgoing connections toward local network addresses. It covers launchd
  agents, so cuttle under `brew services` may need the user's permission
  before it can reach a LAN destination, such as a private destination
  behind an `allow_private` rule or an egress proxy on the gateway
  machine's LAN. Processes started from Terminal or SSH are exempt, so the
  current tests and manual runs do not show it. A denial fails at once and
  could look like an interface or upstream failure. Design 0008 D8 treats
  `EPERM` and `EACCES` as refusals that never fail over and dials
  loopback, own, and private addresses unbound, so it relies on the denial
  returning such an error for local network addresses only (assumption
  F6). If a denial returns a failover error instead, a LAN numbered with
  public addresses is the unprotected case, and the deployment guide must
  name it.
- Next action: researcher — on the user's machine, run cuttle as a
  LaunchAgent with the user's approval, connect to a LAN address, and
  record whether a prompt appears, the exact errno a denial returns, and
  how approval persists; the M1.5 and M3 designs then name the outcome and
  the deployment guide step.

### OI-0008 — Interface behavior on the user's devices

- Origin: research 0003 [open questions][r3-oq] 1–6 and 9; design 0008
  [assumptions][d8-a] F1–F5, F7, and F8; the review of TASK-0008
  iteration 1 (I-R2, I-R3).
- Affected milestones: M1.5 and M1 close.
- Iterations used: 3 of 3 in TASK-0008, for the design; the check itself
  has not run.
- Responsible step: test, with the user.
- Status: open.
- Blocking reason: the design rests on source reading and on observations
  without phones or USB adapters attached. Whether tethered phones keep
  their MACs and BSD names, what kind and media they report, how fast
  notices arrive, what a phone without cellular data does to a connect,
  and whether a bound flow leaves through a non-primary interface need the
  user's devices and presence. A dead uplink behind the primary interface's
  live link fails names at DNS (`502 dns_error` after up to
  `connect_timeout`) before any interface is chosen, so failover and
  demotion help only IP literals and cached names there; the user accepted
  this as a documented limitation in iteration 2, and per-interface name
  lookups are on the roadmap's Later list.
- Next action: tester — run the [0008 device check][d8-dc] with the user
  after M1.5 is implemented, including the optional dead-uplink step if the
  user can cut the router's uplink, and record the results in the
  compatibility matrix. If phone MACs prove unstable (F2), the identity
  question returns to the user before M1 closes. In steps 8–10, record an
  idle baseline `Ibytes` delta over the same time as the downloads, or use
  a larger download, so background traffic cannot satisfy the byte check
  (K-R5 from the TASK-0008 final re-check).

### OI-0006 — Design follow-ups from the TASK-0001 final review

- Origin: TASK-0001 iteration 3 review of candidate `a1e1e61`, verdict
  "approve with minor fixes", findings V1–V12. Landed in `dc52251`. P1
  comes from the TASK-0002 review.
- Affected milestones: M1.2, M1.3, M1.4, and M1 close; V5 also M3.2; P1
  M3.3.
- Iterations used: 3 of 3, so the fixes move to the first M1 task instead of
  another design loop.
- Responsible step: design for wording and criteria, implementer for tests.
- Status: open.
- Blocking reason: None of the findings blocks approval, but each must be
  settled before the stage it affects closes.
- Items:
  - V1 (design): 0003 N3 asserts an upstream route in M1.2, which is
    delivered in M1.3; limit N3 to decisions M1.2 can show. Move the
    roadmap's non-loopback bind retry sentence from M1.2 to M1.4. Settled
    in TASK-0005 (M1.2): N3 now asserts a `block` rule's `403`, the
    `remote-default`, `remote-direct`, and `port` refusals with
    `source=remote` decision lines, and the `allow_direct` address limits;
    the remote upstream route is left to the M1.3 chain tests (U6, U8,
    U12). The roadmap's M1.2 row no longer promises the retry, and the M1.4
    row now says that a non-loopback listener that cannot bind is retried
    and shows as retrying (N7). The retry itself is settled in TASK-0007
    (M1.4): `TestListenerBindRetry` shows `192.0.2.1` as `retrying` in
    `cuttle status` while the loopback listener serves, a busy loopback
    listener still stops startup, and a retried listener that binds later
    serves remote clients.
  - V2 (implementer): add checks that client-sent `Forwarded` and
    `X-Forwarded-*` fields pass unchanged on `disclosure = "via"` routes,
    on the CONNECT head and the plain-HTTP copy-back (U1, U5). Add an
    `HTTP_PROXY` environment check to C3 or drop the 0002 D5 claim that
    every row has a check. The `HTTP_PROXY` half is settled in `8af7956`
    (TASK-0004): C3 and `TestHTTPDirect` check it. The `via` half is
    settled in TASK-0006 (M1.3): `TestUpstreamConnectVia` checks the
    client's `Forwarded` and `X-Forwarded-*` fields in the `CONNECT` head,
    and `TestUpstreamHTTP` checks them after the plain-HTTP copy-back.
  - V3 (design, implementer): check the unreachable hold in the handler
    before `ReverseProxy`, as for CONNECT, and close idle pooled connections
    when an upstream goes down (0003 D4, D8, U11). Settled in TASK-0006
    (M1.3): 0003 D4 now states both, and the plain-HTTP subtest of
    `TestUpstreamUnreachableHold` uses its own upstream, so it passes alone
    and in any order. It checks that a held request never reaches a
    pooled connection and that the probe after an outage dials instead of
    reusing one.
  - V4 (implementer, before M1 close): isolate headless Chrome's background
    requests in the M1 client check. mockenv configs use
    `default = "block"` with rules only for `cuttle.test` and loopback,
    recording upstreams answer 403 outside `cuttle.test`,
    `check-disclosure` compares only the case targets and fails on an
    unmatched one, and the Block evidence names `blocked.cuttle.test`.
    The recording upstream's part is settled in TASK-0006 (M1.3): a name
    without a chosen mode gets 403 outside `cuttle.test` (0007 D2,
    `TestRecordingUpstream`). The rest is settled in TASK-0012 (M1
    close): the single, edge, and gateway configurations that
    `tools/mockenv serve` writes use `default = "block"` with rules only
    for `cuttle.test` and loopback, plus the single instance's rule for
    the two test literals of the V6 observation, which reach only the mock
    origins (0007 D8, `TestServeSession`). `check-disclosure` compares
    only the case targets and fails on one that is missing or unmatched
    (E7, `TestCheckDisclosure`), and the Block evidence names
    `blocked.cuttle.test`. In the implementer's Chrome smoke run,
    egress-direct answered 41 background requests of Chrome to Google
    services with 403, and `check-disclosure` compared only the four case
    targets.
  - V5 (design): Go rejects the bracketless IPv6 CONNECT before any handler,
    so the M3 IPv6 row points the client at the recording mock directly, as
    research 0002 did. Tracked with OI-0002.
  - V6 (user decision, 2026-11-25): private IP literals and this Mac's own
    address stay allowed for local clients without a rule. Record in 0003
    D11 that this is an assumption, and add to the M1 headless Chrome check
    whether a public-origin page's fetch of a private literal through cuttle
    still triggers Chrome's Local Network Access permission. If it does not,
    return the decision to the user. The design half is settled in
    TASK-0012: 0003 D11 cites the new assumption A6, and the M1 client
    check's LNA observation (0007 D8) loads
    `https://lna.cuttle.test/lna.html` through cuttle. The page fetches a
    private, a loopback, and a public literal, and the check records for
    each the page's outcome, whether cuttle logged a decision, and the
    permission states. Remaining: the tester's M1 client check run records
    the observation. In the implementer's smoke run of the tooling with
    Chrome 157 headless, Chrome sent all three fetches through cuttle with
    no permission request, and the page reported `local-network-access` as
    `prompt`. If the tester's run confirms this, A6 does not hold, and the
    orchestrator returns the D11 decision to the user.
    Observed in TASK-0014 (`6cec471`, COMPATIBILITY.md "M1 client check
    run, 2026-11-25"): Chrome 158 headless sent all three `no-cors`
    fetches through cuttle with no permission request; each got an opaque
    response, cuttle logged `outcome=ok` for each, and the origin received
    each once, with no preflight. The page reported all three permission
    names as `prompt`. A6 does not hold in this run, so the D11 decision is
    returned to the user. The run has no direct comparison, so it does not
    show whether this build would ask without a proxy.
    Settled on 2026-11-26: the user kept D11 (private literals and this
    Mac's own address allowed for local clients without a rule) and
    accepted the risk; 0003 D11 records it and A6 is withdrawn.
  - V7 (implementer): mention Go's 500 header-value cap
    (`DefaultMaxHeaderValueCount`) in 0002 D2 and build the C11 60 KiB head
    from fewer than 500 field lines. Settled in `8af7956` (TASK-0004):
    0002 D2 and C11, `TestHeadSizeLimit`.
  - V8 (design, docs): wording fixes in 0002 D5 (the sentence that
    `RequestURI()` returns `URL.Opaque` unchanged, although it prefixes
    `scheme:` for `//` opaque values), 0003 D5 (where the disclosure filter
    is called), the 0003 alternative "Forwarding request heads byte for
    byte", OI-0001 step 2, the 0007 D7 `check-disclosure` flags, the 0007
    D8 chain evidence, 0005 A10 "T2-lite", the COMPATIBILITY curl M3
    WebSocket cell, and ragged wraps in 0003. The two 0007 items are
    settled in TASK-0012: `-names` and `-client` default to the chain's
    instance names and the loopback client address, so D7's `-dir`
    alone is complete, and the D8 Chain row states what shows the
    inspection root CA.
    The 0005 A10 wording is settled in TASK-0013, which names
    `scripts/admin-check` as the T2 check.
  - V9 (implementer): C4 sends the direct request before the block request,
    because cuttle's own responses close the connection. Settled in
    `8af7956` (TASK-0004): 0002 C4, `TestHTTPReusedConnRoutesPerRequest`.
  - V10 (implementer): add a `/status?code=` endpoint to the mock origin for
    C10; mockenv refuses 407 on it. Settled in `8af7956` (TASK-0004): the
    origin answers 407 only when a test sets `Allow407` (0007 D2, 0002 C10,
    `TestHTTPExpectAnd407`, `TestOriginStatus`). E4, mockenv's own refusal,
    is settled in TASK-0012: `TestMockenvRefuses407`.
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
  - R5 (from the TASK-0004 review; implementer at M1.4): after a `101`,
    `httputil.ReverseProxy` owns the hijacked client connection. `Shutdown`
    neither closes nor logs it, and no idle limit applies to it, so 0006
    D6 step 4 and P5 need such upgraded connections tracked like tunnels.
    The C13 remainder of 0002 D7 belongs to the same stage: socket-exact
    plain-HTTP `up`, and exact totals after a `101`. Settled in TASK-0007
    (M1.4): an upgraded connection is tracked like a tunnel, listed in
    status with kind `upgrade`, closed by the idle limit, and closed by
    `Shutdown` with outcome `shutdown` (0002 D5, 0006 D6,
    `TestUpgradedConnections`). Plain-HTTP `up` is counted as the outbound
    socket writes accept body bytes, and after a `101` every byte written to
    either side counts, so the totals are exact (0002 D7,
    `TestHTTPByteCounters`, `TestUpMeter`).
  - Z1–Z4 (from the TASK-0004 final re-check): Z1 (implementer, M1.2):
    the `proxy.remote.sources` `/0` warning, the "repeats rule" warnings,
    and `rule` or `upstream` elements after a non-table element still name
    positions in the filtered list; keep each element's original position
    and add samples. Settled in TASK-0005 (M1.2): the validator keeps the
    written index of every table, rule pattern, and source, and the samples
    `sources-index.toml`, `repeat-index.toml`, and `table-index.toml` list
    each position, which the base code got wrong. Z2 (docs): the 0002 D7
    `decisions_total` row also counts `action=none` for a connection
    without a source class. Z3
    (docs, status half at M1.4): 0006 D7 should say that lines written to
    a deleted log file before the next rotation are lost, that a deleted
    log directory is recreated only after restart, and whether the
    rotation failure count is shown by `cuttle status`. Z4 (implementer,
    only if it flakes): relax the lower bound in
    `TestHTTPClientClosesMidBody`. Z3 is settled in TASK-0007 (M1.4):
    0006 D4 and D7 show the count as `log.rotation_failures` in `cuttle
    status` and state that lines written to a deleted file before the next
    rotation are lost and that a deleted log directory is recreated only
    at the next start.
  - G1–G2 (from the TASK-0005 final re-check; implementer, next change
    to `internal/proxy/warn.go`): G1, the comments at `warn.go:35-36` and
    `:126-127` say "within the interval" where the code and 0003 D12 use a
    sliding last minute; say "within one interval before it". G2, optional:
    end the flood before `late()` in one minute of the "sustained flood"
    case, so it also detects a missing `onTimer` reschedule.
  - W-R1–W-R2 (from the TASK-0006 final re-check): W-R1 (docs): 0003 D5
    l.163 and l.169 say "trace fields", which the glossary retires; say
    "fields that reveal a hop", and optionally note in the D6 known limit
    that `splitMembers` drops bytes outside 0x20–0x7E and trims members.
    W-R2 (implementer, optional): send a harmless `HTTP_USER_AGENT` field
    in TestHTTPDirect "raw client disclosure fields" and assert it arrives.
    Both settled in TASK-0007 (M1.4): 0003 D5 says "fields that reveal a
    hop", the D6 known limit notes what `splitMembers` drops and trims,
    and `TestHTTPDirect` checks that `HTTP_USER_AGENT` arrives.
  - M-R1–M-R4 (from the TASK-0007 re-check): M-R1 (implementer): assert
    exit 1, the "cannot reach the running service at <path>" message, and
    the socket path for `status` and `reload` with a private directory but
    no socket and with a missing directory. M-R2 (design, M2): 0006 D6
    adds up to 5 s drain + 1 s handler wait + 2 s statistics flush = 8 s
    once M2 lands, leaving no slack for P5; state how the steps overlap or
    shorten one bound. M-R3 (implementer, nit): move the `upgrade` helper
    in `internal/proxy/live_test.go` above the TestUpgradedConnections doc
    comment. M-R4 (implementer, optional): `ctlSrv.Close()` does not wait
    for a reload in progress, so its `reloaded` line can follow `stopped`;
    take `s.reloadMu` before logging `stopped`. M-R1, M-R3, and M-R4 are
    settled in TASK-0009 (M1.5): `TestControlUnreachable` asserts exit 1,
    the "cannot reach the running service at <path>" message, and the
    socket path for `status` and `reload` with a missing directory and with
    a private directory without a socket; the `upgrade` helper sits above
    the `TestUpgradedConnections` doc comment; and serve takes `s.reloadMu`
    before it logs `stopped`, which `TestStopWaitsForReload` checks.
    M-R2 is settled in TASK-0010 (M2.1): 0006 D6 shortens the statistics
    flush to 1 s, so the steps take at most 5 s + 1 s + 1 s = 7 s and
    leave 1 s of P5's 8 s. The flush cannot overlap the earlier steps,
    because it writes the final bytes of the tunnels that step 4 closes.
    A 20,000-row flush took 72 ms in `TestStatsRowSize`, and
    `TestStatsShutdownFlush` and `TestServeSIGTERM` check the order and
    the 8 s bound.
  - O-R1 and gate items (from the TASK-0009 re-check): O-R1 (design,
    implementer): the MAC redaction in validation messages also hides
    12-hex-digit labels, `xxxx.xxxx.xxxx` hex names, six-pair dashed labels,
    instance names such as `cuttle-0123456789ab`, and IPv6 addresses
    written with six leading two-digit groups. The operator is trusted
    (AGENTS.md), so narrow or drop the redaction and record the rule in
    0008 D6. O-R1 is settled in TASK-0013: the blanket redaction is
    dropped; messages about `[[interface]]` keys and `interfaces` lists
    still never repeat their value, and a syntax error under those keys
    still gets the fixed hint, while every other message, parser messages
    included, shows the operator's text as written (0008 D6, 0004 D4,
    `TestConfigInterfaces`, `TestConfigEchoesOperatorText`). Gate (infra):
    the cgo step's `go list` returns nothing when `CGO_ENABLED=0` is set in
    the environment; run it as `CGO_ENABLED=1 go list -deps -f
    '{{if .CgoFiles}}{{.ImportPath}}{{end}}' ./cmd/cuttle` in
    `scripts/gate` and 0007 D7. The gate item is settled in TASK-0010
    (M2.1): `scripts/gate` and 0007 D7 run the step with `CGO_ENABLED=1`.
    In a scratch copy with an `import "C"` file in `internal/paths`,
    `CGO_ENABLED=0 sh scripts/gate t0` now fails at that step, which lists
    `runtime/cgo` and `squid/internal/paths`, where the old command listed
    nothing; without the file the gate passes.
  - Q-R1–Q-R8 (from the TASK-0010 review; M2.3 and docs): Q-R1
    (implementer): test that `prepareFile` tightens an existing 0644
    `stats.db`, `-wal`, and `-shm` to 0600. Q-R2 (implementer): do not
    count or log a flush failure caused by cancelling the service context
    at shutdown. Q-R3 (docs): 0005 D2 and S5 should say a newer schema's
    content is left untouched (its mode is tightened). Q-R4 (design): a
    second `cuttle serve` runs retention before it finds the running
    instance; move statistics after the control-socket step or defer
    retention. Q-R5 (design, M2.3): show the last statistics write error in
    status or on the dashboard. Q-R5 is settled in TASK-0013 (M2.3): 0005 D4
    and 0006 D4 add `stats_last_write_error` and its time to status, and the
    overview shows it (`TestStatsLastWriteError`, `TestAdminDashboard`,
    `TestAdminPages`). Q-R6 (implementer): a host whose key folds
    at the 20,000-key cap still uses a `max_hosts_per_hour` slot. Q-R7
    (docs): state that `stats_folded_total` counts folded records. Q-R8
    (docs): rewrap the long line in 0006 D6.
  - P-R2–P-R7 (from the TASK-0011 review; M2.3 and docs): P-R2 (design):
    an admin port that cannot bind is only logged; show the admin listener
    state in `cuttle status` and record the port-squatting risk next to the
    cookie-scope risk in 0005 D7. P-R2 is settled in TASK-0013 (M2.3):
    `cuttle status` shows `admin` as `listening`, `unavailable` with the
    bind error, or `disabled` (0006 D4, `TestAdminListenerBusy`,
    `TestControlSocket`, `TestAdminPages`), and 0005 D7 records the
    port-squatting risk beside the cookie-scope risk. P-R3 (implementer):
    make the uid lookup injectable so the password file's owner check has
    a test. P-R4
    (design): responses `net/http` writes before the handler (`400` for a
    missing, duplicate, or malformed Host, `431`, `OPTIONS *`) carry no D5
    headers; record them as the A7 exception. P-R5 (design): one global
    login backoff lets another local user keep the operator locked out;
    record it as an accepted availability risk in D8. P-R6 (docs): record
    in 0005 D5 and D8 that the listen literal is also accepted, that the
    signed `cuttle_login` token carries its expiry, and that `no-store` is
    on every page except the styles. P-R7 (docs): note `golang.org/x/sys`
    as a transitive dependency in 0002 D1.
  - S-R1–S-R2 (from the TASK-0012 review; docs and test): S-R1 (docs,
    minor): `mockenv check-disclosure -dir <dir>` alone checks only the
    four default case targets, so a hand run skips the body comparison of
    the curl-only `http://plain.cuttle.test/post`; 0007 D7 and the V8 note
    should say to use the rerun line that `m1-check` prints, which lists
    every target with `-target`. `m1-check` itself checks every curl
    target. S-R2 (implementer, nit): the check for an instance name or the
    client address in a request target (`tools/mockenv/disclosure.go`) has
    no test case; disabling it leaves `TestCheckDisclosure` passing.
  - U-R1–U-R5 (from the TASK-0013 review; M2 follow-ups): U-R1
    (implementer, minor): `writeReplace` (`internal/config/atomic.go`) can
    fail after the rename succeeded, when opening or syncing the directory
    fails; the settings page then says the file is unchanged and skips the
    reload, although the file holds the new text. Treat a directory-sync
    failure after the rename as saved, log a warning, and reload. U-R2
    (docs, minor): 0008 D6 says the settings page never shows a MAC
    written where a MAC belongs, but the editor and the 409 view show the
    whole file; say "validation messages on the settings page", and state
    that key names are echoed even inside `[[interface]]`. U-R3
    (implementer, nit): on `/domains`, a date range leaves the window
    select at `1h`, a preset change has no effect while the dates are set,
    and clearing the dates falls back to `1h` instead of `24h`. U-R4
    (implementer, nit): the 409 view's `<pre class="disk">` drops a leading
    empty line of the file on disk; add a newline after the tag as the
    editor does. U-R5 (test, nit): `expectRe` in
    `internal/config/config_test.go` cannot express a key that contains
    `:`, so three MAC-key fixtures no longer check the reported key path.
  - X-R1–X-R5 (from the TASK-0015 reviews; infra unless noted): X-R1
    (minor): `scripts/publish-scan` loses a failing `sed` inside the
    command substitution that tests whether a tag points to a tree, so the
    tree is skipped and the scan can pass; capture the type with `|| die`
    first, and add `|| die` to the next line. X-R2 (minor, also docs):
    `LC_ALL=C` makes `-i` fold ASCII letters only and `.` match one byte;
    say so in the script header and OI-0004 rule 5, and write each case
    form of non-ASCII text in the pattern. X-R3 (minor): the tagger and
    message in a commit's `mergetag` header are not scanned. X-R4 (minor):
    once a pull request exists, GitHub serves `refs/pull/*` and the refs
    check fails; decide whether to allow and scan them. X-R5
    (implementer, nit): `buildVersion` ignores `info.Main.Version`, so
    `go install …/cmd/cuttle@<tag>` reports `dev`.
  - Y-R1–Y-R5 (from the TASK-0017 final re-check; binding requirements for
    M4 task B1, design 0009): Y-R1: `POST /config/apply` checks the
    `reviewed` digest, not the `GET`-time `base`; extend W8 so an apply
    after a notice-row review writes, and a file that changed after the
    review and back to the `base` content still gets `409`; fix the two
    0009 sentences that still say `base` (D7 apply and the alternatives).
    Y-R2: a delete posts the entity's rendered values, and if the current
    entity differs, the review warns that deleting removes the other
    change. Y-R3: `route.host` "Add to rule" answers `409` when the target
    rule no longer has the chosen action or now has `ports`; add the case
    to W25. Y-R4 (nit): D4 lists every choice control (interface identity,
    "Use the default list", "System routing") as mapping by the table, so
    a hidden sub-field cannot keep a key the choice removes. Y-R5 (nit):
    the conflict notice names the fields that changed elsewhere and stay.
- Next action: orchestrator — include the open items (V5, V8, V11, V12, P1,
  W1–W3, Z2, Z4, G1–G2, P-R3–P-R7, Q-R1–Q-R4, Q-R6–Q-R8, S-R1–S-R2,
  U-R1–U-R5, X-R1–X-R5, and Y-R1–Y-R5) in the briefs of the stages they
  affect; the producer closes each item with the commit that settles it.
  The M1.4 items, R5 with the C13 remainder, the per-request source
  recheck of 0003 D10 with N5, the bind retry of V1, Z3, W-R1, and W-R2,
  are settled in TASK-0007.

## Closed

### OI-0004 — Tap hosting, repository remote, and licence

- Origin: research 0001 [open question 7][r1-oq]; [0006 D8][d6-8].
- Affected milestone: M3.1.
- Iterations used: none; recorded in TASK-0001 iteration 1.
- Responsible step: user decision, then infra.
- Status: closed in TASK-0016: `main` and `v0.1.0` are published at
  `neutrospec/cuttle`, and the formula is in the tap. Rules 1–5 govern
  every later push (AGENTS.md "Checkout and landing").
- Decisions (user, 2026-11-26): the repository is the public GitHub
  repository `neutrospec/cuttle`, the formula goes into the user's existing
  public tap `neutrospec/homebrew-tap` as `Formula/cuttle.rb`, and the
  licence is MIT. K1: the user accepts the home-relative checkout path in
  all six commits, so they are not rewritten. Creating the repository and
  every push still need the user's confirmation at that time.
  Publication of v0.1.0 (user, 2026-11-26): confirmed. The pre-push scan
  of `main` and `v0.1.0` found exactly two matches, both the local
  time-zone marker: blob `66bdffd` (an earlier `internal/stats` test,
  fixed in `5cb88c6`) and the message of `5cb88c6`. Every commit date
  already carries the same offset, which the user accepted, so the user
  accepted both; later scans pass when these two are the only matches.
  Rule 6: the user chose to delete the bundle and `.git/filter-repo/`;
  both were deleted on 2026-11-26.
- Prepared in TASK-0015: `LICENSE` (MIT), the module path
  `github.com/neutrospec/cuttle`, the formula source
  `packaging/homebrew/cuttle.rb`, and `scripts/publish-scan`, which
  settles K2–K4.
- Next action: orchestrator — publish after the user confirms, then copy
  the formula into the tap with the tag tarball's `sha256`. Publication
  follows these rules, because the
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
  5. Before and after each push, run `scripts/publish-scan <remote-url>`
     from the main checkout's root. It checks that the remote serves only
     `HEAD`, `refs/heads/main`, and tags, makes a fresh mirror clone in a
     temporary directory that it removes on exit, and counts the matches
     of `CUTTLE_PUBLISH_SCAN_PATTERNS` in every blob of the clone (the
     files of every commit, symbolic link targets, binary files, and blobs
     that only a tag points to); the commit messages without the author
     and committer lines; the name and message of every tag object, inner
     tags of nested tags included; the tip tree; every file and directory
     name, also in trees that a tag points to; and the ref names. Every
     check uses `/usr/bin/grep -a -i -E` in the C locale, so the pattern
     means the same everywhere; UTF-16 text and compressed content are
     not decoded. Every count must be 0. It also checks that the distinct
     author, committer, and tagger identities are exactly
     `CUTTLE_PUBLISH_IDENTITIES`. It ignores `GIT_DIR` and the other
     variables that would point git at another repository. Both values are
     local, so they live in the untracked root `.env` (see
     [`.env.example`](../.env.example)), and the script reads only those
     two variables from it. `--show` prints the matching lines. Exit
     status 0 is a pass, 1 a finding, and 2 a usage or setup error, such
     as an empty or invalid pattern or a failed clone; the script never
     reports a pass after a failed command. Never point it at the working
     repository's path: a path serves every local ref, `refs/t3/*`
     included. Before the first push, scan exactly what the push will send
     by pushing it to a scratch bare repository first:

     ```sh
     d=$(mktemp -d)
     git init -q --bare -b main "$d/pre.git"
     git push -q "$d/pre.git" main 'refs/tags/*:refs/tags/*'
     scripts/publish-scan "$d/pre.git"
     rm -rf "$d"
     ```
  6. Delete or securely store the bundle and the commit map once the user
     approves.

  Before the first push, also settle these items from the TASK-0003 final
  re-check (K1–K4):
  - K1 (user): an early home-relative checkout path appears in
    docs/TASKS.md in `77530ad` and `c335b45`, which the user accepted, and
    again in `3dccaa7`, `de59bc6`, `0cbc9aa`, and `02a2781`. Ask whether
    the acceptance covers the later four; if not, rewrite them before the
    first push.
  - K2 (infra, required): remove `-I` from both `git grep` calls in rule 5,
    so a hit inside a binary file is counted, and add a count over all
    path names, for example `git rev-list --all --objects | cut -s -d' '
    -f2- | grep -i -E -e "$p" | wc -l`. Settled in TASK-0015 by
    `scripts/publish-scan`. The suggested command misses path names:
    `rev-list --objects` prints each object once, under the first path it
    meets, so a file whose content equals an earlier file's goes unseen,
    which the positive control showed. The script lists every commit's
    tree with `git ls-tree -r -t` instead. The TASK-0015 review (F1, F2,
    F4, F5) then found that `git grep` skips symbolic link targets and
    reads `\b` and similar escapes differently from `grep`, that a
    `GIT_DIR` in the environment redirected the scan, and that blobs and
    inner tags reachable only through tags went unscanned; the script now
    reads every blob and tag object with one `grep`, and positive
    controls cover each case.
  - K3 (infra): run the block in a subshell, stop if the clone or `cd`
    fails, check that the pattern compiles and is not empty, read only the
    one variable instead of `set -a`, and clean up with
    `trap 'rm -rf "$d"' EXIT`. Settled in TASK-0015 by the script, which
    runs as its own process and reads `.env` in a subshell.
  - K4 (infra): check that the set of distinct author, committer, and
    tagger identities is exactly the allowed one, because rule 5 skips
    those lines. Settled in TASK-0015 by the script's identity check.

### OI-0003 — Homebrew and launchd behavior unverified

- Origin: research 0001 [open questions 1, 2, 4, 5, and 8][r1-oq];
  [0006 assumptions][d6-a] C2–C4; [0003 assumption A5][d3-a].
- Affected milestone: M3.1.
- Iterations used: none; recorded in TASK-0001 iteration 1.
- Responsible step: test, with infra for the tap tooling.
- Status: closed in TASK-0016; TASK-0015 settled everything except the
  final P8, which passed after publication.
- Approval (user, 2026-11-26): the M3.1 checks may install a throwaway
  local tap and the cuttle formula, and load and unload the user-level
  agent with `brew services`, including `kill -9` relaunch, provided
  everything is stopped, uninstalled, and cleaned up afterwards, nothing
  else holds `127.0.0.1:3128` during the checks, and the user's own cuttle
  configuration and data are only read.
- Results (TASK-0015, Homebrew 7.0.16, a throwaway tap `cuttle-check/local`
  with a `file://` tarball of the candidate commit, every `XDG_*` of the
  service in scratch directories through `cuttle.env`; details in 0006 D8):
  - Build: `fetch` runs `go mod download` with network access, then
    `go build` runs offline from Homebrew's Go caches; modernc.org/sqlite
    and the other modules build with `CGO_ENABLED=0`; 10 s from source.
  - `brew test` passes. Loopback TCP needs network access in the test
    phase: with `deny_network_access! :test` even `free_port` fails with
    `EPERM`, so the formula declares `allow_network_access! :test` (C2).
  - `brew audit --strict` passes, with no complaint about the `file://`
    URL.
  - P9: `~/Library/LaunchAgents/sh.brew.cuttle.plist`, label
    `sh.brew.cuttle`, with `KeepAlive`, `ExitTimeOut` 15, `ProcessType`
    `Interactive`, both log paths, and the `cuttle.env` values; the
    service serves on `127.0.0.1:3128`, and `cuttle status` paths equal
    `cuttle paths`.
  - P10: `brew services stop` delivered `SIGTERM` 0.33 s after the
    command; cuttle closed an open tunnel (`outcome=shutdown`) and logged
    `stopped` 2 ms after the signal, far inside `exit timeout = 15` (C3);
    `restart` gives a new PID in 0.5 s; after a direct `SIGTERM`
    (`last exit code = 0`) and after `kill -9`
    (`last terminating signal = Killed: 9`) launchd relaunched it at once;
    an invalid configuration exits 1 and is relaunched every 10 s with the
    error in both logs, and the service served again at the first
    relaunch after the fix.
  - P11: a changed `XDG_STATE_HOME` in `cuttle.env` reached the plist and
    the service after `brew services restart`; the CLI with the old value
    printed the per-path mismatch warnings (C4).
  - P12: 24,400 decision lines rotated `cuttle.log` at 10 MiB
    (`cuttle.log.1` 10,485,586 bytes, `rotation failures 0`), while
    `var/log/cuttle.log` held only the start-up configuration errors.
  - 0003 A5: the agent's `rlimit_nofile` is 138,240 (`launchctl limit
    maxfiles` 256 soft, unlimited hard), well above 4,096.
  - Uninstall leftovers: `brew uninstall` does not stop the service; the
    job keeps running from the deleted keg with its plist, and
    `brew services cleanup` skips it as "not managed by `brew services`".
    `launchctl bootout gui/<uid>/sh.brew.cuttle` followed by
    `brew services cleanup` removes it. The README and caveats say to
    stop first. `brew services start` leaves an `sh.brew.cuttle =>
    enabled` record in launchd's override database, which needs root to
    remove and is harmless.
  - Side effects outside the tap: installing a fully qualified tap
    formula, even with `--dry-run`, adds it to `trustedformulae` in the
    user's trust store, and `brew untap` removes it again; `brew test`
    installs Homebrew's own test gems into its vendor bundle.
- Final P8 (TASK-0016, 2026-11-26): with the published tag `v0.1.0` and
  the tap at `c54ee0c`, `brew install --build-from-source
  neutrospec/tap/cuttle`, `brew test cuttle`, and `brew audit --strict
  neutrospec/tap/cuttle` exit 0, and `cuttle version` prints `cuttle
  0.1.0`. The user's service then started under `brew services` and
  routed a direct HTTPS and HTTP request. C1 stays with the T3 baseline
  under Later.
[d3-13]: design/0003-upstream-chaining-and-exposure.md#d13-edge-gateway-and-egress-deployment
[d3-a]: design/0003-upstream-chaining-and-exposure.md#assumptions
[d5-a]: design/0005-statistics-and-admin.md#assumptions
[d6-8]: design/0006-packaging-paths-and-lifecycle.md#d8-tap-and-formula
[d6-a]: design/0006-packaging-paths-and-lifecycle.md#assumptions
[d7-8]: design/0007-test-environment-and-gates.md#d8-client-checks
[d8-a]: design/0008-interface-selection.md#assumptions
[d8-dc]: design/0008-interface-selection.md#device-check-i20
[r1-oq]: research/0001-homebrew-launchd-xdg.md#open-questions
[r2-oq]: research/0002-proxy-chaining-and-interception.md#open-questions
[r3-lnp]: research/0003-interface-selection.md#local-network-privacy
[r3-oq]: research/0003-interface-selection.md#open-questions

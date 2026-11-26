# Tasks

The orchestrator tracks each change in the work loop here. The
[task rules](../AGENTS.md#tasks) cover ownership, updates, and closing.

## Status values

| Status | Meaning |
| --- | --- |
| `design` | Research or design is in progress. |
| `implement` | The producer is making the change or fixing findings. |
| `test` | Gate or client evidence is being collected for a candidate. |
| `review` | A fresh reviewer is checking the candidate and its evidence. |
| `approval` | Review has passed. Approval or landing is pending. |
| `blocked` | Work waits on user input or an external dependency. The status line names it. |

Closed tasks are either landed or stopped.

## Entry format

```text
### TASK-NNNN — short title

- Milestone: M1.x or none
- Status: design
- Iteration: 1/3
- Workers: role and branch (never a local path or a tool-internal ID)
- Base: commit SHA
- Candidate: commit and tree SHA, or none
- Evidence: gate tiers with commands and results, or report links
- Findings: open findings with severity and owning step, or none
- Next action: owner role — one concrete step
```

A closed task becomes one line under Closed:

```text
- TASK-NNNN — short title — landed <commit>, gates: <tiers> @ <tree-sha>
- TASK-NNNN — short title — stopped: <reason>, OI-NNNN
```

## Active

### TASK-0024 — v0.2.0 release preparation

- Milestone: M4 close
- Status: review
- Iteration: 1/3
- Workers: implementer done on branch `t3/prepare-v020-release`; fresh
  reviewer running
- Base: `87df62b`
- Candidate: `d9e0072` (tree `dc09c81`)
- Evidence: implementer ran `scripts/gate t0`, `t1`, and
  `scripts/admin-check` on `dc09c81`, all pass; AD-R1–AD-R3 and AD-R5
  with 4 mutation proofs; `CHANGELOG.md` and README updates
- Findings: none yet
- Next action: reviewer — review `87df62b..d9e0072`

Decisions for TASK-0024 (2026-11-26):

- M4 closed on tree `3ad5d43` (TASK-0023). The release tag and the push
  wait for the user's confirmation, as for v0.1.0.

## Closed

The history was rewritten on 2026-11-25 (TASK-0003) to remove local
information. The `gates:` lines of the 25 commits before the TASK-0002
landing merge `7e791a4`, all except the TASK-0002 branch commits `918cdfd`,
`ca4d3a4`, and `a493a4a`, name pre-rewrite trees. The tree IDs below are the
rewritten ones.

- TASK-0023 — M4.4 E: dashboard integration and the M4 browser check —
  landed `af413fe`, gates: T0 T1 T2 @ `3ad5d43` (landed tree differs only in
  tracking records); iteration 1 of 3; the implementer and the reviewer
  each ran T0, T1, and `scripts/admin-check` (W1–W26) on `3ad5d43`, which
  holds every M4 stage, so M4 closes on that tree; follow-ups AD-R1–AD-R5
  in OI-0006
- TASK-0021 — M4.2 C: rules and upstreams — landed `d784fcf`, gates: T0 T1 T2 @
  `c38c5d1` (landed tree differs only in tracking records); iteration 2 of
  3; follow-ups AC-R1–AC-R4 in OI-0006
- TASK-0022 — M4.3 D: interfaces, proxy, remote clients, general, account —
  landed `68d788c`, gates: T0 T1 T2 @ `412f9d0` (landed tree differs only in
  tracking records); iteration 1 of 3; follow-ups AB-R1–AB-R10 in OI-0006
- TASK-0020 — M4.1 B1: save pipeline, review, and Advanced restore —
  landed `e99f703`, gates: T0 T1 T2 @ `c5004d2` (landed tree differs only
  in tracking records); iteration 1 of 3; follow-ups AA-R1–AA-R8 in
  OI-0006
- TASK-0019 — M4.1 B2: admin shell, visual system, and script core —
  landed `8cba20a`, gates: T0 T1 T2 @ `afc5a18` (landed tree differs only in
  tracking records); iteration 2 of 3; follow-ups T-R1–T-R3 in OI-0006
- TASK-0018 — M4.1 A: configuration model and canonical writer — landed
  `63e8090`, gates: T0 T1 @ `5f0459f` (landed tree differs only in tracking
  records); iteration 1 of 3; follow-ups Z-R1–Z-R9 in OI-0006
- TASK-0017 — M4 design: full configuration in the web admin — landed
  `0167a58`, gates: none @ `4b8d4a7`; reviewed tree `dc39942` (iteration 3
  of 3); prototype approved by the user; follow-ups Y-R1–Y-R5 binding for
  task B1 in OI-0006
- TASK-0016 — Publish v0.1.0 — landed: `main` and `v0.1.0` (`8b6acea`)
  pushed to `neutrospec/cuttle`, tap `c54ee0c`, gates: none @ `4c76d26`;
  the remote scan shows only the two accepted matches (OI-0004); final P8
  passed and the user's service runs (OI-0003)
- TASK-0015 — M3.1 Homebrew formula and service — landed `d9eeb98`, gates:
  T0 T1 @ `5d9525a`; reviewed tree `82d51e5` (iteration 2 of 3); service
  checks passed with a throwaway tap; final P8 after publication in
  OI-0003; follow-ups X-R1–X-R5 in OI-0006
- TASK-0013 — M2.3 dashboard and M2.4 settings — landed `46b9f43`, gates: T0 T1
  @ `3bf5e4b`; reviewed tree `9825b25` (iteration 1 of 3); A10 headless
  Chrome check passed on the candidate; follow-ups U-R1–U-R5 in OI-0006
- TASK-0014 — M1 client check run — landed `6cec471`, gates: none @
  `66623ad`; every M1 case passes with headless Chrome 158 and curl; the
  LNA observation returns the 0003 D11 decision to the user (OI-0006 V6)
- TASK-0012 — M1 close: mockenv and the client check tooling — landed
  `169df68`, gates: T0 T1 @ `607aa63`; reviewed tree `ae46bcd` (iteration
  1 of 3); follow-ups S-R1–S-R2 in OI-0006; the client check run is
  TASK-0014
- TASK-0010 — M2.1 statistics — landed `3b3a836`, gates: T0 T1 @ `9bfd095`;
  reviewed tree `2d0b06c` (iteration 1 of 3); follow-ups Q-R1–Q-R8 in
  OI-0006
- TASK-0011 — M2.2 admin access — landed `d5f3603`, gates: T0 T1 @ `21c216f`;
  reviewed tree `dc68654` plus the P-R1 test fix (tree `04d8845`),
  iteration 1 of 3; follow-ups P-R2–P-R7 in OI-0006
- TASK-0009 — M1.5 interface selection implementation — landed `3355992`,
  gates: T0 T1 @ `e15583a`; reviewed tree `36a2c83` (iteration 2 of 3);
  follow-ups O-R1 and the cgo gate fix in OI-0006; device check I20 in
  OI-0008
- TASK-0008 — M1.5 interface selection: research and design — landed
  `fcaebe2`, gates: none @ `9216e50`; reviewed tree `4033ed7` (iteration 3
  of 3); landing fixes in `c436cfa`; follow-ups K-R2 and K-R4 in the M1.5
  brief, K-R5 in OI-0008
- TASK-0007 — M1.4 live operation — landed `106ecd7`, gates: T0 T1 @
  `be7aceb`; reviewed tree `b1febdc` (iteration 2 of 3); follow-ups
  M-R1–M-R4 in OI-0006
- TASK-0006 — M1.3 upstreams and chaining — landed `f53073d`, gates: T0 T1 @
  `6739d92`; reviewed tree `ad5772e` (iteration 3 of 3); follow-ups W-R1–W-R2
  in OI-0006
- TASK-0005 — M1.2 exposure controls — landed `3910ad3`, gates: T0 T1 @
  `9a71cff`; reviewed tree `a9c8992` (iteration 3 of 3); follow-ups G1–G2
  in OI-0006
- TASK-0004 — M1.1 direct and block for local clients — landed `a5b9f02`,
  gates: T0 T1 @ `28f3eca`; reviewed tree `f1e6202` (iteration 3 of 3);
  follow-ups Z1–Z4 and R5 in OI-0006
- TASK-0003 — Remove local information from the tree and history — landed
  `ff51d00`, gates: none @ `8fe2aef`; history of `main` rewritten (tip tree
  `59d2a07` unchanged); iteration 3 of 3; follow-ups K1–K4 in OI-0004
- TASK-0002 — Generalize terminology for a public project — landed
  `7e791a4`, gates: none @ `04c249a`; reviewed tree `bb9dc18` (iteration 2
  of 3); follow-ups W1–W3 in OI-0006
- TASK-0001 — Revise goals and design for chaining, admin, packaging, and
  mocks — landed `dc52251`, gates: none @ `c282643` (pre-rewrite
  `13b7b99`); reviewed tree `6042b86` (pre-rewrite `1f62d55`, iteration 3
  of 3); follow-ups in OI-0006

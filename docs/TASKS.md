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

### TASK-0017 — M4 design: full configuration in the web admin

- Milestone: M4
- Status: design
- Iteration: 2/3
- Workers: designer on branch `t3/web-config-admin-design` (fix round)
- Base: `829a072`
- Candidate: `6421b01` (tree `faf6387`), returned to design
- Evidence: the designer checked the prototype in light and dark and at
  390 px with no horizontal overflow; its canonical TOML example round-trips
- Findings: F1 (major): an invalid file on disk blocks the Advanced and
  restore recovery paths; F2 (major, security): clearing
  `proxy.remote.ports = []` or `admin.listen = ""` would revert to the
  defaults; the medium and minor items are folded into the same round
- Next action: designer — fix F1 and F2 with the folded items and return
  the candidate for a re-check of the fixes

Decisions for TASK-0017 (2026-11-26):

- The user deferred M3.2, M3.3, and the device check I20 and put M4
  first; the M4 decisions are in the roadmap.
- The user approved the prototype at `6421b01` as the visual reference
  for M4 (2026-11-26).
- The user chose the optional `note` key on rules, upstreams, and
  interfaces, and a password change that counts wrong current passwords
  in the login backoff (2026-11-26).

## Closed

The history was rewritten on 2026-11-25 (TASK-0003) to remove local
information. The `gates:` lines of the 25 commits before the TASK-0002
landing merge `7e791a4`, all except the TASK-0002 branch commits `918cdfd`,
`ca4d3a4`, and `a493a4a`, name pre-rewrite trees. The tree IDs below are the
rewritten ones.

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

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

### TASK-0010 — M2.1 statistics

- Milestone: M2.1
- Status: review
- Iteration: 1/3
- Workers: implementer done on branch `t3/implement-hourly-statistics`;
  fresh reviewer pending
- Base: `f94f8e7`
- Candidate: `866aced` (tree `2d0b06c`)
- Evidence: implementer ran `scripts/gate t0` and `t1` on `2d0b06c`, both
  pass; mutation proofs for S2, S3, and S7 each fail their test
- Findings: none yet
- Next action: reviewer — risk-based review of `f94f8e7..866aced`

Decisions for TASK-0010 (2026-11-25):

- The user wants the admin page soon; M2.1 and M2.2 run in parallel, and
  the second to land merges the first.
- Also in scope: OI-0006 M-R2 (fit the statistics flush into the 8 s
  shutdown budget) and the cgo gate fix (`CGO_ENABLED=1 go list` in
  `scripts/gate` and 0007 D7).
- Review follows the risk-based policy in AGENTS.md.

### TASK-0011 — M2.2 admin access

- Milestone: M2.2
- Status: review
- Iteration: 1/3
- Workers: implementer done on branch `t3/admin-access-auth`; fresh
  reviewer pending
- Base: `f94f8e7`
- Candidate: `fe704d9` (tree `dc68654`)
- Evidence: implementer ran `scripts/gate t0` and `t1` on `dc68654`, both
  pass; 34 mutations on the security behavior each fail their test
- Findings: none yet
- Next action: reviewer — risk-based review of `f94f8e7..fe704d9`

Decisions for TASK-0011 (2026-11-25):

- Runs in parallel with TASK-0010; the second to land merges the first.
- Review follows the risk-based policy in AGENTS.md; mutation proofs apply
  to authentication, sessions, CSRF, Host checks, and backoff.

## Closed

The history was rewritten on 2026-11-25 (TASK-0003) to remove local
information. The `gates:` lines of the 25 commits before the TASK-0002
landing merge `7e791a4`, all except the TASK-0002 branch commits `918cdfd`,
`ca4d3a4`, and `a493a4a`, name pre-rewrite trees. The tree IDs below are the
rewritten ones.

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

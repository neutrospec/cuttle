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

### TASK-0005 — M1.2 exposure controls

- Milestone: M1.2
- Status: review
- Iteration: 2/3
- Workers: implementer done on branch `t3/add-remote-proxy-exposure`;
  fresh re-check pending
- Base: `cbf0b07`
- Candidate: `4aea9fa` (tree `483e2d8`) on top of `c7c6217`
- Evidence: implementer ran `scripts/gate t0`, `t1`, and `-race -count=3`
  on the proxy package for `483e2d8`, all pass
- Findings: E1-E7 and the decisions reported fixed; the orchestrator
  accepts mapped prefixes as errors in rule `cidrs` too
- Next action: reviewer — fresh re-check of `c7c6217..4aea9fa`

Decisions for TASK-0005 (2026-11-25):

- OI-0006 items settled in this task: V1 (N3 limited to what M1.2 can
  show; the bind-retry sentence moves to M1.4) and Z1 (validator positions).
- Ignore the `cuttle` binary that the README quick start builds in the
  repository root.
- Iteration 2 (orchestrator): rejected-source and limit warnings keep the
  per-address once-a-minute rule and gain a global budget of 60 lines a
  minute plus one summary line with the suppressed count; a full map never
  resets suppression. An IPv4-mapped source prefix is a validation error
  with a hint. The once-a-minute limit warning (0003 D12, 0006 D7) is
  ratified. 0007 D3 names a dedicated rejected-source address. The
  per-request source recheck after reload goes to the M1.4 brief.

## Closed

The history was rewritten on 2026-11-25 (TASK-0003) to remove local
information. The `gates:` lines of the 25 commits before the TASK-0002
landing merge `7e791a4`, all except the TASK-0002 branch commits `918cdfd`,
`ca4d3a4`, and `a493a4a`, name pre-rewrite trees. The tree IDs below are the
rewritten ones.

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

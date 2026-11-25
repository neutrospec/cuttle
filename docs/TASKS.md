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

### TASK-0003 — Remove local information from the tree and history

- Milestone: none (prepares publication)
- Status: implement
- Iteration: 3/3
- Workers: infra in the main checkout; iteration 2 reviewer done
- Base: `745b4e4` (rewritten ID)
- Candidate: `02a2781` (tree `29ccdc2`), containing the fixes `de59bc6`
  (tree `2b4e6ec`), `3dccaa7`, and `0cbc9aa`; returned for revision
- Evidence: iteration 2 re-check passed checks 1-5 except J1; H3, H4, H7,
  H8, H9 resolved
- Findings: J1, J4, J6 fixed on the board by the orchestrator; J2, J3, J5
  open (infra)
- Next action: infra — fix J2, J3, J5 and create the local `.env`, then a
  fresh final re-check

Decisions for TASK-0003 from the user (2026-11-25):

- Filter all local information out of tracked files and keep local values
  in an untracked `.env`, with `.env.example` listing the variables.
- Remove personal and local information from the commit history too. The
  author email may stay because it is already public.
- After the review (user): commit dates keep their time-zone offset; the
  example private addresses are unrelated to real networks; AGENTS.md keeps
  naming Korean as the user's language; the vendor document cited in
  research 0002 is unrelated to the user's network. An early home-relative
  checkout path in `77530ad` and `c335b45` stays in history.

## Closed

The history was rewritten on 2026-11-25 (TASK-0003) to remove local
information. The `gates:` lines of the 25 commits before the TASK-0002
landing merge `7e791a4`, all except the TASK-0002 branch commits `918cdfd`,
`ca4d3a4`, and `a493a4a`, name pre-rewrite trees. The tree IDs below are the
rewritten ones.

- TASK-0002 — Generalize terminology for a public project — landed
  `7e791a4`, gates: none @ `04c249a`; reviewed tree `bb9dc18` (iteration 2
  of 3); follow-ups W1–W3 in OI-0006
- TASK-0001 — Revise goals and design for chaining, admin, packaging, and
  mocks — landed `dc52251`, gates: none @ `c282643` (pre-rewrite
  `13b7b99`); reviewed tree `6042b86` (pre-rewrite `1f62d55`, iteration 3
  of 3); follow-ups in OI-0006

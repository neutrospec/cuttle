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
- Iteration: 2/3
- Workers: infra in the main checkout; iteration 1 reviewer done
- Base: `745b4e4` (rewritten ID)
- Candidate: `main` at `f6ddf9d` (tree `f31a100`); rewrite tip `bd0d6f7`
  (tree `59d2a07`, identical before and after the rewrite)
- Evidence: infra checks (tree identity, per-version replacement check,
  history scan, structure, docs) passed; orchestrator spot-check found no
  local pattern in any commit on `main`; backup bundle and commit map are
  kept outside the repository
- Findings: iteration 1 review "approve with minor fixes": H3 and H8 fixed
  on the board; H4, H7, H9 open (infra and docs); H1, H2, H5, H6 accepted
  by the user
- Next action: infra — fix H4, H7, and H9, then a fresh re-check

Decisions for TASK-0003 from the user (2026-11-25):

- Filter all local information out of tracked files and keep local values
  in an untracked `.env`, with `.env.example` listing the variables.
- Remove personal and local information from the commit history too. The
  author email may stay because it is already public.
- After the review (user): commit dates keep their time-zone offset; the
  example private addresses are unrelated to real networks; AGENTS.md keeps
  naming Korean as the user's language; the vendor document cited in
  research 0002 is unrelated to the user's network. The early board path
  `~/projects/squid` in history needs no further rewrite.

## Closed

The history was rewritten on 2026-11-25 (TASK-0003) to remove local
information. The tree IDs in `gates:` lines of the commits up to the
TASK-0002 landing merge `7e791a4`, except the three TASK-0002 branch
commits, name pre-rewrite trees. The tree IDs below are the rewritten ones.

- TASK-0002 — Generalize terminology for a public project — landed
  `7e791a4`, gates: none @ `04c249a`; reviewed tree `bb9dc18` (iteration 2
  of 3); follow-ups W1–W3 in OI-0006
- TASK-0001 — Revise goals and design for chaining, admin, packaging, and
  mocks — landed `dc52251`, gates: none @ `c282643` (pre-rewrite
  `13b7b99`); reviewed tree `6042b86` (pre-rewrite `1f62d55`, iteration 3
  of 3); follow-ups in OI-0006

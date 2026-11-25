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
- Workers: role, T3 task or thread ID, checkout path and branch
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

### TASK-0002 — Generalize terminology for a public project

- Milestone: none (prepares M1)
- Status: implement
- Iteration: 1/3
- Workers: docs role in a T3 isolated worktree
- Base: `9e9da92` plus this board update
- Candidate: none
- Evidence: none
- Findings: none
- Next action: docs — add the glossary and apply the general terms across
  the tracked docs, then return the candidate commit for review

Decisions for TASK-0002 from the user (2026-11-25):

- Use general terms so the project can be published: egress proxy,
  TLS-inspecting proxy, inspection root CA, strict upstream, edge instance,
  gateway instance, restricted destinations, private link. The home and
  office setup remains only as a labelled example deployment.
- Rename the per-upstream setting `trace` to `disclosure`, keeping the
  values `"via"` and `"none"`.

### TASK-0003 — Remove local information from the tree and history

- Milestone: none (prepares publication)
- Status: queued until TASK-0002 lands
- Iteration: 1/3
- Workers: infra, in the main checkout with no other writer
- Base: the commit that lands TASK-0002
- Candidate: none
- Evidence: none
- Findings: none
- Next action: orchestrator — dispatch infra after TASK-0002 lands

Decisions for TASK-0003 from the user (2026-11-25):

- Filter all local information out of tracked files and keep local values
  in an untracked `.env`, with `.env.example` listing the variables.
- Remove personal and local information from the commit history too. The
  author email may stay because it is already public.

## Closed

- TASK-0001 — Revise goals and design for chaining, admin, packaging, and
  mocks — landed `ec577ec`, gates: none @ `13b7b99`; reviewed tree `1f62d55`
  (iteration 3 of 3); follow-ups in OI-0006

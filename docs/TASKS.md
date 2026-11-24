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

### TASK-0001 — Revise goals and design for chaining, admin, packaging, and mocks

- Milestone: M1
- Status: review
- Iteration: 1/3
- Workers: researcher ×2 done (notes 0001 and 0002 landed in `c901460`);
  designer done in T3 worktree
  `<worktree>`, branch
  `t3/cuttle-proxy-design`; reviewer pending
- Base: `7c35afa`
- Candidate: `aa1978c` (tree `d4e72ee`), design records 0002-0007, roadmap
  M1-M3, OI-0001 to OI-0005
- Evidence: docs checks on the candidate tree passed (links, anchors,
  whitespace, language); no gate tier applies
- Findings: none yet
- Next action: reviewer — independent review of `aa1978c` against the user
  requirements and the design brief

Decisions for TASK-0001 from the user and orchestrator (2026-11-25):

- Installable name `cuttle` for the formula, binary, service, and XDG
  directories. The repository stays `squid`.
- Go is the implementation language. SQLite is the heaviest allowed storage.
- The corporate upstream proxy needs no proxy authentication.
- Plain `http://` requests are routed with the same rules as HTTPS.
- The admin dashboard and settings pages listen on loopback only and require
  a password.

## Closed

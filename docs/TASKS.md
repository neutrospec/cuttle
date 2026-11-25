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
- Iteration: 3/3
- Workers: designer done in T3 worktree
  `<worktree>`, branch
  `t3/cuttle-proxy-design`; fresh final reviewer pending
- Base: `7c35afa`
- Candidate: `3d33f59` (tree `1f62d55`); history `aa1978c`, `56fd4b7`,
  `e0a94a0`, `3d33f59`
- Evidence: docs checks on `1f62d55` passed; iteration 2 review "revise"
- Findings: S1-S12 and Q1-Q3 reported as addressed by the designer, pending
  verification
- Next action: reviewer — fresh final review of `e0a94a0..3d33f59` and the
  full candidate

Decisions for TASK-0001 from the user and orchestrator (2026-11-25):

- Installable name `cuttle` for the formula, binary, service, and XDG
  directories. The repository stays `squid`.
- Go is the implementation language. SQLite is the heaviest allowed storage.
- The corporate upstream proxy needs no proxy authentication.
- Plain `http://` requests are routed with the same rules as HTTPS.
- The admin dashboard and settings pages listen on loopback only and require
  a password.
- Iteration 2 (user): direct routes refuse private addresses reached by name
  unless a rule allows them. The office-to-home VPN is WireGuard on a home
  server or router. A headless Chrome and `/usr/bin/curl` check runs at M1
  close. Settings are edited as TOML text in the admin page.
- Iteration 2 (orchestrator): `github.com/BurntSushi/toml` is approved. The
  default action is a required setting whose starter value is `direct`.
- Iteration 2 (user, final): requests toward the corporate proxy carry no
  trace of the cuttle hops (no Via, forwarding headers, cuttle names, or home
  client address). Other header handling follows standard `net/http` at the
  designer's discretion.
- Iteration 3 (orchestrator, within the user's delegation): toward `direct`
  and `trace = "none"` routes, cuttle strips proxy-trace headers on both
  CONNECT and plain HTTP, including ones the client sent. This Mac's own
  address typed as an IP literal works for local clients like other private
  literals. Mocks may answer 407 only to cuttle and Go or `/usr/bin/curl`
  tests, never in a session used with a browser or Apple-framework client.

## Closed

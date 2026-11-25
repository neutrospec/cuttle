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

### TASK-0008 — M1.5 interface selection: research and design

- Milestone: M1.5 (new)
- Status: design
- Iteration: 3/3
- Workers: designer on branch `t3/interface-selection-design`; iteration 2
  reviewer done
- Base: merge `e0c4979` of `main` at `508f50e`
- Candidate: `328778b` (tree `1610faf`), approved with minor fixes
- Evidence: reviewer ran the new T0 dependency and no-cgo checks (pass),
  verified constants and SDK headers, and scanned MACs and links
- Findings: open J-R1 (MAC values via TOML syntax errors and `device`),
  J-R2 (`iface.Source` seam), J-R3 (upstream pools after a definition
  change), J-R4 (VPN bypass, document only), J-R5-J-R14 (nits)
- Next action: designer — fix J-R1-J-R14, then a fresh final re-check

Decisions for TASK-0008 from the user (2026-11-25):

- New requirement (translated): because network cost differs, cuttle
  chooses among several network interfaces. Example: with NICs A, B, and C
  in priority order, use B only when A is not online, then C. Some
  destinations, such as large system updates, may be pinned to B even when
  A is online. This applies to `direct` routes, not only to upstreams.
- Placement: a new stage M1.5 after M1.4, before M1 close, so that the M2
  statistics can show traffic per interface.
- "Online" means the link is up with a usable address; a fast connect
  failure moves to the next interface. No periodic probe traffic.
- The user's interfaces are one Wi-Fi interface joined to one fixed
  network, two Ethernet interfaces, and two USB-tethered phones. Interfaces
  of the same kind must be distinguishable; no hotspot joins over Wi-Fi.
- cuttle supports macOS only, so darwin-only mechanisms are acceptable; cgo
  remains a cost to weigh against pure-Go options.
- Iteration 2 (user): a 3 s connect timeout fails over to the next
  interface; after a later interface connects, the failed one goes last
  in every list for 30 s (global demotion, accepting its cost effect); a
  primary interface with a live link but a dead uplink fails names at DNS,
  which stays a documented limitation with per-interface DNS under Later;
  per-destination interface needs on upstream routes use two `[[upstream]]`
  entries with the same address.
- Iteration 3 (user): public destinations are never sent through a VPN,
  so the VPN bypass of bound `direct` dials is documented only; no rule
  option for system routing.

## Closed

The history was rewritten on 2026-11-25 (TASK-0003) to remove local
information. The `gates:` lines of the 25 commits before the TASK-0002
landing merge `7e791a4`, all except the TASK-0002 branch commits `918cdfd`,
`ca4d3a4`, and `a493a4a`, name pre-rewrite trees. The tree IDs below are the
rewritten ones.

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

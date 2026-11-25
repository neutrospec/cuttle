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

### TASK-0006 — M1.3 upstreams and chaining

- Milestone: M1.3
- Status: review
- Iteration: 3/3
- Workers: implementer done on branch `t3/implement-upstream-chaining`;
  fresh final re-check pending
- Base: `8c2e801`
- Candidate: `56f8e0c` (tree `ad5772e`) on top of `bf49f03`
- Evidence: implementer ran `scripts/gate t0`, `t1`, and `-race -count=3`
  on the proxy package for `ad5772e`, all pass; V-R1 and V-R2 proven by
  mutation
- Findings: V-R1-V-R6 reported fixed, pending the final re-check
- Next action: reviewer — fresh final re-check of `bf49f03..56f8e0c`

Decisions for TASK-0006 (2026-11-25):

- OI-0006 items settled in this task: V2 (the `via`-route half), V3 (the
  unreachable hold for plain HTTP), and the M1.3 parts of V4 (recording
  upstreams answer 403 outside `cuttle.test`). G1 and G2 go with any change
  to `internal/proxy/warn.go`.
- Iteration 2 (orchestrator): the disclosure filter also removes client
  fields whose names start with `x-forwarded` (with or without a dash) and
  `forwarded-for`, `x-original-forwarded-for`, `x-client-ip`, and
  `x-cluster-client-ip`. Decision lines for an upstream status record
  `upstream_status=<code>`. `upstream_state{upstream,state}` is a 0/1 gauge
  in 0002 D7. Accepted as built and to be recorded in the design: the
  upstream `connect_timeout` covers the lookup; a close or reset before the
  CONNECT answer is 502 `connection_refused` and not held; a relayed
  pooled response ends a probe; the hold log fields `down` and
  `failed_fast`; cuttle's own bodies may name the upstream; the V3 wording
  in 0003 D4.
- Iteration 3 (orchestrator): every `outcome=upstream_status` line
  carries `upstream_status`, on both paths. The filter also removes
  `CDN-Loop`, `Proxy-Client-IP`, `WL-Proxy-Client-IP`, `X-ProxyUser-Ip`,
  `X-Remote-Addr`, `X-Remote-IP`, `X-Envoy-External-Address`, and
  `X-BlueCoat-Via`, and any listed name behind an `http-` prefix; 0003 D5
  states that the guarantee covers the listed names. Invalid structured
  syntax in upstream `Proxy-Status` members is a recorded known limit.

## Closed

The history was rewritten on 2026-11-25 (TASK-0003) to remove local
information. The `gates:` lines of the 25 commits before the TASK-0002
landing merge `7e791a4`, all except the TASK-0002 branch commits `918cdfd`,
`ca4d3a4`, and `a493a4a`, name pre-rewrite trees. The tree IDs below are the
rewritten ones.

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

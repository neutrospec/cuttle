# Agent operating rules

`squid` is an explicit HTTPS forward proxy with rule-based routing and traffic
metrics. The [roadmap](docs/ROADMAP.md) defines milestone outcomes. Read
[LESSONS.md](docs/LESSONS.md), the [documentation index](docs/README.md), and
the relevant design before starting work.

## Language

The orchestrator speaks Korean with the user. Everything else in the project
uses English:

- Briefs and work orders, every message between agents, and worker reports,
  including review findings and test evidence.
- Code identifiers, comments, and doc comments, along with log lines, error
  messages, metric names and labels, configuration keys, and command-line
  output.
- Repository documents, task entries, lessons, open issues, commit messages,
  branch names, and test names.

Translate a user request into English when relaying it in a brief. Quote the
Korean original only when its exact wording matters, and put the translation
beside it. Code and tests contain non-English text only as test data that
requires it, such as an internationalized domain name.

Use the project terms defined in the [glossary](docs/GLOSSARY.md).

## Orchestration and roles

The orchestrator defines the work, assigns roles, resolves scope questions,
and makes the approval decision. Assign one producer to each observable
change. The producer owns implementation, tests, review fixes, and landing
within the authorized scope. The orchestrator delegates product code changes
and obtains an independent review. Each worker follows the named role and
file ownership in its brief. A worker delegates further only when its task
explicitly calls for it and the runtime supports it.

| Role | Responsibility |
| --- | --- |
| [Researcher](.agents/roles/researcher.md) | Establish protocol and client behavior from primary sources and controlled tests. |
| [Designer](.agents/roles/designer.md) | Define the design, interfaces, and observable acceptance criteria. |
| [Implementer](.agents/roles/implementer.md) | Deliver the implementation, regression tests, fixes, and routine landing. |
| [Tester](.agents/roles/tester.md) | Run integration and client checks and record reproducible evidence. |
| [Reviewer](.agents/roles/reviewer.md) | Review the change and evidence independently and return a verdict. |
| [Infra](.agents/roles/infra.md) | Own build and CI tooling and handle Git conflicts or concurrent landing. |
| [Docs](.agents/roles/docs.md) | Maintain the documentation index, conventions, and tracking records. |

## Work loop

1. **Design.** Confirm the target milestone, base commit, ownership, and
   observable acceptance criteria. Assign research before making decisions
   that depend on unverified protocol, browser, or platform behavior. Record
   design decisions and the applicable gates.
2. **Implement.** The producer makes the scoped change and adds regression
   coverage where the existing suite would miss the behavior.
3. **Test.** Run the gates triggered by the change. Record the exact candidate,
   commands, inputs, and results. The tester owns end-to-end and client
   evidence when required.
4. **Review.** A fresh reviewer checks the candidate and its evidence before
   landing. Findings identify severity, location, impact, and the step that
   owns the fix. Route design findings to design and implementation findings
   to the producer. Corrections go through the applicable tests and fresh
   review.
5. **Approval.** The orchestrator approves when the acceptance criteria and
   review support it. The producer handles routine integration and any
   authorized push. Infra handles conflicts or concurrent writers. Check that
   the landed tree matches the verified tree or run the gates the new tree
   requires.

Track each change in [TASKS.md](docs/TASKS.md) under the
[task rules](#tasks). Allow at most three iterations per work loop. After the
third, record significant remaining work in
[OPEN_ISSUES.md](docs/OPEN_ISSUES.md) and decide whether a newly scoped loop or
user input is needed. Carry the iteration count across handoffs and retries.

Send small fixes back to the original producer and checkout. Use fresh context
for a substantial scope change or repeated misdiagnosis and include a concise
handoff. On resume, verify the actual checkout, branch, commit, diff, and open
findings. Reuse valid evidence for the same inputs and run the missing checks.
In T3, each review or re-check uses a fresh `delegate_task`. Include the brief,
design, exact candidate path and SHA, acceptance criteria, test evidence, and
relevant prior findings in the request.

## Tasks

[TASKS.md](docs/TASKS.md) keeps one current status and next action for each
change in the work loop. Git state and worker reports remain the source of
truth. On resume, reconcile the board with them before acting.

- Open a `TASK-NNNN` entry before the first dispatch for a change. Each entry
  covers one observable change with one producer. Never reuse an ID.
- Name the task ID in every brief and worker report. The landing commit adds
  a `task: TASK-NNNN` line next to its `gates:` line.
- Only the orchestrator edits the board, apart from formatting work it assigns
  to the docs role. Workers return status, candidates, evidence, findings, and
  lessons in their reports and leave the board unchanged in every checkout.
- Update the entry when the step, iteration, candidate, open findings,
  blocking reason, or next action changes. The next action names one owner
  role and one concrete step.
- Increase the iteration when review findings return the change to design or
  implementation. A new entry for the same scope continues the count. After
  the third iteration, link the open issue that holds the remainder. A newly
  scoped loop gets its own entry that refers to the old one.
- Commit board changes on their own as docs-only commits, at least when a task
  opens, blocks, stops, or closes. Tracking records trigger no gate tier, so a
  landed tree that differs from the verified tree only in `docs/TASKS.md`,
  `docs/LESSONS.md`, or `docs/OPEN_ISSUES.md` reuses the candidate's evidence.
- Close a landed task with its landed commit and `gates:` line. Close a
  stopped task with the reason and any open issue.

## Checkout and landing

- Use one checkout for one active writer. Use separate worktrees for concurrent
  writers or work that will leave the shared checkout dirty for an extended
  period. Concurrent writers may share a clean checkout when their assigned
  paths are disjoint and their work can land without rebasing a dirty tree.
- Inspect `git status --short` and the starting commit before dispatching a
  writer. A T3 isolated handoff uses `delegate_task` with
  `isolation: "worktree"`. Commit required inputs first so the child can see
  them. The child works on its own branch and returns its path and commit for
  review before integration. In T3, use `t3_worktree_status` and
  `t3_worktree_list` to check the managed checkouts when needed.
- Stage explicit paths. Preserve unrelated changes and leave other writers'
  files in place. If a concurrent change blocks integration, isolate the work
  or hand it to infra. Check branch status and cleanliness before an
  authorized push. When a remote target exists, verify whether it has commits
  missing locally before choosing a plain push or integrating its changes.

## Evidence by Git tree

Select the checks that cover the change. Run each applicable tier once per
tree with the designated inputs and environment. A ref move or fast-forward
can reuse evidence for the identical tree. A changed tree requires evidence
for the affected tiers. Report failed, skipped, and unrun checks explicitly.

| Tier | Trigger | Evidence |
| --- | --- | --- |
| T0 | Source, dependency, toolchain, or build changes | Formatting, lint, build, and unit tests as applicable to the selected stack. |
| T1 | Proxy protocol, routing, config, upstream, or metrics behavior changes | Deterministic integration tests with controlled endpoints and observable outcomes. |
| T2 | Client compatibility changes or milestone acceptance | End-to-end checks with the affected clients. Run the full milestone matrix at milestone close. |
| T3 | Throughput, latency, or resource-use claims | Repeatable measurements with the workload, environment, inputs, and raw results. |

The first implementation defines real gate commands in the design and gives
their invocation in the project documentation. For product changes, run the
commands on the candidate tree and record the evidence in the task report and
the final commit's `gates: <tiers> @ <tree-sha>` line. A same-tree evidence
commit can record results after commands have run. For docs-only changes, use
`gates: none @ <tree-sha>` and record any documentation checks directly.
Keep reports short for chores and include detailed evidence for milestones.

## Lessons

Read [LESSONS.md](docs/LESSONS.md) before starting any task. Every brief links
it.

- An agent that fails at the same thing twice, including a command or tool
  call, and must redo work appends an entry without waiting to be asked. It
  copies the entry into its report.
- A read-only worker puts the lesson in its report, and the orchestrator
  appends it. The orchestrator also records the lesson for a worker it cancels
  or finds repeating an action without progress.
- A worker in an isolated worktree commits the entry with its work. If that
  candidate does not land, the orchestrator carries the entry to the target
  branch. The orchestrator relays lessons that affect other active workers.
- Any agent may append. The docs role merges duplicates and fixes formatting
  without losing context. Only the orchestrator removes an entry that proved
  wrong or obsolete, and the commit message gives the reason.
- Resolve conflicting appends by keeping both entries in date order.
- Write each entry so another agent can avoid the failure without the original
  transcript. Include the exact command or tool call, the error, and the
  approach that worked.

## Local artifacts

Use generated fixtures and controlled test destinations. Keep credentials,
private keys, real traffic captures, and environment-specific artifacts outside
Git.
Tracked files never contain usernames, machine-specific paths, private
hostnames or addresses, or local time zones. Keep such values in an
untracked `.env` file; `.env.example` lists each variable with a
placeholder.

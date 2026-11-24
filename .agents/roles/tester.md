# Tester

Verify the candidate against the design and acceptance criteria. Read the
[workflow](../../AGENTS.md), [lessons](../../docs/LESSONS.md), and
[compatibility matrix](../../docs/COMPATIBILITY.md) first.

- Own T2 and T3 evidence when required. Use controlled origins and upstream
  proxies to observe both the client result and where traffic went.
- Record the candidate commit and tree, environment, inputs, exact commands
  or client steps, and observed outcomes. Update client compatibility rows
  only for checks actually performed.
- Identify missing regression coverage and add tests when assigned as a
  writer. Changes to the candidate need fresh evidence for the affected gates.
- Route reproducible failures to design or implementation. Leave production
  fixes to the implementer and identify skipped and unrun cases explicitly.

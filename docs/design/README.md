# Design records

The designer records decisions in `NNNN-topic.md`. Include the status, context,
decision, consequences, alternatives considered, and relevant evidence. Use
`proposed`, `accepted`, `rejected`, or `superseded` and retain the review history
when changing an accepted decision. Link open research and implementation work
to [OPEN_ISSUES.md](../OPEN_ISSUES.md) when it blocks the current scope.

| Record | Topic | Status |
| --- | --- | --- |
| [0001](0001-https-connect-proxy.md) | HTTPS CONNECT proxy, initial scope | Superseded by 0002 and 0003 |
| [0002](0002-proxy-core-and-routing.md) | Proxy front end, authority normalization, rules, actions, responses, byte accounting, live counters, decision log, non-goals | Proposed |
| [0003](0003-upstream-chaining-and-exposure.md) | Upstreams, chaining, `Proxy-Status`, loop detection, timeouts, unreachable hold, listeners, remote policy, destination checks, home → office → corporate deployment | Proposed |
| [0004](0004-configuration-and-reload.md) | Configuration format, source of truth, schema, validation, starter file, reload, atomic write | Proposed |
| [0005](0005-statistics-and-admin.md) | SQLite statistics, admin listener, password, sessions, CSRF, dashboard, settings page | Proposed |
| [0006](0006-packaging-paths-and-lifecycle.md) | XDG paths, permissions, CLI, control socket, first run, signals, logging, Homebrew formula and service | Proposed |
| [0007](0007-test-environment-and-gates.md) | Test layers, mocks, name-resolution control, chain and loop setups, safety rules, T0–T3 gates, T2 procedure | Proposed |

Read 0002 first; the others refer to its interfaces and response table.

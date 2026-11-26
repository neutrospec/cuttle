# Design records

The designer records decisions in `NNNN-topic.md`. Include the status, context,
decision, consequences, alternatives considered, and relevant evidence. Use
`proposed`, `accepted`, `rejected`, or `superseded` and retain the review history
when changing an accepted decision. Link open research and implementation work
to [OPEN_ISSUES.md](../OPEN_ISSUES.md) when it blocks the current scope.

| Record | Topic | Status |
| --- | --- | --- |
| [0001](0001-https-connect-proxy.md) | HTTPS CONNECT proxy, initial scope | Superseded by 0002 and 0003 |
| [0002](0002-proxy-core-and-routing.md) | `net/http` front end, authority normalization, rules, forwarding and standard-library defaults, responses, byte accounting, live counters, decision log, non-goals | Proposed |
| [0003](0003-upstream-chaining-and-exposure.md) | Upstream disclosure modes (`via`, `none`), `Via` entries and loop detection, non-disclosure toward an egress proxy, `Proxy-Status`, timeouts, unreachable hold, listeners, remote policy, destination checks, edge → gateway → egress deployment | Proposed |
| [0004](0004-configuration-and-reload.md) | Configuration format, source of truth, schema, validation, starter file, reload, atomic write | Proposed |
| [0005](0005-statistics-and-admin.md) | SQLite statistics, admin listener, password, sessions, CSRF, dashboard, settings page | Proposed |
| [0006](0006-packaging-paths-and-lifecycle.md) | XDG paths, permissions, CLI, control socket, first run, signals, logging, Homebrew formula and service | Proposed |
| [0007](0007-test-environment-and-gates.md) | Test layers, mocks, name-resolution control, chain and loop setups, safety rules, T0–T3 gates, M1 and M3 client checks | Proposed |
| [0008](0008-interface-selection.md) | Interface definitions and matching, online state without probe traffic, change notices, interface lists, bound dials, failover and demotion, upstream binding, DNS limits, status, `cuttle interfaces`, per-interface counters, device check | Proposed |
| [0009](0009-web-configuration.md) | Full configuration in the web admin: information architecture, screens and states, convenience features, canonical TOML writer, edit and review pipeline, Test a URL, script policy, visual system, password change; [clickable prototype](0009-prototype.html) | Proposed |

Read 0002 first; the others refer to its interfaces and response table.

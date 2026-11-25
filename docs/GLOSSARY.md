# Glossary

The documentation uses these terms. Use them in documents, briefs, reports,
code comments, and messages. A particular network setup is an instance of
these terms, not a separate concept: the maintainer's own setup appears
only as the labelled example deployment in [0003 D13][d3-13]. Earlier
commits and the research notes use older wording; the
[mapping table](#mapping-from-earlier-wording) at the end translates it.

## Deployment

- **Instance.** One running cuttle process with its own configuration. Its
  instance name, the top-level `name`, appears in the `Via` entries it adds
  on `via` routes and in its `Proxy-Status` members ([0003 D2][d3-2],
  [D6][d3-6]).
- **Edge instance.** The cuttle instance that clients point at.
- **Gateway instance.** A cuttle instance inside another network. It
  accepts allowlisted remote clients, usually edge instances, and forwards
  to restricted destinations through that network's egress proxy.
- **Edge machine, gateway machine.** The host that runs the edge or gateway
  instance.
- **Restricted destinations.** Destinations reachable only from inside
  another network or through its egress proxy.
- **Private link.** The network path between instances, such as a VPN,
  WireGuard, or Tailscale.
- **Edge → gateway → egress chain.** The chained deployment of
  [design 0003 D13][d3-13]. The edge instance sends restricted
  destinations over a private link to a gateway instance, which forwards
  them through its network's egress proxy.
- **Example deployment.** The maintainer's own setup, mapped onto these
  terms in one labelled subsection of [0003 D13][d3-13].

## Proxies and upstreams

- **Upstream.** A named plain-HTTP proxy, declared in `[[upstream]]`, that
  carries requests on an `upstream:<name>` route. It may be another cuttle
  instance or a third-party proxy.
- **Egress proxy.** A third-party upstream proxy that a network requires
  for outbound traffic. It may inspect TLS.
- **TLS-inspecting proxy.** A proxy that terminates TLS inside tunnels and
  re-signs server certificates with its own root.
- **Inspection root CA.** The private root a TLS-inspecting proxy signs
  with. Clients must trust it.
- **Strict upstream.** An upstream that rejects or mishandles requests that
  reveal an intermediary.
- **Disclosure mode.** The required per-upstream setting `disclosure`.
  `via` adds one cuttle `Via` entry for loop detection, and `none` sends
  requests that disclose no intermediary ([0003 D1][d3-1]).
- **Disclosure fields.** Header fields that reveal an intermediary or the
  original client address, such as `Via`, `Forwarded`, `X-Forwarded`,
  and `X-Forwarded-*`. [0003 D5][d3-5] gives the full list.
- **Disclosure filter.** The one function that removes every disclosure
  field, the client's own included, on `direct` routes and toward
  `disclosure = "none"` upstreams.
- **Non-disclosure.** The rule that a request on a `direct` route or toward
  a `disclosure = "none"` upstream discloses no intermediary: it carries no
  disclosure field and nothing else that reveals a hop ([0003 D5][d3-5]).
- **Loop detection.** An instance refuses a request whose `Via` already
  names it with `502 proxy_loop_detected` ([0003 D2][d3-2]).
- **Unreachable hold.** After cuttle fails to reach an upstream, it answers
  requests routed there with `503` at once for `unreachable_hold` instead
  of dialing again ([0003 D8][d3-8]).

## Clients and routing

- **Local client.** A client that connects from a loopback address. It has
  the reach it would have without a proxy and faces no port policy.
- **Remote client.** A client that connects from an address in
  `proxy.remote.sources`, such as an edge instance reaching a gateway
  instance. Remote clients have their own default, ports, and limits
  ([0003 D10][d3-10]).
- **Original client address.** The address of the client that sent the
  request to the edge instance.
- **Rule.** A `[[rule]]` entry that matches destinations by host name,
  domain, IP prefix, and port and names an action. Rules are tried in file
  order, and the first match wins ([0002 D4][d2-4]).
- **Action.** What cuttle does with a request: `direct`, `block`, or
  `upstream:<name>`. The action, with its upstream, is the request's route.
- **Direct.** cuttle resolves and connects to the destination itself,
  subject to the destination address policy ([0003 D11][d3-11]).
- **Block.** cuttle answers `403` without any lookup or connection.
- **Default.** The action for a request that no rule matches:
  `proxy.default` for local clients and `proxy.remote.default` for remote
  clients.
- **Decision log.** One log line per routing decision with the target,
  rule, action, upstream, outcome, and byte counts, and never paths,
  queries, header values, or bodies ([0002 D8][d2-8]).

## Interfaces

- **System interface.** A network interface as macOS reports it, with a
  BSD name such as `en0`, an index, and usually a MAC address.
- **Interface.** A named `[[interface]]` definition that matches one
  system interface by MAC address, BSD name, or both, with an optional
  kind guard ([0008 D2][d8-2]). Lists, logs, counters, and statistics use
  its name; the MAC stays in the configuration file.
- **Online.** The state of an interface whose link is active and that has
  a usable address and a default route for at least one address family,
  read from the system without sending traffic ([0008 D4][d8-4]).
- **Interface list.** An ordered list of interface names:
  `proxy.interfaces` for `direct` routes, a rule's own `interfaces`, or an
  upstream's `interfaces`. The list in force is the only set of interfaces
  a request may use, and its order is the priority ([0008 D6][d8-6]).
- **Bound dial.** A TCP connection restricted to one interface with the
  socket option `IP_BOUND_IF` or `IPV6_BOUND_IF`. An unbound dial follows
  system routing ([0008 D7][d8-7]).
- **System routing.** macOS's own choice of interface for an unbound
  dial: the primary interface's default route, or a more specific route
  such as a VPN's.
- **Primary interface.** The interface that holds the unscoped default
  route. It carries name lookups and unbound dials ([0008 D13][d8-13]).
- **Failover.** Moving to the next interface in the list in force after a
  fast connect failure or an attempt timeout ([0008 D8][d8-8]). It keeps
  the route: it is neither a fallback from an upstream to `direct` nor a
  move to another upstream.
- **Demotion.** Placing an interface after the others in every list for a
  short period after it failed while a later interface connected
  ([0008 D9][d8-9]). It applies to all destinations at once.

## Testing

- **Mocks.** Controlled origins and upstream proxies in `internal/testenv`
  that record exactly what they receive ([0007 D2][d7-2]).
- **Recording upstream.** A mock proxy that records every request byte for
  byte and answers with a status the test chooses.
- **Inspecting egress mock.** A mock TLS-inspecting egress proxy that signs
  leaves with a generated inspection root CA.
- **mockenv.** `tools/mockenv`, the development command that runs the mocks
  for client checks. `mockenv check-disclosure` compares the requests a
  mock recorded directly and through the chain ([0007 D8][d7-8]).
- **Gate tiers.** T0 to T3, the evidence levels defined in
  [AGENTS.md](../AGENTS.md#evidence-by-git-tree) with commands in
  [0007 D7][d7-7].

## Mapping from earlier wording

Use this table to read the git history before TASK-0002 and the research
notes, which keep their original wording.

| Earlier wording | Current term |
| --- | --- |
| corporate proxy, `corp` | egress proxy |
| intercepting proxy, re-signs TLS | TLS-inspecting proxy |
| corporate root | inspection root CA |
| home cuttle, `cuttle-home` | edge instance, `cuttle-edge` |
| office cuttle, `cuttle-office` | gateway instance, `cuttle-gateway` |
| home Mac, office Mac | edge machine, gateway machine |
| upstream `office` | upstream `gateway` |
| upstream `corp` | upstream `egress` |
| office-only sites, rule `office-only` | restricted destinations, rule `restricted` |
| `corp.example` in examples | `internal.example` |
| VPN, WireGuard | private link |
| home client address | original client address |
| home → office → corporate | edge → gateway → egress chain |
| `trace` (upstream setting) | `disclosure` |
| trace mode | disclosure mode |
| trace fields | disclosure fields |
| trace filter | disclosure filter |
| no trace of proxying, no-trace rule | non-disclosure; discloses no intermediary |
| `TestNoTraceChain` | `TestNoDisclosureChain` |
| `mockenv check-trace`, `TestCheckTrace` | `mockenv check-disclosure`, `TestCheckDisclosure` |
| intercepting mock, intercepting upstream, corporate mock | inspecting egress mock |
| `TestChainIntercepting` | `TestChainInspecting` |
| `corp-direct.jsonl`, `corp-chain.jsonl` | `egress-direct.jsonl`, `egress-chain.jsonl` |

[d2-4]: design/0002-proxy-core-and-routing.md#d4-rules-and-evaluation-order
[d2-8]: design/0002-proxy-core-and-routing.md#d8-decision-log
[d3-1]: design/0003-upstream-chaining-and-exposure.md#d1-upstreams-and-disclosure-modes
[d3-2]: design/0003-upstream-chaining-and-exposure.md#d2-via-entries-and-loop-detection
[d3-5]: design/0003-upstream-chaining-and-exposure.md#d5-non-disclosure-toward-an-egress-proxy
[d3-6]: design/0003-upstream-chaining-and-exposure.md#d6-upstream-failures-and-proxy-status
[d3-8]: design/0003-upstream-chaining-and-exposure.md#d8-unreachable-upstream-hold
[d3-10]: design/0003-upstream-chaining-and-exposure.md#d10-source-classes-and-remote-policy
[d3-11]: design/0003-upstream-chaining-and-exposure.md#d11-destination-address-policy-for-direct-routes
[d3-13]: design/0003-upstream-chaining-and-exposure.md#d13-edge-gateway-and-egress-deployment
[d7-2]: design/0007-test-environment-and-gates.md#d2-mocks
[d7-7]: design/0007-test-environment-and-gates.md#d7-gates
[d7-8]: design/0007-test-environment-and-gates.md#d8-client-checks
[d8-2]: design/0008-interface-selection.md#d2-interface-definitions
[d8-4]: design/0008-interface-selection.md#d4-online-state
[d8-6]: design/0008-interface-selection.md#d6-interface-lists
[d8-7]: design/0008-interface-selection.md#d7-dial-plan
[d8-8]: design/0008-interface-selection.md#d8-failover
[d8-9]: design/0008-interface-selection.md#d9-demotion
[d8-13]: design/0008-interface-selection.md#d13-name-resolution

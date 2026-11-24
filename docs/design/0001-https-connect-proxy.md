# 0001 — HTTPS CONNECT proxy

**Status** — Proposed initial scope.

## Context

Clients need to send HTTPS connections through a service that can reach the
destination directly, pass the connection to another proxy, or refuse it.
Operators need enough evidence to explain the route and account for relayed
traffic. The first release targets clients running on the same machine.

## Decision

- Listen on loopback and accept HTTP/1.1 `CONNECT`. The client supplies a
  destination host and port in the request target and negotiates TLS with the
  origin through the resulting tunnel. A client configures `squid` as an HTTP
  proxy for HTTPS traffic. [RFC 9110 §9.3.6](https://www.rfc-editor.org/rfc/rfc9110.html#section-9.3.6)
- Load an ordered rule set at startup. Match an exact hostname or a
  domain and its subdomains on a DNS label boundary, with an optional port
  condition. Apply the first matching rule and default to `block`. Start with
  destination port 443 enabled and allow an explicitly configured set of
  destination ports for controlled environments. Reject invalid policy at
  startup.
- Evaluate the decision before any outbound DNS or connection attempt. For
  `direct`, connect to the destination. For `upstream`, use a named, configured
  HTTP proxy and establish its `CONNECT` tunnel to the same authority. Report
  success to the client after the selected path is established. Policy blocks
  return `403`. A failed upstream tunnel ends that request without rerouting.
- Stream both directions with bounded buffering and release both sides on
  completion or error. Log the destination authority, matched rule or default,
  action, upstream name when applicable, tunnel outcome, duration, and byte
  counts. Keep TLS contents and credentials out of logs.

## Measurement contract

- Count routing decisions separately from connection and tunnel outcomes.
  Report totals by action, active tunnel count, connection failures by reason,
  and relayed bytes by action and direction on a local metrics endpoint.
- Count bytes successfully written to the receiving socket during the tunnel
  once per direction at `squid`. The totals include the TLS handshake and
  encrypted stream. HTTP `CONNECT` framing and TCP/IP overhead are outside the
  totals. Apply counter updates during transfer so active tunnels contribute.
- Use bounded metric dimensions. The connection log is where an operator can
  look up a destination or rule. A policy block has zero tunnel bytes and an
  upstream failure is visible as an upstream outcome.

## Consequences

Filtering and accounting operate per `CONNECT` tunnel. One tunnel can carry
multiple HTTPS requests. The proxy can see the requested authority and stream
sizes. The HTTP fields are encrypted between the client and origin. Initial
operation uses local configuration and operator metrics.

The M1.1 design will select the language and runtime, config format, socket
limits, metrics format, and executable build and test commands. It will also
define authority validation and normalization, including IP literals, before
the listener and policy become a public contract.

## Alternatives considered

- TLS interception would allow URL and body filtering and require client trust
  configuration and a separate privacy design. M1 uses end-to-end TLS.
- A transparent proxy would require system traffic redirection and its own
  destination discovery and deployment design. M1 uses explicit client proxy
  settings.

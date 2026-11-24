# Roadmap

## M1 — Local HTTPS proxy

An operator can configure Chrome, Safari, and a terminal client to use `squid`
for HTTPS, apply destination rules, and see the resulting traffic volumes. The
initial verification environment is macOS. The service starts on loopback.

| Stage | Done when |
| --- | --- |
| M1.1 — Direct and block | `curl` reaches a controlled HTTPS origin through an explicit `direct` rule with its certificate verified. A matching `block` rule or the default action yields a policy rejection before an outbound connection. |
| M1.2 — Upstream | A controlled upstream HTTP proxy records a `CONNECT` for the requested authority and relays the HTTPS exchange. An upstream refusal or timeout is visible to the client and operator and generates zero direct connection attempts. |
| M1.3 — Observability | An operator can read active tunnel count, decision and connection outcome totals, and bytes in both directions. A controlled byte stream confirms exact counter deltas during transfer and after closure. Policy blocks report zero relayed bytes. |
| M1.4 — Client verification | Chrome and Safari each load the controlled HTTPS origin with its valid certificate intact. For Chrome, Safari, and `curl`, record the observed direct, upstream, and block behavior, client versions, and tested commit. |

Integration tests also cover rule order, the default action, invalid policy,
connection error reporting, and tunnel teardown. M1 closes when all four
stages have evidence in the same release candidate and independent review is
complete. See the [initial proxy design](design/0001-https-connect-proxy.md) and
[compatibility matrix](COMPATIBILITY.md).

## M2 — Operate the proxy

Define the supported deployment model and access controls. Set connection and
timeout limits, define safe policy reload and shutdown behavior, and establish
representative throughput, latency, and resource-use baselines. Set numeric
targets using those measurements and the intended deployment size.

## Later design topics

Consider operator UI, longer-term traffic reporting, additional upstream proxy
protocols, and finer-grained filtering based on demonstrated needs. URL paths,
headers, and bodies are inside the TLS session in M1. Any future design that
filters on those fields must define its TLS trust and privacy model.

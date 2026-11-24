# squid

`squid` is a planned HTTPS forward proxy for explicitly configured clients such
as Chrome, Safari, and command-line tools. A routing policy chooses one action
for each connection:

| Action | Behavior |
| --- | --- |
| `direct` | Open a connection from `squid` to the destination. |
| `upstream` | Ask a configured upstream proxy to open the tunnel. |
| `block` | Reject the connection at `squid`. |

The first release uses HTTP `CONNECT` to make routing decisions by destination
host and port while preserving end-to-end TLS. Operators can observe which rule
matched, whether the tunnel opened, and how many bytes it relayed in each
direction.

- [Roadmap and acceptance criteria](docs/ROADMAP.md)
- [Initial proxy design](docs/design/0001-https-connect-proxy.md)
- [Development workflow and agent roles](AGENTS.md)
- [Documentation index](docs/README.md)

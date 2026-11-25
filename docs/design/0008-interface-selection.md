# 0008 — Interface selection

**Status** — Proposed (TASK-0008, 2026-11-25).

## Context

Network cost differs between the connections of the cuttle machine, so
cuttle chooses which network interface carries each outbound connection.
With interfaces A, B, and C in priority order, cuttle uses B only when A is
not online, and C only when neither A nor B is. Some destinations, such as
large system updates, are pinned to B even when A is online. In general the
user wants an order and a restriction per destination, on `direct` routes
and on upstreams.

The target setup has five interfaces: built-in Wi-Fi joined to one fixed
network, two USB or Thunderbolt Ethernet adapters, and two iPhones tethered
over USB. Interfaces of the same kind must be told apart. No phone hotspot
is joined over Wi-Fi. "Online" means that the link is up and has a usable
address. A fast connect failure moves to the next interface, and cuttle
sends no probe traffic. cuttle supports macOS only, so Darwin mechanisms
are acceptable, and cgo is a cost to weigh against pure-Go options. The
stage is M1.5, after M1.4 and before M1 closes, so that the M2 statistics
can show traffic per interface from their first version.

Evidence is in [research 0003][r3]: binding with `IP_BOUND_IF` and
`IPV6_BOUND_IF` from `net.Dialer.Control`, interface state without traffic,
change notices from the routing and kernel event sockets, BSD naming by MAC
address, name resolution through the primary interface, and the
interactions with VPN routes and local network privacy. Terms are defined
in the [glossary](../GLOSSARY.md#interfaces).

This record defines interface definitions and matching, the online state,
change notices, interface lists, the dial plan, failover, demotion, the
answer when no interface is usable, upstream binding, connection pools,
name resolution, reload, status, the `cuttle interfaces` command, logging,
counters, and statistics. Changes that other records need are listed at
the end, for the orchestrator to apply after TASK-0007 lands.

## Decisions

### D1. Scope

- Interface selection applies to `direct` routes, for `CONNECT`, plain
  HTTP, and upgrades, and to every upstream that sets its own interface
  list (D11).
- A configuration with no `[[interface]]` and no list behaves exactly as
  in M1.4: every dial is unbound and follows system routing, and cuttle
  opens no notice socket.
- Selection never widens what a request may reach. The rules
  ([0002 D4][d2-4]) and the destination address policy
  ([0003 D11][d3-11]) run first, unchanged.
- The mechanisms are Darwin system calls through Go's `syscall` package:
  no cgo, no new module (D16).

### D2. Interface definitions

`[[interface]]` names one system interface. Its `name` is what lists,
logs, counters, and statistics use, so the real identifiers stay in the
configuration file.

| Key | Type | Default | Applies | Meaning |
| --- | --- | --- | --- | --- |
| `name` | string | required | reload | `^[a-z][a-z0-9-]{0,31}$`, unique; `other` is reserved for counters (D15) |
| `mac` | string | none | reload | EUI-48 hardware address, six colon-separated hex pairs, any case |
| `device` | string | none | reload | BSD name such as `en0` |
| `kind` | string | none | reload | Guard: `wired`, `wifi`, or `cellular` (D3) |

- A definition sets `mac`, `device`, or both. With both, both must match.
- `kind` is never an identity. It catches a definition that matches the
  wrong kind of interface, such as `device = "en0"` on a Mac whose `en0` is
  built-in Ethernet.

Recommended keys for the target setup, in the robustness order of
[research 0003 §6][r3-ids]:

| Interface | Keys | Why |
| --- | --- | --- |
| Built-in Wi-Fi | `device = "en0"`, `kind = "wifi"` | Wi-Fi may use a private address per network, while the built-in BSD name is stable ([research 0003 §6][r3-mac]). |
| Ethernet A and B | `mac` | macOS keys the BSD name by MAC, so the name follows the adapter across ports and replugging, but a new adapter of the same model can take over an absent adapter's name ([research 0003 §6][r3-bsd]). The MAC cannot be taken over. |
| Phone A and B | `mac` | As for adapters. Per-phone MAC stability is assumption F2. |

The user reads the MACs and BSD names with `cuttle interfaces` (D15).
Real values live only in the user's configuration. Documents and tests use
the documentation range `00:00:5e:00:53:00`–`00:00:5e:00:53:ff`
([RFC 9542 §2.1.4][rfc9542], which replaced RFC 7042).

### D3. Matching system interfaces

The interface table lists every system interface with its index, BSD name,
MAC, flags, kind, media status, addresses, and default routes (D5).
Loopback interfaces never match. A definition matches a row when every
key it sets is equal: `mac` to the row's hardware address, `device` to its
BSD name.

| Rows matched | State | Reason | Log, once per transition |
| --- | --- | --- | --- |
| None | offline | `absent` | `interface offline` with the reason; when `device` is present with another MAC, also `interface device has a different MAC` |
| One, `kind` equal or unset | Judged by D4 | — | — |
| One, `kind` differs | offline | `kind_mismatch` | `interface kind mismatch` with the device, its kind, and the expected kind |
| Several | offline | `ambiguous` | `interface definition matches several devices` with their BSD names |

- Several rows can match one MAC, for example when a VLAN interface
  carries its parent's MAC (inference). The fix is to add `device`. cuttle
  never guesses: an ambiguous definition is never used.
- Kind comes from the public ioctl `SIOCGIFFUNCTIONALTYPE`: functional
  type 2 is `wired`, 3 (Wi-Fi infrastructure) `wifi`, and 5 `cellular`;
  every other value prints as `other` in `cuttle interfaces` and never
  satisfies a guard ([research 0003 §4][r3-4]). The type of a tethered
  iPhone is not known yet (assumption F3), so the user sets `kind` for the
  phones only after the device check shows it.
- Logs name BSD names and kinds, never MAC addresses.

### D4. Online state

An interface is online for an address family when every check holds:

| # | Check | Source | Offline reason |
| --- | --- | --- | --- |
| 1 | Exactly one row matches and `kind` agrees (D3) | Interface table | `absent`, `ambiguous`, `kind_mismatch` |
| 2 | `IFF_UP` is set | Interface flags | `admin_down` |
| 3 | Media is active, when the interface reports media status | `SIOCGIFMEDIA`: `IFM_AVALID` set requires `IFM_ACTIVE`; a failed ioctl or a clear `IFM_AVALID` passes | `link_down` |
| 4 | It holds a usable address of the family: IPv4 outside `0.0.0.0/8` and `169.254.0.0/16`; IPv6 outside `fe80::/10` | Interface addresses | `no_address` |
| 5 | A default route of the family uses its index, scoped (`RTF_IFSCOPE`) or the primary's unscoped one | Routing table dump | `no_route` |

- Checks 1–3 apply to both families; checks 4 and 5 are per family.
- The interface is `online` when at least one family passes, and its
  `families` lists those families. Otherwise it is `offline` with the
  reason of the first failing check. When checks 1–3 pass, the reason is
  `no_address` if no family has a usable address and `no_route`
  otherwise.
- Go's `FlagRunning` is not a link signal: every present Ethernet-type
  interface had it with or without a link ([research 0003 §3][r3-read]).
- `online` is a local fact. Nothing is sent to learn it, so an uplink that
  fails behind a working link shows only through failover (D8) and
  demotion (D9).
- Address flags such as IPv6 duplicate address detection are not read. An
  address still in detection fails the dial with `EADDRNOTAVAIL`, which
  fails over (D8).
- An interface that only reaches a LAN, with no default route, is never
  online. Destinations on such a LAN are private and dialed unbound (D7).

### D5. Reading state and noticing changes

Each re-read builds the whole interface table again from these sources, all
pure Go through `syscall`, without privileges and without sending a packet:

| Fact | Source |
| --- | --- |
| Index, BSD name, MAC, flags | `net.Interfaces` (the `NET_RT_IFLIST` sysctl) |
| Addresses | `net.Interface.Addrs` |
| Media status | `ioctl(SIOCGIFMEDIA)` on an `AF_INET` datagram socket (`syscall.SIOCGIFMEDIA`) |
| Kind | `ioctl(SIOCGIFFUNCTIONALTYPE)`, public `sys/sockio.h`; the constant is defined in cuttle |
| Default routes | `syscall.RouteRIB(syscall.NET_RT_DUMP, 0)` parsed with `syscall.ParseRoutingMessage`: messages with `RTF_UP` and `RTF_GATEWAY` whose destination and mask are `0.0.0.0/0` or `::/0`; the header's index names the interface, and `RTF_IFSCOPE` separates scoped routes from the primary's |

A re-read happens:

- at start and at every reload, before the new snapshot is used (D14);
- 200 ms after a notice, coalescing every notice in that window, so at most
  five re-reads run per second;
- when a bound dial fails with `ENXIO`, `EADDRNOTAVAIL`, or `ENETDOWN`,
  which shows a stale table, within the same 200 ms limit;
- every 30 s as a safety net for lost notices, or every 5 s while a notice
  socket cannot be opened. These re-reads are system calls, not traffic.

Notices come from two sockets that open without privileges
([research 0003 §3][r3-notice]):

- **Routing socket**, `socket(AF_ROUTE, SOCK_RAW, 0)`. `RTM_NEWADDR`,
  `RTM_DELADDR`, and `RTM_IFINFO` are notices. `RTM_ADD`, `RTM_DELETE`, and
  `RTM_CHANGE` are notices only when neither `RTF_LLINFO` nor `RTF_HOST` is
  set, because macOS keeps ARP and neighbor entries as host routes and a busy
  LAN would otherwise cause constant re-reads (inference). Other types are
  ignored.
- **Kernel event socket**, `socket(AF_SYSTEM, SOCK_RAW, SYSPROTO_EVENT)`,
  filtered with `SIOCSKEVFILT` to vendor Apple and the network class. Any
  event is a notice; the server does not parse the event body. It reports
  attach, detach, and link on and off, which the routing socket does not.
- A read error, such as `ENOBUFS` after a burst, causes an immediate
  re-read. A socket that cannot be opened is logged once at `warn`.

The monitor runs only while the configuration defines at least one
interface. When it runs, each re-read also refreshes the `self` addresses
of [0003 D11][d3-11] from every interface, so a newly attached phone's
address joins `self` within the debounce instead of within 10 s.

If the table cannot be read at start, every definition is offline with
reason `unknown`, so a list fails closed with D10 instead of falling back
to system routing. A later failed re-read keeps the previous table and is
logged once per run of failures.

### D6. Interface lists

| List | Key | Default | Applies to |
| --- | --- | --- | --- |
| Default | `proxy.interfaces` | `[]`: system routing | `direct` routes whose rule has no list, and the `direct` defaults |
| Per rule | `interfaces` in a `direct` `[[rule]]` | none: the default list | That rule's requests; it replaces the default list |
| Per upstream | `interfaces` in `[[upstream]]` | none: system routing | Dials to that upstream's own address (D11) |

- The list in force is the only set of interfaces a request may use, and
  its order is the priority. Failover stays inside the list. cuttle never
  falls back to system routing or to another interface, just as an upstream
  route never falls back to `direct` ([0003 D1][d3-1]).
- A rule's list both pins and restricts. `interfaces = ["eth-b"]` sends
  the rule's traffic only through Ethernet B, even while Ethernet A is
  online; `interfaces = ["eth-b", "eth-a"]` prefers B and allows A.
- Lists apply to local and remote clients alike.
- `cuttle explain` prints the list in force and where it came from (D15).

Validation errors, added to [0004 D4][d4-4]:

- A definition without `mac` and `device`; a `mac` that is not six bytes,
  is all zero, or has the group bit set; a `device` that does not match
  `^[a-z]+[0-9]+$`, is longer than 15 bytes, or names a loopback interface
  (`lo0`); a `kind` other than `wired`, `wifi`, or `cellular`.
- Duplicate definition names, two definitions with the same `mac`, or two
  with the same `device`. The name `other`.
- A list that names an unknown interface or names one twice. An empty list
  in a rule or an upstream, which would read as either "no interface" or
  "the default".
- `interfaces` on a rule whose action is not `direct`. An upstream route
  uses the upstream's list.
- `proxy.interface_attempt_timeout` outside 100 ms–10 min, or
  `proxy.interface_demotion` outside 1 s–10 min and not `0s`.

Validation warnings:

- A definition that no list uses.
- A definition with `device` and neither `mac` nor `kind`.
- A rule with `interfaces` and `allow_loopback` or `allow_private`, or
  whose `cidrs` are all loopback or private: lists do not apply to those
  addresses (D7).
- An upstream with `interfaces` whose address is a loopback literal.
- `proxy.interface_attempt_timeout` at or above `proxy.connect_timeout`:
  a timeout then never fails over.

### D7. Dial plan

After the [0003 D11][d3-11] check leaves the allowed addresses in the
resolver's order, a direct route with a list in force builds a plan of
groups:

1. **Group 0, unbound.** Allowed addresses of the classes `loopback`,
   `self`, and `private` are dialed without binding, through system
   routing. A LAN device, a phone's own subnet, this Mac, a VPN, or an
   overlay such as Tailscale in `100.64.0.0/10` is reachable only through
   its own route, and a bound socket ignores routes through other
   interfaces ([research 0003 §7][r3-d11]). This group comes first.
2. **One group per listed interface,** in list order, holding the `public`
   addresses whose family the interface is online for, each bound to it.
   An offline interface is skipped with its reason. An interface online
   for no family of those addresses is skipped with `no_family`. Demoted
   interfaces come after the others, keeping their relative order (D9).
3. If the plan has no group, the request gets D10.

A route without a list in force dials as today: one unbound group over all
allowed addresses.

The groups run one after another. Within a group the addresses keep the
RFC 8305 stagger of [0003 D11][d3-11]: the next address starts 300 ms after
the previous one unless that one has connected, and at once when it fails.
A NAT64 address is classified by its embedded IPv4 address but is an IPv6
address, so it needs the interface online for IPv6.

Binding sets `IP_BOUND_IF` for a `tcp4` socket or `IPV6_BOUND_IF` for a
`tcp6` socket to the interface index from the table, in
`net.Dialer.Control`, before `connect` ([research 0003 §1][r3-bind]). Go
passes `tcp4` or `tcp6`, never `tcp`, to the control function, because
cuttle dials address literals. An index that disappeared fails
`setsockopt` with `ENXIO`, which fails over (D8). An index reused by a
different device inside the 200 ms debounce would bind the wrong interface;
this race is accepted (assumption F7).

The lookup always runs first, through the system resolver (D13). There is
no check before it, because group 0 may still apply when no listed
interface is online.

### D8. Failover

A group ends when an attempt connects, when every attempt has ended, or at
its deadline. Its result decides what follows:

| Group result | Next step |
| --- | --- |
| An attempt connected | Done; later groups never start |
| None connected, and each attempt ended with a failover error or was still pending at the deadline | The next group starts at once |
| None connected, and at least one attempt ended with another error | The dial ends with the first such error, mapped by [0002 D6][d2-6]; later groups never start |

Failover errors are the local and path errors that say this interface
cannot carry the connection now:

| Error | Seen when |
| --- | --- |
| `ENETUNREACH` | No route through the interface, observed for an interface without an address; or an ICMP network unreachable from its router |
| `EHOSTUNREACH` | No route for the family on the interface, observed for IPv6 without a default route; or an ICMP host unreachable |
| `ENETDOWN`, `EHOSTDOWN` | The interface is down, or its next hop answers no ARP or neighbor solicitation |
| `EADDRNOTAVAIL` | The interface lost or has not finished its address |
| `ENXIO` | The interface index no longer exists |

These are the "fast connect failures" of the requirement. The observed
cases come from [research 0003 §1][r3-bind]; errno alone cannot tell a local
`EHOSTUNREACH` from one caused by ICMP, and both fail over.

Every other error ends the dial:

- `ECONNREFUSED` and `ECONNRESET`: the destination or a middlebox answered
  over a working path, and another interface would reach the same answer at
  a higher cost.
- `EACCES` and `EPERM`: a local policy refused, such as local network
  privacy ([OI-0007][oi-7]), the application firewall, or a content filter.
  These are never interface faults.
- Any error cuttle does not recognize: an unexplained error never spends a
  costlier interface.
- A dial abandoned because the client left is `client_closed`, as today.

Deadlines. Every group except the last gets
`proxy.interface_attempt_timeout` (default 3 s) or the rest of the connect
budget, whichever is shorter. The last group gets the rest of the budget.
The budget is `proxy.connect_timeout`, or the upstream's `connect_timeout`
for an upstream, and covers the lookup and every group, as in
[0003 D7][d3-7]. At a group's deadline its pending attempts are canceled. A
deadline is a failover condition because a phone without cellular data, or
a router whose uplink is down, may stay silent (assumption F4). 3 s lets a
healthy path connect after one lost SYN, since the initial retransmission
timeout is 1 s ([RFC 6298 §2][rfc6298]), and bounds the wait on a silent
path.

When the plan is exhausted, the last group's result maps by
[0002 D6][d2-6]: a deadline becomes `504 connection_timeout`, and a
failover error `502 connection_refused`, as unreachable errors are today.

How the target cases behave (inference except where research observed the
error):

| Case | Behavior | Added wait |
| --- | --- | --- |
| Adapter or phone detached | Skipped as `absent` once the notice is handled | None |
| Cable pulled, interface stays | Skipped as `link_down` after the kernel event; before it, a failover error or the deadline | Up to the deadline, once |
| No address or route on the interface | `ENETUNREACH` or `EHOSTUNREACH` at once (observed for UDP) | None |
| Router answers ICMP for a dead uplink | Failover error | One round trip |
| Silent dead uplink, or a phone without data | Deadline, then demotion (D9) | Once per demotion period |
| Destination refuses | `502`, no failover | — |
| Destination silent on every interface | `504` when the budget ends | `connect_timeout` |

### D9. Demotion

Without memory, a silent interface at the head of a list would delay every
new connection by the attempt timeout. Demotion remembers it briefly.

- **Trigger.** In one dial, the group of interface X failed over, and a
  later group, through another interface, connected. The success elsewhere
  shows that the destination was reachable, so the fault lies on X's path.
  A dial in which every group failed demotes nothing, so one dead
  destination cannot demote an interface.
- **Effect.** For `proxy.interface_demotion` (default `30s`; `0s`
  disables), X comes after the non-demoted interfaces in every list that
  holds it, keeping its place among demoted ones. X stays usable: a list
  with X alone, or whose other interfaces fail, still dials X.
- **End.** The period expires; a re-read changes X's row, such as its
  index, families, or reason; or a connection through X succeeds.
- **Logs and gauge.** One `warn` line when X is demoted,
  `msg="interface demoted" interface=eth-a device=en7
  error=connection_timeout for=30s`, and one `info` line when it ends,
  `msg="interface restored" interface=eth-a cause=expired`, where `cause`
  is `expired`, `state_changed`, or `connected`. Concurrent dials that
  demote X produce one line. `interface_state` shows `demoted` (D15).
- **Clock.** The proxy's injected clock times demotion, as it times the
  upstream hold.

Demotion reorders and never refuses, unlike the upstream hold of
[0003 D8][d3-8], because interfaces in a list are alternatives and a
restricted list must keep working.

### D10. No usable interface

When the plan has no group, the client gets:

| Route | Status | `Proxy-Status` | Outcome |
| --- | --- | --- | --- |
| `direct` | 503 | `<name>; error=destination_unavailable; details="no usable interface (eth-b: link_down)"` | `no_interface` |
| `upstream:<name>` with a list | 503 | The same, with `next-hop="<upstream>"` | `no_interface` |

- The body is `cuttle: no usable interface for <target>`. The details list
  each interface of the list in force with its reason, up to 200 bytes.
  Interface names are configured names, like rule and upstream names.
- No dial is made, and the upstream hold of [0003 D8][d3-8] neither starts
  nor extends.
- `503` says the condition is temporary and at the proxy, and it matches
  the held-upstream answer that operators already know. RFC 9209 defines
  `destination_unavailable` for a next hop the intermediary considers
  unavailable and recommends `503` ([RFC 9209 §2.3][rfc9209]).

### D11. Upstreams

- **No list (default).** The dial is unbound and follows system routing,
  even when `proxy.interfaces` is set. A bound socket ignores routes through
  a VPN's `utun` interface, so binding by default would break a private link
  that ends on this Mac, the third case of [0003 D13][d3-13], and would let
  traffic bypass a full-tunnel VPN ([research 0003 §7][r3-vpn]).
- **With a list.** Every resolved address of the upstream is bound, of any
  class, because [0003 D11][d3-11] does not apply to upstreams. Groups,
  failover, and demotion follow D7–D9 without group 0.
- The hold is checked first: a held upstream answers `503` before any
  planning. An exhausted plan is a failure to reach the upstream and starts
  the hold as today. No usable interface answers D10 and leaves the hold
  unchanged.
- A definition may name a `utun` interface by `device`, but `utun` names
  are assigned at run time (inference) and have no MAC. The supported way
  to use a VPN is to leave the upstream unbound.

### D12. Connection pools and tunnels

- Direct transports exist per policy snapshot, client class, and list in
  force. Each distinct list in a snapshot, the default list and each
  different rule list, gets its own transports, so a connection pooled
  under one list never serves a request under another. Within one list a
  pooled connection may be on any of its interfaces. This extends the pool
  rule of [0002 D5][d2-5].
- When an interface becomes online or leaves demotion, cuttle closes the
  idle connections of every direct and upstream transport whose list holds
  it. The next plain-HTTP request plans again and returns to the preferred
  interface instead of staying on a costlier one.
- Established tunnels and in-flight exchanges keep their interface until
  they close, because TCP cannot move a connection. When that interface goes
  away they fail and close, and the client reconnects with a new plan.
  Moving long tunnels back to a preferred interface is a later topic.

### D13. Name resolution

Lookups stay as they are: `Network.LookupNetIP` uses libSystem's
`getaddrinfo`, and mDNSResponder sends unicast queries over the primary
interface to its resolver ([research 0003 §2][r3-dns]). Consequences:

- **Privacy.** The primary network's resolver sees every name, including
  names pinned to a phone.
- **CDN and split horizon.** Answers suit the primary network. A server
  chosen for it may be slower over the bound interface, and a split-horizon
  name may resolve to an address unreachable through it.
- **Priority.** macOS picks the primary by its service order, not by
  cuttle's list. The user should set the service order in System Settings →
  Network to match `proxy.interfaces`, so that lookups and unbound dials
  follow the same priority.
- **Failures.** A lookup fails before any interface is chosen, so it never
  fails over; it answers `502 dns_error` as today.
- **Cost.** Lookups are small but use the primary network.

Decision: a documented limitation in M1.5, with no code change. Scoped
lookups, by Go's resolver over a bound UDP socket with each interface's
servers or by `DNSServiceGetAddrInfo` with an interface index through cgo,
are a later topic; the first depends on research 0003 open question 8.

### D14. Reload

- Every key of D2 and D6 applies on reload ([0004 D6][d4-6]). Reload
  re-reads the interface table before it swaps the snapshot.
- A definition whose `mac`, `device`, and `kind` are unchanged keeps its
  state, `since` time, and demotion. A changed or new definition starts
  from the fresh table without demotion.
- An upstream whose `interfaces` changed counts as a changed upstream and
  gets a new transport and hold state.
- Adding the first definition starts the monitor; removing the last stops
  it.
- In-flight requests keep their snapshot, and established tunnels keep
  their interface, even when the new lists no longer allow it.

### D15. Status, command line, logging, counters, and statistics

**`cuttle status`.** `/v1/status` ([0006 D4][d6-4]) gains an `interfaces`
array in configuration order, and the text form shows the same fields:

```json
"interfaces": [
  {"name": "eth-a", "device": "en7", "state": "online",
   "families": ["ipv4"], "primary": true,
   "since": "2026-11-25T00:00:00Z"},
  {"name": "eth-b", "device": "en8", "state": "demoted",
   "families": ["ipv4"], "primary": false,
   "since": "2026-11-25T00:58:00Z",
   "demoted_until": "2026-11-25T00:58:30Z"},
  {"name": "phone-b", "device": "", "state": "offline",
   "reason": "absent", "since": "2026-11-25T00:00:00Z"}]
```

No MAC address appears in status output, logs, counters, or statistics.

**`cuttle interfaces [--json] [--all] [--watch] [--config PATH]`.** A new
command that reads the system directly and needs no running service. It
helps the user write definitions and answers the device check (I18).

```text
$ cuttle interfaces
device  kind   mac                media     ipv4  ipv6  default  match
en0     wifi   00:00:5e:00:53:01  active    yes   no    scoped   wifi
en7     wired  00:00:5e:00:53:0a  active    yes   no    primary  eth-a
en3     wired  00:00:5e:00:53:03  inactive  no    no    none     -
```

The `index` and `port` columns are left out of this example for width.

- Columns: index, BSD name, kind (D3), MAC, media (`active`, `inactive`,
  or `none`), whether each family has a usable address, the default route
  (`primary`, `scoped`, or `none`), the hardware port name, and the
  matching definition, with its reason when it is not online.
- The port name comes from one run of `networksetup
  -listallhardwareports`. If that fails, the column is empty. This command
  is the only place cuttle runs another program, and the server never
  does.
- The default lists interfaces with a six-byte MAC; `--all` lists every
  interface, `lo0` and `utun` included. Without a readable configuration
  the `match` column is empty.
- `--watch` prints one timestamped line per change that a D5 re-read
  finds, with the notice kinds that triggered it: `address`, `route`,
  `ifinfo`, `attach`, `detach`, `link_on`, `link_off`, or `poll`. For this
  the command decodes the kernel event codes from the public
  `net/net_kev.h`.
- This command prints MAC addresses to the terminal, because the user needs
  them for the configuration. Nothing else does.
- Exit status 0 on success, 1 when the table cannot be read, 2 on a usage
  error.

**`cuttle explain`.** Adds a line for the list in force:

```text
interfaces  eth-b (rule system-updates); public addresses only
```

It prints `proxy.interfaces` as the source for the default list, the
upstream's name for an upstream with a list, and `system routing` when no
list applies. `explain` reads only the file, never the live state.

**Decision log.** Two fields follow `upstream` ([0002 D8][d2-8]):

- `interface`: the interface that carried the outbound connection,
  present when the decision made or used one, and empty otherwise. For a
  bound dial it is the configured name. For an unbound dial, or a pooled
  connection, it is the definition whose interface owns the connection's
  local address in the current table, or `other` when none or several do.
- `skipped`: present when interfaces that come before the chosen one in
  the list in force were not used, as `name:reason` pairs separated by
  commas in list order. Reasons are the D4 reasons, `no_family`, `demoted`
  for an interface placed later by D9, `unreachable` for a failover error,
  and `timeout`. When no connection was made, it lists every interface of
  the list.

```text
time=2026-11-25T01:01:02.345Z level=INFO msg=decision id=4f2a
client=127.0.0.1:51234 source=local kind=connect
target=updates.example:443 rule=system-updates action=direct upstream=
interface=eth-b outcome=ok status=200 up=5321 down=88410 dur=12.402s
```

A plain-HTTP request learns the interface of a pooled connection from the
connection itself, which the dialer wraps with its binding.

**State lines.** `warn` when an interface leaves `online`:
`msg="interface offline" interface=phone-b device=en9 reason=link_down`.
`info` when it returns: `msg="interface online" interface=phone-b
device=en9 families=ipv4 offline_for=42.3s`. Demotion lines are in D9 and
identity warnings in D3. Each is written once per transition. At start one
`info` line per definition gives its state.

**Counters**, added to [0002 D7][d2-7]:

| Counter | Labels |
| --- | --- |
| `interface_state` | `interface`, `state` = `online`, `demoted`, `offline`; a 0/1 gauge like `upstream_state` |
| `interface_bytes_total` | `interface`, `direction` = `up`, `down`; the D7 bytes of each decision, under its `interface` value |
| `interface_dials_total` | `interface`, `result` = `ok`, `unreachable`, `timeout`, `error`; one count per group of a bound dial |

The `interface` label takes configured names and `other`, so it stays
bounded. `outcomes_total` gains the outcome `no_interface`.

**Statistics (M2).** `host_hour` ([0005 D2][d5-2]) gains a key column
`interface`, empty for decisions without an outbound connection. The
dashboard shows totals per interface and filters the domain table by
interface. Because M2 has not started, schema version 1 includes the
column.

### D16. Dependencies and platform code

- No new module and no cgo. Binding, state, and notices use `syscall`.
  `CGO_ENABLED=0 go build ./cmd/cuttle` must keep working (I17).
- `golang.org/x/net/route` is not used. `syscall.RouteRIB` and
  `syscall.ParseRoutingMessage` return the same default routes. They are
  marked deprecated in favor of `x/net/route` but stay under the Go 1
  compatibility promise, and `go vet` in the T0 gate does not flag them.
  [0002 D1][d2-1] keeps `golang.org/x/net` test-only. If the standard
  parser ever fails on a macOS release, a small parser of `rt_msghdr` in
  cuttle is the fallback, still without a dependency.
- Constants missing from `syscall` are defined in cuttle with the public
  header they come from: `SYSPROTO_EVENT`, `SIOCSKEVFILT`,
  `KEV_VENDOR_APPLE`, and `KEV_NETWORK_CLASS` (`sys/kern_event.h`,
  `sys/sys_domain.h`), `SIOCGIFFUNCTIONALTYPE` and the functional types
  (`sys/sockio.h`, `net/if.h`), and the kernel event codes for `--watch`
  (`net/net_kev.h`). `IP_BOUND_IF`, `IPV6_BOUND_IF`, `SIOCGIFMEDIA`,
  `AF_ROUTE`, `AF_SYSTEM`, `RTF_IFSCOPE`, and `NET_RT_DUMP` are in
  `syscall`.
- The private ioctls `SIOCGIFEFLAGS`, `SIOCGIFXFLAGS`, and `SIOCGIFTYPE`
  are not used. The `expensive` and `constrained` flags they expose are not
  needed when the user orders interfaces explicitly.
- A new package `internal/iface` holds definitions, matching, the online
  predicate, plans, and demotion as portable logic, and the darwin reader
  and monitor in `_darwin.go` files. `internal/netx` gains the binding and
  the bound dial.

## Example configuration

Placeholders only. The MACs are documentation values, and the domain is
reserved.

```toml
name = "cuttle-a1b2c3"

[proxy]
listen = ["127.0.0.1:3128"]
default = "direct"
interfaces = ["eth-a", "eth-b", "wifi", "phone-a", "phone-b"]
# interface_attempt_timeout = "3s"
# interface_demotion = "30s"

[[interface]]
name = "eth-a"
mac = "00:00:5e:00:53:0a"
kind = "wired"

[[interface]]
name = "eth-b"
mac = "00:00:5e:00:53:0b"
kind = "wired"

[[interface]]
name = "wifi"
device = "en0"
kind = "wifi"

[[interface]]
name = "phone-a"
mac = "00:00:5e:00:53:1a"   # kind left unset until the device check

[[interface]]
name = "phone-b"
mac = "00:00:5e:00:53:1b"

[[rule]]
name = "system-updates"     # pinned to B even while A is online
domains = ["updates.example"]
action = "direct"
interfaces = ["eth-b"]
```

## Interfaces

```go
// internal/netx

// Binding names the interface a dial must use. The zero value is an
// unbound dial that follows system routing.
type Binding struct {
    Name  string // configured interface name, for logs and counters
    Index int    // system interface index for IP_BOUND_IF and IPV6_BOUND_IF
}

// Network is the only path from the proxy to name resolution and sockets.
// Tests replace it (0007 D3).
type Network interface {
    LookupNetIP(ctx context.Context, host string) ([]netip.Addr, error)
    DialContext(ctx context.Context, addr netip.AddrPort,
        b Binding) (net.Conn, error)
}

// internal/iface

// Link is one row of the system interface table.
type Link struct {
    Index    int
    Device   string
    MAC      net.HardwareAddr
    Up       bool
    Kind     Kind  // from SIOCGIFFUNCTIONALTYPE
    Media    Media // MediaActive, MediaInactive, MediaUnknown
    Addrs    []netip.Addr
    Default4 Route // RouteNone, RouteScoped, RoutePrimary
    Default6 Route
}

// Source supplies the table. On darwin it reads system calls and sends a
// notice for each routing or kernel event; tests supply rows and notices.
type Source interface {
    Read() ([]Link, error)
    Notices() <-chan Notice
}
```

- `policy.Decision` gains `Interfaces []string`, the list in force for a
  `direct` route: the rule's list, else `proxy.interfaces`. `Decide` stays
  pure.
- `proxy.Deps` gains `Interfaces iface.Source`. `proxy.New` fails when the
  configuration defines an interface and no source is given, so a test
  cannot reach the real table by accident.
- `testenv.Net` records the binding with each dial and accepts behaviors
  per interface and address: route, refuse, a chosen errno, or blackhole.
  An address without a per-interface behavior acts as for an unbound dial.
- `testenv.Interfaces` is a fake source whose rows a test sets and whose
  notices it sends.

## Failure behavior

- An offline interface is skipped and never bound. When no group remains,
  the request gets D10 without a dial.
- A failover error or a group deadline moves to the next group. Any other
  error ends the dial with the [0002 D6][d2-6] mapping.
- An interface that disappears under an established tunnel ends that
  tunnel; the client's next connection gets a new plan.
- A notice socket that cannot be opened is logged once, and the safety
  re-read runs every 5 s instead of 30 s.
- A table that cannot be read at start leaves every definition offline
  (`unknown`), so lists fail closed. A later failed re-read keeps the last
  table.
- Interface selection never sends a request of cuttle's own, as
  [0003 D5][d3-5] requires.

## Acceptance criteria

T1 tests with the mocks of [0007](0007-test-environment-and-gates.md) and
an injected interface source, unless marked T0 or T2. Test interfaces are
named `eth-a`, `eth-b`, and `wifi`, with documentation MACs.

| ID | Criterion | Check |
| --- | --- | --- |
| I1 | Each D6 validation error has a sample file that fails with the documented key path and line, and each D6 warning appears in `cuttle check` output with exit status 0. | T0 `TestConfigInterfaces` |
| I2 | Matching and the online predicate give the D3 and D4 state, reason, and families for injected rows: a MAC match, a `device` match, both keys, `device` present with another MAC (`absent`), no row (`absent`), two rows with one MAC (`ambiguous`), a `kind` mismatch, `admin_down`, `link_down`, media not reported with address and route (online), only `169.254.1.2` (`no_address`), an address without a default route (`no_route`), IPv4 only, IPv6 only, and both. | T0 `TestInterfaceState` |
| I3 | Plans follow D7: list order, a rule list replacing the default list, group 0 first with `loopback`, `self`, and `private` addresses, family filtering with `no_family`, offline interfaces skipped with their reasons, demoted interfaces last in their relative order, and no group giving D10. | T0 `TestDialPlan` |
| I4 | Each failover error of D8 moves to the next group; `ECONNREFUSED`, `ECONNRESET`, `EACCES`, `EPERM`, and an unknown error end the dial; a group with one refused and one unreachable attempt ends the dial; deadlines give every group but the last `interface_attempt_timeout` and the last the rest of the budget. | T0 `TestFailover` |
| I5 | A real socket gets the option. A `tcp4` dial bound to `lo0`'s index connects to a listener on `127.0.0.1`, and one to `192.0.2.1:9` fails with `ENETUNREACH` within 100 ms. A `tcp6` dial bound with `IPV6_BOUND_IF` connects to `[::1]`, and one to `[2001:db8::1]:9` fails at once with `ENETUNREACH` or `EHOSTUNREACH`. An index that names no interface fails with `ENXIO`, classified as a failover error. Where `getsockopt` supports the option, it returns the index. No packet leaves the host, and this settles the no-route half of research open question 1 for TCP. | T1 `TestBoundDial` |
| I6 | With `proxy.interfaces = ["eth-a", "eth-b", "wifi"]`, `interface_attempt_timeout = "300ms"`, and `connect_timeout = "2s"`, a direct `CONNECT`: (a) with all reachable dials once, bound to `eth-a`, and logs `interface=eth-a` without `skipped`; (b) with `eth-a` `link_down` makes no `eth-a` dial and logs `skipped=eth-a:link_down`; (c) with `eth-a` failing `ENETUNREACH` starts the `eth-b` dial within 50 ms; (d) with `eth-a` blackholed starts `eth-b` 300 ± 100 ms after `eth-a` and logs `skipped=eth-a:timeout`; (e) with `eth-a` refusing gets `502 connection_refused` and no `eth-b` or `wifi` dial; (f) with `eth-a` failing `EPERM` gets `502` and no other dial; (g) with all three blackholed gets `504 connection_timeout` after 2 s ± 200 ms, the `wifi` group having the rest of the budget. Cases (a), (b), and (d) also pass for plain HTTP. | `TestInterfaceFailover` |
| I7 | A rule with `interfaces = ["eth-b"]` dials only `eth-b` while `eth-a` is online. With `eth-b` absent it gets `503` with `Proxy-Status: <name>; error=destination_unavailable; details="no usable interface (eth-b: absent)"`, outcome `no_interface`, and no dial. A rule with `["eth-b", "eth-a"]` uses `eth-a` when `eth-b` is `link_down`. With `proxy.interfaces` unset, a request whose rule has no list dials unbound. | `TestInterfacePinning` |
| I8 | After I6 (d), with an injected clock and `interface_demotion = "1s"`, the next request to another destination connects through `eth-b` without an `eth-a` dial and logs `skipped=eth-a:demoted`, `interface_state{interface="eth-a",state="demoted"}` is 1, and one `interface demoted` line is logged. After 1 s `eth-a` is first again with one `interface restored` line. A notice that changes `eth-a`'s row ends a demotion at once. A dial in which every group timed out demotes nothing. | `TestInterfaceDemotion` |
| I9 | With every listed interface offline, the starter `loopback` rule's targets, the literals `10.20.0.5` and `192.0.2.50` (`self`), and a name resolving to `10.20.0.5` through an `allow_private` rule are dialed unbound; a public address gets D10. With `eth-a` online for IPv4 only and `eth-b` for both, a name with AAAA and A records gets only the IPv4 attempt on `eth-a`, an IPv6-only name skips `eth-a` with `no_family` and connects on `eth-b`, and with no interface online for IPv6 it gets D10. | `TestInterfaceAddressClasses` |
| I10 | A plain-HTTP connection pooled under the default list on `eth-a` is not reused for a request to the same host and port under a rule pinned to `eth-b`. With `eth-a` offline a request pools a connection on `eth-b`; after a notice brings `eth-a` online, that idle connection is closed and the next request dials `eth-a`. | `TestInterfacePools` |
| I11 | An upstream without `interfaces` is dialed unbound while `proxy.interfaces` is set. An upstream with `["eth-a", "wifi"]` dials `eth-a` and fails over to `wifi` on `ENETUNREACH`. With neither online it gets `503` with `error=destination_unavailable` and `next-hop` naming the upstream, outcome `no_interface`, no dial, and `upstream_state` stays `up`. When both groups time out, the hold starts as in [0003 U11][d3-ac]. | `TestUpstreamInterfaces` |
| I12 | Row changes followed by a notice give one `interface offline` line with the reason, one `interface online` line with `families` on recovery, and the matching `interface_state` gauge. An ambiguous definition, a `kind` mismatch, and a `device` with another MAC each log their D3 line once. `cuttle status` and `/v1/status` list each interface with name, device, state, reason, families, primary, and since. Neither the status output nor the log contains any test MAC in any case or separator form. | `TestInterfaceStateEvents`, `TestStatusInterfaces` |
| I13 | A tunnel through `eth-b` keeps transferring across a reload that removes `eth-b` from every list and changes its definition; a request after the reload follows the new lists; a demotion of an unchanged definition survives a reload; adding the first definition starts the monitor and removing the last stops it. | `TestReloadInterfaces` |
| I14 | A tunnel through `eth-b` of 1,048,576 bytes down and 65,536 bytes up adds exactly those bytes to `interface_bytes_total{interface="eth-b"}`. `interface_dials_total` counts the results of the I6 cases. A blocked request logs an empty `interface`. Attribution of unbound connections by local address gives the owning definition, or `other` for none or several owners (T0). | `TestInterfaceCounters`; T0 `TestInterfaceAttribution` |
| I15 | `cuttle interfaces --json` exits 0 on the test host with rows holding every D15 column; `--all` also lists `lo0`. A unit test formats an injected table, including `match` with a definition and its reason. `cuttle explain` prints the list in force and its source for a direct rule, the default list, an upstream with a list, and `system routing`. | T1 `TestInterfacesCommand`, `TestExplainInterfaces`; T0 `TestInterfacesFormat` |
| I16 | On darwin, the monitor opens the routing socket and the kernel event socket without privileges. With injected notices, 20 notices within 100 ms cause exactly one re-read 200 ms after the first; a read error causes an immediate re-read; routing messages with `RTF_LLINFO` or `RTF_HOST` cause none; a configuration without definitions opens no socket. | `TestMonitor` |
| I17 | `go list -deps ./cmd/cuttle` lists neither `internal/testenv` nor any package under `golang.org/x/net/` (the standard library's `vendor/golang.org/x/net` packages are allowed), and `CGO_ENABLED=0 go build ./cmd/cuttle` succeeds. | T0 gate (change to [0007 D7][d7-7]) |
| I18 | The device check below is recorded on the user's Mac with the user's five interfaces. | T2, manual, with the user |

### Device check (I18)

The tester runs these steps with the user present, from Terminal, which is
exempt from local network privacy ([research 0003 §7][r3-lnp]). No step
reaches a LAN address. Results go to the
[compatibility matrix](../COMPATIBILITY.md#interfaces) without MACs,
addresses, or network names.

| # | Step | Record | Settles |
| --- | --- | --- | --- |
| 1 | Attach all five interfaces and run `cuttle interfaces --all`; write the definitions from it | Each device's kind, media support, and port name; whether the two phones get distinct BSD names and MACs; whether they share a subnet, compared on screen and not recorded | Research open questions 2 and 9; F2, F3 |
| 2 | Replug each phone into another USB port, then reboot, and run `cuttle interfaces` after each | Whether each phone keeps its MAC and BSD name | Open question 2; F2 |
| 3 | Swap the two Ethernet adapters between ports | The `match` column follows the adapters | Open question 3 |
| 4 | Optional: attach a third adapter of the same model while Ethernet B is absent | Whether it takes B's BSD name, and that `eth-b` stays `absent` | Open question 3 |
| 5 | Optional: move a Thunderbolt adapter or dock to another port | Whether `Slot N` changes | Open question 4 |
| 6 | Run `cuttle interfaces --watch` while pulling an adapter's cable, detaching an adapter, detaching a phone, and turning off a phone's Personal Hotspot | Notice kinds, the time until the state changes, and the index values after reattachment | Open question 5; F5, F7 |
| 7 | Run `cuttle serve` with the example configuration and send `curl -x http://127.0.0.1:<port> https://example.com/` several times | `interface=` names the first online interface, and `nettop -m tcp`, or the local address in `netstat -n -p tcp`, puts the flow on that device | Open question 1; F1 |
| 8 | During step 7, pull Ethernet A's cable | The next decision logs `skipped=eth-a:link_down`, and the `interface offline` line appears | D4, D5 |
| 9 | Pin a test domain to `phone-b`, request it, then detach `phone-b` and request it again | First `interface=phone-b`; then `503` with `no_interface` | D6, D10 |
| 10 | With the user's consent, turn off cellular data on phone A and list it first | The `skipped` reason (`timeout` or `unreachable`), the time to fail over, and the demotion line | Open question 6; F4 |

## Assumptions

| ID | Assumption | Smallest check |
| --- | --- | --- |
| F1 | A bound TCP connect behaves like the observed UDP probe: an interface without an address or route fails locally without a SYN, and a non-primary interface carries the flow. | I5 for the no-route half; device check step 7 for the flow |
| F2 | Each phone's tethering interface keeps its MAC and BSD name across replugging, port changes, and reboots, and two phones have distinct MACs. If not, a phone is defined by `device` with `kind`, accepting the takeover risk of D2. | Device check steps 1–2 |
| F3 | A tethered iPhone reports media status or none, gets a scoped default route while sharing, and reports a functional type that the user can use as `kind`. | Device check steps 1 and 6 |
| F4 | A phone without cellular data either fails a connect fast or stays silent; D8 handles both. | Device check step 10 |
| F5 | After a cable is pulled or sharing stops, a routing or kernel notice arrives within about 1 s, so few requests see the stale state. | Device check step 6 |
| F6 | Local network privacy refuses only connections to local network addresses, which are not `public` and are dialed unbound in group 0, and a denial returns `EPERM`, `EACCES`, or another error outside D8's failover list. A LAN with public addresses would be the exception. | The [OI-0007][oi-7] check records the error |
| F7 | A different device does not reuse a detached interface's index within the 200 ms debounce. | Device check step 6, comparing indexes |

## Changes to other records

The orchestrator applies these after TASK-0007 lands. Each item names the
record, the section, and the change.

1. **0002 D1**, package table: add `internal/iface` — "Interface
   definitions, matching, online state, notice monitor (darwin), dial plans,
   demotion".
2. **0002 D5**, outbound connection pools, first bullet: "Direct transports
   exist per policy snapshot, per client class, and per interface list in
   force ([0008 D12][d8-12])", with the reason that a connection pooled
   under one list never serves another.
3. **0002 D6**, response table: add the row "No usable interface in the
   list in force ([0008 D10][d8-10]) | 503 | `destination_unavailable` |
   `no_interface`". In the paragraph on reflected values, add configured
   interface names.
4. **0002 D7**, counter table: add `interface_state`,
   `interface_bytes_total`, and `interface_dials_total` as in
   [0008 D15][d8-15], and `no_interface` to the outcomes of
   `outcomes_total`.
5. **0002 D8**, decision log: add the fields `interface` and `skipped`
   after `upstream`, with the D15 definitions and example.
6. **0002 Interfaces**: `Network.DialContext` gains the parameter
   `b netx.Binding`; `Decision` gains `Interfaces []string`.
7. **0003 D6**, table: add "No usable interface in the upstream's list |
   503 | `error=destination_unavailable; next-hop="<upstream>"`".
8. **0003 D7**, timeout table: add "Interface attempt | 3 s |
   `proxy.interface_attempt_timeout` | One interface's share of a direct or
   upstream connect budget ([0008 D8][d8-8])", and change the scope of the
   direct and upstream connect rows to "all addresses and interfaces".
9. **0003 D8**: under Trigger, add that no usable interface
   ([0008 D10][d8-10]) neither starts nor extends a hold; under Reload, add
   that a changed `interfaces` list makes a changed upstream.
10. **0003 D11**: in step 4, dial "through the [0008 D7][d8-7] plan when an
    interface list is in force"; in the `self` row, "refreshed at most
    every 10 s, and on interface notices while the 0008 monitor runs".
11. **0003 D13**: after the source-address table, add that an upstream
    reached over a private link that ends on the edge machine must leave
    `interfaces` unset ([0008 D11][d8-11]).
12. **0004 D3**: add the `[[interface]]` table of [0008 D2][d8-2]; add to
    `[proxy]` the keys `interfaces` (array, `[]`, reload),
    `interface_attempt_timeout` (duration, `"3s"`, reload, 100 ms–10 min),
    and `interface_demotion` (duration, `"30s"`, reload, `"0s"` or
    1 s–10 min); add `interfaces` to `[[rule]]` (`direct` only) and to
    `[[upstream]]`.
13. **0004 D4**: add the errors and warnings of [0008 D6][d8-6].
14. **0004 D5**, starter file: under `[proxy]` add the comment
    `# interfaces = ["eth-a", "wifi"]  # see cuttle interfaces; unset uses
    system routing`, and at the end a commented `[[interface]]` example
    with `name = "eth-a"` and `mac = "00:00:5e:00:53:0a"`, and one with
    `name = "wifi"`, `device = "en0"`, and `kind = "wifi"`.
15. **0004 D6**: in step 2 the snapshot includes interface definitions and
    lists; add "re-read the interface table" before the swap; in step 3
    the compared upstream settings include `interfaces`.
16. **0005 D1**: add the field `interface` ("configured interface name,
    `other`, or empty without an outbound connection") and add
    `no_interface` to the outcomes counted in `failures`.
17. **0005 D2**: add `interface TEXT NOT NULL DEFAULT ''` to `host_hour`,
    to its primary key after `source`, and to the `ON CONFLICT` target.
18. **0005 D9**: the overview shows totals per interface and interface
    states; `/domains` takes an `interface` filter.
19. **0005 S1**: the scripted mix includes a direct request through a test
    interface, and its rows carry that interface.
20. **0006 D3**: add `cuttle interfaces [--json] [--all] [--watch]
    [--config PATH]` — "List system interfaces and their match to the
    configuration ([0008 D15][d8-15])".
21. **0006 D4**: `/v1/status` lists `interfaces` with their state; add the
    D15 fragment to the example.
22. **0006 D7**, levels: `warn` adds interfaces going offline or demoted
    and the D3 identity warnings; `info` adds interfaces coming online or
    restored.
23. **0007 D3**: `testenv.Net` records the binding of each dial and takes
    per-interface behaviors; add `testenv.Interfaces`; "Only `Network`,
    `SelfAddrs`, and the interface source are injected".
24. **0007 D7**, T0 commands: replace the dependency pattern with
    `grep -E 'internal/testenv|^golang\.org/x/net/' "$deps"` and add
    `CGO_ENABLED=0 go build -o /dev/null ./cmd/cuttle`; `scripts/gate t0`
    follows. Under "T2 at M1 close", add the [0008 device check][d8-dc].
25. **README.md**: document `[[interface]]`, `proxy.interfaces`, rule and
    upstream lists, `cuttle interfaces`, the advice to match the macOS
    service order, and the DNS limitation of [0008 D13][d8-13].
26. **ROADMAP.md, Later**: add "Scoped DNS lookups per interface" and
    "Moving established tunnels back to a preferred interface".

## Open issues to record

1. **New: Interface behavior on the user's devices.** Origin: research
   0003 open questions 1–6 and 9, TASK-0008. Affected milestones: M1.5 and
   M1 close. Responsible step: tester with the user. Blocking reason:
   assumptions F1–F5 and F7 rest on source reading and on observations
   without phones or USB adapters attached; the device check needs the
   user's devices and presence. Next action: tester — run the
   [0008 device check][d8-dc] with the user and record it in the
   compatibility matrix.
2. **Update OI-0007** (local network privacy): add that
   [0008 D8][d8-8] relies on the denial's error code (assumption F6), so
   the check must record the errno, and that a public-address LAN is the
   case to note in the deployment guide.

## Consequences

- One configuration expresses the cost order and per-destination pins
  without changing macOS's own routing, and other applications keep using
  the system's primary interface.
- A dead uplink behind a live link costs one attempt timeout, then a
  demotion period on the next interface. cuttle cannot tell an expensive
  detour from a needed one without probes, which the user ruled out.
- DNS and unbound dials still follow the macOS service order; the user
  aligns it with `proxy.interfaces`.
- A tunnel stays on a costlier interface until it closes after the
  preferred interface returns.
- Every decision line and the M2 statistics name the interface that
  carried the traffic, including unbound dials, by local address.

## Alternatives considered

- **Binding a source address with `net.Dialer.LocalAddr`.** It needs the
  current address of each family, breaks on DHCP renewal and IPv6 temporary
  addresses, and is ambiguous when two phones give the Mac the same address
  ([research 0003 §1][r3-bind]).
- **Changing the macOS service order or routes.** It needs privileges,
  affects every application, and cannot pin single destinations.
- **Probing reachability**, by connecting to a known host or by reading
  `NWPath`. The user ruled out probe traffic, and `NWPath` needs
  Network.framework through cgo.
- **SystemConfiguration notifications.** They need cgo or trampolines with
  callbacks; the routing and kernel event sockets give the same triggers in
  pure Go.
- **`golang.org/x/net/route`.** The same data comes from `syscall`
  (D16).
- **Matching by hardware port or service name.** Port names are identical
  for identical models, and service names depend on attach order and can
  be renamed ([research 0003 §6][r3-ids]).
- **Racing interfaces in parallel like happy eyeballs.** It would open
  SYNs on costlier interfaces while a cheaper one still answers, and a slow
  cheaper path would lose whole tunnels to a costlier one.
- **No failover on timeout.** It follows the literal "fast failure" rule,
  but a phone without data or a silent dead uplink would make every request
  through it wait the whole connect budget and fail.
- **A fail-closed interface hold like [0003 D8][d3-8].** It would break a
  list with one interface; demotion reorders instead.
- **`502 destination_ip_unroutable` for no usable interface.** It names an
  unroutable destination address, while the cause is the state of cuttle's
  own interfaces, which is temporary.
- **Binding upstreams with the default list.** It ignores VPN routes and
  would break the private link of [0003 D13][d3-13].
- **Scoped DNS in M1.5.** It needs per-interface servers from
  `scutil --dns` parsing or cgo, loses mDNSResponder's cache and VPN split
  DNS, and has an open question on dialogs; deferred (D13).

[d2-1]: 0002-proxy-core-and-routing.md#d1-toolchain-dependencies-and-layout
[d2-4]: 0002-proxy-core-and-routing.md#d4-rules-and-evaluation-order
[d2-5]: 0002-proxy-core-and-routing.md#d5-actions-and-forwarding
[d2-6]: 0002-proxy-core-and-routing.md#d6-responses-generated-by-cuttle
[d2-7]: 0002-proxy-core-and-routing.md#d7-byte-accounting-and-live-counters
[d2-8]: 0002-proxy-core-and-routing.md#d8-decision-log
[d3-1]: 0003-upstream-chaining-and-exposure.md#d1-upstreams-and-disclosure-modes
[d3-5]: 0003-upstream-chaining-and-exposure.md#d5-non-disclosure-toward-an-egress-proxy
[d3-7]: 0003-upstream-chaining-and-exposure.md#d7-timeouts
[d3-8]: 0003-upstream-chaining-and-exposure.md#d8-unreachable-upstream-hold
[d3-11]: 0003-upstream-chaining-and-exposure.md#d11-destination-address-policy-for-direct-routes
[d3-13]: 0003-upstream-chaining-and-exposure.md#d13-edge-gateway-and-egress-deployment
[d3-ac]: 0003-upstream-chaining-and-exposure.md#acceptance-criteria
[d4-4]: 0004-configuration-and-reload.md#d4-validation
[d4-6]: 0004-configuration-and-reload.md#d6-reload
[d5-2]: 0005-statistics-and-admin.md#d2-schema
[d6-4]: 0006-packaging-paths-and-lifecycle.md#d4-control-socket
[d7-7]: 0007-test-environment-and-gates.md#d7-gates
[d8-2]: #d2-interface-definitions
[d8-6]: #d6-interface-lists
[d8-7]: #d7-dial-plan
[d8-8]: #d8-failover
[d8-10]: #d10-no-usable-interface
[d8-11]: #d11-upstreams
[d8-12]: #d12-connection-pools-and-tunnels
[d8-13]: #d13-name-resolution
[d8-15]: #d15-status-command-line-logging-counters-and-statistics
[d8-dc]: #device-check-i18
[oi-7]: ../OPEN_ISSUES.md#oi-0007--local-network-privacy-for-cuttle-under-launchd
[r3]: ../research/0003-interface-selection.md
[r3-4]: ../research/0003-interface-selection.md#4-network-identity-secondary
[r3-bind]: ../research/0003-interface-selection.md#1-binding-a-tcp-connection-to-one-interface
[r3-bsd]: ../research/0003-interface-selection.md#how-macos-assigns-bsd-names
[r3-d11]: ../research/0003-interface-selection.md#destination-checks-0003-d11-and-self
[r3-dns]: ../research/0003-interface-selection.md#2-name-resolution
[r3-ids]: ../research/0003-interface-selection.md#identifier-set-ordered-by-robustness
[r3-lnp]: ../research/0003-interface-selection.md#local-network-privacy
[r3-mac]: ../research/0003-interface-selection.md#mac-addresses
[r3-notice]: ../research/0003-interface-selection.md#noticing-changes
[r3-read]: ../research/0003-interface-selection.md#reading-state
[r3-vpn]: ../research/0003-interface-selection.md#vpns-and-the-private-link
[rfc6298]: https://www.rfc-editor.org/rfc/rfc6298.html#section-2
[rfc9209]: https://www.rfc-editor.org/rfc/rfc9209.html#section-2.3
[rfc9542]: https://www.rfc-editor.org/rfc/rfc9542.html#section-2.1.4

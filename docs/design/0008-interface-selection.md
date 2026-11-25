# 0008 — Interface selection

**Status** — Proposed (TASK-0008, 2026-11-25; revised in iterations 2
and 3).

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

In iteration 2 the user confirmed that a 3 s attempt timeout fails over
(D8), kept demotion global with its cost effect (D9), accepted that a dead
uplink behind the primary interface fails names at DNS (D13), and chose two
`[[upstream]]` entries with one address for per-destination interface
needs on upstream routes (D11). In iteration 3 the user stated that no
public destination goes through a VPN, so bound dials that bypass VPN
routes are documented and get no rule option (D7).

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
counters, and statistics.

## Decisions

### D1. Scope

- Interface selection applies to `direct` routes, for `CONNECT`, plain
  HTTP, and upgrades, and to every upstream that sets its own interface
  list (D11).
- A configuration with no `[[interface]]` and no list routes exactly as
  in M1.4: every dial is unbound and follows system routing, and cuttle
  opens no notice socket. Its decision lines and status entries still
  gain the `interface` field, which stays empty (D15).
- Selection never widens what a request may reach. The rules
  ([0002 D4][d2-4]) and the destination address policy
  ([0003 D11][d3-11]) run first, unchanged.
- Interface failover moves one route to another interface of its own list.
  It is neither route failover nor upstream failover: a `direct` route
  never becomes an upstream route, and an upstream route never moves to
  another upstream ([0003 D1][d3-1]).
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
- A definition that no list names still attributes traffic by local
  address (D15), so the user can count traffic per interface while every
  dial follows system routing.

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
MAC, flags, kind, media status, addresses, and default routes per family
(D5). Loopback interfaces never match. A definition matches a row when
every key it sets is equal: `mac` to the row's hardware address, `device`
to its BSD name.

| Rows matched | State | Reason | Log, once per transition |
| --- | --- | --- | --- |
| None | offline | `absent` | `interface offline` with the reason; when `device` is present with another MAC, also `interface device has a different MAC` |
| One, `kind` equal or unset | Judged by D4 | — | — |
| One, `kind` differs | offline | `kind_mismatch` | `interface kind mismatch` with the device, its kind, and the expected kind |
| Several | offline | `ambiguous` | `interface definition matches several devices` with their BSD names |
| Table unreadable at start | offline | `unknown` | `interface table unreadable` once (D5) |

- Several rows can match one MAC, for example when a VLAN interface
  carries its parent's MAC (inference). The fix is to add `device`. cuttle
  never guesses: an ambiguous definition is never used.
- Kind comes from the public ioctl `SIOCGIFFUNCTIONALTYPE`: functional
  type 2 is `wired`, 3 (Wi-Fi infrastructure) `wifi`, and 5 `cellular`;
  every other value prints as `other` in `cuttle interfaces` and never
  satisfies a guard ([research 0003 §4][r3-4]). The type of a tethered
  iPhone is not known yet (assumption F3), so the user sets `kind` for the
  phones only after the device check shows it.
- Logs, status, validation messages, and `last_reload_error` name
  definitions, key paths, BSD names, and kinds, never a MAC address.

### D4. Online state

An interface is online for an address family when every check holds:

| # | Check | Source | Offline reason |
| --- | --- | --- | --- |
| 1 | Exactly one row matches and `kind` agrees (D3) | Interface table | `absent`, `ambiguous`, `kind_mismatch`, `unknown` |
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
- `primary` lists the families for which the interface holds the
  unscoped default route.
- Go's `FlagRunning` is not a link signal: every present Ethernet-type
  interface had it with or without a link ([research 0003 §3][r3-read]).
- `online` is a local fact. Nothing is sent to learn it, so an uplink that
  fails behind a working link shows only through failover (D8) and
  demotion (D9), and only for traffic that gets past name resolution
  (D13).
- Address flags such as IPv6 duplicate address detection are not read. An
  address still in detection fails the dial with `EADDRNOTAVAIL`, which
  fails over (D8).
- An interface that only reaches a LAN, with no default route, is never
  online. Destinations on such a LAN are private and dialed unbound (D7).

The reasons are, in full: `absent`, `ambiguous`, `kind_mismatch`,
`unknown`, `admin_down`, `link_down`, `no_address`, and `no_route`.

### D5. Reading state and noticing changes

Each re-read builds the whole interface table again from these sources, all
pure Go through `syscall`, without privileges and without sending a packet:

| Fact | Source |
| --- | --- |
| Index, BSD name, MAC, flags | `net.Interfaces` (the `NET_RT_IFLIST` sysctl) |
| Addresses | `net.Interface.Addrs` |
| Media status | `ioctl(SIOCGIFMEDIA)` on an `AF_INET` datagram socket (`syscall.SIOCGIFMEDIA`) |
| Kind | `ioctl(SIOCGIFFUNCTIONALTYPE)`, public `sys/sockio.h`; the constant is defined in cuttle |
| Default routes | `syscall.RouteRIB(syscall.NET_RT_DUMP, 0)` read by a small parser of `rt_msghdr` and its sockaddrs in cuttle (D16): messages with `RTF_UP` and `RTF_GATEWAY` and without `RTF_HOST`, whose destination is `0.0.0.0` or `::` and whose mask is all zero or absent, which reads as /0; the header's index names the interface, and `RTF_IFSCOPE` separates scoped routes from the primary's |

The default-route source was observed in iteration 2 on the research Mac
(macOS 27.2.1, Go 1.27.4) with a read-only program that called only these
three functions: 95 messages parsed, the IPv4 default without
`RTF_IFSCOPE` on `en7`, the scoped IPv4 default on `en0`, and scoped IPv6
defaults on `utun0`–`utun7`. This matches what research 0003 read through
`golang.org/x/net/route` ([research 0003 §3][r3-read]). In TASK-0009,
cuttle's own parser gave the same default routes as
`syscall.ParseRoutingMessage` and `syscall.ParseRoutingSockaddr` on the
implementer's Mac (94 messages). cuttle does not use the standard parser:
it converts each 4-byte aligned message to a struct that needs 8-byte
alignment, which the race detector's pointer checks reject
(`checkptr: misaligned pointer conversion`), so every T1 run under
`-race` would abort (D16).

A re-read happens:

- at start and at every reload, before the new snapshot is used (D14);
- 200 ms after the first notice that follows the last re-read, so every
  notice in that window shares one re-read, and at most five re-reads run
  per second;
- 200 ms after a bound dial fails with `ENXIO`, `EADDRNOTAVAIL`, or
  `ENETDOWN`, which shows a stale table, sharing the same window;
- every 30 s, as a safety net for lost notices or sockets that could not
  be opened. These re-reads are system calls, not traffic.

Notices come from two sockets that open without privileges
([research 0003 §3][r3-notice]). Each message read passes through a pure
classifier over its raw bytes, so fixtures test the filtering without a
socket (I17):

- **Routing socket**, `socket(AF_ROUTE, SOCK_RAW, 0)`. `iface.RouteNotice`
  reads the message type and flags from the fixed `rt_msghdr` header.
  `RTM_NEWADDR`, `RTM_DELADDR`, and `RTM_IFINFO` are notices. `RTM_ADD`,
  `RTM_DELETE`, and `RTM_CHANGE` are notices only when neither
  `RTF_LLINFO` nor `RTF_HOST` is set, because macOS keeps ARP and neighbor
  entries as host routes and a busy LAN would otherwise cause constant
  re-reads (inference). Other types are ignored. A message too short for
  its header counts as a notice, so a malformed read never hides a change.
- **Kernel event socket**, `socket(AF_SYSTEM, SOCK_RAW, SYSPROTO_EVENT)`,
  filtered with `SIOCSKEVFILT` to vendor Apple and the network class.
  `iface.KernelNotice` reads the vendor, class, and subclass from the fixed
  `kern_event_msg` header and accepts the data-link (`KEV_DL_SUBCLASS`),
  IPv4 (`KEV_INET_SUBCLASS`), and IPv6 (`KEV_INET6_SUBCLASS`) subclasses.
  The event body is never parsed, and a message shorter than the
  `kern_event_msg` header counts as a notice, as on the routing socket.
  These events report attach, detach, and link on and off, which the
  routing socket does not.
- A socket read error, such as `ENOBUFS` after a burst, is a notice like
  any other, so it causes a re-read in the same 200 ms window. A socket
  that cannot be opened is logged once at `warn`, and the 30 s re-read
  remains.

The monitor runs only while the configuration defines at least one
interface. It calls `Source.Watch` with a context of its own when the
first definition appears, at start or on reload, and cancels that context
when a reload removes the last definition or the proxy shuts down.
`Watch` opens its sockets for each call and closes them when the context
ends (Interfaces).

**`self`.** The `self` class of [0003 D11][d3-11] has one source,
`Deps.SelfAddrs`. In production that is `netx.InterfaceAddrs`, which keeps
its 10 s cache and gains an invalidate function. `cmd/cuttle` passes that
function as `Deps.SelfChanged`, and the monitor calls it after each
successful re-read, so a newly attached phone's address joins `self` at
the next use. The interface table is never a second source of `self`.
Tests inject `SelfAddrs` as before, and table rows carry addresses outside
it.

**Unreadable table.** If the table cannot be read at start, every
definition is offline with reason `unknown`, so a list fails closed with
D10 instead of falling back to system routing, and one `warn` line names
the error. A later failed re-read keeps the previous table and is logged
once per run of failures.

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
- Lists apply to local and remote clients alike (D7).
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

- A definition with `device` and neither `mac` nor `kind`.
- A rule with `interfaces` and `allow_loopback` or `allow_private`, or
  whose `cidrs` are all loopback or private: lists do not apply to those
  addresses (D7).
- An upstream without its own `interfaces` while `proxy.interfaces` is
  set: its dials follow system routing (D11), which would otherwise pass
  unnoticed.
- An upstream with `interfaces` whose address is a loopback or private
  literal: its dials stay unbound (D11).
- `proxy.interface_attempt_timeout` at or above `proxy.connect_timeout`:
  a timeout then never fails over. The warning appears only when
  `proxy.interfaces`, a rule, or an upstream has a list, because the key
  changes nothing otherwise.

No message about an `[[interface]]` key or an `interfaces` list repeats
the value it is about. Messages name the key path, the line, and the rule
in words, for example `interface[3].mac (line 22): duplicate of
interface[1].mac` or `interface[2].device (line 15): not a BSD name such
as en0`, so a MAC typed into `device` is not echoed either.

TOML syntax errors need the same care. For an unquoted MAC,
`github.com/BurntSushi/toml` v1.6.0 reports `invalid datetime` and quotes
the start of the value, with `LastKey` `interface.mac`, and the same for
`device` (observed in iteration 3), while M1.4 copies the parser's message
into the error (`internal/config/validate.go`, `Parse`). When the
`LastKey` of a syntax error is `interface`, starts with `interface.`, or
ends with `interfaces`, the message is only its position and the fixed
text `TOML syntax: invalid value; put it in double quotes`. So neither
`cuttle check`, the log, `last_reload_error`, nor the settings page
carries a MAC written where a MAC belongs.

That is the whole rule. Every other message shows what the operator
wrote, as written: key names, rule names, instance names, host patterns,
addresses, and the parser's own quoted text, including anything that
merely looks like a MAC, such as a 12-hex-digit label or an IPv6 address
that starts with six two-digit groups. The operator who writes the
configuration is trusted ([AGENTS.md](../../AGENTS.md)), and these messages
reach only the operator's own terminal, log, `cuttle status`, and the
settings page of [0005 D10][d5-10], where hiding a name or an address
would make an error unreadable. A MAC typed under another key, such as a
misspelled `[[interfce]]` table or a rule's `hosts`, is therefore echoed
like any other value. An earlier blanket redaction of MAC-shaped text in
every message hid such names and addresses and was removed
([OI-0006][oi-6] O-R1).

Validation does not require upstream addresses to be distinct: only
upstream names must be unique ([0004 D4][d4-4]; the validator compares
names only). The per-upstream pattern of D11 relies on this.

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

Changes from M1.4 when a list is in force, both deliberate:

- **Order.** A name with both non-public and public addresses, such as a
  name behind an `allow_private` rule, has its non-public addresses tried
  first in group 0 and its public ones after them, instead of in the
  resolver's order.
- **Timing.** Group 0 is a group like the others, so a silent private
  address holds the public addresses back for the attempt timeout (D8),
  not for the 300 ms stagger.

Remote clients with `proxy.remote.allow_direct` reach only `public`
addresses ([0003 D10][d3-10]). Their plans therefore never have group 0,
and every connection they get through a list is bound.

**VPN routes.** A bound socket uses only routes through its interface
([research 0003 §7][r3-vpn]). With `proxy.interfaces` or a rule list in
force, `direct` dials to public addresses therefore bypass a full-tunnel
VPN, and also the routes that a split-tunnel VPN installs for public
prefixes, and leave through the listed interface. Private addresses stay
on system routing in group 0. The user never sends public destinations
through a VPN and decided in iteration 3 that this is documented only,
with no rule option for system routing. Leaving `proxy.interfaces` unset
keeps system routing for every `direct` route without a list.

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

Group 0 is the exception: when it has not connected, the next group
starts whatever its errors were, just as M1.4's stagger moved on to the
next address after any failure. A refused or denied private address of a
name behind `allow_private` therefore never stops that name's public
addresses from being tried, and such a name connects as it did in M1.4.

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
deadline is a failover condition, as the user confirmed in iteration 2,
because a phone without cellular data, or a router whose uplink is down,
may stay silent (assumption F4). 3 s lets a healthy path connect after one
lost SYN, since the initial retransmission timeout is 1 s
([RFC 6298 §2][rfc6298]), and bounds the wait on a silent path.

When the plan is exhausted, the result of the last group that ran maps by
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
| Silent dead uplink on a non-primary interface, or a phone without data | Names resolve through the primary; the bound dial reaches the deadline, fails over, and demotes the interface (D9) | Once per demotion period |
| Silent dead uplink behind the primary interface's live link | Names fail at DNS first: `502 dns_error` after up to `connect_timeout` (D13, assumption F8). Only IP literals and names that mDNSResponder answers from its cache reach the dial, where the deadline, failover, and demotion apply | Up to `connect_timeout` per lookup |
| Destination refuses | `502`, no failover | — |
| Destination silent on every interface | `504` when the budget ends | `connect_timeout` |

### D9. Demotion

Without memory, a silent interface at the head of a list would delay every
new connection by the attempt timeout. Demotion remembers it briefly.

- **Trigger.** In one dial, the group of interface X failed over, and a
  later group, through another interface, connected. The success elsewhere
  shows that the destination was reachable, so cuttle blames X's path. A
  dial in which every group failed demotes nothing. Dials for remote
  clients with `allow_direct` count too, so a remote client, such as an
  edge instance using a gateway's `direct` route, can demote an interface
  for the gateway machine's local clients as well.
- **Effect.** For `proxy.interface_demotion` (default `30s`; `0s`
  disables), X comes after the non-demoted interfaces in every list that
  holds it, keeping its place among demoted ones. X stays usable: a list
  with X alone, or whose other interfaces fail, still dials X.
- **Cost effect.** Demotion is global. One destination that is
  unreachable through X but reachable through Y, for example a server
  that drops traffic from X's provider, demotes X for every destination.
  If X is the cheapest interface, all new connections in its lists use
  costlier interfaces for up to 30 s, and plain-HTTP connections pooled
  on them stay in use until they idle out (D12). The user accepted this in
  iteration 2 in exchange for not waiting on a broken interface again for
  each destination.
- **End.** The period expires; a re-read changes X's row, such as its
  index, families, or reason; or a connection through X succeeds. Expiry
  is observed at the next plan or status read that looks at X, so the
  `interface restored` line and the gauge change come at that moment.
- **Logs and gauge.** One `warn` line when X is demoted,
  `msg="interface demoted" interface=eth-a device=en7 reason=timeout
  for=30s`, where `reason` is `timeout` or `unreachable`, as in `skipped`
  (D15). One `info` line when the demotion ends,
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

- The body is `cuttle: no usable interface for <target>`. For local
  clients the details list each interface of the list in force with its
  reason, up to 200 bytes; interface names are configured names, like rule
  and upstream names. For remote clients `details` is omitted, so an edge
  instance learns nothing of the gateway machine's interfaces; the
  decision line still has `skipped`, and its `detail` field is empty as
  well.
- No dial is made, and the upstream hold of [0003 D8][d3-8] neither starts
  nor extends.
- `503` says the condition is temporary and at the proxy, and it matches
  the held-upstream answer that operators already know. RFC 9209 defines
  `destination_unavailable` for a next hop the intermediary considers
  unavailable and recommends `503` ([RFC 9209 §2.3][rfc9209]).

### D11. Upstreams

- **No list (default).** The dial is unbound and follows system routing,
  even when `proxy.interfaces` is set; `cuttle check` warns about it (D6).
  A bound socket ignores routes through a VPN's `utun` interface, so
  binding by default would break a private link that ends on this Mac, the
  third case of [0003 D13][d3-13], and would let traffic bypass a
  full-tunnel VPN ([research 0003 §7][r3-vpn]).
- **With a list.** The upstream's resolved addresses form a plan as in D7:
  every address that is not `public` is dialed unbound in group 0, which
  holds the `loopback`, `self`, and `private` addresses and also unusable
  ones, which [0003 D11][d3-11] refuses only on direct routes. `public`
  addresses are bound per interface, with failover and demotion as in D8
  and D9. An upstream on a private-link address such as
  `10.8.0.6` therefore always follows system routing, and `cuttle check`
  warns when its address is such a literal (D6). Only the grouping uses the
  address classes; the [0003 D11][d3-11] refusals still do not apply to
  upstreams.
- The hold is checked first: a held upstream answers `503` before any
  planning. An exhausted plan is a failure to reach the upstream and starts
  the hold as today. No usable interface answers D10 and leaves the hold
  unchanged.
- **Per-destination interfaces on upstream routes.** A rule chooses an
  upstream, and an upstream has one list. When some destinations must reach
  the same proxy through other interfaces, the configuration declares two
  `[[upstream]]` entries with the same `address` and `disclosure`,
  different names, and different `interfaces`, and the rules name the
  entry they need (example below). Each entry has its own transport, pool,
  and hold, so an outage seen through one list does not hold the other.
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
- cuttle closes the idle connections of every direct and upstream
  transport whose list holds interface X when X becomes `online`, and when
  X's demotion ends with cause `state_changed` or `connected`. The next
  plain-HTTP request plans again and returns to the preferred interface
  instead of staying on a costlier one. Expiry of a demotion closes
  nothing: it is no evidence that X works again, and connections pooled
  meanwhile age out after the transport's 90 s idle timeout.
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
- **Dead uplink behind the primary.** When the primary interface keeps
  its link and address but its uplink is dead, macOS keeps it primary, and
  lookups time out or fail (inference, assumption F8). A request for a
  name then gets `502 dns_error` after up to `connect_timeout`, before any
  interface is chosen, so failover and demotion never see it. Only IP
  literals and names that mDNSResponder answers from its cache reach the
  dial plan. The user unplugs or disables the primary, or changes the
  service order, to recover. This is a documented limitation, accepted by
  the user in iteration 2 and tracked in [OI-0008][oi-8].
- **Failures in general.** A lookup fails before any interface is chosen,
  so it never fails over; it answers `502 dns_error` as today.
- **Cost.** Lookups are small but use the primary network.

Decision: a documented limitation in M1.5, with no code change.
Per-interface DNS, by Go's resolver over a bound UDP socket with each
interface's servers or by `DNSServiceGetAddrInfo` with an interface index
through cgo, is on the roadmap's Later list; the first depends on
research 0003 open question 8.

### D14. Reload

- Every key of D2 and D6 applies on reload ([0004 D6][d4-6]). Reload
  re-reads the interface table before it swaps the snapshot.
- A definition whose `mac`, `device`, and `kind` are unchanged keeps its
  state, `since` time, and demotion. A changed or new definition starts
  from the fresh table without demotion.
- An upstream whose `interfaces` changed counts as a changed upstream and
  gets a new transport, pool, and hold state, as a changed address does.
  So does an upstream whose list names a definition whose `mac`, `device`,
  or `kind` changed, so that no pooled connection bound to the old device
  is reused. Direct transports are new in every snapshot anyway
  ([0004 D6][d4-6]).
- Adding the first definition starts the monitor with a `Watch` call;
  removing the last ends that call's context (D5). A reload that adds a
  definition while `Deps.Interfaces` is nil is refused (Interfaces).
- In-flight requests keep their snapshot, and established tunnels keep
  their interface, even when the new lists no longer allow it.

### D15. Status, command line, logging, counters, and statistics

**`cuttle status`.** `/v1/status` ([0006 D4][d6-4]) gains an `interfaces`
array in configuration order, and the text form shows the same fields:

```json
"interfaces": [
  {"name": "eth-a", "device": "en7", "state": "online", "reason": "",
   "families": ["ipv4"], "primary": ["ipv4"],
   "since": "2026-11-25T00:00:00Z", "demoted_until": ""},
  {"name": "eth-b", "device": "en8", "state": "demoted", "reason": "",
   "families": ["ipv4"], "primary": [],
   "since": "2026-11-25T00:58:00Z",
   "demoted_until": "2026-11-25T00:58:30Z"},
  {"name": "phone-b", "device": "", "state": "offline",
   "reason": "absent", "families": [], "primary": [],
   "since": "2026-11-25T00:00:00Z", "demoted_until": ""}]
```

- `state` is `online`, `demoted`, or `offline`; `reason` is one of the D4
  reasons when `offline` and empty otherwise; `families` and `primary` are
  lists of `ipv4` and `ipv6` (D4).
- Each entry of `tunnels` gains `interface`, with the decision-log value
  below.
- No MAC address from an `[[interface]]` definition appears in status
  output, logs, counters, or statistics; D6 gives the rule for validation
  messages.

**`cuttle interfaces [--json] [--all] [--watch] [--config PATH]`.** A new
command that reads the system directly and needs no running service. It
helps the user write definitions and answers the device check (I20). It
never runs another program.

```text
$ cuttle interfaces
device  kind   mac                media     ipv4         default4  match
en0     wifi   00:00:5e:00:53:01  active    192.0.2.21   scoped    wifi
en7     wired  00:00:5e:00:53:0a  active    192.0.2.20   primary   eth-a
en3     wired  00:00:5e:00:53:03  inactive  -            none      -
```

The `index`, `ipv6`, and `default6` columns are left out of this example
for width.

- Text columns: `index`, `device`, `kind` (D3), `mac`, `media` (`active`,
  `inactive`, or `none`), `ipv4` and `ipv6` (the first usable address of
  each family, or `-`), `default4` and `default6` (`primary`, `scoped`, or
  `none`), and `match` (the definition's name, followed by its reason when
  it is not online, or `-`).
- `--json` prints an array of objects with the fields `index` (number),
  `device`, `kind`, `mac` (lowercase, colon-separated), `media`,
  `addresses` (an object with `ipv4` and `ipv6` lists of every usable
  address), `default` (an object with `ipv4` and `ipv6`, each `primary`,
  `scoped`, or `none`), `match` (the definition's name or `""`), `state`,
  and `reason` (D4 values for a matched row, `""` otherwise).
- The default lists interfaces with a six-byte MAC; `--all` lists every
  interface, `lo0` and `utun` included. Without a readable configuration
  `match`, `state`, and `reason` are empty.
- `--watch` runs the D5 monitor and prints one line per change that a
  re-read finds, with a timestamp, the device, and the changed fields, such
  as `2026-11-25T01:02:03.456Z en7 media active->inactive`. It does not
  decode notices. Ctrl-C ends it.
- Outside the configuration file and its editor, MACs and the addresses
  of interface table rows appear only in this command's terminal output,
  because the user needs them to write and check the configuration.
  Configured listener addresses still appear in `cuttle status` and the
  listener log lines, as in M1.4.
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

- `interface` is always present. It names the interface that carried the
  outbound connection the decision made or used:
  - for a bound dial, the configured name;
  - for an unbound dial or a pooled connection, the definition whose
    interface holds the connection's local address in the current table,
    or `other` when none or several do;
  - empty when the configuration defines no interface, when the decision
    made and used no outbound connection (`block`, refused targets, D10,
    every failed dial), and when the destination address is `loopback` or
    `self`, whose traffic never leaves the Mac.
- `skipped` lists, in list order as `name:reason` pairs separated by
  commas, the interfaces of the list in force that the dial did not use
  and that come earlier in list order than the interface that carried the
  connection, or that ended a failed dial. An interface that was tried is
  listed wherever it comes in list order, because demotion can place it
  after the one that carried the connection. Offline, passed-over, and
  demoted interfaces later in the list are not listed, so unplugged
  devices at the end of the list leave no trace on every decision line. A
  failed dial that ran out of interfaces lists every interface of the
  list. Its reasons are:
  - the D4 reasons and `no_family`, for an interface the plan passed over;
  - `demoted`, for a demoted interface placed after the one that carried
    the connection and never tried;
  - `unreachable` (a failover error) or `timeout`, for an interface tried
    without a connection; for a demoted interface that was tried and
    failed, the failure reason replaces `demoted`; `timeout` also for an
    interface whose group a failed dial never reached because the connect
    budget ran out;
  - `refused` (`ECONNREFUSED`, `ECONNRESET`) or `error` (any other error),
    for the interface whose error ended a failed dial.

  It is absent when it would be empty, when no list was in force, when the
  plan held no public address so that only group 0 ran, and when group 0
  carried the connection. A failure in group 0 adds nothing to it, because
  group 0 uses no listed interface and passes to the next group (D8).
  After D10 it lists every interface of the list.

```text
time=2026-11-25T01:01:02.345Z level=INFO msg=decision id=4f2a
client=127.0.0.1:51234 source=local kind=connect
target=updates.example:443 rule=system-updates action=direct upstream=
interface=eth-b outcome=ok status=200 up=5321 down=88410 dur=12.402s
```

A plain-HTTP request learns the interface of a pooled connection from the
connection itself, which the dialer wraps with its binding; M1.4's
`GotConn` trace already assigns each pooled connection to its exchange
([0002 D7][d2-7]).

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
| `interface_bytes_total` | `interface`, `direction` = `up`, `down`; the D7 bytes of each decision whose `interface` is not empty |

The `interface` label takes configured names and `other`, so it stays
bounded. With no definitions, neither counter exists. Per-interface dial
results are not counted: `skipped` and the demotion lines carry them.
`outcomes_total` gains the outcome `no_interface`.

**Statistics (M2).** `host_hour` ([0005 D2][d5-2]) gains a key column
`interface` with the decision-log value, empty for decisions without an
interface. The dashboard shows totals per interface and filters the domain
table by interface. Because M2 has not started, schema version 1 includes
the column.

### D16. Dependencies and platform code

- No new module and no cgo. Binding, state, and notices use `syscall`.
  `CGO_ENABLED=0 go build ./cmd/cuttle` must keep working, and no package
  that `cmd/cuttle` links may have cgo files, which that build would drop
  without a word (I19).
- `golang.org/x/net/route` is not used. cuttle reads the dump of
  `syscall.RouteRIB` with a small parser of `rt_msghdr` and its
  sockaddrs, the fallback this record planned: `syscall.ParseRoutingMessage`
  fails the race detector's pointer checks in every T1 run (D5). The two
  parsers gave the same default routes in TASK-0009. `syscall.RouteRIB` is
  marked deprecated in favor of `x/net/route` but stays under the Go 1
  compatibility promise, and `go vet` in the T0 gate does not flag it.
  [0002 D1][d2-1] keeps `golang.org/x/net` test-only.
- Constants and structures missing from `syscall` are defined in cuttle,
  each with the public header it comes from (checked in iteration 3
  against the macOS SDK's headers):
  - `IFM_AVALID` and `IFM_ACTIVE` (`net/if_media.h`), and
    `struct ifmediareq` (`net/if.h`), which the header packs to 4 bytes,
    44 bytes in all, the size that `syscall.SIOCGIFMEDIA` encodes;
  - `SIOCGIFFUNCTIONALTYPE` (`sys/sockio.h`), and `struct ifreq` with its
    `ifru_functional_type` member and the `IFRTYPE_FUNCTIONAL_*` values
    (`net/if.h`);
  - `SYSPROTO_EVENT` (`sys/sys_domain.h`);
  - `SIOCSKEVFILT`, `KEV_VENDOR_APPLE`, `KEV_NETWORK_CLASS`,
    `struct kev_request`, and `struct kern_event_msg`
    (`sys/kern_event.h`);
  - `KEV_INET_SUBCLASS`, `KEV_DL_SUBCLASS`, and `KEV_INET6_SUBCLASS`
    (`net/net_kev.h`).

  `IP_BOUND_IF`, `IPV6_BOUND_IF`, `SIOCGIFMEDIA`, `AF_ROUTE`,
  `AF_SYSTEM`, `RTF_IFSCOPE`, `RTF_LLINFO`, `RTF_HOST`, `NET_RT_DUMP`, and
  the `rt_msghdr` layout (`syscall.RtMsghdr`) are in `syscall`. The
  portable notice classifiers and the route parser keep their own copies
  of the routing constants and the `rt_msghdr` offsets, and a darwin test
  checks them, and the sizes that the ioctl requests encode, against
  `syscall`.
- The private ioctls `SIOCGIFEFLAGS`, `SIOCGIFXFLAGS`, and `SIOCGIFTYPE`
  are not used. The `expensive` and `constrained` flags they expose are not
  needed when the user orders interfaces explicitly.
- A new package `internal/iface` holds definitions, matching, the online
  predicate, plans, demotion, and the notice classifiers as portable logic,
  and the darwin reader and monitor in `_darwin.go` files. `internal/netx`
  gains the binding and the bound dial.

## Example configuration

Placeholders only. The MACs are documentation values, the addresses are
documentation addresses, and the domains are reserved.

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

# One proxy, two interface lists (D11).
[[upstream]]
name = "proxy-wired"
address = "203.0.113.20:3128"
disclosure = "none"
interfaces = ["eth-a", "eth-b"]

[[upstream]]
name = "proxy-any"
address = "203.0.113.20:3128"
disclosure = "none"
interfaces = ["eth-a", "eth-b", "wifi", "phone-a", "phone-b"]

[[rule]]
name = "system-updates"     # pinned to B even while A is online
domains = ["updates.example"]
action = "direct"
interfaces = ["eth-b"]

[[rule]]
name = "bulk-mirror"        # through the proxy, never over a phone
domains = ["mirror.example"]
action = "upstream:proxy-wired"

[[rule]]
name = "partner"
domains = ["partner.example"]
action = "upstream:proxy-any"
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

// InterfaceAddrs lists this host's addresses, refreshed at most once per
// interval; invalidate makes the next call read them again.
func InterfaceAddrs(interval time.Duration) (addrs func() []netip.Addr,
    invalidate func())

// internal/iface

// Link is one row of the system interface table.
type Link struct {
    Index    int
    Device   string
    MAC      net.HardwareAddr
    Up       bool  // IFF_UP
    Loopback bool  // IFF_LOOPBACK; loopback rows never match (D3)
    Kind     Kind  // from SIOCGIFFUNCTIONALTYPE
    Media    Media // MediaActive, MediaInactive, MediaUnknown
    Addrs    []netip.Addr
    Default4 Route // RouteNone, RouteScoped, RoutePrimary
    Default6 Route
}

// Source supplies the table and its change notices. On darwin, Read uses
// system calls; tests supply rows and notices.
type Source interface {
    Read() ([]Link, error)
    // Watch opens the notice sockets for this call and sends one value per
    // accepted message or socket read error until ctx ends; it then closes
    // the sockets and the channel. A socket that cannot be opened is
    // logged and sends nothing.
    Watch(ctx context.Context) <-chan struct{}
}

// RouteNotice and KernelNotice classify one raw message read from the
// routing socket or the kernel event socket (D5).
func RouteNotice(msg []byte) bool
func KernelNotice(msg []byte) bool
```

- `policy.Decision` gains `Interfaces []string`, the list in force for a
  `direct` route: the rule's list, else `proxy.interfaces`. `Decide` stays
  pure.
- `proxy.Deps` gains `Interfaces iface.Source`, `SelfChanged func()`, and
  a timer factory for the debounce and the safety re-read, which tests
  inject. `cmd/cuttle` always passes the darwin source and sets
  `SelfChanged` to the invalidate function of `netx.InterfaceAddrs` (D5).
- When a configuration defines an interface and `Deps.Interfaces` is nil,
  `proxy.New` fails, and a reload is refused like an invalid file: the
  running configuration stays, and the cause is logged and shown as
  `last_reload_error`. A test cannot reach the real table by accident, and
  no case is left undefined.
- The proxy calls `Watch` only while definitions exist (D5).
- `testenv.Net` records the binding with each dial and accepts behaviors
  per interface and address: route, refuse, a chosen errno, or blackhole.
  An address without a per-interface behavior acts as for an unbound dial.
- `testenv.Interfaces` is a fake source whose rows a test sets, whose
  `Watch` channel it feeds, whose `Read` it can make fail, and which
  records each `Watch` call and when its context ends.

## Failure behavior

- An offline interface is skipped and never bound. When no group remains,
  the request gets D10 without a dial.
- A failover error or a group deadline moves to the next group. Any other
  error ends the dial with the [0002 D6][d2-6] mapping, except in group 0,
  which passes to the next group on any error, as M1.4 did (D8).
- An interface that disappears under an established tunnel ends that
  tunnel; the client's next connection gets a new plan.
- A notice socket that cannot be opened is logged once; the 30 s re-read
  still runs.
- A table that cannot be read at start leaves every definition offline
  (`unknown`), so lists fail closed. A later failed re-read keeps the last
  table.
- A dead uplink behind the primary interface fails names at DNS before
  any interface is chosen (D13).
- Interface selection never sends a request of cuttle's own, as
  [0003 D5][d3-5] requires.

## Acceptance criteria

T1 tests with the mocks of [0007](0007-test-environment-and-gates.md), an
injected interface source, and injected timers, unless marked T0 or T2.
Test interfaces are named `eth-a`, `eth-b`, and `wifi`, with documentation
MACs, and their rows carry addresses in `198.18.0.0/15`, outside the
injected `SelfAddrs`.

| ID | Criterion | Check |
| --- | --- | --- |
| I1 | Each D6 validation error has a sample file that fails with the documented key path and line, and each D6 warning, including an upstream without `interfaces` while `proxy.interfaces` is set, appears in `cuttle check` output with exit status 0. Samples include an unquoted MAC in `mac`, an unquoted MAC in `device`, and a quoted MAC in `device`; the first two fail with their position and the fixed TOML hint. No message contains any part of the sample files' MACs in any case or separator form. Two upstreams with one address and different names and lists pass validation. | T0 `TestConfigInterfaces` |
| I2 | Matching and the online predicate give the D3 and D4 state, reason, families, and primary families for injected rows: a MAC match, a `device` match, both keys, `device` present with another MAC (`absent`), no row (`absent`), two rows with one MAC (`ambiguous`), a `kind` mismatch, `admin_down`, `link_down`, media not reported with address and route (online), only `169.254.1.2` (`no_address`), an address without a default route (`no_route`), IPv4 only, IPv6 only, both, and primary for IPv4 only. | T0 `TestInterfaceState` |
| I3 | Plans follow D7 and D11: list order, a rule list replacing the default list, group 0 first with `loopback`, `self`, and `private` addresses and the public addresses after it, family filtering with `no_family`, offline interfaces skipped with their reasons, demoted interfaces last in their relative order, no group giving D10, and an upstream's private address in group 0. | T0 `TestDialPlan` |
| I4 | Each failover error of D8 moves to the next group; `ECONNREFUSED`, `ECONNRESET`, `EACCES`, `EPERM`, and an unknown error end the dial; a group with one refused and one unreachable attempt ends the dial; group 0 passes to the next group on any error; deadlines give every group but the last `interface_attempt_timeout` and the last the rest of the budget. | T0 `TestFailover` |
| I5 | A real socket gets the option. A `tcp4` dial bound to `lo0`'s index connects to a listener on `127.0.0.1`, and one to `192.0.2.1:9` fails with `ENETUNREACH` within 100 ms. A `tcp6` dial bound with `IPV6_BOUND_IF` connects to `[::1]`, and one to `[2001:db8::1]:9` fails at once with `ENETUNREACH` or `EHOSTUNREACH`. An index that names no interface fails with `ENXIO`, classified as a failover error. Where `getsockopt` supports the option, it returns the index. No packet leaves the host, and this settles the no-route half of research open question 1 for TCP. | T1 `TestBoundDial` |
| I6 | With `proxy.interfaces = ["eth-a", "eth-b", "wifi"]`, `interface_attempt_timeout = "300ms"`, and `connect_timeout = "2s"`, a direct `CONNECT`: (a) with all reachable dials once, bound to `eth-a`, and logs `interface=eth-a` without `skipped`, also when `wifi` is `link_down`; (b) with `eth-a` `link_down` makes no `eth-a` dial and logs `skipped=eth-a:link_down`; (c) with `eth-a` failing `ENETUNREACH` starts the `eth-b` dial within 50 ms; (d) with `eth-a` blackholed starts `eth-b` 300 ± 100 ms after `eth-a` and logs `skipped=eth-a:timeout`; (e) with `eth-a` refusing gets `502 connection_refused`, no `eth-b` or `wifi` dial, an empty `interface`, and `skipped=eth-a:refused`; (f) with `eth-a` failing `EPERM` gets `502`, no other dial, and `skipped=eth-a:error`; (g) with all three blackholed gets `504 connection_timeout` after 2 s ± 200 ms, the `wifi` group having the rest of the budget. Cases (a), (b), and (d) also pass for plain HTTP. | `TestInterfaceFailover` |
| I7 | A rule with `interfaces = ["eth-b"]` dials only `eth-b` while `eth-a` is online. With `eth-b` absent a local client gets `503` with `Proxy-Status: <name>; error=destination_unavailable; details="no usable interface (eth-b: absent)"`, outcome `no_interface`, and no dial, and a remote client with `allow_direct` gets the same without `details`. A rule with `["eth-b", "eth-a"]` uses `eth-a` when `eth-b` is `link_down`. With `proxy.interfaces` unset, a request whose rule has no list dials unbound. | `TestInterfacePinning` |
| I8 | After I6 (d), with `interface_demotion = "1s"`, the next request to another destination connects through `eth-b` without an `eth-a` dial and logs `skipped=eth-a:demoted`, `interface_state{interface="eth-a",state="demoted"}` is 1, and one `interface demoted` line with `reason=timeout` is logged. After the injected clock passes 1 s, the next request plans `eth-a` first, and one `interface restored` line with `cause=expired` is logged at that request. A notice that changes `eth-a`'s row ends a demotion at once with `cause=state_changed`. A dial in which every group timed out demotes nothing. | `TestInterfaceDemotion` |
| I9 | With every listed interface offline, the starter `loopback` rule's targets, the literals `10.20.0.5` and `192.0.2.50` (`self` from the injected `SelfAddrs`), and a name resolving to `10.20.0.5` through an `allow_private` rule are dialed unbound, while `198.18.0.10`, a row's own address, is classified `public` and gets D10. With `eth-a` online for IPv4 only and `eth-b` for both, a name with AAAA and A records gets only the IPv4 attempt on `eth-a`, an IPv6-only name skips `eth-a` with `no_family` and connects on `eth-b`, and with no interface online for IPv6 it gets D10. A name behind an `allow_private` rule that resolves to `10.20.0.5`, which refuses, and to a public address connects through `eth-a` on the public address, as in M1.4, and so it does when the private attempt fails with `EPERM`. | `TestInterfaceAddressClasses` |
| I10 | Within one snapshot and its transports, a plain-HTTP connection pooled under the default list on `eth-a` is not reused for a request to the same host and port decided under a list pinned to `eth-b`, nor under the default list's interfaces in another order; one snapshot decides a host and port one way, so the test gives the snapshot a policy with the other list while keeping its transports. With `eth-a` offline a request pools a connection on `eth-b`; after a notice brings `eth-a` online, that idle connection is closed and the next request dials `eth-a`. A demotion of `eth-a` that ends with `cause=connected` closes `eth-b`'s idle connection, and one that ends with `cause=expired` leaves it open. | `TestInterfacePools` |
| I11 | An upstream without `interfaces` is dialed unbound while `proxy.interfaces` is set. An upstream with `["eth-a", "wifi"]` at a public test address dials `eth-a` and fails over to `wifi` on `ENETUNREACH`; at a private test address it is dialed unbound. With neither interface online the public one gets `503` with `error=destination_unavailable` and `next-hop` naming the upstream, outcome `no_interface`, no dial, and `upstream_state` stays `up`. When both groups time out, the hold starts as in [0003 U11][d3-ac]. A notice that brings `eth-a` online closes the idle connections of that upstream's transport. Two upstreams with one address and different lists keep separate holds. | `TestUpstreamInterfaces` |
| I12 | Row changes followed by a notice give one `interface offline` line with the reason, one `interface online` line with `families` on recovery, and the matching `interface_state` gauge. An ambiguous definition, a `kind` mismatch, and a `device` with another MAC each log their D3 line once. `cuttle status` and `/v1/status` list each interface with the D15 fields, and each tunnel with its `interface`. Neither the status output, the log, the error lines of a reload that fails on a duplicate `mac` or on an unquoted MAC, which is a TOML syntax error, nor `last_reload_error` contains any test MAC in any case or separator form. | `TestInterfaceStateEvents`, `TestStatusInterfaces` |
| I13 | A tunnel through `eth-b` keeps transferring across a reload that removes `eth-b` from every list and changes its definition; a request after the reload follows the new lists; a demotion of an unchanged definition survives a reload; an upstream whose `interfaces` changed gets a new transport and starts `up` while an unchanged one keeps its pooled connection and hold; after a reload that changes only the `mac` of a definition that an upstream's list names, that upstream's idle connection is not reused and its next request dials; adding the first definition makes one `Watch` call and removing the last ends its context; a reload that adds a definition to a proxy built without a source is refused with the cause in `last_reload_error`. | `TestReloadInterfaces` |
| I14 | A tunnel through `eth-b` of 1,048,576 bytes down and 65,536 bytes up adds exactly those bytes to `interface_bytes_total{interface="eth-b"}`. The `interface` field is empty for a blocked request, for a `loopback` destination, and in a configuration without definitions, where no `interface_*` counter exists; `skipped` is absent when group 0 carried the connection. Attribution of unbound connections by local address gives the owning definition, or `other` for none or several owners (T0). | `TestInterfaceCounters`; T0 `TestInterfaceAttribution` |
| I15 | `cuttle interfaces --json` exits 0 on the test host with objects holding every D15 field; `--all` also lists `lo0`. The test asserts field names and types only and never prints the command's output, so the host's MACs and addresses never reach gate output. A unit test formats an injected table as text and JSON, including `match` with a definition and its reason. `cuttle explain` prints the list in force and its source for a direct rule, the default list, an upstream with a list, and `system routing`. | T1 `TestInterfacesCommand`, `TestExplainInterfaces`; T0 `TestInterfacesFormat` |
| I16 | On darwin, the source's `Watch` opens the routing socket and the kernel event socket without privileges and closes both when its context ends. With the fake source and an injected timer, a configuration without definitions makes no `Watch` call; 20 notices within 100 ms cause exactly one re-read, when the timer reaches 200 ms after the first; a notice sent for a socket read error causes a re-read in the same way; advancing the timer by 30 s causes one safety re-read; and each successful re-read calls `SelfChanged` once. | `TestMonitor` |
| I17 | Fixture messages classify as D5 says: `RTM_NEWADDR`, `RTM_DELADDR`, `RTM_IFINFO`, and a default-route `RTM_ADD` are notices; an `RTM_ADD` or `RTM_DELETE` with `RTF_LLINFO` or `RTF_HOST`, and an `RTM_GET`, are not; a message shorter than its header is a notice. Kernel events of the data-link, IPv4, and IPv6 subclasses are notices, and another subclass or vendor is not. | T0 `TestRouteNotice`, `TestKernelNotice` |
| I18 | A bound dial failing with `ENXIO`, `EADDRNOTAVAIL`, or `ENETDOWN` causes one re-read when the timer reaches 200 ms. A source whose `Read` fails at start leaves every definition `offline` with reason `unknown`, logs one warning, and a direct request to a public address under a list gets `503 no_interface`; a `Read` that fails later keeps the previous table, so the request still connects, and logs one warning per run of failures. | `TestInterfaceTableFailures` |
| I19 | `go list -deps ./cmd/cuttle` lists neither `internal/testenv` nor any package under `golang.org/x/net/` (the standard library's `vendor/golang.org/x/net` packages are allowed), `go list -deps -f '{{if .CgoFiles}}{{.ImportPath}}{{end}}' ./cmd/cuttle` lists no package in the default environment, and `CGO_ENABLED=0 go build ./cmd/cuttle` succeeds. | T0 gate ([0007 D7][d7-7], E2) |
| I20 | The device check below is recorded on the user's Mac with the user's five interfaces. | T2, manual, with the user |

### Device check (I20)

The user types every command in Terminal, which is exempt from local
network privacy ([research 0003 §7][r3-lnp]); the tester prepares the
configuration with the user and records the results the user reports. No
step reaches a LAN address. Results go to the
[compatibility matrix](../COMPATIBILITY.md#interfaces) as yes, no, or
short notes, without MACs, addresses, or network names.

**Setup.** The check uses a scratch directory outside the repository, its
own XDG directories, and port 18128, so it never touches an installed
cuttle or ports 3128 and 3129:

```sh
mkdir -p "$HOME/cuttle-check/run" && chmod 700 "$HOME/cuttle-check/run"
cp /path/to/candidate/cuttle "$HOME/cuttle-check/"
# Run the next two commands in every new Terminal window and after a reboot:
cd "$HOME/cuttle-check"
export XDG_CONFIG_HOME="$PWD/config" XDG_DATA_HOME="$PWD/data" \
  XDG_STATE_HOME="$PWD/state" XDG_RUNTIME_DIR="$PWD/run"
./cuttle init    # writes config/cuttle/config.toml
```

The user then edits `config/cuttle/config.toml`: `proxy.listen =
["127.0.0.1:18128"]`, `admin.listen = ""`, the five definitions from step
1, `proxy.interfaces` in the user's cost order with an Ethernet adapter
first, called `eth-a` below, and two rules for steps 9 and 10:
`example.net` with `interfaces = ["eth-b"]` and `example.org` with
`interfaces = ["phone-b"]`. This file holds the real MACs; it stays in the
scratch directory and is deleted in step 13.

| # | Step | Record | Settles |
| --- | --- | --- | --- |
| 1 | With no adapter or phone attached, run `./cuttle interfaces`. Attach Ethernet A, run it again, and note the new row; repeat for Ethernet B, phone A (accept the trust and accessory prompts), and phone B. Write the definitions from the new rows | Each device's kind, media, and `default4`; whether the two phones get distinct BSD names and MACs; whether their `ipv4` addresses share the first three numbers, compared on screen only | Research open questions 2 and 9; F2, F3 |
| 2 | Replug each phone into another USB port and run `./cuttle interfaces`. Reboot the Mac, log in, unlock both phones, open Terminal, run the two setup commands for a new window, and run `./cuttle interfaces` again; then continue with step 3 | Whether each phone's `match` column still names it after the port change and after the reboot | Open question 2; F2 |
| 3 | Swap the two Ethernet adapters between their ports and run `./cuttle interfaces` | Whether `match` follows the adapters | Open question 3 |
| 4 | Optional: attach a third adapter of the same model while Ethernet B is unplugged | Whether it takes B's BSD name, and that `eth-b` stays `absent` | Open question 3 |
| 5 | Run `./cuttle interfaces --watch` and, one at a time: pull Ethernet A's cable and plug it back; unplug Ethernet B and plug it back; unplug phone A and plug it back; turn Personal Hotspot off and on on phone B; turn Wi-Fi off and on in Control Center. Stop with Ctrl-C | For each action, the seconds until a change line appears, and whether the index changed after replugging | Open question 5; F5, F7 |
| 6 | In window 1 run `./cuttle serve`. In window 2 run `curl -x http://127.0.0.1:18128 -sS -o /dev/null -w '%{http_code}\n' https://example.com/` three times, then `grep 'msg=decision' state/cuttle/cuttle.log \| tail -3` | `200`, and `interface=eth-a` | D7 |
| 7 | Pull Ethernet A's cable, repeat the `curl` and the `grep`, then plug it back | `skipped=eth-a:link_down` and `interface=` the next interface; the `interface offline` and `interface online` lines appear in the log | D4, D5 |
| 8 | With Ethernet A attached and primary: (a) in window 2 run `curl -p -x http://127.0.0.1:18128 telnet://example.com:443`, which keeps a tunnel open; in window 3 run `lsof -nP -a -c cuttle -iTCP:443 -sTCP:ESTABLISHED` and compare the local address with the `ipv4` column of `eth-a` in `./cuttle interfaces`; stop `curl` with Ctrl-C. (b) Note the `Ibytes` of `eth-a`'s device in `netstat -I <device> -b`, run `for i in 1 2 3 4 5 6 7 8 9 10; do curl -x http://127.0.0.1:18128 -sS -o /dev/null -w '%{size_download}\n' https://example.com/; done`, and read `Ibytes` again | Yes or no: the flow uses `eth-a`'s address; yes or no: `Ibytes` grew by at least the sum of the printed sizes | Open question 1; F1 |
| 9 | Repeat step 8 with `example.net`, pinned to `eth-b` while `eth-a` stays primary, comparing with `eth-b`'s `ipv4` and device. `netstat -n -p tcp` shows the same local address | Yes or no for the address and for the byte count: a bound flow leaves through a non-primary Ethernet | Open question 1; F1 |
| 10 | Repeat step 8 with `example.org`, pinned to `phone-b`, comparing with `phone-b`'s `ipv4` and device. Then unplug phone B and run `curl -x http://127.0.0.1:18128 -sS -o /dev/null -w '%{http_code}\n' https://example.org/` | Yes or no for the address and for the byte count: the flow uses the phone; then `503` and `outcome=no_interface` in the log | Open question 1; F1; D6, D10 |
| 11 | With the user's consent, turn off cellular data on phone A, move `phone-a` to the front of `proxy.interfaces`, run `./cuttle reload`, and repeat the `curl` of step 6 twice. Restore cellular data and the list afterwards | The `skipped` reason for `phone-a` (`timeout` or `unreachable`), the `dur=` of the first request, and whether an `interface demoted` line appears | Open question 6; F4 |
| 12 | Optional, only if the user can cut the internet side of the router behind Ethernet A while the Mac's cable stays in, for example by unplugging the router's WAN cable. Before cutting, run `host example.com` and keep one IPv4 address on screen. After cutting, repeat the `curl` of step 6, then run it with `-k` against `https://<that address>/`. Reconnect afterwards | Whether the name request ends with `502` and `error=dns_error`, and after how many seconds; whether the address request logs `skipped=eth-a:timeout` or `unreachable` and connects through the next interface | D13; F8 |
| 13 | Stop `cuttle serve` with Ctrl-C, run `cd && rm -rf "$HOME/cuttle-check"`, and restore any phone setting changed above | — | — |

## Assumptions

| ID | Assumption | Smallest check |
| --- | --- | --- |
| F1 | A bound TCP connect behaves like the observed UDP probe: an interface without an address or route fails locally without a SYN, and a bound flow leaves through its interface even when another interface is primary. | I5 for the no-route half; device check steps 8–10 for the flow, step 9 for a non-primary Ethernet |
| F2 | Each phone's tethering interface keeps its MAC across replugging, port changes, and reboots, and two phones have distinct MACs. macOS keys BSD names by MAC, so a phone whose MAC changes also gets a new BSD name, and neither `mac` nor `device` can follow it. If the check shows that, the identity question returns to the user, with options such as the USB serial number read by parsing `ioreg` output or through IOKit with cgo, a design change in both cases. | Device check steps 1–2 |
| F3 | A tethered iPhone reports media status or none, gets a scoped default route while sharing, and reports a functional type that the user can use as `kind`. | Device check steps 1 and 5 |
| F4 | A phone without cellular data either fails a connect fast or stays silent; D8 handles both. | Device check step 11 |
| F5 | After a cable is pulled or sharing stops, a routing or kernel notice arrives within about 1 s, so few requests see the stale state. | Device check step 5 |
| F6 | Local network privacy refuses only connections to local network addresses, and a denial returns `EPERM`, `EACCES`, or another error outside D8's failover list. If a denial returns a failover error such as `EHOSTUNREACH` instead, group 0 and unbound upstream dials stay protected, because they never fail over, but a `public` address on a local network, as on a LAN numbered with public addresses or a provider's public subnet on the link, is dialed bound and would fail over to the next interface, possibly reaching the destination over a costlier path. | The [OI-0007][oi-7] check records the error |
| F7 | A different device does not reuse a detached interface's index within the 200 ms debounce. | Device check step 5, comparing indexes |
| F8 | With a dead uplink behind the primary interface's live link, macOS keeps that interface primary, and lookups time out or fail rather than moving to another interface's resolver. | Device check step 12 (optional) |

## Changes to other records

Iteration 2 applied these changes directly, on the text that M1.4 landed:

- **0002:** D1 package table (`internal/iface`); D5 pools per interface
  list; D6 the `503 no_interface` row and reflected interface names; D7
  the interface counters, `no_interface`, and the tunnel `interface`
  field; D8 the `interface` and `skipped` fields; Interfaces
  (`Network.DialContext` binding, `Decision.Interfaces`); Failure behavior
  (interface failover is not route failover).
- **0003:** D1 (not upstream failover); D6 the no-usable-interface row; D7
  the attempt timeout and connect scopes; D8 trigger and reload; D11 the
  dial plan and `self`; D13 the unbound private link.
- **0004:** D3 keys and the `connect_timeout` scope; D4 errors and
  warnings; D5 starter comments; D6 reload steps.
- **0005:** D1 field and lead, D2 schema and upsert, D3 sizes, D4 folding
  key, D9 dashboard, S1, S6, and B3.
- **0006:** D3 `cuttle interfaces`, the status purpose, and the `explain`
  example; D4 status `interfaces` and tunnel `interface`; D7 log levels.
- **0007:** D3 injected interface source and per-binding behavior; D7 T0
  commands and the device check at M1 close; E2.
- **README.md**, **ROADMAP.md** Later list, and **OPEN_ISSUES.md**
  (OI-0008 and the OI-0007 update).

Iteration 3 changed: 0002 D7 (no `interface_dials_total`) and Interfaces
(`Binding`); 0003 D8 and 0004 D6 (a changed named definition changes an
upstream); 0004 D4 (no MAC in messages, including TOML syntax errors);
0007 D3 (wrapping and the injected `SelfChanged`); README.md (VPN
routes); COMPATIBILITY.md (device-check rows); OPEN_ISSUES.md (OI-0008
origin). At landing, D15 limited `skipped` to interfaces before the one
that carried or ended the dial, and narrowed the address claim to
interface table rows.

The TASK-0009 implementation changed, to match the code: D5 and D16 (the
`rt_msghdr` parser in cuttle, an absent mask as /0, and short kernel
event messages as notices); D6 (the attempt-timeout warning only with a
list); D10 (no `detail` for remote clients); D11 (every non-public class
in group 0); D15 (tried interfaces in `skipped` wherever they come, and
`timeout` for groups the budget never reached); Interfaces
(`Link.Loopback`); I10 and I19; and 0007 D7 and E2 (the cgo-file check).

## Consequences

- One configuration expresses the cost order and per-destination pins
  without changing macOS's own routing, and other applications keep using
  the system's primary interface.
- A dead uplink behind a non-primary interface's live link costs one
  attempt timeout, then a demotion period on the next interface. Behind
  the primary interface it fails names at DNS, so most requests get
  `502 dns_error` until the user takes the primary out of service (D13).
  cuttle cannot tell either case without probes, which the user ruled out.
- One destination's fault can demote the cheapest interface for every
  destination for 30 s (D9).
- DNS and unbound dials still follow the macOS service order; the user
  aligns it with `proxy.interfaces`.
- With `proxy.interfaces` or a rule list set, public `direct` traffic
  bypasses full-tunnel and split-tunnel VPN routes (D7). The user sends no
  public destination through a VPN.
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
  be renamed ([research 0003 §6][r3-ids]). Iteration 2 also dropped the
  port-name column of `cuttle interfaces`, which needed `networksetup`;
  attaching devices one at a time identifies them instead.
- **Racing interfaces in parallel like happy eyeballs.** It would open
  SYNs on costlier interfaces while a cheaper one still answers, and a slow
  cheaper path would lose whole tunnels to a costlier one.
- **No failover on timeout.** It follows the literal "fast failure" rule,
  but a phone without data or a silent dead uplink would make every request
  through it wait the whole connect budget and fail. The user chose
  failover.
- **Demotion per destination.** It avoids the cost effect of D9, but it
  needs per-destination state and makes each new destination wait on a
  broken interface again. The user chose global demotion.
- **A fail-closed interface hold like [0003 D8][d3-8].** It would break a
  list with one interface; demotion reorders instead.
- **`502 destination_ip_unroutable` for no usable interface.** It names an
  unroutable destination address, while the cause is the state of cuttle's
  own interfaces, which is temporary.
- **A rule option for system routing of public destinations.** It would
  keep VPN routes for chosen destinations while `proxy.interfaces` is
  set. The user never sends public destinations through a VPN, so
  iteration 3 left it out (D7).
- **Binding upstreams with the default list.** It ignores VPN routes and
  would break the private link of [0003 D13][d3-13].
- **Several lists on one upstream, chosen per rule.** One entry's hold and
  pool would then mix outcomes of different interfaces. Two entries with
  one address keep each state separate (D11).
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
[d3-10]: 0003-upstream-chaining-and-exposure.md#d10-source-classes-and-remote-policy
[d3-11]: 0003-upstream-chaining-and-exposure.md#d11-destination-address-policy-for-direct-routes
[d3-13]: 0003-upstream-chaining-and-exposure.md#d13-edge-gateway-and-egress-deployment
[d3-ac]: 0003-upstream-chaining-and-exposure.md#acceptance-criteria
[d4-4]: 0004-configuration-and-reload.md#d4-validation
[d4-6]: 0004-configuration-and-reload.md#d6-reload
[d5-2]: 0005-statistics-and-admin.md#d2-schema
[d5-10]: 0005-statistics-and-admin.md#d10-settings
[d6-4]: 0006-packaging-paths-and-lifecycle.md#d4-control-socket
[d7-7]: 0007-test-environment-and-gates.md#d7-gates
[oi-6]: ../OPEN_ISSUES.md#oi-0006--design-follow-ups-from-the-task-0001-final-review
[oi-7]: ../OPEN_ISSUES.md#oi-0007--local-network-privacy-for-cuttle-under-launchd
[oi-8]: ../OPEN_ISSUES.md#oi-0008--interface-behavior-on-the-users-devices
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

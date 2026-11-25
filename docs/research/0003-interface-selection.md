# 0003 — Interface selection on macOS: binding, state, and naming

- Task: TASK-0008 (milestone M1.5, interface selection), iteration 1.
- Date: 2026-11-25.
- Environment: macOS 27.2.1 (build 26B131), arm64, Apple M5 Max; Darwin
  27.2.0 with kernel `xnu-13432.41.4`; Go 1.27.4 darwin/arm64 from the
  Homebrew `go` bottle; `golang.org/x/net` v0.61.0 from the module cache.
- Scope: cuttle is macOS-only. Darwin mechanisms are in scope; cgo is a
  cost that the designer weighs. The target setup has five interfaces:
  Wi-Fi joined to one fixed network, Ethernet A, Ethernet B, and USB
  tethering from phone A and phone B. No phone hotspot is joined over
  Wi-Fi.
- Path abbreviations: `GOSRC/` is `/opt/homebrew/Cellar/go/1.27.4/libexec/src`
  (<https://github.com/golang/go/tree/go1.27.4/src>). Apple sources are cited
  at these published tags: `xnu-12377.121.6`, `configd-1405.120.5`,
  `mDNSResponder-2881.120.11`, and `network_cmds-741.100.2`. The running
  kernel is newer than the newest published XNU tag, so behavior that
  matters was also observed on this Mac.
- Observations were read-only: `ifconfig -a`, `ifconfig -v -a`,
  `netstat -rn` (`-f inet`, `-f inet6`, `-l`), `route -n get` (with and
  without `-ifscope`), `scutil --nwi`, `scutil --dns`, `networksetup
  -listallhardwareports`, `networksetup -listnetworkserviceorder`,
  `ipconfig getoption` and `getpacket`, `arp -n <gateway>`,
  `system_profiler SPUSBDataType`, `SPUSBHostDataType`, and
  `SPEthernetDataType`, plus two scratch Go programs in
  `<scratch>/research-0003/`. Raw output stays in the scratch directory.
  This note replaces every address, MAC, and name with a placeholder or
  leaves it out.
- The scratch programs sent no packets. A UDP `connect()` only performs a
  route lookup and selects a source address. The planned TCP connection to
  the default gateway was not run; [§7](#local-network-privacy) gives the
  reason.
- Interfaces present during observation: `en0` (Wi-Fi, active, with a
  scoped default route) and `en7` (`Thunderbolt Ethernet Slot 0`, a
  Thunderbolt-attached 10 Gb/s controller, active, the primary interface).
  Both reach the same router. `en3`–`en5` (`Ethernet Adapter (enN)`) and
  `anpi0`–`anpi2` are present but inactive with no adapter attached. Also
  present: `en1`, `en2`, and `en6` (Thunderbolt IP, members of `bridge0`)
  and `utun0`–`utun7`. A network service `iPhone USB` with device `en9` is
  configured, but no phone was attached. No USB Ethernet adapter or second
  Ethernet adapter was attached, so the claims about those devices come
  from source and are labelled.

## Summary

A socket option binds a TCP socket to one interface: `IP_BOUND_IF` for
IPv4 and `IPV6_BOUND_IF` for IPv6, each taking an interface index. Every
route lookup then requires a route through that interface. That route may
be the interface's own scoped default route or the primary's unscoped one,
so traffic leaves through the bound interface even when another interface
is primary. Go's standard `syscall` package exports both constants for
darwin, and `net.Dialer.Control` can set them with no cgo and no new
dependency.

On this Mac, a socket bound to the non-primary Wi-Fi chose a Wi-Fi source
address. A socket bound to an interface with no address failed with
`ENETUNREACH` at `connect()`, before any packet. The option is
family-specific, so an IPv6 attempt needs `IPV6_BOUND_IF`. An interface
without an IPv6 default route fails that attempt at once with
`EHOSTUNREACH`.

Name resolution cannot follow the binding without cgo or a resolver of
cuttle's own. Go on darwin calls libSystem's `getaddrinfo`, which has no
interface argument. mDNSResponder then queries over the primary interface,
so every lookup uses the primary network's resolver.

"Online" can be read without probe traffic and without cgo. The checks are
media status through `SIOCGIFMEDIA`, the interface's addresses, and a
default route on that interface from the routing table. Go's `FlagUp` and
`FlagRunning` are set on every present Ethernet-type interface, linked or
not, so they cannot serve. A routing socket and a kernel event socket both
open without privileges and report changes.

macOS names interfaces by MAC address. The BSD name follows an adapter or
phone across replugging, port changes, and reboots. However, a new device
of the same model can take over the name of an absent one. Hardware port
names are identical for identical models, and service names are
user-editable. The MAC address is therefore the most robust key for the two
Ethernet adapters and the two phones, and the BSD name is the key for
built-in Wi-Fi.

Binding interacts with VPN routes and with the local network privacy rules
that apply to a `launchd` agent.

## 1. Binding a TCP connection to one interface

### `IP_BOUND_IF` and `IPV6_BOUND_IF`

- Both options are public socket options that take an `int` interface
  index: `IP_BOUND_IF` (25) at level `IPPROTO_IP`
  ([in.h l.443][xnu-in-h]) and `IPV6_BOUND_IF` (125) at level
  `IPPROTO_IPV6` ([in6.h l.644][xnu-in6-h]).
- XNU explains `IP_BOUND_IF`: it "binds a particular AF_INET socket to an
  interface such that data sent on the socket is restricted to that
  interface. This is unlike the SO_DONTROUTE option where the routing table
  is bypassed" ([ip_output.c l.2594–2627][xnu-ipout-opt]). The IPv4 option
  is "settable only for IPv4" and returns `EINVAL` on an IPv6 socket. The
  IPv6 variant is handled in `ip6_ctloutput`
  ([ip6_output.c l.2869–2872][xnu-ip6out-opt]).
- Both options call `inp_bindif`. It returns `ENXIO` for an unknown index,
  and otherwise records the interface on the PCB and drops the cached
  route. The comment reads: "any route lookup from this point on will
  require an exact match for the embedded interface scope"
  ([in_pcb.c l.3196–3240][xnu-bindif]).
- At connect time, `in_pcbladdr` takes the bound interface as the scope
  and calls `rtalloc_scoped(ro, ifscope)`. It then takes the source address
  from the interface of the route it found
  ([in_pcb.c l.1597–1632][xnu-pcbladdr]).
- A scoped lookup "is searched for the best match that would result in a
  route using the same interface as the one associated with the scope (the
  exception to this are routes that point to the loopback interface)"
  ([route.c l.3168–3182][xnu-route-scope]). It therefore finds the
  interface's own scoped default route (`UGScIg`, flag `I`), or the primary
  interface's unscoped default when the bound interface is primary.
- Observed: `route -n get -ifscope en0 default` returned `en0`'s scoped
  default with flag `IFSCOPE`. `-ifscope en7` returned the primary's
  unscoped default. `-ifscope en3` returned `not in table`.

### Binding a source address (`net.Dialer.LocalAddr`)

- TCP output always asks IP to pick the interface from the source address
  (`IPOAF_SELECT_SRCIF | IPOAF_BOUND_SRCADDR`,
  [tcp_output.c l.3650][xnu-tcpout]). `in_selectsrcif` looks for the
  interface that owns the source address. If the unscoped route uses a
  different interface that lacks that address, and forwarding is off, it
  leaves "the caller [to] do a scoped route lookup" on the owning interface
  ([ip_output.c l.3130–3300][xnu-selectsrcif]). Inference: a TCP socket
  bound to an address of `en0` leaves through `en0` even when another
  interface is primary.
- Binding an address has three weaknesses compared with `IP_BOUND_IF`
  (inference):
  - The address must be read and kept current. DHCP renewals, IPv6
    temporary addresses, and reattachment change it, and binding a stale
    address fails with `EADDRNOTAVAIL`.
  - Each family needs a separately chosen address.
  - Two interfaces can hold the same address. XNU allows this and mentions
    "switching between interfaces configured with the same IP address"
    ([ip_output.c l.3172–3175][xnu-selectsrcif]). Two phones that hand out
    the same subnet would produce that case
    ([§4](#4-network-identity-secondary)).

  `IP_BOUND_IF` names the interface and lets the kernel choose the address.
- XNU also defines `SO_BINDTODEVICE` (`0x1134`, binding by name) in the
  public `sys/socket.h` ([socket.h l.210][xnu-socket-h]). Neither Go's
  `syscall` package nor `golang.org/x/sys/unix` v0.49.0 exports it for
  darwin, and it was not tried. `IP_BOUND_IF` is the established option.

### Observation

The scratch program set the option in `net.Dialer.Control` and called
`DialContext("udp4", "192.0.2.1:9")` or `("udp6", "[2001:db8::1]:9")`. It
then reported which interface owned the chosen source address. Both
destinations are documentation addresses, and no datagram was sent.

| Socket | Result |
| --- | --- |
| Unbound, `udp4` | Source on `en7` (primary) |
| `IP_BOUND_IF` `en0` (Wi-Fi, scoped default) | Source on `en0` |
| `IP_BOUND_IF` `en7` (primary) | Source on `en7` |
| `IP_BOUND_IF` `en3` (link inactive, no address) | `connect: network is unreachable` (`ENETUNREACH`, 51) |
| `IP_BOUND_IF` `lo0` | `ENETUNREACH` |
| `IP_BOUND_IF` `utun0` (IPv6 only) | `ENETUNREACH` |
| `IPV6_BOUND_IF` `en0` or `en7` (global IPv6 address, no IPv6 default route) | `connect: no route to host` (`EHOSTUNREACH`, 65) |
| `IPV6_BOUND_IF` `utun0` (scoped IPv6 default) | Source on `utun0` |
| `IP_BOUND_IF` with index 999 | `setsockopt`: `device not configured` (`ENXIO`, 6) |
| `IP_BOUND_IF` on a `udp6` socket, or `IPV6_BOUND_IF` on a `udp4` socket | `setsockopt`: `invalid argument` (`EINVAL`, 22) |

Inference: TCP `connect()` runs the same `in_pcbladdr` route lookup
([in_pcb.c l.1538][xnu-pcbladdr]). A TCP connection bound to an interface
with no address or route should therefore fail locally in the same way,
without a SYN. TCP was not run ([open question 1](#open-questions)).

### Setting the option from Go

- `GOSRC/syscall/zerrors_darwin_arm64.go` defines `IP_BOUND_IF = 0x19`
  (l.552) and `IPV6_BOUND_IF = 0x7d` (l.506), and the amd64 file defines
  the same values. `golang.org/x/sys/unix` defines them too, but it is not
  needed.
- `net.Dialer.Control` runs after the socket is created and before
  `connect`. Its network argument is `"tcp4"` or `"tcp6"`, never plain
  `"tcp"` ([GOSRC/net/dial.go l.209–216][go-dial];
  [GOSRC/net/sock_posix.go l.74–99][go-sockposix]). The control function
  therefore picks `IP_BOUND_IF` or `IPV6_BOUND_IF` from the suffix and calls
  `syscall.SetsockoptInt(fd, level, opt, index)` inside `RawConn.Control`.
  This is what the scratch program does.
- The option takes an index, and the configuration holds names or MACs.
  Inference: the dialer resolves the current index at dial time, or from a
  state snapshot that interface events refresh, because an index can change
  when a device detaches and reattaches. XNU recycles the interface record
  of a reattaching device that has the same unique ID
  ([dlil_ctl.c l.44–109][xnu-dlil]). Whether IONetworkingFamily passes a
  stable unique ID, which would keep the index stable, was not checked.

### IPv6, happy eyeballs, and missing families

- cuttle does not use Go's dual-stack fallback for these dials. It resolves
  first, filters by [0003 D11][d3-11], and then dials literal addresses
  itself with a 300 ms stagger (`internal/netx` `dialAll`). Go's
  `Dialer.FallbackDelay` never applies. Each attempt has one family, so the
  control function always knows which option to set.
- An interface without a usable route for a family fails that family's
  attempt at once (observed: `EHOSTUNREACH` for IPv6 on `en0` and `en7`,
  `ENETUNREACH` for IPv4 on `utun0` and `en3`). The stagger loop then
  starts the next address immediately, because `dialAll` starts the next
  address on any failure. An interface with only one family loses little
  time.
- Observed: `en0` and `en7` each carry a global IPv6 address with no IPv6
  default route, and `scutil --nwi` reports "No IPv6 states found". An
  interface can therefore be online for IPv4 and offline for IPv6 at the
  same moment, and "online" needs to be judged per family.

[xnu-in-h]: https://github.com/apple-oss-distributions/xnu/blob/xnu-12377.121.6/bsd/netinet/in.h#L443
[xnu-in6-h]: https://github.com/apple-oss-distributions/xnu/blob/xnu-12377.121.6/bsd/netinet6/in6.h#L644
[xnu-ipout-opt]: https://github.com/apple-oss-distributions/xnu/blob/xnu-12377.121.6/bsd/netinet/ip_output.c#L2594-L2627
[xnu-ip6out-opt]: https://github.com/apple-oss-distributions/xnu/blob/xnu-12377.121.6/bsd/netinet6/ip6_output.c#L2869-L2872
[xnu-bindif]: https://github.com/apple-oss-distributions/xnu/blob/xnu-12377.121.6/bsd/netinet/in_pcb.c#L3196-L3240
[xnu-pcbladdr]: https://github.com/apple-oss-distributions/xnu/blob/xnu-12377.121.6/bsd/netinet/in_pcb.c#L1538-L1632
[xnu-route-scope]: https://github.com/apple-oss-distributions/xnu/blob/xnu-12377.121.6/bsd/net/route.c#L3168-L3182
[xnu-tcpout]: https://github.com/apple-oss-distributions/xnu/blob/xnu-12377.121.6/bsd/netinet/tcp_output.c#L3650
[xnu-selectsrcif]: https://github.com/apple-oss-distributions/xnu/blob/xnu-12377.121.6/bsd/netinet/ip_output.c#L3130-L3300
[xnu-socket-h]: https://github.com/apple-oss-distributions/xnu/blob/xnu-12377.121.6/bsd/sys/socket.h#L210
[xnu-dlil]: https://github.com/apple-oss-distributions/xnu/blob/xnu-12377.121.6/bsd/net/dlil_ctl.c#L44-L109
[go-dial]: https://github.com/golang/go/blob/go1.27.4/src/net/dial.go#L209-L216
[go-sockposix]: https://github.com/golang/go/blob/go1.27.4/src/net/sock_posix.go#L74-L99
[d3-11]: ../design/0003-upstream-chaining-and-exposure.md#d11-destination-address-policy-for-direct-routes

## 2. Name resolution

- Go prefers the libc resolver on darwin. The source gives the reason:
  "Darwin pops up annoying dialog boxes if programs try to do their own
  DNS requests, so prefer cgo" ([GOSRC/net/conf.go l.167–182][go-conf]).
  Without cgo, the build tag `!netgo && darwin` selects
  `cgo_unix_syscall.go`, which calls libSystem's `getaddrinfo` through a
  `go:cgo_import_dynamic` trampoline
  ([cgo_unix_syscall.go l.5, l.68][go-cgosys];
  [internal/syscall/unix/net_darwin.go l.43–47][go-netdarwin]).
- Observed with `GODEBUG=netdns=2` on a lookup of `localhost` in a no-cgo
  build: `confVal.netCgo = false netGo = false`, then
  `hostLookupOrder(localhost) = cgo`. cuttle's `netx.System` therefore
  resolves through libSystem today.
- `getaddrinfo` has no interface argument. Inference: libSystem hands it to
  mDNSResponder as an interface-0 query. The `dns_sd.h` documentation says
  that passing 0 "causes the query to be sent on all active interfaces via
  Multicast or the primary interface via Unicast"
  ([dns_sd.h l.2144–2145][dnssd-ga]; also l.916–921). Lookups for a
  destination pinned to phone B therefore leave through the primary
  interface and its resolver.
- Observed: `scutil --dns` lists per-interface resolvers under "DNS
  configuration (for scoped queries)". Each has an `if_index` (for example
  `27 (en7)` and `15 (en0)`) and the flag `Scoped`. macOS keeps a separate
  resolver per interface but does not use it for unscoped queries.
- Ways to scope a lookup, by cost:
  1. **Accept primary-interface lookups.** This needs no code. The costs
     (inference):
     - The DNS traffic is small but crosses the primary network.
     - The primary network's resolver sees every name, including names
       routed through a phone.
     - Answers come from the primary network's resolver. A CDN may then
       pick a server suited to the primary network, and a split-horizon
       name may resolve to an address that is unreachable over the bound
       interface.
     - When the primary is offline, macOS picks the next primary by its own
       service order, not cuttle's priority list. The observed order is
       `Thunderbolt Ethernet Slot 0`, `Wi-Fi`, `Thunderbolt Bridge`,
       `iPhone USB`.
     - A DNS failure happens before any interface is chosen, so cuttle
       cannot fail over from it.
  2. **Go's own resolver over a bound socket.** Set `Resolver.PreferGo`
     and a `Resolver.Dial` that ignores the server address it is given and
     dials the interface's scoped server with `IP_BOUND_IF`
     ([GOSRC/net/lookup.go l.138, l.150–161][go-lookup]). This is pure Go.
     It needs the per-interface server list, which comes only from
     `scutil --dns` (a subprocess and text parsing), from
     `ipconfig getoption <if> domain_name_server` (a subprocess; DHCP only,
     and observed to be absent for the manually configured `en7`, whose
     `ipconfig getpacket` failed with `not found`), or from SCDynamicStore
     (cgo). It also loses mDNSResponder's cache, DNS64 synthesis,
     encrypted DNS settings, and VPN split DNS. Go's own comment above
     warns of dialog boxes, and whether that still holds on macOS 27 is
     unknown ([open question 8](#open-questions)).
  3. **`DNSServiceGetAddrInfo` with an interface index.** A positive index
     "indicates to do the operation only on that one specified interface"
     ([dns_sd.h l.923–924][dnssd-if]). This needs cgo, or
     `go:cgo_import_dynamic` trampolines plus a way to receive C
     callbacks. The compiler allows `go:cgo_import_dynamic` outside cgo
     files ([GOSRC/cmd/compile/internal/noder/noder.go l.315–327][go-noder]),
     but the callbacks make this the most complex option.
- Local network privacy does not apply to "Resolving a non-local DNS name
  with the system resolver" ([TN3179][tn3179]).

[go-conf]: https://github.com/golang/go/blob/go1.27.4/src/net/conf.go#L167-L182
[go-cgosys]: https://github.com/golang/go/blob/go1.27.4/src/net/cgo_unix_syscall.go#L5-L70
[go-netdarwin]: https://github.com/golang/go/blob/go1.27.4/src/internal/syscall/unix/net_darwin.go#L43-L47
[go-lookup]: https://github.com/golang/go/blob/go1.27.4/src/net/lookup.go#L138-L161
[go-noder]: https://github.com/golang/go/blob/go1.27.4/src/cmd/compile/internal/noder/noder.go#L315-L327
[dnssd-ga]: https://github.com/apple-oss-distributions/mDNSResponder/blob/mDNSResponder-2881.120.11/mDNSShared/dns_sd.h#L2144-L2145
[dnssd-if]: https://github.com/apple-oss-distributions/mDNSResponder/blob/mDNSResponder-2881.120.11/mDNSShared/dns_sd.h#L916-L924
[tn3179]: https://developer.apple.com/documentation/technotes/tn3179-understanding-local-network-privacy

## 3. Interface state without probe traffic

### Reading state

Observed with `net.Interfaces`, `SIOCGIFMEDIA`, and the routing table:

| Interface | Go flags | Media (`SIOCGIFMEDIA`) | IPv4 addresses | IPv4 default route |
| --- | --- | --- | --- | --- |
| `en0` (Wi-Fi) | `up\|broadcast\|multicast\|running` | active | 1 | scoped (`RTF_IFSCOPE`) |
| `en7` (Thunderbolt Ethernet) | `up\|broadcast\|multicast\|running` | active | 1 | primary, unscoped |
| `en3`–`en5` (`Ethernet Adapter (enN)`) | `up\|broadcast\|multicast\|running` | inactive | 0 | none |
| `en1`, `en2`, `en6` (Thunderbolt IP) | `up\|broadcast\|multicast\|running` | inactive | 0 | none |
| `utun0`–`utun7` | `up\|pointtopoint\|multicast\|running` | not supported | 0 | IPv6 scoped default only |

- **Go flags are not a link signal.** `FlagRunning` maps `IFF_RUNNING`
  ([GOSRC/net/interface.go l.43–48][go-iface]), and every present
  Ethernet-type interface had it whether or not a link existed. `ifconfig`
  showed the same: `en3` reported `RUNNING` and `status: inactive`.
- **Media status** comes from `ioctl(SIOCGIFMEDIA)`. The constant
  `syscall.SIOCGIFMEDIA` (`0xc02c6938`, 44-byte request) exists in Go's
  `syscall` package. `IFM_AVALID` and `IFM_ACTIVE` in `ifm_status` match
  `ifconfig`'s `status: active` and `status: inactive`
  ([sockio.h l.123][xnu-sockio]). This is pure Go through
  `syscall.Syscall(SYS_IOCTL, …)` and was observed working without
  privileges.
- **Addresses** come from `net.Interfaces` and `Interface.Addrs`, which
  read the `NET_RT_IFLIST` sysctl ([GOSRC/net/interface_darwin.go
  l.13][go-ifdarwin]). This is pure Go.
- **Default route per interface.** `route.FetchRIB(AF_INET,
  RIBTypeRoute, 0)` from `golang.org/x/net/route` returned both default
  routes with their interface index and flags: `en0` with `RTF_IFSCOPE`
  (`0x1000000`) and `en7` without it. `NET_RT_DUMP2` gave the same result.
  This is pure Go and needs no privileges. Note that 0002 D1 lists
  `golang.org/x/net` only for test-only WebSocket code, so using
  `x/net/route` in `cmd/cuttle` needs a design decision
  ([0002 D1][d2-1]). The standard library alternative is
  `syscall.RouteRIB` and `syscall.ParseRoutingMessage`, which still exist
  but are marked deprecated in favor of `x/net/route`
  ([GOSRC/syscall/route_bsd.go l.180–331][go-routebsd]). The same fact is
  visible without code through `route -n get -ifscope <if> default`.
- **Extended flags.** The `expensive`, `constrained`, functional type, and
  subfamily flags are readable in pure Go
  ([§4](#4-network-identity-secondary)).
- A candidate "online for a family" predicate built from these reads
  (inference): the interface exists, media is active where media is
  supported, it has a usable address of the family (not link-local), and a
  default route of the family uses it. The predicate needs no packets.

### Noticing changes

| Mechanism | Pure Go | Privileges | What it reports | Cost |
| --- | --- | --- | --- | --- |
| Poll `net.Interfaces`, `SIOCGIFMEDIA`, and the route dump | Yes | None | Full state each time | Sysctls and ioctls; latency equals the interval |
| Routing socket `socket(PF_ROUTE, SOCK_RAW, 0)` | Yes (`syscall`; parse with `x/net/route` or `syscall`) | None (observed: opened and read with a 3 s timeout, no messages while nothing changed) | `RTM_NEWADDR`, `RTM_DELADDR`, `RTM_IFINFO` (up or down and flag changes), `RTM_ADD`, `RTM_DELETE` ([route.h l.197–219][xnu-route-h]) | One blocking reader; no interface attach or detach message, because macOS has no `RTM_IFANNOUNCE` |
| Kernel event socket `socket(AF_SYSTEM, SOCK_RAW, SYSPROTO_EVENT)` with `SIOCSKEVFILT` | Yes (`syscall`) | None (observed: opened and filtered to the network class, no events while nothing changed) | `KEV_DL_IF_ATTACHED`, `KEV_DL_IF_DETACHED`, `KEV_DL_LINK_ON`, `KEV_DL_LINK_OFF`, and address events ([net_kev.h l.50–77][xnu-netkev]; [socket.h l.425][xnu-socket-af]; [sys_domain.h l.38][xnu-sysdomain]) | Binary structures to parse; the event codes are in public headers |
| SystemConfiguration `SCDynamicStore` notifications | No (cgo, or trampolines with callbacks) | None | Per-service state as configd sees it, including DHCP and DNS | cgo in the build and the gates |
| Run `scutil --nwi`, `networksetup`, or `ipconfig` on change | Yes | None | Text for display names and DHCP data | A process spawn per read and text parsing; use only on attach or at startup |

- XNU sends `RTM_IFINFO` from `if_updown` and from flag ioctls
  ([if.c l.1586–1649, l.4060][xnu-if]). It posts `KEV_DL_IF_ATTACHED` and
  `KEV_DL_IF_DETACHED` from DLIL ([dlil.c l.5345, l.6012][xnu-dlil-ev]).
- Inference: on cable removal, the address and the default route are
  removed after configd's IPConfiguration sees the link go down. Those
  removals reach the routing socket, so routing-socket events can trigger a
  full re-read. How long IPConfiguration keeps the address after a link
  loss was not observed ([open question 5](#open-questions)).
- A low-cost combination (inference): re-read the full state after any
  routing-socket or kernel event, after a short debounce, and also on a
  slow timer as a safety net. Only the re-read decides "online", so missed
  or reordered messages do no harm.

### Link up but no internet

The agreed rule is "move on after a fast connect failure, no probes". The
table shows what each case looks like. All entries are inferences except
where marked.

| Case | What a bound connect sees | Caught by the rule? |
| --- | --- | --- |
| Device unplugged | No interface; index lookup fails, or `ENXIO` | Yes, before dialing |
| Cable out, or phone tethering stopped while the interface stays | No address or route soon after; `ENETUNREACH` or `EHOSTUNREACH` locally (observed for an interface with no address) | Yes |
| No IPv6 on the interface | `EHOSTUNREACH` locally (observed) | Yes |
| Router up, upstream ISP down | A timeout, or a fast ICMP unreachable from the router | Only with ICMP |
| Phone attached with no cellular data | Unknown: a timeout, an ICMP error, or a reset from the phone | Unknown ([open question 6](#open-questions)) |
| Destination refuses (`ECONNREFUSED`) | The destination answered over a working path | Must not move on: another interface reaches the same refusal at a higher cost |
| Local network privacy denial for a LAN destination | An immediate error of unknown errno | Must not move on ([§7](#local-network-privacy)) |

The rule therefore catches missing interfaces, missing links, missing
addresses, and missing routes, all of which fail locally without packets.
A silent blackhole on an interface with an address costs a full connect
timeout per attempt. With [0003 D7][d3-7]'s 10 s `connect_timeout` spanning
all attempts, one blackholed interface can use the whole budget. Options
for the designer follow under [Design implications](#design-implications).

[go-iface]: https://github.com/golang/go/blob/go1.27.4/src/net/interface.go#L43-L48
[go-ifdarwin]: https://github.com/golang/go/blob/go1.27.4/src/net/interface_darwin.go#L13
[go-routebsd]: https://github.com/golang/go/blob/go1.27.4/src/syscall/route_bsd.go#L180-L331
[xnu-sockio]: https://github.com/apple-oss-distributions/xnu/blob/xnu-12377.121.6/bsd/sys/sockio.h#L123
[xnu-route-h]: https://github.com/apple-oss-distributions/xnu/blob/xnu-12377.121.6/bsd/net/route.h#L197-L219
[xnu-netkev]: https://github.com/apple-oss-distributions/xnu/blob/xnu-12377.121.6/bsd/net/net_kev.h#L50-L77
[xnu-socket-af]: https://github.com/apple-oss-distributions/xnu/blob/xnu-12377.121.6/bsd/sys/socket.h#L425
[xnu-sysdomain]: https://github.com/apple-oss-distributions/xnu/blob/xnu-12377.121.6/bsd/sys/sys_domain.h#L38
[xnu-if]: https://github.com/apple-oss-distributions/xnu/blob/xnu-12377.121.6/bsd/net/if.c#L1586-L1649
[xnu-dlil-ev]: https://github.com/apple-oss-distributions/xnu/blob/xnu-12377.121.6/bsd/net/dlil.c#L5345
[d2-1]: ../design/0002-proxy-core-and-routing.md#d1-toolchain-dependencies-and-layout
[d3-7]: ../design/0003-upstream-chaining-and-exposure.md#d7-timeouts

## 4. Network identity (secondary)

The target Wi-Fi joins one fixed network, so this section only lists
options for other setups.

- **Gateway address and subnet.** These come from the route dump or
  `route -n get -ifscope <if> default`. They are not unique: on this Mac,
  `en0` and `en7` have the same gateway address and the same /24, because
  both reach one router. Home routers also share default subnets.
- **Gateway MAC.** `arp -n <gateway>` works without privileges and lists
  one entry per interface, marked `ifscope` (observed: two entries, one
  each on `en0` and `en7`, with one MAC). Inference: pure Go can read the
  same table from the `NET_RT_FLAGS` sysctl with `RTF_LLINFO`; this was not
  tried.
- **DHCP server identifier.** `ipconfig getoption <if> server_identifier`
  and `ipconfig getpacket <if>` work without privileges for a DHCP
  interface (observed for `en0`). A manually configured interface has no
  packet (observed for `en7`: `ipconfig_get_packet(en7) failed: not
  found`). There is no pure-Go source short of SCDynamicStore (cgo).
- **IPv6 prefix.** This comes from the interface's global addresses. The
  prefix changes when the ISP renumbers.
- **iPhone Personal Hotspot subnet `172.20.10.0/28`.** No Apple source
  states it. The Personal Hotspot support article ([HT204023][ht204023]),
  the Mac User Guide page on using an iPhone's connection
  ([mchl7403f0ee][mac-iphone]), and the iPhone User Guide do not mention
  any address or subnet. The claim is unconfirmed, so a design must not
  depend on it. If it is true, two phones over USB would give two
  interfaces the same subnet and the same gateway address
  ([open question 9](#open-questions)).
- **Expensive and constrained.** Network framework defines
  `NWPath.isExpensive` as "whether the path uses an interface that is
  considered expensive, such as Cellular or a Personal Hotspot", and
  `isConstrained` as "whether the path uses an interface in Low Data
  Mode" ([isExpensive][nwpath-exp]; [isConstrained][nwpath-con]). Reading
  `NWPath` needs Network.framework and so cgo. The kernel keeps
  per-interface flags `IFEF_EXPENSIVE` ("Data access has a cost") and
  `IFXF_CONSTRAINED` ("Save Data Mode")
  ([if_private.h l.120, l.153][xnu-ifpriv]).
  - `ifconfig` prints ` constrained` on an interface's first line, and
    `ifconfig -v` prints `EXPENSIVE` among the `eflags`
    ([ifconfig.c l.2087–2097, l.2158–2173][ifconfig-c]).
  - The private ioctls `SIOCGIFEFLAGS` and `SIOCGIFXFLAGS`
    ([sockio_private.h l.159, l.265][xnu-sockio-priv]) and the public
    `SIOCGIFFUNCTIONALTYPE` ([sockio.h l.196][xnu-sockio-ft]) work from
    pure Go without privileges. Observed: the values matched `ifconfig -v`
    for `en0` and `en7`, and none of the observed interfaces was expensive
    or constrained. The functional type was Wi-Fi infrastructure (3) for
    `en0` and wired (2) for `en7`. The private `SIOCGIFTYPE` gave subfamily
    USB (1) for `en3` and Thunderbolt (4) for `en1`
    ([kpi_interface.h l.147–159][xnu-kpi]).
  - Inference: `NWPath`'s flags derive from these interface flags. Private
    ioctls may change between macOS releases. Whether macOS marks an
    `iPhone USB` interface expensive was not observed
    ([open question 2](#open-questions)). The fetch did not find where
    macOS documents the Low Data Mode setting.
- **SSID.** Not read, because a command might prompt for Location
  Services. Apple's `CWInterface.ssid()` documentation mentions no
  Location Services requirement ([ssid()][cw-ssid]), and no Apple source
  found here states which macOS commands redact it. The target setup does
  not need it.

[ht204023]: https://support.apple.com/en-us/HT204023
[mac-iphone]: https://support.apple.com/guide/mac-help/iphone-internet-connection-mac-mchl7403f0ee/mac
[nwpath-exp]: https://developer.apple.com/documentation/network/nwpath/isexpensive
[nwpath-con]: https://developer.apple.com/documentation/network/nwpath/isconstrained
[xnu-ifpriv]: https://github.com/apple-oss-distributions/xnu/blob/xnu-12377.121.6/bsd/net/if_private.h#L120-L153
[ifconfig-c]: https://github.com/apple-oss-distributions/network_cmds/blob/network_cmds-741.100.2/ifconfig.tproj/ifconfig.c#L2087-L2173
[xnu-sockio-priv]: https://github.com/apple-oss-distributions/xnu/blob/xnu-12377.121.6/bsd/sys/sockio_private.h#L159-L265
[xnu-sockio-ft]: https://github.com/apple-oss-distributions/xnu/blob/xnu-12377.121.6/bsd/sys/sockio.h#L196
[xnu-kpi]: https://github.com/apple-oss-distributions/xnu/blob/xnu-12377.121.6/bsd/net/kpi_interface.h#L147-L159
[cw-ssid]: https://developer.apple.com/documentation/corewlan/cwinterface/ssid()

## 5. iPhone USB tethering

- **Setup, from Apple.** Apple's article says to "Turn on Allow Others to
  Join" in Personal Hotspot, then "Connect your iPhone or iPad to your
  computer using a USB cable". It adds: "if asked to trust this computer,
  tap Trust", and "If asked to allow this accessory to connect, click
  Allow" ([HT204023][ht204023]). The Mac User Guide says: "As soon as it's
  connected by a cable, your Mac is able to use your iPhone or iPad
  device's cellular connection" ([mchl7403f0ee][mac-iphone]). The
  accessory prompt means that a phone or adapter attached for the first
  time can wait for a click on the Mac.
- **How macOS classifies it.** configd treats a network interface whose
  IOKit ancestry has `SupportsIPhoneOS = true` as an Ethernet-type
  interface sorted as tethered ([SCNetworkInterface.c
  l.1853–1864][cfg-tether]). The hardware port localization maps `iPhone`
  to `iPhone USB`, as read from this Mac's
  `SystemConfiguration.framework/Resources/NetworkInterface.loctable`.
  The port name comes from the IOKit product name through
  `update_interface_name` ([l.1668–1700][cfg-ifname]).
- **Observed.** A configured service named `iPhone USB` with device `en9`
  appears in `networksetup -listnetworkserviceorder` while no phone is
  attached. `networksetup -listallhardwareports` lists only attached
  interfaces, so it omitted `en9`. A detached phone's interface disappears
  completely, and only its service stays.
- **A second phone (inference).** Interface names are keyed by MAC
  ([§6](#6-naming-and-distinguishing-interfaces)), so a second phone gets
  its own `enN`, with a hardware port also named `iPhone USB` and a
  service named `iPhone USB 2`. Whether each phone's tethering interface
  keeps one MAC across replugging and reboots is not documented and was
  not observed ([open question 2](#open-questions)).
- **USB compared with a hotspot over Wi-Fi.** Over USB, the phone is a
  separate `enN` interface with its own scoped default route, so binding
  selects it directly. Over Wi-Fi, the hotspot is `en0` itself: binding
  cannot tell it from the home network, and only network identity can
  ([§4](#4-network-identity-secondary)). The target setup uses USB only.
- **Phone identity beyond the MAC.** On macOS 27.2.1,
  `system_profiler SPUSBDataType` printed nothing, and the USB tree is
  under `SPUSBHostDataType`, which shows each device's serial number
  (observed for attached hubs). Inference: an iPhone's USB serial number
  is its device identifier. Mapping a USB device to its `enN` needs IOKit
  (cgo) or parsing `ioreg`. The interface MAC is available in pure Go.
- **Media type and flags.** `ifconfig -v` prints `type:` and
  `functional type:` lines. What it prints for an `iPhone USB` interface
  was not observed ([open question 2](#open-questions)).

[cfg-tether]: https://github.com/apple-oss-distributions/configd/blob/configd-1405.120.5/SystemConfiguration.fproj/SCNetworkInterface.c#L1853-L1864
[cfg-ifname]: https://github.com/apple-oss-distributions/configd/blob/configd-1405.120.5/SystemConfiguration.fproj/SCNetworkInterface.c#L1668-L1700

## 6. Naming and distinguishing interfaces

### How macOS assigns BSD names

- configd's InterfaceNamer "uses InterfaceNamePrefix and MACAddress as the
  unique identifying keys" and "stores the InterfaceNamePrefix,
  MACAddress, and Unit in permanent storage to give persistent interface
  names" ([ifnamer.c l.50–57][cfg-ifnamer-hdr]).
- A device whose prefix and MAC are known gets its stored unit
  (`lookupInterfaceByAddress`, [l.838–872][cfg-lookup];
  `copyUnitForEstablishedInterface`, [l.3231–3277][cfg-established]).
  Inference: an adapter or phone keeps its `enN` across replugging, USB
  port changes, and reboots, because none of these changes the MAC. This
  was not observed for a USB adapter or phone here.
- **Replacement takes over a name.** A device with an unknown MAC can take
  over the unit of an inactive known entry. That entry must have the same
  prefix, interface type, built-in flag, and product information: the
  non-localized name, USB vendor ID, product ID, and product string
  (`copyEligibleMatchingInterfaces`, [l.2619–2720][cfg-eligible];
  `_SCNetworkInterfaceCopyInterfaceInfo`, [SCNetworkInterface.c
  l.7553–7590][cfg-info]). An entry in use is skipped: "name is already in
  use, not eligible for re-use". Matched devices are named directly
  ([l.3495–3640][cfg-namelist]). The replaced MAC is kept in a
  `MatchingMACs` history of at most 32 entries ([l.1075][cfg-history]).
  Inference: while Ethernet B is unplugged, a third adapter of the same
  model can become B's `enN`. Likewise a different phone that matches
  phone B's product information can become phone B's `enN` while phone B
  is absent.
- **Other exceptions.**
  - Interfaces marked ephemeral get no persistent name ([l.3240–3255][cfg-established]).
  - Built-in interfaces may have reserved units ([l.3279][cfg-established]).
  - On macOS, a new, unknown device is not named while the console is
    locked ([l.3247][cfg-established], [l.3433][cfg-other]).
- **Where the state lives.** The state is in
  `/Library/Preferences/SystemConfiguration/NetworkInterfaces.plist`.
  Inference: resetting it, which some network troubleshooting guides
  suggest, renumbers interfaces.

### Hardware port names and service names

- The hardware port name is the IOKit product name or the USB product
  string ([update_interface_name][cfg-ifname]). Without either, configd
  uses `USB Ethernet (%@)` with the BSD name ([l.2060][cfg-usbether]).
  PCI and Thunderbolt adapters get `Thunderbolt Ethernet Slot %@` from the
  PCI slot name ([l.2022][cfg-tbether]), and generic ports get
  `Ethernet Adapter (%@)`. The strings were read from this Mac's
  `NetworkInterface.loctable`.
- Observed hardware port names: `Thunderbolt Ethernet Slot 0` (`en7`),
  `Ethernet Adapter (en3)`, `(en4)`, and `(en5)`, `Wi-Fi` (`en0`),
  `Thunderbolt 1`–`3`, and `Thunderbolt Bridge`. Inference: two identical
  adapters get identical hardware port names. A name built from the BSD
  name changes when the BSD name does. Whether the Thunderbolt `Slot N`
  number follows the port or the dock was not observed.
- Service names are unique within a set. A duplicate gets the next number
  as a suffix (`copy_next_name`, [SCNetworkSet.c l.428–470][cfg-nextname]),
  so the second identical adapter's service becomes `<name> 2`. The user
  can rename services in System Settings. A service refers to its
  interface by BSD name (observed: `(Hardware Port: iPhone USB, Device:
  en9)`). Which device gets the `2` depends on which was attached first.

### MAC addresses

- Pure Go reads them through `net.Interface.HardwareAddr`. Observed: for
  every interface, the MAC that `networksetup` reports equalled the one
  that `ifconfig` reports.
- Virtual interfaces (`en1`–`en6`, `bridge0`) have locally administered
  MACs. `en0` and `en7` have universally administered MACs.
- Wi-Fi may use a private address per network. Apple says a device
  "identifies itself to each network using a different Wi-Fi address"
  ([private Wi-Fi addresses][apple-private]). The Wi-Fi MAC is therefore a
  poor key, while Wi-Fi's BSD name `en0` is stable for the built-in
  interface. The target Wi-Fi joins one fixed network, so its MAC would not
  change anyway.
- USB Ethernet adapters normally carry a fixed MAC (inference; not
  observed).

### Two identical Ethernet adapters swapped between ports

Inference from the naming source, not observed:

- BSD names, MACs, and services follow the adapters, not the ports.
- Hardware port names are the same for both adapters before and after the
  swap.
- Nothing about the port is part of the identity, except the `Slot N` in a
  Thunderbolt or PCI adapter's port name.

### Interfaces present without a device

`en3`–`en5` (`Ethernet Adapter (enN)`, `type: USB Ethernet`) and
`anpi0`–`anpi2` exist and are inactive with no adapter attached. A matcher
by kind, such as "any USB Ethernet", or by a port-name pattern would match
them. Selection should name specific devices.

### Plugging and unplugging

Inference from the sources above:

| Event | What the system shows |
| --- | --- |
| Adapter or phone attached | `KEV_DL_IF_ATTACHED`; the interface appears with its persistent `enN` (after the accessory prompt and console unlock, if needed); configd assigns an address, which brings `RTM_NEWADDR`, and default routes, which bring `RTM_ADD` |
| Adapter or phone detached | Addresses and routes go; `KEV_DL_IF_DETACHED`; the interface disappears from `net.Interfaces`, while its service stays |
| Cable removed from an attached adapter | The interface stays; media becomes inactive (`KEV_DL_LINK_OFF`); the address and routes are removed after a delay that was not observed |
| Phone stops sharing, or loses cellular data, while attached | Not observed ([open questions 5–6](#open-questions)) |

Inference: connections bound to an interface that detaches fail, and TCP
cannot move them to another interface, so tunnels on that interface end.

### Identifier set, ordered by robustness

Recommendation input for the designer. The examples use the RFC 9542
documentation MAC values `00:00:5e:00:53:00`–`00:00:5e:00:53:ff`
([RFC 9542 §2.1.4][rfc9542]). Real values belong in the user's
`~/.config/cuttle/config.toml` and never in tracked files.

| Rank | Identifier | Use for | Survives replug, port change, reboot | Weaknesses | Read in pure Go |
| --- | --- | --- | --- | --- | --- |
| 1 | Interface MAC | Ethernet A and B, phone A and B | Yes (inference; per-phone stability unverified) | Wi-Fi may randomize it; a reset or changed adapter MAC breaks the match | Yes, `HardwareAddr` |
| 2 | BSD name | Built-in Wi-Fi `en0`; a fallback or cross-check for the others | Yes while the device is known | A same-model replacement can take an absent device's name; renumbered after a reset of `NetworkInterfaces.plist` | Yes |
| 3 | Kind guard (functional type, subfamily) | Detecting misconfiguration, such as a MAC that now belongs to a non-wired interface | Yes | Not an identity; virtual interfaces share kinds | Yes, through private and public ioctls |
| 4 | Hardware port name, service name | Display only | Port name yes; service name until renamed | Identical for identical models (port); order-dependent and editable (service) | No: needs `networksetup` or SystemConfiguration |
| 5 | USB serial number (phone identifier) | Not recommended for the first version | Yes | Needs IOKit (cgo) or `ioreg` parsing to map to `enN` | No |

A label of cuttle's own, such as `eth-a` or `phone-b`, keeps the real
identifiers inside the configuration file and gives logs, metrics, and
rules a stable name.

[cfg-ifnamer-hdr]: https://github.com/apple-oss-distributions/configd/blob/configd-1405.120.5/Plugins/InterfaceNamer/ifnamer.c#L50-L57
[cfg-lookup]: https://github.com/apple-oss-distributions/configd/blob/configd-1405.120.5/Plugins/InterfaceNamer/ifnamer.c#L838-L872
[cfg-established]: https://github.com/apple-oss-distributions/configd/blob/configd-1405.120.5/Plugins/InterfaceNamer/ifnamer.c#L3231-L3290
[cfg-eligible]: https://github.com/apple-oss-distributions/configd/blob/configd-1405.120.5/Plugins/InterfaceNamer/ifnamer.c#L2619-L2720
[cfg-namelist]: https://github.com/apple-oss-distributions/configd/blob/configd-1405.120.5/Plugins/InterfaceNamer/ifnamer.c#L3495-L3640
[cfg-history]: https://github.com/apple-oss-distributions/configd/blob/configd-1405.120.5/Plugins/InterfaceNamer/ifnamer.c#L1075
[cfg-other]: https://github.com/apple-oss-distributions/configd/blob/configd-1405.120.5/Plugins/InterfaceNamer/ifnamer.c#L3433
[cfg-info]: https://github.com/apple-oss-distributions/configd/blob/configd-1405.120.5/SystemConfiguration.fproj/SCNetworkInterface.c#L7553-L7590
[cfg-usbether]: https://github.com/apple-oss-distributions/configd/blob/configd-1405.120.5/SystemConfiguration.fproj/SCNetworkInterface.c#L2060
[cfg-tbether]: https://github.com/apple-oss-distributions/configd/blob/configd-1405.120.5/SystemConfiguration.fproj/SCNetworkInterface.c#L2022
[cfg-nextname]: https://github.com/apple-oss-distributions/configd/blob/configd-1405.120.5/SystemConfiguration.fproj/SCNetworkSet.c#L428-L470
[apple-private]: https://support.apple.com/en-us/102509
[rfc9542]: https://www.rfc-editor.org/rfc/rfc9542.html#section-2.1.4

## 7. Interactions

### VPNs and the private link

- A bound socket uses only routes through its interface
  ([route.c l.3168–3182][xnu-route-scope]). Inference:
  - **The private link ends on this Mac** (a WireGuard or other `utun`
    interface; the third case in [0003 D13][d3-13]). An upstream dial
    bound to a NIC ignores the route through `utunN` and goes to that NIC's
    default gateway, so the upstream becomes unreachable or the traffic
    leaves outside the tunnel.
  - **The private link ends on a LAN router** (routed or NAT, as in the
    example deployment). The dial works only when the bound interface is
    on that LAN.
  - **A full-tunnel VPN.** A direct dial bound to a NIC bypasses the VPN.
    That is a leak if the user expects all traffic in the tunnel.
- Observed: `utun0`–`utun7` exist here, each with a scoped IPv6 default
  route. The system creates them, so `utun` interfaces exist even without a
  user VPN.
- Binding to a `utunN` interface works the same way (observed: the IPv6
  source was on `utun0`).

### Dual stack

- The option and the routes are per family ([§1](#1-binding-a-tcp-connection-to-one-interface)).
- An interface can be online for IPv4 only, which is the observed state of
  `en0` and `en7`. A per-family predicate lets the dialer skip a family
  that an interface cannot carry. Otherwise the attempt fails at once,
  which wastes little time.

### Destination checks (0003 D11) and `self`

- [0003 D11][d3-11] classifies by address and runs before the dial.
  Binding does not change the classes, but it changes reachability.
  Inference:
  - A `private` LAN destination on Wi-Fi's subnet, dialed through a socket
    bound to a phone, goes to the phone's gateway and fails or leaves the
    LAN.
  - `loopback` and `self` destinations still work, because the scoped
    lookup makes an exception for loopback routes
    ([route.c l.3176–3182][xnu-route-scope]).
  - An option is to apply interface selection only to `public`
    destinations and to dial `private`, `self`, and `loopback` destinations
    unbound.
- `self` comes from `net.InterfaceAddrs`, refreshed at most every 10 s
  (`netx.InterfaceAddrs`), so addresses of newly attached phones and
  adapters join `self` within 10 s. If two phones give this Mac the same
  address, `self` is unaffected, but a dial that binds a source address
  would be ambiguous. That is one more reason for `IP_BOUND_IF`.

### Local network privacy

- Local network privacy (macOS 15 and later) applies to outgoing TCP
  connections and to UDP connect and send toward a local network address.
  A local network is "an IP network associated with a broadcast-capable
  network interface. Such interfaces include Wi-Fi and Ethernet, but not
  cellular (WWAN) or VPN", and traffic to a local network address "is not
  forwarded by a router" ([TN3179][tn3179]).
- macOS allows such access without asking for three kinds of process: "Any
  daemon started by `launchd`", "Any program running as root", and
  "Command-line tools run from Terminal or over SSH, including any child
  processes they spawn". However, "The exception for `launchd` daemons
  doesn't apply to `launchd` agents" ([TN3179][tn3179]).
- Under `brew services`, cuttle is a user LaunchAgent
  ([research 0001 §2][r1-2]). Inference: on macOS 15 and later it needs
  the Local Network privilege for direct routes to LAN addresses, such as
  private literals, `allow_private` rules, and an upstream on the LAN. A
  phone's tethering subnet is on an Ethernet-type interface, so it may
  also count as local. This is not specific to M1.5, and the existing
  records do not cover it.
- For M1.5, the point is the failure. A denied operation fails at once
  with an error code that was not observed, and "move on after a fast
  failure" would misread it as an interface failure
  ([open question 7](#open-questions)).
- The planned TCP connection to the default gateway was not run for this
  reason. The agent's shell is not Terminal or SSH, so the exemption may
  not apply, and the connection could have raised the Local Network alert
  on the user's desktop. The UDP probe used only non-local documentation
  addresses.

[d3-13]: ../design/0003-upstream-chaining-and-exposure.md#d13-edge-gateway-and-egress-deployment
[r1-2]: 0001-homebrew-launchd-xdg.md#2-service-do-and-brew-services-without-sudo

## Design implications

These are options for the designer, not decisions.

- **Configuration shape.**
  - An `[[interface]]` table with a label (`name`) and match keys `mac`,
    `device` (BSD name), or both. If both are given, both must match. An
    optional `kind` guard could check `wired`, `wifi`, or `usb`.
  - A default priority list for `direct`, for example
    `proxy.interfaces = ["eth-a", "eth-b", "wifi", "phone-a", "phone-b"]`.
  - A per-rule `interfaces` list for pinning, and an optional per-upstream
    `interfaces` list. Example with placeholders:

    ```toml
    [[interface]]
    name = "eth-a"
    mac = "00:00:5e:00:53:0a"

    [[interface]]
    name = "phone-b"
    mac = "00:00:5e:00:53:1b"

    [[interface]]
    name = "wifi"
    device = "en0"

    [[rule]]
    name = "system-updates"
    domains = ["updates.example"]
    action = "direct"
    interfaces = ["phone-b"]
    ```

- **Pinning when the pinned interface is offline.** The requirement says
  only that B is used even when A is online. One option fails closed, like
  an upstream ([0003 D8][d3-8]) and with a `503`. The other falls back to
  the rest of the default list.
- **Network identity matchers** such as `gateway_mac` or `dhcp_server`
  are not needed for the target setup. They could serve other users whose
  hotspot shares Wi-Fi.
- **Online predicate and refresh.**
  - The predicate: present, media active, an address of the family, and a
    default route on the interface.
  - The refresh: re-read on routing-socket or kernel events with a
    debounce, plus a slow poll. Every part is pure Go.
  - `x/net/route` would be a new production use of `golang.org/x/net`;
    the deprecated `syscall` routing helpers avoid that.
- **Dial loop.**
  - For each online interface in order, dial the allowed addresses with the
    family's option.
  - Move to the next interface on local errors (`ENETUNREACH`, local
    `EHOSTUNREACH`, `EADDRNOTAVAIL`, `ENXIO`, `ENETDOWN`) and possibly on
    ICMP unreachable. Inference: errno alone cannot tell a local
    `EHOSTUNREACH` from one caused by ICMP.
  - Do not move on for `ECONNREFUSED`.
  - For timeouts, choose one of three: split the [0003 D7][d3-7] budget
    across interfaces, give each interface its own connect timeout, or add
    a per-interface hold like [0003 D8][d3-8]. A hold triggered by one
    destination's timeout would penalize an interface for a dead
    destination.
- **DNS.** Either document that lookups use the primary interface, with
  the costs in [§2](#2-name-resolution), or add scoped lookups (pure Go
  with `scutil --dns` parsing, or cgo).
- **Connection pools.** Direct transports are keyed by snapshot and client
  class ([0002 D5][d2-5]). They would also need the interface, so that a
  pooled connection on Ethernet A never serves a request pinned to phone
  B.
- **Upstreams and VPNs.** Options:
  - Do not bind upstream dials unless configured.
  - Allow `utunN` as an interface.
  - Warn when a bound dial's destination has an unscoped route through a
    `utun`.
- **Destination classes.** An option is to apply selection only to
  `public` destinations, and to leave `private`, `self`, and `loopback`
  destinations unbound.
- **Logging and counting per interface.**
  - Decision log fields: the chosen interface label, plus the interfaces
    tried and why each was skipped (`offline`, `no_route`, `unreachable`,
    `timeout`).
  - One `warn` line when an interface goes offline and one `info` line when
    it recovers, naming the reason (`detached`, `link_down`, `no_address`,
    `no_route`).
  - A warning when a configured `device` now carries a different MAC
    (replacement takeover), or when a configured MAC is absent.
  - Metrics: a gauge `interface_state{interface,family}`, counters
    `interface_bytes_total{interface,direction}` and
    `interface_dials_total{interface,outcome}`, and the interface as an
    M2 statistics dimension.
  - `cuttle status` lists each interface with its state, since when, and
    its BSD name, but not its MAC.
- **Testing.** `Network.DialContext` needs an interface argument so that
  `internal/testenv` can simulate interfaces at T1. Real binding needs a
  host with several interfaces, so it is a T2 manual check with the user's
  devices.
- **cgo.** It is not needed for binding, state, change detection, or the
  expensive and constrained flags. It is needed only for `SCDynamicStore`
  notifications, `NWPath`, `DNSServiceGetAddrInfo` with an interface, and
  IOKit USB serial numbers. Each of these has a pure-Go substitute or can
  be left out.

[d3-8]: ../design/0003-upstream-chaining-and-exposure.md#d8-unreachable-upstream-hold
[d2-5]: ../design/0002-proxy-core-and-routing.md#d5-actions-and-forwarding

## Open questions

Each item names the smallest check that would settle it. Every check is
read-only toward the system, but some need the user to plug devices in.
None should run from a process that is not exempt from local network
privacy without the user's consent.

1. **Does TCP match the UDP probe?** That is, a bound TCP connect on an
   interface with no address fails locally with no SYN, and one on a
   non-primary interface leaves through that interface. Check: from
   Terminal, which is exempt under [TN3179][tn3179], run the scratch
   program's TCP variant bound to `en0`, to `en0`'s gateway on a closed
   port. Record the error and its timing, and the interface that `nettop`
   or `netstat -n -p tcp` shows for the flow.
2. **iPhone USB identity and flags.** Do a phone's BSD name and MAC stay
   the same across replugging, a different USB port, and a reboot? Do two
   phones get distinct `enN`, distinct MACs, and services `iPhone USB` and
   `iPhone USB 2`? What do `ifconfig -v enN` `type:`, `functional type:`,
   and `eflags` (`EXPENSIVE`) show? Check: attach phone A, record
   `networksetup -listallhardwareports`, `-listnetworkserviceorder`, and
   `ifconfig -v enN`. Repeat after replugging into another port, after a
   reboot, and then with phone B.
3. **Two identical USB Ethernet adapters.** Do names follow MACs when the
   adapters swap ports? Does a third identical adapter take an absent
   adapter's `enN`? Check: the same commands while swapping ports, then
   plug a third adapter while adapter B is absent.
4. **Thunderbolt `Slot N`.** Does the number in `Thunderbolt Ethernet Slot
   N` change when the adapter or dock moves to another Thunderbolt port?
   Check: move it and compare `networksetup -listallhardwareports`.
5. **Event sequence and timing.** What happens on cable removal, adapter
   removal, phone removal, and when the phone stops sharing? How long
   after a link loss do the address and the default route go? Check: run
   the scratch routing-socket and kernel-event monitor with timestamps
   during each event.
6. **A phone with no cellular data.** What does a bound connect see:
   timeout, ICMP, or reset? Check: with consent, turn off cellular data on
   a tethered phone and connect from Terminal to a host the user controls.
7. **Local network privacy for cuttle as a LaunchAgent.** Does the first
   connection to a LAN address raise the alert, what errno does a denial
   return, and does a Homebrew formula's agent need
   `AssociatedBundleIdentifiers`? Check: in M3, in a test user account or a
   VM, install cuttle with `brew services` and send one request to a LAN
   address through a `direct` rule.
8. **Go's dialog warning for its own resolver.** Does a pure-Go resolver
   over a bound UDP socket raise any dialog on macOS 27 with the
   application firewall on? Check: in a VM with the firewall on, resolve
   one name with `PreferGo` and a bound `Resolver.Dial`.
9. **Two phones on one subnet.** Do two iPhones over USB present the same
   subnet and gateway address? Check: attach both and compare
   `netstat -rn -f inet` and `ifconfig` locally, without recording the
   addresses.
10. **Low Data Mode on macOS.** Where is it set, and does it set
    `IFXF_CONSTRAINED` on the interface? Check: turn it on for one network
    and read `ifconfig enN` (` constrained`).
11. **SSID access.** This matters only if a design ever needs the SSID. It
    is not needed for the target setup. Check: Apple documentation or DTS
    guidance for macOS 27. Do not run commands that may prompt.

Commands not run, by choice:

- The TCP connection to the default gateway, because of local network
  privacy (§7).
- `ioreg` and reading `NetworkInterfaces.plist`, which are not on the
  brief's list.
- `ipconfig getsummary`, `networksetup -getairportnetwork`,
  `system_profiler SPAirPortDataType`, and `wdutil`, which might touch the
  SSID or Location Services.

# NetForge Network Model

This document describes the network NetForge builds, the addresses it assigns,
the routing it installs and the two ways traffic can cross a laboratory. Every
claim here is a statement about the code in this repository; where something is
not implemented it says so rather than describing an intention.

## Shape of a laboratory

A laboratory is a laboratory root namespace plus one namespace per node, and
exactly one of those nodes is the router. For a laboratory built from the scope
`10.42.0.0/24` with one client and one server:

```text
laboratory root (user namespace + net namespace, no address)
      │
      ├── nf-client0-<id>   nfc0  10.42.0.2/30    default via 10.42.0.1
      ├── nf-router-<id>    nfr-c0 10.42.0.1/30
      │                     nfr-s0 10.42.0.5/30
      └── nf-server0-<id>   nfs0  10.42.0.6/30    default via 10.42.0.5
```

The names come from `network.NamespaceName(role, id)`
(`internal/network/plan.go:134`): `nf-` plus the role, plus `-<id>` when an
identifier was given. The role is already indexed, so a client is `client0`, a
server is `server0`, and a laboratory with three of each is `nf-client0-<id>` to
`nf-client2-<id>` and `nf-server0-<id>` to `nf-server2-<id>`.

Interface names come from `iface.SideName` and `iface.RouterName`
(`internal/iface/iface.go:99`, `internal/iface/iface.go:110`): `nfc<i>` and
`nfs<i>` inside the node namespaces, `nfr-c<i>` and `nfr-s<i>` inside the router
namespace. Each node owns exactly one data interface; the router owns one per
node plus loopback.

Two laboratories never share a namespace, a veth, an address or a state
directory. The only thing that is shared is the plan, which is a pure function
of the identifier, the scope and the node counts (`network.BuildPlan`,
`internal/network/plan.go:47`): `TestPlanIsDeterministic`
(`internal/network/plan_test.go:9`) asserts it twice over is the same plan.

## Addressing

The scope prefix is split into one point-to-point link per node. The link
prefix length is the scope length plus six, so a `/24` scope produces `/30`
links, and each node consumes four addresses of the scope:

```text
block address   next + 0    the link's network address
router address  next + 1    the router's address on that link
node address    next + 2    the client or server's address
                next + 3    unused
```

Nodes are emitted clients first and then servers, each with its index restarting
at zero (`internal/network/plan.go:129`). For `10.42.0.0/24` with two clients
and two servers:

| node | link | router side | node address | node interface | router interface |
| --- | --- | --- | --- | --- | --- |
| `nf-client0-<id>` | `10.42.0.0/30` | `10.42.0.1/30` | `10.42.0.2/30` | `nfc0` | `nfr-c0` |
| `nf-client1-<id>` | `10.42.0.4/30` | `10.42.0.5/30` | `10.42.0.6/30` | `nfc1` | `nfr-c1` |
| `nf-server0-<id>` | `10.42.0.8/30` | `10.42.0.9/30` | `10.42.0.10/30` | `nfs0` | `nfr-s0` |
| `nf-server1-<id>` | `10.42.0.12/30` | `10.42.0.13/30` | `10.42.0.14/30` | `nfs1` | `nfr-s1` |

The scope is constrained before it is used. It must be a canonical IPv4 prefix
between `/8` and `/30` with no host bits set (`iface.ValidatePrefix`,
`internal/iface/iface.go:82`), and it must leave at least eight host bits
because the plan needs four per link (`internal/network/plan.go:71`). In
practice only `/8` through `/24` produce a plan. The plan is refused rather than
truncated when the scope holds fewer links than the node counts ask for, and
when a laboratory asks for no client, no server, or more than eight of either
(`internal/network/plan.go:56`, `internal/network/plan.go:60`).

MTU is one value for the whole laboratory, applied to both ends of every link
(`internal/network/lab.go:285`, `internal/network/lab.go:294`). Zero means
1500 (`iface.DefaultMTU`), and the accepted range is 68 to 65535. Loopback MTU is
never touched: only `lo` is raised.

## Routing

Every node gets exactly one route, installed at build time:

```text
ip route replace default via <router address on this link> dev <its interface>
```

(`iface.AddDefaultRoute`, `internal/iface/iface.go:220`, called from
`internal/network/lab.go:300`). The router namespace gets no static route at
all. In kernel mode its forwarding table is only the connected `/30` routes the
kernel derives from the addresses, and in software mode those connected routes
are irrelevant because `ip_forward` is `0`.

There is therefore no route between two nodes other than through the router: a
client has a default route and nothing else, so every client-to-client and
client-to-server packet crosses the router. `ip_forward` is written once, in the
router namespace only (`internal/network/lab.go:241`); no other sysctl is
written anywhere, and the write path refuses anything outside `/proc/sys/net`
on both sides of the process boundary (`internal/iface/iface.go:19`,
`internal/namespace/agent.go:192`).

Two consequences are worth stating because both are observable:

- A cross-lab round trip reports `ttl=63`, which is one router crossed
  (`tests/integration/network_test.go:34`).
- `recovery`'s `restore_route` action can only restore the laboratory's own
  network prefix through a node's gateway, because that is the only route the
  model has (`internal/recovery/lab.go:109`).

## Kernel forwarding and the software router

`network.Routing` selects which of the two carries the traffic
(`internal/network/lab.go:46`):

| mode | who forwards | `ip_forward` in the router namespace |
| --- | --- | --- |
| `RoutingKernel` (default, `""`) | the kernel | `1` |
| `RoutingSoftware` (`"software"`) | `internal/router`, in its own process | `0` |

In software mode the router process starts only after `ip_forward` has been set
to `0` (`internal/network/router.go:43`), and the build step never sets it to
`1`, so the kernel never forwards even transiently. That is what makes
`ip_forward=0` in the router namespace a usable proof that a forwarded byte came
from NetForge rather than from the kernel.

`internal/router` is a userspace router that reads and writes frames on
`AF_PACKET` sockets (`internal/router/forward.go:43`), one socket per configured
device, resolved by interface name and refused unless the device has a hardware
address (`internal/router/router.go:249`). It is a separate process
(`netforge netforge-router '<json>'`, dispatched at `internal/router/router.go:368`)
so the laboratory agent stays a request and response channel and never carries a
data plane. Its lifetime is the agent's lifetime: it blocks reading standard
input and stops when the agent closes the pipe (`internal/router/router.go:301`).

A frame is handled in this order (`internal/router/forward.go:231`):

1. parse Ethernet; a frame that does not parse is counted as malformed;
2. ARP is answered or learned, an ARP reply is dropped;
3. anything that is not IPv4 is ignored, as are packets for the router itself,
   broadcast and multicast;
4. a packet whose time to live is one is dropped and answered with ICMP time
   exceeded;
5. a destination with no route is dropped and answered with ICMP destination
   unreachable;
6. an unresolved next hop is probed with ARP and the packet is dropped, because
   the router has nowhere to send it yet;
7. the time to live is decremented, the transport checksum is recomputed, and
   the frame is rebuilt on the egress interface;
8. a frame larger than the egress MTU is answered with ICMP fragmentation
   needed and the MTU that applies;
9. otherwise the frame is sent.

Two properties of that order were found by measurement rather than by reading
the code, and both are documented in [ARCHITECTURE.md](ARCHITECTURE.md):

- The transport checksum is recomputed rather than copied
  (`internal/router/forward.go:335`). A packet socket shows a sender's
  unsegmented packet, so a stack that offloads the transport sum has written
  only the seed; forwarding those bytes hands the far end a packet it rejects.
- A frame that does not fit the next link is answered rather than dropped
  (`internal/router/forward.go:350`). The socket shows the sender's unsegmented
  packet, so the router can be handed something the egress device cannot
  accept; answering lets the sender send something smaller instead of learning
  the limit by silence.

The routing table, the neighbour cache and the forwarding queue are bounded:
1024 routes, 256 neighbours and a 256-frame queue by default, each refused at
the configuration boundary (`internal/router/table.go:12`,
`internal/router/arp.go:24`, `internal/router/queue.go:10`). Counters are
published as JSON through a temporary file that is renamed into place, and a
reader asks for a refresh with `SIGUSR1`
(`internal/router/counters.go:31`, `internal/router/router.go:315`). The router
exports aggregate counts only: it cannot list its routes, its neighbours or its
queue.

The mode is not selectable from the command line. `RoutingSoftware` is reachable
from Go and from the integration suite
(`tests/integration/networking_audit_test.go:46`); `netforge experiment run`
always builds a kernel-routed laboratory, because the CLI has no flag for it
(`internal/cli/experiment.go:286`).

## Isolation

Nothing NetForge creates is visible to the host's network configuration. The
laboratory agent re-executes the binary with `CLONE_NEWUSER | CLONE_NEWNET` and
a single-entry uid and gid mapping
(`internal/namespace/lab.go:94`), so the agent is uid 0 inside a user namespace
it created and holds nothing on the host. Every child namespace is created by
`clone(CLONE_NEWNET)` from that agent and is held open by a process that blocks
on `SIGTERM` (`internal/namespace/agent.go:211`), so the kernel destroys a
namespace when its holder dies.

veth pairs are the only kernel objects NetForge creates, and they are created
inside the laboratory root namespace (`internal/iface/iface.go:157`) with both
halves then moved by PID into the namespaces that own them. NetForge never calls
`ip netns add`, so a laboratory leaves no entry in `/var/run/netns` and no
`ip netns list` output. NetForge never runs as root and never requests a
capability: it has no `capset`, `setuid` or ambient-capability call anywhere.

Scope is enforced before the first kernel call. `network.Create` refuses a plan
whose router or node namespace is not in the granted set, and a planned address
outside the scope network, before a namespace exists
(`internal/network/lab.go:198`). `internal/chaos` and `internal/probe` enforce
the same scope independently, and both require a laboratory namespace to carry
the `nf-` prefix (`internal/chaos/limits.go`, `internal/probe/scope.go:59`).

`TestHostIsNeverModified` (`tests/integration/phase2_test.go:346`) compares
`ip -o link show`, `ip -o route show` and `/proc/sys/net/ipv4/ip_forward` before
and after a laboratory exists, and `TestSoftwareRouterLeavesTheHostAlone`
compares them around a laboratory whose traffic crosses `internal/router`.

## Lifetime and teardown

A laboratory owns its own state directory, created through `reconcile.Create` and
registered first in the cleanup stack so it is removed last
(`internal/network/lab.go:131`). It is created through the reconciler rather than
`os.MkdirTemp` for one reason: a run killed with `SIGKILL` cannot remove it, so it
carries an ownership marker that lets a later run prove the directory is NetForge's
and take it back. The marker records the directory's kind, the laboratory's
identifier, the owning process and the creation time, and is written atomically
(`internal/reconcile/reconcile.go:221`). The software router has one of its own,
of kind `router` (`internal/network/router.go:52`).

Teardown runs in reverse registration order:
the router is stopped first (`internal/network/router.go:156`), then the fault
engine, then the services, then the namespaces, then the state directory. Each
step is bounded — the router and a service get two seconds before `SIGKILL`, a
namespace holder is given up to two seconds to become a zombie, and the whole
stack gets five (`internal/network/lab.go:170`). `Lab.Destroy` is idempotent
(`internal/network/lab.go:397`).

Cleanup runs on a context that has been detached from cancellation
(`internal/cleanup/cleanup.go:106`), because tearing down with a cancelled
context is how a laboratory would be left behind
(`internal/experiment/controller.go:258`).

## Inspecting a laboratory by hand

Because namespaces are addressed by PID rather than by name, inspection starts
from the process table:

```console
$ ps -eo pid,args | grep -e netforge-namespace-holder -e netforge-lab-agent -e netforge-router
$ ls -l /proc/<pid>/ns/net            # net:[4026532xxx] is the namespace handle
$ nsenter -t <pid> -n ip -o addr show
$ nsenter -t <pid> -n ip route show
$ nsenter -t <pid> -n cat /proc/sys/net/ipv4/ip_forward
$ nsenter -t <pid> -n ip neigh show
$ nsenter -t <pid> -n tc qdisc show dev <dev>
```

Inside a namespace NetForge runs exactly these helpers, with `PATH` fixed to
`/usr/sbin:/usr/bin:/sbin:/bin` and no shell
(`internal/namespace/agent.go:111`): `ip link`, `ip addr`, `ip route`,
`ip route get`, `cat`, `tc qdisc`, `ping`, `arping`.

A healthy laboratory looks like this: each node namespace has loopback plus one
interface whose address is `network + 2` of its block and whose MTU is the plan's
MTU, plus a `default via` pointing at `network + 1`; the router namespace has
one interface per node at `network + 1`; `ip_forward` is `1` in kernel mode and
`0` in software mode; and a ping across the laboratory reports no loss at
`ttl=63`. With the software router, `<tmp>/netforge-router-*/router.json` reports
`state: running`, a `revision` that increases, `routes` equal to the number of
links, `neighbors` above zero after the first packet, and `unresolved` plus
`arpProbes` at least one, because the first packet of a flow is the one that
triggers address resolution.

To prove the host is untouched, compare the same four views the test suite
compares (`tests/integration/networking_audit_test.go:464`):

```console
$ ip -o link show
$ ip -o route show
$ ip netns list
$ tc qdisc show
```

## Not implemented

Stated rather than left to be discovered:

- IPv6, at any layer. The scope reader refuses an IPv6 network as unimplemented
  (`internal/config/config.go:86`).
- DHCP, NAT, bridges, VLANs, tunnels, tap devices, and any uplink to a real
  network. A laboratory is a closed set of point-to-point links.
- More than one router, and any routing mode other than kernel forwarding or
  `internal/router`. The command line offers both and refuses anything else rather
  than falling back to the kernel.
- A CLI flag for the MTU, or for node counts beyond `--clients` and `--servers`.
- Named namespaces, so `ip netns` shows nothing and `ip netns exec` cannot reach
  a laboratory.
- A command that creates, inspects or destroys a laboratory on its own. A
  namespace dies with its holder, and `netforge reconcile` reclaims the state
  directories a `SIGKILL`ed run leaves, but it never builds or inspects a
  laboratory: it removes files and touches nothing else. See
  [OPERATIONS.md](OPERATIONS.md).
- Any sysctl other than `ipv4.ip_forward`.
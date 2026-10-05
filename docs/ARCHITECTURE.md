# NetForge Architecture

This document describes the structure that exists in the repository today.
Later phases add the packages listed in `todo.md`; each is created when the
phase that needs it starts, and this document is updated in the same change.

## Layers

```text
cmd/netforge            process entry point: signals, exit status, wiring only
      │
internal/cli           command parsing, dispatch, help, exit codes, logging setup
      │
internal/config        configuration model, strict decoding, validation
internal/network       topology planning, scope enforcement, lab lifetime, routes
      │                fault engine ownership, measurement
      ├─── internal/chaos     fault model, tc, netem, engine, expiry
      ├─── internal/probe     bounded echo measurement, report parsing
      │
internal/namespace     namespace lifecycle, laboratory agent, setns
internal/iface         veth, address, route and inspection operations
      │
internal/router        software router: routing table, ARP, forwarding, counters
      │
internal/packet        Ethernet, IPv4, ICMP, UDP, TCP and DNS codecs
internal/apperr         error classification shared by every layer
internal/cleanup        ordered, idempotent cleanup stack
internal/logging        structured logging
```

The dependency direction is strictly downward. `internal/cli` depends on
`internal/config`, `internal/cleanup`, `internal/logging` and `internal/apperr`.
`internal/network` depends on `internal/namespace`, `internal/iface`,
`internal/cleanup`, `internal/chaos`, `internal/probe` and `internal/apperr`,
and it is the only package that puts them together: `internal/chaos`,
`internal/probe` and `internal/investigator` are siblings and none of them
depends on `internal/network`. The investigator reaches the network only
through two interfaces it declares itself, a `Prober` and a `RouteReader`,
which `internal/network` satisfies; that is what lets the reasoning be tested
against a scripted wire without a namespace and run against a real one without
changing. `internal/service` holds the endpoints a probe measures and is
started by `internal/network` alongside the fault engine. `internal/namespace` and
`internal/iface` do not depend on `internal/network`, so the plan cannot be
built by the code that also applies it. `internal/config` depends on
`internal/chaos` for the fault model and the shared `Spec.Validate`, so a
fault is validated by the same code that applies it. Nothing depends on `internal/cli`
except `cmd/netforge`. There are no import cycles; the compiler rejects one,
including the transitive initialisation cycle that a package-level function
variable would otherwise create.

## The process boundary

`internal/namespace` is the only package allowed to start a process. It is the
single place where `os/exec` and `syscall` appear, and
`tests/security/source_test.go` enforces that: the shell is banned in every
file, and a file that runs a helper command must be listed in the boundary.

`internal/namespace` re-executes the current binary as a laboratory agent with
`CLONE_NEWUSER` and `CLONE_NEWNET`. The agent owns the namespaces, so the
parent process holds no ambient capability at all. Requests arrive as JSON
lines on the agent's standard input, which the agent verifies is a pipe before
it does anything, so an agent can never be attached to a terminal by accident.
`setns` binds a locked OS thread to a target namespace for the lifetime of one
command, and the thread returns to its own namespace afterwards.

`internal/iface` runs `ip` as the bare name `ip`, resolved through the fixed
`PATH` every child of the agent is given (`/usr/sbin:/usr/bin:/sbin:/bin`), and
never through a shell, so nothing in a document can become part of a command
line. `Space.Write` refuses any path that is not under `/proc/sys/net`, so the
only host-visible state an experiment can change is forwarding inside a namespace
it owns. The path is cleaned and re-checked before the request is sent, so a
crafted path cannot escape that prefix with `..`.

## The software router

Phase 3 adds a router that lives in the router namespace and forwards real
frames without the kernel. It runs as its own process so that the laboratory
agent stays a request and response channel and never carries a data plane.

```text
netforge netforge-router '<json config>'
      │
internal/router.Dispatch   mode check, configuration, signals
      └── Router.Serve      devices, routing table, neighbour cache, readers, workers
            ├── one reader goroutine per interface
            ├── two workers draining one bounded queue
            └── stats file, refreshed on SIGUSR1
```

The parent starts the process through the agent `daemon` operation, which keeps
the write end of a control pipe open. The router reads its standard input and
stops when the pipe is closed, so the lifetime of the data plane is the lifetime
of the laboratory agent: no orphan router can outlive its namespaces. The
daemon's standard output and error are not inherited, because the agent speaks
JSON on its own standard output.

Software routing is selected per laboratory, and from the command line with
`netforge experiment run --routing software`. In that mode `ip_forward` is set
to `0` in the router namespace before the router starts, so the kernel never
forwards a packet and every forwarded byte comes from `internal/router`. Kernel
forwarding is the default, and a mode the router does not implement is refused
rather than substituted: a silently ignored `--routing software` would produce a
report describing a laboratory the kernel forwarded. The run's report carries
`ip_forward` and the router's counters, read once immediately before the
laboratory is taken apart, so a reader can attribute forwarded traffic from
outside the process.

A frame is handled in this order: parse Ethernet, answer or learn ARP, drop
traffic that is not IPv4, drop packets for the router itself, drop broadcast and
multicast, expire the packet when the time to live is one, look the destination
up in the routing table, resolve the next hop through ARP, decrement the time to
live, recompute the transport checksum and rebuild the frame on the egress
interface. Anything that cannot be parsed is counted and dropped; the reader
keeps running.

Two properties of that order are worth stating, because both were found by
measurement rather than by reading the code.

The transport checksum is recomputed rather than copied. A packet socket shows
the bytes as they are on the wire, and a sender whose network stack offloads the
transport sum writes only the seed of the checksum into those bytes and leaves
the rest to the receiving kernel. Forwarding those bytes unchanged hands the far
end a packet it rejects as corrupt: a connection that is refused and never
answered, which on the wire is indistinguishable from a path that is down. ICMP
computes its own sum and crosses either way, so an echo test never noticed and a
handshake failed. `packet.RefreshTransportChecksum` recomputes ICMP, UDP and TCP
sums over the packet as it will be sent; a UDP datagram whose checksum field is
zero said the sender omitted it and is left alone.

A packet larger than the link it would leave by is answered, not dropped. The
same socket sees a sender's unsegmented packet rather than the pieces the wire
would carry, so the router can be handed something its egress device cannot
accept. It compares the rebuilt frame against the MTU the configuration gave the
device and, when the frame does not fit, counts it and answers the sender with
ICMP destination unreachable, fragmentation needed, carrying the MTU that
applies. That is what lets the sender send a smaller packet instead of learning
the limit by silence, and it keeps a size limit from becoming a silent outage.

The routing table, the neighbour cache and the forwarding queue are all
bounded, and the configuration is refused when it asks for more than the
process allows. Counters and the queue depth are published as JSON through a
file that is written to a temporary name and renamed into place, so a reader
never observes a half written document.

## Invariants

1. The CLI contains no networking logic and no privileged operation. It parses
   arguments, dispatches to a command, and maps an error to an exit status.
2. Core packages never import the CLI or the process entry point, and only
   `internal/namespace` imports `os/exec` or `syscall`. No shell is ever run.
3. Configuration parsing performs no side effects. It does not read the network,
   write files, or depend on wall-clock time or process state.
4. The same configuration document always produces the same
   `experiment_id`, because the identifier is derived from the validated
   configuration content.
5. Every long running operation accepts a `context.Context` and observes
   cancellation.
6. Every resource that acquires something releases it through the cleanup
   stack, which runs exactly once, in reverse order, on a live context and
   within a timeout.
7. Errors are classified. `internal/apperr` defines the classification and the
   CLI maps each classification to a stable exit status.
8. Unknown state is never treated as permission. An operation whose scope,
   authorization or ownership cannot be established does not run.
9. The host network is never modified. Every namespace, link, address and route
   belongs to a namespace NetForge created, and an experiment may only tighten
   its granted scope, never widen it.

## Error model

`internal/apperr` provides a `Kind`, an operation, a message and a wrapped
cause. Kinds are stable and machine-checkable with `errors.Is`:

| Kind | Typical source | Exit status |
| --- | --- | --- |
| `invalid_argument` | bad CLI argument, bad CIDR, bad name | 2 |
| `unsupported` | unimplemented option such as IPv6 scope | 2 |
| `invalid_config` | malformed or undecodable configuration | 3 |
| `validation` | configuration decoded but semantically invalid | 3 |
| `limit_exceeded` | request above a supported ceiling | 4 |
| `not_found`, `io`, `permission_denied` | filesystem and privilege failures | 5 |
| `timeout` | deadline exceeded | 124 |
| `canceled` | SIGINT or SIGTERM | 130 |
| `conflict`, `internal` | lifecycle and unexpected failures | 1 |

Validation failures are aggregated: a single load reports every problem it
found, in a fixed field order, and the error preserves each individual cause.

## Lifecycle and shutdown

```text
main
  └── cli.NotifySignals        goroutine: signal -> context cancel
        └── App.Run
              └── cleanup.Stack.Guard    command execution plus exactly-once cleanup
                    └── command
```

`NotifySignals` starts one goroutine. It records the first signal, cancels the
context, and stops signal delivery so that a second signal terminates the
process with the default disposition. `Stop` is idempotent and waits for the
goroutine to finish, which makes process shutdown deterministic.

`cleanup.Stack.Guard` runs the command and then the cleanup phase, joining the
command error with the cleanup errors. A recovered panic becomes an `internal`
error. The cleanup phase runs on `context.WithoutCancel`, so a canceled caller
still releases its resources, and it is bounded so it cannot hang the process.

A laboratory's processes are tied to pipes rather than to signals. The router
already was: it reads its standard input and stops when the agent closes the
pipe, which is why no orphan router can outlive its namespaces. The namespace
holders and the laboratory services now work the same way, and that is what makes
a hard kill release the namespaces rather than leave a holder holding one open.
A holder that waited only for a signal was an orphan after a `SIGKILL` of the
agent, because nothing sends a signal to a process that is already gone, and a
namespace NetForge could not see was a namespace NetForge had not released.

## Reconciliation

```text
netforge reconcile list
netforge reconcile reclaim --id <name> --authorize -why "<reason>"
      │
internal/reconcile.Scan       read the state root, classify every entry
      └── ReadMarker          a directory is a candidate only with a valid marker
internal/reconcile.Authorize  consent + reason + name check, before any syscall
      └── Reclaim             re-verify the marker, then remove one directory
```

`internal/reconcile` exists because a `SIGKILL`ed run cannot remove its own state
directories: it releases its namespaces with its processes, but the files holding
service status and router counters survive. The state root is shared — it is the
system temporary directory — so the package's whole safety argument is ownership.
NetForge writes a marker when it creates a directory, and a directory without a
valid marker is listed and left alone. Removal is confined to the state root, is
bounded, is refused while the owning process runs, and requires an authorization
that only `Authorize` can produce. It removes files and nothing else, so it needs
no privileges and cannot reach host networking.

## Faults and measurement

`internal/chaos` owns the kernel's view of a fault and `internal/probe` owns
the measurement, and neither knows about the other. `internal/network` builds
both, hands the probe a runner scoped to the laboratory, and puts the engine
under the laboratory's cleanup stack so that faults leave the kernel before the
namespaces they live in disappear.

A fault is a request, not a fact. `Apply` validates it against the
laboratory's scope and ceilings, refuses a second fault on a device that
already has one, runs `tc`, and then reads the qdisc back with
`tc -j`. A fault is only reported as active once the kernel's own numbers
match the request, so a configuration value is never reported as a
measurement. `ApplyAll` rolls back every fault it applied if any of them is
refused, and a laboratory that cannot be built completely leaves the kernel as
it found it. A fault with a duration is retired by the engine's own watcher,
and `Remove`, `RemoveAll` and `Close` are idempotent.

Measurement is the other half of the evidence. The probe runs a bounded echo
from a namespace, reads the report the tool prints, and derives loss, round
trip times, goodput, duplicate replies, out-of-order replies and errors from
it. A report it cannot parse is an error, never a zero, so a measurement
cannot pass by being empty.

The Phase 4 tests measure every fault kind on real traffic. Each case measures
the same path before the fault, with the traffic pattern the fault needs to be
visible: a bandwidth ceiling is only visible when the sender offers more than
the ceiling, and packets can only be reordered while several are in flight. No
test asserts a configured value against itself.

## Probing

`internal/probe` is the only code that opens a socket to a target. One
`probe.Request` names a protocol, a namespace, a target and the bounds of the
measurement, and one `probe.Result` reports what came back, including the
measured loss, the round trip and the classified failure. The request is
validated before anything is sent: a protocol needs the fields it uses, the
namespace has to belong to the experiment, and the target has to be inside the
network the laboratory declared. A refused request is never sent.

Two measurements exist and they are not the same thing. An echo is measured by
the kernel's own tool inside the namespace and the report it prints is parsed;
a report that cannot be parsed is an error, never a zero, so a measurement
cannot pass by being empty. The other five protocols are measured by
`netforge-probe`, a mode of the same binary, re-executed in the target
namespace by the agent and handed the request on its standard input. The answer
comes back on standard output as one JSON line, bounded by `MaxResponseSize`.

The engine is the boundary that matters. It holds the concurrency limit, the
timeout, the payload ceiling and the size of the result store, and the agent is
told the same numbers so that a limit cannot be enforced in one place and
forgotten in the other. Every result the engine keeps is bounded, and the store
drops the oldest rather than growing. A failure to *run* the measurement is
returned as an error; a failure to *get an answer* is a classified result.
Conflating the two would let a laboratory problem be reported as a network
fault, so they are different types all the way through.

## Services

`internal/service` provides the far end of a probe, because a probe against a
closed port measures a refusal rather than a service. A specification may ask
for any combination of TCP, UDP, DNS, TLS and HTTP endpoints, and a request
without a single endpoint is refused. Each endpoint is a real listener inside
the laboratory's own server namespace: the HTTP one is a real `http.Server`
behind a listener with a bounded number of connection slots, and a request is
served by a handler that owns the connection for as long as it lives. The
status report names the protocol and address of everything that is really
bound, along with the process, so a test can compare what was asked for against
what is listening.

A service is started after the links exist and before the faults are applied,
so a probe never races the fault it is meant to observe, and it is stopped
before the namespaces disappear. Its work directory belongs to one laboratory,
so two laboratories running at once cannot collide on a socket or a file.

## Diagnosis

`internal/investigator` turns a failed measurement into a diagnosis, and the
order of the pipeline is the design. An `Observation` says what was seen
failing and nothing more; it is validated before any probe runs on its behalf.
The investigator then reads the kernel's routing table for the target, measures
the failing layer and every layer below it lowest first, and measures the
failing layer a second time to tell a steady fault from a moment. Only then does
it generate explanations, correlate them against what was measured, and
classify.

The classification is the part that has to be honest.

```text
CONFIRMED   the blamed layer failed measurably, the layers below it answered,
            the answer was specific, and it came back the same way twice
PROBABLE    the same, measured once
POSSIBLE    the evidence narrows the field without choosing, such as a timeout
            where a slow server and a broken one cannot be told apart
UNKNOWN     the evidence does not separate the explanations at all
```

Three rules hold that line. The blamed layer is the *lowest* layer that failed,
not the layer the caller blamed, so a TLS handshake refused by a closed port is
diagnosed at the transport and the report says the observed layer was never
reached. A measurement that was not taken is not a measurement that found
nothing, so a refused probe is recorded as attempted and not measured, and the
report concludes nothing from it. And every classification is passed through
`clamp`, which lowers it to the level of the hypothesis the report is concluded
from and drops it to UNKNOWN when that conclusion cites nothing. A report also
carries the alternatives that were considered and refuted, with the measurement
that refuted each, so a reader can see what was dropped and why; only the
concluding hypothesis may raise the level.

Every statement in a report cites evidence by index, and the evidence is the
measurement. A configured fault value is not a measurement and never appears as
one: the percentage a report quotes is the percentage a probe measured, which is
why a laboratory asking for fifty percent loss and a laboratory observing ten
produce different reports from identical reasoning.

## Concurrency

The only goroutine in the shipped binary is the signal watcher. Commands are
synchronous. Nothing spawns a background worker, and no worker exists without an
owner, a startup condition and a shutdown condition. Tests assert the
goroutine count does not grow.

## Testing strategy

| Suite | Location | What it proves |
| --- | --- | --- |
| Unit tests | `internal/**/*_test.go` | pure behaviour, error classification, validation, lifecycle, address planning |
| Fuzz tests | `internal/config/fuzz_test.go` | the configuration parser and the percentage parser never panic and never accept an invalid document |
| Integration tests | `tests/integration/` | the built binary behaves as documented, exits with the documented status, is unprivileged, parsing has no side effects, and real isolated traffic crosses a real router |
| Security invariants | `tests/security/` | no shell anywhere, no process execution outside the process boundary, the packet layer is pure and bounded, no secrets, no embedded targets, required documentation present |
| Packet fuzzing | `internal/packet/fuzz_test.go` | the seven packet fuzzers never panic and never accept malformed input |
| Router fuzzing | `internal/router/fuzz_test.go` | the forwarding decision and the ICMP error builder never panic on any frame |

Linux networking behaviour is not mocked. The Phase 2 tests exercise real
namespaces, real veth pairs and real traffic, and the evidence includes
namespace, interface and route inspection together with a comparison of host
links, host routes and host forwarding before and after.

The Phase 3 tests keep that rule: they send ICMP traffic between two real
namespaces through the userspace router with kernel forwarding switched off in
the router namespace, and they check the time to live, the counters and the
neighbour cache that the router reports afterwards.

The Phase 4 tests keep it as well: every fault kind is injected into a real
laboratory, measured with a real probe, and compared against the kernel's own
record of the qdisc. Removing the fault has to restore the path, a fault
outside the declared scope has to be refused, and a laboratory that cannot
honour its declared faults has to fail and leave nothing behind.

The Phase 5 tests keep it too: all six protocols are measured from a client
namespace against a service the laboratory itself started, and the far end of
every probe is a real listener rather than a fixture.

The Phase 6 tests keep it as well, and they are the reason the rules above
exist. Each diagnosis is made against a real namespace and a real listener or a
real netem fault, and the tests assert the *honest* reading rather than a
convenient one: a refused handshake has to be diagnosed at the transport, a
healthy service has to stay undiagnosed, a laboratory asking for fifty percent
loss must not have that number appear in the report, and a target outside the
laboratory has to produce no measurement and no conclusion. The reasoning itself
is tested against a scripted wire, so the cases that cannot be staged with real
faults, such as a failure that does not reproduce, are still covered.

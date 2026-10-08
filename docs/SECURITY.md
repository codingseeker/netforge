# NetForge Security

This document describes the security controls that exist in the code in this
repository today, and the controls that do not exist yet. It is written to
support the claim "the experiment controller validates namespace and CIDR
scope for supported operations" and to avoid claims such as "secure" or
"production-ready", which are not supported by evidence.

NetForge does touch the network, but only inside namespaces it created
itself. The packet layer is pure. The privileged boundary is `internal/iface`
and `internal/namespace`, and it is confined by a security test.

## Implemented controls

### 1. No shell execution

NetForge contains no shell invocation. `tests/security/source_test.go` fails
the build if any shipped source references `sh`, `bash`, `zsh`, `dash`, `ksh`,
`sh -c`, `syscall.Exec`, `syscall.ForkExec`, `os.StartProcess` or `/bin/sh`.
The ban is absolute: it applies to the process boundary as much as to the rest
of the tree.

Command execution exists, in one place. `internal/namespace` is the only package
that may import `os/exec`, and it is where a helper command is run. The narrower
`syscall` import is declared per file rather than confined to that package,
because a signal handler needs it to name a signal and a `setns` needs it to name
a namespace, neither of which starts a process. Both sets are declared as data in
the security test, which fails in two directions: a listed file that stops using
what it was listed for, and a file that starts importing something it was not
listed for. A separate test requires every file that runs a helper command to
live under `internal/namespace`.

NetForge drives `ip` with `exec.Command` and separate arguments, never a shell
string. Arguments are validated first: interface names against the kernel's
character set and its 15 character limit, addresses and prefixes against
`netip`, and routes against both the prefix and the gateway. Exit status and
stderr are checked and surfaced. The user and network namespace pair is created
by the kernel through `CLONE_NEWUSER`, not by `ip`, so no privilege is needed
to start it.

### 2. Host networking is untouched

`TestHostIsNeverModified` snapshots the host's `ip -o link show`, `ip -o route
show` and `/proc/sys/net/ipv4/ip_forward`, creates a laboratory, carries real
traffic across it, destroys the laboratory, and compares the snapshots byte for
byte. `TestPartialCreationIsCleanedUp` and
`TestInterruptedSetupLeavesNothingBehind` do the same around a refused and an
interrupted setup.

The mechanisms behind that evidence:

- The laboratory root is a new network namespace created together with a new
  user namespace, so the process holds no ambient capability in the host's
  namespace.
- Every namespace is held open by a dedicated process, and the kernel destroys a
  namespace when the last process inside it exits. Each holder watches a pipe
  whose write end the owning agent holds, so the end of the pipe — and therefore
  the exit of the holder — arrives however the agent dies, including by
  `SIGKILL`, and not only when it is polite enough to send a signal.
- A veth pair is created in the laboratory root and moved. Its host-visible
  half never leaves the root namespace, and the root namespace is itself
  unlinked from the host.
- `Space.Write` and `Space.Read` accept only paths under `/proc/sys/net`, so
  the single host-shaped knob an experiment needs, forwarding, is addressed by
  name inside a namespace. `/proc/sys/net` follows the namespace of the task
  that opens it, so writing it after `setns` changes only the laboratory.
- No command reaches the host namespace: the agent has no path to it, and the
  parent process never enters any laboratory.

### 3. No privileges required

NetForge never runs as root and never elevates. Nothing in the code calls
`seteuid`, `setuid`, `os/user` or `syscall.Setuid`, and the `os/user` import is
covered by the boundary test. The integration suite asserts that `version`,
`help`, `limits show` and `config validate` succeed without privileges, and the
network suites run the whole topology as the invoking user.

The capabilities that make namespaces work are namespaced capabilities: the
process is root only inside a user namespace it created, and that user namespace
owns every namespace NetForge touches. Where the kernel does not grant
unprivileged user namespaces, the network suites skip rather than pretend.

The `internal/cli/signals.go` boundary is the only place where the process
observes privileged kernel interfaces, and it only maps `SIGINT` and `SIGTERM`
to context cancellation.

### 4. Strict configuration parsing

Configuration is data. The loader:

- decodes with unknown fields rejected (`yaml.Decoder.KnownFields(true)`);
- rejects a document that is not a mapping;
- rejects duplicate keys;
- rejects more than one YAML document;
- requires explicit string types for `max_duration` and `max_bandwidth`, so
  `600000000000` is rejected rather than reinterpreted as nanoseconds;
- rejects negative durations;
- rejects fractional and unit-less bandwidths;
- rejects canonicalisation of CIDR input: `10.42.0.5/16` is rejected rather
  than masked to `10.42.0.0/16`;
- rejects `0.0.0.0/0` as a default route;
- rejects IPv6 scope as unimplemented rather than accepting a value that the
  rest of the system cannot honour.

Parsing has no side effects. The test suite verifies that the contents of the
configuration directory are unchanged after successful and failed loads, and
that no file is created anywhere.

### 5. Bounded input

| Input | Bound |
| --- | --- |
| configuration document | 1 MiB, checked before parsing and while reading a stream |
| configuration path | 4096 bytes, no NUL bytes, must be a regular file |
| experiment name | 63 characters, lowercase letters, digits, `-` |
| namespace name | 63 characters, same character set, must be unique |
| namespaces in scope | 64 |
| scope network | IPv4 CIDR, prefix length 1 to 32 |

`io.ReadAll` appears only behind an `io.LimitReader`; the invariant is enforced
by `TestIOReadsAreBounded`.

### 6. Bounded resource requests

Every experiment limit is bounded. A configuration that requests more than the
supported ceiling is rejected before any resource is created. The ceilings and
defaults are compiled constants and are printed by `netforge limits show`:

| Limit | Default | Ceiling |
| --- | --- | --- |
| `max_namespaces` | 10 | 64 |
| `max_interfaces` | 30 | 512 |
| `max_duration` | 10m | 24h |
| `max_bandwidth` | 1Gbps | 100Gbps |
| `max_packet_rate` | 100000 | 10000000 |
| `max_concurrent_probes` | 100 | 10000 |

A scope that declares more namespaces than `limits.max_namespaces` is rejected.

### 7. No secrets

The configuration model has no field that can carry a credential. A security
test fails if a `password`, `secret`, `token`, `credential`, `api_key`,
`private_key` or `authorization` field appears in the configuration package.
NetForge needs no secret to run.

### 8. No embedded external targets

The shipped sources contain no hard coded URL or public IP address; probe and
experiment targets come from validated configuration. Enforced by
`TestNoEmbeddedNetworkTargets`.

### 9. Fail closed on lifecycle

- Long running work accepts `context.Context` and is cancelled by `SIGINT` and
  `SIGTERM`.
- The first signal cancels the context and restores the default signal
  behaviour, so a second signal terminates the process immediately.
- Cleanup runs in last-registered-first-claimed order, exactly once, and
  continues after a failing or panicking cleanup action.
- Cleanup runs on a live context derived with `context.WithoutCancel`, so
  cancellation of the caller does not skip cleanup, and it is bounded by a
  timeout so cleanup cannot hang.
- Cleanup runs on success, failure, deadline, cancellation and recoverable
  panic. A recovered panic becomes an `internal` error rather than a silent
  success.

### 10. Scope is enforced before anything is created

`internal/network` builds the whole plan before it touches the kernel. Every
planned address must lie inside `scope.network`, every planned namespace must
be named in `scope.namespaces`, and the namespace and interface counts must fit
`limits.max_namespaces` and `limits.max_interfaces`. A scope that names fewer
namespaces than the plan needs is refused, an empty scope grants nothing, and a
scope that grants more than the plan needs is accepted because a grant is a
ceiling. `TestScopeIsEnforced` and `TestEnforceScopeNeedsEveryNamespace` cover
both the granted and the refused direction, including removing one name at a
time.

### 11. The agent cannot be reached from outside

The agent is the same binary as its caller, so it is reachable only through a
pipe. `AgentMain` refuses to run unless its standard input is a pipe or a
socket, so an agent can never be started by hand and left reading a terminal.
The only write the agent accepts is a value for a path under `/proc/sys/net`,
and the only commands it runs are the ones the parent sends. Namespace holders
are started as a distinct mode that holds a namespace open and never inherits
the parent's request channel; they read a pipe the agent holds the write end of,
and stop when it closes, which is what makes them release their namespace on a
`SIGKILL` as well as on a signal.

### 12. No panics in the control plane

`internal/` contains no `panic(`, no `log.Fatal`, and no `os.Exit`. Only
`cmd/netforge/main.go` maps an error to a process exit status. Enforced by
`TestNoPanicsInCore`.

### 13. The software router forwards nothing outside the experiment

`internal/router` opens an `AF_PACKET` socket per interface with `SOCK_RAW` and
binds it to that interface, so a frame can never be sent through a device the
configuration does not name. The socket is opened inside the router namespace,
which the process only enters through the agent; the parent process keeps no
capability at all.

A destination is forwarded only when the routing table holds a route for it.
A route is refused unless its prefix is IPv4, masked, inside the experiment
scope, and no wider than that scope, and its next hop, when present, is inside
the scope as well. Source addresses are not consulted for authorisation, which
is deliberate: the laboratory is the boundary, not the sender.

A raw socket inside a network namespace that NetForge created is the only
privilege the data plane needs, and it exists only while the laboratory exists.

The router never forwards a packet it has not understood. A frame is rebuilt
from parsed fields rather than copied, the time to live is decremented and the
IPv4 header checksum is recomputed before the frame leaves, and a transport
checksum the sender never finished is recomputed rather than passed on: a frame
handed to the far end with a checksum that does not verify is a packet the far
end will refuse, and a refused connection looks exactly like a path that is down.
A packet larger than the link it would leave by is answered with ICMP
fragmentation needed and the MTU that applies, so a size limit is reported to
whoever can act on it instead of becoming a silent drop.

### 14. The data plane is bounded and fails open towards dropping

The routing table, the neighbour cache and the forwarding queue each have a
ceiling. A configuration that asks for more is refused at startup rather than
silently clamped, and a full queue drops the frame and counts it instead of
growing memory. A frame that cannot be parsed is dropped and counted, and the
reader that saw it continues with the next frame, so malformed input cannot
stop the router.

`govulncheck` and the race detector are run over the router package with the
rest of the tree, and the forwarding decision and the ICMP error builder are
fuzzed without a socket, because they must be safe for any byte string.

### 15. A reconciler removes only what it can prove it created

`netforge reconcile reclaim` is the only command that deletes anything, and it
deletes inside a directory shared with every other program on the host. Its
entire safety argument is the ownership check, and that is where the controls are:

- **A directory is a candidate only if it carries a valid marker.** The marker is
  a small JSON document NetForge wrote inside the directory when it created it,
  naming a kind this build knows, an identifier that passes the same name
  validation a namespace does, the owning process and the creation time. It is
  written atomically, so a marker half-written by a killed process is not a
  marker. A directory without one is listed as `unidentified` and never removed —
  including a directory whose *name* is exactly one NetForge generates, which is
  the hardest case and the one the audit tests directly.
- **A marker that has been copied is not ownership.** A marker naming a different
  directory than the one it sits in, one written in another format version, one
  naming a kind that disagrees with the name, and one with no owner or no date
  are all refused.
- **Authorization is a field, not an implication.** `-id` names a directory;
  `--authorize` consents to removing it; `-why` records why. Without consent the
  request is refused before any filesystem call, and a reason is required for the
  same reason a recovery requires one: a later reader has to be able to see why
  something was deleted. Only `reconcile.Authorize` can produce an
  authorization, and its fields are unexported, so holding one means the checks
  ran rather than that a caller believed they had.
- **The name is a name, never a path.** A request cannot carry `..`, a path
  separator or a path of any kind, and every name must be one NetForge generates.
  The check costs no filesystem call, so a name that could reach out of the state
  root is refused before anything is read.
- **An authorization is spent on the root it was issued for.** Passing it to a
  different state root is a `permission` refusal rather than an obedience.
- **Live state is refused.** A directory whose owning process is still running is
  reported and left alone, so reconciling cannot pull state out from under a
  laboratory that is still writing to it. The liveness check reads `/proc` rather
  than signalling, so asking the question carries no power to act on the answer,
  and it fails towards "in use" if `/proc` cannot be read.
- **Removal is bounded and idempotent.** At most 64 directories per
  authorization, a caller cannot widen its own limits past the built maximum, and
  reconciling twice removes nothing the second time and does not fail. A directory
  that is already gone is counted as an absence rather than a refusal, so a
  repeated reconcile does not report a problem where there is none.
- **The blast radius is one directory.** A symlink carrying a NetForge name is not
  a candidate and is not followed, and a file carrying one is not a directory and
  is not removed. Reconciliation removes files under the state root and nothing
  else — no namespace, no link, no route, no qdisc rule — which is why it needs no
  privileges and cannot damage host networking.

Enforced by `tests/security/reconcile_audit_test.go`, which puts an impostor, a
symlink, a file, a garbage marker and a copied marker in the state root,
authorizes removal of all of them plus one genuine directory, and demands that
only the genuine one goes.

## Fault containment

Fault injection is the first part of NetForge that deliberately degrades a
path, so its containment is described here rather than assumed.

A fault can only be placed on a namespace the laboratory's scope names, and
only on a device that laboratory already created. The engine checks the scope
before the first kernel call and refuses a fault whose target is not in it,
which is what keeps a mistake in a configuration file from reaching a host
interface: a host namespace is never named in a laboratory's scope, so it
cannot be a target. Every fault is additionally bounded by compiled ceilings
that no configuration can raise, one fault at a time per device, and a fault
with a duration is retired when the duration passes even if the process that
asked for it is gone.

A fault is only reported as active after the kernel's own `tc -j` output has
been read back and matched against the request, so a fault that silently failed
to apply is reported as a failure rather than as a working experiment.

Every fault is removed on teardown, including when teardown is triggered by a
failed build, an interrupt or a repeated `Destroy`: the engine runs before the
namespaces disappear, so a fault can never outlive the namespace that carried
it. `Remove`, `RemoveAll` and `Close` are idempotent, and the laboratory's
cleanup stack is the single owner of that ordering.

`tc` is run by absolute path inside a laboratory namespace, and the argument
vector is built from validated values: a configuration value is never
concatenated into a command line, and no shell is involved.

## Measurement and diagnosis containment

A probe originates traffic and an investigation originates several probes, so
both are bounded at the same place as any other traffic the laboratory
generates.

Every probe request is validated before anything is sent: the namespace has to
belong to the experiment, the target has to be inside the network the
laboratory declared, and a protocol is refused the fields it does not use. A
target outside the scope is refused by the engine rather than attempted, and
the refusal is an error, not a result. An investigation is bounded the same way
and is bounded again as a whole: the probe budget covers the repeat of the
failing measurement as well as the layers below it, and a budget that runs out
names the layers it could not reach rather than quietly skipping them.

The measurement agent is the shipped binary re-executed inside a target
namespace, which keeps the process boundary in one place, and the answer comes
back over a pipe as one bounded JSON line. The response ceiling and the payload
ceiling are enforced by the engine and passed to the agent, so neither side can
be the only one enforcing a limit. Timeouts are applied by the engine, and a
measurement that exceeded its timeout is a classified result rather than a
value of zero.

A service started by a laboratory binds only inside that laboratory's own
namespace, and its listener holds a bounded number of connections; a request is
served by a handler that owns the connection, so one client cannot make the
listener hold a connection that is never released. A specification that asks
for no endpoints is refused, because a service that serves nothing is a process
with no reason to exist. Each laboratory has its own work directory, so two
laboratories cannot collide on a socket or a file.

The reasoning in `internal/investigator` reaches the system only through the
kernel's routing table and the probe engine, both of which are already bounded
and scoped, so a diagnosis cannot measure anything a direct probe would have
been refused. A route lookup is a kernel question and sends no traffic.

A diagnosis is only as trustworthy as the evidence behind it, and the
implementation treats an overconfident report as the more serious defect. A
measurement that could not be taken is recorded as attempted rather than
measured, so a laboratory problem is never read as a failing layer. The blamed
layer is the lowest layer that actually failed rather than the layer the caller
blamed, and the classification is clamped to the level the evidence carries.
A configured fault value is not a measurement and is never reported as one.

## Controls that do not exist yet

These are known gaps. They are listed so that a reader does not have to infer
them from silence, and none of them is claimed as implemented anywhere else in
this repository.

- Reconciliation of anything a `SIGKILL`ed run leaves other than files. A
  namespace dies with the holder that was keeping it open, because the holder is
  tied to a pipe the agent holds rather than to a signal the agent would have to
  survive to send, so an interrupted or killed run releases its namespaces — there
  is nothing for a reconciler to find there. What a killed run leaves is its state
  directories, and `netforge reconcile` (control 15) takes those back. It does not
  sweep on startup: reclamation is an operator's decision, with an authorization
  and a reason.
- Bandwidth, rate and concurrency enforcement at the kernel.
- Socket buffer limits and per-interface queue disciplines.

## Dependency posture

NetForge has exactly one direct dependency, `gopkg.in/yaml.v3`, pinned in
`go.mod` and `go.sum`. No other third-party module is imported.

`govulncheck` is run in CI against a pinned version
(`golang.org/x/vuln/cmd/govulncheck@v1.7.0`) and through `make vuln`. A scan
of this tree reports zero vulnerabilities in imported packages. It also reports
advisories against the Go standard library of the toolchain that runs the
scan; those are a property of the toolchain patch level, not of this
repository, and CI resolves the latest 1.25 patch via `actions/setup-go`.

## Reporting a vulnerability

This project has no published security contact yet. Until one exists, report
suspected issues by opening an issue in the repository that contains this
file, describing the reproduction steps and the impact.

# NetForge Threat Model

This document describes the assets, threats and trust boundaries that NetForge
designs against. It is scoped to what the project is: a local Linux networking
and failure-injection laboratory. It is not a remote attack framework, and
functionality such as Internet-wide scanning, credential attacks, exploitation,
evasion, persistence, unauthorised interception or destructive external packet
injection is explicitly out of scope.

## 1. Assets

| Asset | Why it matters | Status |
| --- | --- | --- |
| Host network configuration | Corruption breaks the operator's connectivity | verified unchanged by test |
| Host filesystem | A write to a system path is a compromise | only the named config file is read, and `/proc/sys/net` inside a laboratory |
| Experiment isolation | Loss of isolation invalidates every result | user namespace plus a private network namespace per laboratory |
| Namespaces and interfaces | Lab resources are attacker-relevant once probes exist | created, owned, tracked and destroyed |
| Experiment data and reports | Evidence integrity is the product | config documents only |
| Privileged operations | Capability misuse is the highest impact event | confined to `internal/namespace` and `internal/iface` |
| Host availability | Resource exhaustion is a denial of service | bounded inputs and limits |

## 2. Actors

| Actor | Trust level | Capabilities assumed |
| --- | --- | --- |
| Operator | trusted | invokes the CLI, supplies configuration, may supply privileges |
| Experiment configuration author | semi-trusted | writes YAML documents that NetForge validates |
| Configuration file supplied by a third party | untrusted | arbitrary YAML content up to the size limit |
| Network peer in the laboratory | untrusted | can send arbitrary packet bytes once the data plane exists |
| Local user on the same host | untrusted | can read the repository and invoke the binary |
| Process environment | untrusted | environment variables such as `NETFORGE_LOG_LEVEL` are validated before use |

## 3. Trust boundaries

```text
CLI arguments and environment
  ↓  validated, bounded, never interpolated into a shell
Configuration parser (internal/config)
  ↓  strict decoding, size limit, no side effects
Topology planning (internal/network)
  ↓  every address inside scope.network, every namespace named in the scope
Process boundary (internal/namespace, internal/iface)
  ↓  one package may start a process, no shell, validated argv, pipe-only agent
Linux networking subsystem, inside namespaces NetForge owns
```

The second boundary required by the project rules:

```text
Experiment configuration
  ↓  scope validator
Operation authorization
  ↓  action validation
Linux networking operation
```

The privilege boundary is enforced by construction: the process does not become
root, and no code path grants or drops capabilities. When privileged
operations arrive, the rule is that the unprivileged control plane validates
scope and authorization, and a narrow privileged layer performs only the
specific syscall or `netlink` request.

## 4. Threats and current mitigations

### T1 Malformed configuration

An attacker-supplied document causes a panic, an unbounded allocation, a
silent misinterpretation of a value, or a side effect.

Mitigations in place: strict decoding with unknown fields rejected; duplicate
key, multi-document and non-mapping rejection; explicit string types for
duration and bandwidth; 1 MiB size limit enforced before and during reading;
name and CIDR validation; no side effects in the parser. Evidence:
`internal/config/*_test.go`, including `FuzzParse` and
`TestParseDoesNotAllocateUnboundedMemory`.

Residual risk: a document that is valid and within all limits is still
accepted. The planner checks that the scope actually covers what an experiment
creates: every planned address must lie inside `scope.network` and every planned
namespace must be named in `scope.namespaces`, both before any kernel call.

Residual risk: a valid network inside the scope may still be unreachable in
practice, for example because the operator's host forbids forwarding. NetForge
verifies its own forwarding setting, not the reachability of an arbitrary
scope.

### T2 Resource exhaustion

A document requests more resources than the host can provide, or an input
causes unbounded memory or goroutine growth.

Mitigations in place: hard ceilings on every limit; rejection of a scope larger
than `max_namespaces`; size limit on documents and paths; bounded reads; no
background goroutine in the configuration path; the signal watcher is the only
goroutine in the process and it terminates deterministically. Evidence:
`internal/config/limits.go`, `internal/cleanup/cleanup_test.go`,
`internal/cli/shutdown_test.go`.

### T3 Command injection

Untrusted input reaches a shell.

Mitigations in place: NetForge never invokes a shell, and a repository test
fails the build if shell-related constructs appear anywhere in the shipped
sources, including in the process boundary. The only process execution is
`exec.Command` with separate arguments inside `internal/namespace`, which runs
`ip` by absolute path and a re-exec of the agent itself. Evidence:
`tests/security/source_test.go`, `TestNoShellExecution`,
`TestOnlyTheNamespaceBoundaryRunsHelpers`, `TestProcessBoundaryIsMinimal`.

### T4 Accidental host modification

NetForge changes the operator's networking, routing or firewall state.

Mitigations in place: the laboratory root is a new user namespace and a new
network namespace, so no operation can reach the host namespace; every resource
is created inside a namespace NetForge owns; namespace holders die with their
process; `Space.Write` refuses any path outside `/proc/sys/net`. Evidence:
`TestHostIsNeverModified`, `TestPartialCreationIsCleanedUp`,
`TestInterruptedSetupLeavesNothingBehind`, each of which compares host links,
host routes and host forwarding before and after.

### T5 Privilege abuse

NetForge runs more privileged than necessary, or a privileged helper is handed
unvalidated input.

Mitigations in place: NetForge never elevates and never calls `setuid` or
`seteuid`; the boundary test restricts `os/exec`, `syscall`, `os/signal` and
`os/user` to a declared list of files and fails if that list drifts in either
direction; the agent refuses to start unless its standard input is a pipe; the
capabilities it holds are namespaced capabilities confined to namespaces it
created. Residual risk: the agent trusts its parent completely, which is
acceptable because the parent is the same binary, but a modified binary
undermines the whole boundary. Evidence: `TestProcessExecutionIsIsolated`,
`TestAgentRefusesToRunCommandsFromTheOutside`, `TestUnprivilegedExecution`.

### T6 Sensitive logging

Secrets or sensitive values are written to logs or reports.

Mitigations in place: the configuration model cannot carry a credential, and a
test fails if a sensitive field name appears in that package. Logs carry
`timestamp`, `level`, `component`, `event` and, where applicable,
`experiment_id`, plus the specific measurement fields. The report schema is
fixed and declared in `internal/report`; fields that do not apply are omitted
rather than emitted empty, so a report carries only what was measured. Evidence:
`TestSecretsAreNotConfigurable`, `TestAnExperimentRunNeverLogsASecret`.

### T7 Uncontrolled concurrency

A goroutine outlives its owner, or a shutdown path blocks forever.

Mitigations in place: one goroutine, owned by `SignalContext`, stopped by
`Stop`, which is idempotent and waits for termination; cleanup actions run
exactly once with a bounded context and timeout; a recovered panic is reported
as an error. Evidence: `internal/cli/shutdown_test.go`,
`internal/cleanup/cleanup_test.go`, which include goroutine-count assertions
under the race detector.

### T8 Stale resources and unknown ownership

A previous run left namespaces, interfaces, routes or `tc` rules behind, and a
later cleanup destroys something it does not own.

Mitigations in place: every namespace NetForge creates is held open by a
dedicated holder process, so the kernel destroys it when that process dies, and
NetForge holds no reference to any namespace it did not create. Naming is
NetForge-owned and validated.

A holder that is tied only to a signal from the agent is an orphan after a
`SIGKILL` of the agent, because nothing sends a signal to a process that is
already gone. Each holder is therefore tied to a pipe the agent holds the write
end of, so the kernel closes it however the agent dies and the holder reads the
end of it and exits. The same rule applies to the router and to laboratory
services. Evidence: `TestAKilledExperimentIsRecoveredByTheNextRun` and
`TestSIGINTInEveryPhaseLeavesTheLaboratoryClean` now assert that no laboratory
process outlives its run, not only that the host's network objects are unchanged;
`TestInterruptedSetupLeavesNothingBehind`,
`TestNamespaceDestructionIsRepeatable`.

What a `SIGKILL`ed run still leaves is files: its state directory under
`/tmp/netforge-lab-*` and the software router's statistics directory under
`/tmp/netforge-router-*`, holding status files and counters and no namespaces.
`netforge reconcile` lists and reclaims them. Because `/tmp` is shared with every
other program on the host, ownership is established from a marker NetForge wrote
inside each directory when it created it, and a directory without a valid marker
is listed as unidentified and never removed. Removal requires an explicit
authorization and a written reason, is confined to the state root, is refused
while the owning process is running, and is idempotent. Evidence:
`TestTheReconcilerRemovesNothingItCannotProveOwns`,
`TestAKilledRunLeavesIdentifiableState`,
`TestReclaimingWhatAKilledRunLeftIsBoundedReportedAndIdempotent`,
`TestAReconcileRefusesStateThatIsStillInUse`.

Residual risk: a process that can write into the state root can write a marker
that looks valid, and a killed run's directories are reclaimed only when an
operator asks. An attacker with write access to `/tmp` can already do worse to
any process on the host, so this is not treated as a boundary; the marker is an
ownership record, not a capability. Reclamation is never automatic, so a crashed
run's directories persist until someone reconciles them, and a marker whose owner
pid has been reused makes a directory look in use — which costs one manual
removal rather than risking a live one.

### T9 Namespace escape and cross-experiment interference

A lab component reaches outside its namespace, or one experiment affects
another.

Mitigations in place: every node and the router have their own network
namespace, created inside a user namespace that owns them, so a process inside
one cannot name another. `setns` binds a single locked OS thread and the agent
is the only process that can enter a namespace, and only when the parent asks
for one command. Each experiment is a fresh set of namespaces, so two
experiments cannot share one.

Status: the boundary is demonstrated with namespace, interface and route
inspection and real traffic. Evidence: `TestGate2TrafficCrossesTheRouter`,
`TestInterfacesAreInspected`, `TestHostIsNeverModified`.

### T10 Denial of service against a lab target

A probe, fault or load generator overwhelms a target beyond its scope.

Status: mitigated for faults, open for load.

Faults validate scope before the first kernel call, so a fault can only land on
a namespace and a device the laboratory created and the scope names. Duration,
bandwidth, queue length, packet rate and the number of faults are bounded by
compiled ceilings that a configuration cannot raise, one fault at a time is
allowed per device, and a fault with a duration is retired by the engine's own
watcher. A fault that is refused leaves the kernel as it was: `ApplyAll` rolls
back what it already applied, and teardown removes the rest.

Evidence: `TestFaultsAreScopedToTheLaboratory`,
`TestALaboratoryRefusesAnImpossibleFaultSet`,
`TestTheLaboratoryTakesItsFaultsWithIt`.

Still open: load generation has to bound concurrency and response size before
it runs, and the queue and socket buffer bounds are not owned by NetForge. A
fault inside its bounds can still starve its own laboratory, which is the point
of the fault, and the laboratory is torn down as a unit.

### T11 Uncontrolled packet generation

A future packet component allocates or emits beyond limits.

Mitigations in place: `internal/packet` imports only the standard library
allowlist, checks the input length before reading a field, and refuses an
allocation whose bound comes from the input. A malformed message that satisfies
the wire format is still returned to the caller with its problems, so a later
layer can inspect it, and it is never silently accepted as valid. Evidence:
`tests/security/packet_test.go`, `TestParserChecksInputLength`,
`TestEveryAllocationIsBoundedBeforeItHappens`, the packet and router fuzz targets, and
mutation checks that confirm each invariant fails when the guard is removed.

Residual risk: the packet layer is a codec. It does not emit.

### T13 Malformed frames reaching a raw socket

A frame that arrives on an experiment interface is written by a process the
laboratory does not control, and the router reads it with a raw socket.

Mitigations in place: every length is checked before a field is read, the
routing table and the neighbour cache are bounded, and the forwarding queue
drops rather than blocks when it is full. A frame the parser refuses is counted
as malformed and the reader continues, so a flood of rubbish costs time but not
correctness and not unbounded memory. The decision path and the ICMP error
builder are fuzzed with no socket at all, and the real router is exercised by
integration tests that flood it with broadcast, ARP and IPv6 neighbour traffic
while checking that it keeps forwarding afterwards. Evidence:
`TestMalformedFramesAreDropped`, `TestQueueOverflowIsCounted`,
`FuzzDecide`, `FuzzICMPError`, and `TestSoftwareRouterIgnoresForeignTraffic`.

Residual risk: the router trusts the interface it is bound to. A process inside
the laboratory can still send anything on that interface; the scope is the
laboratory, not the sender.

### T12 Scope expansion

An experiment silently widens the scope it was granted.

Mitigations in place: the plan is built and checked in full before the first
kernel call. A namespace that the scope does not name is refused, an empty
scope grants nothing, an address outside `scope.network` is refused, and the
namespace and interface counts are checked against the limits. A grant is a
ceiling, so a wider grant is accepted and a narrower one is not. Evidence:
`TestScopeIsEnforced`, `TestEnforceScopeNeedsEveryNamespace`,
`TestCheckSpecRespectsLimits`.

### T14 Measurement escapes the laboratory

A probe or a diagnosis sends traffic somewhere it was not authorized to reach.

Mitigations in place: every probe request is validated before anything is sent,
and the target must be inside the network the laboratory declared and the
namespace must belong to the experiment. A refusal is an error rather than a
result, so an unauthorized target never becomes evidence. An investigation is
bounded by the same engine, and its budget counts the repeat of the failing
measurement, so the extra probes a diagnosis needs cannot escape the ceiling
that a single probe respects. The investigation reaches the system only through
the probe engine and a kernel routing query, neither of which originates
traffic on its own. Evidence: `TestAProbeCannotLeaveTheLaboratory`,
`TestAnInvestigationCannotLeaveTheLaboratory`, `TestAnInvestigationIsBounded`.

Residual risk: a laboratory with a wide enough scope can still measure anything
inside it. The scope is the trust boundary, not the probe.

### T15 A diagnosis claims more than the evidence supports

A report presents a guess as a finding, and a reader acts on it. This is the
threat this project cares most about, because a confident wrong diagnosis is
worse than no diagnosis: it stops the next question from being asked.

Mitigations in place: every statement in a report cites evidence by index, and
the evidence is the measurement. A layer is blamed only when the layer below it
was measured working, and the blamed layer is the lowest one that failed rather
than the layer the caller reported, so a failure seen at TLS and caused at the
transport is not filed under TLS. A measurement that could not be taken is
recorded as attempted and not measured, so a laboratory problem is never read
as a failing layer. A classification is clamped to the level of the hypothesis
the report is concluded from, and is dropped to UNKNOWN when that conclusion
cites nothing. A configured fault value is never treated as a measurement.
Evidence: `TestTheClampRefusesToOverclaim`,
`TestEveryClaimCitesEvidenceThatExists`, `TestAHealthyServiceIsNotDiagnosedAsBroken`,
`TestARefusedHandshakeIsDiagnosedAtTheTransportLayer`,
`TestAConfiguredFaultIsNotReportedAsAMeasurement`,
`TestARefusedMeasurementIsNotAWorkingLayerOrAFailingOne`.

Residual risk: the reasoning is only as good as the layers it can measure. A
failure that does not reproduce is reported as intermittent rather than
resolved, and a report may say UNKNOWN where an operator wanted a number.

## 5. Out of scope

- Internet-wide scanning and port scanning of unapproved targets.
- Credential attacks, exploitation, post-exploitation.
- Stealth, evasion and persistence.
- Unauthorised traffic interception on host interfaces.
- Destructive packet injection towards external destinations.
- Cloud, container orchestration and distributed control planes.

## 6. Verification plan

| Threat | Verification | Status |
| --- | --- | --- |
| T1 | `go test ./internal/config`, `make fuzz` | implemented |
| T2 | limit and size tests, race detector | implemented |
| T3 | `go test ./tests/security` | implemented |
| T4 | `TestHostIsNeverModified`, side-effect tests, integration suite | implemented |
| T5 | `go test ./tests/security`, unprivileged CI job | implemented |
| T6 | `go test ./tests/security` | implemented |
| T7 | `go test -race ./...` | implemented |
| T8 | holder-per-namespace lifetime tests, `TestTheReconcilerRemovesNothingItCannotProveOwns`, `TestAKilledRunLeavesIdentifiableState`, `TestAReconcileRefusesStateThatIsStillInUse` | implemented |
| T9 | `TestGate2TrafficCrossesTheRouter`, `TestInterfacesAreInspected` | implemented |
| T10 | scope, rate and concurrency tests | partial: scope is enforced, rate and concurrency are not |
| T11 | `make fuzz-packet`, `tests/security/packet_test.go` | implemented |
| T12 | `TestScopeIsEnforced`, `TestEnforceScopeNeedsEveryNamespace` | implemented |

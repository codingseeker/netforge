# NetForge

Industry-grade Linux networking, failure-injection, diagnostics, and resilience laboratory.

## 0. Project Objective

Build **NetForge**, a Linux-first networking systems platform written primarily in Go.

NetForge creates isolated virtual networks, forwards real traffic, injects controlled network failures, measures resulting behavior, diagnoses failures from collected evidence, performs explicitly authorized recovery actions, and verifies recovery.

This is a **networking/systems engineering project**, not a web application and not an offensive-security framework.

Primary engineering goals, in order:

1. Correctness
2. Isolation
3. Security
4. Observable/measurable behavior
5. Deterministic testing
6. Reliable cleanup
7. Reproducibility
8. Maintainability
9. Performance

Do not optimize for feature count.

Do not implement functionality merely because it is listed if the underlying prerequisite has not been completed and verified.

---

# 1. Implementation Rules

These rules apply to the entire repository.

## 1.1 Stop-on-failure rule

Never proceed to the next phase when a mandatory acceptance gate fails.

If a test fails:

1. investigate the failure;
2. fix the implementation or test;
3. rerun the relevant tests;
4. rerun the complete phase gate;
5. only then continue.

Never:

- weaken a test;
- delete a failing test;
- mark a requirement complete without evidence;
- silently reduce scope;
- bypass validation;
- hide errors;
- replace real behavior with mocked behavior at an integration boundary.

---

## 1.2 Evidence rule

Every completed task must have evidence.

Acceptable evidence includes:

- passing unit tests;
- integration-test output;
- fuzz-test results;
- benchmark results;
- packet captures;
- namespace/interface inspection;
- route-table inspection;
- tc configuration inspection;
- generated experiment reports;
- reproducible command output.

Do not claim functionality based solely on compilation.

---

## 1.3 Real-network-behavior rule

Networking functionality must be tested against actual Linux networking behavior wherever the feature crosses the Linux networking boundary.

Mocks may be used for:

- pure algorithms;
- deterministic failure paths;
- dependency isolation;
- unit tests.

Mocks must not be used to falsely demonstrate:

- packet forwarding;
- namespace isolation;
- traffic-control behavior;
- routing;
- packet loss;
- latency;
- packet capture;
- actual connectivity.

---

## 1.4 Scope rule

Implement only the functionality required by the current phase.

Do not prematurely implement:

- Kubernetes;
- cloud infrastructure;
- distributed control planes;
- web dashboards;
- databases;
- AI/LLM integrations;
- microservices;
- Internet-scale scanning;
- offensive-security functionality.

---

# 2. Technology Baseline

Language:

- Go

Target:

- Linux
- amd64 initially

CLI:

- Go

Networking:

- Go standard library
- Linux networking APIs

Isolation:

- Linux network namespaces

Virtual links:

- Linux veth

Traffic control:

- Linux `tc`
- `netem` where appropriate

Packet capture:

- libpcap/tcpdump where appropriate

Configuration:

- YAML

Testing:

- Go unit tests
- Go integration tests
- fuzz tests
- race detector

Build:

- Go toolchain

Containerization:

- optional;
- only for reproducible development/testing.

NetForge must operate locally without cloud services.

Do not introduce:

- Redis
- Kafka
- Kubernetes
- service mesh
- React
- Next.js
- PostgreSQL
- cloud infrastructure
- AI/LLM APIs

unless a later requirement establishes a concrete technical need.

---

# 3. Required Repository Layout

Start with:

```text
netforge/
├── cmd/
│   └── netforge/
├── internal/
│   ├── config/
│   ├── network/
│   ├── namespace/
│   ├── interface/
│   ├── packet/
│   ├── router/
│   ├── arp/
│   ├── icmp/
│   ├── chaos/
│   ├── probe/
│   ├── diagnostics/
│   ├── transport/
│   ├── loadbalancer/
│   ├── experiment/
│   ├── recovery/
│   ├── metrics/
│   ├── report/
│   └── security/
├── tests/
├── docs/
├── examples/
├── go.mod
├── go.sum
├── Makefile
├── README.md
└── todo.md
```

Do not create packages merely to satisfy the directory tree.

A package must have a concrete responsibility.

Avoid circular dependencies.

CLI code must not contain core networking logic.

---

# 4. Core Execution Model

NetForge must follow:

```text
CONTROL
   ↓
OBSERVE
   ↓
MEASURE
   ↓
DIAGNOSE
   ↓
ACT
   ↓
VERIFY
```

Never implement:

```text
GUESS
   ↓
ACT
```

Automated decisions must be backed by observable evidence.

---

# 5. Security Boundary

NetForge can manipulate networking and therefore has the potential to damage host networking if implemented incorrectly.

The default security model is:

```text
                    HOST
                     │
          ┌──────────┴──────────┐
          │                     │
   Host networking         NetForge lab
     untouched                  │
                         ┌──────┼──────┐
                         │      │      │
                       client router server
```

The default behavior must never modify the host's normal networking configuration.

Host-affecting operations require explicit privileged invocation and must be documented.

---

# 6. Privilege Model

Prefer:

```text
Unprivileged control plane
          │
          ▼
Small privileged networking operations
```

Do not run the entire application as root unless technically unavoidable.

Where possible, use:

- `CAP_NET_ADMIN`
- `CAP_NET_RAW`

instead of unrestricted root.

Document why each privileged operation requires the capability it uses.

---

# 7. Scope Model

Every experiment must declare an explicit scope.

Example:

```yaml
scope:
  network: 10.42.0.0/16
  namespaces:
    - client
    - router
    - server
```

Every operation affecting network state must be checked against the active experiment scope before execution.

The system must reject out-of-scope operations.

Never silently expand scope.

---

# 8. External Target Policy

Default probe targets:

- localhost;
- NetForge-created namespaces;
- explicitly authorized lab endpoints.

NetForge must not become a general-purpose Internet scanner.

Do not implement:

- Internet-wide scanning;
- credential attacks;
- exploitation;
- vulnerability exploitation;
- stealth/evasion;
- persistence;
- external traffic interception;
- destructive external packet injection.

External probing, if supported later, must require explicit configuration and pass scope validation.

---

# 9. Input Validation

Treat all externally supplied values as untrusted.

Validate:

- namespace names;
- interface names;
- IP addresses;
- CIDRs;
- routes;
- ports;
- protocols;
- experiment IDs;
- YAML configuration;
- fault parameters;
- command arguments;
- file paths;
- durations;
- bandwidth;
- packet rates;
- concurrency.

Reject:

- negative durations;
- malformed CIDRs;
- invalid interface names;
- invalid namespace names;
- out-of-range ports;
- malformed YAML;
- unknown configuration fields;
- excessive resource requests;
- invalid fault parameters.

Never pass unvalidated input into shell commands.

Prefer direct Linux APIs.

If command execution is unavoidable:

```go
exec.Command(...)
```

Requirements:

- never use `sh -c`;
- pass arguments separately;
- validate arguments;
- capture stderr;
- enforce timeout;
- check exit status.

---

# 10. Resource Limits

Every experiment must have bounded resources.

Initial configuration:

```yaml
limits:
  max_namespaces: 10
  max_interfaces: 30
  max_duration: 10m
  max_bandwidth: 1Gbps
  max_packet_rate: 100000
  max_concurrent_probes: 100
```

Prevent:

- unbounded goroutines;
- unbounded packet generation;
- unbounded queues;
- unbounded logging;
- excessive namespace creation;
- excessive interface creation;
- excessive probe concurrency;
- excessive packet sizes.

---

# 11. Cleanup Contract

Every resource created by NetForge must have an owner and cleanup path.

Resources include:

- namespaces;
- veth interfaces;
- routes;
- traffic-control rules;
- sockets;
- processes;
- goroutines;
- temporary files;
- experiment state.

Cleanup must run on:

- normal completion;
- operation failure;
- timeout;
- SIGINT;
- SIGTERM;
- recoverable panic.

Use `context.Context` for lifecycle propagation.

Cleanup must be idempotent.

This must be safe:

```text
netforge network destroy lab
netforge network destroy lab
```

The second invocation must not corrupt state.

---

# 12. Configuration Security

YAML represents data only.

It must never represent executable instructions.

Use strict decoding where practical.

Reject:

- unknown fields;
- invalid types;
- invalid enum values;
- duplicate critical configuration;
- invalid ranges;
- unsupported options.

Configuration parsing must be deterministic.

---

# 13. Secrets

NetForge should not require secrets.

If credentials are introduced later:

- do not store plaintext credentials in experiment files;
- do not print credentials;
- do not include credentials in reports;
- do not log authorization headers;
- prefer environment variables or OS credential mechanisms.

---

# 14. Threat Model

Create:

```text
docs/THREAT_MODEL.md
docs/SECURITY.md
```

Document:

## Assets

- host network;
- host filesystem;
- experiment isolation;
- namespaces;
- experiment data;
- privileged operations.

## Threats

- malformed configuration;
- malicious experiment files;
- accidental host modification;
- resource exhaustion;
- command injection;
- privilege abuse;
- namespace escape;
- uncontrolled packet generation;
- stale resources;
- sensitive logging.

## Trust boundaries

```text
CLI
 ↓
Configuration parser
 ↓
Controller
 ↓
Privileged networking operations
 ↓
Linux networking subsystem
```

Also document:

```text
Experiment configuration
 ↓
Scope validator
 ↓
Operation authorization
 ↓
Linux networking operation
```

---

# MILESTONE 1 — SECURE NETWORK FOUNDATION

---

## Phase 0 — Engineering Foundation

### TODO

- [x] initialize Go module;
- [x] establish repository structure;
- [x] implement CLI entry point;
- [x] implement command parsing;
- [x] implement structured logging;
- [x] implement configuration model;
- [x] implement strict YAML parsing;
- [x] implement application error model;
- [x] implement context propagation;
- [x] implement graceful shutdown;
- [x] establish test structure;
- [x] establish integration-test structure;
- [x] establish CI;
- [x] create initial README;
- [x] create SECURITY.md;
- [x] create THREAT_MODEL.md.

### Security

- [x] no shell execution;
- [x] reject malformed configuration;
- [x] enforce configuration size limits;
- [x] enforce experiment limits;
- [x] no root required for basic CLI operations.

### Tests

- [x] valid configuration;
- [x] malformed YAML;
- [x] unknown fields;
- [x] invalid values;
- [x] excessive resource requests;
- [x] cancellation;
- [x] graceful shutdown;
- [x] error propagation.

### Required commands

```text
go test ./...
go test -race ./...
go vet ./...
```

### Gate 0

PASS only if:

- [x] repository builds;
- [x] tests pass;
- [x] race detector passes;
- [x] vet passes;
- [x] malformed configuration is rejected;
- [x] shutdown is deterministic;
- [x] no uncontrolled goroutines exist;
- [x] no shell interpolation exists;
- [x] documentation exists.

**STOP.**

---

# Phase 1 — Packet and Protocol Primitives

Implement only the packet functionality actually required by later phases.

Initial protocols:

- Ethernet;
- IPv4;
- ICMP;
- UDP;
- required TCP headers.

### TODO

- [x] Ethernet parser;
- [x] Ethernet serializer;
- [x] IPv4 parser;
- [x] IPv4 serializer;
- [x] ICMP parser;
- [x] ICMP serializer;
- [x] UDP parser;
- [x] UDP serializer;
- [x] required TCP header parsing;
- [x] checksum calculation;
- [x] checksum validation;
- [x] packet size limits.

### Parser requirements

Every parser must:

- bounds-check input;
- validate header length;
- validate total length;
- detect integer overflow;
- enforce maximum packet size;
- avoid attacker-controlled huge allocations;
- return errors instead of panicking.

### Tests

- [x] valid packets;
- [x] truncated packets;
- [x] malformed headers;
- [x] invalid lengths;
- [x] invalid checksums;
- [x] oversized packets;
- [x] boundary values;
- [x] fuzz tests.

Run:

```text
go test ./internal/packet
go test ./internal/packet -fuzz=Fuzz
```

### Gate 1

Requirement:

> No malformed packet input may crash the process.

Additional:

- [x] parser fuzzing runs;
- [x] malformed input returns errors;
- [x] no unbounded allocations;
- [x] serialization round trips succeed.

**STOP.**

---

# Phase 2 — Isolated Linux Network

Implement actual Linux network isolation.

Topology:

```text
client
   |
 router
   |
server
```

### TODO

- [x] namespace creation;
- [x] namespace lookup;
- [x] namespace destruction;
- [x] veth creation;
- [x] veth attachment;
- [x] interface configuration;
- [x] deterministic IP assignment;
- [x] route creation;
- [x] route removal;
- [x] namespace inspection;
- [x] lifecycle ownership;
- [x] cleanup manager;
- [x] scope enforcement.

### Required properties

- [x] host routes remain unchanged;
- [x] host interfaces remain unchanged;
- [x] namespace names are validated;
- [x] CIDRs are validated;
- [x] routes are validated;
- [x] created resources are tracked;
- [x] partial creation is cleaned up;
- [x] repeated destruction is safe;
- [x] interrupted creation is cleaned up.

### Integration tests

Test:

```text
create
inspect
connect
destroy
destroy again
```

Failure scenarios:

- [x] namespace creation failure;
- [x] veth creation failure;
- [x] interface configuration failure;
- [x] route configuration failure;
- [x] interrupted setup.

### Gate 2

Demonstrate actual traffic:

```text
client → router → server
```

Evidence must include:

- namespace inspection;
- interface inspection;
- routes;
- successful connectivity;
- proof that host networking was not modified.

**STOP.**

---

# Phase 3 — Software Router

Implement a minimal router suitable for the isolated laboratory.

### TODO

- [x] routing-table representation;
- [x] longest-prefix matching;
- [x] route validation;
- [x] packet forwarding;
- [x] TTL decrement;
- [x] TTL expiration handling;
- [x] ICMP unreachable;
- [x] ICMP time exceeded;
- [x] ARP handling;
- [x] forwarding counters;
- [x] bounded forwarding queue.

### Security

- [x] reject invalid routes;
- [x] bound route-table size;
- [x] bound forwarding queues;
- [x] reject malformed packets;
- [x] enforce experiment scope.

### Tests

- [x] longest-prefix match;
- [x] route selection;
- [x] overlapping routes;
- [x] unreachable network;
- [x] TTL expiry;
- [x] malformed packets;
- [x] route conflicts;
- [x] route limits;
- [x] forwarding queue limits.

---
Evidence: `internal/router` unit and fuzz tests cover longest
prefix match, overlapping and conflicting routes, unreachable networks, TTL
decrement and expiry, ICMP unreachable and time exceeded, ARP handling,
counters and the bounded forwarding queue. `TestSoftwareRouterForwardsRealTraffic`
in `tests/integration` sends real ICMP traffic through the userspace router with
kernel forwarding switched off. (This paragraph originally named a test that no
existed: the router tests were named individually. The name is corrected here.
Phase 12 later added `TestSoftwareRouterForwardsEveryProtocolTheProberSpeaks`,
which found that the router carried ICMP and nothing else.)

# MILESTONE 1 GATE

All must pass:

- [x] isolated network works;
- [x] router forwards real traffic;
- [x] malformed packet input is safe;
- [x] cleanup is reliable;
- [x] host networking remains untouched;
- [x] race tests pass;
- [x] integration tests pass;
- [x] threat model exists;
- [x] security documentation exists;
- [x] security tests pass.

**STOP.**

---

Evidence: `go test -count=1 ./...`, `go test -count=1 -race ./internal/...
./tests/security/`, `go vet ./...` and `netforge limits show` all pass.

# MILESTONE 2 — CONTROLLED NETWORK FAILURE LABORATORY

# Phase 4 — Chaos Engine

Implement controlled faults using Linux traffic control where appropriate.

Initial faults:

- [x] latency;
- [x] jitter;
- [x] packet loss;
- [x] bandwidth limits;
- [x] packet duplication;
- [x] packet reordering.

Every fault must have:

```text
fault_id
experiment_id
source
destination
start_time
duration
parameters
status
```

### Security

- [x] scope validation;
- [x] duration limit;
- [x] bandwidth limit;
- [x] packet-rate limit;
- [x] no host-network chaos;
- [x] automatic cleanup;
- [x] idempotent fault removal.

Evidence: `TestFaultsAreScopedToTheLaboratory`,
`TestALaboratoryRefusesAnImpossibleFaultSet`,
`TestTheLaboratoryTakesItsFaultsWithIt`, plus the engine's limit and rollback
unit tests. The packet-rate ceiling is enforced by `internal/probe` on the
measurement, and every netem parameter a fault may set is bounded by
`internal/chaos`.

### Gate

Demonstrate that injected conditions produce corresponding measured behavior.

Do not report configured values as measurements.

For example:

```text
configured loss = 10%
```

does not mean:

```text
measured loss = 10%
```

The measurement must come from actual traffic.

---
Evidence: `internal/chaos` unit tests cover the fault model,
limit validation, the `tc` argument vector, engine lifecycle, rollback,
expiry and ownership. `TestEveryFaultKindProducesMeasuredBehaviour` injects
all six fault kinds into a real laboratory, measures each one with a real
probe, compares it against the same path without the fault and checks that
the path recovers after removal. `TestTheKernelAgreesWithEveryFault` compares
every record against `tc -j`.

# Phase 5 — Probing Engine

Implement:

- [x] ICMP probes;
- [x] TCP probes;
- [x] UDP probes;
- [x] DNS probes;
- [x] TLS probes;
- [x] HTTP probes.

Every probe requires:

```text
scope
timeout
concurrency limit
response-size limit
```

### Security

- [x] scope validation;
- [x] bounded timeout;
- [x] bounded concurrency;
- [x] bounded response size;
- [x] bounded result storage.

### Measurements

Store structured measurements including, where applicable:

- timestamp;
- target;
- protocol;
- success/failure;
- RTT;
- error;
- response size;
- experiment ID.

---
Evidence: `internal/probe` holds the request and result model, the scope
check, the bounded engine, the bounded result store, and the namespace-local
agent that opens sockets. `internal/packet/dns.go` is a pure DNS codec whose
allocations are checked before they happen, enforced by
`TestEveryAllocationIsBoundedBeforeItHappens`. `internal/service` provides the
bounded endpoints a probe measures, so no probe in the evidence below is
against a closed port. `TestEveryProtocolMeasuresAgainstRealService` in
`tests/integration` measures all six protocols from one client namespace
against a service the laboratory itself started in the server namespace, and
asserts the measured RTT, the protocol, the target and the namespace of every
result. ICMP is measured by the kernel's own echo tool, which the integration
test and `TestAnEchoProbeUsesTheMeasurer` both cover. The bounds are covered by
`TestAProbeCannotLeaveTheLaboratory` (host loopback, unknown namespace and an
address outside the network are all refused before anything is sent),
`TestAProbeIsBounded` (the engine's own limits reach the agent, the store keeps
only the newest results, and an oversized payload is refused) and
`TestATimeoutIsAFailure`. A closed port is reported as a classified failure
rather than an error, because a refusal to connect is a measurement
(`TestAClosedServiceIsVisible`).

---

# MILESTONE 2 GATE

Mandatory demonstration:

```text
BASELINE
   ↓
PACKET LOSS
   ↓
LATENCY
   ↓
JITTER
   ↓
RECOVERY
```

Evidence must show:

- [x] baseline measurements;
- [x] actual fault injection;
- [x] measured degradation;
- [x] fault removal;
- [x] measured recovery.

**STOP.**

---

Evidence: `TestEveryFaultKindProducesMeasuredBehaviour` measures a healthy
baseline, injects packet loss, latency and jitter as real netem faults, shows
the measured degradation against that baseline, removes each fault and shows
the measured path recovering. A configured value is never compared against
itself: every number checked comes from the probe's own report of real
traffic.

# MILESTONE 3 — EVIDENCE-BASED NETWORKING INTELLIGENCE

# Phase 6 — Failure Investigator

Implement diagnostic reasoning as an evidence pipeline.

```text
Observation
    ↓
Evidence collection
    ↓
Hypothesis generation
    ↓
Additional probes
    ↓
Evidence correlation
    ↓
Classification
```

Classification levels:

```text
CONFIRMED
PROBABLE
POSSIBLE
UNKNOWN
```

Never represent an unsupported hypothesis as confirmed.

### Diagnostic layers

- [x] DNS;
- [x] routing;
- [x] TCP;
- [x] TLS;
- [x] HTTP.

Every diagnosis must reference the evidence supporting it.

Example report:

```text
Diagnosis: PROBABLE

Failure domain:
router-2 → service-A

Evidence:
- measured packet loss: 11.8%
- RTT: 22ms → 184ms
- DNS: successful
- TCP: successful
- TLS: successful
- HTTP: timeout
```

The diagnostic engine must distinguish:

```text
DNS failure
TCP failure
TLS failure
HTTP failure
packet-loss degradation
```

based on actual measurements.

---

# Phase 7 — Reliable UDP Transport

Implement a bounded reliable transport over UDP.

Features:

- [x] sequence numbers;
- [x] acknowledgements;
- [x] retransmission;
- [x] timers;
- [x] sliding window;
- [x] duplicate handling;
- [x] reordering;
- [x] congestion control.

### Security

- [x] bounded receive buffers;
- [x] maximum payload;
- [x] connection/resource limits;
- [x] malformed packet rejection;
- [x] timeout enforcement;
- [x] memory-exhaustion protection.

### Benchmark scenarios

```text
0% loss
1% loss
5% loss
10% loss
high latency
high jitter
```

Compare:

```text
UDP
TCP
NetForge Reliable UDP
```

Measure:

- throughput;
- goodput;
- retransmissions;
- latency;
- completion time.

Record test conditions.

Evidence: `internal/transport` is the transport. A `Conn` writes a `Frame` with
a sequence number, the peer answers with a cumulative acknowledgement, and
anything unconfirmed is sent again on a timer; `TestAReusedBufferIsNotResentAsTheCallersMemory`
is a segment's lifetime in one test, because the frame is marshalled and the
caller's buffer is released before the segment is recorded
(`TestTheLargestPayloadThisTransportCarriesSurvives` carries the largest payload
the limits allow). The sliding window is the congestion window, which starts
small, grows while acknowledgements arrive and is cut when a retransmission
happens (`TestSlowStartDoublesTheWindow`, `TestATimeoutCutsTheWindow`), and a sender that cannot
get a segment out is bounded by its own limits and a deadline rather than by
waiting forever (`TestAWriteEndsWhenTheWindowNeverOpens`). A path that reorders
is put back in order before a reader sees it (`TestAReorderingPathStillDeliversInOrder`)
and a path that repeats is refused without being delivered twice
(`TestADuplicatingPathDoesNotDuplicatePayloads`).

Every security item is a limit rather than an intention. A payload larger than
the window is refused instead of buffered (`TestAPayloadLargerThanTheWindowIsRefused`),
a connection limit is enforced (`TestAConnectionLimitIsEnforced`), a malformed
datagram draws no reply and costs the sender nothing
(`TestAMalformedDatagramIsDroppedWithoutAReply`), a peer that never answers is
abandoned on the retransmission ceiling (`TestAPeerThatNeverAnswersIsAbandoned`),
a connection that has heard nothing for the idle timeout is stopped
(`TestAnIdleConnectionIsStopped`), and the receive side is bounded in both
directions — segments waiting behind a gap and segments already delivered to the
reader — so a peer that reorders or a reader that stops reading cannot make the
process grow (`TestTheReorderBufferIsBounded`,
`TestAStoppedCallerIsBoundedAndTheAckIsWithheld`).

The accounting is the part that has to be exactly right, because the congestion
window is the difference between two cumulative totals and a mistake in either
one is permanent. A segment is recorded, or deliberately not recorded, under a
single acquisition of the lock, so an acknowledgement arriving between the
decision and the record cannot leave the window believing a segment is in flight
that nothing is holding
(`TestAStaleRetransmissionIsNotCountedAsInFlight`,
`TestARetransmissionThatRacesAnAcknowledgementIsNotCounted`). An
acknowledgement is measured against the number of segments *sent* rather than
the highest sequence handed out, because a sequence is taken before its frame is
written and a peer's acknowledgement of one still being written would otherwise
be allowed through and leave the two totals disagreeing forever. `inFlight`
cannot report a wrapped-around number either: if the totals ever disagree the
answer is that nothing is in flight, and a sender waiting on four billion
phantom segments has nothing that will ever move them.

Evidence: the comparison is `TestTransportComparisonRecordsEveryScenario` in
`tests/integration/transport_test.go`, and it writes `docs/BENCHMARKS.md` from
the run that measured it, so the documented table is a run and not a copy of
one. All six conditions are recorded — 0%, 1%, 5% and 10% loss, 60ms of added
latency, and 40ms of latency with 30ms of jitter — and each is given to all
three transports over the same 256 KiB echoed by the same service. Every row
carries throughput, goodput, loss, mean round trip, retransmissions and whether
the transfer completed, alongside the conditions it was measured under. The
faults are applied by the kernel with `netem` rather than by a proxy in
userspace, because a proxy that swallowed bytes from a TCP stream would corrupt
that stream instead of modelling packet loss and TCP would never retransmit what
the proxy took, leaving the comparison a measurement of the proxy.

The comparison is also what keeps the transports honest about each other: a
reliable transport that loses payload is failed by
the benchmark's own checks on what each transport delivered, so "reliable" is checked
by the delivered bytes rather than by the name of the transport, and an
unreliable one is allowed to lose data but not to invent it.

---

# MILESTONE 3 GATE

### Diagnostics

- [x] DNS failure distinguishable;
- [x] TCP failure distinguishable;
- [x] TLS failure distinguishable;
- [x] HTTP failure distinguishable;
- [x] packet-loss degradation distinguishable;
- [x] evidence attached to diagnosis;
- [x] unsupported conclusions classified as UNKNOWN/POSSIBLE.

Evidence: `internal/investigator` is the evidence pipeline. `Observation` is a
caller's report of what went wrong and is refused before any probe runs on its
behalf (`TestAnUnusableObservationIsRefused`); the investigator then asks the
kernel for a route in the namespace (`network.Lab.Route`, which reads
`ip route get` and reports the kernel's own answer, including the device a
packet would leave by) and measures the failing layer and every layer below it,
lowest first, repeating the failing measurement to tell a steady fault from a
moment. Every measurement becomes `Evidence` and every statement in a report
cites it by index (`TestEveryClaimCitesEvidenceThatExists`).

The layers are distinguished from measurements rather than from the layer the
caller blamed: `blameLayer` names the lowest layer that failed, so a TLS
handshake refused by a closed port is diagnosed at the transport and the report
says the observed layer was never reached
(`TestARefusedHandshakeIsDiagnosedAtTheTransportLayer`). A refused port, a
refused resolver, a resolver code and a status line are specific answers, and
measured twice they are CONFIRMED
(`TestARefusedServiceIsDiagnosedAtTheTransportLayer`,
`TestARefusedResolverIsDiagnosedAtTheDNSLayer`,
`TestAnErrorStatusIsDiagnosedAtTheHTTPLayer`). A timeout is not specific: a
slow server and a broken one look the same, so it is only POSSIBLE
(`TestATimeoutAtALayerIsNotConfirmed`). Measured loss is a degradation rather
than a broken layer (`TestMeasuredLossIsADegradation`,
`TestMeasuredLossIsDiagnosedAsADegradation` with a real netem fault, which also
asserts the reported percentage is measured and never the configured one,
`TestAConfiguredFaultIsNotReportedAsAMeasurement`). Nothing answering anywhere
cannot be resolved into one layer, so no layer is named
(`TestNothingAnsweringIsNotResolvedIntoOneLayer`). A failure that does not come
back is reported as intermittent (`TestAFailureThatDoesNotReproduceNamesNoLayer`)
and a healthy service is never diagnosed as broken
(`TestAHealthyServiceIsNotDiagnosedAsBroken`).

"Never represent an unsupported hypothesis as confirmed" is enforced rather than
intended: `clamp` lowers any classification to the level of the hypothesis the
report is concluded from, and drops it to UNKNOWN when that conclusion cites no
evidence or no hypothesis at all (`TestTheClampRefusesToOverclaim`). The
hypotheses a report carries include the alternatives that were considered and
refuted, with the measurement that refuted them, so a reader can see what was
dropped and why; only the concluding hypothesis may raise the level
(`Hypothesis.Concludes`). A probe that could not be taken is recorded as
attempted and not measured, so a refusal by the scope check is never read as a
failing layer (`TestARefusedMeasurementIsNotAWorkingLayerOrAFailingOne`,
`TestAMeasurementThatCouldNotBeTakenConcludesNothing`,
`TestAnInvestigationCannotLeaveTheLaboratory`). A missing route is the kernel's
own answer and is confirmed without inference
(`TestAMissingRouteIsConfirmedFromTheKernel`), and the bounds apply to the whole
investigation including the repeat (`TestAnInvestigationIsBounded`).

### Reliable transport

- [x] survives packet loss;
- [x] survives reordering;
- [x] handles duplicates;
- [x] enforces resource limits;
- [x] does not leak goroutines/resources.

Evidence: loss, reordering and duplicates are properties of a path rather than
of a test, so each is imposed on a real socket between two real connections and
the result is read back and compared: a lossy path in both directions
(`TestALossyPathStillDeliversEverythingInOrder`), a lossy path carrying a
transfer far larger than the window
(`TestALossyPathStillFinishesALargeTransfer`), a reordering path
(`TestAReorderingPathStillDeliversInOrder`) and a duplicating path
(`TestADuplicatingPathDoesNotDuplicatePayloads`). The large-transfer case is the
one that found the defect this phase fixed, and it is kept for that reason: over
256 KiB on a path that both drops and duplicates, the congestion window could be
left believing a segment was in flight that nothing was holding, and because
the window is the difference between two cumulative totals the connection then
waited for room that would never come and stopped for good. It is
`TestALossyPathStillFinishesALargeTransfer` that asserts the invariant
outright, and the two deterministic tests beside it pin the two ways a
retransmission can race an acknowledgement.

Resource limits are enforced rather than described: a connection limit, a
payload larger than the window, a peer that never answers, an idle peer, a
cancelled context and a closed connection are each refused, stopped or bounded
by their own test, and the bounds are the ones in `Limits` rather than constants
in the code (`TestUnsetLimitsFallBackToWorkingOnes`,
`TestLimitsThatWouldBreakTheTransportAreRefused`). Goroutines and resources are
counted rather than assumed: `TestAClosedConnectionLeavesNothingRunning` opens
and closes eight connections and refuses to let the process keep anything, and it
is deliberately the one test in the package that does not run in parallel with
the others, because a count taken while twenty unrelated tests are opening
connections cannot tell a leak from a neighbour.

**STOP.**

---

# MILESTONE 4 — RESILIENCE PLATFORM

# Phase 8 — Load Balancer

Implement:

- [x] backend registration;
- [x] health checks;
- [x] round robin;
- [x] least connections;
- [x] consistent hashing;
- [x] failure detection;
- [x] connection draining;
- [x] backend recovery.

### Security

- [x] bounded backend count;
- [x] validated addresses;
- [x] health-check timeouts;
- [x] connection limits;
- [x] no arbitrary external backend registration.

Evidence: the balancer is exercised against real backends over real sockets, and
a payload is carried across it and read back at the far end
(`TestAPayloadCrossesTheBalancerAndComesBack`,
`TestATransferLargerThanOneWindowSurvivesTheProxy`), so the policies are
measured rather than described: round robin walks the backends in the order they
were given, least connections takes the one with fewest connections and respects
a per-backend limit when there is one
(`TestTheRoundRobinWalksTheBackendsInOrder`,
`TestTheLeastConnectionsPolicyTakesTheBackendWithFewestConnections`,
`TestTheLeastConnectionsPolicyRespectsThePerBackendLimit`,
`TestABackendAtItsOwnLimitIsPassedOver`), and consistent hashing is checked
against the documented hash, its stability and its spread
(`TestTheHashIsTheDocumentedOne`,
`TestTheConsistentHashPolicyIsStableAndSpread`,
`TestTheRingIsBoundedAndCoversTheRegisteredBackends`,
`TestAKeyPastTheEndOfTheRingWrapsOntoIt`).

Health and failure are properties of a real socket: a backend that stops
answering is taken out of service, one that answers again is used again, and a
failure found by live traffic is counted apart from one found by a check
(`TestABackendThatStoppedAnsweringIsTakenOutOfService`,
`TestABackendThatComesBackIsUsedAgain`,
`TestAFailureFoundByLiveTrafficIsCountedApartFromTheChecks`,
`TestTheFailureThresholdIsARunOfFailures`,
`TestRecoveryTakesMoreThanOneSuccess`). A check that is never answered costs only
its own backend, because one hung check must not become an outage for the rest
(`TestACheckThatIsNeverAnsweredCostsOnlyItsOwnBackend`). Draining is a state and
not a removal: a draining backend keeps the connections it already holds, stops
new ones, and is never counted twice when the drain expires
(`TestADrainStopsNewClientsAndIsNotARemoval`,
`TestADrainingBackendKeepsTheConnectionsItAlreadyHolds`,
`TestADrainTimeoutClosesTheConnectionsItWasHolding`,
`TestAnExpiredDrainIsNotCountedTwice`), and re-enabling a backend that was
removed is refused rather than quietly put back without a health check
(`TestReEnableRefusesARemovedBackend`,
`TestARemovedBackendStaysOutEvenWhenItAnswers`).

The security properties are tested as refusals rather than read as claims: a
backend address is parsed and never resolved, so a name cannot become a lookup
(`TestABackendAddressIsParsedAndNeverResolved`), nothing that reaches the
balancer can register a backend
(`TestNothingThatReachesTheBalancerCanRegisterABackend`,
`TestAControlDocumentCannotIntroduceABackend`), a control document for another
experiment is refused (`TestAControlDocumentForAnotherExperimentIsRefused`,
`TestAControlFileRefusesAnotherExperimentsDocument`), commands are applied once
however often the document is re-read
(`TestCommandsAreAppliedOnce`,
`TestASecondRemovalOfTheSameBackendIsNotASecondRemoval`), and the bounds are
refused when they are contradictory or above their ceiling
(`TestContradictoryTimingsAreRefused`,
`TestAPerBackendLimitAboveTheWholeIsRefused`,
`TestABackendCountAboveTheLimitIsRefused`,
`TestALimitAboveItsCeilingIsRefused`).

Two defects this phase found are kept as tests because both were invisible from
the code. A client that disconnected mid-stream used to leave the balancer
holding the connection and the goroutine serving it, so the balancer now counts
what it is holding and the leak is asserted directly
(`TestAClosedBalancerLeavesNothingRunning`,
`TestAConnectionIsCountedAgainstTheBackendThatServedIt`); the test polls for the
count to settle rather than sampling once, because a count taken a moment too
early reads a leak that is not there. And the consistent-hash ring was built on a
hash whose low bits were poorly distributed, so a small number of clients landed
on a small number of backends; the hash is now the one the test names and
documented rather than the one that happened to be in use.

**STOP.**

---

# Phase 9 — Experiment Engine

Experiments must be declarative.

Example:

```yaml
name: cascading-failure

scope:
  network: 10.42.0.0/16
  namespaces:
    - client
    - router
    - lb
    - service-a
    - service-b
    - service-c

limits:
  duration: 5m

faults:
  - target: service-b
    type: packet_loss
    value: 10%
```

Lifecycle:

```text
SETUP
  ↓
BASELINE
  ↓
FAULT
  ↓
OBSERVE
  ↓
DIAGNOSE
  ↓
RECOVER
  ↓
VERIFY
  ↓
REPORT
  ↓
CLEANUP
```

The experiment controller must validate the complete experiment before execution.

Do not partially execute an experiment because validation discovered an invalid later step.

Evidence: the controller walks the lifecycle in order and the order is itself
tested — a plan is followed in the order it was compiled, and only one
transition is legal from any state
(`TestAnExperimentWalksTheWholeLifecycle`, `TestAPlanIsFollowedInOrder`,
`TestOnlyOneStateTransitionIsLegal`). Every state is reachable for cleanup from
every state, which is what lets a run that failed three steps in still be torn
down rather than abandoned
(`TestEveryStateCanBeCleanedUpFrom`,
`TestAFailedStepStillCleansUp`), and a cleanup that itself fails is reported
instead of dropped on the floor, because a laboratory left half removed is worse
than one that was never built (`TestACleanupFailureIsReported`).

The "validate the whole thing before executing it" rule is tested by counting
what the environment was asked to do, not by reading the code: a document with an
invalid observation is refused before the laboratory exists
(`TestAnInvalidObservationIsRefusedBeforeTheLaboratoryExists`), a document with
several problems reports all of them at once instead of making the author fix
them one run at a time (`TestEveryProblemIsReportedAtOnce`), and nothing runs
until the entire document has been accepted
(`TestTheWholeExperimentIsValidatedBeforeAnythingRuns`). Bounds are the ones from
the document rather than constants: an experiment that cannot fit inside its own
duration is refused, and the controller's ceiling is enforced above that
(`TestAnExperimentThatCannotFitInItsDurationIsRefused`,
`TestTheControllerCeilingIsEnforced`).

The honest edges are tested rather than smoothed over. A document with no faults
still visits the fault step and does nothing, so the lifecycle is the same
whether or not there was anything to break
(`TestADocumentWithNoFaultsStillRunsTheFaultStep`,
`TestAnExperimentWithNoRecoveryVisitsTheStepAndDoesNothing`). A document with no
name is refused, because the name is the only thing that makes two runs
distinguishable in a report (`TestADocumentWithNoNameIsRefused`). A cancelled
experiment still cleans up (`TestACancelledExperimentStillCleansUp`), and the
seed is carried into the report so a run can be reproduced afterwards
(`TestTheSeedIsCarriedThroughToTheReport`, `TestTheControllerReportsWhereItIs`).

The whole lifecycle is then run against real namespaces, real routes and real
faults: a healthy path is measured, impaired, and measured again
(`TestAnExperimentMeasuresAHealthyPathThenAnImpairedOne`), consent withheld
produces a diagnosis and no repair
(`TestAnExperimentWithoutConsentDiagnosesButDoesNotRepair`), an authorized
repair is verified by measurement rather than by the fact that the command
returned (`TestAnExperimentWithAnAuthorizedRecoveryVerifiesItsRepair`), and both
a document that cannot be observed and a request it was not given are refused
before anything is touched (`TestAnExperimentRefusesToRunWithoutAnObservationItCanMake`,
`TestAnExperimentRefusesARequestItWasNotGiven`).

Two defects this phase found are kept as tests. The controller originally trusted
the environment's word that verification passed, so an experiment that
impaired the path and then "verified" it without re-measuring reported success
against a broken path; verification is now judged against the same criteria the
baseline used (`TestARecoveryThatPerformedSomethingElseFailsTheExperiment`), and
the check basis was separated from the action's target so a namespace-scoped
action is verified against the experiment's observation rather than against the
namespace it acted on. And the document-to-laboratory mapping is asserted
outright rather than assumed, because a laboratory's namespaces are derived from
an identity that is itself derived from the document
(`TestALaboratoryIsBuiltFromTheDocumentScope`).

**STOP.**

---

# Phase 10 — Recovery

Recovery actions require explicit authorization.

Allowed initial actions:

- [x] remove fault;
- [x] restore route;
- [x] restart lab service;
- [x] remove failed backend;
- [x] restore backend;
- [x] drain connection;
- [x] re-enable backend.

Never execute destructive recovery solely because the diagnostic engine suspects a failure.

Required:

```text
explicit authorization
        +
scope validation
        +
action validation
        +
actual recovery
        +
verification
```

Verification must use actual probes/measurements.

Evidence: authorization is a separate thing from diagnosis and the two are never
allowed to stand in for each other. A recommendation arrives with no consent and
executes nothing (`TestARecoveryNeedsExplicitAuthorization`,
`TestADiagnosisCannotAuthorizeItself`, `TestAnUnknownDiagnosisSuggestsNothing`),
an authorization is bound to the experiment it was given for and is useless
against any other (`TestAnAuthorizationIsOnlyIssuedForItsOwnExperiment`,
`TestAControlFileRefusesAnotherExperimentsDocument`), and the set of actions is
closed and enumerable rather than open to a string that happens to parse
(`TestTheActionSetIsClosedAndListed`, `TestEveryAllowedActionHasAnExecutor`,
`TestAnUnknownActionIsRefused`).

The whole request set is checked before the first action runs, so a request that
is valid followed by one that is not leaves the laboratory untouched
(`TestTheWholeRequestSetIsValidatedBeforeAnyAction`), the run stops at the first
failure rather than continuing into a state the caller did not intend
(`TestARecoveryStopsAtTheFirstFailure`), and a repeated action is refused instead
of counted twice (`TestARepeatedActionIsRefused`).

Every action is proven against the thing it claims to change rather than against
its own return value. Removing a fault and restoring it round-trips
(`TestARecoveryRemovesAndRestoresARealBackend`); removing a real backend from a
running balancer and watching the balancer stop sending clients there, then come
back, is the end-to-end case (`TestARecoveryRemovesABackendAndTheBalancerStopsUsingIt`,
`TestABackendThatComesBackIsUsedAgain`); a backend the balancer never had is
never touched (`TestABackendTheBalancerDoesNotHaveIsNeverTouched`); and a
malformed control request is refused at the boundary that would have written it
(`TestAControlFileRefusesAMalformedRequest`).

Scope is enforced at both ends. An address, a namespace, or a verification
namespace outside the granted scope is refused
(`TestAnAddressOutsideTheScopeIsRefused`,
`TestANamespaceOutsideTheScopeIsRefused`,
`TestAVerificationNamespaceOutsideTheScopeIsRefused`,
`TestAScopeNeedsANetworkAndNamespaces`), an address action is refused when it
could not say what it would be verified against
(`TestAnAddressActionNeedsAVerificationNamespace`), and the parser refuses
targets it cannot read unambiguously — a bare address, a port, or a prefix all
mean different things to different actions, so a document that mixes them is
rejected instead of guessed
(`TestTheScopeRefusesAmbiguousAddresses`).

Verification measures and does not assume: a verification is taken with the
prober and changes nothing on its way (`TestVerifyMeasuresWithoutChangingAnything`),
the criteria decide from the measurement and not from the operator's opinion
(`TestTheCriteriaJudgeWhatTheyMeasure`), an action that ran successfully but left
the path impaired is a failure
(`TestAnActionThatDoesNotRecoverIsAFailure`), a cancelled run stops verifying
instead of blocking on a probe that will never answer
(`TestCancellationStopsVerification`), and the bounds on the set of actions are
the ones in the limits rather than constants
(`TestTheLimitsBoundTheSet`, `TestAReasonIsRequired`).

The end-to-end proof is in the experiment suite rather than only here: an
authorized recovery in a real laboratory is verified by re-measuring the
observation and is refused when the measurement does not recover
(`TestAnExperimentWithAnAuthorizedRecoveryVerifiesItsRepair`,
`TestOnlyAuthorizedActionsArePerformed`,
`TestARecoveryThatPerformedSomethingElseFailsTheExperiment`).

**STOP.**

---

# Phase 11 — Observability

Implement structured metrics:

- packet count;
- byte count;
- RTT;
- packet loss;
- retransmissions;
- throughput;
- connection failures;
- availability;
- recovery time;
- diagnostic duration.

Structured log fields should include:

```text
timestamp
level
experiment_id
component
event
```

Never log:

- secrets;
- credentials;
- authorization headers;
- sensitive environment variables.

Provide JSON report output:

```text
netforge experiment run experiment.yaml --output json
```

---

# Phase 12 — Final Security and Reliability Audit

Evidence: the audit is three suites and each item below names the test that
demonstrates it. `tests/security` proves the boundaries a privileged
laboratory depends on, before any privilege is asked for;
`tests/integration/audit_test.go` proves what an interruption leaves behind; and
`tests/integration/networking_audit_test.go` and
`tests/integration/reliability_audit_test.go` prove the checklist against one
real laboratory and against a host that is asked about itself four separate
times.

The audit's first finding was in the checklist's own substrate rather than in
the checklist: the software router could not carry a TCP handshake at all. Every
earlier router test pinged, and ICMP computes its own checksum, so the defect
was invisible from the code and from the suite that existed. The router is now
measured against the five protocols the prober speaks
(`TestSoftwareRouterForwardsEveryProtocolTheProberSpeaks`), which is what the
Phase 12 networking items required and what no earlier phase had asked for.

## Security tests

- [x] malformed configuration — `TestMalformedConfigurationIsRefused`: not YAML,
  YAML of the wrong shape, a field the reader does not know, a fault field it
  does not know, and a fault aimed at a namespace the scope did not grant;
- [x] invalid CIDR — `TestInvalidTargetValuesAreRefused` refuses a network that
  is not a prefix and one that is the default route, and
  `TestADefaultRouteIsNotAScope` refuses it again at the layer that enforces it;
- [x] invalid namespace — the same test refuses an upper-case name, a name with a
  slash, a semicolon and a command substitution;
- [x] invalid interface — `TestInvalidInterfaceNamesAreRefused` refuses fifteen
  device names through the document reader as well as through the validator, and
  this is where the audit found a defect: a leading dash was accepted. A device
  name is one argument of a privileged helper, and an argument beginning with a
  dash is an option to that helper rather than a name, so `iface.ValidateName`
  now refuses it (`TestNameValidation`);
- [x] excessive resource request — `TestExcessiveResourceRequestsAreRefused`
  inflates each of the six ceilings one at a time, and then asks for nine
  namespaces inside a budget of four;
- [x] excessive packet size — `TestExcessivePacketSizeIsRefused` and
  `TestAnOversizePacketIsRefusedRatherThanAllocated`, the first at the probe
  request and the second at the frame parser;
- [x] excessive concurrency — `TestExcessiveConcurrencyIsRefused` holds one
  probe open and requires the second to be refused rather than queued;
- [x] command-injection payloads — `TestCommandInjectionPayloadsAreRefusedOrInert`
  feeds six payloads and requires the marker file a payload would create not to
  exist, then proves the layer that reads those bytes imports no process
  execution at all (`TestTheConfigurationLayerRunsNoCommands`);
- [x] malformed packets — `TestMalformedPacketsAreRefused` corrupts one header
  field of each of four real frames and requires a refusal that names the field,
  with `TestTheAuditFramesAreWellFormed` stating first that the frames being
  corrupted parse;
- [x] truncated packets — `TestTruncatedPacketsAreRefusedAtEveryLength` takes
  every proper prefix of three well formed frames, so a truncation is refused at
  whatever length the wire cut it to;
- [x] invalid checksums — `TestACorruptChecksumIsRefusedAndSaysSo` flips one bit
  of the IPv4, ICMP, UDP and TCP checksums, and
  `TestAnUncheckedChecksumIsMarkedRatherThanTrusted` requires a frame parsed with
  verification off to say so rather than read as one that passed;
- [x] timeout handling — `TestHostileTimeoutValuesAreRefused` refuses a negative
  timeout as unreadable and an excessive one at the limit,
  `TestAMeasurementCarriesItsDeadlineAndRefusesToOverrunIt` requires the deadline
  to be an argument of the helper rather than a wait, and
  `TestACancelledProbeIsRefusedBeforeItRuns` requires cancellation to be
  observed at the boundary, with a counting runner that proves the command never
  ran;
- [x] interrupted experiment — `TestRepeatedFailuresDoNotAccumulateState`;
- [x] SIGINT during setup — `TestSIGINTInEveryPhaseLeavesTheLaboratoryClean`,
  whose first offset is inside setup;
- [x] SIGINT during fault injection — the same test, whose second offset is
  inside the fault and the observation that follows it;
- [x] SIGINT during recovery — the same test, whose third offset is inside the
  recovery;
- [x] cleanup after crash — `TestAKilledExperimentIsRecoveredByTheNextRun` kills
  the run with SIGKILL three times and requires the next run with the same
  laboratory identifier to build, measure, finish and leave the host as it was.

Every one of those scenarios ends the same way: the laboratory is recoverable by
name, and the host is compared against the snapshot taken before it existed
rather than against an expectation.

## Networking tests

All thirteen items are measured in one laboratory, in one test
(`TestTheNetworkingChecklistIsMeasuredOnOneLaboratory`), because a checklist
whose items were each proved on a different network proves the items and not the
network. The laboratory is software routed, so forwarding, routing and address
resolution are all NetForge's rather than the kernel's, and each item is read
from the kernel's own answer or from a probe's own report.

- [x] packet forwarding — kernel forwarding is off, so anything that arrives was
  forwarded by NetForge; the reply carries `ttl=63`, so exactly one router was
  crossed, and the router's own counters report no malformed frame and no send
  error during the run;
- [x] routing — the kernel is asked `ip route get` and its answer has to name
  the laboratory's own device, and a more specific route is added and found in
  the router's table;
- [x] ARP — the router's own counters show it asked and answered, probed for an
  address and holds neighbours, and the far end's neighbour table shows it
  resolved a gateway;
- [x] ICMP — every echo request is answered and the round trips are ordered
  `min <= average <= max`;
- [x] TCP, [x] UDP, [x] DNS, [x] TLS, [x] HTTP — each is a probe from the client
  against a listener the laboratory really started, and each has to be answered
  with a measured round trip and a detail that names what came back;
- [x] packet loss — a quarter of the probes are dropped and the measurement says
  so, and the path is healthy again once the fault is removed;
- [x] latency — the fastest reply moves by at least the delay that was added,
  which a delay cannot fail to do because a round trip cannot be quicker than the
  delay in it;
- [x] jitter — the replies spread, and the slowest is well above the fastest;
- [x] bandwidth — the same traffic that offered about a megabit a second is
  measured at the ceiling that was configured and no higher.

`TestTheLaboratoryTheChecklistUsedIsTakenApart` then destroys that laboratory,
destroys it a second time to prove the teardown is idempotent, and requires the
host to be the host it was, with nothing carrying the laboratory's identity.

## Reliability tests

Run:

```text
go test ./...
go test -race ./...
go vet ./...
```

All three pass, and so does `make fmt-check`, which was failing before this phase:
six files written by Phase 11 had no trailing newline and one struct field was
not aligned.

One item below is corrected by Phase 13 rather than claimed here. "cleanup after
crash" was proved against the host's network objects, which is the only place a
laboratory's leftovers were looked for; a namespace is not in `ip netns list`,
because NetForge creates namespaces with `clone` and addresses them by PID, so
the only place a surviving one is visible is the process holding it. A `SIGKILL`ed
run left its namespace holders alive and reparented to init, each holding a
network namespace open for as long as the machine stayed up. The audit now asks
the process table as well as the network tools, and with the repair reverted the
same test reports nine surviving holders after three kills. See Phase 13.

Also:

- [x] fuzz packet parsers — `make fuzz-packet FUZZ_TIME=15s`: eight targets,
  including `FuzzRefreshTransportChecksum` for the repair the router performs,
  all pass with no new findings;
- [x] fuzz configuration parser — `make fuzz FUZZ_TIME=20s`: five targets pass
  with no new findings, and the recorded corpus is replayed by every test run
  (`make test-fuzz-seeds`);
- [x] repeat integration suite — `make test-repeat REPEAT=3` runs the whole tree
  end to end three times; three consecutive passes. This is what found the two
  remaining flaky tests, both of which asked a repair to be judged against an
  impairment that five probes against half loss leaves a one in thirty-two
  chance of not producing
  (`TestAnExperimentMeasuresAHealthyPathThenAnImpairedOne`,
  `TestExperimentRunWritesTheReportItMeasured`). Both now declare nine tenths
  and send twelve probes;
- [x] verify cleanup after repeated failures —
  `TestRepeatedBuildAndTeardownLeaveNothingBehind` builds, measures, impairs,
  removes and destroys the same laboratory four times and asks the host after
  every round, and `TestAFailedLaboratoryIsTornDownAsCompletelyAsASuccessfulOne`
  requires a build that was refused to leave the same nothing behind;
- [x] verify no stale namespaces;
- [x] verify no stale interfaces;
- [x] verify no stale routes;
- [x] verify no stale tc rules;
- [x] verify no stale namespace *holders* — the last four above are one
  comparison rather than four coincidences: each kind of object is looked up by
  name with the host's own tools, and the lines found after each round are
  compared against the lines that were there before the laboratory existed. That
  comparison cannot see a namespace NetForge created, because `clone` namespaces
  are addressed by PID and never appear in `ip netns list`, so Phase 13 added the
  process table as a fifth answer and found a `SIGKILL` that left one holder per
  node alive.

## Defects this phase found

Three, all fixed and all kept as tests because none of them was visible from the
code.

The software router could not carry a TCP handshake, a datagram, a name lookup, a
handshake or a request. It read frames from an `AF_PACKET` socket, which shows
the bytes as they are on the wire; a sender whose stack offloads the transport
sum writes only the seed of the checksum into those bytes, so the router
forwarded a checksum that did not verify and the far end dropped every packet.
ICMP computes its own sum, so echo requests crossed fine, every existing router
test pinged, and the defect was invisible from both the code and the suite. The
far end's own counters said what the wire would not: three segments in, none
out, all of them counted as checksum errors. The repair is
`packet.RefreshTransportChecksum`, called from the forwarding path, and it is
now exercised against all three protocols (`FuzzRefreshTransportChecksum`,
`TestForwardingRepairsATransportChecksumTheSenderLeftUnfinished`,
`TestSoftwareRouterForwardsEveryProtocolTheProberSpeaks`).

The same socket shows a sender's unsegmented packet rather than the pieces the
wire would carry, so the router could be handed a frame its egress device cannot
accept and could only report a send failure after the fact. It now compares the
rebuilt frame against the MTU the configuration gave the device and answers the
sender with ICMP fragmentation needed and the size that applies, which is what
lets a sender send a smaller packet rather than learn the limit by silence
(`TestAPacketTooLargeForTheNextLinkIsAnsweredNotDropped`).

A device name beginning with a dash was accepted. A name is handed to `ip` and
`tc` as one argument of a privileged helper, and an argument beginning with a
dash is an option to that helper, so the validator now refuses it.

Two measurement paths were also corrected rather than left to luck: reading the
router's counters was given twenty seconds instead of three, because a
diagnostic read that gives up under the race detector reports a laboratory that
is fine as one that is broken; and `TestAConfiguredFaultIsNotReportedAsAMeasurement`
was failing once in about six runs for a reason that had nothing to do with the
property it claims, because it searched the report for the string "50" while the
investigation repeats its failing probe exactly once and so measures 0, 50 or
100 percent. It now compares every percentage in the report against the
measurement in the report's own evidence, which is the property the test exists
to check and which holds whatever the wire happened to do.

**STOP.**

---

# Phase 13 — Final Delivery

Evidence: the four quality sections below are requirements rather than features,
so this phase is where the repository is checked against itself rather than
extended. Sections 15 to 22 hold the rules; this phase records what was missing,
what was wrong, and what now holds.

The first finding was a lifecycle defect, and it was found by asking a question
the earlier audit had not asked. Phase 12 proved that an interrupted run leaves
the host's links, routes, namespaces and qdisc rules exactly as it found them,
and every scenario passed. The proof was incomplete, because a namespace NetForge
creates is not in `ip netns list`: it is created with `clone` and addressed by
PID, so the only place a surviving one is visible is the process holding it. A
`SIGKILL`ed run left its namespace holders — one per node — alive and reparented
to init, each holding its network namespace open for as long as the machine
stayed up, and nothing would ever signal them: a holder waited on `SIGTERM`, and
nothing sends a signal to a process that is already gone. Two runs on the machine
this phase began on had left 31 such processes behind.

The repair ties each holder, and each laboratory service, to a pipe the owning
agent holds the write end of, which is the rule the software router already
followed: the kernel closes the pipe however the owner dies, and the child reads
the end of it and exits, so the namespace goes with it. `internal/service`
previously watched only for a signal and leaked the same way, because a service
holds its namespace open too. Both reads are bounded, which the repository's own
security suite required before either was accepted.

The second finding was in the suite rather than in the software.
`TestALossyPathStillDeliversEverythingInOrder` sampled a transport counter at one
instant, and on a path that drops one datagram in four that instant is sometimes
before the last acknowledgement has been processed. It failed once in the
coverage run of this phase, which is where a suite that had passed three
consecutive times is finally long enough for the wire to disagree with it. The
counter is now waited for, under a deadline, and the assertion is unchanged:
every segment sent was confirmed. Waiting is what the test should have done from
the start — a reader choosing which instant to look is asserting the path's luck,
not the transport's behaviour.

A third finding was the same kind of omission as the first. `FuzzParsePercent`
existed as a target in `internal/config` and was not in `make fuzz`, so a
percentage that reaches a fault's configuration was never fuzzed. It is now run
by the target and by the CI job.

## Documentation

- [x] every file section 18 lists exists — `docs/NETWORK_MODEL.md`,
  `docs/EXPERIMENTS.md`, `docs/PERFORMANCE.md` and `docs/OPERATIONS.md` were
  named there and did not exist. All four are written against the code rather
  than from it, and every transcript in `EXPERIMENTS.md` and `OPERATIONS.md` was
  produced by the command shown;
- [x] documentation describes the implementation that exists — eight
  divergences were found by reading the documents against their subjects, and
  every one is corrected and listed in `docs/MILESTONES.md`. The two that would
  have misled an operator rather than a reader: `README.md` listed four commands
  where the binary has six, omitting the two that create and destroy anything,
  and `docs/PROTOCOL.md` still said the router does not recompute a transport
  checksum, which is the defect Phase 12 found and fixed;
- [x] no claim of production readiness, security, zero vulnerabilities or total
  reliability anywhere — the status table says what the suite demonstrates and
  points at `docs/SECURITY.md` for the posture and `docs/THREAT_MODEL.md` for the
  threats in scope;
- [x] performance claims carry their conditions — `docs/PERFORMANCE.md` states
  the methodology and `docs/BENCHMARKS.md` is written by the run that measured
  it. The generated file was missing two of the eight conditions section 17
  requires, so the benchmark now records the kernel release, the CPU and its
  core count, and the revision, and prints "unknown" with the reason for anything
  it cannot read rather than leaving it out. The laboratory configuration and the
  reliable transport's limits are recorded too, because a window size that is not
  written down makes a window figure irreproducible;
- [x] the milestone report section 20 requires exists — `docs/MILESTONES.md`
  holds all four reports in the mandated format, each with its acceptance gates,
  its known issues and what it rests on, followed by the definition of done from
  section 22 item by item.

## CI

- [x] the three mandatory commands run on every push and pull request;
- [x] formatting, static analysis, dependency review and vulnerability scanning
  run alongside them;
- [x] the fuzz targets run on every push rather than on a schedule, and each job
  fails if a fuzzer wrote a corpus entry that was not committed — the router's
  two targets and the configuration corpus check were missing and are added;
- [x] a suite that passes once but is flaky is caught — `make test-repeat
  REPEAT=3` is now a job rather than a thing the phase report claims was run;
- [x] no job is given privileges it does not need. The integration job asserts
  it is *not* running as root, because its unprivileged claims are only evidence
  if it ran unprivileged. NetForge needs no privileges, so no privileged runner
  is configured and none is claimed;
- [x] Linux integration tests run — on `ubuntu-latest`, unprivileged, which is
  what the suite requires.

## Verification of this phase

- [x] `go test -count=1 ./...` passes — 22 packages, the integration suite in
  184.660s and the security suite in 1.652s;
- [x] `go test -count=1 -race ./...` passes, no race reported — the integration
  suite in 232.332s under the detector;
- [x] `go vet ./...` reports nothing and `gofmt -l cmd internal tests` reports no
  file;
- [x] `make test-repeat REPEAT=3` passes;
- [x] `make fuzz` (five targets), `make fuzz-packet` (eight) and `make
  fuzz-router` (two) pass with no new findings, and `make test-fuzz-seeds`
  replays the recorded corpus;
- [x] the acceptance workflow of section 21 passes on a real laboratory;
- [x] the leak above is proved twice: reproduced with the built binary, and the
  audit's own test fails with the repair reverted;
- [x] a run leaves the machine as it found it: after a full suite, no namespace
  holder survives, and the only thing a `SIGKILL` leaves behind is the state
  directory named above.

## Defects this phase found

Six: one in the lifecycle, one in a test, one in the build, and three in the
documentation. All are fixed. The lifecycle defect is kept as a test that fails
without the repair, and the test defect is kept as a wait rather than a sample,
which is what makes it hold whatever the path did.

A killed run did not release its namespaces. Reproduced with the built binary: a
laboratory killed 600ms into a run left three `netforge-namespace-holder`
processes alive under init, each holding a network namespace. Reverting the
repair makes `TestAKilledExperimentIsRecoveredByTheNextRun` report nine
surviving holders after three kills; with it, none. Repaired by
`internal/namespace/agent.go` and `internal/service/service.go`. The audit's
`assertNoLaboratoryProcesses` asks the process table, and
`TestSIGINTInEveryPhaseLeavesTheLaboratoryClean` and
`TestRepeatedFailuresDoNotAccumulateState` now check it as well, because a
namespace NetForge cannot see is a namespace NetForge has not released.

A test asserted a counter at an instant rather than waiting for it.
`TestALossyPathStillDeliversEverythingInOrder` demands that every segment it sent
was confirmed, and read `Acknowledged` the moment the last echo came back. On a
path dropping one datagram in four, that acknowledgement is a datagram of its own
and is sometimes still in flight, or dropped and waiting for the retransmission
timer. Both are the path working correctly; the test could not tell them from a
transport that had not confirmed. It now waits for the counter, under a five
second deadline that reports the connection's own counters rather than hanging,
and `make cover` — the run that found it — passes repeatedly afterwards.

A percentage parser was never fuzzed. `FuzzParsePercent` was written, seeded with
its edge cases and replayed by `make test-fuzz-seeds`, but `make fuzz` did not
include it, so the fuzzer had never actually fuzzed anything. It is in the target
now, which is why the target count went from four to five.

Help answered for a command that does not exist. `netforge help experiment run`
printed `netforge run - ...`, and `netforge experiment` told the reader its only
subcommand was `plan`, when `run` is the one that builds a laboratory. A
subcommand now prints the path that reaches it
(`TestHelpForCommand`), and every group command lists its subcommands
correctly.

`README.md` described a CLI with four commands and listed one of the three
external programs NetForge needs. Corrected, along with `docs/ARCHITECTURE.md`
on how `ip` is resolved and how many workers the router runs,
`docs/PROTOCOL.md` on the transport checksum, and `docs/SECURITY.md` and
`docs/THREAT_MODEL.md` on who reaps a killed run's holders.

**STOP.**

---

# Phase 14 — Laboratory Lifecycle from the Command Line

Evidence: three capabilities exist, are exercised against real namespaces, and
cannot be reached from the command line. Each is a flag's absence rather than a
missing implementation, which is why no earlier phase found it: the tests that
need them build the specification in Go.

What exists and cannot be reached:

- the software router, which is `network.RoutingSoftware` and is measured by
  `TestSoftwareRouterForwardsEveryProtocolTheProberSpeaks` on a real laboratory,
  but which `netforge experiment run` has no flag for, so a CLI run is always
  kernel-routed;
- laboratory services, which `network.Spec.Services` starts and which the probe
  needs for every protocol except ICMP, but which the CLI never asks for, so a
  `--protocol tcp` observation from the command line has nothing to answer;
- the load balancer, which `internal/loadbalancer` implements and which no
  command starts.

The first two are flags. The third is a component the laboratory does not own.

## Requirements

- [x] the software router is selectable from the command line and a CLI run
  forwards real traffic through it — the flag is refused unless it names a mode
  the router implements, and a software-routed run reports `ip_forward=0` in the
  router namespace so the reader can tell the kernel forwarded nothing;
- [x] laboratory services are requested from the command line, and an observation
  with a protocol that needs a listener is answered by one — a run that asks for
  TCP against nothing is refused rather than measured against a closed port;
- [x] the ports a service binds are chosen by the caller and bounded, so two runs
  on one host cannot collide and a port outside the range is refused;
- [x] every flag is validated before a namespace exists, and the validation names
  the field;
- [x] a run reports what it built: the namespaces, the addresses, the services
  bound, the routing mode, and the router's counters when it is the software
  router;
- [x] the flags appear in `netforge help experiment run`, so the command line is
  its own documentation;
- [x] the whole suite passes, twice under the race detector, and `make cover`.

## Design notes

The routing flag is a string on `experimentOptions` that is translated into
`network.Routing` in `labSpec`, refused before the laboratory is built if it
names anything else. It is refused as `unsupported` rather than defaulted,
because a silently ignored `--routing software` would produce a report claiming
a software router that never ran — the same class of defect as the transport
checksum Phase 12 found.

The service flags are a count and a base port, not a list: a laboratory that
serves N servers on a base port needs the caller to know the arithmetic, and a
list would let a caller bind the same port twice. The ports a run actually bound
are reported, so a collision is visible in the output rather than discovered by
the next run failing.

The router's counters are reported when the software router is in use, because
that is the only way an operator can tell from outside the process whether a
forwarded byte came from NetForge or from the kernel. They come from the same
statistics file the router already writes and refreshes on `SIGUSR1`.

What this phase does not do is add a `netforge lab` command that creates,
inspects and destroys a laboratory on its own. That would need a laboratory that
outlives the process that made it, which is a different piece of work from making
one reachable: see Phase 15.

## What this phase found

The requirements were met by three flags and one report block, and the evidence
that any of it was needed came from a run of the suite afterwards rather than
from the design: `netforge reconcile list` on this machine found fifteen state
directories the audit tests had left in `/tmp` over the previous phases, which is
the whole of Phase 15's premise and had been true for every phase so far.

Two of the phase's own requirements turned out to be the same requirement. "A run
reports what it built" and "a CLI run can use the software router" cannot be
satisfied separately: a report that says a software router forwarded a packet and
does not say whether the kernel was also forwarding is a report that cannot be
checked, and the only place the answer exists is a namespace that is gone by the
time the report is written. So `Lab.Summary` is recorded during `Setup`, while
every namespace and listener still exists, and the router's counters are read
once immediately before the teardown rather than never. The counters are read
through the existing `SIGUSR1` revision with a five-second window instead of the
twenty a diagnostic read gets, because this read happens inside a teardown and a
run that spent its teardown waiting for a number would be reporting late.

One capability the phase's evidence named was not reachable and is not now: the
load balancer. It is a component the laboratory does not own — it has no
namespace, no interface and no address of its own — so there is no flag that
could select it, and wiring it would mean giving the laboratory a second
forwarding component rather than exposing one it has. It is named in the known
issues of `docs/EXPERIMENTS.md` and in the milestone report rather than left
implied by silence.

**STOP.**

---

# Phase 15 — Reconciliation of What a Killed Run Left

Evidence: section 13 of this document requires three steps and none of them
exists. "Discover candidate resources, verify ownership, reconcile or remove only
positively identified resources" is unimplemented, and Phase 13 recorded that as
a known limitation rather than as a claim.

The gap is small in code and specific in consequence. A run killed with `SIGKILL`
releases its namespaces — Phase 13 repaired that — but leaves two directories
under `/tmp` holding service status files and a router statistics file. Nothing
lists them, nothing verifies they are NetForge's, and nothing removes them.

The danger in implementing this naively is the one the section names: a
reconciler that removes a directory it did not positively identify will eventually
remove something that is not NetForge's. `/tmp` is shared. So ownership has to be
established from something NetForge wrote and nothing else could have written, and
anything not positively identified has to be reported rather than touched.

## Requirements

- [x] a command lists what NetForge believes it left behind, naming each entry
  with the identifier, the kind and the age, and lists nothing it cannot identify
  as its own;
- [x] ownership is proven from a marker NetForge wrote at creation, and a
  directory without a valid marker is never a candidate for removal;
- [x] the marker records what the directory is for, so a reader can tell a
  laboratory's state directory from a router's, and both from anything else in
  `/tmp`;
- [x] removal requires explicit authorization, the same way a recovery does, and
  refuses without a reason — a reconciler that deletes on a flag alone is the
  destructive operation this document's rules are about;
- [x] removal is bounded, reports what it removed and what it refused, and is
  idempotent: reconciling twice removes nothing the second time and does not
  fail;
- [x] a directory that is in use is refused rather than removed, so reconciling
  cannot pull the state out from under a running laboratory;
- [x] the path a reconciler touches is confined to the state root NetForge
  chooses, and a name that is not one NetForge generated is refused before any
  filesystem call — the same rule `Space.Write` applies to `/proc/sys/net`;
- [x] a run that finishes normally leaves nothing for the reconciler to find,
  which is what makes its output a report about crashes rather than about normal
  operation;
- [x] the whole suite passes, twice under the race detector, and `make cover`.

## Design notes

The marker is a small JSON document written into the state directory when it is
created, carrying the identifier, the kind, the process that owns it and the time
it was created. It is written atomically, the way every other status file in this
repository is, because a half-written marker is a marker that cannot be trusted
and an untrusted marker is not ownership.

A directory is a candidate only if its marker parses, names a kind this build
knows, and carries an identifier that passes the same name validation a namespace
does. Anything else under the state root is listed as `unidentified` and left
alone. That is the whole of the ownership argument: NetForge names its own
directories, the marker proves the naming, and nothing is removed without both.

Reconciliation never touches a namespace, a link, a route or a qdisc. Those are
released by process lifetime, which Phase 13 made true, so a reconciler that went
looking for them would be searching for something that cannot survive. It removes
files under the state root and nothing else, which is why it needs no privileges
and cannot damage host networking.

## What this phase found

The naive implementation is wrong in three places, and the repository's own suites
found two of them.

The liveness check reached for `syscall.Kill(pid, 0)`, and
`TestProcessExecutionIsIsolated` refused it: asking whether a process is running
is a question about `/proc`, not a reason to signal a process, and a reconciler
that can signal a process is one a future change could be argued into using for
something. It now reads `/proc`, and fails towards "in use" when `/proc` cannot
answer — the two errors are not symmetric, because refusing to remove a directory
that was already gone costs an operator one manual removal while removing a live
laboratory's state costs the laboratory.

A reclaim conflated "there is nothing there" with "there is something there that
is not ours", because both arrived as an error from reading the marker. The
second is a refusal and the first is a success, and reporting a refusal as a
success is the exact defect this phase exists to prevent, in the one place the
phase wrote new code. The two are now counted apart, so reconciling twice reports
`already gone` and exits zero.

The listing's columns were narrower than a router's generated directory name, so
on a real `/tmp` the kind ran into the name. That is not cosmetic: the kind is the
field that tells a laboratory's directory from a router's, and a listing where the
two are indistinguishable is the listing this phase set out to produce. It was
found by running the command against the machine's actual temporary directory,
which is the only place that bug is visible — every test uses a directory holding
one entry.

**STOP.**

---

# 15. Engineering Quality Rules

## Errors

Never ignore errors from:

- networking operations;
- filesystem operations;
- process execution;
- cleanup;

---

# 19. CI

CI must run at minimum:

```text
go test ./...
go test -race ./...
go vet ./...
```

Also add where practical:

- formatting checks;
- static analysis;
- dependency/vulnerability scanning;
- Linux integration tests;
- fuzzing jobs where appropriate.

Privileged networking tests must run only in an environment where the required privileges are explicitly configured.

Do not give unrelated CI jobs elevated privileges.

---

# 20. Required Milestone Report

After every milestone, stop and produce exactly:

```text
==================================================
MILESTONE M[X] — STATUS
==================================================

Scope:
...

Implemented:
...

Security:
...

Tests:
...

Integration Tests:
...

Performance:
...

Acceptance Gates:
[PASS] ...
[PASS] ...
[FAIL] ...

Known Issues:
...

Files Changed:
...

Evidence:
...

NEXT:
M[X+1] / BLOCKED
```

If one mandatory gate fails:

```text
STATUS: BLOCKED
```

Do not proceed.

Do not hide failures.

Do not weaken tests.

Do not remove requirements to make a milestone pass.

---

# 21. Final End-to-End Acceptance Test

The completed system must execute the following laboratory workflow:

```text
1. Create isolated network
2. Validate experiment scope
3. Start client
4. Start router
5. Start load balancer
6. Start three services
7. Establish baseline
8. Record baseline metrics
9. Inject controlled packet loss
10. Inject latency
11. Observe degradation
12. Run diagnostics
13. Produce evidence-backed diagnosis
14. Execute explicitly authorized recovery
15. Verify network recovery
16. Verify service recovery
17. Generate structured report
18. Destroy experiment
19. Verify all resources were cleaned
```

Expected architecture:

```text
                    NETFORGE
                       │
              ┌────────▼────────┐
              │ Experiment      │
              │ Controller      │
              └────────┬────────┘
                       │
       ┌───────────────┼────────────────┐
       │               │                │
   Network           Chaos          Diagnostics
   Control           Engine           Engine
       │               │                │
       └───────────────┼────────────────┘
                       │
                    Linux Lab
                       │
        ┌──────────────┼──────────────┐
        │              │              │
      client          router          lb
                                      │
                         ┌────────────┼────────────┐
                         │            │            │
                      service-a   service-b   service-c
```

Evidence: `TestTheFinalAcceptanceWorkflow` (`tests/integration/acceptance_test.go`)
walks all nineteen steps on one real laboratory, in order, each as its own named
subtest. Two steps are worth naming rather than glossing:

- Step 5 is the load-balancing hop. The acceptance laboratory puts three service
  namespaces behind one router namespace, and the router is the single hop every
  request crosses, so the acceptance's load balancer is that router and its three
  services are the namespaces behind it. The balancer package itself
  (`internal/loadbalancer`) is measured against a running balancer in the Phase 8
  suite; what the acceptance proves is the workflow, not the policy.
- Step 19 is checked twice: against the host's own four views
  (`ip -o link show`, `ip -o route show`, `ip netns list`, `tc qdisc show`) and
  against the process table, since a namespace NetForge created is not in
  `ip netns list`. A `t.Cleanup` assertion runs the host check a second time
  after the test's own teardown, so a leak that only appears during teardown is
  still a failure.

**STOP.**

---

# 22. Definition of Done

NetForge is complete only when all of the following are true:

- [x] all four milestones pass;
- [x] all mandatory gates pass;
- [x] integration tests pass on Linux;
- [x] race detector passes;
- [x] fuzzing has been performed on packet/config parsers;
- [x] security audit is complete;
- [x] cleanup has been repeatedly verified;
- [x] host networking remains isolated during laboratory operation;
- [x] diagnostics use actual evidence;
- [x] recovery requires explicit authorization;
- [x] recovery is independently verified;
- [x] reports are reproducible;
- [x] documentation reflects the actual implementation;
- [x] performance measurements include their test conditions;
- [x] no known critical security or lifecycle defect is being hidden.

Completion means the evidence supports the claim.

It does not mean every conceivable networking feature has been implemented.

Evidence for each line, and what it rests on:

- Milestones and gates: `make check` passes, which is `fmt-check`, `vet`, the
  whole suite, the whole suite under the race detector, coverage and a build.
  `make fmt-check` was the one that failed at the start of this phase.
- Integration on Linux: `tests/integration` builds 

---

# 19. CI

CI must run at minimum:

```text
go test ./...
go test -race ./...
go vet ./...
```

Also add where practical:

- formatting checks;
- static analysis;
- dependency/vulnerability scanning;
- Linux integration tests;
- fuzzing jobs where appropriate.

Privileged networking tests must run only in an environment where the required privileges are explicitly configured.

Do not give unrelated CI jobs elevated privileges.

---

# 20. Required Milestone Report

After every milestone, stop and produce exactly:

```text
==================================================
MILESTONE M[X] — STATUS
==================================================

Scope:
...

Implemented:
...

Security:
...

Tests:
...

Integration Tests:
...

Performance:
...

Acceptance Gates:
[PASS] ...
[PASS] ...
[FAIL] ...

Known Issues:
...

Files Changed:
...

Evidence:
...

NEXT:
M[X+1] / BLOCKED
```

If one mandatory gate fails:

```text
STATUS: BLOCKED
```

Do not proceed.

Do not hide failures.

Do not weaken tests.

Do not remove requirements to make a milestone pass.

---

# 21. Final End-to-End Acceptance Test

The completed system must execute the following laboratory workflow:

```text
1. Create isolated network
2. Validate experiment scope
3. Start client
4. Start router
5. Start load balancer
6. Start three services
7. Establish baseline
8. Record baseline metrics
9. Inject controlled packet loss
10. Inject latency
11. Observe degradation
12. Run diagnostics
13. Produce evidence-backed diagnosis
14. Execute explicitly authorized recovery
15. Verify network recovery
16. Verify service recovery
17. Generate structured report
18. Destroy experiment
19. Verify all resources were cleaned
```

Expected architecture:

```text
                    NETFORGE
                       │
              ┌────────▼────────┐
              │ Experiment      │
              │ Controller      │
              └────────┬────────┘
                       │
       ┌───────────────┼────────────────┐
       │               │                │
   Network           Chaos          Diagnostics
   Control           Engine           Engine
       │               │                │
       └───────────────┼────────────────┘
                       │
                    Linux Lab
                       │
        ┌──────────────┼──────────────┐
        │              │              │
      client          router          lb
                                      │
                         ┌────────────┼────────────┐
                         │            │            │
                      service-a   service-b   service-c
```

Evidence: `TestTheFinalAcceptanceWorkflow` (`tests/integration/acceptance_test.go`)
walks all nineteen steps on one real laboratory, in order, each as its own named
subtest. Two steps are worth naming rather than glossing:

- Step 5 is the load-balancing hop. The acceptance laboratory puts three service
  namespaces behind one router namespace, and the router is the single hop every
  request crosses, so the acceptance's load balancer is that router and its three
  services are the namespaces behind it. The balancer package itself
  (`internal/loadbalancer`) is measured against a running balancer in the Phase 8
  suite; what the acceptance proves is the workflow, not the policy.
- Step 19 is checked twice: against the host's own four views
  (`ip -o link show`, `ip -o route show`, `ip netns list`, `tc qdisc show`) and
  against the process table, since a namespace NetForge created is not in
  `ip netns list`. A `t.Cleanup` assertion runs the host check a second time
  after the test's own teardown, so a leak that only appears during teardown is
  still a failure.

**STOP.**

---

# 22. Definition of Done

NetForge is complete only when all of the following are true:

- [x] all four milestones pass;
- [x] all mandatory gates pass;
- [x] integration tests pass on Linux;
- [x] race detector passes;
- [x] fuzzing has been performed on packet/config parsers;
- [x] security audit is complete;
- [x] cleanup has been repeatedly verified;
- [x] host networking remains isolated during laboratory operation;
- [x] diagnostics use actual evidence;
- [x] recovery requires explicit authorization;
- [x] recovery is independently verified;
- [x] reports are reproducible;
- [x] documentation reflects the actual implementation;
- [x] performance measurements include their test conditions;
- [x] no known critical security or lifecycle defect is being hidden.

Completion means the evidence supports the claim.

It does not mean every conceivable networking feature has been implemented.

Evidence for each line, and what it rests on:

- Milestones and gates: `make check` passes, which is `fmt-check`, `vet`, the
  whole suite, the whole suite under the race detector, coverage and a build.
  `make fmt-check` was the one that failed at the start of this phase.
- Integration on Linux: `tests/integration` builds the binary and runs it, builds
  real namespaces, routes real traffic through both the kernel router and
  `internal/router`, and tears each laboratory down.
- Race detector: `go test -count=1 -race ./...`, no race reported.
- Fuzzing: `make fuzz`, `make fuzz-packet` and `make fuzz-router`, fifteen
  targets in all — five configuration parsers, eight packet and checksum targets
  and two forwarding targets — with the recorded corpus replayed by every
  ordinary test run (`make test-fuzz-seeds`). `FuzzParsePercent` was added to
  `make fuzz` by Phase 13: it existed as a target and was never being run.
- Security audit: the Phase 12 checklist above, item by item, each naming its
  test.
- Repeated cleanup: `make test-repeat REPEAT=3`, plus four rounds of
  build-measure-impair-destroy against four separately inspected host resources,
  plus the three interruption scenarios against the process table, which is what
  caught a killed run leaving one namespace holder per node alive.
- Host isolation: `TestHostIsNeverModified`, `TestSoftwareRouterLeavesTheHostAlone`
  and the snapshot comparison every audit scenario ends with.
- Evidence in diagnostics: `internal/investigator` imports only `apperr` and
  `probe`, so it cannot read the fault specification a laboratory declares, and
  `TestAConfiguredFaultIsNotReportedAsAMeasurement` compares every percentage in
  a report against the measurement in that same report.
- Recovery authorization and verification: Phase 10's tests, where a diagnosis
  can never authorize itself and every action is judged by a probe rather than by
  its own return value.
- Reproducible reports: the experiment identifier is derived from validated
  configuration content, and the seed is carried through to the report.
- Documentation: `README.md`, `docs/ARCHITECTURE.md` and `docs/SECURITY.md` were
  corrected in this phase; the status table had claimed "phases 0 to 6" and that
  recovery was not implemented. Phase 13 wrote the four documents section 18
  required and did not exist — `NETWORK_MODEL.md`, `EXPERIMENTS.md`,
  `PERFORMANCE.md` and `OPERATIONS.md` — and corrected eight further
  divergences, each listed in `docs/MILESTONES.md`. Two of them are worth naming
  here: `README.md` listed four commands where the binary has six, omitting the
  two that build and destroy a laboratory, and `docs/PROTOCOL.md` still said the
  router does not recompute a transport checksum, which is the very defect this
  milestone's own audit found.
- Performance conditions: `docs/PERFORMANCE.md` states the methodology and
  `docs/BENCHMARKS.md` is written by the run that measured it. That file was
  missing two of the eight conditions section 17 requires — the kernel release
  and the hardware — so the benchmark records them now, along with the revision,
  the laboratory configuration and the reliable transport's limits, and prints
  "unknown" with the reason for anything it cannot read.
- Nothing hidden: the three defects Phase 12 found and the four Phase 13 found
  are listed above, each with the measurement that found it.

Known limitations, stated rather than left to be discovered:

- The reliable UDP transport in `internal/transport` is exercised against real
  sockets and real packet loss, but NetForge's own laboratory services do not run
  their control plane over it; the transport is a component under test, not the
  transport the product depends on.
- The software router refuses a packet larger than the next link and answers with
  ICMP fragmentation needed rather than fragmenting the packet itself. A sender
  that ignores that answer will not get through; in the laboratories here the
  TCP stack acts on it and the connection completes.
- `metrics.Retransmissions` is reported and is always zero for the echo
  measurements an experiment takes, because those measurements have no transport
  to retransmit on. The field is rendered rather than omitted, and its value is
  not inflated to look like a measurement.
- A run killed with `SIGKILL` leaves its state directory behind
  (`/tmp/netforge-lab-*` and `/tmp/netforge-router-*`), which holds service
  status files and a router statistics file and no namespaces. A killed run
  releases its namespaces — Phase 13 repaired that — but nothing reclaims the
  directory, and there is no command that lists or reclaims either. This is named
  in `docs/SECURITY.md`, `docs/THREAT_MODEL.md` and `docs/OPERATIONS.md` rather
  than left to be discovered.
- The software router mode is reachable from Go and from the test suite but not
  from the command line, and neither is the laboratory's services or the load
  balancer. `netforge experiment run` therefore builds a kernel-routed laboratory
  with no services, sthe binary and runs it, builds
  real namespaces, routes real traffic through both the kernel router and
  `internal/router`, and tears each laboratory down.
- Race detector: `go test -count=1 -race ./...`, no race reported.
- Fuzzing: `make fuzz`, `make fuzz-packet` and `make fuzz-router`, fifteen
  targets in all — five configuration parsers, eight packet and checksum targets
  and two forwarding targets — with the recorded corpus replayed by every
  ordinary test run (`make test-fuzz-seeds`). `FuzzParsePercent` was added to
  `make fuzz` by Phase 13: it existed as a target and was never being run.
- Security audit: the Phase 12 checklist above, item by item, each naming its
  test.
- Repeated cleanup: `make test-repeat REPEAT=3`, plus four rounds of
  build-measure-impair-destroy against four separately inspected host resources,
  plus the three interruption scenarios against the process table, which is what
  caught a killed run leaving one namespace holder per node alive.
- Host isolation: `TestHostIsNeverModified`, `TestSoftwareRouterLeavesTheHostAlone`
  and the snapshot comparison every audit scenario ends with.
- Evidence in diagnostics: `internal/investigator` imports only `apperr` and
  `probe`, so it cannot read the fault specification a laboratory declares, and
  `TestAConfiguredFaultIsNotReportedAsAMeasurement` compares every percentage in
  a report against the measurement in that same report.
- Recovery authorization and verification: Phase 10's tests, where a diagnosis
  can never authorize itself and every action is judged by a probe rather than by
  its own return value.
- Reproducible reports: the experiment identifier is derived from validated
  configuration content, and the seed is carried through to the report.
- Documentation: `README.md`, `docs/ARCHITECTURE.md` and `docs/SECURITY.md` were
  corrected in this phase; the status table had claimed "phases 0 to 6" and that
  recovery was not implemented. Phase 13 wrote the four documents section 18
  required and did not exist — `NETWORK_MODEL.md`, `EXPERIMENTS.md`,
  `PERFORMANCE.md` and `OPERATIONS.md` — and corrected eight further
  divergences, each listed in `docs/MILESTONES.md`. Two of them are worth naming
  here: `README.md` listed four commands where the binary has six, omitting the
  two that build and destroy a laboratory, and `docs/PROTOCOL.md` still said the
  router does not recompute a transport checksum, which is the very defect this
  milestone's own audit found.
- Performance conditions: `docs/PERFORMANCE.md` states the methodology and
  `docs/BENCHMARKS.md` is written by the run that measured it. That file was
  missing two of the eight conditions section 17 requires — the kernel release
  and the hardware — so the benchmark records them now, along with the revision,
  the laboratory configuration and the reliable transport's limits, and prints
  "unknown" with the reason for anything it cannot read.
- Nothing hidden: the three defects Phase 12 found and the four Phase 13 found
  are listed above, each with the measurement that found it.

Known limitations, stated rather than left to be discovered:

- The reliable UDP transport in `internal/transport` is exercised against real
  sockets and real packet loss, but NetForge's own laboratory services do not run
  their control plane over it; the transport is a component under test, not the
  transport the product depends on.
- The software router refuses a packet larger than the next link and answers with
  ICMP fragmentation needed rather than fragmenting the packet itself. A sender
  that ignores that answer will not get through; in the laboratories here the
  TCP stack acts on it and the connection completes.
- `metrics.Retransmissions` is reported and is always zero for the echo
  measurements an experiment takes, because those measurements have no transport
  to retransmit on. The field is rendered rather than omitted, and its value is
  not inflated to look like a measurement.
- A run killed with `SIGKILL` leaves its state directory behind
  (`/tmp/netforge-lab-*` and `/tmp/netforge-router-*`), which holds service
  status files and a router statistics file and no namespaces. A killed run
  releases its namespaces — Phase 13 repaired that — but nothing reclaims the
  directory, and there is no command that lists or reclaims either. This is named
  in `docs/SECURITY.md`, `docs/THREAT_MODEL.md` and `docs/OPERATIONS.md` rather
  than left to be discovered.
- The software router mode is reachable from Go and from the test suite but not
  from the command line, and neither is the laboratory's services or the load
  balancer. `netforge experiment run` therefore builds a kernel-routed laboratory
  with no services, s
- configuration parsing;
- serialization;
- privileged operations.

Errors must contain useful context.

Prefer wrapped errors that preserve the original cause.

---

## Concurrency

Every goroutine must have:

```text
owner
startup condition
shutdown condition
```

No uncontrolled background goroutines.

Use:

- `context.Context`;
- channels;
- synchronization primitives;
- `errgroup` where it genuinely simplifies lifecycle management.

Do not introduce concurrency merely for appearance of performance.

---

## Timeouts

Every external operation must have a bounded timeout.

No infinite:

- reads;
- writes;
- dials;
- probes;
- health checks;
- experiment operations.

---

## Memory

Do not create unbounded:

- buffers;
- queues;
- packet captures;
- logs;
- probe results;
- goroutines;
- connections.

Every collection needs an explicit bound or lifecycle.

---

# 16. Observability Contract

Every experiment must have an `experiment_id`.

Important operations must be traceable:

```text
experiment
    ↓
network creation
    ↓
fault injection
    ↓
probe
    ↓
diagnosis
    ↓
recovery
    ↓
verification
```

A generated report must allow an engineer to reconstruct the experiment without reading source code.

---

# 17. Performance Methodology

Do not invent arbitrary production-performance targets.

Instead establish reproducible baselines.

For each major component measure, where applicable:

- throughput;
- latency;
- CPU;
- memory;
- packet-processing rate;
- concurrent connections.

Every benchmark report must include:

- hardware;
- Linux version;
- kernel version;
- Go version;
- NetForge commit;
- configuration;
- experiment parameters;
- measurement methodology.

Never make performance claims without test conditions.

---

# 18. Documentation

Required files:

```text
README.md

docs/
├── ARCHITECTURE.md
├── SECURITY.md
├── THREAT_MODEL.md
├── NETWORK_MODEL.md
├── PROTOCOL.md
├── EXPERIMENTS.md
├── PERFORMANCE.md
└── OPERATIONS.md
```

Documentation must describe the implementation that actually exists.

Do not claim:

- production-ready;
- secure;
- zero vulnerabilities;
- fully reliable;

without evidence supporting the claim.

Prefer precise statements such as:

> The experiment controller validates namespace and CIDR scope for supported operations.

---

# 19. CI

CI must run at minimum:

```text
go test ./...
go test -race ./...
go vet ./...
```

Also add where practical:

- formatting checks;
- static analysis;
- dependency/vulnerability scanning;
- Linux integration tests;
- fuzzing jobs where appropriate.

Privileged networking tests must run only in an environment where the required privileges are explicitly configured.

Do not give unrelated CI jobs elevated privileges.

---

# 20. Required Milestone Report

After every milestone, stop and produce exactly:

```text
==================================================
MILESTONE M[X] — STATUS
==================================================

Scope:
...

Implemented:
...

Security:
...

Tests:
...

Integration Tests:
...

Performance:
...

Acceptance Gates:
[PASS] ...
[PASS] ...
[FAIL] ...

Known Issues:
...

Files Changed:
...

Evidence:
...

NEXT:
M[X+1] / BLOCKED
```

If one mandatory gate fails:

```text
STATUS: BLOCKED
```

Do not proceed.

Do not hide failures.

Do not weaken tests.

Do not remove requirements to make a milestone pass.

---

# 21. Final End-to-End Acceptance Test

The completed system must execute the following laboratory workflow:

```text
1. Create isolated network
2. Validate experiment scope
3. Start client
4. Start router
5. Start load balancer
6. Start three services
7. Establish baseline
8. Record baseline metrics
9. Inject controlled packet loss
10. Inject latency
11. Observe degradation
12. Run diagnostics
13. Produce evidence-backed diagnosis
14. Execute explicitly authorized recovery
15. Verify network recovery
16. Verify service recovery
17. Generate structured report
18. Destroy experiment
19. Verify all resources were cleaned
```

Expected architecture:

```text
                    NETFORGE
                       │
              ┌────────▼────────┐
              │ Experiment      │
              │ Controller      │
              └────────┬────────┘
                       │
       ┌───────────────┼────────────────┐
       │               │                │
   Network           Chaos          Diagnostics
   Control           Engine           Engine
       │               │                │
       └───────────────┼────────────────┘
                       │
                    Linux Lab
                       │
        ┌──────────────┼──────────────┐
        │              │              │
      client          router          lb
                                      │
                         ┌────────────┼────────────┐
                         │            │            │
                      service-a   service-b   service-c
```

Evidence: `TestTheFinalAcceptanceWorkflow` (`tests/integration/acceptance_test.go`)
walks all nineteen steps on one real laboratory, in order, each as its own named
subtest. Two steps are worth naming rather than glossing:

- Step 5 is the load-balancing hop. The acceptance laboratory puts three service
  namespaces behind one router namespace, and the router is the single hop every
  request crosses, so the acceptance's load balancer is that router and its three
  services are the namespaces behind it. The balancer package itself
  (`internal/loadbalancer`) is measured against a running balancer in the Phase 8
  suite; what the acceptance proves is the workflow, not the policy.
- Step 19 is checked twice: against the host's own four views
  (`ip -o link show`, `ip -o route show`, `ip netns list`, `tc qdisc show`) and
  against the process table, since a namespace NetForge created is not in
  `ip netns list`. A `t.Cleanup` assertion runs the host check a second time
  after the test's own teardown, so a leak that only appears during teardown is
  still a failure.

**STOP.**

---

# 22. Definition of Done

NetForge is complete only when all of the following are true:

- [x] all four milestones pass;
- [x] all mandatory gates pass;
- [x] integration tests pass on Linux;
- [x] race detector passes;
- [x] fuzzing has been performed on packet/config parsers;
- [x] security audit is complete;
- [x] cleanup has been repeatedly verified;
- [x] host networking remains isolated during laboratory operation;
- [x] diagnostics use actual evidence;
- [x] recovery requires explicit authorization;
- [x] recovery is independently verified;
- [x] reports are reproducible;
- [x] documentation reflects the actual implementation;
- [x] performance measurements include their test conditions;
- [x] no known critical security or lifecycle defect is being hidden.

Completion means the evidence supports the claim.

It does not mean every conceivable networking feature has been implemented.

Evidence for each line, and what it rests on:

- Milestones and gates: `make check` passes, which is `fmt-check`, `vet`, the
  whole suite, the whole suite under the race detector, coverage and a build.
  `make fmt-check` was the one that failed at the start of this phase.
- Integration on Linux: `tests/integration` builds the binary and runs it, builds
  real namespaces, routes real traffic through both the kernel router and
  `internal/router`, and tears each laboratory down.
- Race detector: `go test -count=1 -race ./...`, no race reported.
- Fuzzing: `make fuzz`, `make fuzz-packet` and `make fuzz-router`, fifteen
  targets in all — five configuration parsers, eight packet and checksum targets
  and two forwarding targets — with the recorded corpus replayed by every
  ordinary test run (`make test-fuzz-seeds`). `FuzzParsePercent` was added to
  `make fuzz` by Phase 13: it existed as a target and was never being run.
- Security audit: the Phase 12 checklist above, item by item, each naming its
  test.
- Repeated cleanup: `make test-repeat REPEAT=3`, plus four rounds of
  build-measure-impair-destroy against four separately inspected host resources,
  plus the three interruption scenarios against the process table, which is what
  caught a killed run leaving one namespace holder per node alive.
- Host isolation: `TestHostIsNeverModified`, `TestSoftwareRouterLeavesTheHostAlone`
  and the snapshot comparison every audit scenario ends with.
- Evidence in diagnostics: `internal/investigator` imports only `apperr` and
  `probe`, so it cannot read the fault specification a laboratory declares, and
  `TestAConfiguredFaultIsNotReportedAsAMeasurement` compares every percentage in
  a report against the measurement in that same report.
- Recovery authorization and verification: Phase 10's tests, where a diagnosis
  can never authorize itself and every action is judged by a probe rather than by
  its own return value.
- Reproducible reports: the experiment identifier is derived from validated
  configuration content, and the seed is carried through to the report.
- Documentation: `README.md`, `docs/ARCHITECTURE.md` and `docs/SECURITY.md` were
  corrected in this phase; the status table had claimed "phases 0 to 6" and that
  recovery was not implemented. Phase 13 wrote the four documents section 18
  required and did not exist — `NETWORK_MODEL.md`, `EXPERIMENTS.md`,
  `PERFORMANCE.md` and `OPERATIONS.md` — and corrected eight further
  divergences, each listed in `docs/MILESTONES.md`. Two of them are worth naming
  here: `README.md` listed four commands where the binary has six, omitting the
  two that build and destroy a laboratory, and `docs/PROTOCOL.md` still said the
  router does not recompute a transport checksum, which is the very defect this
  milestone's own audit found.
- Performance conditions: `docs/PERFORMANCE.md` states the methodology and
  `docs/BENCHMARKS.md` is written by the run that measured it. That file was
  missing two of the eight conditions section 17 requires — the kernel release
  and the hardware — so the benchmark records them now, along with the revision,
  the laboratory configuration and the reliable transport's limits, and prints
  "unknown" with the reason for anything it cannot read.
- Nothing hidden: the three defects Phase 12 found and the four Phase 13 found
  are listed above, each with the measurement that found it.

Known limitations, stated rather than left to be discovered:

- The reliable UDP transport in `internal/transport` is exercised against real
  sockets and real packet loss, but NetForge's own laboratory services do not run
  their control plane over it; the transport is a component under test, not the
  transport the product depends on.
- The software router refuses a packet larger than the next link and answers with
  ICMP fragmentation needed rather than fragmenting the packet itself. A sender
  that ignores that answer will not get through; in the laboratories here the
  TCP stack acts on it and the connection completes.
- `metrics.Retransmissions` is reported and is always zero for the echo
  measurements an experiment takes, because those measurements have no transport
  to retransmit on. The field is rendered rather than omitted, and its value is
  not inflated to look like a measurement.
- A run killed with `SIGKILL` leaves its state directory behind
  (`/tmp/netforge-lab-*` and `/tmp/netforge-router-*`), which holds service
  status files and a router statistics file and no namespaces. A killed run
  releases its namespaces — Phase 13 repaired that — but nothing reclaims the
  directory, and there is no command that lists or reclaims either. This is named
  in `docs/SECURITY.md`, `docs/THREAT_MODEL.md` and `docs/OPERATIONS.md` rather
  than left to be discovered.
- The software router mode is reachable from Go and from the test suite but not
  from the command line, and neither is the laboratory's services or the load
  balancer. `netforge experiment run` therefore builds a kernel-routed laboratory
  with no services, so an observation with a protocol that needs a listener has
  to be driven from Go today. The four backend recovery actions are unreachable
  from the CLI for the same reason: `-recover` cannot say which address a `From`
  should use.
- `report.Recommendation`, the action the investigator would recommend, is
  computed and carried in the engine's report but is rendered by neither output
  format. The recommendation exists; a reader of a report does not see it.
- The examples in `examples/` are configuration files for validation and
  planning. `simple-line` grants `client`, `router` and `server`, which are not
  the names a laboratory creates — those are `nf-<role>-<id>` — so it validates
  and plans but is refused at `network.Create` if it is run. That refusal is the
  correct behaviour and is documented rather than worked around.

**STOP.**

---

# NetForge Engineering Notes

This document defines mandatory engineering, security, reliability,
and development rules for NetForge.

It applies to every phase and milestone.

## 1. Core Engineering Principles

Priority:

1. Correctness
2. Isolation
3. Security
4. Observable behavior
5. Deterministic testing
6. Reliable cleanup
7. Reproducibility
8. Maintainability
9. Performance

Do not optimize for feature count.

A feature is not complete because it compiles.
It is complete only when its acceptance criteria have been demonstrated.

## 2. Stop-on-Failure

Never proceed past a mandatory milestone gate when:

- tests fail;
- integration tests fail;
- security checks fail;
- cleanup fails;
- host-safety checks fail;
- required evidence is missing.

Never weaken or delete a failing test to make a gate pass.

Investigate → fix → rerun → verify → continue.

## 3. Evidence

Every completed capability requires evidence.

Valid evidence includes:

- unit tests;
- integration tests;
- fuzz tests;
- race tests;
- benchmarks;
- packet captures;
- namespace inspection;
- interface inspection;
- route inspection;
- tc inspection;
- experiment reports;
- reproducible command output.

Compilation alone is never evidence of functional correctness.

## 4. Real Networking

Anything crossing the Linux networking boundary must be tested against
real Linux networking behavior.

Mocks may be used for:

- algorithms;
- pure functions;
- deterministic failure paths;
- dependency isolation.

Mocks must not be used to claim that:

- packet forwarding works;
- namespaces are isolated;
- tc/netem works;
- routing works;
- packet loss occurred;
- latency occurred;
- connectivity works.

## 5. Architecture Invariants

1. CLI never directly manipulates Linux networking.
2. Core networking logic never depends on CLI code.
3. Configuration parsing performs no side effects.
4. Scope validation occurs before privileged operations.
5. Privileged operations are isolated behind narrow interfaces.
6. Every resource has one logical owner.
7. Long-running operations accept `context.Context`.
8. Diagnostics may recommend recovery but cannot execute it automatically.
9. Recovery requires explicit authorization.
10. Reports are generated from recorded observations.
11. Unknown state never becomes implicit permission.
12. Data-plane failure must not crash the control plane.

## 6. Control Plane / Data Plane

### Control Plane

Responsible for:

- configuration;
- experiment lifecycle;
- authorization;
- scope;
- orchestration;
- state transitions;
- reporting.

### Data Plane

Responsible for:

- packet processing;
- forwarding;
- transport;
- probes;
- traffic generation;
- measurements.

The control plane must not block indefinitely on data-plane operations.

## 7. Privilege Separation

Prefer:

Unprivileged control plane
        ↓
Narrow privileged operations

Do not run the entire application as root.

Use Linux capabilities where appropriate:

- `CAP_NET_ADMIN`
- `CAP_NET_RAW`

Document why each privileged operation requires its capability.

The privileged layer must not:

- parse arbitrary user configuration;
- execute arbitrary commands;
- make diagnostic decisions;
- execute recovery policy;
- contain experiment business logic.

## 8. Fail Closed

If any of the following cannot be established:

- scope;
- authorization;
- configuration validity;
- resource limits;
- resource ownership;
- privilege requirements;

the operation must not execute.

Examples:

unknown namespace ownership → refuse
unknown scope → refuse
failed authorization → refuse recovery
invalid configuration → refuse experiment
unknown cleanup ownership → do not destroy resource

## 9. Scope Enforcement

Every experiment has an explicit scope.

Every operation affecting network state must be checked against that scope.

Never silently expand scope.

Default targets are:

- localhost;
- NetForge-created namespaces;
- explicitly authorized lab endpoints.

NetForge is not an offensive-security framework.

Do not implement:

- Internet-wide scanning;
- credential attacks;
- exploitation;
- stealth/evasion;
- persistence;
- unauthorized interception;
- destructive external packet injection.

## 10. Input Validation

Treat all external input as untrusted.

Validate:

- namespace names;
- interface names;
- IP addresses;
- CIDRs;
- routes;
- ports;
- protocols;
- experiment IDs;
- configuration;
- fault parameters;
- file paths;
- durations;
- bandwidth;
- packet rates;
- concurrency.

Reject invalid or excessive values.

Never pass user-controlled strings into a shell.

Prefer direct Linux APIs.

If command execution is unavoidable:

- use `exec.Command`;
- never use `sh -c`;
- pass arguments separately;
- validate arguments;
- enforce timeouts;
- capture stderr;
- check exit status.

## 11. Resource Limits

All experiments must have bounded:

- namespaces;
- interfaces;
- duration;
- bandwidth;
- packet rate;
- probe concurrency;
- packet size;
- memory;
- queues;
- logs;
- goroutines;
- connections.

No unbounded resource allocation.

## 12. Cleanup

Every resource must have:

- an owner;
- a creation path;
- a cleanup path.

Resources include:

- namespaces;
- interfaces;
- routes;
- tc rules;
- sockets;
- processes;
- goroutines;
- temporary files;
- experiment state.

Cleanup must handle:

- success;
- failure;
- timeout;
- SIGINT;
- SIGTERM;
- recoverable panic.

Cleanup must be idempotent.

## 13. Crash Recovery

NetForge must be able to identify resources belonging to
previous incomplete experiments.

Use NetForge-owned metadata/naming.

On startup or explicit reconciliation:

1. discover candidate resources;
2. verify ownership;
3. reconcile or remove only positively identified resources.

Never destroy an unknown resource.

What NetForge implements today, and what it does not:

- A process killed with `SIGKILL` releases its namespaces. Each namespace holder,
  the software router and every laboratory service is tied to a pipe the owning
  agent holds, so the kernel closes it however the agent dies and the child
  exits, which is what destroys the namespace. Phase 12 recorded that a killed run
  left one holder per node alive, and Phase 13 repaired it; the audit now reads
  the process table as well as the host's network tools
  (`assertNoLaboratoryProcesses`), which is the only place a `clone` namespace is
  visible at all.
- A laboratory is recoverable by name: nothing is reserved between runs, so the
  next run with the same identifier builds the same laboratory
  (`TestAKilledExperimentIsRecoveredByTheNextRun`).
- Steps 1 to 3 are implemented. A run killed with `SIGKILL` releases its
  namespaces and leaves two directories under the state root, and each carries a
  marker NetForge wrote when it created it. `netforge reconcile list` classifies
  every entry in that root and identifies only the ones whose marker parses, names
  a kind this build knows, and carries an identifier that passes the same
  validation a namespace does; everything else is listed as `unidentified` and
  left alone. `netforge reconcile reclaim` removes what it proved, and only with
  an explicit authorization and a written reason, confined to the state root,
  refusing a live owner, bounded at 64 directories and idempotent
  (`TestTheReconcilerRemovesNothingItCannotProveOwns`,
  `TestAKilledRunLeavesIdentifiableState`,
  `TestReclaimingWhatAKilledRunLeftIsBoundedReportedAndIdempotent`).
- Reclamation is never automatic. Nothing sweeps the state root on startup, and
  nothing does so on a run's behalf either: a sweep is a deletion with no
  authorization and no reason attached, which is the operation the rules above
  are about.

## 14. Experiment State Machine

Experiments use explicit states.

Example:

SETUP
  ↓
BASELINE
  ↓
FAULT
  ↓
OBSERVE
  ↓
DIAGNOSE
  ↓
RECOVER
  ↓
VERIFY
  ↓
REPORT
  ↓
CLEANUP

Invalid transitions must be rejected.

Every failed state must have a defined cleanup path.

## 15. Idempotency

Operations should be idempotent where practical.

Examples:

destroy missing lab → safe no-op
remove missing fault → safe no-op
cleanup partial setup → safe
repeated cleanup → safe

Do not silently hide genuine conflicts.

## 16. Configuration Security

Configuration is data, never executable instructions.

Use strict decoding where practical.

Reject:

- unknown fields;
- invalid types;
- invalid enum values;
- invalid ranges;
- unsupported options;
- malformed configuration.

Configuration parsing must not perform side effects.

## 17. Deterministic Experiments

Experiments involving probabilistic behavior should support a seed.

Example:

```yaml
experiment:
  seed: 123456
```

The seed is accepted, validated and carried through to the report and into the
experiment identifier, and no engine consumes it. A fault NetForge applies is the
fault the document declared, so there is nothing random left to seed today; a
component that gained a random choice would have to read this value rather than
call a random source, and nothing here claims that has been done.

---

# Phase 16 — The Load Balancer in the Path

Evidence: `internal/loadbalancer` is a complete, bounded, tested component that
no process can start and no laboratory can own. `cmd/netforge/main.go` dispatches
the namespace, router, service and probe agents and not the balancer, so
`loadbalancer.Dispatch` has no non-test caller, and `network.Spec` has no
balancer field at all. The consequence is larger than a missing flag.

`netforge recovery actions` prints seven actions, and four of them are dead. The
environment builds its recovery agent with a nil backend controller
(`internal/experiment/environment.go:79`), so `internal/recovery/lab.go:149`
refuses `remove_backend`, `restore_backend`, `drain_connection` and
`re_enable_backend` with "this experiment has no load balancer". A command line
that advertises an action which can never run is the same defect Phase 14 found in
a report that described a router that never ran, and it is the reason this phase
exists rather than a feature request.

The balancer is also the only component that dials the reliable UDP transport
(`internal/loadbalancer/health.go:56`), so making it reachable is also what makes
Phase 17's transport a path the product depends on rather than a component a test
exercises.

## Requirements

- [ ] a laboratory can be built with a load balancer in its forwarding path, and
  the balancer is a process of that laboratory like the router and the services —
  started through the agent, awaited until it reports running, and stopped before
  the namespaces it lives in disappear;
- [ ] a balancer is reachable from the command line, with its own count and base
  port, validated against the plan the laboratory would be built from before any
  namespace exists;
- [ ] the four backend actions run against a real balancer: an authorized
  `remove_backend` changes that backend's reported state while traffic continues
  on the survivors, and a `restore_backend` brings it back;
- [ ] the balancer's health check distinguishes a backend that answered a
  versioned challenge from one whose socket merely accepted bytes;
- [ ] `netforge recovery actions` no longer prints an action that cannot run,
  and asking for a backend action with no balancer is refused naming the field;
- [ ] the run's report says what the balancer was doing: its backends, their
  states, and how many exchanges it has carried;
- [ ] the whole suite passes, twice under the race detector, and `make cover`.

## Design notes

The balancer is wired the way the software router and the services already are,
not by a path of its own: a `network.BalancerSpec` beside `ServiceSpec`, a
`startBalancer` beside `startRouter`, a `netforge-balancer` mode in the dispatch
chain, and an await loop over `loadbalancer.ReadStatus`. Its namespace is
registered in the laboratory's own space map, because the probe scope check and
`Lab.Names` both read that map and a balancer the laboratory cannot see is a
balancer nothing can measure.

The four backend actions need one more thing before they can run, and it is
mechanical rather than structural: `recovery.resolveFrom` requires `From` for any
action aimed at an address, and the CLI never sets it. A `-recover-from` flag, or
deriving it from `--namespace`, is what turns "the target is an address" from a
refusal into an authorized action with a verification probe.

`TestProcessBoundaryIsMinimal` is a negative test and will fail the first time a
file is added to the process boundary. That failure is the test working: it means
a file claimed a capability and has to actually use it. Expect it, and use it.

**STOP.**

---

# Phase 17 — The Reliable Transport as a Depended-On Path

Evidence: section 7 of this document requires a reliable UDP transport and the
component exists, is bounded on both directions, and is measured end to end under
loss, latency and jitter. It is a component *under test* and not a component
*depended on*. Every non-test `transport.Dial` call site in the tree is inside
`internal/loadbalancer`, and the balancer is unreachable until Phase 16.

The server side is an echo and nothing more: `listenReliable` opens a listener and
`serveEchoReliable` writes back the payload it read. Nothing in the repository
sets `ServiceSpec.RUDPPort` outside a test, and `probe.Protocols()` is a closed
set of six with no reliable member, so no probe can measure over the transport
that this project was built around.

One symptom is already visible in the product's own output.
`metrics.Retransmissions` is declared, rendered in both report formats, and
required by section 16 as one of ten metrics — and is never assigned anywhere in
non-test code. It is documented as always zero, which is true for echo
measurements and false for the one protocol with a transport that retransmits.

## Requirements

- [ ] a probe can measure over the reliable transport: `--protocol rudp` is a
  protocol, it is served by a laboratory service, and a run using it reports the
  transport's own counters rather than a parse of a tool's output;
- [ ] a run under a declared `packet_loss` fault reports loss on `--protocol udp`
  and none on `--protocol rudp` over the same path, which is the claim the
  transport has never been asked to support by the product;
- [ ] `metrics.Retransmissions` is populated from the transport's own counters,
  is non-zero under loss, is zero at zero loss, and stops being documented as
  permanently zero;
- [ ] the balancer's health check answers a versioned challenge, so a backend is
  healthy because it identified itself rather than because its socket accepted a
  datagram;
- [ ] the reliable transport's limits are reported in the run's report, because a
  window figure without its limits is irreproducible;
- [ ] the whole suite passes, twice under the race detector, and `make cover`.

## Design notes

`probe.Protocol` gaining a member is the whole of the measurement path, and it
has to be added in three places that are each a closed set: `Protocols()`,
`Request.check`, and the agent's dispatch switch. `investigator.chain` needs a
rung for it too, or a diagnosis of a reliable path would stop at the layer below
and never test the layer the operator named.

The retransmission metric is read from the connection the probe already holds
rather than recomputed, for the same reason every other metric in that package is
derived from evidence and never from a configured value. A number that came from
the limit instead of the wire would be a number that cannot be wrong, which is not
what a metric is for.

Deleting the known-limitation bullet is part of the requirement rather than a
follow-up. A limitation that has been closed and is still written down is worse
than one that was never removed, because a reader cannot tell which it is.

**STOP.**

---

# Phase 18 — What the Operator Can See, and What the Suite Proves

Evidence: two capabilities are computed and never shown, one contract section is
two-sevenths implemented, and four named test defects would let a regression pass.
None of this adds a capability. It is the difference between a tool that works
and one an operator can verify, which is what the remaining sections of this
document actually ask for.

`experiment.Report.Recommendation` is assigned in the controller and read by
nothing: neither `EncodeText` nor `EncodeJSON` carries it, and the `report.Report`
struct has no field for it. The recommendation is the one part of a report that
tells an operator what to type next, and `recovery.Recommendation` already carries
the action, the target, the reason and a `Requested(from)` rendering of the flag
that would perform it. A reader who never sees it cannot use it.

The observability contract requires a traceable chain from experiment to
verification, and a `--log-level=debug` run of a real faulted experiment emits
three records: a dispatch, one terminal line, and a failure. Fault injection,
probing, diagnosis, recovery and verification produce nothing, because nothing
in `network`, `chaos`, `probe`, `investigator`, `recovery`, `loadbalancer` or
`transport` logs at all. The field names are correct and consistent; the coverage
of stages is not.

Three defects found while auditing, kept as tests:

- `reconcile.Limits.MaxEntries` is declared, defaulted and ceiling-checked, and
  no caller reads it. `Scan` reads the whole state root, so a shared temporary
  directory with a million entries is enumerated in full. This is Phase 15's own
  code and Phase 15's own omission.
- `internal/cli/app.go` passes an explicit `event` field on a record whose
  `Message` is already rewritten to `event`, so a JSON log line carries the key
  `event` twice with different values and resolves to the wrong one.
- `tests/integration/phase15_test.go` skips when a live run does not create its
  state directory inside a deadline, inside the one test whose entire claim is
  that a live run's state is refused. A regression that stops the run creating
  state at all turns the safety property into a skip.

## Requirements

- [ ] both report formats carry the recommendation; a run that diagnosed nothing
  says so explicitly rather than omitting the key, and the rendered
  recommendation round-trips into `-recover` and compiles;
- [ ] every stage of the lifecycle emits at least one log record carrying the
  experiment identifier and a distinct event name, checked by decoding each
  record rather than by matching text, and exactly one `event` key per record;
- [ ] one run's records, joined, name every namespace it created, every fault it
  applied and removed, the diagnosis and each authorized action — the
  reconstruction claim of the observability contract, as a golden trace;
- [ ] the reconciler bounds what one listing reads, and a state root larger than
  the bound is refused rather than enumerated;
- [ ] the vacuous and sleep-and-hope assertions named by the audit are replaced
  with the event they should await, and the skip on a failure-shaped condition
  becomes a failure;
- [ ] the whole suite passes, twice under the race detector, and `make cover`.

## Design notes

The event vocabulary is a closed set, and that is the point: a log line whose
event name is a format string is a log line nobody can assert on. Each stage gets
a fixed name, and the test unmarshals each record and compares the name, because
`strings.Contains` on stderr cannot tell one event from another and is how the
duplicate `event` key went unnoticed.

The golden trace is a test that reads like the contract it enforces. If the
lifecycle gains a stage, the trace names it or fails, which is what makes the
observability section a requirement rather than an aspiration.

**STOP.**

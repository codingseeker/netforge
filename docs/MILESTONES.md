# NetForge Milestone Reports

Section 20 of [todo.md](../todo.md) requires a report after every milestone,
in a fixed format, with a gate that is either passed or failed. This file holds
those reports. They are not rewritten after the fact: each states what was
implemented, what was measured and which gates were run, so a reader can check a
claim against the test named beside it.

Every gate below was run on Linux/amd64, kernel 6.19.13, Go 1.25.12, unprivileged,
in this repository.

---

## MILESTONE 1 — PASS

```text
==================================================
MILESTONE M1 — PASS
==================================================

Scope:
  Packet and protocol primitives, and an isolated Linux network. The foundation
  a later phase could measure: bytes NetForge can parse and build, namespaces it
  owns, and real traffic crossing them.

Implemented:
  internal/packet       Ethernet II, IPv4, ICMP, UDP, TCP and DNS codecs, with
                        bounds, checksum verification and a marked-unchecked
                        result rather than a trusted one
  internal/iface        veth, address, route, MTU and inspection operations
  internal/namespace    namespace lifecycle and the laboratory agent
  internal/network      topology planning, addressing, scope enforcement,
                        lifetime and cleanup
  internal/apperr       the error classification every layer shares
  internal/cleanup      ordered, idempotent cleanup stack
  internal/logging      structured logging on stderr
  internal/cli          command parsing, dispatch, help, exit codes, signals
  internal/config       strict YAML decoding and validation

Security:
  No root, no ambient capability, no shell. Every child process is started by
  internal/namespace and is given a bare environment; tests/security/source_test.go
  bans the shell in every file and lists every file allowed to execute anything.
  A laboratory is a user namespace NetForge created, and the only host object it
  mutates is a veth pair created inside it.

Tests:
  Unit tests per package. tests/integration builds real namespaces and carries
  real traffic between them.

Integration Tests:
  Client to server through the kernel router, ttl=63, no loss
  (TestGate2TrafficCrossesTheRouter). Host links, routes and ip_forward identical
  before and after (TestHostIsNeverModified).

Performance:
  No figure claimed. No measurement surface existed to measure.

Acceptance Gates:
  [PASS] go test ./...
  [PASS] go test -race ./...
  [PASS] go vet ./...
  [PASS] make fmt-check

Known Issues:
  The scope of the first milestone was the network, so nothing here yet
  forwards a frame NetForge itself wrote. internal/packet parses what the kernel
  hands it; it does not yet inject.

Files Changed:
  cmd/netforge, internal/{apperr,cleanup,cli,config,iface,logging,namespace,
  network,packet}, tests/integration, tests/security, Makefile, README.md

Evidence:
  internal/packet/*_test.go for each codec and each bound.
  tests/integration/phase2_test.go for the topology and host isolation.

NEXT: M2
```

---

## MILESTONE 2 — PASS

```text
==================================================
MILESTONE M2 — PASS
==================================================

Scope:
  Controlled network failure. The ability to impair a real path on purpose,
  measure what a client sees, and remove the impairment again.

Implemented:
  internal/router   userspace router: routing table, ARP, forwarding, TTL, ICMP
                    errors, counters, MTU handling, checksum repair
  internal/chaos    fault model, tc/netem/tbf application, verified state, expiry
  internal/probe    bounded ICMP measurement from ping's own report, plus
                    namespace-local TCP, UDP, DNS, TLS and HTTP probes
  internal/service  real endpoints for a probe to measure

Security:
  A fault can only be applied to a namespace in the granted scope, and only
  through the device the laboratory reports (internal/chaos/engine.go). Every
  fault is verified against the kernel's own qdisc state before it is called
  active, so a reported fault is one the kernel installed.

Tests:
  Unit tests for the fault model, the tc argument vectors and the report parser.
  Integration tests that apply each fault to a real path and measure it.

Integration Tests:
  Packet forwarding, routing, ARP, ICMP, TCP, UDP, DNS, TLS and HTTP, each
  measured against a listener the laboratory really started
  (TestTheNetworkingChecklistIsMeasuredOnOneLaboratory). Packet loss, latency,
  jitter and bandwidth, each measured as a difference from a healthy baseline.

Performance:
  The fault engine reads back tc state after every change, so an impaired
  measurement costs one subprocess per change.

Acceptance Gates:
  [PASS] go test ./...
  [PASS] go test -race ./...
  [PASS] go vet ./...
  [PASS] make fmt-check

Known Issues:
  The software router could not carry a TCP handshake, because a sender whose
  stack offloads the transport sum writes only the seed into the bytes a packet
  socket shows. Every earlier router test pinged, and ICMP computes its own sum,
  so the defect was invisible from the code and from the suite that existed. It
  was found by Phase 12, repaired with packet.RefreshTransportChecksum, and is
  now covered by TestSoftwareRouterForwardsEveryProtocolTheProberSpeaks.

Files Changed:
  internal/{router,chaos,probe,service,packet}, tests/integration/{chaos,probe,
  router,phase2}_test.go

Evidence:
  tests/integration/router_test.go, chaos_test.go, probe_test.go.
  tests/integration/networking_audit_test.go for the checklist on one laboratory.

NEXT: M3
```

---

## MILESTONE 3 — PASS

```text
==================================================
MILESTONE M3 — PASS
==================================================

Scope:
  Evidence-based networking intelligence. Turning what was measured into a
  diagnosis that says only what the evidence supports.

Implemented:
  internal/investigator   the evidence pipeline: collect, hypothesize, test,
                          conclude, clamp. It imports only apperr and probe, so
                          it cannot read the fault specification the document
                          declared.
  internal/transport      reliable UDP: sequence numbers, cumulative
                          acknowledgements, sliding window, retransmission
                          timers, duplicate suppression, Reno congestion control

Security:
  A diagnosis cannot read the configuration that caused the failure, and cannot
  authorize anything. internal/investigator has no dependency on internal/experiment
  or internal/recovery.

Tests:
  internal/investigator_test.go drives the pipeline against a scripted wire and
  against real namespaces; internal/transport tests impose real loss,
  reordering and duplication on real sockets.

Integration Tests:
  Each of DNS, TCP, TLS and HTTP failure distinguished from the others, and
  measured loss distinguished from a broken layer
  (TestAnErrorStatusIsDiagnosedAtTheHTTPLayer,
   TestMeasuredLossIsDiagnosedAsADegradation).
  Reliable delivery over a path that drops, reorders and duplicates.

Performance:
  An investigation is bounded, including its repeat
  (TestAnInvestigationIsBounded).

Acceptance Gates:
  [PASS] go test ./...
  [PASS] go test -race ./...
  [PASS] go vet ./...
  [PASS] make fmt-check

Known Issues:
  The congestion controller infers loss only from a retransmission timeout,
  because a datagram gives nothing else away. TCP also retransmits on duplicate
  acknowledgements, which is why the benchmark's loss rows cost it less. A
  transport that inferred loss from reordering or duplication would narrow that
  gap; nothing here claims otherwise.

Files Changed:
  internal/{investigator,transport}, tests/integration/{investigator,transport}_test.go

Evidence:
  internal/investigator_test.go for the classification rules and the clamp.
  internal/transport/*_test.go and tests/integration/transport_test.go for the
  transport, including the large-transfer case that found the window defect.

NEXT: M4
```

---

## MILESTONE 4 — PASS

```text
==================================================
MILESTONE M4 — PASS
==================================================

Scope:
  The resilience platform, and the audit of it. Load balancing, the experiment
  lifecycle, authorized and verified recovery, structured observability, and the
  final security and reliability audit.

Implemented:
  internal/loadbalancer  backends, health checks, round robin, least
                         connections, consistent hashing, draining, a control file
  internal/experiment    the declarative nine-state lifecycle, compiled and
                         validated in full before anything runs
  internal/recovery      a closed set of seven actions, each authorized, scoped,
                         executed and verified by measurement
  internal/metrics       ten derived metrics, computed from measurements
  internal/report        text and JSON reports of a run
  internal/investigator  reachable from the lifecycle, still unable to authorize

Security:
  A diagnosis recommends and never acts. An action runs only with explicit
  consent, a reason, a scope check and a closed action name, and is judged by a
  probe rather than by its own return value.

Tests:
  The Phase 12 audit: tests/security for the boundaries before privileged work,
  and tests/integration for what an interruption leaves behind.

Integration Tests:
  TestTheFinalAcceptanceWorkflow walks all nineteen steps of section 21 on one
  laboratory: create, validate scope, start client, router, load-balancing hop
  and three services, baseline, record metrics, inject loss, inject latency,
  observe the degradation, diagnose, recover with consent, verify the network,
  verify the service, produce the report, destroy, and confirm nothing is left.

Performance:
  docs/PERFORMANCE.md records the methodology; docs/BENCHMARKS.md is written by
  the run that measured it, with the machine, the kernel, the Go version, the
  configuration and the transport limits attached to every figure.

Acceptance Gates:
  [PASS] go test ./...
  [PASS] go test -race ./...
  [PASS] go vet ./...
  [PASS] make fmt-check
  [PASS] make fuzz FUZZ_TIME=20s        four configuration targets
  [PASS] make fuzz-packet FUZZ_TIME=15s eight packet and checksum targets
  [PASS] make fuzz-router FUZZ_TIME=20s FuzzDecide, FuzzICMPError
  [PASS] make test-repeat REPEAT=3      the whole tree, three times

Known Issues:
  metrics.Retransmissions is always zero for the echo measurements an experiment
  takes, because those measurements have no transport to retransmit on. The
  field is rendered rather than omitted.
  The software router answers a packet larger than the next link with ICMP
  fragmentation needed rather than fragmenting it. A sender that ignores that
  answer will not get through.
  The reliable UDP transport is exercised against real sockets and real loss,
  but the laboratory services do not run their control plane over it: it is a
  component under test, not the transport the product depends on.

Files Changed:
  internal/{loadbalancer,experiment,recovery,metrics,report,investigator},
  tests/integration/{acceptance,audit,networking_audit,reliability_audit}_test.go,
  tests/security/*, docs/, README.md

Evidence:
  The Phase 12 checklist in todo.md, item by item, each naming its test.
  Section 22 below for the definition of done, line by line.

NEXT: Phase 13
```

---

## MILESTONE 4 — the audit found four defects

Recorded because none of them was visible from the code, and each is kept as a
test.

**1. The software router could not carry a TCP handshake.** It read frames from
an `AF_PACKET` socket, which shows the bytes as they are on the wire; a sender
whose stack offloads the transport sum writes only the seed of the checksum into
those bytes, so the router forwarded a checksum that did not verify and the far
end dropped every packet. ICMP computes its own sum, so echo requests crossed
fine, every existing router test pinged, and the defect was invisible from both
the code and the suite. The far end's own counters said what the wire would not:
three segments in, none out, all counted as checksum errors. Repaired by
`packet.RefreshTransportChecksum`, called from the forwarding path, and now
exercised against all three protocols (`FuzzRefreshTransportChecksum`,
`TestForwardingRepairsATransportChecksumTheSenderLeftUnfinished`,
`TestSoftwareRouterForwardsEveryProtocolTheProberSpeaks`).

**2. The router could be handed a frame its egress device cannot accept.** The
same socket shows a sender's unsegmented packet rather than the pieces the wire
would carry, so the router could only report a send failure after the fact. It
now compares the rebuilt frame against the configured MTU and answers the sender
with ICMP fragmentation needed and the size that applies, which lets a sender
send something smaller instead of learning the limit by silence
(`TestAPacketTooLargeForTheNextLinkIsAnsweredNotDropped`).

**3. A device name beginning with a dash was accepted.** A name is handed to `ip`
and `tc` as one argument of a privileged helper, and an argument beginning with a
dash is an option to that helper rather than a name, so `iface.ValidateName` now
refuses it (`TestNameValidation`).

**4. A killed run left the namespaces alive.** Phase 12 proved that an
interrupted run leaves the host's links, routes, namespaces and qdisc rules as it
found them, and every scenario it checked passed. The check was incomplete: a
namespace is not in `ip netns list`, because NetForge creates namespaces with
`clone` and addresses them by PID, so the only place a surviving one is visible
is the process holding it. A `SIGKILL`ed run left its namespace holders — one per
node — alive and reparented to init, each holding its network namespace open for
as long as the machine stayed up. The documentation claimed init would reap them;
nothing does, because a holder blocked on `SIGTERM` and nothing sends one to a
process that is already gone. The repair ties each holder, and each laboratory
service, to a pipe the owning agent holds, so the kernel closes it however the
owner dies and the child reads the end of it and exits
(`internal/namespace/agent.go`, `internal/service/service.go`). The audit now
asks the process table as well as the network tools
(`assertNoLaboratoryProcesses`), and with the repair reverted the same test
reports nine surviving holders after three kills.

Two measurement paths were also corrected rather than left to luck: reading the
router's counters was given twenty seconds instead of three, because a diagnostic
read that gives up under the race detector reports a working laboratory as a
broken one; and `TestAConfiguredFaultIsNotReportedAsAMeasurement` was failing
once in about six runs for a reason unrelated to the property it claims, because
it searched the report for the string "50" while the investigation repeats its
failing probe exactly once and so measures 0, 50 or 100 percent. It now compares
every percentage in a report against the measurement in that report's own
evidence.

---

## Definition of done

Section 22 of [todo.md](../todo.md), item by item, with what each rests on.

| requirement | state | evidence |
| --- | --- | --- |
| all milestones pass | PASS | the five reports above |
| all mandatory gates pass | PASS | `go test ./...`, `go test -race ./...`, `go vet ./...`, `make fmt-check`, all run on this machine |
| integration tests pass on Linux | PASS | `tests/integration`, real namespaces, real routes, real faults |
| race detector passes | PASS | `go test -count=1 -race ./...`, no race reported |
| fuzzing performed on packet and config parsers | PASS | fourteen targets: four config, eight packet, two router; the recorded corpus is replayed by every ordinary test run |
| security audit complete | PASS | the Phase 12 checklist, item by item, each naming its test |
| cleanup repeatedly verified | PASS | `make test-repeat REPEAT=3`, four rounds of build-measure-impair-destroy, and the three interruption scenarios now checking the process table as well as the host |
| host networking remains isolated | PASS | `TestHostIsNeverModified`, `TestSoftwareRouterLeavesTheHostAlone`, and the snapshot comparison every audit scenario ends with |
| diagnostics use actual evidence | PASS | `internal/investigator` imports only `apperr` and `probe`; `TestAConfiguredFaultIsNotReportedAsAMeasurement` compares every percentage in a report against the measurement in that report |
| recovery requires explicit authorization | PASS | `TestARecoveryNeedsExplicitAuthorization`, `TestADiagnosisCannotAuthorizeItself` |
| recovery is independently verified | PASS | `TestAnExperimentWithAnAuthorizedRecoveryVerifiesItsRepair` |
| reports are reproducible | PASS | the experiment identifier is derived from validated configuration content, and the seed is carried into the report |
| documentation reflects the implementation | PASS | `README.md` and the nine documents in `docs/`; the divergences found during Phases 13, 14 and 15 are corrected and named below |
| performance measurements include their conditions | PASS | `docs/PERFORMANCE.md` for the methodology, `docs/BENCHMARKS.md` for the recorded run, written by the run that measured it |
| no known critical security or lifecycle defect hidden | PASS | the four defects above, each with the measurement that found it, and the six found in milestone 5 |

## Divergences Phase 13 found and corrected

Documentation that had drifted from the code, each checked against the source
before being changed:

| where | what it said | what it does |
| --- | --- | --- |
| `README.md` | four commands | six: `experiment` and `recovery` were missing |
| `README.md` | `ip` must be on `PATH` | `ip`, `tc` and `ping`, and how they are resolved |
| `docs/ARCHITECTURE.md` | `internal/iface` runs `ip` by absolute path | it runs the bare name through the fixed `PATH` the agent gives every child |
| `docs/ARCHITECTURE.md` | one router worker per core | two workers draining one bounded queue |
| `docs/PROTOCOL.md` | the transport header is not inspected and its checksum is not recomputed | both are recomputed, which is the defect in milestone 2 above |
| `netforge help experiment run` | `netforge run` | `netforge experiment run`, the path that was typed |
| `docs/SECURITY.md`, `docs/THREAT_MODEL.md` | init reaps a killed run's namespace holders | nothing reaps them; they are tied to a pipe now, and a killed run's leftover temporary directories are named with the command that removes them |
## MILESTONE 5 — PASS

```text
==================================================
MILESTONE M5 — PASS
==================================================

Scope:
  The laboratory lifecycle and the reconciliation of what a crash left, both
  reachable from the command line. Phases 14 and 15.

Implemented:
  internal/cli              --routing kernel|software, --service-ports,
                             --service-base-port on experiment run, validated
                             before a namespace exists; a laboratory block in
                             the report naming what the run built
  internal/reconcile        the state marker, the listing, ownership verification
                             and a bounded, authorized, idempotent reclaim
  netforge reconcile        list, and reclaim --id --authorize -why
  internal/network          network.Summary, ServiceFor, and state directories
                             created through the reconciler so they are marked

Evidence for Phase 14:
  Three capabilities existed, were exercised against real namespaces, and could
  not be reached from the command line. Each was a flag's absence, which is why
  no earlier phase found it.

  The software router is now selected with --routing software. The mode is
  refused rather than defaulted, because a silently ignored flag would produce a
  report describing a software router that never ran. A software-routed run
  reports ip_forward=0 in the router namespace, which is the proof that the kernel
  forwarded nothing, alongside the router's own counters read once immediately
  before the laboratory is taken apart.
  TestASoftwareRoutedRunForwardsRealTraffic,
  TestAKernelRoutedRunSaysSoAndLeavesTheKernelForwarding.

  Laboratory services are now requested with --service-ports N, serving the
  observation's endpoint in the first N server namespaces at base+i, so two runs
  on one host do not collide. A protocol that needs a listener and did not ask
  for one is refused rather than measured against a closed port, and a run whose
  --target is a server it is not serving is refused against the plan it would
  have built.
  TestAServiceAnswersATCPObservationFromTheCommandLine,
  TestAProtocolThatNeedsAListenerIsRefusedAgainstNothing,
  TestTheServicePortsAreChosenByTheCaller.

  Every flag is validated before a namespace exists, and the report now says what
  the run built: the namespaces, their addresses and gateways, the routing mode,
  ip_forward, the endpoints actually bound, and the router's counters.
  TestARunReportsWhatItBuilt, TestEveryFlagIsRefusedBeforeAnythingExists, and
  netforge help experiment run.

Evidence for Phase 15:
  Section 13's three steps were unimplemented and recorded as a known limitation.
  A SIGKILL releases the namespaces, because each holder is tied to a pipe, but
  the two state directories survived with nothing listing them, nothing
  proving they were NetForge's, and nothing removing them.

  Every state directory is now created through reconcile.Create, which writes a
  marker naming its kind, the laboratory's identifier, the owning process and the
  creation time, atomically. A directory is a candidate for removal only if that
  marker parses, names a kind this build knows, and carries an identifier that
  passes the same name validation a namespace does. Everything else is listed as
  unidentified and left alone.
  netforge reconcile list, and TestTheReconcilerRemovesNothingItCannotProveOwns,
  which puts an impostor, a symlink, a file, a garbage marker and a copied marker
  in the state root, authorizes removal of all of them plus one genuine
  directory, and demands that only the genuine one goes.

  Removal needs explicit authorization and a written reason, exactly as a
  recovery does, is confined to the state root, refuses a live owner, is bounded
  at 64 directories, and is idempotent. It removes files and nothing else.
  TestAReconcileNeedsAuthorizationAndAReason,
  TestTheReconcilerIsConfinedToItsStateRoot,
  TestAReconcileRefusesStateThatIsStillInUse,
  TestReclaimingWhatAKilledRunLeftIsBoundedReportedAndIdempotent.

  A run that finishes normally leaves nothing to find, which is what makes the
  listing a report about crashes rather than about normal operation.
  TestAFinishedRunLeavesNothingForTheReconciler, for both routing modes.

Defects these phases found, all in the implementation rather than in the
documentation:

  1. A protocol that needs a listener could be measured against a closed port.
     --protocol tcp from the command line had nothing to answer it, and the run
     would succeed having measured a closed port. Now refused.

  2. A run could ask to serve one server and measure another. The count and the
     base port make that possible, so the observation is now checked against the
     plan the laboratory would have been built from.

  3. A --routing value that was not a mode the router implements was worth
     refusing rather than defaulting, which is the same class of defect as the
     transport checksum Phase 12 found: a report describing something that never
     ran.

  4. A reconciler written against syscall.Kill for its liveness check reached
     the process boundary. Asking whether a process is running is a question
     about /proc, not a reason to signal a process, and it now reads /proc and
     fails towards "in use" if it cannot answer.

  5. A reconciler that conflated "there is nothing there" with "there is
     something there that is not ours" reported a refusal as a success. They are
     now counted apart, so an absence exits 0 and a refusal does not.

  6. The listing's columns were narrower than a router's generated name, so the
     kind ran into the name in a shared state root — and the kind is the field
     that tells a laboratory's directory from a router's.

  On this machine, the finished reconciler listed 15 directories the audit tests
  had left in /tmp and reclaimed all 15, which is the end-to-end proof that a
  real crash's leftovers are now findable and removable.

Acceptance Gates:
  [PASS] go test ./...
  [PASS] go test -race ./..., twice
  [PASS] go vet ./...
  [PASS] make fmt-check
  [PASS] make cover

Known Issues:
  --protocol dns is refused from the command line: an observation there carries
  no name to ask a resolver about, so a resolver would answer nothing. The probe
  and the service that implement DNS are reachable from Go.
  The load balancer and the MTU are still not selectable from the command line.
  There is still no command that creates, inspects or destroys a laboratory on
  its own; reconcile reclaims what a dead run left and never builds one.
  Reclamation is never automatic. Nothing sweeps the state root on startup,
  because a sweep is a deletion with no authorization and no reason attached.
  A marker whose owner pid has been reused makes a directory look in use. The
  check is deliberately one-sided: it costs an operator one manual removal rather
  than risking a live laboratory's state.
  A process that can write into the state root can write a marker that looks
  valid. The marker is an ownership record, not a capability; /tmp is already
  writable by such a process for every other program on the host.

Files Changed:
  internal/{reconcile,network,cli,report,experiment,probe}/,
  tests/integration/{phase14,phase15}_test.go,
  tests/security/reconcile_audit_test.go, docs/, README.md, todo.md

Evidence:
  The Phase 14 and Phase 15 checklists in todo.md, item by item, each naming its
  test.

NEXT: none; the phase list is complete
```

## Divergences Phases 14 and 15 found and corrected

Documentation that had drifted from the code, each checked against the source
before being changed:

| where | what it said | what it does |
| --- | --- | --- |
| `README.md` | four commands | seven: `experiment`, `recovery` and `reconcile` were missing |
| `docs/EXPERIMENTS.md` | the software router and the laboratory's services are not selectable from the command line | `--routing` and `--service-ports` make both reachable |
| `docs/EXPERIMENTS.md` | a TCP, DNS, TLS or HTTP observation needs something listening, which today means running it from Go | `--service-ports N` serves it; a run that asks for none is refused |
| `docs/NETWORK_MODEL.md` | no command lists or reclaims what a killed run left | `netforge reconcile list` and `reclaim` |
| `docs/OPERATIONS.md` | remove the leftovers with `rm -rf /tmp/netforge-*` | `netforge reconcile reclaim --id <name> --authorize -why "<reason>"`, which proves ownership first |
| `docs/SECURITY.md` | a command that lists or reclaims the state a `SIGKILL`ed run leaves is absent | control 15, with the ownership argument and the refusals |
| `docs/THREAT_MODEL.md` | T8 residual risk: nothing reclaims a killed run's directories | `netforge reconcile` reclaims them; T8 is marked implemented |
| `docs/ARCHITECTURE.md`, `docs/NETWORK_MODEL.md` | state directories are made with `os.MkdirTemp` | they are made through `reconcile.Create`, which marks them |

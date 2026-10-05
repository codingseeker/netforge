# NetForge

Linux networking, failure-injection, diagnostics and resilience laboratory.

NetForge builds isolated virtual networks on Linux, forwards real traffic,
injects controlled network failures, measures the resulting behaviour, diagnoses
failures from collected evidence, performs explicitly authorized recovery and
verifies the recovery. It is a networking and systems engineering project.

## Status

**Phases 0 to 13 are implemented.** Every command below exists and is tested
against real namespaces, real routes and real faults. The state of each area is
in the table below; nothing in it is claimed beyond what the suite in this
repository demonstrates.

| Area | State |
| --- | --- |
| Go module, repository layout, CI | present |
| CLI entry point, command parsing, exit codes | present |
| Structured logging (`log/slog`, text and JSON) | present |
| Configuration model, strict YAML parsing, validation | present |
| Application error model, context propagation, graceful shutdown | present |
| Cleanup stack with LIFO ordering and idempotency | present |
| Packet codecs: Ethernet II, IPv4, ICMP, UDP, TCP, DNS | present, pure, unprivileged |
| Namespace, veth, address, route and lifetime handling | present |
| Isolated client, router and server topology with real traffic | present |
| Userspace software router: routing table, ARP, forwarding, TTL, ICMP, counters | present; carries TCP, UDP, DNS, TLS and HTTP, and repairs a transport checksum its sender left unfinished |
| Fault injection | `internal/chaos`, configured through `faults:` |
| Probing | `internal/probe`, bounded ICMP echo measurement and namespace-local TCP, UDP, DNS, TLS and HTTP probes |
| Laboratory services | `internal/service`, bounded real endpoints for a probe to measure |
| Diagnosis | `internal/investigator`, evidence-based classification as CONFIRMED, PROBABLE, POSSIBLE or UNKNOWN |
| Reliable UDP transport | `internal/transport`: sequence numbers, acknowledgements, retransmission, timers, sliding window, congestion control |
| Load balancer | `internal/loadbalancer`: registration, health checks, round robin, least connections, consistent hashing, draining |
| Experiment engine | `internal/experiment`: the declarative lifecycle, validated in full before anything runs |
| Recovery | `internal/recovery`: closed set of actions, each explicitly authorized and independently verified by measurement |
| Observability | `internal/metrics`, `internal/logging`, `internal/report`: ten derived metrics, structured fields, JSON report |
| Laboratory lifecycle from the CLI | `netforge experiment run --routing kernel\|software`, `--service-ports`, and a report that names the namespaces, addresses, services, routing mode and router counters |
| Crash reconciliation | `internal/reconcile`, `netforge reconcile list\|reclaim`: state a `SIGKILL`ed run left, identified by a marker, reclaimed only with an authorization and a reason |
| Security audit | `tests/security`: configuration, resource, injection, packet, timeout and repository-invariant audits |
| Reliability audit | `tests/integration`: repeated build and teardown, per-resource leak checks, interruption and recovery, and the assertion that no laboratory process outlives its run |
| Milestone reports | `docs/MILESTONES.md`, with each milestone's gates and the definition of done item by item |
| Documentation | `docs/`: architecture, protocol, network model, experiments, configuration, operations, performance, security, threat model, milestones |

Claims in this document describe only what the code and the test suite in this
repository demonstrate. NetForge is not described as production-ready or secure;
see [docs/SECURITY.md](docs/SECURITY.md) for the security posture that is
actually implemented and [docs/THREAT_MODEL.md](docs/THREAT_MODEL.md) for the
threats that are in scope.

## Requirements

- Linux (developed and tested on amd64, kernel 6.x)
- Go 1.25 or newer
- Unprivileged user namespaces must be available. NetForge needs no ambient
  capabilities and never runs as root: it creates a user namespace, and the
  capabilities that come with it are confined to the namespaces NetForge owns.
- `ip` and `tc` must be on `PATH`; `tc` comes with `iproute2`. NetForge drives
  them as bare names resolved through a fixed `PATH`, never through a shell.
- `ping` must be on `PATH` for the ICMP measurement. The five agent protocols
  (TCP, UDP, DNS, TLS, HTTP) do not need it.
- Without unprivileged user namespaces the network suites skip and the rest of
  the suite still runs.

## Build and test

```console
$ make build          # produces bin/netforge
$ make check          # fmt-check, vet, tests, race detector, coverage, build
```

The mandatory gate commands are:

```console
$ go test ./...
$ go test -race ./...
$ go vet ./...
```

Additional suites:

```console
$ make test-security      # repository security invariants
$ make test-integration   # end-to-end execution of the built binary
$ make test-repeat        # the whole suite REPEAT times, for flakiness
$ make fuzz               # configuration parser fuzzing
$ make fuzz-packet        # packet parsers and the forwarded checksum
$ make fuzz-router        # the forwarding decision and the ICMP it builds
$ make vuln               # govulncheck against the module
```

The integration suite builds the CLI once, then executes it as an unprivileged
process. It also builds real namespaces and carries real traffic between them
over veth pairs, which needs unprivileged user namespaces and no root.

### Current evidence

Recorded on Go 1.25.12, Linux/amd64:

| Check | Command | Result |
| --- | --- | --- |
| formatting | `gofmt -l cmd internal tests` | no output |
| vet | `go vet ./...` | pass |
| unit, integration, security | `go test -count=1 ./...` | pass |
| race detector | `go test -count=1 -race ./...` | pass, no race reported |
| statement coverage | `make cover` | 86.5% of statements across `./...`; `apperr`, `cleanup` and `logging` at 100% |
| fuzzing | `make fuzz FUZZ_TIME=20s` | `FuzzParse`, `FuzzParseBandwidth`, `FuzzParseDuration`, `FuzzValidateName` pass, no new findings |
| packet fuzzing | `make fuzz-packet FUZZ_TIME=3s` | `FuzzParseEthernet`, `FuzzParseIPv4`, `FuzzParseICMP`, `FuzzParseUDP`, `FuzzParseTCP`, `FuzzParseFrame`, `FuzzChecksum`, `FuzzRefreshTransportChecksum` pass, no new findings |
| repeated suite | `make test-repeat REPEAT=3` | three consecutive end-to-end passes of every package, including the network suite |
| isolated traffic | `go test -count=1 -run TestGate2TrafficCrossesTheRouter -v ./tests/integration/` | client `10.42.0.2/30` to server `10.42.0.6/30` through the kernel router, `ttl=63`, 0% loss |
| software router | `go test -count=1 -run TestSoftwareRouter -v ./tests/integration/` | the same traffic crosses `internal/router` with `ip_forward=0` in the router namespace, `ttl=63`, 0% loss, time to live and missing route answered with ICMP |
| software router protocols | `go test -count=1 -run TestSoftwareRouterForwardsEveryProtocolTheProberSpeaks -v ./tests/integration/` | TCP, UDP, DNS, TLS and HTTP each answered by a real service behind `internal/router`; a packet larger than the next link is answered with fragmentation needed rather than dropped |
| router fuzzing | `make fuzz-router FUZZ_TIME=20s` | `FuzzDecide` and `FuzzICMPError` pass, no new findings |
| host isolation | `go test -count=1 -run TestHostIsNeverModified -v ./tests/integration/` | host links, routes and `ip_forward` identical before and after |
| interrupted run leaves no process | `go test -count=1 -run TestAKilledExperimentIsRecoveredByTheNextRun -v ./tests/integration/` | after three `SIGKILL`s the next run builds, measures, finishes and no namespace holder survives; with the repair reverted the same test reports nine survivors |
| dependencies | `make vuln` | 0 vulnerabilities in imported packages; remaining advisories belong to the standard library of the local toolchain |

Coverage is measured with `-coverpkg=./...` so that the packages the
integration suite drives, `internal/namespace`, `internal/iface` and
`internal/network`, are attributed. `cmd/netforge` still reports 0% because the
entry point is exercised by the integration suite, which runs the built binary
as a separate process rather than in-process.

The privileged packages have deliberately low per-package coverage in a plain
`go test ./internal/...` run: their real behaviour is only reachable through the
kernel, so it is proven by the integration suite rather than by a unit test
with a mock.

## Commands

```text
netforge [global flags] <command> [arguments]

Commands:
  config      Validate and render experiment configuration.
  experiment  Compile a configuration into a plan, or run it against a real
              laboratory.
  help        Print help for a command.
  limits      Inspect the resource limits enforced by NetForge.
  recovery    Inspect the recovery actions NetForge will perform.
  reconcile   List and reclaim the state a killed run left behind.
  version     Print NetForge version information.

Global flags:
  --log-level    debug, info, warn, error (default info)
  --log-format   text, json (default text)
  --help         print this help and exit
  --version      print the version and exit
```

`--log-level` and `--log-format` also read `NETFORGE_LOG_LEVEL` and
`NETFORGE_LOG_FORMAT`. A flag wins over the environment, and the environment
wins over the built-in default. An invalid value is rejected with exit code 2
rather than being silently ignored.

Command flags:

| Command | Flag | Meaning |
| --- | --- | --- |
| `config validate` | `--show-effective` | print the canonical configuration after validation |
| `experiment plan`, `experiment run` | `--namespace` | laboratory namespace the experiment measures from |
| | `--target` | address the experiment observes |
| | `--port` | port measured, for the protocols that use one |
| | `--protocol` | `icmp`, `tcp`, `udp`, `dns`, `tls` or `http` |
| | `--count`, `--size` | probes per measurement, payload bytes per probe |
| | `--timeout`, `--deadline` | how long one probe waits, how long one measurement takes |
| | `--after-fault`, `--settle` | how long a fault is given to reach the kernel, how long a change is given to take effect |
| | `-recover` | a recovery action as `action:target`; may be repeated |
| | `-authorize`, `-why` | the consent and the reason a recovery requires |
| `experiment run` | `--output` | `text` or `json` |
| | `--clients`, `--servers` | nodes the laboratory creates, 1 to 8 each |
| | `--id` | laboratory identifier; its namespaces are `nf-<role>-<id>` |
| | `--routing` | `kernel` or `software`: who forwards in the router namespace |
| | `--service-ports` | server namespaces that serve the observation's endpoint, 0 for none |
| | `--service-base-port` | the first port a serving namespace binds; the *i*-th binds base+*i* |
| `reconcile list` | `--state-root` | the state directory to list; default `$NETFORGE_STATE_ROOT` or the system temporary directory |
| `reconcile reclaim` | `--id` | a state directory to reclaim; may be repeated |
| | `--authorize`, `-why` | the consent and the reason a reclaim requires |
| | `--older-than` | only reclaim state at least this old |

Examples:

```console
$ netforge config validate examples/simple-line/experiment.yaml
configuration: simple-line
experiment_id: nf-simple-line-391328ec
scope.network: 10.42.0.0/16
scope.namespaces: client,router,server

$ netforge config show examples/simple-line/experiment.yaml
$ cat examples/simple-line/experiment.yaml | netforge config validate -
$ netforge config validate --show-effective examples/simple-line/experiment.yaml
$ netforge limits show
$ netforge experiment plan --namespace nf-client0-demo --target 10.42.0.6 \
    --protocol icmp experiment.yaml
$ netforge recovery actions
$ netforge reconcile list
$ NETFORGE_LOG_FORMAT=json netforge config validate examples/simple-line/experiment.yaml
```

A run needs a document whose `scope.namespaces` names the namespaces the
laboratory will create, which are `nf-<role>-<id>` for the `--id` it is given;
`docs/EXPERIMENTS.md` has the full command, the report it writes and what each
field means.

`--protocol tcp`, `udp`, `tls` or `http` needs something to answer, so a run with
one of those must also ask for a listener with `--service-ports`; a run that does
not is refused rather than measured against a closed port. `--routing software`
puts NetForge's own router in the router namespace instead of the kernel's, and
the report then carries `ip_forward: 0` and the router's counters, so a reader can
tell which of the two forwarded. `docs/OPERATIONS.md` covers both.

`reconcile list` prints the state directories NetForge can prove it created and
leaves everything else alone. `reconcile reclaim` removes them, and needs
`--authorize` and a reason for the same reason a recovery does.

### Exit codes

| Code | Meaning |
| --- | --- |
| 0 | success |
| 1 | internal failure |
| 2 | invalid usage or argument |
| 3 | invalid or rejected configuration |
| 4 | resource limit exceeded |
| 5 | not found, I/O or permission failure |
| 124 | deadline exceeded |
| 130 | interrupted (SIGINT) |
| 143 | terminated (SIGTERM) |

The first `SIGINT` or `SIGTERM` cancels the process context, so the command
stops and its cleanup runs. A second signal terminates the process immediately
with the default disposition.

## Configuration

Configuration is data. It never expresses executable instructions, and the
decoder rejects unknown fields, invalid types, invalid enum values, duplicate
keys, multiple documents and unsupported options.

```yaml
name: simple-line

seed: 20260101

scope:
  network: 10.42.0.0/16
  namespaces:
    - client
    - router
    - server

limits:
  max_namespaces: 4
  max_interfaces: 8
  max_duration: 2m
  max_bandwidth: 200Mbps
  max_packet_rate: 5000
  max_concurrent_probes: 10
```

Rules enforced by the configuration loader:

- `name` is required, at most 63 characters, and uses lowercase letters, digits
  and `-`. The same rule applies to every namespace name.
- `scope.network` is required and must be a canonical IPv4 CIDR. A prefix with
  host bits set is rejected rather than silently masked, and `0.0.0.0/0` is
  rejected as a default route. IPv6 is rejected as unimplemented.
- `scope.namespaces` must contain at least one unique name, and its length may
  not exceed `limits.max_namespaces`.
- `limits` may be omitted entirely; every omitted limit adopts the NetForge
  default. A limit above the supported ceiling is rejected.
- `max_duration` accepts a duration string such as `10m`. Negative or
  unit-less values are rejected.
- `max_bandwidth` accepts a whole number with an explicit `bps`, `kbps`, `mbps`
  or `gbps` suffix. Fractional and unit-less values are rejected.
- The document is at most 1 MiB. Larger files, larger streams and oversized
  paths are rejected before parsing.
- Parsing performs no side effects: no files are written, no network is
  touched, and no host state is read or modified.

`faults:` is optional and each entry names one place a fault is put. `target`
must be a namespace named in `scope.namespaces`, `type` is one of `latency`,
`jitter`, `packet_loss`, `bandwidth`, `duplication` or `reordering`, and
`value` carries the unit the type needs. A `jitter` or a `reordering` also
needs a `delay`, `direction` defaults to `out`, and `duration` makes the fault
expire on its own. See `docs/CONFIGURATION.md` for the full schema.

```yaml
faults:
  - target: nf-client0
    type: latency
    value: 60ms
    duration: 1m
  - target: nf-client0
    direction: "in"
    type: packet_loss
    value: 20%
```

A field the chosen type does not use is a configuration error rather than
something quietly dropped, two faults cannot claim the same target, device and
direction, and a fault the laboratory cannot honour is refused when it is
applied.

### Defaults and ceilings

| Limit | Default | Ceiling |
| --- | --- | --- |
| `max_namespaces` | 10 | 64 |
| `max_interfaces` | 30 | 512 |
| `max_duration` | 10m | 24h |
| `max_bandwidth` | 1Gbps | 100Gbps |
| `max_packet_rate` | 100000 | 10000000 |
| `max_concurrent_probes` | 100 | 10000 |
| configuration size | - | 1 MiB |

Run `netforge limits show` for the values compiled into the binary.

## Repository layout

```text
netforge/
├── cmd/netforge/         process entry point: signals and exit status only
├── internal/apperr/      application error model
├── internal/cleanup/     ordered, idempotent cleanup stack
├── internal/cli/         command parsing, dispatch, help, exit codes, signals
├── internal/config/      configuration model, strict YAML, validation
├── internal/iface/       veth, address and route operations
├── internal/logging/     structured logging
├── internal/namespace/   namespace lifecycle and the laboratory agent
├── internal/network/     topology planning, scope, lifetime, routes
├── internal/packet/      Ethernet, IPv4, ICMP, UDP, TCP and DNS codecs
├── internal/router/      routing table, ARP, forwarding, counters, statistics
├── internal/transport/   reliable UDP: windows, retransmission, congestion control
├── internal/loadbalancer/ backends, health, policies, draining
├── internal/experiment/  the declarative experiment lifecycle
├── internal/recovery/    authorized actions and their verification
├── internal/metrics/     the numbers a run reports
├── internal/report/      JSON and text reports of a run
├── internal/probe/       bounded echo measurement and the namespace-local probe agent
├── internal/service/     bounded laboratory endpoints for probes to measure
├── internal/investigator/ evidence collection, hypotheses and classification
├── tests/integration/    end-to-end tests, including real namespaces
├── tests/security/       security audit and repository security invariants
├── docs/                 architecture, network model, protocol, experiments,
│                         configuration, operations, performance, security,
│                         threat model and the recorded benchmark
├── examples/             valid and intentionally invalid configurations
└── Makefile
```

A package is created when the phase that needs it starts, not before, so that no
empty package exists. `internal/packet` is the addition required by Phase 1;
`internal/iface`, `internal/namespace` and `internal/network` by Phase 2;
`internal/router` by Phase 3; `internal/chaos`, `internal/probe`,
`internal/service` and `internal/investigator` by Phases 4 to 6;
`internal/transport` by Phase 7; `internal/loadbalancer` by Phase 8;
`internal/experiment` and `internal/recovery` by Phases 9 and 10; and
`internal/metrics` and `internal/report` by Phase 11. Phase 13 added no package:
it wrote the four documents section 18 required and corrected the ones that had
drifted.

Architecture invariants are listed in
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Documentation

- [docs/SECURITY.md](docs/SECURITY.md)
- [docs/THREAT_MODEL.md](docs/THREAT_MODEL.md)
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)
- [docs/NETWORK_MODEL.md](docs/NETWORK_MODEL.md)
- [docs/PROTOCOL.md](docs/PROTOCOL.md)
- [docs/EXPERIMENTS.md](docs/EXPERIMENTS.md)
- [docs/CONFIGURATION.md](docs/CONFIGURATION.md)
- [docs/OPERATIONS.md](docs/OPERATIONS.md)
- [docs/PERFORMANCE.md](docs/PERFORMANCE.md)
- [docs/BENCHMARKS.md](docs/BENCHMARKS.md)
- [docs/MILESTONES.md](docs/MILESTONES.md)
- [examples/README.md](examples/README.md)

## Development rules

The engineering rules for this repository, including stop-on-failure, the
evidence requirement and the rule against mocks at the Linux networking
boundary, are in [todo.md](todo.md). They apply to every phase.

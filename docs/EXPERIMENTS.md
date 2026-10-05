# NetForge Experiments

This document describes an experiment: what the document says, what the engine
does with it, how a recovery is authorized and verified, and what the report
records. Every transcript here was produced by the commands shown, against real
namespaces on the machine this repository was measured on.

The configuration schema itself is in [CONFIGURATION.md](CONFIGURATION.md). The
network a run creates is in [NETWORK_MODEL.md](NETWORK_MODEL.md).

## What an experiment is

An experiment is a declaration plus an observation. The declaration is a
validated document: a name, the scope it may touch, the budgets it may spend and
the faults it wants injected. The observation is what to measure, from where, over
which protocol. Compiling the two together produces a plan, and the plan is
walked as nine states:

```text
setup    create the laboratory from the document
baseline measure the path before anything is impaired
fault    inject the declared faults
observe  measure the path while the faults are in place
diagnose explain the measurement with evidence
recover  perform the authorized recovery actions
verify   measure the path after recovery
report   write down what happened
cleanup  take the laboratory apart
```

The order is fixed and a step may only be entered from the one before it
(`internal/experiment/state.go:121`). Nothing is created before the plan exists:
`experiment.Compile` validates the document, the observation against the scope,
the fault list and every recovery's authorization, and refuses with all of the
problems joined (`internal/experiment/plan.go:163`). A refusal there happens
before a namespace exists.

The engine measures, and it measures before it decides. `diagnose` refuses to
run at all when the observation shows a path that answered everything, because
there is nothing to diagnose (`internal/experiment/environment.go:184`).

## The document

```yaml
name: acceptance-line
seed: 20260101

scope:
  network: 10.42.0.0/24
  namespaces:
    - nf-router-demo
    - nf-client0-demo
    - nf-server0-demo

limits:
  max_namespaces: 4
  max_interfaces: 8
  max_duration: 2m
  max_bandwidth: 200Mbps
  max_packet_rate: 5000
  max_concurrent_probes: 10

faults:
  - target: nf-server0-demo
    type: packet_loss
    value: 40%
```

Two things about the scope are worth stating because they decide whether a run
can start at all.

The scope names namespaces, and a laboratory's namespaces are generated from its
identifier: `nf-router-<id>`, `nf-client0-<id>`, `nf-server0-<id>`. A document
that grants `client`, `router` and `server` is a valid document — the examples
in `examples/` are — but it cannot be run, because `network.Create` refuses a
plan whose namespaces the scope does not grant, before creating anything
(`internal/network/lab.go:198`). Pass `--id demo` and grant the names that
identifier produces, as above, or leave `--id` out and grant the names the
plan's own experiment identifier produces.

The seed is recorded and carried into the report and the experiment identifier.
No engine consumes it: the faults NetForge applies are the faults the document
declares, so there is nothing random to seed.

## Compiling before running

```console
$ netforge experiment plan --namespace nf-client0-demo --target 10.42.0.6 \
    --protocol icmp \
    --recover remove_fault:nf-server0-demo -authorize -why "operator authorized removing the declared loss" \
    experiment.yaml
name: acceptance-line
experiment_id: nf-acceptance-line-518463df
scope: nf-acceptance-line-518463df network=10.42.0.0/24 namespaces=[nf-router-demo,nf-client0-demo,nf-server0-demo]
seed: 20260101
faults: 1
recoveries: 1
budget: 15.8s
lifecycle:
  setup    create the laboratory from the document
  baseline measure the path before anything is impaired
  fault    inject the declared faults
  observe  measure the path while the faults are in place
  diagnose explain the measurement with evidence
  recover  perform remove_fault nf-server0-demo (from nf-server0-demo)
  verify   measure the path after recovery
  report   write down what happened
  cleanup  take the laboratory apart
```

`plan` has no side effects: it reads a document and prints what the run would do,
including the time budget, which is `3 × measurement + after-fault + 3 × settle`
and must fit inside both the engine's own total and the document's
`max_duration` (`internal/experiment/plan.go:316`).

The experiment identifier is derived from the document rather than chosen:
`nf-` plus the name plus the first four bytes of a SHA-256 over the name, the
network, every namespace in order, the seed or the absence of one, and every
limit (`internal/config/experiment.go:10`). Two runs of the same document are the
same experiment; changing any of those fields makes it a different one.

## Running

```console
$ netforge experiment run --namespace nf-client0-demo --target 10.42.0.6 \
    --protocol icmp --id demo --count 9 \
    --recover remove_fault:nf-server0-demo \
    -authorize -why "operator authorized removing the declared loss" \
    experiment.yaml
experiment: acceptance-line
experiment_id: nf-acceptance-line-518463df
seed: 20260101
result: succeeded
duration: 1.128328017s
cleaned up: true

lifecycle:
  setup     create the laboratory from the document (180.256631ms) ok
  baseline  measure the path before anything is impaired (96.505306ms) ok
  fault     inject the declared faults (15.399484ms) ok
  observe   measure the path while the faults are in place (339.726757ms) ok
  diagnose  explain the measurement with evidence (10.244974ms) ok
  recover   perform the authorized recovery actions (186.633654ms) ok
             recovery: remove_fault nf-server0-demo (from nf-server0-demo)
  verify    measure the path after recovery (294.939674ms) ok
  report    write down what happened (1.204µs) ok
  cleanup   take the laboratory apart (4.549266ms) ok

metrics:
  packets sent:         31
  bytes:                1944
  rtt:                  min 57µs average 106.666µs max 174µs
  packet loss:          30.8%
  retransmissions:      0
  throughput:           55040 bps
  connection failures:  0
  availability:         69.2%
  recovery time:        186.633654ms
  diagnostic duration:  10.244974ms
```

The measurement flags are `--namespace`, `--target`, `--port`, `--protocol`,
`--count`, `--size`, `--timeout`, `--deadline`, `--after-fault` and `--settle`;
the laboratory flags are `--clients`, `--servers`, `--id`, `--routing`,
`--service-ports` and `--service-base-port`; the report flags are
`--output text|json`. Flags come before the path. `netforge help experiment run`
prints them.

The protocol decides how the observation is taken: `icmp` is measured with the
kernel's own `ping` and its printed report is parsed
(`internal/probe/probe.go:367`); `tcp`, `udp`, `dns`, `tls` and `http` are
measured by a probe agent re-executed inside the namespace
(`internal/probe/engine.go:158`). `icmp` needs no listener, which is why the
transcript above uses it against a scope that starts no service.

`--routing software` puts NetForge's own router in the router namespace instead
of the kernel's, and `--service-ports N` serves the observation's endpoint in the
first *N* server namespaces so a protocol that needs a listener has one. Both are
covered in [OPERATIONS.md](OPERATIONS.md), together with the report's
`laboratory` block, which names the namespaces, their addresses, the routing mode
and — when the software router is in use — its counters.

`--output json` writes the same run as a structured document: `name`,
`experiment_id`, `seed`, `succeeded`, `cleaned_up`, `duration`, `laboratory`,
`steps[]`, `faults[]`,
`diagnosis`, `recoveries[]`, `metrics`, and the three measurement sets
`baseline`, `observed` and `verified`. `laboratory` carries `id`, `network`,
`routing`, `ip_forward`, `router_namespace`, `router_links`, `nodes[]` (role,
namespace, address, gateway), `services[]` (namespace and the endpoints actually
bound) and, for a software-routed run, the router's `router` counters or a
`router_error` saying why they could not be read. Every measurement carries `sent`,
`received`, `loss`, `min_rtt`, `average_rtt`, `max_rtt`, `deviation_rtt`,
`duplicates`, `errors`, `out_of_order`, `bytes`, `duration`, `goodput`, `at` and
the `options` it was taken with. `metrics` carries the ten derived numbers plus a
`phases[]` breakdown of `baseline`, `observe` and `verify`.

One run's phases, from that run's report:

```json
[
  {"name": "baseline", "sent": 9, "received": 9, "loss": 0, "bytes": 648,
   "throughput": 54953, "average_rtt": "134µs", "duration": "94.334769ms"},
  {"name": "observe",  "sent": 19, "received": 9, "loss": 52.6, "bytes": 648,
   "throughput": 25334, "average_rtt": "118µs", "duration": "204.62498ms"},
  {"name": "verify",   "sent": 9, "received": 9, "loss": 0, "bytes": 648,
   "throughput": 54287, "average_rtt": "89µs", "duration": "95.491415ms"}
]
```

`observe` sent 19 rather than 9 because the investigator repeats the probe that
failed once, so a diagnosis can say whether the path is impaired or broken. That
is why `packet loss` in the text report, which is the mean of the observe phase,
reads 30.8% while this phase row reads 52.6%.

## Diagnosis

A diagnosis is a classification, a layer, the rule that produced it and the
evidence behind it:

```json
{
  "classification": "PROBABLE",
  "layer": "network",
  "summary": "PROBABLE: the path to the target is impaired rather than broken. measured loss of 66.7% on the path to the target (evidence 2).",
  "rule": "degraded-path",
  "evidence_count": 3,
  "probes_run": 2
}
```

The classifications are `CONFIRMED`, `PROBABLE`, `POSSIBLE` and `UNKNOWN`
(`internal/investigator`). The investigator reaches the network only through the
evidence it collects itself: it cannot read the fault specification the document
declared, because `internal/investigator` imports only `apperr` and `probe`
(`tests/integration/investigator_test.go`). `TestAConfiguredFaultIsNotReportedAsAMeasurement`
is the check that the diagnosis is not simply the configuration read back.

## Recovery

A recovery is a change to the laboratory, so it needs five things, in order:

```text
explicit authorization
        +
scope validation
        +
action validation
        +
actual recovery
        +
verification by measurement
```

`netforge recovery actions` prints the closed set:

```console
$ netforge recovery actions
remove_fault         additive      namespace in the scope
restore_route        additive      route destination, inside scope.network
restart_service      destructive   namespace in the scope
remove_backend       destructive   backend endpoint, inside scope.network
restore_backend      additive      backend endpoint, inside scope.network
drain_connection     additive      backend endpoint, inside scope.network
re_enable_backend    additive      backend endpoint, inside scope.network

a recovery requires: explicit authorization + scope validation + action validation
                       + actual recovery + verification by measurement
```

The set is closed: a request naming anything else is refused rather than guessed
at (`TestTheActionSetIsClosedAndListed`, `TestAnUnknownActionIsRefused`). Naming
an action is not consent — without `-authorize` the requests are still built and
still handed to the compiler, which refuses them:

```console
$ netforge experiment plan --namespace nf-client0-demo --target 10.42.0.6 \
    --protocol icmp -recover remove_fault:nf-server0-demo experiment.yaml
netforge: permission_denied: recovery.Authorize: a recovery needs explicit authorization,
and none was given for remove_fault on nf-server0-demo
```

Every authorization carries a reason, which `-why` supplies, and is bound to the
experiment it was compiled into, so it is useless against any other document or
any other laboratory. An action that ran and left the path impaired is a failure
rather than a partial success: the outcome in that report reads

```json
{
  "action": "remove_fault",
  "target": "nf-server0-demo",
  "from": "nf-server0-demo",
  "reason": "operator authorized removing the declared loss",
  "passed": true,
  "recovered_at": "2026-10-05T13:19:19+06:30",
  "result": "loss is 0% over 9 replies, average round trip 107µs"
}
```

and the criteria that judged it are the defaults from
`recovery.DefaultCriteria`: no loss, at least one reply, an average round trip
under two seconds (`internal/recovery/verify.go:17`). The whole request set is
validated before the first action runs, so a valid request followed by an invalid
one leaves the laboratory untouched.

The four backend actions are not reachable from the command line: `-recover` has
no way to say which address a `From` should use, so the CLI can authorize the
three namespace actions and refuses the four address actions. They are exercised
from Go, against a laboratory with a running balancer, in
`TestAnExperimentWithAnAuthorizedRecoveryVerifiesItsRepair`.

## What the CLI does not expose

Stated rather than discovered:

- What to measure is not in the document. There is no `probes:`, `services:` or
  `diagnosis:` key; the observation and the recovery requests are flags.
- The load balancer and the MTU are not selectable from the command line. The
  software router (`--routing software`) and the laboratory's services
  (`--service-ports`) are, and were not before Phase 14.
- `--protocol dns` is refused from the command line. A DNS observation needs a
  name to ask the resolver about, and the CLI's observation carries none, so a
  resolver started for it would answer nothing. The probe and the service that
  implement DNS are reachable from Go.
- A protocol that needs a listener and did not ask for one is refused rather than
  measured against a closed port. A run that asks for services is checked against
  the plan it will build, so `--target` pointing at a server the run is not serving
  is caught before a namespace exists.
- `report.Recommendation`, the recovery the investigator would recommend, is
  computed and carried in the engine's report but is not rendered in either
  output format.
- There is no `netforge lab` command, so a laboratory cannot be created,
  inspected or destroyed on its own. See [OPERATIONS.md](OPERATIONS.md).

## Determinism and repetition

The same document, the same observation and the same authorization compile to the
same experiment identifier, the same plan and the same budget. What is measured
is not deterministic, and is not claimed to be: a fault applied at a percentage
drops a different set of packets every run, so the observed loss moves and the
investigator's probes move with it. That is why the transport benchmark records
one run as one sample of a random fault
([BENCHMARKS.md](BENCHMARKS.md)) and why the suite that found two flaky tests
raised their declared thresholds rather than pinning a result
([todo.md](../todo.md), Phase 12).

The evidence a run leaves is the report and the host state. The report says what
was measured, what was concluded, what was changed and whether the change was
verified; the host says what is left behind, which after `cleanup` is nothing
carrying the laboratory's identity — no link, no route, no namespace, no qdisc,
and no process still holding one of its namespaces.
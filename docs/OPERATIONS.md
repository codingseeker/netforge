# NetForge Operations

This document is for the person running NetForge: what it needs, how to drive it,
what a healthy run looks like, what a broken one says, and what is left behind
when something is interrupted.

The experiment model is in [EXPERIMENTS.md](EXPERIMENTS.md). The network a run
builds is in [NETWORK_MODEL.md](NETWORK_MODEL.md). The security posture is in
[SECURITY.md](SECURITY.md).

## Requirements

| requirement | why | if it is missing |
| --- | --- | --- |
| Linux, amd64, kernel 6.x | every namespace, veth and `tc` call is a Linux one | the build compiles; nothing runs |
| Go 1.25 or newer | the module declares `go 1.25` | the build fails |
| unprivileged user namespaces | NetForge creates a user namespace and is uid 0 inside it, which is how it needs no root | the network suites skip with `unprivileged user namespaces are unavailable`; everything else still runs |
| `ip` on `PATH` | every link, address and route operation | the laboratory cannot be built |
| `tc` on `PATH` | every fault, and reading a fault back | faults are refused; a run without `faults:` still works |
| `ping` on `PATH` | the ICMP measurement | any `icmp` observation fails; the six agent protocols do not need it |

NetForge resolves those helpers through the fixed `PATH` its namespace agent
gives every child (`/usr/sbin:/usr/bin:/sbin:/bin`), never through a shell and
never through a path of its own. `tests/security/source_test.go` enforces that
the shell does not appear anywhere in the repository.

NetForge never runs as root and never requests a capability. If you are about to
run it as root, that is not required and it loses the property that makes it
safe to run on someone else's machine.

## Build and verify

```console
$ make build          # bin/netforge
$ make check          # fmt-check, vet, tests, race detector, coverage, build
$ bin/netforge version
netforge dev
commit: unknown
go: go1.25.12 X:nodwarf5
platform: linux/amd64
```

`commit: unknown` means the tree was not built from a version control checkout,
which is what `make build` reads the revision from. In a checkout it is the short
commit the binary was built from.

The suites, and what each is evidence of:

```console
$ make test-unit          # the packages, with no kernel involved
$ make test-integration   # end to end, against real namespaces
$ make test-security      # the repository's own security invariants
$ make test-repeat REPEAT=3
$ make cover
$ make fuzz FUZZ_TIME=20s
$ make fuzz-packet FUZZ_TIME=15s
$ make fuzz-router FUZZ_TIME=20s
$ make vuln
```

The integration suite needs user namespaces and takes a few minutes; the
benchmark comparison is part of it and rewrites `docs/BENCHMARKS.md` from the run
it just measured.

## Commands

```console
$ netforge config validate experiment.yaml
$ netforge config show experiment.yaml
$ netforge config validate --show-effective experiment.yaml
$ cat experiment.yaml | netforge config validate -
$ netforge limits show
$ netforge experiment plan [flags] experiment.yaml
$ netforge experiment run  [flags] experiment.yaml
$ netforge recovery actions
$ netforge reconcile list
$ netforge reconcile reclaim [flags]
$ netforge version
$ netforge help <command> [<subcommand>]
```

`config`, `limits show`, `experiment plan` and `recovery actions` read a file or
print what is compiled into the binary; none of them touches the network.
`reconcile list` reads one directory and removes nothing. `experiment run` is the
only command that builds a laboratory, and `reconcile reclaim` is the only one
that deletes anything.

`--log-level` and `--log-format` apply to every command, read `NETFORGE_LOG_LEVEL`
and `NETFORGE_LOG_FORMAT` when the flag is absent, and reject an unknown value
with exit 2 rather than ignoring it. Logs go to standard error, reports go to
standard output, so `netforge experiment run … --output json > report.json` keeps
the report clean.

### The event names a log record can carry

A record's message is its event name, and the name comes from a closed set: there
is no other place in NetForge that decides what a log line is called, so a record
can be asserted on by its name rather than matched as text. Every record carries
exactly one `event` key, and every record of a run after the first carries that
run's `experiment_id`.

| event | written when |
| --- | --- |
| `dispatch` | a command has been resolved, before it runs |
| `command_failed` | the command returned an error |
| `stage_setup`, `stage_baseline`, `stage_fault`, `stage_observe`, `stage_diagnose`, `stage_recover`, `stage_verify`, `stage_report`, `stage_cleanup` | one lifecycle stage has finished; each carries its `state`, `detail`, `duration` and `error` |
| `namespace_created` | the laboratory created a namespace; carries `namespace` and `role` |
| `fault_applied`, `fault_removed` | a fault entered or left the laboratory; carries `fault_id`, `type`, `target`, `direction` and `status` |
| `diagnosis_made` | a failure was explained; carries `classification`, `layer`, `rule` and the number of `evidence` items |
| `action_authorized` | an authorized action is about to run; carries `action`, `target`, `from` and `reason` |
| `verification_passed`, `verification_failed` | the path after recovery was measured |
| `configuration validated`, `state reconciled` | a document was validated, a reclaim finished |
| `experiment run succeeded`, `experiment run failed` | the run's terminal record; the failing one carries `state` |

So one run's records account for the whole of it:

```console
$ netforge --log-level=debug --log-format=json experiment run … 2>trace.log
$ jq -r .event trace.log
dispatch
namespace_created
namespace_created
namespace_created
stage_setup
stage_baseline
fault_applied
stage_fault
stage_observe
diagnosis_made
stage_diagnose
action_authorized
fault_removed
stage_recover
verification_passed
stage_verify
stage_report
stage_cleanup
experiment run succeeded
```

### A run, end to end

```console
$ netforge experiment run --namespace nf-client0-demo --target 10.42.0.6 \
    --protocol icmp --id demo --count 9 --routing software \
    --recover remove_fault:nf-server0-demo \
    -authorize -why "operator authorized removing the declared loss" \
    experiment.yaml
experiment: acceptance-line
experiment_id: nf-acceptance-line-70fc02c5
result: succeeded
duration: 3.198766971s
cleaned up: true
...
laboratory:
  id:         demo
  network:    10.42.0.0/24
  routing:    software
  ip_forward: 0
  namespaces:
    nf-client0-demo         client  10.42.0.2/30 via 10.42.0.1
    nf-server0-demo         server  10.42.0.6/30 via 10.42.0.5
    nf-router-demo          router    forwarding here, nfr-c0 10.42.0.1/30, nfr-s0 10.42.0.5/30
  router:
    received 93, forwarded 82, bytes 8532
    ...
metrics:
  packet loss:          30.8%
  availability:         69.2%
  recovery time:        186.633654ms
```

The `laboratory` block says what the run built: the namespaces, the addresses
they held, the routing mode, what `ip_forward` read in the router namespace, the
endpoints that were listening, and — when the software router is in use — its
own counters. `ip_forward: 0` is the proof that the kernel forwarded nothing and
every forwarded byte was NetForge's, which is the one thing a reader cannot
otherwise tell from outside the process. It is recorded while the laboratory
exists and printed after it is taken apart, because that is the only order in
which both halves are available.

### What to type next

The `recommendation` block is the one part of a report that says what the
diagnosis suggests. Both formats carry it, and it is present whether or not
anything was diagnosed:

```text
recommendation:
  diagnosed: true
  remove_fault on nf-client0-demo; PROBABLE: the path to the target is impaired
  rather than broken. measured loss of 75.0% on the path to the target (evidence
  1).; rule degraded-path
  next:      -recover remove_fault:nf-client0-demo
  from:      nf-client0-demo
```

`next` is a complete `-recover` argument; a repair still needs the `-authorize`
and the `-why` that every recovery requires.

The suggestion is derived from the layer the diagnosis blamed, not from a list
of faults: a `network` layer diagnosis names the namespace the probe measured
from, because that is the only endpoint the evidence carries. Compare it with
the `faults` block before following it — the fault a document declared may be on
a different namespace than the one named here.

A run that diagnosed nothing says so rather than leaving the block out, so an
absent diagnosis and a report that forgot to look cannot be confused:

```text
recommendation:
  diagnosed: false
  nothing was diagnosed, so there is nothing to recommend
```

With `--output json` the same block is the `recommendation` object, and `flag`
holds the string `next` renders.

### Serving the endpoint a protocol needs

`--protocol icmp` needs no listener: the network answers an echo request. Every
other protocol is answered by a process, so a run that observes one has to ask
for it:

```console
$ netforge experiment run --namespace nf-client0-demo --target 10.42.0.6 \
    --protocol tcp --port 7000 --service-ports 1 --id demo \
    experiment.yaml
```

That serves a TCP endpoint in the first `--service-ports` server namespaces. The
*i*-th binds `--service-base-port` + *i*, so two runs on one host do not collide as
long as they choose different ports, and a range that would run past 65535 is
refused rather than clamped. The report then carries:

```text
  services:
    nf-server0-demo          tcp/[::]:7000
```

The refusals here are the interesting part, and each one is a refusal rather
than a measurement:

| refusal | what it means |
| --- | --- |
| `-routing "ebpf" is not a mode this build forwards with, want kernel or software` | the mode is not implemented; a run never falls back to the kernel, because a report claiming a software router that never ran is the defect this exists to prevent |
| `-protocol tcp is answered by a process, and -service-ports 0 asks for none` | the observation would be measured against a closed port |
| `-protocol icmp needs none: an echo request is answered by the network itself` | a listener was asked for that nothing would probe |
| `-target "10.42.0.99" is not served by this run` | the listener and the measurement are on different servers |
| `-service-ports is 3 but -servers is 1` | more serving namespaces than there are servers |

Every one of them happens before a namespace exists, so a refused run leaves no
state for the reconciler to find.

The things that surprise people, all of which are refusals rather than
failures:

- The document's `scope.namespaces` must name the namespaces the laboratory will
  actually create, which are `nf-<role>-<id>` for the `--id` you pass. A document
  that grants `client`, `router` and `server` validates and plans, and is refused
  at `network.Create` with
  `the laboratory needs namespace nf-router-demo, which scope.namespaces does not grant`.
  Nothing is created first.
- `-recover` needs `-authorize` and `-why`. Naming an action is not consent, and
  the refusal comes from the recovery layer rather than from the command:
  `permission_denied: recovery.Authorize: a recovery needs explicit authorization`.
- `--protocol dns` is refused from the command line: an observation here carries
  no name to ask a resolver about, so a resolver would answer nothing. The probe
  and the laboratory service that implement DNS are reachable from Go.

## Exit codes

| code | meaning | what it usually means |
| --- | --- | --- |
| 0 | success | |
| 1 | internal failure | a bug; the message carries the operation that failed |
| 2 | invalid usage or argument | unknown command, unknown flag, missing subcommand, an unknown `--protocol`, an unusable `--output` |
| 3 | invalid or rejected configuration | YAML, a field the reader does not know, a name that breaks the naming rule, a CIDR that is not a prefix |
| 4 | resource limit exceeded | a limit above its ceiling, a scope larger than `max_namespaces`, too many concurrent probes, a packet over the size ceiling |
| 5 | not found, I/O or permission | missing file, unreadable file, scope refused a namespace or address, a recovery without authorization |
| 124 | deadline exceeded | a bounded operation ran out of time |
| 130 | interrupted | `SIGINT` reached the run |
| 143 | terminated | `SIGTERM` reached the run |

Every message is prefixed with the operation that produced it and its
classification, so `netforge` output can be matched on either.

## Signals

The first `SIGINT` or `SIGTERM` cancels the process context: the run stops at the
next step boundary and its cleanup runs, which is why an interrupted run still
reports `cleaned up: true` and exits 130 or 143. A second signal terminates the
process with the default disposition, which is the operator's way out.

Inside a laboratory the same rule is enforced without a signal: the agent, the
namespace holders, the router and the services are all tied to pipes the owning
process holds. When that process goes away — by `SIGINT`, by `SIGTERM` or by
`SIGKILL` — the kernel closes the pipes, the children read the end of them and
exit, and the namespaces go with them. That is the property
`TestAKilledExperimentIsRecoveredByTheNextRun` and
`TestSIGINTInEveryPhaseLeavesTheLaboratoryClean` hold in place, both against the
host's network objects and against the process table.

## What a healthy laboratory looks like

While a run is going, another shell can look at it:

```console
$ ps -eo pid,args | grep -e netforge-lab-agent -e netforge-namespace-holder \
      -e netforge-router -e netforge-service
$ nsenter -t <client-holder-pid> -n ip -o addr show
$ nsenter -t <client-holder-pid> -n ip route show
$ nsenter -t <router-holder-pid> -n cat /proc/sys/net/ipv4/ip_forward
$ nsenter -t <router-holder-pid> -n tc qdisc show dev nfr-s0
```

- Each node namespace has `lo` plus one interface whose MTU is the plan's and
  whose address is the third address of its block, and one `default via` pointing
  at the second.
- The router namespace has one interface per node at the second address of each
  block.
- `ip_forward` is `1` in kernel mode.
- With a fault applied, `tc qdisc show dev <dev>` reports a root qdisc of kind
  `netem` or `tbf`, and an inbound fault shows up on the peer's egress device.
- The router's statistics file, when the software router is in use, reports
  `state: running` and a `revision` that increases.

Namespaces are not in `ip netns list`: NetForge creates them with `clone` and
addresses them by PID. `nsenter -t <pid> -n` is the way in.

## When a run is interrupted

After `SIGINT`, `SIGTERM` or `SIGKILL`, check the host:

```console
$ ip -o link show | grep nf-          # expect nothing
$ ip -o route show | grep -e nf- -e 10.42
$ ip netns list | grep nf-            # expect nothing
$ tc qdisc show | grep nf-            # expect nothing
$ ps -eo args | grep -e netforge-lab-agent -e netforge-namespace-holder \
      -e netforge-router -e netforge-service
$ netforge reconcile list
```

What survives, and what to do about it:

| left behind | why | action |
| --- | --- | --- |
| nothing | the run finished or was signalled | none |
| a `netforge-namespace-holder` process | only from a process killed before the fix that now watches the pipe, or from a binary older than this release | `pkill -f netforge-namespace-holder`; the namespaces go with it |
| state directories listed as `laboratory` or `router` | the state of a run that was `SIGKILL`ed: service status files, and a router's statistics | `netforge reconcile reclaim --id <name> --authorize -why "<reason>"` |

The namespaces are held by processes, not by files, so a `SIGKILL`ed run leaves
no links, routes or qdisc rules behind — only the two directories above, holding
status files. `reconcile list` finds them, and `reconcile reclaim` takes them
back.

### Reclaiming what a killed run left

```console
$ netforge reconcile list
state root: /tmp
NAME                         KIND          ID        AGE      STATE
netforge-lab-3f8a1c05b2de    laboratory    nfdemo    2h14m    idle
netforge-router-91ac7d3e6f04 router        nfdemo    2h13m    idle

2 identified as NetForge's, 0 not identified and left alone
reclaim one with: netforge reconcile reclaim -id <name> -authorize -why "<reason>"

$ netforge reconcile reclaim --id netforge-lab-3f8a1c05b2de \
    --authorize -why "the run that owned this was killed; clearing the host"
state root: /tmp
reason:     the run that owned this was killed; clearing the host
authorized: 2026-10-01T09:14:22Z

/tmp/netforge-lab-3f8a1c05b2de  removed: laboratory state directory nfdemo, created 2h14m0s ago

removed 1, refused 0, already gone 0
```

Three things to know about a reclaim, all of which are refusals rather than
failures:

- **Ownership is proven, not assumed.** A directory is only a candidate if it
  carries the marker NetForge wrote when it created it, naming a kind this build
  knows and an identifier a namespace could carry. Everything else in the state
  root is listed as `unidentified` and left alone, including a directory that
  merely *looks* like one of ours. `/tmp` is shared, so this is the whole of the
  safety argument.
- **Live state is refused.** A directory whose owning process is still running
  is reported `in use by pid N` and not removed, so a reconcile cannot pull state
  out from under a laboratory that is still writing to it. `--older-than 24h`
  refuses anything younger still, which is what to reach for when a run may still
  be starting.
- **`--authorize` and a reason are required**, exactly as for a recovery. Naming a
  directory is not consent. Reconciling twice is safe: the second pass reports
  `already gone` and exits 0.

The state root defaults to the system temporary directory, or to
`$NETFORGE_STATE_ROOT` when that is set. `--state-root` overrides both, which is
how you point a reconcile at a directory of your own.

One listing reads at most 1024 entries. The state root is shared with everything
else on the host, so a root that holds more than that is refused rather than
enumerated — a million strangers' files are not NetForge's problem to walk. The
refusal names the bound; move or clean the root, or point `--state-root` at a
directory of your own.

Reconciliation removes files and nothing else: no namespace, no link, no route and
no qdisc rule, which is why it needs no privileges and cannot damage host
networking. There is still no command that creates, inspects or destroys a
laboratory on its own — see [NETWORK_MODEL.md](NETWORK_MODEL.md).

A laboratory is recoverable by name. The next run with the same `--id` builds the
same namespaces again, because nothing is reserved between runs.

## Troubleshooting

| symptom | cause | what to do |
| --- | --- | --- |
| `unprivileged user namespaces are unavailable` | the kernel or the distribution forbids them | run the unit and security suites; the network suites skip themselves rather than failing |
| `the laboratory needs namespace nf-router-<id>, which scope.namespaces does not grant` | the document grants names the plan does not create | grant `nf-router-<id>`, `nf-client0-<id>`, `nf-server0-<id>`, or pass the `--id` the document grants |
| `a recovery needs explicit authorization` | `-recover` without `-authorize` | add `-authorize` and `-why`, or drop `-recover` |
| `the scope refuses a default route` / `is not a prefix` | `scope.network` is `0.0.0.0/0` or has host bits set | give a canonical prefix such as `10.42.0.0/24` |
| `the scope leaves N addresses, too few for point to point links` | the prefix is longer than `/24` | use a prefix between `/8` and `/24` |
| `limit_exceeded` on a run that looks small | the laboratory needs `clients + servers + 1` namespaces and `2 × (clients + servers)` interfaces | raise `max_namespaces` and `max_interfaces` within their ceilings |
| the run succeeded but `packet loss` is above the declared fault | the declared loss applies to the packets leaving that namespace, and the report averages the observe phase, which includes the investigator's repeat | read `metrics.phases` for the counts rather than the mean |
| `the path did not recover` | an authorized action ran and the measurement still failed its criteria | read `recoveries[].result`, which is the measurement that refused the repair |
| `exit 5` reading a document from a pipe | the document was empty | `cat` the file and check it parses with `netforge config validate -` |
| a fault that does not appear | `tc` is not on `PATH` inside the agent's fixed `PATH` | install `iproute2`; `tc` ships with it |
| `-protocol tcp is answered by a process, and -service-ports 0 asks for none` | the observation needs a listener and the run asked for none | add `--service-ports 1` |
| `-target "..." is not served by this run` | the listener is on a different server than the one measured | point `--target` at a served server, or raise `--service-ports` |
| `-routing "..." is not a mode this build forwards with` | the mode is not implemented | use `kernel` or `software`; nothing is silently substituted |
| `refused: no ownership marker in netforge-lab-...` | the directory is not provably NetForge's, so it is never removed | leave it alone, or remove it by hand if you are certain of what it is |
| `refused: the process that created it, pid N, is still running` | the state belongs to a live laboratory | wait for the run to finish, or use `--older-than` if you doubt the pid |

## Limits

```console
$ netforge limits show
max_config_bytes: 1048576
max_name_length: 63
default.max_namespaces: 10
default.max_interfaces: 30
default.max_duration: 10m0s
default.max_bandwidth: 1Gbps
default.max_packet_rate: 100000
default.max_concurrent_probes: 100
ceiling.max_namespaces: 64
ceiling.max_interfaces: 512
ceiling.max_duration: 24h0s
ceiling.max_bandwidth: 100Gbps
ceiling.max_packet_rate: 10000000
ceiling.max_concurrent_probes: 10000
```

A document's `limits:` block may lower these and may not raise them past the
ceilings. An omitted block adopts the defaults. Every one of them is checked
before anything is created, and the values in a document are the values the
laboratory and the fault engine are then given, so a fault the document accepted
is not refused at the moment it would take effect.

## Continuous integration

`.github/workflows/ci.yml` runs, on every push and pull request:

| job | what it runs |
| --- | --- |
| `build-and-test` | `go mod verify`, `gofmt -l`, `make build`, `go vet`, the whole suite, the whole suite under `-race`, coverage |
| `integration` | `tests/integration` and `tests/security`, and fails if the suite ran as root, because its unprivileged claims are only evidence if it ran unprivileged |
| `fuzz` | the five configuration fuzzers for 30s each |
| `fuzz-packet` | the recorded packet corpus replayed, then eight packet and checksum fuzzers, and fails if a fuzzer wrote a new corpus entry that was not committed |
| `static-analysis` | `go vet`, dependency review at moderate severity, `govulncheck` |

Privileged or host-affecting work is deliberately absent: NetForge needs none, so
no CI job is given elevated privileges. A job that did need them would have to be
pinned to a runner where they are configured explicitly, and would fail rather
than acquire them.

## Not in this release

Stated so an operator is not left looking for it:

- No `netforge lab` command: a laboratory cannot be created, listed, inspected or
  destroyed on its own, only through `experiment run`. `reconcile` does the
  opposite — it reclaims state a finished or killed run left, and never creates
  or inspects a running laboratory.
- No report file. The report is written to standard output; redirect it.
- No persistence. A run keeps nothing between processes but the report you kept
  and, if it was `SIGKILL`ed, the state directories `reconcile reclaim` removes.
- No automatic reconciliation. Reclaiming is an operator's decision, taken with an
  authorization and a reason; nothing sweeps the state root on startup.
- No daemon mode, no server, no scheduler: each run is one process that ends.
- No remote or cross-host laboratory. Everything a run touches is a namespace on
  the machine it runs on.

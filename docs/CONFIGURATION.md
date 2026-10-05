# NetForge Configuration Reference

The configuration document is data. NetForge never executes instructions from
it, and the decoder rejects anything it does not fully understand.

## Schema

```yaml
name: simple-line            # required
seed: 20260101               # optional uint64
scope:                       # required
  network: 10.42.0.0/16      # required, canonical IPv4 CIDR
  namespaces:                # required, at least one unique name
    - client
    - router
    - server
limits:                      # optional, every omitted limit uses the default
  max_namespaces: 10
  max_interfaces: 30
  max_duration: 10m
  max_bandwidth: 1Gbps
  max_packet_rate: 100000
  max_concurrent_probes: 100
```

`name` and every namespace name are NetForge resource names:

- 1 to 63 characters;
- lowercase letters, digits and `-` only;
- no leading or trailing `-`;
- namespaces must be unique within a scope.

`seed` makes probabilistic experiments reproducible. It is recorded and
reported; NetForge does not consume it yet.

## Rejected input

| Input | Result |
| --- | --- |
| unknown field, at any level | rejected |
| `10.42.0.0/33` | rejected, not a valid CIDR |
| `10.42.0.5/16` | rejected, host bits set; use the canonical network |
| `0.0.0.0/0` | rejected, a default route is not an experiment scope |
| `fd00:42::/64` | rejected, IPv6 scope is not implemented |
| `-10m` | rejected, negative duration |
| `600000000000` | rejected, a duration must be a string with a unit |
| `1.5Gbps`, `10G`, `10` | rejected, bandwidth needs a whole number and an explicit unit |
| duplicate mapping key | rejected |
| two YAML documents | rejected |
| a top-level list or scalar | rejected |
| more than 1 MiB | rejected before parsing |
| a request above any ceiling | rejected |
| a scope larger than `limits.max_namespaces` | rejected |

## Values

### Durations

Durations use Go duration syntax: `300ms`, `30s`, `10m`, `1h30m`. A duration
must be a quoted or bare string scalar, must not be negative, and must not
exceed 24h in a limit. `0s` parses but is rejected for a limit, because every
limit must be positive.

### Bandwidth

Bandwidth is a whole number of bits per second with an explicit suffix:

| Suffix | Factor |
| --- | --- |
| `bps` | 1 |
| `kbps` | 1000 |
| `mbps` | 1000000 |
| `gbps` | 1000000000 |

Suffixes are case-insensitive. Values must be positive and must not overflow
64 bits. The canonical rendering of a limit uses the largest exact unit, so
`1000kbps` is rendered as `1Mbps`.

## Defaults and ceilings

| Limit | Default | Ceiling | Meaning |
| --- | --- | --- | --- |
| `max_namespaces` | 10 | 64 | namespaces an experiment may own |
| `max_interfaces` | 30 | 512 | interfaces an experiment may own |
| `max_duration` | 10m | 24h | wall-clock bound for an experiment |
| `max_bandwidth` | 1Gbps | 100Gbps | bandwidth an experiment may configure |
| `max_packet_rate` | 100000 | 10000000 | packets per second an experiment may generate |
| `max_concurrent_probes` | 100 | 10000 | probes in flight at once |

The configuration document itself is limited to 1 MiB, and a resource name to 63
characters. `netforge limits show` prints the values compiled into the binary.

## Scope

```yaml
scope:
  network: 10.42.0.0/16
  namespaces:
    - client
    - router
    - server
```

The scope is an explicit declaration. Every operation that affects network
state must be checked against it before it runs, and scope is never silently
widened. The declaration is validated when the document loads, and
`internal/network` enforces it again before the first kernel call: every planned
address must lie inside `scope.network` and every planned namespace must be
named in `scope.namespaces`. A namespace the scope does not name is refused, and
an empty scope grants nothing.

## Canonical form

`netforge config show <file>` prints the validated configuration in canonical
form: fixed field order, string durations, canonical bandwidth units, and
omitted fields removed. The output of `config show` re-validates without
modification, and is byte-identical across runs and processes.

## Faults

`faults:` is an optional list. Every entry names one place a fault is put, and
a document that leaves it out describes a healthy path.

```yaml
faults:
  - target: nf-client0                 # a namespace named in scope.namespaces
    type: latency                      # latency | jitter | packet_loss | bandwidth
                                       # duplication | reordering
    value: 60ms                        # what the fault does, see the table
    direction: out                     # optional, out is the default
    duration: 1m                       # optional, the fault expires on its own
  - target: nf-client0
    direction: "in"
    type: packet_loss
    value: 20%
  - target: nf-router-file
    interface: nfr-c0                  # optional, the namespace's only device
    type: bandwidth
    value: 200kbps
    queue: "500"                       # optional, netem's queue length
```

`value` carries the unit, and the unit has to match the type:

| `type` | `value` | Also required |
| --- | --- | --- |
| `latency` | a duration, e.g. `60ms` | |
| `jitter` | a duration, e.g. `30ms` | `delay`, the latency it spreads around |
| `packet_loss` | a percentage, e.g. `20%` | |
| `duplication` | a percentage, e.g. `40%` | |
| `reordering` | a percentage, e.g. `30%` | `delay`, the delay the packets queue behind |
| `bandwidth` | a whole number with `bps`, `kbps`, `mbps` or `gbps` | |

`gap` (the number of later packets a reordered packet is put in front of) and
`queue` are quoted numbers, because a plain `500` is a YAML integer and this
schema refuses one where a string belongs. `correlation` and `burst` take
percentages and whole packets respectively.

The schema is strict in both directions. A field that the chosen type does not
use, a field the type does not know, a percentage that is not a whole number
between 1 and 100, a `jitter` without `delay` or a `reordering` without
`delay`, and an unknown `type` are all configuration errors with the offending
line named. Nothing is ignored and nothing is guessed.

Faults are bounded by the same limits as the rest of the document and by the
engine's own ceilings: `duration` cannot exceed `max_duration`, `value` cannot
exceed `max_bandwidth`, and a laboratory accepts at most 64 faults. Two faults
cannot claim the same place: the same target, device and direction twice is
refused when the document loads. The topology remains the authority at run
time, because a configuration file can only see what it declares, so a fault
whose target the laboratory does not have is refused when it is applied.

## Probes, services and diagnosis

There is no `probes:`, `services:` or `diagnosis:` key. A document describes a
network and the faults on it; what to measure, which endpoints exist to measure
against, and how a failure is explained are decided by the code that runs the
experiment, not by the document.

This is deliberate. The endpoints a probe measures against and the bounds a
diagnosis runs under are part of the experiment's definition, and letting a
document choose them would put the trust boundary inside the configuration file
rather than in the code that enforces it. A probe target still has to be inside
`scope.network` and a probe namespace still has to be one the plan created, so
the scope above constrains measurements taken by code exactly as it constrains
faults declared in a file.

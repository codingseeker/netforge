# NetForge Performance

This document describes how performance is measured in this repository, what each
number means and where the recorded results live. It makes no capacity claim
about any real network: the measurement surfaces are laboratory links inside
namespaces, and a figure from one is a figure about that laboratory.

The recorded comparison is [BENCHMARKS.md](BENCHMARKS.md). That file is written by
the run that produced it, so the numbers in it are a measurement rather than a
copy of one.

## Rules the methodology follows

- No performance target is invented. Every figure below was measured on the
  machine named in the recorded file, with its conditions attached.
- A benchmark report records the hardware, the kernel, the Go version, the
  revision, the configuration, the experiment parameters and the method. The
  generated report records all of them and says "unknown" for anything it cannot
  read rather than leaving it out.
- Nothing is compared against a figure from another machine or another run. The
  comparison inside one run is the finding; an absolute byte rate is a property of
  that machine, that kernel and that load.

## The recorded comparison

One test produces it: `TestTransportComparisonRecordsEveryScenario` in
`tests/integration/transport_test.go`. It is an ordinary test, not a
`go test -bench` benchmark — there is no `func Benchmark` in this repository — so
it runs with `make test-integration` and writes its table as part of passing.

```console
$ make test-integration
$ go test -count=1 -run '^TestTransportComparisonRecordsEveryScenario$' -v ./tests/integration/
```

What it measures: plain UDP, TCP and NetForge's reliable UDP over the same
impaired path, each offered the same 256 payloads of 1024 bytes (256 KiB),
echoed by a real service in the server namespace and read by a client process in
the client namespace.

How it is measured:

- The client runs inside the client namespace. A test process cannot enter a
  network namespace without taking its own threads with it, so the parent
  re-executes the test binary with `-test.run=^TestTransportBenchmarkChild$` and
  reads one `RESULT` line per transport
  (`tests/integration/transport_test.go:281`).
- The fault is applied by the kernel with `netem` on the client end of the link.
  A userspace proxy that dropped bytes from a TCP stream would corrupt the stream
  rather than model packet loss, so TCP would never retransmit what the proxy
  swallowed and the measurement would be of the proxy.
- Sending and receiving run concurrently, because offering the whole transfer
  first would stall against the transport's own backpressure and measure the
  stall.
- Each payload carries its index in its first four bytes, so an echo can be
  matched to the payload that was sent rather than to a repeated pattern.
- Throughput is bytes offered per second, goodput is bytes that arrived intact
  per second, loss is the share of offered payload that never came back, and the
  round trip is measured per payload from the call that sent it to the read that
  returned it.
- Every result is checked before it is recorded: exactly 262144 bytes offered, no
  delivery beyond what was sent, and the reliable transport required to have
  delivered all of it (`checkMeasurement`,
  `tests/integration/transport_test.go:340`).

The six conditions are an unimpaired path, 1%, 5% and 10% packet loss, 60ms of
added latency, and 40ms of latency with 30ms of jitter.

## What the numbers mean

| column | meaning |
| --- | --- |
| Throughput | bytes handed to the transport per second, whether or not they arrived |
| Goodput | payload bytes that arrived intact per second |
| Loss | share of offered payload that never came back |
| Mean RTT | mean of the per-payload round trips, in milliseconds |
| Retransmits | segments the transport itself reports sending again |
| Complete | every payload index came back |

Plain UDP is included to show what the path does with no recovery at all. TCP
recovers everything and pays for it with head-of-line blocking. The reliable
transport recovers payloads independently, so a loss costs the segment that was
lost rather than the window behind it, which is what the retransmit and complete
columns show.

It infers loss only from a retransmission timeout, because a datagram gives
nothing else away. TCP also retransmits on duplicate acknowledgements, which is
why its loss rows cost it less. A transport that inferred loss from reordering or
duplication would narrow that gap; this comparison does not claim otherwise.

One run is one sample of a random fault. Re-running the test rewrites
`BENCHMARKS.md` with a different sample, and a row that reports no retransmission
under loss is a run the path happened not to impair.

## The metrics a run reports

An experiment report carries ten derived metrics
(`internal/metrics/metrics.go:17`). Each is computed from the measurements of the
run, never from a configured value:

| metric | how it is derived |
| --- | --- |
| `packets_sent` | sum of `sent` over every measurement of all three phases |
| `bytes` | sum of `bytes` over every measurement |
| `rtt.min` / `rtt.max` | smallest `min_rtt` and largest `max_rtt` |
| `rtt.average` | reply-count-weighted mean of `average_rtt`, over the measurements that had replies |
| `packet_loss` | recomputed from the counts of the observe phase, never read from the measurement |
| `retransmissions` | always zero for echo measurements, which have no transport to retransmit on |
| `throughput` | the largest `goodput` of any measurement |
| `connection_failures` | measurements that sent something and received nothing |
| `availability` | `100 - packet_loss` |
| `recovery_time` | the duration of the recover step |
| `diagnostic_duration` | the duration of the diagnose step |

`metrics.phases[]` repeats the counts per phase, so a report says which of the
three measurements carried the loss rather than only that some measurement did.

A run also reports what it built, under `laboratory`: the namespaces it created
and the addresses they held, the routing mode, what `ip_forward` read in the
router namespace, the endpoints each serving namespace reported bound, and — for
a run with `--routing software` — the software router's counters as they stood
immediately before the laboratory was taken apart. The counters are the only way
a reader can tell from outside the process whether a forwarded byte came from
NetForge or from the kernel. They are read through the same statistics file and
the same `SIGUSR1` revision the router already refreshes, bounded at five seconds
rather than the twenty a diagnostic read is given, because this read happens
inside a teardown.

An individual measurement carries what the kernel reported, not what NetForge
hoped for: `sent`, `received`, `loss`, `min_rtt`, `average_rtt`, `max_rtt`,
`deviation_rtt`, `duplicates`, `errors`, `out_of_order`, `bytes`, `duration` and
`goodput` (`internal/probe/probe.go:177`). An unparsable `ping` report is an
error, never a zero, and a report that shows replies but no round trip line is an
error rather than a measurement of nothing.

## The counters a laboratory publishes

Beyond a run's report, three components publish their own counters as JSON
through a file written to a temporary name and renamed into place, so a reader
never sees half a document.

The software router writes `<state root>/netforge-router-*/router.json`
(`internal/router/counters.go:31`): `revision`, `state`, `error`, `lastError`,
`received`, `forwarded`, `bytes`, `ignored`, `local`, `malformed`, `noRoute`,
`ttlExceeded`, `oversize`, `unreachable`, `arpRequests`, `arpReplies`,
`arpProbes`, `unresolved`, `queueFull`, `sendErrors`, `queueDepth`, `queueSize`,
`routes` and `neighbors`. It refreshes every 500ms and on `SIGUSR1`. `arpProbes`
is the count of ARP requests the router sent, `arpRequests` the count it
answered. A request for a fresh snapshot takes `SIGUSR1` and waits for
`revision` to advance, bounded at 20 seconds, or at five when the caller is a
teardown reporting what the run built. The state directory carries a marker
NetForge wrote when it created it, so a run killed with `SIGKILL` leaves
something `netforge reconcile list` can identify and `reconcile reclaim` can
remove; see [OPERATIONS.md](OPERATIONS.md).

The reliable transport reports its own numbers about the path rather than a
configured value (`internal/transport/congestion.go:204`): window, ceiling,
whether it is in slow start, whether it is in recovery, timeouts, round trip,
smoothed round trip, jitter, and whether a round trip was ever measured. The
connection additionally counts sent, acknowledged, retransmitted, received,
duplicated, dropped and timeouts; the receive side counts delivered, duplicated,
dropped, buffered and waiting. Those counters are what the benchmark's
`Retransmits` column is, which is why they are per transport and not interpolated
for the other two rows.

The load balancer writes a status file
(`internal/loadbalancer/agent.go:38`): `state`, `listen`, `policy`, and per
backend `address`, `state` (`healthy`, `draining`, `removed`), `active`,
`accepted`, `skipped`, `checks`, `failed_checks`, `consecutive_failures`,
`recoveries`, `forwarded_bytes`, `returned_bytes`, `drained_connections`,
`latency`, `last_checked` and `last_error`. Health checks run every 500ms with a
250ms per-check bound, and two consecutive failures take a backend out of service
while two consecutive successes bring it back
(`internal/loadbalancer/limits.go:44`).

## Properties measured as limits rather than as figures

Some performance properties are enforced as bounds rather than reported as
numbers, and a test is what holds them in place:

- The routing table, the neighbour cache and the forwarding queue: 1024 routes,
  256 neighbours, a 256-frame queue by default, each refused at the
  configuration boundary, with a bounded queue depth in the published counters.
- The probe engine: a probe per request over a non-blocking slot, so a probe over
  the concurrency limit is refused rather than queued
  (`TestExcessiveConcurrencyIsRefused`); a wall-clock bound one second above the
  measurement's own deadline; a reply-size ceiling of 65507 bytes and a result
  store that drops its oldest entry at the limit.
- The reliable transport: a payload ceiling of 1380 bytes, a window ceiling, a
  retransmit-attempt ceiling after which the connection is abandoned, and a
  kernel socket buffer sized so the kernel cannot queue frames the transport
  must then refuse (`internal/transport/conn.go:166`).
- The chaos engine: a fault becomes `active` only once the kernel's own `tc -j
  qdisc show` numbers match what was asked for
  (`internal/chaos/tc.go:306`), so a reported fault is one the kernel installed.
- The laboratory itself: services are ready when their status file says every
  listener is bound, the router is ready when its statistics file says
  `running`, and both waits are bounded (5s and 3s).

## Reproducing the whole picture

```console
$ make build
$ make check          # fmt-check, vet, tests, race detector, coverage, build
$ make test-integration
$ make test-repeat REPEAT=3
$ make fuzz FUZZ_TIME=20s
$ make fuzz-packet FUZZ_TIME=15s
$ make fuzz-router FUZZ_TIME=20s
$ make vuln
```

`make check` is the gate; the rest are the evidence behind the individual
claims. `make test-repeat` exists because a suite that passes once may still be
flaky, and a flaky suite makes every other measurement untrustworthy.
# Transport benchmark

The comparison below was produced by `TestTransportComparisonRecordsEveryScenario` in `tests/integration/transport_test.go`, which writes this file from the run it just measured. Re-running the test rewrites it.

## Conditions

Every transport is offered the same work: 256 payloads of 1024 bytes, echoed back by the same service in the
server namespace and read by a client in the client namespace.
One transfer is bounded at 1m0s, and a read on an unreliable transport
gives up after 400ms, because a datagram that is never retransmitted
is not going to arrive.

Faults are applied by the kernel to the client end of the link with
`netem`, not by a proxy in userspace. A proxy that dropped bytes from a TCP
stream would corrupt that stream rather than model packet loss, so TCP
would never retransmit what the proxy swallowed and the measurement would be
of the proxy.

## Measured on

- go1.25.12 X:nodwarf5, linux/amd64
- kernel 6.19.13-200.fc43.x86_64
- 12 CPUs, 13th Gen Intel(R) Core(TM) i5-1334U
- revision unknown: the toolchain stamps no VCS revision into a test binary

## Configuration

- laboratory: one client and one server on 10.42.0.0/24, MTU 1500, one
  point-to-point link per node
- endpoints: stream 9303, udp 9301, rudp 9302, in the server namespace
- work: 256 payloads of 1024 bytes per transport, 262144 bytes offered
- bounds: a transfer ends after 1m0s, an unreliable read gives up after 400ms
- reliable transport: window 32, payload 1024, retransmit timeout 100ms, at
  most 40 attempts per segment, idle timeout 30s

## Results

Throughput is what was offered to the transport; goodput is what arrived
intact. Loss is the share of the offered payload that never came back.

| Condition | Transport | Throughput | Goodput | Loss | Mean RTT | Retransmits | Complete |
| --- | --- | ---: | ---: | ---: | ---: | ---: | :---: |
| 0% loss | rudp | 17659930 B/s | 17659930 B/s | 0.0% | 1.78 ms | 0 | true |
| 0% loss | tcp | 42050690 B/s | 42050690 B/s | 0.0% | 0.09 ms | 0 | true |
| 0% loss | udp | 63627184 B/s | 63627184 B/s | 0.0% | 0.08 ms | 0 | true |
| 1% loss | rudp | 614136 B/s | 614136 B/s | 0.0% | 32.81 ms | 74 | true |
| 1% loss | tcp | 31856119 B/s | 31856119 B/s | 0.0% | 0.14 ms | 0 | true |
| 1% loss | udp | 646581 B/s | 641530 B/s | 0.8% | 0.04 ms | 0 | false |
| 5% loss | rudp | 227705 B/s | 227705 B/s | 0.0% | 52.24 ms | 91 | true |
| 5% loss | tcp | 43567226 B/s | 43567226 B/s | 0.0% | 0.14 ms | 0 | true |
| 5% loss | udp | 646195 B/s | 620953 B/s | 3.9% | 0.06 ms | 0 | false |
| 10% loss | rudp | 211957 B/s | 211957 B/s | 0.0% | 34.68 ms | 74 | true |
| 10% loss | tcp | 1225029 B/s | 1225029 B/s | 0.0% | 83.44 ms | 0 | true |
| 10% loss | udp | 648918 B/s | 570338 B/s | 12.1% | 0.47 ms | 0 | false |
| high latency | rudp | 430347 B/s | 430347 B/s | 0.0% | 63.01 ms | 0 | true |
| high latency | tcp | 724619 B/s | 724619 B/s | 0.0% | 184.51 ms | 0 | true |
| high latency | udp | 500928 B/s | 180021 B/s | 64.1% | 120.20 ms | 0 | false |
| high jitter | rudp | 444299 B/s | 444299 B/s | 0.0% | 62.90 ms | 0 | true |
| high jitter | tcp | 159990 B/s | 159990 B/s | 0.0% | 502.56 ms | 0 | true |
| high jitter | udp | 516577 B/s | 185645 B/s | 64.1% | 75.99 ms | 0 | false |

## What each scenario is for

- **0% loss**: no fault is applied. an unimpaired path, the baseline every other row is read against.
- **1% loss**: packet_loss (1% of packets dropped). a path that is nearly clean, where retransmission should cost almost nothing.
- **5% loss**: packet_loss (5% of packets dropped). a lossy path, where the transports should start to differ.
- **10% loss**: packet_loss (10% of packets dropped). a badly lossy path, where a reliable transport should still deliver everything.
- **high latency**: latency (60ms of added latency). a slow path, where a window larger than the bandwidth delay product pays off.
- **high jitter**: jitter (40ms of added latency, 30ms of jitter). a path whose delay varies, which also reorders packets.

## Reading the numbers

- Plain UDP is offered the same payloads and loses whatever the path drops. That is the point of including it: it shows what the path does on its own, with no recovery at all.
- TCP recovers everything, at the cost of head-of-line blocking: a lost segment holds up every later payload until it is retransmitted.
- The reliable UDP transport recovers payloads independently, so a loss costs the segment that was lost rather than the whole window behind it, which is what the retransmit column and the completion column show.
- Retransmissions are counted by the transport itself and are zero for the other two rows, which have no such counter to report.

## What these numbers do not show

They are a small-scale comparison, and the absolute figures are not capacities of anything:

- The transfer is 256 KiB over virtual links in a namespace with no bandwidth ceiling, so a clean-path row is mostly measuring how fast a namespace loopback can carry 256 payloads. The shapes across rows are the finding; the byte rates are not a claim about a real network.
- The fault is applied to the client end of the link, so it is the client's sends that are impaired and the client's window that has to recover.
- The reliable transport infers loss only from a retransmission timeout, because a datagram gives nothing else away. TCP also retransmits on duplicate acknowledgements, which is why its loss rows cost it far less. A transport that inferred loss from reordering or duplication would narrow that gap, and this comparison does not claim otherwise.
- One run is one sample of a random fault. The retransmit counts in particular move with the draw: rerunning the test rewrites this file with a different sample, and a row that reports no retransmission under loss is a run the path happened not to impair.

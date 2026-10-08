# Protocol Support

This document describes the packet layer that exists in this repository today:
`internal/packet`. It covers Ethernet II, IPv4, ICMP, UDP and TCP headers.

Every statement here is a statement about code in this repository. Where a
protocol leaves a choice open, the choice NetForge makes is written down and
explained, because a laboratory tool has to be predictable about what it will
accept and what it will send.

Scope limits worth stating plainly:

- The package parses and renders packets. It performs no syscalls, opens no
  sockets, sends nothing and receives nothing. Traffic enters and leaves through
  `internal/network` and `internal/router`.
- TCP is header-only. There is no state machine, no handshake, no
  retransmission, no stream and no congestion control. Connection behaviour
  belongs to the host kernel or to the Go standard library.
- IPv4 only. IPv6, VLAN tag decoding, IP options interpretation, fragment
  reassembly and extension headers are not implemented.
- ICMP covers types 0, 3, 8 and 11 only. Other types are rejected rather than
  guessed at.

## Where the code lives

| File | Contents |
| --- | --- |
| `internal/packet/limits.go` | protocol constants, `Limits`, `DefaultLimits` |
| `internal/packet/checksum.go` | RFC 1071 checksum, pseudo headers, incremental update |
| `internal/packet/ethernet.go` | Ethernet II parse and serialize |
| `internal/packet/ipv4.go` | IPv4 parse and serialize, header checksum, TTL update |
| `internal/packet/icmp.go` | ICMP parse and serialize, message checksum |
| `internal/packet/udp.go` | UDP parse and serialize |
| `internal/packet/tcp.go` | TCP header parse and serialize |
| `internal/packet/frame.go` | `ParseFrame`, `Frame`, packet builders |

## The rules every parser follows

1. **Bounds are supplied by the caller.** Every parser takes a `Limits` value
   and calls `Limits.Validate()` first. A caller may tighten the defaults and
   can never widen them, because the protocol defines the outer bound.
2. **A length is checked against the bytes that exist** before any slice is
   taken. A header length, a total length and a transport length are each
   validated separately, and each is compared with the remaining buffer.
3. **Lengths are range-checked before conversion.** A declared length is
   compared against its maximum while it is still a `uint16`, so a large
   declared value can neither truncate into a small `int` nor be used as an
   allocation size.
4. **Parsing is in place.** No parser copies a packet into a new buffer sized by
   a field from the packet. The returned `Payload`, `Options` and `Padding`
   slices alias the caller's buffer, which is why the documentation on each type
   says so.
5. **Malformed input returns an error.** Errors come from `internal/apperr` with
   an operation, a message naming the offending value and the bound it violated,
   and a classification the CLI maps to an exit code. The package contains no
   `panic`, no `log.Fatal` and no `os.Exit`.
6. **No attacker-controlled allocation.** The only `make` calls in the package
   are in serializers, and each one is preceded by a comparison of the value it
   is about to size against a protocol maximum or a caller limit.

`tests/security/packet_test.go` enforces points 3, 5 and 6 as source
invariants, so a future change that removes a bound is caught by the test suite
rather than by a crash.

## Checksums

`Checksum` implements the 16-bit one's complement Internet checksum of RFC 1071.
An odd trailing byte is padded with a zero byte on the right. The result is
host-order and is written with `binary.BigEndian.PutUint16`, which is what
"network byte order" means for a checksum.

Where a checksum is verified:

| Header | Covers | Verified by |
| --- | --- | --- |
| IPv4 | the header only, including options | `IPv4.ChecksumValid`, `ParseIPv4` |
| ICMP | the whole message, no pseudo header | `ICMP.ChecksumValid`, `ParseICMP` |
| UDP | pseudo header plus the datagram | `UDP.Validate`, `ParseFrame` |
| TCP | pseudo header plus the segment | `TCP.Validate`, `ParseFrame` |

The transport checks need the endpoints, which live in the enclosing IPv4
header, so `ParseUDP` and `ParseTCP` do not verify on their own. `ParseFrame`
calls `Validate` for the caller.

Three details are easy to get wrong and are therefore pinned by tests:

- **A computed zero is transmitted as all ones.** `0x0000` in a UDP checksum
  field means "no checksum was computed" (RFC 768), so a computed zero would be
  read back as an omitted checksum. Every field written by this package goes
  through `checksumField` first. The raw `Checksum` result can legitimately be
  zero and is not changed.
- **The stored value takes part in its own verification.** `IPv4.ChecksumValid`
  and `ICMP.ChecksumValid` sum the bytes as they appear on the wire, including
  the checksum field, instead of zeroing it first. A correct packet sums to all
  ones.
- **A zero UDP checksum is valid.** IPv4 permits omitting it. `UDP.Validate`
  reports such a datagram as valid and `UDP.ChecksumOmitted` records the fact,
  so a frame that arrived without a checksum is rendered without one again.

`IPv4.SetTimeToLive` repairs the stored header checksum with the incremental
update of RFC 1624 rather than leaving the caller with a packet that no longer
verifies.

Verification is on by default (`DefaultLimits`). It can be turned off through
`Limits.VerifyChecksums` because Linux receive-path checksum offload
(`CHECKSUM_PARTIAL`) presents frames whose transport checksum is not yet
correct. Turning it off does not disable any framing, length or header check:
malformed framing is still rejected. A message parsed with verification off
records that in its `ChecksumUnchecked` field.

## Ethernet II

A 14-byte header: 6-byte destination, 6-byte source, 16-bit type. Frames larger
than `Limits.MaxFrameSize` are rejected. A frame shorter than 14 bytes is
rejected.

- VLAN-tagged frames are reported with `EtherTypeVLAN` (0x8100) and not
  untagged, so a caller never has to guess which framing it received.
- A multicast source address is rejected: an Ethernet source is unicast.
- A destination that is neither broadcast nor multicast is unicast, as usual.
- The payload is not interpreted. Link-layer padding stays in `Payload` for
  `Ethernet`, and moves to `Frame.Padding` once the IP packet length is known.

Frames may legitimately be shorter than the 60-byte Ethernet minimum, because
loopback and virtual links do not pad, so no minimum beyond the header size is
enforced.

## IPv4

The version and IHL nibbles are checked together: the version must be 4 and the
IHL must be at least 5 words and at most 15 words. The header length is
`IHL * 4`, which is why it is bounded by construction before it is used.

- The total length must be at least the header length, at most
  `MaxIPv4TotalLength` (65535), at most `Limits.MaxPacketSize`, and no larger
  than the bytes actually available.
- Options must be a multiple of 4 bytes, since the header is measured in words.
  They are preserved verbatim and never interpreted.
- The fragment field is exposed three ways: `DontFragment`, `MoreFragments` and
  `FragmentOffset`. NetForge does not reassemble fragments. `ParseIPv4` accepts
  a non-first fragment and reports its offset, but a non-first fragment has no
  transport header, so `ParseFrame` hands its payload to the transport parser,
  which rejects it — for example `length field is 1, the minimum is 8`. A caller
  that needs to look at fragments must call `ParseIPv4` and check
  `FragmentOffset()` before expecting a transport message.
- The padding that follows the packet inside the frame is preserved separately
  from the payload, so the packet length and the frame length never get confused.

## ICMP

Types 0 (echo reply), 3 (destination unreachable), 8 (echo request) and 11 (time
exceeded) are handled. Any other type is rejected as unsupported by this phase
rather than parsed as one of the four.

- An error message is 8 bytes: the 4-byte header plus the 4 bytes that quote the
  rest of the triggering internet header. Anything beyond that is the payload.
- An echo message is 8 bytes: the header plus identifier and sequence.
- The checksum covers the whole message and nothing else. There is no pseudo
  header, which is why an ICMP checksum stays valid wherever the message is
  forwarded.

**Malformed but checksum-valid messages are reproduced, not corrected.** A
nonzero code in an echo message, or a short quote in an error message, is
accepted when its checksum verifies and is rendered back exactly as received.
The reasoning is that a verifying checksum means something sent it, and a
laboratory that reproduces a malformed packet needs the malformed form back.
Correcting it would invent protocol content that was never observed.

## UDP

The 8-byte header is fixed. The declared length must be at least 8 and no
larger than the bytes present; bytes beyond the declared length belong to the IP
padding, not to the datagram. The length field is the only place where trailing
bytes inside the IP payload can exist, so `Frame.PayloadTail` keeps them and
`Frame.Serialize` appends them again.

A datagram that arrived with a zero checksum is rendered without one. Call
`UDP.Serialize` directly to turn an omitted checksum into a computed one.

## TCP

The data offset must be at least 5 words and at most 15 words, and no larger
than the bytes available. Options are preserved verbatim.

- The four bits after the data offset, and the two reserved bits in the ninth
  octet, are preserved exactly as received. RFC 793 reserves them and a receiver
  must not depend on their value, but a laboratory tool that re-renders a
  captured segment must not change a bit it does not understand.
- There is no form of TCP that omits the checksum, so `TCP.Validate` has no
  "unchecked" case: a segment that does not verify was corrupted.
- `Serialize` computes a valid checksum over the pseudo header of the endpoints.
  `SerializeRaw` leaves the field zero for a caller that fills it in
  later.

## Frames

`ParseFrame` parses a link-layer frame carrying IPv4 and, when the protocol is
one NetForge handles, its transport message. A frame with a protocol NetForge
does not handle is still returned, with only `Link` and `IP` populated, because
dropping it would hide evidence from the laboratory.

`Frame.Serialize` recomputes the IP header checksum and the transport checksum
from the endpoints in the IP header, and preserves what it observed rather than
normalising it: a datagram that arrived without a UDP checksum goes out without
one, link-layer padding is restored, uncovered payload bytes are restored, and
ICMP malformation is reproduced.

Builders for outgoing traffic: `EchoRequest`, `EchoReply` and
`Datagram`. Each returns a complete Ethernet frame with valid checksums.
`EchoReply` copies the identifier, sequence and data from the request,
because a host that answers a ping is required to echo them unchanged.

## What the router does with a frame

`internal/router` uses the codecs above and adds no wire format of its own. The
order of the checks is fixed, and a frame that fails any of them is counted and
dropped:

1. Ethernet is parsed. A frame that is not Ethernet II is malformed.
2. ARP is answered for an address the router owns, and every ARP message teaches
   the neighbour cache the sender pair. ARP for another host is ignored.
3. A frame that is not ARP and not IPv4 is ignored. This is where IPv6 neighbour
   discovery and multicast traffic ends up.
4. A packet for one of the router's own addresses is left to the kernel.
5. A broadcast or multicast destination is ignored rather than forwarded.
6. A packet whose time to live is one or less is not forwarded. The router
   answers the sender with ICMP type 11, code 0.
7. A destination with no route is answered with ICMP type 3, code 0.
8. A destination with a route is resolved through the neighbour cache. An
   unknown next hop produces an ARP request on the egress interface, and that
   packet is dropped; the reply arrives before the next packet from the sender.
9. A resolvable packet has its time to live decremented, its Ethernet header
   rewritten with the egress source address and the next hop address, and it is
   rebuilt with a fresh header checksum. The transport header is inspected for
   exactly one thing — its protocol number, so the right sum is recomputed — and
   its checksum is recomputed rather than copied. That is not cosmetic: a packet
   socket shows the bytes as they are on the wire, and a sender whose stack
   offloads the transport sum has written only the seed of it, so forwarding
   those bytes unchanged hands the far end a packet it drops. A UDP datagram
   whose checksum field is zero said the sender omitted the sum, and is left
   alone. See `packet.RefreshTransportChecksum` and the reasoning in
   [ARCHITECTURE.md](ARCHITECTURE.md).

### The ICMP error quote

An ICMP error carries the internet header and the first eight bytes of the
original data, so the sender can match the error to the packet it sent. The
implementation follows RFC 792 with the layout the Linux kernel reads: the
first four bytes of the original header also appear in the unused word, and the
complete original datagram starts at offset eight of the ICMP message. Stacks
that read the header at offset four and stacks that read it at offset eight
therefore both find the original packet, which is why a real client reports a
time to live expiry produced by the router.

The quote is capped so the whole ICMP datagram stays at or below 576 bytes, the
smallest buffer every IPv4 host is required to have. A packet larger than that
is truncated inside its payload, never inside its header.

## Errors

| Condition | Classification |
| --- | --- |
| buffer shorter than the header | invalid argument |
| header length, total length or transport length out of range | invalid argument |
| checksum does not verify | invalid argument |
| ICMP type not handled, non-IPv4 ethertype | unsupported |
| frame, packet or message larger than a limit | limit exceeded |

Every message names the value that was wrong and the bound it violated, for
example `total length would be 70000 bytes, the maximum is 65535`.

## What the tests cover

`internal/packet` has tests for valid packets, truncated packets at every header
boundary, malformed headers, invalid lengths, invalid checksums, oversized
packets, boundary values at the exact minimum and maximum of each field, and
round trips.

- `internal/packet/vectors_test.go` holds golden byte strings derived from the RFCs
  independently of this implementation, plus a vector produced by a separate
  generator. Round trips are tested against the golden bytes, not against
  whatever the code happens to produce.
- `internal/packet/allocations_test.go` measures allocations and asserts bounds on both count
  and bytes.
- `internal/packet/fuzz_test.go` has eight targets: `FuzzParseEthernet`,
  `FuzzParseIPv4`, `FuzzParseICMP`, `FuzzParseUDP`, `FuzzParseTCP`,
  `FuzzParseFrame`, `FuzzChecksum` and `FuzzRefreshTransportChecksum`. Each asserts round-trip and validity invariants, so a fuzzer
  finds a behavioural regression and not only a crash.
- `make fuzz-packet FUZZ_TIME=30s` runs all seven. `make fuzz-router
  FUZZ_TIME=30s` runs `FuzzDecide` and `FuzzICMPError` in `internal/router`,
  which fuzz the forwarding decision and the ICMP error builder over any byte
  string and need no interface. Go allows one `-fuzz` pattern
  per invocation, so the target runs them one at a time; a target that stopped
  early would hide the ones after it.
- Findings are kept as corpus entries in `internal/packet/testdata/fuzz/` and are
  replayed by every ordinary test run, so a fixed bug stays fixed.

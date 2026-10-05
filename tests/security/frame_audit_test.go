package security

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/netforge/netforge/internal/apperr"
	"github.com/netforge/netforge/internal/packet"
)

// The audit works from frames that are known to be well formed and then breaks
// one thing about each of them. A parser that only ever sees correct input
// proves nothing about a laboratory that will eventually be handed a corrupted
// one, so every case below is a real frame with exactly one thing wrong, and
// the requirement is always the same: refuse it, never half-read it, and never
// allocate on the strength of a length the bytes do not support.
//
// The frames are the laboratory's own: an echo request over the wire the router
// forwards, a datagram, and a TCP segment. Their checksums verify as they
// stand, so a case that changes a header also has to repair the checksum or it
// would be refused for the wrong reason.
var (
	auditEchoFrame = mustDecode("020000000001020000000002080045000028123400004001544b0a2a00020a2a00010800aa3c1234000170696e672d7061796c6f6164")
	auditUDPFrame  = mustDecode("020000000001020000000002080045000024123400004011543f0a2a00010a2a0002003514e900101cb66e6574666f726765")
	auditTCPFrame  = mustDecode("020000000001020000000002080045000030123400004006543e0a2a00010a2a00029c40005011223344000000007002ffff8dd20000020405b401010402")
	auditARPFrame  = mustDecode("ffffffffffff020000000001080600010800060400010200000000010200000000020a2a0001")
)

// mustDecode reads one of the audit's own frames at package set-up. A frame
// that is not valid hex is a mistake in this file rather than a finding about
// the product, so it stops the run instead of being reported as one.
func mustDecode(encoded string) []byte {
	data, err := hex.DecodeString(encoded)
	if err != nil {
		panic("the audit's own frame is not valid hex: " + err.Error())
	}
	return data
}

// clone copies a frame so a case can corrupt one byte of it without changing
// the frame the next case starts from.
func clone(data []byte) []byte {
	return append([]byte(nil), data...)
}

// TestTheAuditFramesAreWellFormed first. A corpus of hostile frames proves
// nothing if the frames it corrupts were already refused, so the audit states
// up front that each of its baselines parses and round trips before it starts
// breaking them.
func TestTheAuditFramesAreWellFormed(t *testing.T) {
	limits := packet.DefaultLimits()
	for name, data := range map[string][]byte{
		"echo request": auditEchoFrame,
		"datagram":     auditUDPFrame,
		"segment":      auditTCPFrame,
	} {
		t.Run(name, func(t *testing.T) {
			frame, err := packet.ParseFrame(limits, data)
			if err != nil {
				t.Fatalf("ParseFrame() = %v, want nil", err)
			}
			round, err := frame.Serialize(limits)
			if err != nil {
				t.Fatalf("Serialize() = %v, want nil", err)
			}
			if string(round) != string(data) {
				t.Errorf("the frame did not round trip:\n got %x\nwant %x", round, data)
			}
		})
	}
}

// TestMalformedPacketsAreRefused corrupts one header field of each frame at a
// time. A frame whose declared lengths, version or addresses disagree with its
// own bytes is not a frame, and reading it anyway is how a laboratory forwards
// a packet it never understood.
func TestMalformedPacketsAreRefused(t *testing.T) {
	const (
		ethernetHeader = packet.EthernetHeaderSize
		ipHeader       = packet.MinIPv4HeaderSize
	)
	cases := []struct {
		name    string
		frame   []byte
		at      int
		value   byte
		mention string
	}{
		{"IPv6 version", auditEchoFrame, ethernetHeader, 0x60, "only IPv4"},
		{"header length below the minimum", auditEchoFrame, ethernetHeader, 0x44, "minimum is 5"},
		{"header length past the packet", auditEchoFrame, ethernetHeader, 0x4f, "bytes are present"},
		{"total length below the header", auditEchoFrame, ethernetHeader + 3, 0x00, "smaller than"},
		{"total length past the bytes", auditEchoFrame, ethernetHeader + 2, 0xff, "bytes are present"},
		{"group address as the source", auditEchoFrame, 6, 0x01, "group address"},
		{"ethertype that is not IPv4", auditEchoFrame, 12, 0x86, "not IPv4"},
		{"datagram length below the header", auditUDPFrame, ethernetHeader + ipHeader + 5, 0x00, "minimum is"},
		{"datagram length past the bytes", auditUDPFrame, ethernetHeader + ipHeader + 4, 0xff, "bytes are present"},
		{"segment data offset below the minimum", auditTCPFrame, ethernetHeader + ipHeader + 12, 0x40, "offset"},
		{"an ARP frame offered as IPv4", auditARPFrame, 12, 0x08, "not IPv4"},
	}

	limits := packet.DefaultLimits()
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			corrupted := clone(test.frame)
			corrupted[test.at] = test.value

			frame, err := packet.ParseFrame(limits, corrupted)
			if err == nil {
				t.Fatalf("a frame with %s was parsed into %+v", test.name, frame)
			}
			if !strings.Contains(err.Error(), test.mention) {
				t.Errorf("the refusal %q does not mention %q", err, test.mention)
			}
			kind, ok := apperr.KindOf(err)
			if !ok {
				t.Fatalf("the refusal %q carries no kind", err)
			}
			switch kind {
			case apperr.KindInvalidArgument, apperr.KindLimitExceeded, apperr.KindUnsupported:
			default:
				t.Errorf("a malformed frame was refused with kind %q, want a refusal about the input", kind)
			}
		})
	}
}

// TestTruncatedPacketsAreRefusedAtEveryLength takes one well formed frame and
// tries every prefix of it. This is the property a laboratory depends on: a
// frame the wire cut short must be refused at whatever length it arrived,
// because the length is exactly the thing a truncation destroys. Testing one
// hand-picked truncation proves only that one.
func TestTruncatedPacketsAreRefusedAtEveryLength(t *testing.T) {
	limits := packet.DefaultLimits()
	for name, data := range map[string][]byte{
		"echo request": auditEchoFrame,
		"datagram":     auditUDPFrame,
		"segment":      auditTCPFrame,
	} {
		t.Run(name, func(t *testing.T) {
			for length := 0; length < len(data); length++ {
				if _, err := packet.ParseFrame(limits, data[:length]); err == nil {
					t.Fatalf("a %d byte prefix of a %d byte frame was accepted", length, len(data))
				}
			}
		})
	}
}

// TestACorruptChecksumIsRefusedAndSaysSo flips one bit of each checksum the
// laboratory verifies. A frame that arrives with a bad checksum is either
// corrupt or forged, and both are refused; what matters is that the refusal
// names the checksum rather than reporting a packet whose header did not verify.
func TestACorruptChecksumIsRefusedAndSaysSo(t *testing.T) {
	const (
		ethernetHeader = packet.EthernetHeaderSize
		ipHeader       = packet.MinIPv4HeaderSize
	)
	limits := packet.DefaultLimits()
	cases := []struct {
		name  string
		frame []byte
		at    int
	}{
		{"IPv4 header checksum", auditEchoFrame, ethernetHeader + 10},
		{"ICMP checksum", auditEchoFrame, ethernetHeader + ipHeader + 2},
		{"UDP checksum", auditUDPFrame, ethernetHeader + ipHeader + 6},
		{"TCP checksum", auditTCPFrame, ethernetHeader + ipHeader + 16},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			corrupted := clone(test.frame)
			corrupted[test.at] ^= 0x01

			if _, err := packet.ParseFrame(limits, corrupted); err == nil {
				t.Fatal("a frame with a corrupt checksum was parsed")
			} else if !strings.Contains(err.Error(), "checksum") {
				t.Errorf("the refusal %q does not say a checksum did not verify", err)
			}
		})
	}
}

// TestAnUncheckedChecksumIsMarkedRatherThanTrusted turns checksum verification
// off, which the router does for the packets it builds itself, and requires the
// result to say so. Verification that can be turned off without leaving a mark
// is indistinguishable from verification that was never performed, so a packet
// that skipped the check must not read as one that passed it.
func TestAnUncheckedChecksumIsMarkedRatherThanTrusted(t *testing.T) {
	corrupted := clone(auditEchoFrame)
	corrupted[packet.EthernetHeaderSize+10] ^= 0x01

	limits := packet.DefaultLimits()
	limits.VerifyChecksums = false

	frame, err := packet.ParseFrame(limits, corrupted)
	if err != nil {
		t.Fatalf("ParseFrame() = %v, want a frame that was not checked rather than a refusal", err)
	}
	if frame.IP.ChecksumValid() {
		t.Error("a corrupted header reported a checksum that verifies")
	}
	if !frame.IP.ChecksumUnchecked {
		t.Error("a frame parsed without verification is not marked as unchecked, so a later reader cannot tell")
	}
}

// TestAnOversizePacketIsRefusedRatherThanAllocated offers the parser a frame
// that claims more payload than the limit allows. The refusal has to come from
// the limit rather than from the kernel, so the bound is the one the laboratory
// asked for and not the one the machine happens to have.
func TestAnOversizePacketIsRefusedRatherThanAllocated(t *testing.T) {
	limits := packet.DefaultLimits()
	limits.MaxFrameSize = 128

	oversize := make([]byte, 4096)
	copy(oversize, auditEchoFrame)

	_, err := packet.ParseFrame(limits, oversize)
	if err == nil {
		t.Fatal("a frame past the frame limit was parsed")
	}
	if kind, ok := apperr.KindOf(err); !ok || kind != apperr.KindLimitExceeded {
		t.Errorf("the oversize frame was refused with kind %q, want a limit refusal: %v", kind, err)
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("the refusal %q does not say the limit was the reason", err)
	}
}

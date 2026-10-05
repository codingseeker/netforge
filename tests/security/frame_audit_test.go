package security

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/netforge/netforge/internal/apperr"
	"github.com/netforge/netforge/internal/packet"
)

var (
	auditEchoFrame = mustDecode("020000000001020000000002080045000028123400004001544b0a2a00020a2a00010800aa3c1234000170696e672d7061796c6f6164")
	auditUDPFrame  = mustDecode("020000000001020000000002080045000024123400004011543f0a2a00010a2a0002003514e900101cb66e6574666f726765")
	auditTCPFrame  = mustDecode("020000000001020000000002080045000030123400004006543e0a2a00010a2a00029c40005011223344000000007002ffff8dd20000020405b401010402")
	auditARPFrame  = mustDecode("ffffffffffff020000000001080600010800060400010200000000010200000000020a2a0001")
)

func mustDecode(encoded string) []byte {
	data, err := hex.DecodeString(encoded)
	if err != nil {
		panic("the audit's own frame is not valid hex: " + err.Error())
	}
	return data
}

func clone(data []byte) []byte {
	return append([]byte(nil), data...)
}

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

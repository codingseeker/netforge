package router

import (
	"encoding/binary"
	"encoding/json"
	"net/netip"
	"path/filepath"
	"testing"

	"github.com/codingseeker/netforge/internal/packet"
)

func testRouter(t *testing.T) *Router {
	t.Helper()
	r, err := New(Config{
		Scope: mustPrefix(t, "10.42.0.0/24"),
		Devices: []Device{
			{Name: "nfr-c0", Addresses: []netip.Prefix{mustPrefix(t, "10.42.0.1/30")}},
			{Name: "nfr-s0", Addresses: []netip.Prefix{mustPrefix(t, "10.42.0.5/30")}},
		},
		Routes: []Route{
			{Prefix: mustPrefix(t, "10.42.0.0/30"), Device: "nfr-c0"},
			{Prefix: mustPrefix(t, "10.42.0.4/30"), Device: "nfr-s0"},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.attach(r.cfg.Devices[0], 2, testMAC(1), 0)
	r.attach(r.cfg.Devices[1], 3, testMAC(5), 0)
	r.neighbors.Learn(mustAddr(t, "10.42.0.2"), testMAC(2), "nfr-c0")
	r.neighbors.Learn(mustAddr(t, "10.42.0.6"), testMAC(6), "nfr-s0")
	return r
}

func echoFrame(t *testing.T, source, destination string, ttl uint8) []byte {
	t.Helper()
	frame, err := packet.EchoRequest(
		packet.DefaultLimits(), testMAC(2), testMAC(1),
		mustAddr(t, source), mustAddr(t, destination), 7, 1, ttl, []byte("ping"),
	)
	if err != nil {
		t.Fatalf("EchoRequest: %v", err)
	}
	return frame
}

func datagramFrame(t *testing.T, source, destination string, ttl uint8, payload []byte) []byte {
	t.Helper()
	frame, err := packet.Datagram(
		packet.DefaultLimits(), testMAC(2), testMAC(1),
		mustAddr(t, source), mustAddr(t, destination), 40000, 9000, ttl, payload,
	)
	if err != nil {
		t.Fatalf("Datagram: %v", err)
	}
	return frame
}

func segmentFrame(t *testing.T, source, destination string, ttl uint8, payload []byte) []byte {
	t.Helper()
	segment := &packet.TCP{
		SourcePort:      40000,
		DestinationPort: 9000,
		SequenceNumber:  0x11223344,
		Flags:           byte(packet.TCPFlagPSH) | byte(packet.TCPFlagACK),
		Window:          65535,
		Payload:         payload,
	}
	body, err := segment.Serialize(mustAddr(t, source), mustAddr(t, destination))
	if err != nil {
		t.Fatalf("TCP.Serialize: %v", err)
	}
	ip := &packet.IPv4{
		TimeToLive:  ttl,
		Protocol:    packet.ProtocolTCP,
		Source:      mustAddr(t, source),
		Destination: mustAddr(t, destination),
		Payload:     body,
	}
	frame := &packet.Frame{
		Link: &packet.Ethernet{
			Destination: testMAC(1),
			Source:      testMAC(2),
			EtherType:   packet.EtherTypeIPv4,
		},
		IP:  ip,
		TCP: segment,
	}
	frame.Link.Payload = body
	serialized, err := frame.Serialize(packet.DefaultLimits())
	if err != nil {
		t.Fatalf("Frame.Serialize: %v", err)
	}
	return serialized
}

func TestForwardingRewritesTheFrame(t *testing.T) {
	r := testRouter(t)
	frame := echoFrame(t, "10.42.0.2", "10.42.0.6", 64)
	step := r.decide(r.byName["nfr-c0"], frame)
	if step.kind != forwardFrame {
		t.Fatalf("decision is %d (%s), want forward", step.kind, step.reason)
	}
	if step.egress != "nfr-s0" {
		t.Errorf("egress is %s, want nfr-s0", step.egress)
	}
	out, err := packet.ParseFrame(r.limits, step.frame)
	if err != nil {
		t.Fatalf("the forwarded frame does not parse: %v", err)
	}
	if out.Link.Destination.String() != testMAC(6).String() {
		t.Errorf("destination mac is %s, want the server", out.Link.Destination)
	}
	if out.Link.Source.String() != testMAC(5).String() {
		t.Errorf("source mac is %s, want the router side of the server link", out.Link.Source)
	}
	if out.IP.Destination != mustAddr(t, "10.42.0.6") || out.IP.Source != mustAddr(t, "10.42.0.2") {
		t.Errorf("addresses changed to %s -> %s", out.IP.Source, out.IP.Destination)
	}
	if out.IP.TimeToLive != 63 {
		t.Errorf("ttl is %d, want 63", out.IP.TimeToLive)
	}
	if !out.IP.ChecksumValid() {
		t.Error("the forwarded header checksum does not verify")
	}
	if len(out.Link.Payload) != len(frame)-14 {
		t.Errorf("payload length changed from %d to %d", len(frame)-14, len(out.Link.Payload))
	}
}

func TestForwardingAnswersTheReturnPath(t *testing.T) {
	r := testRouter(t)
	frame, err := packet.EchoRequest(
		packet.DefaultLimits(), testMAC(6), testMAC(5),
		mustAddr(t, "10.42.0.6"), mustAddr(t, "10.42.0.2"), 7, 1, 64, []byte("pong"),
	)
	if err != nil {
		t.Fatal(err)
	}
	step := r.decide(r.byName["nfr-s0"], frame)
	if step.kind != forwardFrame {
		t.Fatalf("decision is %d (%s), want forward", step.kind, step.reason)
	}
	if step.egress != "nfr-c0" {
		t.Errorf("egress is %s, want nfr-c0", step.egress)
	}
	out, err := packet.ParseFrame(r.limits, step.frame)
	if err != nil {
		t.Fatal(err)
	}
	if out.Link.Destination.String() != testMAC(2).String() {
		t.Errorf("the reply is addressed to %s, want the client", out.Link.Destination)
	}
	if out.Link.Source.String() != testMAC(1).String() {
		t.Errorf("the reply comes from %s, want the client link", out.Link.Source)
	}
	if out.IP.TimeToLive != 63 {
		t.Errorf("ttl is %d, want 63", out.IP.TimeToLive)
	}
}

func TestForwardingRepairsATransportChecksumTheSenderLeftUnfinished(t *testing.T) {
	r := testRouter(t)
	for _, test := range []struct {
		name   string
		frame  func(t *testing.T) []byte
		offset int
		verify func(t *testing.T, ip *packet.IPv4)
	}{
		{
			name: "ICMP",
			frame: func(t *testing.T) []byte {
				return echoFrame(t, "10.42.0.2", "10.42.0.6", 64)
			},
			offset: 2,
			verify: func(t *testing.T, ip *packet.IPv4) {
				message, err := packet.ParseICMP(r.limits, ip.Payload)
				if err != nil {
					t.Fatalf("ParseICMP: %v", err)
				}
				if !message.ChecksumValid() {
					t.Error("the forwarded ICMP checksum does not verify")
				}
			},
		},
		{
			name: "UDP",
			frame: func(t *testing.T) []byte {
				return datagramFrame(t, "10.42.0.2", "10.42.0.6", 64, []byte("hello"))
			},
			offset: packet.UDPChecksumOffset,
			verify: func(t *testing.T, ip *packet.IPv4) {
				if !packet.TransportValid(ip.Source, ip.Destination, packet.ProtocolUDP, ip.Payload) {
					t.Error("the forwarded datagram's checksum does not verify")
				}
			},
		},
		{
			name: "TCP",
			frame: func(t *testing.T) []byte {
				return segmentFrame(t, "10.42.0.2", "10.42.0.6", 64, []byte("hello"))
			},
			offset: packet.TCPChecksumOffset,
			verify: func(t *testing.T, ip *packet.IPv4) {
				if !packet.TransportValid(ip.Source, ip.Destination, packet.ProtocolTCP, ip.Payload) {
					t.Error("the forwarded segment's checksum does not verify")
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := test.frame(t)
			parsed, err := packet.ParseFrame(r.limits, original)
			if err != nil {
				t.Fatalf("ParseFrame: %v", err)
			}

			seeded, err := parsed.Link.Serialize(r.limits)
			if err != nil {
				t.Fatalf("Serialize: %v", err)
			}
			copy(seeded[ethernetMinimum+packet.MinIPv4HeaderSize+test.offset:], []byte{0x12, 0x34})
			step := r.decide(r.byName["nfr-c0"], seeded)
			if step.kind != forwardFrame {
				t.Fatalf("decision is %d (%s), want forward", step.kind, step.reason)
			}
			out, err := packet.ParseFrame(r.limits, step.frame)
			if err != nil {
				t.Fatalf("the forwarded frame does not parse: %v", err)
			}
			test.verify(t, out.IP)
		})
	}
}

func TestAPacketTooLargeForTheNextLinkIsAnsweredNotDropped(t *testing.T) {
	r := testRouter(t)
	for _, dev := range r.devices {
		dev.mtu = 128
	}
	frame, err := packet.EchoRequest(
		packet.DefaultLimits(), testMAC(2), testMAC(1),
		mustAddr(t, "10.42.0.2"), mustAddr(t, "10.42.0.6"), 7, 1, 64, make([]byte, 300),
	)
	if err != nil {
		t.Fatal(err)
	}
	step := r.decide(r.byName["nfr-c0"], frame)
	if step.kind != dropOversize {
		t.Fatalf("decision is %d (%s), want the packet refused as too large", step.kind, step.reason)
	}
	if step.frame == nil {
		t.Fatal("the oversized packet was dropped without an answer")
	}
	out, err := packet.ParseFrame(r.limits, step.frame)
	if err != nil {
		t.Fatalf("the answer does not parse: %v", err)
	}
	if out.ICMP == nil || out.ICMP.Type != packet.ICMPTypeDestinationUnreachable {
		t.Fatalf("the answer carries %+v, want a destination unreachable", out.ICMP)
	}
	if out.ICMP.Code != packet.ICMPCodeFragmentationNeeded {
		t.Errorf("the answer is code %d, want fragmentation needed", out.ICMP.Code)
	}
	if mtu := binary.BigEndian.Uint16(out.ICMP.RestOfHeader[2:4]); mtu != 128 {
		t.Errorf("the answer names a %d byte link, want the 128 the link really is", mtu)
	}
	if out.IP.Destination != mustAddr(t, "10.42.0.2") {
		t.Errorf("the answer went to %s, want the sender", out.IP.Destination)
	}

	small := echoFrame(t, "10.42.0.2", "10.42.0.6", 64)
	if step := r.decide(r.byName["nfr-c0"], small); step.kind != forwardFrame {
		t.Errorf("a packet that fits was not forwarded: %d (%s)", step.kind, step.reason)
	}
}

func TestADeviceWithoutAnMTUStillHasOne(t *testing.T) {
	r := testRouter(t)
	for _, dev := range r.devices {
		if dev.mtu != defaultMTU {
			t.Errorf("%s has an MTU of %d, want the default %d", dev.name, dev.mtu, defaultMTU)
		}
		if limit := dev.frameLimit(); limit != defaultMTU+ethernetMinimum {
			t.Errorf("%s accepts frames of %d bytes, want %d", dev.name, limit, defaultMTU+ethernetMinimum)
		}
	}
}

func TestTTLExpiryAnswersWithTimeExceeded(t *testing.T) {
	r := testRouter(t)
	frame := echoFrame(t, "10.42.0.2", "10.42.0.6", 1)
	step := r.decide(r.byName["nfr-c0"], frame)
	if step.kind != dropTTL {
		t.Fatalf("ttl 1 produced %d (%s), want an expiry", step.kind, step.reason)
	}
	assertICMPError(t, r, step.frame, packet.ICMPTypeTimeExceeded,
		packet.ICMPCodeTimeExceededInTransit, "10.42.0.2")
}

func TestTTLIsDecrementedBeforeForwarding(t *testing.T) {
	r := testRouter(t)
	step := r.decide(r.byName["nfr-c0"], echoFrame(t, "10.42.0.2", "10.42.0.6", 2))
	if step.kind != forwardFrame {
		t.Fatalf("ttl 2 produced %d (%s), want a forward", step.kind, step.reason)
	}
	out, err := packet.ParseFrame(r.limits, step.frame)
	if err != nil {
		t.Fatal(err)
	}
	if out.IP.TimeToLive != 1 {
		t.Errorf("ttl is %d, want 1", out.IP.TimeToLive)
	}
	if !out.IP.ChecksumValid() {
		t.Error("the rewritten checksum does not verify")
	}
}

func TestUnknownDestinationAnswersWithUnreachable(t *testing.T) {
	r := testRouter(t)
	frame := echoFrame(t, "10.42.0.2", "10.42.0.99", 64)
	step := r.decide(r.byName["nfr-c0"], frame)
	if step.kind != dropNoRoute {
		t.Fatalf("decision is %d (%s), want no route", step.kind, step.reason)
	}
	assertICMPError(t, r, step.frame, packet.ICMPTypeDestinationUnreachable,
		packet.ICMPCodeNetworkUnreachable, "10.42.0.2")
}

func TestOutOfScopeDestinationIsNotForwarded(t *testing.T) {
	r := testRouter(t)
	for _, destination := range []string{"8.8.8.8", "192.168.1.1", "10.43.0.1", "127.0.0.1"} {
		frame := echoFrame(t, "10.42.0.2", destination, 64)
		step := r.decide(r.byName["nfr-c0"], frame)
		switch step.kind {
		case dropNoRoute, dropLocal, dropIgnored:
		default:
			t.Errorf("%s produced %d (%s), want a refusal", destination, step.kind, step.reason)
		}
	}
}

func TestPacketsForTheRouterAreLeftToTheKernel(t *testing.T) {
	r := testRouter(t)
	for _, destination := range []string{"10.42.0.1", "10.42.0.5"} {
		frame := echoFrame(t, "10.42.0.2", destination, 64)
		step := r.decide(r.byName["nfr-c0"], frame)
		if step.kind != dropLocal {
			t.Errorf("%s produced %d (%s), want local", destination, step.kind, step.reason)
		}
	}
}

func TestUnknownNextHopTriggersAnARPProbe(t *testing.T) {
	r := testRouter(t)
	r.neighbors.Forget(mustAddr(t, "10.42.0.6"))
	frame := echoFrame(t, "10.42.0.2", "10.42.0.6", 64)
	step := r.decide(r.byName["nfr-c0"], frame)
	if step.kind != probeARP {
		t.Fatalf("decision is %d (%s), want an arp probe", step.kind, step.reason)
	}
	if step.target != mustAddr(t, "10.42.0.6") {
		t.Errorf("the probe asks for %s, want the server", step.target)
	}
	if step.egress != "nfr-s0" {
		t.Errorf("the probe leaves through %s, want nfr-s0", step.egress)
	}
}

func TestMalformedFramesAreDropped(t *testing.T) {
	r := testRouter(t)
	good := echoFrame(t, "10.42.0.2", "10.42.0.6", 64)
	cases := map[string][]byte{
		"empty":            {},
		"runt":             good[:7],
		"header only":      good[:14],
		"truncated ip":     good[:30],
		"bad version":      mutate(good, 14, 0x65),
		"bad ihl":          mutate(good, 14, 0x4f),
		"bad protocol":     mutate(good, 23, 0xfe),
		"zero ttl":         mutate(good, 22, 0x00),
		"broken checksum":  mutate(good, 24, 0xff),
		"short total":      mutate(good, 17, 0x0a),
		"huge total":       mutate(good, 16, 0xff),
		"unsupported type": mutate(good, 12, 0x86),
		"garbage":          {1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
	}
	for name, frame := range cases {
		step := r.decide(r.byName["nfr-c0"], frame)
		switch step.kind {
		case dropMalformed, dropIgnored, dropLocal, dropTTL:
		default:
			t.Errorf("%s produced %d (%s), want a drop", name, step.kind, step.reason)
		}
	}
}

func mutate(frame []byte, offset int, value byte) []byte {
	out := append([]byte(nil), frame...)
	if offset < len(out) {
		out[offset] = value
	}
	return out
}

func TestARPRequestsForTheRouterAreAnswered(t *testing.T) {
	r := testRouter(t)
	frame, err := arpFrame(broadcastMAC, testMAC(2), &ARP{
		Operation: arpRequest,
		Sender:    testMAC(2),
		SenderIP:  mustAddr(t, "10.42.0.2"),
		Target:    zeroMAC,
		TargetIP:  mustAddr(t, "10.42.0.1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	step := r.decide(r.byName["nfr-c0"], frame)
	if step.kind != answerARP {
		t.Fatalf("decision is %d (%s), want an arp reply", step.kind, step.reason)
	}
	reply, err := ParseARP(step.frame)
	if err != nil {
		t.Fatalf("the reply does not parse: %v", err)
	}
	if reply.Operation != arpReply {
		t.Errorf("operation is %d, want a reply", reply.Operation)
	}
	if reply.SenderIP != mustAddr(t, "10.42.0.1") {
		t.Errorf("the reply claims %s, want 10.42.0.1", reply.SenderIP)
	}
	if reply.Sender.String() != testMAC(1).String() {
		t.Errorf("the reply comes from %s, want the router side mac", reply.Sender)
	}
	if reply.TargetIP != mustAddr(t, "10.42.0.2") {
		t.Errorf("the reply targets %s, want the client", reply.TargetIP)
	}
	if step.learn != mustAddr(t, "10.42.0.2") {
		t.Errorf("the sender is not learned: %s", step.learn)
	}
}

func TestARPForAnotherHostIsIgnored(t *testing.T) {
	r := testRouter(t)
	frame, err := arpFrame(broadcastMAC, testMAC(2), &ARP{
		Operation: arpRequest,
		Sender:    testMAC(2),
		SenderIP:  mustAddr(t, "10.42.0.2"),
		Target:    zeroMAC,
		TargetIP:  mustAddr(t, "10.42.0.6"),
	})
	if err != nil {
		t.Fatal(err)
	}
	step := r.decide(r.byName["nfr-c0"], frame)
	if step.kind != dropIgnored {
		t.Errorf("decision is %d (%s), want ignore", step.kind, step.reason)
	}
}

func TestARPRepliesAreLearned(t *testing.T) {
	r := testRouter(t)
	frame, err := arpFrame(testMAC(1), testMAC(6), &ARP{
		Operation: arpReply,
		Sender:    testMAC(6),
		SenderIP:  mustAddr(t, "10.42.0.6"),
		Target:    testMAC(1),
		TargetIP:  mustAddr(t, "10.42.0.5"),
	})
	if err != nil {
		t.Fatal(err)
	}
	step := r.decide(r.byName["nfr-s0"], frame)
	if step.kind != dropIgnored {
		t.Errorf("decision is %d (%s), want ignore", step.kind, step.reason)
	}
	if step.learn != mustAddr(t, "10.42.0.6") || step.learnMAC.String() != testMAC(6).String() {
		t.Errorf("the reply was not learned: %s", step.learn)
	}
}

func TestBroadcastAndMulticastAreNotForwarded(t *testing.T) {
	r := testRouter(t)
	for _, destination := range []string{"255.255.255.255", "224.0.0.1", "239.1.2.3"} {
		frame := echoFrame(t, "10.42.0.2", destination, 64)
		step := r.decide(r.byName["nfr-c0"], frame)
		if step.kind != dropIgnored {
			t.Errorf("%s produced %d (%s), want ignore", destination, step.kind, step.reason)
		}
	}
}

func TestSourceAddressFollowsTheIngressLink(t *testing.T) {
	r := testRouter(t)
	frame := echoFrame(t, "10.42.0.2", "10.42.0.99", 64)
	step := r.decide(r.byName["nfr-c0"], frame)
	out, err := packet.ParseFrame(r.limits, step.frame)
	if err != nil {
		t.Fatalf("the error does not parse: %v", err)
	}
	if out.IP.Source != mustAddr(t, "10.42.0.1") {
		t.Errorf("the error comes from %s, want the client link address", out.IP.Source)
	}
	if out.IP.Destination != mustAddr(t, "10.42.0.2") {
		t.Errorf("the error goes to %s, want the client", out.IP.Destination)
	}
	if out.IP.TimeToLive != 64 {
		t.Errorf("the error has ttl %d, want 64", out.IP.TimeToLive)
	}
}

func assertICMPError(t *testing.T, r *Router, frame []byte, kind packet.ICMPType, code uint8, to string) {
	t.Helper()
	if frame == nil {
		t.Fatal("no icmp error was produced")
	}
	out, err := packet.ParseFrame(r.limits, frame)
	if err != nil {
		t.Fatalf("the icmp error does not parse: %v", err)
	}
	if out.ICMP == nil {
		t.Fatal("the frame carries no icmp message")
	}
	if out.ICMP.Type != kind || out.ICMP.Code != code {
		t.Errorf("icmp %d/%d, want %d/%d", out.ICMP.Type, out.ICMP.Code, kind, code)
	}
	if out.IP.Destination.String() != to {
		t.Errorf("the error is sent to %s, want %s", out.IP.Destination, to)
	}
	if len(out.ICMP.RestOfHeader) == 0 || out.ICMP.RestOfHeader[0]>>4 != 4 {
		t.Errorf("the quoted header is not IPv4: %x", out.ICMP.RestOfHeader)
	}
	if len(out.ICMP.Payload) < 8 {
		t.Errorf("the quote carries %d bytes, want at least 8", len(out.ICMP.Payload))
	}
	if out.Link.Destination.String() != testMAC(2).String() {
		t.Errorf("the error is addressed to %s, want the sender", out.Link.Destination)
	}
}

func TestCountersFollowDecisions(t *testing.T) {
	r := testRouter(t)
	dev := r.byName["nfr-c0"]
	r.apply(dev, r.decide(dev, echoFrame(t, "10.42.0.2", "10.42.0.6", 64)))
	r.apply(dev, r.decide(dev, echoFrame(t, "10.42.0.2", "10.42.0.6", 1)))
	r.apply(dev, r.decide(dev, echoFrame(t, "10.42.0.2", "10.42.0.99", 64)))
	r.apply(dev, r.decide(dev, []byte{1, 2, 3}))
	r.apply(dev, r.decide(dev, echoFrame(t, "10.42.0.2", "10.42.0.1", 64)))

	stats := r.Stats()
	if stats.Forwarded != 0 {
		t.Errorf("forwarded is %d, want 0 because no device is open", stats.Forwarded)
	}
	if stats.SendErrors == 0 {
		t.Error("sending to a closed device was not counted")
	}
	if stats.TTLExceeded != 1 {
		t.Errorf("ttl expiries are %d, want 1", stats.TTLExceeded)
	}
	if stats.NoRoute != 1 {
		t.Errorf("no-route drops are %d, want 1", stats.NoRoute)
	}
	if stats.Malformed != 1 {
		t.Errorf("malformed drops are %d, want 1", stats.Malformed)
	}
	if stats.Local != 1 {
		t.Errorf("local drops are %d, want 1", stats.Local)
	}
	if stats.Routes != 2 {
		t.Errorf("routes are %d, want 2", stats.Routes)
	}
	if stats.QueueSize != defaultQueueSize {
		t.Errorf("queue size is %d, want %d", stats.QueueSize, defaultQueueSize)
	}
}

func TestQueueOverflowIsCounted(t *testing.T) {
	r, err := New(Config{
		Scope:     mustPrefix(t, "10.42.0.0/24"),
		Devices:   []Device{{Name: "nfr-c0", Addresses: []netip.Prefix{mustPrefix(t, "10.42.0.1/30")}}},
		Routes:    []Route{{Prefix: mustPrefix(t, "10.42.0.0/30"), Device: "nfr-c0"}},
		QueueSize: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.attach(r.cfg.Devices[0], 2, testMAC(1), 0)
	for i := 0; i < 16; i++ {
		if !r.queue.offer(work{ingress: "nfr-c0", frame: []byte{byte(i)}}) {
			r.counters.addQueueFull()
		}
	}
	stats := r.Stats()
	if stats.QueueFull != 14 {
		t.Errorf("dropped %d packets, want 14", stats.QueueFull)
	}
	if stats.QueueDepth != 2 {
		t.Errorf("the queue holds %d items, want 2", stats.QueueDepth)
	}
}

func TestConfigValidation(t *testing.T) {
	valid := Config{
		Scope: mustPrefix(t, "10.42.0.0/24"),
		Devices: []Device{
			{Name: "nfr-c0", Addresses: []netip.Prefix{mustPrefix(t, "10.42.0.1/30")}},
		},
		Routes: []Route{{Prefix: mustPrefix(t, "10.42.0.0/30"), Device: "nfr-c0"}},
	}
	if _, err := New(valid); err != nil {
		t.Fatalf("a valid configuration was refused: %v", err)
	}

	cases := map[string]func(Config) Config{
		"no scope":        func(c Config) Config { c.Scope = netip.Prefix{}; return c },
		"no devices":      func(c Config) Config { c.Devices = nil; return c },
		"empty device":    func(c Config) Config { c.Devices[0].Name = ""; return c },
		"long device":     func(c Config) Config { c.Devices[0].Name = "a-very-long-device-name"; return c },
		"repeated device": func(c Config) Config { c.Devices = append(c.Devices, c.Devices[0]); return c },
		"no addresses":    func(c Config) Config { c.Devices[0].Addresses = nil; return c },
		"bad address": func(c Config) Config {
			c.Devices[0].Addresses = []netip.Prefix{mustPrefix(t, "2001:db8::1/128")}
			return c
		},
		"address off lab": func(c Config) Config {
			c.Devices[0].Addresses = []netip.Prefix{mustPrefix(t, "192.168.9.1/30")}
			return c
		},
		"unknown device":   func(c Config) Config { c.Routes[0].Device = "nfr-x9"; return c },
		"route off scope":  func(c Config) Config { c.Routes[0].Prefix = mustPrefix(t, "10.9.0.0/16"); return c },
		"relative stats":   func(c Config) Config { c.StatsPath = "stats.json"; return c },
		"host bits in use": func(c Config) Config { c.Routes[0].Prefix = mustPrefix(t, "10.42.0.1/30"); return c },
	}
	for name, mutate := range cases {
		if _, err := New(mutate(valid)); err == nil {
			t.Errorf("%s was accepted, want a refusal", name)
		}
	}
}

func TestStatsFileRoundTrip(t *testing.T) {
	path := t.TempDir() + "/stats.json"
	r, err := New(Config{
		Scope:     mustPrefix(t, "10.42.0.0/24"),
		Devices:   []Device{{Name: "nfr-c0", Addresses: []netip.Prefix{mustPrefix(t, "10.42.0.1/30")}}},
		Routes:    []Route{{Prefix: mustPrefix(t, "10.42.0.0/30"), Device: "nfr-c0"}},
		StatsPath: path,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.writeStats(); err != nil {
		t.Fatalf("writeStats: %v", err)
	}
	stats, err := ReadStats(path)
	if err != nil {
		t.Fatalf("ReadStats: %v", err)
	}
	if stats.Routes != 1 {
		t.Errorf("the file reports %d routes, want 1", stats.Routes)
	}
	if _, err := ReadStats(path + ".missing"); err == nil {
		t.Error("a missing stats file was accepted")
	}
}

func TestDispatchIgnoresOtherModes(t *testing.T) {
	if handled, _ := Dispatch(nil); handled {
		t.Error("an empty argument list was claimed")
	}
	if handled, _ := Dispatch([]string{"netforge"}); handled {
		t.Error("an unrelated mode was claimed")
	}
	if handled, code := Dispatch([]string{mode}); !handled || code != 2 {
		t.Errorf("a missing configuration returned %v/%d, want handled/2", handled, code)
	}
	if handled, code := Dispatch([]string{mode, "{"}); !handled || code != 2 {
		t.Errorf("invalid json returned %v/%d, want handled/2", handled, code)
	}
	if handled, code := Dispatch([]string{mode, `{"scope":"10.0.0.0/8"}`}); !handled || code != 1 {
		t.Errorf("an invalid configuration returned %v/%d, want handled/1", handled, code)
	}
}

func TestICMPErrorQuotesTheOriginalDatagramWhereStacksLook(t *testing.T) {
	r := testRouter(t)
	original := echoFrame(t, "10.42.0.2", "10.42.0.6", 1)
	step := r.decide(r.byName["nfr-c0"], original)
	if step.kind != dropTTL {
		t.Fatalf("ttl 1 produced %d (%s), want an expiry", step.kind, step.reason)
	}
	out, err := packet.ParseFrame(r.limits, step.frame)
	if err != nil {
		t.Fatalf("the icmp error does not parse: %v", err)
	}
	datagram := original[14:]
	message := out.ICMP
	if string(message.RestOfHeader[:]) != string(datagram[0:4]) {
		t.Errorf("the unused word %x is not the start of the original header %x",
			message.RestOfHeader[:], datagram[0:4])
	}
	if string(message.Payload) != string(datagram) {
		t.Fatalf("the quote carries %d bytes, the original datagram is %d",
			len(message.Payload), len(datagram))
	}
	if message.Payload[0]>>4 != 4 {
		t.Errorf("the quoted datagram does not start with IPv4: %#x", message.Payload[0])
	}
	if message.Payload[9] != byte(packet.ProtocolICMP) {
		t.Errorf("the quoted datagram carries protocol %d, want %d", message.Payload[9], packet.ProtocolICMP)
	}
	headerLength := int(message.Payload[0]&0x0f) * 4
	if len(message.Payload) < headerLength+8 {
		t.Errorf("the quote is %d bytes, a stack needs the header and the 8 byte icmp message",
			len(message.Payload))
	}
}

func TestICMPErrorStaysInsideTheClassicalLimit(t *testing.T) {
	r := testRouter(t)
	frame, err := packet.EchoRequest(r.limits, testMAC(2), testMAC(1),
		mustAddr(t, "10.42.0.2"), mustAddr(t, "10.42.0.6"), 7, 1, 1, make([]byte, 1400))
	if err != nil {
		t.Fatalf("EchoRequest: %v", err)
	}
	step := r.decide(r.byName["nfr-c0"], frame)
	out, err := packet.ParseFrame(r.limits, step.frame)
	if err != nil {
		t.Fatalf("the icmp error does not parse: %v", err)
	}
	if out.IP.TotalLength() > 576 {
		t.Errorf("the error is %d bytes, a host may drop anything above 576", out.IP.TotalLength())
	}
	if out.ICMP.Payload[9] != byte(packet.ProtocolICMP) {
		t.Errorf("a truncated quote lost the header a stack needs")
	}
}

func TestDispatchReportsWhyTheRouterCannotRun(t *testing.T) {
	stats := filepath.Join(t.TempDir(), "router.json")
	settings, err := json.Marshal(Config{
		Scope:     mustPrefix(t, "10.42.0.0/24"),
		Devices:   []Device{{Name: "nfr-missing0", Addresses: []netip.Prefix{mustPrefix(t, "10.42.0.1/30")}}},
		StatsPath: stats,
	})
	if err != nil {
		t.Fatal(err)
	}
	handled, code := Dispatch([]string{mode, string(settings)})
	if !handled || code != 1 {
		t.Fatalf("a router that cannot open its device returned %v/%d, want handled/1", handled, code)
	}
	report, err := ReadStats(stats)
	if err != nil {
		t.Fatalf("the failure was not published: %v", err)
	}
	if report.State != "failed" {
		t.Errorf("the published state is %q, want failed", report.State)
	}
	if report.Error == "" {
		t.Error("the published failure carries no reason")
	}
	if report.Revision == 0 {
		t.Error("the published failure carries no revision")
	}
}

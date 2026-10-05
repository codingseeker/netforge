package router

import (
	"net/netip"
	"testing"

	"github.com/netforge/netforge/internal/packet"
)

func FuzzDecide(f *testing.F) {
	seeds := [][]byte{
		{},
		{0xff},
		make([]byte, 14),
		make([]byte, 60),
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	scope := mustPrefixF(f, "10.42.0.0/24")
	gateway := mustPrefixF(f, "10.42.0.1/30")
	service := mustPrefixF(f, "10.42.0.5/30")
	client := mustPrefixF(f, "10.42.0.0/30")
	server := mustPrefixF(f, "10.42.0.4/30")

	f.Fuzz(func(t *testing.T, data []byte) {
		r, err := New(Config{
			Scope: scope,
			Devices: []Device{
				{Name: "nfr-c0", Addresses: []netip.Prefix{gateway}},
				{Name: "nfr-s0", Addresses: []netip.Prefix{service}},
			},
			Routes: []Route{
				{Prefix: client, Device: "nfr-c0"},
				{Prefix: server, Device: "nfr-s0"},
			},
			QueueSize: 1,
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		r.attach(r.cfg.Devices[0], 2, testMAC(1), 0)
		r.attach(r.cfg.Devices[1], 3, testMAC(5), 0)

		dev := r.byName["nfr-c0"]
		step := r.decide(dev, data)
		switch step.kind {
		case dropIgnored, dropLocal, dropMalformed, dropNoRoute, dropTTL,
			forwardFrame, answerARP, icmpError, probeARP:
		default:
			t.Fatalf("unknown decision %d", step.kind)
		}
		if step.kind == forwardFrame || step.kind == answerARP || step.kind == icmpError {
			if len(step.frame) < 14 {
				t.Fatalf("a %d decision produced a %d byte frame", step.kind, len(step.frame))
			}
			if step.kind == forwardFrame && step.egress == "" {
				t.Fatal("a forward decision has no egress")
			}
		}
		if step.kind != probeARP && step.target.IsValid() {
			t.Fatalf("decision %d carries a stray probe target %s", step.kind, step.target)
		}
		if step.learn.IsValid() && !step.learn.Is4() {
			t.Fatalf("decision %d learns a non-IPv4 address", step.kind)
		}
	})
}

func mustPrefixF(f *testing.F, text string) netip.Prefix {
	prefix, err := netip.ParsePrefix(text)
	if err != nil {
		f.Fatalf("prefix %q: %v", text, err)
	}
	return prefix
}

func FuzzICMPError(f *testing.F) {
	seeds := [][]byte{
		{},
		make([]byte, 14),
		make([]byte, 34),
		make([]byte, 60),
		make([]byte, 1514),
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	scope := mustPrefixF(f, "10.42.0.0/24")
	gateway := mustPrefixF(f, "10.42.0.1/30")
	service := mustPrefixF(f, "10.42.0.5/30")
	client := mustPrefixF(f, "10.42.0.0/30")
	source := netip.MustParseAddr("10.42.0.2")

	f.Fuzz(func(t *testing.T, data []byte) {
		r, err := New(Config{
			Scope: scope,
			Devices: []Device{
				{Name: "nfr-c0", Addresses: []netip.Prefix{gateway}},
				{Name: "nfr-s0", Addresses: []netip.Prefix{service}},
			},
			Routes: []Route{{Prefix: client, Device: "nfr-c0"}},
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		dev := r.cfg.Devices[0]
		r.attach(dev, 2, testMAC(1), 0)
		frame := r.icmpFrame(r.byName["nfr-c0"], data, source,
			packet.ICMPTypeTimeExceeded, packet.ICMPCodeTimeExceededInTransit, 0)
		if frame == nil {
			return
		}
		out, err := packet.ParseFrame(r.limits, frame)
		if err != nil {
			t.Fatalf("the icmp error built from %d bytes does not parse: %v", len(data), err)
		}
		if out.IP.TotalLength() > 576 {
			t.Fatalf("the error is %d bytes, above the classical limit", out.IP.TotalLength())
		}
		headerLength := int(out.ICMP.Payload[0]&0x0f) * 4
		if headerLength < 20 || len(out.ICMP.Payload) < headerLength {
			t.Fatalf("the quote holds %d bytes for a %d byte header", len(out.ICMP.Payload), headerLength)
		}
	})
}

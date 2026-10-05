package router

import (
	"net"
	"net/netip"
	"testing"
	"time"
)

func testMAC(last byte) net.HardwareAddr {
	return net.HardwareAddr{0x02, 0, 0, 0, 0, last}
}

func TestARPRoundTrip(t *testing.T) {
	want := &ARP{
		Operation: arpRequest,
		Sender:    testMAC(1),
		SenderIP:  mustAddr(t, "10.42.0.2"),
		Target:    zeroMAC,
		TargetIP:  mustAddr(t, "10.42.0.1"),
	}
	frame, err := arpFrame(broadcastMAC, want.Sender, want)
	if err != nil {
		t.Fatalf("arpFrame: %v", err)
	}
	got, err := ParseARP(frame)
	if err != nil {
		t.Fatalf("ParseARP: %v", err)
	}
	if got.Operation != want.Operation || got.SenderIP != want.SenderIP || got.TargetIP != want.TargetIP {
		t.Errorf("round trip changed the message: %+v", got)
	}
	if got.Sender.String() != want.Sender.String() {
		t.Errorf("sender %s, want %s", got.Sender, want.Sender)
	}
	if got.Target.String() != want.Target.String() {
		t.Errorf("target %s, want %s", got.Target, want.Target)
	}
}

func TestARPRejectsMalformedFrames(t *testing.T) {
	good, err := arpFrame(broadcastMAC, testMAC(1), &ARP{
		Operation: arpRequest,
		Sender:    testMAC(1),
		SenderIP:  mustAddr(t, "10.42.0.2"),
		Target:    zeroMAC,
		TargetIP:  mustAddr(t, "10.42.0.1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func([]byte) []byte{
		"empty":            func([]byte) []byte { return nil },
		"short":            func(b []byte) []byte { return b[:20] },
		"long":             func(b []byte) []byte { return append(b, 0) },
		"wrong ethertype":  func(b []byte) []byte { b[12] = 0x00; return b },
		"wrong hardware":   func(b []byte) []byte { b[15] = 2; return b },
		"wrong protocol":   func(b []byte) []byte { b[17] = 0x86; return b },
		"wrong mac length": func(b []byte) []byte { b[18] = 8; return b },
		"wrong ip length":  func(b []byte) []byte { b[19] = 16; return b },
		"unknown op":       func(b []byte) []byte { b[21] = 9; return b },
	}
	for name, mutate := range cases {
		frame := mutate(append([]byte(nil), good...))
		if _, err := ParseARP(frame); err == nil {
			t.Errorf("%s was parsed, want an error", name)
		}
	}
}

func TestNeighborCacheIsBounded(t *testing.T) {
	neighbors, err := NewNeighbors(4)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 32; i++ {
		address := netip.AddrFrom4([4]byte{10, 42, 0, byte(i + 1)})
		neighbors.Learn(address, testMAC(byte(i+1)), "eth0")
		time.Sleep(time.Millisecond)
	}
	if got := neighbors.Len(); got > 4 {
		t.Errorf("the cache holds %d entries, want at most 4", got)
	}
	if _, ok := neighbors.Lookup(mustAddr(t, "10.42.0.32")); !ok {
		t.Error("the most recent entry was evicted")
	}
	if _, ok := neighbors.Lookup(mustAddr(t, "10.42.0.1")); ok {
		t.Error("the oldest entry survived eviction")
	}
}

func TestNeighborCacheIgnoresUnusableAddresses(t *testing.T) {
	neighbors, err := NewNeighbors(0)
	if err != nil {
		t.Fatal(err)
	}
	neighbors.Learn(mustAddr(t, "10.42.0.2"), net.HardwareAddr{1, 2, 3}, "eth0")
	neighbors.Learn(mustAddr(t, "10.42.0.3"), net.HardwareAddr{0x01, 0, 0, 0, 0, 1}, "eth0")
	neighbors.Learn(netip.Addr{}, testMAC(9), "eth0")
	neighbors.Learn(mustAddr(t, "10.42.0.4"), net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, "eth0")
	if got := neighbors.Len(); got != 0 {
		t.Errorf("the cache stored %d unusable entries", got)
	}
}

func TestNeighborCacheForgets(t *testing.T) {
	neighbors, err := NewNeighbors(0)
	if err != nil {
		t.Fatal(err)
	}
	neighbors.Learn(mustAddr(t, "10.42.0.2"), testMAC(2), "eth0")
	neighbors.Forget(mustAddr(t, "10.42.0.2"))
	if _, ok := neighbors.Lookup(mustAddr(t, "10.42.0.2")); ok {
		t.Error("a forgotten address is still known")
	}
	neighbors.Forget(mustAddr(t, "10.42.0.2"))
}

func TestNeighborCacheLimitIsValidated(t *testing.T) {
	if _, err := NewNeighbors(1 << 20); err == nil {
		t.Error("an excessive neighbor limit was accepted")
	}
	neighbors, err := NewNeighbors(0)
	if err != nil {
		t.Fatal(err)
	}
	if neighbors.Len() != 0 {
		t.Error("a new cache is not empty")
	}
}

func TestNetworkOrderConversion(t *testing.T) {
	cases := map[uint16]uint16{
		0x0800: 0x0008,
		0x0806: 0x0608,
		0x0300: 0x0003,
		0xffff: 0xffff,
		0x0000: 0x0000,
	}
	for host, network := range cases {
		if got := htons(host); got != network {
			t.Errorf("htons(%#04x) is %#04x, want %#04x", host, got, network)
		}
	}
}

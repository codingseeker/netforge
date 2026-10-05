package iface

import (
	"net/netip"
	"testing"
)

func TestNameValidation(t *testing.T) {
	for _, name := range []string{
		"nfc0", "nfr-c0", "nfr-s12", "eth0.100", "br-lan", "a_b", "0", "x",
	} {
		if err := ValidateName("test", "interface", name); err != nil {
			t.Errorf("ValidateName rejected %q: %v", name, err)
		}
	}
	for _, name := range []string{
		"", "nfc 0", "nfc\n0", "sla/sh", "nfc:0", "nfc@0", "lo:1",
		"aaaaaaaaaaaaaaaa", "..", ".",
		"-nfc0", "--", "-", "-master", "-scope",
	} {
		if err := ValidateName("test", "interface", name); err == nil {
			t.Errorf("ValidateName accepted %q", name)
		}
	}
	long := ""
	for i := 0; i < MaxNameLength; i++ {
		long += "a"
	}
	if err := ValidateName("test", "interface", long); err != nil {
		t.Errorf("ValidateName rejected a name of exactly %d characters: %v", MaxNameLength, err)
	}
	if err := ValidateName("test", "interface", long+"a"); err == nil {
		t.Error("ValidateName accepted a name one character too long")
	}
}

func TestPlanNamesFitTheKernel(t *testing.T) {
	for i := 0; i < 64; i++ {
		for _, name := range []string{
			SideName(prefixClient, i), SideName(prefixServer, i),
			RouterName(prefixClient, i), RouterName(prefixServer, i),
		} {
			if len(name) > MaxNameLength {
				t.Errorf("%q is %d characters, the kernel allows %d", name, len(name), MaxNameLength)
			}
		}
	}
	if got := SideName(prefixClient, 3); got != "nfc3" {
		t.Errorf("SideName client 3 is %q", got)
	}
	if got := SideName(prefixServer, 3); got != "nfs3" {
		t.Errorf("SideName server 3 is %q", got)
	}
	if got := RouterName(prefixClient, 0); got != "nfr-c0" {
		t.Errorf("RouterName client 0 is %q", got)
	}
	if got := RouterName(prefixServer, 7); got != "nfr-s7" {
		t.Errorf("RouterName server 7 is %q", got)
	}
}

func TestMTUValidation(t *testing.T) {
	for _, mtu := range []int{MinMTU, 1500, 9000, MaxMTU} {
		if err := ValidateMTU("test", mtu); err != nil {
			t.Errorf("ValidateMTU rejected %d: %v", mtu, err)
		}
	}
	for _, mtu := range []int{-1, 0, MinMTU - 1, MaxMTU + 1} {
		if err := ValidateMTU("test", mtu); err == nil {
			t.Errorf("ValidateMTU accepted %d", mtu)
		}
	}
}

func TestPrefixValidation(t *testing.T) {
	for _, value := range []string{"10.42.0.0/24", "192.168.0.0/16", "10.0.0.0/8", "10.42.0.0/30"} {
		prefix := netip.MustParsePrefix(value)
		if err := ValidatePrefix("test", "network", prefix); err != nil {
			t.Errorf("ValidatePrefix rejected %q: %v", value, err)
		}
	}
	for _, value := range []string{
		"10.42.0.0/0", "10.42.0.0/7", "10.42.0.0/31", "10.42.0.0/32", "fd00::/120",
	} {
		prefix := netip.MustParsePrefix(value)
		if err := ValidatePrefix("test", "network", prefix); err == nil {
			t.Errorf("ValidatePrefix accepted %q", value)
		}
	}
	if err := ValidatePrefix("test", "network", netip.Prefix{}); err == nil {
		t.Error("ValidatePrefix accepted the zero prefix")
	}
}

func TestAddressValidation(t *testing.T) {
	if err := ValidateAddress("test", "address", netip.MustParseAddr("10.42.0.1")); err != nil {
		t.Errorf("ValidateAddress rejected an IPv4 address: %v", err)
	}
	for _, value := range []string{"fd00::1", "::"} {
		if err := ValidateAddress("test", "address", netip.MustParseAddr(value)); err == nil {
			t.Errorf("ValidateAddress accepted %q", value)
		}
	}
	if err := ValidateAddress("test", "address", netip.Addr{}); err == nil {
		t.Error("ValidateAddress accepted the zero address")
	}
}

func TestStateLinkLookup(t *testing.T) {
	state := State{Links: []Link{
		{Name: "lo", Index: 1, Up: true, MTU: 65536, Addresses: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/8")}},
		{Name: "nfc0", Index: 2, Up: true, MTU: 1500, Addresses: []netip.Prefix{netip.MustParsePrefix("10.42.0.2/30")}},
	}}
	link, err := state.Link("nfc0")
	if err != nil {
		t.Fatal(err)
	}
	if link.MTU != 1500 || !link.Up {
		t.Errorf("link nfc0 is %+v", link)
	}
	if _, err := state.Link("absent"); err == nil {
		t.Error("Link returned an interface that is not present")
	}
	if len(state.Links) != 2 {
		t.Errorf("the state holds %d links", len(state.Links))
	}
}

func TestBitsOfIsBounded(t *testing.T) {
	for _, tc := range []struct{ in, want int }{
		{-1, 32}, {0, 0}, {8, 8}, {24, 24}, {32, 32}, {33, 32},
	} {
		if got := bitsOf(tc.in); got != tc.want {
			t.Errorf("bitsOf(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestFlagSetLookup(t *testing.T) {
	link := ipLink{Flags: []string{"BROADCAST", "MULTICAST", "UP"}}
	if !link.flagSet().contains("UP") {
		t.Error("the UP flag was not found")
	}
	if link.flagSet().contains("LOWER_UP") {
		t.Error("an absent flag was reported as present")
	}
}

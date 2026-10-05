package loadbalancer

import (
	"net/netip"
	"testing"
)

func FuzzABackendAddressIsParsedNotResolved(f *testing.F) {

	for _, seed := range []string{
		"",
		"10.0.0.1:7000",
		"127.0.0.1:1",
		"[::1]:7000",
		"[fd00::1]:65535",
		"0.0.0.0:7000",
		"[::]:7000",
		"224.0.0.1:7000",
		"255.255.255.255:7000",
		"10.0.0.1:0",
		"10.0.0.1:65536",
		"10.0.0.1",
		"10.0.0.1:",
		":7000",
		"backend.internal:7000",
		"10.0.0.0/24:7000",
		"10.0.0.1:7000 ",
		" 10.0.0.1:7000",
		"10.0.0.1:0x1bb",
		"[fd00::1%eth0]:7000",
		"10.0.0.1:7000:8000",
		"١٠.0.0.1:7000",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, address string) {
		endpoint, err := parseEndpoint("loadbalancer.parseEndpoint", "backend 0", address, false)
		if err != nil {

			if endpoint.IsValid() {
				t.Fatalf("parseEndpoint(%q) returned %s and an error, want an unusable address", address, endpoint)
			}
			return
		}

		if reparse, parseErr := netip.ParseAddrPort(address); parseErr != nil || reparse != endpoint {
			t.Errorf("parseEndpoint(%q) = %s, but netip reads it as %s (%v), want the two to agree",
				address, endpoint, reparse, parseErr)
		}
		if endpoint.Port() == 0 {
			t.Errorf("parseEndpoint(%q) = %s, want no zero port on a backend", address, endpoint)
		}
		if endpoint.Addr().IsUnspecified() || endpoint.Addr().IsMulticast() {
			t.Errorf("parseEndpoint(%q) = %s, want a host to send to", address, endpoint)
		}
		if endpoint.Addr().Zone() != "" {
			t.Errorf("parseEndpoint(%q) = %s, want no interface zone: the address alone decides where it goes",
				address, endpoint)
		}
	})
}

func FuzzAListenAddressIsParsedNotResolved(f *testing.F) {
	for _, seed := range []string{
		"127.0.0.1:0",
		"127.0.0.1:7000",
		"[::]:0",
		"0.0.0.0:0",
		"[fd00::1]:0",
		"balancer.internal:7000",
		"127.0.0.1",
		"224.0.0.1:7000",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, address string) {
		endpoint, err := parseEndpoint("loadbalancer.parseEndpoint", "the listen address", address, true)
		if err != nil {
			return
		}
		if endpoint.Addr().IsUnspecified() || endpoint.Addr().IsMulticast() {
			t.Errorf("parseEndpoint(%q) = %s, want an address a client can send to", address, endpoint)
		}
		if reparse, parseErr := netip.ParseAddrPort(address); parseErr != nil || reparse != endpoint {
			t.Errorf("parseEndpoint(%q) = %s, but netip reads it as %s (%v), want the two to agree",
				address, endpoint, reparse, parseErr)
		}
	})
}

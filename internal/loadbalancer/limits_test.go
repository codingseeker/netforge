package loadbalancer

import (
	"strings"
	"testing"
	"time"

	"github.com/netforge/netforge/internal/apperr"
	"github.com/netforge/netforge/internal/transport"
)

func TestTheDefaultLimitsAreBoundedAndUsable(t *testing.T) {
	t.Parallel()
	checked, err := DefaultLimits().Validate()
	if err != nil {
		t.Fatalf("DefaultLimits().Validate() = %v, want nil", err)
	}
	limits := checked
	if limits.MaxBackends <= 0 || limits.MaxBackends > MaxBackendCeiling {
		t.Errorf("backends = %d, want between 1 and the ceiling of %d", limits.MaxBackends, MaxBackendCeiling)
	}
	if limits.MaxConnections <= 0 || limits.MaxConnections > MaxConnectionCeiling {
		t.Errorf("connections = %d, want between 1 and the ceiling of %d", limits.MaxConnections, MaxConnectionCeiling)
	}
	if limits.MaxBackendConnections <= 0 || limits.MaxBackendConnections > limits.MaxConnections {
		t.Errorf("connections per backend = %d, want between 1 and the %d a balancer may hold",
			limits.MaxBackendConnections, limits.MaxConnections)
	}
	if limits.Replicas <= 0 || limits.Replicas > MaxReplicaCeiling {
		t.Errorf("replicas = %d, want between 1 and the ceiling of %d", limits.Replicas, MaxReplicaCeiling)
	}

	if limits.HealthTimeout >= limits.HealthInterval {
		t.Errorf("the health timeout %s is not shorter than the interval %s it is spaced by",
			limits.HealthTimeout, limits.HealthInterval)
	}
	if limits.DrainTimeout < limits.HealthInterval {
		t.Errorf("the drain timeout %s is shorter than the health interval %s it is measured in",
			limits.DrainTimeout, limits.HealthInterval)
	}
	if limits.HealthInterval > MaxHealthInterval || limits.DrainTimeout > MaxDrainTimeout {
		t.Errorf("the defaults are above the ceilings: interval %s of %s, drain %s of %s",
			limits.HealthInterval, MaxHealthInterval, limits.DrainTimeout, MaxDrainTimeout)
	}

	if limits.Transport.MaxConnections != limits.MaxConnections {
		t.Errorf("the transport was given %d connections, want the balancer's own %d",
			limits.Transport.MaxConnections, limits.MaxConnections)
	}
}

func TestALimitAboveItsCeilingIsRefused(t *testing.T) {
	t.Parallel()
	over := []struct {
		name   string
		limits func(*Limits)
	}{
		{"backends", func(l *Limits) { l.MaxBackends = MaxBackendCeiling + 1 }},
		{"connections", func(l *Limits) { l.MaxConnections = MaxConnectionCeiling + 1 }},
		{"replicas", func(l *Limits) { l.Replicas = MaxReplicaCeiling + 1 }},
		{"health interval", func(l *Limits) { l.HealthInterval = MaxHealthInterval + time.Second }},
		{"health timeout", func(l *Limits) { l.HealthTimeout = MaxHealthTimeout + time.Second }},
		{"drain timeout", func(l *Limits) { l.DrainTimeout = MaxDrainTimeout + time.Second }},
		{"exchange timeout", func(l *Limits) { l.ExchangeTimeout = MaxExchangeTimeout + time.Second }},
	}
	for _, limit := range over {
		config := testConfig(PolicyRoundRobin, "127.0.0.1:9301")
		limit.limits(&config.Limits)
		if _, err := New(config); err == nil {
			t.Errorf("New() with %s above its ceiling = nil, want a refusal", limit.name)
		} else if !apperr.IsKind(err, apperr.KindLimitExceeded) {
			t.Errorf("New() with %s above its ceiling = %v, want a refusal for exceeding a limit",
				limit.name, err)
		}

		atCeiling := testConfig(PolicyRoundRobin, "127.0.0.1:9301")
		limit.limits(&atCeiling.Limits)

		switch limit.name {
		case "backends":
			atCeiling.Limits.MaxBackends = MaxBackendCeiling
		case "connections":
			atCeiling.Limits.MaxConnections = MaxConnectionCeiling
			atCeiling.Limits.MaxBackendConnections = 1
		case "replicas":
			atCeiling.Limits.Replicas = MaxReplicaCeiling
		case "health interval":
			atCeiling.Limits.HealthInterval = MaxHealthInterval
			atCeiling.Limits.DrainTimeout = MaxHealthInterval
		case "health timeout":
			atCeiling.Limits.HealthTimeout = MaxHealthTimeout
			atCeiling.Limits.HealthInterval = 2 * MaxHealthTimeout
			atCeiling.Limits.DrainTimeout = 2 * MaxHealthTimeout
		case "drain timeout":
			atCeiling.Limits.DrainTimeout = MaxDrainTimeout
		case "exchange timeout":
			atCeiling.Limits.ExchangeTimeout = MaxExchangeTimeout
		}
		if _, err := New(atCeiling); err != nil {
			t.Errorf("New() with %s at its ceiling = %v, want nil", limit.name, err)
		}
	}
}

func TestContradictoryTimingsAreRefused(t *testing.T) {
	t.Parallel()
	config := testConfig(PolicyRoundRobin, "127.0.0.1:9302")
	config.Limits.HealthInterval = 100 * time.Millisecond
	config.Limits.HealthTimeout = 200 * time.Millisecond
	if _, err := New(config); err == nil {
		t.Error("New() with a health timeout longer than its interval = nil, want a refusal")
	}
	config = testConfig(PolicyRoundRobin, "127.0.0.1:9302")
	config.Limits.HealthInterval = time.Second
	config.Limits.DrainTimeout = 100 * time.Millisecond
	if _, err := New(config); err == nil {
		t.Error("New() with a drain shorter than its health interval = nil, want a refusal")
	}

	config = testConfig(PolicyRoundRobin, "127.0.0.1:9302")
	config.Limits.Transport.MaxPayload = transport.MaxPayloadSize * 8
	if _, err := New(config); err == nil {
		t.Error("New() with a transport limit above its ceiling = nil, want a refusal")
	} else if !apperr.IsKind(err, apperr.KindLimitExceeded) {
		t.Errorf("New() = %v, want a refusal for exceeding a limit", err)
	}
}

func TestAZeroLimitMeansTheDefault(t *testing.T) {
	t.Parallel()
	defaults, err := DefaultLimits().Validate()
	if err != nil {
		t.Fatalf("DefaultLimits().Validate() = %v, want nil", err)
	}
	zeroed := Limits{
		MaxBackends: 0, MaxConnections: 0, MaxBackendConnections: 0, Replicas: 0,
		HealthInterval: 0, HealthTimeout: 0, DrainTimeout: 0, ExchangeTimeout: 0,
	}
	checked, err := zeroed.Validate()
	if err != nil {
		t.Fatalf("a limits document of zeroes = %v, want the defaults", err)
	}
	if checked.MaxBackends != defaults.MaxBackends || checked.MaxConnections != defaults.MaxConnections {
		t.Errorf("a limits document of zeroes = %+v, want the defaults it left out", checked)
	}
	if checked.HealthInterval != defaults.HealthInterval || checked.Transport.MaxPayload != defaults.Transport.MaxPayload {
		t.Errorf("a limits document of zeroes = %+v, want every unset limit defaulted", checked)
	}
}

func TestALimitThatIsNotPositiveIsTheDefault(t *testing.T) {
	t.Parallel()
	defaults, err := DefaultLimits().Validate()
	if err != nil {
		t.Fatalf("DefaultLimits().Validate() = %v, want nil", err)
	}
	notPositive := []struct {
		name   string
		limits func(*Limits)
	}{
		{"backends", func(l *Limits) { l.MaxBackends = -1 }},
		{"connections", func(l *Limits) { l.MaxConnections = -1 }},
		{"connections per backend", func(l *Limits) { l.MaxBackendConnections = -1 }},
		{"replicas", func(l *Limits) { l.Replicas = -1 }},
		{"health interval", func(l *Limits) { l.HealthInterval = -time.Second }},
		{"health timeout", func(l *Limits) { l.HealthTimeout = -time.Second }},
		{"failure threshold", func(l *Limits) { l.FailureThreshold = -1 }},
		{"recovery threshold", func(l *Limits) { l.RecoveryThreshold = -1 }},
		{"drain timeout", func(l *Limits) { l.DrainTimeout = -time.Second }},
		{"exchange timeout", func(l *Limits) { l.ExchangeTimeout = -time.Second }},
	}
	for _, limit := range notPositive {
		config := testConfig(PolicyRoundRobin, "127.0.0.1:9303")

		config.Limits = DefaultLimits()
		limit.limits(&config.Limits)
		checked, err := config.Limits.Validate()
		if err != nil {
			t.Errorf("Validate() with a negative %s = %v, want the default", limit.name, err)
			continue
		}
		if checked.MaxBackends != defaults.MaxBackends || checked.MaxConnections != defaults.MaxConnections {
			t.Errorf("Validate() with a negative %s = %+v, want the defaults it did not set",
				limit.name, checked)
		}
	}
}

func TestAPerBackendLimitAboveTheWholeIsRefused(t *testing.T) {
	t.Parallel()
	config := testConfig(PolicyRoundRobin, "127.0.0.1:9304")
	config.Limits.MaxConnections = 4
	config.Limits.MaxBackendConnections = 8
	if _, err := New(config); err == nil {
		t.Error("New() with more connections per backend than in total = nil, want a refusal")
	}
	config.Limits.MaxBackendConnections = 4
	if _, err := New(config); err != nil {
		t.Errorf("New() with the two limits equal = %v, want nil", err)
	}
}

func TestABackendAddressIsParsedAndNeverResolved(t *testing.T) {
	t.Parallel()
	refused := []struct {
		name    string
		address string
	}{
		{"a name", "backend.internal:7000"},
		{"a name in a laboratory", "service-1.lab:7000"},
		{"no port", "10.0.0.1"},
		{"port zero", "10.0.0.1:0"},
		{"a port that is not one", "10.0.0.1:70000"},
		{"a port that is not a number", "10.0.0.1:http"},
		{"the unspecified address", "0.0.0.0:7000"},
		{"the unspecified address in six", "[::]:7000"},
		{"a multicast group", "224.0.0.1:7000"},
		{"a broadcast address", "255.255.255.255:7000"},
		{"a prefix rather than a host", "10.0.0.0/24:7000"},
		{"an interface rather than a network", "[fd00::1%eth0]:7000"},
		{"empty", ""},
		{"whitespace", " 10.0.0.1:7000 "},
	}
	for _, address := range refused {
		config := testConfig(PolicyRoundRobin, address.address)
		if _, err := New(config); err == nil {
			t.Errorf("New() with %s as a backend = nil, want a refusal", address.name)
		}
	}
	accepted := []string{
		"10.0.0.1:7000",
		"127.0.0.1:1",
		"[fd00::1]:7000",
		"[2001:db8::1]:65535",
	}
	for _, address := range accepted {
		config := testConfig(PolicyRoundRobin, address)
		balancer, err := New(config)
		if err != nil {
			t.Errorf("New() with %q as a backend = %v, want nil", address, err)
			continue
		}
		if err := balancer.Close(); err != nil {
			t.Errorf("Close() = %v, want nil", err)
		}
	}
}

func TestAListenAddressMayAskForAnyPort(t *testing.T) {
	t.Parallel()
	config := testConfig(PolicyRoundRobin, "127.0.0.1:7000")
	config.Listen = "127.0.0.1:0"
	balancer := startBalancer(t, config)
	if got := balancer.Address(); !strings.HasSuffix(got, ":0") && balancer.Address() == "127.0.0.1:0" {
		t.Errorf("address = %q, want the port the kernel chose", got)
	}
	if _, port, err := splitPort(balancer.Address()); err != nil || port == 0 {
		t.Errorf("address = %q on port %d, want a real port", balancer.Address(), port)
	}

	config.Listen = "balancer.internal:7000"
	if _, err := New(config); err == nil {
		t.Error("New() with a name as the listen address = nil, want a refusal")
	}
}

func TestAScopeMustBeAPrefix(t *testing.T) {
	t.Parallel()
	for _, scope := range []string{"10.0.0.1/24", "10.0.0.0/33", "not a prefix", "10.0.0.0/", "/24", "10.0.0.1"} {
		config := testConfig(PolicyRoundRobin, "10.0.0.1:7000")
		config.Scope = scope
		if _, err := New(config); err == nil {
			t.Errorf("New() with the scope %q = nil, want a refusal", scope)
		}
	}

	config := testConfig(PolicyRoundRobin, "10.0.0.9:7000")
	config.Scope = "10.0.0.0/24"
	if _, err := New(config); err != nil {
		t.Errorf("New() with a canonical scope = %v, want nil", err)
	}

	config.Scope = "0.0.0.0/0"
	if _, err := New(config); err == nil {
		t.Error("New() with a default route as the scope = nil, want a refusal")
	}
}

func TestTheAddressIsWhereTheBalancerIsListening(t *testing.T) {
	t.Parallel()
	balancer := startBalancer(t, testConfig(PolicyRoundRobin, "127.0.0.1:7000"))
	address, port, err := splitPort(balancer.Address())
	if err != nil {
		t.Fatalf("the address %q could not be read: %v", balancer.Address(), err)
	}
	if address != "127.0.0.1" {
		t.Errorf("address = %q, want the loopback address it was asked for", address)
	}
	if port == 0 {
		t.Error("the balancer reports port 0, want the port it is listening on")
	}
}

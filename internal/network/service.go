package network

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/netforge/netforge/internal/apperr"
	"github.com/netforge/netforge/internal/investigator"
	"github.com/netforge/netforge/internal/probe"
	"github.com/netforge/netforge/internal/service"
)

const (
	serviceMode  = "netforge-service"
	serviceReady = 5 * time.Second
	serviceStop  = 2 * time.Second
	servicePoll  = 20 * time.Millisecond
)

// ServiceSpec asks a laboratory namespace to serve an endpoint set. The
// namespace has to be one the laboratory created, which is checked before the
// service is started.
type ServiceSpec struct {
	// Namespace is the laboratory namespace that serves the endpoints.
	Namespace string
	// TCPPort and UDPPort are the echo endpoints. Zero disables them.
	TCPPort uint16
	UDPPort uint16
	// StreamPort is the stream echo endpoint. Zero disables it.
	StreamPort uint16
	// RUDPPort is the reliable-UDP echo endpoint. Zero disables it.
	RUDPPort uint16
	// DNSPort is the resolver endpoint. Zero disables it.
	DNSPort uint16
	// DNSNames maps a name the resolver answers for to the address it answers
	// with.
	DNSNames map[string]string
	// TLSPort and HTTPPort are the TLS and HTTP endpoints. Zero disables them.
	TLSPort  uint16
	HTTPPort uint16
	// HTTPPrefix is the path the HTTP endpoint serves. It defaults to "/".
	HTTPPrefix string
	// HTTPStatus is the status the HTTP endpoint answers with.
	HTTPStatus int
	// HTTPBody is the body the HTTP endpoint answers with.
	HTTPBody string
	// MaxConnections and MaxRequest bound the service. Zero uses the service
	// defaults.
	MaxConnections int
	MaxRequest     int
	// Timeout bounds one exchange. Zero uses the service default.
	Timeout time.Duration
}

// serviceProcess is one running service.
type serviceProcess struct {
	spec ServiceSpec
	pid  int
	path string
	// bound is what the service reported it was listening on.
	bound []string
}

// ServiceFor builds the specification for one namespace serving one protocol on
// one port.
//
// The endpoint kind follows the protocol, because the probe that measures it
// speaks that protocol: a TCP endpoint answers a TCP probe and the TCP half of
// a TLS or HTTP chain, and an HTTP endpoint answers an HTTP request on the same
// connection. A protocol with no endpoint NetForge serves is refused rather than
// mapped to something else, because a laboratory that measured a port nothing
// was bound to would be reporting a closed port as a measurement.
func ServiceFor(namespace string, protocol probe.Protocol, port uint16) (ServiceSpec, error) {
	const op = "network.ServiceFor"
	if strings.TrimSpace(namespace) == "" {
		return ServiceSpec{}, apperr.InvalidArgument(op, "a service needs a namespace to serve from", nil)
	}
	if port == 0 {
		return ServiceSpec{}, apperr.InvalidArgument(op, "a service needs a port to bind", nil)
	}
	spec := ServiceSpec{Namespace: namespace}
	switch protocol {
	case probe.ProtocolTCP:
		spec.TCPPort = port
	case probe.ProtocolUDP:
		spec.UDPPort = port
	case probe.ProtocolTLS:
		spec.TLSPort = port
	case probe.ProtocolHTTP:
		spec.HTTPPort = port
	default:
		return ServiceSpec{}, apperr.Unsupported(op,
			"protocol "+strconv.Quote(string(protocol))+" has no endpoint NetForge serves", nil)
	}
	return spec, nil
}

// ServiceEndpoints returns what each running service is bound to, which is the
// answer to "what was listening", asked of the laboratory rather than guessed
// from the specification it was given.
func (l *Lab) ServiceEndpoints() []BoundService {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]BoundService, 0, len(l.services))
	for _, process := range l.services {
		out = append(out, BoundService{
			Namespace: process.spec.Namespace,
			Endpoints: append([]string(nil), process.bound...),
		})
	}
	return out
}

// BoundService is one running service and the endpoints it reports listening.
type BoundService struct {
	// Namespace is the laboratory namespace serving the endpoints.
	Namespace string
	// Endpoints are the "protocol/address" strings the service reported, which
	// is what was really bound rather than what was asked for.
	Endpoints []string
}

// Services returns the endpoints the laboratory is serving, which is what a
// diagnosis reads to know what was supposed to be on the far end.
func (l *Lab) Services() []ServiceSpec {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]ServiceSpec, 0, len(l.services))
	for _, process := range l.services {
		out = append(out, process.spec)
	}
	return out
}

// ServiceStatus reports what one service says about itself.
func (l *Lab) ServiceStatus(index int) (service.Status, error) {
	const op = "network.ServiceStatus"
	l.mu.Lock()
	process := l.serviceAt(index)
	l.mu.Unlock()
	if process == nil {
		return service.Status{}, apperr.NotFound(op, "the laboratory has no such service", nil)
	}
	status, err := service.ReadStatus(process.path)
	if err != nil {
		return service.Status{}, err
	}
	return status, nil
}

func (l *Lab) serviceAt(index int) *serviceProcess {
	if index < 0 || index >= len(l.services) {
		return nil
	}
	return l.services[index]
}

// startServices starts every requested service and puts it under the
// laboratory's ownership, so a teardown stops the listeners before the
// namespaces they live in disappear.
func (l *Lab) startServices() error {
	const op = "network.startServices"
	if len(l.spec.Services) == 0 {
		return nil
	}
	for i, spec := range l.spec.Services {
		record, err := l.startService(op, i, spec)
		if err != nil {
			return err
		}
		l.mu.Lock()
		l.services = append(l.services, record)
		l.mu.Unlock()
	}
	return nil
}

// startService starts one service process and hands back its record. It is
// shared with RestartService so a restarted service is configured, registered
// for teardown and awaited in exactly the way a first start is: a restart that
// skipped any of those would leave a listener nothing owns.
func (l *Lab) startService(op string, index int, spec ServiceSpec) (*serviceProcess, error) {
	// The same check a probe gets: a service may only be started in a namespace
	// this laboratory created.
	if err := l.probeScope().Check(op, spec.Namespace, l.plan.Network.Addr().String()); err != nil {
		return nil, fmt.Errorf("%s: service %d: %w", op, index, err)
	}
	l.mu.Lock()
	space, ok := l.spaces[spec.Namespace]
	l.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%s: service %d: the namespace %s is not part of this laboratory",
			op, index, spec.Namespace)
	}
	settings, err := json.Marshal(l.serviceConfig(index, spec))
	if err != nil {
		return nil, fmt.Errorf("%s: encode the service configuration: %w", op, err)
	}
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	process, err := space.Handle.Start(self, serviceMode, string(settings))
	if err != nil {
		return nil, fmt.Errorf("%s: start the service in %s: %w", op, spec.Namespace, err)
	}
	record := &serviceProcess{
		spec: spec,
		pid:  process,
		path: filepath.Join(l.work, "service-"+strconv.Itoa(index)+".json"),
	}
	// Each start registers its own stop. A restart therefore leaves the earlier
	// stop behind, which costs one signal to a process that has already gone:
	// stopping a process that is not running is not an error, and the teardown
	// still stops the process that is.
	if err := l.stack.Defer("service-"+strconv.Itoa(index), func(context.Context) error {
		return stopService(process)
	}); err != nil {
		_ = stopService(process)
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	if err := l.awaitService(record, space); err != nil {
		_ = stopService(process)
		return nil, err
	}
	return record, nil
}

// RestartService stops a laboratory service and starts it again with the same
// configuration.
//
// It exists for the recovery step that has to prove a service was the problem
// rather than assume it. Restarting is destructive, so it is only ever reached
// through an authorized recovery action, and the laboratory refuses a namespace
// that is not one of its own services.
func (l *Lab) RestartService(ctx context.Context, namespace string) error {
	const op = "network.RestartService"
	if err := apperr.FromContext(ctx, op); err != nil {
		return err
	}
	index := l.serviceIndex(namespace)
	if index < 0 {
		return apperr.NotFound(op, "no service in this laboratory runs in namespace "+strconv.Quote(namespace), nil)
	}
	l.mu.Lock()
	spec := l.services[index].spec
	l.mu.Unlock()

	if err := stopService(l.servicePID(index)); err != nil {
		return fmt.Errorf("%s: stop the service in %s: %w", op, namespace, err)
	}
	record, err := l.startService(op, index, spec)
	if err != nil {
		return err
	}
	l.mu.Lock()
	l.services[index] = record
	l.mu.Unlock()
	return nil
}

// serviceIndex finds the service that runs in a namespace.
func (l *Lab) serviceIndex(namespace string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	for index, process := range l.services {
		if process.spec.Namespace == namespace {
			return index
		}
	}
	return -1
}

func (l *Lab) servicePID(index int) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	process := l.serviceAt(index)
	if process == nil {
		return 0
	}
	return process.pid
}

// serviceConfig renders a service specification into a service configuration,
// filling in the laboratory's own status path and the service defaults.
func (l *Lab) serviceConfig(index int, spec ServiceSpec) service.Config {
	defaults := service.DefaultConfig()
	config := service.Config{
		Namespace:      spec.Namespace,
		StatusPath:     filepath.Join(l.work, "service-"+strconv.Itoa(index)+".json"),
		TCPPort:        spec.TCPPort,
		UDPPort:        spec.UDPPort,
		StreamPort:     spec.StreamPort,
		RUDPPort:       spec.RUDPPort,
		DNSPort:        spec.DNSPort,
		DNSNames:       spec.DNSNames,
		TLSPort:        spec.TLSPort,
		HTTPPort:       spec.HTTPPort,
		HTTPPrefix:     spec.HTTPPrefix,
		HTTPStatus:     spec.HTTPStatus,
		HTTPBody:       spec.HTTPBody,
		MaxConnections: spec.MaxConnections,
		MaxRequest:     spec.MaxRequest,
		Timeout:        spec.Timeout,
	}
	if config.HTTPPrefix == "" {
		config.HTTPPrefix = defaults.HTTPPrefix
	}
	if config.HTTPStatus == 0 {
		config.HTTPStatus = defaults.HTTPStatus
	}
	if config.HTTPBody == "" {
		config.HTTPBody = defaults.HTTPBody
	}
	if config.MaxConnections == 0 {
		config.MaxConnections = defaults.MaxConnections
	}
	if config.MaxRequest == 0 {
		config.MaxRequest = defaults.MaxRequest
	}
	if config.Timeout == 0 {
		config.Timeout = defaults.Timeout
	}
	return config
}

// awaitService waits for a service to report that its listeners are bound, so
// a laboratory is never returned before it can answer.
func (l *Lab) awaitService(record *serviceProcess, space *Space) error {
	const op = "network.awaitService"
	deadline := time.Now().Add(serviceReady)
	for time.Now().Before(deadline) {
		status, err := service.ReadStatus(record.path)
		if err == nil {
			switch status.State {
			case service.StateRunning:
				record.bound = status.Bound
				return nil
			case service.StateFailed:
				return fmt.Errorf("%s: the service in %s stopped: %s", op, record.spec.Namespace, status.Error)
			}
		}
		if !space.Handle.Alive() || syscall.Kill(record.pid, 0) != nil {
			return fmt.Errorf("%s: the service in %s is gone", op, record.spec.Namespace)
		}
		time.Sleep(servicePoll)
	}
	return fmt.Errorf("%s: the service in %s did not become ready within %s", op, record.spec.Namespace, serviceReady)
}

// stopService asks a service to stop and insists that it does.
func stopService(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		return fmt.Errorf("stop the service: %w", err)
	}
	deadline := time.Now().Add(serviceStop)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return nil
		}
		time.Sleep(servicePoll)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	return nil
}

// Prober returns the probe engine of the laboratory, or nil when the laboratory
// was not created with one.
func (l *Lab) Prober() *probe.Engine { return l.prober }

// Investigator returns an investigator bound to this laboratory, so a failure
// inside it is explained from measurements taken inside it.
func (l *Lab) Investigator(limits investigator.Limits) (*investigator.Investigator, error) {
	const op = "network.Investigator"
	if l.prober == nil {
		return nil, apperr.Internal(op, "the laboratory has no probe engine", nil)
	}
	return investigator.New(l, l, limits)
}

// Route asks the kernel what it would do to reach a target from a namespace,
// and reports the answer as the kernel gave it. It is how a diagnosis learns
// whether a route exists, without guessing from the probes.
func (l *Lab) Route(_ context.Context, namespace, target string) (investigator.Route, error) {
	const op = "network.Route"
	address, err := netip.ParseAddr(target)
	if err != nil {
		return investigator.Route{}, apperr.InvalidArgument(op,
			target+" is not an address this laboratory can route to", err)
	}
	stdout, stderr, code, err := l.Run(namespace, "ip", "route", "get", address.String())
	if err != nil {
		return investigator.Route{}, err
	}
	if code != 0 {
		// The kernel refusing to route is an answer, not a failure to ask.
		return investigator.Route{Found: false, Reason: strings.TrimSpace(stderr)}, nil
	}
	// "203.0.113.7 via 10.57.0.1 dev nfc0 src 10.57.0.2 uid 0". The next hop
	// and the device are the fields after their names, not part of them.
	route := investigator.Route{Found: true}
	fields := strings.Fields(stdout)
	for index := 0; index+1 < len(fields); index++ {
		switch fields[index] {
		case "dev":
			route.Device = fields[index+1]
		case "via":
			route.Via = fields[index+1]
		case "src":
			route.Source = fields[index+1]
		}
	}
	if route.Device == "" {
		// A route with no device is not a route the packet can leave by, and
		// reporting it as one would let a diagnosis blame the wrong layer.
		return investigator.Route{Found: false, Reason: strings.TrimSpace(stdout)}, nil
	}
	return route, nil
}

// Probe runs one protocol probe from inside a laboratory namespace and reports
// what came back. A probe that fails returns a result that says so, because a
// failure is the evidence an investigator needs.
func (l *Lab) Probe(ctx context.Context, request probe.Request) (probe.Result, error) {
	const op = "network.Probe"
	if l.prober == nil {
		return probe.Result{}, apperr.Internal(op, "the laboratory has no probe engine", nil)
	}
	return l.prober.Run(ctx, request)
}

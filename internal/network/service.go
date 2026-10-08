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

	"github.com/codingseeker/netforge/internal/apperr"
	"github.com/codingseeker/netforge/internal/investigator"
	"github.com/codingseeker/netforge/internal/probe"
	"github.com/codingseeker/netforge/internal/service"
)

const (
	serviceMode  = "netforge-service"
	serviceReady = 5 * time.Second
	serviceStop  = 2 * time.Second
	servicePoll  = 20 * time.Millisecond
)

type ServiceSpec struct {
	Namespace string

	TCPPort uint16
	UDPPort uint16

	StreamPort uint16

	RUDPPort uint16

	DNSPort uint16

	DNSNames map[string]string

	TLSPort  uint16
	HTTPPort uint16

	HTTPPrefix string

	HTTPStatus int

	HTTPBody string

	MaxConnections int
	MaxRequest     int

	Timeout time.Duration
}

type serviceProcess struct {
	spec ServiceSpec
	pid  int
	path string

	bound []string
}

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
	case probe.ProtocolRUDP:
		spec.RUDPPort = port
	default:
		return ServiceSpec{}, apperr.Unsupported(op,
			"protocol "+strconv.Quote(string(protocol))+" has no endpoint NetForge serves", nil)
	}
	return spec, nil
}

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

type BoundService struct {
	Namespace string

	Endpoints []string
}

func (l *Lab) Services() []ServiceSpec {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]ServiceSpec, 0, len(l.services))
	for _, process := range l.services {
		out = append(out, process.spec)
	}
	return out
}

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

func (l *Lab) startService(op string, index int, spec ServiceSpec) (*serviceProcess, error) {

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

func (l *Lab) Prober() *probe.Engine { return l.prober }

func (l *Lab) Investigator(limits investigator.Limits) (*investigator.Investigator, error) {
	const op = "network.Investigator"
	if l.prober == nil {
		return nil, apperr.Internal(op, "the laboratory has no probe engine", nil)
	}
	return investigator.New(l, l, limits)
}

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

		return investigator.Route{Found: false, Reason: strings.TrimSpace(stderr)}, nil
	}

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

		return investigator.Route{Found: false, Reason: strings.TrimSpace(stdout)}, nil
	}
	return route, nil
}

func (l *Lab) Probe(ctx context.Context, request probe.Request) (probe.Result, error) {
	const op = "network.Probe"
	if l.prober == nil {
		return probe.Result{}, apperr.Internal(op, "the laboratory has no probe engine", nil)
	}
	return l.prober.Run(ctx, request)
}

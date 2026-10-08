package network

import (
	"time"

	"github.com/codingseeker/netforge/internal/loadbalancer"
	"github.com/codingseeker/netforge/internal/router"
)

type Summary struct {
	ID string

	Network string

	Routing Routing

	KernelForwarding string

	RouterNamespace string

	RouterLinks []string

	Nodes []SummaryNode

	Services []SummaryService

	Router *router.Stats

	RouterError string

	Balancer *BalancerSummary
}

type BalancerSummary struct {
	Namespace string

	Listen string

	Policy string

	Backends []BalancerBackend

	Served int

	Refused int

	Healthy int

	At time.Time
}

func (b BalancerSummary) Marked() time.Time { return b.At }

type BalancerBackend struct {
	Address string

	State string

	Accepted int

	Forwarded int

	Checks int

	Failed int

	Recoveries int
}

type SummaryNode struct {
	Role string

	Namespace string

	Address string

	Gateway string
}

type SummaryService struct {
	Namespace string

	Endpoints []string
}

func (l *Lab) Routing() Routing { return l.spec.Routing }

func (l *Lab) Summary() Summary {
	l.mu.Lock()
	spec, plan := l.spec, l.plan
	l.mu.Unlock()

	forwarding := "unknown"
	if on, err := l.Forwarding(); err == nil {
		forwarding = "0"
		if on {
			forwarding = "1"
		}
	}
	summary := Summary{
		ID:               spec.ID,
		Network:          plan.Network.String(),
		Routing:          spec.Routing,
		KernelForwarding: forwarding,
		RouterNamespace:  plan.RouterNS,
	}
	for _, link := range plan.Links {
		summary.RouterLinks = append(summary.RouterLinks,
			link.RouterIf+" "+link.Node.Router.String())
	}
	for _, node := range plan.Nodes {
		summary.Nodes = append(summary.Nodes, SummaryNode{
			Role:      string(node.Role),
			Namespace: node.Namespace,
			Address:   node.Address.String(),
			Gateway:   node.Gateway.String(),
		})
	}
	for _, bound := range l.ServiceEndpoints() {
		summary.Services = append(summary.Services, SummaryService{
			Namespace: bound.Namespace,
			Endpoints: bound.Endpoints,
		})
	}
	return summary
}

func (s *Summary) RecordRouter(stats router.Stats) {
	if s == nil {
		return
	}
	s.RouterError = ""
	s.Router = &stats
}

func (s *Summary) RecordRouterFailure(reason string) {
	if s == nil {
		return
	}
	s.Router = nil
	s.RouterError = reason
}

func (l *Lab) BalancerSummary() *BalancerSummary {
	return l.BalancerSummaryAfter(time.Time{}, 0)
}

func (l *Lab) BalancerSummaryAfter(after time.Time, window time.Duration) *BalancerSummary {
	for attempt := 0; ; attempt++ {
		summary := l.readBalancerSummary()
		if summary == nil {
			return nil
		}
		if after.IsZero() || window <= 0 {
			return summary
		}
		if summary.At.After(after) {
			return summary
		}
		if time.Now().After(after.Add(window)) {
			return summary
		}
		time.Sleep(balancerPoll)
	}
}

func (l *Lab) readBalancerSummary() *BalancerSummary {
	l.mu.Lock()
	process := l.balancer
	l.mu.Unlock()
	if process == nil {
		return nil
	}
	status, err := loadbalancer.ReadStatus(process.path)
	if err != nil {
		return nil
	}
	summary := &BalancerSummary{
		At:      status.At,
		Listen:  status.Listen,
		Policy:  status.Policy,
		Served:  status.Stats.Served,
		Refused: status.Stats.RefusedNoBackend,
		Healthy: status.Stats.Healthy,
	}
	if l.spec.Balancer != nil {
		summary.Namespace = l.spec.Balancer.Namespace
	}
	for _, backend := range status.Backends {
		summary.Backends = append(summary.Backends, BalancerBackend{
			Address:    backend.Address,
			State:      backend.State,
			Accepted:   backend.Accepted,
			Forwarded:  backend.Forwarded,
			Checks:     backend.Checks,
			Failed:     backend.FailedChecks,
			Recoveries: backend.Recoveries,
		})
	}
	return summary
}

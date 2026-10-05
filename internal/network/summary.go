package network

import (
	"github.com/netforge/netforge/internal/router"
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

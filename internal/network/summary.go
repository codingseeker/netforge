package network

import (
	"github.com/netforge/netforge/internal/router"
)

// Summary is what a laboratory is, as a value.
//
// It is a value rather than a view because the only moment a run can report it
// is the moment before the laboratory is taken apart: a reader wants to know
// which namespaces existed, which addresses they held, what was listening and
// who forwarded, and none of that is answerable once the namespaces are gone.
type Summary struct {
	// ID is the laboratory's identifier, which is what its namespaces are named
	// from.
	ID string
	// Network is the network the laboratory was built on.
	Network string
	// Routing is who forwarded in the router namespace.
	Routing Routing
	// KernelForwarding is what the router namespace reported for
	// net.ipv4.ip_forward: "1", "0", or "unknown" when the kernel would not
	// answer. A software-routed run reads "0", which is the proof that the
	// kernel forwarded nothing and every forwarded byte was NetForge's.
	KernelForwarding string
	// RouterNamespace is the namespace that forwards.
	RouterNamespace string
	// RouterLinks are the router's addresses, one per link, as "device address".
	RouterLinks []string
	// Nodes are the client and server namespaces with the addresses they held.
	Nodes []SummaryNode
	// Services are the endpoints each serving namespace reported bound.
	Services []SummaryService
	// Router is the software router's counters, when a run reported them.
	Router *router.Stats
	// RouterError is why they could not be reported, when they could not be.
	RouterError string
}

// SummaryNode is one namespace of a laboratory and the address it held.
type SummaryNode struct {
	// Role is client, server or router.
	Role string
	// Namespace is the namespace the node lives in.
	Namespace string
	// Address is the address the node's interface held.
	Address string
	// Gateway is the next hop the node was given, which is the router's address
	// on that link.
	Gateway string
}

// SummaryService is one serving namespace and what it reported listening.
type SummaryService struct {
	// Namespace is the laboratory namespace serving the endpoints.
	Namespace string
	// Endpoints are the "protocol/address" strings the service reported.
	Endpoints []string
}

// Routing returns who forwards in the router namespace.
func (l *Lab) Routing() Routing { return l.spec.Routing }

// Summary describes the laboratory as it is now.
//
// The software router's counters are not part of it: reading them means asking
// a running router, which is why they are recorded separately with RecordRouter
// before the laboratory is destroyed.
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

// RecordRouter stores the software router's counters on a summary, so a run can
// report what forwarded its traffic after the router has gone.
func (s *Summary) RecordRouter(stats router.Stats) {
	if s == nil {
		return
	}
	s.RouterError = ""
	s.Router = &stats
}

// RecordRouterFailure stores why the router's counters could not be read. A
// diagnostic that failed is reported as a failure rather than as an absent
// reading, because "no counters" and "the counters could not be asked for" are
// different answers.
func (s *Summary) RecordRouterFailure(reason string) {
	if s == nil {
		return
	}
	s.Router = nil
	s.RouterError = reason
}

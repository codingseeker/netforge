package recovery

import (
	"net/netip"
	"strconv"
	"strings"

	"github.com/netforge/netforge/internal/apperr"
	"github.com/netforge/netforge/internal/config"
	"github.com/netforge/netforge/internal/namespace"
)

// Scope is the part of a laboratory a recovery is allowed to touch. It is
// taken from the experiment's own scope rather than from the running
// laboratory, so an authorization cannot be widened by whatever happens to be
// up at the time it is granted.
type Scope struct {
	// Network is the network the scope grants. An address outside it is out of
	// scope even when it is reachable.
	Network netip.Prefix
	// Namespaces are the laboratory namespaces the scope grants.
	Namespaces []string
	// Experiment is the experiment the scope belongs to. An authorization
	// granted for one experiment is refused for another, so a grant cannot be
	// carried from a previous run into this one.
	Experiment string
}

// NewScope builds a scope from a network and the namespaces of an experiment.
func NewScope(experiment, network string, namespaces []string) (Scope, error) {
	const op = "recovery.NewScope"
	if experiment == "" {
		return Scope{}, apperr.InvalidArgument(op, "a scope needs the experiment it belongs to", nil)
	}
	prefix, err := config.ParseNetwork(network)
	if err != nil {
		return Scope{}, err
	}
	if len(namespaces) == 0 {
		return Scope{}, apperr.InvalidArgument(op, "a scope needs at least one namespace", nil)
	}
	seen := make(map[string]struct{}, len(namespaces))
	granted := make([]string, 0, len(namespaces))
	for i, name := range namespaces {
		if err := namespace.ValidateName(op, "scope.namespaces["+strconv.Itoa(i)+"]", name); err != nil {
			return Scope{}, apperr.Validation(op, err.Error(), err)
		}
		if _, duplicate := seen[name]; duplicate {
			return Scope{}, apperr.Validation(op, "scope.namespaces duplicates "+name, nil)
		}
		seen[name] = struct{}{}
		granted = append(granted, name)
	}
	return Scope{Network: prefix, Namespaces: granted, Experiment: experiment}, nil
}

// ScopeOf builds the scope of a validated experiment document. It is the one
// place a scope is derived, so the controller and an operator cannot end up
// working from different grants.
func ScopeOf(document config.Config) (Scope, error) {
	return NewScope(document.ExperimentID(), document.Scope.Network, document.Scope.Namespaces)
}

// AllowsNamespace reports whether the scope grants a laboratory namespace.
func (s Scope) AllowsNamespace(name string) bool {
	_, granted := s.lookup(name)
	return granted
}

func (s Scope) lookup(name string) (int, bool) {
	for i, granted := range s.Namespaces {
		if granted == name {
			return i, true
		}
	}
	return 0, false
}

// AllowsAddress reports whether an address falls inside the granted network.
// An address with a zone, a port or a host part is refused rather than
// trimmed: a target that does not parse as a bare address is not a target
// this package will act on.
func (s Scope) AllowsAddress(address string) bool {
	addr, err := netip.ParseAddr(address)
	return err == nil && addr.Zone() == "" && s.Network.Contains(addr.Unmap())
}

// AllowsObservation checks the address an experiment measures. A measurement is
// taken of one address, so a port or a prefix is refused here: allowing a port
// would let a plan claim to observe one address while the probe was pointed at
// another service on the same host, which is a different experiment.
func (s Scope) AllowsObservation(target string) error {
	const op = "recovery.Scope"
	if target == "" {
		return apperr.Validation(op, "an observation needs a target address", nil)
	}
	addr, err := netip.ParseAddr(target)
	switch {
	case err != nil:
		return apperr.Validation(op, "observation target "+strconv.Quote(target)+
			" is not a single address, and a measurement is taken of one address", err)
	case addr.Zone() != "":
		return apperr.Unsupported(op, "observation target "+strconv.Quote(target)+
			" carries a zone, which a laboratory does not route", nil)
	case !s.Network.Contains(addr.Unmap()):
		return apperr.Permission(op, "observation target "+addr.String()+" is outside scope.network "+s.Network.String(), nil)
	}
	return nil
}

// AllowsTarget checks a target against the scope, using the form the action acts
// on: a namespace for a namespace action, a destination for a route, and an
// endpoint for a backend. A namespace action is checked against the namespace
// list and an address action against the network, so neither can borrow the
// other's grant.
func (s Scope) AllowsTarget(action Action, target string) error {
	const op = "recovery.Scope"
	if target == "" {
		return apperr.Validation(op, "a target is required for "+string(action), nil)
	}
	switch {
	case !action.NeedsAddress():
		if _, granted := s.lookup(target); !granted {
			return apperr.Permission(op, "target namespace "+strconv.Quote(target)+" is not in the scope of experiment "+s.Experiment+
				", which grants "+strings.Join(s.Namespaces, ", "), nil)
		}
		return nil
	case action == ActionRestoreRoute:
		return s.allowsDestination(op, target)
	default:
		return s.allowsEndpoint(op, action, target)
	}
}

// allowsDestination accepts a route destination: a prefix, or a bare address
// which is read as the host route to that one address.
//
// The whole destination has to sit inside the granted network. A prefix that
// shares a few leading bits with the scope but reaches past it is refused,
// because restoring a route to somewhere outside the scope is precisely the
// mistake a scope exists to prevent, and a prefix that contains the scope is
// the clearest case of it.
func (s Scope) allowsDestination(op, target string) error {
	prefix, err := netip.ParsePrefix(target)
	if err != nil {
		addr, addrErr := netip.ParseAddr(target)
		if addrErr != nil || addr.Zone() != "" {
			return apperr.Validation(op, "target "+strconv.Quote(target)+
				" is not a destination prefix or address "+string(ActionRestoreRoute)+" acts on", err)
		}
		prefix = netip.PrefixFrom(addr.Unmap(), addr.BitLen())
	}
	if prefix.Addr().Is4In6() {
		prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits())
	}
	granted := s.Network
	switch {
	case prefix.Addr().BitLen() != granted.Addr().BitLen():
		return apperr.Permission(op, "target "+prefix.String()+" is not an address of the family scope.network "+
			granted.String()+" belongs to", nil)
	case prefix.Bits() < granted.Bits():
		return apperr.Permission(op, "target "+prefix.String()+" is wider than scope.network "+granted.String()+
			", so it would route past the scope", nil)
	case !granted.Contains(prefix.Addr()):
		return apperr.Permission(op, "target "+prefix.String()+" is outside scope.network "+granted.String(), nil)
	}
	return nil
}

// allowsEndpoint accepts the form a balancer knows a backend by: an address and
// a port, or a bare address for a backend that is a whole device.
//
// The port is not part of the scope question, but the address it carries is: a
// backend outside scope.network is refused however it is spelled, and a zone is
// refused because a laboratory has no interfaces to route a zone to.
func (s Scope) allowsEndpoint(op string, action Action, target string) error {
	endpoint, err := netip.ParseAddrPort(target)
	if err != nil {
		addr, addrErr := netip.ParseAddr(target)
		if addrErr != nil {
			return apperr.Validation(op, "target "+strconv.Quote(target)+" is not an endpoint "+string(action)+" acts on", err)
		}
		endpoint = netip.AddrPortFrom(addr, 0)
	}
	if endpoint.Addr().Zone() != "" {
		return apperr.Unsupported(op, "target "+strconv.Quote(target)+" carries a zone, which a laboratory does not route", nil)
	}
	if !s.Network.Contains(endpoint.Addr().Unmap()) {
		return apperr.Permission(op, "target "+endpoint.Addr().String()+" is outside scope.network "+s.Network.String(), nil)
	}
	return nil
}

// String renders the scope for a log line or a report.
func (s Scope) String() string {
	return s.Experiment + " network=" + s.Network.String() + " namespaces=[" + strings.Join(s.Namespaces, ",") + "]"
}

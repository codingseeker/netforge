package recovery

import (
	"net/netip"
	"strconv"
	"strings"

	"github.com/codingseeker/netforge/internal/apperr"
	"github.com/codingseeker/netforge/internal/config"
	"github.com/codingseeker/netforge/internal/namespace"
)

type Scope struct {
	Network netip.Prefix

	Namespaces []string

	Experiment string
}

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

func ScopeOf(document config.Config) (Scope, error) {
	return NewScope(document.ExperimentID(), document.Scope.Network, document.Scope.Namespaces)
}

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

func (s Scope) AllowsAddress(address string) bool {
	addr, err := netip.ParseAddr(address)
	return err == nil && addr.Zone() == "" && s.Network.Contains(addr.Unmap())
}

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

func (s Scope) String() string {
	return s.Experiment + " network=" + s.Network.String() + " namespaces=[" + strings.Join(s.Namespaces, ",") + "]"
}

package probe

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/netforge/netforge/internal/apperr"
	"github.com/netforge/netforge/internal/namespace"
)

// Scope is the part of the world a laboratory is allowed to look at. A probe
// is refused unless both the namespace it runs in and the address it aims at
// are inside the scope, so a mistake in a request cannot reach a host
// interface, a host address or a namespace the laboratory does not own.
//
// A scope that names nothing grants nothing.
type Scope struct {
	// Network is the network the laboratory may address.
	Network netip.Prefix `json:"network"`
	// Namespaces are the namespaces the laboratory may run in.
	Namespaces []string `json:"namespaces"`
}

// NewScope builds a scope. The network and every name are validated, so a
// scope that exists is a scope that can be enforced.
func NewScope(network netip.Prefix, namespaces []string) (Scope, error) {
	const op = "probe.NewScope"
	if !network.IsValid() {
		return Scope{}, apperr.InvalidArgument(op, "a scope needs a network", nil)
	}
	if network.Bits() == 0 {
		return Scope{}, apperr.InvalidArgument(op, fmt.Sprintf(
			"%s is not an experiment scope, a default route is not one", network), nil)
	}
	if len(namespaces) == 0 {
		return Scope{}, apperr.InvalidArgument(op, "a scope needs at least one namespace", nil)
	}
	seen := make([]string, 0, len(namespaces))
	for _, name := range namespaces {
		name = strings.TrimSpace(name)
		if name == "" {
			return Scope{}, apperr.InvalidArgument(op, "a scope cannot name an empty namespace", nil)
		}
		if err := checkName(op, name); err != nil {
			return Scope{}, err
		}
		if slices.Contains(seen, name) {
			return Scope{}, apperr.Validation(op, fmt.Sprintf(
				"the scope names %s more than once", name), nil)
		}
		seen = append(seen, name)
	}
	slices.Sort(seen)
	return Scope{Network: network, Namespaces: seen}, nil
}

// Check refuses an operation whose namespace or target is outside the scope.
// It is exported so the laboratory can apply the same rule to the services it
// starts, which is what stops a service being started in a namespace the
// laboratory does not own.
func (s Scope) Check(op, namespace, target string) error { return s.check(op, namespace, target) }

// check refuses a probe whose namespace or target is outside the scope.
func (s Scope) check(op, namespace, target string) error {
	if !s.Network.IsValid() || len(s.Namespaces) == 0 {
		return apperr.Permission(op, "the probe has no scope to be checked against", nil)
	}
	if !slices.Contains(s.Namespaces, namespace) {
		return apperr.Permission(op, fmt.Sprintf(
			"the namespace %s is not in the scope of this laboratory", namespace), nil)
	}
	address, err := netip.ParseAddr(target)
	if err != nil || !address.Is4() {
		return apperr.InvalidArgument(op, fmt.Sprintf(
			"%q is not an IPv4 address inside the laboratory", target), nil)
	}
	if !s.Network.Contains(address) {
		return apperr.Permission(op, fmt.Sprintf(
			"the address %s is outside the scope network %s", address, s.Network), nil)
	}
	return nil
}

// Contains reports whether an address is inside the scope network. It is used
// by callers that want to check an address without running a probe.
func (s Scope) Contains(address netip.Addr) bool {
	return s.Network.IsValid() && s.Network.Contains(address)
}

// checkName validates a namespace name, so a scope cannot name something the
// laboratory could never create. The length bound is the namespace layer's,
// which is the authority on what a namespace may be called.
func checkName(op, name string) error {
	if name == "" {
		return apperr.InvalidArgument(op, "a namespace name cannot be empty", nil)
	}
	if err := namespace.ValidateName(op, "namespace", name); err != nil {
		return apperr.Validation(op, err.Error(), err)
	}
	return nil
}

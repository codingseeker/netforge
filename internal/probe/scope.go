package probe

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/netforge/netforge/internal/apperr"
	"github.com/netforge/netforge/internal/namespace"
)

type Scope struct {
	Network netip.Prefix `json:"network"`

	Namespaces []string `json:"namespaces"`
}

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

func (s Scope) Check(op, namespace, target string) error { return s.check(op, namespace, target) }

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

func (s Scope) Contains(address netip.Addr) bool {
	return s.Network.IsValid() && s.Network.Contains(address)
}

func checkName(op, name string) error {
	if name == "" {
		return apperr.InvalidArgument(op, "a namespace name cannot be empty", nil)
	}
	if err := namespace.ValidateName(op, "namespace", name); err != nil {
		return apperr.Validation(op, err.Error(), err)
	}
	return nil
}

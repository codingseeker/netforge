package router

import (
	"net/netip"
	"strconv"
	"sync"

	"github.com/netforge/netforge/internal/apperr"
)

const (
	defaultMaxRoutes = 1024
	maxDeviceName    = 15
)

type Route struct {
	Prefix  netip.Prefix `json:"prefix"`
	NextHop netip.Addr   `json:"nextHop,omitempty"`
	Device  string       `json:"device"`
}

func (r Route) String() string {
	text := r.Prefix.String() + " dev " + r.Device
	if r.NextHop.IsValid() {
		text += " via " + r.NextHop.String()
	}
	return text
}

type Table struct {
	mu     sync.RWMutex
	scope  netip.Prefix
	limit  int
	routes []Route
}

func NewTable(scope netip.Prefix, limit int) (*Table, error) {
	const op = "router.NewTable"
	if !scope.IsValid() || !scope.Addr().Is4() {
		return nil, apperr.InvalidArgument(op, "scope "+scope.String()+" is not a valid IPv4 prefix", nil)
	}
	if limit <= 0 {
		limit = defaultMaxRoutes
	}
	if limit > defaultMaxRoutes {
		return nil, apperr.InvalidArgument(op,
			"route limit "+strconv.Itoa(limit)+" exceeds the maximum of "+strconv.Itoa(defaultMaxRoutes), nil)
	}
	return &Table{scope: scope.Masked(), limit: limit}, nil
}

func (t *Table) Scope() netip.Prefix { return t.scope }

func (t *Table) inside(prefix netip.Prefix) bool {
	return prefix.Bits() >= t.scope.Bits() && t.scope.Contains(prefix.Addr())
}

func (t *Table) Limit() int { return t.limit }

func (t *Table) Len() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.routes)
}

func (t *Table) List() []Route {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]Route, len(t.routes))
	copy(out, t.routes)
	return out
}

func (t *Table) Add(route Route) error {
	const op = "router.Table.Add"
	if err := t.check(route); err != nil {
		return err
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	for i, existing := range t.routes {
		if existing.Prefix != route.Prefix {
			continue
		}
		if existing.NextHop == route.NextHop && existing.Device == route.Device {
			t.routes[i] = route
			return nil
		}
		return apperr.Conflict(op,
			route.Prefix.String()+" already routes to "+existing.String(), nil)
	}
	if len(t.routes) >= t.limit {
		return apperr.LimitExceeded(op,
			"the table already holds the maximum of "+strconv.Itoa(t.limit)+" routes", nil)
	}
	t.routes = append(t.routes, route)
	return nil
}

func (t *Table) Remove(prefix netip.Prefix) error {
	const op = "router.Table.Remove"
	if !prefix.IsValid() || !prefix.Addr().Is4() {
		return apperr.InvalidArgument(op, "prefix "+prefix.String()+" is not a valid IPv4 prefix", nil)
	}
	prefix = prefix.Masked()

	t.mu.Lock()
	defer t.mu.Unlock()
	for i, existing := range t.routes {
		if existing.Prefix == prefix {
			t.routes = append(t.routes[:i], t.routes[i+1:]...)
			return nil
		}
	}
	return apperr.NotFound(op, "no route for "+prefix.String(), nil)
}

func (t *Table) Lookup(destination netip.Addr) (Route, bool) {
	if !destination.IsValid() || !destination.Is4() {
		return Route{}, false
	}

	t.mu.RLock()
	defer t.mu.RUnlock()
	var best Route
	found := false
	for _, route := range t.routes {
		if !route.Prefix.Contains(destination) {
			continue
		}
		if found && route.Prefix.Bits() <= best.Prefix.Bits() {
			continue
		}
		best = route
		found = true
	}
	return best, found
}

func (t *Table) check(route Route) error {
	const op = "router.Table.Add"
	if !route.Prefix.IsValid() || !route.Prefix.Addr().Is4() {
		return apperr.InvalidArgument(op, "prefix "+route.Prefix.String()+" is not a valid IPv4 prefix", nil)
	}
	if route.Prefix.Masked() != route.Prefix {
		return apperr.InvalidArgument(op,
			"prefix "+route.Prefix.String()+" has host bits set", nil)
	}
	if !t.inside(route.Prefix) {
		return apperr.Permission(op,
			"prefix "+route.Prefix.String()+" is outside the experiment scope "+t.scope.String(), nil)
	}
	if route.Device == "" || len(route.Device) > maxDeviceName {
		return apperr.InvalidArgument(op,
			"device "+strconv.Quote(route.Device)+" must be 1 to "+
				strconv.Itoa(maxDeviceName)+" characters", nil)
	}
	for i := 0; i < len(route.Device); i++ {
		c := route.Device[i]
		if c <= ' ' || c == '/' || c == ':' {
			return apperr.InvalidArgument(op,
				"device "+strconv.Quote(route.Device)+" contains an invalid character", nil)
		}
	}
	if route.NextHop.IsValid() {
		if !route.NextHop.Is4() {
			return apperr.InvalidArgument(op, "next hop "+route.NextHop.String()+" is not IPv4", nil)
		}
		if !t.scope.Contains(route.NextHop) {
			return apperr.Permission(op,
				"next hop "+route.NextHop.String()+" is outside the experiment scope "+
					t.scope.String(), nil)
		}
	}
	return nil
}

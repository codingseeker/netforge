package iface

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"github.com/codingseeker/netforge/internal/namespace"
)

const (
	MaxNameLength   = 15
	MinMTU          = 68
	MaxMTU          = 65535
	DefaultMTU      = 1500
	forwardingPath  = "/proc/sys/net/ipv4/ip_forward"
	ipPath          = "ip"
	prefixClient    = "cl"
	prefixServer    = "sv"
	prefixBalancer  = "lb"
	prefixRouterEnd = "rt"
)

func ValidateName(op, field, name string) error {
	if name == "" {
		return fmt.Errorf("%s: %s must not be empty", op, field)
	}
	if len(name) > MaxNameLength {
		return fmt.Errorf("%s: %s is %d characters, the kernel allows %d",
			op, field, len(name), MaxNameLength)
	}
	if name[0] == '-' {

		return fmt.Errorf("%s: %s %q starts with a dash, which the kernel's argument parser reads as an option",
			op, field, name)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '-':
		case c == '_':
		case c == '.':
		default:
			return fmt.Errorf("%s: %s contains %q, which the kernel does not allow in an interface name",
				op, field, string(c))
		}
	}
	if name == "." || name == ".." {
		return fmt.Errorf("%s: %s must not be %q", op, field, name)
	}
	return nil
}

func ValidateMTU(op string, mtu int) error {
	if mtu < MinMTU || mtu > MaxMTU {
		return fmt.Errorf("%s: mtu %d is outside %d to %d", op, mtu, MinMTU, MaxMTU)
	}
	return nil
}

func ValidateAddress(op, field string, address netip.Addr) error {
	if !address.IsValid() {
		return fmt.Errorf("%s: %s is not a valid address", op, field)
	}
	if !address.Is4() {
		return fmt.Errorf("%s: %s is %s, only IPv4 is implemented", op, field, address)
	}
	return nil
}

func ValidatePrefix(op, field string, prefix netip.Prefix) error {
	if !prefix.IsValid() {
		return fmt.Errorf("%s: %s is not a valid prefix", op, field)
	}
	if !prefix.Addr().Is4() {
		return fmt.Errorf("%s: %s is %s, only IPv4 is implemented", op, field, prefix)
	}
	if prefix.Masked() != prefix {
		return fmt.Errorf("%s: %s is %s, which has host bits set", op, field, prefix)
	}
	if prefix.Bits() < 8 || prefix.Bits() > 30 {
		return fmt.Errorf("%s: %s is %s, a laboratory needs between a /8 and a /30",
			op, field, prefix)
	}
	return nil
}

func SideName(kind string, index int) string {
	switch kind {
	case prefixClient:
		return fmt.Sprintf("nfc%d", index)
	case prefixServer:
		return fmt.Sprintf("nfs%d", index)
	case prefixBalancer:
		return fmt.Sprintf("nflb%d", index)
	default:
		return fmt.Sprintf("nfx%d", index)
	}
}

func RouterName(kind string, index int) string {
	switch kind {
	case prefixServer:
		return fmt.Sprintf("nfr-s%d", index)
	case prefixBalancer:
		return fmt.Sprintf("nfr-lb%d", index)
	default:
		return fmt.Sprintf("nfr-c%d", index)
	}
}

type Address struct {
	Interface string
	Value     netip.Prefix
	Scope     string
}

type Link struct {
	Name      string
	Index     int
	Up        bool
	MTU       int
	Addresses []netip.Prefix
}

type State struct {
	Links     []Link
	Addresses []Address
	Routes    []string
}

type Controller struct {
	lab  *namespace.Lab
	Root *namespace.Space
}

func New(lab *namespace.Lab) *Controller {
	return &Controller{lab: lab, Root: lab.Root()}
}

func run(op string, space *namespace.Space, argv ...string) (string, error) {
	stdout, stderr, err := space.Exec(argv...)
	if err != nil {
		return "", fmt.Errorf("%s: %w", op, err)
	}
	if trimmed := strings.TrimSpace(stderr); trimmed != "" {
		return "", fmt.Errorf("%s: %s", op, trimmed)
	}
	return stdout, nil
}

func (c *Controller) CreatePair(op, left, right string) error {
	if err := ValidateName(op, "left", left); err != nil {
		return err
	}
	if err := ValidateName(op, "right", right); err != nil {
		return err
	}
	_, err := run(op, c.Root, ipPath, "link", "add", left, "type", "veth", "peer", "name", right)
	return err
}

func (c *Controller) Move(op, name string, target *namespace.Space) error {
	if err := ValidateName(op, "name", name); err != nil {
		return err
	}
	_, err := run(op, c.Root, ipPath, "link", "set", name, "netns", strconv.Itoa(target.PID()))
	return err
}

func (c *Controller) AddAddress(op string, space *namespace.Space, name string, prefix netip.Prefix) error {
	if err := ValidateName(op, "interface", name); err != nil {
		return err
	}
	if err := ValidateAddress(op, "address", prefix.Addr()); err != nil {
		return err
	}
	if prefix.Bits() < 0 || prefix.Bits() > 30 {
		return fmt.Errorf("%s: %s has an unusable prefix length", op, prefix)
	}
	_, err := run(op, space, ipPath, "addr", "add", prefix.String(), "dev", name)
	return err
}

func (c *Controller) SetUp(op string, space *namespace.Space, name string) error {
	if err := ValidateName(op, "interface", name); err != nil {
		return err
	}
	_, err := run(op, space, ipPath, "link", "set", name, "up")
	return err
}

func (c *Controller) SetMTU(op string, space *namespace.Space, name string, mtu int) error {
	if err := ValidateName(op, "interface", name); err != nil {
		return err
	}
	if err := ValidateMTU(op, mtu); err != nil {
		return err
	}
	_, err := run(op, space, ipPath, "link", "set", name, "mtu", strconv.Itoa(mtu))
	return err
}

func (c *Controller) AddRoute(op string, space *namespace.Space, target netip.Prefix, via netip.Addr) error {
	if err := ValidatePrefix(op, "route", target); err != nil {
		return err
	}
	if err := ValidateAddress(op, "gateway", via); err != nil {
		return err
	}
	_, err := run(op, space, ipPath, "route", "replace", target.String(), "via", via.String())
	return err
}

func (c *Controller) AddDefaultRoute(op string, space *namespace.Space, via netip.Addr, device string) error {
	if err := ValidateAddress(op, "gateway", via); err != nil {
		return err
	}
	if err := ValidateName(op, "device", device); err != nil {
		return err
	}
	_, err := run(op, space, ipPath, "route", "replace", "default", "via", via.String(), "dev", device)
	return err
}

func (c *Controller) RemoveRoute(op string, space *namespace.Space, target netip.Prefix) error {
	if err := ValidatePrefix(op, "route", target); err != nil {
		return err
	}
	_, err := run(op, space, ipPath, "route", "del", target.String())
	return err
}

func (c *Controller) SetLoopback(op string, space *namespace.Space) error {
	_, err := run(op, space, ipPath, "link", "set", "lo", "up")
	return err
}

func (c *Controller) EnableForwarding(op string, space *namespace.Space) error {
	reply, err := space.Write(forwardingPath, "1")
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if reply != "" {
		return fmt.Errorf("%s: enabling forwarding returned %q", op, reply)
	}
	return nil
}

func (c *Controller) DisableForwarding(op string, space *namespace.Space) error {
	reply, err := space.Write(forwardingPath, "0")
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if reply != "" {
		return fmt.Errorf("%s: disabling forwarding returned %q", op, reply)
	}
	return nil
}

func (c *Controller) Forwarding(op string, space *namespace.Space) (bool, error) {
	value, err := space.Read(forwardingPath)
	if err != nil {
		return false, fmt.Errorf("%s: %w", op, err)
	}
	return strings.TrimSpace(value) == "1", nil
}

func (c *Controller) Inspect(op string, space *namespace.Space) (State, error) {
	var state State
	stdout, err := run(op, space, ipPath, "-j", "addr", "show")
	if err != nil {
		return state, err
	}
	var links []ipLink
	if err := json.Unmarshal([]byte(stdout), &links); err != nil {
		return state, fmt.Errorf("%s: decode the interfaces of %s: %w", op, space.Name, err)
	}
	for _, link := range links {
		entry := Link{Name: link.IfName, Index: link.IfIndex, Up: link.OperState == "UP" || link.flagSet().contains("UP"), MTU: link.MTU}
		for _, info := range link.AddrInfo {
			address, parseErr := netip.ParsePrefix(info.Local + "/" + strconv.Itoa(bitsOf(info.Prefixlen)))
			if parseErr != nil {
				continue
			}
			if !address.Addr().Is4() {
				continue
			}
			entry.Addresses = append(entry.Addresses, address)
			state.Addresses = append(state.Addresses, Address{Interface: link.IfName, Value: address, Scope: info.Scope})
		}
		state.Links = append(state.Links, entry)
	}
	stdout, err = run(op, space, ipPath, "-j", "route", "show")
	if err != nil {
		return state, err
	}
	var routes []ipRoute
	if err := json.Unmarshal([]byte(stdout), &routes); err != nil {
		return state, fmt.Errorf("%s: decode the routes of %s: %w", op, space.Name, err)
	}
	for _, route := range routes {
		state.Routes = append(state.Routes, route.Dst+" via "+route.Gateway+" dev "+route.Dev)
	}
	return state, nil
}

func bitsOf(length int) int {
	if length < 0 || length > 32 {
		return 32
	}
	return length
}

type ipLink struct {
	IfName    string   `json:"ifname"`
	IfIndex   int      `json:"ifindex"`
	MTU       int      `json:"mtu"`
	OperState string   `json:"operstate"`
	Flags     []string `json:"flags"`
	AddrInfo  []struct {
		Family    string `json:"family"`
		Local     string `json:"local"`
		Prefixlen int    `json:"prefixlen"`
		Scope     string `json:"scope"`
	} `json:"addr_info"`
}

func (l ipLink) flagSet() linkFlags { return linkFlags(l.Flags) }

type linkFlags []string

func (f linkFlags) contains(flag string) bool {
	for _, candidate := range f {
		if candidate == flag {
			return true
		}
	}
	return false
}

type ipRoute struct {
	Dst     string `json:"dst"`
	Gateway string `json:"gateway"`
	Dev     string `json:"dev"`
}

var ErrNoInterface = errors.New("interface not found")

func (s State) Link(name string) (Link, error) {
	for _, link := range s.Links {
		if link.Name == name {
			return link, nil
		}
	}
	return Link{}, fmt.Errorf("%w: %s", ErrNoInterface, name)
}

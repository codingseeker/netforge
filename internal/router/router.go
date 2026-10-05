package router

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/netforge/netforge/internal/apperr"
	"github.com/netforge/netforge/internal/packet"
)

const (
	mode         = "netforge-router"
	statsEvery   = 500 * time.Millisecond
	readTimeout  = 200 * time.Millisecond
	socketBuffer = 1 << 20
	defaultMTU   = 1500
)

type Device struct {
	Name      string         `json:"name"`
	Addresses []netip.Prefix `json:"addresses"`
	// MTU is the largest packet the link will carry. The router uses it to
	// decide whether a packet can leave by this device at all, because a
	// packet that cannot is a decision to make rather than a send to attempt.
	MTU int `json:"mtu,omitempty"`
}

type Config struct {
	Scope        netip.Prefix `json:"scope"`
	Devices      []Device     `json:"devices"`
	Routes       []Route      `json:"routes"`
	MaxRoutes    int          `json:"maxRoutes,omitempty"`
	MaxNeighbors int          `json:"maxNeighbors,omitempty"`
	QueueSize    int          `json:"queueSize,omitempty"`
	StatsPath    string       `json:"statsPath,omitempty"`
}

type device struct {
	name      string
	index     int
	mac       net.HardwareAddr
	addresses []netip.Prefix
	mtu       int
	fd        int
}

// frameLimit is the largest frame the link will accept: the payload MTU plus
// the Ethernet header the MTU does not count.
func (d *device) frameLimit() int {
	if d == nil || d.mtu <= 0 {
		return 0
	}
	return d.mtu + ethernetMinimum
}

type Router struct {
	cfg       Config
	state     string
	failure   string
	revision  atomic.Uint64
	last      atomic.Value
	table     *Table
	neighbors *Neighbors
	counters  *Counters
	queue     *queue
	limits    packet.Limits
	devices   []*device
	byName    map[string]*device
	pool      sync.Pool
	stop      chan struct{}
	stopOnce  sync.Once
	stopped   chan struct{}
}

func New(cfg Config) (*Router, error) {
	const op = "router.New"
	table, err := NewTable(cfg.Scope, cfg.MaxRoutes)
	if err != nil {
		return nil, err
	}
	neighbors, err := NewNeighbors(cfg.MaxNeighbors)
	if err != nil {
		return nil, err
	}
	queue, err := newQueue(cfg.QueueSize)
	if err != nil {
		return nil, err
	}
	if err := checkConfig(op, cfg, table); err != nil {
		return nil, err
	}
	for _, entry := range cfg.Routes {
		if err := table.Add(entry); err != nil {
			return nil, err
		}
	}

	r := &Router{
		cfg:       cfg,
		state:     stateStarting,
		table:     table,
		neighbors: neighbors,
		counters:  &Counters{},
		queue:     queue,
		limits:    packet.DefaultLimits(),
		byName:    map[string]*device{},
		stop:      make(chan struct{}),
		stopped:   make(chan struct{}),
	}
	r.pool.New = func() any {
		buffer := make([]byte, r.limits.MaxPacketSize)
		return &buffer
	}
	return r, nil
}

func checkConfig(op string, cfg Config, table *Table) error {
	if !cfg.Scope.IsValid() || !cfg.Scope.Addr().Is4() {
		return apperr.InvalidArgument(op, "scope is not a valid IPv4 prefix", nil)
	}
	if len(cfg.Devices) == 0 {
		return apperr.InvalidArgument(op, "the router needs at least one device", nil)
	}
	seen := map[string]bool{}
	for _, entry := range cfg.Devices {
		if entry.Name == "" || len(entry.Name) > maxDeviceName {
			return apperr.InvalidArgument(op,
				"device "+strconv.Quote(entry.Name)+" has an invalid name", nil)
		}
		if seen[entry.Name] {
			return apperr.Conflict(op, "device "+strconv.Quote(entry.Name)+" appears twice", nil)
		}
		seen[entry.Name] = true
		if len(entry.Addresses) == 0 {
			return apperr.InvalidArgument(op,
				"device "+strconv.Quote(entry.Name)+" has no addresses", nil)
		}
		for _, address := range entry.Addresses {
			if !address.IsValid() || !address.Addr().Is4() {
				return apperr.InvalidArgument(op,
					"address "+address.String()+" on "+entry.Name+" is not IPv4", nil)
			}
			if !table.Scope().Contains(address.Addr()) {
				return apperr.Permission(op,
					"address "+address.String()+" on "+entry.Name+
						" is outside the experiment scope "+table.Scope().String(), nil)
			}
		}
	}
	for _, route := range cfg.Routes {
		if !seen[route.Device] {
			return apperr.InvalidArgument(op,
				"route "+route.Prefix.String()+" uses unknown device "+
					strconv.Quote(route.Device), nil)
		}
	}
	if cfg.StatsPath != "" && !filepath.IsAbs(cfg.StatsPath) {
		return apperr.InvalidArgument(op, "stats path "+strconv.Quote(cfg.StatsPath)+" is not absolute", nil)
	}
	return nil
}

func (r *Router) Stats() Stats {
	stats := r.counters.snapshot()
	stats.State = r.state
	stats.Error = r.failure
	if last, ok := r.last.Load().(string); ok {
		stats.LastError = last
	}
	stats.QueueDepth = r.queue.depth()
	stats.QueueSize = r.queue.size()
	stats.Routes = r.table.Len()
	stats.Neighbors = r.neighbors.Len()
	return stats
}

func (r *Router) Serve(ctx context.Context) error {
	defer close(r.stopped)
	if err := r.open(); err != nil {
		r.fail(err)
		return err
	}
	r.state = stateRunning
	if err := r.writeStats(); err != nil {
		r.close()
		r.fail(err)
		return err
	}
	defer r.close()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	updates := make(chan os.Signal, 1)
	signal.Notify(updates, syscall.SIGUSR1)
	defer signal.Stop(updates)

	var group sync.WaitGroup
	for _, dev := range r.devices {
		group.Add(1)
		go func(d *device) {
			defer group.Done()
			r.read(d, cancel)
		}(dev)
	}
	for i := 0; i < workers; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			r.work(cancel)
		}()
	}
	group.Add(1)
	go func() {
		defer group.Done()
		r.ticker(cancel, updates)
	}()
	group.Add(1)
	go func() {
		defer group.Done()
		r.watch(cancel)
	}()

	<-ctx.Done()
	r.halt()
	group.Wait()
	r.state = stateStopped
	_ = r.writeStats()
	return nil
}

func (r *Router) fail(err error) {
	r.state = stateFailed
	r.failure = err.Error()
	_ = r.writeStats()
}

func (r *Router) open() error {
	for _, entry := range r.cfg.Devices {
		iface, err := net.InterfaceByName(entry.Name)
		if err != nil {
			return fmt.Errorf("router.Serve: device %s: %w", entry.Name, err)
		}
		if len(iface.HardwareAddr) != arpMaxMAC {
			return fmt.Errorf("router.Serve: device %s has no Ethernet address", entry.Name)
		}
		fd, err := openSocket(iface.Index)
		if err != nil {
			return fmt.Errorf("router.Serve: device %s: %w", entry.Name, err)
		}
		r.attach(entry, iface.Index, cloneMAC(iface.HardwareAddr), fd)
	}
	return r.writeStats()
}

func (r *Router) attach(entry Device, index int, mac net.HardwareAddr, fd int) *device {
	dev := &device{
		name:      entry.Name,
		index:     index,
		mac:       mac,
		addresses: append([]netip.Prefix(nil), entry.Addresses...),
		mtu:       entry.MTU,
		fd:        fd,
	}
	if dev.mtu <= 0 {
		// A configuration that says nothing about the size is read as the
		// laboratory's own default rather than as unlimited, so an absent MTU
		// cannot quietly remove the check.
		dev.mtu = defaultMTU
	}
	r.devices = append(r.devices, dev)
	r.byName[dev.name] = dev
	return dev
}

func (r *Router) close() {
	for _, dev := range r.devices {
		if dev.fd > 0 {
			_ = syscall.Close(dev.fd)
			dev.fd = 0
		}
	}
}

func (r *Router) halt() {
	r.stopOnce.Do(func() { close(r.stop) })
	r.queue.close()
}

func (r *Router) watch(cancel context.CancelFunc) {
	defer cancel()
	reader := os.NewFile(0, "agent")
	if reader == nil {
		return
	}
	buffer := make([]byte, 64)
	for {
		if _, err := reader.Read(buffer); err != nil {
			return
		}
	}
}

func (r *Router) ticker(cancel context.CancelFunc, updates <-chan os.Signal) {
	defer cancel()
	ticker := time.NewTicker(statsEvery)
	defer ticker.Stop()
	last := r.Stats()
	for {
		select {
		case <-r.stop:
			_ = r.writeStats()
			return
		case <-ticker.C:
		case <-updates:
		}
		current := r.Stats()
		if current != last {
			if err := r.writeStats(); err != nil {
				return
			}
			last = current
		}
	}
}

func (r *Router) writeStats() error {
	if r.cfg.StatsPath == "" {
		return nil
	}
	stats := r.Stats()
	stats.Revision = r.revision.Add(1)
	data, err := json.Marshal(stats)
	if err != nil {
		return err
	}
	temporary := r.cfg.StatsPath + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, r.cfg.StatsPath)
}

func ReadStats(path string) (Stats, error) {
	const op = "router.ReadStats"
	data, err := os.ReadFile(path)
	if err != nil {
		return Stats{}, apperr.IO(op, "read "+path, err)
	}
	var stats Stats
	if err := json.Unmarshal(data, &stats); err != nil {
		return Stats{}, apperr.IO(op, "decode "+path, err)
	}
	return stats, nil
}

func Dispatch(args []string) (bool, int) {
	if len(args) == 0 || args[0] != mode {
		return false, 0
	}
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, mode+": expected one configuration argument")
		return true, 2
	}
	var cfg Config
	if err := json.Unmarshal([]byte(args[1]), &cfg); err != nil {
		fmt.Fprintln(os.Stderr, mode+": "+err.Error())
		return true, 2
	}
	instance, err := New(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, mode+": "+err.Error())
		reportFailure(cfg, err)
		return true, 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := instance.Serve(ctx); err != nil {
		fmt.Fprintln(os.Stderr, mode+": "+err.Error())
		return true, 1
	}
	return true, 0
}

func reportFailure(cfg Config, err error) {
	if cfg.StatsPath == "" {
		return
	}
	stats := Stats{State: stateFailed, Error: err.Error()}
	data, marshalErr := json.Marshal(stats)
	if marshalErr != nil {
		return
	}
	_ = os.WriteFile(cfg.StatsPath+".tmp", data, 0o600)
	_ = os.Rename(cfg.StatsPath+".tmp", cfg.StatsPath)
}

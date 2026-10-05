package router

import "sync/atomic"

const (
	stateStarting = "starting"
	stateRunning  = "running"
	stateStopped  = "stopped"
	stateFailed   = "failed"
)

type Counters struct {
	received      atomic.Uint64
	forwarded     atomic.Uint64
	bytes         atomic.Uint64
	ignored       atomic.Uint64
	local         atomic.Uint64
	malformed     atomic.Uint64
	noRoute       atomic.Uint64
	ttlExceeded   atomic.Uint64
	oversize      atomic.Uint64
	unreachable   atomic.Uint64
	arpRequests   atomic.Uint64
	arpReplies    atomic.Uint64
	arpRequestsIn atomic.Uint64
	unresolved    atomic.Uint64
	queueFull     atomic.Uint64
	sendErrors    atomic.Uint64
}

type Stats struct {
	Revision    uint64 `json:"revision"`
	State       string `json:"state"`
	Error       string `json:"error,omitempty"`
	LastError   string `json:"lastError,omitempty"`
	Received    uint64 `json:"received"`
	Forwarded   uint64 `json:"forwarded"`
	Bytes       uint64 `json:"bytes"`
	Ignored     uint64 `json:"ignored"`
	Local       uint64 `json:"local"`
	Malformed   uint64 `json:"malformed"`
	NoRoute     uint64 `json:"noRoute"`
	TTLExceeded uint64 `json:"ttlExceeded"`
	Oversize    uint64 `json:"oversize"`
	Unreachable uint64 `json:"unreachable"`
	ARPRequests uint64 `json:"arpRequests"`
	ARPReplies  uint64 `json:"arpReplies"`
	ARPProbes   uint64 `json:"arpProbes"`
	Unresolved  uint64 `json:"unresolved"`
	QueueFull   uint64 `json:"queueFull"`
	SendErrors  uint64 `json:"sendErrors"`
	QueueDepth  int    `json:"queueDepth"`
	QueueSize   int    `json:"queueSize"`
	Routes      int    `json:"routes"`
	Neighbors   int    `json:"neighbors"`
}

func (c *Counters) addReceived()          { c.received.Add(1) }
func (c *Counters) addForwarded(size int) { c.forwarded.Add(1); c.bytes.Add(uint64(size)) }
func (c *Counters) addIgnored()           { c.ignored.Add(1) }
func (c *Counters) addLocal()             { c.local.Add(1) }
func (c *Counters) addMalformed()         { c.malformed.Add(1) }
func (c *Counters) addNoRoute()           { c.noRoute.Add(1) }
func (c *Counters) addTTL()               { c.ttlExceeded.Add(1) }
func (c *Counters) addOversize()          { c.oversize.Add(1) }
func (c *Counters) addUnreachable()       { c.unreachable.Add(1) }
func (c *Counters) addARPRequest()        { c.arpRequests.Add(1) }
func (c *Counters) addARPReply()          { c.arpReplies.Add(1) }
func (c *Counters) addARPProbe()          { c.arpRequestsIn.Add(1) }
func (c *Counters) addUnresolved()        { c.unresolved.Add(1) }
func (c *Counters) addQueueFull()         { c.queueFull.Add(1) }
func (c *Counters) addSendError()         { c.sendErrors.Add(1) }

func (c *Counters) snapshot() Stats {
	return Stats{
		State:       stateRunning,
		Received:    c.received.Load(),
		Forwarded:   c.forwarded.Load(),
		Bytes:       c.bytes.Load(),
		Ignored:     c.ignored.Load(),
		Local:       c.local.Load(),
		Malformed:   c.malformed.Load(),
		NoRoute:     c.noRoute.Load(),
		TTLExceeded: c.ttlExceeded.Load(),
		Oversize:    c.oversize.Load(),
		Unreachable: c.unreachable.Load(),
		ARPRequests: c.arpRequests.Load(),
		ARPReplies:  c.arpReplies.Load(),
		ARPProbes:   c.arpRequestsIn.Load(),
		Unresolved:  c.unresolved.Load(),
		QueueFull:   c.queueFull.Load(),
		SendErrors:  c.sendErrors.Load(),
	}
}

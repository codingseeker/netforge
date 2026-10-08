package router

import (
	"encoding/binary"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/codingseeker/netforge/internal/apperr"
)

const (
	etherARP     = 0x0806
	arpRequest   = 1
	arpReply     = 2
	arpHeader    = 28
	arpFrameLen  = 14 + arpHeader
	arpEtherIPv4 = 0x0800
	arpHardEther = 1
	arpMaxMAC    = 6

	defaultMaxNeighbors = 256
	neighborLifetime    = 5 * time.Minute
)

var (
	broadcastMAC = net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	zeroMAC      = net.HardwareAddr{0, 0, 0, 0, 0, 0}
)

type ARP struct {
	Operation uint16
	Sender    net.HardwareAddr
	SenderIP  netip.Addr
	Target    net.HardwareAddr
	TargetIP  netip.Addr
}

func ParseARP(data []byte) (*ARP, error) {
	const op = "router.ParseARP"
	if len(data) != arpFrameLen {
		return nil, apperr.InvalidArgument(op,
			"frame is "+strconv.Itoa(len(data))+" bytes, an ARP frame is exactly "+
				strconv.Itoa(arpFrameLen), nil)
	}
	if binary.BigEndian.Uint16(data[12:14]) != etherARP {
		return nil, apperr.InvalidArgument(op, "frame is not ARP", nil)
	}
	if binary.BigEndian.Uint16(data[14:16]) != arpHardEther {
		return nil, apperr.InvalidArgument(op, "ARP hardware type is not Ethernet", nil)
	}
	if binary.BigEndian.Uint16(data[16:18]) != arpEtherIPv4 {
		return nil, apperr.InvalidArgument(op, "ARP protocol type is not IPv4", nil)
	}
	if data[18] != arpMaxMAC || data[19] != 4 {
		return nil, apperr.InvalidArgument(op, "ARP address lengths are not 6 and 4", nil)
	}
	operation := binary.BigEndian.Uint16(data[20:22])
	if operation != arpRequest && operation != arpReply {
		return nil, apperr.InvalidArgument(op,
			"ARP operation "+strconv.Itoa(int(operation))+" is unknown", nil)
	}

	message := &ARP{
		Operation: operation,
		Sender:    cloneMAC(data[22:28]),
		SenderIP:  netip.AddrFrom4([4]byte(data[28:32])),
		Target:    cloneMAC(data[32:38]),
		TargetIP:  netip.AddrFrom4([4]byte(data[38:42])),
	}
	if !message.SenderIP.Is4() || !message.TargetIP.Is4() {
		return nil, apperr.InvalidArgument(op, "ARP addresses are not IPv4", nil)
	}
	return message, nil
}

func (a *ARP) Serialize() ([]byte, error) {
	const op = "router.ARP.Serialize"
	if a == nil {
		return nil, apperr.InvalidArgument(op, "message is nil", nil)
	}
	if a.Operation != arpRequest && a.Operation != arpReply {
		return nil, apperr.InvalidArgument(op,
			"operation "+strconv.Itoa(int(a.Operation))+" is not a request or a reply", nil)
	}
	if len(a.Sender) != arpMaxMAC || len(a.Target) != arpMaxMAC {
		return nil, apperr.InvalidArgument(op, "ARP addresses must be 6 bytes", nil)
	}
	if !a.SenderIP.Is4() || !a.TargetIP.Is4() {
		return nil, apperr.InvalidArgument(op, "ARP addresses must be IPv4", nil)
	}

	body := make([]byte, arpHeader)
	binary.BigEndian.PutUint16(body[0:2], arpHardEther)
	binary.BigEndian.PutUint16(body[2:4], arpEtherIPv4)
	body[4] = arpMaxMAC
	body[5] = 4
	binary.BigEndian.PutUint16(body[6:8], a.Operation)
	copy(body[8:14], a.Sender)
	copy(body[14:18], a.SenderIP.AsSlice())
	copy(body[18:24], a.Target)
	copy(body[24:28], a.TargetIP.AsSlice())
	return body, nil
}

func arpFrame(destination, source net.HardwareAddr, message *ARP) ([]byte, error) {
	body, err := message.Serialize()
	if err != nil {
		return nil, err
	}
	if len(destination) != arpMaxMAC || len(source) != arpMaxMAC {
		return nil, apperr.InvalidArgument("router.arpFrame", "ARP addresses must be 6 bytes", nil)
	}
	frame := make([]byte, 0, 14+len(body))
	frame = append(frame, destination...)
	frame = append(frame, source...)
	frame = binary.BigEndian.AppendUint16(frame, etherARP)
	return append(frame, body...), nil
}

func cloneMAC(data []byte) net.HardwareAddr {
	out := make(net.HardwareAddr, arpMaxMAC)
	copy(out, data)
	return out
}

func macUsable(address net.HardwareAddr) bool {
	return len(address) == arpMaxMAC && address[0]&1 == 0
}

type neighbor struct {
	mac    net.HardwareAddr
	device string
	seen   time.Time
}

type Neighbors struct {
	mu      sync.Mutex
	limit   int
	entries map[netip.Addr]neighbor
}

func NewNeighbors(limit int) (*Neighbors, error) {
	const op = "router.NewNeighbors"
	if limit <= 0 {
		limit = defaultMaxNeighbors
	}
	if limit > defaultMaxNeighbors {
		return nil, apperr.InvalidArgument(op,
			"neighbor limit "+strconv.Itoa(limit)+" exceeds the maximum of "+
				strconv.Itoa(defaultMaxNeighbors), nil)
	}
	return &Neighbors{limit: limit, entries: map[netip.Addr]neighbor{}}, nil
}

func (n *Neighbors) Learn(address netip.Addr, mac net.HardwareAddr, device string) {
	if !address.Is4() || !macUsable(mac) {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, known := n.entries[address]; !known {
		for len(n.entries) >= n.limit {
			n.evict()
		}
	}
	n.entries[address] = neighbor{mac: cloneMAC(mac), device: device, seen: time.Now()}
}

func (n *Neighbors) Lookup(address netip.Addr) (net.HardwareAddr, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	entry, ok := n.entries[address]
	if !ok {
		return nil, false
	}
	if time.Since(entry.seen) > neighborLifetime {
		delete(n.entries, address)
		return nil, false
	}
	return entry.mac, true
}

func (n *Neighbors) Forget(address netip.Addr) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.entries, address)
}

func (n *Neighbors) Len() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.entries)
}

func (n *Neighbors) evict() {
	oldest := netip.Addr{}
	var oldestAt time.Time
	for address, entry := range n.entries {
		if !oldest.IsValid() || entry.seen.Before(oldestAt) {
			oldest = address
			oldestAt = entry.seen
		}
	}
	delete(n.entries, oldest)
}

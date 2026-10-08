package router

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"

	"github.com/codingseeker/netforge/internal/packet"
)

const (
	etherAll = 0x0003

	dropIgnored = iota
	dropLocal
	dropMalformed
	dropNoRoute
	dropTTL
	dropOversize
	forwardFrame
	answerARP
	icmpError
	probeARP
)

type plan struct {
	kind      int
	reason    string
	ethertype packet.EtherType
	egress    string
	frame     []byte
	icmpType  packet.ICMPType
	icmpCode  uint8
	icmpMTU   uint16
	target    netip.Addr
	learn     netip.Addr
	learnMAC  net.HardwareAddr
}

func openSocket(index int) (int, error) {
	fd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW, 0x0300)
	if err != nil {
		return -1, err
	}
	if err := syscall.Bind(fd, &syscall.SockaddrLinklayer{Protocol: 0x0300, Ifindex: index}); err != nil {
		_ = syscall.Close(fd)
		return -1, err
	}
	timeout := syscall.NsecToTimeval(int64(readTimeout))
	if err := syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &timeout); err != nil {
		_ = syscall.Close(fd)
		return -1, err
	}
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, socketBuffer); err != nil {
		_ = syscall.Close(fd)
		return -1, err
	}
	return fd, nil
}

func send(dev *device, frame []byte, kind packet.EtherType) error {
	if dev == nil || dev.fd <= 0 {
		return errors.New("the device is not open")
	}
	if len(frame) < ethernetMinimum {
		return errors.New("the frame is shorter than an Ethernet header")
	}
	if dev.mtu > 0 && len(frame) > dev.mtu+ethernetMinimum {

		return fmt.Errorf("a frame of %d bytes does not fit a link of %d", len(frame), dev.mtu)
	}
	address := &syscall.SockaddrLinklayer{
		Protocol: htons(uint16(kind)),
		Ifindex:  dev.index,
		Halen:    arpMaxMAC,
	}
	copy(address.Addr[:arpMaxMAC], frame[0:arpMaxMAC])
	return syscall.Sendto(dev.fd, frame, 0, address)
}

func (r *Router) remember(err error) {
	r.last.Store(err.Error())
}

func htons(value uint16) uint16 {
	return value<<8 | value>>8
}

func (r *Router) read(dev *device, cancel context.CancelFunc) {
	defer cancel()
	for {
		select {
		case <-r.stop:
			return
		default:
		}
		slot := r.pool.Get().(*[]byte)
		n, _, err := syscall.Recvfrom(dev.fd, *slot, 0)
		if err != nil {
			r.pool.Put(slot)
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EINTR) {
				continue
			}
			return
		}
		if n <= 0 {
			r.pool.Put(slot)
			continue
		}
		frame := make([]byte, n)
		copy(frame, (*slot)[:n])
		r.pool.Put(slot)
		if !r.queue.offer(work{ingress: dev.name, frame: frame}) {
			r.counters.addQueueFull()
		}
	}
}

func (r *Router) work(cancel context.CancelFunc) {
	for {
		item, ok := r.queue.take()
		if !ok {
			return
		}
		dev := r.byName[item.ingress]
		if dev == nil {
			r.counters.addIgnored()
			continue
		}
		r.counters.addReceived()
		r.apply(dev, r.decide(dev, item.frame))
	}
}

func (r *Router) apply(dev *device, step plan) {
	if step.learn.IsValid() {
		r.neighbors.Learn(step.learn, step.learnMAC, dev.name)
	}
	switch step.kind {
	case dropIgnored:
		r.counters.addIgnored()
	case dropLocal:
		r.counters.addLocal()
	case dropMalformed:
		r.counters.addMalformed()
	case dropNoRoute:
		r.counters.addNoRoute()
		r.emit(dev, step)
	case dropTTL:
		r.counters.addTTL()
		r.emit(dev, step)
	case dropOversize:
		r.counters.addOversize()
		r.emit(dev, step)
	case forwardFrame:
		if out := r.byName[step.egress]; out != nil {
			if err := send(out, step.frame, step.ethertype); err == nil {
				r.counters.addForwarded(len(step.frame))
				return
			} else {
				r.remember(err)
			}
		}
		r.counters.addSendError()
	case answerARP:
		r.counters.addARPRequest()
		if err := send(dev, step.frame, packet.EtherTypeARP); err == nil {
			r.counters.addARPReply()
			return
		} else {
			r.remember(err)
		}
		r.counters.addSendError()
	case icmpError:
		r.emit(dev, step)
	case probeARP:
		r.counters.addUnresolved()
		r.probe(dev, step)
	}
}

func (r *Router) emit(dev *device, step plan) {
	if step.icmpType == packet.ICMPTypeDestinationUnreachable {
		r.counters.addUnreachable()
	}
	if step.frame == nil {
		return
	}
	if err := send(dev, step.frame, packet.EtherTypeIPv4); err != nil {
		r.counters.addSendError()
		r.remember(err)
	}
}

func (r *Router) probe(dev *device, step plan) {
	if !step.target.IsValid() {
		return
	}
	if _, known := r.neighbors.Lookup(step.target); known {
		return
	}
	out := r.byName[step.egress]
	if out == nil {
		out = dev
	}
	request := &ARP{
		Operation: arpRequest,
		Sender:    out.mac,
		SenderIP:  r.addressOn(out, step.target),
		Target:    zeroMAC,
		TargetIP:  step.target,
	}
	frame, err := arpFrame(broadcastMAC, out.mac, request)
	if err != nil {
		r.counters.addSendError()
		return
	}
	if err := send(out, frame, packet.EtherTypeARP); err != nil {
		r.counters.addSendError()
		r.remember(err)
		return
	}
	r.counters.addARPProbe()
}

func (r *Router) decide(dev *device, raw []byte) plan {
	link, err := packet.ParseEthernet(r.limits, raw)
	if err != nil {
		return plan{kind: dropMalformed, reason: "ethernet"}
	}
	switch link.EtherType {
	case packet.EtherTypeARP:
		return r.decideARP(dev, raw)
	case packet.EtherTypeIPv4:
		return r.decideIPv4(dev, raw, link)
	default:
		return plan{kind: dropIgnored, reason: "ethertype"}
	}
}

func (r *Router) decideARP(dev *device, raw []byte) plan {
	message, err := ParseARP(raw)
	if err != nil {
		return plan{kind: dropMalformed, reason: "arp"}
	}
	step := plan{learn: message.SenderIP, learnMAC: message.Sender}
	if message.Operation == arpReply {
		step.kind = dropIgnored
		step.reason = "arp reply"
		return step
	}
	if !dev.owns(message.TargetIP) {
		step.kind = dropIgnored
		step.reason = "arp for another host"
		return step
	}
	answer := &ARP{
		Operation: arpReply,
		Sender:    dev.mac,
		SenderIP:  message.TargetIP,
		Target:    message.Sender,
		TargetIP:  message.SenderIP,
	}
	frame, err := arpFrame(message.Sender, dev.mac, answer)
	if err != nil {
		step.kind = dropMalformed
		step.reason = "arp reply"
		return step
	}
	step.kind = answerARP
	step.ethertype = packet.EtherTypeARP
	step.frame = frame
	return step
}

func (r *Router) decideIPv4(dev *device, raw []byte, link *packet.Ethernet) plan {
	ip, err := packet.ParseIPv4(r.limits, link.Payload)
	if err != nil {
		return plan{kind: dropMalformed, reason: "ipv4"}
	}
	if !ip.ChecksumValid() {
		return plan{kind: dropMalformed, reason: "ipv4 checksum"}
	}
	if !ip.Destination.Is4() || ip.Destination.IsUnspecified() {
		return plan{kind: dropMalformed, reason: "ipv4 destination"}
	}
	if r.owns(ip.Destination) {
		return plan{kind: dropLocal, reason: "for the router"}
	}
	if broadcast4(ip.Destination) || multicast4(ip.Destination) {
		return plan{kind: dropIgnored, reason: "multicast"}
	}
	if ip.TimeToLive <= 1 {
		step := plan{
			kind:     dropTTL,
			reason:   "ttl",
			icmpType: packet.ICMPTypeTimeExceeded,
			icmpCode: packet.ICMPCodeTimeExceededInTransit,
		}
		step.frame = r.icmpFrame(dev, raw, ip.Source, step.icmpType, step.icmpCode, 0)
		return step
	}

	route, ok := r.table.Lookup(ip.Destination)
	if !ok {
		step := plan{
			kind:     dropNoRoute,
			reason:   "no route",
			icmpType: packet.ICMPTypeDestinationUnreachable,
			icmpCode: packet.ICMPCodeNetworkUnreachable,
		}
		step.frame = r.icmpFrame(dev, raw, ip.Source, step.icmpType, step.icmpCode, 0)
		return step
	}
	egress, ok := r.byName[route.Device]
	if !ok {
		return plan{kind: dropMalformed, reason: "unknown device " + route.Device}
	}

	target := route.NextHop
	if !target.IsValid() {
		target = ip.Destination
	}
	mac, known := r.neighbors.Lookup(target)
	if !known {
		return plan{kind: probeARP, reason: "unresolved " + target.String(), egress: route.Device, target: target}
	}

	ip.TimeToLive--
	if err := packet.RefreshTransportChecksum(ip); err != nil {
		return plan{kind: dropMalformed, reason: "forward checksum"}
	}
	body, err := ip.Serialize(r.limits)
	if err != nil {
		return plan{kind: dropMalformed, reason: "forward serialize"}
	}
	link.Source = egress.mac
	link.Destination = mac
	link.EtherType = packet.EtherTypeIPv4
	link.Payload = body
	frame, err := link.Serialize(r.limits)
	if err != nil {
		return plan{kind: dropMalformed, reason: "forward frame"}
	}
	if egress.mtu > 0 && len(frame) > egress.mtu+ethernetMinimum {

		step := plan{
			kind:     dropOversize,
			reason:   "oversize for " + egress.name,
			icmpType: packet.ICMPTypeDestinationUnreachable,
			icmpCode: packet.ICMPCodeFragmentationNeeded,
			icmpMTU:  uint16(egress.mtu),
		}
		step.frame = r.icmpFrame(dev, raw, ip.Source, step.icmpType, step.icmpCode, step.icmpMTU)
		return step
	}
	return plan{
		kind:      forwardFrame,
		reason:    "forward",
		ethertype: packet.EtherTypeIPv4,
		egress:    route.Device,
		frame:     frame,
	}
}

const (
	minIPv4Header   = 20
	icmpHeader      = 8
	icmpErrorRoom   = 576 - minIPv4Header - icmpHeader
	ethernetMinimum = 14
)

func (r *Router) icmpFrame(dev *device, raw []byte, source netip.Addr, kind packet.ICMPType, code uint8, mtu uint16) []byte {
	if len(raw) <= ethernetMinimum {
		return nil
	}
	first := raw[ethernetMinimum]
	headerLength := int(first&0x0f) * 4
	if first>>4 != 4 || headerLength < minIPv4Header || ethernetMinimum+headerLength > len(raw) {
		return nil
	}
	original := raw[ethernetMinimum : ethernetMinimum+headerLength]

	room := len(raw) - ethernetMinimum
	if room > icmpErrorRoom {
		room = icmpErrorRoom
	}
	if room < headerLength {
		return nil
	}
	message := &packet.ICMP{Type: kind, Code: code}
	copy(message.RestOfHeader[:], original[0:4])
	if mtu > 0 && kind == packet.ICMPTypeDestinationUnreachable && code == packet.ICMPCodeFragmentationNeeded {

		putUint16(message.RestOfHeader[2:4], mtu)
	}
	message.Payload = append([]byte(nil), raw[ethernetMinimum:ethernetMinimum+room]...)
	body, err := message.Serialize()
	if err != nil {
		return nil
	}
	header := &packet.IPv4{
		TimeToLive:  64,
		Protocol:    packet.ProtocolICMP,
		Source:      r.addressOn(dev, source),
		Destination: source,
		Payload:     body,
	}
	packetBytes, err := header.Serialize(r.limits)
	if err != nil {
		return nil
	}
	link := &packet.Ethernet{
		Destination: cloneMAC(raw[6:12]),
		Source:      dev.mac,
		EtherType:   packet.EtherTypeIPv4,
		Payload:     packetBytes,
	}
	frame, err := link.Serialize(r.limits)
	if err != nil {
		return nil
	}
	return frame
}

func (r *Router) owns(address netip.Addr) bool {
	for _, dev := range r.devices {
		if dev.owns(address) {
			return true
		}
	}
	return false
}

func (d *device) owns(address netip.Addr) bool {
	for _, entry := range d.addresses {
		if entry.Addr() == address {
			return true
		}
	}
	return false
}

func (r *Router) addressOn(dev *device, target netip.Addr) netip.Addr {
	if dev == nil || len(dev.addresses) == 0 {
		return netip.Addr{}
	}
	for _, entry := range dev.addresses {
		if entry.Contains(target) {
			return entry.Addr()
		}
	}
	return dev.addresses[0].Addr()
}

func putUint16(dst []byte, value uint16) {
	dst[0] = byte(value >> 8)
	dst[1] = byte(value)
}

func multicast4(address netip.Addr) bool {
	octets := address.As4()
	return octets[0] >= 224 && octets[0] <= 239
}

func broadcast4(address netip.Addr) bool {
	return address == netip.AddrFrom4([4]byte{255, 255, 255, 255})
}

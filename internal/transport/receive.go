package transport

import (
	"time"
)

type receiver struct {
	next uint32

	slots []*segment

	out []*segment

	delivered int

	duplicated int

	dropped int

	closed bool

	limit int

	outLimit int

	maxPayload int
}

type segment struct {
	sequence uint32
	payload  []byte
	at       time.Time
}

func newReceiver(limits Limits) *receiver {
	return &receiver{
		next:       1,
		limit:      limits.MaxReorder,
		outLimit:   limits.MaxWindow,
		maxPayload: limits.MaxPayload,
	}
}

func (r *receiver) accept(frame Frame, now time.Time) (duplicate, refused bool) {
	if frame.Sequence < r.next {
		r.duplicated++
		return true, false
	}

	if len(frame.Payload) > r.maxPayload {
		r.dropped++
		return false, true
	}
	if r.indexOf(frame.Sequence) >= 0 {
		r.duplicated++
		return true, false
	}

	stored := &segment{
		sequence: frame.Sequence,
		payload:  append([]byte(nil), frame.Payload...),
		at:       now,
	}

	if frame.Sequence == r.next && len(r.out) < r.outLimit {
		r.out = append(r.out, stored)
		r.next++
		r.delivered++
		r.release()
		return false, false
	}

	if len(r.slots) >= r.limit {
		r.dropped++
		return false, true
	}
	r.slots = append(r.slots, stored)
	r.release()
	return false, false
}

func (r *receiver) release() {
	for len(r.out) < r.outLimit {
		index := r.indexOf(r.next)
		if index < 0 {
			return
		}
		slot := r.slots[index]
		r.slots = append(r.slots[:index], r.slots[index+1:]...)
		r.out = append(r.out, slot)
		r.next++
		r.delivered++
	}
}

func (r *receiver) pop() *segment {
	if len(r.out) == 0 {
		return nil
	}
	head := r.out[0]
	r.out = r.out[1:]
	r.release()
	return head
}

func (r *receiver) waiting() bool { return len(r.out) > 0 }

func (r *receiver) buffered() int { return len(r.slots) }

func (r *receiver) drained() bool { return r.closed && len(r.slots) == 0 }

func (r *receiver) ack() uint32 {
	if r.next == 0 {
		return 0
	}
	return r.next - 1
}

func (r *receiver) indexOf(sequence uint32) int {
	for index, slot := range r.slots {
		if slot.sequence == sequence {
			return index
		}
	}
	return -1
}

func (r *receiver) close() { r.closed = true }

type Stats struct {
	Delivered  int
	Duplicated int
	Dropped    int
	Buffered   int
	Waiting    int
	Closed     bool
}

func (r *receiver) stats() Stats {
	return Stats{
		Delivered:  r.delivered,
		Duplicated: r.duplicated,
		Dropped:    r.dropped,
		Buffered:   len(r.slots),
		Waiting:    len(r.out),
		Closed:     r.closed,
	}
}

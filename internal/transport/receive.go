package transport

import (
	"time"
)

// receiver is the read side of a connection: what has arrived, what is still
// missing, and what has been handed to the caller.
//
// A peer decides the order frames arrive in and may send the same frame any
// number of times, so both are handled here rather than assumed. Two fixed sets
// of slots are decided when the connection is built, which is what means a peer
// cannot make this side allocate by sending more out-of-order data than the
// limit allows: the frame is refused and acknowledged, the sender learns that
// what it sent is not what is wanted, and the buffers stay the size they were.
type receiver struct {
	// next is the sequence number the next segment released must have. Data
	// sequences start at one rather than zero so that a zero acknowledgement
	// can mean nothing has been received, which is what lets a frame be refused
	// without a separate flag saying so.
	next uint32
	// slots holds segments that arrived ahead of next and are waiting for the
	// gap in front of them to close.
	slots []*segment
	// out holds segments released in order and waiting for the caller to read
	// them. It is separate from slots because a caller that stops reading must
	// not hold the arrival path, and because the acknowledgement has to cover
	// what reached this queue rather than what the caller has yet to look at.
	out []*segment
	// delivered counts segments released, which is what the connection's
	// goodput is measured from.
	delivered int
	// duplicated counts frames refused because they had already been seen. A
	// path that duplicates is a path worth counting, not one worth hiding.
	duplicated int
	// dropped counts frames refused because a buffer was full or the payload
	// was larger than this connection allows.
	dropped int
	// closed reports that the peer's sending side finished.
	closed bool
	// limit is the most segments held behind a gap at once.
	limit int
	// outLimit is the most segments waiting for the caller at once.
	outLimit int
	// maxPayload refuses a segment larger than the connection allows, before
	// it is stored.
	maxPayload int
}

// segment is one stored payload.
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

// accept takes one data frame and reports what became of it. The result says
// whether the frame was a duplicate and whether it was refused, and the caller
// learns what to acknowledge from ack afterwards rather than from a returned
// value, because the acknowledgement is a fact about this receiver and not about
// the frame that happened to trigger it.
func (r *receiver) accept(frame Frame, now time.Time) (duplicate, refused bool) {
	if frame.Sequence < r.next {
		r.duplicated++
		return true, false
	}
	// Refused before it is stored, so a peer cannot make this side hold a
	// segment larger than the connection agreed to carry.
	if len(frame.Payload) > r.maxPayload {
		r.dropped++
		return false, true
	}
	if r.indexOf(frame.Sequence) >= 0 {
		r.duplicated++
		return true, false
	}
	// The payload is copied because the receive buffer outlives the datagram it
	// came in. This is the one copy on this path, and it is bounded by the
	// payload limit and the slot counts together.
	stored := &segment{
		sequence: frame.Sequence,
		payload:  append([]byte(nil), frame.Payload...),
		at:       now,
	}
	// A frame that closes the gap is released straight away rather than stored
	// behind the frames it unblocks. Giving it the queue's space before a slot
	// is what keeps a full reorder buffer from refusing the one segment that
	// would have emptied it, which would leave the stream waiting on data this
	// side already had. When the queue is full too, it waits in the slots like
	// anything else, and the caller draining the queue makes room for it.
	if frame.Sequence == r.next && len(r.out) < r.outLimit {
		r.out = append(r.out, stored)
		r.next++
		r.delivered++
		r.release()
		return false, false
	}
	// The reorder buffer is a fixed number of slots, and this is the check that
	// keeps it that size. A peer cannot make this side hold more by sending more
	// than the limit allows while a gap is open: the frame is refused, the
	// acknowledgement still says what arrived, and the sender learns to
	// retransmit the gap rather than to send more of what is already held.
	if len(r.slots) >= r.limit {
		r.dropped++
		return false, true
	}
	r.slots = append(r.slots, stored)
	r.release()
	return false, false
}

// release moves everything now in order out of the slots and into the queue for
// the caller, stopping when the gap is still open or the queue is full.
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

// pop returns the oldest released segment, or nil when none is waiting.
//
// Taking one frees queue space, so whatever became contiguous behind it is
// released here rather than waiting for the next datagram. Without that, a
// caller that caught up would still see nothing until the peer happened to send
// again, which is a stall this side controls and can avoid.
func (r *receiver) pop() *segment {
	if len(r.out) == 0 {
		return nil
	}
	head := r.out[0]
	r.out = r.out[1:]
	r.release()
	return head
}

// waiting reports whether anything has been released for the caller, so the
// reader can be woken without polling.
func (r *receiver) waiting() bool { return len(r.out) > 0 }

// buffered reports how many segments are still held behind a gap.
func (r *receiver) buffered() int { return len(r.slots) }

// drained reports whether the peer has finished sending and everything it sent
// has been delivered, which is the condition for an orderly read to reach the
// end.
func (r *receiver) drained() bool { return r.closed && len(r.slots) == 0 }

// ack is the cumulative acknowledgement: everything up to and including the
// segment released before the one still wanted. A cumulative acknowledgement
// means a lost acknowledgement costs a retransmission rather than a stalled
// stream, and a frame held behind a gap is never acknowledged, which is what
// keeps a sender from getting ahead of what this side can hold.
func (r *receiver) ack() uint32 {
	if r.next == 0 {
		return 0
	}
	return r.next - 1
}

// indexOf finds a stored sequence number, which is how a duplicate is detected
// without a set that a peer could grow.
func (r *receiver) indexOf(sequence uint32) int {
	for index, slot := range r.slots {
		if slot.sequence == sequence {
			return index
		}
	}
	return -1
}

// close marks the peer's sending side finished. Whatever it already sent is
// still delivered, because a peer that finished sending has sent it and
// dropping it would lose data the peer believes it delivered.
func (r *receiver) close() { r.closed = true }

// Stats is what a connection's receive side has done with what arrived. It is
// reported so a caller that is waiting on data, and a benchmark that is
// measuring it, can tell an empty queue from a queue the reader was never
// woken for.
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

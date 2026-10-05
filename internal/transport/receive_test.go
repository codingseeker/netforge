package transport

import (
	"testing"
	"time"
)

func dataFrame(sequence uint32, payload string) Frame {
	return Frame{Flags: FlagData, Sequence: sequence, Payload: []byte(payload)}
}

// give feeds one frame to a receiver and reports what became of it, the way a
// connection's read path does.
func give(t *testing.T, r *receiver, frame Frame) (duplicate, refused bool) {
	t.Helper()
	duplicate, refused = r.accept(frame, time.Now())
	return duplicate, refused
}

// take pops the oldest released segment and requires it to be there, so a test
// cannot silently pass by not releasing anything.
func take(t *testing.T, r *receiver, want string) {
	t.Helper()
	segment := r.pop()
	if segment == nil {
		t.Fatalf("nothing was released, want %q", want)
	}
	if string(segment.payload) != want {
		t.Fatalf("released %q, want %q", segment.payload, want)
	}
}

// takeNone requires that nothing was released, which is how a gap is asserted.
func takeNone(t *testing.T, r *receiver) {
	t.Helper()
	if segment := r.pop(); segment != nil {
		t.Fatalf("%q was released, want nothing: the gap in front of it is still open",
			segment.payload)
	}
}

// bounds are the receiver's own limits. They are small and explicit because
// every assertion below is about a bound being enforced at a known size.
func bounds() Limits {
	return Limits{MaxPayload: MaxPayloadSize, MaxReorder: 8, MaxWindow: 8}
}

func TestSegmentsAreReleasedInOrder(t *testing.T) {
	t.Parallel()
	r := newReceiver(bounds())

	if duplicate, refused := give(t, r, dataFrame(1, "one")); duplicate || refused {
		t.Fatalf("the first segment was refused: duplicate %v refused %v", duplicate, refused)
	}
	take(t, r, "one")
	if r.ack() != 1 {
		t.Errorf("ack() = %d, want 1", r.ack())
	}
	if duplicate, refused := give(t, r, dataFrame(2, "two")); duplicate || refused {
		t.Fatalf("the second segment was refused: duplicate %v refused %v", duplicate, refused)
	}
	take(t, r, "two")
	if r.ack() != 2 {
		t.Errorf("ack() = %d, want 2", r.ack())
	}
	if stats := r.stats(); stats.Delivered != 2 || stats.Buffered != 0 || stats.Waiting != 0 {
		t.Errorf("stats = %+v, want 2 delivered and nothing held", stats)
	}
}

// A segment that arrives early waits for the gap in front of it, and everything
// behind that gap is released the moment it closes. This is the whole of the
// reordering requirement.
func TestAnEarlySegmentWaitsForItsGapAndIsThenReleased(t *testing.T) {
	t.Parallel()
	r := newReceiver(bounds())

	// Three arrives first and must be held, not released.
	if duplicate, refused := give(t, r, dataFrame(3, "three")); duplicate || refused {
		t.Fatalf("an early segment was refused: duplicate %v refused %v", duplicate, refused)
	}
	takeNone(t, r)
	if r.buffered() != 1 {
		t.Errorf("buffered = %d, want 1", r.buffered())
	}
	// Nothing may be acknowledged that has not been released, or a sender would
	// move on and the gap would never be filled.
	if r.ack() != 0 {
		t.Errorf("ack() = %d, want 0: the peer must not be told a segment it has "+
			"not sent was received", r.ack())
	}

	// Two is still a gap, so it waits too.
	give(t, r, dataFrame(2, "two"))
	takeNone(t, r)
	if r.buffered() != 2 {
		t.Errorf("buffered = %d, want 2", r.buffered())
	}

	// One closes the gap and everything behind it comes out, in order, at once.
	give(t, r, dataFrame(1, "one"))
	take(t, r, "one")
	take(t, r, "two")
	take(t, r, "three")
	takeNone(t, r)
	if r.ack() != 3 {
		t.Errorf("ack() = %d, want 3: the acknowledgement covers everything released", r.ack())
	}
	if stats := r.stats(); stats.Buffered != 0 || stats.Delivered != 3 {
		t.Errorf("stats = %+v, want 3 delivered and nothing still held", stats)
	}
}

// A duplicated segment must be counted and answered, not stored and not
// delivered twice.
func TestADuplicateIsNotDeliveredTwice(t *testing.T) {
	t.Parallel()
	r := newReceiver(bounds())

	give(t, r, dataFrame(1, "one"))
	take(t, r, "one")
	// A retransmission of a segment already delivered.
	duplicate, refused := give(t, r, dataFrame(1, "one"))
	if !duplicate {
		t.Error("a duplicate of a delivered segment was not reported as one")
	}
	if refused {
		t.Error("a duplicate was reported as refused, which is a different thing")
	}
	takeNone(t, r)
	// A duplicate of a segment still held, which is what a path that copies
	// out-of-order data looks like.
	give(t, r, dataFrame(3, "three"))
	duplicate, refused = give(t, r, dataFrame(3, "three"))
	if !duplicate {
		t.Error("a duplicate of a held segment was not reported as one")
	}
	if refused {
		t.Error("a duplicate of a held segment was reported as refused")
	}
	if r.buffered() != 1 {
		t.Errorf("buffered = %d, want 1: the duplicate was not stored twice", r.buffered())
	}
	if stats := r.stats(); stats.Duplicated != 2 || stats.Delivered != 1 {
		t.Errorf("stats = %+v, want 2 duplicated and 1 delivered", stats)
	}
}

// The reorder buffer is the bound on what a peer can make this side hold. A
// peer that fills it has the rest refused and its gap left open, not an
// allocation.
func TestTheReorderBufferIsBounded(t *testing.T) {
	t.Parallel()
	r := newReceiver(Limits{MaxPayload: MaxPayloadSize, MaxReorder: 4, MaxWindow: 8})

	// Sequence 1 never arrives, so everything else waits behind an open gap.
	var refused int
	for sequence := uint32(2); sequence <= 20; sequence++ {
		if _, wasRefused := give(t, r, dataFrame(sequence, "x")); wasRefused {
			refused++
		}
	}
	if r.buffered() != 4 {
		t.Errorf("buffered = %d, want the limit of 4: a peer filled the buffer past its bound",
			r.buffered())
	}
	if refused != 15 {
		t.Errorf("refused = %d, want 15: the 15 frames past the limit", refused)
	}
	if stats := r.stats(); stats.Dropped != 15 {
		t.Errorf("Dropped = %d, want 15", stats.Dropped)
	}

	// Closing the gap releases what was held, so the buffer empties rather than
	// staying full and refusing data that now has somewhere to go.
	give(t, r, dataFrame(1, "one"))
	take(t, r, "one")
	if r.buffered() != 0 {
		t.Errorf("buffered = %d, want 0: the held segments were released", r.buffered())
	}
	if r.ack() != 5 {
		t.Errorf("ack() = %d, want 5: one through five are all released now", r.ack())
	}
}

// A segment larger than the connection agreed to carry is refused before it is
// stored, which is the other half of the memory bound.
func TestAnOversizedSegmentIsRefusedBeforeItIsStored(t *testing.T) {
	t.Parallel()
	r := newReceiver(Limits{MaxPayload: 16, MaxReorder: 8, MaxWindow: 8})

	oversized := Frame{Flags: FlagData, Sequence: 2, Payload: make([]byte, 17)}
	if _, refused := give(t, r, oversized); !refused {
		t.Error("an oversized segment was not refused")
	}
	if r.buffered() != 0 {
		t.Errorf("buffered = %d, want 0: the segment was not stored", r.buffered())
	}
	if r.stats().Dropped != 1 {
		t.Errorf("Dropped = %d, want 1", r.stats().Dropped)
	}
	// A segment within the limit is accepted afterwards, so the refusal was
	// about that segment and not about the connection.
	if _, refused := give(t, r, dataFrame(2, "small")); refused {
		t.Fatal("a segment within the limit was refused")
	}
	if r.buffered() != 1 {
		t.Errorf("buffered = %d, want 1", r.buffered())
	}
}

// A caller that stops reading must not be able to fill this side's memory, and
// the acknowledgement has to be withheld until there is room again, so the
// segment is retransmitted rather than lost.
func TestAStoppedCallerIsBoundedAndTheAckIsWithheld(t *testing.T) {
	t.Parallel()
	r := newReceiver(Limits{MaxPayload: MaxPayloadSize, MaxReorder: 8, MaxWindow: 2})

	// The caller reads nothing, so the queue fills and then everything is
	// refused, including data that is already in order.
	for sequence := uint32(1); sequence <= 10; sequence++ {
		give(t, r, dataFrame(sequence, "x"))
	}
	stats := r.stats()
	if stats.Waiting != 2 {
		t.Errorf("Waiting = %d, want the window of 2", stats.Waiting)
	}
	// What this side can hold is the window plus the reorder buffer, and no more
	// than that however much the peer sends: eight of the ten fit behind the
	// full queue and the other two were refused.
	if stats.Buffered != 8 {
		t.Errorf("Buffered = %d, want the reorder bound of 8", stats.Buffered)
	}
	if stats.Dropped != 0 {
		t.Errorf("Dropped = %d, want 0: all ten fit within the two bounds", stats.Dropped)
	}
	// The acknowledgement must not run past what is held for the caller, or the
	// sender would stop retransmitting data that was dropped here.
	if r.ack() != 2 {
		t.Errorf("ack() = %d, want 2: only what the caller can hold is acknowledged", r.ack())
	}
	// The caller catches up, and the peer retransmits from where the
	// acknowledgement stopped. Everything is then delivered, in order, with no
	// gap: the refused segments were never acknowledged, so the peer still has
	// them to resend.
	for sequence := uint32(3); sequence <= 10; sequence++ {
		give(t, r, dataFrame(sequence, "x"))
	}
	for want := 1; want <= 10; want++ {
		take(t, r, "x")
	}
	if r.ack() != 10 {
		t.Errorf("ack() = %d, want 10", r.ack())
	}
}

// A stored payload is a copy, because the receive buffer outlives the datagram
// buffer it was read from.
func TestAStoredPayloadIsACopy(t *testing.T) {
	t.Parallel()
	r := newReceiver(bounds())

	frame := dataFrame(2, "held")
	give(t, r, frame)

	// Overwrite the datagram the way the next read of that buffer would.
	for index := range frame.Payload {
		frame.Payload[index] = 'X'
	}
	give(t, r, dataFrame(1, "one"))
	take(t, r, "one")
	take(t, r, "held")
}

// A peer that finishes sending has not thereby stopped sending, so an orderly
// end is only reached once everything it sent has been delivered.
func TestAnOrderlyEndWaitsForEverythingSent(t *testing.T) {
	t.Parallel()
	r := newReceiver(bounds())

	give(t, r, dataFrame(2, "two"))
	r.close()
	if r.drained() {
		t.Error("drained() before the gap closed, want false: a segment is still held")
	}
	give(t, r, dataFrame(1, "one"))
	take(t, r, "one")
	take(t, r, "two")
	if !r.drained() {
		t.Error("drained() = false, want true once the peer's data is all delivered")
	}
}

package transport

import (
	"testing"
	"time"
)

func dataFrame(sequence uint32, payload string) Frame {
	return Frame{Flags: FlagData, Sequence: sequence, Payload: []byte(payload)}
}

func give(t *testing.T, r *receiver, frame Frame) (duplicate, refused bool) {
	t.Helper()
	duplicate, refused = r.accept(frame, time.Now())
	return duplicate, refused
}

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

func takeNone(t *testing.T, r *receiver) {
	t.Helper()
	if segment := r.pop(); segment != nil {
		t.Fatalf("%q was released, want nothing: the gap in front of it is still open",
			segment.payload)
	}
}

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

func TestAnEarlySegmentWaitsForItsGapAndIsThenReleased(t *testing.T) {
	t.Parallel()
	r := newReceiver(bounds())

	if duplicate, refused := give(t, r, dataFrame(3, "three")); duplicate || refused {
		t.Fatalf("an early segment was refused: duplicate %v refused %v", duplicate, refused)
	}
	takeNone(t, r)
	if r.buffered() != 1 {
		t.Errorf("buffered = %d, want 1", r.buffered())
	}

	if r.ack() != 0 {
		t.Errorf("ack() = %d, want 0: the peer must not be told a segment it has "+
			"not sent was received", r.ack())
	}

	give(t, r, dataFrame(2, "two"))
	takeNone(t, r)
	if r.buffered() != 2 {
		t.Errorf("buffered = %d, want 2", r.buffered())
	}

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

func TestADuplicateIsNotDeliveredTwice(t *testing.T) {
	t.Parallel()
	r := newReceiver(bounds())

	give(t, r, dataFrame(1, "one"))
	take(t, r, "one")

	duplicate, refused := give(t, r, dataFrame(1, "one"))
	if !duplicate {
		t.Error("a duplicate of a delivered segment was not reported as one")
	}
	if refused {
		t.Error("a duplicate was reported as refused, which is a different thing")
	}
	takeNone(t, r)

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

func TestTheReorderBufferIsBounded(t *testing.T) {
	t.Parallel()
	r := newReceiver(Limits{MaxPayload: MaxPayloadSize, MaxReorder: 4, MaxWindow: 8})

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

	give(t, r, dataFrame(1, "one"))
	take(t, r, "one")
	if r.buffered() != 0 {
		t.Errorf("buffered = %d, want 0: the held segments were released", r.buffered())
	}
	if r.ack() != 5 {
		t.Errorf("ack() = %d, want 5: one through five are all released now", r.ack())
	}
}

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

	if _, refused := give(t, r, dataFrame(2, "small")); refused {
		t.Fatal("a segment within the limit was refused")
	}
	if r.buffered() != 1 {
		t.Errorf("buffered = %d, want 1", r.buffered())
	}
}

func TestAStoppedCallerIsBoundedAndTheAckIsWithheld(t *testing.T) {
	t.Parallel()
	r := newReceiver(Limits{MaxPayload: MaxPayloadSize, MaxReorder: 8, MaxWindow: 2})

	for sequence := uint32(1); sequence <= 10; sequence++ {
		give(t, r, dataFrame(sequence, "x"))
	}
	stats := r.stats()
	if stats.Waiting != 2 {
		t.Errorf("Waiting = %d, want the window of 2", stats.Waiting)
	}

	if stats.Buffered != 8 {
		t.Errorf("Buffered = %d, want the reorder bound of 8", stats.Buffered)
	}
	if stats.Dropped != 0 {
		t.Errorf("Dropped = %d, want 0: all ten fit within the two bounds", stats.Dropped)
	}

	if r.ack() != 2 {
		t.Errorf("ack() = %d, want 2: only what the caller can hold is acknowledged", r.ack())
	}

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

func TestAStoredPayloadIsACopy(t *testing.T) {
	t.Parallel()
	r := newReceiver(bounds())

	frame := dataFrame(2, "held")
	give(t, r, frame)

	for index := range frame.Payload {
		frame.Payload[index] = 'X'
	}
	give(t, r, dataFrame(1, "one"))
	take(t, r, "one")
	take(t, r, "held")
}

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

package router

import (
	"sync"
	"testing"
	"time"
)

func TestQueueIsBounded(t *testing.T) {
	queue, err := newQueue(4)
	if err != nil {
		t.Fatal(err)
	}
	accepted := 0
	for i := 0; i < 64; i++ {
		if queue.offer(work{ingress: "eth0", frame: []byte{byte(i)}}) {
			accepted++
		}
	}
	if accepted != 4 {
		t.Errorf("the queue accepted %d items, want 4", accepted)
	}
	if got := queue.depth(); got != 4 {
		t.Errorf("depth is %d, want 4", got)
	}
	if got := queue.size(); got != 4 {
		t.Errorf("size is %d, want 4", got)
	}
}

func TestQueueDrainsInOrder(t *testing.T) {
	queue, err := newQueue(8)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if !queue.offer(work{ingress: "eth0", frame: []byte{byte(i)}}) {
			t.Fatalf("item %d was refused", i)
		}
	}
	for i := 0; i < 5; i++ {
		item, ok := queue.take()
		if !ok {
			t.Fatalf("item %d was missing", i)
		}
		if item.frame[0] != byte(i) {
			t.Fatalf("item %d arrived out of order: %d", i, item.frame[0])
		}
	}
	if queue.depth() != 0 {
		t.Errorf("depth is %d after draining, want 0", queue.depth())
	}
}

func TestQueueCloseDrainsThenStops(t *testing.T) {
	queue, err := newQueue(8)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		queue.offer(work{frame: []byte{byte(i)}})
	}
	queue.close()
	queue.close()
	for i := 0; i < 3; i++ {
		if _, ok := queue.take(); !ok {
			t.Fatalf("item %d was dropped by close", i)
		}
	}
	if _, ok := queue.take(); ok {
		t.Error("a closed queue kept producing items")
	}
	if queue.offer(work{frame: []byte{9}}) {
		t.Error("a closed queue accepted an item")
	}
}

func TestQueueServesWorkersUntilClosed(t *testing.T) {
	queue, err := newQueue(64)
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	seen := make(chan byte, workers*32)
	for i := 0; i < workers; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for {
				item, ok := queue.take()
				if !ok {
					return
				}
				seen <- item.frame[0]
			}
		}()
	}
	for i := 0; i < 40; i++ {
		if !queue.offer(work{frame: []byte{byte(i)}}) {
			t.Fatalf("item %d was refused", i)
		}
	}
	queue.close()
	group.Wait()
	close(seen)
	count := 0
	for range seen {
		count++
	}
	if count != 40 {
		t.Errorf("workers handled %d items, want 40", count)
	}
}

func TestQueueSizeIsValidated(t *testing.T) {
	if _, err := newQueue(1 << 20); err == nil {
		t.Error("an excessive queue size was accepted")
	}
	queue, err := newQueue(0)
	if err != nil {
		t.Fatal(err)
	}
	if queue.size() != defaultQueueSize {
		t.Errorf("default size is %d, want %d", queue.size(), defaultQueueSize)
	}
}

func TestQueueCloseUnblocksTake(t *testing.T) {
	queue, err := newQueue(4)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, ok := queue.take(); ok {
			t.Error("take produced an item from an empty queue")
		}
	}()
	time.Sleep(20 * time.Millisecond)
	queue.close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("close did not unblock take")
	}
}

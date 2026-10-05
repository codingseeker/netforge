package router

import (
	"strconv"
	"sync"

	"github.com/netforge/netforge/internal/apperr"
)

const (
	defaultQueueSize = 256
	maxQueueSize     = 4096
	workers          = 2
)

type work struct {
	ingress string
	frame   []byte
}

type queue struct {
	items  chan work
	closed chan struct{}
	once   sync.Once
}

func newQueue(size int) (*queue, error) {
	const op = "router.newQueue"
	if size <= 0 {
		size = defaultQueueSize
	}
	if size > maxQueueSize {
		return nil, apperr.InvalidArgument(op,
			"queue size "+strconv.Itoa(size)+" exceeds the maximum of "+
				strconv.Itoa(maxQueueSize), nil)
	}
	return &queue{
		items:  make(chan work, size),
		closed: make(chan struct{}),
	}, nil
}

func (q *queue) size() int { return cap(q.items) }

func (q *queue) depth() int { return len(q.items) }

func (q *queue) offer(item work) bool {
	select {
	case <-q.closed:
		return false
	default:
	}
	select {
	case q.items <- item:
		return true
	default:
		return false
	}
}

func (q *queue) take() (work, bool) {
	select {
	case item := <-q.items:
		return item, true
	case <-q.closed:
		for {
			select {
			case item := <-q.items:
				return item, true
			default:
				return work{}, false
			}
		}
	}
}

func (q *queue) close() {
	q.once.Do(func() { close(q.closed) })
}

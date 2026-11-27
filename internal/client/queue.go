package client

import (
	"context"
	"maps"
	"sync"

	"github.com/ContractorKeith/machine-monitor/internal/history"
)

type eventQueue struct {
	mu      sync.Mutex
	items   []*Event
	head    int
	live    int
	reports map[string]int
	status  int
	closed  bool
	wake    chan struct{}
}

func newEventQueue() *eventQueue {
	return &eventQueue{reports: map[string]int{}, status: -1, wake: make(chan struct{}, 1)}
}

func (q *eventQueue) push(e Event, limit int) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	switch e.Type {
	case EventReport:
		id := e.ID
		if e.Device != nil {
			id = e.Device.ID
		}
		if i, ok := q.reports[id]; ok && q.items[i] != nil {
			e.Samples = mergeSamples(q.items[i].Samples, e.Samples)
			if e.Sample == nil && len(e.Samples) > 0 {
				e.Sample = &e.Samples[len(e.Samples)-1]
			}
			q.drop(i)
		}
		q.reports[id] = len(q.items)
	case EventStatus:
		if q.status >= 0 && q.items[q.status] != nil {
			on := maps.Clone(q.items[q.status].Online)
			if on == nil {
				on = map[string]bool{}
			}
			maps.Copy(on, e.Online)
			e.Online = on
			q.drop(q.status)
		}
		q.status = len(q.items)
	case EventSnapshot, EventConnected, EventDisconnected:
		clear(q.reports)
		q.status = -1
	}
	q.items = append(q.items, &e)
	q.live++
	q.compact()
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return limit < 0 || q.live <= limit
}

func (q *eventQueue) drop(i int) {
	q.items[i] = nil
	q.live--
}

func (q *eventQueue) pop() (e Event, ok bool, closed bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for q.head < len(q.items) {
		p := q.items[q.head]
		q.items[q.head] = nil
		q.head++
		if p == nil {
			continue
		}
		q.live--
		q.compact()
		return *p, true, q.closed
	}
	return Event{}, false, q.closed
}

func (q *eventQueue) compact() {
	if len(q.items) < 64 || len(q.items) <= 2*q.live {
		return
	}
	positions := make(map[int]int, q.live)
	n := 0
	for i := q.head; i < len(q.items); i++ {
		if q.items[i] != nil {
			positions[i] = n
			q.items[n] = q.items[i]
			n++
		}
	}
	clear(q.items[n:])
	q.items = q.items[:n]
	for id, i := range q.reports {
		if pos, ok := positions[i]; ok {
			q.reports[id] = pos
		} else {
			delete(q.reports, id)
		}
	}
	if q.status >= 0 {
		if pos, ok := positions[q.status]; ok {
			q.status = pos
		} else {
			q.status = -1
		}
	}
	q.head = 0
}

func (q *eventQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *eventQueue) pump(ctx context.Context, out chan<- Event) {
	defer close(out)
	for {
		e, ok, closed := q.pop()
		if !ok {
			if closed {
				return
			}
			select {
			case <-q.wake:
				continue
			case <-ctx.Done():
				return
			}
		}
		select {
		case out <- e:
		case <-ctx.Done():
			return
		}
	}
}

func mergeSamples(a, b []history.Point) []history.Point {
	out := make([]history.Point, 0, len(a)+len(b))
	out = append(append(out, a...), b...)
	if len(out) > maxSamples {
		out = out[len(out)-maxSamples:]
	}
	return out
}

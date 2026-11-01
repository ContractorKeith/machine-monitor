package client

import (
	"context"
	"maps"
	"sync"

	"github.com/ContractorKeith/machine-monitor/internal/history"
)

// eventQueue sits between the network reader and the consumer, so a busy consumer never
// stalls the reader (the hub drops a stream that falls behind). Events that only replace
// state are merged while they wait: a device's reports into its newest one, status events
// into one. A busy hub sends one report per device every couple of seconds, so the queue
// stays about one slot per device however long the consumer takes.
//
// A merged event moves to the newer one's place, so newer state always lands last. The
// connection events and snapshots are barriers that nothing merges across: a report from
// before a reconnect must not land after the fresh snapshot. Other events (order, rename,
// remove, alerts) keep their place, and a report merged past one is newer than it anyway.
type eventQueue struct {
	mu      sync.Mutex
	items   []*Event       // nil marks a slot merged into a later one
	head    int            // items[:head] are delivered
	live    int            // queued, not yet delivered
	reports map[string]int // device id -> index of its queued report, since the last barrier
	status  int            // index of the queued status event since the last barrier, or -1
	closed  bool
	wake    chan struct{} // signalled when an event is queued or the queue closes
}

func newEventQueue() *eventQueue {
	return &eventQueue{reports: map[string]int{}, status: -1, wake: make(chan struct{}, 1)}
}

// push queues e, merging it into a queued event it supersedes. It returns false when more
// than limit events are waiting (limit < 0: no limit); e is queued either way.
func (q *eventQueue) push(e Event, limit int) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	switch e.Type {
	case EventReport:
		id := e.ID
		if e.Device != nil {
			id = e.Device.ID
		}
		if i, ok := q.reports[id]; ok && q.items[i] != nil { // nil: delivered already
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

// drop empties slot i after its event was merged into a newer one.
func (q *eventQueue) drop(i int) {
	q.items[i] = nil
	q.live--
}

// pop takes the oldest queued event; ok is false when none is queued.
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

// compact reclaims delivered and superseded slots even while the consumer is blocked.
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
			delete(q.reports, id) // already delivered: nothing to merge into
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

// close ends the queue: pump delivers what is left, then closes its channel.
func (q *eventQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// pump delivers queued events to out, in order, until the queue is closed and empty or ctx
// ends; then it closes out.
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

// mergeSamples appends b to a, keeping the newest maxSamples, in a fresh slice.
func mergeSamples(a, b []history.Point) []history.Point {
	out := make([]history.Point, 0, len(a)+len(b))
	out = append(append(out, a...), b...)
	if len(out) > maxSamples {
		out = out[len(out)-maxSamples:]
	}
	return out
}

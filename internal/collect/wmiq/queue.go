// Package wmiq runs every WMI query the gpu and sensors probes make on one long-lived
// worker goroutine, one at a time.
//
// yusufpapurcu/wmi holds a package-global lock and a locked OS thread for the whole of a
// query, and a query can't be cancelled. If each caller ran its own goroutine and gave up
// on a deadline, every abandoned query would still hold the lock, queue up behind the
// others and pin another OS thread, blocking all WMI in the process (gopsutil included).
// Here a caller that gives up just stops waiting: the worker finishes the query and drops
// the result. A caller only hands over a query when the worker is idle, so there is never
// more than one in flight and abandoned queries can't pile up.
package wmiq

import (
	"context"
	"sync"
)

// Queue is a single worker that runs funcs one at a time.
type Queue struct {
	start sync.Once
	jobs  chan job
}

type job struct {
	ctx  context.Context
	fn   func() error
	done chan error // buffered, so the worker never blocks on a caller that left
}

// NewQueue returns an idle queue; its worker starts on the first Do.
func NewQueue() *Queue { return &Queue{jobs: make(chan job)} }

// Do runs fn on the worker and returns its error, or ctx.Err() if ctx is done first,
// either while the worker is busy with an earlier func or while fn runs. After Do returns
// a ctx error, fn may still be running or not have run at all, so the caller must not
// read anything fn writes.
func (q *Queue) Do(ctx context.Context, fn func() error) error {
	q.start.Do(func() { go q.loop() })
	j := job{ctx: ctx, fn: fn, done: make(chan error, 1)}
	select {
	case q.jobs <- j:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-j.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (q *Queue) loop() {
	for j := range q.jobs {
		if err := j.ctx.Err(); err != nil {
			j.done <- err
			continue
		}
		j.done <- j.fn()
	}
}

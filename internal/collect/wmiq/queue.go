package wmiq

import (
	"context"
	"sync"
)

type Queue struct {
	start sync.Once
	jobs  chan job
}

type job struct {
	ctx  context.Context
	fn   func() error
	done chan error
}

func NewQueue() *Queue { return &Queue{jobs: make(chan job)} }

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

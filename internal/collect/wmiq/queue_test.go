package wmiq

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func TestDoReturnsResult(t *testing.T) {
	q := NewQueue()
	want := errors.New("boom")
	if err := q.Do(context.Background(), func() error { return want }); err != want {
		t.Errorf("err = %v, want %v", err, want)
	}
	ran := false
	if err := q.Do(context.Background(), func() error { ran = true; return nil }); err != nil || !ran {
		t.Errorf("err = %v ran = %v", err, ran)
	}
}

// TestSlowQueryDoesNotStack plays many 2 s samples against one query that hangs: each
// caller gives up on its deadline, but only the hung query ever runs and no goroutines
// pile up behind it.
func TestSlowQueryDoesNotStack(t *testing.T) {
	q := NewQueue()
	release := make(chan struct{})
	var started, inFlight, maxInFlight atomic.Int32
	slow := func() error {
		started.Add(1)
		if n := inFlight.Add(1); n > maxInFlight.Load() {
			maxInFlight.Store(n)
		}
		defer inFlight.Add(-1)
		<-release
		return nil
	}

	// Warm up so the worker goroutine is part of the baseline.
	if err := q.Do(context.Background(), func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	before := runtime.NumGoroutine()

	for i := 0; i < 50; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		err := q.Do(ctx, slow)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("sample %d: err = %v, want deadline exceeded", i, err)
		}
	}
	if n := started.Load(); n != 1 {
		t.Errorf("%d queries started while the first hung, want 1", n)
	}
	if n := maxInFlight.Load(); n != 1 {
		t.Errorf("%d queries in flight at once, want 1", n)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Errorf("goroutines grew from %d to %d across samples", before, after)
	}

	// Once the hung query finishes, the queue serves the next caller normally.
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := q.Do(ctx, func() error { return nil }); err != nil {
		t.Errorf("after release: %v", err)
	}
}

func TestDoSkipsExpiredJob(t *testing.T) {
	q := NewQueue()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ran := false
	if err := q.Do(ctx, func() error { ran = true; return nil }); !errors.Is(err, context.Canceled) || ran {
		t.Errorf("err = %v ran = %v; want canceled, not run", err, ran)
	}
}

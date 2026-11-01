package client

import "testing"

// A blocked consumer must not retain one empty slot for every superseded report.
func TestCoalescingStorageBoundedWithoutConsumer(t *testing.T) {
	q := newEventQueue()
	for range 10000 {
		if !q.push(Event{Type: EventReport, ID: "one"}, 256) {
			t.Fatal("one pending report overflowed")
		}
		q.push(Event{Type: EventStatus, Online: map[string]bool{"one": true}}, 256)
	}
	if len(q.items) > 64 || cap(q.items) > 128 || q.live != 2 {
		t.Fatalf("coalesced queue retains %d slots for %d events", len(q.items), q.live)
	}
	for _, typ := range []string{EventReport, EventStatus} {
		e, ok, _ := q.pop()
		if !ok || e.Type != typ {
			t.Fatalf("pop = %s, %v; want %s", e.Type, ok, typ)
		}
	}
	if _, ok, _ := q.pop(); ok {
		t.Fatal("queue not empty")
	}
}

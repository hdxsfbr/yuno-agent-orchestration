package events

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

func TestHubFiltersByRun(t *testing.T) {
	h := NewHub()
	all, cancelAll := h.Subscribe("", 8)
	defer cancelAll()
	one, cancelOne := h.Subscribe("r1", 8)
	defer cancelOne()

	h.Publish(Event{Seq: 1, RunID: "r1", Type: "a", Data: json.RawMessage(`{}`), At: time.Now()})
	h.Publish(Event{Seq: 2, RunID: "r2", Type: "b", Data: json.RawMessage(`{}`), At: time.Now()})

	if e := <-all; e.Seq != 1 {
		t.Fatalf("all got %d", e.Seq)
	}
	if e := <-all; e.Seq != 2 {
		t.Fatalf("all got %d", e.Seq)
	}
	if e := <-one; e.Seq != 1 {
		t.Fatalf("one got %d", e.Seq)
	}
	select {
	case e := <-one:
		t.Fatalf("one should not get r2, got %d", e.Seq)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestHubDropsWhenFull(t *testing.T) {
	h := NewHub()
	ch, cancel := h.Subscribe("", 1)
	defer cancel()
	h.Publish(Event{Seq: 1})
	h.Publish(Event{Seq: 2}) // dropped, must not block
	if e := <-ch; e.Seq != 1 {
		t.Fatalf("got %d", e.Seq)
	}
	if h.Dropped() != 1 {
		t.Fatalf("dropped = %d", h.Dropped())
	}
}

func TestCancelTwiceIsSafe(t *testing.T) {
	h := NewHub()
	_, cancel := h.Subscribe("", 1)
	cancel()
	cancel()
	if h.Subscribers() != 0 {
		t.Fatal("subscriber not removed")
	}
}

// Publishers run concurrently under the read lock; the drop counter must not race
// (audit #26, run with -race) nor lose counts.
func TestHubConcurrentDrops(t *testing.T) {
	h := NewHub()
	_, cancel := h.Subscribe("", 1)
	defer cancel()
	const publishers, each = 8, 200
	var wg sync.WaitGroup
	for range publishers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				h.Publish(Event{Seq: int64(i)})
			}
		}()
	}
	wg.Wait()
	if got, want := h.Dropped(), int64(publishers*each-1); got != want {
		t.Fatalf("dropped = %d, want %d", got, want)
	}
}

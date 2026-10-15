// Package events is the in-process fan-out behind SSE. The DB (run_events) is the
// record; the hub is best effort (DESIGN 5.1 rule 6, 5.12 rule 3).
package events

import (
	"encoding/json"
	"sync"
	"time"
)

type Event struct {
	Seq   int64           `json:"seq"`
	RunID string          `json:"run_id,omitempty"`
	Type  string          `json:"event"`
	Data  json.RawMessage `json:"data"`
	At    time.Time       `json:"at"`
}

type subscriber struct {
	ch    chan Event
	runID string // "" subscribes to every run
}

type Hub struct {
	mu      sync.RWMutex
	subs    map[*subscriber]struct{}
	dropped int64
}

func NewHub() *Hub {
	return &Hub{subs: map[*subscriber]struct{}{}}
}

// Subscribe returns a channel of events for runID ("" for all) and a cancel func.
func (h *Hub) Subscribe(runID string, buffer int) (<-chan Event, func()) {
	if buffer <= 0 {
		buffer = 256
	}
	s := &subscriber{ch: make(chan Event, buffer), runID: runID}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return s.ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, s)
			h.mu.Unlock()
			close(s.ch)
		})
	}
}

// Publish delivers e to matching subscribers without blocking; a full subscriber
// drops the event (it can recover via Last-Event-ID replay).
func (h *Hub) Publish(e Event) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for s := range h.subs {
		if s.runID != "" && s.runID != e.RunID {
			continue
		}
		select {
		case s.ch <- e:
		default:
			h.dropped++
		}
	}
}

// Subscribers is for tests and /api/status.
func (h *Hub) Subscribers() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs)
}

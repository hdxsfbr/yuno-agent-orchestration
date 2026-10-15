// Package engine executes runs: the state machine, limits, approval suspension and
// resumption (DESIGN 5.1 to 5.5).
package engine

import (
	"sync"
	"time"
)

// Clock is injected so every time boundary in DESIGN 5.2 is testable from both sides.
type Clock interface {
	Now() time.Time
	Sleep(d time.Duration)
}

type RealClock struct{}

func (RealClock) Now() time.Time        { return time.Now().UTC() }
func (RealClock) Sleep(d time.Duration) { time.Sleep(d) }

// FakeClock advances only when told to. Sleep advances it immediately so backoff tests
// run instantly while still recording the requested waits.
type FakeClock struct {
	mu     sync.Mutex
	now    time.Time
	Sleeps []time.Duration
}

func NewFakeClock(start time.Time) *FakeClock {
	return &FakeClock{now: start.UTC()}
}

func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func (c *FakeClock) Sleep(d time.Duration) {
	c.mu.Lock()
	c.Sleeps = append(c.Sleeps, d)
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

package httpapi

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// The watchdog trips only after continuous failures for the whole window: a single
// failure, or failures interrupted by a success, never end the process.
func TestDBWatchdogTripsAfterContinuousFailures(t *testing.T) {
	var fail atomic.Bool
	ping := func(context.Context) error {
		if fail.Load() {
			return errors.New("lookup postgres: no such host")
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- dbWatchdog(ctx, ping, 10*time.Millisecond, 100*time.Millisecond) }()

	// Healthy, then one failure, then healthy again: no trip.
	time.Sleep(40 * time.Millisecond)
	fail.Store(true)
	time.Sleep(30 * time.Millisecond)
	fail.Store(false)
	time.Sleep(150 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("watchdog tripped on an interrupted failure: %v", err)
	default:
	}

	// Continuous failures for longer than the window: trip, with the cause.
	fail.Store(true)
	select {
	case err := <-done:
		if err == nil || !errors.Is(err, err) || err.Error() == "" {
			t.Fatalf("unexpected result %v", err)
		}
		if got := err.Error(); !contains(got, "database unreachable") || !contains(got, "no such host") {
			t.Fatalf("error text %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not trip")
	}
}

// Ending the context stops the watchdog without an error, even while failing.
func TestDBWatchdogStopsWithContext(t *testing.T) {
	ping := func(context.Context) error { return errors.New("down") }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- dbWatchdog(ctx, ping, 5*time.Millisecond, time.Hour) }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected nil on context end, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("watchdog did not stop")
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

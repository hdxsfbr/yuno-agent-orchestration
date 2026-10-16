package httpapi

import (
	"context"
	"fmt"
	"time"
)

// dbWatchdog pings the database every tick and returns an error once the pings have
// failed continuously for window. The api shares the gateway container's network
// namespace (compose `network_mode: service:openclaw`); when that container dies and
// restarts, this process keeps a namespace that no longer resolves `postgres` and can
// never recover on its own. Exiting lets the container restart policy start a fresh
// process in the gateway's new namespace. It returns nil when ctx ends first.
func dbWatchdog(ctx context.Context, ping func(context.Context) error, tick, window time.Duration) error {
	t := time.NewTicker(tick)
	defer t.Stop()
	var failingSince time.Time
	var last error
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-t.C:
			pctx, cancel := context.WithTimeout(ctx, tick)
			err := ping(pctx)
			cancel()
			if err == nil {
				failingSince = time.Time{}
				continue
			}
			last = err
			if failingSince.IsZero() {
				failingSince = now
				continue
			}
			if now.Sub(failingSince) >= window {
				return fmt.Errorf("database unreachable for %s, exiting so the container restarts: %w", window, last)
			}
		}
	}
}

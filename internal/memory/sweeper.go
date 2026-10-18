package memory

import (
	"context"
	"log/slog"
	"time"

	"yuno/internal/store"
)

// SweepInterval is how often expired memory is deleted (DESIGN 5.10 rule 6).
const SweepInterval = 10 * time.Minute

// SweepFunc deletes one kind of expired memory at now and returns how many rows.
type SweepFunc func(ctx context.Context, now time.Time) (int64, error)

// Sweeper deletes expired facts and expired project notes every Interval. Also holds
// further retention sweeps (conversation messages older than conversation_ttl_days live
// with the channel code).
// Reads already exclude expired rows, so when the sweep runs is invisible.
type Sweeper struct {
	Pool     store.Querier
	Log      *slog.Logger
	Now      func() time.Time
	Interval time.Duration
	Also     []SweepFunc
}

// Run sweeps at start and then every Interval until ctx ends.
func (s *Sweeper) Run(ctx context.Context) {
	every := s.Interval
	if every <= 0 {
		every = SweepInterval
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		s.SweepOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// SweepOnce runs every sweep once; failures are logged and the others still run.
func (s *Sweeper) SweepOnce(ctx context.Context) int64 {
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	log := s.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	// Agent facts and project notes are swept in the same pass (DESIGN 5.18 rule 5).
	sweeps := append([]SweepFunc{
		func(ctx context.Context, now time.Time) (int64, error) { return SweepExpired(ctx, s.Pool, now) },
		func(ctx context.Context, now time.Time) (int64, error) {
			return SweepExpiredProjectFacts(ctx, s.Pool, now)
		},
	}, s.Also...)
	var total int64
	for _, sweep := range sweeps {
		n, err := sweep(ctx, now)
		if err != nil {
			if ctx.Err() == nil {
				log.Warn("memory sweep failed", "error", err)
			}
			continue
		}
		total += n
	}
	if total > 0 {
		log.Info("memory sweep", "deleted", total)
	}
	return total
}

package sched

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"yuno/internal/engine"
	"yuno/internal/guardrails"
	"yuno/internal/store"
)

// DefaultTick is SCHEDULER_TICK's default (DESIGN 5.9 inputs).
const DefaultTick = 10 * time.Second

// tickBatch bounds how many due schedules one transaction fires; the rest wait for the
// next tick.
const tickBatch = 50

// ErrorWorkflowDeleted is last_error for a schedule whose workflow is gone (rule 4).
const ErrorWorkflowDeleted = "workflow deleted"

// Triggerer is the part of the engine the scheduler needs.
type Triggerer interface {
	Trigger(ctx context.Context, req engine.TriggerRequest) (*store.Run, error)
}

// Clock is the scheduler's time source; tests pin it.
type Clock interface {
	Now() time.Time
}

// Scheduler fires due schedules every Tick. Emit publishes SSE events (nil skips them).
type Scheduler struct {
	Pool   *pgxpool.Pool
	Engine Triggerer
	Clock  Clock
	Tick   time.Duration
	Log    *slog.Logger
	Emit   func(ctx context.Context, runID, typ string, v any)
}

// Fired is one run a tick created.
type Fired struct {
	Schedule store.Schedule
	Run      *store.Run
}

// Run ticks until ctx ends. A failed tick is logged and the next one tries again.
func (s *Scheduler) Run(ctx context.Context) {
	tick := s.Tick
	if tick <= 0 {
		tick = DefaultTick
	}
	s.log().Info("scheduler started", "tick", tick.String())
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		if _, err := s.TickOnce(ctx); err != nil && ctx.Err() == nil {
			s.log().Warn("scheduler tick failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// TickOnce fires every due schedule (DESIGN 5.9 rules 2 to 4). The due rows are
// selected and updated in one transaction that holds their locks while each run is
// created through the engine; a cron schedule then moves to its next occurrence after
// now, so downtime across several occurrences yields one run, and a delay schedule is
// disabled. The run is created on the engine's own connection, keyed by the occurrence:
// an occurrence whose run exists already (the update was lost after the run was created)
// counts as fired and advances. A run the engine refuses (validation, rate limit) records
// last_error and still advances; a missing workflow disables the schedule; any other
// failure records last_error and leaves the schedule due for the next tick.
func (s *Scheduler) TickOnce(ctx context.Context) ([]Fired, error) {
	now := s.now()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	due, err := store.LockDueSchedules(ctx, tx, now, tickBatch)
	if err != nil {
		return nil, fmt.Errorf("select due schedules: %w", err)
	}
	var fired []Fired
	for i := range due {
		d := &due[i]
		out, run := s.fire(ctx, d, now)
		if err := store.RecordScheduleOutcome(ctx, tx, d.ID, out); err != nil {
			return nil, fmt.Errorf("record schedule %s: %w", d.ID, err)
		}
		if run != nil {
			sc := d.Schedule
			sc.Enabled, sc.NextRunAt, sc.LastRunAt, sc.LastRunID, sc.LastError, sc.UpdatedAt =
				out.Enabled, out.NextRunAt, out.LastRunAt, out.LastRunID, out.LastError, now
			fired = append(fired, Fired{Schedule: sc, Run: run})
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit tick: %w", err)
	}
	for _, f := range fired {
		if s.Emit != nil {
			s.Emit(ctx, f.Run.ID, "schedule.fired", map[string]any{"schedule": f.Schedule, "run": f.Run})
		}
	}
	return fired, nil
}

// fire creates the run for one due schedule and returns the outcome to record.
func (s *Scheduler) fire(ctx context.Context, d *store.DueSchedule, now time.Time) (store.ScheduleOutcome, *store.Run) {
	log := s.log().With("schedule_id", d.ID, "workflow_id", d.WorkflowID)
	out := store.ScheduleOutcome{Enabled: d.Enabled, NextRunAt: d.NextRunAt, At: now}
	if d.WorkflowGone {
		log.Warn("schedule disabled: workflow deleted")
		return disabled(out, ErrorWorkflowDeleted), nil
	}
	if d.Kind == store.ScheduleCron {
		if _, err := NextRun(&d.Schedule, now); err != nil {
			// Validated on save, so this is a row written around the API or before the
			// check existed; its next_run_at is not a real occurrence.
			msg := "invalid schedule: " + err.Error()
			log.Warn("schedule disabled", "reason", msg)
			return disabled(out, msg), nil
		}
	}
	// The run is keyed by the occurrence it fires, so firing it again after the schedule
	// update was lost finds that run instead of creating a second one. The schedule's
	// project goes with the request (rule 6); the engine checks it against the workflow
	// and snapshots it onto the run like a manual request's.
	ref := store.ScheduleOccurrenceRef(d.ID, *d.NextRunAt)
	req := engine.TriggerRequest{
		WorkflowID: d.WorkflowID,
		Input:      d.Input,
		Trigger:    store.TriggerSchedule,
		TriggerRef: ref,
	}
	if d.ProjectID != nil {
		req.ProjectID = *d.ProjectID
	}
	run, err := s.Engine.Trigger(ctx, req)
	var ve *engine.ValidationError
	var rl *guardrails.RateLimitError
	switch {
	case err == nil:
		log.Info("schedule fired", "run_id", run.ID, "trigger_ref", ref)
		out = s.advance(d, out, now, nil)
		out.LastRunAt, out.LastRunID = &now, &run.ID
		return out, run
	case errors.Is(err, engine.ErrDuplicateTrigger):
		log.Warn("schedule occurrence had already fired; advancing", "trigger_ref", ref)
		out = s.advance(d, out, now, nil)
		if prev, gerr := store.GetRunByTrigger(ctx, s.Pool, store.TriggerSchedule, ref); gerr == nil {
			out.LastRunAt, out.LastRunID = &prev.CreatedAt, &prev.ID
		}
		return out, nil
	case errors.As(err, &ve):
		msg := "the workflow cannot run: " + strings.Join(ve.Messages, "; ")
		log.Warn("scheduled run refused", "reason", msg)
		return s.advance(d, out, now, &msg), nil
	case errors.As(err, &rl):
		msg := rl.Error()
		log.Warn("scheduled run refused", "reason", msg)
		return s.advance(d, out, now, &msg), nil
	case errors.Is(err, store.ErrNotFound):
		log.Warn("schedule disabled: workflow deleted")
		return disabled(out, ErrorWorkflowDeleted), nil
	default:
		// Most likely transient (database, shutdown): keep the occurrence due.
		log.Error("scheduled run could not be created", "error", err)
		msg := "the run could not be created; retrying"
		out.LastError = &msg
		return out, nil
	}
}

// advance moves a fired schedule on: a cron schedule to its next occurrence after now,
// a delay schedule to disabled.
func (s *Scheduler) advance(d *store.DueSchedule, out store.ScheduleOutcome, now time.Time, lastError *string) store.ScheduleOutcome {
	out.LastError = lastError
	if d.Kind != store.ScheduleCron {
		out.Enabled, out.NextRunAt = false, nil
		return out
	}
	next, err := NextRun(&d.Schedule, now)
	if err != nil {
		// Validated on save, so this is a row written around the API.
		msg := "invalid schedule: " + err.Error()
		return disabled(out, msg)
	}
	out.NextRunAt = next
	return out
}

func disabled(out store.ScheduleOutcome, reason string) store.ScheduleOutcome {
	out.Enabled, out.NextRunAt, out.LastError = false, nil, &reason
	return out
}

func (s *Scheduler) now() time.Time {
	if s.Clock != nil {
		return s.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Scheduler) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.New(slog.DiscardHandler)
}

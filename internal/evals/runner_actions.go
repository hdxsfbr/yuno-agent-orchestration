package evals

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"yuno/internal/store"
)

// startRun triggers the case's run: POST /api/runs, or a one-shot schedule when the case
// asks for actions.schedule. Whatever it learns about the schedule goes into obs.
func (r *Runner) startRun(ctx context.Context, c Case, req RunRequest, cr *caseResources, obs *Observed) (*store.Run, error) {
	if c.Actions.Schedule == nil {
		run, err := r.api.CreateRun(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("create run: %w", err)
		}
		return run, nil
	}
	return r.runThroughSchedule(ctx, c, req, cr, obs)
}

// runThroughSchedule creates a delay schedule for the case's workflow (DESIGN 5.9) and
// waits for the scheduler to fire it: the schedule's last_run_id names the run it
// created. A schedule that records an error instead, or does not fire in time, fails the
// case.
func (r *Runner) runThroughSchedule(ctx context.Context, c Case, req RunRequest, cr *caseResources, obs *Observed) (*store.Run, error) {
	act := c.Actions.Schedule
	runAt := r.now().Add(time.Duration(act.RunInSeconds) * time.Second).UTC().Truncate(time.Second)
	sch, err := r.api.CreateSchedule(ctx, ScheduleRequest{
		WorkflowID: req.WorkflowID, Kind: act.Kind, RunAt: runAt, Input: req.Input, Enabled: true,
	})
	if err != nil {
		return nil, fmt.Errorf("create schedule: %w", err)
	}
	cr.scheduleID = sch.ID
	obs.Schedule = sch
	r.log.Info("eval schedule created", "case", c.ID, "schedule", sch.ID, "run_at", runAt.Format(time.RFC3339))

	wait := time.Duration(act.wait()) * time.Second
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	failures := 0
	for {
		cur, err := r.api.GetSchedule(ctx, sch.ID)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			failures++
			r.log.Warn("eval schedule poll failed", "case", c.ID, "schedule", sch.ID, "error", err, "consecutive", failures)
			if failures >= maxPollFailures {
				return nil, fmt.Errorf("poll schedule: %w", err)
			}
		default:
			failures = 0
			obs.Schedule = cur
			if id := cur.RunID(); id != "" {
				run, err := r.api.GetRun(ctx, id)
				if err != nil {
					return nil, fmt.Errorf("get the scheduled run %s: %w", id, err)
				}
				return run, nil
			}
			if msg := cur.LastErrorText(); msg != "" {
				return nil, fmt.Errorf("the schedule recorded an error instead of a run: %s", msg)
			}
		}
		wake := time.NewTimer(r.poll)
		select {
		case <-ctx.Done():
			wake.Stop()
			return nil, ctx.Err()
		case <-deadline.C:
			wake.Stop()
			return nil, fmt.Errorf("the schedule created no run within %s of its creation (run_at %s)", wait, runAt.Format(time.RFC3339))
		case <-wake.C:
		}
	}
}

// observe collects what the case's expectations need from outside the trace, once the run
// has ended: the schedule's final state, the answer to an extra trigger, the tool list. A
// failure to observe is the case's error: its expectation cannot be judged.
func (r *Runner) observe(ctx context.Context, c Case, req RunRequest, obs *Observed, res *CaseResult) {
	fail := func(format string, args ...any) {
		if res.Error == "" {
			res.Error = fmt.Sprintf(format, args...)
		}
	}
	if obs.Schedule != nil {
		cur, err := r.api.GetSchedule(ctx, obs.Schedule.ID)
		if err != nil {
			fail("read the schedule back: %v", err)
		} else {
			obs.Schedule = cur
		}
	}
	if c.Actions.Retrigger {
		obs.Retrigger = r.retrigger(ctx, c, req)
	}
	if len(c.Expect.ToolsPresent) > 0 || len(c.Expect.ToolsAbsent) > 0 {
		names, err := r.api.ListTools(ctx)
		if err != nil {
			fail("list tools: %v", err)
			return
		}
		obs.Tools, obs.ToolsListed = names, true
	}
}

// retrigger triggers the case's run request a second time and records the answer. The
// request names a script that was never registered: should the api accept it, the run
// it creates cannot take steps from the first run's script, and the runner cancels it.
func (r *Runner) retrigger(ctx context.Context, c Case, req RunRequest) *TriggerAttempt {
	again := req
	if req.ScriptID != "" {
		again.ScriptID = req.ScriptID + "-again"
	}
	run, err := r.api.CreateRun(ctx, again)
	if err == nil {
		r.log.Warn("eval extra trigger was accepted", "case", c.ID, "run", run.ID)
		r.cancelRun(ctx, c.ID, run.ID)
		return &TriggerAttempt{Status: http.StatusCreated, Message: "the api created run " + run.ID}
	}
	var ae *APIError
	if errors.As(err, &ae) {
		r.log.Info("eval extra trigger answered", "case", c.ID, "status", ae.Status, "message", ae.Message)
		return &TriggerAttempt{Status: ae.Status, Message: ae.Message}
	}
	return &TriggerAttempt{Message: err.Error()}
}

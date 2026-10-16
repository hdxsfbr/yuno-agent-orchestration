package sched

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"yuno/internal/config"
	"yuno/internal/engine"
	"yuno/internal/guardrails"
	"yuno/internal/store"
)

// testPool is the yuno_test database when DATABASE_URL is set; DB-backed tests skip
// otherwise. Tests delete the schedules they create, and pin the clock in 2001 so a
// tick never sees another package's schedules (their next run is computed from the
// real clock).
var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	if url := os.Getenv("DATABASE_URL"); url != "" {
		ctx := context.Background()
		pool, err := store.Open(ctx, url)
		if err != nil {
			fmt.Fprintln(os.Stderr, "sched tests: open database:", err)
			os.Exit(1)
		}
		if err := store.Migrate(ctx, pool, slog.New(slog.DiscardHandler)); err != nil {
			fmt.Fprintln(os.Stderr, "sched tests: migrate:", err)
			os.Exit(1)
		}
		testPool = pool
	}
	code := m.Run()
	if testPool != nil {
		testPool.Close()
	}
	os.Exit(code)
}

func needDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testPool == nil {
		t.Skip("DATABASE_URL not set")
	}
	return testPool
}

// fakeTrigger records trigger requests and answers with err, or a fresh run.
type fakeTrigger struct {
	mu   sync.Mutex
	reqs []engine.TriggerRequest
	err  error
}

func (f *fakeTrigger) Trigger(_ context.Context, req engine.TriggerRequest) (*store.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	if f.err != nil {
		return nil, f.err
	}
	ref := req.TriggerRef
	return &store.Run{ID: store.NewID(), WorkflowID: req.WorkflowID, Trigger: req.Trigger, TriggerRef: &ref, Status: store.RunQueued}, nil
}

func (f *fakeTrigger) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

type emitted struct {
	runID, typ string
	payload    map[string]any
}

type fixture struct {
	t      *testing.T
	pool   *pgxpool.Pool
	clock  *engine.FakeClock
	trig   *fakeTrigger
	events []emitted
	s      *Scheduler
	wf     *store.Workflow
}

func newFixture(t *testing.T, now time.Time) *fixture {
	t.Helper()
	pool := needDB(t)
	f := &fixture{t: t, pool: pool, clock: engine.NewFakeClock(now), trig: &fakeTrigger{}}
	f.s = &Scheduler{Pool: pool, Engine: f.trig, Clock: f.clock, Emit: func(_ context.Context, runID, typ string, v any) {
		f.events = append(f.events, emitted{runID: runID, typ: typ, payload: v.(map[string]any)})
	}}
	// A definition the real engine accepts (start to end, no agents).
	f.wf = &store.Workflow{Name: "sched test", Definition: json.RawMessage(`{"nodes":[{"id":"start","kind":"start"},
		{"id":"end","kind":"end"}],"edges":[{"from":"start","to":"end"}]}`)}
	if err := store.CreateWorkflow(context.Background(), pool, f.wf); err != nil {
		t.Fatal(err)
	}
	return f
}

// schedule stores a schedule due at nextRunAt and deletes it when the test ends.
func (f *fixture) schedule(kind, expr, tz string, nextRunAt time.Time) *store.Schedule {
	f.t.Helper()
	s := &store.Schedule{WorkflowID: f.wf.ID, Kind: kind, TZ: tz, Enabled: true, NextRunAt: &nextRunAt,
		Input: json.RawMessage(`{"topic":"weekly"}`)}
	if kind == store.ScheduleCron {
		s.CronExpr = &expr
	} else {
		s.RunAt = &nextRunAt
	}
	if err := store.CreateSchedule(context.Background(), f.pool, s); err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = store.DeleteSchedule(context.Background(), f.pool, s.ID) })
	return s
}

func (f *fixture) tick() []Fired {
	f.t.Helper()
	fired, err := f.s.TickOnce(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	return fired
}

func (f *fixture) get(id string) *store.Schedule {
	f.t.Helper()
	s, err := store.GetSchedule(context.Background(), f.pool, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

func at(t *testing.T, s string) time.Time { return mustTime(t, s) }

// Rule 2: a due cron schedule creates one run with trigger schedule, the occurrence
// "<schedule id>@<due time>" as the reference and its input, then moves to the next
// occurrence after now.
func TestTickFiresCron(t *testing.T) {
	f := newFixture(t, at(t, "2001-01-01T09:00:05Z"))
	s := f.schedule(store.ScheduleCron, "0 9 * * *", "UTC", at(t, "2001-01-01T09:00:00Z"))
	notDue := f.schedule(store.ScheduleCron, "0 9 * * *", "UTC", at(t, "2001-01-02T09:00:00Z"))

	fired := f.tick()
	if len(fired) != 1 || fired[0].Schedule.ID != s.ID {
		t.Fatalf("fired = %+v, want only %s", fired, s.ID)
	}
	req := f.trig.reqs[0]
	if req.WorkflowID != f.wf.ID || req.Trigger != store.TriggerSchedule || req.TriggerRef != s.ID+"@2001-01-01T09:00:00Z" || string(req.Input) != `{"topic": "weekly"}` && string(req.Input) != `{"topic":"weekly"}` {
		t.Fatalf("trigger request = %+v (input %s)", req, req.Input)
	}
	got := f.get(s.ID)
	if !got.Enabled || got.NextRunAt == nil || !got.NextRunAt.Equal(at(t, "2001-01-02T09:00:00Z")) {
		t.Fatalf("after fire: enabled %v next %v", got.Enabled, got.NextRunAt)
	}
	if got.LastRunID == nil || *got.LastRunID != fired[0].Run.ID || got.LastRunAt == nil || !got.LastRunAt.Equal(f.clock.Now()) || got.LastError != nil {
		t.Fatalf("after fire: last run %v at %v error %v", got.LastRunID, got.LastRunAt, got.LastError)
	}
	if n := f.get(notDue.ID); n.LastRunID != nil {
		t.Fatal("a schedule that was not due fired")
	}
	// SSE schedule.fired {schedule, run}, filed under the run.
	if len(f.events) != 1 || f.events[0].typ != "schedule.fired" || f.events[0].runID != fired[0].Run.ID {
		t.Fatalf("events = %+v", f.events)
	}
	if sc, ok := f.events[0].payload["schedule"].(store.Schedule); !ok || sc.ID != s.ID || sc.NextRunAt == nil {
		t.Fatalf("event schedule = %#v", f.events[0].payload["schedule"])
	}
	if _, ok := f.events[0].payload["run"].(*store.Run); !ok {
		t.Fatalf("event run = %#v", f.events[0].payload["run"])
	}
	// Exactly once: the same instant again fires nothing.
	if again := f.tick(); len(again) != 0 || f.trig.count() != 1 {
		t.Fatalf("second tick fired %d, triggers %d", len(again), f.trig.count())
	}
}

// T5.9.2: a delay schedule fires once and disables itself.
func TestTickDelayFiresOnce(t *testing.T) {
	f := newFixture(t, at(t, "2001-02-01T12:00:00Z"))
	s := f.schedule(store.ScheduleDelay, "", "UTC", at(t, "2001-02-01T11:59:58Z"))
	if fired := f.tick(); len(fired) != 1 {
		t.Fatalf("fired %d, want 1", len(fired))
	}
	got := f.get(s.ID)
	if got.Enabled || got.NextRunAt != nil || got.LastRunID == nil {
		t.Fatalf("after fire: enabled %v next %v last run %v", got.Enabled, got.NextRunAt, got.LastRunID)
	}
	f.clock.Advance(time.Hour)
	if fired := f.tick(); len(fired) != 0 || f.trig.count() != 1 {
		t.Fatalf("fired again: %d, triggers %d", len(fired), f.trig.count())
	}
}

// T5.9.3: down across three occurrences, the scheduler creates one run on restart and
// computes the next occurrence from now, not from the missed ones.
func TestTickNoCatchUp(t *testing.T) {
	f := newFixture(t, at(t, "2001-03-04T10:30:00Z"))
	s := f.schedule(store.ScheduleCron, "0 9 * * *", "UTC", at(t, "2001-03-01T09:00:00Z")) // missed 1st, 2nd, 3rd, 4th
	if fired := f.tick(); len(fired) != 1 {
		t.Fatalf("fired %d, want exactly 1", len(fired))
	}
	if fired := f.tick(); len(fired) != 0 {
		t.Fatalf("second tick fired %d", len(fired))
	}
	got := f.get(s.ID)
	if got.NextRunAt == nil || !got.NextRunAt.Equal(at(t, "2001-03-05T09:00:00Z")) {
		t.Fatalf("next = %v, want the first occurrence after now", got.NextRunAt)
	}
	if f.trig.count() != 1 {
		t.Fatalf("triggers = %d", f.trig.count())
	}
}

// T5.9.5: a schedule whose workflow was deleted is disabled at the next tick with
// last_error "workflow deleted", and nothing is triggered.
func TestTickDeletedWorkflow(t *testing.T) {
	f := newFixture(t, at(t, "2001-04-01T09:00:00Z"))
	s := f.schedule(store.ScheduleCron, "*/5 * * * *", "UTC", at(t, "2001-04-01T08:55:00Z"))
	if err := store.DeleteWorkflow(context.Background(), f.pool, f.wf.ID); err != nil {
		t.Fatal(err)
	}
	if fired := f.tick(); len(fired) != 0 {
		t.Fatalf("fired %d", len(fired))
	}
	got := f.get(s.ID)
	if got.Enabled || got.NextRunAt != nil || got.LastError == nil || *got.LastError != "workflow deleted" {
		t.Fatalf("after tick: enabled %v next %v error %v", got.Enabled, got.NextRunAt, got.LastError)
	}
	if f.trig.count() != 0 {
		t.Fatalf("triggered %d", f.trig.count())
	}
}

// Rule 4 when the deletion races the tick: the engine's not-found disables it too.
func TestTickWorkflowNotFoundAtTrigger(t *testing.T) {
	f := newFixture(t, at(t, "2001-04-02T09:00:00Z"))
	s := f.schedule(store.ScheduleCron, "0 * * * *", "UTC", at(t, "2001-04-02T09:00:00Z"))
	f.trig.err = fmt.Errorf("workflow %s: %w", f.wf.ID, store.ErrNotFound)
	f.tick()
	if got := f.get(s.ID); got.Enabled || got.LastError == nil || *got.LastError != ErrorWorkflowDeleted {
		t.Fatalf("after tick: enabled %v error %v", got.Enabled, got.LastError)
	}
}

// Failure handling: a run the engine refuses (422 or a guardrail's 429) records
// last_error and still advances, so a broken workflow does not fire every tick; any
// other failure keeps the occurrence due for the next tick.
func TestTickRefusedRun(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		wantError string
		advances  bool
	}{
		{"validation", &engine.ValidationError{Messages: []string{"input.topic: missing property"}}, "the workflow cannot run: input.topic: missing property", true},
		{"rate limit", &guardrails.RateLimitError{AgentName: "Coder", Limit: 2}, "rate limit: 2 runs/hour for agent Coder", true},
		{"transient", errors.New("connection reset"), "the run could not be created; retrying", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, at(t, "2001-05-01T09:00:30Z"))
			due := at(t, "2001-05-01T09:00:00Z")
			s := f.schedule(store.ScheduleCron, "0 9 * * *", "UTC", due)
			f.trig.err = tc.err
			if fired := f.tick(); len(fired) != 0 {
				t.Fatalf("fired %d", len(fired))
			}
			got := f.get(s.ID)
			if got.LastError == nil || *got.LastError != tc.wantError {
				t.Fatalf("last_error = %v, want %q", got.LastError, tc.wantError)
			}
			wantNext := due
			if tc.advances {
				wantNext = at(t, "2001-05-02T09:00:00Z")
			}
			if !got.Enabled || got.NextRunAt == nil || !got.NextRunAt.Equal(wantNext) || got.LastRunID != nil {
				t.Fatalf("enabled %v next %v (want %s) last run %v", got.Enabled, got.NextRunAt, wantNext, got.LastRunID)
			}
			if len(f.events) != 0 {
				t.Fatalf("events for a refused run: %+v", f.events)
			}
			// A later success clears the error.
			f.trig.err = nil
			f.clock.Advance(24 * time.Hour)
			if fired := f.tick(); len(fired) != 1 {
				t.Fatalf("recovery fired %d", len(fired))
			}
			if got := f.get(s.ID); got.LastError != nil {
				t.Fatalf("last_error after success = %q", *got.LastError)
			}
		})
	}
}

// Rule 2: a row another transaction holds is skipped, never fired twice.
func TestTickSkipsLockedRows(t *testing.T) {
	f := newFixture(t, at(t, "2001-06-01T09:00:00Z"))
	s := f.schedule(store.ScheduleCron, "0 9 * * *", "UTC", at(t, "2001-06-01T09:00:00Z"))
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // releases the connection when the test fails early
	locked, err := store.LockDueSchedules(ctx, tx, f.clock.Now(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(locked) != 1 || locked[0].ID != s.ID {
		t.Fatalf("locked %+v", locked)
	}
	if fired := f.tick(); len(fired) != 0 {
		t.Fatalf("fired a row another transaction holds: %d", len(fired))
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if fired := f.tick(); len(fired) != 1 {
		t.Fatalf("after release fired %d", len(fired))
	}
}

// The zone decides when a cron schedule fires (T5.9.6 through the tick).
func TestTickAdvancesInZone(t *testing.T) {
	f := newFixture(t, at(t, "2001-07-02T12:00:10Z"))
	s := f.schedule(store.ScheduleCron, "0 9 * * *", "America/Sao_Paulo", at(t, "2001-07-02T12:00:00Z"))
	f.tick()
	// Brazil kept DST until 2019, but July is winter there: UTC-3.
	if got := f.get(s.ID); got.NextRunAt == nil || !got.NextRunAt.Equal(at(t, "2001-07-03T12:00:00Z")) {
		t.Fatalf("next = %v", got.NextRunAt)
	}
}

// A cron row whose expression names no date to come (written before validation refused
// it, so next_run_at holds the zero time) is disabled with the reason at its first tick
// instead of firing every tick.
func TestTickDisablesCronWithNoFutureOccurrence(t *testing.T) {
	f := newFixture(t, at(t, "2001-10-01T09:00:00Z"))
	s := f.schedule(store.ScheduleCron, "0 0 31 2 *", "UTC", time.Time{})
	if fired := f.tick(); len(fired) != 0 || f.trig.count() != 0 {
		t.Fatalf("fired %d, triggers %d", len(fired), f.trig.count())
	}
	got := f.get(s.ID)
	if got.Enabled || got.NextRunAt != nil || got.LastError == nil || *got.LastError != "invalid schedule: no future occurrence" {
		t.Fatalf("after tick: enabled %v next %v error %v", got.Enabled, got.NextRunAt, got.LastError)
	}
	if fired := f.tick(); len(fired) != 0 || f.trig.count() != 0 {
		t.Fatalf("fired again: %d", len(fired))
	}
}

// Run ticks at start and stops with its context.
func TestRunStopsWithContext(t *testing.T) {
	f := newFixture(t, at(t, "2001-08-01T09:00:00Z"))
	f.schedule(store.ScheduleDelay, "", "UTC", at(t, "2001-08-01T08:00:00Z"))
	f.s.Tick = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.s.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for f.trig.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
	if f.trig.count() != 1 {
		t.Fatalf("triggers = %d, want the first tick's one", f.trig.count())
	}
}

// T5.9.7: the run is created on the engine's connection, outside the schedule's
// transaction. When the schedule update is lost after the run was created (a crash or a
// failed commit between the two), the next tick fires the same occurrence again: the
// engine finds its run, no second run is created, and the schedule advances with that
// run as its last one and no error.
func TestTickRefiresLostOccurrenceOnce(t *testing.T) {
	f := newFixture(t, at(t, "2001-09-01T09:00:10Z"))
	f.s.Engine = engine.New(engine.Deps{Pool: f.pool, Cfg: engineConfig(), Clock: f.clock})
	ctx := context.Background()
	t.Cleanup(func() { _, _ = f.pool.Exec(ctx, `DELETE FROM runs WHERE workflow_id = $1`, f.wf.ID) })
	due := at(t, "2001-09-01T09:00:00Z")
	s := f.schedule(store.ScheduleCron, "0 9 * * *", "UTC", due)
	ref := s.ID + "@2001-09-01T09:00:00Z"

	fired := f.tick()
	if len(fired) != 1 || fired[0].Run.TriggerRef == nil || *fired[0].Run.TriggerRef != ref {
		t.Fatalf("first tick fired %+v", fired)
	}
	// The state a rolled-back schedule update leaves: the run exists, the schedule is
	// still due at the same occurrence.
	if _, err := f.pool.Exec(ctx, `UPDATE schedules SET next_run_at = $2, last_run_at = NULL, last_run_id = NULL,
		last_error = NULL WHERE id = $1`, s.ID, due); err != nil {
		t.Fatal(err)
	}

	if again := f.tick(); len(again) != 0 {
		t.Fatalf("the occurrence fired twice: %+v", again)
	}
	var runs int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM runs WHERE workflow_id = $1`, f.wf.ID).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 1 {
		t.Fatalf("runs = %d, want 1", runs)
	}
	got := f.get(s.ID)
	if !got.Enabled || got.NextRunAt == nil || !got.NextRunAt.Equal(at(t, "2001-09-02T09:00:00Z")) || got.LastError != nil {
		t.Fatalf("after the re-fire: enabled %v next %v error %v", got.Enabled, got.NextRunAt, got.LastError)
	}
	if got.LastRunID == nil || *got.LastRunID != fired[0].Run.ID {
		t.Fatalf("last run = %v, want the occurrence's run %s", got.LastRunID, fired[0].Run.ID)
	}
}

// engineConfig is a platform configuration the real engine accepts a trigger under.
func engineConfig() config.Config {
	return config.Config{MaxSteps: 200, MaxDurationSeconds: 3600, MaxNodeIterations: 50, MaxLoopVisits: 10,
		MaxCostUSD: 20, MaxToolCalls: 300, ApprovalTimeoutMaxSec: 604800}
}

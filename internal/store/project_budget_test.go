package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// DESIGN 5.18 rule 1, 5.19 rule 1: the instructions and the two caps are stored with the
// other settings, read back with the project, replaced by an update, and cleared by one
// that leaves them out; a deleted project's instructions can still be read by id.
func TestProjectInstructionsAndCapsRoundTrip(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	day, month := USDMicros(5_000_000), USDMicros(1_000_000_000_000)
	p := &Project{Name: "Capped", RepoURL: "https://gitlab.example/g/capped.git", Provider: "gitlab", DefaultBranch: "main",
		Instructions: "Use tabs.\nNever edit vendor/.", BudgetDayUSDMicros: &day, BudgetMonthUSDMicros: &month}
	mustDo(t, CreateProject(ctx, pool, p, nil))
	got, err := GetProject(ctx, pool, p.ID)
	mustDo(t, err)
	if got.Instructions != p.Instructions || got.BudgetDayUSDMicros == nil || *got.BudgetDayUSDMicros != 5_000_000 ||
		got.BudgetMonthUSDMicros == nil || *got.BudgetMonthUSDMicros != 1_000_000_000_000 {
		t.Fatalf("read back = %+v", got)
	}
	if b, _ := json.Marshal(got); !strings.Contains(string(b), `"max_cost_usd_per_day":5`) || !strings.Contains(string(b), `"max_cost_usd_per_month":1000000`) ||
		!strings.Contains(string(b), `"instructions":"Use tabs.\nNever edit vendor/."`) {
		t.Fatalf("JSON = %s", b)
	}
	p.Instructions, p.BudgetDayUSDMicros = "Only src/.", nil
	mustDo(t, UpdateProject(ctx, pool, p))
	got, _ = GetProject(ctx, pool, p.ID)
	if got.Instructions != "Only src/." || got.BudgetDayUSDMicros != nil || got.BudgetMonthUSDMicros == nil {
		t.Fatalf("after update = %+v", got)
	}
	if b, _ := json.Marshal(got); strings.Contains(string(b), "max_cost_usd_per_day") {
		t.Fatalf("a cleared cap is in the JSON: %s", b)
	}

	mustDo(t, DeleteProject(ctx, pool, p.ID))
	if text, err := ProjectInstructions(ctx, pool, p.ID); err != nil || text != "Only src/." {
		t.Fatalf("instructions of a deleted project = %q %v", text, err)
	}
	if text, err := ProjectInstructions(ctx, pool, "no-such-project"); err != nil || text != "" {
		t.Fatalf("instructions of a missing project = %q %v", text, err)
	}
}

// DESIGN 5.19 units: the UTC day and month around an instant, end exclusive, and the
// sums over them count the edges the right way round.
func TestBudgetBoundsAndSums(t *testing.T) {
	now := mustParse(t, "2026-12-31T23:59:59.5Z")
	ds, de := DayBounds(now)
	ms, me := MonthBounds(now)
	if !ds.Equal(mustParse(t, "2026-12-31T00:00:00Z")) || !de.Equal(mustParse(t, "2027-01-01T00:00:00Z")) ||
		!ms.Equal(mustParse(t, "2026-12-01T00:00:00Z")) || !me.Equal(mustParse(t, "2027-01-01T00:00:00Z")) {
		t.Fatalf("bounds = %v %v %v %v", ds, de, ms, me)
	}
	ds, _ = DayBounds(time.Date(2026, 10, 18, 1, 0, 0, 0, time.FixedZone("p5", 5*3600)))
	if !ds.Equal(mustParse(t, "2026-10-17T00:00:00Z")) {
		t.Fatalf("day of a +05:00 instant = %v", ds)
	}

	pool := needDB(t)
	ctx := context.Background()
	p := &Project{Name: "Spender", RepoURL: "https://gitlab.example/g/s.git", Provider: "gitlab", DefaultBranch: "main"}
	mustDo(t, CreateProject(ctx, pool, p, nil))
	wf := &Workflow{Name: "wf", Definition: json.RawMessage(`{"nodes":[],"edges":[]}`)}
	mustDo(t, CreateWorkflow(ctx, pool, wf))
	add := func(at string, micros int64) {
		t.Helper()
		settings := p.Settings()
		r := &Run{WorkflowID: wf.ID, WorkflowVersion: 1, WorkflowSnapshot: wf.Definition, Trigger: TriggerManual, ProjectID: &p.ID, Project: &settings}
		mustDo(t, CreateRun(ctx, pool, r))
		_, err := pool.Exec(ctx, `UPDATE runs SET created_at=$2, cost_usd_micros=$3 WHERE id=$1`, r.ID, mustParse(t, at), micros)
		mustDo(t, err)
	}
	add("2026-12-31T00:00:00Z", 1)             // first instant of the day
	add("2026-12-31T23:59:59.999999Z", 10)     // last microsecond
	add("2027-01-01T00:00:00Z", 100)           // the next day, the next month
	add("2026-12-30T23:59:59.999999Z", 1000)   // the day before
	add("2026-12-01T00:00:00Z", 10000)         // first instant of the month
	add("2026-11-30T23:59:59.999999Z", 100000) // the month before
	sum := func(from, to string) int64 {
		t.Helper()
		n, err := SumProjectSpend(ctx, pool, p.ID, mustParse(t, from), mustParse(t, to))
		mustDo(t, err)
		return n
	}
	if got := sum("2026-12-31T00:00:00Z", "2027-01-01T00:00:00Z"); got != 11 {
		t.Errorf("day = %d, want 11", got)
	}
	if got := sum("2026-12-01T00:00:00Z", "2027-01-01T00:00:00Z"); got != 11011 {
		t.Errorf("month = %d, want 11011", got)
	}
	if got := sum("2026-12-31T00:00:00Z", "2026-12-31T00:00:00Z"); got != 0 {
		t.Errorf("an empty window = %d", got)
	}
}

// DESIGN 5.19 rule 5: the alert ledger records a level once per window; the key is the
// project, the window kind, its start and the level.
func TestBudgetAlertLedger(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	p := &Project{Name: "Alerted", RepoURL: "https://gitlab.example/g/a.git", Provider: "gitlab", DefaultBranch: "main"}
	mustDo(t, CreateProject(ctx, pool, p, nil))
	start := mustParse(t, "2026-10-17T00:00:00Z")
	if got, err := SentBudgetAlerts(ctx, pool, p.ID, BudgetWindowDay, start); err != nil || len(got) != 0 {
		t.Fatalf("empty ledger = %v %v", got, err)
	}
	mustDo(t, RecordBudgetAlert(ctx, pool, p.ID, BudgetWindowDay, start, BudgetAlertWarning, start.Add(time.Hour)))
	mustDo(t, RecordBudgetAlert(ctx, pool, p.ID, BudgetWindowDay, start, BudgetAlertWarning, start.Add(2*time.Hour))) // a repeat is a no-op
	mustDo(t, RecordBudgetAlert(ctx, pool, p.ID, BudgetWindowMonth, start, BudgetAlertReached, start.Add(time.Hour)))
	mustDo(t, RecordBudgetAlert(ctx, pool, p.ID, BudgetWindowDay, start.Add(24*time.Hour), BudgetAlertWarning, start.Add(25*time.Hour)))
	if got, err := SentBudgetAlerts(ctx, pool, p.ID, BudgetWindowDay, start); err != nil || len(got) != 1 || !got[BudgetAlertWarning] {
		t.Fatalf("day ledger = %v %v", got, err)
	}
	if got, _ := SentBudgetAlerts(ctx, pool, p.ID, BudgetWindowMonth, start); len(got) != 1 || !got[BudgetAlertReached] {
		t.Fatalf("month ledger = %v", got)
	}
	var n int
	mustDo(t, pool.QueryRow(ctx, `SELECT count(*) FROM project_budget_alerts WHERE project_id=$1`, p.ID).Scan(&n))
	if n != 3 {
		t.Fatalf("rows = %d, want 3", n)
	}
}

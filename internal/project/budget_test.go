package project

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"yuno/internal/store"
)

func at(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func usd(v float64) *store.USDMicros {
	m := store.USDMicros(v * 1e6)
	return &m
}

// T5.19.2 (windows): the UTC calendar day and month that contain now, end exclusive,
// whatever zone now carries.
func TestWindows(t *testing.T) {
	day, month := Windows(at(t, "2026-10-17T13:45:10Z"))
	if day.Kind != "day" || !day.Start.Equal(at(t, "2026-10-17T00:00:00Z")) || !day.End.Equal(at(t, "2026-10-18T00:00:00Z")) {
		t.Fatalf("day = %+v", day)
	}
	if month.Kind != "month" || !month.Start.Equal(at(t, "2026-10-01T00:00:00Z")) || !month.End.Equal(at(t, "2026-11-01T00:00:00Z")) {
		t.Fatalf("month = %+v", month)
	}
	// The first and the last instant of a day belong to it; the next instant does not.
	for _, c := range []struct{ now, dayStart, monthEnd string }{
		{"2026-10-17T00:00:00Z", "2026-10-17T00:00:00Z", "2026-11-01T00:00:00Z"},
		{"2026-10-17T23:59:59.999999999Z", "2026-10-17T00:00:00Z", "2026-11-01T00:00:00Z"},
		{"2026-10-18T00:00:00Z", "2026-10-18T00:00:00Z", "2026-11-01T00:00:00Z"},
		{"2026-12-31T23:59:59Z", "2026-12-31T00:00:00Z", "2027-01-01T00:00:00Z"}, // year end
		{"2028-02-29T10:00:00Z", "2028-02-29T00:00:00Z", "2028-03-01T00:00:00Z"}, // leap day
	} {
		d, m := Windows(at(t, c.now))
		if !d.Start.Equal(at(t, c.dayStart)) || !d.End.Equal(d.Start.AddDate(0, 0, 1)) || !m.End.Equal(at(t, c.monthEnd)) {
			t.Errorf("now %s: day %v to %v, month ends %v", c.now, d.Start, d.End, m.End)
		}
	}
	// 01:30 on the 18th in UTC+5 is 20:30 on the 17th UTC: the day is the UTC one.
	zone := time.FixedZone("plus5", 5*3600)
	d, _ := Windows(time.Date(2026, 10, 18, 1, 30, 0, 0, zone))
	if !d.Start.Equal(at(t, "2026-10-17T00:00:00Z")) || d.Start.Location() != time.UTC {
		t.Fatalf("day for a +05:00 instant = %v", d.Start)
	}
}

// fixture is a project with a workflow to hang runs on.
type fixture struct {
	t    *testing.T
	pool *pgxpool.Pool
	wf   *store.Workflow
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := needDB(t)
	wf := &store.Workflow{Name: "budget test", Definition: json.RawMessage(`{"nodes":[],"edges":[]}`)}
	if err := store.CreateWorkflow(context.Background(), pool, wf); err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, pool: pool, wf: wf}
}

// project stores a live project with the given caps.
func (f *fixture) project(name string, day, month *store.USDMicros) *store.Project {
	f.t.Helper()
	p := &store.Project{Name: name + " " + strings.ReplaceAll(store.NewID(), "-", "")[20:], RepoURL: "https://gitlab.test/g/" + name + ".git",
		Provider: "gitlab", DefaultBranch: "main", BudgetDayUSDMicros: day, BudgetMonthUSDMicros: month}
	if err := store.CreateProject(context.Background(), f.pool, p, nil); err != nil {
		f.t.Fatal(err)
	}
	return p
}

// run stores a run of the project (nil: none) created at createdAt that cost micros.
func (f *fixture) run(p *store.Project, createdAt time.Time, micros int64, status string) *store.Run {
	f.t.Helper()
	r := &store.Run{WorkflowID: f.wf.ID, WorkflowVersion: 1, WorkflowSnapshot: f.wf.Definition, Trigger: store.TriggerManual}
	if p != nil {
		settings := p.Settings()
		r.ProjectID, r.Project = &p.ID, &settings
	}
	ctx := context.Background()
	if err := store.CreateRun(ctx, f.pool, r); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE runs SET created_at=$2, cost_usd_micros=$3, status=$4 WHERE id=$1`, r.ID, createdAt, micros, status); err != nil {
		f.t.Fatal(err)
	}
	return r
}

func sumDay(t *testing.T, pool *pgxpool.Pool, p *store.Project, now time.Time) int64 {
	t.Helper()
	d, _ := Windows(now)
	n, err := store.SumProjectSpend(context.Background(), pool, p.ID, d.Start, d.End)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func sumMonth(t *testing.T, pool *pgxpool.Pool, p *store.Project, now time.Time) int64 {
	t.Helper()
	_, m := Windows(now)
	n, err := store.SumProjectSpend(context.Background(), pool, p.ID, m.Start, m.End)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// T5.19.2: the spend sums only the project's runs, in the window, whatever their
// status, on both edges of a day and of a month; the runs of a deleted project keep
// counting.
func TestSpendWindowEdges(t *testing.T) {
	f := newFixture(t)
	now := at(t, "2026-10-17T12:00:00Z")
	p, other := f.project("alpha", nil, nil), f.project("beta", nil, nil)

	f.run(p, at(t, "2026-10-17T00:00:00Z"), 1_000_000, store.RunSucceeded)        // first instant of the day: in
	f.run(p, at(t, "2026-10-17T23:59:59.999999Z"), 2_000_000, store.RunRunning)   // last microsecond: in, still running
	f.run(p, at(t, "2026-10-18T00:00:00Z"), 4_000_000, store.RunFailed)           // the next day: out of the day, in the month
	f.run(p, at(t, "2026-10-16T23:59:59.999999Z"), 8_000_000, store.RunSucceeded) // yesterday: out of the day, in the month
	f.run(p, at(t, "2026-10-01T00:00:00Z"), 16_000_000, store.RunCancelled)       // first instant of the month: in
	f.run(p, at(t, "2026-09-30T23:59:59.999999Z"), 32_000_000, store.RunSucceeded)
	f.run(p, at(t, "2026-11-01T00:00:00Z"), 64_000_000, store.RunQueued) // next month: out
	f.run(other, at(t, "2026-10-17T10:00:00Z"), 128_000_000, store.RunSucceeded)
	f.run(nil, at(t, "2026-10-17T10:00:00Z"), 256_000_000, store.RunSucceeded)

	if got := sumDay(t, f.pool, p, now); got != 3_000_000 {
		t.Errorf("day spend = %d, want the two runs of the 17th", got)
	}
	// October: the 17th (3) + the 18th (4) + the 16th (8) + the 1st (16).
	if got := sumMonth(t, f.pool, p, now); got != 31_000_000 {
		t.Errorf("month spend = %d, want 31000000", got)
	}
	if got := sumDay(t, f.pool, other, now); got != 128_000_000 {
		t.Errorf("another project's spend = %d: the sum must not mix projects", got)
	}
	// The list view: two queries, both windows, the end of each.
	list := []store.Project{*p, *other}
	if err := store.FillProjectSpend(context.Background(), f.pool, list, now); err != nil {
		t.Fatal(err)
	}
	if s := list[0].Spend; s == nil || s.DayUSDMicros != 3_000_000 || s.MonthUSDMicros != 31_000_000 ||
		!s.DayUntil.Equal(at(t, "2026-10-18T00:00:00Z")) || !s.MonthUntil.Equal(at(t, "2026-11-01T00:00:00Z")) {
		t.Errorf("spend of alpha = %+v", list[0].Spend)
	}
	if s := list[1].Spend; s == nil || s.DayUSDMicros != 128_000_000 || s.MonthUSDMicros != 128_000_000 {
		t.Errorf("spend of beta = %+v", list[1].Spend)
	}
	// A project with no runs in the window spends zero, not absent.
	idle := f.project("idle", nil, nil)
	one := []store.Project{*idle}
	if err := store.FillProjectSpend(context.Background(), f.pool, one, now); err != nil || one[0].Spend == nil || one[0].Spend.DayUSDMicros != 0 {
		t.Errorf("idle spend = %+v %v", one[0].Spend, err)
	}

	if err := store.DeleteProject(context.Background(), f.pool, p.ID); err != nil {
		t.Fatal(err)
	}
	if got := sumDay(t, f.pool, p, now); got != 3_000_000 {
		t.Errorf("day spend after the project was deleted = %d: its runs keep counting", got)
	}
}

// T5.19.2/T5.19.3 (check): spend one micro under the cap passes, at the cap refuses,
// over it refuses; the day cap is checked before the month cap; the error carries the
// numbers and reads as rule 3 spells it.
func TestCheckBudget(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	now := at(t, "2026-10-17T12:00:00Z")

	if err := CheckBudget(ctx, f.pool, f.project("free", nil, nil), now); err != nil {
		t.Fatalf("no cap: %v", err)
	}
	if err := CheckBudget(ctx, f.pool, nil, now); err != nil {
		t.Fatalf("no project: %v", err)
	}

	p := f.project("Alpha", usd(5), usd(20))
	check := func() *BudgetError {
		t.Helper()
		err := CheckBudget(ctx, f.pool, p, now)
		if err == nil {
			return nil
		}
		var be *BudgetError
		if !errors.As(err, &be) {
			t.Fatalf("not a budget error: %v", err)
		}
		return be
	}
	r := f.run(p, at(t, "2026-10-17T08:00:00Z"), 5_000_000-1, store.RunRunning) // a running run counts what it spent so far
	cost := func(micros int64) {
		t.Helper()
		if _, err := f.pool.Exec(ctx, `UPDATE runs SET cost_usd_micros = $2 WHERE id = $1`, r.ID, micros); err != nil {
			t.Fatal(err)
		}
	}
	if be := check(); be != nil {
		t.Fatalf("cap - 1: refused: %v", be)
	}
	cost(5_000_000)
	be := check()
	if be == nil {
		t.Fatal("spend exactly at the cap must refuse")
	}
	if be.Window != "day" || be.ProjectID != p.ID || be.ProjectName != p.Name || be.CapMicros != 5_000_000 || be.SpendMicros != 5_000_000 ||
		!be.WindowEnd.Equal(at(t, "2026-10-18T00:00:00Z")) {
		t.Fatalf("budget error = %+v", be)
	}
	want := "project " + p.Name + " has reached its daily budget ($5.00); new runs are refused until 2026-10-18T00:00:00Z"
	if be.Error() != want {
		t.Fatalf("text = %q\nwant   %q", be.Error(), want)
	}
	cost(5_000_001)
	if be := check(); be == nil || be.SpendMicros != 5_000_001 {
		t.Fatalf("cap + 1: %+v", be)
	}

	// The month cap alone: yesterday's runs count in the month, not in the day. Both
	// caps reached: the day's error comes first.
	cost(0)
	f.run(p, at(t, "2026-10-03T08:00:00Z"), 20_000_000-1, store.RunSucceeded)
	if be := check(); be != nil {
		t.Fatalf("month cap - 1: refused: %v", be)
	}
	f.run(p, at(t, "2026-10-04T08:00:00Z"), 1, store.RunFailed)
	be = check()
	if be == nil || be.Window != "month" || be.CapMicros != 20_000_000 || be.SpendMicros != 20_000_000 || !be.WindowEnd.Equal(at(t, "2026-11-01T00:00:00Z")) {
		t.Fatalf("month cap reached: %+v", be)
	}
	if be.Error() != "project "+p.Name+" has reached its monthly budget ($20.00); new runs are refused until 2026-11-01T00:00:00Z" {
		t.Fatalf("monthly text = %q", be.Error())
	}
	cost(5_000_000)
	if be := check(); be == nil || be.Window != "day" {
		t.Fatalf("both caps reached: %+v, want the day first", be)
	}

	// The next day the day window is empty again: only the month cap is left to refuse.
	next := at(t, "2026-10-18T00:00:00Z")
	if err := CheckBudget(ctx, f.pool, p, next); err == nil || !strings.Contains(err.Error(), "monthly") {
		t.Fatalf("next day: %v", err)
	}

	// A failed spend query fails the check; it never silently allows.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	err := CheckBudget(cancelled, f.pool, p, now)
	var be2 *BudgetError
	if err == nil || errors.As(err, &be2) {
		t.Fatalf("a failed query = %v, want a plain error", err)
	}
}

func TestFormatUSD(t *testing.T) {
	for micros, want := range map[int64]string{0: "0.00", 5_000_000: "5.00", 1_500_000: "1.50", 4_123_456: "4.12", 9_999: "0.0100", 1_500: "0.0015", 1_000_000_000_000: "1000000.00"} {
		if got := FormatUSD(micros); got != want {
			t.Errorf("FormatUSD(%d) = %q, want %q", micros, got, want)
		}
	}
}

// ---- alerts (5.19 rules 5 and 6) ------------------------------------------------------

type sentMessage struct{ conversation, key, text string }

// fakeReporter records what the watcher sends; deferred makes it answer "deferred".
type fakeReporter struct {
	mu       sync.Mutex
	sent     []sentMessage
	attempts int
	deferred bool
}

func (r *fakeReporter) SendPlatformMessage(_ context.Context, runID, conversationID, key, text string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attempts++
	if runID != "" {
		panic("a budget alert belongs to no run")
	}
	if r.deferred {
		return true
	}
	r.sent = append(r.sent, sentMessage{conversationID, key, text})
	return false
}

func (r *fakeReporter) messages() []sentMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]sentMessage(nil), r.sent...)
}

func (r *fakeReporter) texts() []string {
	var out []string
	for _, m := range r.messages() {
		out = append(out, m.text)
	}
	return out
}

// clock is the watcher's injected clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// alertEnv is a watcher on an empty alert setup: no binding, no conversation.
type alertEnv struct {
	*fixture
	clock    *clock
	reporter *fakeReporter
	logs     *bytes.Buffer
}

func newAlertEnv(t *testing.T) *alertEnv {
	t.Helper()
	f := newFixture(t)
	if _, err := f.pool.Exec(context.Background(), `TRUNCATE channel_bindings, conversations, project_budget_alerts CASCADE`); err != nil {
		t.Fatal(err)
	}
	return &alertEnv{fixture: f, clock: &clock{t: at(t, "2026-10-17T12:00:00Z")}, reporter: &fakeReporter{}, logs: &bytes.Buffer{}}
}

func (e *alertEnv) watcher() *Watcher {
	return &Watcher{Pool: e.pool, Tick: time.Hour, Now: e.clock.Now, Reporter: e.reporter,
		Log: slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelInfo}))}
}

// bind binds a new agent to Telegram with one conversation holding a message sent at
// lastMessage, and returns the conversation id.
func (e *alertEnv) bind() (agentID, conversationID string) {
	e.t.Helper()
	ctx := context.Background()
	a := &store.Agent{Name: "Chief " + store.NewID(), Model: "claude-opus-5-5"}
	if err := store.CreateAgent(ctx, e.pool, a); err != nil {
		e.t.Fatal(err)
	}
	if _, err := store.BindChannel(ctx, e.pool, store.ChannelTelegram, a.ID); err != nil {
		e.t.Fatal(err)
	}
	return a.ID, e.conversation(a.ID, "1", "2026-10-17T09:00:00Z")
}

// conversation stores a conversation of the agent whose newest message is at lastMessage.
func (e *alertEnv) conversation(agentID, chat, lastMessage string) string {
	e.t.Helper()
	ctx := context.Background()
	c, err := store.UpsertConversation(ctx, e.pool, &store.Conversation{Channel: store.ChannelTelegram, ExternalChatID: chat, AgentID: agentID,
		SessionKey: "agent:" + agentID + ":telegram:direct:" + chat})
	if err != nil {
		e.t.Fatal(err)
	}
	m := &store.ConversationMessage{ConversationID: c.ID, Direction: store.DirectionIn, ExternalMessageID: "m1", Text: "hi",
		CreatedAt: at(e.t, lastMessage)}
	if _, err := store.InsertConversationMessage(ctx, e.pool, m); err != nil {
		e.t.Fatal(err)
	}
	return c.ID
}

func (e *alertEnv) rows(p *store.Project) []string {
	e.t.Helper()
	rows, err := e.pool.Query(context.Background(), `SELECT window_kind || ' ' || to_char(window_start AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"') || ' ' || level
		FROM project_budget_alerts WHERE project_id = $1 ORDER BY window_kind, window_start, level`, p.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func eq(t *testing.T, what string, got, want []string) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%s = %q\nwant %q", what, got, want)
	}
}

// T5.19.5: one warning and one reached alert per window start; nothing below 80 %; a
// tick again, or after a restart, re-sends nothing; the next day sends again. The texts
// are rule 5's, the key is rule 6's.
func TestWatcherSendsEachLevelOncePerWindow(t *testing.T) {
	e := newAlertEnv(t)
	_, conv := e.bind()
	p := e.project("Alpha", usd(10), usd(1000))
	w := e.watcher()

	e.run(p, at(t, "2026-10-17T06:00:00Z"), 7_900_000, store.RunSucceeded) // 79 %
	if n := w.TickOnce(context.Background()); n != 0 || len(e.rows(p)) != 0 {
		t.Fatalf("79 %% sent %d alerts, rows %v", n, e.rows(p))
	}
	e.run(p, at(t, "2026-10-17T07:00:00Z"), 100_000, store.RunRunning) // 80 % exactly
	if n := w.TickOnce(context.Background()); n != 1 {
		t.Fatalf("80 %% sent %d alerts, want the warning", n)
	}
	eq(t, "texts", e.reporter.texts(), []string{"[platform] Project " + p.Name + " has used $8.00 of its $10.00 daily budget (80 %)."})
	if m := e.reporter.messages()[0]; m.conversation != conv || m.key != "budget-"+p.ID+"-day-2026-10-17T00:00:00Z-warning" {
		t.Fatalf("destination and key = %+v", m)
	}
	if n := w.TickOnce(context.Background()); n != 0 {
		t.Fatalf("second tick sent %d", n)
	}
	e.run(p, at(t, "2026-10-17T08:00:00Z"), 1_999_999, store.RunSucceeded) // one micro under the cap
	if n := w.TickOnce(context.Background()); n != 0 {
		t.Fatalf("cap - 1 sent %d: the warning was sent already and the cap is not reached", n)
	}
	e.run(p, at(t, "2026-10-17T09:00:00Z"), 1, store.RunSucceeded) // at the cap
	if n := w.TickOnce(context.Background()); n != 1 {
		t.Fatalf("at the cap sent %d, want reached", n)
	}
	wantReached := "[platform] Project " + p.Name + " has reached its $10.00 daily budget; new runs on it are refused until 2026-10-18T00:00:00Z."
	eq(t, "texts", e.reporter.texts()[1:], []string{wantReached})
	if got := e.reporter.messages()[1].key; got != "budget-"+p.ID+"-day-2026-10-17T00:00:00Z-reached" {
		t.Fatalf("reached key = %q", got)
	}
	eq(t, "ledger", e.rows(p), []string{"day 2026-10-17T00:00:00Z reached", "day 2026-10-17T00:00:00Z warning"})

	// More spend, more ticks, and a restart (a new watcher): nothing is sent twice.
	e.run(p, at(t, "2026-10-17T10:00:00Z"), 3_000_000, store.RunRunning)
	if n := w.TickOnce(context.Background()) + e.watcher().TickOnce(context.Background()); n != 0 {
		t.Fatalf("ticks after both levels sent %d", n)
	}
	if len(e.reporter.messages()) != 2 {
		t.Fatalf("messages = %q", e.reporter.texts())
	}

	// The next UTC day is a new window: nothing spent, nothing sent; at 80 % the warning
	// goes out again under the new window's key.
	e.clock.set(at(t, "2026-10-18T09:00:00Z"))
	if n := w.TickOnce(context.Background()); n != 0 {
		t.Fatalf("empty next day sent %d", n)
	}
	e.run(p, at(t, "2026-10-18T08:00:00Z"), 9_000_000, store.RunSucceeded)
	if n := w.TickOnce(context.Background()); n != 1 {
		t.Fatalf("next day at 90 %% sent %d", n)
	}
	last := e.reporter.messages()[2]
	if last.key != "budget-"+p.ID+"-day-2026-10-18T00:00:00Z-warning" || !strings.HasSuffix(last.text, "daily budget (90 %).") {
		t.Fatalf("next day's alert = %+v", last)
	}
}

// T5.19.5 (month): a project with both caps alerts per window; the month text says
// monthly and names the month's end.
func TestWatcherAlertsPerWindow(t *testing.T) {
	e := newAlertEnv(t)
	e.bind()
	p := e.project("Alpha", usd(100), usd(15))
	w := e.watcher()
	e.run(p, at(t, "2026-10-03T06:00:00Z"), 9_000_000, store.RunSucceeded) // 60 % of the month, 0 of the day
	if n := w.TickOnce(context.Background()); n != 0 {
		t.Fatalf("sent %d", n)
	}
	e.run(p, at(t, "2026-10-17T06:00:00Z"), 3_500_000, store.RunSucceeded) // 83 % of the month, 3.5 % of the day
	if n := w.TickOnce(context.Background()); n != 1 {
		t.Fatalf("sent %d", n)
	}
	eq(t, "texts", e.reporter.texts(), []string{"[platform] Project " + p.Name + " has used $12.50 of its $15.00 monthly budget (83 %)."})
	if got := e.reporter.messages()[0].key; got != "budget-"+p.ID+"-month-2026-10-01T00:00:00Z-warning" {
		t.Fatalf("key = %q", got)
	}
	e.run(p, at(t, "2026-10-17T07:00:00Z"), 2_500_000, store.RunSucceeded) // 100 % of the month
	if n := w.TickOnce(context.Background()); n != 1 {
		t.Fatalf("sent %d", n)
	}
	eq(t, "texts", e.reporter.texts()[1:], []string{"[platform] Project " + p.Name + " has reached its $15.00 monthly budget; new runs on it are refused until 2026-11-01T00:00:00Z."})
	eq(t, "ledger", e.rows(p), []string{"month 2026-10-01T00:00:00Z reached", "month 2026-10-01T00:00:00Z warning"})
}

// A jump over both thresholds between two ticks sends both levels, the warning first; a
// cap lowered below the current spend alerts at the next tick; a deleted project and a
// project without a cap are not alerted on.
func TestWatcherJumpsAndCaps(t *testing.T) {
	e := newAlertEnv(t)
	e.bind()
	p := e.project("Alpha", usd(10), nil)
	e.run(p, at(t, "2026-10-17T06:00:00Z"), 12_000_000, store.RunSucceeded)
	uncapped := e.project("Free", nil, nil)
	e.run(uncapped, at(t, "2026-10-17T06:00:00Z"), 900_000_000, store.RunSucceeded)
	gone := e.project("Gone", usd(1), nil)
	e.run(gone, at(t, "2026-10-17T06:00:00Z"), 5_000_000, store.RunSucceeded)
	if err := store.DeleteProject(context.Background(), e.pool, gone.ID); err != nil {
		t.Fatal(err)
	}
	w := e.watcher()
	if n := w.TickOnce(context.Background()); n != 2 {
		t.Fatalf("sent %d, want warning and reached for the one project", n)
	}
	eq(t, "texts", e.reporter.texts(), []string{
		"[platform] Project " + p.Name + " has used $12.00 of its $10.00 daily budget (120 %).",
		"[platform] Project " + p.Name + " has reached its $10.00 daily budget; new runs on it are refused until 2026-10-18T00:00:00Z.",
	})
	if len(e.rows(uncapped)) != 0 || len(e.rows(gone)) != 0 {
		t.Fatalf("alerted on a project without a cap or a deleted one: %v %v", e.rows(uncapped), e.rows(gone))
	}

	// The cap of a second project is lowered below what it already spent.
	q := e.project("Beta", usd(100), nil)
	e.run(q, at(t, "2026-10-17T06:00:00Z"), 30_000_000, store.RunSucceeded)
	if n := w.TickOnce(context.Background()); n != 0 {
		t.Fatalf("30 %% sent %d", n)
	}
	q.BudgetDayUSDMicros = usd(20)
	if err := store.UpdateProject(context.Background(), e.pool, q); err != nil {
		t.Fatal(err)
	}
	if n := w.TickOnce(context.Background()); n != 2 {
		t.Fatalf("a cap lowered below the spend sent %d at the next tick", n)
	}
}

// T5.19.6: the alert goes to the newest conversation of the agent bound to Telegram,
// not to another agent's newer one; without a binding, or without a conversation, it is
// a log line and the row only.
func TestWatcherRoutesToTheBoundAgentsNewestConversation(t *testing.T) {
	e := newAlertEnv(t)
	agentID, older := e.bind() // last message 09:00
	newest := e.conversation(agentID, "2", "2026-10-17T11:00:00Z")
	other := &store.Agent{Name: "Other " + store.NewID(), Model: "claude-opus-5-5"}
	if err := store.CreateAgent(context.Background(), e.pool, other); err != nil {
		t.Fatal(err)
	}
	e.conversation(other.ID, "3", "2026-10-17T11:59:00Z") // newer, but not the bound agent's
	if older == newest {
		t.Fatal("fixture: one conversation")
	}
	p := e.project("Alpha", usd(10), nil)
	e.run(p, at(t, "2026-10-17T06:00:00Z"), 8_000_000, store.RunSucceeded)
	e.watcher().TickOnce(context.Background())
	if msgs := e.reporter.messages(); len(msgs) != 1 || msgs[0].conversation != newest {
		t.Fatalf("messages = %+v, want one to the bound agent's newest conversation %s", msgs, newest)
	}
}

func TestWatcherWithoutDestination(t *testing.T) {
	e := newAlertEnv(t)
	p := e.project("Alpha", usd(10), nil)
	e.run(p, at(t, "2026-10-17T06:00:00Z"), 8_000_000, store.RunSucceeded)
	w := e.watcher()

	// No binding at all.
	if n := w.TickOnce(context.Background()); n != 0 {
		t.Fatalf("sent %d without a binding", n)
	}
	eq(t, "ledger", e.rows(p), []string{"day 2026-10-17T00:00:00Z warning"})
	if e.reporter.attempts != 0 {
		t.Fatal("a send was attempted without a destination")
	}
	if got := strings.Count(e.logs.String(), "nowhere to go"); got != 1 || !strings.Contains(e.logs.String(), p.ID) {
		t.Fatalf("log lines about the alert = %d:\n%s", got, e.logs.String())
	}
	if n := w.TickOnce(context.Background()); n != 0 || strings.Count(e.logs.String(), "nowhere to go") != 1 {
		t.Fatal("the alert was logged again on the next tick")
	}

	// A binding whose agent has no conversation yet: the same.
	a := &store.Agent{Name: "Chief " + store.NewID(), Model: "claude-opus-5-5"}
	if err := store.CreateAgent(context.Background(), e.pool, a); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindChannel(context.Background(), e.pool, store.ChannelTelegram, a.ID); err != nil {
		t.Fatal(err)
	}
	e.run(p, at(t, "2026-10-17T07:00:00Z"), 2_000_000, store.RunSucceeded)
	if n := w.TickOnce(context.Background()); n != 0 || e.reporter.attempts != 0 {
		t.Fatalf("sent %d, attempts %d without a conversation", n, e.reporter.attempts)
	}
	eq(t, "ledger", e.rows(p), []string{"day 2026-10-17T00:00:00Z reached", "day 2026-10-17T00:00:00Z warning"})
}

// T5.19.7: a deferred send is not recorded and is tried again at the next tick; the
// later level waits behind it.
func TestWatcherRetriesADeferredSend(t *testing.T) {
	e := newAlertEnv(t)
	e.bind()
	p := e.project("Alpha", usd(10), nil)
	e.run(p, at(t, "2026-10-17T06:00:00Z"), 12_000_000, store.RunSucceeded) // both levels are due
	w := e.watcher()

	e.reporter.deferred = true
	if n := w.TickOnce(context.Background()); n != 0 {
		t.Fatalf("a deferred send counted as sent: %d", n)
	}
	if len(e.rows(p)) != 0 {
		t.Fatalf("a deferred alert was recorded: %v", e.rows(p))
	}
	if e.reporter.attempts != 1 {
		t.Fatalf("attempts = %d: the reached alert must wait behind the deferred warning", e.reporter.attempts)
	}
	e.reporter.deferred = false
	if n := w.TickOnce(context.Background()); n != 2 {
		t.Fatalf("next tick sent %d, want both", n)
	}
	eq(t, "ledger", e.rows(p), []string{"day 2026-10-17T00:00:00Z reached", "day 2026-10-17T00:00:00Z warning"})
	if n := w.TickOnce(context.Background()); n != 0 {
		t.Fatalf("a third tick sent %d", n)
	}
}

// Run ticks until the context ends and returns.
func TestWatcherRunStopsWithItsContext(t *testing.T) {
	e := newAlertEnv(t)
	e.bind()
	p := e.project("Alpha", usd(10), nil)
	e.run(p, at(t, "2026-10-17T06:00:00Z"), 8_000_000, store.RunSucceeded)
	w := e.watcher()
	w.Tick = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for len(e.reporter.messages()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context ended")
	}
	if len(e.reporter.messages()) != 1 {
		t.Fatalf("messages = %q", e.reporter.texts())
	}
}

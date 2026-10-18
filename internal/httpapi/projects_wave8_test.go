package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"yuno/internal/project"
	"yuno/internal/store"
	"yuno/internal/tools"
)

// projectBody is a valid create body with the extra members spliced in.
func projectBody(sfx, extra string) string {
	if extra != "" {
		extra = "," + extra
	}
	return `{"name":"w8-` + sfx + `","repo_url":"https://gitlab.test/group/w8-` + sfx + `.git","token":"` + acceptedTokenPrefix + sfx + `"` + extra + `}`
}

// fieldMessages decodes a 422's details as path: message pairs.
func fieldMessages(t *testing.T, body []byte) []string {
	t.Helper()
	var fe []fieldError
	if err := json.Unmarshal(decodeError(t, body).Error.Details, &fe); err != nil {
		t.Fatalf("details are not field errors: %s", body)
	}
	out := make([]string, len(fe))
	for i, f := range fe {
		out[i] = f.Path + ": " + f.Message
	}
	return out
}

func jsonText(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// T5.18.1: instructions are validated on both sides of 8192 bytes (after trimming) and
// for control characters; trimmed on the way in; whitespace only is empty; an update
// replaces them and an absent field clears them; the value is returned as stored.
func TestProjectInstructionsValidation(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	_, ts := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)

	reject := func(what, instructions, want string) {
		t.Helper()
		code, body := do(t, ts, http.MethodPost, "/api/projects", projectBody(uniqueSuffix(), `"instructions":`+jsonText(instructions)))
		if code != http.StatusUnprocessableEntity || fmt.Sprint(fieldMessages(t, body)) != "["+want+"]" {
			t.Errorf("%s: %d %s, want 422 [%s]", what, code, body, want)
		}
	}
	accept := func(what, instructions, stored string) *store.Project {
		t.Helper()
		code, body := do(t, ts, http.MethodPost, "/api/projects", projectBody(uniqueSuffix(), `"instructions":`+jsonText(instructions)))
		if code != http.StatusCreated {
			t.Errorf("%s: %d %s, want 201", what, code, body)
			return nil
		}
		p := decodeProject(t, body)
		t.Cleanup(func() { _ = store.DeleteProject(ctx, pool, p.ID) })
		if p.Instructions != stored {
			t.Errorf("%s: instructions = %q, want %q", what, p.Instructions, stored)
		}
		if row, err := store.GetProject(ctx, pool, p.ID); err != nil || row.Instructions != stored {
			t.Errorf("%s: stored %q (%v), want %q", what, row.Instructions, err, stored)
		}
		return &p
	}

	const tooLong, control = "instructions: at most 8192 bytes", "instructions: contains control characters"
	accept("8192 bytes", strings.Repeat("a", 8192), strings.Repeat("a", 8192))
	reject("8193 bytes", strings.Repeat("a", 8193), tooLong)
	accept("8192 bytes between whitespace", "  \n\t"+strings.Repeat("a", 8192)+" \n ", strings.Repeat("a", 8192))
	reject("8193 bytes between whitespace", "  "+strings.Repeat("a", 8193)+"  ", tooLong)
	accept("4096 two-byte characters", strings.Repeat("é", 4096), strings.Repeat("é", 4096))
	reject("4097 two-byte characters, 8194 bytes in 4097 characters", strings.Repeat("é", 4097), tooLong)
	accept("newlines and tabs", "Use tabs:\n\tfoo\n\nbar", "Use tabs:\n\tfoo\n\nbar")
	reject("a bell", "ring\u0007", control)
	reject("a carriage return", "line\r\nline", control)
	reject("a NUL", "nul\u0000nul", control)
	reject("an escape", "\u001b[31mred", control)
	reject("both at once", strings.Repeat("a", 8193)+"\u0007", tooLong+" "+control)
	accept("whitespace only", " \n\t \n", "")
	p := accept("trimmed", "\n  Run make test.  \n\n", "Run make test.")
	if p == nil {
		return
	}

	// Update replaces them; an absent field clears them; PUT back keeps them.
	put := func(body string) (int, []byte) { return do(t, ts, http.MethodPut, "/api/projects/"+p.ID, body) }
	base := `"name":"` + p.Name + `","repo_url":"` + p.RepoURL + `"`
	code, body := put(`{` + base + `,"instructions":"Only edit src/."}`)
	if code != http.StatusOK || decodeProject(t, body).Instructions != "Only edit src/." {
		t.Fatalf("replace: %d %s", code, body)
	}
	_, body = do(t, ts, http.MethodGet, "/api/projects/"+p.ID, "")
	var back map[string]any
	mustDo(t, json.Unmarshal(body, &back))
	raw, _ := json.Marshal(back)
	if code, body := put(string(raw)); code != http.StatusOK || decodeProject(t, body).Instructions != "Only edit src/." {
		t.Fatalf("PUT back what was fetched: %d %s", code, body)
	}
	if code, body := put(`{` + base + `,"instructions":"x` + strings.Repeat("a", 8192) + `"}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("update over the cap: %d %s", code, body)
	}
	if row, _ := store.GetProject(ctx, pool, p.ID); row.Instructions != "Only edit src/." {
		t.Fatalf("a refused update changed the instructions to %q", row.Instructions)
	}
	code, body = put(`{` + base + `}`)
	if code != http.StatusOK || decodeProject(t, body).Instructions != "" {
		t.Fatalf("absent clears: %d %s", code, body)
	}
	if row, _ := store.GetProject(ctx, pool, p.ID); row.Instructions != "" {
		t.Fatalf("stored after clearing: %q", row.Instructions)
	}
	// The token is the only field an update keeps when absent.
	if !decodeProject(t, body).TokenSet {
		t.Fatal("the token was cleared")
	}
}

// T5.19.1: a cap is above 0 and at most 1000000 USD; absent or null clears it; the
// stored value is micro-USD and the API shows USD.
func TestProjectBudgetCapValidation(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	_, ts := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)

	create := func(extra string) (int, []byte) {
		return do(t, ts, http.MethodPost, "/api/projects", projectBody(uniqueSuffix(), extra))
	}
	const (
		day, month = "max_cost_usd_per_day", "max_cost_usd_per_month"
		positive   = ": must be greater than 0"
		atMost     = ": at most 1000000"
	)
	for _, c := range []struct {
		name, value, want string
	}{
		{"zero", "0", positive},
		{"negative", "-1", positive},
		{"negative zero", "-0.0", positive},
		{"under a micro", "0.0000004", positive},
		{"above the maximum", "1000000.01", atMost},
		{"far above the maximum", "1e30", atMost},
		{"a micro over the maximum", "1000000.000001", atMost},
	} {
		for _, field := range []string{day, month} {
			code, body := create(`"` + field + `":` + c.value)
			if code != http.StatusUnprocessableEntity || fmt.Sprint(fieldMessages(t, body)) != "["+field+c.want+"]" {
				t.Errorf("%s %s: %d %s, want 422 [%s%s]", field, c.name, code, body, field, c.want)
			}
		}
	}
	// Both wrong: one message each, in field order. A string is not a number.
	code, body := create(`"` + day + `":0,"` + month + `":2000000`)
	if code != http.StatusUnprocessableEntity || fmt.Sprint(fieldMessages(t, body)) != "["+day+positive+" "+month+atMost+"]" {
		t.Errorf("both: %d %s", code, body)
	}
	if code, body := create(`"` + day + `":"5"`); code != http.StatusUnprocessableEntity || detailPaths(t, body)[0] != day {
		t.Errorf("a string cap: %d %s", code, body)
	}

	// The edges are valid: one micro, the maximum; stored as micro-USD, shown as USD.
	code, body = create(`"` + day + `":0.000001,"` + month + `":1000000`)
	if code != http.StatusCreated {
		t.Fatalf("edges: %d %s", code, body)
	}
	p := decodeProject(t, body)
	t.Cleanup(func() { _ = store.DeleteProject(ctx, pool, p.ID) })
	row, err := store.GetProject(ctx, pool, p.ID)
	mustDo(t, err)
	if row.BudgetDayUSDMicros == nil || *row.BudgetDayUSDMicros != 1 || row.BudgetMonthUSDMicros == nil || *row.BudgetMonthUSDMicros != 1_000_000_000_000 {
		t.Fatalf("stored caps = %v %v, want 1 and 1000000000000 micro-USD", row.BudgetDayUSDMicros, row.BudgetMonthUSDMicros)
	}
	if !strings.Contains(string(body), `"max_cost_usd_per_day":0.000001`) || !strings.Contains(string(body), `"max_cost_usd_per_month":1000000`) {
		t.Fatalf("the caps are not shown in USD: %s", body)
	}

	// Rounding to the micro, and the update paths: replace, PUT back, null, absent.
	put := func(extra string) (int, []byte) {
		return do(t, ts, http.MethodPut, "/api/projects/"+p.ID, `{"name":"`+p.Name+`","repo_url":"`+p.RepoURL+`"`+extra+`}`)
	}
	code, body = put(`,"` + day + `":1.2345678,"` + month + `":50`)
	if got := decodeProject(t, body); code != http.StatusOK || got.BudgetDayUSDMicros == nil || *got.BudgetDayUSDMicros != 1_234_568 {
		t.Fatalf("replace: %d %s", code, body)
	}
	_, body = do(t, ts, http.MethodGet, "/api/projects/"+p.ID, "")
	var back map[string]any
	mustDo(t, json.Unmarshal(body, &back))
	raw, _ := json.Marshal(back)
	if code, body := do(t, ts, http.MethodPut, "/api/projects/"+p.ID, string(raw)); code != http.StatusOK ||
		*decodeProject(t, body).BudgetDayUSDMicros != 1_234_568 || *decodeProject(t, body).BudgetMonthUSDMicros != 50_000_000 {
		t.Fatalf("PUT back what was fetched: %d %s", code, body)
	}
	if code, body := put(`,"` + day + `":0`); code != http.StatusUnprocessableEntity {
		t.Fatalf("update with a zero cap: %d %s", code, body)
	}
	if row, _ := store.GetProject(ctx, pool, p.ID); *row.BudgetDayUSDMicros != 1_234_568 {
		t.Fatal("a refused update changed the cap")
	}
	code, body = put(`,"` + day + `":null`)
	got := decodeProject(t, body)
	if code != http.StatusOK || got.BudgetDayUSDMicros != nil || got.BudgetMonthUSDMicros != nil {
		t.Fatalf("null day, absent month clear both: %d %s", code, body)
	}
	if strings.Contains(string(body), "max_cost_usd_per") {
		t.Fatalf("a cleared cap is still in the representation: %s", body)
	}
	if row, _ := store.GetProject(ctx, pool, p.ID); row.BudgetDayUSDMicros != nil || row.BudgetMonthUSDMicros != nil {
		t.Fatal("the caps are still stored")
	}
}

// T5.19.8: spend in a get and in the list: the sum of the project's runs created in the
// current UTC day and month, in USD, and when each window ends; only the project's own
// runs; zero for a project without runs.
func TestProjectSpendInGetAndList(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	_, ts := newTestServer(t, pool, &fakeEngine{pool: pool}, nil)
	sfx := uniqueSuffix()
	busy := seedProject(t, testCipher(t), "spend-busy-"+sfx, acceptedTokenPrefix+sfx)
	idle := seedProject(t, testCipher(t), "spend-idle-"+sfx, acceptedTokenPrefix+sfx)
	other := seedProject(t, testCipher(t), "spend-other-"+sfx, acceptedTokenPrefix+sfx)
	cost := func(p *store.Project, micros int64, age time.Duration) {
		r := projectRun(t, p)
		_, err := pool.Exec(ctx, `UPDATE runs SET cost_usd_micros=$2, created_at=created_at - make_interval(secs => $3) WHERE id=$1`, r.ID, micros, age.Seconds())
		mustDo(t, err)
	}
	cost(busy, 2_000_000, 0)
	cost(busy, 500_000, 0)
	cost(busy, 700_000_000, 70*24*time.Hour) // in no current window
	cost(other, 900_000_000, 0)

	now := time.Now().UTC()
	_, dayEnd := store.DayBounds(now)
	_, monthEnd := store.MonthBounds(now)
	want := func(p store.Project, day, month float64) {
		t.Helper()
		if p.Spend == nil || p.Spend.DayUSDMicros.USD() != day || p.Spend.MonthUSDMicros.USD() != month ||
			!p.Spend.DayUntil.Equal(dayEnd) || !p.Spend.MonthUntil.Equal(monthEnd) {
			t.Errorf("%s spend = %+v, want day %v until %s and month %v until %s", p.Name, p.Spend, day, dayEnd, month, monthEnd)
		}
	}

	code, body := do(t, ts, http.MethodGet, "/api/projects/"+busy.ID, "")
	if code != http.StatusOK {
		t.Fatalf("get: %d %s", code, body)
	}
	want(decodeProject(t, body), 2.5, 2.5)
	for _, key := range []string{`"day_usd":2.5`, `"month_usd":2.5`, `"day_until":"` + dayEnd.Format(time.RFC3339) + `"`, `"month_until":"` + monthEnd.Format(time.RFC3339) + `"`} {
		if !strings.Contains(string(body), key) {
			t.Errorf("the representation lacks %s: %s", key, body)
		}
	}
	code, body = do(t, ts, http.MethodGet, "/api/projects/"+idle.ID, "")
	idleView := decodeProject(t, body)
	want(idleView, 0, 0)
	if code != http.StatusOK || !strings.Contains(string(body), `"day_usd":0`) {
		t.Fatalf("an idle project has spend zero, not absent: %d %s", code, body)
	}

	code, body = do(t, ts, http.MethodGet, "/api/projects", "")
	var list []store.Project
	if code != http.StatusOK || json.Unmarshal(body, &list) != nil {
		t.Fatalf("list: %d %s", code, body)
	}
	seen := 0
	for _, p := range list {
		switch p.ID {
		case busy.ID:
			want(p, 2.5, 2.5)
			seen++
		case idle.ID:
			want(p, 0, 0)
			seen++
		case other.ID:
			want(p, 900, 900)
			seen++
		}
		if p.Spend == nil {
			t.Errorf("%s has no spend in the list", p.Name)
		}
	}
	if seen != 3 {
		t.Fatalf("listed %d of 3 projects", seen)
	}

	// An update returns the project with its spend too.
	code, body = do(t, ts, http.MethodPut, "/api/projects/"+busy.ID, `{"name":"`+busy.Name+`","repo_url":"`+busy.RepoURL+`","max_cost_usd_per_day":10}`)
	if code != http.StatusOK {
		t.Fatalf("put: %d %s", code, body)
	}
	want(decodeProject(t, body), 2.5, 2.5)

	// A create returns the project with its spend too, like a get: a new project has spent
	// nothing, and says so rather than omitting the member.
	code, body = do(t, ts, http.MethodPost, "/api/projects", projectBody(uniqueSuffix(), `"max_cost_usd_per_day":1`))
	if code != http.StatusCreated {
		t.Fatalf("post: %d %s", code, body)
	}
	created := decodeProject(t, body)
	t.Cleanup(func() { _ = store.DeleteProject(ctx, pool, created.ID) })
	want(created, 0, 0)
	if !strings.Contains(string(body), `"day_usd":0`) || !strings.Contains(string(body), `"month_usd":0`) {
		t.Errorf("a created project has spend zero, not absent: %s", body)
	}
	_, got := do(t, ts, http.MethodGet, "/api/projects/"+created.ID, "")
	if fmt.Sprint(decodeProject(t, got).Spend) != fmt.Sprint(created.Spend) {
		t.Errorf("the created project's spend differs from a get's:\n%s\n%s", body, got)
	}
}

// DESIGN 5.19 rule 3: budgetRefusal is 429 project_budget_reached with the refusal's
// text and details {project_id, window, max_cost_usd, spent_usd, until}.
func TestBudgetRefusalBody(t *testing.T) {
	end := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	be := &project.BudgetError{ProjectID: "pr-1", ProjectName: "Playground", Window: project.WindowDay, CapMicros: 5_000_000, SpendMicros: 5_250_000, WindowEnd: end}
	rec := httptest.NewRecorder()
	budgetRefusal(rec, be)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("response = %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	e := decodeError(t, rec.Body.Bytes())
	const text = "project Playground has reached its daily budget ($5.00); new runs are refused until 2026-10-18T00:00:00Z"
	if e.Error.Code != "project_budget_reached" || e.Error.Message != text {
		t.Fatalf("error = %+v", e.Error)
	}
	var details map[string]any
	mustDo(t, json.Unmarshal(e.Error.Details, &details))
	wantDetails := map[string]any{"project_id": "pr-1", "window": "day", "max_cost_usd": 5.0, "spent_usd": 5.25, "until": "2026-10-18T00:00:00Z"}
	if fmt.Sprint(details) != fmt.Sprint(wantDetails) {
		t.Fatalf("details = %v, want %v", details, wantDetails)
	}

	be.Window, be.WindowEnd = project.WindowMonth, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	rec = httptest.NewRecorder()
	budgetRefusal(rec, be)
	if e := decodeError(t, rec.Body.Bytes()); !strings.Contains(e.Error.Message, "monthly budget") || !strings.HasSuffix(e.Error.Message, "until 2026-11-01T00:00:00Z") {
		t.Fatalf("monthly: %+v", e.Error)
	}
}

// T5.19.3 (delegation): delegate_task answers the refusal's text as a tool error and
// queues nothing.
func TestDelegatorReportsTheBudgetRefusal(t *testing.T) {
	be := &project.BudgetError{ProjectName: "Playground", Window: project.WindowMonth, CapMicros: 20_000_000, SpendMicros: 20_000_000,
		WindowEnd: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	for name, trigErr := range map[string]error{"direct": be, "wrapped": fmt.Errorf("create run: %w", be)} {
		fe := &fakeEngine{triggerErr: trigErr}
		s, _ := newTestServer(t, nil, fe, nil)
		_, err := delegator{s}.Delegate(context.Background(), tools.Delegation{AgentID: "ag-1", Task: "fix the bug", ProjectID: "pr-1"})
		var me *tools.ModelError
		const want = "the run was not started: project Playground has reached its monthly budget ($20.00); new runs are refused until 2026-11-01T00:00:00Z"
		if !errors.As(err, &me) || me.Message != want {
			t.Errorf("%s: err = %v, want a tool error %q", name, err, want)
		}
	}
}

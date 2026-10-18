package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"yuno/internal/engine"
	"yuno/internal/store"
)

// POST /api/runs/{id}/retry (DESIGN 5.21 rule 1).

// retryCall is one call the handler made to the engine.
type retryCall struct {
	runID, by string
	limits    *store.Limits
}

// retryEngine is the fake engine with a Retry of the test's own.
type retryEngine struct {
	*fakeEngine
	retry func(ctx context.Context, runID string, limits *store.Limits) (*store.Run, error)

	retryMu sync.Mutex
	calls   []retryCall
}

func (e *retryEngine) Retry(ctx context.Context, runID, by string, limits *store.Limits) (*store.Run, error) {
	e.retryMu.Lock()
	e.calls = append(e.calls, retryCall{runID, by, limits})
	e.retryMu.Unlock()
	return e.retry(ctx, runID, limits)
}

func (e *retryEngine) retryCalls() []retryCall {
	e.retryMu.Lock()
	defer e.retryMu.Unlock()
	return slices.Clone(e.calls)
}

// endedTestRun stores a run in the given status, stopped at node "agent" with the given
// step count and limits, as the engine leaves one.
func endedTestRun(t *testing.T, pool *pgxpool.Pool, status string, steps int, limits store.Limits) *store.Run {
	t.Helper()
	ctx := context.Background()
	run, err := createTestRun(ctx, pool, store.TriggerManual, nil, limits)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE runs SET status=$2, current_node='agent', step_count=$3,
		failure_reason='boom', finished_at=now() WHERE id=$1`, run.ID, status, steps); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetRun(ctx, pool, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// storeRetry is an engine whose Retry is the store's conditional update.
func storeRetry(pool *pgxpool.Pool) func(context.Context, string, *store.Limits) (*store.Run, error) {
	return func(ctx context.Context, runID string, limits *store.Limits) (*store.Run, error) {
		return conformanceRetry(ctx, pool, runID, limits)
	}
}

// conformanceRetry stands in for the engine's Retry where a test needs the answers
// without the engine: it refuses the way the engine does for the statuses, the exhausted
// step limit and a negative limit, and re-queues with the store's update otherwise.
// TestOpenAPIConformance's fakeEngine.Retry (main_test.go) delegates to it.
func conformanceRetry(ctx context.Context, pool *pgxpool.Pool, runID string, limits *store.Limits) (*store.Run, error) {
	run, err := store.GetRun(ctx, pool, runID)
	if err != nil {
		return nil, fmt.Errorf("run %s: %w", runID, err)
	}
	if !slices.Contains([]string{store.RunFailed, store.RunTimedOut, store.RunCancelled}, run.Status) {
		return nil, &engine.RetryError{Code: "run_not_retryable", Message: "run is " + run.Status}
	}
	if limits != nil && limits.MaxSteps < 0 {
		return nil, &engine.ValidationError{Messages: []string{"limits.max_steps: must not be negative"}}
	}
	under := run.Limits
	if limits != nil && limits.MaxSteps > 0 {
		under.MaxSteps = limits.MaxSteps
	}
	if under.MaxSteps > 0 && run.StepCount >= under.MaxSteps {
		return nil, &engine.RetryError{Code: "limits_exhausted", Message: "limits exhausted: max_steps",
			Details: []string{fmt.Sprintf("max_steps: %d of %d used", run.StepCount, under.MaxSteps)}}
	}
	retried, err := store.RetryRun(ctx, pool, runID, limits, time.Now())
	if errors.Is(err, store.ErrNotFound) {
		// A concurrent retry won between the read and the update.
		return nil, &engine.RetryError{Code: "run_not_retryable", Message: "run is " + store.RunQueued}
	}
	return retried, err
}

// retryConformanceSteps exercises the retry path against the contract: 202 with and
// without a body, 409 for a running run and for an exhausted limit, 422, 404.
func retryConformanceSteps(t *testing.T) []conformanceStep {
	t.Helper()
	failed := endedTestRun(t, testPool, store.RunFailed, 3, store.Limits{MaxSteps: 10})
	cancelled := endedTestRun(t, testPool, store.RunCancelled, 3, store.Limits{MaxSteps: 10})
	exhausted := endedTestRun(t, testPool, store.RunTimedOut, 10, store.Limits{MaxSteps: 10})
	running := endedTestRun(t, testPool, store.RunRunning, 3, store.Limits{MaxSteps: 10})
	return []conformanceStep{
		{http.MethodPost, "/api/runs/" + failed.ID + "/retry", "", http.StatusAccepted},
		{http.MethodPost, "/api/runs/" + cancelled.ID + "/retry", `{"limits":{"max_steps":50,"max_cost_usd":3}}`, http.StatusAccepted},
		{http.MethodPost, "/api/runs/" + running.ID + "/retry", "", http.StatusConflict},
		{http.MethodPost, "/api/runs/" + exhausted.ID + "/retry", "", http.StatusConflict},
		{http.MethodPost, "/api/runs/" + exhausted.ID + "/retry", `{"limits":{"max_steps":-1}}`, http.StatusUnprocessableEntity},
		{http.MethodPost, "/api/runs/" + exhausted.ID + "/retry", `{"limits":{"max_steps":"many"}}`, http.StatusUnprocessableEntity},
		{http.MethodPost, "/api/runs/no-such-run/retry", "", http.StatusNotFound},
	}
}

// The handler re-queues through the engine as the operator and answers 202 with the run;
// the body is optional and its limits reach the engine as RunCreate's do.
func TestRetryRunAccepts(t *testing.T) {
	pool := needDB(t)
	eng := &retryEngine{fakeEngine: &fakeEngine{pool: pool}, retry: storeRetry(pool)}
	_, ts := newTestServer(t, pool, eng, nil)

	tests := []struct {
		name, body string
		want       *store.Limits
	}{
		{"no body", "", nil},
		{"an empty object", `{}`, nil},
		{"null limits", `{"limits":null}`, nil},
		{"empty limits", `{"limits":{}}`, &store.Limits{}},
		{"raised limits", `{"limits":{"max_steps":80,"max_cost_usd":3.5,"max_tool_calls":120}}`,
			&store.Limits{MaxSteps: 80, MaxCostUSD: 3.5, MaxToolCalls: 120}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			run := endedTestRun(t, pool, store.RunFailed, 3, store.Limits{MaxSteps: 10})
			before := len(eng.retryCalls())
			code, body := do(t, ts, http.MethodPost, "/api/runs/"+run.ID+"/retry", tc.body)
			if code != http.StatusAccepted {
				t.Fatalf("status %d: %s", code, body)
			}
			var got store.Run
			if err := json.Unmarshal(body, &got); err != nil || got.ID != run.ID || got.Status != store.RunQueued || got.Retries != 1 {
				t.Fatalf("run = %s (%v)", body, err)
			}
			if !strings.Contains(string(body), `"retries":1`) {
				t.Fatalf("the run does not say it was retried: %s", body)
			}
			calls := eng.retryCalls()
			if len(calls) != before+1 {
				t.Fatalf("%d engine calls", len(calls)-before)
			}
			call := calls[len(calls)-1]
			if call.runID != run.ID || call.by != "operator" {
				t.Fatalf("call = %+v", call)
			}
			switch {
			case tc.want == nil && call.limits != nil:
				t.Fatalf("limits = %+v, want none", *call.limits)
			case tc.want != nil && (call.limits == nil || *call.limits != *tc.want):
				t.Fatalf("limits = %v, want %+v", call.limits, *tc.want)
			}
		})
	}
}

// A body that is not RunRetry is a 422 and the engine is not asked.
func TestRetryRunBodyValidation(t *testing.T) {
	pool := needDB(t)
	eng := &retryEngine{fakeEngine: &fakeEngine{pool: pool}, retry: storeRetry(pool)}
	_, ts := newTestServer(t, pool, eng, nil)
	run := endedTestRun(t, pool, store.RunFailed, 3, store.Limits{MaxSteps: 10})
	tests := []struct {
		name, body, path string
	}{
		{"a limit of the wrong type", `{"limits":{"max_steps":"many"}}`, "limits.max_steps"},
		{"a fractional step limit", `{"limits":{"max_steps":1.5}}`, "limits.max_steps"},
		{"limits that are not an object", `{"limits":5}`, "limits"},
		{"an unknown field", `{"nope":1}`, "nope"},
		{"an unknown limit", `{"limits":{"max_dollars":1}}`, ""},
		{"malformed JSON", `{"limits":`, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, body := do(t, ts, http.MethodPost, "/api/runs/"+run.ID+"/retry", tc.body)
			if code != http.StatusUnprocessableEntity || decodeError(t, body).Error.Code != "validation_failed" {
				t.Fatalf("status %d: %s", code, body)
			}
			if tc.path != "" {
				if got := detailPaths(t, body); !slices.Equal(got, []string{tc.path}) {
					t.Fatalf("paths = %q", got)
				}
			}
		})
	}
	if n := len(eng.retryCalls()); n != 0 {
		t.Fatalf("the engine was asked %d times", n)
	}
}

// 409 carries the refusal's code, message and details; 422 the limit that is not valid;
// 404 an unknown run; anything else is a 500 that does not leak its cause.
func TestRetryRunMapsTheEnginesAnswers(t *testing.T) {
	pool := needDB(t)
	run := endedTestRun(t, pool, store.RunFailed, 3, store.Limits{MaxSteps: 10})
	tests := []struct {
		name     string
		err      error
		status   int
		code     string
		message  string
		details  []fieldError
		noDetail bool
	}{
		{name: "not retryable", err: &engine.RetryError{Code: "run_not_retryable", Message: "run is running"},
			status: http.StatusConflict, code: "run_not_retryable", message: "run is running", noDetail: true},
		{name: "no step", err: &engine.RetryError{Code: "run_not_retryable", Message: "run has no step to retry from"},
			status: http.StatusConflict, code: "run_not_retryable", message: "run has no step to retry from", noDetail: true},
		{name: "retry limit", err: &engine.RetryError{Code: "retry_limit", Message: "run was retried 5 times"},
			status: http.StatusConflict, code: "retry_limit", message: "run was retried 5 times", noDetail: true},
		{name: "exhausted limits", err: &engine.RetryError{Code: "limits_exhausted", Message: "limits exhausted: max_steps, max_cost_usd",
			Details: []string{"max_steps: 40 of 40 used", "max_cost_usd: $2.00 of $2.00 used"}},
			status: http.StatusConflict, code: "limits_exhausted", message: "limits exhausted: max_steps, max_cost_usd",
			details: []fieldError{{"max_steps", "max_steps: 40 of 40 used"}, {"max_cost_usd", "max_cost_usd: $2.00 of $2.00 used"}}},
		{name: "a refusal inside a wrapped error", err: fmt.Errorf("retry: %w", &engine.RetryError{Code: "retry_limit", Message: "run was retried 5 times"}),
			status: http.StatusConflict, code: "retry_limit", message: "run was retried 5 times", noDetail: true},
		{name: "invalid limits", err: &engine.ValidationError{Messages: []string{"limits.max_steps: must not be negative", "limits.max_cost_usd: must not be negative"}},
			status: http.StatusUnprocessableEntity, code: "validation_failed", message: "invalid limits",
			details: []fieldError{{"limits.max_steps", "must not be negative"}, {"limits.max_cost_usd", "must not be negative"}}},
		{name: "the run vanished", err: fmt.Errorf("run %s: %w", run.ID, store.ErrNotFound),
			status: http.StatusNotFound, code: "not_found", message: "run not found", noDetail: true},
		{name: "an engine failure", err: errors.New("pq: connection refused to 10.0.0.7"),
			status: http.StatusInternalServerError, code: "internal", message: "internal error", noDetail: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			eng := &retryEngine{fakeEngine: &fakeEngine{pool: pool},
				retry: func(context.Context, string, *store.Limits) (*store.Run, error) { return nil, tc.err }}
			_, ts := newTestServer(t, pool, eng, nil)
			code, body := do(t, ts, http.MethodPost, "/api/runs/"+run.ID+"/retry", `{"limits":{"max_steps":50}}`)
			if code != tc.status {
				t.Fatalf("status %d: %s", code, body)
			}
			e := decodeError(t, body).Error
			if e.Code != tc.code || e.Message != tc.message {
				t.Fatalf("error = %s %q", e.Code, e.Message)
			}
			if tc.noDetail {
				if len(e.Details) != 0 {
					t.Fatalf("details = %s", e.Details)
				}
			} else {
				var got []fieldError
				if err := json.Unmarshal(e.Details, &got); err != nil || !slices.Equal(got, tc.details) {
					t.Fatalf("details = %s (%v), want %v", e.Details, err, tc.details)
				}
			}
			if strings.Contains(string(body), "10.0.0.7") {
				t.Fatalf("the cause leaked: %s", body)
			}
		})
	}
}

// An unknown run is a 404 before the engine is asked; without an engine the path is 503.
func TestRetryRunUnknownRunAndNoEngine(t *testing.T) {
	pool := needDB(t)
	eng := &retryEngine{fakeEngine: &fakeEngine{pool: pool}, retry: storeRetry(pool)}
	_, ts := newTestServer(t, pool, eng, nil)
	code, body := do(t, ts, http.MethodPost, "/api/runs/does-not-exist/retry", "")
	if e := decodeError(t, body).Error; code != http.StatusNotFound || e.Code != "not_found" || e.Message != "run not found" {
		t.Fatalf("status %d: %s", code, body)
	}
	if n := len(eng.retryCalls()); n != 0 {
		t.Fatalf("the engine was asked %d times", n)
	}

	run := endedTestRun(t, pool, store.RunFailed, 3, store.Limits{MaxSteps: 10})
	_, bare := newTestServerDeps(t, Deps{Cfg: testConfig(), Pool: pool}, nil)
	code, body = do(t, bare, http.MethodPost, "/api/runs/"+run.ID+"/retry", "")
	if code != http.StatusServiceUnavailable || decodeError(t, body).Error.Code != "engine_unavailable" {
		t.Fatalf("without an engine: %d %s", code, body)
	}
}

// T5.21.7 at the handler: of two retries at once, one is 202 and one 409 run_not_retryable
// (the engine's lock and the store's conditional update decide which).
func TestRetryRunTwoAtOnce(t *testing.T) {
	pool := needDB(t)
	eng := &retryEngine{fakeEngine: &fakeEngine{pool: pool}, retry: storeRetry(pool)}
	_, ts := newTestServer(t, pool, eng, nil)
	run := endedTestRun(t, pool, store.RunTimedOut, 3, store.Limits{MaxSteps: 10})
	codes := make([]int, 2)
	bodies := make([][]byte, 2)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], bodies[i] = do(t, ts, http.MethodPost, "/api/runs/"+run.ID+"/retry", "")
		}()
	}
	wg.Wait()
	accepted, refused := 0, 0
	for i, code := range codes {
		switch code {
		case http.StatusAccepted:
			accepted++
		case http.StatusConflict:
			refused++
			if e := decodeError(t, bodies[i]).Error; e.Code != "run_not_retryable" || e.Message != "run is queued" {
				t.Fatalf("loser = %s", bodies[i])
			}
		default:
			t.Fatalf("status %d: %s", code, bodies[i])
		}
	}
	if accepted != 1 || refused != 1 {
		t.Fatalf("statuses = %v: %s / %s", codes, bodies[0], bodies[1])
	}
}

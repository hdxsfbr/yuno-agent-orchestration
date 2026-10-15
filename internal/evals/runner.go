package evals

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"yuno/internal/config"
	"yuno/internal/store"
)

// Runner defaults (DESIGN 5.13).
const (
	// ScriptedModel is the model override every scripted run uses.
	ScriptedModel = "scripted/scripted-1"

	defaultPoll         = 500 * time.Millisecond
	defaultCaseTimeout  = 180 * time.Second
	defaultReadyTimeout = 60 * time.Second
	readyInterval       = time.Second
	// maxPollFailures is how many consecutive failed status polls end a case.
	maxPollFailures = 5
	// cleanupTimeout bounds the best-effort cleanup of a case's temporary agent.
	cleanupTimeout = 15 * time.Second

	// timeoutReason is the failure reason recorded when a case does not finish in time.
	timeoutReason = "runner timeout"
)

// Runner executes cases one at a time against the api.
type Runner struct {
	cfg config.Config
	api *Client
	log *slog.Logger

	poll         time.Duration
	caseTimeout  time.Duration
	readyTimeout time.Duration
	evaluate     func(Case, store.Trace) []EvalResult
	rubric       func(Case, store.Trace) RubricResult
	now          func() time.Time
}

// NewRunner builds a runner for cfg with the DESIGN 5.13 defaults: 500 ms polling and a
// 180 s per-case timeout.
func NewRunner(cfg config.Config, log *slog.Logger) *Runner {
	return &Runner{
		cfg:          cfg,
		api:          NewClient(cfg.EvalAPIURL, cfg.APIToken),
		log:          log,
		poll:         defaultPoll,
		caseTimeout:  defaultCaseTimeout,
		readyTimeout: defaultReadyTimeout,
		evaluate:     Evaluate,
		rubric:       ScoreRubric,
		now:          time.Now,
	}
}

// Execute waits for the api and runs every case in order, one at a time. It returns
// the results gathered so far together with ctx's error when the context ends early.
func (r *Runner) Execute(ctx context.Context, cases []Case) ([]CaseResult, error) {
	readyCtx, cancel := context.WithTimeout(ctx, r.readyTimeout)
	err := r.api.WaitReady(readyCtx, readyInterval)
	cancel()
	if err != nil {
		return nil, err
	}

	results := make([]CaseResult, 0, len(cases))
	for i, c := range cases {
		if err := ctx.Err(); err != nil {
			return results, err
		}
		r.log.Info("eval case start", "case", c.ID, "split", c.Split, "n", i+1, "of", len(cases))
		res := r.runCase(ctx, c)
		r.log.Info("eval case done", "case", c.ID, "status", res.Status, "passed", res.Passed(),
			"e2e", res.E2E, "reason", res.Reason(), "duration_ms", res.DurationMs)
		results = append(results, res)
	}
	return results, nil
}

// runCase runs one case and always returns a result: an error or panic anywhere in the
// case is recorded as that case failing, and the suite continues.
func (r *Runner) runCase(ctx context.Context, c Case) (res CaseResult) {
	started := r.now()
	res = CaseResult{ID: c.ID, Version: c.Version, Split: c.Split, Description: c.Description}
	defer func() {
		if p := recover(); p != nil {
			r.log.Error("eval case panic", "case", c.ID, "panic", p)
			res.Error = fmt.Sprintf("runner panic: %v", p)
			res.Evaluators = failedResults(res.Error)
			res.Rubric = RubricResult{}
		}
		if res.DurationMs == 0 {
			res.DurationMs = r.now().Sub(started).Milliseconds()
		}
		res.E2E = res.Error == "" && res.Status == c.Expect.Status
	}()

	trace, err := r.execCase(ctx, c, &res)
	if trace == nil {
		if res.Error == "" {
			res.Error = err.Error()
		}
		res.Evaluators = failedResults(res.Error)
		return res
	}
	r.fill(&res, trace)
	res.Evaluators = r.evaluate(c, *trace)
	res.Rubric = r.rubric(c, *trace)
	return res
}

// execCase drives one case against the api: agent, script, run, wait, trace. The agent
// is deleted before it returns. It returns the fetched trace when there is one; a
// timeout is recorded in res.Error and still returns the trace taken at that moment.
func (r *Runner) execCase(ctx context.Context, c Case, res *CaseResult) (*store.Trace, error) {
	suffix, err := randomSuffix()
	if err != nil {
		return nil, fmt.Errorf("generate id: %w", err)
	}
	steps, err := c.ModelSteps()
	if err != nil {
		return nil, err
	}

	agent, err := r.api.CreateAgent(ctx, AgentRequest{
		Name:          "eval-" + c.ID + "-" + suffix,
		Role:          c.Agent.Name,
		SystemPrompt:  c.Agent.SystemPrompt,
		Model:         r.cfg.DefaultModel,
		ToolAllowlist: c.Agent.ToolAllowlist,
		Limits:        c.Agent.Limits.Store(),
	})
	if err != nil {
		return nil, fmt.Errorf("create agent: %w", err)
	}
	res.AgentID = agent.ID
	defer r.deleteAgent(c.ID, agent.ID)

	scriptID := c.ID + "-" + suffix
	res.ScriptID = scriptID
	if err := r.api.RegisterScript(ctx, scriptID, steps); err != nil {
		return nil, fmt.Errorf("register script: %w", err)
	}

	run, err := r.api.CreateRun(ctx, RunRequest{
		AgentID:  agent.ID,
		Task:     c.Task,
		Model:    ScriptedModel,
		ScriptID: scriptID,
		Limits:   c.Agent.Limits.Store(),
	})
	if err != nil {
		return nil, fmt.Errorf("create run: %w", err)
	}
	res.RunID = run.ID
	// Every exit that did not see the run finish cancels it (a timeout, an interrupted
	// runner, a poll that kept failing), with a context of its own, so the api does not
	// keep working on a case nobody waits for. It runs after the trace is taken below.
	finished := false
	defer func() {
		if !finished {
			r.cancelRun(ctx, c.ID, run.ID)
		}
	}()

	last, timedOut, err := r.await(ctx, c, run)
	if err != nil {
		res.Status = last.Status
		return nil, err
	}
	finished = !timedOut

	// On a timeout the trace is taken before the cancel so it shows the run as it was
	// when the runner gave up.
	trace, traceErr := r.api.GetTrace(ctx, run.ID)
	if timedOut {
		res.Error = timeoutReason
	}
	if traceErr != nil {
		res.Status = last.Status
		if res.Error == "" {
			res.Error = "fetch trace: " + traceErr.Error()
		}
		return nil, fmt.Errorf("fetch trace: %w", traceErr)
	}
	if trace.Run == nil {
		trace.Run = last
	}
	return trace, nil
}

// await polls the run until it is terminal or the case times out. For cases with
// actions.cancel_after_steps it requests cancellation once step_count reaches that
// value. It returns the last run it saw; timedOut is true when the deadline passed first.
func (r *Runner) await(ctx context.Context, c Case, run *store.Run) (last *store.Run, timedOut bool, err error) {
	last = run
	deadline := time.NewTimer(r.caseTimeout)
	defer deadline.Stop()

	cancelled := false
	failures := 0
	for {
		got, err := r.api.GetRun(ctx, run.ID)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return last, false, ctx.Err()
			}
			failures++
			r.log.Warn("eval poll failed", "case", c.ID, "run", run.ID, "error", err, "consecutive", failures)
			if failures >= maxPollFailures {
				return last, false, fmt.Errorf("poll run: %w", err)
			}
		default:
			failures = 0
			last = got
			if store.IsTerminal(got.Status) {
				return last, false, nil
			}
			if n := c.Actions.CancelAfterSteps; n > 0 && !cancelled && got.StepCount >= n {
				if err := r.api.CancelRun(ctx, run.ID); err != nil {
					r.log.Warn("eval cancel failed", "case", c.ID, "run", run.ID, "error", err)
				} else {
					cancelled = true
					r.log.Info("eval cancel requested", "case", c.ID, "run", run.ID, "step_count", got.StepCount)
				}
			}
		}

		wait := time.NewTimer(r.poll)
		select {
		case <-ctx.Done():
			wait.Stop()
			return last, false, ctx.Err()
		case <-deadline.C:
			wait.Stop()
			return last, true, nil
		case <-wait.C:
		}
	}
}

// fill copies the run-level facts of a trace into the case result.
func (r *Runner) fill(res *CaseResult, t *store.Trace) {
	res.ModelCalls = len(t.ModelCalls)
	res.ToolCalls = len(t.ToolCalls)
	run := t.Run
	if run == nil {
		return
	}
	res.Status = run.Status
	if run.FailureReason != nil {
		res.FailureReason = *run.FailureReason
	}
	res.StepCount = run.StepCount
	res.CostUSDMicros = run.CostUSDMicros
	if run.StartedAt != nil && run.FinishedAt != nil {
		active := run.FinishedAt.Sub(*run.StartedAt) - time.Duration(run.PausedTotalMs)*time.Millisecond
		if active > 0 {
			res.DurationMs = active.Milliseconds()
		}
	}
}

// cancelRun asks the api to cancel a run, ignoring failures: it is cleanup, bounded by
// cleanupTimeout and independent of ctx being cancelled.
func (r *Runner) cancelRun(ctx context.Context, caseID, runID string) {
	cctx, cancel := cleanupContext(ctx)
	defer cancel()
	if err := r.api.CancelRun(cctx, runID); err != nil {
		r.log.Warn("eval run cleanup cancel failed", "case", caseID, "run", runID, "error", err)
	}
}

// deleteAgent removes a case's temporary agent. It runs even when ctx has ended.
func (r *Runner) deleteAgent(caseID, agentID string) {
	ctx, cancel := cleanupContext(context.Background())
	defer cancel()
	if err := r.api.DeleteAgent(ctx, agentID); err != nil {
		r.log.Warn("eval agent cleanup failed", "case", caseID, "agent", agentID, "error", err)
	}
}

// cleanupContext returns a context for best-effort cleanup that outlives a cancelled
// parent but is bounded in time.
func cleanupContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), cleanupTimeout)
}

// randomSuffix returns six random hex characters.
func randomSuffix() (string, error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// errInterrupted is wrapped by Run when the context ends before every case ran.
var errInterrupted = errors.New("eval interrupted")

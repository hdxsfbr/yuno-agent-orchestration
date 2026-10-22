package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"yuno/internal/guardrails"
	"yuno/internal/obs"
	"yuno/internal/openclaw"
	"yuno/internal/project"
	"yuno/internal/store"
	"yuno/internal/workflow"
)

// Engine runs workflows on OpenClaw (DESIGN 5.1, 5.2, 5.5, 5.6, 5.14).
type Engine struct {
	deps   Deps
	clock  Clock
	log    *slog.Logger
	tokens *tokenRegistry

	mu     sync.Mutex
	active map[string][]*visit // run id → node visits in progress (several inside a fork, DESIGN 5.26 rule 4)

	waitMu  sync.Mutex
	waiters map[string]*approvalWaiter // tool approval id → the MCP call waiting for it

	// projects resolves a run's project configuration from its snapshot and the row's
	// sealed token (DESIGN 5.17 rule 6); sbxHolds counts the project visits in progress
	// per OpenClaw agent id, whose sandbox block is cleared when the last one ends
	// (5.16 rule 7).
	projects *project.Resolver
	sbxMu    sync.Mutex
	sbxHolds map[string]int
	bg       sync.WaitGroup // side effects executed after a late approval
	reports  sync.WaitGroup // report-back sends in flight (report.go)
	// settling holds the tool approvals whose late outcome this process is settling
	// (settleUnwaited), so the sweep that repeats an interrupted one does not run it twice.
	settling sync.Map

	// unsavedStarts holds, per run id, the started_at stamp whose write failed in runRun:
	// the engine's re-reads of the run put it back (readRun), so the attempt still has
	// the time limit it was claimed with.
	unsavedStarts sync.Map

	// execute runs one claimed run; tests replace it to observe claims.
	execute func(ctx context.Context, run *store.Run)

	pollInterval  time.Duration // worker claim cadence
	sweepInterval time.Duration // stale-run sweeper cadence
	approvalSweep time.Duration // approval expiry sweeper cadence (DESIGN 5.2 rule 10)
	tickInterval  time.Duration // limit checks while a turn runs
	abortWait     time.Duration // how long to wait for "aborted" after chat.abort
	finishGrace   time.Duration // how long a turn may continue after finish was accepted
	modelStall    time.Duration // how long a turn may wait on the model with nothing arriving; 0 never
	sendTimeout   time.Duration // one chat.send attempt
	setupTimeout  time.Duration // one setup RPC (materialise, sessions.patch, subscribe)
	convRetry     time.Duration // conversation mirror's retry tick (roster sync, catch-up)
	agentSync     time.Duration // KeepAgents' materialisation pass between full syncs
}

// New builds an engine from its dependencies. It does not start workers.
func New(d Deps) *Engine {
	if d.Clock == nil {
		d.Clock = RealClock{}
	}
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	e := &Engine{
		deps:          d,
		clock:         d.Clock,
		log:           d.Log.With("component", "engine"),
		tokens:        newTokenRegistry(d.Clock),
		active:        map[string][]*visit{},
		waiters:       map[string]*approvalWaiter{},
		sbxHolds:      map[string]int{},
		pollInterval:  500 * time.Millisecond,
		sweepInterval: 60 * time.Second,
		approvalSweep: 30 * time.Second,
		tickInterval:  2 * time.Second,
		abortWait:     10 * time.Second,
		finishGrace:   20 * time.Second,
		modelStall:    d.Cfg.ModelStallAfter,
		sendTimeout:   15 * time.Second,
		setupTimeout:  30 * time.Second,
		convRetry:     convResyncEvery,
		agentSync:     30 * time.Second,
	}
	e.execute = e.runRun
	cipher, err := project.NewCipher(d.Cfg.ProjectTokenKey)
	if err != nil && !errors.Is(err, project.ErrNoKey) {
		e.log.Warn("project token key unusable; no project token can be read", "error", err)
	}
	e.projects = &project.Resolver{Cipher: cipher}
	if d.Pool != nil {
		e.projects.DB = d.Pool
	}
	return e
}

// holdSandbox sets the agent's sandbox block for a project visit (DESIGN 5.16 rule 7)
// and counts the hold; the later of two concurrent holds on one agent wins.
func (e *Engine) holdSandbox(ctx context.Context, ocID string, spec openclaw.SandboxSpec) error {
	e.sbxMu.Lock()
	e.sbxHolds[ocID]++
	e.sbxMu.Unlock()
	if err := openclaw.SetAgentSandbox(ctx, e.deps.Gateway, ocID, spec); err != nil {
		e.releaseSandbox(ctx, ocID)
		return err
	}
	return nil
}

// releaseSandbox drops one hold and clears the agent's sandbox block when it was the
// last; a failed clear is logged, and the next api start clears it (ClearSandboxes).
func (e *Engine) releaseSandbox(ctx context.Context, ocID string) {
	e.sbxMu.Lock()
	e.sbxHolds[ocID]--
	last := e.sbxHolds[ocID] <= 0
	if last {
		delete(e.sbxHolds, ocID)
	}
	e.sbxMu.Unlock()
	if !last || e.deps.Gateway == nil {
		return
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.setupTimeout)
	defer cancel()
	if err := openclaw.ClearAgentSandbox(cctx, e.deps.Gateway, ocID); err != nil {
		e.log.Warn("clear agent sandbox failed", "openclaw_agent_id", ocID, "error", err)
	}
}

// sandboxHolds reports the live holds (tests).
func (e *Engine) sandboxHolds() map[string]int {
	e.sbxMu.Lock()
	defer e.sbxMu.Unlock()
	out := make(map[string]int, len(e.sbxHolds))
	for k, v := range e.sbxHolds {
		out[k] = v
	}
	return out
}

var _ Service = (*Engine)(nil)

// Start runs the worker pool, the stale-run and approval sweepers, the gateway
// approval listener and the conversation mirror until ctx is done.
func (e *Engine) Start(ctx context.Context) error {
	workers := e.deps.Cfg.EngineWorkers
	if workers <= 0 {
		workers = 1
	}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			e.worker(ctx, n)
		}(i)
	}
	wg.Add(5)
	go func() {
		defer wg.Done()
		e.sweeper(ctx)
	}()
	go func() {
		defer wg.Done()
		e.mirrorConversations(ctx)
	}()
	go func() {
		defer wg.Done()
		e.approvalSweeper(ctx)
	}()
	go func() {
		defer wg.Done()
		e.listenGateway(ctx)
	}()
	go func() {
		defer wg.Done()
		e.runDirSweeper(ctx)
	}()
	e.log.Info("engine started", "workers", workers)
	wg.Wait()
	// Reports still being sent finish before the engine stops (DESIGN 5.15 rule 9).
	e.reports.Wait()
	return nil
}

func (e *Engine) approvalSweeper(ctx context.Context) {
	t := time.NewTicker(e.approvalSweep)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		e.sweepApprovals(ctx)
	}
}

func (e *Engine) worker(ctx context.Context, n int) {
	t := time.NewTicker(e.pollInterval)
	defer t.Stop()
	for {
		run, err := store.ClaimQueuedRun(ctx, e.deps.Pool)
		switch {
		case err == nil:
			e.log.Info("run claimed", "run_id", run.ID, "worker", n, "attempt", run.Attempt)
			e.execute(ctx, run)
			continue
		case errors.Is(err, store.ErrNotFound):
		case ctx.Err() != nil:
			return
		default:
			e.log.Warn("claim failed", "worker", n, "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (e *Engine) sweeper(ctx context.Context) {
	t := time.NewTicker(e.sweepInterval)
	defer t.Stop()
	// Reports a previous process claimed nothing for are sent now (DESIGN 5.15).
	e.sweepReports(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		e.sweepStale(ctx)
		// After the stale-run sweep, so a run it failed is reported in the same tick.
		e.sweepReports(ctx)
	}
}

// sweepStale re-queues or fails runs a lost worker left running (DESIGN 5.1 rule 8).
func (e *Engine) sweepStale(ctx context.Context) {
	if e.deps.Cfg.EngineStaleAfter <= 0 {
		return
	}
	requeued, failed, err := store.RequeueStaleRuns(ctx, e.deps.Pool, e.deps.Cfg.EngineStaleAfter)
	if err != nil {
		e.log.Warn("stale-run sweep failed", "error", err)
		return
	}
	if requeued > 0 || failed > 0 {
		e.log.Warn("stale runs swept", "requeued", requeued, "failed", failed)
	}
}

// ---- agents -------------------------------------------------------------------------

func (e *Engine) loadAgent(ctx context.Context, agentID string) (*store.Agent, []store.Skill, error) {
	a, err := store.GetAgent(ctx, e.deps.Pool, agentID)
	if err != nil {
		return nil, nil, fmt.Errorf("agent %s: %w", agentID, err)
	}
	skills, err := store.ListSkillsByName(ctx, e.deps.Pool, a.Skills)
	if err != nil {
		return nil, nil, fmt.Errorf("agent %s: skills: %w", agentID, err)
	}
	return a, skills, nil
}

// loadAgentForRun is loadAgent for a node visit: an agent deleted after the run started
// still runs (DESIGN 5.6 edge cases).
func (e *Engine) loadAgentForRun(ctx context.Context, agentID string) (*store.Agent, []store.Skill, error) {
	a, err := store.GetAgentForRun(ctx, e.deps.Pool, agentID)
	if err != nil {
		return nil, nil, fmt.Errorf("agent %s: %w", agentID, err)
	}
	skills, err := store.ListSkillsByName(ctx, e.deps.Pool, a.Skills)
	if err != nil {
		return nil, nil, fmt.Errorf("agent %s: skills: %w", agentID, err)
	}
	return a, skills, nil
}

// materialize pushes the agent when its configuration changed since the last push and
// records the hash and OpenClaw id.
func (e *Engine) materialize(ctx context.Context, a *store.Agent, skills []store.Skill) error {
	prev := a.MaterializedHash
	ex, err := openclaw.LoadExtras(ctx, e.deps.Pool, a.ID, skills)
	if err != nil {
		return fmt.Errorf("agent files: %w", err)
	}
	hash, err := openclaw.MaterializeWith(ctx, e.deps.Gateway, e.deps.StateDir, a, skills, ex)
	if err != nil {
		return err
	}
	// Materialising rewrites MEMORY.md as a bare header when the configuration changed,
	// so the saved facts are rendered back before anything else reads the file.
	if err := openclaw.RenderMemory(ctx, e.deps.Pool, e.deps.StateDir, a, e.clock.Now()); err != nil {
		e.log.Warn("render MEMORY.md after materialise failed", "agent_id", a.ID, "error", err)
	}
	if hash == prev && a.OpenClawAgentID != nil {
		return nil
	}
	if err := store.SetAgentMaterialized(ctx, e.deps.Pool, a.ID, *a.OpenClawAgentID, hash); err != nil {
		return fmt.Errorf("record materialisation: %w", err)
	}
	a.MaterializedHash = hash
	e.log.Info("agent materialised", "agent_id", a.ID, "openclaw_agent_id", *a.OpenClawAgentID)
	return nil
}

// desired is the hash materialising the agent now would record, with the operator's file
// edits and the skills' bundle file digests it was computed from (DESIGN 5.27 rules 4 and
// 8). ok is false when they cannot be read: the caller then materialises (and fails
// visibly) rather than assuming the agent is in sync.
func (e *Engine) desired(ctx context.Context, a *store.Agent, skills []store.Skill) (hash string, ex openclaw.Extras, ok bool) {
	ex, err := openclaw.LoadExtras(ctx, e.deps.Pool, a.ID, skills)
	if err != nil {
		e.log.Warn("agent files not read for the hash", "agent_id", a.ID, "error", err)
		return "", openclaw.Extras{}, false
	}
	return openclaw.DesiredHashWith(e.deps.StateDir, a, skills, ex), ex, true
}

// desiredHash is desired's hash alone; "" when it could not be computed, which never equals
// a stored hash.
func (e *Engine) desiredHash(ctx context.Context, a *store.Agent, skills []store.Skill) string {
	h, _, _ := e.desired(ctx, a, skills)
	return h
}

// refreshWorkspace writes the agent's workspace files again while the gateway already holds
// its configuration: OpenClaw rewrites the runtime files it seeds, and the operator's
// overrides, additions and the skills' bundle files must come back (identical files are
// skipped, bundle bytes are read only for a file that must be written).
func (e *Engine) refreshWorkspace(a *store.Agent, skills []store.Skill, ex openclaw.Extras) {
	if err := openclaw.SyncWorkspace(e.deps.StateDir, a, skills, ex); err != nil {
		e.log.Warn("agent workspace files not refreshed", "agent_id", a.ID, "error", err)
	}
}

// MaterializeAgent pushes the agent's current configuration to OpenClaw and returns
// the runtime view.
func (e *Engine) MaterializeAgent(ctx context.Context, agentID string) (*RuntimeView, error) {
	a, skills, err := e.loadAgent(ctx, agentID)
	if err != nil {
		return nil, err
	}
	if err := e.materialize(ctx, a, skills); err != nil {
		return nil, err
	}
	return e.runtimeView(ctx, a, skills)
}

// AgentRuntime reports what OpenClaw currently holds for an agent without writing.
func (e *Engine) AgentRuntime(ctx context.Context, agentID string) (*RuntimeView, error) {
	a, skills, err := e.loadAgent(ctx, agentID)
	if err != nil {
		return nil, err
	}
	return e.runtimeView(ctx, a, skills)
}

func (e *Engine) runtimeView(ctx context.Context, a *store.Agent, skills []store.Skill) (*RuntimeView, error) {
	v, err := openclaw.Runtime(ctx, e.deps.Gateway, e.deps.StateDir, a)
	if err != nil {
		return nil, err
	}
	inSync := v.InSync && a.MaterializedHash == e.desiredHash(ctx, a, skills)
	return &RuntimeView{
		OpenClawAgentID: v.OpenClawAgentID,
		Materialized:    v.Materialized,
		InSync:          inSync,
		Workspace:       v.Workspace,
		ConfigEntry:     v.ConfigEntry,
		ExecPolicy:      v.ExecPolicy,
		Files:           v.Files,
	}, nil
}

// ---- runs ---------------------------------------------------------------------------

// runOverrides are per-run settings kept in the workflow snapshot next to the
// definition, so a worker that claims the run later still has them.
type runOverrides struct {
	Model    string `json:"model,omitempty"`
	ScriptID string `json:"script_id,omitempty"`
}

// runSnapshot is runs.workflow_snapshot: the definition as run, plus overrides. It
// decodes as a plain workflow.Definition for readers that ignore the extra key.
type runSnapshot struct {
	workflow.Definition
	RunOverrides *runOverrides `json:"run_overrides,omitempty"`
}

// ErrDuplicateTrigger is Trigger's answer when the request's schedule occurrence
// (TriggerRef) already has its run: the occurrence has fired, nothing new is queued
// (DESIGN 5.9 rule 2).
var ErrDuplicateTrigger = errors.New("this schedule occurrence already has a run")

// QuickRunTemplateKey is the template_key of the hidden one-node workflow behind an
// agent's quick runs.
func QuickRunTemplateKey(agentID string) string { return "quickrun:" + agentID }

// Trigger validates and queues a run. Unknown agents wrap store.ErrNotFound; bad input
// is a *ValidationError.
func (e *Engine) Trigger(ctx context.Context, req TriggerRequest) (*store.Run, error) {
	if req.WorkflowID != "" {
		return e.triggerWorkflow(ctx, req)
	}
	var msgs []string
	if strings.TrimSpace(req.AgentID) == "" {
		msgs = append(msgs, "agent_id: required")
	}
	if strings.TrimSpace(req.Task) == "" {
		msgs = append(msgs, "task: required")
	}
	// A quick run opts in by naming a project (DESIGN 5.17 rule 4).
	proj, projMsgs, err := e.projectForRequest(ctx, req.ProjectID, req.ProjectID != "")
	if err != nil {
		return nil, err
	}
	msgs = append(msgs, projMsgs...)
	// The request's merge request, after the project check (DESIGN 5.22 rule 1): a quick
	// run takes one like a project, its one-node definition opting in through the project.
	mrMsgs, mrFinal, err := e.requestedMergeRequest(ctx, req.MergeRequestID, proj, len(projMsgs) > 0)
	if err != nil {
		return nil, err
	}
	msgs = append(msgs, mrMsgs...)
	if len(msgs) > 0 {
		return nil, newValidationError(msgs, mrFinal)
	}
	a, err := store.GetAgent(ctx, e.deps.Pool, req.AgentID)
	if err != nil {
		return nil, fmt.Errorf("agent %s: %w", req.AgentID, err)
	}
	limits, err := resolveLimits(req.Limits, a.Limits, e.deps.Cfg)
	if err != nil {
		return nil, err
	}
	agents := []*store.Agent{a}
	limits = guardrails.EffectiveLimits(limits, agents, e.deps.Cfg.MaxCostUSD)
	def := workflow.SingleAgent(a.ID, "{{.input.task}}", nil, limits)
	def.Project = proj != nil
	wf, err := e.quickRunWorkflow(ctx, a, def)
	if err != nil {
		return nil, err
	}
	input, _ := json.Marshal(map[string]string{"task": req.Task})
	return e.queueRun(ctx, wf, def, input, req, agents, proj, "agent_id", a.ID)
}

// projectForRequest is ProjectForRequest on the engine's pool.
func (e *Engine) projectForRequest(ctx context.Context, id string, optsIn bool) (*store.Project, []string, error) {
	return ProjectForRequest(ctx, e.deps.Pool, id, optsIn)
}

// ProjectForRequest resolves a run request's project (DESIGN 5.17 rule 4): required
// when the definition opts in, refused when it does not, and a live project when one is
// named. The messages are path-qualified validation failures ("project_id: ..."). The
// schedule handlers check a schedule's project with it when it is saved, so a schedule
// is refused exactly as its run would be when it fires (DESIGN 5.9 rule 6).
func ProjectForRequest(ctx context.Context, q store.Querier, id string, optsIn bool) (*store.Project, []string, error) {
	id = strings.TrimSpace(id)
	switch {
	case id == "" && optsIn:
		return nil, []string{"project_id: required; the workflow works on a project repository"}, nil
	case id == "":
		return nil, nil, nil
	case !optsIn:
		return nil, []string{"project_id: the workflow does not use a project"}, nil
	}
	p, err := store.GetProject(ctx, q, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, []string{"project_id: no project with id " + id}, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("project %s: %w", id, err)
	}
	return p, nil, nil
}

// queueRun stores a run with its snapshot (the definition as run, limits resolved, plus
// the run overrides) and publishes it. The participating agents' hourly rate limits are
// checked in the transaction that inserts the run (DESIGN 5.11 rule 2); a refusal is a
// *guardrails.RateLimitError. A project at a budget cap is refused there too, as a
// *project.BudgetError (DESIGN 5.19 rule 3).
func (e *Engine) queueRun(ctx context.Context, wf *store.Workflow, def *workflow.Definition, input json.RawMessage, req TriggerRequest, agents []*store.Agent, proj *store.Project, logAttrs ...any) (*store.Run, error) {
	snap := runSnapshot{Definition: *def}
	if req.Model == "" && req.ScriptID == "" && proj != nil && e.deps.Scripts != nil &&
		(req.Trigger == store.TriggerIssue || req.Trigger == store.TriggerPlan) {
		// A run the platform starts for an eval project takes the script bound to it.
		if id, ok := e.deps.Scripts.TakeBound(proj.ID, req.Trigger); ok {
			req.Model, req.ScriptID = ScriptedRunModel, id
		}
	}
	if req.Model != "" || req.ScriptID != "" {
		snap.RunOverrides = &runOverrides{Model: req.Model, ScriptID: req.ScriptID}
	}
	snapJSON, err := json.Marshal(snap)
	if err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	trigger := req.Trigger
	if trigger == "" {
		trigger = store.TriggerManual
	}
	run := &store.Run{
		WorkflowID:       wf.ID,
		WorkflowVersion:  wf.Version,
		WorkflowSnapshot: snapJSON,
		Trigger:          trigger,
		Status:           store.RunQueued,
		Input:            input,
		Limits:           def.Limits,
	}
	if req.TriggerRef != "" {
		ref := req.TriggerRef
		run.TriggerRef = &ref
	}
	if proj != nil {
		// The project's settings travel with the run (DESIGN 5.17 rule 4); the token
		// stays on the project's row and is read when it is needed.
		pid, settings := proj.ID, proj.Settings()
		run.ProjectID, run.Project = &pid, &settings
		logAttrs = append(logAttrs, "project_id", pid)
	}
	if id := strings.TrimSpace(req.MergeRequestID); id != "" {
		// The request the run updates (DESIGN 5.22 rule 1): its record was checked before,
		// and the live-run index below settles a concurrent run on the same record.
		run.MergeRequestID = &id
		logAttrs = append(logAttrs, "merge_request_id", id)
	}
	insert := func(tx pgx.Tx) error {
		// An occurrence fired again must not count its own first run against the rate
		// limit; the unique index settles a concurrent insert.
		if trigger == store.TriggerSchedule && run.TriggerRef != nil {
			if _, err := store.GetRunByTrigger(ctx, tx, trigger, *run.TriggerRef); err == nil {
				return store.ErrDuplicateTrigger
			} else if !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		if err := guardrails.CheckRateLimit(ctx, tx, agents, e.clock.Now()); err != nil {
			return err
		}
		// A project at its daily or monthly cap takes no new run (DESIGN 5.19 rule 3);
		// the spend is read in this transaction, after the rate check, and a failed
		// read fails the trigger.
		if proj != nil {
			if err := project.CheckBudget(ctx, tx, proj, e.clock.Now()); err != nil {
				return err
			}
		}
		return store.CreateRun(ctx, tx, run)
	}
	err = pgx.BeginFunc(ctx, e.deps.Pool, insert)
	for tries := 0; errors.Is(err, store.ErrMergeRequestBusy) && run.MergeRequestID != nil && tries < 2; tries++ {
		// Another live run names the record (rule 1, the index runs_merge_request_live_idx):
		// the 422 says which. A run that ended between the refusal and the read leaves
		// nothing to name, and the insert goes again.
		busy, berr := e.mergeRequestBusy(ctx, *run.MergeRequestID)
		if berr != nil {
			return nil, fmt.Errorf("create run: %w", berr)
		}
		if busy != nil {
			return nil, busy
		}
		err = pgx.BeginFunc(ctx, e.deps.Pool, insert)
	}
	var rl *guardrails.RateLimitError
	if errors.As(err, &rl) {
		e.log.Info("run refused by a guardrail", "workflow_id", wf.ID, "agent_id", rl.AgentID, "trigger", trigger, "reason", rl.Error())
		return nil, rl
	}
	var be *project.BudgetError
	if errors.As(err, &be) {
		e.log.Info("run refused by a project budget", "workflow_id", wf.ID, "project_id", be.ProjectID, "window", be.Window,
			"trigger", trigger, "reason", be.Error())
		return nil, be
	}
	if errors.Is(err, store.ErrDuplicateTrigger) {
		e.log.Info("run not created: its schedule occurrence already has one", "workflow_id", wf.ID, "trigger_ref", req.TriggerRef)
		return nil, ErrDuplicateTrigger
	}
	if err != nil {
		return nil, fmt.Errorf("create run: %w", err)
	}
	created, err := store.GetRun(ctx, e.deps.Pool, run.ID)
	if err != nil {
		return nil, fmt.Errorf("read run: %w", err)
	}
	e.emit(ctx, created.ID, "run.status", map[string]any{"run": created})
	e.log.Info("run queued", append([]any{"run_id", created.ID, "workflow_id", wf.ID, "trigger", trigger}, logAttrs...)...)
	return created, nil
}

// mergeRequestBusy is the 422 for a run request that lost the live-run index of DESIGN 5.22
// rule 1 to another run: "merge_request_id: <merge request !n|pull request #n> is being
// updated by run <id>". It is nil when no run is live on the record any more, or the
// record is gone.
func (e *Engine) mergeRequestBusy(ctx context.Context, mrID string) (*ValidationError, error) {
	live, err := store.LiveRunForMergeRequest(ctx, e.deps.Pool, mrID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	mr, err := store.GetMergeRequest(ctx, e.deps.Pool, mrID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ValidationError{Messages: []string{"merge_request_id: " + mr.Label() + " is being updated by run " + live.ID}}, nil
}

func (e *Engine) quickRunWorkflow(ctx context.Context, a *store.Agent, def *workflow.Definition) (*store.Workflow, error) {
	key := QuickRunTemplateKey(a.ID)
	wf, err := store.GetWorkflowByTemplateKey(ctx, e.deps.Pool, key)
	if err == nil {
		return wf, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("quick-run workflow: %w", err)
	}
	// The hidden workflow carries no project flag (DESIGN 5.17 edge case): the flag
	// lives in each run's snapshot, so the workflow stays runnable without a project.
	stored := *def
	stored.Project = false
	defJSON, err := json.Marshal(stored)
	if err != nil {
		return nil, err
	}
	wf = &store.Workflow{
		Name:        "Quick run: " + a.Name,
		Description: "One-node workflow behind the agent's quick runs.",
		Definition:  defJSON,
		TemplateKey: &key,
	}
	if err := store.CreateWorkflow(ctx, e.deps.Pool, wf); err != nil {
		// A concurrent trigger created it first (unique template_key).
		if existing, gerr := store.GetWorkflowByTemplateKey(ctx, e.deps.Pool, key); gerr == nil {
			return existing, nil
		}
		return nil, fmt.Errorf("quick-run workflow: %w", err)
	}
	return wf, nil
}

// Cancel flags a run for cancellation (DESIGN 5.2). A queued run is cancelled at once;
// a running one is aborted by its worker on the next check, which this call triggers.
// Finished runs are returned unchanged.
func (e *Engine) Cancel(ctx context.Context, runID, by string) (*store.Run, error) {
	if by == "" {
		by = "user"
	}
	run, err := store.GetRun(ctx, e.deps.Pool, runID)
	if err != nil {
		return nil, fmt.Errorf("run %s: %w", runID, err)
	}
	if store.IsTerminal(run.Status) {
		return run, nil
	}
	if err := store.RequestCancel(ctx, e.deps.Pool, runID, by); err != nil {
		return nil, fmt.Errorf("run %s: %w", runID, err)
	}
	if run.Status == store.RunQueued {
		reason := "cancelled by " + by
		if done, err := store.TransitionRun(ctx, e.deps.Pool, runID, []string{store.RunQueued}, store.RunCancelled, &reason, e.clock.Now()); err == nil {
			e.log.Info("run cancelled before start", "run_id", runID, "by", by)
			// A run re-queued after a release (DESIGN 5.26 rule 8) still has its fork row,
			// its parked visits and their questions open: closed as a waiting run's are.
			e.closeWaitingRun(context.WithoutCancel(ctx), done, reason, "run cancelled")
			return done, nil
		}
	}
	live := e.activeVisits(runID)
	for _, v := range live {
		v.requestCancel()
	}
	if len(live) == 0 && run.Status == store.RunWaitingApproval {
		// Paused with no worker (an approval node): nothing else would see the flag.
		if done, ok := e.cancelWaitingRun(context.WithoutCancel(ctx), runID, "cancelled by "+by); ok {
			e.log.Info("run cancelled while waiting for approval", "run_id", runID, "by", by)
			return done, nil
		}
	}
	run, err = store.GetRun(ctx, e.deps.Pool, runID)
	if err != nil {
		return nil, fmt.Errorf("run %s: %w", runID, err)
	}
	e.emit(ctx, runID, "run.status", map[string]any{"run": run})
	return run, nil
}

// ResolveRunToken maps a token to its run context; ok=false when unknown or revoked.
func (e *Engine) ResolveRunToken(token string) (RunContext, bool) {
	t, ok := e.tokens.get(token)
	if !ok {
		return RunContext{}, false
	}
	return t.rc, true
}

// AcceptFinish validates result against the node's output schema, stores it and marks
// the visit "finish accepted"; the visit ends when the turn does (DESIGN 5.6). A result
// the schema refuses counts toward the visit's three attempts.
func (e *Engine) AcceptFinish(ctx context.Context, token string, result json.RawMessage) error {
	t, ok := e.tokens.get(token)
	if !ok {
		return ErrInvalidRunToken
	}
	if msg := validateOutput(t.schema, result); msg != "" {
		t.v.rejectFinish(msg)
		return &FinishValidationError{Message: msg}
	}
	if msg := t.v.checkResult(ctx, result); msg != "" {
		t.v.rejectFinish(msg)
		return &FinishValidationError{Message: msg}
	}
	return t.v.acceptFinish(ctx, result)
}

// RejectFinish counts a finish call the MCP input schema refused before it reached
// AcceptFinish (DESIGN 5.6: it is an attempt too). Unknown tokens are ignored.
func (e *Engine) RejectFinish(_ context.Context, token, message string) {
	if t, ok := e.tokens.get(token); ok {
		t.v.rejectFinish(message)
	}
}

// ---- shared helpers ------------------------------------------------------------------

// addActive and removeActive keep the live visits of a run; activeVisits lists them.
// A sequential run has at most one; the branches of a fork have one each (DESIGN 5.26
// rule 4), and Cancel, failRun, visitOf and the unattended re-queue look at all of them.
func (e *Engine) addActive(v *visit) {
	e.mu.Lock()
	e.active[v.run.ID] = append(e.active[v.run.ID], v)
	e.mu.Unlock()
}

func (e *Engine) removeActive(v *visit) {
	e.mu.Lock()
	defer e.mu.Unlock()
	live := e.active[v.run.ID]
	for i, cur := range live {
		if cur == v {
			live = append(live[:i:i], live[i+1:]...)
			break
		}
	}
	if len(live) == 0 {
		delete(e.active, v.run.ID)
	} else {
		e.active[v.run.ID] = live
	}
}

func (e *Engine) activeVisits(runID string) []*visit {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]*visit(nil), e.active[runID]...)
}

// emit records an SSE event durably and fans it out; failures are logged.
func (e *Engine) emit(ctx context.Context, runID, typ string, v any) {
	if e.deps.Recorder == nil {
		return
	}
	if _, err := e.deps.Recorder.Emit(context.WithoutCancel(ctx), runID, typ, v); err != nil {
		e.log.Warn("emit event failed", "run_id", runID, "event", typ, "error", err)
	}
}

// logEvent writes a run log line to the structured log and to SSE, redacted for both.
func (e *Engine) logEvent(ctx context.Context, runID, level, msg string, attrs ...any) {
	msg = obs.Redact(msg)
	l := e.log.With(append([]any{"run_id", runID}, attrs...)...)
	switch level {
	case "warn":
		l.Warn(msg)
	case "error":
		l.Error(msg)
	default:
		level = "info"
		l.Info(msg)
	}
	e.emit(ctx, runID, "log", map[string]any{"run_id": runID, "level": level, "message": msg})
}

// startSpan and endSpan store span attributes redacted: they carry error texts and
// tool commands.
func (e *Engine) startSpan(ctx context.Context, runID string, parent *string, kind, name string, at time.Time, end *time.Time, attrs map[string]any) string {
	b, _ := json.Marshal(attrs)
	s := &store.Span{RunID: runID, ParentID: parent, Kind: kind, Name: name, StartedAt: at, EndedAt: end, Attrs: obs.RedactJSON(b)}
	if err := store.InsertSpan(context.WithoutCancel(ctx), e.deps.Pool, s); err != nil {
		e.log.Warn("insert span failed", "run_id", runID, "kind", kind, "error", err)
	}
	return s.ID
}

func (e *Engine) endSpan(ctx context.Context, id string, at time.Time, attrs map[string]any) {
	if id == "" {
		return
	}
	b, _ := json.Marshal(attrs)
	if err := store.EndSpanAt(context.WithoutCancel(ctx), e.deps.Pool, id, at, obs.RedactJSON(b)); err != nil {
		e.log.Warn("end span failed", "span_id", id, "error", err)
	}
}

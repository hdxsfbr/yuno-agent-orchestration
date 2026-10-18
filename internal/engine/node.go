package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"yuno/internal/guardrails"
	"yuno/internal/memory"
	ymodel "yuno/internal/model"
	"yuno/internal/obs"
	"yuno/internal/openclaw"
	"yuno/internal/project"
	"yuno/internal/runtoken"
	"yuno/internal/store"
	"yuno/internal/tools"
	"yuno/internal/workflow"
)

const (
	maxContinueRetries = 3 // DESIGN 5.5 rule 3
	maxSendAttempts    = 5 // DESIGN 5.5 rule 2
	maxFinishAttempts  = 3 // DESIGN 5.6: refused finish submissions per visit
	heartbeatEvery     = 30 * time.Second
	quietCatchUp       = 30 * time.Second // re-read chat.history after this long without events
	historyLimit       = 200
	abortRPCTimeout    = 5 * time.Second
)

type toolState struct {
	row    *store.ToolCall
	spanID string
	ended  bool
}

type toolInput struct {
	input       json.RawMessage
	modelCallID *string
	at          time.Time // when the assistant asked for the call
}

// visit is one execution of an agent node: one OpenClaw session, one or more turns
// (retries), and the mirror of everything the gateway reports about them. The fields
// from turn on are owned by the visit goroutine; mu guards the finish state, which the
// MCP handler changes through AcceptFinish and RejectFinish.
type visit struct {
	e        *Engine
	log      *slog.Logger
	run      *store.Run
	node     *workflow.Node
	agent    *store.Agent
	skills   []store.Skill
	rn       *store.RunNode
	over     runOverrides
	limits   store.Limits
	rootSpan string
	nodeSpan string
	ocID     string
	key      string
	token    string
	schema   *jsonschema.Schema
	early    *outcome // set when the visit cannot start
	task     string   // the rendered task; the protocol block is added per message
	// carryFrom are the workspaces (OpenClaw agent ids) whose runs/<run_id>/ is copied
	// into this agent's before the turn, oldest first (DESIGN 5.6 rule 13).
	carryFrom []string
	// project is the repository this visit works on (DESIGN 5.16): nil unless the run
	// opted in and one is configured. toolchain is read from the checkout before the
	// turn; sandboxHeld says the agent's sandbox block is this visit's to release.
	project     *project.Config
	toolchain   project.Toolchain
	sandboxHeld bool
	// mergeRequest is the record the run names and updates (DESIGN 5.22 rule 1), read once
	// in prepareProject: nil for a run that names none.
	mergeRequest *store.MergeRequest

	events <-chan openclaw.Event
	unsub  func()
	signal chan struct{}
	// cancelCtx ends when Engine.Cancel asks for this run: it interrupts setup RPCs,
	// which run before the event loop that otherwise reacts to the request.
	cancelCtx    context.Context
	cancelSignal context.CancelFunc
	stale        []store.RunNode // visits a lost worker left behind, aborted before the first turn

	// calls run closures on the visit goroutine for callers that need the result
	// (RequestApproval); loopDone closes when the visit stops serving them.
	calls    chan func()
	loopDone chan struct{}
	doneOnce sync.Once

	mu            sync.Mutex
	posted        []func(context.Context) // run on the visit goroutine at the next signal
	finishOutput  json.RawMessage
	finishAt      time.Time
	closed        bool // the outcome is fixed: no result is accepted any more
	rejections    int  // finish submissions refused by a schema
	lastRejection string

	// sessionCreated is set once sessions.patch or chat.send may have reached the
	// gateway, which creates the session: it is deleted at the end however the visit ends.
	sessionCreated bool
	// turnLive is set while a turn may run in OpenClaw: from the first chat.send attempt
	// (whose answer can be lost) until the turn's terminal chat event. A visit that ends
	// with it set aborts the turn.
	turnLive bool

	turn           int
	runID          string
	runIDs         map[string]bool
	steps          int
	seenMsgs       map[string]bool
	messages       []json.RawMessage
	tools          map[string]*toolState
	inputs         map[string]toolInput
	pendingOutputs map[string]json.RawMessage
	approvals      map[string]*store.Approval // exec approvals by gateway id
	approvalSpans  map[string]string
	toolBlocking   map[string]bool   // tool approval ids whose MCP call is waiting
	platformSpans  map[string]string // yuno__* tool call id → span (the MCP handler records the row)
	lastText       string
	finishLogged   bool
	aborting       *outcome
	abortAt        time.Time
	disconnected   bool
	closing        bool
	result         *outcome
	lastTouch      time.Time
	lastEventAt    time.Time
}

// SessionKey is the node-scoped OpenClaw session of a visit (DESIGN 5.14 rule 5). It
// carries the whole run id: a prefix of a UUIDv7 is a timestamp that two runs of one
// agent started close together share.
func SessionKey(openclawAgentID, runID, nodeID string, visitN int) string {
	return fmt.Sprintf("agent:%s:yuno-%s-%s-%d", openclawAgentID, runID, nodeID, visitN)
}

// newVisit creates the run_nodes row and the node span. It returns a nil visit only
// when the row cannot be written.
func (e *Engine) newVisit(ctx context.Context, run *store.Run, node *workflow.Node, over runOverrides, rootSpan string, stale []store.RunNode) (*visit, outcome) {
	now := e.clock.Now()
	v := &visit{
		e: e, run: run, node: node, over: over, limits: run.Limits, rootSpan: rootSpan, stale: stale,
		log:    e.log.With("run_id", run.ID, "node_id", node.ID),
		signal: make(chan struct{}, 1),
		runIDs: map[string]bool{}, seenMsgs: map[string]bool{}, tools: map[string]*toolState{},
		inputs: map[string]toolInput{}, pendingOutputs: map[string]json.RawMessage{},
		approvals: map[string]*store.Approval{}, approvalSpans: map[string]string{},
		toolBlocking: map[string]bool{}, platformSpans: map[string]string{},
		calls: make(chan func()), loopDone: make(chan struct{}),
		lastTouch: time.Now(), lastEventAt: time.Now(),
	}
	v.cancelCtx, v.cancelSignal = context.WithCancel(context.Background())
	prior, err := store.CountNodeVisits(ctx, e.deps.Pool, run.ID, node.ID)
	if err != nil {
		return nil, failed(store.RunFailed, "count node visits: "+err.Error())
	}
	agentID := node.AgentID
	v.rn = &store.RunNode{RunID: run.ID, NodeID: node.ID, AgentID: &agentID, Visit: prior + 1, Status: store.NodeRunning, StartedAt: &now}
	a, skills, err := e.loadAgentForRun(ctx, node.AgentID)
	if err != nil {
		o := failed(store.RunFailed, err.Error())
		v.early = &o
	} else {
		v.agent, v.skills = a, skills
		v.ocID = openclaw.DeriveAgentID(a.ID)
		if a.OpenClawAgentID != nil && *a.OpenClawAgentID != "" {
			v.ocID = *a.OpenClawAgentID
		}
		v.key = SessionKey(v.ocID, run.ID, node.ID, v.rn.Visit)
		v.rn.SessionKey = &v.key
	}
	if err := store.CreateRunNode(ctx, e.deps.Pool, v.rn); err != nil {
		return nil, failed(store.RunFailed, "create node visit: "+err.Error())
	}
	v.nodeSpan = e.startSpan(ctx, run.ID, &rootSpan, "node", node.ID, now, nil,
		map[string]any{"agent_id": node.AgentID, "visit": v.rn.Visit, "session_key": v.key, "run_node_id": v.rn.ID})
	e.emit(ctx, run.ID, "node.status", map[string]any{"node": v.rn})
	return v, outcome{}
}

func (v *visit) wake() {
	select {
	case v.signal <- struct{}{}:
	default:
	}
}

// execute prepares the session and runs turns until the visit has an outcome.
func (v *visit) execute(ctx context.Context) outcome {
	if v.early != nil {
		return *v.early
	}
	e := v.e
	gw := e.deps.Gateway
	if gw == nil {
		return failed(store.RunFailed, "gateway not connected")
	}
	if res := v.awaitGateway(ctx); res != nil {
		return *res
	}
	v.abortStale(ctx)
	if v.agent.MaterializedHash != openclaw.DesiredHash(e.deps.StateDir, v.agent, v.skills) || v.agent.OpenClawAgentID == nil {
		sctx, done := v.setupContext(ctx)
		err := e.materialize(sctx, v.agent, v.skills)
		done()
		if err != nil {
			return v.setupFailed(ctx, "agent materialisation failed: "+err.Error())
		}
		if id := *v.agent.OpenClawAgentID; id != v.ocID {
			v.ocID = id
			v.key = SessionKey(id, v.run.ID, v.node.ID, v.rn.Visit)
			v.rn.SessionKey = &v.key
			v.saveNode(ctx)
		}
	}
	if err := v.prepareRunDir(); err != nil {
		return failed(store.RunFailed, "run directory: "+err.Error())
	}
	if v.project != nil {
		if res := v.prepareProject(ctx); res != nil {
			return *res
		}
	}
	v.renderMemory(ctx)

	v.events, v.unsub = gw.Subscribe(4096)
	if v.over.Model != "" {
		v.sessionCreated = true // sessions.patch creates the session, even if its answer is lost
		sctx, done := v.setupContext(ctx)
		err := openclaw.SessionsPatchModel(sctx, gw, v.key, v.over.Model)
		done()
		if err != nil {
			return v.setupFailed(ctx, fmt.Sprintf("model override %s failed: %v", v.over.Model, err))
		}
	}
	sctx, done := v.setupContext(ctx)
	_, err := openclaw.SessionsSubscribe(sctx, gw, v.key, v.ocID, true)
	done()
	if err != nil {
		return v.setupFailed(ctx, "session subscribe failed: "+err.Error())
	}
	schema, err := compileSchema(v.node.OutputSchema)
	if err != nil {
		return failed(store.RunFailed, err.Error())
	}
	v.schema = schema
	v.token = runtoken.New()
	// No clock expiry: a run paused for an approval keeps a valid token. The token is
	// revoked when the visit ends (release), which also refuses late submissions.
	e.tokens.add(v.token, &tokenEntry{
		rc: RunContext{
			RunID: v.run.ID, RunNodeID: v.rn.ID, NodeID: v.node.ID, AgentID: v.agent.ID,
			OpenClawAgentID: v.ocID, OutputSchema: v.node.OutputSchema,
			ToolAllowlist: v.agent.ToolAllowlist, InteractionRules: v.agent.InteractionRules,
		},
		v: v, schema: schema,
	})

	msg, err := v.taskMessageFor(ctx)
	if err != nil {
		return v.setupFailed(ctx, "project notes: "+err.Error())
	}
	if res := v.startTurn(ctx, msg); res != nil {
		return *res
	}
	return v.loop(ctx)
}

// setupContext bounds one setup RPC (materialisation, sessions.patch, subscribe) by the
// setup timeout and the run's remaining time, and ends it when the run is cancelled:
// a gateway that keeps the connection but never answers must not hold the worker past
// the run's limits (DESIGN 5.2).
func (v *visit) setupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	d := min(v.e.setupTimeout, v.remaining())
	c, cancel := context.WithTimeout(ctx, max(d, 0))
	stop := context.AfterFunc(v.cancelCtx, cancel)
	return c, func() {
		stop()
		cancel()
	}
}

// setupFailed turns a failed setup step into the visit's outcome: a shutdown leaves the
// run for recovery, a cancellation or the time limit that cut the step short wins, and
// anything else fails the node with reason.
func (v *visit) setupFailed(ctx context.Context, reason string) outcome {
	if ctx.Err() != nil {
		return outcome{interrupted: true}
	}
	if res := v.preflight(ctx); res != nil {
		return *res
	}
	return failed(store.RunFailed, reason)
}

// requestCancel interrupts setup RPCs in flight; the loop picks the request up from
// the run row.
func (v *visit) requestCancel() {
	v.cancelSignal()
	v.wake()
}

// abortStale stops the OpenClaw turns of the visits a lost worker left unfinished and
// deletes their sessions, before this replacement visit starts its own turn.
func (v *visit) abortStale(ctx context.Context) {
	for _, n := range v.stale {
		if n.SessionKey == nil || *n.SessionKey == "" {
			continue
		}
		if n.OpenClawRunID != nil && *n.OpenClawRunID != "" {
			actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), abortRPCTimeout)
			res, err := openclaw.ChatAbort(actx, v.e.deps.Gateway, *n.SessionKey, *n.OpenClawRunID)
			cancel()
			if err != nil {
				v.log.Warn("abort of a lost visit's turn failed", "session_key", *n.SessionKey, "error", err)
			} else if res.Aborted {
				v.e.logEvent(ctx, v.run.ID, "warn", fmt.Sprintf("stopped the turn of visit %d left running by a lost worker", n.Visit))
			}
		}
		v.e.deleteSession(context.WithoutCancel(ctx), *n.SessionKey)
	}
	v.stale = nil
}

// renderMemory writes the agent's newest facts into its MEMORY.md before the turn, so
// OpenClaw's own memory loading sees them (DESIGN 5.10 rule 5 and implementation note).
// A failure leaves the previous file and is logged; the turn still runs.
func (v *visit) renderMemory(ctx context.Context) {
	if !v.agent.MemoryPolicy.Enabled || v.ocID == "" {
		return
	}
	facts, err := memory.PromptFactsFor(ctx, v.e.deps.Pool, v.agent.ID, v.e.clock.Now())
	if err == nil {
		err = openclaw.WriteMemoryMD(v.e.deps.StateDir, v.ocID, memory.RenderMemoryMD(facts))
	}
	if err != nil {
		v.log.Warn("render MEMORY.md failed", "error", err)
	}
}

// prepareRunDir creates runs/<run_id>/ in the agent's workspace (best effort) and
// copies into it the run directories of the agents that worked on the run before, in
// the walk's order so a later visit's file wins (DESIGN 5.6 rule 13). It runs once the
// agent is materialised, when its workspace id is final; a failed copy fails the node.
func (v *visit) prepareRunDir() error {
	if v.e.deps.StateDir == "" {
		return nil
	}
	if err := openclaw.EnsureRunDir(v.e.deps.StateDir, v.ocID, v.run.ID); err != nil {
		v.log.Warn("create run directory failed", "error", err)
	}
	for _, from := range v.carryFrom {
		if from == v.ocID {
			continue
		}
		n, err := openclaw.CopyRunDir(v.e.deps.StateDir, from, v.ocID, v.run.ID)
		if err != nil {
			return fmt.Errorf("copy from %s: %w", from, err)
		}
		v.log.Info("run directory copied forward", "from", from, "files", n)
	}
	return nil
}

// prepareProject clones the project repository into this agent's run directory when
// the run has none yet (DESIGN 5.16 rule 3), reads the toolchain from the checkout and
// selects the agent's sandbox for the visit (rule 7). A failure ends the visit, and the
// run, before any turn starts.
func (v *visit) prepareProject(ctx context.Context) *outcome {
	e := v.e
	if e.deps.StateDir == "" {
		return nil
	}
	ws := openclaw.WorkspaceDir(e.deps.StateDir, v.ocID)
	repo := filepath.Join(ws, project.RepoRel(v.run.ID))
	// A run that names a merge request works on that request's branch (DESIGN 5.22 rule 2):
	// the record is read once per visit, for the clone and for the protocol paragraph of
	// rule 5. Nothing checks its state here (rule 3, D-044).
	branch := v.project.DefaultBranch
	if v.run.MergeRequestID != nil && v.mergeRequest == nil {
		mr, err := store.GetMergeRequest(ctx, e.deps.Pool, *v.run.MergeRequestID)
		switch {
		case err == nil:
			v.mergeRequest = mr
		case errors.Is(err, store.ErrNotFound) && v.run.Toolchain == nil:
			o := failed(store.RunFailed, "project clone: merge request "+*v.run.MergeRequestID+" no longer exists")
			return &o
		case errors.Is(err, store.ErrNotFound):
			// A later visit of a run already cloned: the checkout is there, and there is
			// no request left to tell the agent about.
			e.log.Warn("merge request of the run no longer exists", "run_id", v.run.ID, "merge_request_id", *v.run.MergeRequestID)
		default:
			o := failed(store.RunFailed, "read merge request: "+err.Error())
			return &o
		}
	}
	if v.mergeRequest != nil {
		branch = v.mergeRequest.SourceBranch
	}
	if v.run.Toolchain == nil {
		cctx, done := v.boundedContext(ctx, project.GitTimeout)
		start := e.clock.Now()
		sha, err := cloneInto(cctx, *v.project, ws, v.run.ID, branch)
		done()
		if err != nil {
			if ctx.Err() != nil {
				return &outcome{interrupted: true}
			}
			if res := v.preflight(ctx); res != nil {
				return res
			}
			o := failed(store.RunFailed, "project clone: "+err.Error())
			return &o
		}
		tc := project.Detect(repo)
		end := e.clock.Now()
		attrs := map[string]any{"repo": v.project.RepoURL, "branch": branch, "toolchain": tc.Name, "duration_ms": end.Sub(start).Milliseconds()}
		if v.mergeRequest != nil {
			attrs["merge_request"] = v.mergeRequest.Number
		}
		e.startSpan(ctx, v.run.ID, &v.nodeSpan, "project", "project_clone", start, &end, attrs)
		// The checkout's commit is the base the branch guard of rule 4 compares against;
		// it is written before the toolchain, which marks the clone as done.
		if err := store.SetRunCheckoutSHA(context.WithoutCancel(ctx), e.deps.Pool, v.run.ID, sha); err != nil {
			o := failed(store.RunFailed, "record checkout sha: "+err.Error())
			return &o
		}
		if err := store.SetRunToolchain(context.WithoutCancel(ctx), e.deps.Pool, v.run.ID, tc.Name); err != nil {
			o := failed(store.RunFailed, "record toolchain: "+err.Error())
			return &o
		}
		name := tc.Name
		v.run.Toolchain = &name
		v.refreshRun(ctx)
		e.emit(ctx, v.run.ID, "run.status", map[string]any{"run": v.run})
		e.logEvent(ctx, v.run.ID, "info", fmt.Sprintf("project repository cloned: %s (%s), toolchain %s", v.project.RepoURL, branch, tc.Name))
	}
	v.toolchain = project.Detect(repo)
	if v.toolchain.Name != *v.run.Toolchain {
		// The manifest changed since the clone (an agent edited it): the run's record
		// wins for the image, the fresh commands go into the task.
		v.toolchain.Name = *v.run.Toolchain
	}
	spec := openclaw.SandboxSpec{Image: project.SandboxImage(e.deps.Cfg.OpenClawSandboxImage, v.toolchain.Name), Network: v.project.SandboxNetwork}
	sctx, done := v.setupContext(ctx)
	err := e.holdSandbox(sctx, v.ocID, spec)
	done()
	if err != nil {
		o := v.setupFailed(ctx, "sandbox selection failed: "+err.Error())
		return &o
	}
	v.sandboxHeld = true
	return nil
}

// boundedContext bounds a setup step by d and the run's remaining time, ending it when
// the run is cancelled, like setupContext with its own duration.
func (v *visit) boundedContext(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	d = min(d, v.remaining())
	c, cancel := context.WithTimeout(ctx, max(d, 0))
	stop := context.AfterFunc(v.cancelCtx, cancel)
	return c, func() {
		stop()
		cancel()
	}
}

// cloneInto clones branch of the project into runs/<run id>/repo of a workspace and
// returns the commit the checkout is at (DESIGN 5.22 rule 2: the project's default branch,
// or the source branch of the merge request the run updates). The run directory is
// checked through an os.Root of the workspace (a link planted at runs or runs/<run id> is
// refused, as the copy-forward does), and a repo entry left by an earlier attempt is
// removed first.
func cloneInto(ctx context.Context, pc project.Config, ws, runID, branch string) (string, error) {
	root, err := os.OpenRoot(ws)
	if err != nil {
		return "", err
	}
	defer root.Close()
	runRel := filepath.Join("runs", runID)
	for _, rel := range []string{"runs", runRel} {
		info, err := root.Lstat(rel)
		if err != nil {
			return "", err
		}
		if !info.IsDir() {
			return "", fmt.Errorf("%s: run directory is not a plain directory", rel)
		}
	}
	if err := root.RemoveAll(filepath.Join(runRel, project.RepoDir)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	dest := filepath.Join(ws, runRel, project.RepoDir)
	if err := pc.CloneBranch(ctx, dest, branch); err != nil {
		return "", err
	}
	return pc.HeadSHA(ctx, dest)
}

// taskMessage is the rendered node task plus the fixed protocol block (DESIGN 5.6
// rule 5), with the project lines of DESIGN 5.16 rule 9 for a project visit, without the
// operator's instructions and the project notes (taskMessageFor reads those).
func (v *visit) taskMessage() string {
	return v.taskMessageWith(project.Extras{})
}

// taskMessageFor is taskMessage with what a visit on a project starts knowing (DESIGN
// 5.18 rule 4): the project row's instructions and its 10 newest live notes, read now
// from the run's project_id. A failed read is an error: a visit never runs on a silently
// incomplete prompt.
func (v *visit) taskMessageFor(ctx context.Context) (string, error) {
	var x project.Extras
	if v.project != nil && v.toolchain.Name != "" && v.run.ProjectID != nil {
		pool, id := v.e.deps.Pool, *v.run.ProjectID
		instructions, err := store.ProjectInstructions(ctx, pool, id)
		if err != nil {
			return "", err
		}
		notes, err := memory.NewestProjectFacts(ctx, pool, id, project.TaskNotes, v.e.clock.Now())
		if err != nil {
			return "", err
		}
		x = project.Extras{Instructions: instructions, Notes: notes}
	}
	return v.taskMessageWith(x), nil
}

func (v *visit) taskMessageWith(x project.Extras) string {
	schema := v.node.OutputSchema
	var buf bytes.Buffer
	if err := json.Compact(&buf, schema); err == nil {
		schema = buf.Bytes()
	}
	if len(schema) == 0 {
		schema = json.RawMessage(`{"type":"object"}`)
	}
	var sb strings.Builder
	sb.WriteString(v.task)
	sb.WriteString("\n\n" + runtoken.ProtocolHeader + "\n")
	sb.WriteString("run_id: " + v.run.ID + "\n")
	sb.WriteString("run_token: " + v.token + "\n")
	if v.over.ScriptID != "" {
		sb.WriteString("script_id: " + v.over.ScriptID + "\n")
	}
	sb.WriteString("Work inside the directory runs/" + v.run.ID + "/ of your workspace.\n")
	if v.project != nil && v.toolchain.Name != "" {
		cfg := *v.project
		if v.mergeRequest != nil {
			// The repository line names the branch the checkout is on (DESIGN 5.22 rule 5);
			// the project's own default branch stays the merge target.
			cfg.DefaultBranch = v.mergeRequest.SourceBranch
		}
		text := project.InstructionsWith(cfg, v.run.ID, v.toolchain, x)
		if v.mergeRequest != nil {
			// After the repository line, before the sandbox network line.
			first, rest, _ := strings.Cut(text, "\n")
			text = first + "\n" + mergeRequestParagraph(v.mergeRequest) + rest
		}
		sb.WriteString(text)
	}
	sb.WriteString(DeliverablesLine(v.run.ID))
	if v.agent != nil && slices.Contains(v.agent.ToolAllowlist, tools.AskOperatorName) {
		sb.WriteString(QuestionsParagraph(v.run.ID))
	}
	sb.WriteString(`When finished, call the tool yuno__finish with {"run_token": "` + v.token +
		`", "result": <object matching this JSON schema>}:` + "\n")
	sb.Write(schema)
	return sb.String()
}

// mergeRequestParagraph is the paragraph of DESIGN 5.22 rule 5 that tells a visit of a run
// updating a merge request what it is working on: one line of two sentences, newline
// included.
func mergeRequestParagraph(mr *store.MergeRequest) string {
	return "Merge request: this run updates " + mr.Label() + " (" + mr.URL + "), whose branch " + mr.SourceBranch +
		" is the checkout. Read it first with read_merge_request when you have that tool and address its unresolved threads; " +
		"the platform pushes your working tree onto that branch as a new commit when open_merge_request is called.\n"
}

// remaining is the time left before the run's deadline.
func (v *visit) remaining() time.Duration {
	if v.limits.MaxDurationSeconds <= 0 {
		return time.Duration(math.MaxInt64)
	}
	s := limitState{run: v.run, limits: v.limits, now: v.e.clock.Now()}
	return time.Duration(v.limits.MaxDurationSeconds)*time.Second - s.elapsed()
}

func (v *visit) timedOut() outcome {
	return failed(store.RunTimedOut, fmt.Sprintf("time limit %ds exceeded", v.limits.MaxDurationSeconds))
}

// startTurn sends one chat.send, retrying transport failures with the same
// idempotency key (DESIGN 5.5 rule 2). It returns a non-nil outcome when the visit
// must end instead.
func (v *visit) startTurn(ctx context.Context, message string) *outcome {
	e := v.e
	v.turn++
	key := v.rn.ID
	if v.turn > 1 {
		key = fmt.Sprintf("%s:%d", v.rn.ID, v.turn)
	}
	// OpenClaw runs the turn under the idempotency key [probed], so its id is known
	// before the first attempt. It is recorded now: a send whose answer is lost may
	// still have started the turn, which the visit then aborts or reconciles at its end.
	v.runID = key
	v.runIDs[key] = true
	v.rn.OpenClawRunID = &v.runID
	v.saveNode(ctx)
	var lastErr error
	for attempt := 1; attempt <= maxSendAttempts; attempt++ {
		if res := v.preflight(ctx); res != nil {
			return res
		}
		v.sessionCreated, v.turnLive = true, true
		sctx, cancel := context.WithTimeout(ctx, e.sendTimeout)
		r, err := openclaw.ChatSend(sctx, e.deps.Gateway, openclaw.ChatSendParams{
			SessionKey: v.key, AgentID: v.ocID, Message: message, IdempotencyKey: key,
		})
		cancel()
		if err == nil {
			if r.RunID != "" && r.RunID != v.runID {
				v.runID = r.RunID
				v.runIDs[v.runID] = true
				v.saveNode(ctx)
			}
			e.logEvent(ctx, v.run.ID, "info", fmt.Sprintf("turn %d started", v.turn), "openclaw_run_id", v.runID)
			if r.Status == "ok" {
				// The gateway deduplicated the key: the run already happened (a retry after a
				// lost response). Its events are gone, so read the outcome from the history.
				v.catchUp(ctx)
			}
			return nil
		}
		if ctx.Err() != nil {
			return &outcome{interrupted: true}
		}
		lastErr = err
		now := e.clock.Now()
		e.startSpan(ctx, v.run.ID, &v.nodeSpan, "node", fmt.Sprintf("chat.send attempt %d", attempt), now, &now,
			map[string]any{"attempt": attempt, "turn": v.turn, "status": "error", "error": err.Error()})
		e.logEvent(ctx, v.run.ID, "warn", fmt.Sprintf("chat.send attempt %d of %d failed: %v", attempt, maxSendAttempts, err))
		if attempt == maxSendAttempts {
			break
		}
		wait := retryBackoff(attempt)
		if wait >= v.remaining() {
			o := v.timedOut()
			return &o
		}
		e.clock.Sleep(wait)
		if errors.Is(err, openclaw.ErrDisconnected) {
			if res := v.awaitGateway(ctx); res != nil {
				return res
			}
		}
	}
	o := failed(store.RunFailed, fmt.Sprintf("chat.send failed after %d attempts: %v", maxSendAttempts, lastErr))
	return &o
}

// preflight re-reads the run and refuses to start a turn past a limit.
func (v *visit) preflight(ctx context.Context) *outcome {
	if run, err := v.e.readRun(ctx, v.run.ID); err == nil {
		v.run = run
	}
	if viol := checkLimits(v.limitState()); viol != nil {
		o := violated(viol)
		return &o
	}
	return nil
}

func (v *visit) limitState() limitState {
	_, accepted := v.finished()
	return limitState{run: v.run, limits: v.limits, now: v.e.clock.Now(), nodeIterations: v.steps, finishAccepted: accepted}
}

// awaitGateway waits for the gateway connection, bounded by cancellation and time.
func (v *visit) awaitGateway(ctx context.Context) *outcome {
	gw := v.e.deps.Gateway
	if _, up := gw.Info(); up {
		return nil
	}
	v.e.logEvent(ctx, v.run.ID, "warn", "waiting for the OpenClaw gateway connection")
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return &outcome{interrupted: true}
		case <-t.C:
		}
		if _, up := gw.Info(); up {
			return nil
		}
		if res := v.preflight(ctx); res != nil {
			return res
		}
		if time.Since(v.lastTouch) >= heartbeatEvery {
			v.touch(ctx)
		}
	}
}

func (v *visit) touch(ctx context.Context) {
	v.lastTouch = time.Now()
	if err := store.TouchRun(ctx, v.e.deps.Pool, v.run.ID); err != nil {
		v.log.Warn("heartbeat failed", "error", err)
	}
}

// loop consumes gateway events, finish signals and the limit ticker until the visit
// has an outcome.
func (v *visit) loop(ctx context.Context) outcome {
	t := time.NewTicker(v.e.tickInterval)
	defer t.Stop()
	for v.result == nil {
		select {
		case <-ctx.Done():
			return outcome{interrupted: true}
		case ev, ok := <-v.events:
			if !ok {
				v.events = nil
				continue
			}
			v.handle(ctx, ev)
		case <-v.signal:
			v.onSignal(ctx)
		case fn := <-v.calls:
			fn()
		case <-t.C:
			v.onTick(ctx)
		}
	}
	return *v.result
}

// do runs fn on the visit goroutine and waits for it. It fails with ErrInvalidRunToken
// when the visit has stopped.
func (v *visit) do(ctx context.Context, fn func()) error {
	done := make(chan struct{})
	select {
	case v.calls <- func() { fn(); close(done) }:
	case <-v.loopDone:
		return ErrInvalidRunToken
	case <-ctx.Done():
		return ctx.Err()
	}
	<-done
	return nil
}

// post queues fn for the visit goroutine without waiting; a stopped visit drops it.
func (v *visit) post(fn func(context.Context)) {
	v.mu.Lock()
	v.posted = append(v.posted, fn)
	v.mu.Unlock()
	v.wake()
}

func (v *visit) handle(ctx context.Context, ev openclaw.Event) {
	switch ev.Name {
	case openclaw.EventDisconnected:
		if !v.disconnected {
			v.disconnected = true
			v.e.logEvent(ctx, v.run.ID, "warn", "gateway connection lost; waiting to re-attach")
		}
	case openclaw.EventReconnected:
		v.reattach(ctx)
	case openclaw.EventGap:
		// Events were dropped on a full buffer, usage or the turn's end among them: the
		// history is authoritative, so reconcile now rather than after a quiet period.
		if !v.disconnected && v.runID != "" {
			v.e.logEvent(ctx, v.run.ID, "warn", "gateway events were dropped; reconciling from the session history")
			v.lastEventAt = time.Now()
			v.catchUp(ctx)
		}
	case "chat":
		var ce openclaw.ChatEvent
		if json.Unmarshal(ev.Payload, &ce) != nil || ce.SessionKey != v.key || !v.runIDs[ce.RunID] {
			return
		}
		v.lastEventAt = time.Now()
		v.onChat(ctx, ce)
	case "agent":
		var ae openclaw.AgentEvent
		if json.Unmarshal(ev.Payload, &ae) != nil || ae.SessionKey != v.key || !v.runIDs[ae.RunID] {
			return
		}
		v.lastEventAt = time.Now()
		v.onAgent(ctx, ae)
	case "session.message":
		var sm struct {
			SessionKey string          `json:"sessionKey"`
			MessageID  string          `json:"messageId"`
			Message    json.RawMessage `json:"message"`
		}
		if json.Unmarshal(ev.Payload, &sm) != nil || sm.SessionKey != v.key {
			return
		}
		v.lastEventAt = time.Now()
		v.onTranscript(ctx, sm.Message, sm.MessageID)
	case "session.approval":
		var sa openclaw.SessionApprovalEvent
		if json.Unmarshal(ev.Payload, &sa) != nil || sa.SessionKey != v.key {
			return
		}
		v.onApproval(ctx, sa)
	}
}

// ---- transcript ---------------------------------------------------------------------

func msTime(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

func (v *visit) onTranscript(ctx context.Context, raw json.RawMessage, messageID string) {
	var h openclaw.HistoryEntry
	if err := json.Unmarshal(raw, &h); err != nil {
		return
	}
	meta := h.Meta()
	id := messageID
	if id == "" {
		id = meta.ID
	}
	if id != "" {
		if v.seenMsgs[id] {
			return
		}
		v.seenMsgs[id] = true
	}
	if h.Role == "assistant" && h.Usage != nil {
		v.steps++
		v.rn.Iteration = v.steps // persisted with the transcript entry by mirror
	}
	v.mirror(ctx, raw, h, id, meta)
	switch h.Role {
	case "assistant":
		var mcID *string
		if h.Usage != nil {
			mcID = v.recordModelCall(ctx, h, meta)
		}
		asked := v.e.clock.Now()
		if h.Timestamp > 0 {
			asked = msTime(h.Timestamp)
		}
		for _, p := range h.Parts() {
			if p.Type != "toolCall" || p.ID == "" {
				continue
			}
			args := p.Arguments
			if len(args) == 0 || string(args) == "null" {
				args = json.RawMessage(`{}`)
			}
			args = obs.RedactJSON(args)
			v.inputs[p.ID] = toolInput{input: args, modelCallID: mcID, at: asked}
			if st := v.tools[p.ID]; st != nil {
				st.row.Input, st.row.ModelCallID = args, mcID
				if err := store.SetToolCallInput(ctx, v.e.deps.Pool, st.row.ID, args, mcID); err != nil {
					v.log.Warn("set tool input failed", "error", err)
				}
				v.e.emit(ctx, v.run.ID, "tool_call", map[string]any{"tool_call": st.row})
			}
		}
		if t := h.Text(); t != "" {
			v.lastText = t
		}
		if h.Usage != nil {
			v.checkAndAbort(ctx)
		}
	case "toolResult":
		v.toolResult(ctx, h)
	}
}

// toolResult completes a tool call from its transcript result, which only chat.history
// carries [probed]. A call whose item events were dropped, or that ran while the gateway
// was away, gets its row, span, count and limit check here; a call already ended only
// gains its output. Replaying the history again changes nothing.
func (v *visit) toolResult(ctx context.Context, h openclaw.HistoryEntry) {
	if h.ToolCallID == "" || isPlatformTool(h.ToolName) {
		return
	}
	out := v.toolOutputJSON(h.Text())
	if st := v.tools[h.ToolCallID]; st != nil && st.ended {
		if isNullJSON(st.row.Output) {
			st.row.Output = out
			v.updateTool(ctx, st)
		}
		return
	}
	var ts int64
	if h.Timestamp > 0 {
		ts = h.Timestamp
	}
	it := openclaw.AgentItem{ToolCallID: h.ToolCallID, Name: h.ToolName, Kind: "tool", Status: "completed"}
	if h.IsError {
		it.Status = "error"
	}
	if in, ok := v.inputs[h.ToolCallID]; ok && !in.at.IsZero() {
		it.StartedAt = in.at.UnixMilli()
	}
	st := v.toolStart(ctx, it, ts)
	if isNullJSON(st.row.Output) {
		st.row.Output = out
	}
	v.toolEnd(ctx, it, ts)
}

// mirror appends a transcript entry to the node's messages without reasoning parts
// (DESIGN 5.14 rule 11).
func (v *visit) mirror(ctx context.Context, raw json.RawMessage, h openclaw.HistoryEntry, id string, meta openclaw.Meta) {
	m := map[string]any{"role": h.Role, "content": sanitizeContent(h.Content), "timestamp": h.Timestamp}
	if id != "" {
		m["id"] = id
	}
	if meta.RunID != "" {
		m["runId"] = meta.RunID
	}
	for k, s := range map[string]string{"provider": h.Provider, "model": h.Model, "stopReason": h.StopReason,
		"responseId": h.ResponseID, "toolCallId": h.ToolCallID, "toolName": h.ToolName} {
		if s != "" {
			m[k] = s
		}
	}
	if h.IsError {
		m["isError"] = true
	}
	if h.Usage != nil {
		m["usage"] = h.Usage
	}
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	v.messages = append(v.messages, obs.RedactJSON(b)) // credentials never reach the trace
	v.saveNode(ctx)
}

// sanitizeContent drops reasoning parts from a content array; strings pass through.
func sanitizeContent(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`""`)
	}
	var parts []map[string]any
	if err := json.Unmarshal(raw, &parts); err != nil {
		return raw
	}
	kept := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		if typ, _ := p["type"].(string); openclaw.IsReasoningPart(typ) {
			continue
		}
		for k := range p {
			if openclaw.IsReasoningPart(k) {
				delete(p, k)
			}
		}
		kept = append(kept, p)
	}
	b, _ := json.Marshal(kept)
	return b
}

func isNullJSON(b json.RawMessage) bool {
	s := strings.TrimSpace(string(b))
	return s == "" || s == "null"
}

// costMicros is the larger of OpenClaw's reported cost and our price table's
// (DESIGN 5.2).
func costMicros(modelID string, u *openclaw.Usage) int64 {
	reported := int64(math.Round(u.Cost.Total * 1e6))
	if i := strings.LastIndex(modelID, "/"); i >= 0 {
		modelID = modelID[i+1:]
	}
	ours, _ := ymodel.CostMicros(modelID, ymodel.Usage{
		InputTokens: u.Input, OutputTokens: u.Output, CacheReadTokens: u.CacheRead, CacheWriteTokens: u.CacheWrite,
	})
	return max(reported, ours)
}

func (v *visit) recordModelCall(ctx context.Context, h openclaw.HistoryEntry, meta openclaw.Meta) *string {
	e := v.e
	u := h.Usage
	started := e.clock.Now()
	if h.Timestamp > 0 {
		started = msTime(h.Timestamp)
	}
	finished := started
	if meta.RecordTimestampMs > 0 && msTime(meta.RecordTimestampMs).After(started) {
		finished = msTime(meta.RecordTimestampMs)
	}
	cost := costMicros(h.Model, u)
	mc := &store.ModelCall{
		RunID: v.run.ID, RunNodeID: v.rn.ID, Provider: h.Provider, Model: h.Model,
		InputTokens: u.Input, OutputTokens: u.Output, CacheReadTokens: u.CacheRead, CacheWriteTokens: u.CacheWrite,
		CostUSDMicros: cost, DurationMs: finished.Sub(started).Milliseconds(), Attempt: v.turn,
		StartedAt: started, FinishedAt: &finished,
	}
	if h.ResponseID != "" {
		mc.RequestID = &h.ResponseID
	}
	if h.StopReason != "" {
		mc.StopReason = &h.StopReason
	}
	if err := store.InsertModelCall(ctx, e.deps.Pool, mc); err != nil {
		v.log.Warn("insert model call failed", "error", err)
		return nil
	}
	if run, err := store.AddRunUsageReturning(ctx, e.deps.Pool, v.run.ID, 1, 0, u.Input, u.Output, u.CacheRead, u.CacheWrite, cost); err == nil {
		v.run = run
	} else {
		v.log.Warn("add run usage failed", "error", err)
	}
	e.startSpan(ctx, v.run.ID, &v.nodeSpan, "model_call", h.Provider+"/"+h.Model, started, &finished, map[string]any{
		"status": "ok", "attempt": v.turn, "step": v.steps, "stop_reason": h.StopReason,
		"input_tokens": u.Input, "output_tokens": u.Output, "cache_read_tokens": u.CacheRead,
		"cache_write_tokens": u.CacheWrite, "cost_usd_micros": cost, "model_call_id": mc.ID,
	})
	e.emit(ctx, v.run.ID, "model_call", map[string]any{"model_call": mc, "run": runCounters(v.run)})
	return &mc.ID
}

// recordFailedModelCall stores a turn that ended in a provider error, so retries show
// in the trace as attempts.
func (v *visit) recordFailedModelCall(ctx context.Context, ce openclaw.ChatEvent, provider, model string) {
	e := v.e
	now := e.clock.Now()
	msg := strings.TrimSpace(ce.ErrorKind + ": " + ce.ErrorMessage)
	msg = obs.Redact(strings.Trim(msg, ": "))
	if msg == "" {
		msg = "model error"
	}
	mc := &store.ModelCall{
		RunID: v.run.ID, RunNodeID: v.rn.ID, Provider: provider, Model: model, Attempt: v.turn,
		Error: &msg, StartedAt: now, FinishedAt: &now,
	}
	if err := store.InsertModelCall(ctx, e.deps.Pool, mc); err != nil {
		v.log.Warn("insert failed model call failed", "error", err)
		return
	}
	e.startSpan(ctx, v.run.ID, &v.nodeSpan, "model_call", provider+"/"+model, now, &now,
		map[string]any{"status": "error", "attempt": v.turn, "error": msg, "model_call_id": mc.ID})
	e.emit(ctx, v.run.ID, "model_call", map[string]any{"model_call": mc})
}

// ---- agent stream: tool calls -------------------------------------------------------

func (v *visit) onAgent(ctx context.Context, ae openclaw.AgentEvent) {
	switch ae.Stream {
	case "reasoning":
		return // never stored (DESIGN 5.14 rule 11)
	case "item":
		var it openclaw.AgentItem
		if json.Unmarshal(ae.Data, &it) != nil || it.ToolCallID == "" || (it.Kind != "" && it.Kind != "tool") {
			return
		}
		if isPlatformTool(it.Name) {
			v.platformToolSpan(ctx, it, ae.TS)
			return
		}
		switch it.Phase {
		case "start":
			v.toolStart(ctx, it, ae.TS)
		case "end":
			v.toolEnd(ctx, it, ae.TS)
		}
	case "command_output":
		var co openclaw.AgentCommandOutput
		if json.Unmarshal(ae.Data, &co) != nil || co.ToolCallID == "" {
			return
		}
		out := v.toolOutputJSON(co.Output)
		st := v.tools[co.ToolCallID]
		if st == nil {
			v.pendingOutputs[co.ToolCallID] = out
			return
		}
		st.row.Output = out
		if st.ended {
			v.updateTool(ctx, st)
		}
	}
}

// isPlatformTool reports a platform MCP tool as OpenClaw names it: the MCP handler
// records those calls with their full input and output, so the item recorder skips them.
func isPlatformTool(name string) bool { return strings.HasPrefix(name, openclaw.PlatformToolPrefix) }

// platformToolSpan keeps a span per platform tool call so the trace shows its timing.
func (v *visit) platformToolSpan(ctx context.Context, it openclaw.AgentItem, ts int64) {
	at := v.e.clock.Now()
	switch {
	case it.Phase == "start" && it.StartedAt > 0:
		at = msTime(it.StartedAt)
	case it.Phase == "end" && it.EndedAt > 0:
		at = msTime(it.EndedAt)
	case ts > 0:
		at = msTime(ts)
	}
	id, open := v.platformSpans[it.ToolCallID]
	switch it.Phase {
	case "start":
		if !open {
			v.platformSpans[it.ToolCallID] = v.e.startSpan(ctx, v.run.ID, &v.nodeSpan, "tool_call", it.Name, at, nil,
				map[string]any{"tool": it.Name, "tool_use_id": it.ToolCallID, "attempt": v.turn, "recorded_by": "mcp"})
		}
	case "end":
		if open && id != "" {
			v.e.endSpan(ctx, id, at, map[string]any{"status": it.Status, "error": it.Error})
			v.platformSpans[it.ToolCallID] = ""
		}
	}
}

func (v *visit) toolStart(ctx context.Context, it openclaw.AgentItem, ts int64) *toolState {
	if st := v.tools[it.ToolCallID]; st != nil {
		return st
	}
	e := v.e
	started := e.clock.Now()
	if it.StartedAt > 0 {
		started = msTime(it.StartedAt)
	} else if ts > 0 {
		started = msTime(ts)
	}
	in := v.inputs[it.ToolCallID]
	row := &store.ToolCall{
		RunID: v.run.ID, RunNodeID: v.rn.ID, ModelCallID: in.modelCallID, ToolUseID: it.ToolCallID,
		ToolName: it.Name, Input: in.input, Status: store.ToolExecuted, Attempt: v.turn, StartedAt: &started,
	}
	if out, ok := v.pendingOutputs[it.ToolCallID]; ok {
		row.Output = out
		delete(v.pendingOutputs, it.ToolCallID)
	}
	if err := store.InsertToolCall(ctx, e.deps.Pool, row); err != nil {
		v.log.Warn("insert tool call failed", "error", err)
	}
	st := &toolState{row: row}
	st.spanID = e.startSpan(ctx, v.run.ID, &v.nodeSpan, "tool_call", it.Name, started, nil,
		map[string]any{"tool": it.Name, "tool_use_id": it.ToolCallID, "attempt": v.turn, "tool_call_id": row.ID})
	v.tools[it.ToolCallID] = st
	e.emit(ctx, v.run.ID, "tool_call", map[string]any{"tool_call": row})
	return st
}

var failedItemStatus = map[string]bool{"failed": true, "error": true, "denied": true, "cancelled": true, "aborted": true, "timeout": true}

func (v *visit) toolEnd(ctx context.Context, it openclaw.AgentItem, ts int64) {
	st := v.toolStart(ctx, it, ts)
	if st.ended {
		return
	}
	e := v.e
	st.ended = true
	finished := e.clock.Now()
	if it.EndedAt > 0 {
		finished = msTime(it.EndedAt)
	} else if ts > 0 {
		finished = msTime(ts)
	}
	row := st.row
	row.FinishedAt = &finished
	if row.StartedAt != nil && finished.After(*row.StartedAt) {
		row.DurationMs = finished.Sub(*row.StartedAt).Milliseconds()
	}
	row.Status = store.ToolExecuted
	if it.Error != "" || failedItemStatus[strings.ToLower(it.Status)] {
		row.Status, row.IsError = store.ToolFailed, true
		if isNullJSON(row.Output) && it.Error != "" {
			row.Output, _ = json.Marshal(map[string]string{"error": obs.Redact(it.Error)})
		}
	}
	if run, err := store.AddRunUsageReturning(ctx, e.deps.Pool, v.run.ID, 0, 1, 0, 0, 0, 0, 0); err == nil {
		v.run = run
	} else {
		v.log.Warn("add tool call count failed", "error", err)
	}
	v.updateTool(ctx, st)
	e.endSpan(ctx, st.spanID, finished, map[string]any{"status": row.Status, "duration_ms": row.DurationMs, "error": it.Error})
	v.checkAndAbort(ctx)
}

// updateTool stores a tool call and publishes it with the run counters it may have moved.
func (v *visit) updateTool(ctx context.Context, st *toolState) {
	if err := store.UpdateToolCall(ctx, v.e.deps.Pool, st.row); err != nil {
		v.log.Warn("update tool call failed", "error", err)
	}
	v.e.emit(ctx, v.run.ID, "tool_call", map[string]any{"tool_call": st.row, "run": runCounters(v.run)})
}

// runCounters is the part of the run that call events carry, so a live run summary
// follows the counters without a status change.
func runCounters(r *store.Run) map[string]any {
	return map[string]any{
		"id": r.ID, "step_count": r.StepCount, "tool_call_count": r.ToolCallCount,
		"input_tokens": r.InputTokens, "output_tokens": r.OutputTokens, "cache_read_tokens": r.CacheReadTokens,
		"cache_write_tokens": r.CacheWriteTokens, "cost_usd_micros": r.CostUSDMicros,
	}
}

// toolOutputJSON stores JSON output as JSON and anything else as a string, capped at
// TOOL_OUTPUT_MAX_BYTES.
func (v *visit) toolOutputJSON(s string) json.RawMessage {
	limit := v.e.deps.Cfg.ToolOutputMaxBytes
	t := strings.TrimSpace(s)
	if (strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[")) && json.Valid([]byte(t)) && (limit <= 0 || len(t) <= limit) {
		var buf bytes.Buffer
		if json.Compact(&buf, []byte(t)) == nil {
			return obs.RedactJSON(buf.Bytes())
		}
	}
	s = obs.Redact(s)
	if limit > 0 && len(s) > limit {
		s = strings.ToValidUTF8(s[:limit], "") + fmt.Sprintf("\n[truncated %d bytes]", len(s)-limit)
	}
	b, _ := json.Marshal(s)
	return b
}

// ---- approvals ----------------------------------------------------------------------

var approvalStatus = map[string]string{"allowed": "approved", "approved": "approved", "denied": "rejected", "rejected": "rejected", "expired": "expired"}

func (v *visit) onApproval(ctx context.Context, sa openclaw.SessionApprovalEvent) {
	e := v.e
	ap := sa.Approval
	// Plugin approvals are the platform's own tool approvals, registered by
	// RequestApproval; the gateway may echo them on the session.
	if ap.ID == "" || ap.Presentation.Kind == "plugin" || strings.HasPrefix(ap.ID, "plugin:") {
		return
	}
	now := e.clock.Now()
	a := v.approvals[ap.ID]
	if a == nil {
		requested := now
		if ap.CreatedAtMs > 0 {
			requested = msTime(ap.CreatedAtMs)
		}
		expires := requested.Add(30 * time.Minute)
		if ap.ExpiresAtMs > 0 {
			expires = msTime(ap.ExpiresAtMs)
		}
		// The platform's approval timeout applies when it is the shorter one: the
		// sweeper then resolves deny itself (DESIGN 5.3 rule 4).
		if t := v.limits.ApprovalTimeoutSeconds; t > 0 && requested.Add(secondsDuration(t)).Before(expires) {
			expires = requested.Add(secondsDuration(t))
		}
		id := ap.ID
		a = &store.Approval{
			RunID: v.run.ID, RunNodeID: v.rn.ID, Kind: "exec", NodeID: v.node.ID,
			Message: obs.Redact(ap.Presentation.CommandText), Status: "pending", ExternalID: &id,
			RequestedAt: requested, ExpiresAt: expires,
		}
		if err := store.InsertApproval(ctx, e.deps.Pool, a); err != nil {
			v.log.Warn("insert approval failed", "error", err)
			return
		}
		v.approvals[ap.ID] = a
		v.approvalSpans[ap.ID] = e.startSpan(ctx, v.run.ID, &v.nodeSpan, "approval_wait", "exec approval", requested, nil,
			map[string]any{"approval_id": a.ID, "external_id": ap.ID, "command": ap.Presentation.CommandText})
		e.emit(ctx, v.run.ID, "approval.requested", map[string]any{"approval": a})
		v.pause(ctx)
		// A command one of the agent's exec patterns blocks is denied at once (DESIGN
		// 5.11 rule 4); the gateway's terminal event then finds the row decided.
		if p, blocked := guardrails.MatchCommand(v.agent.Guardrails, ap.Presentation.CommandText); blocked && sa.Phase != "terminal" {
			if _, err := e.DecideApproval(ctx, a.ID, DecisionReject, "guardrail", guardrails.BlockedMessage(p)); err != nil {
				v.log.Warn("deny blocked command failed", "approval_id", a.ID, "error", err)
			}
			return
		}
		if sa.Phase != "terminal" {
			e.afterApproval(ctx, v.run) // DESIGN 5.15 rule 10
		}
	}
	if sa.Phase != "terminal" || a.Status != "pending" {
		return
	}
	status := approvalStatus[ap.Status]
	if status == "" {
		status = "rejected"
		if strings.HasPrefix(ap.Decision, "allow") {
			status = "approved"
		}
	}
	reason := ap.Reason
	if reason == "" {
		reason = ap.Decision
	}
	decidedAt := now
	if ap.ResolvedAtMs > 0 {
		decidedAt = msTime(ap.ResolvedAtMs)
	}
	decided, err := store.DecideApprovalAt(ctx, e.deps.Pool, a.ID, status, "openclaw", reason, decidedAt)
	if errors.Is(err, store.ErrNotFound) {
		// Decided on the platform already (DecideApproval, the sweeper): keep that.
		if cur, gerr := store.GetApproval(ctx, e.deps.Pool, a.ID); gerr == nil {
			v.approvalDecided(ctx, cur)
		}
		return
	}
	if err != nil {
		v.log.Warn("decide approval failed", "error", err)
		return
	}
	v.approvals[ap.ID] = decided
	e.endSpan(ctx, v.approvalSpans[ap.ID], decidedAt, map[string]any{"status": status, "reason": reason})
	e.emit(ctx, v.run.ID, "approval.decided", map[string]any{"approval": decided})
	v.resumeIfClear(ctx)
}

// approvalDecided takes a decision made outside the visit: an exec approval decided on
// the platform, or a tool approval whose MCP call stopped waiting.
func (v *visit) approvalDecided(ctx context.Context, a *store.Approval) {
	for ext, cur := range v.approvals {
		if cur.ID == a.ID {
			v.approvals[ext] = a
		}
	}
	if a.Status != store.ApprovalPending {
		delete(v.toolBlocking, a.ID)
	}
	v.resumeIfClear(ctx)
}

// pause moves the run and the node to waiting_approval (the wait does not count
// toward the time limit, DESIGN 5.2).
func (v *visit) pause(ctx context.Context) {
	if v.rn.Status == store.NodeWaitingApproval {
		return
	}
	e := v.e
	if run, err := store.PauseRunAt(ctx, e.deps.Pool, v.run.ID, v.rn.ID, e.clock.Now()); err == nil {
		v.run = run
		e.emit(ctx, v.run.ID, "run.status", map[string]any{"run": run})
	}
	v.rn.Status = store.NodeWaitingApproval
	v.saveNode(ctx)
	e.emit(ctx, v.run.ID, "node.status", map[string]any{"node": v.rn})
}

// resumeIfClear returns the run to running once nothing blocks the turn: no exec
// approval pending and no MCP call waiting for a tool approval.
func (v *visit) resumeIfClear(ctx context.Context) {
	for _, a := range v.approvals {
		if a.Status == store.ApprovalPending {
			return
		}
	}
	if len(v.toolBlocking) > 0 || v.rn.Status != store.NodeWaitingApproval {
		return
	}
	e := v.e
	if run, err := store.ResumeRunAt(ctx, e.deps.Pool, v.run.ID, e.clock.Now()); err == nil {
		v.run = run
		e.emit(ctx, v.run.ID, "run.status", map[string]any{"run": run})
	}
	v.rn.Status = store.NodeRunning
	v.saveNode(ctx)
	e.emit(ctx, v.run.ID, "node.status", map[string]any{"node": v.rn})
}

// ---- turn end, limits, finish -------------------------------------------------------

func (v *visit) onChat(ctx context.Context, ce openclaw.ChatEvent) {
	if ce.RunID != v.runID {
		return // a late event of an earlier turn of this visit
	}
	switch ce.State {
	case "final":
		v.turnLive = false
		v.turnFinal(ctx, ce)
	case "error":
		v.turnLive = false
		v.turnError(ctx, ce)
	case "aborted":
		v.turnLive = false
		v.turnAborted(ctx, ce)
	}
}

// end fixes the visit's outcome and closes finish acceptance (a result arriving later is
// refused). A success yields to a cancellation or the time limit in effect at this
// moment, re-read from the run row, so a result never outruns a cancel (DESIGN 5.2).
func (v *visit) end(ctx context.Context, o outcome) {
	if v.result != nil {
		return
	}
	accepted, ok := v.closeAcceptance()
	if o.runStatus == store.RunSucceeded {
		if ok {
			o.output = accepted // the first accepted result is the node's result
		}
		v.refreshRun(ctx)
		s := v.limitState()
		s.finishAccepted = true // only cancellation and time
		if viol := checkLimits(s); viol != nil {
			o = violated(viol)
		}
	}
	v.result = &o
}

// closeAcceptance stops further finish submissions and returns the accepted result.
func (v *visit) closeAcceptance() (json.RawMessage, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.closed = true
	return v.finishOutput, v.finishOutput != nil
}

func (v *visit) refreshRun(ctx context.Context) {
	if run, err := v.e.readRun(ctx, v.run.ID); err == nil {
		v.run = run
	}
}

func (v *visit) turnFinal(ctx context.Context, ce openclaw.ChatEvent) {
	if v.aborting != nil {
		v.end(ctx, *v.aborting)
		return
	}
	if out, ok := v.finished(); ok {
		v.end(ctx, succeeded(out))
		return
	}
	// No finish: before a result from the reply text can win, the turn's usage must be
	// in (events may have been missed) and every limit must still hold.
	v.reconcile(ctx)
	if v.result != nil {
		return // a limit ended the visit while the history was replayed
	}
	if out, ok := v.finished(); ok {
		v.end(ctx, succeeded(out))
		return
	}
	v.refreshRun(ctx)
	if viol := checkLimits(v.limitState()); viol != nil {
		v.end(ctx, violated(viol))
		return
	}
	if n, msg := v.finishRejections(); n >= maxFinishAttempts {
		v.end(ctx, finishRejected(n, msg))
		return
	}
	text := v.lastText
	if len(ce.Message) > 0 {
		var t openclaw.TranscriptEntry
		if json.Unmarshal(ce.Message, &t) == nil && t.Text() != "" {
			text = t.Text()
		}
	}
	if out, ok := findJSONResult(v.schema, text); ok {
		if accepted, ok := v.closeAcceptance(); ok {
			v.end(ctx, succeeded(accepted)) // a finish got in first
			return
		}
		if err := store.SetRunNodeOutput(ctx, v.e.deps.Pool, v.rn.ID, out); err != nil {
			v.log.Warn("store node output failed", "error", err)
		}
		v.e.logEvent(ctx, v.run.ID, "info", "result taken from the final reply (finish was not called)")
		v.end(ctx, succeeded(out))
		return
	}
	v.end(ctx, failed(store.RunFailed, "agent did not call finish"))
}

type errorDetail struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	HTTPStatus int    `json:"httpStatus"`
}

func (v *visit) turnError(ctx context.Context, ce openclaw.ChatEvent) {
	if v.aborting != nil {
		v.end(ctx, *v.aborting)
		return
	}
	var d errorDetail
	_ = json.Unmarshal(ce.ErrorDetail, &d)
	v.recordFailedModelCall(ctx, ce, d.Provider, d.Model)
	if out, ok := v.finished(); ok {
		v.end(ctx, succeeded(out))
		return
	}
	retryable := retryableHTTPStatus[d.HTTPStatus] || transportLike(ce.ErrorKind) ||
		(ce.ErrorKind == "" && transportLike(ce.ErrorMessage))
	kind := ce.ErrorKind
	if kind == "" {
		kind = ce.ErrorMessage
	}
	if kind == "" {
		kind = "unknown error"
	}
	if retryable && v.turn <= maxContinueRetries {
		wait := retryBackoff(v.turn)
		if wait >= v.remaining() {
			v.end(ctx, v.timedOut())
			return
		}
		v.e.logEvent(ctx, v.run.ID, "warn", fmt.Sprintf("model error (%s); continuing in a new turn, retry %d of %d",
			kind, v.turn, maxContinueRetries))
		v.e.clock.Sleep(wait)
		if res := v.startTurn(ctx, "Continue."); res != nil {
			v.end(ctx, *res)
		}
		return
	}
	v.end(ctx, failed(store.RunFailed, "model error: "+kind))
}

func (v *visit) turnAborted(ctx context.Context, ce openclaw.ChatEvent) {
	if v.aborting != nil {
		v.end(ctx, *v.aborting)
		return
	}
	if out, ok := v.finished(); ok {
		v.end(ctx, succeeded(out))
		return
	}
	reason := "turn aborted"
	if ce.StopReason != "" {
		reason += " (" + ce.StopReason + ")"
	}
	v.end(ctx, failed(store.RunFailed, reason))
}

// checkAndAbort applies the limits after a counter moved or on the ticker, then the
// finish attempt limit (DESIGN 5.6).
func (v *visit) checkAndAbort(ctx context.Context) {
	if v.closing || v.aborting != nil || v.result != nil {
		return
	}
	if viol := checkLimits(v.limitState()); viol != nil {
		v.startAbort(ctx, violated(viol))
		return
	}
	if n, msg := v.finishRejections(); n >= maxFinishAttempts {
		v.startAbort(ctx, finishRejected(n, msg))
	}
}

// finishRejected fails a node whose finish submissions were refused too often.
func finishRejected(n int, lastMessage string) outcome {
	return failed(store.RunFailed, fmt.Sprintf("finish result rejected %d times: %s", n, lastMessage))
}

// startAbort aborts the current turn; the visit ends with o when the gateway confirms
// or after the abort wait (DESIGN 5.2). With no turn running it ends at once.
func (v *visit) startAbort(ctx context.Context, o outcome) {
	e := v.e
	v.aborting = &o
	v.abortAt = e.clock.Now()
	if o.reason != "" {
		e.logEvent(ctx, v.run.ID, "warn", "stopping the turn: "+o.reason)
	} else {
		e.logEvent(ctx, v.run.ID, "info", "finish was accepted; ending the turn")
	}
	if !v.turnLive {
		v.end(ctx, o)
		return
	}
	actx, cancel := context.WithTimeout(ctx, abortRPCTimeout)
	res, err := openclaw.ChatAbort(actx, e.deps.Gateway, v.key, v.runID)
	cancel()
	if err != nil {
		v.log.Warn("chat.abort failed", "error", err)
		return
	}
	if !res.Aborted {
		v.turnLive = false
		v.end(ctx, o) // nothing was running any more
	}
}

// abortLiveTurn aborts a turn that may still run when the visit stops without its
// terminal event: a chat.send whose answer was lost, an abort that was not confirmed,
// or a worker shutting down. Best effort, bounded.
func (v *visit) abortLiveTurn(ctx context.Context) {
	gw := v.e.deps.Gateway
	if !v.turnLive || v.runID == "" || gw == nil {
		return
	}
	v.turnLive = false
	if _, up := gw.Info(); !up {
		v.log.Warn("turn may still run in OpenClaw; gateway not connected", "openclaw_run_id", v.runID)
		return
	}
	actx, cancel := context.WithTimeout(ctx, abortRPCTimeout)
	res, err := openclaw.ChatAbort(actx, gw, v.key, v.runID)
	cancel()
	if err != nil {
		v.log.Warn("chat.abort of the remaining turn failed", "openclaw_run_id", v.runID, "error", err)
		return
	}
	if res.Aborted {
		v.e.logEvent(ctx, v.run.ID, "warn", "stopped a turn that was still running", "openclaw_run_id", v.runID)
	}
}

func (v *visit) onSignal(ctx context.Context) {
	v.mu.Lock()
	posted := v.posted
	v.posted = nil
	v.mu.Unlock()
	for _, fn := range posted {
		fn(ctx)
	}
	if v.result != nil {
		return
	}
	if _, ok := v.finished(); ok && !v.finishLogged {
		v.finishLogged = true
		v.e.logEvent(ctx, v.run.ID, "info", "finish accepted")
	}
	v.refreshRun(ctx)
	v.checkAndAbort(ctx)
}

func (v *visit) onTick(ctx context.Context) {
	e := v.e
	if time.Since(v.lastTouch) >= heartbeatEvery {
		v.touch(ctx)
	}
	if v.aborting != nil {
		if !e.clock.Now().Before(v.abortAt.Add(e.abortWait)) {
			v.log.Warn("abort not confirmed in time; ending the visit")
			v.end(ctx, *v.aborting)
		}
		return
	}
	if v.disconnected {
		if _, up := e.deps.Gateway.Info(); up {
			v.reattach(ctx)
		}
	} else if v.runID != "" && time.Since(v.lastEventAt) >= quietCatchUp {
		// Events may have been dropped on a full buffer; the history is authoritative.
		v.lastEventAt = time.Now()
		v.catchUp(ctx)
		if v.result != nil {
			return
		}
	}
	v.refreshRun(ctx)
	// Cancellation and the time limit come before the finish grace: on a tick where
	// both apply, the run is cancelled or timed out, not succeeded.
	v.checkAndAbort(ctx)
	if v.aborting != nil || v.result != nil {
		return
	}
	if out, ok := v.finished(); ok {
		v.mu.Lock()
		at := v.finishAt
		v.mu.Unlock()
		if e.clock.Now().Sub(at) > e.finishGrace {
			v.startAbort(ctx, succeeded(out))
		}
	}
}

// reattach renews the session subscription after a reconnect, replays approvals the
// gateway reports as still open, and reconciles from chat.history whether the turn
// finished meanwhile (DESIGN 5.5 rule 1).
func (v *visit) reattach(ctx context.Context) {
	e := v.e
	v.disconnected = false
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	res, err := openclaw.SessionsSubscribe(sctx, e.deps.Gateway, v.key, v.ocID, true)
	cancel()
	if err != nil {
		v.disconnected = true
		v.log.Warn("re-subscribe failed", "error", err)
		return
	}
	e.logEvent(ctx, v.run.ID, "info", "gateway connection restored; session re-attached")
	for _, raw := range res.ApprovalReplay.Approvals {
		sa := openclaw.SessionApprovalEvent{SessionKey: v.key, Phase: "terminal"}
		if json.Unmarshal(raw, &sa.Approval) != nil || sa.Approval.ID == "" {
			continue
		}
		if sa.Approval.Status == "" || sa.Approval.Status == "pending" {
			sa.Phase = "pending"
		}
		v.onApproval(ctx, sa)
	}
	v.lastEventAt = time.Now()
	v.catchUp(ctx)
}

// catchUp mirrors any transcript entries missed and ends the turn when the history
// shows it finished.
func (v *visit) catchUp(ctx context.Context) {
	last := v.reconcile(ctx)
	if v.result != nil || last == nil {
		return
	}
	var h openclaw.HistoryEntry
	if json.Unmarshal(last, &h) != nil || h.Role != "assistant" || h.Meta().RunID != v.runID {
		return
	}
	switch strings.ToLower(h.StopReason) {
	case "tooluse", "tool_use", "":
		return // still working
	case "error":
		v.onChat(ctx, openclaw.ChatEvent{RunID: v.runID, SessionKey: v.key, State: "error", ErrorKind: "error"})
	case "aborted":
		v.onChat(ctx, openclaw.ChatEvent{RunID: v.runID, SessionKey: v.key, State: "aborted", StopReason: h.StopReason})
	default:
		v.onChat(ctx, openclaw.ChatEvent{RunID: v.runID, SessionKey: v.key, State: "final", StopReason: h.StopReason, Message: last})
	}
}

// reconcile replays chat.history through the transcript handler (entries already seen
// are skipped) and returns the newest entry.
func (v *visit) reconcile(ctx context.Context) json.RawMessage {
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	hist, err := openclaw.ChatHistory(hctx, v.e.deps.Gateway, v.key, v.ocID, historyLimit)
	cancel()
	if err != nil {
		v.log.Warn("chat.history failed", "error", err)
		return nil
	}
	for _, raw := range hist.Messages {
		v.onTranscript(ctx, raw, "")
	}
	if len(hist.Messages) == 0 {
		return nil
	}
	return hist.Messages[len(hist.Messages)-1]
}

// acceptFinish records a validated finish result; called from the MCP handler. It runs
// under the visit's finish lock, as does closing the visit, so a result is never stored
// after the outcome was fixed. The first accepted result is final: the same result again
// is accepted, a different one refused, and neither restarts the finish grace.
func (v *visit) acceptFinish(ctx context.Context, result json.RawMessage) error {
	var buf bytes.Buffer
	if err := json.Compact(&buf, result); err != nil {
		return &FinishValidationError{Message: "result is not valid JSON"}
	}
	// The result is stored on the node and merged into runs.state, which the API and the
	// trace serve: credentials and tokens an agent echoes are redacted first (DESIGN
	// 5.12 rule 4, 5.15 rule 1).
	out := json.RawMessage(obs.RedactJSON(buf.Bytes()))
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return ErrInvalidRunToken
	}
	if v.finishOutput != nil {
		if bytes.Equal(v.finishOutput, out) {
			return nil
		}
		return &FinishValidationError{Message: "a result was already accepted for this node and cannot be changed"}
	}
	if err := store.SetRunNodeOutput(ctx, v.e.deps.Pool, v.rn.ID, out); err != nil {
		return fmt.Errorf("store finish result: %w", err)
	}
	v.finishOutput = out
	v.finishAt = v.e.clock.Now()
	v.wake()
	return nil
}

// rejectFinish counts a finish submission a schema refused (DESIGN 5.6); the visit
// fails at the third.
func (v *visit) rejectFinish(message string) {
	v.mu.Lock()
	if v.closed || v.finishOutput != nil {
		v.mu.Unlock()
		return
	}
	v.rejections++
	v.lastRejection = message
	n := v.rejections
	v.mu.Unlock()
	if n >= maxFinishAttempts {
		v.wake()
	}
}

func (v *visit) finishRejections() (int, string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.rejections, v.lastRejection
}

func (v *visit) finished() (json.RawMessage, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.finishOutput, v.finishOutput != nil
}

func (v *visit) saveNode(ctx context.Context) {
	if v.messages != nil {
		b, _ := json.Marshal(v.messages)
		v.rn.Messages = b
	}
	if err := store.UpdateRunNodeProgress(context.WithoutCancel(ctx), v.e.deps.Pool, v.rn); err != nil {
		v.log.Warn("update node visit failed", "error", err)
	}
}

// release closes finish acceptance and drops the visit's token and subscriptions
// without writing a terminal state (shutdown, and the start of finish).
func (v *visit) release() {
	v.mu.Lock()
	v.closed = true
	v.mu.Unlock()
	if v.unsub != nil {
		v.unsub()
	}
	if v.token != "" {
		v.e.tokens.remove(v.token)
	}
	v.cancelSignal()
	v.doneOnce.Do(func() { close(v.loopDone) })
}

// finish is the visit's terminal housekeeping: revoke the token, reconcile the
// transcript, close open tool calls and approvals, write the node row and span.
func (v *visit) finish(ctx context.Context, res outcome) {
	e := v.e
	v.closing = true
	v.release()
	if v.sandboxHeld {
		v.sandboxHeld = false
		e.releaseSandbox(ctx, v.ocID)
	}
	if e.deps.Gateway != nil && v.sessionCreated {
		if _, up := e.deps.Gateway.Info(); up {
			v.reconcile(ctx)
		}
	}
	now := e.clock.Now()
	for _, st := range v.tools {
		if st.ended {
			continue
		}
		st.ended = true
		st.row.Status, st.row.IsError, st.row.FinishedAt = store.ToolFailed, true, &now
		if isNullJSON(st.row.Output) {
			st.row.Output = json.RawMessage(`{"error":"the turn ended before the tool finished"}`)
		}
		v.updateTool(ctx, st)
		e.endSpan(ctx, st.spanID, now, map[string]any{"status": store.ToolFailed})
	}
	for extID, a := range v.approvals {
		if a.Status != store.ApprovalPending {
			continue
		}
		reason := "run ended"
		if res.runStatus == store.RunCancelled {
			reason = "run cancelled"
		}
		if decided, ok := e.expireApproval(ctx, a, reason); ok {
			v.approvals[extID] = decided
		}
	}
	// A turn that ended while an MCP call still waited for its approval leaves the run
	// paused; the walk continues with it running. The approval stays pending. After a
	// result no turn can use the call's answer any more, so a later grant is executed
	// by the engine (DESIGN 5.3 rule 5); after a failure the call stays attached, so an
	// expiry when the run ends still answers it.
	if v.rn.Status == store.NodeWaitingApproval {
		if run, err := store.ResumeRunAt(ctx, e.deps.Pool, v.run.ID, now); err == nil {
			v.run = run
		}
	}
	if res.nodeStatus == store.NodeSucceeded {
		e.detachWaiters(v)
	}
	clear(v.toolBlocking)
	v.rn.Status = res.nodeStatus
	v.rn.FinishedAt = &now
	if res.reason != "" {
		r := res.reason
		v.rn.FailureReason = &r
	}
	v.saveNode(ctx)
	if res.runStatus == store.RunSucceeded && len(res.output) > 0 {
		if err := store.SetRunNodeOutput(ctx, e.deps.Pool, v.rn.ID, res.output); err != nil {
			v.log.Warn("store node output failed", "error", err)
		}
		v.rn.Output = res.output
	}
	e.endSpan(ctx, v.nodeSpan, now, map[string]any{"status": res.nodeStatus, "steps": v.steps, "turns": v.turn, "reason": res.reason})
	e.emit(ctx, v.run.ID, "node.status", map[string]any{"node": v.rn})
}

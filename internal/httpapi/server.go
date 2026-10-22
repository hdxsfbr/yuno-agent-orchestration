// Package httpapi is the HTTP layer: routes, middleware, SSE, the MCP server and the
// model proxy mounts. It depends on the store and engine packages, never the other
// way round.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"yuno/api"
	"yuno/internal/artifacts"
	"yuno/internal/channels"
	"yuno/internal/config"
	"yuno/internal/deploytarget"
	"yuno/internal/engine"
	"yuno/internal/events"
	"yuno/internal/intake"
	"yuno/internal/memory"
	"yuno/internal/mergereq"
	"yuno/internal/model"
	"yuno/internal/openclaw"
	"yuno/internal/plans"
	"yuno/internal/project"
	"yuno/internal/sched"
	"yuno/internal/store"
	"yuno/internal/tools"
	"yuno/internal/waves"
)

// Deps are the server's collaborators. Engine, Gateway and Scripts may be nil (tests):
// engine-backed endpoints then answer 503, readyz reports the gateway as not
// connected, and the scripted provider has no scripts.
type Deps struct {
	Cfg     config.Config
	Log     *slog.Logger
	Pool    *pgxpool.Pool
	Hub     *events.Hub
	Engine  engine.Service
	Gateway openclaw.Client
	Scripts model.ScriptStore
	// ModelTransport is the model proxy's upstream round tripper; nil means the default.
	ModelTransport http.RoundTripper
	// Recorder writes SSE events durably; nil builds one on Pool and Hub when there is a
	// pool.
	Recorder *events.Recorder
	// FetchTransport is http_fetch's transport; nil means the default.
	FetchTransport http.RoundTripper
	// CheckTransport is the project check's transport (DESIGN 5.17 rule 5); nil means
	// the default.
	CheckTransport http.RoundTripper
	// TemplatesDir holds the workflow templates loaded at start (DESIGN 5.6 rule 9).
	TemplatesDir string
}

type Server struct {
	cfg     config.Config
	log     *slog.Logger
	pool    *pgxpool.Pool
	hub     *events.Hub
	engine  engine.Service
	gateway openclaw.Client
	scripts model.ScriptStore
	tools   *tools.Registry
	effects *tools.Service
	// cipher seals project tokens (DESIGN 5.17 rule 2), nil without PROJECT_TOKEN_KEY;
	// projects resolves a run's project for the tools and the check.
	cipher   *project.Cipher
	projects *project.Resolver
	// plans keeps plan tickets and the wave pause (DESIGN 5.29).
	plans *plans.Service
	// waves watches the open plans and advances shipped waves (DESIGN 5.29 rules 12 and 14).
	waves *waves.Service
	// checkClient talks to the providers for POST /api/projects/{id}/check.
	checkClient *http.Client
	// tokenOpens, when set, replaces the provider check add_project makes before it reuses
	// a token (tests); nil asks the provider.
	tokenOpens func(ctx context.Context, pc project.Config) bool
	rec        *events.Recorder
	// templatesDir is where LoadTemplates reads workflow templates.
	templatesDir string
	startedAt    time.Time
	mux          *http.ServeMux
	// versionLocks are If-Match's per-target locks (DESIGN 5.31 rule 4).
	versionLocks targetLocks

	// botName is the Telegram bot username once known (channels.go).
	botName atomic.Pointer[string]
}

// Gateway dial policy for Run (DESIGN 5.5 rule 1): every failure, including the
// gateway's "starting; retry shortly" refusal while it boots or restarts, is retried
// with backoff from 1 s doubling to 30 s until the process is told to stop.
const (
	gatewayDialBackoffBase = time.Second
	gatewayDialBackoffMax  = 30 * time.Second
)

// Run is the `yuno api` entrypoint: connect, migrate, dial the gateway, start the
// engine and serve until ctx is cancelled.
func Run(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	pool, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := store.Migrate(ctx, pool, log); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	bootstrapProject(ctx, pool, cfg, log)

	hub := events.NewHub()
	rec := &events.Recorder{Hub: hub, Append: func(ctx context.Context, runID, typ string, data json.RawMessage) (int64, error) {
		return store.AppendRunEvent(ctx, pool, runID, typ, data)
	}}

	ctx, stop := context.WithCancel(ctx)
	defer stop()
	// The gateway connection outlives ctx until the engine has stopped, so a shutdown
	// can still abort the turns in flight (the engine does that on its way out). A
	// shutdown while dialling still stops the dial.
	gwCtx, gwStop := context.WithCancel(context.WithoutCancel(ctx))
	defer gwStop()
	stopDialOnShutdown := context.AfterFunc(ctx, gwStop)
	gw, err := openclaw.Dial(gwCtx, openclaw.DialConfig{
		URL:          cfg.OpenClawGatewayURL,
		Token:        cfg.OpenClawGatewayToken,
		Version:      config.Version,
		BackoffBase:  gatewayDialBackoffBase,
		BackoffMax:   gatewayDialBackoffMax,
		RetryRefused: true,
	}, log)
	if err != nil {
		return fmt.Errorf("openclaw gateway: %w", err)
	}
	stopDialOnShutdown()

	// The server comes first: it owns the platform tools, whose executor is the engine's
	// fallback for side effects approved after the MCP wait (DESIGN 5.3 rule 5).
	s, err := New(Deps{
		Cfg:          cfg,
		Log:          log,
		Pool:         pool,
		Hub:          hub,
		Gateway:      gw,
		Scripts:      model.NewMemoryScriptStore(),
		Recorder:     rec,
		TemplatesDir: "templates",
	})
	if err != nil {
		return err
	}
	// Run artifacts (DESIGN 5.25 rules 2 and 4): the collector runs after every agent visit
	// and keeps the files under ARTIFACT_DIR; an unusable directory disables it and the
	// visits run as before. Its event helpers are the engine's, set once it exists.
	engineDeps := engine.Deps{
		Cfg:      cfg,
		Log:      log,
		Pool:     pool,
		Gateway:  gw,
		Hub:      hub,
		Recorder: rec,
		StateDir: cfg.OpenClawStateDir,
		Effects:  s.effects,
		Plans:    s.plans,
	}
	if b, ok := s.scripts.(engine.ScriptBinder); ok {
		engineDeps.Scripts = b
	}
	collector, err := artifacts.New(cfg.ArtifactDir, pool, log, nil, nil, cfg.ArtifactTTLDays)
	if err != nil {
		log.Warn("artifacts: " + cfg.ArtifactDir + ": " + err.Error() + "; artifacts are disabled")
		collector = nil
	} else {
		engineDeps.Artifacts = collector
	}
	eng := engine.New(engineDeps)
	s.plans.Reporter = eng
	go s.plans.Run(ctx) // resends the notices not delivered (DESIGN 5.29 rule 8)
	if collector != nil {
		collector.Emit, collector.LogEvent = eng.Emit, eng.LogEvent
	}
	s.engine = eng
	engineDone := make(chan error, 1)
	go func() { engineDone <- eng.Start(ctx) }()
	// The gateway's agents follow the platform's: stale ones removed at start and after
	// every reconnect, missing or out-of-date ones (a fresh seed) materialised then and
	// every 30 s in between.
	go eng.KeepAgents(ctx)

	// Telegram binding re-asserted at start and after every gateway reconnect (DESIGN
	// 5.8 rules 3 and 7).
	go (&channels.Keeper{Pool: pool, Gateway: gw, Log: log, StateDir: cfg.OpenClawStateDir}).Run(ctx)

	// Scheduler and memory retention (DESIGN 5.9, 5.10 rule 6); both stop with ctx.
	go (&sched.Scheduler{Pool: pool, Engine: eng, Tick: cfg.SchedulerTick, Log: log, Emit: s.emit}).Run(ctx)
	sweeps := []memory.SweepFunc{
		func(ctx context.Context, now time.Time) (int64, error) {
			return store.DeleteExpiredConversationMessages(ctx, pool, now)
		},
		// Idempotency keys expire after 24 hours (DESIGN 5.31 rule 3).
		func(ctx context.Context, now time.Time) (int64, error) {
			return store.SweepIdempotencyKeys(ctx, pool, now)
		},
	}
	if collector != nil {
		sweeps = append(sweeps, collector.Sweep) // DESIGN 5.25 rule 6
	}
	go (&memory.Sweeper{Pool: pool, Log: log, Also: sweeps}).Run(ctx)
	// Project budget alerts (DESIGN 5.19 rule 5) and merge request records refreshed
	// from their provider (DESIGN 5.20 rule 3), the backfill of rule 7 first so the
	// first pass settles the records of runs that predate the table.
	go (&project.Watcher{Pool: pool, Tick: cfg.BudgetTick, Log: log, Reporter: eng}).Run(ctx)
	mr := &mergereq.Sync{Pool: pool, Projects: s.projects, Client: s.checkClient, Reporter: eng,
		Plans: s.plans, Interval: cfg.MergeRequestSyncInterval, Log: log}
	if n, err := mr.Backfill(ctx); err == nil && n > 0 {
		log.Info("merge requests backfilled from the trace", "count", n)
	}
	go mr.Run(ctx)
	// The wave watcher (DESIGN 5.29 rule 12) releases and ships the waves of the open plans.
	s.waves = &waves.Service{Pool: pool, Plans: s.plans, Projects: s.projects, Client: s.checkClient, Engine: eng,
		Interval: cfg.WaveWatchInterval, Log: log}
	go s.waves.Run(ctx)
	// Issue intake (DESIGN 5.23 rule 2): projects with intake enabled are polled for
	// labelled issues and their finished issue runs are posted back on the issue.
	go (&intake.Loop{Pool: pool, Projects: s.projects, Client: s.checkClient, Engine: eng, Plans: s.plans,
		Interval: cfg.IntakePollInterval, Log: log, PublicBaseURL: cfg.PublicBaseURL}).Run(ctx)

	if err := s.LoadTemplates(ctx); err != nil {
		log.Warn("workflow templates not loaded", "error", err)
	}
	s.ImportSkillLibrary(ctx)
	if cfg.MCPToken == "" {
		log.Warn("YUNO_MCP_TOKEN is empty; every MCP request will be refused")
	}
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.ListenAndServe() }()
	go refreshMCPAfterStart(ctx, cfg, gw, log)
	go s.learnBotUsername(ctx)
	// The database watchdog ends the process when the shared network namespace has
	// died under it (see dbWatchdog); the container restart policy starts a fresh one.
	watchdogDone := make(chan error, 1)
	go func() { watchdogDone <- dbWatchdog(ctx, pool.Ping, 5*time.Second, 30*time.Second) }()
	log.Info("api listening", "addr", cfg.ListenAddr, "version", config.Version)

	// Serve until the process is told to stop, the listener fails, or the engine
	// stops on its own (which leaves nothing to run the platform's work).
	var runErr error
	engineStopped := false
	select {
	case <-ctx.Done():
	case err := <-serveDone:
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = fmt.Errorf("listen: %w", err)
		}
	case err := <-watchdogDone:
		runErr = err
		if err != nil {
			log.Error("database watchdog", "error", err)
		}
	case err := <-engineDone:
		engineStopped = true
		if err == nil {
			err = errors.New("stopped unexpectedly")
		}
		runErr = fmt.Errorf("engine: %w", err)
		log.Error("engine stopped", "error", err)
	}
	stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	if !engineStopped {
		select {
		case <-engineDone:
		case <-shutdownCtx.Done():
			log.Warn("engine did not stop within the shutdown window")
		}
	}
	return runErr
}

// New builds the server and its routes.
func New(d Deps) (*Server, error) {
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	if d.Hub == nil {
		d.Hub = events.NewHub()
	}
	if d.Scripts == nil {
		d.Scripts = model.NewMemoryScriptStore()
	}
	s := &Server{
		cfg:       d.Cfg,
		log:       d.Log,
		pool:      d.Pool,
		hub:       d.Hub,
		engine:    d.Engine,
		gateway:   d.Gateway,
		scripts:   d.Scripts,
		startedAt: time.Now(),
	}
	s.rec = d.Recorder
	if s.rec == nil && d.Pool != nil {
		pool := d.Pool
		s.rec = &events.Recorder{Hub: d.Hub, Append: func(ctx context.Context, runID, typ string, data json.RawMessage) (int64, error) {
			return store.AppendRunEvent(ctx, pool, runID, typ, data)
		}}
	}
	s.templatesDir = d.TemplatesDir
	if c, err := project.NewCipher(d.Cfg.ProjectTokenKey); err == nil {
		s.cipher = c
	} else if !errors.Is(err, project.ErrNoKey) {
		return nil, fmt.Errorf("project token key: %w", err)
	}
	s.projects = &project.Resolver{DB: d.Pool, Cipher: s.cipher}
	s.plans = &plans.Service{Pool: d.Pool, Hub: d.Hub, Log: d.Log}
	s.checkClient = tools.ProviderClient(d.CheckTransport)
	s.tools = tools.NewPlatformRegistry(s.platformDeps(d))
	s.effects = &tools.Service{
		Pool:           d.Pool,
		Registry:       s.tools,
		Log:            d.Log,
		Workspace:      s.workspace,
		MaxOutputBytes: d.Cfg.ToolOutputMaxBytes,
		Emit:           s.emit,
	}
	if err := s.routes(d.ModelTransport); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Server) Handler() http.Handler {
	return s.withMiddleware(s.mux)
}

func (s *Server) routes(modelTransport http.RoundTripper) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /openapi.yaml", s.openapi)
	mux.HandleFunc("GET /api/openapi.yaml", s.openapi)
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("GET /api/openclaw/status", s.openclawStatus)
	mux.HandleFunc("GET /api/events", s.eventsAll)
	mux.HandleFunc("GET /api/runs/{id}/events", s.eventsRun)

	mux.HandleFunc("GET /api/agents", s.listAgents)
	mux.HandleFunc("POST /api/agents", s.createAgent)
	mux.HandleFunc("GET /api/agents/{id}", s.getAgent)
	mux.HandleFunc("PUT /api/agents/{id}", s.updateAgent)
	mux.HandleFunc("DELETE /api/agents/{id}", s.deleteAgent)
	mux.HandleFunc("POST /api/agents/{id}/materialize", s.materializeAgent)
	mux.HandleFunc("GET /api/agents/{id}/runtime", s.agentRuntime)
	mux.HandleFunc("GET /api/agents/{id}/files", s.listAgentFiles)
	mux.HandleFunc("PUT /api/agents/{id}/files/{name}", s.putAgentFile)
	mux.HandleFunc("POST /api/agents/{id}/restore", s.restoreAgent)

	mux.HandleFunc("GET /api/tools", s.listTools)

	mux.HandleFunc("GET /api/approvals", s.listApprovals)
	mux.HandleFunc("POST /api/approvals/{id}/approve", s.approveApproval)
	mux.HandleFunc("POST /api/approvals/{id}/reject", s.rejectApproval)
	mux.HandleFunc("POST /api/approvals/{id}/answer", s.answerApproval)

	mux.HandleFunc("GET /api/workflows", s.listWorkflows)
	mux.HandleFunc("POST /api/workflows", s.createWorkflow)
	mux.HandleFunc("POST /api/workflows/validate", s.validateWorkflow)
	mux.HandleFunc("GET /api/workflows/{id}", s.getWorkflow)
	mux.HandleFunc("PUT /api/workflows/{id}", s.updateWorkflow)
	mux.HandleFunc("DELETE /api/workflows/{id}", s.deleteWorkflow)
	mux.HandleFunc("GET /api/workflows/{id}/versions", s.listWorkflowVersions)
	mux.HandleFunc("GET /api/workflows/{id}/versions/{version}", s.getWorkflowVersion)
	mux.HandleFunc("POST /api/workflows/{id}/versions/{version}/restore", s.restoreWorkflowVersion)
	mux.HandleFunc("GET /api/workflow-templates", s.listTemplates)
	mux.HandleFunc("POST /api/workflow-templates/{key}/instantiate", s.instantiateTemplate)

	mux.HandleFunc("GET /api/runs", s.listRuns)
	mux.HandleFunc("POST /api/runs", s.createRun)
	mux.HandleFunc("GET /api/runs/{id}", s.getRun)
	mux.HandleFunc("POST /api/runs/{id}/cancel", s.cancelRun)
	mux.HandleFunc("POST /api/runs/{id}/retry", s.retryRun)
	mux.HandleFunc("GET /api/runs/{id}/trace", s.getTrace)
	mux.HandleFunc("GET /api/runs/{id}/artifacts", s.listArtifacts)
	mux.HandleFunc("GET /api/runs/{id}/artifacts/{artifactId}", s.downloadArtifact)
	// Liveness from evidence (DESIGN 5.12).
	mux.HandleFunc("GET /api/runs/diagnosis", s.listRunDiagnoses)
	mux.HandleFunc("GET /api/runs/{id}/diagnosis", s.getRunDiagnosis)
	mux.HandleFunc("GET /api/runs/{id}/events/page", s.listRunEventsPage)

	mux.HandleFunc("POST /api/evals/scripts", s.registerScript)

	// Schedules, skills and agent memory (DESIGN 5.9, section 6, 5.10 rule 7).
	mux.HandleFunc("GET /api/schedules", s.listSchedules)
	mux.HandleFunc("POST /api/schedules", s.createSchedule)
	mux.HandleFunc("GET /api/schedules/{id}", s.getSchedule)
	mux.HandleFunc("PUT /api/schedules/{id}", s.updateSchedule)
	mux.HandleFunc("DELETE /api/schedules/{id}", s.deleteSchedule)
	mux.HandleFunc("GET /api/skills", s.listSkills)
	mux.HandleFunc("POST /api/skills", s.createSkill)
	mux.HandleFunc("GET /api/skills/{id}", s.getSkill)
	mux.HandleFunc("PUT /api/skills/{id}", s.updateSkill)
	mux.HandleFunc("DELETE /api/skills/{id}", s.deleteSkill)
	// Skill bundles (DESIGN 5.27 rules 6, 7 and 11).
	mux.HandleFunc("POST /api/skills/import", s.importSkillZip)
	mux.HandleFunc("POST /api/skills/import-git", s.importSkillGit)
	mux.HandleFunc("GET /api/skills/{id}/files", s.listSkillFiles)
	mux.HandleFunc("GET /api/skills/{id}/files/{path...}", s.getSkillFile)
	mux.HandleFunc("PUT /api/skills/{id}/files/{path...}", s.putSkillFile)
	mux.HandleFunc("DELETE /api/skills/{id}/files/{path...}", s.deleteSkillFile)
	mux.HandleFunc("GET /api/skills/{id}/export", s.exportSkill)
	// Projects (DESIGN 5.17 rule 3).
	mux.HandleFunc("GET /api/projects", s.listProjects)
	mux.HandleFunc("POST /api/projects", s.createProject)
	mux.HandleFunc("GET /api/projects/{id}", s.getProject)
	mux.HandleFunc("PUT /api/projects/{id}", s.updateProject)
	mux.HandleFunc("DELETE /api/projects/{id}", s.deleteProject)
	mux.HandleFunc("POST /api/projects/{id}/check", s.checkProject)
	mux.HandleFunc("POST /api/projects/{id}/intake/poll", s.pollIntake)
	mux.HandleFunc("GET /api/projects/{id}/plans", s.listPlans)
	mux.HandleFunc("GET /api/plans/{id}", s.getPlan)
	mux.HandleFunc("POST /api/plans/{id}/cancel", s.cancelPlan)
	mux.HandleFunc("POST /api/plans/{id}/tickets/{ticket}/changes", s.requestChanges)
	mux.HandleFunc("POST /api/projects/{id}/waves/pause", s.pauseWaves)
	mux.HandleFunc("POST /api/projects/{id}/waves/resume", s.resumeWaves)
	// Project notes (DESIGN 5.18 rule 6) and merge requests (DESIGN 5.20 rule 6).
	mux.HandleFunc("GET /api/projects/{id}/facts", s.listProjectFacts)
	mux.HandleFunc("DELETE /api/projects/{id}/facts/{factId}", s.deleteProjectFact)
	mux.HandleFunc("GET /api/merge-requests", s.listMergeRequests)
	mux.HandleFunc("GET /api/merge-requests/{id}", s.getMergeRequest)
	mux.HandleFunc("POST /api/merge-requests/{id}/sync", s.syncMergeRequest)
	mux.HandleFunc("POST /api/merge-requests/{id}/merge", s.mergeMergeRequest)
	mux.HandleFunc("GET /api/agents/{id}/memory", s.listAgentMemory)
	mux.HandleFunc("DELETE /api/agents/{id}/memory/{factId}", s.deleteAgentMemory)

	// Channels and conversations (DESIGN 5.8, section 6).
	mux.HandleFunc("GET /api/channels", s.getChannels)
	mux.HandleFunc("PUT /api/channels/telegram/binding", s.bindTelegram)
	mux.HandleFunc("DELETE /api/channels/telegram/binding", s.unbindTelegram)
	mux.HandleFunc("GET /api/conversations", s.listConversations)
	mux.HandleFunc("GET /api/conversations/{id}/messages", s.listConversationMessages)
	mux.HandleFunc("GET /api/conversations/{id}/tool-calls", s.listConversationToolCalls)

	// The audit log (DESIGN 5.31 rule 5).
	mux.HandleFunc("GET /api/audit", s.listAudit)

	mux.Handle("/mcp", s.mcpHandler())

	proxyCfg := model.AnthropicProxyConfig{
		Prefix:        anthropicProxyPrefix,
		BaseURL:       s.cfg.AnthropicBaseURL,
		APIKey:        s.cfg.AnthropicAPIKey,
		RatePerMinute: s.cfg.ModelProxyRatePerMinute,
		Log:           s.log,
		Transport:     modelTransport,
	}
	if s.cfg.ModelUpstream == "openrouter" {
		proxyCfg.BaseURL = s.cfg.OpenRouterBaseURL
		if proxyCfg.BaseURL == "" {
			proxyCfg.BaseURL = model.DefaultOpenRouterBaseURL
		}
		proxyCfg.APIKey = s.cfg.OpenRouterAPIKey
		proxyCfg.AuthScheme = "bearer"
		proxyCfg.ModelMap = model.OpenRouterModelID
	}
	proxy, err := model.NewAnthropicProxy(proxyCfg)
	if err != nil {
		return err
	}
	mux.Handle(anthropicProxyPrefix+"/", loopbackOnly(proxy))
	mux.Handle(scriptedProxyPrefix+"/", loopbackOnly(model.NewScriptedHandler(s.scripts, scriptedProxyPrefix, s.log)))
	s.mux = mux
	return nil
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte("ok"))
}

// readyz is 200 only when the database answers and the gateway connection is up with
// operator.admin (DESIGN 5.14 rule 13).
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	if s.pool == nil || s.pool.Ping(ctx) != nil {
		writeError(w, http.StatusServiceUnavailable, "db_unavailable", "database ping failed", nil)
		return
	}
	if ok, why := s.gatewayReady(); !ok {
		writeError(w, http.StatusServiceUnavailable, "gateway_unavailable", why, nil)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte("ready"))
}

func (s *Server) openapi(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	_, _ = w.Write(api.OpenAPI)
}

type statusResponse struct {
	Version   string           `json:"version"`
	UptimeS   int64            `json:"uptime_s"`
	DBOK      bool             `json:"db_ok"`
	Workers   int              `json:"workers"`
	Runs      map[string]int64 `json:"runs"`
	SSEClient int              `json:"sse_clients"`
	Scheduler schedulerStatus  `json:"scheduler"`
}

// schedulerStatus is how often the scheduler checks and when the next enabled schedule
// is due (null when none is).
type schedulerStatus struct {
	TickSeconds float64    `json:"tick_seconds"`
	NextDueAt   *time.Time `json:"next_due_at"`
}

// status answers within a 500 ms budget (DESIGN 5.12 rule 5).
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 500*time.Millisecond)
	defer cancel()
	tick := s.cfg.SchedulerTick
	if tick <= 0 {
		tick = sched.DefaultTick
	}
	resp := statusResponse{
		Version:   config.Version,
		UptimeS:   int64(time.Since(s.startedAt).Seconds()),
		Workers:   s.cfg.EngineWorkers,
		Runs:      map[string]int64{"queued": 0, "running": 0, "waiting_approval": 0},
		SSEClient: s.hub.Subscribers(),
		Scheduler: schedulerStatus{TickSeconds: tick.Seconds()},
	}
	if s.pool != nil {
		if next, err := store.NextScheduleDue(ctx, s.pool); err == nil && next != nil {
			at := next.UTC()
			resp.Scheduler.NextDueAt = &at
		}
		rows, err := s.pool.Query(ctx, `SELECT status, count(*) FROM runs WHERE status IN ('queued','running','waiting_approval') GROUP BY status`)
		if err == nil {
			resp.DBOK = true
			for rows.Next() {
				var st string
				var n int64
				if rows.Scan(&st, &n) == nil {
					resp.Runs[st] = n
				}
			}
			rows.Close()
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// bootstrapProject creates the project PROJECT_* describe at the first start of an
// install that predates the Projects page (DESIGN 5.17 rule 9). It logs and carries on:
// nothing when there is nothing to do or the project exists, a warning when the
// setting is wrong or PROJECT_TOKEN_KEY is missing.
func bootstrapProject(ctx context.Context, q store.Querier, cfg config.Config, log *slog.Logger) {
	c, err := project.NewCipher(cfg.ProjectTokenKey)
	if err != nil && !errors.Is(err, project.ErrNoKey) {
		log.Warn("project not bootstrapped from PROJECT_*", "error", err)
		return
	}
	p, created, err := project.Bootstrap(ctx, q, c, cfg)
	switch {
	case err != nil:
		log.Warn("project not bootstrapped from PROJECT_*", "error", err)
	case created:
		log.Info("project bootstrapped from PROJECT_*", "project_id", p.ID, "name", p.Name, "repo_url", p.RepoURL)
	}
}

// refreshMCPAfterStart waits until this process answers on its own listener, then asks
// the gateway to rebuild its client for the platform MCP server (DESIGN 5.14 rule 8;
// openclaw.RefreshMCPServer explains why). Failures are logged, not fatal: the gateway
// retries on its own schedule.
func refreshMCPAfterStart(ctx context.Context, cfg config.Config, gw openclaw.Client, log *slog.Logger) {
	addr := cfg.ListenAddr
	if len(addr) > 0 && addr[0] == ':' {
		addr = "127.0.0.1" + addr
	}
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		resp, err := client.Get("http://" + addr + "/healthz")
		if err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if ctx.Err() != nil {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := openclaw.RefreshMCPServer(rctx, gw, openclaw.MCPServerName); err != nil {
		log.Warn("openclaw mcp server refresh failed", "error", err)
		return
	}
	log.Info("openclaw mcp server refreshed", "server", openclaw.MCPServerName)
}

// platformDeps configures the platform tools from the environment (DESIGN 5.7 table).
func (s *Server) platformDeps(d Deps) tools.PlatformDeps {
	cfg := d.Cfg
	pd := tools.PlatformDeps{
		AcceptFinish:   s.acceptFinish,
		RejectFinish:   s.rejectFinish,
		Pool:           d.Pool,
		MemorySaved:    s.memorySaved,
		MaxOutputBytes: cfg.ToolOutputMaxBytes,
		Delegator:      delegator{s},
		// ask_operator and answer_question reach the engine through the server at call
		// time (DESIGN 5.24): Run sets s.engine after this registry is built.
		Asker: asker{s},
		// decide_approval and add_project reach the engine and the project code at call time (D-081).
		DecideApproval: func(ctx context.Context, id, decision, by, reason string) (*store.Approval, error) {
			if s.engine == nil {
				return nil, errEngineUnavailable
			}
			return s.engine.DecideApproval(ctx, id, decision, by, reason)
		},
		AddProject: s.chatAddProject,
		// decide_approval's codes: keyed by the server's own secrets, random when none is set.
		ApprovalCodeKey: approvalCodeKey(cfg),
		// escalate and publish_tickets reach the engine at call time as ask_operator does.
		Escalate: func(ctx context.Context, runID, kind, reason string) error {
			e, ok := s.engine.(interface {
				EscalateRun(ctx context.Context, runID, kind, reason string) error
			})
			if !ok || e == nil {
				return errEngineUnavailable
			}
			return e.EscalateRun(ctx, runID, kind, reason)
		},
		// advance_wave reaches the wave service at call time: Run sets s.waves after this registry is built.
		AdvanceWave: func(ctx context.Context, runID string) (tools.AdvanceResult, error) {
			if s.waves == nil {
				return tools.AdvanceResult{}, errEngineUnavailable
			}
			return s.waves.Advance(ctx, runID)
		},
		RunIDForToken: func(token string) (string, bool) {
			if s.engine == nil {
				return "", false
			}
			rc, ok := s.engine.ResolveRunToken(token)
			return rc.RunID, ok
		},
		PublicBaseURL: cfg.PublicBaseURL,
		MCPToolWait:   cfg.MCPToolWait,
		Deploy: tools.DeployConfig{
			TargetURL:       cfg.DeployTargetURL,
			MaxBytes:        int64(cfg.DeployTargetMaxMB) << 20,
			MaxRequestBytes: deploytarget.MaxBodyBytes,
			PublicBaseURL:   cfg.PublicBaseURL,
		},
		Fetch: tools.FetchConfig{
			Allowlist:      cfg.HTTPFetchAllowlist,
			MaxOutputBytes: cfg.ToolOutputMaxBytes,
			Transport:      d.FetchTransport,
		},
	}
	if cfg.GitLabEnabled() {
		pd.GitLab = &tools.GitLabConfig{
			BaseURL: cfg.GitLabBaseURL, ProjectID: cfg.GitLabProjectID, Token: cfg.GitLabToken,
			MaxBytes: int64(cfg.DeployTargetMaxMB) << 20,
		}
	}
	// open_merge_request takes the project from the calling run (DESIGN 5.17 rule 6).
	if d.Pool != nil {
		pd.Projects = s.projects
		pd.ProviderClient = s.checkClient
	}
	return pd
}

// workspace is an agent's OpenClaw workspace as the api sees it (the state directory
// is mounted at the same path in both containers).
func (s *Server) workspace(openclawAgentID string) string {
	if openclawAgentID == "" {
		return ""
	}
	return openclaw.WorkspaceDir(s.cfg.OpenClawStateDir, openclawAgentID)
}

// emit records an SSE event durably and fans it out; failures are logged.
func (s *Server) emit(ctx context.Context, runID, typ string, v any) {
	if s.rec == nil {
		return
	}
	if _, err := s.rec.Emit(context.WithoutCancel(ctx), runID, typ, v); err != nil {
		s.log.Warn("emit event failed", "run_id", runID, "event", typ, "error", err)
	}
}

// approvalCodeKey is the key of the codes decide_approval requires: the project token key
// and the API token, whichever are set; nil (a random key per process) when neither is.
func approvalCodeKey(cfg config.Config) []byte {
	if cfg.ProjectTokenKey == "" && cfg.APIToken == "" {
		return nil
	}
	return []byte("approval-code\x00" + cfg.ProjectTokenKey + "\x00" + cfg.APIToken)
}

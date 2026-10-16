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
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"yuno/api"
	"yuno/internal/channels"
	"yuno/internal/config"
	"yuno/internal/deploytarget"
	"yuno/internal/engine"
	"yuno/internal/events"
	"yuno/internal/memory"
	"yuno/internal/model"
	"yuno/internal/openclaw"
	"yuno/internal/sched"
	"yuno/internal/store"
	"yuno/internal/tools"
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
	rec     *events.Recorder
	// templatesDir is where LoadTemplates reads workflow templates.
	templatesDir string
	startedAt    time.Time
	mux          *http.ServeMux
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
	eng := engine.New(engine.Deps{
		Cfg:      cfg,
		Log:      log,
		Pool:     pool,
		Gateway:  gw,
		Hub:      hub,
		Recorder: rec,
		StateDir: cfg.OpenClawStateDir,
		Effects:  s.effects,
	})
	s.engine = eng
	engineDone := make(chan error, 1)
	go func() { engineDone <- eng.Start(ctx) }()

	// Telegram binding re-asserted at start and after every gateway reconnect (DESIGN
	// 5.8 rules 3 and 7).
	go (&channels.Keeper{Pool: pool, Gateway: gw, Log: log}).Run(ctx)

	// Scheduler and memory retention (DESIGN 5.9, 5.10 rule 6); both stop with ctx.
	go (&sched.Scheduler{Pool: pool, Engine: eng, Tick: cfg.SchedulerTick, Log: log, Emit: s.emit}).Run(ctx)
	go (&memory.Sweeper{Pool: pool, Log: log, Also: []memory.SweepFunc{
		func(ctx context.Context, now time.Time) (int64, error) {
			return store.DeleteExpiredConversationMessages(ctx, pool, now)
		},
	}}).Run(ctx)

	if err := s.LoadTemplates(ctx); err != nil {
		log.Warn("workflow templates not loaded", "error", err)
	}
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

	mux.HandleFunc("GET /api/tools", s.listTools)

	mux.HandleFunc("GET /api/approvals", s.listApprovals)
	mux.HandleFunc("POST /api/approvals/{id}/approve", s.approveApproval)
	mux.HandleFunc("POST /api/approvals/{id}/reject", s.rejectApproval)

	mux.HandleFunc("GET /api/workflows", s.listWorkflows)
	mux.HandleFunc("POST /api/workflows", s.createWorkflow)
	mux.HandleFunc("POST /api/workflows/validate", s.validateWorkflow)
	mux.HandleFunc("GET /api/workflows/{id}", s.getWorkflow)
	mux.HandleFunc("PUT /api/workflows/{id}", s.updateWorkflow)
	mux.HandleFunc("DELETE /api/workflows/{id}", s.deleteWorkflow)
	mux.HandleFunc("GET /api/workflow-templates", s.listTemplates)
	mux.HandleFunc("POST /api/workflow-templates/{key}/instantiate", s.instantiateTemplate)

	mux.HandleFunc("GET /api/runs", s.listRuns)
	mux.HandleFunc("POST /api/runs", s.createRun)
	mux.HandleFunc("GET /api/runs/{id}", s.getRun)
	mux.HandleFunc("POST /api/runs/{id}/cancel", s.cancelRun)
	mux.HandleFunc("GET /api/runs/{id}/trace", s.getTrace)

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
	mux.HandleFunc("GET /api/agents/{id}/memory", s.listAgentMemory)
	mux.HandleFunc("DELETE /api/agents/{id}/memory/{factId}", s.deleteAgentMemory)

	// Channels and conversations (DESIGN 5.8, section 6).
	mux.HandleFunc("GET /api/channels", s.getChannels)
	mux.HandleFunc("PUT /api/channels/telegram/binding", s.bindTelegram)
	mux.HandleFunc("DELETE /api/channels/telegram/binding", s.unbindTelegram)
	mux.HandleFunc("GET /api/conversations", s.listConversations)
	mux.HandleFunc("GET /api/conversations/{id}/messages", s.listConversationMessages)

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
		AcceptFinish: s.acceptFinish,
		RejectFinish: s.rejectFinish,
		Pool:         d.Pool,
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

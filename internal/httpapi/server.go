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
	"yuno/internal/config"
	"yuno/internal/engine"
	"yuno/internal/events"
	"yuno/internal/model"
	"yuno/internal/openclaw"
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
}

type Server struct {
	cfg       config.Config
	log       *slog.Logger
	pool      *pgxpool.Pool
	hub       *events.Hub
	engine    engine.Service
	gateway   openclaw.Client
	scripts   model.ScriptStore
	tools     *tools.Registry
	startedAt time.Time
	mux       *http.ServeMux
}

// Gateway dial policy for Run: transport failures are retried every 2 s for up to 60 s.
const (
	gatewayDialWindow  = 60 * time.Second
	gatewayDialBackoff = 2 * time.Second
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
	// shutdown during the dial window still stops the dial.
	gwCtx, gwStop := context.WithCancel(context.WithoutCancel(ctx))
	defer gwStop()
	stopDialOnShutdown := context.AfterFunc(ctx, gwStop)
	gw, err := dialWithin(gwCtx, gatewayDialWindow, gatewayDialBackoff, func(ctx context.Context) (openclaw.Client, error) {
		return openclaw.Dial(ctx, openclaw.DialConfig{
			URL:         cfg.OpenClawGatewayURL,
			Token:       cfg.OpenClawGatewayToken,
			Version:     config.Version,
			BackoffBase: gatewayDialBackoff,
			BackoffMax:  gatewayDialBackoff,
		}, log)
	})
	if err != nil {
		return err
	}
	stopDialOnShutdown()

	eng := engine.New(engine.Deps{
		Cfg:      cfg,
		Log:      log,
		Pool:     pool,
		Gateway:  gw,
		Hub:      hub,
		Recorder: rec,
		StateDir: cfg.OpenClawStateDir,
	})
	engineDone := make(chan error, 1)
	go func() { engineDone <- eng.Start(ctx) }()

	s, err := New(Deps{
		Cfg:     cfg,
		Log:     log,
		Pool:    pool,
		Hub:     hub,
		Engine:  eng,
		Gateway: gw,
		Scripts: model.NewMemoryScriptStore(),
	})
	if err != nil {
		return err
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

// dialWithin calls dial with a context that outlives a successful dial (the client
// keeps its connection on it) but is cancelled when no dial has succeeded within
// window. Failed dials are retried after backoff; a handshake the gateway refused is
// returned at once because retrying with the same token cannot help.
func dialWithin(ctx context.Context, window, backoff time.Duration, dial func(context.Context) (openclaw.Client, error)) (openclaw.Client, error) {
	connCtx, cancel := context.WithCancel(ctx)
	timer := time.AfterFunc(window, cancel)
	var lastErr error
	for {
		c, err := dial(connCtx)
		if err == nil {
			if !timer.Stop() {
				cancel()
				return nil, fmt.Errorf("openclaw gateway: no connection within %s", window)
			}
			// connCtx now lives as long as ctx; cancelling it would close the connection.
			context.AfterFunc(ctx, cancel)
			return c, nil
		}
		lastErr = err
		var he *openclaw.HandshakeError
		if errors.As(err, &he) {
			timer.Stop()
			cancel()
			return nil, fmt.Errorf("openclaw gateway: %w", err)
		}
		select {
		case <-connCtx.Done():
			cancel()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("openclaw gateway: no connection within %s: %w", window, lastErr)
		case <-time.After(backoff):
		}
	}
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
	s.tools = tools.NewPlatformRegistry(tools.PlatformDeps{AcceptFinish: s.acceptFinish, RejectFinish: s.rejectFinish})
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

	mux.HandleFunc("GET /api/runs", s.listRuns)
	mux.HandleFunc("POST /api/runs", s.createRun)
	mux.HandleFunc("GET /api/runs/{id}", s.getRun)
	mux.HandleFunc("POST /api/runs/{id}/cancel", s.cancelRun)
	mux.HandleFunc("GET /api/runs/{id}/trace", s.getTrace)

	mux.HandleFunc("POST /api/evals/scripts", s.registerScript)

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
}

// status answers within a 500 ms budget (DESIGN 5.12 rule 5).
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 500*time.Millisecond)
	defer cancel()
	resp := statusResponse{
		Version:   config.Version,
		UptimeS:   int64(time.Since(s.startedAt).Seconds()),
		Workers:   s.cfg.EngineWorkers,
		Runs:      map[string]int64{"queued": 0, "running": 0, "waiting_approval": 0},
		SSEClient: s.hub.Subscribers(),
	}
	if s.pool != nil {
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

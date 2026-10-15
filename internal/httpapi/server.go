// Package httpapi is the HTTP layer: routes, middleware, SSE. It depends on the store
// and engine packages, never the other way round.
package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"yuno/api"
	"yuno/internal/config"
	"yuno/internal/events"
	"yuno/internal/store"
)

type Server struct {
	cfg       config.Config
	log       *slog.Logger
	pool      *pgxpool.Pool
	hub       *events.Hub
	startedAt time.Time
	mux       *http.ServeMux
}

// Run is the `yuno api` entrypoint: connect, migrate, serve until ctx is cancelled.
func Run(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	pool, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := store.Migrate(ctx, pool, log); err != nil {
		return err
	}

	s := New(cfg, log, pool, events.NewHub())
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	log.Info("api listening", "addr", cfg.ListenAddr, "version", config.Version)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func New(cfg config.Config, log *slog.Logger, pool *pgxpool.Pool, hub *events.Hub) *Server {
	s := &Server{cfg: cfg, log: log, pool: pool, hub: hub, startedAt: time.Now()}
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler {
	return s.withMiddleware(s.mux)
}

func (s *Server) routes() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /openapi.yaml", s.openapi)
	mux.HandleFunc("GET /api/openapi.yaml", s.openapi)
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("GET /api/events", s.eventsAll)
	mux.HandleFunc("GET /api/runs/{id}/events", s.eventsRun)
	s.mux = mux
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	if err := s.pool.Ping(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "db_unavailable", "database ping failed", nil)
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
	writeJSON(w, http.StatusOK, resp)
}

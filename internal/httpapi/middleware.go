package httpapi

import (
	"crypto/subtle"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/google/uuid"

	"yuno/internal/obs"
)

func (s *Server) withMiddleware(next http.Handler) http.Handler {
	// The platform gate sits after auth (and the audit's actor and note, DESIGN B6) and
	// before idempotency.
	return s.recoverer(s.requestID(s.logging(s.auth(s.platformGate(next)))))
}

func (s *Server) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" || len(id) > 64 {
			id = uuid.NewString()
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(obs.WithRequestID(r.Context(), id)))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush keeps SSE working through the recorder.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			return
		}
		s.log.Info("http",
			"request_id", obs.RequestID(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic", "request_id", obs.RequestID(r.Context()), "panic", rec, "stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, "internal", "internal error", nil)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// auth enforces a bearer token on /api/* when API_TOKEN is set. Health, readiness,
// the OpenAPI document and webhooks (which have their own verification) are open.
func (s *Server) auth(next http.Handler) http.Handler {
	if s.cfg.APIToken == "" {
		return next
	}
	want := []byte(s.cfg.APIToken)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api/openapi.yaml" {
			next.ServeHTTP(w, r)
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Model proxy mounts (DESIGN 5.14 rules 8 and 9). The gateway's provider config points
// its baseUrl at these paths on 127.0.0.1:8080.
const (
	anthropicProxyPrefix = "/model-proxy/anthropic"
	scriptedProxyPrefix  = "/model-proxy/scripted/v1"
)

// loopbackOnly admits only callers on a loopback address. The model proxy carries no
// bearer: the gateway shares the api's network namespace and is its only expected
// caller, while other containers reach the api as openclaw:8080 from a non-loopback
// address and must not be able to spend the Anthropic key.
func loopbackOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			writeError(w, http.StatusForbidden, "forbidden", "the model proxy accepts loopback callers only", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

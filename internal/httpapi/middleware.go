package httpapi

import (
	"context"
	"crypto/subtle"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/google/uuid"

	"yuno/internal/actor"
	"yuno/internal/obs"
)

// withMiddleware is the chain of DESIGN 5.31 rule 6: recoverer, requestID, logging, auth,
// actor, audit, platform gate, idempotency, If-Match (versions, which also sets the GET
// ETags), then the routes. auth only judges the bearer; its refusal is answered inside the
// audit (refuseUnauthorized), so a refused request gets its row (rule 1).
func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return s.recoverer(s.requestID(s.logging(s.auth(s.withActor(s.audit(s.refuseUnauthorized(
		s.platformGate(s.idempotency(s.versions(next))))))))))
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

// authVerdict is what auth decided about a request's bearer.
type authVerdict struct {
	// valid: the bearer is the API token, or API_TOKEN is unset.
	valid bool
	// refused: the path needs the bearer and valid is false.
	refused bool
}

type authKey struct{}

func authFrom(ctx context.Context) authVerdict {
	v, _ := ctx.Value(authKey{}).(authVerdict)
	return v
}

// auth enforces a bearer token on /api/* when API_TOKEN is set. Health, readiness,
// the OpenAPI document and webhooks (which have their own verification) are open. It
// records its verdict for the actor and the audit; refuseUnauthorized answers the 401
// once the audit has opened the request's row (DESIGN 5.31 rules 1 and 6).
func (s *Server) auth(next http.Handler) http.Handler {
	want := []byte(s.cfg.APIToken)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := authVerdict{valid: true}
		if len(want) > 0 {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			v.valid = subtle.ConstantTimeCompare([]byte(got), want) == 1
			v.refused = !v.valid && strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/openapi.yaml"
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authKey{}, v)))
	})
}

// refuseUnauthorized answers 401 to a request auth refused.
func (s *Server) refuseUnauthorized(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authFrom(r.Context()).refused {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withActor names who makes a request from the network (DESIGN 5.31 rule 6, and the
// actor rule of 5.15): the operator when the bearer is valid (via "ui" when the UI sent
// X-Yuno-Client: web, "api" otherwise), anonymous when it is not. A request whose context
// already carries an actor (an agent's in-process dispatch) keeps it: no header can set one.
func (s *Server) withActor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := actor.From(r.Context()); !ok {
			a := actor.Actor{Kind: actor.Anonymous, Via: actor.ViaAPI}
			if r.Header.Get("X-Yuno-Client") == "web" {
				a.Via = actor.ViaUI
			}
			if authFrom(r.Context()).valid {
				a.Kind = actor.Operator
			}
			r = r.WithContext(actor.With(r.Context(), a))
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

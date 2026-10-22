package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"yuno/internal/actor"
	"yuno/internal/obs"
	"yuno/internal/store"
)

const (
	// idempotencyKeyMax is the longest Idempotency-Key (DESIGN 5.31 rule 3).
	idempotencyKeyMax = 200
	// idempotencyBodyMax is the longest response kept for a replay; a longer one is replayed
	// with its status and headers only.
	idempotencyBodyMax = 1 << 20
)

// validIdempotencyKey: 1 to 200 printable ASCII characters.
func validIdempotencyKey(k string) bool {
	if len(k) < 1 || len(k) > idempotencyKeyMax {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x20 || k[i] > 0x7e {
			return false
		}
	}
	return true
}

// actorKey is whose key it is: the actor's kind, and its id when it has one ("operator",
// "agent:<id>").
func actorKey(a actor.Actor) string {
	if a.ID == "" {
		return string(a.Kind)
	}
	return string(a.Kind) + ":" + a.ID
}

// idempotency makes a mutating /api request with an Idempotency-Key repeat-safe (DESIGN
// 5.31 rule 3). The first use runs the handler and keeps a response below 500; the same key
// and body again replays it with Idempotency-Replayed: true (the audit outcome is
// "replayed"); the same key with another body is refused 422 idempotency_key_reused, and
// while the first is still running 409 request_in_progress. A request without the header
// runs as before. A server without a database (tests) keeps no keys.
func (s *Server) idempotency(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if s.pool == nil || key == "" || !isMutation(r.Method) || !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		if !validIdempotencyKey(key) {
			writeValidation(w, "invalid Idempotency-Key", fieldErrors{{Path: "Idempotency-Key",
				Message: "must be 1 to 200 printable ASCII characters"}})
			return
		}
		ctx := r.Context()
		a, _ := actor.From(ctx)
		var body []byte
		body, r = requestBody(r)
		sum := sha256.Sum256(body)
		k := store.IdempotencyKey{Actor: actorKey(a), Method: r.Method, Path: r.URL.EscapedPath(), Key: key,
			BodySHA256: hex.EncodeToString(sum[:])}
		claimed, held, err := store.ClaimIdempotencyKey(ctx, s.pool, k, time.Now())
		if err != nil {
			s.internalError(w, r, "claim idempotency key", err)
			return
		}
		if !claimed {
			switch {
			case held.BodySHA256 != k.BodySHA256:
				writeError(w, http.StatusUnprocessableEntity, "idempotency_key_reused",
					"this Idempotency-Key was already used for a different request; send a new key for a new action", nil)
			case held.State != store.IdempotencyDone:
				writeError(w, http.StatusConflict, "request_in_progress",
					"the request with this Idempotency-Key is still running; repeat it once it has finished", nil)
			default:
				replay(w, r, held)
			}
			return
		}
		bg := context.WithoutCancel(ctx)
		release := func() {
			if err := store.ReleaseIdempotencyKey(bg, s.pool, k.Actor, k.Method, k.Path, k.Key); err != nil {
				s.log.Error("idempotency key release failed", "request_id", obs.RequestID(ctx), "error", err)
			}
		}
		rec := &captureWriter{ResponseWriter: w, limit: idempotencyBodyMax}
		finished := false
		defer func() {
			// A handler that panicked released nothing it could replay: the caller may retry.
			if !finished {
				release()
			}
		}()
		next.ServeHTTP(rec, r)
		finished = true
		status := rec.statusCode()
		if status >= 500 {
			release()
			return
		}
		k.Status = status
		if !rec.overflow {
			k.Body = append([]byte{}, rec.body.Bytes()...)
		}
		h := rec.Header()
		k.ContentType, k.ETag, k.Location = h.Get("Content-Type"), h.Get("ETag"), h.Get("Location")
		// A key whose response could not be stored stays running (409 to a repeat) until
		// the sweep: the action happened, so running it again is the wrong direction.
		if err := store.CompleteIdempotencyKey(bg, s.pool, k); err != nil {
			s.log.Error("idempotency key completion failed", "request_id", obs.RequestID(ctx), "error", err)
		}
	})
}

// replay answers with the response a key's first use got, and tells the audit.
func replay(w http.ResponseWriter, r *http.Request, k *store.IdempotencyKey) {
	h := w.Header()
	for name, v := range map[string]string{"Content-Type": k.ContentType, "ETag": k.ETag, "Location": k.Location} {
		if v != "" {
			h.Set(name, v)
		}
	}
	h.Set("Idempotency-Replayed", "true")
	if n := actor.NoteFrom(r.Context()); n != nil {
		n.Outcome = store.AuditReplayed
	}
	w.WriteHeader(k.Status)
	_, _ = w.Write(k.Body)
}

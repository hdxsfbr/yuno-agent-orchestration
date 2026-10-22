package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"yuno/internal/actor"
	"yuno/internal/store"
)

const testRunBody = `{"agent_id":"ag","task":"t","model":"scripted/scripted-1","script_id":"c1"}`

// DESIGN 5.31 rule 3: the same key and body replays the stored response, marked, once per
// actor; the audit records the repeat as replayed.
func TestIdempotencyReplay(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	fe := &fakeEngine{pool: pool}
	s, _ := newTestServer(t, pool, fe, nil)
	h := s.Handler()
	key := map[string]string{"Idempotency-Key": "replay-" + uniqueSuffix()}

	code, hdr, first := send(t, h, ctx, http.MethodPost, "/api/runs", testRunBody, key)
	if code != http.StatusCreated || hdr.Get("Idempotency-Replayed") != "" {
		t.Fatalf("first: %d %v %s", code, hdr, first)
	}
	code, hdr, again := send(t, h, ctx, http.MethodPost, "/api/runs", testRunBody, key)
	if code != http.StatusCreated || hdr.Get("Idempotency-Replayed") != "true" || string(again) != string(first) ||
		hdr.Get("Content-Type") != "application/json" {
		t.Fatalf("repeat: %d %v %s", code, hdr, again)
	}
	if len(fe.triggers) != 1 {
		t.Fatalf("triggers = %d, want 1", len(fe.triggers))
	}
	var replayed []store.AuditEvent
	for _, e := range auditRows(t, "/api/runs") {
		if e.IdempotencyKey == key["Idempotency-Key"] {
			replayed = append(replayed, e)
		}
	}
	if len(replayed) != 2 || replayed[0].Outcome != store.AuditOK || replayed[1].Outcome != store.AuditReplayed ||
		*replayed[1].Status != http.StatusCreated {
		t.Fatalf("audit rows = %+v", replayed)
	}
	// Keys are per actor: the Chief's use of the same key is its own first use.
	code, hdr, _ = send(t, h, actor.With(ctx, chiefActor("chief-"+uniqueSuffix())), http.MethodPost, "/api/runs", testRunBody, key)
	if code != http.StatusCreated || hdr.Get("Idempotency-Replayed") != "" || len(fe.triggers) != 2 {
		t.Fatalf("another actor: %d %v, triggers %d", code, hdr, len(fe.triggers))
	}
	// A request without a key runs every time.
	send(t, h, ctx, http.MethodPost, "/api/runs", testRunBody, nil)
	send(t, h, ctx, http.MethodPost, "/api/runs", testRunBody, nil)
	if len(fe.triggers) != 4 {
		t.Fatalf("triggers without a key = %d, want 4", len(fe.triggers))
	}
}

// DESIGN 5.31 rule 3: the same key with another body is refused 422 and runs nothing; a
// malformed key is refused 422; an expired key is free again.
func TestIdempotencyMismatch(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	fe := &fakeEngine{pool: pool}
	s, _ := newTestServer(t, pool, fe, nil)
	h := s.Handler()
	k := "mismatch-" + uniqueSuffix()
	key := map[string]string{"Idempotency-Key": k}
	if code, _, _ := send(t, h, ctx, http.MethodPost, "/api/runs", testRunBody, key); code != http.StatusCreated {
		t.Fatalf("first: %d", code)
	}
	other := strings.Replace(testRunBody, `"task":"t"`, `"task":"another"`, 1)
	code, _, body := send(t, h, ctx, http.MethodPost, "/api/runs", other, key)
	if code != http.StatusUnprocessableEntity || decodeError(t, body).Error.Code != "idempotency_key_reused" || len(fe.triggers) != 1 {
		t.Fatalf("other body: %d %s, triggers %d", code, body, len(fe.triggers))
	}
	// A header may carry bytes Postgres text refuses (invalid UTF-8): the audit row still
	// takes them, so the answer is the 422, not a 503.
	for _, bad := range []string{strings.Repeat("k", 201), "clé", "a\tb", "cl\xe9"} {
		code, _, body := send(t, h, ctx, http.MethodPost, "/api/runs", testRunBody,
			map[string]string{"Idempotency-Key": bad, "X-Request-Id": "req-\xe9"})
		if code != http.StatusUnprocessableEntity || decodeError(t, body).Error.Code != "validation_failed" {
			t.Fatalf("key %q: %d %s", bad, code, body)
		}
	}
	if code, _, _ := send(t, h, ctx, http.MethodPost, "/api/runs", testRunBody, map[string]string{"Idempotency-Key": strings.Repeat("k", 200)}); code != http.StatusCreated {
		t.Fatalf("a 200-character key: %d", code)
	}
	// 24 hours on, the key is unused: the other body runs.
	_, err := pool.Exec(ctx, `UPDATE idempotency_keys SET expires_at = now() - interval '1 second' WHERE key = $1`, k)
	mustDo(t, err)
	if code, hdr, body := send(t, h, ctx, http.MethodPost, "/api/runs", other, key); code != http.StatusCreated || hdr.Get("Idempotency-Replayed") != "" {
		t.Fatalf("expired key: %d %s", code, body)
	}
}

// DESIGN 5.31 rule 3: a repeat while the first request runs is refused 409; once it ends,
// the repeat replays it.
func TestIdempotencyInProgress(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	s, _ := newTestServer(t, pool, nil, nil)
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	h := s.withMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		writeJSON(w, http.StatusCreated, map[string]string{"id": "slow"})
	}))
	key := map[string]string{"Idempotency-Key": "progress-" + uniqueSuffix()}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if code, _, body := send(t, h, ctx, http.MethodPost, "/api/runs", `{}`, key); code != http.StatusCreated {
			t.Errorf("first: %d %s", code, body)
		}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the first request did not start")
	}
	code, _, body := send(t, h, ctx, http.MethodPost, "/api/runs", `{}`, key)
	close(release)
	wg.Wait()
	if code != http.StatusConflict || decodeError(t, body).Error.Code != "request_in_progress" {
		t.Fatalf("repeat while running: %d %s", code, body)
	}
	code, hdr, body := send(t, h, ctx, http.MethodPost, "/api/runs", `{}`, key)
	if code != http.StatusCreated || hdr.Get("Idempotency-Replayed") != "true" || calls.Load() != 1 {
		t.Fatalf("repeat after: %d %s, calls %d", code, body, calls.Load())
	}
}

// DESIGN 5.31 rule 3: a 5xx (or a panic) is not stored, so the same key runs again.
func TestIdempotency5xxNotStored(t *testing.T) {
	pool := needDB(t)
	ctx := context.Background()
	s, _ := newTestServer(t, pool, nil, nil)
	var calls atomic.Int32
	h := s.withMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			writeError(w, http.StatusBadGateway, "provider_unavailable", "the provider did not answer", nil)
		case 2:
			panic("handler failed")
		default:
			writeJSON(w, http.StatusOK, map[string]int32{"call": calls.Load()})
		}
	}))
	key := map[string]string{"Idempotency-Key": "fivexx-" + uniqueSuffix()}
	for i, want := range []int{http.StatusBadGateway, http.StatusInternalServerError, http.StatusOK} {
		code, hdr, body := send(t, h, ctx, http.MethodPost, "/api/runs/r1/cancel", `{}`, key)
		if code != want || hdr.Get("Idempotency-Replayed") != "" {
			t.Fatalf("attempt %d: %d %v %s", i+1, code, hdr, body)
		}
	}
	code, hdr, body := send(t, h, ctx, http.MethodPost, "/api/runs/r1/cancel", `{}`, key)
	var got map[string]int32
	_ = json.Unmarshal(body, &got)
	if code != http.StatusOK || hdr.Get("Idempotency-Replayed") != "true" || got["call"] != 3 || calls.Load() != 3 {
		t.Fatalf("after a success: %d %s, calls %d", code, body, calls.Load())
	}
	// The panicked attempt's audit row is completed as a 500, not left started.
	var outcomes []string
	for _, e := range auditRows(t, "/api/runs/r1/cancel") {
		if e.IdempotencyKey == key["Idempotency-Key"] {
			outcomes = append(outcomes, fmt.Sprintf("%s:%d", e.Outcome, *e.Status))
		}
	}
	if strings.Join(outcomes, ",") != "failed:502,failed:500,ok:200,replayed:200" {
		t.Fatalf("audit outcomes = %v", outcomes)
	}
}

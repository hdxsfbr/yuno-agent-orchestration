package model

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const testKey = "sk-ant-test-0123456789abcdef"

// upstreamCapture records what the proxy sent upstream.
type upstreamCapture struct {
	mu     sync.Mutex
	path   string
	query  string
	header http.Header
	body   []byte
}

func newUpstream(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *upstreamCapture) {
	t.Helper()
	c := &upstreamCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.path, c.query, c.header, c.body = r.URL.Path, r.URL.RawQuery, r.Header.Clone(), b
		c.mu.Unlock()
		if handler != nil {
			handler(w, r)
			return
		}
		w.Header().Set("Request-Id", "req_test_1")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message"}`)
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

func newProxy(t *testing.T, cfg AnthropicProxyConfig) http.Handler {
	t.Helper()
	if cfg.Prefix == "" {
		cfg.Prefix = "/model-proxy/anthropic"
	}
	h, err := NewAnthropicProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestProxyInjectsKeyAndVersion(t *testing.T) {
	up, got := newUpstream(t, nil)
	var logs bytes.Buffer
	h := newProxy(t, AnthropicProxyConfig{BaseURL: up.URL, APIKey: testKey, Log: slog.New(slog.NewTextHandler(&logs, nil))})
	tests := []struct {
		name        string
		header      map[string]string
		wantVersion string
	}{
		{name: "placeholder x-api-key, no version", header: map[string]string{"X-Api-Key": "proxy"}, wantVersion: DefaultAnthropicVersion},
		{name: "bearer and explicit version", header: map[string]string{"Authorization": "Bearer proxy", "Anthropic-Version": "2024-01-01", "Anthropic-Beta": "foo-2026-01-01"}, wantVersion: "2024-01-01"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/model-proxy/anthropic/v1/messages?beta=true", strings.NewReader(`{"model":"claude-opus-5-5","max_tokens":10,"messages":[]}`))
			for k, v := range tc.header {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			if got.path != "/v1/messages" || got.query != "beta=true" {
				t.Fatalf("upstream path %q query %q", got.path, got.query)
			}
			if k := got.header.Get("X-Api-Key"); k != testKey {
				t.Fatalf("x-api-key = %q", k)
			}
			if a := got.header.Get("Authorization"); a != "" {
				t.Fatalf("authorization forwarded: %q", a)
			}
			if v := got.header.Get("Anthropic-Version"); v != tc.wantVersion {
				t.Fatalf("anthropic-version = %q, want %q", v, tc.wantVersion)
			}
			if b := tc.header["Anthropic-Beta"]; b != "" && got.header.Get("Anthropic-Beta") != b {
				t.Fatalf("anthropic-beta not forwarded")
			}
		})
	}
	line := logs.String()
	if !strings.Contains(line, "request_id=req_test_1") || !strings.Contains(line, "model=claude-opus-5-5") ||
		!strings.Contains(line, "status=200") || !strings.Contains(line, "duration_ms=") {
		t.Fatalf("log line missing fields: %s", line)
	}
	if strings.Contains(line, testKey) {
		t.Fatal("log contains the API key")
	}
}

func TestProxyThinkingShaping(t *testing.T) {
	up, got := newUpstream(t, nil)
	h := newProxy(t, AnthropicProxyConfig{BaseURL: up.URL, APIKey: testKey})
	tests := []struct {
		name         string
		body         string
		wantThinking string // "" means absent
		wantVerbatim bool
	}{
		{name: "display and budget dropped", body: `{"model":"m","thinking":{"type":"enabled","budget_tokens":2048,"display":"summarized"},"messages":[{"role":"user","content":"<b>&</b>"}]}`, wantThinking: `{"type":"adaptive"}`},
		{name: "adaptive with display", body: `{"model":"m","thinking":{"type":"adaptive","display":"summarized"}}`, wantThinking: `{"type":"adaptive"}`},
		{name: "absent stays absent", body: `{"model":"m",  "messages":[]}`, wantVerbatim: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/model-proxy/anthropic/v1/messages", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			if tc.wantVerbatim {
				if string(got.body) != tc.body {
					t.Fatalf("body changed: %s", got.body)
				}
				return
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(got.body, &fields); err != nil {
				t.Fatalf("upstream body not JSON: %v", err)
			}
			if string(fields["thinking"]) != tc.wantThinking {
				t.Fatalf("thinking = %s, want %s", fields["thinking"], tc.wantThinking)
			}
			if _, ok := fields["messages"]; strings.Contains(tc.body, "messages") && !ok {
				t.Fatal("messages dropped")
			}
		})
	}
}

func TestProxyMissingKey(t *testing.T) {
	up, got := newUpstream(t, nil)
	h := newProxy(t, AnthropicProxyConfig{BaseURL: up.URL})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/model-proxy/anthropic/v1/messages", strings.NewReader(`{}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d", rec.Code)
	}
	assertAnthropicError(t, rec.Body.Bytes(), "authentication_error")
	if got.path != "" {
		t.Fatal("request reached upstream without a key")
	}
}

func TestProxyRateLimit(t *testing.T) {
	up, _ := newUpstream(t, nil)
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	h := newProxy(t, AnthropicProxyConfig{BaseURL: up.URL, APIKey: testKey, RatePerMinute: 2, Now: clock})
	call := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/model-proxy/anthropic/v1/models", nil))
		return rec
	}
	for i := 0; i < 2; i++ {
		if rec := call(); rec.Code != http.StatusOK {
			t.Fatalf("request %d: status %d", i+1, rec.Code)
		}
	}
	rec := call()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third request: status %d, want 429", rec.Code)
	}
	assertAnthropicError(t, rec.Body.Bytes(), "rate_limit_error")
	if ra := rec.Header().Get("Retry-After"); ra != "30" {
		t.Fatalf("retry-after = %q, want 30", ra)
	}
	now = now.Add(30 * time.Second)
	if rec := call(); rec.Code != http.StatusOK {
		t.Fatalf("after refill: status %d", rec.Code)
	}
}

func TestProxyStreamsUnbuffered(t *testing.T) {
	release := make(chan struct{})
	up, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Request-Id", "req_stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "event: message_stop\ndata: {}\n\n")
	})
	h := newProxy(t, AnthropicProxyConfig{BaseURL: up.URL, APIKey: testKey})
	front := httptest.NewServer(h)
	defer front.Close()
	resp, err := http.Post(front.URL+"/model-proxy/anthropic/v1/messages", "application/json", strings.NewReader(`{"model":"m","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	first := make(chan string, 1)
	go func() {
		line, _ := br.ReadString('\n')
		first <- line
	}()
	select {
	case line := <-first:
		if line != "event: message_start\n" {
			t.Fatalf("first line %q", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first event was buffered until the upstream finished")
	}
	close(release)
	rest, _ := io.ReadAll(br)
	if !strings.Contains(string(rest), "message_stop") {
		t.Fatalf("stream tail missing: %q", rest)
	}
}

func TestProxyUpstreamErrorsPassThrough(t *testing.T) {
	up, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(529)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`)
	})
	h := newProxy(t, AnthropicProxyConfig{BaseURL: up.URL, APIKey: testKey})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/model-proxy/anthropic/v1/messages", strings.NewReader(`{"model":"m"}`)))
	if rec.Code != 529 {
		t.Fatalf("status %d", rec.Code)
	}
	assertAnthropicError(t, rec.Body.Bytes(), "overloaded_error")
}

func TestProxyRejectsNonObjectBody(t *testing.T) {
	up, got := newUpstream(t, nil)
	h := newProxy(t, AnthropicProxyConfig{BaseURL: up.URL, APIKey: testKey})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/model-proxy/anthropic/v1/messages", strings.NewReader(`[1]`)))
	if rec.Code != http.StatusBadRequest || got.path != "" {
		t.Fatalf("status %d, upstream path %q", rec.Code, got.path)
	}
	assertAnthropicError(t, rec.Body.Bytes(), "invalid_request_error")
}

func assertAnthropicError(t *testing.T, body []byte, wantType string) {
	t.Helper()
	var e struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil || e.Type != "error" || e.Error.Type != wantType || e.Error.Message == "" {
		t.Fatalf("body %s is not an Anthropic %s error", body, wantType)
	}
}

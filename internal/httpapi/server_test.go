package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"yuno/internal/openclaw"
)

func newRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }

func TestDialWithin(t *testing.T) {
	gw := adminGateway()
	handshake := &openclaw.HandshakeError{Reason: "bad token"}
	tests := []struct {
		name      string
		failures  int   // dials that fail before one succeeds; -1 never succeeds
		failWith  error // error of the failing dials
		wantErr   bool
		wantDials int32
	}{
		{name: "first dial", failures: 0, wantDials: 1},
		{name: "transient failures then success", failures: 2, failWith: errors.New("connection refused"), wantDials: 3},
		{name: "refused handshake is not retried", failures: -1, failWith: handshake, wantErr: true, wantDials: 1},
		{name: "gives up after the window", failures: -1, failWith: errors.New("connection refused"), wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var dials atomic.Int32
			var connCtx context.Context
			c, err := dialWithin(ctx, 300*time.Millisecond, 20*time.Millisecond, func(dctx context.Context) (openclaw.Client, error) {
				n := dials.Add(1)
				if tc.failures < 0 || int(n) <= tc.failures {
					return nil, tc.failWith
				}
				connCtx = dctx
				return gw, nil
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantDials != 0 && dials.Load() != tc.wantDials {
				t.Fatalf("dials = %d, want %d", dials.Load(), tc.wantDials)
			}
			if tc.wantErr {
				if errors.Is(tc.failWith, handshake) && !errors.As(err, new(*openclaw.HandshakeError)) {
					t.Fatalf("handshake error not wrapped: %v", err)
				}
				return
			}
			if c != gw {
				t.Fatal("wrong client")
			}
			time.Sleep(400 * time.Millisecond) // past the dial window
			if connCtx.Err() != nil {
				t.Fatal("connection context cancelled after a successful dial")
			}
			cancel()
			select {
			case <-connCtx.Done():
			case <-time.After(time.Second):
				t.Fatal("connection context outlived the parent")
			}
		})
	}
}

func TestReadyz(t *testing.T) {
	pool := needDB(t)
	noAdmin := adminGateway()
	noAdmin.info.Scopes = []string{"operator.read"}
	down := adminGateway()
	down.connected = false
	tests := []struct {
		name string
		gw   openclaw.Client
		want int
	}{
		{name: "no gateway", gw: nil, want: http.StatusServiceUnavailable},
		{name: "reconnecting", gw: down, want: http.StatusServiceUnavailable},
		{name: "missing admin scope", gw: noAdmin, want: http.StatusServiceUnavailable},
		{name: "ready", gw: adminGateway(), want: http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, ts := newTestServer(t, pool, nil, tc.gw)
			code, body := do(t, ts, http.MethodGet, "/readyz", "")
			if code != tc.want {
				t.Fatalf("status %d: %s", code, body)
			}
			if code != http.StatusOK && decodeError(t, body).Error.Code != "gateway_unavailable" {
				t.Fatalf("body %s", body)
			}
		})
	}
	t.Run("no database", func(t *testing.T) {
		_, ts := newTestServer(t, nil, nil, adminGateway())
		if code, _ := do(t, ts, http.MethodGet, "/readyz", ""); code != http.StatusServiceUnavailable {
			t.Fatalf("status %d", code)
		}
	})
}

func TestOpenClawStatus(t *testing.T) {
	failing := adminGateway()
	failing.agentsErr = errors.New("timeout")
	tests := []struct {
		name       string
		gw         openclaw.Client
		want       string
		wantAgents *int
	}{
		{name: "no gateway", gw: nil, want: `{"connected":false,"scopes":[]}`},
		{name: "connected", gw: adminGateway(), want: `{"connected":true,"version":"2026.10.3","conn_id":"conn-1","scopes":["operator.read","operator.write","operator.admin","operator.approvals"],"agents":2}`},
		{name: "agents.list fails", gw: failing, want: `{"connected":true,"version":"2026.10.3","conn_id":"conn-1","scopes":["operator.read","operator.write","operator.admin","operator.approvals"]}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, ts := newTestServer(t, nil, nil, tc.gw)
			code, body := do(t, ts, http.MethodGet, "/api/openclaw/status", "")
			if code != http.StatusOK || strings.TrimSpace(string(body)) != tc.want {
				t.Fatalf("status %d body %s, want %s", code, body, tc.want)
			}
		})
	}
}

// GET /api/tools lists every platform tool with its risk class (DESIGN 5.7 table),
// then the OpenClaw built-ins.
func TestListTools(t *testing.T) {
	_, ts := newTestServer(t, nil, nil, nil)
	code, body := do(t, ts, http.MethodGet, "/api/tools", "")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	var got []toolView
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tv := range got {
		names = append(names, string(tv.Source)+":"+tv.Name)
	}
	want := "platform:deploy_release platform:finish platform:http_fetch platform:memory_recall platform:memory_save " +
		"openclaw:apply_patch openclaw:edit openclaw:exec openclaw:ls openclaw:process openclaw:read openclaw:write"
	if strings.Join(names, " ") != want {
		t.Fatalf("tools = %v", names)
	}
	table := map[string]struct {
		risk, hits string
		timeout    int
		untrusted  bool
	}{
		"deploy_release": {"side_effecting", "local_service", 30, false},
		"finish":         {"computational", "postgres", 5, false},
		"http_fetch":     {"read_only", "external", 15, true},
		"memory_recall":  {"read_only", "postgres", 5, false},
		"memory_save":    {"computational", "postgres", 5, false},
	}
	for _, tv := range got {
		if tv.Source == "openclaw" {
			if tv.Hits != "openclaw" || tv.Description == "" {
				t.Fatalf("built-in = %+v", tv)
			}
			continue
		}
		w := table[tv.Name]
		if string(tv.Risk) != w.risk || string(tv.Hits) != w.hits || tv.TimeoutSeconds != w.timeout ||
			tv.UntrustedOutput != w.untrusted || len(tv.InputSchema) == 0 {
			t.Errorf("%s = %+v", tv.Name, tv)
		}
	}
}

func TestModelProxyIsLoopbackOnly(t *testing.T) {
	s, err := New(Deps{Cfg: testConfig()})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		remote string
		path   string
		want   int
	}{
		{name: "anthropic from another container", remote: "172.18.0.5:41000", path: "/model-proxy/anthropic/v1/messages", want: http.StatusForbidden},
		{name: "scripted from another container", remote: "172.18.0.5:41000", path: "/model-proxy/scripted/v1/chat/completions", want: http.StatusForbidden},
		{name: "anthropic over loopback reaches the proxy", remote: "127.0.0.1:41000", path: "/model-proxy/anthropic/v1/messages", want: http.StatusUnauthorized},
		{name: "scripted over IPv6 loopback reaches the provider", remote: "[::1]:41000", path: "/model-proxy/scripted/v1/chat/completions", want: http.StatusInternalServerError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
			req.RemoteAddr = tc.remote
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.want, rec.Body)
			}
		})
	}
}

package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"yuno/internal/openclaw"
	"yuno/internal/tools"
)

func newRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }

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

// T5.16.8: open_merge_request is listed only with a configured project, and the
// status endpoint describes the project without its token.
func TestListToolsAndStatusWithProject(t *testing.T) {
	cfg := testConfig()
	cfg.ProjectRepoURL, cfg.ProjectGitToken, cfg.ProjectSandboxNetwork = "https://gitlab.com/group/demo.git", "glpat-not-a-real-token-00", true
	_, ts := newTestServerDeps(t, Deps{Cfg: cfg}, nil)
	code, body := do(t, ts, http.MethodGet, "/api/tools", "")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	var got []toolView
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tv := range got {
		if tv.Name == "open_merge_request" {
			found = true
			if tv.Risk != "side_effecting" || tv.Hits != "external" || tv.TimeoutSeconds != 120 || tv.Chat || tv.Source != "platform" {
				t.Fatalf("open_merge_request = %+v", tv)
			}
		}
	}
	if !found {
		t.Fatal("open_merge_request not listed with a project configured")
	}
	if strings.Contains(string(body), cfg.ProjectGitToken) {
		t.Fatal("the token is in the tool listing")
	}
	code, body = do(t, ts, http.MethodGet, "/api/status", "")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	var st statusResponse
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatal(err)
	}
	if !st.Project.Configured || st.Project.RepoURL != cfg.ProjectRepoURL || st.Project.DefaultBranch != "main" || st.Project.Provider != "gitlab" || !st.Project.SandboxNetwork {
		t.Fatalf("status project = %+v", st.Project)
	}
	if strings.Contains(string(body), cfg.ProjectGitToken) {
		t.Fatal("the token is in the status")
	}

	// A misconfigured project (no token) registers nothing and reports unconfigured.
	cfg.ProjectGitToken = ""
	_, ts2 := newTestServerDeps(t, Deps{Cfg: cfg}, nil)
	_, body = do(t, ts2, http.MethodGet, "/api/tools", "")
	if strings.Contains(string(body), "open_merge_request") {
		t.Fatal("open_merge_request listed without a token")
	}
	_, body = do(t, ts2, http.MethodGet, "/api/status", "")
	if err := json.Unmarshal(body, &st); err != nil || st.Project.Configured {
		t.Fatalf("status project = %+v %v", st.Project, err)
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
	want := "platform:delegate_task platform:deploy_release platform:finish platform:http_fetch platform:list_agents " +
		"platform:list_workflows platform:memory_recall platform:memory_save platform:run_status " +
		"openclaw:apply_patch openclaw:edit openclaw:exec openclaw:ls openclaw:process openclaw:read openclaw:write"
	if strings.Join(names, " ") != want {
		t.Fatalf("tools = %v", names)
	}
	table := map[string]struct {
		risk, hits string
		timeout    int
		untrusted  bool
		chat       bool
	}{
		"delegate_task":  {"computational", "postgres", 15, false, true},
		"deploy_release": {"side_effecting", "local_service", 30, false, false},
		"finish":         {"computational", "postgres", 5, false, false},
		"http_fetch":     {"read_only", "external", 15, true, false},
		"list_agents":    {"read_only", "postgres", 5, false, true},
		"list_workflows": {"read_only", "postgres", 5, false, true},
		"memory_recall":  {"read_only", "postgres", 5, true, true},
		"memory_save":    {"computational", "postgres", 5, false, true},
		"run_status":     {"read_only", "postgres", 5, false, true},
	}
	var chat []string
	for _, tv := range got {
		if tv.Source == "openclaw" {
			if tv.Hits != "openclaw" || tv.Description == "" || tv.Chat {
				t.Fatalf("built-in = %+v", tv)
			}
			continue
		}
		w := table[tv.Name]
		if string(tv.Risk) != w.risk || string(tv.Hits) != w.hits || tv.TimeoutSeconds != w.timeout ||
			tv.UntrustedOutput != w.untrusted || tv.Chat != w.chat || len(tv.InputSchema) == 0 {
			t.Errorf("%s = %+v", tv.Name, tv)
		}
		if tv.Chat {
			chat = append(chat, tv.Name)
		}
	}
	// DESIGN 5.15 rule 2: the registry's chat flags are the list the materialiser and
	// the seed use.
	if !slices.Equal(chat, tools.ChatToolNames) {
		t.Fatalf("chat tools = %v, want %v", chat, tools.ChatToolNames)
	}
	// The chat field is on the wire for every entry (the UI's contract).
	if !strings.Contains(string(body), `"chat":false`) || !strings.Contains(string(body), `"chat":true`) {
		t.Fatalf("chat field missing: %s", body)
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

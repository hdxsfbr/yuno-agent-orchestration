package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"yuno/internal/engine"
)

// bearer adds an Authorization header to every request.
type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(r)
}

func connectMCP(t *testing.T, url, token string) (*mcp.ClientSession, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "yuno-test", Version: "1"}, nil)
	transport := &mcp.StreamableClientTransport{
		Endpoint:             url + "/mcp",
		HTTPClient:           &http.Client{Transport: bearer{token: token, next: http.DefaultTransport}},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}
	return client.Connect(ctx, transport, nil)
}

func TestMCPListsFinish(t *testing.T) {
	_, ts := newTestServer(t, nil, &fakeEngine{}, nil)
	cs, err := connectMCP(t, ts.URL, testMCPToken)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cs.Close()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != 1 || res.Tools[0].Name != "finish" || res.Tools[0].Description == "" {
		t.Fatalf("tools = %+v", res.Tools)
	}
	schema, _ := json.Marshal(res.Tools[0].InputSchema)
	var s struct {
		Type                 string         `json:"type"`
		Required             []string       `json:"required"`
		AdditionalProperties bool           `json:"additionalProperties"`
		Properties           map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(schema, &s); err != nil || s.Type != "object" || s.AdditionalProperties ||
		len(s.Required) != 2 || s.Properties["run_token"] == nil || s.Properties["result"] == nil {
		t.Fatalf("input schema = %s", schema)
	}
}

func TestMCPFinish(t *testing.T) {
	tests := []struct {
		name       string
		args       map[string]any
		engineErr  error
		wantText   string
		wantIsErr  bool
		wantCall   bool
		wantReject bool
	}{
		{name: "accepted", args: map[string]any{"run_token": "r_tok", "result": map[string]any{"summary": "ok"}},
			wantText: `{"status":"accepted"}`, wantCall: true},
		{name: "schema failure goes back to the model", args: map[string]any{"run_token": "r_tok", "result": map[string]any{}},
			engineErr: &engine.FinishValidationError{Message: "result: missing property 'summary'"},
			wantText:  "result: missing property 'summary'", wantIsErr: true, wantCall: true},
		{name: "invalid run token", args: map[string]any{"run_token": "r_old", "result": map[string]any{}},
			engineErr: engine.ErrInvalidRunToken, wantText: "invalid run token", wantIsErr: true, wantCall: true},
		{name: "wrapped invalid run token", args: map[string]any{"run_token": "r_old", "result": map[string]any{}},
			engineErr: errors.Join(errors.New("lookup"), engine.ErrInvalidRunToken), wantText: "invalid run token", wantIsErr: true, wantCall: true},
		{name: "input violates the tool schema", args: map[string]any{"result": map[string]any{}},
			wantText: "missing property 'run_token'", wantIsErr: true},
		{name: "schema refusal with a token counts as an attempt", args: map[string]any{"run_token": "r_tok", "result": "done"},
			wantText: "/result", wantIsErr: true, wantReject: true},
		{name: "internal failure is opaque", args: map[string]any{"run_token": "r_tok", "result": map[string]any{}},
			engineErr: errors.New("db: connection refused"), wantText: "internal error", wantIsErr: true, wantCall: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fe := &fakeEngine{finishErr: tc.engineErr}
			_, ts := newTestServer(t, nil, fe, nil)
			cs, err := connectMCP(t, ts.URL, testMCPToken)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			defer cs.Close()
			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "finish", Arguments: tc.args})
			if err != nil {
				t.Fatalf("CallTool: %v", err)
			}
			if len(res.Content) != 1 {
				t.Fatalf("content = %+v", res.Content)
			}
			text := res.Content[0].(*mcp.TextContent).Text
			if res.IsError != tc.wantIsErr || !strings.Contains(text, tc.wantText) {
				t.Fatalf("result isError=%v text=%q, want isError=%v containing %q", res.IsError, text, tc.wantIsErr, tc.wantText)
			}
			if strings.Contains(text, "connection refused") {
				t.Fatalf("internal error leaked to the model: %q", text)
			}
			if called := len(fe.finishes) > 0; called != tc.wantCall {
				t.Fatalf("engine called = %v, want %v", called, tc.wantCall)
			}
			if rejected := len(fe.rejections) == 1 && strings.HasPrefix(fe.rejections[0], "r_tok "); rejected != tc.wantReject {
				t.Fatalf("rejections = %q, want one for r_tok: %v", fe.rejections, tc.wantReject)
			}
			if tc.name == "accepted" && fe.finishes[0] != `r_tok {"summary":"ok"}` {
				t.Fatalf("engine got %q", fe.finishes[0])
			}
		})
	}
}

func TestMCPRequiresBearer(t *testing.T) {
	_, ts := newTestServer(t, nil, &fakeEngine{}, nil)
	tests := []struct {
		name   string
		header string
	}{
		{name: "no header"},
		{name: "wrong token", header: "Bearer nope"},
		{name: "wrong scheme", header: "Basic " + testMCPToken},
		{name: "prefix of the token", header: "Bearer " + testMCPToken[:10]},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			resp, err := ts.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") == "" {
				t.Fatalf("status %d, WWW-Authenticate %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
			}
		})
	}
	if _, err := connectMCP(t, ts.URL, "nope"); err == nil {
		t.Fatal("client connected with a wrong token")
	}
}

func TestMCPRefusesWhenTokenUnset(t *testing.T) {
	s, err := New(Deps{Engine: &fakeEngine{}})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer ")
	rec := newRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d with an unset MCP token", rec.Code)
	}
}

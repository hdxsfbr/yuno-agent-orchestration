package httpapi

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestRegisterScript(t *testing.T) {
	s, ts := newTestServer(t, nil, nil, nil)
	tests := []struct {
		name      string
		body      string
		want      int
		wantPaths []string
	}{
		{name: "valid", body: `{"script_id":"case-01.a","steps":[{"tool":{"name":"exec","args":{"command":"ls"}}},{"text":"done","usage":{"prompt":5,"completion":1}}]}`, want: http.StatusNoContent},
		{name: "missing id and steps", body: `{}`, want: http.StatusUnprocessableEntity, wantPaths: []string{"script_id", "steps"}},
		{name: "bad id", body: `{"script_id":"a b","steps":[{"text":"x"}]}`, want: http.StatusUnprocessableEntity, wantPaths: []string{"script_id"}},
		{name: "id ending in a dash", body: `{"script_id":"case-","steps":[{"text":"x"}]}`, want: http.StatusUnprocessableEntity, wantPaths: []string{"script_id"}},
		{name: "too long", body: `{"script_id":"` + strings.Repeat("a", 81) + `","steps":[{"text":"x"}]}`, want: http.StatusUnprocessableEntity, wantPaths: []string{"script_id"}},
		{name: "bad step", body: `{"script_id":"ok","steps":[{"text":"x"},{"text":"y","tool":{"name":"exec","args":{}}}]}`, want: http.StatusUnprocessableEntity, wantPaths: []string{"steps[1]"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, body := do(t, ts, http.MethodPost, "/api/evals/scripts", tc.body)
			if code != tc.want {
				t.Fatalf("status %d: %s", code, body)
			}
			if tc.wantPaths != nil {
				if got := detailPaths(t, body); !slices.Equal(got, tc.wantPaths) {
					t.Fatalf("paths = %q, want %q", got, tc.wantPaths)
				}
			}
		})
	}

	// The registered script is what the scripted provider replays.
	req := httptest.NewRequest(http.MethodPost, "/model-proxy/scripted/v1/chat/completions",
		strings.NewReader(`{"model":"scripted-1","stream":false,"messages":[{"role":"user","content":"task\n\n--- platform protocol ---\nscript_id: case-01.a"}]}`))
	req.RemoteAddr = "127.0.0.1:5000"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"name":"exec"`) {
		t.Fatalf("scripted replay: %d %s", rec.Code, rec.Body)
	}
}

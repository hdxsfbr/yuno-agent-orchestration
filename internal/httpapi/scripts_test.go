package httpapi

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"yuno/internal/model"
	"yuno/internal/store"
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

// A script can be bound to the next run the platform starts on a project (an eval of the
// wave loop, DESIGN 5.29 rule 17): the store hands it out once, in order.
func TestRegisterScriptWithABinding(t *testing.T) {
	pool := needDB(t)
	p := seedProject(t, testCipher(t), "bound-"+store.NewID(), "tok")
	s, ts := newTestServer(t, pool, nil, nil)
	post := func(id, trigger, project string) (int, []byte) {
		return do(t, ts, http.MethodPost, "/api/evals/scripts",
			`{"script_id":"`+id+`","steps":[{"text":"x"}],"bind":{"project_id":"`+project+`","trigger":"`+trigger+`"}}`)
	}
	if code, body := post("bound-a", "issue", p.ID); code != http.StatusNoContent {
		t.Fatalf("%d %s", code, body)
	}
	if code, body := post("bound-b", "issue", p.ID); code != http.StatusNoContent {
		t.Fatalf("%d %s", code, body)
	}
	binder := s.scripts.(model.ScriptBinder)
	for _, want := range []string{"bound-a", "bound-b"} {
		if id, ok := binder.TakeBound(p.ID, "issue"); !ok || id != want {
			t.Fatalf("TakeBound = %q %v, want %q", id, ok, want)
		}
	}
	for name, c := range map[string]struct{ trigger, project, path string }{
		"a trigger that cannot be bound": {"manual", p.ID, "bind.trigger"},
		"a project that is not there":    {"issue", "no-such-project", "bind.project_id"},
	} {
		code, body := post("bound-c", c.trigger, c.project)
		if code != http.StatusUnprocessableEntity || !slices.Contains(detailPaths(t, body), c.path) {
			t.Errorf("%s: %d %s", name, code, body)
		}
	}
}

package evals

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Every run the eval runner starts is tagged trigger eval, so the run list can tell it
// from an operator's (audit #30).
func TestCreateRunTagsEval(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"run-1","trigger":"eval"}`))
	}))
	defer srv.Close()
	if _, err := NewClient(srv.URL, "").CreateRun(context.Background(), RunRequest{AgentID: "a", Task: "t"}); err != nil {
		t.Fatal(err)
	}
	if got["trigger"] != "eval" {
		t.Fatalf("POST /api/runs body = %v, want trigger eval", got)
	}
}

package model

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenRouterModelID(t *testing.T) {
	cases := map[string]string{
		"claude-opus-5-5":           "anthropic/claude-opus-5.5",
		"claude-sonnet-5-5":         "anthropic/claude-sonnet-5.5",
		"claude-haiku-5-5":          "anthropic/claude-haiku-5.5",
		"claude-opus-5":             "anthropic/claude-opus-5",
		"claude-haiku-4-5":          "anthropic/claude-haiku-4.5",
		"claude-fable-5-1":          "anthropic/claude-fable-5.1",
		"anthropic/claude-opus-5.5": "anthropic/claude-opus-5.5",
	}
	for in, want := range cases {
		if got := OpenRouterModelID(in); got != want {
			t.Errorf("%s: got %s, want %s", in, got, want)
		}
	}
}

// The OpenRouter upstream gets a bearer token, no x-api-key, and the mapped model id;
// the request is otherwise the shaped Anthropic body.
func TestProxyOpenRouterUpstream(t *testing.T) {
	var gotAuth, gotKey, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotKey = r.Header.Get("X-Api-Key")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","content":[]}`))
	}))
	defer upstream.Close()

	h, err := NewAnthropicProxy(AnthropicProxyConfig{
		Prefix:     "/model-proxy/anthropic",
		BaseURL:    upstream.URL,
		APIKey:     "sk-or-v1-testkey",
		AuthScheme: "bearer",
		ModelMap:   OpenRouterModelID,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	body := `{"model":"claude-opus-5-5","max_tokens":5,"thinking":{"type":"adaptive","display":"summarized"},"messages":[]}`
	req, _ := http.NewRequest("POST", srv.URL+"/model-proxy/anthropic/v1/messages", strings.NewReader(body))
	req.Header.Set("X-Api-Key", "proxy")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if gotAuth != "Bearer sk-or-v1-testkey" || gotKey != "" {
		t.Fatalf("auth headers: Authorization=%q X-Api-Key=%q", gotAuth, gotKey)
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(gotBody), &fields); err != nil {
		t.Fatal(err)
	}
	if fields["model"] != "anthropic/claude-opus-5.5" {
		t.Fatalf("model not mapped: %v", fields["model"])
	}
	if th, _ := fields["thinking"].(map[string]any); th["type"] != "adaptive" || th["display"] != nil {
		t.Fatalf("thinking not shaped: %v", fields["thinking"])
	}
}

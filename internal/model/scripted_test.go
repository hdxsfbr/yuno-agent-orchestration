package model

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const scriptedPrefix = "/model-proxy/scripted/v1"

func scriptedRequest(stream bool, messages string) *http.Request {
	body := `{"model":"scripted-1","stream":` + map[bool]string{true: "true", false: "false"}[stream] + `,"messages":` + messages + `}`
	return httptest.NewRequest(http.MethodPost, scriptedPrefix+"/chat/completions", strings.NewReader(body))
}

const taskMessages = `[{"role":"system","content":"You are an agent."},{"role":"user","content":"Write it.\n\n--- platform protocol ---\nrun_token: r_aaaaaaaaaaaaaaaaaaaaaaaa\nscript_id: case-01"}]`

// sseData returns the data payloads of an SSE body in order.
func sseData(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, block := range strings.Split(strings.TrimSpace(body), "\n\n") {
		data, ok := strings.CutPrefix(block, "data: ")
		if !ok {
			t.Fatalf("unexpected SSE block %q", block)
		}
		out = append(out, data)
	}
	return out
}

func TestScriptedStreamingShapes(t *testing.T) {
	store := NewMemoryScriptStore()
	store.Register("case-01", []ScriptStep{
		{Tool: &ScriptToolCall{Name: "exec", Args: json.RawMessage(`{ "command": "pytest -q" }`)}},
		{Text: "All tests pass.", Usage: &ScriptUsage{Prompt: 321, Completion: 7}},
	})
	h := NewScriptedHandler(store, scriptedPrefix, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, scriptedRequest(true, taskMessages))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d content-type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	data := sseData(t, rec.Body.String())
	want := []string{
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":0,"model":"scripted-1","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"exec","arguments":"{\"command\":\"pytest -q\"}"}}]},"finish_reason":null}]}`,
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":0,"model":"scripted-1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110}}`,
		`[DONE]`,
	}
	assertLines(t, data, want)

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, scriptedRequest(true, taskMessages))
	want = []string{
		`{"id":"chatcmpl-2","object":"chat.completion.chunk","created":0,"model":"scripted-1","choices":[{"index":0,"delta":{"role":"assistant","content":"All tests pass."},"finish_reason":null}]}`,
		`{"id":"chatcmpl-2","object":"chat.completion.chunk","created":0,"model":"scripted-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":321,"completion_tokens":7,"total_tokens":328}}`,
		`[DONE]`,
	}
	assertLines(t, sseData(t, rec.Body.String()), want)
}

func TestScriptedNonStreaming(t *testing.T) {
	store := NewMemoryScriptStore()
	store.Register("case-02", []ScriptStep{
		{Tool: &ScriptToolCall{Name: "yuno__finish", Args: json.RawMessage(`{"run_token":"r_x","result":{"ok":true}}`)}},
		{Text: "Done."},
	})
	h := NewScriptedHandler(store, scriptedPrefix, nil)
	msgs := `[{"role":"user","content":"--- platform protocol ---\nscript_id: case-02"}]`
	tests := []struct {
		want string
	}{
		{want: `{"id":"chatcmpl-1","object":"chat.completion","created":0,"model":"scripted-1","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"yuno__finish","arguments":"{\"run_token\":\"r_x\",\"result\":{\"ok\":true}}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110}}`},
		{want: `{"id":"chatcmpl-2","object":"chat.completion","created":0,"model":"scripted-1","choices":[{"index":0,"message":{"role":"assistant","content":"Done."},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110}}`},
	}
	for i, tc := range tests {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, scriptedRequest(false, msgs))
		if rec.Code != http.StatusOK {
			t.Fatalf("call %d: status %d", i+1, rec.Code)
		}
		if got := strings.TrimSpace(rec.Body.String()); got != tc.want {
			t.Fatalf("call %d:\n got %s\nwant %s", i+1, got, tc.want)
		}
	}
}

func TestScriptedExhaustionAndUnknown(t *testing.T) {
	store := NewMemoryScriptStore()
	store.Register("one-step", []ScriptStep{{Text: "hi"}})
	h := NewScriptedHandler(store, scriptedPrefix, nil)
	tests := []struct {
		name     string
		messages string
		wantMsg  string
	}{
		{name: "first call", messages: `[{"role":"user","content":"--- platform protocol ---\nscript_id: one-step"}]`},
		{name: "exhausted", messages: `[{"role":"user","content":"--- platform protocol ---\nscript_id: one-step"}]`, wantMsg: "script exhausted or unknown: one-step"},
		{name: "unknown", messages: `[{"role":"user","content":"--- platform protocol ---\nscript_id: nobody"}]`, wantMsg: "script exhausted or unknown: nobody"},
		{name: "no marker", messages: `[{"role":"user","content":"hello"}]`, wantMsg: "no script_id marker in the request messages"},
		{name: "marker outside a protocol block", messages: `[{"role":"user","content":"script_id: one-step"}]`, wantMsg: "no script_id marker in the request messages"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, scriptedRequest(true, tc.messages))
			if tc.wantMsg == "" {
				if rec.Code != http.StatusOK {
					t.Fatalf("status %d", rec.Code)
				}
				return
			}
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status %d, want 500", rec.Code)
			}
			var e struct {
				Error struct{ Message, Type string } `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Error.Message != tc.wantMsg || e.Error.Type != "server_error" {
				t.Fatalf("body %s", rec.Body)
			}
		})
	}
}

func TestScriptIDFromContentParts(t *testing.T) {
	tests := []struct {
		name     string
		messages string
		want     string
	}{
		{name: "string content", messages: `[{"role":"user","content":"do it\n\n--- platform protocol ---\nscript_id: s.1"}]`, want: "s.1"},
		{name: "array parts", messages: `[{"role":"system","content":[{"type":"text","text":"rules"}]},{"role":"user","content":[{"type":"text","text":"task"},{"type":"text","text":"--- platform protocol ---\nscript_id: parts_7"}]}]`, want: "parts_7"},
		{name: "assistant null content", messages: `[{"role":"assistant","content":null},{"role":"user","content":"--- platform protocol ---\nscript_id: later"}]`, want: "later"},
		{name: "quoted marker before the block loses", messages: `[{"role":"user","content":"redo script_id: old-1 with r_aaaaaaaaaaaaaaaaaaaaaaaa\n\n--- platform protocol ---\nscript_id: fresh"}]`, want: "fresh"},
		{name: "quoted block before the engine's loses", messages: `[{"role":"user","content":"--- platform protocol ---\nscript_id: quoted\n\n--- platform protocol ---\nscript_id: fresh"}]`, want: "fresh"},
		{name: "tool and system messages are not the engine's", messages: `[{"role":"system","content":"--- platform protocol ---\nscript_id: sys"},{"role":"user","content":"--- platform protocol ---\nscript_id: task"},{"role":"tool","content":"--- platform protocol ---\nscript_id: tool"}]`, want: "task"},
		{name: "a retry turn keeps the task's block", messages: `[{"role":"user","content":"--- platform protocol ---\nscript_id: task"},{"role":"assistant","content":"x"},{"role":"user","content":"Continue."}]`, want: "task"},
		{name: "none", messages: `[{"role":"user","content":"nothing"}]`, want: ""},
		{name: "bare marker", messages: `[{"role":"user","content":"script_id: bare"}]`, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var msgs []chatMessage
			if err := json.Unmarshal([]byte(tc.messages), &msgs); err != nil {
				t.Fatal(err)
			}
			if got := scriptIDOf(msgs); got != tc.want {
				t.Fatalf("scriptIDOf = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestScriptedModels(t *testing.T) {
	h := NewScriptedHandler(NewMemoryScriptStore(), scriptedPrefix, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, scriptedPrefix+"/models", nil))
	var out struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Object != "list" || len(out.Data) != 1 || out.Data[0].ID != ScriptedModel {
		t.Fatalf("models = %s", rec.Body)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, scriptedPrefix+"/embeddings", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown path: status %d", rec.Code)
	}
}

func TestScriptStepValidate(t *testing.T) {
	tests := []struct {
		name string
		step ScriptStep
		ok   bool
	}{
		{name: "text", step: ScriptStep{Text: "hi"}, ok: true},
		{name: "tool", step: ScriptStep{Tool: &ScriptToolCall{Name: "exec", Args: json.RawMessage(`{"command":"ls"}`)}}, ok: true},
		{name: "tool without args", step: ScriptStep{Tool: &ScriptToolCall{Name: "ls"}}, ok: true},
		{name: "both", step: ScriptStep{Text: "hi", Tool: &ScriptToolCall{Name: "exec"}}},
		{name: "neither", step: ScriptStep{}},
		{name: "tool without name", step: ScriptStep{Tool: &ScriptToolCall{Args: json.RawMessage(`{}`)}}},
		{name: "array args", step: ScriptStep{Tool: &ScriptToolCall{Name: "exec", Args: json.RawMessage(`[1]`)}}},
		{name: "negative usage", step: ScriptStep{Text: "x", Usage: &ScriptUsage{Prompt: -1}}},
	}
	for _, tc := range tests {
		if err := tc.step.Validate(); (err == nil) != tc.ok {
			t.Errorf("%s: Validate() = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

func assertLines(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d:\n got %s\nwant %s", i, got[i], want[i])
		}
	}
}

func TestScriptedPlaceholders(t *testing.T) {
	const (
		token = "r_abcdefghijklmnopqrstuvwx"
		runID = "0199f0a2-7c3e-7b1a-9d2e-1234567890ab"
	)
	task := `[{"role":"system","content":"rules"},{"role":"user","content":[{"type":"text","text":"Write it."},` +
		`{"type":"text","text":"--- platform protocol ---\nrun_id: ` + runID + `\nrun_token: ` + token + `\nscript_id: ph-1"}]}]`
	tests := []struct {
		name     string
		messages string
		args     string
		want     string
	}{
		{
			name:     "token and run id, nested and repeated",
			messages: task,
			args:     `{"run_token":"$RUN_TOKEN","result":{"run":"$RUN_ID","note":"for $RUN_ID with $RUN_TOKEN"}}`,
			want:     `{"run_token":"` + token + `","result":{"run":"` + runID + `","note":"for ` + runID + ` with ` + token + `"}}`,
		},
		{
			name: "an old token quoted in the task is not used",
			messages: `[{"role":"user","content":"Redo run_id: 0199f0a2-0000-7000-8000-000000000000 with r_oldoldoldoldoldoldoldold.\n\n` +
				`--- platform protocol ---\nrun_id: ` + runID + `\nrun_token: ` + token + `\nscript_id: ph-1"}]`,
			args: `{"run_token":"$RUN_TOKEN","run":"$RUN_ID"}`,
			want: `{"run_token":"` + token + `","run":"` + runID + `"}`,
		},
		{
			name:     "values missing from the request stay as written",
			messages: `[{"role":"user","content":"--- platform protocol ---\nscript_id: ph-1"}]`,
			args:     `{"run_token":"$RUN_TOKEN","run":"$RUN_ID"}`,
			want:     `{"run_token":"$RUN_TOKEN","run":"$RUN_ID"}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMemoryScriptStore()
			h := NewScriptedHandler(store, scriptedPrefix, nil)
			for _, stream := range []bool{true, false} {
				store.Register("ph-1", []ScriptStep{{Tool: &ScriptToolCall{Name: "yuno__finish", Args: json.RawMessage(tc.args)}}})
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, scriptedRequest(stream, tc.messages))
				body := rec.Body.String()
				if stream {
					body = sseData(t, body)[0]
				}
				var out struct {
					Choices []struct {
						Delta   chatDelta             `json:"delta"`
						Message chatCompletionMessage `json:"message"`
					} `json:"choices"`
				}
				if err := json.Unmarshal([]byte(body), &out); err != nil || len(out.Choices) != 1 {
					t.Fatalf("stream=%v: body %s", stream, body)
				}
				calls := out.Choices[0].Message.ToolCalls
				if stream {
					calls = out.Choices[0].Delta.ToolCalls
				}
				if len(calls) != 1 || calls[0].Function.Arguments != tc.want {
					t.Fatalf("stream=%v: arguments = %+v, want %s", stream, calls, tc.want)
				}
			}
		})
	}
}

// Scripts are reclaimed once used up and an hour after registration (finding 18).
func TestMemoryScriptStoreReclaims(t *testing.T) {
	store := NewMemoryScriptStore()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	store.Register("used-up", []ScriptStep{{Text: "a"}, {Text: "b"}})
	store.Register("abandoned", []ScriptStep{{Text: "a"}, {Text: "b"}})
	for _, want := range []string{"a", "b"} {
		if step, ok := store.Next("used-up"); !ok || step.Text != want {
			t.Fatalf("step = %+v %v, want %q", step, ok, want)
		}
	}
	if _, kept := store.scripts["used-up"]; kept {
		t.Fatal("a used-up script is kept")
	}
	if _, ok := store.Next("used-up"); ok {
		t.Fatal("a used-up script served another step")
	}
	if _, ok := store.Next("abandoned"); !ok {
		t.Fatal("first step of a live script")
	}

	now = now.Add(ScriptTTL - time.Second)
	store.Register("fresh", []ScriptStep{{Text: "x"}})
	if _, kept := store.scripts["abandoned"]; !kept {
		t.Fatal("script dropped before its hour")
	}
	now = now.Add(time.Second)
	if _, ok := store.Next("abandoned"); ok {
		t.Fatal("an expired script served a step")
	}
	store.Register("another", []ScriptStep{{Text: "y"}})
	if len(store.scripts) != 2 {
		t.Fatalf("scripts kept = %d, want fresh and another", len(store.scripts))
	}
	now = now.Add(ScriptTTL)
	store.Register("last", []ScriptStep{{Text: "z"}})
	if _, kept := store.scripts["fresh"]; kept || len(store.scripts) != 1 {
		t.Fatalf("expired scripts not swept on registration: %d kept", len(store.scripts))
	}
}

package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"

	"yuno/internal/runtoken"
)

// ScriptedModel is the one model the scripted provider serves; OpenClaw refers to it
// as scripted/scripted-1.
const ScriptedModel = "scripted-1"

// Placeholders a scripted tool call may carry in its arguments. They are replaced with
// the run token and run id from the request's task message, so a case script can call
// platform tools for whatever run replays it.
const (
	RunTokenPlaceholder = "$RUN_TOKEN"
	RunIDPlaceholder    = "$RUN_ID"
)

// runIDPattern matches the "run_id: <uuid>" line of the task message's protocol block.
var runIDPattern = regexp.MustCompile(`\brun_id: ([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})\b`)

// Usage reported for a scripted step that does not set its own.
const (
	defaultScriptedPromptTokens     = 100
	defaultScriptedCompletionTokens = 10
)

// Validate checks a step: exactly one of a non-empty text or a tool call, a tool name,
// object arguments and non-negative usage.
func (s ScriptStep) Validate() error {
	switch {
	case s.Text != "" && s.Tool != nil:
		return errors.New("set either text or tool, not both")
	case s.Text == "" && s.Tool == nil:
		return errors.New("set either text or tool")
	}
	if s.Tool != nil {
		if strings.TrimSpace(s.Tool.Name) == "" {
			return errors.New("tool.name is required")
		}
		args := bytes.TrimSpace(s.Tool.Args)
		if len(args) > 0 && string(args) != "null" && args[0] != '{' {
			return errors.New("tool.args must be a JSON object")
		}
	}
	if s.Usage != nil && (s.Usage.Prompt < 0 || s.Usage.Completion < 0) {
		return errors.New("usage must not be negative")
	}
	return nil
}

// NewScriptedHandler returns the deterministic offline provider (DESIGN 5.14 rule 9):
// the OpenAI chat completions shape OpenClaw's openai-completions provider consumes.
// Each request is matched to a script by the "script_id: <id>" line of the protocol
// block in its newest task message and answered with the script's next step, streamed exactly like a real provider
// (role and content or tool_calls deltas, a final chunk with finish_reason and usage,
// then [DONE]). prefix is the mount path removed before routing, e.g.
// /model-proxy/scripted/v1.
func NewScriptedHandler(store ScriptStore, prefix string, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &scriptedHandler{store: store, prefix: prefix, log: log}
}

type scriptedHandler struct {
	store  ScriptStore
	prefix string
	log    *slog.Logger
	n      atomic.Int64
}

func (h *scriptedHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path, ok := strings.CutPrefix(r.URL.Path, h.prefix)
	switch {
	case ok && path == "/chat/completions" && r.Method == http.MethodPost:
		h.complete(w, r)
	case ok && path == "/models" && r.Method == http.MethodGet:
		writeOpenAIJSON(w, http.StatusOK, map[string]any{
			"object": "list",
			"data":   []any{map[string]any{"id": ScriptedModel, "object": "model", "created": 0, "owned_by": "yuno"}},
		})
	default:
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "unknown scripted provider endpoint: "+r.Method+" "+r.URL.Path)
	}
}

type chatRequest struct {
	Model    string        `json:"model"`
	Stream   bool          `json:"stream"`
	Messages []chatMessage `json:"messages"`
}

type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func (h *scriptedHandler) complete(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxProxyBodyBytes))
	if err != nil {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body too large or unreadable")
		return
	}
	var req chatRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "request body is not a chat completions request")
		return
	}
	marks := markersOf(req.Messages)
	id := marks.scriptID
	if id == "" {
		h.log.Warn("scripted provider: no script_id in request")
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "no script_id marker in the request messages")
		return
	}
	step, ok := h.store.Next(id)
	if !ok {
		h.log.Warn("scripted provider: script exhausted or unknown", "script_id", id)
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "script exhausted or unknown: "+id)
		return
	}
	n := h.n.Add(1)
	resp := scriptedResponse{
		id:       fmt.Sprintf("chatcmpl-%d", n),
		model:    req.Model,
		step:     step,
		call:     fmt.Sprintf("call_%d", n),
		runToken: marks.runToken,
		runID:    marks.runID,
	}
	if req.Stream {
		resp.stream(w)
	} else {
		writeOpenAIJSON(w, http.StatusOK, resp.completion())
	}
}

// markers are the protocol values the engine writes into a task message.
type markers struct {
	scriptID, runToken, runID string
}

// markersOf reads the script id, run token and run id from the engine's protocol block:
// the text after the last platform protocol header in the newest user message that has
// one (DESIGN 5.6). Anything before the header, in other messages or in other roles (a
// task quoting an old token, a tool result echoing a block) is ignored, so the three
// values always come from one block and belong to the same run.
func markersOf(msgs []chatMessage) markers {
	var m markers
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "user" {
			continue
		}
		text := contentText(msgs[i].Content)
		at := strings.LastIndex(text, runtoken.ProtocolHeader)
		if at < 0 {
			continue
		}
		block := text[at+len(runtoken.ProtocolHeader):]
		m.scriptID = runtoken.FindScriptID(block)
		m.runToken = runtoken.Find(block)
		if sub := runIDPattern.FindStringSubmatch(block); sub != nil {
			m.runID = sub[1]
		}
		return m
	}
	return m
}

// scriptIDOf returns the script id of the request's protocol block.
func scriptIDOf(msgs []chatMessage) string {
	return markersOf(msgs).scriptID
}

func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
		b.WriteByte('\n')
	}
	return b.String()
}

// Wire shapes, field order as OpenAI sends them.

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type chatFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatToolCall struct {
	Index    *int         `json:"index,omitempty"`
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatDelta struct {
	Role      string         `json:"role,omitempty"`
	Content   string         `json:"content,omitempty"`
	ToolCalls []chatToolCall `json:"tool_calls,omitempty"`
}

type chatChunkChoice struct {
	Index        int       `json:"index"`
	Delta        chatDelta `json:"delta"`
	FinishReason *string   `json:"finish_reason"`
}

type chatChunk struct {
	ID      string            `json:"id"`
	Object  string            `json:"object"`
	Created int64             `json:"created"`
	Model   string            `json:"model"`
	Choices []chatChunkChoice `json:"choices"`
	Usage   *chatUsage        `json:"usage,omitempty"`
}

type chatCompletionMessage struct {
	Role      string         `json:"role"`
	Content   *string        `json:"content"`
	ToolCalls []chatToolCall `json:"tool_calls,omitempty"`
}

type chatCompletionChoice struct {
	Index        int                   `json:"index"`
	Message      chatCompletionMessage `json:"message"`
	FinishReason string                `json:"finish_reason"`
}

type chatCompletion struct {
	ID      string                 `json:"id"`
	Object  string                 `json:"object"`
	Created int64                  `json:"created"`
	Model   string                 `json:"model"`
	Choices []chatCompletionChoice `json:"choices"`
	Usage   chatUsage              `json:"usage"`
}

type scriptedResponse struct {
	id, model, call string
	runToken, runID string
	step            ScriptStep
}

func (s scriptedResponse) usage() chatUsage {
	u := chatUsage{PromptTokens: defaultScriptedPromptTokens, CompletionTokens: defaultScriptedCompletionTokens}
	if s.step.Usage != nil {
		u.PromptTokens, u.CompletionTokens = s.step.Usage.Prompt, s.step.Usage.Completion
	}
	u.TotalTokens = u.PromptTokens + u.CompletionTokens
	return u
}

// toolCall renders the step's tool call with placeholders substituted in the
// serialised arguments (which reaches nested values). A placeholder whose value is not
// in the request stays as written.
func (s scriptedResponse) toolCall(index *int) chatToolCall {
	args := string(bytes.TrimSpace(s.step.Tool.Args))
	if args == "" || args == "null" {
		args = "{}"
	} else {
		var buf bytes.Buffer
		if json.Compact(&buf, []byte(args)) == nil {
			args = buf.String()
		}
	}
	if s.runToken != "" {
		args = strings.ReplaceAll(args, RunTokenPlaceholder, s.runToken)
	}
	if s.runID != "" {
		args = strings.ReplaceAll(args, RunIDPlaceholder, s.runID)
	}
	return chatToolCall{Index: index, ID: s.call, Type: "function", Function: chatFunction{Name: s.step.Tool.Name, Arguments: args}}
}

func (s scriptedResponse) finishReason() string {
	if s.step.Tool != nil {
		return "tool_calls"
	}
	return "stop"
}

func (s scriptedResponse) stream(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	first := chatDelta{Role: "assistant"}
	if s.step.Tool != nil {
		zero := 0
		first.ToolCalls = []chatToolCall{s.toolCall(&zero)}
	} else {
		first.Content = s.step.Text
	}
	reason := s.finishReason()
	u := s.usage()
	writeSSEChunk(w, chatChunk{ID: s.id, Object: "chat.completion.chunk", Model: s.model,
		Choices: []chatChunkChoice{{Delta: first}}})
	writeSSEChunk(w, chatChunk{ID: s.id, Object: "chat.completion.chunk", Model: s.model,
		Choices: []chatChunkChoice{{FinishReason: &reason}}, Usage: &u})
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flush(w)
}

func (s scriptedResponse) completion() chatCompletion {
	msg := chatCompletionMessage{Role: "assistant"}
	if s.step.Tool != nil {
		msg.ToolCalls = []chatToolCall{s.toolCall(nil)}
	} else {
		text := s.step.Text
		msg.Content = &text
	}
	return chatCompletion{ID: s.id, Object: "chat.completion", Model: s.model,
		Choices: []chatCompletionChoice{{Message: msg, FinishReason: s.finishReason()}}, Usage: s.usage()}
}

func writeSSEChunk(w http.ResponseWriter, c chatChunk) {
	b, _ := json.Marshal(c)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
	flush(w)
}

func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func writeOpenAIJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeOpenAIError answers in the OpenAI error shape.
func writeOpenAIError(w http.ResponseWriter, status int, typ, message string) {
	writeOpenAIJSON(w, status, map[string]any{"error": map[string]string{"message": message, "type": typ}})
}

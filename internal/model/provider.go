// Package model defines the provider-neutral message shapes the engine persists and
// the Provider interface the agent loop calls. Thinking blocks are carried as opaque
// signatures only (DESIGN 5.7 rule 7): no reasoning text is ever requested or stored.
package model

import (
	"context"
	"encoding/json"
)

// Block types.
const (
	BlockText             = "text"
	BlockToolUse          = "tool_use"
	BlockToolResult       = "tool_result"
	BlockThinking         = "thinking"
	BlockRedactedThinking = "redacted_thinking"
)

// Stop reasons, normalised across providers.
const (
	StopEndTurn   = "end_turn"
	StopToolUse   = "tool_use"
	StopMaxTokens = "max_tokens"
	StopRefusal   = "refusal"
	StopOther     = "other"
)

type Message struct {
	Role   string  `json:"role"` // user | assistant
	Blocks []Block `json:"blocks"`
}

type Block struct {
	Type string `json:"type"`
	// text
	Text string `json:"text,omitempty"`
	// tool_use
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	// tool_result (ToolUseID is the id being answered)
	Content string `json:"content,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
	// thinking: signature only; Text stays empty. redacted_thinking: Data.
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"`
}

type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	Strict      bool            `json:"strict"`
}

type Request struct {
	Model     string    `json:"model"`
	System    string    `json:"system"`
	Messages  []Message `json:"messages"`
	Tools     []ToolDef `json:"tools"`
	MaxTokens int       `json:"max_tokens"`
	Effort    string    `json:"effort"` // low | medium | high | xhigh | max | ""
	// Hints for the scripted provider and for tracing; the Anthropic provider ignores them.
	RunID  string `json:"run_id,omitempty"`
	NodeID string `json:"node_id,omitempty"`
}

type Usage struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
}

type Response struct {
	Blocks     []Block `json:"blocks"`
	StopReason string  `json:"stop_reason"`
	Usage      Usage   `json:"usage"`
	RequestID  string  `json:"request_id,omitempty"`
	Model      string  `json:"model"`
	// Refusal details when StopReason == refusal.
	RefusalCategory string `json:"refusal_category,omitempty"`
}

// Provider is one model backend. Implementations must be safe for concurrent use and
// must not retry internally: the engine owns retries (DESIGN 5.5 rule 3).
type Provider interface {
	Name() string
	Complete(ctx context.Context, req Request) (*Response, error)
}

// Error classifies a provider failure for the retry policy (DESIGN 5.5 rule 1).
type Error struct {
	StatusCode int
	Type       string // API error type, e.g. rate_limit_error
	Message    string
	RetryAfter int // seconds, 0 if absent
	Transient  bool
}

func (e *Error) Error() string {
	if e.StatusCode != 0 {
		return "model: " + e.Type + " (" + itoa(e.StatusCode) + "): " + e.Message
	}
	return "model: " + e.Message
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// AssistantToolUses returns the tool_use blocks of a response in order.
func (r *Response) AssistantToolUses() []Block {
	var out []Block
	for _, b := range r.Blocks {
		if b.Type == BlockToolUse {
			out = append(out, b)
		}
	}
	return out
}

// AssistantMessage turns a response into the message to append to history.
func (r *Response) AssistantMessage() Message {
	return Message{Role: "assistant", Blocks: r.Blocks}
}

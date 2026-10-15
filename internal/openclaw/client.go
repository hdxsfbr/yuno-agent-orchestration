package openclaw

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Everything below is the engine's view of the gateway (DESIGN 5.14). The concrete
// WebSocket client lives in gateway.go (wave 1); tests use a fake that satisfies Client.

// Frame is one gateway protocol frame (type req | res | event) [probed].
type Frame struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	OK      *bool           `json:"ok,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
	Event   string          `json:"event,omitempty"`
}

// RPCError is the error object in a res frame [probed].
type RPCError struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Details json.RawMessage `json:"details,omitempty"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return "gateway: unknown error"
	}
	return fmt.Sprintf("gateway: %s: %s", e.Code, e.Message)
}

// Event is a gateway event with its arrival time.
type Event struct {
	Name    string          `json:"event"`
	Payload json.RawMessage `json:"payload"`
	At      time.Time       `json:"at"`
}

// ConnectInfo is what hello-ok told us.
type ConnectInfo struct {
	Version string   `json:"version"`
	ConnID  string   `json:"conn_id"`
	Scopes  []string `json:"scopes"`
	Methods int      `json:"methods"`
}

// Client is the gateway connection the engine, the materialiser and the API use.
type Client interface {
	// Call sends a req frame and returns the payload or the RPC error. It blocks until
	// the response arrives or ctx ends.
	Call(ctx context.Context, method string, params any) (json.RawMessage, error)
	// Subscribe returns a channel of every event the connection receives (fan-out, no
	// filtering) and a cancel func. Buffer overflows drop events, which the engine
	// recovers from through chat.history.
	Subscribe(buffer int) (<-chan Event, func())
	// Info reports the last successful handshake; Connected is false while reconnecting.
	Info() (ConnectInfo, bool)
}

// CallAs decodes the payload of Call into v.
func CallAs(ctx context.Context, c Client, method string, params any, v any) error {
	raw, err := c.Call(ctx, method, params)
	if err != nil {
		return err
	}
	if v == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, v)
}

// ---- payload shapes the engine relies on [probed unless noted] ----------------------

// ChatSendParams are the params of chat.send.
type ChatSendParams struct {
	SessionKey     string `json:"sessionKey"`
	AgentID        string `json:"agentId"`
	Message        string `json:"message"`
	IdempotencyKey string `json:"idempotencyKey"`
}

type ChatSendResult struct {
	RunID  string `json:"runId"`
	Status string `json:"status"` // "started"
}

// ChatEvent is the payload of event "chat".
type ChatEvent struct {
	RunID        string          `json:"runId"`
	SessionKey   string          `json:"sessionKey"`
	AgentID      string          `json:"agentId"`
	Seq          int             `json:"seq"`
	State        string          `json:"state"` // status | delta | final | error | aborted
	Phase        string          `json:"phase,omitempty"`
	StopReason   string          `json:"stopReason,omitempty"`
	DeltaText    string          `json:"deltaText,omitempty"`
	Message      json.RawMessage `json:"message,omitempty"`
	ErrorKind    string          `json:"errorKind,omitempty"`
	ErrorMessage string          `json:"errorMessage,omitempty"`
	ErrorDetail  json.RawMessage `json:"errorDetail,omitempty"`
}

// AgentEvent is the payload of event "agent"; Data varies by Stream.
type AgentEvent struct {
	RunID      string          `json:"runId"`
	SessionKey string          `json:"sessionKey"`
	AgentID    string          `json:"agentId"`
	Stream     string          `json:"stream"` // run_status | lifecycle | assistant | usage | item | command_output | reasoning
	Seq        int             `json:"seq"`
	TS         int64           `json:"ts"`
	Data       json.RawMessage `json:"data"`
}

// AgentItem is AgentEvent.Data for stream "item" (tool calls).
type AgentItem struct {
	ItemID     string `json:"itemId"`
	ToolCallID string `json:"toolCallId"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Phase      string `json:"phase"` // start | end
	Title      string `json:"title"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
	StartedAt  int64  `json:"startedAt"`
	EndedAt    int64  `json:"endedAt,omitempty"`
}

// AgentLifecycle is AgentEvent.Data for stream "lifecycle".
type AgentLifecycle struct {
	Phase    string `json:"phase"` // start | model | end
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
}

// AgentCommandOutput is AgentEvent.Data for stream "command_output".
type AgentCommandOutput struct {
	ItemID     string `json:"itemId"`
	ToolCallID string `json:"toolCallId"`
	Name       string `json:"name"`
	Phase      string `json:"phase"`
	Output     string `json:"output"`
}

// SessionMessageEvent is the payload of "session.message" (transcript entries).
type SessionMessageEvent struct {
	SessionKey string          `json:"sessionKey"`
	AgentID    string          `json:"agentId"`
	Message    TranscriptEntry `json:"message"`
	MessageID  string          `json:"messageId"`
	MessageSeq int             `json:"messageSeq"`
}

// TranscriptEntry is one message in chat.history / session.message.
type TranscriptEntry struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"` // string for user, []part for assistant
	Provider   string          `json:"provider,omitempty"`
	Model      string          `json:"model,omitempty"`
	API        string          `json:"api,omitempty"`
	StopReason string          `json:"stopReason,omitempty"`
	ResponseID string          `json:"responseId,omitempty"`
	Timestamp  int64           `json:"timestamp"`
	Usage      *Usage          `json:"usage,omitempty"`
	OpenClaw   json.RawMessage `json:"__openclaw,omitempty"`
}

// Usage is OpenClaw's per-message usage block [probed].
type Usage struct {
	Input       int64 `json:"input"`
	Output      int64 `json:"output"`
	TotalTokens int64 `json:"totalTokens"`
	CacheRead   int64 `json:"cacheRead"`
	CacheWrite  int64 `json:"cacheWrite"`
	Cost        struct {
		Input      float64 `json:"input"`
		Output     float64 `json:"output"`
		CacheRead  float64 `json:"cacheRead"`
		CacheWrite float64 `json:"cacheWrite"`
		Total      float64 `json:"total"`
	} `json:"cost"`
}

// SessionApprovalEvent is the payload of "session.approval" [probed].
type SessionApprovalEvent struct {
	SessionKey string `json:"sessionKey"`
	Phase      string `json:"phase"` // pending | terminal
	Approval   struct {
		ID           string `json:"id"`
		Status       string `json:"status"` // pending | allowed | denied | expired
		Presentation struct {
			Kind             string   `json:"kind"` // exec | plugin
			CommandText      string   `json:"commandText"`
			AgentID          string   `json:"agentId"`
			AllowedDecisions []string `json:"allowedDecisions"`
		} `json:"presentation"`
		CreatedAtMs  int64  `json:"createdAtMs"`
		ExpiresAtMs  int64  `json:"expiresAtMs"`
		ResolvedAtMs int64  `json:"resolvedAtMs,omitempty"`
		Decision     string `json:"decision,omitempty"`
		Reason       string `json:"reason,omitempty"`
	} `json:"approval"`
}

// ExecApprovalResolveParams are the params of exec.approval.resolve [docs].
type ExecApprovalResolveParams struct {
	ID       string `json:"id"`
	Decision string `json:"decision"` // allow-once | allow-always | deny
}

// AgentsListResult is the payload of agents.list [probed].
type AgentsListResult struct {
	DefaultID string `json:"defaultId"`
	Agents    []struct {
		ID        string `json:"id"`
		Name      string `json:"name,omitempty"`
		Workspace string `json:"workspace"`
		Model     struct {
			Primary string `json:"primary"`
		} `json:"model"`
	} `json:"agents"`
}

// ConfigGetResult is the subset of config.get the materialiser needs [probed].
type ConfigGetResult struct {
	Path   string          `json:"path"`
	Hash   string          `json:"hash"`
	Raw    string          `json:"raw"`
	Config json.RawMessage `json:"config"`
}

// ConfigPatchParams are the params of config.patch [probed]: Raw is a JSON document of
// the paths to merge; null values delete. Deleting or replacing anything that holds an
// array (a provider with its models, an agent entry with skills) is refused unless the
// exact path is listed in ReplacePaths [probed].
type ConfigPatchParams struct {
	Raw          string   `json:"raw"`
	BaseHash     string   `json:"baseHash,omitempty"`
	ReplacePaths []string `json:"replacePaths,omitempty"`
}

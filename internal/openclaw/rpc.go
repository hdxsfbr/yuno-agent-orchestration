package openclaw

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// Thin typed wrappers over Client.Call with the parameter names verified against the
// 2026.10.3 gateway. Each returns the decoded payload or the RPC error unchanged.

// ChatSend starts a turn. The gateway uses IdempotencyKey as the run id and dedupes on
// it: a repeat returns the same runId with status "ok" [probed].
func ChatSend(ctx context.Context, c Client, p ChatSendParams) (ChatSendResult, error) {
	var out ChatSendResult
	err := CallAs(ctx, c, "chat.send", p, &out)
	return out, err
}

// ChatAbortResult is the payload of chat.abort [probed].
type ChatAbortResult struct {
	OK      bool     `json:"ok"`
	Aborted bool     `json:"aborted"`
	RunIDs  []string `json:"runIds"`
}

// ChatAbort aborts one run of a session; the chat event for it then reaches state
// "aborted" with stopReason "rpc" [probed].
func ChatAbort(ctx context.Context, c Client, sessionKey, runID string) (ChatAbortResult, error) {
	var out ChatAbortResult
	err := CallAs(ctx, c, "chat.abort", map[string]any{"sessionKey": sessionKey, "runId": runID}, &out)
	return out, err
}

// SessionsSubscribeResult is the payload of sessions.messages.subscribe [probed].
type SessionsSubscribeResult struct {
	Subscribed     bool   `json:"subscribed"`
	Key            string `json:"key"`
	AgentID        string `json:"agentId"`
	ApprovalReplay struct {
		SessionKey string            `json:"sessionKey"`
		Approvals  []json.RawMessage `json:"approvals"`
		Truncated  bool              `json:"truncated"`
	} `json:"approvalReplay"`
}

// SessionsSubscribe subscribes this connection to session.message (and, with
// includeApprovals, session.approval) events of one session. Subscriptions are per
// connection and must be renewed after a reconnect.
func SessionsSubscribe(ctx context.Context, c Client, key, agentID string, includeApprovals bool) (SessionsSubscribeResult, error) {
	var out SessionsSubscribeResult
	err := CallAs(ctx, c, "sessions.messages.subscribe",
		map[string]any{"key": key, "agentId": agentID, "includeApprovals": includeApprovals}, &out)
	return out, err
}

// SessionsPatchModel sets the session's model override ("provider/model"). It works
// before the session's first turn and creates the session entry [probed].
func SessionsPatchModel(ctx context.Context, c Client, key, modelRef string) error {
	_, err := c.Call(ctx, "sessions.patch", map[string]any{"key": key, "model": modelRef})
	return err
}

// SessionsDelete removes a session; the gateway archives its transcript [probed].
func SessionsDelete(ctx context.Context, c Client, key string) error {
	_, err := c.Call(ctx, "sessions.delete", map[string]any{"key": key})
	return err
}

// AgentsList returns the configured agents.
func AgentsList(ctx context.Context, c Client) (AgentsListResult, error) {
	var out AgentsListResult
	err := CallAs(ctx, c, "agents.list", map[string]any{}, &out)
	return out, err
}

// AgentsCreateResult is the payload of agents.create [probed].
type AgentsCreateResult struct {
	OK        bool   `json:"ok"`
	AgentID   string `json:"agentId"`
	Name      string `json:"name"`
	Workspace string `json:"workspace"`
}

// AgentsCreate creates an agent. The gateway accepts only name and workspace and derives
// the id from the name as a lowercase slug ("ZZ Probe Two!" becomes "zz-probe-two");
// an existing id is refused with "agent \"<id>\" already exists" [probed].
func AgentsCreate(ctx context.Context, c Client, name, workspace string) (AgentsCreateResult, error) {
	var out AgentsCreateResult
	err := CallAs(ctx, c, "agents.create", map[string]any{"name": name, "workspace": workspace}, &out)
	return out, err
}

// AgentsDelete removes an agent and its config entry [probed].
func AgentsDelete(ctx context.Context, c Client, agentID string) error {
	_, err := c.Call(ctx, "agents.delete", map[string]any{"agentId": agentID})
	return err
}

// ConfigGet returns the current config and its hash.
func ConfigGet(ctx context.Context, c Client) (ConfigGetResult, error) {
	var out ConfigGetResult
	err := CallAs(ctx, c, "config.get", map[string]any{}, &out)
	return out, err
}

// ConfigPatchResult is the subset of the config.patch payload the platform reads.
type ConfigPatchResult struct {
	OK   bool   `json:"ok"`
	Path string `json:"path"`
	Hash string `json:"hash"`
}

// ConfigPatch merges raw (a JSON document) into the config. baseHash is the hash from
// config.get; replacePaths lists every array-bearing path being replaced or deleted.
func ConfigPatch(ctx context.Context, c Client, raw, baseHash string, replacePaths []string) (ConfigPatchResult, error) {
	var out ConfigPatchResult
	err := CallAs(ctx, c, "config.patch", ConfigPatchParams{Raw: raw, BaseHash: baseHash, ReplacePaths: replacePaths}, &out)
	return out, err
}

// ExecApprovalsGetResult is the payload of exec.approvals.get [probed]. File holds
// {version, socket, defaults, agents}.
type ExecApprovalsGetResult struct {
	Path   string          `json:"path"`
	Exists bool            `json:"exists"`
	File   json.RawMessage `json:"file"`
	Hash   string          `json:"hash"`
}

// ExecApprovalsGet returns the gateway's exec approvals file and its hash.
func ExecApprovalsGet(ctx context.Context, c Client) (ExecApprovalsGetResult, error) {
	var out ExecApprovalsGetResult
	err := CallAs(ctx, c, "exec.approvals.get", map[string]any{}, &out)
	return out, err
}

// ExecApprovalsSet replaces the exec approvals file; baseHash comes from the get.
func ExecApprovalsSet(ctx context.Context, c Client, file json.RawMessage, baseHash string) error {
	_, err := c.Call(ctx, "exec.approvals.set", map[string]any{"file": file, "baseHash": baseHash})
	return err
}

// ChatHistoryResult is the subset of chat.history the engine reconciles from [probed].
type ChatHistoryResult struct {
	SessionKey string            `json:"sessionKey"`
	SessionID  string            `json:"sessionId"`
	Messages   []json.RawMessage `json:"messages"`
	HasMore    bool              `json:"hasMore"`
}

// ChatHistory returns the newest limit transcript entries of a session, oldest first.
func ChatHistory(ctx context.Context, c Client, sessionKey, agentID string, limit int) (ChatHistoryResult, error) {
	var out ChatHistoryResult
	err := CallAs(ctx, c, "chat.history", map[string]any{"sessionKey": sessionKey, "agentId": agentID, "limit": limit}, &out)
	return out, err
}

// HistoryEntry is a transcript entry as chat.history and session.message carry it,
// including the toolResult fields and the gateway's bookkeeping block [probed].
type HistoryEntry struct {
	TranscriptEntry
	ToolCallID string `json:"toolCallId,omitempty"`
	ToolName   string `json:"toolName,omitempty"`
	IsError    bool   `json:"isError,omitempty"`
	// IdempotencyKey is set on a user entry injected with chat.send: the send's key plus
	// ":user" [probed 2026-10-16].
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
}

// Meta is the __openclaw block of a transcript entry.
type Meta struct {
	ID                string `json:"id"`
	RunID             string `json:"runId"`
	RecordTimestampMs int64  `json:"recordTimestampMs"`
	Seq               int    `json:"seq"`
}

// Meta decodes the __openclaw block; zero when absent.
func (h HistoryEntry) Meta() Meta {
	var m Meta
	if len(h.OpenClaw) > 0 {
		_ = json.Unmarshal(h.OpenClaw, &m)
	}
	return m
}

// ContentPart is one part of an assistant or tool result content array [probed:
// text and toolCall; thinking parts are dropped before storage, DESIGN 5.14 rule 11].
type ContentPart struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// Parts decodes Content as a part array; a plain string becomes one text part.
func (t TranscriptEntry) Parts() []ContentPart {
	if len(t.Content) == 0 {
		return nil
	}
	var parts []ContentPart
	if err := json.Unmarshal(t.Content, &parts); err == nil {
		return parts
	}
	var s string
	if err := json.Unmarshal(t.Content, &s); err == nil {
		return []ContentPart{{Type: "text", Text: s}}
	}
	return nil
}

// Text joins the text parts of an entry.
func (t TranscriptEntry) Text() string {
	var sb strings.Builder
	for _, p := range t.Parts() {
		if p.Type == "text" && p.Text != "" {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// IsReasoningPart reports whether a content part type carries model reasoning, which
// the platform never stores (DESIGN 5.14 rule 11).
func IsReasoningPart(typ string) bool {
	t := strings.ToLower(typ)
	return strings.Contains(t, "thinking") || strings.Contains(t, "reasoning")
}

// ---- approvals (DESIGN 5.3 rules 3 and 5a) ---------------------------------------------

// Error codes the gateway answers a resolve with when its side is already settled
// [probed 2026-10-15]: the decision was made elsewhere, the approval timed out or the
// gateway restarted. The platform's own decision stands either way.
const (
	CodeApprovalAlreadyResolved = "APPROVAL_ALREADY_RESOLVED"
	CodeApprovalNotFound        = "APPROVAL_NOT_FOUND"
)

// PluginApprovalRequestParams are the params of plugin.approval.request [probed]. The
// platform always sets TwoPhase: without it the call blocks until the decision.
type PluginApprovalRequestParams struct {
	Title            string        `json:"title"`
	Description      string        `json:"description"`
	Detail           string        `json:"detail,omitempty"`
	Severity         string        `json:"severity"` // info | warning | critical
	ToolName         string        `json:"toolName"`
	ToolCallID       string        `json:"toolCallId"`
	MCPTool          PluginMCPTool `json:"mcpTool"`
	AllowedDecisions []string      `json:"allowedDecisions"`
	AgentID          string        `json:"agentId,omitempty"`
	SessionKey       string        `json:"sessionKey,omitempty"`
	TimeoutMs        int64         `json:"timeoutMs"` // at most MaxPluginApprovalTimeoutMs
	TwoPhase         bool          `json:"twoPhase"`
}

// PluginMCPTool names the MCP tool a plugin approval gates.
type PluginMCPTool struct {
	Server string `json:"server"`
	Tool   string `json:"tool"`
}

// MaxPluginApprovalTimeoutMs is the gateway's ceiling for timeoutMs [probed].
const MaxPluginApprovalTimeoutMs = 600_000

// PluginApprovalRequestResult is the two-phase answer [probed]: status "accepted" and
// the gateway's id ("plugin:<uuid>").
type PluginApprovalRequestResult struct {
	Status        string          `json:"status"`
	ID            string          `json:"id"`
	DeliveryRoute json.RawMessage `json:"deliveryRoute,omitempty"`
	CreatedAtMs   int64           `json:"createdAtMs"`
	ExpiresAtMs   int64           `json:"expiresAtMs"`
}

// PluginApprovalRequest registers a plugin approval with the gateway's approval clients.
func PluginApprovalRequest(ctx context.Context, c Client, p PluginApprovalRequestParams) (PluginApprovalRequestResult, error) {
	var out PluginApprovalRequestResult
	err := CallAs(ctx, c, "plugin.approval.request", p, &out)
	return out, err
}

// PluginApprovalResolve decides a plugin approval (allow-once | allow-always | deny);
// the gateway answers {ok: true} and broadcasts plugin.approval.resolved [probed].
func PluginApprovalResolve(ctx context.Context, c Client, id, decision string) error {
	_, err := c.Call(ctx, "plugin.approval.resolve", map[string]any{"id": id, "decision": decision})
	return err
}

// ExecApprovalResolve decides an exec approval (allow-once | allow-always | deny) [docs].
func ExecApprovalResolve(ctx context.Context, c Client, id, decision string) error {
	_, err := c.Call(ctx, "exec.approval.resolve", ExecApprovalResolveParams{ID: id, Decision: decision})
	return err
}

// EventPluginApprovalResolved is broadcast to every approval client on any decision,
// and on the gateway's own timeout with ResolvedBy null [probed].
const EventPluginApprovalResolved = "plugin.approval.resolved"

// PluginApprovalResolvedEvent is the payload of plugin.approval.resolved [probed].
type PluginApprovalResolvedEvent struct {
	ID         string          `json:"id"`
	Decision   string          `json:"decision"` // allow-once | allow-always | deny
	ResolvedBy json.RawMessage `json:"resolvedBy"`
	TS         int64           `json:"ts"`
	Request    json.RawMessage `json:"request,omitempty"`
}

// Decided reports whether someone decided: the gateway's own timeout sends the same
// event with resolvedBy null, which is no decision (DESIGN 5.3 rule 5a).
func (e PluginApprovalResolvedEvent) Decided() bool {
	s := strings.TrimSpace(string(e.ResolvedBy))
	return s != "" && s != "null"
}

// IsSettledApprovalError reports whether a resolve failed because the gateway's side
// of the approval is already settled (decided, timed out, or forgotten on restart).
func IsSettledApprovalError(err error) bool {
	var re *RPCError
	if !errors.As(err, &re) || re == nil {
		return false
	}
	all := re.Code + " " + re.Message + " " + string(re.Details)
	return strings.Contains(all, CodeApprovalAlreadyResolved) || strings.Contains(all, CodeApprovalNotFound)
}

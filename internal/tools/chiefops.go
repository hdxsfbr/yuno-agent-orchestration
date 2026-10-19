package tools

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"yuno/internal/obs"
	"yuno/internal/store"
)

// The Chief's operator tools (D-081): list and decide approvals, and add a project, from
// the bound Telegram chat. They are chat-only and computational: a chat tool cannot be
// approval-gated (an approval pauses a run and a conversation has none), and the person
// asking in their own bound chat is the operator. What the model decides cannot be
// trusted (text models wrote reaches the same session as [platform] turns), so a decision
// carries the approval's short code, and the server accepts it only when the newest
// message the person sent in that chat contains the code (D-081).

const (
	ListApprovalsName  = "list_approvals"
	DecideApprovalName = "decide_approval"
	AddProjectName     = "add_project"

	listApprovalsMax     = 20
	approvalMessageMax   = 600
	approvalInputMax     = 240
	decisionReasonMax    = 2000
	notBoundChatText     = " is only available to the Chief of Staff in its Telegram chat"
	approvalDecisionBy   = "chat:"
	decisionApprove      = "approve"
	decisionReject       = "reject"
	listApprovalsScanMax = 100
)

var listApprovalsSchema = listAgentsSchema

var decideApprovalSchema = json.RawMessage(`{
  "type": "object",
  "properties": {` + tokenProperties + `
    "approval_id": {"type": "string", "minLength": 1, "description": "The id of the pending approval, as list_approvals shows it."},
    "code": {"type": "string", "minLength": 1, "description": "The approval's code, as list_approvals shows it. The person's latest message must contain it."},
    "decision": {"type": "string", "enum": ["approve", "reject"], "description": "What the person decided."},
    "reason": {"type": "string", "description": "Why, in the person's words. Required for reject."}
  },
  "required": ["approval_id", "code", "decision"],
  "additionalProperties": false
}`)

var addProjectSchema = json.RawMessage(`{
  "type": "object",
  "properties": {` + tokenProperties + `
    "repo_url": {"type": "string", "minLength": 1, "description": "The https URL of the repository, without credentials."},
    "provider": {"type": "string", "enum": ["gitlab", "github"], "description": "Inferred from the URL when absent."},
    "default_branch": {"type": "string", "description": "The branch merge requests target; main when absent."},
    "name": {"type": "string", "description": "The project's name; the repository's name when absent."},
    "intake": {
      "type": "object",
      "properties": {
        "label": {"type": "string", "description": "The issue label that becomes a run; yuno when absent."},
        "workflow": {"type": "string", "description": "The name of the workflow an issue runs; the Wave pipeline when absent."}
      },
      "additionalProperties": false,
      "description": "Present: turn issue intake on for the project."
    },
    "budget_usd_per_day": {"type": "number", "exclusiveMinimum": 0, "description": "The project's daily spending cap in USD; 10 when intake is requested and none is given."}
  },
  "required": ["repo_url"],
  "additionalProperties": false
}`)

// DecideApprovalFunc is the engine's DecideApproval (the web UI's path) behind a func.
type DecideApprovalFunc func(ctx context.Context, approvalID, decision, by, reason string) (*store.Approval, error)

// AddProjectRequest is one add_project call. Token is never part of it.
type AddProjectRequest struct {
	RepoURL       string
	Provider      string
	DefaultBranch string
	Name          string
	// Intake is nil when the call did not ask for intake.
	Intake          *AddProjectIntake
	BudgetUSDPerDay *float64
}

// AddProjectIntake carries the optional members of add_project's intake.
type AddProjectIntake struct {
	Label    string
	Workflow string
}

// AddProjectResult is what add_project answers.
type AddProjectResult struct {
	ProjectID      string   `json:"project_id"`
	Name           string   `json:"name"`
	IntakeEnabled  bool     `json:"intake_enabled"`
	IntakeLabel    string   `json:"intake_label,omitempty"`
	IntakeWorkflow string   `json:"intake_workflow,omitempty"`
	BudgetUSDDay   *float64 `json:"budget_usd_per_day,omitempty"`
	// TokenFrom names the project whose token was reused, "" when the server's
	// configured token was used. The token itself never leaves the server.
	TokenFrom string `json:"token_from_project,omitempty"`
	TokenSrc  string `json:"token_source"`
}

// AddProjectFunc creates the project through the API's own code; failures the model
// should read are *ModelError.
type AddProjectFunc func(ctx context.Context, req AddProjectRequest) (*AddProjectResult, error)

const (
	approvalCodeLen      = 4
	approvalCodeAlphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ" // no 0 O 1 I L
	platformMarker       = "[platform]"
)

// ApprovalCode is the short code the person must send to decide approval id: 4 characters
// of an unambiguous alphabet taken from an HMAC of the id under the server's key, so it
// needs no storage and a restart keeps it.
func ApprovalCode(key []byte, approvalID string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("approval-code:" + approvalID))
	sum := mac.Sum(nil)
	out := make([]byte, approvalCodeLen)
	for i := range out {
		out[i] = approvalCodeAlphabet[int(sum[i])%len(approvalCodeAlphabet)]
	}
	return string(out)
}

// containsCode reports whether text holds code as a whole token (not inside a longer run
// of letters or digits), ignoring case.
func containsCode(text, code string) bool {
	t, c := strings.ToUpper(text), strings.ToUpper(code)
	if c == "" {
		return false
	}
	for from := 0; ; {
		i := strings.Index(t[from:], c)
		if i < 0 {
			return false
		}
		i += from
		end := i + len(c)
		before, _ := utf8.DecodeLastRuneInString(t[:i])
		after, _ := utf8.DecodeRuneInString(t[end:])
		if !isWordRune(before) && !isWordRune(after) {
			return true
		}
		from = i + 1
	}
}

func isWordRune(r rune) bool {
	return r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r > 0x7f && unicode.IsLetter(r)
}

// newestPersonMessage is the text of the newest message the person sent in the
// conversation: inbound, and neither a [platform] report the engine injected (those carry
// the run they are about, and their text starts with the marker) nor the Chief's own.
func newestPersonMessage(ctx context.Context, pool *pgxpool.Pool, conversationID string) (string, error) {
	var text string
	err := pool.QueryRow(ctx, `SELECT text FROM conversation_messages
		WHERE conversation_id = $1 AND direction = $2 AND run_id IS NULL AND ltrim(text) NOT LIKE $3
		ORDER BY created_at DESC, id DESC LIMIT 1`, conversationID, store.DirectionIn, platformMarker+"%").Scan(&text)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return text, err
}

// boundChat checks that a call came with the chat token of the conversation bound to the
// Chief: a chat call (no run), whose conversation exists, is the calling agent's own and
// is on Telegram. It returns the conversation.
func boundChat(ctx context.Context, pool *pgxpool.Pool, name string, call Call) (*store.Conversation, error) {
	refused := &ModelError{Message: name + notBoundChatText}
	if !call.Chat || call.RunID != "" || call.ConversationID == "" || pool == nil {
		return nil, refused
	}
	conv, err := store.GetConversation(ctx, pool, call.ConversationID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, refused
	}
	if err != nil {
		return nil, err
	}
	if conv.AgentID != call.AgentID || conv.Channel != store.ChannelTelegram {
		return nil, refused
	}
	return conv, nil
}

// NewListApprovals builds list_approvals: the pending approvals of runs, newest first.
func NewListApprovals(pool *pgxpool.Pool, codeKey []byte) *Tool {
	return &Tool{
		Name: ListApprovalsName,
		Description: "List what is waiting for the person's approval: for each pending approval its id, its code (show it to " +
			"the person: they must send it to decide), run id, " +
			"workflow, step, the approval message, whether it is an approval step or a tool call (with the tool " +
			"and a short summary of its input) and when it was asked, newest first, at most 20. Questions a run " +
			"asks are not listed; those are answered with answer_question.",
		InputSchema: listApprovalsSchema,
		Risk:        RiskReadOnly,
		Hits:        HitsPostgres,
		Timeout:     5 * time.Second,
		Chat:        true,
		Exec: func(ctx context.Context, call Call) (Result, error) {
			if _, err := boundChat(ctx, pool, ListApprovalsName, call); err != nil {
				return Result{}, err
			}
			return execListApprovals(ctx, pool, codeKey)
		},
	}
}

func execListApprovals(ctx context.Context, pool *pgxpool.Pool, codeKey []byte) (Result, error) {
	items, err := store.ListApprovalItems(ctx, pool, store.ApprovalPending, "", listApprovalsScanMax)
	if err != nil {
		return Result{}, err
	}
	var runIDs []string
	for _, it := range items {
		if it.Kind != store.ApprovalKindQuestion && it.RunID != "" {
			runIDs = append(runIDs, it.RunID)
		}
	}
	names, err := workflowNamesOfRuns(ctx, pool, runIDs)
	if err != nil {
		return Result{}, err
	}
	type view struct {
		ID           string `json:"id"`
		Code         string `json:"code"`
		RunID        string `json:"run_id"`
		Workflow     string `json:"workflow"`
		Node         string `json:"node"`
		Message      string `json:"message"`
		Kind         string `json:"kind"`
		Tool         string `json:"tool,omitempty"`
		InputSummary string `json:"input_summary,omitempty"`
		CreatedAt    string `json:"created_at"`
	}
	out := []view{}
	for _, it := range items {
		if it.Kind == store.ApprovalKindQuestion || len(out) == listApprovalsMax {
			continue
		}
		v := view{ID: it.ID, Code: ApprovalCode(codeKey, it.ID), RunID: it.RunID, Workflow: names[it.RunID], Node: it.NodeID,
			Message: cutRunes(it.Message, approvalMessageMax), CreatedAt: it.RequestedAt.UTC().Format(time.RFC3339)}
		switch it.Kind {
		case store.ApprovalKindNode:
			v.Kind = "approval"
		case store.ApprovalKindTool:
			v.Kind = "tool_call"
			if it.ToolCall != nil {
				v.Tool = it.ToolCall.ToolName
				v.InputSummary = summarizeInput(it.ToolCall.Input)
			}
		default:
			v.Kind = it.Kind
		}
		out = append(out, v)
	}
	return JSONResult(map[string]any{"approvals": out}), nil
}

func workflowNamesOfRuns(ctx context.Context, pool *pgxpool.Pool, runIDs []string) (map[string]string, error) {
	out := map[string]string{}
	if len(runIDs) == 0 {
		return out, nil
	}
	rows, err := pool.Query(ctx, `SELECT r.id, w.name FROM runs r JOIN workflows w ON w.id = r.workflow_id WHERE r.id = ANY($1)`, runIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

// summarizeInput is a tool call's input as one short line, secrets redacted (again: the
// stored input is redacted already) and the platform's own token fields left out.
func summarizeInput(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	red := obs.RedactJSON(raw)
	var m map[string]json.RawMessage
	if json.Unmarshal(red, &m) == nil {
		delete(m, RunTokenField)
		delete(m, ChatTokenField)
		if b, err := json.Marshal(m); err == nil {
			red = b
		}
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, red); err != nil {
		return ""
	}
	return cutRunes(buf.String(), approvalInputMax)
}

func cutRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

type decideApprovalInput struct {
	ApprovalID string `json:"approval_id"`
	Code       string `json:"code"`
	Decision   string `json:"decision"`
	Reason     string `json:"reason"`
}

// NewDecideApproval builds decide_approval: the decision goes through the engine's
// DecideApproval, the call the web UI makes, so on_reject, auto_after and idempotency
// apply as they do there. The audit trail reads decided_by chat:<channel>.
func NewDecideApproval(pool *pgxpool.Pool, decide DecideApprovalFunc, codeKey []byte) *Tool {
	return &Tool{
		Name: DecideApprovalName,
		Description: "Approve or reject a pending approval, but only when the person has told you to, in this " +
			"conversation, after you quoted the approval and its code to them. Give the approval id and code from list_approvals, " +
			"the decision (approve or reject) and, for a reject, the reason in their words. The platform accepts " +
			"the call only when the person's latest message contains the code. Returns the decided " +
			"approval and its run.",
		InputSchema: decideApprovalSchema,
		Risk:        RiskComputational,
		Hits:        HitsPostgres,
		Timeout:     30 * time.Second,
		Chat:        true,
		Exec: func(ctx context.Context, call Call) (Result, error) {
			conv, err := boundChat(ctx, pool, DecideApprovalName, call)
			if err != nil {
				return Result{}, err
			}
			return execDecideApproval(ctx, pool, decide, codeKey, conv, call)
		},
	}
}

func execDecideApproval(ctx context.Context, pool *pgxpool.Pool, decide DecideApprovalFunc, codeKey []byte, conv *store.Conversation, call Call) (Result, error) {
	var in decideApprovalInput
	if err := json.Unmarshal(call.Input, &in); err != nil {
		return Result{}, &ModelError{Message: "invalid input: " + err.Error()}
	}
	id := strings.TrimSpace(in.ApprovalID)
	reason := strings.TrimSpace(in.Reason)
	switch {
	case !plainText(id):
		return Result{}, &ModelError{Message: "no approval " + strings.ToValidUTF8(strings.ReplaceAll(id, "\x00", ""), "")}
	case in.Decision != decisionApprove && in.Decision != decisionReject:
		return Result{}, &ModelError{Message: "decision: approve or reject"}
	case in.Decision == decisionReject && reason == "":
		return Result{}, &ModelError{Message: "reason: required to reject; ask the person why"}
	case utf8.RuneCountInString(reason) > decisionReasonMax || !plainText(reason):
		return Result{}, &ModelError{Message: "reason: at most 2000 characters"}
	}
	if decide == nil {
		return Result{}, errors.New("decide_approval: not configured")
	}
	a, err := pendingApproval(ctx, pool, id)
	if err != nil {
		return Result{}, err
	}
	want := ApprovalCode(codeKey, id)
	if !strings.EqualFold(strings.TrimSpace(in.Code), want) {
		return Result{}, &ModelError{Message: "code: not this approval's code; list_approvals shows it. Show the approval and its code to the person and ask them to reply with the code"}
	}
	latest, err := newestPersonMessage(ctx, pool, conv.ID)
	if err != nil {
		return Result{}, err
	}
	if !containsCode(latest, want) {
		return Result{}, &ModelError{Message: "the person's latest message does not contain the approval's code. Ask them to reply with the code " + want +
			" to decide this approval; do not decide without it"}
	}
	row, err := decide(ctx, id, in.Decision, approvalDecisionBy+conv.Channel, reason)
	if err != nil {
		var me *ModelError
		if errors.As(err, &me) {
			return Result{}, err
		}
		// The engine's sentinels are out of reach (it imports this package): a row decided
		// between the check and the decision reads as already decided.
		if _, rerr := pendingApproval(ctx, pool, id); rerr != nil {
			return Result{}, rerr
		}
		return Result{}, err
	}
	status := ""
	if row != nil {
		status = row.Status
	}
	return JSONResult(map[string]any{"approval_id": id, "status": status, "run_id": a.RunID}), nil
}

// pendingApproval returns the row when id is a pending approval a person decides (a
// question is answered, not decided); otherwise the *ModelError the model reads.
func pendingApproval(ctx context.Context, pool *pgxpool.Pool, id string) (*store.Approval, error) {
	a, err := store.GetApproval(ctx, pool, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, &ModelError{Message: "no approval " + id}
	case err != nil:
		return nil, err
	case a.Kind == store.ApprovalKindQuestion:
		return nil, &ModelError{Message: id + " is a question; answer it with answer_question"}
	case a.Status != store.ApprovalPending:
		return nil, &ModelError{Message: "approval " + id + " is already " + a.Status}
	}
	return a, nil
}

type addProjectInput struct {
	RepoURL         string   `json:"repo_url"`
	Provider        string   `json:"provider"`
	DefaultBranch   string   `json:"default_branch"`
	Name            string   `json:"name"`
	BudgetUSDPerDay *float64 `json:"budget_usd_per_day"`
	Intake          *struct {
		Label    string `json:"label"`
		Workflow string `json:"workflow"`
	} `json:"intake"`
}

// NewAddProject builds add_project. The schema has no token member and refuses extra
// ones: the server picks the token itself (that of a project in the same repository
// namespace, else PROJECT_GIT_TOKEN).
func NewAddProject(pool *pgxpool.Pool, add AddProjectFunc) *Tool {
	return &Tool{
		Name: AddProjectName,
		Description: "Add a git repository as a project. Give the repository URL (https, no credentials) and, " +
			"if the person wants, the provider, default branch, name, and intake (labelled issues become runs) " +
			"with its label and workflow, and a daily budget in USD. Never ask the person for a token and never " +
			"put one in a message: the platform uses the token of a project in the same group or owner, or the " +
			"configured one, and says when there is none (the person then adds the project once on the Projects page). " +
			"Returns the project id, name, intake state and which project's token was reused.",
		InputSchema: addProjectSchema,
		Risk:        RiskComputational,
		Hits:        HitsPostgres,
		Timeout:     30 * time.Second,
		Chat:        true,
		Exec: func(ctx context.Context, call Call) (Result, error) {
			if _, err := boundChat(ctx, pool, AddProjectName, call); err != nil {
				return Result{}, err
			}
			var in addProjectInput
			if err := json.Unmarshal(call.Input, &in); err != nil {
				return Result{}, &ModelError{Message: "invalid input: " + err.Error()}
			}
			if add == nil {
				return Result{}, errors.New("add_project: not configured")
			}
			req := AddProjectRequest{RepoURL: strings.TrimSpace(in.RepoURL), Provider: in.Provider,
				DefaultBranch: in.DefaultBranch, Name: in.Name, BudgetUSDPerDay: in.BudgetUSDPerDay}
			if in.Intake != nil {
				req.Intake = &AddProjectIntake{Label: in.Intake.Label, Workflow: in.Intake.Workflow}
			}
			res, err := add(ctx, req)
			if err != nil {
				return Result{}, err
			}
			return JSONResult(res), nil
		},
	}
}

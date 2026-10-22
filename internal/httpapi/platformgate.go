package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"yuno/internal/actor"
	"yuno/internal/obs"
	"yuno/internal/openclaw"
	"yuno/internal/store"
	"yuno/internal/tools"
)

// The platform gate (Chief platform control, wave 1, DESIGN A3, A5, A6 and A8; its place
// in the chain is B6): a request whose context carries an agent actor, which only
// platform_call's in-process dispatch can set, is held to the risk class of its operation.
// Every other request passes untouched.

const (
	// platformApprovalTTL is how long a platform approval may wait and then be used,
	// checked at use (rule A6).
	platformApprovalTTL = time.Hour
	// platformReplayWindow is how long a used approval lets its repeat through to
	// idempotency, which replays the stored response: the key's own lifetime (DESIGN B3).
	// After it a repeat asks again rather than run unapproved.
	platformReplayWindow = 24 * time.Hour
	// gateBodyMax bounds the body the gate reads to check and hash.
	gateBodyMax = 4 << 20
	// approvalShownMax caps the body and the current state an approval message shows.
	approvalShownMax = 2000
)

// The gate's refusals (rules A3, A6 and A8), in the texts the agent reads.
const (
	gateMachine     = "not available to agents"
	gateKeyRequired = "idempotency_key is required: a new one per action, the same one when you repeat this action"
	gateSecret      = "secrets are entered by the operator in the UI"
	gateExpired     = "expired; call again to ask again"
	gateAskFailed   = "could not ask for approval"
	gateBodyTooBig  = "the body is larger than 4 MiB"
)

// Audit outcomes the gate decides (DESIGN B1).
const (
	outcomeDenied          = "denied"
	outcomePendingApproval = "pending_approval"
)

func (s *Server) platformGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		who, ok := actor.From(r.Context())
		if !ok || who.Kind != actor.Agent {
			next.ServeHTTP(w, r)
			return
		}
		g := &gateRequest{s: s, w: w, r: r, who: who, next: next, note: actor.NoteFrom(r.Context()), evidence: map[string]any{}}
		g.serve()
	})
}

// gateRequest is one agent request on its way through the gate.
type gateRequest struct {
	s        *Server
	w        http.ResponseWriter
	r        *http.Request
	who      actor.Actor
	next     http.Handler
	note     *actor.Note
	op       *tools.PlatformOperation
	evidence map[string]any
}

func (g *gateRequest) serve() {
	_, pattern := g.s.mux.Handler(g.r)
	if pattern == "" {
		// No route: the mux answers 404, 405 or its redirect.
		g.next.ServeHTTP(g.w, g.r)
		return
	}
	op, ok := g.s.contract.ForPattern(pattern)
	if !ok || op.Risk == tools.PlatformMachine {
		// /mcp, the model proxy and every route the contract calls machine (rule A3).
		g.deny(http.StatusForbidden, "not_available_to_agents", gateMachine)
		return
	}
	g.op = op
	g.evidence["operation_id"], g.evidence["risk"] = op.ID, string(op.Risk)
	switch op.Risk {
	case tools.PlatformDedicated:
		g.deny(http.StatusForbidden, "use_dedicated_tool", "use "+op.Tool)
		return
	case tools.PlatformRead:
		g.pass()
		return
	}
	key := strings.TrimSpace(g.r.Header.Get(tools.HeaderIdempotencyKey))
	if key == "" {
		g.deny(http.StatusPreconditionRequired, "idempotency_key_required", gateKeyRequired)
		return
	}
	if op.GetTwin != "" && (op.Method == http.MethodPut || op.Method == http.MethodPatch || op.Method == http.MethodDelete) &&
		strings.TrimSpace(g.r.Header.Get(tools.HeaderIfMatch)) == "" {
		g.deny(http.StatusPreconditionRequired, "if_match_required", "read it first with "+op.GetTwin+" and pass its etag as if_match")
		return
	}
	var body []byte
	if g.r.Body != nil {
		var err error
		if body, err = io.ReadAll(io.LimitReader(g.r.Body, gateBodyMax+1)); err != nil || len(body) > gateBodyMax {
			g.deny(http.StatusRequestEntityTooLarge, "body_too_large", gateBodyTooBig)
			return
		}
	}
	g.r.Body = io.NopCloser(bytes.NewReader(body))
	if tools.SecretInBody(body) {
		g.deny(http.StatusUnprocessableEntity, "secret_in_body", gateSecret)
		return
	}
	if machineFields(op, body) {
		g.deny(http.StatusForbidden, "not_available_to_agents", gateMachine)
		return
	}
	risk, reasons, err := g.s.platformRisk(g.r.Context(), op, g.r, g.who, body)
	if err != nil {
		g.s.internalError(g.w, g.r, "platform gate", err)
		return
	}
	g.evidence["risk"] = string(risk)
	if len(reasons) > 0 {
		g.evidence["reasons"] = reasons
	}
	if !risk.Gated() {
		g.pass()
		return
	}
	g.approval(key, risk, reasons, body)
}

// machineFields reports a request that names what only the platform's own machinery
// may (the parity plan, row 32): a run with a scripted model or the eval trigger.
func machineFields(op *tools.PlatformOperation, body []byte) bool {
	if op.ID != "createRun" {
		return false
	}
	var in runCreate
	if json.Unmarshal(body, &in) != nil {
		return false
	}
	return strings.TrimSpace(in.ScriptID) != "" || in.Trigger == store.TriggerEval
}

// approval is rule A6: a destructive or privileged call runs only with an approved,
// unused approval bound to it; the first call asks for one.
func (g *gateRequest) approval(key string, risk tools.PlatformRisk, reasons []string, body []byte) {
	ctx := g.r.Context()
	if g.s.pool == nil {
		g.deny(http.StatusServiceUnavailable, "approval_unavailable", gateAskFailed)
		return
	}
	now := time.Now().UTC()
	b := store.PlatformBinding{AgentID: g.who.ID, OperationID: g.op.ID, Path: g.r.URL.RequestURI(),
		BodySHA256: tools.BodySHA256(body), IdempotencyKey: key}
	since := now.Add(-platformReplayWindow)
	a, err := store.FindPlatformApproval(ctx, g.s.pool, b, since)
	if errors.Is(err, store.ErrNotFound) {
		var created bool
		a, created, err = store.CreatePlatformApproval(ctx, g.s.pool, g.newApproval(now, risk, reasons, body), b, since)
		if err == nil && created {
			g.s.emit(ctx, "", "approval.requested", map[string]any{"approval": a})
			g.s.log.Info("platform call waits for approval", "agent_id", g.who.ID, "operation_id", g.op.ID, "approval_id", a.ID)
			g.pending(a)
			return
		}
	}
	if err != nil {
		g.s.log.Error("platform approval failed", "agent_id", g.who.ID, "operation_id", g.op.ID, "error", err)
		g.deny(http.StatusServiceUnavailable, "approval_unavailable", gateAskFailed)
		return
	}
	g.evidence["approval_id"] = a.ID
	if g.note != nil {
		g.note.ApprovalID = a.ID
	}
	switch a.Status {
	case store.ApprovalPending:
		if !now.Before(a.ExpiresAt) {
			g.expire(a, now)
			return
		}
		g.pending(a)
	case store.ApprovalApproved:
		if a.UsedAt == nil {
			if !now.Before(a.ExpiresAt) {
				g.expire(a, now)
				return
			}
			// Spent before the handler runs: a crash between the two leaves the action
			// not done and the approval spent, the safe direction (A6 consistency). A
			// repeat that lost the race to spend it passes on to idempotency.
			if _, err := store.UsePlatformApproval(ctx, g.s.pool, a.ID, now); err != nil {
				g.s.log.Error("spend platform approval failed", "approval_id", a.ID, "error", err)
				g.deny(http.StatusServiceUnavailable, "approval_unavailable", gateAskFailed)
				return
			}
		}
		g.evidence["decided_by"] = deref(a.DecidedBy, "")
		if a.DecidedAt != nil {
			g.evidence["decided_at"] = a.DecidedAt.UTC().Format(time.RFC3339)
		}
		g.pass()
	case store.ApprovalRejected:
		g.deny(http.StatusForbidden, "approval_rejected", "rejected by "+deref(a.DecidedBy, "the operator"))
	default:
		g.expire(a, now)
	}
}

// expire tells the agent its approval expired and marks that told, so the next repeat
// asks again (rule A6, checked at use).
func (g *gateRequest) expire(a *store.Approval, now time.Time) {
	ctx := g.r.Context()
	changed, err := store.ExpirePlatformApproval(ctx, g.s.pool, a.ID, now)
	switch {
	case err == nil && a.Status != store.ApprovalExpired:
		g.s.emit(ctx, "", "approval.decided", map[string]any{"approval": changed})
	case err != nil && !errors.Is(err, store.ErrNotFound):
		g.s.log.Warn("record platform approval expiry failed", "approval_id", a.ID, "error", err)
	}
	g.deny(http.StatusForbidden, "approval_expired", gateExpired)
}

// platformPending is the gate's answer while a human decides (rule A6).
type platformPending struct {
	Status     string    `json:"status"` // "pending_approval"
	ApprovalID string    `json:"approval_id"`
	Code       string    `json:"code"`
	ExpiresAt  time.Time `json:"expires_at"`
	Message    string    `json:"message"`
}

const platformPendingMessage = "a human must approve this call first: give the person the code; once they approve it " +
	"(in the web UI, or by sending you the code, which you pass to decide_approval), repeat this exact call with the same idempotency_key"

func (g *gateRequest) pending(a *store.Approval) {
	if g.note != nil {
		g.note.Outcome, g.note.ApprovalID = outcomePendingApproval, a.ID
		g.note.Evidence = g.evidence
	}
	g.w.Header().Set(tools.HeaderPlatformGate, tools.GatePending)
	writeJSON(g.w, http.StatusAccepted, platformPending{Status: tools.GatePending, ApprovalID: a.ID,
		Code: tools.ApprovalCode(g.s.codeKey, a.ID), ExpiresAt: a.ExpiresAt.UTC(), Message: platformPendingMessage})
}

func (g *gateRequest) pass() {
	if g.note != nil {
		g.note.Evidence = g.evidence
	}
	g.next.ServeHTTP(g.w, g.r)
}

func (g *gateRequest) deny(status int, code, message string) {
	if g.note != nil {
		g.note.Outcome, g.note.ErrorCode = outcomeDenied, code
		g.note.Evidence = g.evidence
	}
	g.s.log.Info("platform call refused", "agent_id", g.who.ID, "path", g.r.URL.Path, "code", code)
	g.w.Header().Set(tools.HeaderPlatformGate, tools.GateDenied)
	writeError(g.w, status, code, message, nil)
}

// newApproval is the pending approval of the call (rule A6): its message names who asks,
// the operation, the path, why it needs a human, the redacted body and a redacted read of
// the target as it is now. It expires an hour from now.
func (g *gateRequest) newApproval(now time.Time, risk tools.PlatformRisk, reasons []string, body []byte) *store.Approval {
	who := g.who.Name
	if who == "" {
		who = "An agent"
	}
	var m strings.Builder
	m.WriteString(who + " asks to run " + g.op.ID + ": " + g.op.Method + " " + g.r.URL.RequestURI())
	if g.op.Summary != "" {
		m.WriteString(" (" + g.op.Summary + ")")
	}
	m.WriteString(".")
	if g.who.OnBehalfOf != "" {
		m.WriteString(" On behalf of " + g.who.OnBehalfOf + ".")
	}
	why := reasons
	if len(why) == 0 {
		why = []string{riskReason(risk)}
	}
	m.WriteString("\nIt needs your approval because " + strings.Join(why, "; ") + ".")
	if len(bytes.TrimSpace(body)) > 0 {
		m.WriteString("\nBody: " + obs.Truncate(string(obs.RedactJSON(body)), approvalShownMax))
	}
	if before := g.s.approvalBefore(g.r); before != "" {
		m.WriteString("\nNow: " + obs.Truncate(before, approvalShownMax))
	}
	return &store.Approval{Message: obs.Redact(m.String()), Status: store.ApprovalPending, RequestedAt: now,
		ExpiresAt: now.Add(platformApprovalTTL)}
}

func riskReason(risk tools.PlatformRisk) string {
	if risk == tools.PlatformDestructive {
		return "it deletes, cancels or merges something"
	}
	return "it changes who can do what on the platform"
}

// approvalBefore is a redacted read of what the request changes, the audit's target
// (DESIGN 5.31 rule 2), for the approval's message; "" when there is none, when it is a
// collection a POST adds to, or when the read fails.
func (s *Server) approvalBefore(r *http.Request) string {
	_, target, collection := s.routeTarget(r)
	if target == "" || collection {
		return ""
	}
	status, ct, body := s.readTarget(r.Context(), target)
	if status != http.StatusOK || !isJSON(ct) {
		return ""
	}
	return string(obs.RedactJSON(body))
}

// platformRisk is rule A5: the operation's risk, raised to privileged, with the reasons a
// person reads, when the call grants more than its class says. The comparisons are with the
// stored agent or project; a body the handler will refuse anyway raises nothing.
func (s *Server) platformRisk(ctx context.Context, op *tools.PlatformOperation, r *http.Request, who actor.Actor, body []byte) (tools.PlatformRisk, []string, error) {
	params := contractPathValues(op.Path, r.URL.EscapedPath())
	if hasControl(params["id"]) {
		// No record has such an id (the handler answers 404), and the lookup would fail.
		return op.Risk, nil, nil
	}
	var reasons []string
	switch op.ID {
	case "createAgent":
		var in agentInput
		if json.Unmarshal(body, &in) == nil && (len(in.ToolAllowlist) > 0 || len(in.InteractionRules) > 0 || !emptyGuardrails(in.Guardrails)) {
			reasons = append(reasons, "it gives a new agent tools, interaction rules or guardrails")
		}
	case "updateAgent":
		if params["id"] == who.ID {
			reasons = append(reasons, "it changes the calling agent itself")
		}
		var in agentInput
		if json.Unmarshal(body, &in) != nil {
			break
		}
		cur, err := store.GetAgent(ctx, s.pool, params["id"])
		if errors.Is(err, store.ErrNotFound) {
			break
		}
		if err != nil {
			return "", nil, err
		}
		if addsTools(cur.ToolAllowlist, in.ToolAllowlist) {
			reasons = append(reasons, "it adds tools to the agent's allowlist")
		}
		if s.loosensRules(cur.InteractionRules, in.InteractionRules) {
			reasons = append(reasons, "it adds or loosens the agent's interaction rules")
		}
		if loosensGuardrails(cur.Guardrails, in.Guardrails) {
			reasons = append(reasons, "it loosens the agent's guardrails")
		}
		if loosensExecPolicy(cur.ExecPolicy, in.ExecPolicy) {
			reasons = append(reasons, "it loosens the agent's exec policy")
		}
	case "putAgentFile":
		if params["id"] == who.ID {
			reasons = append(reasons, "it changes the calling agent itself")
		}
	case "updateProject":
		var in projectInput
		if json.Unmarshal(body, &in) != nil {
			break
		}
		next, token, fe := in.validate(false)
		if len(fe) > 0 {
			break
		}
		cur, err := store.GetProject(ctx, s.pool, params["id"])
		if errors.Is(err, store.ErrNotFound) {
			break
		}
		if err != nil {
			return "", nil, err
		}
		if next.RepoURL != cur.RepoURL || next.Provider != cur.Provider || token != "" {
			reasons = append(reasons, "it changes the project's repository, provider or token")
		}
		if raisesCap(cur.BudgetDayUSDMicros, next.BudgetDayUSDMicros) || raisesCap(cur.BudgetMonthUSDMicros, next.BudgetMonthUSDMicros) {
			reasons = append(reasons, "it raises the project's budget cap")
		}
	case "createRun":
		var in runCreate
		if json.Unmarshal(body, &in) == nil && limitsAbove(in.Limits, store.DefaultLimits) {
			reasons = append(reasons, "it sets run limits above the platform defaults")
		}
	}
	if len(reasons) > 0 && !op.Risk.Gated() {
		return tools.PlatformPrivileged, reasons, nil
	}
	return op.Risk, reasons, nil
}

// contractPathValues maps the {name}s of a contract path to the request's unescaped
// segments; the last one takes the rest of the path.
func contractPathValues(contractPath, escapedPath string) map[string]string {
	want := strings.Split(strings.Trim(contractPath, "/"), "/")
	got := strings.Split(strings.Trim(escapedPath, "/"), "/")
	out := map[string]string{}
	for i, seg := range want {
		if i >= len(got) || !strings.HasPrefix(seg, "{") {
			continue
		}
		raw := got[i]
		if i == len(want)-1 {
			raw = strings.Join(got[i:], "/")
		}
		if v, err := url.PathUnescape(raw); err == nil {
			out[strings.Trim(seg, "{}")] = v
		}
	}
	return out
}

func emptyGuardrails(g store.Guardrails) bool {
	return g.MaxCostUSDPerRun == 0 && g.MaxRunsPerHour == 0 && len(g.BlockedTools) == 0 && len(g.BlockedInputPatterns) == 0
}

// addsTools reports a tool in next that cur does not allow, prefix or not.
func addsTools(cur, next []string) bool {
	for _, t := range next {
		if !slices.ContainsFunc(cur, func(c string) bool { return tools.PlainName(c) == tools.PlainName(t) }) {
			return true
		}
	}
	return false
}

// policyRank orders the interaction rules from the loosest.
var policyRank = map[string]int{tools.PolicyAuto: 0, tools.PolicyRequireApproval: 1, tools.PolicyBlocked: 2}

// loosensRules reports a tool whose effective interaction rule in next is looser than in
// cur: an entry added or changed to something looser than what applied, or removed where
// the default is looser (DESIGN 5.3 rule 6's default: require_approval for a
// side-effecting tool, auto otherwise).
func (s *Server) loosensRules(cur, next map[string]string) bool {
	effective := func(rules map[string]string, name string) int {
		for k, v := range rules {
			if tools.PlainName(k) == name && v != "" {
				return policyRank[v]
			}
		}
		if t, ok := s.tools.Get(name); ok && t.Risk == tools.RiskSideEffecting {
			return policyRank[tools.PolicyRequireApproval]
		}
		return policyRank[tools.PolicyAuto]
	}
	names := map[string]bool{}
	for k := range cur {
		names[tools.PlainName(k)] = true
	}
	for k := range next {
		names[tools.PlainName(k)] = true
	}
	for name := range names {
		if effective(next, name) < effective(cur, name) {
			return true
		}
	}
	return false
}

// loosensGuardrails reports a cap raised or removed, or a blocked tool or input pattern
// taken away.
func loosensGuardrails(cur, next store.Guardrails) bool {
	raised := func(c, n float64) bool { return c > 0 && (n <= 0 || n > c) }
	if raised(cur.MaxCostUSDPerRun, next.MaxCostUSDPerRun) || raised(float64(cur.MaxRunsPerHour), float64(next.MaxRunsPerHour)) {
		return true
	}
	for _, t := range cur.BlockedTools {
		if !slices.ContainsFunc(next.BlockedTools, func(n string) bool { return tools.PlainName(n) == tools.PlainName(t) }) {
			return true
		}
	}
	for _, p := range cur.BlockedInputPatterns {
		if !slices.Contains(next.BlockedInputPatterns, p) {
			return true
		}
	}
	return false
}

// Exec policy ranks (DESIGN 5.3 rule 1): security from the strictest, ask from the loosest.
var (
	execSecurityRank = map[string]int{"deny": 0, "allowlist": 1, "full": 2}
	execAskRank      = map[string]int{"off": 0, "on-miss": 1, "always": 2}
)

// loosensExecPolicy reports a wider security or fallback, a laxer ask, or a command
// pattern added to the allowlist; an empty value is the default the runtime applies.
func loosensExecPolicy(cur, next store.ExecPolicy) bool {
	or := func(v, def string) string {
		if v == "" {
			return def
		}
		return v
	}
	if execSecurityRank[or(next.Security, openclaw.DefaultExecSecurity)] > execSecurityRank[or(cur.Security, openclaw.DefaultExecSecurity)] ||
		execAskRank[or(next.Ask, openclaw.DefaultExecAsk)] < execAskRank[or(cur.Ask, openclaw.DefaultExecAsk)] ||
		execSecurityRank[or(next.AskFallback, openclaw.DefaultExecAskFallback)] > execSecurityRank[or(cur.AskFallback, openclaw.DefaultExecAskFallback)] {
		return true
	}
	for _, p := range next.Allowlist {
		if !slices.Contains(cur.Allowlist, p) {
			return true
		}
	}
	return false
}

// raisesCap reports a budget cap removed or raised.
func raisesCap(cur, next *store.USDMicros) bool {
	return cur != nil && (next == nil || *next > *cur)
}

// limitsAbove reports a run limit above the platform default (zero is the default).
func limitsAbove(l, d store.Limits) bool {
	return l.MaxSteps > d.MaxSteps || l.MaxDurationSeconds > d.MaxDurationSeconds || l.MaxNodeIterations > d.MaxNodeIterations ||
		l.MaxLoopVisits > d.MaxLoopVisits || l.MaxCostUSD > d.MaxCostUSD || l.MaxToolCalls > d.MaxToolCalls ||
		l.ApprovalTimeoutSeconds > d.ApprovalTimeoutSeconds
}

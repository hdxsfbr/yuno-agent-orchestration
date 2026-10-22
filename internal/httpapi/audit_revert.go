package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"unicode/utf8"

	"yuno/internal/actor"
	"yuno/internal/obs"
	"yuno/internal/store"
	"yuno/internal/tools"
)

// POST /api/audit/{id}/revert (DESIGN 5.35 rule 5): a change the audit log recorded (DESIGN
// 5.31 rules 1 and 2) is undone by writing the state from before it back through the
// resource's own PUT handler, behind the same If-Match check as any other update (5.31 rule
// 4). Workflows are reverted by restoring a version (DESIGN 5.35 rule 3).

// revertRoute is the revert's mux pattern without its method.
const revertRoute = "/api/audit/{id}/revert"

// The revert's answers, in the texts a person reads.
const (
	revertChangedSince  = "changed again since; revert the later change first"
	revertNothing       = "nothing to revert: the change touched only what a revert leaves alone, such as a token"
	revertNoBefore      = "nothing to revert: the state before the change was not recorded"
	revertNoAfter       = "the state after the change was not recorded, so a later change cannot be ruled out"
	revertRedacted      = "the state before the change holds a value the audit log keeps redacted, such as a credential in a text, so it cannot be written back; change it by hand"
	revertOnlyPut       = "only a change that succeeded (PUT) on an agent, a schedule, a skill or a project can be reverted"
	revertWorkflow      = "a workflow change is undone by restoring an earlier version: POST /api/workflows/{id}/versions/{version}/restore"
	revertAuditNotFound = "audit event not found"
)

const (
	codeNothingToRevert = "nothing_to_revert"
	codeNotRevertible   = "not_revertible"
	codeVersionConflict = "version_conflict"
	// auditIDMax bounds an audit id looked up (ids are uuidv7 text).
	auditIDMax = 200
)

// revertMapper is one resource's mapper (rule 5): from a recorded state, the GET
// representation, to the input of the resource's PUT route.
type revertMapper struct {
	// op is the PUT operation a revert applies.
	op string
	// fields are the input's fields that the representation carries; the read-only ones
	// (ids, times, spend, channels, the token's presence) are left out, and so is every token:
	// tokens are write-only and never reverted.
	fields []string
	// clear are fields the PUT keeps when absent; a state without them sends "" to clear them.
	clear []string
}

// revertMappers are the routes a revert undoes, by mux pattern (rule 5).
var revertMappers = map[string]revertMapper{
	"/api/agents/{id}": {op: "updateAgent", fields: []string{"name", "role", "system_prompt", "model", "effort", "thinking",
		"max_tokens", "tool_allowlist", "interaction_rules", "limits", "guardrails", "memory_policy", "exec_policy", "skills"}},
	"/api/schedules/{id}": {op: "updateSchedule", fields: []string{"workflow_id", "project_id", "merge_request_id", "kind",
		"cron_expr", "tz", "run_at", "enabled", "input"}, clear: []string{"project_id", "merge_request_id"}},
	"/api/skills/{id}": {op: "updateSkill", fields: []string{"name", "description", "body"}},
	"/api/projects/{id}": {op: "updateProject", fields: []string{"name", "repo_url", "provider", "default_branch", "sandbox_network",
		"description", "instructions", "max_cost_usd_per_day", "max_cost_usd_per_month", "intake"}},
}

// input is the PUT input a recorded state maps to, its keys sorted; ok is false when the
// state is not a record of the resource (null, the audit's {"error": ...}, or a document cut
// to the audit's cap).
func (m revertMapper) input(state json.RawMessage) (json.RawMessage, bool) {
	var obj map[string]json.RawMessage
	if len(state) == 0 || json.Unmarshal(state, &obj) != nil {
		return nil, false
	}
	if _, ok := obj["id"]; !ok {
		return nil, false
	}
	out := make(map[string]json.RawMessage, len(m.fields))
	for _, f := range m.fields {
		if v, ok := obj[f]; ok {
			out[f] = v
		}
	}
	for _, f := range m.clear {
		if _, ok := out[f]; !ok {
			out[f] = json.RawMessage(`""`)
		}
	}
	b, err := json.Marshal(out)
	return b, err == nil
}

// revertPlan is a revert the audit row allows: the resource, the PUT input made from the
// state before the change, and the input made from the state the change left.
type revertPlan struct {
	event  *store.AuditEvent
	mapper revertMapper
	target string
	body   json.RawMessage
	after  json.RawMessage
}

// revertRefusal is why an audit row cannot be reverted.
type revertRefusal struct {
	status        int
	code, message string
}

// planRevert reads the audit row and maps it (rule 5): refused unless it is a PUT that
// succeeded on an agent, a schedule, a skill or a project, with a recorded before and after
// that differ in what a revert writes, and a before without redacted values.
func (s *Server) planRevert(ctx context.Context, id string) (*revertPlan, *revertRefusal, error) {
	if id == "" || len(id) > auditIDMax || hasControl(id) || !utf8.ValidString(id) || s.pool == nil {
		return nil, &revertRefusal{http.StatusNotFound, "not_found", revertAuditNotFound}, nil
	}
	e, err := store.GetAuditEvent(ctx, s.pool, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, &revertRefusal{http.StatusNotFound, "not_found", revertAuditNotFound}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	m, ok := revertMappers[e.Route]
	if e.Method != http.MethodPut || e.Outcome != store.AuditOK || !ok || e.Target == "" {
		msg := revertOnlyPut
		if e.Route == "/api/workflows/{id}" && e.Method == http.MethodPut {
			msg = revertWorkflow
		}
		return nil, &revertRefusal{http.StatusUnprocessableEntity, codeNotRevertible, msg}, nil
	}
	before, ok := m.input(e.Before)
	if !ok {
		return nil, &revertRefusal{http.StatusUnprocessableEntity, codeNothingToRevert, revertNoBefore}, nil
	}
	after, ok := m.input(e.After)
	if !ok {
		return nil, &revertRefusal{http.StatusUnprocessableEntity, codeNotRevertible, revertNoAfter}, nil
	}
	if sameValue(before, after) {
		return nil, &revertRefusal{http.StatusUnprocessableEntity, codeNothingToRevert, revertNothing}, nil
	}
	if holdsRedacted(before) {
		return nil, &revertRefusal{http.StatusUnprocessableEntity, codeNotRevertible, revertRedacted}, nil
	}
	return &revertPlan{event: e, mapper: m, target: e.Target, body: before, after: after}, nil, nil
}

// revertAudit undoes an audited change (rule 5). The resource must still be what the change
// left, compared on what the revert writes (its read-only fields, such as a schedule's next
// run or a project's spend, move on their own): otherwise 412, "changed again since". The
// state before the change then goes through the resource's PUT behind If-Match with the
// version just compared, so a change in between is refused the same way.
func (s *Server) revertAudit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	plan, refusal, err := s.planRevert(ctx, r.PathValue("id"))
	if err != nil {
		s.internalError(w, r, "plan revert", err)
		return
	}
	if refusal != nil {
		writeError(w, refusal.status, refusal.code, refusal.message, nil)
		return
	}
	etag := ""
	var now json.RawMessage
	if status, ct, raw := s.readTarget(ctx, plan.target); status == http.StatusOK && isJSON(ct) {
		etag, now = etagOf(raw), auditJSON(ct, raw, auditStateMax)
	}
	if cur, ok := plan.mapper.input(now); !ok || !sameValue(cur, plan.after) {
		writeError(w, http.StatusPreconditionFailed, codeVersionConflict, revertChangedSince, map[string]string{"current_etag": etag})
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, plan.target, bytes.NewReader(plan.body))
	if err != nil {
		s.internalError(w, r, "build revert", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(tools.HeaderIfMatch, etag)
	res := &memWriter{header: http.Header{}}
	s.versions(s.mux).ServeHTTP(res, req)
	status := res.statusCode()
	if status == http.StatusPreconditionFailed && errorCodeOf(res.body.Bytes()) == codeVersionConflict {
		var e struct {
			Error struct {
				Details json.RawMessage `json:"details"`
			} `json:"error"`
		}
		_ = json.Unmarshal(res.body.Bytes(), &e)
		writeError(w, status, codeVersionConflict, revertChangedSince, e.Error.Details)
		return
	}
	if status < http.StatusBadRequest {
		who, _ := actor.From(ctx)
		s.log.Info("audited change reverted", "audit_id", plan.event.ID, "target", plan.target, "actor", who.Label())
	}
	for k, v := range res.header {
		w.Header()[k] = v
	}
	w.WriteHeader(status)
	_, _ = w.Write(res.body.Bytes())
}

// revertTarget is the audit target of a revert (DESIGN 5.31 rule 2): the resource the audit
// row it reverts changed, so the revert's own row reads that resource before and after, and
// an approval shows it as it is now; "" when the row names nothing a revert changes.
func (s *Server) revertTarget(r *http.Request) string {
	id := contractPathValues(revertRoute, r.URL.EscapedPath())["id"]
	// A request the bearer check refused reads nothing (DESIGN 5.31 rule 2).
	if s.pool == nil || authFrom(r.Context()).refused || id == "" || len(id) > auditIDMax || hasControl(id) || !utf8.ValidString(id) {
		return ""
	}
	e, err := store.GetAuditEvent(r.Context(), s.pool, id)
	if err != nil {
		return ""
	}
	if _, ok := revertMappers[e.Route]; !ok || e.Method != http.MethodPut {
		return ""
	}
	return e.Target
}

// revertRisk is rule A5 for a revert (DESIGN 5.35 rule 5): it is held to the risk of the
// change it writes back, the PUT of its resource with the state before the audited change
// as the body, judged against the resource as it is now. A revert the handler refuses
// raises nothing.
func (s *Server) revertRisk(ctx context.Context, op *tools.PlatformOperation, auditID string, who actor.Actor) (tools.PlatformRisk, []string, error) {
	plan, refusal, err := s.planRevert(ctx, auditID)
	if err != nil {
		return "", nil, err
	}
	if refusal != nil {
		return op.Risk, nil, nil
	}
	put, ok := s.contract.Operation(plan.mapper.op)
	if !ok {
		return op.Risk, nil, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, plan.target, nil)
	if err != nil {
		return op.Risk, nil, nil
	}
	risk, reasons, err := s.platformRisk(ctx, put, req, who, plan.body)
	if err != nil || !risk.Gated() {
		return op.Risk, nil, err
	}
	return risk, reasons, nil
}

// revertApprovalText is what a revert writes back, for its approval's message: "" when it
// is not a revert or cannot be planned.
func (s *Server) revertApprovalText(ctx context.Context, op *tools.PlatformOperation, r *http.Request) string {
	if op.ID != "revertAudit" {
		return ""
	}
	plan, refusal, err := s.planRevert(ctx, contractPathValues(op.Path, r.URL.EscapedPath())["id"])
	if err != nil || refusal != nil {
		return ""
	}
	return "It writes back to " + plan.target + ": " + string(plan.body)
}

// sameValue reports whether two JSON documents hold the same value, numbers compared as
// numbers (a jsonb column writes some as Go does not).
func sameValue(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// holdsRedacted reports a string in a JSON document that carries the redaction marker: the
// value it stands for is not in the audit log.
func holdsRedacted(doc json.RawMessage) bool {
	var v any
	if json.Unmarshal(doc, &v) != nil {
		return false
	}
	var walk func(any) bool
	walk = func(v any) bool {
		switch x := v.(type) {
		case map[string]any:
			for _, val := range x {
				if walk(val) {
					return true
				}
			}
		case []any:
			for _, val := range x {
				if walk(val) {
					return true
				}
			}
		case string:
			return strings.Contains(x, obs.Redacted)
		}
		return false
	}
	return walk(v)
}

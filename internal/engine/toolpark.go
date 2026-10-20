package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"yuno/internal/store"
	"yuno/internal/tools"
)

// A tool call told pending_approval parks the step (DESIGN 5.3 rule 5, D-085). The MCP gate
// answers the model pending_approval when MCP_TOOL_WAIT passes with no decision, and the
// model is told to end its turn without finishing. The step then waits on the approval the
// way it waits on a question's answer (questions.go): the visit is parked-on the approval
// (waitEnded), the turn's end parks it while the approval or its effect is undecided
// (questionTurnEnd), and the settled outcome, once the engine ran the effect or recorded the
// refusal (settleUnwaited), goes to the live visit as its next turn or re-queues the parked
// run, whose continuation visit's task carries it (deliverSettled, parkedToolOutcomes).

// waitEnded tells the waiter's visit that its tool call stopped waiting with no decision:
// the call answers the model pending_approval, so the visit is marked parked-on the
// approval before that answer leaves, as QuestionPending marks a question; the run and the
// node return to running. A visit whose loop stays busy past handoffTimeout gets the mark
// posted instead; a visit that ended needs none.
func (w *approvalWaiter) waitEnded(ctx context.Context, id string) {
	v := w.v
	if v == nil {
		return
	}
	wctx := context.WithoutCancel(ctx)
	mark := func(ctx context.Context) {
		delete(v.toolBlocking, id)
		if _, known := v.parkedTools[id]; !known {
			v.parkedTools[id] = nil
		}
		v.resumeIfClear(ctx)
	}
	hctx, cancel := context.WithTimeout(wctx, handoffTimeout)
	defer cancel()
	if err := v.do(hctx, func() { mark(wctx) }); err != nil && !errors.Is(err, ErrInvalidRunToken) {
		v.post(mark)
	}
}

// toolSettled takes a settled tool approval handed to the live visit through its own loop
// (the 5.24 rule 5 (b) path): the turn's end sends its outcome as the next turn. An
// approval the visit does not wait on (taken at a turn's end already) is dropped.
func (v *visit) toolSettled(a *store.Approval) {
	if _, waits := v.parkedTools[a.ID]; waits {
		v.parkedTools[a.ID] = a
	}
}

// toolOutcome is a gated tool call whose approval was decided: the approval and the call's
// row (nil when the approval has no tool call).
type toolOutcome struct {
	approval *store.Approval
	call     *store.ToolCall
}

// settledTools reads the approvals the visit is parked-on, oldest first: the settled ones
// with their calls, and the id of one that still waits ("" when none does): an undecided
// approval, or an approved call whose effect still runs. A read that fails counts as
// waiting: the visit parks, and the outcome continues it.
func (v *visit) settledTools(ctx context.Context) ([]toolOutcome, string) {
	var out []toolOutcome
	var waiting []string
	for id, a := range v.parkedTools {
		o, settled, err := v.e.toolOutcome(ctx, id, a)
		if err != nil {
			v.log.Warn("read a tool approval failed", "approval_id", id, "error", err)
		}
		if err != nil || !settled {
			waiting = append(waiting, id)
			continue
		}
		out = append(out, o)
	}
	slices.SortFunc(out, func(x, y toolOutcome) int {
		if c := x.approval.RequestedAt.Compare(y.approval.RequestedAt); c != 0 {
			return c
		}
		return strings.Compare(x.approval.ID, y.approval.ID)
	})
	slices.Sort(waiting)
	if len(waiting) > 0 {
		return out, waiting[0]
	}
	return out, ""
}

// toolOutcome reads a tool approval (a is the decided row when the visit has it, else
// nil) and its call, and reports whether it is settled: decided, and when approved, its
// effect executed (the row no longer awaiting_approval).
func (e *Engine) toolOutcome(ctx context.Context, id string, a *store.Approval) (toolOutcome, bool, error) {
	if a == nil || a.Status == store.ApprovalPending {
		cur, err := store.GetApproval(ctx, e.deps.Pool, id)
		if err != nil {
			return toolOutcome{}, false, err
		}
		a = cur
	}
	if a.Status == store.ApprovalPending {
		return toolOutcome{}, false, nil
	}
	o := toolOutcome{approval: a}
	if a.ToolCallID != nil && *a.ToolCallID != "" {
		call, err := store.GetToolCall(ctx, e.deps.Pool, *a.ToolCallID)
		if err != nil {
			return toolOutcome{}, false, err
		}
		if a.Status == store.ApprovalApproved && call.Status == store.ToolAwaitingApproval {
			return toolOutcome{}, false, nil
		}
		o.call = call
	}
	return o, true, nil
}

// block is what the agent is told about a gated call it was answered pending_approval for
// (DESIGN 5.3 rule 5): approved, with the call's result as JSON; rejected, with the
// operator's reason; or expired.
func (o toolOutcome) block() string {
	a := o.approval
	name := "a platform tool"
	if o.call != nil {
		name = tools.PlatformPrefix + tools.PlainName(o.call.ToolName)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Earlier in this step you called %s, which needed a human approval (approval %s). ", name, a.ID)
	switch a.Status {
	case store.ApprovalApproved:
		var out []byte
		if o.call != nil {
			out = compactJSON(o.call.Output)
		}
		switch {
		case o.call != nil && (o.call.IsError || o.call.Status == store.ToolFailed):
			sb.WriteString("It was approved but failed: " + string(out))
		case len(out) == 0 || string(out) == "null":
			sb.WriteString("It was approved and ran.")
		default:
			sb.WriteString("It was approved and ran; its result: " + string(out))
		}
	case store.ApprovalRejected:
		if r := strings.TrimSpace(strValue(a.Reason)); r != "" {
			sb.WriteString(sentence("It was rejected: " + r))
		} else {
			sb.WriteString("It was rejected.")
		}
	default:
		sb.WriteString("It expired: nobody decided in time.")
	}
	return sb.String()
}

func compactJSON(b json.RawMessage) []byte {
	var buf bytes.Buffer
	if json.Compact(&buf, b) != nil {
		return b
	}
	return buf.Bytes()
}

// parkedToolOutcomes is what a continuation visit's task carries about the gated calls of
// the visits it continues (DESIGN 5.3 rule 5): one block per decided tool approval of the
// parked visit, and of the visit that one continued in turn, oldest first. continues is
// the new visit's question_id: empty for a visit that continues nothing.
func (e *Engine) parkedToolOutcomes(ctx context.Context, continues *string) (string, error) {
	var visits []string
	seen := map[string]bool{}
	for id := continues; id != nil && *id != "" && !seen[*id]; {
		seen[*id] = true
		a, err := store.GetApproval(ctx, e.deps.Pool, *id)
		if err != nil {
			return "", err
		}
		visits = append(visits, a.RunNodeID)
		parked, err := store.GetRunNode(ctx, e.deps.Pool, a.RunNodeID)
		if err != nil {
			return "", err
		}
		id = parked.QuestionID
	}
	var blocks []string
	for i := len(visits) - 1; i >= 0; i-- {
		rows, err := store.ListVisitToolApprovals(ctx, e.deps.Pool, visits[i])
		if err != nil {
			return "", err
		}
		for j := range rows {
			o, settled, err := e.toolOutcome(ctx, rows[j].ID, &rows[j])
			if err != nil {
				return "", err
			}
			if settled {
				blocks = append(blocks, o.block())
			}
		}
	}
	return strings.Join(blocks, "\n\n"), nil
}

// deliverSettled hands a settled tool approval (settleUnwaited) to the visit that was told
// pending_approval, in the order of DESIGN 5.24 rule 5: (i) the run parked on it is
// re-queued, once its visit waits on nothing else; (ii) the live visit takes it through its
// own loop, and its turn's end sends it; a visit gone meanwhile is the park landing, so (i)
// once more; (iii) a run left waiting with no worker by a restart is re-queued as before,
// unless the approval expired: the sweep that expired it fails that run.
func (e *Engine) deliverSettled(ctx context.Context, a *store.Approval) {
	wctx := context.WithoutCancel(ctx)
	if e.requeueParked(wctx, a) {
		return
	}
	if v := e.visitOf(a); v != nil {
		if err := e.handoff(wctx, v, func() { v.toolSettled(a) }); err == nil || !errors.Is(err, ErrInvalidRunToken) {
			// Handed over, or the loop stayed busy past the bound: the visit is alive and its
			// turn's end reads the rows; a park that lands later is the sweep's to re-queue.
			return
		}
		e.requeueParked(wctx, a)
		return
	}
	if e.requeueParked(wctx, a) {
		return
	}
	if a.Status != store.ApprovalExpired {
		e.requeueIfUnattended(wctx, a.RunID)
	}
}

// sweepToolApproval expires a tool approval past its expires_at (DESIGN 5.2 rule 10). A
// call that still waits gets the expiry and the run fails with approval expired, as does a
// run none of whose visits waits for it any more. A visit that was told pending_approval
// and waits for the outcome, parked or in its turn, resumes with the expiry instead (5.3
// rule 5): the agent decides what to do without the call.
func (e *Engine) sweepToolApproval(ctx context.Context, a *store.Approval) {
	waits := e.visitOf(a) != nil
	if !waits {
		parked, err := store.IsParkedOn(ctx, e.deps.Pool, a.RunID, a.RunNodeID)
		waits = err == nil && parked
	}
	if _, delivered, ok := e.expire(ctx, a, "approval expired"); ok && (delivered || !waits) {
		e.failRun(ctx, a.RunID, "approval expired")
	}
}

// requeueParkedTools repeats, for the decided tool approvals of parked visits, what a
// decision could not finish: an outcome whose settling was cut short (the row still
// awaiting_approval, as after a restart while the effect ran) is settled again, which
// replays a finished effect by its key; any other is re-queued (requeueParked, a no-op
// while the visit still waits on something).
func (e *Engine) requeueParkedTools(ctx context.Context) {
	parked, err := store.ListParkedToolApprovals(ctx, e.deps.Pool)
	if err != nil {
		e.log.Warn("list parked tool approvals failed", "error", err)
		return
	}
	for i := range parked {
		a := &parked[i]
		if a.ToolCallID != nil && *a.ToolCallID != "" {
			call, err := store.GetToolCall(ctx, e.deps.Pool, *a.ToolCallID)
			if err != nil {
				e.log.Warn("read a parked tool call failed", "approval_id", a.ID, "error", err)
				continue
			}
			if call.Status == store.ToolAwaitingApproval {
				e.settleUnwaited(ctx, a)
				continue
			}
		}
		e.requeueParked(ctx, a)
	}
}

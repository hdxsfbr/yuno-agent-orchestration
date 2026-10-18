package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"yuno/internal/obs"
	"yuno/internal/openclaw"
	"yuno/internal/project"
	"yuno/internal/store"
	"yuno/internal/tools"
)

// Report-back (DESIGN 5.15 rules 9 to 11). A run delegated from a conversation
// (trigger channel, trigger_ref conversation:<id>) tells that conversation how it ended,
// and announces every approval it waits for, by injecting a [platform] message into the
// conversation's OpenClaw session with chat.send {deliver: true}: the message becomes a
// user turn and the agent's reply reaches Telegram through the gateway [probed
// 2026-10-16]. Each report is claimed in the database (runs.reported_at,
// approvals.reported_at) before it is sent, so it is sent at most once and never
// retried after a gateway error: the UI holds the truth. A send that never reached the
// gateway (no connection, the client reconnecting) gives its claim back for the sweep. The claim follows the status change immediately
// rather than inside its transaction, because the transition is the engine's shared
// state-machine step for every run; a periodic sweep reports the terminal runs and
// pending approvals no hook saw (the stale-run sweep, a crash in between).

const (
	// reportOutcomeMax caps the outcome in a run report (DESIGN 5.15 rule 9).
	reportOutcomeMax = 1 << 10
	// reportApprovalMessageMax caps an approval's message in its notice (DESIGN 5.15 rule 10).
	reportApprovalMessageMax = 500
	// reportKeyPrefix starts the idempotency key of every report: report-<run id>-<kind>.
	// The gateway gives the injected user entry the key plus ":user", which is how the
	// conversation mirror links the stored message to its run (DESIGN 5.15 rule 11).
	reportKeyPrefix = "report-"
	// reportSweepLimit bounds one sweep's catch-up of unreported runs.
	reportSweepLimit = 50
)

// reportKey is the chat.send idempotency key of one report.
func reportKey(runID, kind string) string { return reportKeyPrefix + runID + "-" + kind }

// reportRunID returns the run a report's idempotency key (or the ":user" key of the
// entry it injected) names.
func reportRunID(key string) (string, bool) {
	rest, ok := strings.CutPrefix(key, reportKeyPrefix)
	const idLen = 36 // a UUID with dashes (store.NewID)
	if !ok || len(rest) <= idLen || rest[idLen] != '-' {
		return "", false
	}
	id := rest[:idLen]
	for i, c := range id {
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return "", false
			}
		case (c < '0' || c > '9') && (c < 'a' || c > 'f'):
			return "", false
		}
	}
	return id, true
}

// reportsTo reports whether a run reports back to a conversation.
func reportsTo(run *store.Run) bool {
	_, ok := store.ConversationOfRun(run)
	return ok
}

// afterTerminal reports a run that just reached a terminal status, in the background
// so the caller (a worker, an HTTP cancel, the approval sweeper) is not held up by the
// gateway. Runs that do not report back cost nothing.
func (e *Engine) afterTerminal(ctx context.Context, run *store.Run) {
	if run == nil || !reportsTo(run) || !store.IsTerminal(run.Status) {
		return
	}
	id := run.ID
	e.reports.Add(1)
	go func() {
		defer e.reports.Done()
		e.reportRun(context.WithoutCancel(ctx), id)
	}()
}

// afterApproval announces the pending approvals of a run that just started waiting for
// one, in the background.
func (e *Engine) afterApproval(ctx context.Context, run *store.Run) {
	if run == nil || !reportsTo(run) {
		return
	}
	id := run.ID
	e.reports.Add(1)
	go func() {
		defer e.reports.Done()
		e.reportApprovals(context.WithoutCancel(ctx), id)
	}()
}

// runReportKind is the kind of a run's terminal report, the last part of its idempotency
// key: "end" for a run that was never retried, "terminal-r<n>" once the operator has
// retried it n times (DESIGN 5.21 rule 6), so the gateway does not dedupe the new
// outcome's report against the one it sent before.
func runReportKind(run *store.Run) string {
	if run.Retries > 0 {
		return fmt.Sprintf("terminal-r%d", run.Retries)
	}
	return "end"
}

// reportRun claims and sends the report of a terminal channel run (DESIGN 5.15 rule 9).
func (e *Engine) reportRun(ctx context.Context, runID string) {
	run, err := store.ClaimRunReport(ctx, e.deps.Pool, runID, e.clock.Now())
	if errors.Is(err, store.ErrNotFound) {
		return // not a run that reports back, not terminal, or reported already
	}
	if err != nil {
		e.log.Warn("run report failed", "run_id", runID, "error", err)
		return
	}
	convID, _ := store.ConversationOfRun(run)
	if e.sendReport(ctx, run.ID, convID, runReportKind(run), e.runReportText(ctx, run)) {
		// Never sent: the claim is given back so the next sweep sends it.
		if err := store.UnclaimRunReport(ctx, e.deps.Pool, run.ID); err != nil {
			e.log.Warn("give back the run report claim failed", "run_id", run.ID, "error", err)
		}
	}
}

// reportApprovals claims and sends one notice per pending approval of a channel run
// (DESIGN 5.15 rule 10).
func (e *Engine) reportApprovals(ctx context.Context, runID string) {
	claimed, err := store.ClaimApprovalReports(ctx, e.deps.Pool, runID, e.clock.Now())
	if err != nil {
		e.log.Warn("run report failed", "run_id", runID, "kind", "approval", "error", err)
		return
	}
	if len(claimed) == 0 {
		return
	}
	run, err := store.GetRun(ctx, e.deps.Pool, runID)
	if err != nil {
		e.log.Warn("run report failed", "run_id", runID, "kind", "approval", "error", err)
		return
	}
	convID, _ := store.ConversationOfRun(run)
	for i := range claimed {
		a := &claimed[i]
		if e.sendReport(ctx, run.ID, convID, "approval-"+a.ID, e.approvalReportText(run, a)) {
			if err := store.UnclaimApprovalReport(ctx, e.deps.Pool, a.ID); err != nil {
				e.log.Warn("give back the approval notice claim failed", "run_id", run.ID, "approval_id", a.ID, "error", err)
			}
		}
	}
}

// sweepReports reports what the hooks missed: terminal channel runs never reported and
// pending approvals never announced.
func (e *Engine) sweepReports(ctx context.Context) {
	if e.deps.Pool == nil {
		return
	}
	ids, err := store.ListUnreportedChannelRuns(ctx, e.deps.Pool, reportSweepLimit)
	if err != nil {
		e.log.Warn("report sweep failed", "error", err)
		return
	}
	for _, id := range ids {
		e.reportRun(ctx, id)
	}
	ids, err = store.ListRunsWithUnreportedApprovals(ctx, e.deps.Pool)
	if err != nil {
		e.log.Warn("report sweep failed", "error", err)
		return
	}
	for _, id := range ids {
		e.reportApprovals(ctx, id)
	}
}

// sendReport injects a run's report of the given kind into the conversation's session
// under the report key (DESIGN 5.15 rule 9); see sendWithKey.
func (e *Engine) sendReport(ctx context.Context, runID, convID, kind, text string) (deferred bool) {
	return e.sendWithKey(ctx, runID, convID, reportKey(runID, kind), text)
}

// RunReportsTo returns the conversation a run reports back to (DESIGN 5.15 rule 9),
// ok=false for a run that reports to none.
func RunReportsTo(run *store.Run) (conversationID string, ok bool) {
	if run == nil {
		return "", false
	}
	return store.ConversationOfRun(run)
}

// SendPlatformMessage sends text into a conversation's session with deliver, one
// attempt, under the caller's idempotency key, and reports whether the send was
// deferred (the gateway or the database unreachable: the caller tries again later).
// With a run id the log lines are the run's events; without one they go to the api
// log. The budget alerts of DESIGN 5.19 rule 6 and the merge request reports of 5.20
// rule 4 use it.
func (e *Engine) SendPlatformMessage(ctx context.Context, runID, conversationID, key, text string) (deferred bool) {
	return e.sendWithKey(ctx, runID, conversationID, key, text)
}

// sendWithKey injects text into the conversation's session with deliver, one attempt,
// and reports whether the send was deferred: never attempted because the gateway (or
// the database) could not be reached, in which case the caller gives the claim back for
// the next sweep. A conversation that no longer exists (retention) or whose session
// names no agent is skipped; a gateway error on the send is logged at warn and never
// retried (DESIGN 5.15 rule 9: the UI holds the truth).
func (e *Engine) sendWithKey(ctx context.Context, runID, convID, key, text string) (deferred bool) {
	conv, err := store.GetConversation(ctx, e.deps.Pool, convID)
	if errors.Is(err, store.ErrNotFound) {
		e.reportLog(ctx, runID, "info", "run report skipped: the conversation no longer exists", "conversation_id", convID, "key", key)
		return false
	}
	if err != nil {
		e.reportLog(ctx, runID, "warn", "run report deferred: "+err.Error(), "conversation_id", convID, "key", key)
		return true
	}
	// The session key names the agent the gateway requires with it (DESIGN 5.8 rule 3).
	k, ok := openclaw.ParseChannelSessionKey(conv.SessionKey)
	if !ok {
		e.reportLog(ctx, runID, "warn", "run report failed: the conversation's session names no agent", "conversation_id", conv.ID, "key", key)
		return false
	}
	if e.deps.Gateway == nil {
		e.reportLog(ctx, runID, "warn", "run report deferred: no gateway connection", "conversation_id", conv.ID, "key", key)
		return true
	}
	cctx, cancel := context.WithTimeout(ctx, e.sendTimeout)
	defer cancel()
	_, err = openclaw.ChatSend(cctx, e.deps.Gateway, openclaw.ChatSendParams{
		SessionKey: conv.SessionKey, AgentID: k.AgentID, Message: text,
		IdempotencyKey: key, Deliver: true,
	})
	if errors.Is(err, openclaw.ErrDisconnected) {
		// The client refused the call while it reconnects (a gateway restart), or lost
		// the connection after writing it; a resend under the same idempotency key is
		// deduped by the gateway [probed, DESIGN 5.14 rule 6].
		e.reportLog(ctx, runID, "warn", "run report deferred: "+err.Error(), "conversation_id", conv.ID, "key", key)
		return true
	}
	if err != nil {
		e.reportLog(ctx, runID, "warn", "run report failed: "+err.Error(), "conversation_id", conv.ID, "key", key)
		return false
	}
	e.reportLog(ctx, runID, "info", "run reported", "conversation_id", conv.ID, "key", key)
	return false
}

// reportLog writes a report's log line as a run event when the message belongs to a
// run, and to the api log otherwise (a budget alert has no run).
func (e *Engine) reportLog(ctx context.Context, runID, level, msg string, attrs ...any) {
	if runID != "" {
		e.logEvent(ctx, runID, level, msg, attrs...)
		return
	}
	if level == "warn" {
		e.log.Warn(msg, attrs...)
		return
	}
	e.log.Info(msg, attrs...)
}

// runReportText is DESIGN 5.15 rule 9's message: "[platform] Run <id> <status> after <duration>,
// cost $<x>: <outcome>. <url>", the outcome being the last node's output (1 KB at
// most) or the failure reason. After a retry the status is followed by " (retry <n>)"
// (DESIGN 5.21 rule 6) and the duration is that of the last attempt, since a retry
// restarts started_at.
func (e *Engine) runReportText(ctx context.Context, run *store.Run) string {
	start := run.CreatedAt
	if run.StartedAt != nil {
		start = *run.StartedAt
	}
	end := e.clock.Now()
	if run.FinishedAt != nil {
		end = *run.FinishedAt
	}
	outcome := ""
	if run.Status == store.RunSucceeded {
		outcome = e.lastNodeOutput(ctx, run.ID)
		if outcome == "" {
			outcome = "no output"
		}
	} else {
		outcome = strings.TrimSpace(strValue(run.FailureReason))
		if outcome == "" {
			outcome = "no reason given"
		}
	}
	outcome = obs.Truncate(obs.Redact(outcome), reportOutcomeMax)
	status := run.Status
	if run.Retries > 0 {
		status += fmt.Sprintf(" (retry %d)", run.Retries)
	}
	text := fmt.Sprintf("[platform] Run %s %s after %s, cost $%s: %s %s", run.ID, status, reportDuration(end.Sub(start)),
		reportCost(run.CostUSDMicros), sentence(outcome), tools.RunURL(e.deps.Cfg.PublicBaseURL, run.ID))
	if run.MergeRequestID != nil {
		// DESIGN 5.22 rule 9: a run that names a request ends with its link when it pushed
		// onto it, and says nothing of the request otherwise. The run has no record of its
		// own (rule 4), so the opened-request sentence below does not apply.
		if provider, url := e.updatedMergeRequest(ctx, run.ID); url != "" {
			text += " Updated " + strings.ToLower(requestLabel(provider)) + ": " + url
		}
		return text
	}
	// DESIGN 5.17 rule 10: the request the run opened, when it did before this report
	// (its record since 5.20 rule 6).
	if mr, err := store.MergeRequestByRun(ctx, e.deps.Pool, run.ID); err != nil {
		e.log.Warn("read the run's merge request for the report failed", "run_id", run.ID, "error", err)
	} else if mr != nil {
		text += " " + requestLabel(mr.Provider) + ": " + mr.URL
	}
	return text
}

// updatedMergeRequest reads the run's trace for the push of DESIGN 5.22 rule 9: the newest
// executed open_merge_request call whose output has updated: true. It returns the
// provider and the URL of that call's answer ("" when the run pushed nothing, or the
// trace cannot be read, which is logged).
func (e *Engine) updatedMergeRequest(ctx context.Context, runID string) (provider, url string) {
	calls, err := store.ListToolCalls(ctx, e.deps.Pool, runID)
	if err != nil {
		e.log.Warn("read the run's tool calls for the report failed", "run_id", runID, "error", err)
		return "", ""
	}
	for i := len(calls) - 1; i >= 0; i-- {
		c := calls[i]
		if (c.ToolName != "open_merge_request" && c.ToolName != "yuno__open_merge_request") || c.Status != store.ToolExecuted || c.IsError {
			continue
		}
		var out struct {
			URL      string `json:"url"`
			Provider string `json:"provider"`
			Updated  bool   `json:"updated"`
		}
		if json.Unmarshal(c.Output, &out) != nil || !out.Updated || out.URL == "" {
			continue
		}
		if out.Provider == "" && strings.Contains(out.URL, "/pull/") {
			out.Provider = "github" // the answer names it; the URL is the fallback
		}
		return out.Provider, out.URL
	}
	return "", ""
}

// requestLabel is what the provider calls the thing open_merge_request opened.
func requestLabel(provider string) string {
	if provider == "github" {
		return "Pull request"
	}
	return "Merge request"
}

// reportLateMergeRequest tells the conversation about a merge request a late grant
// opened after the run was reported (DESIGN 5.17 rule 10): once, one attempt, under
// the key report-<run id>-effect-<tool call id>; the gateway dedupes a repeat.
func (e *Engine) reportLateMergeRequest(ctx context.Context, runID, toolCallID string, output json.RawMessage) {
	var out struct {
		URL      string `json:"url"`
		Provider string `json:"provider"`
		Updated  bool   `json:"updated"`
	}
	if json.Unmarshal(output, &out) != nil || out.URL == "" {
		return
	}
	run, err := store.GetRun(ctx, e.deps.Pool, runID)
	if err != nil || !reportsTo(run) || run.ReportedAt == nil {
		return // the run's own report, still to come, carries the URL
	}
	convID, _ := store.ConversationOfRun(run)
	// A late push onto the request the run updates (DESIGN 5.22 rule 4) is "updated",
	// a late first request "opened".
	verb := "opened"
	if out.Updated && run.MergeRequestID != nil {
		verb = "updated"
	}
	text := fmt.Sprintf("[platform] Run %s: %s %s after the run ended: %s", run.ID, strings.ToLower(requestLabel(out.Provider)), verb, out.URL)
	e.sendReport(ctx, run.ID, convID, "effect-"+toolCallID, text)
}

// sentence ends s with a full stop unless it already ends a sentence.
func sentence(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, ".") || strings.HasSuffix(s, "?") || strings.HasSuffix(s, "!") {
		return s
	}
	return s + "."
}

// questionKey is the chat.send idempotency key of a question's notice to the Telegram-bound
// agent's conversation under DESIGN 5.24 rule 6 (b): report-<run id>-question-<approval id>,
// the shape reportRunID reads the run from, so the mirrored message links the run.
func questionKey(runID, approvalID string) string { return reportKey(runID, "question-"+approvalID) }

// announceQuestion is the notice of DESIGN 5.24 rule 6, sent when AskOperator has recorded
// a question, in the background like every report.
//
// (a) A run that reports to a conversation announces it through afterApproval: the claim
// on approvals.reported_at, the give-back of a deferred send and the sweep are the
// approval notice's, and approvalReportText writes the question notice in place of the
// waiting line, so the conversation hears it once.
//
// (b) Any other run sends the same notice to the most recent conversation of the agent
// bound to Telegram, through the route of the budget alerts: SendPlatformMessage, one
// attempt per call, under the key question-<approval id> (the gateway dedupes a repeat).
// Without a binding or a conversation it is a log line, and the inbox and the run page are
// the surfaces.
func (e *Engine) announceQuestion(ctx context.Context, run *store.Run, a *store.Approval) {
	if run == nil || a == nil || a.Kind != store.ApprovalKindQuestion {
		return
	}
	if reportsTo(run) {
		e.afterApproval(ctx, run)
		return
	}
	runID, approvalID, text := run.ID, a.ID, e.questionReportText(run, a)
	e.reports.Add(1)
	go func() {
		defer e.reports.Done()
		e.announceToBoundAgent(context.WithoutCancel(ctx), runID, approvalID, text)
	}()
}

// announceToBoundAgent sends the notice of a run that has no conversation to the
// conversation of the agent bound to Telegram (DESIGN 5.24 rule 6 (b)).
func (e *Engine) announceToBoundAgent(ctx context.Context, runID, approvalID, text string) {
	convID, ok, err := project.BoundAgentConversation(ctx, e.deps.Pool)
	if err != nil {
		e.reportLog(ctx, runID, "warn", "question "+approvalID+" could not be announced: "+err.Error())
		return
	}
	if !ok {
		e.reportLog(ctx, runID, "info", "question "+approvalID+" has nowhere to be announced: no conversation and no Telegram binding")
		return
	}
	// A deferred send (the gateway unreachable) is not retried here: the next call for the
	// same question sends it, and the gateway drops a repeat of a key it already took.
	e.SendPlatformMessage(ctx, runID, convID, questionKey(runID, approvalID), text)
}

// questionReportText is DESIGN 5.24 rule 6's notice: "[platform] Run <id> asks:
// <question> Options: <a> | <b>. Answer it at <url>, or tell me the answer and I will pass
// it on (question <id>)." The Options sentence is only there when the question has options;
// the question ends with a full stop unless it already ends a sentence.
func (e *Engine) questionReportText(run *store.Run, a *store.Approval) string {
	question := sentence(obs.Redact(strings.TrimSpace(a.Message)))
	options := ""
	var list []string
	if len(a.Options) > 0 && json.Unmarshal(a.Options, &list) == nil && len(list) > 0 {
		options = " Options: " + obs.Redact(strings.Join(list, " | ")) + "."
	}
	return fmt.Sprintf("[platform] Run %s asks: %s%s Answer it at %s, or tell me the answer and I will pass it on (question %s).",
		run.ID, question, options, tools.RunURL(e.deps.Cfg.PublicBaseURL, run.ID), a.ID)
}

// approvalReportText is DESIGN 5.15 rule 10's message: "[platform] Run <id> is waiting for an
// approval: <kind>, <message>. Decide it at <url>". A question has its own notice (DESIGN
// 5.24 rule 6), which replaces this line.
func (e *Engine) approvalReportText(run *store.Run, a *store.Approval) string {
	if a.Kind == store.ApprovalKindQuestion {
		return e.questionReportText(run, a)
	}
	msg := obs.Truncate(obs.Redact(strings.TrimSpace(a.Message)), reportApprovalMessageMax)
	if msg == "" {
		msg = "no description"
	}
	return fmt.Sprintf("[platform] Run %s is waiting for an approval: %s, %s Decide it at %s", run.ID, a.Kind,
		sentence(msg), tools.RunURL(e.deps.Cfg.PublicBaseURL, run.ID))
}

// lastNodeOutput is the output of the run's last node visit that produced one, as
// compact JSON (a string output as its text); "" when none did.
func (e *Engine) lastNodeOutput(ctx context.Context, runID string) string {
	nodes, err := store.ListRunNodes(ctx, e.deps.Pool, runID)
	if err != nil {
		e.log.Warn("read node outputs for the run report failed", "run_id", runID, "error", err)
		return ""
	}
	for i := len(nodes) - 1; i >= 0; i-- {
		out := nodes[i].Output
		if t := strings.TrimSpace(string(out)); t == "" || t == "null" {
			continue
		}
		var s string
		if json.Unmarshal(out, &s) == nil {
			return s
		}
		var buf bytes.Buffer
		if json.Compact(&buf, out) != nil {
			return string(out)
		}
		return buf.String()
	}
	return ""
}

// reportDuration renders a run's duration for a person: "12 s", "3 min 5 s", "2 h 4 min".
func reportDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	s := int64(d.Round(time.Second) / time.Second)
	switch {
	case s < 60:
		return fmt.Sprintf("%d s", s)
	case s < 3600:
		if s%60 == 0 {
			return fmt.Sprintf("%d min", s/60)
		}
		return fmt.Sprintf("%d min %d s", s/60, s%60)
	default:
		if (s%3600)/60 == 0 {
			return fmt.Sprintf("%d h", s/3600)
		}
		return fmt.Sprintf("%d h %d min", s/3600, (s%3600)/60)
	}
}

// reportCost renders micro-dollars with two decimals, or four for a cost under a cent so
// a cheap run does not read as free.
func reportCost(micros int64) string {
	usd := float64(micros) / 1e6
	if micros > 0 && usd < 0.01 {
		return fmt.Sprintf("%.4f", usd)
	}
	return fmt.Sprintf("%.2f", usd)
}

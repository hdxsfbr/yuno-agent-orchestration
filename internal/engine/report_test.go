package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"yuno/internal/openclaw"
	"yuno/internal/store"
)

// chiefConvKey is the Telegram session the delegating agent chats in.
const chiefConvKey = "agent:agchief:telegram:direct:100000002"

// chiefConversation stores the conversation a delegated run reports back to.
func (env *testEnv) chiefConversation(key string) *store.Conversation {
	env.t.Helper()
	k, _ := openclaw.ParseChannelSessionKey(key)
	conv, err := store.UpsertConversation(context.Background(), env.pool, &store.Conversation{Channel: store.ChannelTelegram,
		ExternalChatID: k.PeerID, SessionKey: key})
	if err != nil {
		env.t.Fatal(err)
	}
	return conv
}

// reportSends are the chat.send calls that carried a report, in order.
func (env *testEnv) reportSends() []openclaw.ChatSendParams {
	var out []openclaw.ChatSendParams
	for _, p := range env.fc.sendParams() {
		if strings.HasPrefix(p.IdempotencyKey, reportKeyPrefix) {
			out = append(out, p)
		}
	}
	return out
}

// finishing answers every node turn with a finish of result; report sends start a turn
// in the conversation and nothing else.
func (env *testEnv) finishing(result string) {
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		if !strings.HasPrefix(p.IdempotencyKey, reportKeyPrefix) {
			go finishThenFinal(env.t, env, p, result)
		}
		return startedTurn(p)
	}
}

func (env *testEnv) reportedAt(table, id string) *time.Time {
	env.t.Helper()
	var at *time.Time
	if err := env.pool.QueryRow(context.Background(), `SELECT reported_at FROM `+table+` WHERE id = $1`, id).Scan(&at); err != nil {
		env.t.Fatal(err)
	}
	return at
}

// DESIGN 5.15 rule 9: a run delegated from a conversation reports how it ended into the
// conversation's session, with deliver, exactly once.
func TestReportBackOncePerTerminalRun(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	env.e.deps.Cfg.PublicBaseURL = "https://yuno.example/"
	conv := env.chiefConversation(chiefConvKey)
	a := env.agent(nil)
	env.finishing(`{"summary":"hello from the platform"}`)
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "say hello", Trigger: store.TriggerChannel,
		TriggerRef: store.ConversationRef(conv.ID)})
	if r := env.wait(done, run.ID); r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %v", r.Status, r.FailureReason)
	}
	env.e.reports.Wait()
	sends := env.reportSends()
	if len(sends) != 1 {
		t.Fatalf("reports = %+v", sends)
	}
	p := sends[0]
	if p.SessionKey != chiefConvKey || p.AgentID != "agchief" || !p.Deliver || p.IdempotencyKey != "report-"+run.ID+"-end" {
		t.Fatalf("report params = %+v", p)
	}
	wantStart := "[platform] Run " + run.ID + " succeeded after "
	if !strings.HasPrefix(p.Message, wantStart) || !regexp.MustCompile(` s, cost \$[0-9]+\.[0-9]{2,4}: `).MatchString(p.Message) ||
		!strings.Contains(p.Message, `{"summary":"hello from the platform"}. https://yuno.example/runs/`+run.ID) ||
		!strings.HasSuffix(p.Message, "/runs/"+run.ID) {
		t.Fatalf("report = %q", p.Message)
	}
	if env.reportedAt("runs", run.ID) == nil {
		t.Fatal("reported_at not set")
	}
	// Every later path finds it reported.
	r, _ := store.GetRun(ctx, env.pool, run.ID)
	env.e.afterTerminal(ctx, r)
	env.e.reportRun(ctx, run.ID)
	env.e.sweepReports(ctx)
	env.e.reports.Wait()
	if n := len(env.reportSends()); n != 1 {
		t.Fatalf("reported %d times", n)
	}
}

// DESIGN 5.15 rule 9: never for runs without a conversation ref; a conversation that is
// gone is skipped; a cancelled run reports its reason.
func TestReportBackSkipsAndCancel(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	a := env.agent(nil)
	env.finishing(`{"summary":"ok"}`)

	manual, done := env.start(TriggerRequest{AgentID: a.ID, Task: "manual"})
	env.wait(done, manual.ID)
	noRef, done := env.start(TriggerRequest{AgentID: a.ID, Task: "no conversation", Trigger: store.TriggerChannel})
	env.wait(done, noRef.ID)
	gone, done := env.start(TriggerRequest{AgentID: a.ID, Task: "conversation gone", Trigger: store.TriggerChannel,
		TriggerRef: store.ConversationRef(store.NewID())})
	env.wait(done, gone.ID)
	env.e.sweepReports(ctx)
	env.e.reports.Wait()
	if sends := env.reportSends(); len(sends) != 0 {
		t.Fatalf("reports = %+v", sends)
	}
	if env.reportedAt("runs", manual.ID) != nil || env.reportedAt("runs", noRef.ID) != nil {
		t.Fatal("a run without a conversation was claimed")
	}
	if env.reportedAt("runs", gone.ID) == nil {
		t.Fatal("the run of a deleted conversation is reported again by every sweep")
	}

	// Cancelled before it started: reported with the reason.
	conv := env.chiefConversation(chiefConvKey)
	queued, err := env.e.Trigger(ctx, TriggerRequest{AgentID: a.ID, Task: "never mind", Trigger: store.TriggerChannel,
		TriggerRef: store.ConversationRef(conv.ID)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.e.Cancel(ctx, queued.ID, "tester"); err != nil {
		t.Fatal(err)
	}
	env.e.reports.Wait()
	sends := env.reportSends()
	if len(sends) != 1 || !strings.HasPrefix(sends[0].Message, "[platform] Run "+queued.ID+" cancelled after ") ||
		!strings.Contains(sends[0].Message, ": cancelled by tester. /runs/"+queued.ID) {
		t.Fatalf("cancel report = %+v", sends)
	}
}

// DESIGN 5.15 rule 9 failure handling: one attempt; a gateway error leaves the run as
// it is and is not retried by the sweep.
func TestReportBackOneAttempt(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	conv := env.chiefConversation(chiefConvKey)
	a := env.agent(nil)
	attempts := 0
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		if strings.HasPrefix(p.IdempotencyKey, reportKeyPrefix) {
			attempts++
			return nil, &openclaw.RPCError{Code: "UNAVAILABLE", Message: "gateway starting; retry shortly", Retryable: true}
		}
		go finishThenFinal(env.t, env, p, `{"summary":"ok"}`)
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "x", Trigger: store.TriggerChannel, TriggerRef: store.ConversationRef(conv.ID)})
	if r := env.wait(done, run.ID); r.Status != store.RunSucceeded {
		t.Fatalf("run = %s", r.Status)
	}
	env.e.reports.Wait()
	env.e.sweepReports(ctx)
	if attempts != 1 {
		t.Fatalf("report attempts = %d", attempts)
	}
	if r, _ := store.GetRun(ctx, env.pool, run.ID); r.Status != store.RunSucceeded || r.ReportedAt == nil {
		t.Fatalf("run after a failed report = %+v", r)
	}
}

// DESIGN 5.15 rule 10: a delegated run that starts waiting for an approval announces it
// once, with the kind, the message and where to decide it.
func TestApprovalNoticeOnce(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	conv := env.chiefConversation(chiefConvKey)
	wf := gated(t, env, false, store.Limits{})
	env.script(map[string][]string{"ship": {`{"shipped":true}`}})
	inner := env.fc.onSend
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		if strings.HasPrefix(p.IdempotencyKey, reportKeyPrefix) {
			return startedTurn(p)
		}
		return inner(p, n)
	}
	r := env.triggerAndRun(TriggerRequest{WorkflowID: wf.ID, Input: json.RawMessage(`{"what":"v2"}`), Trigger: store.TriggerChannel,
		TriggerRef: store.ConversationRef(conv.ID)})
	if r.Status != store.RunWaitingApproval {
		t.Fatalf("run = %s", r.Status)
	}
	env.e.reports.Wait()
	appr := pendingApproval(t, env, r.ID)
	sends := env.reportSends()
	want := "[platform] Run " + r.ID + " is waiting for an approval: node, Ship v2? Decide it at /runs/" + r.ID
	if len(sends) != 1 || sends[0].Message != want || sends[0].IdempotencyKey != "report-"+r.ID+"-approval-"+appr.ID || !sends[0].Deliver {
		t.Fatalf("notices = %+v, want %q", sends, want)
	}
	if env.reportedAt("approvals", appr.ID) == nil {
		t.Fatal("approvals.reported_at not set")
	}
	env.e.reportApprovals(ctx, r.ID)
	env.e.sweepReports(ctx)
	env.e.reports.Wait()
	if n := len(env.reportSends()); n != 1 {
		t.Fatalf("announced %d times", n)
	}
	// A manual run's approval is never announced.
	manual := env.triggerAndRun(TriggerRequest{WorkflowID: wf.ID, Input: json.RawMessage(`{"what":"v3"}`)})
	env.e.reports.Wait()
	env.e.sweepReports(ctx)
	if n := len(env.reportSends()); n != 1 || env.reportedAt("approvals", pendingApproval(t, env, manual.ID).ID) != nil {
		t.Fatalf("manual run announced: %d", n)
	}
}

// DESIGN 5.15 rule 11: the injected report is mirrored as an inbound message linked to
// its run; the person's own messages are not.
func TestMirrorLinksReportToRun(t *testing.T) {
	c := newConvEnv(t, true)
	c.fc.push(openclaw.EventSessionsChanged, changed(convKey, "agconv"))
	eventually(t, "subscribed", func() bool { return len(c.fc.callsFor("sessions.messages.subscribe")) == 1 })
	conv := c.conversation()
	wf := &store.Workflow{Name: "delegated", Definition: json.RawMessage(`{"nodes":[],"edges":[]}`)}
	if err := store.CreateWorkflow(context.Background(), c.pool, wf); err != nil {
		t.Fatal(err)
	}
	run := &store.Run{WorkflowID: wf.ID, WorkflowVersion: 1, WorkflowSnapshot: wf.Definition, Trigger: store.TriggerChannel}
	if err := store.CreateRun(context.Background(), c.pool, run); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	report := userEntry("m-report", "[platform] Run "+run.ID+" succeeded after 3 s, cost $0.00: done.", now)
	report["idempotencyKey"] = reportKey(run.ID, "end") + ":user"
	stale := userEntry("m-stale", "[platform] Run gone", now+1)
	stale["idempotencyKey"] = reportKey("0199f0a2-0000-7000-8000-00000000abcd", "end") + ":user"
	c.fc.push("session.message", map[string]any{"sessionKey": convKey, "agentId": "agconv", "messageId": "m-report", "message": report})
	c.fc.push("session.message", map[string]any{"sessionKey": convKey, "agentId": "agconv", "messageId": "m-stale", "message": stale})
	c.fc.push("session.message", map[string]any{"sessionKey": convKey, "agentId": "agconv", "messageId": "m-person",
		"message": userEntry("m-person", "thanks", now+2)})
	eventually(t, "three messages", func() bool { return len(c.messages(conv.ID)) == 3 })
	for _, m := range c.messages(conv.ID) {
		switch m.ExternalMessageID {
		case "m-report":
			if m.Direction != store.DirectionIn || m.RunID == nil || *m.RunID != run.ID {
				t.Fatalf("report = %+v", m)
			}
		default:
			if m.RunID != nil {
				t.Fatalf("%s linked to a run: %+v", m.ExternalMessageID, m)
			}
		}
	}
}

func TestReportRunID(t *testing.T) {
	id := "0199f0a2-1234-7abc-8def-0123456789ab"
	for key, want := range map[string]string{
		"report-" + id + "-end":                           id,
		"report-" + id + "-end:user":                      id,
		"report-" + id + "-approval-" + id + ":u":         id,
		"report-" + id:                                    "",
		"report-0199F0A2-1234-7abc-8def-0123456789ab-end": "",
		"yuno-" + id + "-end":                             "",
		"":                                                "",
	} {
		got, ok := reportRunID(key)
		if got != want || ok != (want != "") {
			t.Errorf("reportRunID(%q) = %q %v, want %q", key, got, ok, want)
		}
	}
	for d, want := range map[time.Duration]string{0: "0 s", 12 * time.Second: "12 s", 185 * time.Second: "3 min 5 s",
		2 * time.Minute: "2 min", 2*time.Hour + 4*time.Minute + 9*time.Second: "2 h 4 min", time.Hour: "1 h"} {
		if got := reportDuration(d); got != want {
			t.Errorf("reportDuration(%v) = %q, want %q", d, got, want)
		}
	}
	for micros, want := range map[int64]string{0: "0.00", 1234: "0.0012", 12500: "0.01", 2_340_000: "2.34"} {
		if got := reportCost(micros); got != want {
			t.Errorf("reportCost(%d) = %q, want %q", micros, got, want)
		}
	}
}

// A report the gateway never received (the client reconnecting, or no gateway at all)
// gives its claim back, and the next sweep sends it; a real gateway error stays one
// attempt (TestReportBackOneAttempt).
func TestReportDeferredWhileDisconnected(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	conv := env.chiefConversation(chiefConvKey)
	a := env.agent(nil)
	var down atomic.Bool
	down.Store(true)
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		if strings.HasPrefix(p.IdempotencyKey, reportKeyPrefix) && down.Load() {
			return nil, fmt.Errorf("fake: chat.send: %w", openclaw.ErrDisconnected)
		}
		if !strings.HasPrefix(p.IdempotencyKey, reportKeyPrefix) {
			go finishThenFinal(env.t, env, p, `{"summary":"ok"}`)
		}
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "x", Trigger: store.TriggerChannel, TriggerRef: store.ConversationRef(conv.ID)})
	if r := env.wait(done, run.ID); r.Status != store.RunSucceeded {
		t.Fatalf("run = %s", r.Status)
	}
	env.e.reports.Wait()
	if env.reportedAt("runs", run.ID) != nil {
		t.Fatal("a report the gateway never received kept its claim")
	}

	// No gateway at all: deferred the same way.
	gw := env.e.deps.Gateway
	env.e.deps.Gateway = nil
	env.e.sweepReports(ctx)
	if env.reportedAt("runs", run.ID) != nil {
		t.Fatal("a report sent without a gateway kept its claim")
	}
	env.e.deps.Gateway = gw

	// Reconnected: the sweep sends it, once.
	down.Store(false)
	env.e.sweepReports(ctx)
	env.e.sweepReports(ctx)
	var sent []openclaw.ChatSendParams
	for _, p := range env.reportSends() {
		if p.IdempotencyKey == "report-"+run.ID+"-end" {
			sent = append(sent, p)
		}
	}
	// Two refused attempts went through chat.send (the fake records them); one went out.
	if env.reportedAt("runs", run.ID) == nil || len(sent) != 2 {
		t.Fatalf("after reconnecting: reported_at %v, sends %d", env.reportedAt("runs", run.ID), len(sent))
	}
}

// The same for an approval notice.
func TestApprovalNoticeDeferredWhileDisconnected(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	conv := env.chiefConversation(chiefConvKey)
	wf := gated(t, env, false, store.Limits{})
	env.script(map[string][]string{"ship": {`{"shipped":true}`}})
	inner := env.fc.onSend
	var down atomic.Bool
	down.Store(true)
	notices := 0
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		if strings.HasPrefix(p.IdempotencyKey, reportKeyPrefix) {
			if down.Load() {
				return nil, fmt.Errorf("fake: chat.send: %w", openclaw.ErrDisconnected)
			}
			notices++
			return startedTurn(p)
		}
		return inner(p, n)
	}
	r := env.triggerAndRun(TriggerRequest{WorkflowID: wf.ID, Input: json.RawMessage(`{"what":"v2"}`), Trigger: store.TriggerChannel,
		TriggerRef: store.ConversationRef(conv.ID)})
	env.e.reports.Wait()
	appr := pendingApproval(t, env, r.ID)
	if env.reportedAt("approvals", appr.ID) != nil {
		t.Fatal("an undelivered notice kept its claim")
	}
	down.Store(false)
	env.e.sweepReports(ctx)
	env.e.sweepReports(ctx)
	if env.reportedAt("approvals", appr.ID) == nil || notices != 1 {
		t.Fatalf("after reconnecting: notices %d", notices)
	}
}

// A finish result is stored redacted: an agent that echoes a credential or its chat
// token does not put it into the run's state, the trace or GET /api/runs/{id}.
func TestFinishResultRedacted(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	const token = "c_abcdefghijklmnopqrstuvwxyz234567"
	a := env.agent(nil)
	env.finishing(`{"summary":"done; my chat token is ` + token + `","chat_token":"` + token + `"}`)
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "x"})
	if r := env.wait(done, run.ID); r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %v", r.Status, r.FailureReason)
	}
	r, err := store.GetRun(ctx, env.pool, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := store.ListRunNodes(ctx, env.pool, run.ID)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes = %+v %v", nodes, err)
	}
	for what, raw := range map[string]json.RawMessage{"state": r.State, "node output": nodes[0].Output} {
		var buf bytes.Buffer
		if err := json.Compact(&buf, raw); err != nil {
			t.Fatal(err)
		}
		if got := buf.String(); strings.Contains(got, token) || !strings.Contains(got, `"chat_token":"[redacted]"`) ||
			!strings.Contains(got, "my chat token is [redacted]") {
			t.Errorf("%s = %s", what, got)
		}
	}
}

// mrEffects is a SideEffectExecutor whose approved call opened a merge request.
type mrEffects struct{ countingEffects }

func (m *mrEffects) ExecuteApproved(ctx context.Context, toolCallID string) (json.RawMessage, bool, error) {
	_, _, _ = m.countingEffects.ExecuteApproved(ctx, toolCallID)
	return json.RawMessage(`{"number":7,"url":"https://gitlab.test/g/p/-/merge_requests/7","branch":"yuno/x","provider":"gitlab"}`), false, nil
}

// T5.17.7, DESIGN 5.17 rule 10: a run that opened a merge request before it ended
// reports its URL.
func TestReportBackCarriesMergeRequest(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	conv := env.chiefConversation(chiefConvKey)
	a := env.agent(nil)
	env.finishing(`{"summary":"proposed"}`)
	run, err := env.e.Trigger(ctx, TriggerRequest{AgentID: a.ID, Task: "propose", Trigger: store.TriggerChannel,
		TriggerRef: store.ConversationRef(conv.ID)})
	if err != nil {
		t.Fatal(err)
	}
	// What open_merge_request records once the Deployer's call executed (DESIGN 5.20
	// rule 2): the run's merge request record, on a project of its own.
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	proj := &store.Project{Name: "report-" + store.NewID()[:8], RepoURL: "https://gitlab.test/g/p.git", Provider: "gitlab", DefaultBranch: "main"}
	must(store.CreateProject(ctx, env.pool, proj, nil))
	_, err = store.InsertMergeRequest(ctx, env.pool, &store.MergeRequest{ProjectID: proj.ID, RunID: run.ID, Provider: "gitlab", Number: 5,
		URL: "https://gitlab.test/g/p/-/merge_requests/5", SourceBranch: "yuno/abc123", TargetBranch: "main", Title: "Add it"})
	must(err)
	if r := env.resume(env.e, run.ID); r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %v", r.Status, r.FailureReason)
	}
	env.e.reports.Wait()
	sends := env.reportSends()
	if len(sends) != 1 || !strings.HasSuffix(sends[0].Message, "/runs/"+run.ID+" Merge request: https://gitlab.test/g/p/-/merge_requests/5") {
		t.Fatalf("reports = %+v", sends)
	}
}

// T5.17.7, DESIGN 5.17 rule 10: a merge request opened by a late grant after the run
// was reported gets one follow-up into the conversation, under its own key.
func TestLateMergeRequestFollowUp(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()
	conv := env.chiefConversation(chiefConvKey)
	eff := &mrEffects{}
	env.e.deps.Effects = eff
	a := env.agent(nil)
	var mu sync.Mutex
	var g *gate
	env.fc.onSend = func(p openclaw.ChatSendParams, n int) (any, error) {
		if strings.HasPrefix(p.IdempotencyKey, reportKeyPrefix) {
			return startedTurn(p)
		}
		go func() {
			wctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
			got := env.gatedCall(p, wctx)
			cancel()
			<-got.waitErr
			mu.Lock()
			g = got
			mu.Unlock()
			finishThenFinal(t, env, p, `{"summary":"merge request pending approval"}`)
		}()
		return startedTurn(p)
	}
	run, done := env.start(TriggerRequest{AgentID: a.ID, Task: "propose", Trigger: store.TriggerChannel,
		TriggerRef: store.ConversationRef(conv.ID)})
	if r := env.wait(done, run.ID); r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %v", r.Status, r.FailureReason)
	}
	env.e.reports.Wait()
	// The approval notice and the end report went out; neither carries a request.
	effects := func() []openclaw.ChatSendParams {
		var out []openclaw.ChatSendParams
		for _, p := range env.reportSends() {
			if strings.Contains(p.IdempotencyKey, "-effect-") {
				out = append(out, p)
			}
		}
		return out
	}
	if sends := env.reportSends(); len(sends) != 2 || len(effects()) != 0 || strings.Contains(sends[1].Message, "merge_requests") {
		t.Fatalf("reports before the grant = %+v", sends)
	}
	mu.Lock()
	gated := g
	mu.Unlock()
	// The gate recorded a deploy_release row; the Deployer's call was open_merge_request.
	if _, err := env.pool.Exec(ctx, `UPDATE tool_calls SET tool_name='open_merge_request' WHERE id=$1`, gated.call.ID); err != nil {
		t.Fatal(err)
	}
	appr := recv(t, gated.approval, "approval")
	if _, err := env.e.DecideApproval(ctx, appr.ID, DecisionApprove, "andre", ""); err != nil {
		t.Fatal(err)
	}
	env.e.bg.Wait()
	sends := effects()
	if len(sends) != 1 {
		t.Fatalf("follow-ups after the grant = %+v", sends)
	}
	follow := sends[0]
	if follow.IdempotencyKey != "report-"+run.ID+"-effect-"+gated.call.ID || !follow.Deliver || follow.SessionKey != chiefConvKey ||
		follow.Message != "[platform] Run "+run.ID+": merge request opened after the run ended: https://gitlab.test/g/p/-/merge_requests/7" {
		t.Fatalf("follow-up = %+v", follow)
	}
}

// openMergeRequestCall stores a tool call row of the run's trace: open_merge_request with
// the given status, error flag and output, started at offset seconds after the run.
func (f *iterFixture) openMergeRequestCall(run *store.Run, tool, status string, isError bool, output string, offset int) {
	f.env.t.Helper()
	at := run.CreatedAt.Add(time.Duration(offset) * time.Second)
	call := &store.ToolCall{RunID: run.ID, ToolUseID: "call-" + store.NewID(), ToolName: tool, Status: status, IsError: isError,
		Output: json.RawMessage(output), StartedAt: &at, FinishedAt: &at}
	if err := store.InsertToolCall(context.Background(), f.env.pool, call); err != nil {
		f.env.t.Fatal(err)
	}
}

// T5.22.10, DESIGN 5.22 rule 9: the report of a run that names a request ends with
// " Updated merge request: <url>" (" Updated pull request:" on GitHub) when an executed
// open_merge_request call of its trace answered updated: true, the newest such call's
// URL; with nothing about the request otherwise. A run that names none keeps the text of
// 5.17 rule 10.
func TestReportBackTextOfAnIteratingRun(t *testing.T) {
	f := newIterFixture(t)
	env, ctx := f.env, context.Background()
	env.e.deps.Cfg.PublicBaseURL = "https://yuno.example/"
	const url = "https://gitlab.test/g/p/-/merge_requests/4"
	updated := `{"number":4,"url":"` + url + `","branch":"` + iterBranch + `","provider":"gitlab","replayed":true,"updated":true}`
	replayed := `{"number":4,"url":"` + url + `","branch":"` + iterBranch + `","provider":"gitlab","replayed":true}`

	text := func(run *store.Run) string {
		t.Helper()
		r := env.run(run.ID)
		r.Status = store.RunSucceeded // the text of a finished run; the fixture's run never ran
		return env.e.runReportText(ctx, r)
	}
	// Each case is a run of its own on the request, queued after the earlier one ended.
	fresh := func() *store.Run {
		t.Helper()
		run, err := env.e.Trigger(ctx, f.request())
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	end := func(run *store.Run) { f.setRunStatus(run.ID, store.RunSucceeded) }
	runURL := func(run *store.Run) string { return "https://yuno.example/runs/" + run.ID }
	noRequestText := func(what, got string, run *store.Run) {
		t.Helper()
		if !strings.HasSuffix(got, " "+runURL(run)) {
			t.Errorf("%s: the report does not end with the run's link: %q", what, got)
		}
		if strings.Contains(got, "Updated") || strings.Contains(got, "Merge request:") || strings.Contains(got, "merge_requests") {
			t.Errorf("%s: the report mentions the request: %q", what, got)
		}
	}

	// No call at all: nothing about the request.
	run := fresh()
	noRequestText("no call", text(run), run)
	// Only a replay (an unchanged tree), a failed call, one not executed: nothing.
	f.openMergeRequestCall(run, "open_merge_request", store.ToolExecuted, false, replayed, 1)
	f.openMergeRequestCall(run, "open_merge_request", store.ToolExecuted, true, updated, 2)
	f.openMergeRequestCall(run, "open_merge_request", store.ToolAwaitingApproval, false, updated, 3)
	f.openMergeRequestCall(run, "open_merge_request", store.ToolRejected, false, updated, 4)
	f.openMergeRequestCall(run, "deploy_release", store.ToolExecuted, false, updated, 5)
	noRequestText("replayed, failed, gated and other calls", text(run), run)

	// An executed call that updated: the sentence, with the URL of the call's answer.
	f.openMergeRequestCall(run, "open_merge_request", store.ToolExecuted, false, updated, 6)
	if got, want := text(run), " "+runURL(run)+" Updated merge request: "+url; !strings.HasSuffix(got, want) {
		t.Errorf("after a push: %q does not end with %q", got, want)
	}
	// A later replay does not take it back: the run did push.
	f.openMergeRequestCall(run, "open_merge_request", store.ToolExecuted, false, replayed, 7)
	if got, want := text(run), " "+runURL(run)+" Updated merge request: "+url; !strings.HasSuffix(got, want) {
		t.Errorf("push then replay: %q does not end with %q", got, want)
	}
	// The newest push wins, whichever name the gateway recorded the tool under.
	f.openMergeRequestCall(run, "yuno__open_merge_request", store.ToolExecuted, false,
		strings.ReplaceAll(updated, "/merge_requests/4", "/merge_requests/5"), 8)
	if got, want := text(run), " Updated merge request: https://gitlab.test/g/p/-/merge_requests/5"; !strings.HasSuffix(got, want) {
		t.Errorf("two pushes: %q does not end with %q", got, want)
	}
	end(run)

	// GitHub's name for it.
	run = fresh()
	f.openMergeRequestCall(run, "open_merge_request", store.ToolExecuted, false,
		`{"number":4,"url":"https://github.test/g/p/pull/4","provider":"github","replayed":true,"updated":true}`, 1)
	if got, want := text(run), " Updated pull request: https://github.test/g/p/pull/4"; !strings.HasSuffix(got, want) {
		t.Errorf("github: %q does not end with %q", got, want)
	}
	end(run)

	// A run that names none keeps today's text: its own record's sentence, and no
	// "Updated" even when its trace holds a call that updated (the same-run update path).
	opener := env.run(f.opener.ID)
	opener.Status = store.RunSucceeded
	f.openMergeRequestCall(opener, "open_merge_request", store.ToolExecuted, false, updated, 1)
	if got, want := env.e.runReportText(ctx, opener), " "+runURL(opener)+" Merge request: "+f.rec.URL; !strings.HasSuffix(got, want) || strings.Contains(got, "Updated") {
		t.Errorf("run without a named request: %q, want it to end with %q", got, want)
	}
}

// T5.22.10, DESIGN 5.22 rule 9 end to end: a delegated run that iterated on a request
// reports into its conversation once, ending with the Updated line.
func TestReportBackSendsTheUpdatedMergeRequest(t *testing.T) {
	f := newIterFixture(t)
	env, ctx := f.env, context.Background()
	conv := env.chiefConversation(chiefConvKey)
	env.finishing(`{"summary":"fixed the threads"}`)
	const url = "https://gitlab.test/g/p/-/merge_requests/4"
	run, err := env.e.Trigger(ctx, TriggerRequest{AgentID: f.agent.ID, Task: "fix the review comments", ProjectID: f.proj.ID,
		MergeRequestID: f.rec.ID, Trigger: store.TriggerChannel, TriggerRef: store.ConversationRef(conv.ID)})
	if err != nil {
		t.Fatal(err)
	}
	f.openMergeRequestCall(run, "open_merge_request", store.ToolExecuted, false,
		`{"number":4,"url":"`+url+`","provider":"gitlab","replayed":true,"updated":true}`, 1)
	if r := env.resume(env.e, run.ID); r.Status != store.RunSucceeded {
		t.Fatalf("run = %s %q", r.Status, deref(r.FailureReason))
	}
	env.e.reports.Wait()
	sends := env.reportSends()
	if len(sends) != 1 || !strings.HasSuffix(sends[0].Message, "/runs/"+run.ID+" Updated merge request: "+url) ||
		sends[0].IdempotencyKey != "report-"+run.ID+"-end" {
		t.Fatalf("reports = %+v", sends)
	}
}

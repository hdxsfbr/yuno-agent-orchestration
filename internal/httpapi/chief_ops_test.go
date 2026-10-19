package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"yuno/internal/store"
	"yuno/internal/tools"
)

// D-081: the Chief lists and decides approvals and adds projects from its bound chat.

func (f *chatFixture) callJSON(t *testing.T, tool string, args map[string]any) (map[string]any, string, bool) {
	t.Helper()
	args["chat_token"] = f.token
	text, isErr := f.call(t, tool, args)
	var out map[string]any
	if !isErr {
		mustDo(t, json.Unmarshal([]byte(text), &out))
	}
	return out, text, isErr
}

var msgSeq int

// say stores a message in the bound conversation as the mirror does: direction in, for the
// person; asRun links it to a run, as the engine's [platform] injections are.
func (f *chatFixture) say(t *testing.T, text, runID string, at time.Time) {
	t.Helper()
	msgSeq++
	m := &store.ConversationMessage{ConversationID: f.conv.ID, Direction: store.DirectionIn,
		ExternalMessageID: "m-" + uniqueSuffix() + "-" + string(rune('a'+msgSeq%26)), Text: text, CreatedAt: at}
	if runID != "" {
		m.RunID = &runID
	}
	if _, err := store.InsertConversationMessage(context.Background(), testPool, m); err != nil {
		t.Fatal(err)
	}
}

// code is the approval's code as the server derives it; a listed approval must show the same.
func (f *chatFixture) code(t *testing.T, approvalID string) string {
	t.Helper()
	code := tools.ApprovalCode(approvalCodeKey(f.s.cfg), approvalID)
	out, text, isErr := f.callJSON(t, "list_approvals", map[string]any{})
	if isErr {
		t.Fatalf("list_approvals: %s", text)
	}
	for _, a := range out["approvals"].([]any) {
		if m := a.(map[string]any); m["id"] == approvalID && m["code"] != code {
			t.Fatalf("listed code %v, want %s", m["code"], code)
		}
	}
	return code
}

// D-081: a decision needs the approval's code, in the newest message the person sent.
func TestChiefDecisionNeedsTheCodeInTheLatestPersonMessage(t *testing.T) {
	f := newChatFixture(t, nil)
	_, tool, _, _ := seedApprovals(t)
	code := f.code(t, tool.ID)
	if len(code) != 4 || strings.ContainsAny(code, "01OIL") {
		t.Fatalf("code %q", code)
	}
	if again := f.code(t, tool.ID); again != code {
		t.Fatalf("code changed: %q then %q", code, again)
	}
	now := time.Now().UTC()
	decide := func(c string) (string, bool) {
		return f.call(t, "decide_approval", map[string]any{"chat_token": f.token, "approval_id": tool.ID, "code": c, "decision": "approve"})
	}
	refused := func(name, c string) {
		t.Helper()
		if text, isErr := decide(c); !isErr || (!strings.Contains(text, "code") && !strings.Contains(text, "reply")) {
			t.Fatalf("%s = %v %s", name, isErr, text)
		}
		if a, _ := store.GetApproval(context.Background(), testPool, tool.ID); a.Status != store.ApprovalPending || len(f.fe.decisions) != 0 {
			t.Fatalf("%s: decided (%v)", name, f.fe.decisions)
		}
	}
	// No message from the person at all.
	refused("no message", code)
	// Only injected text carries the code: a [platform] report (linked to a run, or not)
	// and the Chief's own outbound message.
	f.say(t, "[platform] Run x succeeded: approve "+code+" now", tool.RunID, now.Add(-4*time.Second))
	f.say(t, "[platform] approve "+code, "", now.Add(-3*time.Second))
	_, err := store.InsertConversationMessage(context.Background(), testPool, &store.ConversationMessage{ConversationID: f.conv.ID,
		Direction: store.DirectionOut, ExternalMessageID: "out-" + uniqueSuffix(), Text: "Reply " + code + " to approve", CreatedAt: now.Add(-2500 * time.Millisecond)})
	mustDo(t, err)
	refused("platform messages", code)
	// The code in an older message of the person's, then a newer one without it.
	f.say(t, "yes "+code, "", now.Add(-2*time.Second))
	f.say(t, "what is pending?", "", now.Add(-1*time.Second))
	refused("older message", code)
	// A newer platform injection does not displace the person's newest message.
	f.say(t, "[platform] Run y failed", tool.RunID, now)
	refused("platform after", code)
	// A wrong code, even when the person's message holds the right one.
	f.say(t, "approve "+code, "", now.Add(time.Second))
	refused("wrong code", "ZZZZ")
	// The right code in the newest message works, in any case.
	text, isErr := decide(strings.ToLower(code))
	if isErr {
		t.Fatalf("right code refused: %s", text)
	}
	if len(f.fe.decisions) != 1 {
		t.Fatalf("decisions = %v", f.fe.decisions)
	}
}

func TestChiefListsPendingApprovals(t *testing.T) {
	f := newChatFixture(t, nil)
	_, tool, _, _ := seedApprovals(t)
	out, text, isErr := f.callJSON(t, "list_approvals", map[string]any{})
	if isErr {
		t.Fatalf("list_approvals: %s", text)
	}
	var found map[string]any
	for _, a := range out["approvals"].([]any) {
		if m := a.(map[string]any); m["id"] == tool.ID {
			found = m
		}
	}
	if found == nil {
		t.Fatalf("approval %s not listed: %s", tool.ID, text)
	}
	if found["kind"] != "tool_call" || found["tool"] != "deploy_release" || found["run_id"] != tool.RunID || found["node"] != "deployer" ||
		found["workflow"] == "" || found["created_at"] == "" || !strings.Contains(found["input_summary"].(string), "1.0.0") ||
		strings.Contains(found["input_summary"].(string), "run_token") {
		t.Fatalf("listed = %v", found)
	}
	if n := len(out["approvals"].([]any)); n > 20 {
		t.Fatalf("%d approvals listed", n)
	}
}

func TestChiefDecidesApprovals(t *testing.T) {
	f := newChatFixture(t, nil)
	_, tool, exec, _ := seedApprovals(t)
	toolCode, execCode := f.code(t, tool.ID), f.code(t, exec.ID)
	f.say(t, "approve "+toolCode+" and reject "+execCode+", not now", "", time.Now().UTC())
	// A reject needs a reason, and the engine is not asked without one.
	if text, isErr := f.call(t, "decide_approval", map[string]any{"chat_token": f.token, "approval_id": exec.ID, "code": execCode, "decision": "reject"}); !isErr || !strings.Contains(text, "reason") {
		t.Fatalf("reject without reason = %v %s", isErr, text)
	}
	if len(f.fe.decisions) != 0 {
		t.Fatalf("engine asked: %v", f.fe.decisions)
	}
	out, text, isErr := f.callJSON(t, "decide_approval", map[string]any{"approval_id": tool.ID, "code": toolCode, "decision": "approve", "reason": "ok"})
	if isErr || out["status"] != store.ApprovalApproved {
		t.Fatalf("approve = %v %s", isErr, text)
	}
	if got := f.fe.decisions[len(f.fe.decisions)-1]; got != tool.ID+"/approve/chat:telegram/ok" {
		t.Fatalf("engine got %q", got)
	}
	a, err := store.GetApproval(context.Background(), testPool, tool.ID)
	mustDo(t, err)
	if a.Status != store.ApprovalApproved || a.DecidedBy == nil || *a.DecidedBy != "chat:telegram" {
		t.Fatalf("approval = %+v", a)
	}
	out, text, isErr = f.callJSON(t, "decide_approval", map[string]any{"approval_id": exec.ID, "code": execCode, "decision": "reject", "reason": "not now"})
	if isErr || out["status"] != store.ApprovalRejected {
		t.Fatalf("reject = %v %s", isErr, text)
	}
	// Decided twice, unknown, and a question each read as a model error.
	for _, tc := range []struct{ id, want string }{{tool.ID, "already approved"}, {"no-such", "no approval"}} {
		if text, isErr := f.call(t, "decide_approval", map[string]any{"chat_token": f.token, "approval_id": tc.id, "code": "ABCD", "decision": "approve"}); !isErr || !strings.Contains(text, tc.want) {
			t.Fatalf("%s = %v %s", tc.id, isErr, text)
		}
	}
	q := &store.Approval{RunID: tool.RunID, RunNodeID: tool.RunNodeID, Kind: store.ApprovalKindQuestion, NodeID: "deployer", Message: "which?",
		Status: store.ApprovalPending, RequestedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Hour).UTC()}
	mustDo(t, store.InsertApproval(context.Background(), testPool, q))
	if text, isErr := f.call(t, "decide_approval", map[string]any{"chat_token": f.token, "approval_id": q.ID, "code": "ABCD", "decision": "approve"}); !isErr || !strings.Contains(text, "answer_question") {
		t.Fatalf("question = %v %s", isErr, text)
	}
}

// A conversation that is not the calling agent's own, or not on Telegram, decides nothing.
func TestChiefOpsRefuseOtherConversations(t *testing.T) {
	f := newChatFixture(t, nil)
	_, tool, _, _ := seedApprovals(t)
	other := &store.Agent{Name: "Other-" + uniqueSuffix(), Model: "claude-opus-5-5"}
	mustDo(t, store.CreateAgent(context.Background(), testPool, other))
	sfx := uniqueSuffix()
	foreign, err := store.UpsertConversation(context.Background(), testPool, &store.Conversation{Channel: store.ChannelTelegram,
		ExternalChatID: "9" + sfx[:4], AgentID: other.ID, SessionKey: "agent:x" + sfx + ":telegram:direct:9"})
	mustDo(t, err)
	cc := f.fe.chats[f.token]
	for name, convID := range map[string]string{"foreign": foreign.ID, "none": "", "unknown": "no-such-conversation"} {
		cc.ConversationID = convID
		f.fe.chats[f.token] = cc
		for _, tl := range []struct {
			name string
			args map[string]any
		}{
			{"list_approvals", map[string]any{}},
			{"decide_approval", map[string]any{"approval_id": tool.ID, "code": "ABCD", "decision": "approve"}},
			{"add_project", map[string]any{"repo_url": "https://h" + sfx + ".test/g/p.git"}},
		} {
			tl.args["chat_token"] = f.token
			if text, isErr := f.call(t, tl.name, tl.args); !isErr || !strings.Contains(text, "only available") {
				t.Errorf("%s with the %s conversation = %v %s", tl.name, name, isErr, text)
			}
		}
	}
	if a, _ := store.GetApproval(context.Background(), testPool, tool.ID); a.Status != store.ApprovalPending {
		t.Fatalf("approval decided: %+v", a)
	}
}

func sealedToken(t *testing.T, projectID string) string {
	t.Helper()
	blob, err := store.GetProjectTokenEnc(context.Background(), testPool, projectID)
	mustDo(t, err)
	tok, err := testCipher(t).Open(projectID, blob)
	mustDo(t, err)
	return tok
}

func TestChiefAddProject(t *testing.T) {
	f := newChatFixture(t, nil)
	ctx := context.Background()
	sfx := uniqueSuffix()
	host := "h" + sfx + ".test"
	key := "wave_pipeline"
	wave := &store.Workflow{Name: "Wave pipeline " + sfx, FromTemplate: &key,
		Definition: json.RawMessage(`{"nodes":[],"edges":[],"project":true,"input_schema":` + taskSchema + `}`)}
	mustDo(t, store.CreateWorkflow(ctx, testPool, wave))
	t.Cleanup(func() { _ = store.DeleteWorkflow(ctx, testPool, wave.ID) })

	// No project on the host and no configured token: the model is sent to the web UI.
	text, isErr := f.call(t, "add_project", map[string]any{"chat_token": f.token, "repo_url": "https://" + host + "/g/first-" + sfx + ".git"})
	if !isErr || !strings.Contains(text, "Projects page") || !strings.Contains(text, "never ask") {
		t.Fatalf("no token = %v %s", isErr, text)
	}
	// A token field is refused by the schema, and nothing is created.
	text, isErr = f.call(t, "add_project", map[string]any{"chat_token": f.token, "repo_url": "https://" + host + "/g/x-" + sfx + ".git", "token": "glpat-secret"})
	if !isErr || strings.Contains(text, "glpat-secret") {
		t.Fatalf("token field = %v %s", isErr, text)
	}
	if _, err := store.GetProjectByName(ctx, testPool, "x-"+sfx); err == nil {
		t.Fatal("a project was created from a call with a token")
	}

	// The configured token is the fallback.
	f.s.cfg.ProjectGitToken = acceptedTokenPrefix + "cfg" + sfx
	out, text, isErr := f.callJSON(t, "add_project", map[string]any{"repo_url": "https://" + host + "/g/first-" + sfx + ".git"})
	if isErr || out["name"] != "first-"+sfx || out["token_source"] != "config" || out["intake_enabled"] != false || strings.Contains(text, acceptedTokenPrefix) {
		t.Fatalf("fallback = %v %s", isErr, text)
	}
	if got := sealedToken(t, out["project_id"].(string)); got != f.s.cfg.ProjectGitToken {
		t.Fatalf("sealed token = %q", got)
	}
	t.Cleanup(func() { _ = store.DeleteProject(ctx, testPool, out["project_id"].(string)) })

	// A project in the same namespace lends its token, which wins over the config.
	f.s.cfg.ProjectGitToken = acceptedTokenPrefix + "other" + sfx
	host = "r" + sfx + ".test"
	lender := &store.Project{ID: store.NewID(), Name: "lender-" + sfx, RepoURL: "https://" + host + "/g/lender.git", Provider: "gitlab", DefaultBranch: "main"}
	blob, err := testCipher(t).Seal(lender.ID, acceptedTokenPrefix+"lent"+sfx)
	mustDo(t, err)
	mustDo(t, store.CreateProject(ctx, testPool, lender, blob))
	t.Cleanup(func() { _ = store.DeleteProject(ctx, testPool, lender.ID) })
	out, text, isErr = f.callJSON(t, "add_project", map[string]any{"repo_url": "https://" + host + "/g/second-" + sfx + ".git",
		"name": "Second " + sfx, "intake": map[string]any{}})
	if isErr || out["token_source"] != "project" || out["token_from_project"] != lender.Name || strings.Contains(text, acceptedTokenPrefix) {
		t.Fatalf("reuse = %v %s", isErr, text)
	}
	id := out["project_id"].(string)
	t.Cleanup(func() { _ = store.DeleteProject(ctx, testPool, id) })
	// M4: another group on the same host gets the configured token, never the lender's; with
	// none configured it gets the answer that sends the person to the Projects page. A
	// subgroup is a different namespace, and the namespace match ignores case.
	cfgTok := f.s.cfg.ProjectGitToken
	for _, repo := range []string{"other/tool-" + sfx, "g/sub/subtool-" + sfx} {
		o, text, isErr := f.callJSON(t, "add_project", map[string]any{"repo_url": "https://" + host + "/" + repo + ".git"})
		if isErr || o["token_source"] != "config" || o["token_from_project"] != nil || strings.Contains(text, acceptedTokenPrefix) {
			t.Fatalf("%s: other namespace = %v %s", repo, isErr, text)
		}
		other := o["project_id"].(string)
		t.Cleanup(func() { _ = store.DeleteProject(ctx, testPool, other) })
		if got := sealedToken(t, other); got != cfgTok {
			t.Fatalf("%s: sealed token is not the configured one", repo)
		}
	}
	f.s.cfg.ProjectGitToken = ""
	text, isErr = f.call(t, "add_project", map[string]any{"chat_token": f.token, "repo_url": "https://" + host + "/third/none-" + sfx + ".git"})
	if !isErr || !strings.Contains(text, "Projects page") {
		t.Fatalf("other namespace, no config = %v %s", isErr, text)
	}
	f.s.cfg.ProjectGitToken = cfgTok
	caseOut, text, isErr := f.callJSON(t, "add_project", map[string]any{"repo_url": "https://" + host + "/G/case-" + sfx + ".git"})
	if isErr || caseOut["token_from_project"] != lender.Name {
		t.Fatalf("same namespace, other case = %v %s", isErr, text)
	}
	t.Cleanup(func() { _ = store.DeleteProject(ctx, testPool, caseOut["project_id"].(string)) })
	if got := sealedToken(t, id); got != acceptedTokenPrefix+"lent"+sfx {
		t.Fatalf("sealed token = %q", got)
	}
	// Intake defaults: label yuno, the Wave pipeline copy, a daily cap of 10 USD.
	p, err := store.GetProject(ctx, testPool, id)
	mustDo(t, err)
	wave, err = store.GetWorkflowByFromTemplate(ctx, testPool, key) // the oldest copy, whichever test left it
	mustDo(t, err)
	if out["intake_enabled"] != true || out["intake_label"] != "yuno" || out["intake_workflow"] != wave.Name || out["budget_usd_per_day"] != float64(10) ||
		!p.IntakeEnabled || p.IntakeLabel != "yuno" || p.IntakeWorkflowID == nil || *p.IntakeWorkflowID != wave.ID ||
		p.BudgetDayUSDMicros == nil || int64(*p.BudgetDayUSDMicros) != 10_000_000 {
		t.Fatalf("intake = %v %+v", out, p)
	}
	// The API's own rules apply: a name in use is refused.
	text, isErr = f.call(t, "add_project", map[string]any{"chat_token": f.token, "repo_url": "https://" + host + "/g/third.git", "name": "Second " + sfx})
	if !isErr || !strings.Contains(text, "already exists") {
		t.Fatalf("duplicate name = %v %s", isErr, text)
	}
}

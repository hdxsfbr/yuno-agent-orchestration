package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"yuno/internal/store"
)

// questionRow stores a question of the fixture's run with the given status.
func questionRow(t *testing.T, pool *pgxpool.Pool, f *fixture, status string) *store.Approval {
	t.Helper()
	a := &store.Approval{RunID: f.run.ID, RunNodeID: f.visit.ID, Kind: store.ApprovalKindQuestion, NodeID: "coder",
		Message: "Which greeting language?", Options: json.RawMessage(`["Python","Go"]`), Status: status,
		RequestedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Hour).UTC()}
	must(t, store.InsertApproval(context.Background(), pool, a))
	return a
}

func answerInput(id, answer string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"chat_token": "c_testtoken", "question_id": id, "answer": answer})
	return b
}

// T5.24.11 (DESIGN 5.24 rule 9): the Chief answers a pending question; the engine records
// the answer as decided by "chat:<agent name>" and the result names the run and its page.
func TestAnswerQuestionHappyPath(t *testing.T) {
	pool := needDB(t)
	f := newFixture(t, pool, nil)
	q := questionRow(t, pool, f, store.ApprovalPending)
	chief := chiefAgent(t, pool, "Chief-"+uniq(), "answer_question")
	asker := &fakeAsker{}
	tool := NewAnswerQuestion(PlatformDeps{Pool: pool, Asker: asker, PublicBaseURL: "https://yuno.example/"})

	res := invoke(t, tool, Call{AgentID: chief.ID, Chat: true, ConversationID: "conv-1", Input: answerInput(q.ID, "  Spanish, please \n")})
	want := `{"run_id":"` + f.run.ID + `","status":"answered","url":"https://yuno.example/runs/` + f.run.ID + `"}`
	canon, err := CanonicalJSON(res.Output)
	if res.IsError || err != nil || string(canon) != want {
		t.Fatalf("result = %s (%+v), want %s", res.Output, res, want)
	}
	if got := asker.answerCalls(); len(got) != 1 || got[0] != (answerCall{ID: q.ID, Answer: "Spanish, please", By: "chat:" + chief.Name}) {
		t.Fatalf("engine calls = %+v", got)
	}
}

// The link is relative without PUBLIC_BASE_URL, as delegate_task's is.
func TestAnswerQuestionRelativeLink(t *testing.T) {
	pool := needDB(t)
	f := newFixture(t, pool, nil)
	q := questionRow(t, pool, f, store.ApprovalPending)
	chief := chiefAgent(t, pool, "Chief-"+uniq(), "answer_question")
	res := invoke(t, NewAnswerQuestion(PlatformDeps{Pool: pool, Asker: &fakeAsker{}}), Call{AgentID: chief.ID, Chat: true, Input: answerInput(q.ID, "Go")})
	var out struct {
		URL   string `json:"url"`
		RunID string `json:"run_id"`
	}
	must(t, json.Unmarshal(res.Output, &out))
	if out.URL != "/runs/"+f.run.ID || out.RunID != f.run.ID {
		t.Fatalf("result = %s", res.Output)
	}
}

// T5.24.11: the refusals, in the texts of rule 9; none of them reaches the engine.
func TestAnswerQuestionRefusals(t *testing.T) {
	pool := needDB(t)
	f := newFixture(t, pool, nil)
	chief := chiefAgent(t, pool, "Chief-"+uniq(), "answer_question")
	asker := &fakeAsker{}
	tool := NewAnswerQuestion(PlatformDeps{Pool: pool, Asker: asker})

	approved := questionRow(t, pool, f, store.ApprovalApproved)
	rejected := questionRow(t, pool, f, store.ApprovalRejected)
	expired := questionRow(t, pool, f, store.ApprovalExpired)
	pending := questionRow(t, pool, f, store.ApprovalPending)
	toolApproval := &store.Approval{RunID: f.run.ID, RunNodeID: f.visit.ID, Kind: store.ApprovalKindTool, NodeID: "deployer",
		Message: "Deploy release 1.0.0", Status: store.ApprovalPending, ExpiresAt: time.Now().Add(time.Hour).UTC()}
	must(t, store.InsertApproval(context.Background(), pool, toolApproval))

	for _, tc := range []struct {
		name string
		call Call
		want string
	}{
		{"an unknown id", Call{AgentID: chief.ID, Chat: true, Input: answerInput("no-such-id", "Go")}, "no question no-such-id"},
		{"a decided question (approved)", Call{AgentID: chief.ID, Chat: true, Input: answerInput(approved.ID, "Go")},
			"question " + approved.ID + " is already approved"},
		{"a decided question (declined)", Call{AgentID: chief.ID, Chat: true, Input: answerInput(rejected.ID, "Go")},
			"question " + rejected.ID + " is already rejected"},
		{"an expired question", Call{AgentID: chief.ID, Chat: true, Input: answerInput(expired.ID, "Go")},
			"question " + expired.ID + " is already expired"},
		{"a tool approval's id", Call{AgentID: chief.ID, Chat: true, Input: answerInput(toolApproval.ID, "Go")},
			toolApproval.ID + " is not a question"},
		{"a run token", Call{RunID: f.run.ID, NodeVisitID: f.visit.ID, AgentID: f.agent.ID,
			Input: json.RawMessage(`{"run_token":"r_testtoken","question_id":"` + pending.ID + `","answer":"Go"}`)},
			"answer_question is not available in a run"},
		{"a call that is neither a chat nor a run call", Call{AgentID: chief.ID, Input: answerInput(pending.ID, "Go")},
			"answer_question is not available in a run"},
		{"an empty answer", Call{AgentID: chief.ID, Chat: true, Input: answerInput(pending.ID, "")}, "answer: 1 to 4096 characters"},
		{"a blank answer", Call{AgentID: chief.ID, Chat: true, Input: answerInput(pending.ID, " \n ")}, "answer: 1 to 4096 characters"},
		{"an answer of 4097 bytes", Call{AgentID: chief.ID, Chat: true, Input: answerInput(pending.ID, strings.Repeat("a", 4097))},
			"answer: 1 to 4096 characters"},
		{"a blank id", Call{AgentID: chief.ID, Chat: true, Input: answerInput("  ", "Go")}, "question_id: must not be empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := invoke(t, tool, tc.call)
			if !res.IsError || res.Text != tc.want {
				t.Fatalf("result = %+v, want the error %q", res, tc.want)
			}
		})
	}
	if got := asker.answerCalls(); len(got) != 0 {
		t.Fatalf("a refused call reached the engine: %+v", got)
	}

	// 4096 bytes is the limit, and it is accepted.
	res := invoke(t, tool, Call{AgentID: chief.ID, Chat: true, Input: answerInput(pending.ID, strings.Repeat("a", 4096))})
	if res.IsError {
		t.Fatalf("an answer of 4096 bytes was refused: %+v", res)
	}
}

// The schema lets a run token through to the executor (which refuses it with the text of
// rule 9) and wants the id and the answer.
func TestAnswerQuestionSchema(t *testing.T) {
	tool := NewAnswerQuestion(PlatformDeps{})
	NewRegistry().Register(tool)
	if !tool.Chat || tool.Risk != RiskComputational || tool.Hits != HitsPostgres || tool.Name != "answer_question" {
		t.Fatalf("tool = %+v", tool)
	}
	for _, ok := range []string{
		`{"chat_token":"c_x","question_id":"q","answer":"a"}`,
		`{"run_token":"r_x","question_id":"q","answer":"a"}`,
	} {
		if err := tool.ValidateInput(json.RawMessage(ok)); err != nil {
			t.Errorf("schema refused %s: %v", ok, err)
		}
	}
	for _, bad := range []string{
		`{"chat_token":"c_x","answer":"a"}`,
		`{"chat_token":"c_x","question_id":"q"}`,
		`{"chat_token":"c_x","question_id":"","answer":"a"}`,
		`{"chat_token":"c_x","question_id":"q","answer":"a","extra":1}`,
	} {
		if err := tool.ValidateInput(json.RawMessage(bad)); err == nil {
			t.Errorf("schema accepted %s", bad)
		}
	}
	// It is one of the tools a chat token reaches (5.15 rule 2).
	if !IsChatTool("yuno__answer_question") || !IsChatTool("answer_question") {
		t.Error("answer_question is not in ChatToolNames")
	}
}

// A question decided between the Chief's lookup and the engine's write: the engine
// refuses, and the model reads which state the row is in now.
func TestAnswerQuestionDecidedMeanwhile(t *testing.T) {
	pool := needDB(t)
	f := newFixture(t, pool, nil)
	chief := chiefAgent(t, pool, "Chief-"+uniq(), "answer_question")
	q := questionRow(t, pool, f, store.ApprovalPending)
	asker := &fakeAsker{answerFn: func(id string) (*store.Approval, error) {
		// The operator answers in the UI at that moment.
		_, err := pool.Exec(context.Background(), `UPDATE approvals SET status = 'approved', answer = 'Python', decided_by = 'andre',
			decided_at = now() WHERE id = $1`, id)
		must(t, err)
		return nil, errors.New("approval is not pending")
	}}
	res := invoke(t, NewAnswerQuestion(PlatformDeps{Pool: pool, Asker: asker}), Call{AgentID: chief.ID, Chat: true, Input: answerInput(q.ID, "Go")})
	if !res.IsError || res.Text != "question "+q.ID+" is already approved" {
		t.Fatalf("result = %+v", res)
	}

	// A refusal of the engine's own that the row does not explain is an internal failure.
	q2 := questionRow(t, pool, f, store.ApprovalPending)
	broken := &fakeAsker{answerFn: func(string) (*store.Approval, error) { return nil, errors.New("record answer: connection reset") }}
	tool := NewAnswerQuestion(PlatformDeps{Pool: pool, Asker: broken})
	NewRegistry().Register(tool)
	if _, err := tool.Invoke(context.Background(), Call{AgentID: chief.ID, Chat: true, Input: answerInput(q2.ID, "Go")}); err == nil ||
		!strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("err = %v", err)
	}

	// A *ModelError of the engine passes through.
	texty := &fakeAsker{answerFn: func(string) (*store.Approval, error) { return nil, &ModelError{Message: "answer: nope"} }}
	res = invoke(t, NewAnswerQuestion(PlatformDeps{Pool: pool, Asker: texty}), Call{AgentID: chief.ID, Chat: true, Input: answerInput(q2.ID, "Go")})
	if !res.IsError || res.Text != "answer: nope" {
		t.Fatalf("result = %+v", res)
	}
}

// Without an engine or a database the tool cannot run; it says so as an internal failure.
func TestAnswerQuestionNotConfigured(t *testing.T) {
	tool := NewAnswerQuestion(PlatformDeps{})
	NewRegistry().Register(tool)
	if _, err := tool.Invoke(context.Background(), Call{Chat: true, AgentID: "a", Input: answerInput("q", "Go")}); err == nil {
		t.Fatal("no error")
	}
}

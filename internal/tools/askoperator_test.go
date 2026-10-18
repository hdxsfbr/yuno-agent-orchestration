package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"yuno/internal/store"
)

// fakeAsker is the engine behind ask_operator and answer_question: it records what the
// tools pass and answers with the rows a test queued.
type fakeAsker struct {
	mu sync.Mutex
	// asked are the AskOperator calls.
	asked []askedCall
	// askErr fails AskOperator.
	askErr error
	// decided is what WaitQuestion returns; nil makes it block until its context ends.
	decided *store.Approval
	// waitErr is returned by WaitQuestion after decided (a failure of the wait itself).
	waitErr error
	// answers are the AnswerQuestion calls; answerFn, when set, runs first and may fail
	// the call.
	answers  []answerCall
	answerFn func(id string) (*store.Approval, error)
}

type askedCall struct {
	Token, Question string
	Options         []string
}

type answerCall struct{ ID, Answer, By string }

func (f *fakeAsker) AskOperator(_ context.Context, token, question string, options []string) (*store.Approval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, askedCall{Token: token, Question: question, Options: options})
	if f.askErr != nil {
		return nil, f.askErr
	}
	return &store.Approval{ID: "q-1", RunID: "run-1", Kind: store.ApprovalKindQuestion, Message: question, Status: store.ApprovalPending}, nil
}

func (f *fakeAsker) WaitQuestion(ctx context.Context, id string) (*store.Approval, error) {
	f.mu.Lock()
	decided, werr := f.decided, f.waitErr
	f.mu.Unlock()
	if werr != nil {
		return nil, werr
	}
	if decided != nil {
		d := *decided
		d.ID = id
		return &d, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (f *fakeAsker) AnswerQuestion(_ context.Context, id, answer, by string) (*store.Approval, error) {
	f.mu.Lock()
	fn := f.answerFn
	f.answers = append(f.answers, answerCall{ID: id, Answer: answer, By: by})
	f.mu.Unlock()
	if fn != nil {
		return fn(id)
	}
	return &store.Approval{ID: id, RunID: "run-1", Kind: store.ApprovalKindQuestion, Status: store.ApprovalApproved}, nil
}

func (f *fakeAsker) answerCalls() []answerCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]answerCall(nil), f.answers...)
}

func (f *fakeAsker) askedCalls() []askedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]askedCall(nil), f.asked...)
}

// parkingAsker is a fakeAsker that also satisfies QuestionParker.
type parkingAsker struct {
	*fakeAsker
	mu      sync.Mutex
	parked  []string
	err     error
	ctxDead bool
}

func (p *parkingAsker) QuestionPending(ctx context.Context, token, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.parked = append(p.parked, token+"/"+id)
	p.ctxDead = ctx.Err() != nil
	return p.err
}

func askInput(question string, options ...string) json.RawMessage {
	in := map[string]any{"run_token": "r_testtoken", "question": question}
	if options != nil {
		in["options"] = options
	}
	b, _ := json.Marshal(in)
	return b
}

func decidedRow(status string, mut func(a *store.Approval)) *store.Approval {
	a := &store.Approval{Kind: store.ApprovalKindQuestion, Status: status}
	if mut != nil {
		mut(a)
	}
	return a
}

// DESIGN 5.24 rule 1: the tool's identity. It is a run tool (no chat token, which the MCP
// gate answers "ask_operator is not available in a conversation"), computational, never
// retried, and its timeout is the wait plus 30 s.
func TestAskOperatorIdentity(t *testing.T) {
	tool := NewAskOperator(PlatformDeps{})
	if tool.Name != "ask_operator" || tool.Risk != RiskComputational || tool.Hits != HitsPostgres || tool.Chat || tool.MaxAttempts != 1 {
		t.Fatalf("tool = %+v", tool)
	}
	if tool.Timeout != 540*time.Second+30*time.Second {
		t.Errorf("default timeout = %v, want 570 s", tool.Timeout)
	}
	if got := NewAskOperator(PlatformDeps{MCPToolWait: 90 * time.Second}).Timeout; got != 120*time.Second {
		t.Errorf("timeout for a 90 s wait = %v, want 120 s", got)
	}
	// A chat token reaches the gate's "not available in a conversation" because the tool is
	// not chat-capable, in the registry and in the list the materialiser reads.
	reg := NewPlatformRegistry(PlatformDeps{})
	if got, ok := reg.Get(AskOperatorName); !ok || got.Chat || IsChatTool(AskOperatorName) || IsChatTool("yuno__ask_operator") {
		t.Fatalf("ask_operator is chat-capable: %+v", got)
	}
	// The description is the paragraph of rule 12 in two sentences.
	if n := strings.Count(tool.Description, ". ") + 1; n != 2 || !strings.HasSuffix(tool.Description, ".") {
		t.Errorf("description has %d sentences: %q", n, tool.Description)
	}
	for _, want := range []string{"at most 3 per step", `"pending"`, "yuno__finish", "the answer added to your task"} {
		if !strings.Contains(tool.Description, want) {
			t.Errorf("description lacks %q: %s", want, tool.Description)
		}
	}
	// The schema asks for the run token and the question; options are optional.
	NewRegistry().Register(tool)
	for _, bad := range []string{`{"question":"q"}`, `{"run_token":"r_x"}`, `{"run_token":"r_x","question":"q","extra":1}`,
		`{"run_token":"r_x","question":"q","options":"a"}`, `{"chat_token":"c_x","question":"q"}`} {
		if err := tool.ValidateInput(json.RawMessage(bad)); err == nil {
			t.Errorf("schema accepted %s", bad)
		}
	}
	if err := tool.ValidateInput(askInput("Which language?", "go", "rust")); err != nil {
		t.Errorf("schema refused a good call: %v", err)
	}
}

// T5.24.1: the validation texts of rule 1, on the tool's own executor, before anything is
// asked of the engine.
func TestAskOperatorValidation(t *testing.T) {
	ok := strings.Repeat("a", 100)
	for _, tc := range []struct {
		name  string
		input json.RawMessage
		want  string // "" = accepted
	}{
		{"empty question", askInput(""), "question: 1 to 1024 characters"},
		{"blank question", askInput(" \n\t "), "question: 1 to 1024 characters"},
		{"question of 1025 bytes", askInput(strings.Repeat("q", 1025)), "question: 1 to 1024 characters"},
		{"question of 1025 bytes inside spaces", askInput("  " + strings.Repeat("q", 1025) + "  "), "question: 1 to 1024 characters"},
		{"question of 1024 bytes", askInput(strings.Repeat("q", 1024)), ""},
		{"question of 1024 bytes inside spaces", askInput("  " + strings.Repeat("q", 1024) + "\n"), ""},
		{"multibyte characters count as bytes", askInput(strings.Repeat("é", 513)), "question: 1 to 1024 characters"},
		{"a NUL in the question", askInput("which\x00one?"), "question: 1 to 1024 characters"},
		{"a NUL alone", askInput("\x00"), "question: 1 to 1024 characters"},
		{"a newline in the question is fine", askInput("which\none?"), ""},
		{"no options", askInput("q?"), ""},
		{"null options", json.RawMessage(`{"run_token":"r_x","question":"q?","options":null}`), ""},
		{"an empty list", json.RawMessage(`{"run_token":"r_x","question":"q?","options":[]}`), "options: 2 to 5 distinct options of at most 100 characters"},
		{"one option", askInput("q?", "yes"), "options: 2 to 5 distinct options of at most 100 characters"},
		{"two options", askInput("q?", "yes", "no"), ""},
		{"five options", askInput("q?", "a", "b", "c", "d", "e"), ""},
		{"six options", askInput("q?", "a", "b", "c", "d", "e", "f"), "options: 2 to 5 distinct options of at most 100 characters"},
		{"a duplicate", askInput("q?", "yes", "no", "yes"), "options: 2 to 5 distinct options of at most 100 characters"},
		{"a duplicate after trimming", askInput("q?", "yes", " yes "), "options: 2 to 5 distinct options of at most 100 characters"},
		{"an empty entry", askInput("q?", "yes", ""), "options: 2 to 5 distinct options of at most 100 characters"},
		{"a blank entry", askInput("q?", "yes", "  "), "options: 2 to 5 distinct options of at most 100 characters"},
		{"a control character", askInput("q?", "yes", "n\x00o"), "options: 2 to 5 distinct options of at most 100 characters"},
		{"a newline", askInput("q?", "yes", "n\no"), "options: 2 to 5 distinct options of at most 100 characters"},
		{"a tab", askInput("q?", "yes", "n\to"), "options: 2 to 5 distinct options of at most 100 characters"},
		{"an option of 101 bytes", askInput("q?", "yes", ok+"b"), "options: 2 to 5 distinct options of at most 100 characters"},
		{"an option of 100 bytes", askInput("q?", "yes", ok), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asker := &fakeAsker{decided: decidedRow(store.ApprovalApproved, func(a *store.Approval) { a.Answer = ptr("fine") })}
			tool := NewAskOperator(PlatformDeps{Asker: asker, MCPToolWait: time.Second})
			res := invoke(t, tool, Call{RunID: "run-1", Input: tc.input})
			if tc.want != "" {
				if !res.IsError || res.Text != tc.want {
					t.Fatalf("result = %+v, want the error %q", res, tc.want)
				}
				if n := len(asker.askedCalls()); n != 0 {
					t.Fatalf("a refused call reached the engine %d times", n)
				}
				return
			}
			if res.IsError {
				t.Fatalf("a good call was refused: %+v", res)
			}
			if n := len(asker.askedCalls()); n != 1 {
				t.Fatalf("engine calls = %d, want 1", n)
			}
		})
	}
}

// DESIGN 5.24 rule 2: the engine gets the token, the trimmed question and the trimmed
// options, or none.
func TestAskOperatorPassesTrimmedInput(t *testing.T) {
	asker := &fakeAsker{decided: decidedRow(store.ApprovalApproved, nil)}
	tool := NewAskOperator(PlatformDeps{Asker: asker, MCPToolWait: time.Second})
	invoke(t, tool, Call{RunID: "run-1", Input: askInput("  Which greeting language?\n", " Python ", "Go")})
	invoke(t, tool, Call{RunID: "run-1", Input: askInput("No options?")})
	got := asker.askedCalls()
	if len(got) != 2 {
		t.Fatalf("asked = %+v", got)
	}
	if got[0].Token != "r_testtoken" || got[0].Question != "Which greeting language?" || strings.Join(got[0].Options, "|") != "Python|Go" {
		t.Errorf("first = %+v", got[0])
	}
	if got[1].Options != nil {
		t.Errorf("a question without options passed %v", got[1].Options)
	}
}

// DESIGN 5.24 rule 4 (T5.24.2, T5.24.3): every outcome of the wait is a result, with the
// shapes the rule gives.
func TestAskOperatorResultShapes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		decided *store.Approval
		want    string
	}{
		{"answered", decidedRow(store.ApprovalApproved, func(a *store.Approval) { a.Answer = ptr("Spanish, please") }),
			`{"answer":"Spanish, please","question_id":"q-1","status":"answered"}`},
		{"answered with an empty answer row", decidedRow(store.ApprovalApproved, nil),
			`{"answer":"","question_id":"q-1","status":"answered"}`},
		{"declined with a reason", decidedRow(store.ApprovalRejected, func(a *store.Approval) { a.Reason = ptr("not now") }),
			`{"question_id":"q-1","reason":"not now","status":"declined"}`},
		{"declined without a reason", decidedRow(store.ApprovalRejected, nil),
			`{"question_id":"q-1","reason":"the operator declined to answer","status":"declined"}`},
		{"declined with a blank reason", decidedRow(store.ApprovalRejected, func(a *store.Approval) { a.Reason = ptr("  ") }),
			`{"question_id":"q-1","reason":"the operator declined to answer","status":"declined"}`},
		{"expired within the wait", decidedRow(store.ApprovalExpired, nil),
			`{"question_id":"q-1","status":"unanswered"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asker := &parkingAsker{fakeAsker: &fakeAsker{decided: tc.decided}}
			tool := NewAskOperator(PlatformDeps{Asker: asker, MCPToolWait: time.Second})
			res := invoke(t, tool, Call{RunID: "run-1", Input: askInput("Which language?")})
			canon, err := CanonicalJSON(res.Output)
			if res.IsError || err != nil || string(canon) != tc.want {
				t.Fatalf("result = %s (%+v), want %s", res.Output, res, tc.want)
			}
			if len(asker.parked) != 0 {
				t.Errorf("a decided question was marked pending: %v", asker.parked)
			}
		})
	}
}

// DESIGN 5.24 rule 4 (T5.24.4): the wait ends first. The result says pending, the engine
// is told so through QuestionParker when it implements it, and the question is not lost
// when it does not.
func TestAskOperatorPendingAfterTheWait(t *testing.T) {
	t.Run("the engine parks the visit", func(t *testing.T) {
		asker := &parkingAsker{fakeAsker: &fakeAsker{}}
		tool := NewAskOperator(PlatformDeps{Asker: asker, MCPToolWait: 30 * time.Millisecond})
		start := time.Now()
		res := invoke(t, tool, Call{RunID: "run-1", Input: askInput("Which language?")})
		if res.IsError || string(res.Output) != `{"question_id":"q-1","status":"pending"}` {
			t.Fatalf("result = %+v", res)
		}
		if d := time.Since(start); d < 30*time.Millisecond || d > 5*time.Second {
			t.Errorf("returned after %v", d)
		}
		if len(asker.parked) != 1 || asker.parked[0] != "r_testtoken/q-1" || asker.ctxDead {
			t.Errorf("QuestionPending calls = %v (context dead: %v)", asker.parked, asker.ctxDead)
		}
	})
	t.Run("an engine without the parker", func(t *testing.T) {
		tool := NewAskOperator(PlatformDeps{Asker: &fakeAsker{}, MCPToolWait: 20 * time.Millisecond})
		res := invoke(t, tool, Call{RunID: "run-1", Input: askInput("Which language?")})
		if res.IsError || string(res.Output) != `{"question_id":"q-1","status":"pending"}` {
			t.Fatalf("result = %+v", res)
		}
	})
	t.Run("a parker that fails is still pending", func(t *testing.T) {
		asker := &parkingAsker{fakeAsker: &fakeAsker{}, err: errors.New("database down")}
		tool := NewAskOperator(PlatformDeps{Asker: asker, MCPToolWait: 20 * time.Millisecond})
		res := invoke(t, tool, Call{RunID: "run-1", Input: askInput("Which language?")})
		if res.IsError || string(res.Output) != `{"question_id":"q-1","status":"pending"}` || len(asker.parked) != 1 {
			t.Fatalf("result = %+v, parked %v", res, asker.parked)
		}
	})
	t.Run("a wait that fails after the row exists is pending, never an error", func(t *testing.T) {
		asker := &parkingAsker{fakeAsker: &fakeAsker{waitErr: errors.New("waiter gone")}}
		tool := NewAskOperator(PlatformDeps{Asker: asker, MCPToolWait: time.Minute})
		res := invoke(t, tool, Call{RunID: "run-1", Input: askInput("Which language?")})
		if res.IsError || string(res.Output) != `{"question_id":"q-1","status":"pending"}` || len(asker.parked) != 1 {
			t.Fatalf("result = %+v, parked %v", res, asker.parked)
		}
	})
	t.Run("a row that is still pending when the wait returns", func(t *testing.T) {
		asker := &parkingAsker{fakeAsker: &fakeAsker{decided: decidedRow(store.ApprovalPending, nil)}}
		tool := NewAskOperator(PlatformDeps{Asker: asker, MCPToolWait: time.Minute})
		res := invoke(t, tool, Call{RunID: "run-1", Input: askInput("Which language?")})
		if res.IsError || string(res.Output) != `{"question_id":"q-1","status":"pending"}` || len(asker.parked) != 1 {
			t.Fatalf("result = %+v, parked %v", res, asker.parked)
		}
	})
	t.Run("the request is cancelled while waiting", func(t *testing.T) {
		// The MCP client went away: the engine still hears that the wait is over, on a
		// context that is not cancelled, or the run would stay paused on a dead call.
		asker := &parkingAsker{fakeAsker: &fakeAsker{}}
		tool := NewAskOperator(PlatformDeps{Asker: asker, MCPToolWait: time.Minute})
		NewRegistry().Register(tool)
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(20*time.Millisecond, cancel)
		res, err := tool.Invoke(ctx, Call{RunID: "run-1", Input: askInput("Which language?")})
		if err != nil || res.IsError || string(res.Output) != `{"question_id":"q-1","status":"pending"}` {
			t.Fatalf("result = %+v, err %v", res, err)
		}
		if len(asker.parked) != 1 || asker.ctxDead {
			t.Errorf("QuestionPending calls = %v (context dead: %v)", asker.parked, asker.ctxDead)
		}
	})
}

// DESIGN 5.24 rule 2: what the engine refuses reaches the model as its own text; a
// token that ended since the gate resolved it reads "invalid run token"; a failure of the
// engine is the tool's internal error.
func TestAskOperatorEngineRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want string // "" = an internal failure
	}{
		"one at a time": {&ModelError{Message: "one question at a time: question q-0 is still pending"}, "one question at a time: question q-0 is still pending"},
		"three per step": {fmt.Errorf("ask: %w", &ModelError{Message: "ask fewer questions: this step already asked 3"}),
			"ask fewer questions: this step already asked 3"},
		"invalid token":        {errors.New("invalid run token"), "invalid run token"},
		"wrapped invalid":      {fmt.Errorf("ask: %w", errors.New("invalid run token")), "invalid run token"},
		"joined invalid token": {errors.Join(errors.New("lookup"), errors.New("invalid run token")), "invalid run token"},
		"a failed insert":      {errors.New("record question: connection reset"), ""},
	} {
		t.Run(name, func(t *testing.T) {
			asker := &fakeAsker{askErr: tc.err}
			tool := NewAskOperator(PlatformDeps{Asker: asker, MCPToolWait: time.Second})
			NewRegistry().Register(tool)
			res, err := tool.Invoke(context.Background(), Call{RunID: "run-1", Input: askInput("Which language?")})
			if tc.want == "" {
				if err == nil || !strings.Contains(err.Error(), "connection reset") {
					t.Fatalf("err = %v, result %+v", err, res)
				}
				return
			}
			if err != nil || !res.IsError || res.Text != tc.want {
				t.Fatalf("result = %+v, err %v, want the error %q", res, err, tc.want)
			}
		})
	}
	// No engine at all is an internal failure, after the input checks.
	tool := NewAskOperator(PlatformDeps{})
	NewRegistry().Register(tool)
	if res, err := tool.Invoke(context.Background(), Call{Input: askInput("")}); err != nil || res.Text != "question: 1 to 1024 characters" {
		t.Errorf("validation comes first: %+v %v", res, err)
	}
	if _, err := tool.Invoke(context.Background(), Call{Input: askInput("q?")}); err == nil {
		t.Error("no engine, no error")
	}
}

// The registry offers both question tools, and only answer_question is chat-capable.
func TestPlatformRegistryHasTheQuestionTools(t *testing.T) {
	reg := NewPlatformRegistry(PlatformDeps{})
	ask, ok1 := reg.Get(AskOperatorName)
	answer, ok2 := reg.Get(AnswerQuestionName)
	if !ok1 || !ok2 || ask.Chat || !answer.Chat {
		t.Fatalf("ask_operator %v chat %v, answer_question %v chat %v", ok1, ask != nil && ask.Chat, ok2, answer != nil && answer.Chat)
	}
}

// Review L4: a NUL or invalid UTF-8 cannot be stored. JSON input never carries invalid
// UTF-8 into the executor (the decoder replaces it), so the check on bytes is shown on the
// helper both validations use.
func TestPlainText(t *testing.T) {
	for s, want := range map[string]bool{
		"": true, "plain": true, "linhas\ncom acentuação": true, "日本語": true,
		"a\x00b": false, "\x00": false, "a\xffb": false, "\xc3": false, "\xed\xa0\x80": false, // a surrogate half
	} {
		if got := plainText(s); got != want {
			t.Errorf("plainText(%q) = %v, want %v", s, got, want)
		}
	}
}

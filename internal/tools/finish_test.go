package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestFinishRun(t *testing.T) {
	internal := errors.New("db down")
	tests := []struct {
		name      string
		input     string
		acceptErr error
		wantText  string // substring of the model-visible text
		wantIsErr bool
		wantErr   bool
		wantCall  bool
	}{
		{name: "accepted", input: `{"run_token":"r_abc","result":{"ok":true}}`, wantText: `{"status":"accepted"}`, wantCall: true},
		{name: "schema violation from engine", input: `{"run_token":"r_abc","result":{}}`, acceptErr: &ModelError{Message: "result: missing property 'summary'"}, wantText: "missing property 'summary'", wantIsErr: true, wantCall: true},
		{name: "invalid token", input: `{"run_token":"r_nope","result":{}}`, acceptErr: &ModelError{Message: "invalid run token"}, wantText: "invalid run token", wantIsErr: true, wantCall: true},
		{name: "missing run_token", input: `{"result":{}}`, wantText: "missing property 'run_token'", wantIsErr: true},
		// D-104: the result as JSON text, as strict OpenAI tool schemas allow.
		{name: "result as JSON text", input: `{"run_token":"r_abc","result":"{\"ok\":true}"}`, wantText: `{"status":"accepted"}`, wantCall: true},
		{name: "text that is not an object", input: `{"run_token":"r_abc","result":"done"}`, wantText: "JSON text of an object", wantIsErr: true},
		{name: "text holding an array", input: `{"run_token":"r_abc","result":"[1]"}`, wantText: "JSON text of an object", wantIsErr: true},
		{name: "result neither text nor object", input: `{"run_token":"r_abc","result":7}`, wantText: "at '/result'", wantIsErr: true},
		{name: "extra property", input: `{"run_token":"r_abc","result":{},"note":"x"}`, wantText: "note", wantIsErr: true},
		{name: "no arguments", input: ``, wantText: "invalid input", wantIsErr: true},
		{name: "internal failure", input: `{"run_token":"r_abc","result":{}}`, acceptErr: internal, wantErr: true, wantCall: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotToken string
			var gotResult json.RawMessage
			called := false
			r := NewPlatformRegistry(PlatformDeps{AcceptFinish: func(ctx context.Context, token string, result json.RawMessage) error {
				called = true
				gotToken, gotResult = token, result
				if _, ok := ctx.Deadline(); !ok {
					t.Error("finish ran without a timeout")
				}
				return tc.acceptErr
			}})
			tool, ok := r.Get(FinishName)
			if !ok {
				t.Fatal("finish is not registered")
			}
			res, err := tool.Run(context.Background(), Call{Input: json.RawMessage(tc.input)}, 1024)
			if tc.wantErr {
				if !errors.Is(err, internal) {
					t.Fatalf("err = %v, want wrapped internal error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.IsError != tc.wantIsErr || !strings.Contains(res.Text, tc.wantText) {
				t.Fatalf("result = %+v, want IsError=%v text containing %q", res, tc.wantIsErr, tc.wantText)
			}
			if called != tc.wantCall {
				t.Fatalf("accept called = %v, want %v", called, tc.wantCall)
			}
			if (tc.name == "accepted" || tc.name == "result as JSON text") && (gotToken != "r_abc" || string(gotResult) != `{"ok":true}`) {
				t.Fatalf("accept got token %q result %s", gotToken, gotResult)
			}
		})
	}
}

func TestFinishRegistration(t *testing.T) {
	tool, _ := NewPlatformRegistry(PlatformDeps{}).Get(FinishName)
	if tool.Risk != RiskComputational || tool.Hits != HitsPostgres || tool.Timeout != 5*time.Second || tool.TimeoutS != 5 {
		t.Fatalf("finish = risk %s hits %s timeout %s (%d s)", tool.Risk, tool.Hits, tool.Timeout, tool.TimeoutS)
	}
	var schema struct {
		Required             []string       `json:"required"`
		AdditionalProperties *bool          `json:"additionalProperties"`
		Properties           map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.AdditionalProperties == nil || *schema.AdditionalProperties || len(schema.Properties) != 2 || len(schema.Required) != 2 {
		t.Fatalf("schema = %s", tool.InputSchema)
	}
}

func TestRunRedactsAndCaps(t *testing.T) {
	r := NewRegistry()
	r.Register(&Tool{
		Name:        "echo",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Exec: func(context.Context, Call) (Result, error) {
			return Result{Text: "key sk-ant-abcdefghijklmnop then " + strings.Repeat("x", 200)}, nil
		},
	})
	tool, _ := r.Get("echo")
	res, err := tool.Run(context.Background(), Call{Input: json.RawMessage(`{}`)}, 64)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Text, "sk-ant-") || !strings.Contains(res.Text, "[redacted]") {
		t.Fatalf("secret not redacted: %q", res.Text)
	}
	if !strings.Contains(res.Text, "[truncated ") || len(res.Text) > 64+32 {
		t.Fatalf("output not capped: %d bytes %q", len(res.Text), res.Text)
	}
}

func TestRegisterRejectsBadSchema(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Register accepted an invalid schema")
		}
	}()
	NewRegistry().Register(&Tool{Name: "bad", InputSchema: json.RawMessage(`{"type":"nope"}`)})
}

func TestOpenClawBuiltins(t *testing.T) {
	want := []string{"apply_patch", "edit", "exec", "ls", "process", "read", "write"}
	got := OpenClawBuiltins()
	if len(got) != len(want) {
		t.Fatalf("got %d built-ins, want %d", len(got), len(want))
	}
	for i, b := range got {
		if b.Name != want[i] || b.Hits != HitsOpenClaw || b.Description == "" {
			t.Errorf("built-in %d = %+v", i, b)
		}
		if b.Risk != RiskReadOnly && b.Risk != RiskComputational {
			t.Errorf("%s risk %s", b.Name, b.Risk)
		}
		if !IsOpenClawBuiltin(b.Name) {
			t.Errorf("IsOpenClawBuiltin(%q) = false", b.Name)
		}
	}
	if IsOpenClawBuiltin("finish") || IsOpenClawBuiltin("browser") {
		t.Error("IsOpenClawBuiltin accepts a non-built-in")
	}
}

// Schema refusals go through the same redaction and cap as executor output (finding 19).
func TestRunSanitisesSchemaErrors(t *testing.T) {
	r := NewRegistry()
	r.Register(&Tool{
		Name:        "strict",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}},"additionalProperties":false}`),
		Exec:        func(context.Context, Call) (Result, error) { return Result{Text: "ran"}, nil },
	})
	tool, _ := r.Get("strict")
	props := map[string]any{"sk-ant-api03-abcdefghijklmnopqrstuvwxyz": 1, "1234567890:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw_X": 2}
	for i := range 300 {
		props[fmt.Sprintf("unexpected_property_number_%03d", i)] = i
	}
	in, _ := json.Marshal(props)
	res, err := tool.Run(context.Background(), Call{Input: in}, 256)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.HasPrefix(res.Text, "invalid input") {
		t.Fatalf("result = %+v", res)
	}
	if strings.Contains(res.Text, "sk-ant-") || strings.Contains(res.Text, "AAHdqTcvCH1") {
		t.Fatalf("credential-shaped property name echoed: %q", res.Text)
	}
	if !strings.Contains(res.Text, "[truncated ") || len(res.Text) > 256+32 {
		t.Fatalf("schema error not capped: %d bytes", len(res.Text))
	}
}

// A finish call refused by the input schema is reported for the run token it carries,
// so it counts toward the node's finish attempts (finding 13).
func TestFinishReportsSchemaRejections(t *testing.T) {
	type rejection struct{ token, msg string }
	var got []rejection
	r := NewPlatformRegistry(PlatformDeps{
		AcceptFinish: func(context.Context, string, json.RawMessage) error { t.Error("accept called"); return nil },
		RejectFinish: func(_ context.Context, token, msg string) { got = append(got, rejection{token, msg}) },
	})
	tool, _ := r.Get(FinishName)
	for _, in := range []string{
		`{"run_token":"r_abc","result":7}`,
		`{"run_token":"r_abc"}`,
		`{"result":{}}`,                         // no token: nothing to count against
		`{"run_token":7,"result":1}`,            // not a token
		`{"run_token":"r_abc","result":"done"}`, // text that is not an object (D-104)
	} {
		res, err := tool.Run(context.Background(), Call{Input: json.RawMessage(in)}, 1024)
		if err != nil || !res.IsError {
			t.Fatalf("%s: %+v %v", in, res, err)
		}
	}
	if len(got) != 3 || got[0].token != "r_abc" || got[1].token != "r_abc" || got[2].token != "r_abc" ||
		!strings.Contains(got[0].msg, "/result") || !strings.Contains(got[1].msg, "result") ||
		!strings.Contains(got[2].msg, "JSON text of an object") {
		t.Fatalf("rejections = %+v", got)
	}
}

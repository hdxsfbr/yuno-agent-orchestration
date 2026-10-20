// Package tools is the registry of things an agent can call. Every tool has an explicit
// JSON Schema, a risk class, a timeout, a retry budget and a declaration of whether its
// output is untrusted (DESIGN 5.7 rule 9).
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"yuno/internal/obs"
)

type Risk string

const (
	RiskReadOnly      Risk = "read_only"
	RiskComputational Risk = "computational"
	RiskSideEffecting Risk = "side_effecting"
)

// Hits says what the tool talks to, for the UI and the README's tool table.
type Hits string

const (
	HitsNone         Hits = "none"
	HitsPostgres     Hits = "postgres"
	HitsLocalService Hits = "local_service"
	HitsExternal     Hits = "external"
	// HitsOpenClaw marks the runtime's built-in tools, which OpenClaw executes.
	HitsOpenClaw Hits = "openclaw"
)

// Call is one invocation as requested by the model, with run context.
type Call struct {
	RunID       string
	NodeVisitID string
	NodeID      string
	AgentID     string
	ToolUseID   string
	Input       json.RawMessage
	// IdempotencyKey is set for side-effecting tools before the approval gate (DESIGN
	// 5.4 rule 1) and travels with the external request.
	IdempotencyKey string
	// ToolCallID is the tool_calls row recording this call.
	ToolCallID string
	// OpenClawAgentID and Workspace locate the workspace the call's visit works in, where
	// run artifacts live under runs/<run_id>/ (DESIGN section 4): the visit's private
	// workspace for a run call (5.30), the agent's for a chat call.
	OpenClawAgentID string
	Workspace       string
	// Chat marks a chat-time call (DESIGN 5.15): it carried a chat token, has no run or
	// node visit, and ConversationID is the calling agent's most recent conversation
	// ("" when it has none).
	Chat           bool
	ConversationID string
	// Tx is the transaction a side-effecting tool runs in, set by the executor
	// (store.RunSideEffectTx): rows the tool writes through it commit together with the
	// effect's stored result and disappear with the key when the call fails (open_merge_request
	// writes its merge request record this way, DESIGN 5.20 rule 2). Nil for every other
	// call; the tool does not commit or roll it back.
	Tx pgx.Tx
}

// Result is what goes back to the model. Output is JSON; Text is a convenience for
// plain-text results and is wrapped into {"text": ...} when Output is nil.
type Result struct {
	Output  json.RawMessage
	Text    string
	IsError bool
	// Transient marks an error the retry policy may retry (DESIGN 5.5 rule 4).
	Transient bool
	// Blocked marks an error result that is a refusal rather than a failure, such as
	// delegate_task's rate limit (DESIGN 5.15 rule 6): the call is recorded as blocked.
	Blocked bool
}

// ModelText is what the model sees: Output as JSON text when set, else Text.
func (r Result) ModelText() string {
	if len(r.Output) > 0 {
		return string(r.Output)
	}
	return r.Text
}

// ModelError is an executor failure the model can act on, such as a result that does
// not match the node's output schema or an invalid run token. Run turns it into an
// error result the model sees instead of a failed call.
type ModelError struct {
	Message string
}

func (e *ModelError) Error() string { return e.Message }

type Executor func(ctx context.Context, call Call) (Result, error)

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	Risk        Risk            `json:"risk"`
	Hits        Hits            `json:"hits"`
	Timeout     time.Duration   `json:"-"`
	TimeoutS    int             `json:"timeout_seconds"`
	MaxAttempts int             `json:"max_attempts"`
	Untrusted   bool            `json:"untrusted_output"`
	// Chat marks a tool an agent may call while chatting with a person, with its chat
	// token instead of a run token (DESIGN 5.15 rule 2).
	Chat bool     `json:"chat"`
	Exec Executor `json:"-"`
	// OnInvalidInput, when set, is told about a call refused by the input schema before
	// the refusal goes back to the model (finish counts it as a rejected attempt).
	OnInvalidInput func(ctx context.Context, call Call, message string) `json:"-"`
	// Describe, when set, renders the sentence a human reads when the call waits for an
	// approval (DESIGN 5.3 rule 5). The input has already passed the schema.
	Describe func(input json.RawMessage) string `json:"-"`

	schema *jsonschema.Schema
}

type Registry struct {
	mu    sync.RWMutex
	tools map[string]*Tool
}

func NewRegistry() *Registry {
	return &Registry{tools: map[string]*Tool{}}
}

// Register adds t, filling in the timeout and retry defaults. It panics when the input
// schema does not compile: schemas are static, so that is a programming error.
func (r *Registry) Register(t *Tool) {
	if len(t.InputSchema) > 0 {
		sch, err := compileSchema(t.Name, t.InputSchema)
		if err != nil {
			panic(err)
		}
		t.schema = sch
	}
	if t.Timeout == 0 {
		t.Timeout = 30 * time.Second
	}
	t.TimeoutS = int(t.Timeout / time.Second)
	if t.MaxAttempts == 0 {
		if t.Risk == RiskSideEffecting {
			t.MaxAttempts = 2
		} else {
			t.MaxAttempts = 3
		}
	}
	r.mu.Lock()
	r.tools[t.Name] = t
	r.mu.Unlock()
}

func (r *Registry) Get(name string) (*Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// List returns tools sorted by name.
func (r *Registry) List() []*Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Names is the set of registered names, for allowlist validation.
func (r *Registry) Names() map[string]bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]bool, len(r.tools))
	for n := range r.tools {
		out[n] = true
	}
	return out
}

// ValidateInput checks input against the tool's input schema. Missing input is
// validated as an empty object. The error lists every violation with its JSON pointer.
func (t *Tool) ValidateInput(input json.RawMessage) error {
	if t.schema == nil {
		return nil
	}
	if len(bytes.TrimSpace(input)) == 0 {
		input = json.RawMessage(`{}`)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(input))
	if err != nil {
		return fmt.Errorf("invalid input: not valid JSON: %v", err)
	}
	err = t.schema.Validate(inst)
	if err == nil {
		return nil
	}
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return fmt.Errorf("invalid input: %v", err)
	}
	var msgs []string
	collectLeaves(ve, &msgs)
	return errors.New("invalid input: " + strings.Join(msgs, "; "))
}

// Run executes one call the way every platform tool is executed (DESIGN 5.7 rule 4):
// the input is validated against the schema, the executor runs under the tool's
// timeout, and the text for the model is redacted and capped at maxOutputBytes (0 means
// no cap). The returned Result carries that text in Text and no Output. Schema
// violations and *ModelError failures come back as error results, sanitised the same
// way; err is reserved for failures the model cannot fix.
func (t *Tool) Run(ctx context.Context, call Call, maxOutputBytes int) (Result, error) {
	if err := t.ValidateInput(call.Input); err != nil {
		if t.OnInvalidInput != nil {
			t.OnInvalidInput(ctx, call, err.Error())
		}
		return Result{IsError: true, Text: obs.Truncate(obs.Redact(err.Error()), maxOutputBytes)}, nil
	}
	res, err := t.Invoke(ctx, call)
	if err != nil {
		return Result{}, err
	}
	text, _ := Render(res, maxOutputBytes)
	return Result{Text: text, IsError: res.IsError, Transient: res.Transient}, nil
}

// Invoke runs the executor once under the tool's timeout, without validation, retries
// or output shaping. A *ModelError becomes an error result; any other error is an
// internal failure.
func (t *Tool) Invoke(ctx context.Context, call Call) (Result, error) {
	if t.Exec == nil {
		return Result{}, fmt.Errorf("tool %s: no executor", t.Name)
	}
	if t.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, t.Timeout)
		defer cancel()
	}
	res, err := t.Exec(ctx, call)
	if err != nil {
		var me *ModelError
		if errors.As(err, &me) {
			return Result{IsError: true, Text: me.Message}, nil
		}
		return Result{}, fmt.Errorf("tool %s: %w", t.Name, err)
	}
	return res, nil
}

// Message is the approval sentence for a call (Describe, or a generic one).
func (t *Tool) Message(input json.RawMessage) string {
	if t.Describe != nil {
		if m := t.Describe(input); m != "" {
			return m
		}
	}
	return "Run " + t.Name
}

func compileSchema(name string, raw json.RawMessage) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("tool %s: input schema is not JSON: %w", name, err)
	}
	loc := "https://yuno.local/tools/" + name + ".json"
	c := jsonschema.NewCompiler()
	if err := c.AddResource(loc, doc); err != nil {
		return nil, fmt.Errorf("tool %s: input schema: %w", name, err)
	}
	sch, err := c.Compile(loc)
	if err != nil {
		return nil, fmt.Errorf("tool %s: input schema: %w", name, err)
	}
	return sch, nil
}

// collectLeaves renders the innermost validation errors, which name the offending
// location ("at '/run_token': got number, want string").
func collectLeaves(e *jsonschema.ValidationError, out *[]string) {
	if len(e.Causes) == 0 {
		*out = append(*out, e.Error())
		return
	}
	for _, c := range e.Causes {
		collectLeaves(c, out)
	}
}

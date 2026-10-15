// Package tools is the registry of things an agent can call. Every tool has an explicit
// JSON Schema, a risk class, a timeout, a retry budget and a declaration of whether its
// output is untrusted (DESIGN 5.7 rule 9).
package tools

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"
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
)

// Call is one invocation as requested by the model, with run context.
type Call struct {
	RunID       string
	NodeVisitID string
	NodeID      string
	AgentID     string
	ToolUseID   string
	Input       json.RawMessage
	// IdempotencyKey is set by the engine for side-effecting tools (DESIGN 5.4).
	IdempotencyKey string
}

// Result is what goes back to the model. Output is JSON; Text is a convenience for
// plain-text results and is wrapped into {"text": ...} when Output is nil.
type Result struct {
	Output  json.RawMessage
	Text    string
	IsError bool
	// Transient marks an error the retry policy may retry (DESIGN 5.5 rule 4).
	Transient bool
}

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
	Exec        Executor        `json:"-"`
}

type Registry struct {
	mu    sync.RWMutex
	tools map[string]*Tool
}

func NewRegistry() *Registry {
	return &Registry{tools: map[string]*Tool{}}
}

func (r *Registry) Register(t *Tool) {
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

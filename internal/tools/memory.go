package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"yuno/internal/memory"
	"yuno/internal/store"
)

// Memory tools (DESIGN 5.7 table, 5.10, 5.18): an agent's own facts (scope agent, the
// default) and the notes shared by every agent of a run on a project (scope project).
// Retention, eviction and recall order are internal/memory's. Both also work in a
// conversation, with the agent's chat token (DESIGN 5.15 rule 2): the schemas require
// neither token and the MCP gate requires exactly one; scope project needs a run.
const (
	MemorySaveName   = "memory_save"
	MemoryRecallName = "memory_recall"
)

var memorySaveSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "run_token": {"type": "string", "description": "The run token from the task message. In a conversation, pass chat_token instead."},
    "chat_token": {"type": "string", "description": "Your chat token from AGENTS.md, only when you are chatting with a person and have no run token."},
    "key": {"type": "string", "minLength": 1, "maxLength": 200, "description": "What the fact is about; saving the same key again replaces it."},
    "value": {"type": "string", "minLength": 1, "maxLength": 2048, "description": "The fact, at most 2048 bytes."},
    "ttl_days": {"type": "integer", "minimum": 0, "maximum": 3650, "description": "Forget the fact after this many days; default from the agent's memory policy (30 days unless set), 90 days for scope project."},
    "scope": {"type": "string", "enum": ["agent", "project"], "description": "agent (the default) keeps the fact for you; project shares it with every agent working on this run's project, and needs a run token of a run on a project."}
  },
  "required": ["key", "value"],
  "additionalProperties": false
}`)

var memoryRecallSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "run_token": {"type": "string", "description": "The run token from the task message. In a conversation, pass chat_token instead."},
    "chat_token": {"type": "string", "description": "Your chat token from AGENTS.md, only when you are chatting with a person and have no run token."},
    "query": {"type": "string", "maxLength": 500, "description": "Words that must all appear in a saved key or value; empty returns the newest facts."},
    "scope": {"type": "string", "enum": ["agent", "project"], "description": "agent (the default) searches your own facts; project searches the notes saved for this run's project, and needs a run token of a run on a project."}
  },
  "required": ["query"],
  "additionalProperties": false
}`)

// errMemoryDisabled is what the model sees when its agent's memory policy is off.
var errMemoryDisabled = &ModelError{Message: "memory is not enabled for this agent"}

// errNoProjectRun is what the model sees when it asks for scope project outside a run on
// a project: in a conversation (a chat token) or in a run that names none (DESIGN 5.18
// rule 3).
var errNoProjectRun = &ModelError{Message: "project notes need a run on a project"}

// Memory scopes a call may name (DESIGN 5.18 rule 3).
const (
	memoryScopeAgent   = "agent"
	memoryScopeProject = "project"
)

// projectOfCall is the project a call's run works on; errNoProjectRun when the call has
// no run (a chat token), the run is gone or it names no project.
func projectOfCall(ctx context.Context, pool *pgxpool.Pool, call Call) (string, error) {
	if call.RunID == "" {
		return "", errNoProjectRun
	}
	run, err := store.GetRun(ctx, pool, call.RunID)
	if errors.Is(err, store.ErrNotFound) {
		return "", errNoProjectRun
	}
	if err != nil {
		return "", err
	}
	if run.ProjectID == nil || *run.ProjectID == "" {
		return "", errNoProjectRun
	}
	return *run.ProjectID, nil
}

// checkScope validates a call's scope: agent when absent.
func checkScope(scope string) (string, error) {
	switch scope {
	case "", memoryScopeAgent:
		return memoryScopeAgent, nil
	case memoryScopeProject:
		return memoryScopeProject, nil
	}
	return "", &ModelError{Message: "invalid input: scope must be agent or project"}
}

// memoryPolicy loads the calling agent's memory policy; a missing agent has none.
func memoryPolicy(ctx context.Context, pool *pgxpool.Pool, agentID string) (store.MemoryPolicy, error) {
	a, err := store.GetAgent(ctx, pool, agentID)
	if errors.Is(err, store.ErrNotFound) {
		return store.MemoryPolicy{}, nil
	}
	if err != nil {
		return store.MemoryPolicy{}, err
	}
	return a.MemoryPolicy, nil
}

// MemorySaved is told about every fact memory_save stored, after the save; the api
// re-renders the agent's MEMORY.md with it (tools cannot import the openclaw package).
type MemorySaved func(ctx context.Context, call Call)

// NewMemorySave builds memory_save: upsert the fact for the agent, expiring after
// ttl_days (else the policy's fact_ttl_days, else 30 days), keeping at most the
// policy's max_facts live facts (DESIGN 5.10 rule 3). saved, when set, runs after each
// save.
func NewMemorySave(pool *pgxpool.Pool, saved MemorySaved) *Tool {
	return &Tool{
		Name:        MemorySaveName,
		Description: "Remember a fact for your future runs and conversations under a short key. Saving an existing key replaces its value.",
		InputSchema: memorySaveSchema,
		Risk:        RiskComputational,
		Hits:        HitsPostgres,
		Timeout:     5 * time.Second,
		Chat:        true,
		Exec: func(ctx context.Context, call Call) (Result, error) {
			var in struct {
				Key     string `json:"key"`
				Value   string `json:"value"`
				TTLDays *int   `json:"ttl_days"`
				Scope   string `json:"scope"`
			}
			if err := json.Unmarshal(call.Input, &in); err != nil {
				return Result{}, &ModelError{Message: "invalid input: " + err.Error()}
			}
			scope, err := checkScope(in.Scope)
			if err != nil {
				return Result{}, err
			}
			if pool == nil {
				return Result{}, errors.New("memory: no database")
			}
			pol, err := memoryPolicy(ctx, pool, call.AgentID)
			if err != nil {
				return Result{}, err
			}
			if !pol.Enabled {
				return Result{}, errMemoryDisabled
			}
			if len(in.Value) > memory.MaxValueBytes {
				return Result{}, &ModelError{Message: memory.ErrValueTooLong.Error()}
			}
			now := time.Now().UTC()
			if scope == memoryScopeProject {
				return saveProjectNote(ctx, pool, call, in.Key, in.Value, in.TTLDays, now)
			}
			exp := memory.ExpiresAt(pol, in.TTLDays, now)
			f := &store.MemoryFact{Scope: store.MemoryScopeAgent, AgentID: call.AgentID, Key: in.Key, Value: in.Value, ExpiresAt: &exp}
			if call.RunID != "" {
				f.SourceRunID = &call.RunID
			}
			if err := memory.Save(ctx, pool, f, memory.MaxFacts(pol), now); err != nil {
				return Result{}, err
			}
			if saved != nil {
				saved(ctx, call)
			}
			return JSONResult(map[string]any{"status": "saved", "key": f.Key, "expires_at": exp.Format(time.RFC3339)}), nil
		},
	}
}

// saveProjectNote is memory_save with scope project (DESIGN 5.18 rule 3): an upsert by
// (project, key) for the project the call's run works on, recorded with the calling
// agent and run. The agent's own MEMORY.md does not change, so the render hook is not
// called.
func saveProjectNote(ctx context.Context, pool *pgxpool.Pool, call Call, key, value string, ttlDays *int, now time.Time) (Result, error) {
	projectID, err := projectOfCall(ctx, pool, call)
	if err != nil {
		return Result{}, err
	}
	exp := memory.ProjectFactExpiresAt(ttlDays, now)
	agentID, runID := call.AgentID, call.RunID
	f := &store.ProjectFact{ProjectID: projectID, Key: key, Value: value, AgentID: &agentID, SourceRunID: &runID, ExpiresAt: &exp}
	if err := memory.SaveProjectFact(ctx, pool, f, now); err != nil {
		if errors.Is(err, memory.ErrProjectKeyLength) || errors.Is(err, memory.ErrEmptyValue) || errors.Is(err, memory.ErrValueTooLong) {
			return Result{}, &ModelError{Message: err.Error()}
		}
		return Result{}, err
	}
	return JSONResult(map[string]any{"status": "saved", "scope": memoryScopeProject, "key": f.Key, "expires_at": exp.Format(time.RFC3339)}), nil
}

// NewMemoryRecall builds memory_recall: the agent's live facts containing every word
// of the query, newest first, at most twenty (DESIGN 5.10 rule 4). Facts are text the
// agent wrote from what it read, so they come back as untrusted content, escaped like
// http_fetch's (audit #31). maxOutputBytes is the cap the MCP server applies to every
// result (0 means none): the oldest facts are left out until the block fits, so the cap
// never cuts the closing tag.
func NewMemoryRecall(pool *pgxpool.Pool, maxOutputBytes int) *Tool {
	return &Tool{
		Name:        MemoryRecallName,
		Description: "Look up facts you saved earlier. Returns up to twenty that contain all the words of the query, newest first.",
		InputSchema: memoryRecallSchema,
		Risk:        RiskReadOnly,
		Hits:        HitsPostgres,
		Timeout:     5 * time.Second,
		Untrusted:   true,
		Chat:        true,
		Exec: func(ctx context.Context, call Call) (Result, error) {
			var in struct {
				Query string `json:"query"`
				Scope string `json:"scope"`
			}
			if err := json.Unmarshal(call.Input, &in); err != nil {
				return Result{}, &ModelError{Message: "invalid input: " + err.Error()}
			}
			scope, err := checkScope(in.Scope)
			if err != nil {
				return Result{}, err
			}
			if pool == nil {
				return Result{}, errors.New("memory: no database")
			}
			pol, err := memoryPolicy(ctx, pool, call.AgentID)
			if err != nil {
				return Result{}, err
			}
			if !pol.Enabled {
				return Result{}, errMemoryDisabled
			}
			now := time.Now().UTC()
			var out []recalledFact
			if scope == memoryScopeProject {
				projectID, err := projectOfCall(ctx, pool, call)
				if err != nil {
					return Result{}, err
				}
				notes, err := memory.RecallProjectFacts(ctx, pool, projectID, in.Query, memory.RecallLimit, now)
				if err != nil {
					return Result{}, err
				}
				out = make([]recalledFact, len(notes))
				for i, f := range notes {
					out[i] = recalledFact{Key: f.Key, Value: f.Value, UpdatedAt: f.UpdatedAt.UTC().Format(time.RFC3339)}
				}
			} else {
				facts, err := memory.Recall(ctx, pool, call.AgentID, in.Query, memory.RecallLimit, now)
				if err != nil {
					return Result{}, err
				}
				out = make([]recalledFact, len(facts))
				for i, f := range facts {
					out[i] = recalledFact{Key: f.Key, Value: f.Value, UpdatedAt: f.UpdatedAt.UTC().Format(time.RFC3339)}
				}
			}
			return Result{Text: recallText(out, maxOutputBytes)}, nil
		},
	}
}

type recalledFact struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	UpdatedAt string `json:"updated_at"`
}

// recallText is the facts as JSON in an untrusted_content block of at most maxBytes,
// with "omitted" counting the oldest facts left out to fit.
func recallText(facts []recalledFact, maxBytes int) string {
	for omitted := 0; ; omitted++ {
		body := map[string]any{"facts": facts[:len(facts)-omitted]}
		if omitted > 0 {
			body["omitted"] = omitted
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false) // the block escapes its closing tag itself, as http_fetch's does
		_ = enc.Encode(body)
		text := WrapUntrustedFrom(MemoryRecallName, strings.TrimSuffix(buf.String(), "\n"))
		if maxBytes <= 0 || len(text) <= maxBytes || omitted == len(facts) {
			return text
		}
	}
}

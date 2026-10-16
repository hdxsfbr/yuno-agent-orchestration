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

// Memory tools (DESIGN 5.7 table, 5.10): an agent's own facts, scope agent this wave.
// Retention, eviction and recall order are internal/memory's.
const (
	MemorySaveName   = "memory_save"
	MemoryRecallName = "memory_recall"
)

var memorySaveSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "run_token": {"type": "string", "description": "The run token from the task message."},
    "key": {"type": "string", "minLength": 1, "maxLength": 200, "description": "What the fact is about; saving the same key again replaces it."},
    "value": {"type": "string", "minLength": 1, "maxLength": 2048, "description": "The fact, at most 2048 bytes."},
    "ttl_days": {"type": "integer", "minimum": 0, "maximum": 3650, "description": "Forget the fact after this many days; default from the agent's memory policy (30 days unless set)."}
  },
  "required": ["run_token", "key", "value"],
  "additionalProperties": false
}`)

var memoryRecallSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "run_token": {"type": "string", "description": "The run token from the task message."},
    "query": {"type": "string", "maxLength": 500, "description": "Words that must all appear in a saved key or value; empty returns the newest facts."}
  },
  "required": ["run_token", "query"],
  "additionalProperties": false
}`)

// errMemoryDisabled is what the model sees when its agent's memory policy is off.
var errMemoryDisabled = &ModelError{Message: "memory is not enabled for this agent"}

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
		Description: "Remember a fact for your future runs under a short key. Saving an existing key replaces its value.",
		InputSchema: memorySaveSchema,
		Risk:        RiskComputational,
		Hits:        HitsPostgres,
		Timeout:     5 * time.Second,
		Exec: func(ctx context.Context, call Call) (Result, error) {
			var in struct {
				Key     string `json:"key"`
				Value   string `json:"value"`
				TTLDays *int   `json:"ttl_days"`
			}
			if err := json.Unmarshal(call.Input, &in); err != nil {
				return Result{}, &ModelError{Message: "invalid input: " + err.Error()}
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

// NewMemoryRecall builds memory_recall: the agent's live facts containing every word
// of the query, newest first, at most twenty (DESIGN 5.10 rule 4). Facts are text the
// agent wrote from what it read, so they come back as untrusted content, escaped like
// http_fetch's (audit #31). maxOutputBytes is the cap the MCP server applies to every
// result (0 means none): the oldest facts are left out until the block fits, so the cap
// never cuts the closing tag.
func NewMemoryRecall(pool *pgxpool.Pool, maxOutputBytes int) *Tool {
	return &Tool{
		Name:        MemoryRecallName,
		Description: "Look up facts you saved in earlier runs. Returns up to twenty that contain all the words of the query, newest first.",
		InputSchema: memoryRecallSchema,
		Risk:        RiskReadOnly,
		Hits:        HitsPostgres,
		Timeout:     5 * time.Second,
		Untrusted:   true,
		Exec: func(ctx context.Context, call Call) (Result, error) {
			var in struct {
				Query string `json:"query"`
			}
			if err := json.Unmarshal(call.Input, &in); err != nil {
				return Result{}, &ModelError{Message: "invalid input: " + err.Error()}
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
			facts, err := memory.Recall(ctx, pool, call.AgentID, in.Query, memory.RecallLimit, time.Now().UTC())
			if err != nil {
				return Result{}, err
			}
			out := make([]recalledFact, len(facts))
			for i, f := range facts {
				out[i] = recalledFact{Key: f.Key, Value: f.Value, UpdatedAt: f.UpdatedAt.UTC().Format(time.RFC3339)}
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

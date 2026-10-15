package tools

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"yuno/internal/store"
)

// Memory tools (DESIGN 5.7 table, 5.10): an agent's own facts, scope agent this wave.
const (
	MemorySaveName   = "memory_save"
	MemoryRecallName = "memory_recall"

	// defaultMaxFacts applies when the agent's memory policy sets no max_facts; recall
	// returns at most recallLimit facts (DESIGN 5.10 rules 3 and 4).
	defaultMaxFacts = 200
	recallLimit     = 20
)

var memorySaveSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "run_token": {"type": "string", "description": "The run token from the task message."},
    "key": {"type": "string", "minLength": 1, "maxLength": 200, "description": "What the fact is about; saving the same key again replaces it."},
    "value": {"type": "string", "minLength": 1, "maxLength": 4000, "description": "The fact."},
    "ttl_days": {"type": "integer", "minimum": 1, "maximum": 3650, "description": "Forget the fact after this many days; default from the agent's memory policy."}
  },
  "required": ["run_token", "key", "value"],
  "additionalProperties": false
}`)

var memoryRecallSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "run_token": {"type": "string", "description": "The run token from the task message."},
    "query": {"type": "string", "maxLength": 500, "description": "Words to look for in saved keys and values; empty returns the newest facts."}
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

// NewMemorySave builds memory_save: upsert the fact for the agent, expiring after
// ttl_days (else the policy's fact_ttl_days, else never), keeping at most the policy's
// max_facts newest facts.
func NewMemorySave(pool *pgxpool.Pool) *Tool {
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
				TTLDays int    `json:"ttl_days"`
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
			ttl := in.TTLDays
			if ttl == 0 {
				ttl = pol.FactTTLDays
			}
			f := &store.MemoryFact{Scope: store.MemoryScopeAgent, AgentID: call.AgentID, Key: in.Key, Value: in.Value}
			if call.RunID != "" {
				f.SourceRunID = &call.RunID
			}
			if ttl > 0 {
				exp := time.Now().UTC().Add(time.Duration(ttl) * 24 * time.Hour)
				f.ExpiresAt = &exp
			}
			maxFacts := pol.MaxFacts
			if maxFacts <= 0 {
				maxFacts = defaultMaxFacts
			}
			if err := store.SaveMemoryFact(ctx, pool, f, maxFacts); err != nil {
				return Result{}, err
			}
			out := map[string]any{"status": "saved", "key": f.Key}
			if f.ExpiresAt != nil {
				out["expires_at"] = f.ExpiresAt.Format(time.RFC3339)
			}
			return JSONResult(out), nil
		},
	}
}

// NewMemoryRecall builds memory_recall: the agent's live facts matching the query's
// words, best matches first, at most twenty.
func NewMemoryRecall(pool *pgxpool.Pool) *Tool {
	return &Tool{
		Name:        MemoryRecallName,
		Description: "Look up facts you saved in earlier runs. Returns up to twenty, best matches first.",
		InputSchema: memoryRecallSchema,
		Risk:        RiskReadOnly,
		Hits:        HitsPostgres,
		Timeout:     5 * time.Second,
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
			facts, err := store.RecallMemoryFacts(ctx, pool, call.AgentID, store.MemoryScopeAgent, in.Query, recallLimit)
			if err != nil {
				return Result{}, err
			}
			type fact struct {
				Key       string `json:"key"`
				Value     string `json:"value"`
				UpdatedAt string `json:"updated_at"`
			}
			out := make([]fact, len(facts))
			for i, f := range facts {
				out[i] = fact{Key: f.Key, Value: f.Value, UpdatedAt: f.UpdatedAt.UTC().Format(time.RFC3339)}
			}
			return JSONResult(map[string]any{"facts": out}), nil
		},
	}
}

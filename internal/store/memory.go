package store

import "time"

// MemoryScopeAgent is the only memory scope this wave (DESIGN 5.10).
const MemoryScopeAgent = "agent"

// MemoryFact is one remembered key/value for an agent.
type MemoryFact struct {
	ID          string     `db:"id" json:"id"`
	Scope       string     `db:"scope" json:"scope"`
	AgentID     string     `db:"agent_id" json:"agent_id"`
	Key         string     `db:"key" json:"key"`
	Value       string     `db:"value" json:"value"`
	SourceRunID *string    `db:"source_run_id" json:"source_run_id,omitempty"`
	CreatedAt   time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt   time.Time  `db:"updated_at" json:"updated_at"`
	ExpiresAt   *time.Time `db:"expires_at" json:"expires_at,omitempty"`
}

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"yuno/internal/memory"
	"yuno/internal/store"
)

func memoryCall(f *fixture, input string) Call {
	return Call{RunID: f.run.ID, NodeVisitID: f.visit.ID, AgentID: f.agent.ID, Input: json.RawMessage(input)}
}

func recallKeys(t *testing.T, tool *Tool, f *fixture, query string) []string {
	t.Helper()
	res, err := tool.Invoke(context.Background(), memoryCall(f, `{"run_token":"r","query":`+jsonString(query)+`}`))
	if err != nil || res.IsError {
		t.Fatalf("recall %q: %+v %v", query, res, err)
	}
	var out struct {
		Facts []struct {
			Key, Value string
		} `json:"facts"`
	}
	must(t, json.Unmarshal(res.Output, &out))
	keys := make([]string, len(out.Facts))
	for i, f := range out.Facts {
		keys[i] = f.Key
	}
	return keys
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// T5.7.5 (memory_save, memory_recall) on the real database.
func TestMemorySaveRecall(t *testing.T) {
	pool := needDB(t)
	f := newFixture(t, pool, &store.MemoryPolicy{Enabled: true, MaxFacts: 3})
	save, recall := NewMemorySave(pool), NewMemoryRecall(pool)
	ctx := context.Background()
	put := func(key, value string, extra string) Result {
		t.Helper()
		res, err := save.Invoke(ctx, memoryCall(f, `{"run_token":"r","key":`+jsonString(key)+`,"value":`+jsonString(value)+extra+`}`))
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if r := put("deploy cadence", "Releases go out on Tuesdays", ""); r.IsError || !strings.Contains(string(r.Output), `"saved"`) {
		t.Fatalf("save: %+v", r)
	}
	time.Sleep(5 * time.Millisecond)
	put("python version", "The sandbox runs Python 3.12", "")
	time.Sleep(5 * time.Millisecond)
	put("deploy cadence", "Releases go out on Thursdays", "") // replaces, becomes newest

	if got := recallKeys(t, recall, f, "THURSDAYS"); len(got) != 1 || got[0] != "deploy cadence" {
		t.Fatalf("recall by value, case-insensitive = %v", got)
	}
	if got := recallKeys(t, recall, f, "python sandbox"); len(got) != 1 || got[0] != "python version" {
		t.Fatalf("recall by words = %v", got)
	}
	if got := recallKeys(t, recall, f, ""); len(got) != 2 || got[0] != "deploy cadence" {
		t.Fatalf("empty query = %v, want newest first", got)
	}
	if got := recallKeys(t, recall, f, "nothing matches"); len(got) != 0 {
		t.Fatalf("no match = %v", got)
	}

	// max_facts 3 evicts the oldest by created_at: deploy cadence was first saved before
	// python version, even though its replacement is the newest write (DESIGN 5.10
	// rule 3).
	for _, k := range []string{"a", "b"} {
		time.Sleep(5 * time.Millisecond)
		put("fact "+k, "value "+k, "")
	}
	if got := recallKeys(t, recall, f, ""); len(got) != 3 || strings.Join(got, ",") != "fact b,fact a,python version" {
		t.Fatalf("after eviction = %v", got)
	}

	// An expired fact is not recalled.
	r := put("short lived", "gone soon", `,"ttl_days":1`)
	if !strings.Contains(string(r.Output), "expires_at") {
		t.Fatalf("ttl not reported: %s", r.Output)
	}
	if _, err := pool.Exec(ctx, `UPDATE memory_facts SET expires_at = now() - interval '1 minute' WHERE agent_id=$1 AND key='short lived'`, f.agent.ID); err != nil {
		t.Fatal(err)
	}
	for _, k := range recallKeys(t, recall, f, "gone") {
		t.Fatalf("expired fact recalled: %s", k)
	}

	// ttl_days 0 expires at once and is never returned (DESIGN 5.10 edge cases).
	if r := put("instant", "never seen", `,"ttl_days":0`); r.IsError {
		t.Fatalf("ttl 0: %+v", r)
	}
	for _, k := range recallKeys(t, recall, f, "never seen") {
		t.Fatalf("ttl 0 fact recalled: %s", k)
	}
	// Values are bounded to 2 KiB.
	if r := put("big", strings.Repeat("é", 1025), ""); !r.IsError || !strings.Contains(r.Text, "2048 bytes") {
		t.Fatalf("over 2 KiB: %+v", r)
	}
}

// Without ttl_days or a policy TTL a fact lives 30 days (DESIGN 5.10 inputs).
func TestMemoryDefaultTTL(t *testing.T) {
	pool := needDB(t)
	f := newFixture(t, pool, &store.MemoryPolicy{Enabled: true})
	before := time.Now().UTC()
	res, err := NewMemorySave(pool).Invoke(context.Background(), memoryCall(f, `{"run_token":"r","key":"k","value":"v"}`))
	if err != nil || res.IsError {
		t.Fatalf("save: %+v %v", res, err)
	}
	var exp time.Time
	must(t, pool.QueryRow(context.Background(), `SELECT expires_at FROM memory_facts WHERE agent_id=$1`, f.agent.ID).Scan(&exp))
	if d := exp.Sub(before); d < 30*24*time.Hour || d > 30*24*time.Hour+time.Minute {
		t.Fatalf("expires in %s, want 30 days", d)
	}
}

func TestMemoryDisabled(t *testing.T) {
	pool := needDB(t)
	f := newFixture(t, pool, &store.MemoryPolicy{Enabled: false})
	for _, tool := range []*Tool{NewMemorySave(pool), NewMemoryRecall(pool)} {
		res, err := tool.Invoke(context.Background(), memoryCall(f, `{"run_token":"r","key":"k","value":"v","query":"k"}`))
		if err != nil || !res.IsError || !strings.Contains(res.Text, "not enabled") {
			t.Fatalf("%s with memory off: %+v %v", tool.Name, res, err)
		}
	}
}

// DESIGN 5.10 rules 3 and 4: without max_facts in the policy an agent keeps its 200
// newest facts, and recall returns at most 20.
func TestMemoryDefaults(t *testing.T) {
	pool := needDB(t)
	f := newFixture(t, pool, &store.MemoryPolicy{Enabled: true})
	ctx := context.Background()
	seeded := time.Now().Add(-time.Hour)
	for i := range 200 {
		fact := &store.MemoryFact{Scope: store.MemoryScopeAgent, AgentID: f.agent.ID, Key: fmt.Sprintf("fact %03d", i), Value: "v"}
		must(t, memory.Save(ctx, pool, fact, 0, seeded.Add(time.Duration(i)*time.Second)))
	}
	res, err := NewMemorySave(pool).Invoke(ctx, memoryCall(f, `{"run_token":"r","key":"newest","value":"v"}`))
	if err != nil || res.IsError {
		t.Fatalf("save: %+v %v", res, err)
	}
	var kept int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM memory_facts WHERE agent_id=$1`, f.agent.ID).Scan(&kept))
	if kept != 200 {
		t.Fatalf("facts kept = %d, want the default max_facts 200", kept)
	}
	if got := recallKeys(t, NewMemoryRecall(pool), f, ""); len(got) != 20 || got[0] != "newest" {
		t.Fatalf("recall returned %d facts (first %v), want 20 newest first", len(got), got[:min(len(got), 1)])
	}
}

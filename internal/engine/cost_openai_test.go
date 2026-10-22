package engine

import (
	"testing"

	ymodel "yuno/internal/model"
	"yuno/internal/openclaw"
)

// D-091: an OpenAI call costs what the gateway reports from its own model catalog, an
// API-equivalent estimate (the subscription itself is paid in quota). The price table
// has no OpenAI entry, so nothing of ours is added to it, and a call the gateway does not
// price is recorded at 0 rather than guessed.
func TestCostMicrosOpenAI(t *testing.T) {
	reported := &openclaw.Usage{Input: 9_000, Output: 900, CacheRead: 20_000}
	reported.Cost.Total = 0.138052
	for _, ref := range openclaw.OpenAIModels {
		id := ref[len(openclaw.OpenAIProviderID)+1:]
		if _, ok := ymodel.LookupPrice(id); ok {
			t.Fatalf("%s has a price-table entry", id)
		}
		if got := costMicros(id, reported); got != 138_052 {
			t.Fatalf("%s: cost %d, want the gateway's 138052", id, got)
		}
		if got := costMicros(ref, reported); got != 138_052 {
			t.Fatalf("%s: cost %d", ref, got)
		}
		if got := costMicros(id, &openclaw.Usage{Input: 9_000, Output: 900}); got != 0 {
			t.Fatalf("%s without a reported cost: %d", id, got)
		}
	}
	// A Claude call still records the larger of the gateway's cost and the table's.
	claude := &openclaw.Usage{Input: 1_000, Output: 500}
	if got := costMicros("claude-opus-5-5", claude); got != 14_000 {
		t.Fatalf("claude cost %d, want 14000 from the table", got)
	}
}

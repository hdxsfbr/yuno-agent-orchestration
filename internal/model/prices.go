package model

import (
	_ "embed"
	"encoding/json"
	"sort"
	"strings"
	"sync"
)

//go:embed prices.json
var pricesJSON []byte

// Price is USD per million tokens, as published. Cache write defaults to 1.25 × input
// when zero; cache read defaults to 0.1 × input when zero.
type Price struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

var (
	pricesOnce sync.Once
	prices     map[string]Price
)

func loadPrices() {
	pricesOnce.Do(func() {
		prices = map[string]Price{}
		_ = json.Unmarshal(pricesJSON, &prices)
	})
}

// ScriptedModelID is the price-table entry of the offline scripted provider.
const ScriptedModelID = "scripted"

// AgentModelIDs returns the model ids an agent may be configured with: every entry of
// the price table except the scripted provider, sorted.
func AgentModelIDs() []string {
	loadPrices()
	out := make([]string, 0, len(prices))
	for id := range prices {
		if id != ScriptedModelID {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// LookupPrice returns the price for a model id. Unknown models return ok=false and the
// caller records cost 0 (DESIGN 5.12 rule 2).
func LookupPrice(modelID string) (Price, bool) {
	loadPrices()
	p, ok := prices[modelID]
	if !ok {
		// tolerate date-suffixed ids by prefix match on the longest key
		best := ""
		for k := range prices {
			if strings.HasPrefix(modelID, k) && len(k) > len(best) {
				best = k
			}
		}
		if best == "" {
			return Price{}, false
		}
		p = prices[best]
	}
	if p.CacheWrite == 0 {
		p.CacheWrite = p.Input * 1.25
	}
	if p.CacheRead == 0 {
		p.CacheRead = p.Input * 0.1
	}
	return p, true
}

// CostMicros computes micro-USD for a usage record with integer arithmetic:
// USD per MTok equals nano-USD per token, so cost_nanos = tokens × price × 1000 and
// cost_micros = cost_nanos / 1000. Unknown models cost 0 and ok=false.
func CostMicros(modelID string, u Usage) (int64, bool) {
	p, ok := LookupPrice(modelID)
	if !ok {
		return 0, false
	}
	nanos := int64(float64(u.InputTokens)*p.Input*1000) +
		int64(float64(u.OutputTokens)*p.Output*1000) +
		int64(float64(u.CacheReadTokens)*p.CacheRead*1000) +
		int64(float64(u.CacheWriteTokens)*p.CacheWrite*1000)
	return nanos / 1000, true
}

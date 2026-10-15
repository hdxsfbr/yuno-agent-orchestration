package model

import "testing"

// T5.12.2: cost arithmetic per priced model.
func TestCostMicros(t *testing.T) {
	cases := []struct {
		model string
		u     Usage
		want  int64
	}{
		// 1M input at $4/MTok = $4.00 = 4_000_000 micro-USD
		{"claude-opus-5-5", Usage{InputTokens: 1_000_000}, 4_000_000},
		// 1000 in + 500 out on Opus 5.5: 1000×4 + 500×20 = 14000 nano... ×1000/1000 → 4000+10000 = 14_000 micro
		{"claude-opus-5-5", Usage{InputTokens: 1000, OutputTokens: 500}, 14_000},
		// cache read 10000 tokens at $0.20/MTok = 2000 micro
		{"claude-opus-5-5", Usage{CacheReadTokens: 10_000}, 2_000},
		// cache write defaults to 1.25 × input: 10000 × 5.0 = 50_000 micro
		{"claude-opus-5-5", Usage{CacheWriteTokens: 10_000}, 50_000},
		// haiku fractional price: 1000 tokens × $0.10/MTok = 100 micro
		{"claude-haiku-5-5", Usage{InputTokens: 1000}, 100},
		// sonnet
		{"claude-sonnet-5-5", Usage{InputTokens: 2000, OutputTokens: 100}, 4_000 + 1_000},
		// date-suffixed id falls back to prefix
		{"claude-sonnet-5-5-20260901", Usage{InputTokens: 1000}, 2_000},
	}
	for _, c := range cases {
		got, ok := CostMicros(c.model, c.u)
		if !ok {
			t.Errorf("%s: unknown model", c.model)
			continue
		}
		if got != c.want {
			t.Errorf("%s %+v: got %d, want %d", c.model, c.u, got, c.want)
		}
	}
	if got, ok := CostMicros("no-such-model", Usage{InputTokens: 100}); ok || got != 0 {
		t.Errorf("unknown model: got %d ok=%v", got, ok)
	}
}

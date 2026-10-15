package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

// T5.7.3: outputs are capped at TOOL_OUTPUT_MAX_BYTES with the truncation marker and
// redacted before they are stored or returned.
func TestRender(t *testing.T) {
	long := strings.Repeat("x", 300)
	tests := []struct {
		name       string
		res        Result
		max        int
		wantText   string // substring
		wantStored string // exact
	}{
		{"json output kept as json", Result{Output: json.RawMessage(`{"release_id":"r1","replayed":true}`)}, 100,
			`{"release_id":"r1","replayed":true}`, `{"release_id":"r1","replayed":true}`},
		{"text stored under text", Result{Text: "hello"}, 100, "hello", `{"text":"hello"}`},
		{"error stored under error", Result{IsError: true, Text: "boom"}, 100, "boom", `{"error":"boom"}`},
		{"truncated text has the marker", Result{Text: long}, 100, "[truncated 200 bytes]",
			`{"text":"` + strings.Repeat("x", 100) + `\n[truncated 200 bytes]"}`},
		{"truncated json is stored as text", Result{Output: json.RawMessage(`{"body":"` + long + `"}`)}, 50, "[truncated", ""},
		{"secrets redacted in text and storage", Result{Text: "key sk-ant-abcdefghijklmnop"}, 0, "[redacted]", `{"text":"key [redacted]"}`},
		{"secret keys redacted in stored json", Result{Output: json.RawMessage(`{"api_key":"plain","ok":1}`)}, 0, `"api_key":"plain"`, `{"api_key":"[redacted]","ok":1}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			text, stored := Render(tc.res, tc.max)
			if !strings.Contains(text, tc.wantText) {
				t.Fatalf("text = %q, want it to contain %q", text, tc.wantText)
			}
			if !json.Valid(stored) {
				t.Fatalf("stored is not JSON: %s", stored)
			}
			if tc.wantStored != "" && string(stored) != tc.wantStored {
				t.Fatalf("stored = %s, want %s", stored, tc.wantStored)
			}
			if tc.max > 0 && len(text) > tc.max+40 {
				t.Fatalf("text not capped: %d bytes", len(text))
			}
		})
	}
}

func TestWithFields(t *testing.T) {
	got := WithFields(json.RawMessage(`{"release_id":"r1"}`), map[string]any{"replayed": true, "policy": "auto"})
	if string(got) != `{"policy":"auto","release_id":"r1","replayed":true}` {
		t.Fatalf("WithFields = %s", got)
	}
	if got := WithFields(json.RawMessage(`[1]`), map[string]any{"replayed": true}); string(got) != `{"replayed":true,"result":[1]}` {
		t.Fatalf("non-object = %s", got)
	}
}

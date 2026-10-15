package tools

import (
	"encoding/json"
	"testing"
)

// T5.4.5: key order and whitespace differences produce the same key.
func TestIdempotencyKeyCanonical(t *testing.T) {
	base := `{"run_token":"r_a","version":"1.0.0","notes":"n","paths":["a.py","b.py"]}`
	key := func(run, visit, tool, input string) string {
		t.Helper()
		k, err := IdempotencyKey(run, visit, tool, json.RawMessage(input))
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	want := key("run1", "visit1", "deploy_release", base)
	if len(want) != 64 {
		t.Fatalf("key %q is not 64 hex characters", want)
	}
	same := []struct{ name, input string }{
		{"key order", `{"paths":["a.py","b.py"],"notes":"n","version":"1.0.0","run_token":"r_a"}`},
		{"whitespace", "{ \"run_token\" : \"r_a\",\n \"version\": \"1.0.0\", \"notes\":\"n\",\t\"paths\": [ \"a.py\", \"b.py\" ] }"},
		{"another run token of the same visit", `{"run_token":"r_b","version":"1.0.0","notes":"n","paths":["a.py","b.py"]}`},
		{"nested key order", `{"version":"1.0.0","notes":"n","paths":["a.py","b.py"],"run_token":"r_a"}`},
	}
	for _, tc := range same {
		if got := key("run1", "visit1", "deploy_release", tc.input); got != want {
			t.Errorf("%s: key %s, want %s", tc.name, got, want)
		}
	}
	differ := []struct{ name, run, visit, tool, input string }{
		{"different visit", "run1", "visit2", "deploy_release", base},
		{"different run", "run2", "visit1", "deploy_release", base},
		{"different tool", "run1", "visit1", "gitlab_create_mr", base},
		{"different value", "run1", "visit1", "deploy_release", `{"run_token":"r_a","version":"1.0.1","notes":"n","paths":["a.py","b.py"]}`},
		{"array order matters", "run1", "visit1", "deploy_release", `{"run_token":"r_a","version":"1.0.0","notes":"n","paths":["b.py","a.py"]}`},
	}
	for _, tc := range differ {
		if got := key(tc.run, tc.visit, tc.tool, tc.input); got == want {
			t.Errorf("%s: same key as the base input", tc.name)
		}
	}
}

func TestCanonicalJSON(t *testing.T) {
	tests := []struct{ in, want string }{
		{`{"b":1,"a":{"d":[1,2],"c":"x"}}`, `{"a":{"c":"x","d":[1,2]},"b":1}`},
		{`{"html":"<a>&</a>"}`, `{"html":"<a>&</a>"}`},
		{`{"n":1.50,"big":12345678901234567890}`, `{"big":12345678901234567890,"n":1.50}`},
		{`{"s":"żółw"}`, `{"s":"żółw"}`},
	}
	for _, tc := range tests {
		got, err := CanonicalJSON(json.RawMessage(tc.in))
		if err != nil || string(got) != tc.want {
			t.Errorf("CanonicalJSON(%s) = %s, %v; want %s", tc.in, got, err, tc.want)
		}
	}
	if _, err := CanonicalJSON(json.RawMessage(`{"a":1} {"b":2}`)); err == nil {
		t.Error("trailing data accepted")
	}
}

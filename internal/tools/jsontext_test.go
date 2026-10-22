package tools

import (
	"encoding/json"
	"fmt"
	"testing"
)

// D-104: no tool takes an open object (one with no declared properties, or one that allows
// extra ones) unless the same parameter also takes JSON text. A strict OpenAI tool schema
// closes such an object, so an OpenAI agent could only ever send {} there.
func TestToolSchemasTakeJSONText(t *testing.T) {
	var walk func(s any, path string, textAlt bool, bad *[]string)
	walk = func(s any, path string, textAlt bool, bad *[]string) {
		switch v := s.(type) {
		case map[string]any:
			if v["type"] == "object" {
				props, _ := v["properties"].(map[string]any)
				ap, hasAP := v["additionalProperties"]
				if (len(props) == 0 || (hasAP && ap != false)) && !textAlt {
					*bad = append(*bad, path)
				}
			}
			for k, c := range v {
				if k == "anyOf" {
					alts, _ := c.([]any)
					text := false
					for _, a := range alts {
						if m, ok := a.(map[string]any); ok && m["type"] == "string" {
							text = true
						}
					}
					for i, a := range alts {
						walk(a, fmt.Sprintf("%s/anyOf[%d]", path, i), text, bad)
					}
					continue
				}
				walk(c, path+"/"+k, false, bad)
			}
		case []any:
			for i, c := range v {
				walk(c, fmt.Sprintf("%s[%d]", path, i), false, bad)
			}
		}
	}
	for _, tool := range NewPlatformRegistry(PlatformDeps{}).List() {
		var s any
		if err := json.Unmarshal(tool.InputSchema, &s); err != nil {
			t.Fatalf("%s: %v", tool.Name, err)
		}
		var bad []string
		walk(s, "", false, &bad)
		for _, p := range bad {
			t.Errorf("%s: %s is an open object with no JSON-text form", tool.Name, p)
		}
	}
}

func TestJSONText(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		object  bool
		want    string
		wantErr bool
	}{
		{`{"a":1}`, true, `{"a":1}`, false},
		{`"{\"a\":1}"`, true, `{"a":1}`, false},
		{`" {\"a\":1} "`, true, `{"a":1}`, false},
		{`"[1]"`, true, "", true},
		{`"nope"`, true, "", true},
		{`"[1,2]"`, false, `[1,2]`, false},
		{`"nope"`, false, "", true},
		{``, true, "", false},
		{`null`, true, "", false},
	} {
		got, err := jsonText(json.RawMessage(tc.raw), "x", tc.object)
		if (err != nil) != tc.wantErr || string(got) != tc.want {
			t.Errorf("jsonText(%s, %v) = %s, %v", tc.raw, tc.object, got, err)
		}
	}
}

// D-104: platform_call reads path_params, query and body given as JSON text, as an OpenAI
// agent sends them, and as objects.
func TestPlatformCallReadsJSONText(t *testing.T) {
	for _, raw := range []string{
		`{"operation_id":"x","path_params":"{\"id\":\"a1\"}","query":"{\"limit\":5}","body":"{\"name\":\"n\"}"}`,
		`{"operation_id":"x","path_params":{"id":"a1"},"query":{"limit":5},"body":{"name":"n"}}`,
	} {
		var in platformCallInput
		if err := json.Unmarshal([]byte(raw), &in); err != nil {
			t.Fatal(err)
		}
		if err := in.readText(); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if string(in.PathParams["id"]) != `"a1"` || string(in.Query["limit"]) != `5` || string(in.Body) != `{"name":"n"}` {
			t.Fatalf("%s: path %v query %v body %s", raw, in.PathParams, in.Query, in.Body)
		}
	}
	var in platformCallInput
	_ = json.Unmarshal([]byte(`{"operation_id":"x","path_params":"id=a1"}`), &in)
	if err := in.readText(); err == nil {
		t.Fatal("path_params text that is not an object was accepted")
	}
}
